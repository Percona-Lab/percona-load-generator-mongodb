package mongo

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/accesspattern"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/datagen"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/workloadrec"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/workloads"
	"github.com/brianvoe/gofakeit/v6"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// Deterministic generated traffic (workload_deterministic). Every operation
// draws its values from a random stream derived from (seed, iteration,
// worker, per-worker sequence), so the same seed and configuration send the
// same commands per worker on any instance, and a failed or retried
// operation never shifts the values of later ones. Values for existing
// records come from regenerating the seeded documents or from the worker's
// own earlier inserts, never from a database read.

const (
	trafficFormat           = "plgm-generated-traffic/1"
	trafficStatusComplete   = "complete"
	trafficStatusIncomplete = "incomplete"

	// seededDocCacheMax bounds the regenerated seeded documents a worker
	// keeps; the cache is cleared when full.
	seededDocCacheMax = 4096
)

// splitMix64 is a rand.Source64 that a worker re-seeds for every operation
// without allocating.
type splitMix64 struct{ s uint64 }

func (r *splitMix64) Seed(seed int64) { r.s = uint64(seed) }
func (r *splitMix64) Int63() int64    { return int64(r.Uint64() >> 1) }
func (r *splitMix64) Uint64() uint64 {
	r.s += 0x9e3779b97f4a7c15
	return mix64(r.s)
}

