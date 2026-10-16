package mongo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/workloadrec"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func recordReplayFixtures() ([]config.CollectionDefinition, []config.QueryDefinition) {
	cols := []config.CollectionDefinition{{Name: "users", DatabaseName: "appdb", Fields: map[string]config.CollectionField{"age": {Type: "int"}}}}
	queries := []config.QueryDefinition{
		{Name: "find_users", Database: "appdb", Operation: "find", Collection: "users", Filter: map[string]interface{}{"age": map[string]interface{}{"$gte": 1}}},
	}
	return cols, queries
}

func withReplayExec(t *testing.T, fn func(ctx context.Context, wCfg workloadConfig, workerID int, op *workloadrec.OpLine, cols replayCollections, out *workloadrec.Outcome) (bool, []byte, error)) {
	t.Helper()
	orig := replayExecFn
	replayExecFn = fn
	t.Cleanup(func() { replayExecFn = orig })
}

func noShardingSeams(t *testing.T, runContinuous func(ctx context.Context, wCfg workloadConfig) error) {
	withMongoSeams(t, mongoSeams{
		ensureGenericSharding: func(ctx context.Context, db *mongo.Database, col config.CollectionDefinition, cfg *config.AppConfig) error {
			return nil
		},
		getPrimaryFilterField: func(ctx context.Context, db *mongo.Database, col config.CollectionDefinition) string { return "age" },
		runContinuousWorkload: runContinuous,
	})
}

func recordCfg(path string) *config.AppConfig {
	cfg := &config.AppConfig{Duration: "1s", Concurrency: 2, RandomSeed: 99, StatusRefreshRateSec: 1, WorkloadRecordEnabled: true, WorkloadRecordPath: path, WorkloadRecordMaxBytes: config.DefaultWorkloadRecordMaxBytes}
	cfg.WebUI.Enabled = true
	config.NormalizeWorkloadRecordReplay(cfg)
	return cfg
}

func replayCfg(path string) *config.AppConfig {
	cfg := &config.AppConfig{Duration: "1s", Concurrency: 2, RandomSeed: 99, StatusRefreshRateSec: 1, WorkloadReplayEnabled: true, WorkloadReplayPath: path}
	cfg.WebUI.Enabled = true
	config.NormalizeWorkloadRecordReplay(cfg)
	return cfg
}

type recordedOp struct {
	worker int
	entry  workloadrec.Entry
}

// recordThroughRunWorkload runs RunWorkload with recording on, with the
// worker loop replaced by ops, and returns the recording path.
func recordThroughRunWorkload(t *testing.T, ops []recordedOp, offsets []time.Duration) (string, *stats.Collector) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rec.ndjson")
	cols, queries := recordReplayFixtures()
	noShardingSeams(t, func(ctx context.Context, wCfg workloadConfig) error {
		if wCfg.recorder == nil {
			t.Fatal("recording on but no recorder was passed to the workers")
		}
		start := time.Now()
		wCfg.recorder.SetStart(start)
		for i, op := range ops {
			e := op.entry
			e.Worker = op.worker
			at := start
			if offsets != nil {
				at = start.Add(offsets[i])
			}
			recordOp(wCfg, e, at)
		}
		return nil
	})
	collector := stats.NewCollector()
	if err := RunWorkload(context.Background(), nil, cols, queries, recordCfg(path), collector); err != nil {
		t.Fatalf("RunWorkload(record) error = %v", err)
	}
	return path, collector
}

func sampleOps() []recordedOp {
	findCmd := func(i int) bson.D {
		return bson.D{{Key: "filter", Value: map[string]interface{}{"age": int64(i)}}, {Key: "limit", Value: int64(10)}, {Key: "batchSize", Value: int32(10)}}
	}
	shape := &workloadrec.ShapeRef{Key: "find|age|limit:10", Summary: "find age", Fields: []string{"age"}}
	query := &workloadrec.QueryRef{Name: "find_users", Index: 0, Label: "find_users"}
	var ops []recordedOp
	for i := 0; i < 6; i++ {
		ops = append(ops, recordedOp{worker: 1 + i%2, entry: workloadrec.Entry{NS: "appdb.users", Op: "find", OK: true, Query: query, Shape: shape, Command: findCmd(i)}})
	}
	return ops
}

