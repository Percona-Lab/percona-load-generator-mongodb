package mongo

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/workloadrec"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

const replayQueueSize = 1024

// Replay statuses besides the recording statuses.
const (
	replayStatusRunning   = "running"
	replayStatusCancelled = "cancelled"
)

// replayExecFn sends one recorded operation and returns whether it
// succeeded and the canonical form of the command it sent. A non-nil out
// receives the outcome.
var replayExecFn = executeReplayOp

// replayMismatches compares a recording's header with the current run. An
// imported recording has no definitions to compare: its namespaces are
// replayed as captured.
func replayMismatches(h workloadrec.Header, db *mongo.Database, collections []config.CollectionDefinition, queries []config.QueryDefinition) []string {
	if h.Import != nil {
		return nil
	}
	var out []string
	if colHash, err := workloadrec.DefinitionHash(collections); err != nil || colHash != h.CollectionsHash {
		out = append(out, "collection definitions differ from the recording (collections_hash)")
	}
	if queryHash, err := workloadrec.DefinitionHash(queries); err != nil || queryHash != h.QueriesHash {
		out = append(out, "query definitions differ from the recording (queries_hash)")
	}
	if got, want := workloadNamespaces(db, collections), h.Namespaces; strings.Join(got, ",") != strings.Join(want, ",") {
		out = append(out, fmt.Sprintf("namespaces differ from the recording (recorded %s, configured %s)", strings.Join(want, ", "), strings.Join(got, ", ")))
	}
	return out
}

type replayCollections map[string]config.CollectionDefinition

func newReplayCollections(db *mongo.Database, collections []config.CollectionDefinition) replayCollections {
	out := replayCollections{}
	for _, col := range collections {
		out[collectionNamespace(db, col)] = col
	}
	return out
}

// lookup returns the collection of a recorded namespace. A namespace that
// is not configured (allowed with workload_replay_allow_mismatch) is used
// as recorded.
func (c replayCollections) lookup(ns string) config.CollectionDefinition {
	if col, ok := c[ns]; ok {
		return col
	}
	dbName, colName, _ := strings.Cut(ns, ".")
	return config.CollectionDefinition{DatabaseName: dbName, Name: colName}
}

type replayLag struct {
	mu    sync.Mutex
	max   time.Duration
	total time.Duration
	n     int64
}

func (l *replayLag) add(d time.Duration) {
	if d < 0 {
		d = 0
	}
	l.mu.Lock()
	if d > l.max {
		l.max = d
	}
	l.total += d
	l.n++
	l.mu.Unlock()
}

