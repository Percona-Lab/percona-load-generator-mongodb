package mongo

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/workloadrec"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func iterationOps(iteration, n int) []recordedOp {
	shape := &workloadrec.ShapeRef{Key: "find|age|limit:10", Summary: "find age", Fields: []string{"age"}}
	var ops []recordedOp
	for i := 0; i < n; i++ {
		cmd := bson.D{{Key: "filter", Value: map[string]interface{}{"age": int64(iteration*100 + i)}}, {Key: "limit", Value: int64(10)}, {Key: "batchSize", Value: int32(10)}}
		ops = append(ops, recordedOp{worker: 1 + i%2, entry: workloadrec.Entry{NS: "appdb.users", Op: "find", OK: true, Shape: shape, Command: cmd}})
	}
	return ops
}

// recordIterations runs one RunWorkload call per element of iterations
// inside a recording session, the way the CLI and Web UI loops do, with gap
// between calls. It returns the path and the collector before and after
// the session finished.
func recordIterations(t *testing.T, cfg *config.AppConfig, iterations [][]recordedOp, gap time.Duration) (string, *stats.WorkloadRecordingInfo, *stats.Collector) {
	t.Helper()
	cols, queries := recordReplayFixtures()
	call := 0
	noShardingSeams(t, func(ctx context.Context, wCfg workloadConfig) error {
		start := time.Now()
		wCfg.recorder.SetStart(start)
		for _, op := range iterations[call] {
			e := op.entry
			e.Worker = op.worker
			recordOp(wCfg, e, time.Now())
		}
		call++
		return nil
	})
	cfg.Iterations = len(iterations)
	ctx, finish := WithWorkloadRecordingSession(context.Background())
	collector := stats.NewCollector()
	var during *stats.WorkloadRecordingInfo
	for i := range iterations {
		if i > 0 {
			time.Sleep(gap)
		}
		if err := RunWorkload(ctx, nil, cols, queries, cfg, collector); err != nil {
			t.Fatalf("RunWorkload iteration %d: %v", i+1, err)
		}
		during = collector.GetFinalInsights().Metadata.Recording
	}
	finish()
	finish()
	return cfg.WorkloadRecordPath, during, collector
}