func mix64(z uint64) uint64 {
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// deriveOpSeed is the seed of operation seq (1-based) of worker in
// iteration. seq 0 seeds the worker's setup (its prebuilt insert batches).
func deriveOpSeed(seed int64, iteration, worker int, seq uint64) uint64 {
	h := mix64(uint64(seed) + 0x9e3779b97f4a7c15)
	h = mix64(h ^ (uint64(iteration) + 0x632be59bd9b4e019))
	h = mix64(h ^ (uint64(worker) + 0x85157af5d2a3e6b1))
	return mix64(h ^ (seq + 0xd6e8feb86659fd93))
}

// deterministicDocumentID is the _id of document j inserted by operation
// seq of worker in iteration.
func deterministicDocumentID(seed int64, iteration, worker int, seq uint64, j int) bson.ObjectID {
	var id bson.ObjectID
	binary.BigEndian.PutUint32(id[0:4], uint32(datagen.SeededReferenceTime.Unix()))
	binary.BigEndian.PutUint64(id[4:12], mix64(deriveOpSeed(seed, iteration, worker, seq)^(uint64(j+1)*0x9e3779b97f4a7c15)))
	return id
}

// trafficStream is one worker's part of the fingerprint. Only that worker
// writes it while workers run.
type trafficStream struct {
	hash      *workloadrec.StreamHash
	generated uint64
	sent      uint64
	completed bool
}

// trafficHeader is what a fingerprint covers besides the commands.
type trafficHeader struct {
	Format              string `json:"format"`
	PLGMVersion         string `json:"plgm_version"`
	CollectionsHash     string `json:"collections_hash"`
	QueriesHash         string `json:"queries_hash"`
	RandomSeed          int64  `json:"random_seed"`
	Workers             int    `json:"workers"`
	OperationsPerWorker int    `json:"operations_per_worker"`
	Iterations          int    `json:"iterations"`
}

// trafficFingerprint accumulates the per-worker command hashes of a
// deterministic run across its iterations.
type trafficFingerprint struct {
	header        trafficHeader
	streams       map[int]*trafficStream
	safetyTimeout string
	status        string
	reason        string
}

func newTrafficFingerprint(collections []config.CollectionDefinition, queries []config.QueryDefinition, cfg *config.AppConfig, workers int) (*trafficFingerprint, error) {
	colHash, err := workloadrec.DefinitionHash(collections)
	if err != nil {
		return nil, fmt.Errorf("hash collection definitions: %w", err)
	}
	queryHash, err := workloadrec.DefinitionHash(queries)
	if err != nil {
		return nil, fmt.Errorf("hash query definitions: %w", err)
	}
	t := &trafficFingerprint{
		header: trafficHeader{
			Format: trafficFormat, PLGMVersion: config.BuildVersion, CollectionsHash: colHash, QueriesHash: queryHash,
			RandomSeed: cfg.RandomSeed, Workers: workers, OperationsPerWorker: cfg.OperationsPerWorker,
		},
		streams: make(map[int]*trafficStream, workers), safetyTimeout: cfg.Duration, status: trafficStatusComplete,
	}
	for id := 1; id <= workers; id++ {
		t.streams[id] = &trafficStream{hash: workloadrec.NewStreamHash()}
	}
	return t, nil
}

// info is the fingerprint after the iterations run so far.
func (t *trafficFingerprint) info() *stats.GeneratedTrafficInfo {
	sums := make(map[int]string, len(t.streams))
	out := &stats.GeneratedTrafficInfo{
		Status: t.status, Reason: t.reason, PLGMVersion: t.header.PLGMVersion,
		CollectionsHash: t.header.CollectionsHash, QueriesHash: t.header.QueriesHash, RandomSeed: t.header.RandomSeed,
		Workers: t.header.Workers, OperationsPerWorker: t.header.OperationsPerWorker, Iterations: t.header.Iterations,
		OpsPlanned:    uint64(t.header.Workers) * uint64(t.header.OperationsPerWorker) * uint64(t.header.Iterations),
		WorkerHashes:  make(map[string]string, len(t.streams)),
		SafetyTimeout: t.safetyTimeout,
	}
	for id, s := range t.streams {
		sums[id] = s.hash.Sum()
		out.WorkerHashes[strconv.Itoa(id)] = sums[id]
		out.OpsGenerated += s.generated
		out.OpsSent += s.sent
	}
	out.CommandsHash = workloadrec.CombineHashes(sums)
	h, _ := json.Marshal(t.header)
	out.Fingerprint = workloadrec.HashBytes(append(append(h, '\n'), out.CommandsHash...))
	return out
}

// finishIteration marks the fingerprint incomplete when a worker did not
// send its whole budget, and reports it on collector.
func (t *trafficFingerprint) finishIteration(iteration int, collector *stats.Collector, timedOut bool) {
	for _, s := range t.streams {
		if !s.completed && t.status == trafficStatusComplete {
			t.status = trafficStatusIncomplete
			t.reason = "the run was stopped before every worker sent its operations_per_worker operations"
			if timedOut {
				t.reason = fmt.Sprintf("the safety timeout (duration=%s) was reached before every worker sent its operations_per_worker operations", t.safetyTimeout)
			}
		}
		s.completed = false
	}
	info := t.info()
	if collector != nil {
		collector.SetGeneratedTraffic(info)
	}
	msg := fmt.Sprintf("[Deterministic] iteration %d: %d of %d operations generated, %d sent; traffic fingerprint %s (%s)", iteration, info.OpsGenerated, info.OpsPlanned, info.OpsSent, info.Fingerprint, info.Status)
	if info.Reason != "" {
		msg += ": " + info.Reason
	}
	log.Print(msg)
}

// deterministicRun is the state of one deterministic RunWorkload call.
type deterministicRun struct {
	seed      int64
	iteration int
	budget    int
	// seeded is the number of documents seeded per collection; seededIDs
	// is set when they got seeded _ids (drop_collections).
	seeded    int
	seededIDs bool
	selector  accesspattern.Selector
	maxOwn    int
	traffic   *trafficFingerprint
}

// newDeterministicRun prepares a deterministic RunWorkload call. Inside a
// recording session the fingerprint spans every iteration of the run.
func newDeterministicRun(ctx context.Context, collections []config.CollectionDefinition, queries []config.QueryDefinition, cfg *config.AppConfig, workers int) (*deterministicRun, error) {
	sess, _ := ctx.Value(recordingSessionKey{}).(*recordingSession)
	var traffic *trafficFingerprint
	iteration := 1
	if sess == nil {
		if cfg.Iterations > 1 {
			return nil, fmt.Errorf("a deterministic run of %d iterations needs one traffic fingerprint for the whole run (WithWorkloadRecordingSession)", cfg.Iterations)
		}
	} else {
		traffic = sess.traffic
		sess.trafficIteration++
		iteration = sess.trafficIteration
	}
	if traffic == nil {
		t, err := newTrafficFingerprint(collections, queries, cfg, workers)
		if err != nil {
			return nil, err
		}
		traffic = t
		if sess != nil {
			sess.traffic = t
		}
	}
	traffic.header.Iterations = iteration
	sel, err := accesspattern.Compile(cfg.AccessPattern)
	if err != nil {
		log.Printf("[AccessPattern] invalid configuration (%v); falling back to uniform access", err)
		sel, _ = accesspattern.Compile(accesspattern.Config{})
	}
	run := &deterministicRun{
		seed: cfg.RandomSeed, iteration: iteration, budget: cfg.OperationsPerWorker,
		seededIDs: cfg.DropCollections, selector: sel, maxOwn: cfg.RecordPoolMaxSize, traffic: traffic,
	}
	if !cfg.SkipSeed && cfg.DocumentsCount > 0 {
		run.seeded = cfg.DocumentsCount
	}
	if run.maxOwn <= 0 {
		run.maxOwn = 10000
	}
	log.Printf("[Deterministic] iteration %d: %d workers x %d operations, seed %d; values for existing records come from the %d seeded documents per collection and each worker's own inserts (no database reads); the same workload repeats until duration %s ends", iteration, workers, run.budget, run.seed, run.seeded, cfg.Duration)
	return run, nil
}

const (
	detStandard = iota
	detFastInsert
	detFastFind
	detTransaction
	detSkip
)

// detOp is one generated operation, ready to send.
type detOp struct {
	seq    uint64
	kind   int
	opType string
	col    config.CollectionDefinition
	ns     string
	pause  time.Duration

	req opRequest

	cmdRaw    []byte
	docCount  int
	ids       []int64
	batchSize int
	pooled    bool

	txOps []txInnerOp

	// command is the operation in its recorded form; hashLine is what the
	// fingerprint adds for it.
	command  interface{}
	hashLine []byte
	// shape is the shape the operation reported once sent.
	shape string
}

// detWorker generates one worker's operations.
type detWorker struct {
	id     int
	run    *deterministicRun
	wCfg   workloadConfig
	src    splitMix64
	rng    *rand.Rand
	faker  *gofakeit.Faker
	stream *trafficStream

	own         map[string][]map[string]interface{}
	seededCache map[string]map[string]interface{}

	fastPool  []PreBuiltBatch
	patchKey  string
	optimized bool
}

func newDetWorker(id int, wCfg workloadConfig) *detWorker {
	run := wCfg.det
	w := &detWorker{
		id: id, run: run, wCfg: wCfg, stream: run.traffic.streams[id],
		own: map[string][]map[string]interface{}{}, seededCache: map[string]map[string]interface{}{},
	}
	w.rng = rand.New(&w.src)
	w.faker = &gofakeit.Faker{Rand: w.rng}
	if len(wCfg.collections) > 0 {
		col := wCfg.collections[0]
		setup := &gofakeit.Faker{Rand: rand.New(&splitMix64{s: deriveOpSeed(run.seed, run.iteration, id, 0)})}
		gen := func() map[string]interface{} { return workloads.GenerateDocumentWithFaker(col, wCfg.appConfig, setup) }
		marshal := func(m map[string]interface{}) ([]byte, error) { return bson.Marshal(workloadrec.Canonical(m)) }
		if batches, key, err := prepareGenericBatchPoolWith(col, wCfg.appConfig.InsertBatchSize, 20, gen, marshal); err == nil {
			w.fastPool, w.patchKey, w.optimized = batches, key, true
		} else if id == 1 && wCfg.debug {
			log.Printf("Optimization unavailable for '%s': %v (Standard path)", col.Name, err)
		}
	}
	return w
}

func (w *detWorker) reseed(seq uint64) {
	w.src.s = deriveOpSeed(w.run.seed, w.run.iteration, w.id, seq)
}

// seededDoc regenerates the k-th seeded document of col exactly as
// insertRandomDocuments generated it.
func (w *detWorker) seededDoc(col config.CollectionDefinition, k int) map[string]interface{} {
	seedNS := col.DatabaseName + "." + col.Name
	key := seedNS + "\x00" + strconv.Itoa(k)
	if d, ok := w.seededCache[key]; ok {
		return d
	}
	doc := workloads.GenerateDocumentWithFaker(col, w.wCfg.appConfig, gofakeit.New(w.run.seed+int64(k)+1))
	if w.run.seededIDs {
		if _, ok := doc["_id"]; !ok {
			doc["_id"] = seededDocumentID(w.run.seed, seedNS, k)
		}
	}
	if len(w.seededCache) >= seededDocCacheMax {
		w.seededCache = map[string]map[string]interface{}{}
	}
	w.seededCache[key] = doc
	return doc
}

// pickExisting chooses "existing record #k" among the seeded documents of
// col and this worker's own earlier inserts into ns, by the access pattern.
func (w *detWorker) pickExisting(col config.CollectionDefinition, ns string) (map[string]interface{}, bool) {
	own := w.own[ns]
	n := w.run.seeded + len(own)
	if n == 0 {
		return nil, false
	}
	idx := w.run.selector.Index(n, w.rng)
	if idx < 0 || idx >= n {
		idx = w.rng.Intn(n)
	}
	if idx < w.run.seeded {
		return w.seededDoc(col, idx), true
	}
	return own[idx-w.run.seeded], true
}

// addOwn remembers a document this worker inserted (whether or not the
// insert succeeded, so a failure never changes later picks).
func (w *detWorker) addOwn(ns string, doc map[string]interface{}) {
	list := append(w.own[ns], doc)
	if over := len(list) - w.run.maxOwn; over > 0 {
		list = append(list[:0:0], list[over:]...)
	}
	w.own[ns] = list
}

// fillPlaceholders resolves the placeholders of v in sorted key order and
// reports whether an existing record supplied the values.
func (w *detWorker) fillPlaceholders(v interface{}, col config.CollectionDefinition, ns string) bool {
	po := &placeholderOptions{rng: w.rng, sortedKeys: true}
	if hit := w.wCfg.appConfig.ExistingRecordHitRate; hit > 0 && w.rng.Intn(100) < hit {
		if rec, ok := w.pickExisting(col, ns); ok {
			po.useExisting, po.existingRecord = true, rec
		}
	}
	processFilterPlaceholders(v, po, "")
	return po.useExisting
}

func (w *detWorker) newDocument(col config.CollectionDefinition, seq uint64, j int) map[string]interface{} {
	doc := workloads.GenerateDocumentWithFaker(col, w.wCfg.appConfig, w.faker)
	if !hasField(col, "_id") {
		doc["_id"] = deterministicDocumentID(w.run.seed, w.run.iteration, w.id, seq, j)
	}
	return doc
}

func (w *detWorker) fallbackUpdate(col config.CollectionDefinition, cfg *config.AppConfig, rng *rand.Rand) map[string]interface{} {
	return workloads.GenerateFallbackUpdateWithFaker(col, &gofakeit.Faker{Rand: rng})
}

// canonicalValue is v with every map as a sorted document, so the bytes
// sent do not depend on Go map order; nil stays nil.
func canonicalValue(v interface{}) interface{} {
	if isNilDoc(v) {
		return nil
	}
	return workloadrec.Canonical(v)
}

// generate builds operation seq from its derived random stream alone.
func (w *detWorker) generate(seq uint64) *detOp {
	w.reseed(seq)
	wCfg, cfg, rng := w.wCfg, w.wCfg.appConfig, w.rng
	op := &detOp{seq: seq, kind: detStandard}
	if wCfg.thinkTime > 0 || wCfg.thinkJitter > 0 {
		op.pause = wCfg.thinkTime
		if wCfg.thinkJitter > 0 {
			op.pause += time.Duration(rng.Int63n(int64(wCfg.thinkJitter) + 1))
		}
	}
	col := wCfg.collections[rng.Intn(len(wCfg.collections))]
	opType := selectOperation(wCfg.percentages, rng)
	if opType == "transaction" {
		if cfg.UseTransactions {
			w.generateTransaction(op)
			return w.finish(op)
		}
		opType = "find"
	}
	op.col, op.opType, op.ns = col, opType, collectionNamespace(wCfg.database, col)

	if w.optimized && col.Name == wCfg.collections[0].Name && (opType == "insert" || opType == "insertMany") {
		w.generateFastInsert(op)
		return w.finish(op)
	}
	if w.optimized && col.Name == wCfg.collections[0].Name && opType == "find" {
		w.generateFastFind(op)
		return w.finish(op)
	}

	var q config.QueryDefinition
	var docs []interface{}
	switch opType {
	case "insert":
		doc := w.newDocument(col, seq, 0)
		w.addOwn(op.ns, doc)
		q = config.QueryDefinition{Collection: col.Name, Operation: "insert", Filter: doc, SourceType: "generated_insert", SourceFile: "runtime_generated", WorkloadName: "generated_workload", DefinitionIndex: -1, Label: "generated insert document"}
	case "insertMany":
		docs = make([]interface{}, cfg.InsertBatchSize)
		for j := range docs {
			doc := w.newDocument(col, seq, j)
			w.addOwn(op.ns, doc)
			docs[j] = doc
		}
	case "find", "updateOne", "updateMany", "deleteOne", "deleteMany", "aggregate":
		var run bool
		q, run = selectQueryByTypeWith(opType, wCfg.queryMap, col, rng, wCfg.primaryFilterField, cfg, w.fallbackUpdate)
		if !run {
			op.kind = detSkip
			return op
		}
	default:
		op.kind = detSkip
		return op
	}

	var filter map[string]interface{}
	var pipeline []interface{}
	usedExisting := false
	switch opType {
	case "aggregate":
		if cloned, ok := deepClone(q.Pipeline).([]interface{}); ok {
			pipeline = cloned
			usedExisting = w.fillPlaceholders(pipeline, col, op.ns)
		}
	case "insert", "insertMany":
	default:
		filter = cloneMap(q.Filter)
		usedExisting = w.fillPlaceholders(filter, col, op.ns)
	}
	findLimit := q.Limit
	if findLimit <= 0 {
		findLimit = wCfg.findLimit
	}
	batch := wCfg.findBatchSize
	if batch <= 0 {
		batch = 10
	}
	op.req = opRequest{
		opType: opType, col: col, filter: canonicalValue(filter), update: canonicalValue(q.Update), projection: canonicalValue(q.Projection),
		upsert: q.Upsert, findLimit: findLimit, findBatch: batch, usedExisting: usedExisting,
		q: q, filterMap: filter, pipelineIn: pipeline,
	}
	if pipeline != nil {
		op.req.pipeline = workloadrec.Canonical(pipeline)
	}
	switch opType {
	case "insert":
		op.req.filter, op.req.filterMap, op.req.doc = nil, nil, workloadrec.Canonical(q.Filter)
	case "insertMany":
		op.req.docs = make([]interface{}, len(docs))
		for j, d := range docs {
			op.req.docs[j] = workloadrec.Canonical(d)
		}
	}
	op.command = standardCommand(&op.req)
	return w.finish(op)
}

func (w *detWorker) generateFastInsert(op *detOp) {
	batch := w.fastPool[w.rng.Intn(len(w.fastPool))]
	raw := append([]byte(nil), batch.CmdRaw...)
	n := len(batch.Offsets)
	base := int64(w.id)*100000000 + int64(w.run.iteration-1)*int64(w.run.budget)*int64(n) + int64(op.seq-1)*int64(n)
	for j, offset := range batch.Offsets {
		id := base + int64(j)
		binary.LittleEndian.PutUint64(raw[offset:], uint64(id))
		w.addOwn(op.ns, map[string]interface{}{w.patchKey: id})
	}
	if w.patchKey != "_id" && !hasField(op.col, "_id") {
		seq, run := op.seq, w.run
		if withIDs, err := withBatchIDsFrom(raw, func(j int) bson.ObjectID { return deterministicDocumentID(run.seed, run.iteration, w.id, seq, j) }); err == nil {
			raw = withIDs
		}
	}
	op.kind, op.opType, op.cmdRaw, op.docCount = detFastInsert, "insertMany", raw, w.wCfg.appConfig.InsertBatchSize
	op.command = bson.Raw(raw)
}

func (w *detWorker) generateFastFind(op *detOp) {
	cfg := w.wCfg.appConfig
	batchSize := cfg.FindBatchSize
	if batchSize <= 1 {
		batchSize = 10
	}
	ids := make([]int64, 0, batchSize)
	pooled := 0
	if w.run.seeded+len(w.own[op.ns]) > 0 && w.rng.Intn(100) < cfg.ExistingRecordHitRate {
		for tries := 0; len(ids) < batchSize && tries < 2*batchSize; tries++ {
			rec, ok := w.pickExisting(op.col, op.ns)
			if !ok {
				break
			}
			switch v := rec[w.patchKey].(type) {
			case int:
				ids = append(ids, int64(v))
			case int32:
				ids = append(ids, int64(v))
			case int64:
				ids = append(ids, v)
			default:
				continue
			}
			pooled++
		}
	}
	for len(ids) < batchSize {
		targetW := w.rng.Intn(w.wCfg.concurrency)
		targetSeq := w.rng.Int63n(10000000)
		ids = append(ids, int64(targetW)*100000000+targetSeq)
	}
	op.kind, op.ids, op.batchSize, op.pooled = detFastFind, ids, batchSize, pooled > 0
	op.command = fastFindRecord(op.col, w.patchKey, ids, batchSize)
}

// generateTransaction builds every operation of a transaction up front, so a
// retry of the transaction sends the same values.
func (w *detWorker) generateTransaction(op *detOp) {
	wCfg, cfg, rng := w.wCfg, w.wCfg.appConfig, w.rng
	op.kind, op.opType = detTransaction, "transaction"
	op.ns = wCfg.dbName() + ".(multiple)"
	numOps := rng.Intn(max(cfg.MaxTransactionOps, 1)) + 1
	recorded := bson.A{}
	docIndex := 0
	for i := 0; i < numOps; i++ {
		col := wCfg.collections[rng.Intn(len(wCfg.collections))]
		innerOp := selectOperation(wCfg.percentages, rng)
		if innerOp == "aggregate" || innerOp == "transaction" {
			innerOp = "find"
		}
		ns := collectionNamespace(wCfg.database, col)
		tx := txInnerOp{opType: innerOp, col: col}
		switch innerOp {
		case "insert":
			doc := w.newDocument(col, op.seq, docIndex)
			docIndex++
			w.addOwn(ns, doc)
			tx.doc = workloadrec.Canonical(doc)
		case "insertMany":
			tx.docs = make([]interface{}, cfg.InsertBatchSize)
			for j := range tx.docs {
				doc := w.newDocument(col, op.seq, docIndex)
				docIndex++
				w.addOwn(ns, doc)
				tx.docs[j] = workloadrec.Canonical(doc)
			}
		default:
			q, run := selectQueryByTypeWith(innerOp, wCfg.queryMap, col, rng, wCfg.primaryFilterField, cfg, w.fallbackUpdate)
			if !run {
				continue
			}
			filter := cloneMap(q.Filter)
			w.fillPlaceholders(filter, col, ns)
			tx.filter, tx.update, tx.upsert, tx.filterMap = canonicalValue(filter), canonicalValue(q.Update), q.Upsert, filter
		}
		op.txOps = append(op.txOps, tx)
		recorded = append(recorded, txInnerCommand(wCfg.database, &op.txOps[len(op.txOps)-1]))
	}
	op.command = bson.D{{Key: "ops", Value: recorded}}
}

// finish encodes the operation's fingerprint line: namespace, operation,
// and the canonical Extended JSON of its command.
func (w *detWorker) finish(op *detOp) *detOp {
	cmd, err := workloadrec.EncodeCommand(op.command)
	if err != nil {
		cmd = []byte("encode_error: " + err.Error())
	}
	line := make([]byte, 0, len(op.ns)+len(op.opType)+len(cmd)+2)
	line = append(append(append(append(line, op.ns...), '\t'), op.opType...), '\t')
	op.hashLine = append(line, cmd...)
	return op
}

// deterministicExecFn sends one generated operation; tests replace it.
var deterministicExecFn = executeDeterministicOp

// executeDeterministicOp sends op; a non-nil out receives its outcome, and
// op.shape the shape it reported.
func executeDeterministicOp(ctx context.Context, wCfg workloadConfig, w *detWorker, op *detOp, out *workloadrec.Outcome) bool {
	start := time.Now()
	recording := wCfg.recorder.Active()
	var ok bool
	switch op.kind {
	case detFastInsert:
		ok, op.shape = executeFastInsert(ctx, wCfg, w.id, op.col, op.cmdRaw, op.docCount, start, nil, out), fastInsertShapeKey
		if recording {
			recordOp(wCfg, workloadrec.Entry{Worker: w.id, NS: op.ns, Op: "insertMany", Path: workloadrec.PathFastInsertBatch, OK: ok, Command: bson.Raw(op.cmdRaw), Outcome: out}, start)
		}
	case detFastFind:
		ok, op.shape = executeFastFind(ctx, wCfg, w.id, op.col, w.patchKey, op.ids, op.batchSize, op.pooled, start, out), fastFindShapeKey(op.batchSize)
		if recording {
			recordOp(wCfg, workloadrec.Entry{Worker: w.id, NS: op.ns, Op: "find", Path: workloadrec.PathFastFind, OK: ok, Existing: op.pooled, Command: op.command, Outcome: out}, start)
		}
	case detTransaction:
		ok, op.shape = runTransaction(ctx, w.id, wCfg, nil, op.txOps, out), transactionShapeKey
	default:
		var opID opIdentity
		ok, opID = executeStandard(ctx, wCfg, &op.req, start, out)
		op.shape = opID.shapeKey
		if recording {
			recordOp(wCfg, workloadrec.Entry{Worker: w.id, NS: op.ns, Op: op.opType, OK: ok, Existing: op.req.usedExisting, Query: recordQueryRef(op.req.q, opID), Shape: recordShapeRef(opID), Command: op.command, Outcome: out}, start)
		}
	}
	return ok
}

// deterministicWorker sends the worker's operations_per_worker operations,
// then keeps sending the next operations of that same workload until ctx
// ends. The first pass is the traffic fingerprint. A later pass uses the
// next operation numbers, so inserts keep distinct deterministic _ids.
// The worker stops only when ctx ends.
func deterministicWorker(ctx context.Context, w *detWorker) {
	if w.wCfg.activeCount != nil {
		atomic.AddInt64(w.wCfg.activeCount, 1)
		defer atomic.AddInt64(w.wCfg.activeCount, -1)
	}
	budget := uint64(w.run.budget)
	if budget == 0 {
		return
	}
	dbOpCtx := context.Background()
	var passed uint64
	for {
		if ctx.Err() != nil {
			return
		}
		for seq := uint64(1); seq <= budget; seq++ {
			if ctx.Err() != nil {
				return
			}
			op := w.generate(passed*budget + seq)
			w.stream.generated++
			if op.pause > 0 {
				t := time.NewTimer(op.pause)
				select {
				case <-ctx.Done():
					t.Stop()
					return
				case <-t.C:
				}
			}
			if op.kind == detSkip {
				continue
			}
			if ctx.Err() != nil {
				return
			}
			if passed == 0 {
				w.stream.hash.Add(op.hashLine)
			}
			w.stream.sent++
			var out *workloadrec.Outcome
			if w.wCfg.outcomeFile != nil || w.wCfg.recorder.Active() {
				out = w.wCfg.newOutcome()
			}
			deterministicExecFn(dbOpCtx, w.wCfg, w, op, out)
			if out != nil && w.wCfg.outcomeFile != nil {
				w.wCfg.outcomeFile.Write(workloadrec.OutcomeLine{Worker: w.id, WorkerSeq: op.seq, Iteration: w.run.iteration, NS: op.ns, Op: op.opType, Shape: op.shape, Outcome: *out})
			}
		}
		w.stream.completed = true
		passed++
	}
}

func (c workloadConfig) dbName() string {
	if c.database == nil {
		if len(c.collections) > 0 {
			return c.collections[0].DatabaseName
		}
		return "unknown"
	}
	return c.database.Name()
}