func runWorkloadReplay(ctx context.Context, db *mongo.Database, collections []config.CollectionDefinition, queries []config.QueryDefinition, cfg *config.AppConfig, collector *stats.Collector) error {
	info, err := workloadrec.Scan(cfg.WorkloadReplayPath)
	if err != nil {
		return fmt.Errorf("workload replay: %w", err)
	}
	mode := cfg.WorkloadReplayMode
	if mode == "" {
		mode = config.WorkloadReplayModePerWorker
	}
	workers := info.Workers
	if workers < 1 {
		workers = 1
	}
	rep := stats.WorkloadReplayInfo{
		FileName: filepath.Base(cfg.WorkloadReplayPath), HeaderHash: info.HeaderHash, Mode: mode, Workers: workers,
		OpsTotal: info.Ops, Status: replayStatusRunning, SourceStatus: info.Status,
		DurationIgnored: strings.TrimSpace(cfg.Duration),
		Warnings:        config.WorkloadReplayWarnings(cfg),
	}
	if info.Compression != workloadrec.CompressionNone {
		rep.Compression = info.Compression
	}
	if info.Iterations > 1 {
		rep.Iterations = info.Iterations
	}
	rep.Mismatches = replayMismatches(info.Header, db, collections, queries)
	if imp := info.Header.Import; imp != nil {
		rep.ImportedFrom = imp.Sources
		rep.Warnings = append(rep.Warnings, importedReplayWarnings(imp, cfg)...)
	}
	check, err := checkDataFingerprint(ctx, db, collections, info.Header.DataFingerprint)
	if err != nil {
		return fmt.Errorf("workload replay: %w", err)
	}
	if check != nil {
		rep.DataFingerprint = &stats.DataFingerprintCheck{Mode: check.Mode, Match: check.Match, Differences: check.Differences, Notes: check.Notes}
		if !check.Match {
			rep.Mismatches = append(rep.Mismatches, "the starting data differs from the recording's fingerprint: "+strings.Join(check.Differences, "; "))
		}
		rep.Warnings = append(rep.Warnings, check.Notes...)
		log.Printf("[Workload Replay] starting-data fingerprint (%s): match %t", check.Mode, check.Match)
	}
	readsOnly := config.ReplayReadsOnly(cfg)
	if readsOnly {
		rep.OpsFilter = config.WorkloadReplayOpsReadsOnly
		log.Printf("[Workload Replay] workload_replay_ops=reads_only: write operations are skipped and counted, so this target keeps its starting data")
	}
	tgt := targetFromContext(ctx)
	if tgt.progress != nil {
		tgt.progress.Total.Store(info.Ops)
	}
	switch info.Status {
	case workloadrec.StatusComplete:
	case workloadrec.StatusTruncated:
		rep.Warnings = append(rep.Warnings, "the recording was truncated at workload_record_max_bytes; replaying the recorded prefix")
	default:
		rep.Mismatches = append(rep.Mismatches, fmt.Sprintf("the recording status is %q, not complete", info.Status))
	}
	if len(rep.Mismatches) > 0 {
		if !cfg.WorkloadReplayAllowMismatch {
			return fmt.Errorf("workload replay: %s does not match this run: %s (set workload_replay_allow_mismatch to replay anyway)", rep.FileName, strings.Join(rep.Mismatches, "; "))
		}
		for _, m := range rep.Mismatches {
			log.Printf("[Workload Replay] WARNING: %s (allowed by workload_replay_allow_mismatch)", m)
		}
	}
	for _, w := range rep.Warnings {
		log.Printf("[Workload Replay] WARNING: %s", w)
	}
	if rep.DurationIgnored != "" {
		log.Printf("[Workload Replay] duration %s is ignored: the replay ends when the recording is exhausted", rep.DurationIgnored)
	}
	log.Printf("[Workload Replay] replaying %d operations in %d iteration(s) from %s on %d workers (%s mode, recording %s, compression %s)", info.Ops, max(info.Iterations, 1), rep.FileName, workers, mode, info.Status, info.Compression)
	if collector != nil {
		collector.SetWorkloadReplay(&rep)
	}

	wCfg := workloadConfig{database: db, appConfig: cfg, concurrency: workers, collections: collections, collector: collector, debug: cfg.DebugMode}
	if cfg.WorkloadRecordOutcomes {
		ow, err := openOutcomeFile(ctx, db, cfg.WorkloadReplayOutcomesPath, cfg, workloadrec.OutcomeHeader{
			Source: workloadrec.OutcomeSourceReplay, Workers: workers, RecordingFile: rep.FileName, RecordingHeaderHash: info.HeaderHash, OpsFilter: rep.OpsFilter,
		})
		if err != nil {
			return fmt.Errorf("workload replay outcomes: %w", err)
		}
		wCfg.outcomes, wCfg.checksums, wCfg.outcomeFile = true, cfg.WorkloadRecordResultChecksums, ow
	}
	cols := newReplayCollections(db, collections)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var panicMu sync.Mutex
	var panicErr error
	reportPanic := func(where string, recovered interface{}) {
		panicMu.Lock()
		if panicErr == nil {
			panicErr = fmt.Errorf("panic in %s: %v", where, recovered)
		}
		panicMu.Unlock()
		log.Printf("[Workload Panic] panic in %s: %v", where, recovered)
		cancel()
	}

	if collector != nil {
		collector.MarkWorkloadStart()
		collector.SetConcurrency(workers, workers)
	}
	monitorDone := make(chan struct{})
	if collector != nil {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					reportPanic("monitor goroutine", r)
				}
			}()
			collector.Monitor(monitorDone, cfg.StatusRefreshRateSec, workers, cfg.CSVExportEnabled, cfg.CSVExportAppend, cfg.CSVExportPath, cfg.WebUI.Enabled)
		}()
	}

	queues := make([]chan *workloadrec.OpLine, workers+1)
	for i := 1; i <= workers; i++ {
		queues[i] = make(chan *workloadrec.OpLine, replayQueueSize)
	}
	hashes := make([]*workloadrec.StreamHash, workers+1)
	var replayed, failed atomic.Uint64
	var execErrMu sync.Mutex
	var execErr error
	var lag replayLag
	timed := mode == config.WorkloadReplayModeTimed
	measureLag := timed || tgt.concurrent
	var skipped uint64
	replayStart := time.Now()

	// pending counts operations handed to workers and not yet finished, so
	// an iteration starts only after every operation of the previous one ran.
	var pending sync.WaitGroup
	replayOne := func(workerID int, op *workloadrec.OpLine) {
		defer pending.Done()
		if runCtx.Err() != nil {
			return
		}
		if measureLag {
			target := replayStart.Add(time.Duration(op.OffsetUS) * time.Microsecond)
			if wait := time.Until(target); timed && wait > 0 {
				t := time.NewTimer(wait)
				select {
				case <-runCtx.Done():
					t.Stop()
					return
				case <-t.C:
				}
			}
			lag.add(time.Since(target))
		}
		out := wCfg.newOutcome()
		ok, sent, err := replayExecFn(runCtx, wCfg, workerID, op, cols, out)
		if out != nil && err == nil {
			wCfg.outcomeFile.Write(workloadrec.OutcomeLine{Seq: op.Seq, Worker: workerID, WorkerSeq: op.WorkerSeq, Iteration: op.Iteration, NS: op.NS, Op: op.Op, Shape: replayShapeKey(op), Outcome: *out})
		}
		if err != nil {
			execErrMu.Lock()
			if execErr == nil {
				execErr = fmt.Errorf("worker %d operation %d: %w", workerID, op.WorkerSeq, err)
			}
			execErrMu.Unlock()
			failed.Add(1)
			cancel()
			return
		}
		if hashes[workerID] == nil {
			hashes[workerID] = workloadrec.NewStreamHash()
		}
		hashes[workerID].Add(sent)
		replayed.Add(1)
		if tgt.progress != nil {
			tgt.progress.Sent.Add(1)
		}
		if !ok {
			failed.Add(1)
		}
	}

	var wg sync.WaitGroup
	for i := 1; i <= workers; i++ {
		wg.Add(1)
		workerID := i
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					reportPanic(fmt.Sprintf("replay worker %d", workerID), r)
					for range queues[workerID] {
						pending.Done()
					}
				}
			}()
			for op := range queues[workerID] {
				replayOne(workerID, op)
			}
		}()
	}

	iteration := 0
	streamErr := workloadrec.Stream(runCtx, cfg.WorkloadReplayPath, func(op *workloadrec.OpLine) error {
		if op.Worker > workers {
			return fmt.Errorf("operation %d names worker %d of %d", op.Seq, op.Worker, workers)
		}
		if readsOnly && isReplayWrite(op) {
			skipped++
			if tgt.progress != nil {
				tgt.progress.Skipped.Add(1)
			}
			return nil
		}
		if it := op.OpIteration(); it != iteration {
			pending.Wait()
			iteration = it
		}
		pending.Add(1)
		select {
		case queues[op.Worker] <- op:
			return nil
		case <-runCtx.Done():
			pending.Done()
			return runCtx.Err()
		}
	})
	for i := 1; i <= workers; i++ {
		close(queues[i])
	}
	wg.Wait()
	elapsed := time.Since(replayStart)
	if collector != nil {
		collector.MarkWorkloadEnd()
	}
	close(monitorDone)

	rep.OpsReplayed, rep.OpsFailed = replayed.Load(), failed.Load()
	sums := map[int]string{}
	for i := 1; i <= workers; i++ {
		if hashes[i] != nil {
			sums[i] = hashes[i].Sum()
		}
	}
	rep.CommandsHash = workloadrec.CombineHashes(sums)
	rep.OpsSkipped = skipped
	switch {
	case skipped > 0:
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("commands hash not compared with the recording: %d write operations were skipped (workload_replay_ops=reads_only)", skipped))
	case info.Trailer != nil && info.Trailer.CommandsHash != "":
		match := rep.CommandsHash == info.Trailer.CommandsHash
		rep.CommandsHashMatch = &match
	}
	if measureLag && lag.n > 0 {
		maxMs := float64(lag.max.Microseconds()) / 1000
		avgMs := float64((lag.total / time.Duration(lag.n)).Microseconds()) / 1000
		rep.LagMaxMs, rep.LagAvgMs = &maxMs, &avgMs
	}

	panicMu.Lock()
	pErr := panicErr
	panicMu.Unlock()
	var runErr error
	switch {
	case pErr != nil:
		rep.Status, rep.Reason, runErr = workloadrec.StatusFailed, pErr.Error(), pErr
	case execErr != nil:
		rep.Status, rep.Reason, runErr = workloadrec.StatusFailed, execErr.Error(), fmt.Errorf("workload replay: %w", execErr)
	case streamErr != nil && ctx.Err() != nil:
		rep.Status, rep.Reason = replayStatusCancelled, "the run was stopped before the recording was exhausted"
	case streamErr != nil:
		rep.Status, rep.Reason, runErr = workloadrec.StatusFailed, streamErr.Error(), fmt.Errorf("workload replay: %w", streamErr)
	case ctx.Err() != nil && rep.OpsReplayed+rep.OpsSkipped < rep.OpsTotal:
		rep.Status, rep.Reason = replayStatusCancelled, "the run was stopped before the recording was exhausted"
	default:
		rep.Status = workloadrec.StatusComplete
	}
	if collector != nil {
		collector.SetWorkloadReplay(&rep)
	}
	if wCfg.outcomeFile != nil {
		res := wCfg.outcomeFile.Close(workloadrec.OutcomeTrailer{CommandsHash: rep.CommandsHash}, rep.Status, rep.Reason)
		fi := outcomeFileInfo(workloadrec.OutcomeSourceReplay, res, wCfg.checksums)
		fi.RecordingHeaderHash = info.HeaderHash
		if collector != nil {
			collector.SetWorkloadOutcomeFile(fi)
		}
		logOutcomeFile(res, wCfg.checksums)
	}
	msg := fmt.Sprintf("[Workload Replay] %s: %d of %d operations replayed (%d failed) in %s", rep.Status, rep.OpsReplayed, rep.OpsTotal, rep.OpsFailed, elapsed.Round(time.Millisecond))
	if tgt.label != "" {
		msg = fmt.Sprintf("[Workload Replay] target %s: %s", tgt.label, strings.TrimPrefix(msg, "[Workload Replay] "))
	}
	if rep.OpsFilter != "" {
		msg += fmt.Sprintf("; %d writes skipped (%s)", rep.OpsSkipped, rep.OpsFilter)
	}
	if rep.CommandsHashMatch != nil {
		msg += fmt.Sprintf("; commands hash match: %t", *rep.CommandsHashMatch)
	}
	if rep.LagMaxMs != nil {
		msg += fmt.Sprintf("; lag max %.1f ms avg %.1f ms", *rep.LagMaxMs, *rep.LagAvgMs)
	}
	if rep.Reason != "" {
		msg += " (" + rep.Reason + ")"
	}
	log.Print(msg)
	if runErr != nil {
		return runErr
	}
	if collector != nil {
		collector.PrintFinalSummary(elapsed, cfg.WebUI.Enabled)
	}
	return nil
}

