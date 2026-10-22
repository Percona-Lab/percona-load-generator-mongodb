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
	"sync/atomic"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/accesspattern"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/datagen"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/loadprofile"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/workloadrec"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/workloads"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
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
	recordPool         *RecordPool

	// schedule shapes how many workers are active over the run. activeTarget and
	// activeCount are shared atomics: the profile controller publishes the target
	// to activeTarget; workers maintain activeCount as they park/un-park. They are
	// pointers so the by-value workloadConfig copies handed to workers share state.
	schedule     loadprofile.Schedule
	activeTarget *int64
	activeCount  *int64

	// thinkTime/thinkJitter pace each worker between operations to emulate
	// realistic clients. Zero means no pacing. Pacing happens outside the
	// measured latency window.
	thinkTime   time.Duration
	thinkJitter time.Duration

	// recorder appends every sent operation to a workload recording; nil
	// when recording is off.
	recorder *workloadrec.Recorder

	// det is set for deterministic generation (workload_deterministic).
	det *deterministicRun

	// outcomes makes the execute functions capture each operation's
	// outcome (workload_record_outcomes), with result checksums when
	// checksums is set; outcomeFile receives them in replay and
	// deterministic runs.
	outcomes    bool
	checksums   bool
	outcomeFile *workloadrec.OutcomeWriter
}

// applyPacing pauses a worker between operations by thinkTime plus a uniform
// random 0..jitter, returning early if the context is cancelled. It is a no-op
// when both durations are zero so fixed-rate runs are unaffected.
func applyPacing(ctx context.Context, rng *rand.Rand, thinkTime, jitter time.Duration) {
	if thinkTime <= 0 && jitter <= 0 {
		return
	}
	d := thinkTime
	if jitter > 0 && rng != nil {
		d += time.Duration(rng.Int63n(int64(jitter) + 1))
	}
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

var InsertDocumentCache chan map[string]interface{}

var operationTypes = []string{"find", "update", "delete", "insert", "insertMany", "aggregate", "transaction"}

var (
	ensureGenericShardingFn = ensureGenericSharding
	runAllQueriesOnceFn     = runAllQueriesOnce
	runContinuousWorkloadFn = runContinuousWorkload
	runWorkloadReplayFn     = runWorkloadReplay
	getPrimaryFilterFieldFn = getPrimaryFilterField
	executeQueryOnceFn      = executeQueryOnce
)

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
		return col.ShardConfig.Key[0].Field
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
	gen := func() map[string]interface{} { return workloads.GenerateDocument(col, cfg) }
	marshal := func(m map[string]interface{}) ([]byte, error) { return bson.Marshal(m) }
	return prepareGenericBatchPoolWith(col, batchSize, poolSize, gen, marshal)
}

// prepareGenericBatchPoolWith builds the prebuilt insert batches from the
// documents gen returns, encoded by marshal.
func prepareGenericBatchPoolWith(col config.CollectionDefinition, batchSize int, poolSize int, gen func() map[string]interface{}, marshal func(map[string]interface{}) ([]byte, error)) ([]PreBuiltBatch, string, error) {
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
			docMap := gen()
			docMap[patchKey] = magic
			rawDoc, err := marshal(docMap)
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
	return generateFallbackQueryWith(opType, col, rng, filterField, cfg, workloads.GenerateFallbackUpdate)
}

// fallbackUpdateFn generates the update of a fallback update query.
type fallbackUpdateFn func(col config.CollectionDefinition, cfg *config.AppConfig, rng *rand.Rand) map[string]interface{}

func generateFallbackQueryWith(opType string, col config.CollectionDefinition, rng *rand.Rand, filterField string, cfg *config.AppConfig, genUpdate fallbackUpdateFn) (config.QueryDefinition, bool) {
	collectionName := col.Name
	fieldType := "int"
	if filterField == "_id" {
		fieldType = "string"
	}
	if def, ok := col.Fields[filterField]; ok {
		fieldType = def.Type
	}
	filter := map[string]interface{}{filterField: fmt.Sprintf("<%s>", fieldType)}
	base := config.QueryDefinition{
		Collection:      collectionName,
		Operation:       opType,
		SourceType:      "generated_fallback",
		SourceFile:      "runtime_generated",
		WorkloadName:    "generated_workload",
		DefinitionIndex: -1,
		Label:           fmt.Sprintf("fallback %s query on %s", opType, filterField),
	}
	if opType == "updateOne" || opType == "updateMany" {
		updatePayload := genUpdate(col, cfg, rng)
		base.Filter = filter
		base.Update = updatePayload
		return base, true
	}
	if opType == "deleteOne" || opType == "deleteMany" {
		base.Filter = filter
		return base, true
	}
	return config.QueryDefinition{}, false
}

func selectRandomQueryByType(ctx context.Context, db *mongo.Database, opType string, queryMap map[string][]config.QueryDefinition, col config.CollectionDefinition, debug bool, rng *rand.Rand, filterField string, cfg *config.AppConfig) (config.QueryDefinition, bool) {
	return selectQueryByTypeWith(opType, queryMap, col, rng, filterField, cfg, workloads.GenerateFallbackUpdate)
}