func TestRunWorkloadRecordsHeaderOpsAndMetadata(t *testing.T) {
	path, collector := recordThroughRunWorkload(t, sampleOps(), nil)
	info, err := workloadrec.Scan(path)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	h := info.Header
	if info.Status != workloadrec.StatusComplete || info.Ops != 6 {
		t.Fatalf("scan = %+v", info)
	}
	if h.Workers != 2 || h.RandomSeed != 99 || h.PLGMVersion != config.BuildVersion || len(h.CollectionsHash) != 64 || len(h.QueriesHash) != 64 {
		t.Fatalf("header = %+v", h)
	}
	if strings.Join(h.Namespaces, ",") != "appdb.users" || h.FailurePolicy != workloadrec.FailurePolicy {
		t.Fatalf("header namespaces/policy = %v / %q", h.Namespaces, h.FailurePolicy)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("recording mode = %v, want 0600", fi.Mode().Perm())
	}
	md := collector.GetFinalInsights().Metadata
	if md.Recording == nil || md.Recording.Status != workloadrec.StatusComplete || md.Recording.OpsWritten != 6 || md.Recording.Path != path {
		t.Fatalf("recording metadata = %+v", md.Recording)
	}
	raw, _ := json.Marshal(md)
	if bytes.Contains(raw, []byte(`"age":`)) {
		t.Fatalf("insights metadata leaks recorded values: %s", raw)
	}
}

func TestRunWorkloadDefaultsDoNotRecord(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	cols, queries := recordReplayFixtures()
	var captured workloadConfig
	noShardingSeams(t, func(ctx context.Context, wCfg workloadConfig) error { captured = wCfg; return nil })
	collector := stats.NewCollector()
	cfg := &config.AppConfig{Duration: "1s", Concurrency: 2}
	if err := RunWorkload(context.Background(), nil, cols, queries, cfg, collector); err != nil {
		t.Fatal(err)
	}
	if captured.recorder != nil {
		t.Fatal("a recorder was created with recording off")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("files written with recording off: %v", entries)
	}
	md := collector.GetFinalInsights().Metadata
	if md.Recording != nil || md.Replay != nil {
		t.Fatalf("metadata must not carry recording/replay with defaults: %+v %+v", md.Recording, md.Replay)
	}
}

func TestRunWorkloadRejectsRecordAndReplayTogether(t *testing.T) {
	cols, queries := recordReplayFixtures()
	noShardingSeams(t, func(ctx context.Context, wCfg workloadConfig) error {
		t.Fatal("the workload must not start")
		return nil
	})
	cfg := recordCfg(filepath.Join(t.TempDir(), "r.ndjson"))
	cfg.WorkloadReplayEnabled, cfg.WorkloadReplayPath = true, "x.ndjson"
	err := RunWorkload(context.Background(), nil, cols, queries, cfg, stats.NewCollector())
	if err == nil || !strings.Contains(err.Error(), "cannot both be on") {
		t.Fatalf("RunWorkload error = %v", err)
	}
}

func TestReplayExecutesRecordedCommandsPerWorkerInOrder(t *testing.T) {
	ops := sampleOps()
	path, _ := recordThroughRunWorkload(t, ops, nil)

	var mu sync.Mutex
	got := map[int][]string{}
	withReplayExec(t, func(ctx context.Context, wCfg workloadConfig, workerID int, op *workloadrec.OpLine, cols replayCollections, out *workloadrec.Outcome) (bool, []byte, error) {
		if workerID != op.Worker {
			return false, nil, fmt.Errorf("op of worker %d ran on worker %d", op.Worker, workerID)
		}
		mu.Lock()
		got[workerID] = append(got[workerID], string(op.Cmd))
		mu.Unlock()
		return true, op.Cmd, nil
	})
	noShardingSeams(t, func(ctx context.Context, wCfg workloadConfig) error {
		t.Fatal("replay must not generate operations")
		return nil
	})
	cols, queries := recordReplayFixtures()
	collector := stats.NewCollector()
	if err := RunWorkload(context.Background(), nil, cols, queries, replayCfg(path), collector); err != nil {
		t.Fatalf("RunWorkload(replay) error = %v", err)
	}

	want := map[int][]string{}
	for _, op := range ops {
		enc, _ := workloadrec.EncodeCommand(op.entry.Command)
		want[op.worker] = append(want[op.worker], string(enc))
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("replayed commands differ:\n got %v\nwant %v", got, want)
	}
	rep := collector.WorkloadReplay()
	if rep == nil || rep.Status != workloadrec.StatusComplete || rep.OpsTotal != 6 || rep.OpsReplayed != 6 || rep.OpsFailed != 0 || rep.Workers != 2 {
		t.Fatalf("replay info = %+v", rep)
	}
	if rep.CommandsHashMatch == nil || !*rep.CommandsHashMatch || rep.DurationIgnored != "1s" || rep.Mode != config.WorkloadReplayModePerWorker {
		t.Fatalf("replay info = %+v", rep)
	}
	if len(rep.HeaderHash) != 64 || rep.FileName != "rec.ndjson" {
		t.Fatalf("replay identity = %q %q", rep.HeaderHash, rep.FileName)
	}
}

