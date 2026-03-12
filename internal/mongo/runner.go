package mongo

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/workloads"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Define BSON Types locally
const (
	TypeEmbeddedDocument = 0x03
	TypeArray            = 0x04
	TypeInt64            = 0x12
)

type queryTask struct {
	definition config.QueryDefinition
	database   *mongo.Database
	runID      int64
	debug      bool
	rng        *rand.Rand
}

type workloadConfig struct {
	database           *mongo.Database
	appConfig          *config.AppConfig
	concurrency        int
	duration           time.Duration
	collections        []config.CollectionDefinition
	queryMap           map[string][]config.QueryDefinition
	percentages        map[string]int
	debug              bool
	findBatchSize      int32
	findLimit          int64
	maxInsertCache     int
	primaryFilterField string
	collector          *stats.Collector
}

var InsertDocumentCache chan map[string]interface{}

var operationTypes = []string{"find", "update", "delete", "insert", "insertMany", "aggregate", "transaction"}

func buildRawInt64Array(ids []int64) []byte {
	var buf bytes.Buffer
	buf.Write(make([]byte, 4))

	for i, val := range ids {
		key := strconv.Itoa(i)
		buf.WriteByte(TypeInt64)
		buf.Write([]byte(key))
		buf.WriteByte(0x00)

		b := make([]byte, 8)
		binary.LittleEndian.PutUint64(b, uint64(val))
		buf.Write(b)
	}
	buf.WriteByte(0x00)

	totalLen := int32(buf.Len())
	binary.LittleEndian.PutUint32(buf.Bytes()[0:4], uint32(totalLen))

	return buf.Bytes()
}

func buildRawBatch(template []byte, count int, magic []byte) ([]byte, []int, error) {
	var buf bytes.Buffer
	buf.Write(make([]byte, 4))

	offsets := make([]int, count)

	for i := 0; i < count; i++ {
		key := strconv.Itoa(i)
		buf.WriteByte(TypeEmbeddedDocument)
		buf.Write([]byte(key))
		buf.WriteByte(0x00)

		magicOffset := bytes.Index(template, magic)
		if magicOffset < 0 {
			return nil, nil, errors.New("magic marker not found in template")
		}

		offsets[i] = buf.Len() + magicOffset
		buf.Write(template)
	}
	buf.WriteByte(0x00)

	totalLen := int32(buf.Len())
	binary.LittleEndian.PutUint32(buf.Bytes()[0:4], uint32(totalLen))

	return buf.Bytes(), offsets, nil
}

type PreBuiltBatch struct {
	CmdRaw  []byte
	Offsets []int
}

// identifyPatchKey determines which field is the primary integer key for sharding/patching.
func identifyPatchKey(col config.CollectionDefinition) string {
	if col.ShardConfig != nil && len(col.ShardConfig.Key) > 0 {
		for k := range col.ShardConfig.Key {
			if f, ok := col.Fields[k]; ok && (f.Type == "int" || f.Type == "long") {
				return k
			}
			// Use the first key found if no int type match (Go map order is random, but this is a heuristic)
			return k
		}
	}
	if f, ok := col.Fields["_id"]; ok && (f.Type == "int" || f.Type == "long") {
		return "_id"
	}
	for k, f := range col.Fields {
		if (f.Type == "int" || f.Type == "long") && (strings.Contains(strings.ToLower(k), "id")) {
			return k
		}
	}
	return ""
}