func selectQueryByTypeWith(opType string, queryMap map[string][]config.QueryDefinition, col config.CollectionDefinition, rng *rand.Rand, filterField string, cfg *config.AppConfig, genUpdate fallbackUpdateFn) (config.QueryDefinition, bool) {
	candidates, ok := queryMap[opType]
	if !ok || len(candidates) == 0 {
		if opType == "find" || opType == "updateOne" || opType == "updateMany" || opType == "deleteOne" || opType == "deleteMany" {
			return generateFallbackQueryWith(opType, col, rng, filterField, cfg, genUpdate)
		}
		return config.QueryDefinition{}, false
	}

	matched := make([]config.QueryDefinition, 0, len(candidates))
	for _, q := range candidates {
		if queryMatchesCollection(q, col) {
			matched = append(matched, q)
		}
	}
	if len(matched) == 0 {
		if opType == "find" || opType == "updateOne" || opType == "updateMany" || opType == "deleteOne" || opType == "deleteMany" {
			return generateFallbackQueryWith(opType, col, rng, filterField, cfg, genUpdate)
		}
		return config.QueryDefinition{}, false
	}
	return matched[rng.Intn(len(matched))], true
}

func queryMatchesCollection(q config.QueryDefinition, col config.CollectionDefinition) bool {
	if !strings.EqualFold(strings.TrimSpace(q.Collection), strings.TrimSpace(col.Name)) {
		return false
	}
	if strings.TrimSpace(q.Database) == "" {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(q.Database), strings.TrimSpace(col.DatabaseName))
}

func generateInsertQuery(col config.CollectionDefinition, rng *rand.Rand, cfg *config.AppConfig) config.QueryDefinition {
	var doc map[string]interface{}
	select {
	case doc = <-InsertDocumentCache:
	default:
		doc = workloads.GenerateDocument(col, cfg)
	}
	return config.QueryDefinition{
		Collection:      col.Name,
		Operation:       "insert",
		Filter:          doc,
		SourceType:      "generated_insert",
		SourceFile:      "runtime_generated",
		WorkloadName:    "generated_workload",
		DefinitionIndex: -1,
		Label:           "generated insert document",
	}
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

// runTransaction runs one transaction. With replayOps it sends those
// operations on every attempt; otherwise it generates them (again on every
// retry of the callback). A recording keeps the operations of the last
// attempt. A non-nil out receives whether it committed or aborted.
func runTransaction(ctx context.Context, id int, wCfg workloadConfig, rng *rand.Rand, replayOps []txInnerOp, out *workloadrec.Outcome) bool {
	session, err := wCfg.database.Client().StartSession()
	if err != nil {
		return false
	}
	defer session.EndSession(ctx)
	recording := wCfg.recorder.Active()
	recorded := bson.A{}
	if recording {
		for i := range replayOps {
			recorded = append(recorded, txInnerCommand(wCfg.database, &replayOps[i]))
		}
	}
	start := time.Now()
	_, err = session.WithTransaction(ctx, func(sessCtx context.Context) (interface{}, error) {
		if replayOps != nil {
			for i := range replayOps {
				executeTxInnerOp(sessCtx, wCfg, &replayOps[i])
			}
			return nil, nil
		}
		recorded = recorded[:0]
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
			filter := cloneMap(q.Filter)
			ns := collectionNamespace(wCfg.database, currentCol)
			processRecursiveWithPool(filter, rng, wCfg.recordPool, ns, wCfg.appConfig.ExistingRecordHitRate)
			op := txInnerOp{opType: innerOp, col: currentCol, filter: filter, update: q.Update, upsert: q.Upsert, filterMap: filter}
			switch innerOp {
			case "insert":
				op.doc, op.poolDoc = q.Filter, q.Filter
				if recording {
					op.doc = withDocumentID(q.Filter)
				}
			case "insertMany":
				op.docs, op.poolDocs = insertManyDocs, insertManyDocs
				if recording {
					op.docs = withDocumentIDs(insertManyDocs)
				}
			}
			executeTxInnerOp(sessCtx, wCfg, &op)
			if recording {
				recorded = append(recorded, txInnerCommand(wCfg.database, &op))
			}
		}
		return nil, nil
	})
	dur := time.Since(start)
	if out != nil {
		transactionOutcome(out, dur, err)
	}
	if recording {
		recordOp(wCfg, workloadrec.Entry{Worker: id, NS: wCfg.database.Name() + ".(multiple)", Op: "transaction", OK: err == nil, Command: bson.D{{Key: "ops", Value: recorded}}, Outcome: out}, start)
	}
	if err == nil {
		wCfg.collector.Track("transaction", dur)
		wCfg.collector.RecordOperationEvent("transaction", wCfg.database.Name(), "(multiple)", transactionShapeKey, "transaction batch", nil, dur, true, wCfg.collector.CurrentIteration, nil, "transaction batch", "runtime_transaction", "transaction_workload", "runtime", "transaction_batch", "", "", -1)
	}
	return err == nil
}

