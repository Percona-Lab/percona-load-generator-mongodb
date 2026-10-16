package mongo

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/datagen"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/workloadrec"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func withDataFingerprint(t *testing.T, fn func(ctx context.Context, db *mongo.Database, cols []config.CollectionDefinition, mode string) (*workloadrec.DataFingerprint, error)) {
	t.Helper()
	orig := dataFingerprintFn
	dataFingerprintFn = fn
	t.Cleanup(func() { dataFingerprintFn = orig })
}

// mixedOps has one operation of every kind; reads are named read-*.
func mixedOps() []recordedOp {
	shape := func(k string) *workloadrec.ShapeRef { return &workloadrec.ShapeRef{Key: k} }
	find := bson.D{{Key: "filter", Value: bson.D{{Key: "age", Value: int64(1)}}}, {Key: "limit", Value: int64(1)}, {Key: "batchSize", Value: int32(1)}}
	agg := func(stage string) bson.D {
		return bson.D{{Key: "pipeline", Value: bson.A{bson.D{{Key: "$match", Value: bson.D{{Key: "x", Value: "$out"}}}}, bson.D{{Key: stage, Value: "other"}}}}}
	}
	tx := func(ops ...string) bson.D {
		a := bson.A{}
		for _, o := range ops {
			a = append(a, bson.D{{Key: "op", Value: o}, {Key: "ns", Value: "appdb.users"}, {Key: "args", Value: bson.D{{Key: "filter", Value: bson.D{}}}}})
		}
		return bson.D{{Key: "ops", Value: a}}
	}
	e := func(worker int, op, path, key string, cmd bson.D) recordedOp {
		return recordedOp{worker: worker, entry: workloadrec.Entry{NS: "appdb.users", Op: op, Path: path, OK: true, Shape: shape(key), Command: cmd}}
	}
	return []recordedOp{
		e(1, "find", "", "read-find", find),
		e(2, "updateOne", "", "write-update", bson.D{{Key: "filter", Value: bson.D{}}, {Key: "update", Value: bson.D{{Key: "$set", Value: bson.D{{Key: "a", Value: 1}}}}}}),
		e(1, "aggregate", "", "read-agg", agg("$limit")),
		e(2, "aggregate", "", "write-agg-out", agg("$out")),
		e(1, "aggregate", "", "write-agg-merge", agg("$merge")),
		e(2, "insert", "", "write-insert", bson.D{{Key: "document", Value: bson.D{{Key: "_id", Value: 1}}}}),
		e(1, "insertMany", workloadrec.PathFastInsertBatch, "write-fast-insert", bson.D{{Key: "insert", Value: "users"}, {Key: "documents", Value: bson.A{}}}),
		e(2, "find", workloadrec.PathFastFind, "read-fast-find", bson.D{{Key: "find", Value: "users"}, {Key: "filter", Value: bson.D{{Key: "seq", Value: bson.D{{Key: "$in", Value: bson.A{int64(1)}}}}}}, {Key: "limit", Value: int64(1)}}),
		e(1, "transaction", "", "read-tx", tx("find", "find")),
		e(2, "transaction", "", "write-tx", tx("find", "deleteOne")),
		e(1, "deleteMany", "", "write-delete", bson.D{{Key: "filter", Value: bson.D{}}}),
	}
}