// executeReplayOp decodes one recorded operation and sends it through the
// same execute functions a generated run uses.
func executeReplayOp(ctx context.Context, wCfg workloadConfig, workerID int, op *workloadrec.OpLine, cols replayCollections, out *workloadrec.Outcome) (bool, []byte, error) {
	cmd, err := workloadrec.DecodeCommand(op.Cmd)
	if err != nil {
		return false, nil, err
	}
	col := cols.lookup(op.NS)
	start := time.Now()
	switch op.Path {
	case workloadrec.PathFastInsertBatch:
		raw, err := bson.Marshal(cmd)
		if err != nil {
			return false, nil, err
		}
		docs, _ := docValue(cmd, "documents").(bson.A)
		ok := executeFastInsert(ctx, wCfg, workerID, col, raw, len(docs), start, nil, out)
		sent, err := workloadrec.EncodeCommand(bson.Raw(raw))
		return ok, sent, err
	case workloadrec.PathFastFind:
		patchKey, ids, limit, err := decodeFastFind(cmd)
		if err != nil {
			return false, nil, err
		}
		ok := executeFastFind(ctx, wCfg, workerID, col, patchKey, ids, limit, op.Existing, start, out)
		sent, err := workloadrec.EncodeCommand(fastFindRecord(col, patchKey, ids, limit))
		return ok, sent, err
	}
	if op.Op == "transaction" {
		ops, err := decodeTxOps(cmd, cols)
		if err != nil {
			return false, nil, err
		}
		ok := runTransaction(ctx, workerID, wCfg, nil, ops, out)
		recorded := bson.A{}
		for i := range ops {
			recorded = append(recorded, txInnerCommand(wCfg.database, &ops[i]))
		}
		sent, err := workloadrec.EncodeCommand(bson.D{{Key: "ops", Value: recorded}})
		return ok, sent, err
	}
	req, err := decodeStandard(op, cmd, col)
	if err != nil {
		return false, nil, err
	}
	ok, _ := executeStandard(ctx, wCfg, req, start, out)
	sent, err := workloadrec.EncodeCommand(standardCommand(req))
	return ok, sent, err
}