// appendPooledIntIDs appends up to want-len(ids) integer patchKey values drawn
// from the pool. Draws are capped because pooled records may lack an integer
// patchKey, and skewed selectors keep returning the same few records.
func appendPooledIntIDs(ctx context.Context, ids []int64, pool *RecordPool, ns, patchKey string, want int, rng *rand.Rand) []int64 {
	maxDraws := 4 * want
	for draws := 0; len(ids) < want && draws < maxDraws && ctx.Err() == nil; draws++ {
		rec, ok := pool.Random(ns, rng)
		if !ok {
			break
		}
		switch v := rec[patchKey].(type) {
		case int:
			ids = append(ids, int64(v))
		case int32:
			ids = append(ids, int64(v))
		case int64:
			ids = append(ids, v)
		}
	}
	return ids
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

	// parked tracks whether this worker is currently counted as active so the
	// shared activeCount gauge stays balanced as the load profile scales.
	parked := true
	defer func() {
		if !parked && wCfg.activeCount != nil {
			atomic.AddInt64(wCfg.activeCount, -1)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Load-profile gating: workers with id within the current target run;
		// the rest park briefly and re-check, so concurrency tracks the schedule.
		if wCfg.activeTarget != nil {
			shouldRun := int64(id) <= atomic.LoadInt64(wCfg.activeTarget)
			if shouldRun && parked {
				if wCfg.activeCount != nil {
					atomic.AddInt64(wCfg.activeCount, 1)
				}
				parked = false
			} else if !shouldRun {
				if !parked {
					if wCfg.activeCount != nil {
						atomic.AddInt64(wCfg.activeCount, -1)
					}
					parked = true
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(25 * time.Millisecond):
				}
				continue
			}
		}

		// Optional client pacing between operations (excluded from latency).
		applyPacing(ctx, rng, wCfg.thinkTime, wCfg.thinkJitter)

		currentCol := wCfg.collections[rng.Intn(len(wCfg.collections))]
		opType := selectOperation(wCfg.percentages, rng)

		if opType == "transaction" {
			if wCfg.appConfig.UseTransactions {
				var out *workloadrec.Outcome
				if wCfg.recorder.Active() {
					out = wCfg.newOutcome()
				}
				runTransaction(ctx, id, wCfg, rng, nil, out)
				continue
			}
			opType = "find"
		}

		start := time.Now()
		recording := wCfg.recorder.Active()
		var out *workloadrec.Outcome
		if recording {
			out = wCfg.newOutcome()
		}

		if isOptimized && currentCol.Name == wCfg.collections[0].Name && (opType == "insert" || opType == "insertMany") {
			batch := fastBatchPool[rng.Intn(len(fastBatchPool))]
			for _, offset := range batch.Offsets {
				binary.LittleEndian.PutUint64(batch.CmdRaw[offset:], uint64(seq))
				seq++
			}
			sent := batch.CmdRaw
			if recording && patchKey != "_id" && !hasField(currentCol, "_id") {
				if withIDs, err := withBatchIDs(batch.CmdRaw); err == nil {
					sent = withIDs
				} else {
					wCfg.recorder.Fail("encode_error: " + err.Error())
					recording = false
				}
			}
			ok := executeFastInsert(dbOpCtx, wCfg, id, currentCol, sent, wCfg.appConfig.InsertBatchSize, start, func() {
				registerFastBatchIDs(wCfg.recordPool, wCfg.database, currentCol, patchKey, batch.CmdRaw, batch.Offsets)
			}, out)
			if recording {
				recordOp(wCfg, workloadrec.Entry{Worker: id, NS: collectionNamespace(wCfg.database, currentCol), Op: "insertMany", Path: workloadrec.PathFastInsertBatch, OK: ok, Command: bson.Raw(sent), Outcome: out}, start)
			}
			continue
		}

		if isOptimized && currentCol.Name == wCfg.collections[0].Name && opType == "find" {
			batchSize := wCfg.appConfig.FindBatchSize
			if batchSize <= 1 {
				batchSize = 10
			}

			ns := collectionNamespace(wCfg.database, currentCol)
			ids := make([]int64, 0, batchSize)
			if wCfg.recordPool != nil && wCfg.recordPool.Len(ns) > 0 && rng.Intn(100) < wCfg.appConfig.ExistingRecordHitRate {
				ids = appendPooledIntIDs(ctx, ids, wCfg.recordPool, ns, patchKey, batchSize, rng)
			}
			pooledIDs := len(ids)
			for len(ids) < batchSize {
				targetW := rng.Intn(wCfg.concurrency)
				targetSeq := rng.Int63n(10000000)
				ids = append(ids, int64(targetW)*100000000+targetSeq)
			}
			ok := executeFastFind(dbOpCtx, wCfg, id, currentCol, patchKey, ids, batchSize, pooledIDs > 0, start, out)
			if recording {
				recordOp(wCfg, workloadrec.Entry{Worker: id, NS: ns, Op: "find", Path: workloadrec.PathFastFind, OK: ok, Existing: pooledIDs > 0, Command: fastFindRecord(currentCol, patchKey, ids, batchSize), Outcome: out}, start)
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

		ns := collectionNamespace(wCfg.database, currentCol)
		hitRate := wCfg.appConfig.ExistingRecordHitRate

		var filter map[string]interface{}
		var pipeline []interface{}
		usedExisting := false

		if opType == "aggregate" {
			if cloned, ok := deepClone(q.Pipeline).([]interface{}); ok {
				pipeline = cloned
				usedExisting = processRecursiveWithPool(pipeline, rng, wCfg.recordPool, ns, hitRate)
			}
		} else if opType != "insertMany" {
			filter = cloneMap(q.Filter)
			usedExisting = processRecursiveWithPool(filter, rng, wCfg.recordPool, ns, hitRate)
		}

		findLimit := q.Limit
		if findLimit <= 0 {
			findLimit = wCfg.findLimit
		}
		batch := wCfg.findBatchSize
		if batch <= 0 {
			batch = 10
		}
		req := opRequest{
			opType: opType, col: currentCol, filter: filter, pipeline: pipeline, update: q.Update, projection: q.Projection,
			upsert: q.Upsert, findLimit: findLimit, findBatch: batch, usedExisting: usedExisting,
			q: q, filterMap: filter, pipelineIn: pipeline,
		}
		switch opType {
		case "insert":
			req.doc, req.poolDoc = q.Filter, q.Filter
			if recording {
				req.doc = withDocumentID(q.Filter)
			}
		case "insertMany":
			req.docs, req.poolDocs = insertManyDocs, insertManyDocs
			if recording {
				req.docs = withDocumentIDs(insertManyDocs)
			}
		}
		ok, opID := executeStandard(dbOpCtx, wCfg, &req, start, out)
		if recording {
			recordOp(wCfg, workloadrec.Entry{Worker: id, NS: ns, Op: opType, OK: ok, Existing: usedExisting, Query: recordQueryRef(q, opID), Shape: recordShapeRef(opID), Command: standardCommand(&req), Outcome: out}, start)
		}
	}
}

func hasField(col config.CollectionDefinition, name string) bool {
	_, ok := col.Fields[name]
	return ok
}

// buildExecutableQuery returns the query exactly as the worker sent it, for
// post-run explain. It returns nil for operations explain does not cover.
func buildExecutableQuery(opType, database string, q config.QueryDefinition, filter map[string]interface{}, pipeline []interface{}, findLimit int64) *stats.ExecutableQuery {
	switch opType {
	case "find":
		return &stats.ExecutableQuery{Database: database, Filter: filter, Projection: q.Projection, Limit: findLimit}
	case "updateOne", "updateMany":
		return &stats.ExecutableQuery{Database: database, Filter: filter, Update: q.Update, Upsert: q.Upsert}
	case "deleteOne", "deleteMany":
		return &stats.ExecutableQuery{Database: database, Filter: filter}
	case "aggregate":
		return &stats.ExecutableQuery{Database: database, Pipeline: pipeline}
	default:
		return nil
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
	case bson.D:
		d := make(bson.D, len(t))
		for i, e := range t {
			d[i] = bson.E{Key: e.Key, Value: deepClone(e.Value)}
		}
		return d
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

func sortUniqueStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func renderShape(v interface{}) string {
	switch t := v.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s:%s", k, renderShape(t[k])))
		}
		return "{" + strings.Join(parts, ",") + "}"
	case bson.D:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			parts = append(parts, fmt.Sprintf("%s:%s", e.Key, renderShape(e.Value)))
		}
		return "{" + strings.Join(parts, ",") + "}"
	case []interface{}:
		parts := make([]string, 0, len(t))
		for _, item := range t {
			parts = append(parts, renderShape(item))
		}
		return "[" + strings.Join(parts, ",") + "]"
	case string:
		switch t {
		case "<int>", "<string>":
			return t
		default:
			return "<str>"
		}
	case int, int32, int64, uint, uint32, uint64, float32, float64:
		return "<num>"
	case bool:
		return "<bool>"
	case nil:
		return "<nil>"
	default:
		return "<val>"
	}
}

// renderPipelineShape renders a pipeline for shape identity. $match literals are
// redacted like filters; $sort/$limit/$skip/$sample/$project values and
// "$field" paths in other stages are structural and kept, so pipelines that
// differ in those semantics get different shapes.
func renderPipelineShape(pipeline []interface{}) string {
	parts := make([]string, 0, len(pipeline))
	for _, raw := range pipeline {
		stage, ok := raw.(map[string]interface{})
		if !ok {
			parts = append(parts, renderShape(raw))
			continue
		}
		keys := make([]string, 0, len(stage))
		for k := range stage {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fields := make([]string, 0, len(keys))
		for _, k := range keys {
			fields = append(fields, k+":"+renderStageShape(k, stage[k]))
		}
		parts = append(parts, "{"+strings.Join(fields, ",")+"}")
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func renderStageShape(name string, v interface{}) string {
	switch name {
	case "$match":
		return renderShape(v)
	case "$sort", "$limit", "$skip", "$sample", "$project":
		return renderSpecShape(v)
	case "$facet":
		if m, ok := v.(map[string]interface{}); ok {
			keys := make([]string, 0, len(m))
			for k := range m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			parts := make([]string, 0, len(keys))
			for _, k := range keys {
				if sub, ok := m[k].([]interface{}); ok {
					parts = append(parts, k+":"+renderPipelineShape(sub))
				} else {
					parts = append(parts, k+":"+renderExprShape(m[k]))
				}
			}
			return "{" + strings.Join(parts, ",") + "}"
		}
	}
	return renderOwnedExprShape(v, name)
}

// renderSpecShape keeps direct numeric/boolean spec values (sort direction,
// projection inclusion, limit/skip/sample size); nested expressions go through
// renderExprShape so their literals stay redacted.
func renderSpecShape(v interface{}) string {
	switch t := v.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+":"+renderSpecValue(t[k]))
		}
		return "{" + strings.Join(parts, ",") + "}"
	case bson.D:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			parts = append(parts, e.Key+":"+renderSpecValue(e.Value))
		}
		return "{" + strings.Join(parts, ",") + "}"
	case []interface{}:
		return renderExprShape(t)
	default:
		return renderSpecScalar(t)
	}
}

func renderSpecValue(v interface{}) string {
	switch v.(type) {
	case map[string]interface{}, []interface{}, bson.D:
		return renderExprShape(v)
	}
	return renderSpecScalar(v)
}

func renderSpecScalar(v interface{}) string {
	switch t := v.(type) {
	case bool:
		return strconv.FormatBool(t)
	case int:
		return strconv.Itoa(t)
	case int32:
		return strconv.FormatInt(int64(t), 10)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case string:
		if strings.HasPrefix(t, "$") {
			return t
		}
	}
	return renderShape(v)
}

func renderExprShape(v interface{}) string {
	return renderOwnedExprShape(v, "")
}

// renderOwnedExprShape renders an expression whose enclosing key is owner, so
// a $sortArray sortBy direction is kept like other sort specifications.
func renderOwnedExprShape(v interface{}, owner string) string {
	switch t := v.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+":"+renderExprField(owner, k, t[k]))
		}
		return "{" + strings.Join(parts, ",") + "}"
	case bson.D:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			parts = append(parts, e.Key+":"+renderExprField(owner, e.Key, e.Value))
		}
		return "{" + strings.Join(parts, ",") + "}"
	case []interface{}:
		parts := make([]string, 0, len(t))
		for _, item := range t {
			parts = append(parts, renderExprShape(item))
		}
		return "[" + strings.Join(parts, ",") + "]"
	case string:
		if strings.HasPrefix(t, "$") {
			return t
		}
	}
	return renderShape(v)
}