func prepareGenericBatchPool(col config.CollectionDefinition, cfg *config.AppConfig, batchSize int, poolSize int) ([]PreBuiltBatch, string, error) {
	patchKey := identifyPatchKey(col)
	if patchKey == "" {
		return nil, "", fmt.Errorf("collection '%s' has no suitable integer key for optimization", col.Name)
	}

	pool := make([]PreBuiltBatch, poolSize)
	magic := int64(0x1122334455667788)
	magicBytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(magicBytes, uint64(magic))

	for p := 0; p < poolSize; p++ {
		var buf bytes.Buffer
		buf.Write(make([]byte, 4))
		offsets := make([]int, batchSize)

		for i := 0; i < batchSize; i++ {
			docMap := workloads.GenerateDocument(col, cfg)
			docMap[patchKey] = magic
			rawDoc, err := bson.Marshal(docMap)
			if err != nil {
				return nil, "", err
			}

			key := strconv.Itoa(i)
			buf.WriteByte(TypeEmbeddedDocument)
			buf.Write([]byte(key))
			buf.WriteByte(0x00)

			magicOffset := bytes.Index(rawDoc, magicBytes)
			if magicOffset < 0 {
				return nil, "", fmt.Errorf("magic marker not found in doc %d", i)
			}
			offsets[i] = buf.Len() + magicOffset
			buf.Write(rawDoc)
		}
		buf.WriteByte(0x00)
		totalLen := int32(buf.Len())
		binary.LittleEndian.PutUint32(buf.Bytes()[0:4], uint32(totalLen))
		arrayBytes := buf.Bytes()

		cmd := bson.D{
			{Key: "insert", Value: col.Name},
			{Key: "documents", Value: bson.RawValue{Type: TypeArray, Value: arrayBytes}},
			{Key: "ordered", Value: false},
		}
		cmdRaw, _ := bson.Marshal(cmd)
		arrStart := bytes.Index(cmdRaw, arrayBytes)
		finalOffsets := make([]int, batchSize)
		for i, off := range offsets {
			finalOffsets[i] = arrStart + off
		}

		pool[p] = PreBuiltBatch{CmdRaw: cmdRaw, Offsets: finalOffsets}
	}
	return pool, patchKey, nil
}

func selectOperation(percentages map[string]int, rng *rand.Rand) string {
	if percentages == nil {
		return "find"
	}
	r := rng.Intn(100)
	cum := 0
	for _, op := range operationTypes {
		cum += percentages[op]
		if r < cum {
			switch op {
			case "update":
				if rng.Intn(100) < 90 {
					return "updateOne"
				}
				return "updateMany"
			case "delete":
				if rng.Intn(100) < 90 {
					return "deleteOne"
				}
				return "deleteMany"
			default:
				return op
			}
		}
	}
	return "find"
}

func getPrimaryFilterField(ctx context.Context, db *mongo.Database, col config.CollectionDefinition) string {
	client := db.Client()
	dbName := db.Name()
	namespace := fmt.Sprintf("%s.%s", dbName, col.Name)
	configColl := client.Database("config").Collection("collections")
	var result struct {
		Key bson.M `bson:"key"`
	}
	filter := bson.M{"_id": namespace, "dropped": false}
	err := configColl.FindOne(ctx, filter).Decode(&result)
	if err != nil {
		return "_id"
	}
	for k := range result.Key {
		return k
	}
	return "_id"
}

func generateFallbackQuery(ctx context.Context, db *mongo.Database, opType string, col config.CollectionDefinition, rng *rand.Rand, filterField string, cfg *config.AppConfig) (config.QueryDefinition, bool) {
	collectionName := col.Name
	fieldType := "int"
	if filterField == "_id" {
		fieldType = "string"
	}
	if def, ok := col.Fields[filterField]; ok {
		fieldType = def.Type
	}
	filter := map[string]interface{}{filterField: fmt.Sprintf("<%s>", fieldType)}
	if opType == "updateOne" || opType == "updateMany" {
		updatePayload := workloads.GenerateFallbackUpdate(col, cfg, rng)
		return config.QueryDefinition{Collection: collectionName, Operation: opType, Filter: filter, Update: updatePayload}, true
	}
	if opType == "deleteOne" || opType == "deleteMany" {
		return config.QueryDefinition{Collection: collectionName, Operation: opType, Filter: filter}, true
	}
	return config.QueryDefinition{}, false
}

func selectRandomQueryByType(ctx context.Context, db *mongo.Database, opType string, queryMap map[string][]config.QueryDefinition, col config.CollectionDefinition, debug bool, rng *rand.Rand, filterField string, cfg *config.AppConfig) (config.QueryDefinition, bool) {
	candidates, ok := queryMap[opType]
	if !ok || len(candidates) == 0 {
		if opType == "find" || opType == "updateOne" || opType == "updateMany" || opType == "deleteOne" || opType == "deleteMany" {
			return generateFallbackQuery(ctx, db, opType, col, rng, filterField, cfg)
		}
		return config.QueryDefinition{}, false
	}
	return candidates[rng.Intn(len(candidates))], true
}

func generateInsertQuery(col config.CollectionDefinition, rng *rand.Rand, cfg *config.AppConfig) config.QueryDefinition {
	var doc map[string]interface{}
	select {
	case doc = <-InsertDocumentCache:
	default:
		doc = workloads.GenerateDocument(col, cfg)
	}
	return config.QueryDefinition{Collection: col.Name, Operation: "insert", Filter: doc}
}