func TestReplayReadsOnlySkipsWritesAndCountsThem(t *testing.T) {
	ops := mixedOps()
	path, _ := recordThroughRunWorkload(t, ops, nil)
	var mu sync.Mutex
	var sent []string
	withReplayExec(t, func(ctx context.Context, wCfg workloadConfig, workerID int, op *workloadrec.OpLine, cols replayCollections, out *workloadrec.Outcome) (bool, []byte, error) {
		mu.Lock()
		sent = append(sent, op.Shape.Key)
		mu.Unlock()
		return true, op.Cmd, nil
	})
	cols, queries := recordReplayFixtures()
	outPath := filepath.Join(t.TempDir(), "out.ndjson")
	cfg := replayCfg(path)
	cfg.WorkloadReplayOps = config.WorkloadReplayOpsReadsOnly
	cfg.WorkloadRecordOutcomes, cfg.WorkloadReplayOutcomesPath = true, outPath
	collector := stats.NewCollector()
	var progress TargetProgress
	if err := RunWorkload(WithWorkloadTarget(context.Background(), "", false, &progress), nil, cols, queries, cfg, collector); err != nil {
		t.Fatal(err)
	}
	reads, writes := 0, 0
	for _, op := range ops {
		if strings.HasPrefix(op.entry.Shape.Key, "read-") {
			reads++
		} else {
			writes++
		}
	}
	if len(sent) != reads {
		t.Fatalf("sent %v, want only the %d reads", sent, reads)
	}
	for _, k := range sent {
		if !strings.HasPrefix(k, "read-") {
			t.Fatalf("reads_only replay sent a write: %s", k)
		}
	}
	rep := collector.WorkloadReplay()
	if rep.Status != workloadrec.StatusComplete || rep.OpsFilter != config.WorkloadReplayOpsReadsOnly || rep.OpsSkipped != uint64(writes) || rep.OpsReplayed != uint64(reads) {
		t.Fatalf("replay info = %+v", rep)
	}
	if progress.Sent.Load() != uint64(reads) || progress.Skipped.Load() != uint64(writes) {
		t.Fatalf("progress = %d sent, %d skipped", progress.Sent.Load(), progress.Skipped.Load())
	}
	if rep.CommandsHashMatch != nil || !strings.Contains(strings.Join(rep.Warnings, " "), "write operations were skipped") {
		t.Fatalf("a filtered replay must not claim a commands hash match: %+v", rep)
	}
	h, err := workloadrec.ReadOutcomeHeader(outPath)
	if err != nil || h.OpsFilter != config.WorkloadReplayOpsReadsOnly || h.Target != "" {
		t.Fatalf("outcome header = %+v, %v", h, err)
	}

	// Default ops replay every operation.
	sent = nil
	cfg = replayCfg(path)
	collector = stats.NewCollector()
	if err := RunWorkload(context.Background(), nil, cols, queries, cfg, collector); err != nil {
		t.Fatal(err)
	}
	if rep := collector.WorkloadReplay(); len(sent) != len(ops) || rep.OpsSkipped != 0 || rep.OpsFilter != "" || rep.CommandsHashMatch == nil || !*rep.CommandsHashMatch {
		t.Fatalf("default replay sent %d of %d: %+v", len(sent), len(ops), rep)
	}
}

