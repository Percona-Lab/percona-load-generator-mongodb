package benchmark

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Define BSON Types locally
const (
	TypeEmbeddedDocument = 0x03
	TypeArray            = 0x04
)

func GeneratePayload(size int) []byte {
	if size <= 0 {
		size = 1024
	}
	b := make([]byte, size)
	_, _ = crand.Read(b)
	return b
}

func GenerateRandomString(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}
	return string(b)
}

func RunRawInjector(ctx context.Context, db *mongo.Database, cfg *config.AppConfig, uiCollector ...*stats.Collector) error {
	collName := cfg.RawInjector.CollectionName
	coll := db.Collection(collName)

	if cfg.RawInjector.DropCollection {
		log.Printf("[RawInjector] Dropping collection '%s'...", collName)
		if err := coll.Drop(ctx); err != nil {
			return err
		}
		time.Sleep(2 * time.Second)
	}

	if err := ensureRangeSharding(ctx, db, cfg); err != nil {
		log.Printf("[RawInjector] Sharding setup warning: %v", err)
	}

	startSeqs := make([]int64, cfg.Concurrency)
	shouldScan := !cfg.RawInjector.DropCollection && (cfg.RawInjector.Type == "insert" || cfg.RawInjector.Type == "mixed" || cfg.RawInjector.Type == "upsert")

	if shouldScan {
		log.Println("[RawInjector] Checking for existing data to resume sequences...")
		var wgSeq sync.WaitGroup
		for i := 0; i < cfg.Concurrency; i++ {
			wgSeq.Add(1)
			go func(wID int) {
				defer wgSeq.Done()
				lastSeq, err := findLastSequence(ctx, coll, wID)
				if err != nil {
					log.Printf("Warning: failed to find last sequence for worker %d: %v", wID, err)
				} else {
					startSeqs[wID] = lastSeq + 1
				}
			}(i)
		}
		wgSeq.Wait()
	}

	log.Printf(">>> RAW INJECTOR START [%s] workers=%d batch=%d maxDocs=%d docSize=%d <<<",
		cfg.RawInjector.Type,
		cfg.Concurrency,
		cfg.RawInjector.BatchSize,
		cfg.RawInjector.MaxDocs,
		cfg.RawInjector.DocumentSize,
	)

	payload := GeneratePayload(cfg.RawInjector.DocumentSize)

	// 2. USE THE UI COLLECTOR IF PASSED, OTHERWISE CREATE A NEW ONE
	var collector *stats.Collector
	if len(uiCollector) > 0 && uiCollector[0] != nil {
		collector = uiCollector[0]
	} else {
		collector = stats.NewCollector()
	}
	collector.ConfigureInsights(cfg)

	monitorDone := make(chan struct{})

	// 3. PASS THE WEBUI FLAG TO THE MONITOR
	go collector.Monitor(monitorDone, cfg.StatusRefreshRateSec, cfg.Concurrency, cfg.CSVExportEnabled, cfg.CSVExportAppend, cfg.CSVExportPath, cfg.WebUI.Enabled)

	duration, err := time.ParseDuration(cfg.Duration)
	if err != nil {
		return err
	}

	workCtx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()

	var wg sync.WaitGroup
	docsPerWorker := cfg.RawInjector.MaxDocs / int64(cfg.Concurrency)
	if docsPerWorker < 1 {
		docsPerWorker = 1
	}

	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			runWorker(
				workCtx, workerID, db, collName, cfg.RawInjector.Type, payload,
				collector, docsPerWorker, cfg.RawInjector.BatchSize,
				cfg.Concurrency, startSeqs[workerID], cfg,
			)
		}(i)
	}

	<-workCtx.Done()
	wg.Wait()
	close(monitorDone)

	// 4. PASS THE WEBUI FLAG TO THE SUMMARY PRINTER
	collector.PrintFinalSummary(duration, cfg.WebUI.Enabled)
	return nil
}