func generateInsertManyQuery(col config.CollectionDefinition, rng *rand.Rand, cfg *config.AppConfig) []interface{} {
	count := cfg.InsertBatchSize
	docs := make([]interface{}, count)
	for i := 0; i < count; i++ {
		select {
		case docs[i] = <-InsertDocumentCache:
		default:
			docs[i] = workloads.GenerateDocument(col, cfg)
		}
	}
	return docs
}

func insertDocumentProducer(ctx context.Context, col config.CollectionDefinition, cacheSize int, cfg *config.AppConfig) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
			doc := workloads.GenerateDocument(col, cfg)
			select {
			case InsertDocumentCache <- doc:
			case <-ctx.Done():
				return
			}
		}
	}
}

func getCollectionHandle(db *mongo.Database, col config.CollectionDefinition) *mongo.Collection {
	return db.Client().Database(col.DatabaseName).Collection(col.Name)
}

func runTransaction(ctx context.Context, id int, wCfg workloadConfig, rng *rand.Rand) {
	session, err := wCfg.database.Client().StartSession()
	if err != nil {
		return
	}
	defer session.EndSession(ctx)
	start := time.Now()
	_, err = session.WithTransaction(ctx, func(sessCtx context.Context) (interface{}, error) {
		numOps := rng.Intn(wCfg.appConfig.MaxTransactionOps) + 1
		for i := 0; i < numOps; i++ {
			currentCol := wCfg.collections[rng.Intn(len(wCfg.collections))]
			innerOp := selectOperation(wCfg.percentages, rng)
			if innerOp == "aggregate" || innerOp == "transaction" {
				innerOp = "find"
			}
			var q config.QueryDefinition
			var insertManyDocs []interface{}
			var run bool
			switch innerOp {
			case "insert":
				q = generateInsertQuery(currentCol, rng, wCfg.appConfig)
				run = true
			case "insertMany":
				insertManyDocs = generateInsertManyQuery(currentCol, rng, wCfg.appConfig)
				run = true
			default:
				q, run = selectRandomQueryByType(sessCtx, wCfg.database, innerOp, wCfg.queryMap, currentCol, wCfg.debug, rng, wCfg.primaryFilterField, wCfg.appConfig)
			}
			if !run {
				continue
			}
			coll := getCollectionHandle(wCfg.database, currentCol)
			filter := cloneMap(q.Filter)
			processRecursive(filter, rng)
			switch innerOp {
			case "find":
				cursor, err := coll.Find(sessCtx, filter, options.Find().SetLimit(1))
				if err == nil {
					for cursor.Next(sessCtx) {
					}
					_ = cursor.Close(sessCtx)
				}
			case "updateOne":
				opts := options.UpdateOne().SetUpsert(q.Upsert)
				coll.UpdateOne(sessCtx, filter, q.Update, opts)
			case "updateMany":
				opts := options.UpdateMany().SetUpsert(q.Upsert)
				coll.UpdateMany(sessCtx, filter, q.Update, opts)
			case "deleteOne":
				coll.DeleteOne(sessCtx, filter)
			case "deleteMany":
				coll.DeleteMany(sessCtx, filter)
			case "insert":
				coll.InsertOne(sessCtx, q.Filter)
			case "insertMany":
				coll.InsertMany(sessCtx, insertManyDocs)
			}
		}
		return nil, nil
	})
	if err == nil {
		wCfg.collector.Track("transaction", time.Since(start))
	}
}