func renderExprField(owner, k string, v interface{}) string {
	switch k {
	case "$literal":
		return renderShape(v)
	case "pipeline":
		if sub, ok := v.([]interface{}); ok {
			return renderPipelineShape(sub)
		}
	case "sortBy":
		if _, ok := v.(bson.D); ok || owner == "$sortArray" {
			return renderSpecShape(v)
		}
	}
	return renderOwnedExprShape(v, k)
}

func collectFilterFields(v interface{}, prefix string, out *[]string) {
	switch t := v.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			next := k
			if prefix != "" {
				next = prefix + "." + k
			}
			if strings.HasPrefix(k, "$") {
				collectFilterFields(t[k], prefix, out)
				continue
			}
			*out = append(*out, next)
			collectFilterFields(t[k], next, out)
		}
	case []interface{}:
		for _, item := range t {
			collectFilterFields(item, prefix, out)
		}
	}
}

func collectPipelineFields(pipeline []interface{}) []string {
	fields := make([]string, 0, 8)
	for _, stageRaw := range pipeline {
		stage, ok := stageRaw.(map[string]interface{})
		if !ok {
			continue
		}
		if matchRaw, ok := stage["$match"]; ok {
			collectFilterFields(matchRaw, "", &fields)
		}
	}
	return sortUniqueStrings(fields)
}