func TestRecordingSessionSpansIterations(t *testing.T) {
	cfg := recordCfg(filepath.Join(t.TempDir(), "rec.ndjson"))
	cfg.IntervalDelay = "20ms"
	path, during, collector := recordIterations(t, cfg, [][]recordedOp{iterationOps(1, 3), iterationOps(2, 2)}, 25*time.Millisecond)

	if during == nil || during.Status != workloadrec.StatusRecording {
		t.Fatalf("recording before the session finished = %+v, want still recording", during)
	}
	info, err := workloadrec.Scan(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != workloadrec.StatusComplete || info.Ops != 5 || info.Header.Iterations != 2 || info.Header.IntervalDelay != "20ms" {
		t.Fatalf("scan = %+v", info)
	}
	if info.IterationOps[1] != 3 || info.IterationOps[2] != 2 {
		t.Fatalf("iteration ops = %v", info.IterationOps)
	}
	var iters []int
	var offsets []int64
	if err := workloadrec.Stream(context.Background(), path, func(op *workloadrec.OpLine) error {
		iters, offsets = append(iters, op.Iteration), append(offsets, op.OffsetUS)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(iters) != "[1 1 1 2 2]" {
		t.Fatalf("iterations = %v", iters)
	}
	if gap := offsets[3] - offsets[2]; gap < (25 * time.Millisecond).Microseconds() {
		t.Fatalf("offset gap between iterations = %d us, want >= the 25 ms between calls", gap)
	}
	md := collector.GetFinalInsights().Metadata.Recording
	if md == nil || md.Status != workloadrec.StatusComplete || md.OpsWritten != 5 || md.Iterations != 2 {
		t.Fatalf("final recording metadata = %+v", md)
	}
}

func TestRecordingIterationsWithoutSessionFails(t *testing.T) {
	cols, queries := recordReplayFixtures()
	noShardingSeams(t, func(ctx context.Context, wCfg workloadConfig) error {
		t.Fatal("the workload must not start")
		return nil
	})
	cfg := recordCfg(filepath.Join(t.TempDir(), "rec.ndjson"))
	cfg.Iterations = 2
	err := RunWorkload(context.Background(), nil, cols, queries, cfg, stats.NewCollector())
	if err == nil || !strings.Contains(err.Error(), "WithWorkloadRecordingSession") {
		t.Fatalf("RunWorkload error = %v", err)
	}
}

// TestReplayRunsRecordedIterationsInOrder checks the per_worker barrier: no
// operation of iteration 2 starts before every operation of iteration 1
// finished, even when one worker is slow.
func TestReplayRunsRecordedIterationsInOrder(t *testing.T) {
	cfg := recordCfg(filepath.Join(t.TempDir(), "rec.ndjson"))
	path, _, _ := recordIterations(t, cfg, [][]recordedOp{iterationOps(1, 4), iterationOps(2, 4), iterationOps(3, 2)}, 0)

	type span struct {
		iteration  int
		start, end time.Time
	}
	var mu sync.Mutex
	var spans []span
	withReplayExec(t, func(ctx context.Context, wCfg workloadConfig, workerID int, op *workloadrec.OpLine, cols replayCollections, out *workloadrec.Outcome) (bool, []byte, error) {
		s := span{iteration: op.OpIteration(), start: time.Now()}
		if workerID == 2 && s.iteration == 1 {
			time.Sleep(30 * time.Millisecond)
		}
		s.end = time.Now()
		mu.Lock()
		spans = append(spans, s)
		mu.Unlock()
		return true, op.Cmd, nil
	})
	noShardingSeams(t, func(ctx context.Context, wCfg workloadConfig) error {
		t.Fatal("replay must not generate operations")
		return nil
	})
	cols, queries := recordReplayFixtures()
	rcfg := replayCfg(path)
	rcfg.Iterations = 3
	collector := stats.NewCollector()
	if err := RunWorkload(context.Background(), nil, cols, queries, rcfg, collector); err != nil {
		t.Fatalf("replay: %v", err)
	}
	for _, a := range spans {
		for _, b := range spans {
			if b.iteration > a.iteration && b.start.Before(a.end) {
				t.Fatalf("an iteration %d op started before an iteration %d op finished", b.iteration, a.iteration)
			}
		}
	}
	rep := collector.WorkloadReplay()
	if rep.Status != workloadrec.StatusComplete || rep.OpsReplayed != 10 || rep.Iterations != 3 || rep.CommandsHashMatch == nil || !*rep.CommandsHashMatch {
		t.Fatalf("replay info = %+v", rep)
	}
	var warned bool
	for _, w := range rep.Warnings {
		warned = warned || strings.Contains(w, "iterations=3 is ignored")
	}
	if !warned {
		t.Fatalf("configured iterations during replay must be reported as ignored: %v", rep.Warnings)
	}
}

func TestReplayTimedKeepsTheGapBetweenIterations(t *testing.T) {
	cols, queries := recordReplayFixtures()
	h, err := workloadRecordHeader(context.Background(), nil, cols, mustBind(t, cols, queries), recordCfg(""), 1)
	if err != nil {
		t.Fatal(err)
	}
	h.Iterations, h.IntervalDelay = 2, "80ms"
	path := filepath.Join(t.TempDir(), "r.ndjson")
	rec, err := workloadrec.NewRecorder(path, workloadrec.Options{}, h)
	if err != nil {
		t.Fatal(err)
	}
	cmd := bson.D{{Key: "filter", Value: bson.D{}}, {Key: "limit", Value: int64(1)}, {Key: "batchSize", Value: int32(1)}}
	rec.Record(workloadrec.Entry{Worker: 1, NS: "appdb.users", Op: "find", Command: cmd})
	rec.SetIteration(2)
	rec.Record(workloadrec.Entry{Worker: 1, NS: "appdb.users", Op: "find", Offset: 90 * time.Millisecond, Command: cmd})
	rec.Close()

	var mu sync.Mutex
	issued := map[int]time.Time{}
	withReplayExec(t, func(ctx context.Context, wCfg workloadConfig, workerID int, op *workloadrec.OpLine, cols replayCollections, out *workloadrec.Outcome) (bool, []byte, error) {
		mu.Lock()
		issued[op.OpIteration()] = time.Now()
		mu.Unlock()
		return true, op.Cmd, nil
	})
	noShardingSeams(t, nil)
	cfg := replayCfg(path)
	cfg.WorkloadReplayMode = config.WorkloadReplayModeTimed
	collector := stats.NewCollector()
	if err := RunWorkload(context.Background(), nil, cols, queries, cfg, collector); err != nil {
		t.Fatal(err)
	}
	if gap := issued[2].Sub(issued[1]); gap < 80*time.Millisecond {
		t.Fatalf("iteration 2 started %s after iteration 1, want the recorded gap (>= 80 ms)", gap)
	}
	if rep := collector.WorkloadReplay(); rep.Iterations != 2 || rep.LagMaxMs == nil {
		t.Fatalf("replay info = %+v", rep)
	}
}

func TestRecordBlockZstdThroughRunWorkloadAndReplay(t *testing.T) {
	cfg := recordCfg(filepath.Join(t.TempDir(), "rec.ndjson.zst"))
	cfg.WorkloadRecordBackpressure, cfg.WorkloadRecordCompression, cfg.WorkloadRecordMaxBytes = "block", "zstd", 0
	path, _, collector := recordIterations(t, cfg, [][]recordedOp{iterationOps(1, 6)}, 0)
	md := collector.GetFinalInsights().Metadata.Recording
	if md.Status != workloadrec.StatusComplete || md.Backpressure != "block" || md.Compression != "zstd" || md.MaxBytes != 0 {
		t.Fatalf("recording metadata = %+v", md)
	}
	if md.FileBytes <= 0 || md.CompressionRatio <= 0 || md.WaitTotalMs == nil || md.WaitMaxMs == nil || md.Iterations != 0 {
		t.Fatalf("recording size/wait metadata = %+v", md)
	}
	info, err := workloadrec.Scan(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Compression != workloadrec.CompressionZstd || info.Header.Backpressure != "block" || info.Header.MaxBytes != 0 {
		t.Fatalf("scan = %+v", info)
	}

	withReplayExec(t, func(ctx context.Context, wCfg workloadConfig, workerID int, op *workloadrec.OpLine, cols replayCollections, out *workloadrec.Outcome) (bool, []byte, error) {
		return true, op.Cmd, nil
	})
	cols, queries := recordReplayFixtures()
	rc := stats.NewCollector()
	if err := RunWorkload(context.Background(), nil, cols, queries, replayCfg(path), rc); err != nil {
		t.Fatalf("replay of a zstd recording: %v", err)
	}
	if rep := rc.WorkloadReplay(); rep.Compression != "zstd" || rep.OpsReplayed != 6 || rep.CommandsHashMatch == nil || !*rep.CommandsHashMatch || rep.Iterations != 0 {
		t.Fatalf("replay info = %+v", rep)
	}
}

func TestDefaultRecordingMetadataStatesFailPolicy(t *testing.T) {
	_, collector := recordThroughRunWorkload(t, sampleOps(), nil)
	md := collector.GetFinalInsights().Metadata.Recording
	if md.Backpressure != "fail" || md.Compression != "none" || md.MaxBytes != config.DefaultWorkloadRecordMaxBytes {
		t.Fatalf("recording metadata = %+v", md)
	}
	if md.WaitTotalMs != nil || md.Waits != 0 || md.CompressionRatio != 0 || md.Iterations != 0 || md.FileBytes != md.Bytes {
		t.Fatalf("default recording must not report waits, ratio, or iterations: %+v", md)
	}
}