func independentWorker(ctx context.Context, id int, wg *sync.WaitGroup, wCfg workloadConfig, rng *rand.Rand) {
	defer wg.Done()
	dbOpCtx := context.Background()

	var fastBatchPool []PreBuiltBatch
	var patchKey string
	isOptimized := false

	if len(wCfg.collections) > 0 {
		col := wCfg.collections[0]
		batches, key, err := prepareGenericBatchPool(col, wCfg.appConfig, wCfg.appConfig.InsertBatchSize, 20)
		if err != nil {
			if id == 1 && wCfg.debug {
				log.Printf("Optimization unavailable for '%s': %v (Standard path)", col.Name, err)
			}
		} else {
			fastBatchPool = batches
			patchKey = key
			isOptimized = true
		}
	}

	seq := int64(id) * 100000000

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		currentCol := wCfg.collections[rng.Intn(len(wCfg.collections))]
		opType := selectOperation(wCfg.percentages, rng)

		if opType == "transaction" {
			if wCfg.appConfig.UseTransactions {
				runTransaction(ctx, id, wCfg, rng)
				continue
			}
			opType = "find"
		}

		start := time.Now()

		if isOptimized && currentCol.Name == wCfg.collections[0].Name && (opType == "insert" || opType == "insertMany") {
			batch := fastBatchPool[rng.Intn(len(fastBatchPool))]
			for _, offset := range batch.Offsets {
				binary.LittleEndian.PutUint64(batch.CmdRaw[offset:], uint64(seq))
				seq++
			}

			fastCtx, cancel := context.WithTimeout(dbOpCtx, 30*time.Second)
			err := wCfg.database.RunCommand(fastCtx, bson.Raw(batch.CmdRaw)).Err()
			cancel()

			if err != nil {
				if wCfg.debug {
					log.Printf("[Worker %d] FastInsert error: %v", id, err)
				}
				wCfg.collector.Add("insert", 1, time.Since(start))
			} else {
				wCfg.collector.Add("insert", int64(wCfg.appConfig.InsertBatchSize), time.Since(start)/time.Duration(wCfg.appConfig.InsertBatchSize))
			}
			continue
		}

		if isOptimized && currentCol.Name == wCfg.collections[0].Name && opType == "find" {
			batchSize := wCfg.appConfig.FindBatchSize
			if batchSize <= 1 {
				batchSize = 10
			}

			ids := make([]int64, batchSize)
			for i := 0; i < batchSize; i++ {
				targetW := rng.Intn(wCfg.concurrency)
				targetSeq := rng.Int63n(10000000)
				ids[i] = int64(targetW)*100000000 + targetSeq
			}

			rawIDs := buildRawInt64Array(ids)
			cmd := bson.D{
				{Key: "find", Value: currentCol.Name},
				{Key: "filter", Value: bson.D{{Key: patchKey, Value: bson.D{{Key: "$in", Value: bson.RawValue{Type: TypeArray, Value: rawIDs}}}}}},
				{Key: "limit", Value: batchSize},
			}

			fastCtx, cancel := context.WithTimeout(dbOpCtx, 30*time.Second)
			err := wCfg.database.RunCommand(fastCtx, cmd).Err()
			cancel()

			if err != nil {
				if wCfg.debug {
					log.Printf("[Worker %d] FastFind error: %v", id, err)
				}
				wCfg.collector.Add("find", 1, time.Since(start))
			} else {
				wCfg.collector.Add("find", 1, time.Since(start))
			}
			continue
		}

		var q config.QueryDefinition
		var insertManyDocs []interface{}
		var run bool

		switch opType {
		case "insert":
			q = generateInsertQuery(currentCol, rng, wCfg.appConfig)
			run = true
		case "insertMany":
			insertManyDocs = generateInsertManyQuery(currentCol, rng, wCfg.appConfig)
			run = true
		case "find", "updateOne", "updateMany", "deleteOne", "deleteMany", "aggregate":
			q, run = selectRandomQueryByType(dbOpCtx, wCfg.database, opType, wCfg.queryMap, currentCol, wCfg.debug, rng, wCfg.primaryFilterField, wCfg.appConfig)
		default:
			time.Sleep(100 * time.Microsecond)
			continue
		}

		if !run {
			continue
		}

		coll := getCollectionHandle(wCfg.database, currentCol)

		var filter map[string]interface{}
		var pipeline []interface{}

		if opType == "aggregate" {
			if cloned, ok := deepClone(q.Pipeline).([]interface{}); ok {
				pipeline = cloned
				processRecursive(pipeline, rng)
			}
		} else if opType != "insertMany" {
			filter = cloneMap(q.Filter)
			processRecursive(filter, rng)
		}

		switch opType {
		case "find":
			limit := q.Limit
			if limit <= 0 {
				limit = wCfg.findLimit
			}
			batch := wCfg.findBatchSize
			if batch <= 0 {
				batch = 10
			}
			cursor, err := coll.Find(dbOpCtx, filter,
				options.Find().SetLimit(limit),
				options.Find().SetBatchSize(batch),
				options.Find().SetProjection(q.Projection),
			)
			if err == nil {
				for cursor.Next(dbOpCtx) {
				}
				_ = cursor.Close(dbOpCtx)
			}
		case "aggregate":
			cursor, err := coll.Aggregate(dbOpCtx, pipeline)
			if err == nil {
				for cursor.Next(dbOpCtx) {
				}
				_ = cursor.Close(dbOpCtx)
			}
		case "updateOne":
			opts := options.UpdateOne().SetUpsert(q.Upsert)
			coll.UpdateOne(dbOpCtx, filter, q.Update, opts)
		case "updateMany":
			opts := options.UpdateMany().SetUpsert(q.Upsert)
			coll.UpdateMany(dbOpCtx, filter, q.Update, opts)
		case "deleteOne":
			coll.DeleteOne(dbOpCtx, filter)
		case "deleteMany":
			coll.DeleteMany(dbOpCtx, filter)
		case "insert":
			coll.InsertOne(dbOpCtx, q.Filter)
		case "insertMany":
			coll.InsertMany(dbOpCtx, insertManyDocs)
		}

		wCfg.collector.Track(opType, time.Since(start))
	}
}