// shapeOptions carries the executed options that change query semantics and
// therefore belong in the shape identity (stats.ShapeIdentityVersion).
type shapeOptions struct {
	projection map[string]interface{}
	limit      int64
	upsert     bool
}

// identitySuffix appends the options that were actually sent for opType.
func (o shapeOptions) identitySuffix(opType string) string {
	var b strings.Builder
	switch opType {
	case "find":
		if len(o.projection) > 0 {
			b.WriteString("|projection:")
			b.WriteString(renderSpecShape(o.projection))
		}
		if o.limit > 0 {
			fmt.Fprintf(&b, "|limit:%d", o.limit)
		}
	case "updateOne", "updateMany":
		if o.upsert {
			b.WriteString("|upsert")
		}
	}
	return b.String()
}

func buildOperationShape(opType string, filter map[string]interface{}, pipeline []interface{}, opts shapeOptions) (string, string, []string) {
	const maxShapeLen = 512
	trim := func(v string) string {
		if len(v) <= maxShapeLen {
			return v
		}
		return v[:maxShapeLen] + "...(truncated)"
	}

	switch opType {
	case "find", "updateOne", "updateMany", "deleteOne", "deleteMany":
		// The suffix stays outside truncation so long filters keep their options.
		suffix := trim(opts.identitySuffix(opType))
		if len(filter) == 0 {
			return opType + "|{}" + suffix, opType + " empty filter", nil
		}
		fields := make([]string, 0, 8)
		collectFilterFields(filter, "", &fields)
		fields = sortUniqueStrings(fields)
		shape := trim(renderShape(filter))
		return trim(opType+"|"+shape) + suffix, trim(fmt.Sprintf("%s on fields: %s", opType, strings.Join(fields, ", "))), fields
	case "aggregate":
		if len(pipeline) == 0 {
			return "aggregate|[]", "aggregate empty pipeline", nil
		}
		fields := collectPipelineFields(pipeline)
		shape := trim(renderPipelineShape(pipeline))
		return trim("aggregate|" + shape), trim(fmt.Sprintf("aggregate match fields: %s", strings.Join(fields, ", "))), fields
	case "insert":
		return "insert|generated_doc", "insert generated document", nil
	case "insertMany":
		return "insertMany|generated_docs", "insertMany generated documents", nil
	case "transaction":
		return "transaction|batch", "transaction batch", nil
	default:
		return opType + "|(unknown)", opType, nil
	}
}

func summarizeQueryPreview(filter map[string]interface{}, maxLen int) string {
	if len(filter) == 0 {
		return ""
	}
	shape := renderShape(filter)
	if len(shape) > maxLen {
		return shape[:maxLen] + "...(truncated)"
	}
	return shape
}

func summarizePipelinePreview(pipeline []interface{}, maxLen int) string {
	if len(pipeline) == 0 {
		return ""
	}
	shape := renderShape(pipeline)
	if len(shape) > maxLen {
		return shape[:maxLen] + "...(truncated)"
	}
	return shape
}

func buildQueryReferenceFromDefinition(q config.QueryDefinition, opType string, filter map[string]interface{}, pipeline []interface{}) (queryLabel, querySource, workloadName, queryFile, queryDefID, querySummary, pipelineSummary string, queryDefIndex int) {
	queryDefIndex = q.DefinitionIndex
	queryLabel = strings.TrimSpace(q.Label)
	if queryLabel == "" {
		queryLabel = strings.TrimSpace(q.Name)
	}
	if queryLabel == "" {
		if q.DefinitionIndex >= 0 {
			queryLabel = fmt.Sprintf("%s query #%d", opType, q.DefinitionIndex+1)
		} else {
			queryLabel = fmt.Sprintf("%s query", opType)
		}
	}
	querySource = strings.TrimSpace(q.SourceType)
	if querySource == "" {
		querySource = "query_definition"
	}
	workloadName = strings.TrimSpace(q.WorkloadName)
	if workloadName == "" {
		workloadName = "custom_workload"
	}
	queryFile = strings.TrimSpace(q.SourceFile)
	if queryFile == "" {
		queryFile = "runtime"
	}
	queryDefID = strings.TrimSpace(q.ID)
	if queryDefID == "" && q.DefinitionIndex >= 0 {
		queryDefID = fmt.Sprintf("%s:%d", queryFile, q.DefinitionIndex+1)
	}
	querySummary = summarizeQueryPreview(filter, 256)
	pipelineSummary = summarizePipelinePreview(pipeline, 256)
	return
}