// replayShapeKey is the shape an outcome line of a replayed operation
// reports: the recorded shape, or the shape of its optimized path.
func replayShapeKey(op *workloadrec.OpLine) string {
	switch {
	case op.Shape != nil:
		return op.Shape.Key
	case op.Path == workloadrec.PathFastInsertBatch:
		return fastInsertShapeKey
	case op.Op == "transaction":
		return transactionShapeKey
	case op.Path == workloadrec.PathFastFind:
		if cmd, err := workloadrec.DecodeCommand(op.Cmd); err == nil {
			if _, _, limit, err := decodeFastFind(cmd); err == nil {
				return fastFindShapeKey(limit)
			}
		}
	}
	return ""
}

func docValue(d bson.D, key string) interface{} {
	for _, e := range d {
		if e.Key == key {
			return e.Value
		}
	}
	return nil
}

func hasKey(d bson.D, key string) bool {
	for _, e := range d {
		if e.Key == key {
			return true
		}
	}
	return false
}

func intValue(v interface{}) (int64, bool) {
	switch n := v.(type) {
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		return int64(n), n == float64(int64(n))
	}
	return 0, false
}

func docsValue(v interface{}) ([]interface{}, error) {
	a, ok := v.(bson.A)
	if !ok {
		return nil, fmt.Errorf("documents is %T, not an array", v)
	}
	return []interface{}(a), nil
}