func deepClone(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		m := make(map[string]interface{}, len(t))
		for k, val := range t {
			m[k] = deepClone(val)
		}
		return m
	case []interface{}:
		s := make([]interface{}, len(t))
		for i, val := range t {
			s[i] = deepClone(val)
		}
		return s
	default:
		return t
	}
}

func cloneMap(m map[string]interface{}) map[string]interface{} {
	if res, ok := deepClone(m).(map[string]interface{}); ok {
		return res
	}
	return nil
}

func processRecursive(v interface{}, rng *rand.Rand) {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, val := range t {
			if s, ok := val.(string); ok {
				if s == "<int>" {
					t[k] = rng.Intn(1000)
				} else if s == "<string>" {
					t[k] = fmt.Sprintf("val-%d", rng.Intn(1000))
				}
			} else {
				processRecursive(val, rng)
			}
		}
	case []interface{}:
		for _, val := range t {
			processRecursive(val, rng)
		}
	}
}

func RunWorkload(ctx context.Context, db *mongo.Database, collections []config.CollectionDefinition, queries []config.QueryDefinition, cfg *config.AppConfig, uiCollector ...*stats.Collector) error {
	duration, err := time.ParseDuration(cfg.Duration)
	if err != nil {
		return err
	}

	for _, col := range collections {
		if err := ensureGenericSharding(ctx, db, col, cfg); err != nil {
			log.Printf("Sharding setup warning for %s: %v", col.Name, err)
		}
	}

	var collector *stats.Collector
	if len(uiCollector) > 0 && uiCollector[0] != nil {
		collector = uiCollector[0]
	} else {
		collector = stats.NewCollector()
	}

	if duration <= 0 {
		return runAllQueriesOnce(ctx, db, queries, cfg.DebugMode)
	}

	findBatch := int32(cfg.FindBatchSize)
	if findBatch <= 0 {
		findBatch = 10
	}
	findLimit := int64(cfg.FindLimit)
	if findLimit <= 0 {
		findLimit = 10
	}

	qMap := make(map[string][]config.QueryDefinition)
	for _, q := range queries {
		qMap[q.Operation] = append(qMap[q.Operation], q)
	}

	cachedFilterField := getPrimaryFilterField(ctx, db, collections[0])

	wCfg := workloadConfig{
		database:    db,
		appConfig:   cfg,
		concurrency: cfg.Concurrency,
		duration:    duration,
		collections: collections,
		queryMap:    qMap,
		percentages: map[string]int{
			"find":        cfg.FindPercent,
			"update":      cfg.UpdatePercent,
			"delete":      cfg.DeletePercent,
			"insert":      cfg.InsertPercent,
			"insertMany":  cfg.BulkInsertPercent,
			"aggregate":   cfg.AggregatePercent,
			"transaction": cfg.TransactionPercent,
		},
		debug:              cfg.DebugMode,
		findBatchSize:      findBatch,
		findLimit:          findLimit,
		maxInsertCache:     cfg.InsertCacheSize,
		primaryFilterField: cachedFilterField,
		collector:          collector,
	}

	return runContinuousWorkload(ctx, wCfg)
}