func TestReplayHeaderMismatchFailsUnlessAllowed(t *testing.T) {
	path, _ := recordThroughRunWorkload(t, sampleOps(), nil)
	withReplayExec(t, func(ctx context.Context, wCfg workloadConfig, workerID int, op *workloadrec.OpLine, cols replayCollections, out *workloadrec.Outcome) (bool, []byte, error) {
		return true, op.Cmd, nil
	})
	cols, queries := recordReplayFixtures()
	cols[0].Fields["name"] = config.CollectionField{Type: "string"}

	err := RunWorkload(context.Background(), nil, cols, queries, replayCfg(path), stats.NewCollector())
	if err == nil || !strings.Contains(err.Error(), "collections_hash") || !strings.Contains(err.Error(), "workload_replay_allow_mismatch") {
		t.Fatalf("RunWorkload error = %v, want header mismatch", err)
	}

	cfg := replayCfg(path)
	cfg.WorkloadReplayAllowMismatch = true
	collector := stats.NewCollector()
	if err := RunWorkload(context.Background(), nil, cols, queries, cfg, collector); err != nil {
		t.Fatalf("allowed mismatch: %v", err)
	}
	if rep := collector.WorkloadReplay(); rep == nil || len(rep.Mismatches) != 1 || rep.Status != workloadrec.StatusComplete {
		t.Fatalf("replay info = %+v", rep)
	}
}

func TestReplayReportsTruncatedAndRejectsFailedRecordings(t *testing.T) {
	cols, queries := recordReplayFixtures()
	withReplayExec(t, func(ctx context.Context, wCfg workloadConfig, workerID int, op *workloadrec.OpLine, cols replayCollections, out *workloadrec.Outcome) (bool, []byte, error) {
		return true, op.Cmd, nil
	})
	noShardingSeams(t, nil)

	write := func(t *testing.T, maxBytes int64, fail bool, n int) string {
		h, err := workloadRecordHeader(context.Background(), nil, cols, mustBind(t, cols, queries), recordCfg(""), 1)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "r.ndjson")
		rec, err := workloadrec.NewRecorder(path, workloadrec.Options{MaxBytes: maxBytes}, h)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < n; i++ {
			rec.Record(workloadrec.Entry{Worker: 1, NS: "appdb.users", Op: "find", Command: bson.D{{Key: "filter", Value: bson.D{{Key: "pad", Value: strings.Repeat("x", 200)}}}, {Key: "limit", Value: int64(1)}, {Key: "batchSize", Value: int32(1)}}})
		}
		if fail {
			rec.Fail("simulated")
		}
		rec.Close()
		return path
	}

	truncated := write(t, 4000, false, 50)
	collector := stats.NewCollector()
	if err := RunWorkload(context.Background(), nil, cols, queries, replayCfg(truncated), collector); err != nil {
		t.Fatalf("truncated replay: %v", err)
	}
	rep := collector.WorkloadReplay()
	if rep.SourceStatus != workloadrec.StatusTruncated || rep.OpsReplayed == 0 || rep.OpsReplayed != rep.OpsTotal || len(rep.Warnings) == 0 {
		t.Fatalf("truncated replay info = %+v", rep)
	}

	failed := write(t, 0, true, 2)
	err := RunWorkload(context.Background(), nil, cols, queries, replayCfg(failed), stats.NewCollector())
	if err == nil || !strings.Contains(err.Error(), `status is "failed"`) {
		t.Fatalf("failed recording replay error = %v", err)
	}
}

func mustBind(t *testing.T, cols []config.CollectionDefinition, queries []config.QueryDefinition) []config.QueryDefinition {
	t.Helper()
	qs := append([]config.QueryDefinition(nil), queries...)
	if err := config.NormalizeAndValidateQueries(qs); err != nil {
		t.Fatal(err)
	}
	bound, err := config.ValidateAndBindQueriesToCollections(qs, cols)
	if err != nil {
		t.Fatal(err)
	}
	return bound
}