// decodeStandard rebuilds the request of a recorded standardCommand.
func decodeStandard(op *workloadrec.OpLine, cmd bson.D, col config.CollectionDefinition) (*opRequest, error) {
	req := &opRequest{opType: op.Op, col: col, usedExisting: op.Existing, line: op}
	switch op.Op {
	case "find":
		req.filter = docValue(cmd, "filter")
		req.projection = map[string]interface{}(nil)
		if hasKey(cmd, "projection") {
			req.projection = docValue(cmd, "projection")
		}
		limit, ok := intValue(docValue(cmd, "limit"))
		if !ok {
			return nil, errors.New("find limit is missing")
		}
		batch, ok := intValue(docValue(cmd, "batchSize"))
		if !ok {
			return nil, errors.New("find batchSize is missing")
		}
		req.findLimit, req.findBatch = limit, int32(batch)
		req.findSort, req.hint = docValue(cmd, "sort"), docValue(cmd, "hint")
		if hasKey(cmd, "skip") {
			skip, ok := intValue(docValue(cmd, "skip"))
			if !ok || skip < 0 {
				return nil, errors.New("find skip is not a non-negative integer")
			}
			req.findSkip = skip
		}
	case "aggregate":
		p, ok := docValue(cmd, "pipeline").(bson.A)
		if !ok {
			return nil, errors.New("aggregate pipeline is missing")
		}
		req.pipeline = p
		req.hint = docValue(cmd, "hint")
	case "updateOne", "updateMany":
		req.filter, req.update = docValue(cmd, "filter"), docValue(cmd, "update")
		req.upsert, _ = docValue(cmd, "upsert").(bool)
	case "deleteOne", "deleteMany":
		req.filter = docValue(cmd, "filter")
	case "insert":
		req.doc = docValue(cmd, "document")
		if req.doc == nil {
			return nil, errors.New("insert document is missing")
		}
	case "insertMany":
		docs, err := docsValue(docValue(cmd, "documents"))
		if err != nil {
			return nil, err
		}
		req.docs = docs
	default:
		return nil, fmt.Errorf("unsupported operation %q", op.Op)
	}
	return req, nil
}