func runContinuousWorkload(ctx context.Context, wCfg workloadConfig) error {
	InsertDocumentCache = make(chan map[string]interface{}, wCfg.maxInsertCache)
	workloadCtx, cancel := context.WithTimeout(ctx, wCfg.duration)
	defer cancel()

	for _, col := range wCfg.collections {
		go insertDocumentProducer(workloadCtx, col, wCfg.maxInsertCache, wCfg.appConfig)
	}

	monitorDone := make(chan struct{})
	go func() {
		wCfg.collector.Monitor(monitorDone, wCfg.appConfig.StatusRefreshRateSec, wCfg.concurrency, wCfg.appConfig.CSVExportEnabled, wCfg.appConfig.CSVExportAppend, wCfg.appConfig.CSVExportPath, wCfg.appConfig.WebUI.Enabled)
	}()

	var wg sync.WaitGroup
	for i := 1; i <= wCfg.concurrency; i++ {
		wg.Add(1)
		rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(i)))
		go independentWorker(workloadCtx, i, &wg, wCfg, rng)
	}

	<-workloadCtx.Done()
	wg.Wait()
	close(monitorDone)
	wCfg.collector.PrintFinalSummary(wCfg.duration, wCfg.appConfig.WebUI.Enabled)
	return nil
}

func runAllQueriesOnce(ctx context.Context, db *mongo.Database, queries []config.QueryDefinition, debug bool) error {
	if len(queries) == 0 {
		return nil
	}
	tasks := make(chan *queryTask, len(queries))
	var wg sync.WaitGroup
	wg.Add(1)
	go queryWorkerOnce(ctx, 1, tasks, &wg)
	for i, q := range queries {
		if q.Operation == "insert" || q.Operation == "insertMany" {
			continue
		}
		tasks <- &queryTask{definition: q, database: db, runID: int64(i + 1), debug: debug, rng: rand.New(rand.NewSource(time.Now().UnixNano()))}
	}
	close(tasks)
	wg.Wait()
	return nil
}

func queryWorkerOnce(ctx context.Context, id int, tasks <-chan *queryTask, wg *sync.WaitGroup) {
	defer wg.Done()
	dbOpCtx := context.Background()
	for task := range tasks {
		q := task.definition
		coll := task.database.Client().Database(q.Database).Collection(q.Collection)
		if q.Database == "" {
			coll = task.database.Collection(q.Collection)
		}
		filter := cloneMap(q.Filter)
		processRecursive(filter, task.rng)
		switch q.Operation {
		case "find":
			cursor, _ := coll.Find(dbOpCtx, filter)
			if cursor != nil {
				cursor.Close(dbOpCtx)
			}
		case "updateOne":
			coll.UpdateOne(dbOpCtx, filter, q.Update)
		}
	}
}