func TestDataFingerprintRecordedAndCheckedBeforeReplay(t *testing.T) {
	calls := 0
	recorded := &workloadrec.DataFingerprint{Mode: workloadrec.FingerprintCounts, Collections: []workloadrec.CollectionFingerprint{
		{NS: "appdb.users", Count: 300, Indexes: []workloadrec.IndexFingerprint{{Name: "_id_", Spec: `{"key":{"_id":1}}`}}},
	}}
	current := recorded
	withDataFingerprint(t, func(ctx context.Context, db *mongo.Database, cols []config.CollectionDefinition, mode string) (*workloadrec.DataFingerprint, error) {
		calls++
		if mode != workloadrec.FingerprintCounts || len(cols) != 1 {
			return nil, fmt.Errorf("mode %q cols %d", mode, len(cols))
		}
		return current, nil
	})

	// Off (default): nothing is read and the header has no fingerprint.
	path, _ := recordThroughRunWorkload(t, sampleOps(), nil)
	if info, _ := workloadrec.Scan(path); info.Header.DataFingerprint != nil || calls != 0 {
		t.Fatalf("fingerprint with the setting off: %+v calls=%d", info.Header.DataFingerprint, calls)
	}

	path = filepath.Join(t.TempDir(), "fp.ndjson")
	cols, queries := recordReplayFixtures()
	noShardingSeams(t, func(ctx context.Context, wCfg workloadConfig) error {
		for _, op := range sampleOps() {
			e := op.entry
			e.Worker = op.worker
			recordOp(wCfg, e, time.Now())
		}
		return nil
	})
	rcfg := recordCfg(path)
	rcfg.WorkloadDataFingerprint = config.DataFingerprintCounts
	if err := RunWorkload(context.Background(), nil, cols, queries, rcfg, stats.NewCollector()); err != nil {
		t.Fatal(err)
	}
	info, err := workloadrec.Scan(path)
	if err != nil || !reflect.DeepEqual(info.Header.DataFingerprint, recorded) {
		t.Fatalf("header fingerprint = %+v, %v", info.Header.DataFingerprint, err)
	}

	withReplayExec(t, func(ctx context.Context, wCfg workloadConfig, workerID int, op *workloadrec.OpLine, cols replayCollections, out *workloadrec.Outcome) (bool, []byte, error) {
		return true, op.Cmd, nil
	})
	collector := stats.NewCollector()
	if err := RunWorkload(context.Background(), nil, cols, queries, replayCfg(path), collector); err != nil {
		t.Fatalf("matching data: %v", err)
	}
	if fp := collector.WorkloadReplay().DataFingerprint; fp == nil || !fp.Match || fp.Mode != workloadrec.FingerprintCounts {
		t.Fatalf("replay fingerprint check = %+v", fp)
	}

	changed := *recorded
	changed.Collections = []workloadrec.CollectionFingerprint{recorded.Collections[0]}
	changed.Collections[0].Count = 299
	current = &changed
	var sentAny atomic.Bool
	withReplayExec(t, func(ctx context.Context, wCfg workloadConfig, workerID int, op *workloadrec.OpLine, cols replayCollections, out *workloadrec.Outcome) (bool, []byte, error) {
		sentAny.Store(true)
		return true, op.Cmd, nil
	})
	err = RunWorkload(context.Background(), nil, cols, queries, replayCfg(path), stats.NewCollector())
	if err == nil || !strings.Contains(err.Error(), "300 documents recorded, 299 on this target") || !strings.Contains(err.Error(), "workload_replay_allow_mismatch") || sentAny.Load() {
		t.Fatalf("different starting data: err=%v sent=%v", err, sentAny.Load())
	}
	cfg := replayCfg(path)
	cfg.WorkloadReplayAllowMismatch = true
	collector = stats.NewCollector()
	if err := RunWorkload(context.Background(), nil, cols, queries, cfg, collector); err != nil {
		t.Fatalf("allowed mismatch: %v", err)
	}
	rep := collector.WorkloadReplay()
	if rep.DataFingerprint == nil || rep.DataFingerprint.Match || len(rep.Mismatches) != 1 || !sentAny.Load() {
		t.Fatalf("allowed fingerprint mismatch: %+v", rep)
	}
}

func TestIndexFingerprintsNormalizeServerTypes(t *testing.T) {
	a := indexFingerprintsFromSpecs([]bson.D{
		{{Key: "v", Value: int32(2)}, {Key: "key", Value: bson.D{{Key: "b", Value: int32(1)}, {Key: "a", Value: int32(-1)}}}, {Key: "name", Value: "b_1_a_-1"}, {Key: "unique", Value: true}},
		{{Key: "v", Value: int32(2)}, {Key: "key", Value: bson.D{{Key: "_id", Value: int32(1)}}}, {Key: "name", Value: "_id_"}},
	})
	b := indexFingerprintsFromSpecs([]bson.D{
		{{Key: "key", Value: bson.D{{Key: "_id", Value: float64(1)}}}, {Key: "name", Value: "_id_"}, {Key: "ns", Value: "x.y"}},
		{{Key: "key", Value: bson.D{{Key: "b", Value: int64(1)}, {Key: "a", Value: float64(-1)}}}, {Key: "name", Value: "b_1_a_-1"}, {Key: "unique", Value: true}, {Key: "v", Value: int32(1)}},
	})
	if !reflect.DeepEqual(a, b) || a[0].Name != "_id_" {
		t.Fatalf("index fingerprints differ:\n %+v\n %+v", a, b)
	}
	reordered := indexFingerprintsFromSpecs([]bson.D{{{Key: "key", Value: bson.D{{Key: "a", Value: int32(-1)}, {Key: "b", Value: int32(1)}}}, {Key: "name", Value: "b_1_a_-1"}, {Key: "unique", Value: true}}})
	if reordered[0].Spec == a[1].Spec {
		t.Fatal("key order must be part of the index spec")
	}
}