// decodeFastFind reads a recorded fast find: {find, filter: {key: {$in}}, limit}.
func decodeFastFind(cmd bson.D) (string, []int64, int, error) {
	filter, ok := docValue(cmd, "filter").(bson.D)
	if !ok || len(filter) != 1 {
		return "", nil, 0, errors.New("fast find filter is malformed")
	}
	in, ok := filter[0].Value.(bson.D)
	if !ok {
		return "", nil, 0, errors.New("fast find $in is malformed")
	}
	arr, ok := docValue(in, "$in").(bson.A)
	if !ok {
		return "", nil, 0, errors.New("fast find $in is malformed")
	}
	ids := make([]int64, 0, len(arr))
	for _, v := range arr {
		id, ok := v.(int64)
		if !ok {
			return "", nil, 0, fmt.Errorf("fast find id is %T, not int64", v)
		}
		ids = append(ids, id)
	}
	limit, ok := intValue(docValue(cmd, "limit"))
	if !ok {
		return "", nil, 0, errors.New("fast find limit is missing")
	}
	return filter[0].Key, ids, int(limit), nil
}

// decodeTxOps reads a recorded transaction: {ops: [{op, ns, args}, ...]}.
func decodeTxOps(cmd bson.D, cols replayCollections) ([]txInnerOp, error) {
	arr, ok := docValue(cmd, "ops").(bson.A)
	if !ok {
		return nil, errors.New("transaction ops are missing")
	}
	out := make([]txInnerOp, 0, len(arr))
	for _, x := range arr {
		d, ok := x.(bson.D)
		if !ok {
			return nil, fmt.Errorf("transaction op is %T", x)
		}
		opType, _ := docValue(d, "op").(string)
		ns, _ := docValue(d, "ns").(string)
		args, _ := docValue(d, "args").(bson.D)
		op := txInnerOp{opType: opType, col: cols.lookup(ns)}
		switch opType {
		case "find", "deleteOne", "deleteMany":
			op.filter = docValue(args, "filter")
		case "updateOne", "updateMany":
			op.filter, op.update = docValue(args, "filter"), docValue(args, "update")
			op.upsert, _ = docValue(args, "upsert").(bool)
		case "insert":
			op.doc = docValue(args, "document")
		case "insertMany":
			docs, err := docsValue(docValue(args, "documents"))
			if err != nil {
				return nil, err
			}
			op.docs = docs
		default:
			return nil, fmt.Errorf("unsupported transaction operation %q", opType)
		}
		out = append(out, op)
	}
	return out, nil
}