func buildOrderedShardKey(col config.CollectionDefinition, shardKeyMap map[string]interface{}, isHashed bool) bson.D {
	keys := make([]string, 0, len(shardKeyMap))
	for k := range shardKeyMap {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	var cmd bson.D
	for _, k := range keys {
		if isHashed {
			cmd = append(cmd, bson.E{Key: k, Value: "hashed"})
		} else {
			cmd = append(cmd, bson.E{Key: k, Value: 1})
		}
	}
	return cmd
}

// Check if a collection is already sharded and return its chunk count.
func checkShardingStatus(ctx context.Context, db *mongo.Database, collectionName string) (bool, int64, error) {
	res := db.RunCommand(ctx, bson.D{{Key: "collStats", Value: collectionName}})
	if res.Err() != nil {
		// If collection doesn't exist, collStats fails -> not sharded
		return false, 0, nil
	}

	var stats struct {
		Sharded bool  `bson:"sharded"`
		NChunks int64 `bson:"nchunks"`
	}
	if err := res.Decode(&stats); err != nil {
		return false, 0, err
	}

	if !stats.Sharded {
		return false, 0, nil
	}

	// If collStats returned nchunks use it
	if stats.NChunks > 0 {
		return true, stats.NChunks, nil
	}

	// Fallback: If collStats is silent on chunks (rare in sharded=true), assume at least 1
	return true, 1, nil
}

func ensureGenericSharding(ctx context.Context, db *mongo.Database, col config.CollectionDefinition, cfg *config.AppConfig) error {
	admin := db.Client().Database("admin")

	// 1. Check if Cluster
	var hello bson.M
	if err := admin.RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello); err != nil {
		return nil
	}
	if hello["msg"] != "isdbgrid" {
		return nil
	}

	dbName := db.Name()
	ns := fmt.Sprintf("%s.%s", dbName, col.Name)

	// 2. Is it sharded? Do chunks exist?
	isSharded, chunkCount, _ := checkShardingStatus(ctx, db, col.Name)

	// If it is already sharded AND has more than 1 chunk, assume setup is done.
	if !cfg.DropCollections && isSharded && chunkCount > 1 {
		return nil
	}

	patchKey := identifyPatchKey(col)
	if patchKey == "" {
		return nil
	}

	isHashed := false
	if col.ShardConfig != nil {
		for _, v := range col.ShardConfig.Key {
			if vStr, ok := v.(string); ok && vStr == "hashed" {
				isHashed = true
			}
			break
		}
	}

	shardKeyDoc := buildOrderedShardKey(col, col.ShardConfig.Key, isHashed)

	// 4. Enable Sharding on DB
	_ = admin.RunCommand(ctx, bson.D{{Key: "enableSharding", Value: dbName}}).Err()

	// 5. Shard Collection (if not already)
	if !isSharded {
		cmd := bson.D{
			{Key: "shardCollection", Value: ns},
			{Key: "key", Value: shardKeyDoc},
		}
		if err := admin.RunCommand(ctx, cmd).Err(); err != nil {
			// If it failed because "already sharded" (race condition), just log and continue to splits
			if !strings.Contains(err.Error(), "already sharded") &&
				!strings.Contains(err.Error(), "AlreadyInitialized") &&
				!strings.Contains(err.Error(), "already exists") {
				log.Printf("[Sharding] Warning: Failed to shard %s: %v", ns, err)
			}
		}
	}

	// 6. Pre-Split (ONLY for Range Sharding)
	if isHashed {
		return nil
	}

	shards, err := listShards(ctx, admin)
	if err != nil || len(shards) == 0 {
		return fmt.Errorf("failed to list shards: %v", err)
	}

	log.Printf("[Sharding] Initializing splits for '%s' (Range) on key '%s' for %d workers...", col.Name, patchKey, cfg.Concurrency)

	// Helper to build the compound key for split/move.
	buildCompoundKey := func(boundary int64) bson.D {
		var doc bson.D
		for _, elem := range shardKeyDoc {
			if elem.Key == patchKey {
				doc = append(doc, bson.E{Key: elem.Key, Value: boundary})
			} else {
				doc = append(doc, bson.E{Key: elem.Key, Value: 0})
			}
		}
		return doc
	}

	// Step 6a: Create all split points.
	// Ensure MinKey -> 0 split exists
	splitStart := bson.D{{Key: "split", Value: ns}, {Key: "middle", Value: buildCompoundKey(0)}}
	_ = admin.RunCommand(ctx, splitStart).Err()

	// Create splits for every worker boundary
	for w := 1; w <= cfg.Concurrency; w++ {
		boundary := int64(w) * 100000000
		splitCmd := bson.D{{Key: "split", Value: ns}, {Key: "middle", Value: buildCompoundKey(boundary)}}
		if err := admin.RunCommand(ctx, splitCmd).Err(); err != nil {
			// Tolerate "chunks already exist" or similar errors during this phase
			if !strings.Contains(err.Error(), "already exists") {
				log.Printf("[Sharding] Split attempt at %d: %v", boundary, err)
			}
		}
	}

	// Allow metadata to propagate briefly
	time.Sleep(1 * time.Second)

	// Step 6b: Move the specific chunks that workers will actually use.
	for w := 1; w <= cfg.Concurrency; w++ {
		chunkStartProbe := int64(w) * 100000000
		targetShard := shards[(w-1)%len(shards)]

		moveCmd := bson.D{
			{Key: "moveChunk", Value: ns},
			{Key: "find", Value: buildCompoundKey(chunkStartProbe)},
			{Key: "to", Value: targetShard},
		}

		if err := admin.RunCommand(ctx, moveCmd).Err(); err != nil {
			if !strings.Contains(err.Error(), "already on shard") {
				log.Printf("[Sharding] MoveChunk error for %d -> %s: %v", chunkStartProbe, targetShard, err)
			}
		}
	}
	return nil
}

func listShards(ctx context.Context, admin *mongo.Database) ([]string, error) {
	var result struct {
		Shards []struct {
			ID string `bson:"_id"`
		} `bson:"shards"`
	}
	if err := admin.RunCommand(ctx, bson.D{{Key: "listShards", Value: 1}}).Decode(&result); err != nil {
		return nil, err
	}
	names := make([]string, len(result.Shards))
	for i, s := range result.Shards {
		names[i] = s.ID
	}
	return names, nil
}