// TestConcurrentTargetReplaysAreIsolated replays one recording on three
// targets at once, as a concurrent multi-target run does: each has its own
// collector and outcome file, keeps every worker's order, and reports lag.
func TestConcurrentTargetReplaysAreIsolated(t *testing.T) {
	var ops []recordedOp
	for i := 0; i < 40; i++ {
		e := sampleOps()[0]
		e.worker = 1 + i%3
		e.entry.Command = bson.D{{Key: "filter", Value: bson.D{{Key: "age", Value: int64(i)}}}, {Key: "limit", Value: int64(1)}, {Key: "batchSize", Value: int32(1)}}
		ops = append(ops, e)
	}
	path, _ := recordThroughRunWorkload(t, ops, nil)
	type key struct {
		c      *stats.Collector
		worker int
	}
	var mu sync.Mutex
	got := map[key][]string{}
	withReplayExec(t, func(ctx context.Context, wCfg workloadConfig, workerID int, op *workloadrec.OpLine, cols replayCollections, out *workloadrec.Outcome) (bool, []byte, error) {
		wCfg.collector.Track("find", time.Millisecond)
		mu.Lock()
		got[key{wCfg.collector, workerID}] = append(got[key{wCfg.collector, workerID}], string(op.Cmd))
		mu.Unlock()
		return true, op.Cmd, nil
	})
	cols, queries := recordReplayFixtures()
	dir := t.TempDir()
	labels := []string{"m7", "m8", "m9"}
	collectors := make([]*stats.Collector, len(labels))
	errs := make([]error, len(labels))
	progress := make([]TargetProgress, len(labels))
	var wg sync.WaitGroup
	for i, label := range labels {
		collectors[i] = stats.NewCollector()
		cfg := replayCfg(path)
		cfg.WorkloadRecordOutcomes, cfg.WorkloadReplayOutcomesPath = true, config.TargetPath(filepath.Join(dir, "out.ndjson"), label)
		wg.Add(1)
		go func(i int, cfg *config.AppConfig) {
			defer wg.Done()
			errs[i] = RunWorkload(WithWorkloadTarget(context.Background(), labels[i], true, &progress[i]), nil, config.CloneCollections(cols), config.CloneQueries(queries), cfg, collectors[i])
		}(i, cfg)
	}
	wg.Wait()

	want := map[int][]string{}
	for _, op := range ops {
		enc, _ := workloadrec.EncodeCommand(op.entry.Command)
		want[op.worker] = append(want[op.worker], string(enc))
	}
	for i, c := range collectors {
		if errs[i] != nil {
			t.Fatalf("target %s: %v", labels[i], errs[i])
		}
		for w, cmds := range want {
			if !reflect.DeepEqual(got[key{c, w}], cmds) {
				t.Fatalf("target %s worker %d replayed %d commands out of order or mixed with another target", labels[i], w, len(got[key{c, w}]))
			}
		}
		rep := c.WorkloadReplay()
		if rep.Status != workloadrec.StatusComplete || rep.OpsReplayed != uint64(len(ops)) || rep.LagMaxMs == nil || rep.CommandsHashMatch == nil || !*rep.CommandsHashMatch {
			t.Fatalf("target %s replay = %+v", labels[i], rep)
		}
		if p := &progress[i]; p.Sent.Load() != uint64(len(ops)) || p.Total.Load() != uint64(len(ops)) || p.Skipped.Load() != 0 {
			t.Fatalf("target %s progress = %d/%d sent, %d skipped", labels[i], p.Sent.Load(), p.Total.Load(), p.Skipped.Load())
		}
		if n := c.FindOps; n != uint64(len(ops)) {
			t.Fatalf("target %s collector counted %d finds, want %d (stats must not mix targets)", labels[i], n, len(ops))
		}
		h, n := outcomeHeaderAndCount(t, config.TargetPath(filepath.Join(dir, "out.ndjson"), labels[i]))
		if h.Target != labels[i] || n != len(ops) {
			t.Fatalf("target %s outcome file: target %q, %d lines", labels[i], h.Target, n)
		}
	}
}