func RunWorkload(ctx context.Context, db *mongo.Database, collections []config.CollectionDefinition, queries []config.QueryDefinition, cfg *config.AppConfig, uiCollector ...*stats.Collector) error {
	duration, err := time.ParseDuration(cfg.Duration)
	if err != nil {
		return err
	}

	// Configure deterministic generation for the workload phase (no-op when 0).
	datagen.ConfigureSeed(cfg.RandomSeed)

	if err := config.ValidateCollectionDefinitions(collections); err != nil {
		return err
	}
	if err := config.NormalizeAndValidateQueries(queries); err != nil {
		return err
	}
	boundQueries, err := config.ValidateAndBindQueriesToCollections(queries, collections)
	if err != nil {
		return err
	}
	queries = boundQueries

	for _, col := range collections {
		if err := ensureGenericShardingFn(ctx, db, col, cfg); err != nil {
			if config.NormalizeShardingMode(cfg.ShardingMode) == "force_on" {
				return fmt.Errorf("sharding setup failed for %s.%s: %w", col.DatabaseName, col.Name, err)
			}
			log.Printf("Sharding setup warning for %s: %v", col.Name, err)
		}
	}

	var collector *stats.Collector
	if len(uiCollector) > 0 && uiCollector[0] != nil {
		collector = uiCollector[0]
	} else {
		collector = stats.NewCollector()
	}
	collector.ConfigureInsights(cfg)
	collector.SetCollectionsForInsights(collections)

	if err := config.ValidateWorkloadRecordReplay(cfg); err != nil {
		return err
	}
	if err := config.ValidateWorkloadDeterministic(cfg); err != nil {
		return err
	}
	if err := config.ValidateWorkloadOutcomes(cfg); err != nil {
		return err
	}
	if err := config.ValidateWorkloadTargets(cfg); err != nil {
		return err
	}
	if cfg.WorkloadReplayEnabled {
		return runWorkloadReplayFn(ctx, db, collections, queries, cfg, collector)
	}

	if duration <= 0 {
		return runAllQueriesOnceFn(ctx, db, queries, cfg.DebugMode)
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

	cachedFilterField := getPrimaryFilterFieldFn(ctx, db, collections[0])

	// Compile the load profile. An empty/fixed profile yields a constant
	// schedule equal to cfg.Concurrency, preserving historical behavior. The
	// worker pool is sized to the schedule peak; the controller gates how many
	// are active at any moment.
	schedule, err := loadprofile.Compile(cfg.LoadProfile, cfg.Concurrency)
	if err != nil {
		return fmt.Errorf("invalid load profile: %w", err)
	}

	wCfg := workloadConfig{
		database:    db,
		appConfig:   cfg,
		concurrency: schedule.MaxWorkers(),
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
		schedule:           schedule,
		thinkTime:          time.Duration(cfg.ThinkTimeMs) * time.Millisecond,
		thinkJitter:        time.Duration(cfg.ThinkJitterMs) * time.Millisecond,
		outcomes:           cfg.WorkloadRecordOutcomes,
		checksums:          cfg.WorkloadRecordOutcomes && cfg.WorkloadRecordResultChecksums,
	}
	if cfg.WorkloadDeterministic {
		det, err := newDeterministicRun(ctx, collections, queries, cfg, wCfg.concurrency)
		if err != nil {
			return fmt.Errorf("deterministic workload: %w", err)
		}
		wCfg.det = det
		ow, closeOutcomes, err := deterministicOutcomeWriter(ctx, db, cfg, det, collector)
		if err != nil {
			return fmt.Errorf("deterministic workload outcomes: %w", err)
		}
		wCfg.outcomeFile = ow
		defer closeOutcomes()
	}

	if !cfg.WorkloadRecordEnabled {
		return runContinuousWorkloadFn(ctx, wCfg)
	}
	rec, done, err := workloadRecorder(ctx, db, collections, queries, cfg, wCfg.concurrency, collector)
	if err != nil {
		return fmt.Errorf("workload recording: %w", err)
	}
	wCfg.recorder = rec
	err = runContinuousWorkloadFn(ctx, wCfg)
	done()
	return err
}

func runContinuousWorkload(ctx context.Context, wCfg workloadConfig) error {
	// Directly-constructed configs (and tests) may not carry a compiled schedule.
	// Fall back to a fixed schedule from the requested concurrency so the worker
	// pool and gating logic always have a valid target.
	if wCfg.schedule.IsZero() {
		workers := wCfg.concurrency
		if workers < 1 {
			workers = 1
		}
		schedule, err := loadprofile.Compile(loadprofile.Config{Kind: string(loadprofile.KindFixed), Workers: workers}, workers)
		if err != nil {
			return err
		}
		wCfg.schedule = schedule
	}
	wCfg.concurrency = wCfg.schedule.MaxWorkers()
	if wCfg.concurrency < 1 {
		wCfg.concurrency = 1
	}

	// Shared load-profile state: the controller publishes the desired target;
	// workers maintain the active count as they park/un-park.
	var activeTarget int64 = int64(wCfg.schedule.TargetAt(0))
	var activeCount int64
	wCfg.activeTarget = &activeTarget
	wCfg.activeCount = &activeCount

	// Deterministic runs derive existing-record values from the seed: no
	// shared record pool, bootstrap sampling, or insert cache.
	if wCfg.recordPool == nil && wCfg.det == nil {
		wCfg.recordPool = NewRecordPool(wCfg.appConfig.RecordPoolMaxSize)
	}
	if wCfg.appConfig != nil && wCfg.det == nil {
		if sel, err := accesspattern.Compile(wCfg.appConfig.AccessPattern); err != nil {
			log.Printf("[AccessPattern] invalid configuration (%v); falling back to uniform access", err)
		} else {
			wCfg.recordPool.SetSelector(sel)
			if wCfg.debug {
				log.Printf("[AccessPattern] using %s", sel.Summary())
			}
		}
	}
	if wCfg.det == nil {
		bootstrapRecordPool(ctx, wCfg.database, wCfg.collections, wCfg.recordPool, wCfg.appConfig.RecordPoolBootstrapSample, wCfg.appConfig.RandomSeed != 0)
	}
	if wCfg.debug && wCfg.det == nil {
		for _, col := range wCfg.collections {
			ns := collectionNamespace(wCfg.database, col)
			if wCfg.recordPool.Len(ns) == 0 {
				log.Printf("[RecordPool] No existing records for %s; non-insert operations may use random filters until inserts populate the pool", ns)
			}
		}
	}

	// Deterministic runs never read the insert cache; leaving the global
	// alone lets the targets of a concurrent multi-target run start together.
	if wCfg.det == nil {
		InsertDocumentCache = make(chan map[string]interface{}, wCfg.maxInsertCache)
	}

	// Mark the precise start of the measured workload window. This is done after
	// all preparation (seeding, sharding, record-pool bootstrap) so the UI/CLI
	// elapsed timer reflects the true execution duration rather than a late,
	// poll-dependent estimate.
	if wCfg.collector != nil {
		wCfg.collector.MarkWorkloadStart()
	}
	wCfg.recorder.SetStart(time.Now())

	workloadCtx, cancel := context.WithTimeout(ctx, wCfg.duration)
	defer cancel()

	var panicMu sync.Mutex
	var panicErr error
	reportPanic := func(where string, recovered interface{}) {
		msg := fmt.Errorf("panic in %s: %v", where, recovered)
		panicMu.Lock()
		if panicErr == nil {
			panicErr = msg
		}
		panicMu.Unlock()
		log.Printf("[Workload Panic] %v", msg)
		cancel()
	}

	for _, col := range wCfg.collections {
		if wCfg.det != nil {
			break
		}
		seedCol := col
		go func() {
			defer func() {
				if r := recover(); r != nil {
					reportPanic(fmt.Sprintf("insert producer for %s.%s", seedCol.DatabaseName, seedCol.Name), r)
				}
			}()
			insertDocumentProducer(workloadCtx, seedCol, wCfg.maxInsertCache, wCfg.appConfig)
		}()
	}

	monitorDone := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				reportPanic("monitor goroutine", r)
			}
		}()
		wCfg.collector.Monitor(monitorDone, wCfg.appConfig.StatusRefreshRateSec, wCfg.concurrency, wCfg.appConfig.CSVExportEnabled, wCfg.appConfig.CSVExportAppend, wCfg.appConfig.CSVExportPath, wCfg.appConfig.WebUI.Enabled)
	}()

	// Load-profile controller: periodically recompute the target active worker
	// count from the schedule and publish it (along with the observed active
	// count) to the collector for live metrics. For a fixed profile the target is
	// constant, so this loop is effectively a no-op beyond reporting.
	runStart := time.Now()
	if wCfg.collector != nil {
		wCfg.collector.SetConcurrency(wCfg.schedule.TargetAt(0), 0)
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				reportPanic("load profile controller", r)
			}
		}()
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-workloadCtx.Done():
				return
			case <-ticker.C:
				target := wCfg.schedule.TargetAt(time.Since(runStart))
				atomic.StoreInt64(wCfg.activeTarget, int64(target))
				if wCfg.collector != nil {
					active := int(atomic.LoadInt64(wCfg.activeCount))
					wCfg.collector.SetConcurrency(target, active)
				}
			}
		}
	}()

	var wg sync.WaitGroup
	for i := 1; i <= wCfg.concurrency; i++ {
		wg.Add(1)
		if wCfg.det != nil {
			workerID := i
			go func() {
				defer wg.Done()
				defer func() {
					if r := recover(); r != nil {
						reportPanic(fmt.Sprintf("worker %d", workerID), r)
					}
				}()
				deterministicWorker(workloadCtx, newDetWorker(workerID, wCfg))
			}()
			continue
		}
		// Seed each worker deterministically when a random seed is configured so
		// per-worker operation streams are reproducible; otherwise stay random.
		workerSeed := time.Now().UnixNano() + int64(i)
		if wCfg.appConfig != nil && wCfg.appConfig.RandomSeed != 0 {
			workerSeed = wCfg.appConfig.RandomSeed + int64(i)*1000003
		}
		rng := rand.New(rand.NewSource(workerSeed))
		workerID := i
		go func() {
			defer func() {
				if r := recover(); r != nil {
					reportPanic(fmt.Sprintf("worker %d", workerID), r)
				}
			}()
			independentWorker(workloadCtx, workerID, &wg, wCfg, rng)
		}()
	}

	// Deterministic workers repeat their operation budget until duration
	// ends. timedOut is set when that deadline wins before the workers
	// notice; a run that never finished one budget is incomplete.
	timedOut := false
	if wCfg.det != nil {
		allDone := make(chan struct{})
		go func() {
			wg.Wait()
			close(allDone)
		}()
		select {
		case <-allDone:
		case <-workloadCtx.Done():
			timedOut = ctx.Err() == nil && errors.Is(workloadCtx.Err(), context.DeadlineExceeded)
		}
		cancel()
	}
	<-workloadCtx.Done()
	wg.Wait()
	if wCfg.collector != nil {
		wCfg.collector.MarkWorkloadEnd()
	}
	close(monitorDone)
	if wCfg.det != nil {
		wCfg.det.traffic.finishIteration(wCfg.det.iteration, wCfg.collector, timedOut)
	}

	panicMu.Lock()
	err := panicErr
	panicMu.Unlock()
	if err != nil {
		return err
	}

	summaryDuration := wCfg.duration
	if d, ok := wCfg.collector.WorkloadDuration(); ok && wCfg.det != nil {
		summaryDuration = d
	}
	wCfg.collector.PrintFinalSummary(summaryDuration, wCfg.appConfig.WebUI.Enabled)
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
		tasks <- &queryTask{definition: q, database: db, runID: int64(i + 1), debug: debug, rng: rand.New(rand.NewSource(datagen.NextSeed()))}
	}
	close(tasks)
	wg.Wait()
	return nil
}