func TestReplayTimedModeWaitsForOffsetsAndReportsLag(t *testing.T) {
	ops := sampleOps()[:2]
	path, _ := recordThroughRunWorkload(t, ops, []time.Duration{0, 60 * time.Millisecond})
	var mu sync.Mutex
	var issued []time.Time
	withReplayExec(t, func(ctx context.Context, wCfg workloadConfig, workerID int, op *workloadrec.OpLine, cols replayCollections, out *workloadrec.Outcome) (bool, []byte, error) {
		mu.Lock()
		issued = append(issued, time.Now())
		mu.Unlock()
		return true, op.Cmd, nil
	})
	cols, queries := recordReplayFixtures()
	cfg := replayCfg(path)
	cfg.WorkloadReplayMode = config.WorkloadReplayModeTimed
	collector := stats.NewCollector()
	start := time.Now()
	if err := RunWorkload(context.Background(), nil, cols, queries, cfg, collector); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 60*time.Millisecond {
		t.Fatalf("timed replay finished in %s, before the last recorded offset", elapsed)
	}
	rep := collector.WorkloadReplay()
	if rep.LagMaxMs == nil || rep.LagAvgMs == nil || *rep.LagMaxMs < 0 || len(issued) != 2 {
		t.Fatalf("timed replay info = %+v issued=%d", rep, len(issued))
	}
}