func buildBatchArray(template []byte, count int, magic []byte) ([]byte, []int, error) {
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
			return nil, nil, errors.New("magic marker not found")
		}

		offsets[i] = buf.Len() + magicOffset
		buf.Write(template)
	}
	buf.WriteByte(0x00)

	totalLen := int32(buf.Len())
	binary.LittleEndian.PutUint32(buf.Bytes()[0:4], uint32(totalLen))

	return buf.Bytes(), offsets, nil
}

func runWorker(
	workCtx context.Context,
	workerID int,
	db *mongo.Database,
	collName string,
	initialOpType string,
	payload []byte,
	collector *stats.Collector,
	maxSeq int64,
	batchSize int,
	totalWorkers int,
	startSeq int64,
	cfg *config.AppConfig,
) {
	rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(workerID)))
	seq := startSeq
	if initialOpType == "upsert" {
		seq = 0
	}

	magic := int64(0x1122334455667788)
	magicBytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(magicBytes, uint64(magic))

	workerPayload := make([]byte, len(payload))
	copy(workerPayload, payload)

	payloadMagic := int64(0x778899AABBCCDDEE)
	payloadMagicBytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(payloadMagicBytes, uint64(payloadMagic))

	if len(workerPayload) >= 8 {
		copy(workerPayload[:8], payloadMagicBytes)
	}

	staticTime := time.Now()

	docTmplObj := bson.D{
		{Key: "_id", Value: bson.D{{Key: "w", Value: workerID}, {Key: "i", Value: magic}}},
		{Key: "fld0", Value: rng.Int63()},
		{Key: "fld1", Value: staticTime},
		{Key: "fld2", Value: GenerateRandomString(30)},
		{Key: "fld3", Value: GenerateRandomString(30)},
		{Key: "fld4", Value: GenerateRandomString(30)},
		{Key: "fld5", Value: staticTime.Add(-24 * time.Hour)},
		{Key: "fld6", Value: rng.Int63()},
		{Key: "fld7", Value: GenerateRandomString(30)},
		{Key: "fld8", Value: GenerateRandomString(30)},
		{Key: "fld9", Value: rng.Int63()},
		{Key: "bin", Value: workerPayload},
	}
	docTmplBytes, _ := bson.Marshal(docTmplObj)

	filterTmplObj := bson.D{{Key: "_id", Value: bson.D{{Key: "w", Value: workerID}, {Key: "i", Value: magic}}}}
	filterTmplBytes, _ := bson.Marshal(filterTmplObj)

	updateOpTmpl := bson.D{
		{Key: "q", Value: bson.Raw(filterTmplBytes)},
		{Key: "u", Value: bson.D{{Key: "$set", Value: bson.D{{Key: "fld1", Value: staticTime}}}}},
	}
	updateOpBytes, _ := bson.Marshal(updateOpTmpl)

	deleteOpTmpl := bson.D{{Key: "q", Value: bson.Raw(filterTmplBytes)}, {Key: "limit", Value: 1}}
	deleteOpBytes, _ := bson.Marshal(deleteOpTmpl)

	upsertOpTmpl := bson.D{
		{Key: "q", Value: bson.Raw(filterTmplBytes)},
		{Key: "u", Value: bson.Raw(docTmplBytes)},
		{Key: "upsert", Value: true},
	}
	upsertOpBytes, _ := bson.Marshal(upsertOpTmpl)

	idValObj := bson.D{{Key: "w", Value: workerID}, {Key: "i", Value: magic}}
	idValBytes, _ := bson.Marshal(idValObj)

	var insertArray []byte
	var insertOffsets []int
	var insertCmdRaw []byte
	var insertPayloadOffsets []int
	if initialOpType == "insert" || initialOpType == "mixed" {
		insertArray, insertOffsets, _ = buildBatchArray(docTmplBytes, batchSize, magicBytes)
		cmd := bson.D{
			{Key: "insert", Value: collName},
			{Key: "documents", Value: bson.RawValue{Type: TypeArray, Value: insertArray}},
			{Key: "ordered", Value: false},
		}
		insertCmdRaw, _ = bson.Marshal(cmd)
		arrStart := bytes.Index(insertCmdRaw, insertArray)
		for i := range insertOffsets {
			insertOffsets[i] += arrStart
		}

		if len(workerPayload) >= 8 {
			idx := 0
			for {
				i := bytes.Index(insertCmdRaw[idx:], payloadMagicBytes)
				if i == -1 {
					break
				}
				insertPayloadOffsets = append(insertPayloadOffsets, idx+i)
				idx += i + 8
			}
		}
	}

	var updateArray []byte
	var updateOffsets []int
	var updateCmdRaw []byte
	if initialOpType == "update" || initialOpType == "mixed" {
		updateArray, updateOffsets, _ = buildBatchArray(updateOpBytes, batchSize, magicBytes)
		cmd := bson.D{
			{Key: "update", Value: collName},
			{Key: "updates", Value: bson.RawValue{Type: TypeArray, Value: updateArray}},
			{Key: "ordered", Value: false},
		}
		updateCmdRaw, _ = bson.Marshal(cmd)
		arrStart := bytes.Index(updateCmdRaw, updateArray)
		for i := range updateOffsets {
			updateOffsets[i] += arrStart
		}
	}

	var deleteArray []byte
	var deleteOffsets []int
	var deleteCmdRaw []byte
	if initialOpType == "delete" || initialOpType == "mixed" {
		deleteArray, deleteOffsets, _ = buildBatchArray(deleteOpBytes, batchSize, magicBytes)
		cmd := bson.D{
			{Key: "delete", Value: collName},
			{Key: "deletes", Value: bson.RawValue{Type: TypeArray, Value: deleteArray}},
			{Key: "ordered", Value: false},
		}
		deleteCmdRaw, _ = bson.Marshal(cmd)
		arrStart := bytes.Index(deleteCmdRaw, deleteArray)
		for i := range deleteOffsets {
			deleteOffsets[i] += arrStart
		}
	}

	var upsertArray []byte
	var upsertOffsets []int
	var upsertCmdRaw []byte
	var upsertPayloadOffsets []int
	if initialOpType == "upsert" || initialOpType == "mixed" {
		upsertArray, upsertOffsets, _ = buildBatchArray(upsertOpBytes, batchSize, magicBytes)
		cmd := bson.D{
			{Key: "update", Value: collName},
			{Key: "updates", Value: bson.RawValue{Type: TypeArray, Value: upsertArray}},
			{Key: "ordered", Value: false},
		}
		upsertCmdRaw, _ = bson.Marshal(cmd)
		arrStart := bytes.Index(upsertCmdRaw, upsertArray)
		for i := range upsertOffsets {
			upsertOffsets[i] += arrStart
		}

		if len(workerPayload) >= 8 {
			idx := 0
			for {
				i := bytes.Index(upsertCmdRaw[idx:], payloadMagicBytes)
				if i == -1 {
					break
				}
				upsertPayloadOffsets = append(upsertPayloadOffsets, idx+i)
				idx += i + 8
			}
		}
	}

	var findArray []byte
	var findOffsets []int
	var findCmdRaw []byte
	if initialOpType == "find" || initialOpType == "mixed" {
		findArray, findOffsets, _ = buildBatchArray(idValBytes, batchSize, magicBytes)
		filter := bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: bson.RawValue{Type: TypeArray, Value: findArray}}}}}
		cmd := bson.D{{Key: "find", Value: collName}, {Key: "filter", Value: filter}, {Key: "limit", Value: batchSize}}
		findCmdRaw, _ = bson.Marshal(cmd)
		arrStart := bytes.Index(findCmdRaw, findArray)
		for i := range findOffsets {
			findOffsets[i] += arrStart
		}
	}

	pFind := cfg.FindPercent
	pInsert := pFind + cfg.InsertPercent
	pUpdate := pInsert + cfg.UpdatePercent
	pDelete := pUpdate + cfg.DeletePercent
	isMixed := initialOpType == "mixed"

	for {
		select {
		case <-workCtx.Done():
			return
		default:
		}

		currentOp := initialOpType
		if isMixed {
			r := rng.Intn(100)
			if r < pFind {
				currentOp = "find"
			} else if r < pInsert {
				currentOp = "insert"
			} else if r < pUpdate {
				currentOp = "update"
			} else if r < pDelete {
				currentOp = "delete"
			} else {
				currentOp = "find"
			}
		}

		start := time.Now()
		var runErr error

		timeoutCtx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.OpTimeoutMs)*time.Millisecond)
		switch currentOp {
		case "insert":
			tempSeq := seq
			for i, offset := range insertOffsets {
				binary.LittleEndian.PutUint64(insertCmdRaw[offset:], uint64(tempSeq))

				if i < len(insertPayloadOffsets) {
					binary.LittleEndian.PutUint64(insertCmdRaw[insertPayloadOffsets[i]:], uint64(tempSeq))
				}
				tempSeq++
			}
			seq = tempSeq
			runErr = db.RunCommand(timeoutCtx, bson.Raw(insertCmdRaw)).Err()

		case "update":
			for _, offset := range updateOffsets {
				var rndSeq int64
				if seq > 0 {
					rndSeq = rng.Int63n(seq)
				} else {
					rndSeq = 0 // skip the operation if there is absolutely no data
				}
				binary.LittleEndian.PutUint64(updateCmdRaw[offset:], uint64(rndSeq))
			}
			runErr = db.RunCommand(timeoutCtx, bson.Raw(updateCmdRaw)).Err()

		case "delete":
			for _, offset := range deleteOffsets {
				var rndSeq int64
				if seq > 0 {
					rndSeq = rng.Int63n(seq)
				} else {
					rndSeq = 0 // skip the operation if there is absolutely no data
				}
				binary.LittleEndian.PutUint64(deleteCmdRaw[offset:], uint64(rndSeq))
			}
			runErr = db.RunCommand(timeoutCtx, bson.Raw(deleteCmdRaw)).Err()

		case "upsert":
			tempSeq := seq
			for i, offset := range upsertOffsets {
				binary.LittleEndian.PutUint64(upsertCmdRaw[offset:], uint64(tempSeq))

				if i < len(upsertPayloadOffsets) {
					binary.LittleEndian.PutUint64(upsertCmdRaw[upsertPayloadOffsets[i]:], uint64(tempSeq))
				}
				tempSeq++
			}
			seq = tempSeq
			runErr = db.RunCommand(timeoutCtx, bson.Raw(upsertCmdRaw)).Err()

		case "find":
			for _, offset := range findOffsets {
				var rndSeq int64
				if seq > 0 {
					rndSeq = rng.Int63n(seq)
				} else {
					rndSeq = 0 // skip the operation if there is absolutely no data
				}
				binary.LittleEndian.PutUint64(findCmdRaw[offset:], uint64(rndSeq))
			}
			runErr = db.RunCommand(timeoutCtx, bson.Raw(findCmdRaw)).Err()
		}

		if runErr != nil && !mongo.IsDuplicateKeyError(runErr) {
			log.Printf("[Worker %d] %s error: %v", workerID, currentOp, runErr)
		}

		collector.Add(currentOp, int64(batchSize), time.Since(start)/time.Duration(batchSize))

		cancel()
	}
}