func queryWorkerOnce(ctx context.Context, id int, tasks <-chan *queryTask, wg *sync.WaitGroup) {
	defer wg.Done()
	for task := range tasks {
		q := task.definition
		filter := cloneMap(q.Filter)
		processRecursive(filter, task.rng)
		executeQueryOnceFn(ctx, task.database, q, filter)
	}
}

func executeQueryOnce(ctx context.Context, database *mongo.Database, q config.QueryDefinition, filter map[string]interface{}) {
	dbOpCtx := context.Background()
	coll := database.Client().Database(q.Database).Collection(q.Collection)
	if q.Database == "" {
		coll = database.Collection(q.Collection)
	}

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

// buildOrderedShardKey returns the shard key in declared order with declared
// values; compound shard key order is significant and must not be sorted.
func buildOrderedShardKey(key config.IndexKeys) bson.D {
	return key.BSON()
}

func shardKeyIsHashed(key config.IndexKeys) bool {
	for _, e := range key {
		if e.Value == "hashed" {
			return true
		}
	}
	return false
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
	if db == nil {
		return fmt.Errorf("cannot ensure sharding: database handle is nil")
	}
	if cfg == nil {
		cfg = &config.AppConfig{Concurrency: 1}
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}

	admin := db.Client().Database("admin")

	// 1. Check if Cluster
	var hello bson.M
	if err := admin.RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello); err != nil {
		if config.NormalizeShardingMode(cfg.ShardingMode) == "force_on" {
			return fmt.Errorf("failed topology detection for sharding_mode=force_on: %w", err)
		}
		return nil
	}
	isMongos := hello["msg"] == "isdbgrid"
	shouldApply, decisionErr := shouldApplyGenericSharding(config.NormalizeShardingMode(cfg.ShardingMode), cfg.ShardingSkipGenericWithoutConfig, isMongos, col)
	if decisionErr != nil {
		return decisionErr
	}
	if !shouldApply {
		return nil
	}

	dbName := strings.TrimSpace(col.DatabaseName)
	if dbName == "" {
		dbName = db.Name()
	}
	targetDB := db.Client().Database(dbName)
	ns := fmt.Sprintf("%s.%s", dbName, col.Name)

	// 2. Is it sharded? Do chunks exist?
	isSharded, chunkCount, _ := checkShardingStatus(ctx, targetDB, col.Name)

	// If it is already sharded AND has more than 1 chunk, assume setup is done.
	if !cfg.DropCollections && isSharded && chunkCount > 1 {
		return nil
	}

	patchKey := identifyPatchKey(col)
	if patchKey == "" {
		return nil
	}

	shardKeyDoc, isHashed, err := resolveShardKeyDoc(col, patchKey)
	if err != nil {
		return fmt.Errorf("invalid shard key config for %s: %w", ns, err)
	}

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

func resolveShardKeyDoc(col config.CollectionDefinition, patchKey string) (bson.D, bool, error) {
	if strings.TrimSpace(patchKey) == "" {
		return nil, false, fmt.Errorf("patch key is empty")
	}
	if col.ShardConfig == nil || len(col.ShardConfig.Key) == 0 {
		return bson.D{{Key: patchKey, Value: 1}}, false, nil
	}
	return buildOrderedShardKey(col.ShardConfig.Key), shardKeyIsHashed(col.ShardConfig.Key), nil
}

func shouldApplyGenericSharding(mode string, skipWithoutConfig bool, isMongos bool, col config.CollectionDefinition) (bool, error) {
	hasShardConfig := col.ShardConfig != nil && len(col.ShardConfig.Key) > 0
	ns := strings.TrimSpace(col.DatabaseName) + "." + strings.TrimSpace(col.Name)
	if strings.HasPrefix(ns, ".") {
		ns = strings.TrimPrefix(ns, ".")
	}

	switch mode {
	case "force_off":
		return false, nil
	case "force_on":
		if !isMongos {
			return false, fmt.Errorf("sharding_mode=force_on requires mongos topology for %s", ns)
		}
		if skipWithoutConfig && !hasShardConfig {
			return false, fmt.Errorf("invalid sharding setup for %s: sharding_mode=force_on conflicts with sharding_skip_generic_without_config=true when shardConfig is missing", ns)
		}
		return true, nil
	default: // auto
		if !isMongos {
			return false, nil
		}
		if skipWithoutConfig && !hasShardConfig {
			return false, nil
		}
		return true, nil
	}
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