func TestReplayCancelledRunIsReported(t *testing.T) {
	path, _ := recordThroughRunWorkload(t, sampleOps(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	withReplayExec(t, func(c context.Context, wCfg workloadConfig, workerID int, op *workloadrec.OpLine, cols replayCollections, out *workloadrec.Outcome) (bool, []byte, error) {
		cancel()
		return true, op.Cmd, nil
	})
	cols, queries := recordReplayFixtures()
	collector := stats.NewCollector()
	if err := RunWorkload(ctx, nil, cols, queries, replayCfg(path), collector); err != nil {
		t.Fatalf("cancelled replay returned %v", err)
	}
	if rep := collector.WorkloadReplay(); rep.Status != "cancelled" || rep.OpsReplayed >= rep.OpsTotal {
		t.Fatalf("cancelled replay info = %+v", rep)
	}
}

// TestRecordedCommandsDecodeToTheSameCommand checks every recorded form:
// the replay rebuilds a request whose recorded form is byte-identical.
func TestRecordedCommandsDecodeToTheSameCommand(t *testing.T) {
	col := config.CollectionDefinition{DatabaseName: "appdb", Name: "users"}
	oid := bson.NewObjectID()
	sortSpec := bson.D{{Key: "z", Value: -1}, {Key: "a", Value: 1}}
	reqs := []*opRequest{
		{opType: "find", filter: map[string]interface{}{"_id": oid}, projection: map[string]interface{}(nil), findLimit: 5, findBatch: 10},
		{opType: "find", filter: map[string]interface{}{"age": map[string]interface{}{"$gt": int32(3)}}, projection: map[string]interface{}{"age": 1}, findLimit: 1, findBatch: 10},
		{opType: "aggregate", pipeline: []interface{}{map[string]interface{}{"$match": map[string]interface{}{"age": 1}}, map[string]interface{}{"$sort": sortSpec}}},
		{opType: "updateOne", filter: map[string]interface{}{"_id": oid}, update: map[string]interface{}{"$inc": map[string]interface{}{"age": 1}}, upsert: true},
		{opType: "updateMany", filter: map[string]interface{}{}, update: map[string]interface{}{"$set": map[string]interface{}{"at": time.Unix(1700000000, 0).UTC()}}},
		{opType: "deleteOne", filter: map[string]interface{}{"age": int64(9)}},
		{opType: "deleteMany", filter: map[string]interface{}(nil)},
		{opType: "insert", doc: withDocumentID(map[string]interface{}{"age": int32(1)})},
		{opType: "insertMany", docs: withDocumentIDs([]interface{}{map[string]interface{}{"age": int32(1)}, map[string]interface{}{"age": int32(2)}})},
	}
	for _, req := range reqs {
		t.Run(req.opType, func(t *testing.T) {
			req.col = col
			enc, err := workloadrec.EncodeCommand(standardCommand(req))
			if err != nil {
				t.Fatal(err)
			}
			dec, err := workloadrec.DecodeCommand(enc)
			if err != nil {
				t.Fatal(err)
			}
			line := &workloadrec.OpLine{Op: req.opType, NS: "appdb.users", Cmd: enc, Shape: &workloadrec.ShapeRef{Key: "k|" + req.opType}}
			back, err := decodeStandard(line, dec, col)
			if err != nil {
				t.Fatal(err)
			}
			again, _ := workloadrec.EncodeCommand(standardCommand(back))
			if !bytes.Equal(enc, again) {
				t.Fatalf("recorded %s, replay would send %s", enc, again)
			}
			if id := back.identity(); id.shapeKey != "k|"+req.opType {
				t.Fatalf("replayed identity = %+v, want the recorded shape", id)
			}
		})
	}

	if req := reqs[2]; true {
		dec, _ := workloadrec.DecodeCommand(mustEncode(t, standardCommand(req)))
		back, _ := decodeStandard(&workloadrec.OpLine{Op: "aggregate"}, dec, col)
		stage := back.runtimePipeline()[1].(map[string]interface{})
		spec, ok := stage["$sort"].(bson.D)
		if !ok || spec[0].Key != "z" {
			t.Fatalf("runtime $sort = %#v, want ordered bson.D", stage["$sort"])
		}
	}

	tx := []txInnerOp{
		{opType: "find", col: col, filter: map[string]interface{}{"age": 1}},
		{opType: "updateOne", col: col, filter: map[string]interface{}{"age": 1}, update: map[string]interface{}{"$set": map[string]interface{}{"age": 2}}},
		{opType: "insert", col: col, doc: withDocumentID(map[string]interface{}{"age": 3})},
		{opType: "insertMany", col: col, docs: withDocumentIDs([]interface{}{map[string]interface{}{"age": 4}})},
		{opType: "deleteMany", col: col, filter: map[string]interface{}{"age": 5}},
	}
	recorded := bson.A{}
	for i := range tx {
		recorded = append(recorded, txInnerCommand(nil, &tx[i]))
	}
	txEnc := mustEncode(t, bson.D{{Key: "ops", Value: recorded}})
	txDec, _ := workloadrec.DecodeCommand(txEnc)
	backTx, err := decodeTxOps(txDec, newReplayCollections(nil, []config.CollectionDefinition{col}))
	if err != nil {
		t.Fatal(err)
	}
	again := bson.A{}
	for i := range backTx {
		again = append(again, txInnerCommand(nil, &backTx[i]))
	}
	if got := mustEncode(t, bson.D{{Key: "ops", Value: again}}); !bytes.Equal(got, txEnc) {
		t.Fatalf("transaction recorded %s, replay would send %s", txEnc, got)
	}

	ffEnc := mustEncode(t, fastFindRecord(col, "seq", []int64{100000001, 7}, 2))
	ffDec, _ := workloadrec.DecodeCommand(ffEnc)
	key, ids, limit, err := decodeFastFind(ffDec)
	if err != nil || key != "seq" || !reflect.DeepEqual(ids, []int64{100000001, 7}) || limit != 2 {
		t.Fatalf("fast find decoded %q %v %d %v", key, ids, limit, err)
	}

	raw, _ := bson.Marshal(bson.D{{Key: "insert", Value: "users"}, {Key: "documents", Value: bson.A{bson.D{{Key: "seq", Value: int64(1)}}}}})
	withIDs, err := withBatchIDs(raw)
	if err != nil {
		t.Fatal(err)
	}
	fiEnc := mustEncode(t, bson.Raw(withIDs))
	fiDec, _ := workloadrec.DecodeCommand(fiEnc)
	fiRaw, _ := bson.Marshal(fiDec)
	if !bytes.Equal(fiRaw, withIDs) {
		t.Fatal("fast insert batch does not decode to the sent bytes")
	}
	if docs := docValue(fiDec, "documents").(bson.A); docs[0].(bson.D)[0].Key != "_id" {
		t.Fatalf("fast insert documents lack _id: %v", docs)
	}
}

func mustEncode(t *testing.T, v interface{}) []byte {
	t.Helper()
	b, err := workloadrec.EncodeCommand(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestWithDocumentIDKeepsOriginalAndExistingID(t *testing.T) {
	doc := map[string]interface{}{"a": 1}
	out := withDocumentID(doc)
	if _, ok := doc["_id"]; ok {
		t.Fatal("original document was changed")
	}
	if _, ok := out["_id"].(bson.ObjectID); !ok {
		t.Fatalf("no _id added: %v", out)
	}
	has := map[string]interface{}{"_id": "fixed"}
	if got := withDocumentID(has); got["_id"] != "fixed" {
		t.Fatalf("existing _id replaced: %v", got)
	}
}