func ensureRangeSharding(ctx context.Context, db *mongo.Database, cfg *config.AppConfig) error {
	admin := db.Client().Database("admin")
	collName := cfg.RawInjector.CollectionName
	if collName == "" {
		collName = "injector_data"
	}
	var hello bson.M
	if err := admin.RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello); err != nil {
		return err
	}
	if hello["msg"] != "isdbgrid" {
		return nil
	}
	dbName := db.Name()
	ns := fmt.Sprintf("%s.%s", dbName, collName)
	if err := admin.RunCommand(ctx, bson.D{{Key: "enableSharding", Value: dbName}}).Err(); err != nil {
		return err
	}
	cmd := bson.D{
		{Key: "shardCollection", Value: ns},
		{Key: "key", Value: bson.D{{Key: "_id", Value: 1}}},
	}
	err := admin.RunCommand(ctx, cmd).Err()
	if err != nil && !strings.Contains(err.Error(), "already sharded") {
		return err
	}
	log.Println("[RawInjector] Range sharding enabled on { _id: 1 }")

	if n, err := countExistingChunks(ctx, db, ns); err == nil && n > 1 {
		log.Printf("[RawInjector] Collection '%s' already has %d chunks. Skipping pre-splitting.", ns, n)
		return nil
	}
	shards, err := listShards(ctx, admin)
	if err != nil {
		return fmt.Errorf("failed to list shards: %w", err)
	}
	if len(shards) == 0 {
		return errors.New("no shards found")
	}
	log.Printf("[RawInjector] Pre-splitting: %d workers -> %d shards", cfg.Concurrency, len(shards))
	splitStart := bson.D{{Key: "split", Value: ns}, {Key: "middle", Value: bson.D{{Key: "_id", Value: bson.D{{Key: "w", Value: 0}, {Key: "i", Value: int64(0)}}}}}}
	_ = admin.RunCommand(ctx, splitStart).Err()
	for w := 0; w < cfg.Concurrency; w++ {
		nextWorkerSplit := bson.D{{Key: "split", Value: ns}, {Key: "middle", Value: bson.D{{Key: "_id", Value: bson.D{{Key: "w", Value: w + 1}, {Key: "i", Value: int64(0)}}}}}}
		_ = admin.RunCommand(ctx, nextWorkerSplit).Err()
		targetShard := shards[w%len(shards)]
		moveCmd := bson.D{
			{Key: "moveChunk", Value: ns},
			{Key: "find", Value: bson.D{{Key: "_id", Value: bson.D{{Key: "w", Value: w}, {Key: "i", Value: int64(0)}}}}},
			{Key: "to", Value: targetShard},
		}
		_ = admin.RunCommand(ctx, moveCmd).Err()
	}
	return nil
}