func TestConcurrentDeterministicTargetsSendIdenticalTraffic(t *testing.T) {
	t.Cleanup(func() { datagen.ConfigureSeed(0) })
	noShardingSeams(t, nil)
	orig := deterministicExecFn
	deterministicExecFn = func(ctx context.Context, wCfg workloadConfig, w *detWorker, op *detOp, out *workloadrec.Outcome) bool {
		if out != nil {
			out.OK, out.LatencyUS, op.shape = true, int64(op.seq), op.opType+"|fake"
		}
		return true
	}
	t.Cleanup(func() { deterministicExecFn = orig })
	dir := t.TempDir()
	labels := []string{"m8", "m9"}
	// RunWorkload normalizes index keys in place: shared definitions race.
	cols, queries := detCollections(), detQueries()
	cols[len(cols)-1].Indexes = []config.IndexDefinition{{Keys: config.IndexKeys{{Field: "age", Value: 1}}}}
	infos := make([]*stats.GeneratedTrafficInfo, len(labels))
	errs := make([]error, len(labels))
	var wg sync.WaitGroup
	for i, label := range labels {
		cfg := detCfg()
		cfg.WorkloadRecordOutcomes, cfg.WorkloadDeterministicOutcomesPath = true, config.TargetPath(filepath.Join(dir, "det.ndjson"), label)
		wg.Add(1)
		go func(i int, cfg *config.AppConfig) {
			defer wg.Done()
			c := stats.NewCollector()
			ctx, finish := WithWorkloadRecordingSession(WithWorkloadTarget(context.Background(), labels[i], true, nil))
			errs[i] = RunWorkload(ctx, nil, config.CloneCollections(cols), config.CloneQueries(queries), cfg, c)
			finish()
			infos[i] = c.GeneratedTraffic()
		}(i, cfg)
	}
	wg.Wait()
	for i := range labels {
		if errs[i] != nil || infos[i] == nil || infos[i].Status != "complete" {
			t.Fatalf("target %s: %v %+v", labels[i], errs[i], infos[i])
		}
		h, n := outcomeHeaderAndCount(t, config.TargetPath(filepath.Join(dir, "det.ndjson"), labels[i]))
		if h.Target != labels[i] || uint64(n) != infos[i].OpsSent {
			t.Fatalf("target %s outcome file: target %q, %d lines of %d sent", labels[i], h.Target, n, infos[i].OpsSent)
		}
	}
	if infos[0].Fingerprint != infos[1].Fingerprint || !reflect.DeepEqual(infos[0].WorkerHashes, infos[1].WorkerHashes) {
		t.Fatalf("concurrent deterministic targets sent different traffic:\n%+v\n%+v", infos[0], infos[1])
	}
}

func outcomeHeaderAndCount(t *testing.T, path string) (*workloadrec.OutcomeHeader, int) {
	t.Helper()
	n := 0
	h, _, err := workloadrec.StreamOutcomes(context.Background(), path, func(*workloadrec.OutcomeLine) error { n++; return nil })
	if err != nil {
		t.Fatal(err)
	}
	return h, n
}