func countExistingChunks(ctx context.Context, db *mongo.Database, ns string) (int64, error) {
	configDB := db.Client().Database("config")
	n, err := configDB.Collection("chunks").CountDocuments(ctx, bson.D{{Key: "ns", Value: ns}})
	if err == nil && n > 0 {
		return n, nil
	}
	return 0, nil
}

func findLastSequence(ctx context.Context, coll *mongo.Collection, workerID int) (int64, error) {
	filter := bson.D{
		{Key: "_id", Value: bson.D{
			{Key: "$gte", Value: bson.D{{Key: "w", Value: workerID}, {Key: "i", Value: int64(0)}}},
			{Key: "$lt", Value: bson.D{{Key: "w", Value: workerID + 1}, {Key: "i", Value: int64(0)}}},
		}},
	}
	opts := options.FindOne().SetSort(bson.D{{Key: "_id", Value: -1}})
	var result struct {
		ID struct {
			I int64 `bson:"i"`
		} `bson:"_id"`
	}
	err := coll.FindOne(ctx, filter, opts).Decode(&result)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return 0, nil
		}
		return 0, err
	}
	return result.ID.I, nil
}

func listShards(ctx context.Context, admin *mongo.Database) ([]string, error) {
	var result struct {
		Shards []struct {
			ID string `bson:"_id"`
		} `bson:"shards"`
	}
	err := admin.RunCommand(ctx, bson.D{{Key: "listShards", Value: 1}}).Decode(&result)
	if err != nil {
		return nil, err
	}
	shardNames := make([]string, len(result.Shards))
	for i, s := range result.Shards {
		shardNames[i] = s.ID
	}
	return shardNames, nil
}
