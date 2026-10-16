package mongo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/workloadrec"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

const secretMessage = "E11000 duplicate key error collection: appdb.users index: _id_ dup key: { _id: \"alice@example.com\" }"

func TestErrorOutcomesKeepCodesNeverMessages(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		code     int
		codeName string
	}{
		{"command error", mongo.CommandError{Code: 50, Name: "MaxTimeMSExpired", Message: secretMessage}, 50, "MaxTimeMSExpired"},
		{"write error", mongo.WriteException{WriteErrors: mongo.WriteErrors{{Code: 11000, Message: secretMessage}}}, 11000, "DuplicateKey"},
		{"write concern error", mongo.WriteException{WriteConcernError: &mongo.WriteConcernError{Code: 64, Name: "WriteConcernFailed", Message: secretMessage}}, 64, "WriteConcernFailed"},
		{"bulk write error", mongo.BulkWriteException{WriteErrors: []mongo.BulkWriteError{{WriteError: mongo.WriteError{Index: 2, Code: 121, Message: secretMessage}}}}, 121, "DocumentValidationFailure"},
		{"unnamed code", mongo.WriteException{WriteErrors: mongo.WriteErrors{{Code: 99999, Message: secretMessage}}}, 99999, ""},
		{"wrapped", fmt.Errorf("op: %w", mongo.CommandError{Code: 13, Name: "Unauthorized", Message: secretMessage}), 13, "Unauthorized"},
		{"deadline", fmt.Errorf("%s: %w", secretMessage, context.DeadlineExceeded), 0, workloadrec.ErrorNameClientTimeout},
		{"cancelled", context.Canceled, 0, workloadrec.ErrorNameCancelled},
		{"other", errors.New(secretMessage), 0, workloadrec.ErrorNameClient},
	}
	for _, tc := range cases {
		out := &workloadrec.Outcome{}
		finishOutcome(out, 1500*time.Microsecond, tc.err)
		if out.OK || out.ErrorCode != tc.code || out.ErrorName != tc.codeName || out.LatencyUS != 1500 {
			t.Fatalf("%s: outcome = %+v, want code %d name %q", tc.name, out, tc.code, tc.codeName)
		}
		b, _ := json.Marshal(out)
		if strings.Contains(string(b), "E11000") || strings.Contains(string(b), "alice") {
			t.Fatalf("%s: outcome keeps the error message: %s", tc.name, b)
		}
	}
	out := &workloadrec.Outcome{ErrorCode: 1, ErrorName: "x"}
	finishOutcome(out, time.Millisecond, nil)
	if !out.OK || out.ErrorCode != 0 || out.ErrorName != "" {
		t.Fatalf("success outcome = %+v", out)
	}
}

func cursorOf(t *testing.T, docs ...bson.D) *mongo.Cursor {
	t.Helper()
	in := make([]any, len(docs))
	for i, d := range docs {
		in[i] = d
	}
	cur, err := mongo.NewCursorFromDocuments(in, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return cur
}

func TestFindAndAggregateOutcomesCountAndChecksumResults(t *testing.T) {
	a := bson.D{{Key: "_id", Value: int32(1)}, {Key: "name", Value: "a"}}
	b := bson.D{{Key: "_id", Value: int32(2)}, {Key: "name", Value: "b"}}
	read := func(checksums bool, docs ...bson.D) *workloadrec.Outcome {
		out := &workloadrec.Outcome{}
		rr := newResultReader(out, checksums)
		n, err := drainCursor(context.Background(), cursorOf(t, docs...), rr)
		if err != nil || n != int64(len(docs)) {
			t.Fatalf("drain = %d, %v", n, err)
		}
		finishOutcome(out, time.Millisecond, err)
		rr.finish(out)
		return out
	}
	first, again := read(true, a, b), read(true, a, b)
	if first.Returned == nil || *first.Returned != 2 || len(first.Checksum) != 64 || !first.OK {
		t.Fatalf("outcome = %+v", first)
	}
	if first.Checksum != again.Checksum {
		t.Fatal("the same result must give the same checksum")
	}
	if read(true, b, a).Checksum == first.Checksum {
		t.Fatal("the checksum must be order-sensitive")
	}
	if empty := read(true); empty.Returned == nil || *empty.Returned != 0 || empty.Checksum == "" {
		t.Fatalf("empty result outcome = %+v", empty)
	}
	if off := read(false, a, b); off.Checksum != "" || *off.Returned != 2 {
		t.Fatalf("checksums off outcome = %+v", off)
	}
	// Outcome capture off: the cursor is drained and counted as before.
	if n, err := drainCursor(context.Background(), cursorOf(t, a, b), newResultReader(nil, true)); n != 2 || err != nil {
		t.Fatalf("drain without outcome = %d, %v", n, err)
	}
}

func TestWriteAndTransactionOutcomes(t *testing.T) {
	out := &workloadrec.Outcome{}
	fastInsertOutcome(out, time.Millisecond, fastInsertReply{N: 5}, nil)
	if !out.OK || *out.Inserted != 5 {
		t.Fatalf("fast insert = %+v", out)
	}
	reply := fastInsertReply{N: 3}
	reply.WriteErrors = append(reply.WriteErrors, struct {
		Code int `bson:"code"`
	}{Code: 11000})
	out = &workloadrec.Outcome{}
	fastInsertOutcome(out, time.Millisecond, reply, nil)
	if out.OK || out.ErrorCode != 11000 || out.ErrorName != "DuplicateKey" || *out.Inserted != 3 {
		t.Fatalf("fast insert with a write error in the reply = %+v", out)
	}
	out = &workloadrec.Outcome{}
	fastInsertOutcome(out, time.Millisecond, fastInsertReply{}, mongo.CommandError{Code: 13, Name: "Unauthorized"})
	if out.OK || out.ErrorCode != 13 || *out.Inserted != 0 {
		t.Fatalf("failed fast insert = %+v", out)
	}

	bulk := mongo.BulkWriteException{WriteErrors: []mongo.BulkWriteError{{WriteError: mongo.WriteError{Index: 2, Code: 11000}}}}
	if insertedCount(4, nil) != 4 || insertedCount(4, bulk) != 2 || insertedCount(1, mongo.WriteException{}) != 0 {
		t.Fatal("inserted counts of an ordered insertMany are wrong")
	}

	out = &workloadrec.Outcome{}
	transactionOutcome(out, time.Millisecond, nil)
	if !out.OK || out.Transaction != workloadrec.TransactionCommit {
		t.Fatalf("committed transaction = %+v", out)
	}
	out = &workloadrec.Outcome{}
	transactionOutcome(out, time.Millisecond, mongo.CommandError{Code: 251, Name: "NoSuchTransaction"})
	if out.OK || out.Transaction != workloadrec.TransactionAbort || out.ErrorCode != 251 {
		t.Fatalf("aborted transaction = %+v", out)
	}

	if (workloadConfig{}).newOutcome() != nil || (workloadConfig{outcomes: true}).newOutcome() == nil {
		t.Fatal("newOutcome must follow the outcomes setting")
	}
}

// fakeReplayOutcome fills the outcome a real send of op would report.
func fakeReplayOutcome(op *workloadrec.OpLine, out *workloadrec.Outcome) {
	if out == nil {
		return
	}
	out.LatencyUS, out.OK = int64(op.Seq*10), true
	switch op.Op {
	case "find", "aggregate":
		out.Returned = workloadrec.Count(int64(op.Seq))
	case "insert", "insertMany":
		out.Inserted = workloadrec.Count(1)
	}
}

func TestReplayWritesOutcomesWithRecordingSeqNumbers(t *testing.T) {
	recPath, _ := recordThroughRunWorkload(t, sampleOps(), nil)
	info, err := workloadrec.Scan(recPath)
	if err != nil {
		t.Fatal(err)
	}
	withReplayExec(t, func(ctx context.Context, wCfg workloadConfig, workerID int, op *workloadrec.OpLine, cols replayCollections, out *workloadrec.Outcome) (bool, []byte, error) {
		fakeReplayOutcome(op, out)
		return true, op.Cmd, nil
	})
	noShardingSeams(t, nil)
	cols, queries := recordReplayFixtures()

	off := stats.NewCollector()
	if err := RunWorkload(context.Background(), nil, cols, queries, replayCfg(recPath), off); err != nil {
		t.Fatal(err)
	}
	if md := off.GetFinalInsights().Metadata; md.OutcomeFile != nil {
		t.Fatalf("outcome capture off must not report an outcome file: %+v", md.OutcomeFile)
	}

	outPath := filepath.Join(t.TempDir(), "replay_outcomes.ndjson")
	cfg := replayCfg(recPath)
	cfg.WorkloadRecordOutcomes, cfg.WorkloadRecordResultChecksums, cfg.WorkloadReplayOutcomesPath = true, true, outPath
	collector := stats.NewCollector()
	if err := RunWorkload(context.Background(), nil, cols, queries, cfg, collector); err != nil {
		t.Fatal(err)
	}
	rep := collector.WorkloadReplay()
	if rep.CommandsHashMatch == nil || !*rep.CommandsHashMatch {
		t.Fatal("outcome capture must not change the commands replayed")
	}
	recorded := map[uint64]workloadrec.OpLine{}
	if err := workloadrec.Stream(context.Background(), recPath, func(op *workloadrec.OpLine) error { recorded[op.Seq] = *op; return nil }); err != nil {
		t.Fatal(err)
	}
	seen := map[uint64]bool{}
	h, tr, err := workloadrec.StreamOutcomes(context.Background(), outPath, func(l *workloadrec.OutcomeLine) error {
		r, ok := recorded[l.Seq]
		if !ok || r.Worker != l.Worker || r.WorkerSeq != l.WorkerSeq || r.NS != l.NS || r.Op != l.Op || l.Shape != r.Shape.Key {
			return fmt.Errorf("outcome line %+v does not match recorded op %+v", l, r)
		}
		if l.Outcome.LatencyUS != int64(l.Seq*10) || l.Outcome.Returned == nil || *l.Outcome.Returned != int64(l.Seq) {
			return fmt.Errorf("outcome of seq %d = %+v", l.Seq, l.Outcome)
		}
		seen[l.Seq] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != len(recorded) {
		t.Fatalf("%d outcomes for %d recorded operations", len(seen), len(recorded))
	}
	if h.Source != workloadrec.OutcomeSourceReplay || h.RecordingHeaderHash != info.HeaderHash || h.RecordingFile != "rec.ndjson" || !h.ResultChecksums || h.Workers != 2 {
		t.Fatalf("outcome header = %+v", h)
	}
	if tr == nil || tr.Status != workloadrec.StatusComplete || tr.Ops != 6 || tr.CommandsHash != rep.CommandsHash {
		t.Fatalf("outcome trailer = %+v", tr)
	}
	fi := collector.GetFinalInsights().Metadata.OutcomeFile
	if fi == nil || fi.Source != "replay" || fi.Path != outPath || fi.Ops != 6 || fi.Status != workloadrec.StatusComplete || fi.RecordingHeaderHash != info.HeaderHash || !fi.ResultChecksums {
		t.Fatalf("outcome file metadata = %+v", fi)
	}
	if st, _ := os.Stat(outPath); st.Mode().Perm() != 0o600 {
		t.Fatalf("outcome file mode = %v", st.Mode().Perm())
	}
}

// benchmarkFindOutcome is the worker's outcome work for a find that returned
// 10 documents: the outcome, the count, and (with checksums) the copies and
// hashing done after the latency window.
func benchmarkFindOutcome(b *testing.B, checksums bool) {
	doc, _ := bson.Marshal(bson.D{{Key: "_id", Value: bson.NewObjectID()}, {Key: "flight_code", Value: "AA1234"}, {Key: "seats", Value: int32(180)},
		{Key: "price", Value: 412.5}, {Key: "status", Value: "ontime"}, {Key: "gate", Value: "G12"}, {Key: "departure", Value: bson.DateTime(1767225600000)},
		{Key: "origin", Value: "LIS"}, {Key: "dest", Value: "JFK"}, {Key: "tags", Value: bson.A{"a", "b"}}})
	wCfg := workloadConfig{outcomes: true, checksums: checksums}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		out := wCfg.newOutcome()
		rr := newResultReader(out, wCfg.checksums)
		for j := 0; j < 10; j++ {
			rr.add(doc)
		}
		finishOutcome(out, time.Millisecond, nil)
		rr.finish(out)
	}
}

func BenchmarkFindOutcomeCounts(b *testing.B)    { benchmarkFindOutcome(b, false) }
func BenchmarkFindOutcomeChecksums(b *testing.B) { benchmarkFindOutcome(b, true) }

func readOutcomeLines(t *testing.T, path string) ([]workloadrec.OutcomeLine, *workloadrec.OutcomeHeader, *workloadrec.OutcomeTrailer) {
	t.Helper()
	var lines []workloadrec.OutcomeLine
	h, tr, err := workloadrec.StreamOutcomes(context.Background(), path, func(l *workloadrec.OutcomeLine) error { lines = append(lines, *l); return nil })
	if err != nil {
		t.Fatal(err)
	}
	return lines, h, tr
}

func TestDeterministicRunWritesOutcomesStampedWithFingerprint(t *testing.T) {
	plain, _, err := runDeterministic(t, context.Background(), detCfg(), nil)
	if err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(t.TempDir(), "det_outcomes.ndjson")
	cfg := detCfg()
	cfg.WorkloadRecordOutcomes, cfg.WorkloadDeterministicOutcomesPath = true, outPath
	if err := config.ValidateWorkloadOutcomes(cfg); err != nil {
		t.Fatal(err)
	}
	g, c, err := runDeterministic(t, context.Background(), cfg, func(worker int, seq uint64) bool { return seq%7 == 0 })
	if err != nil {
		t.Fatal(err)
	}
	if g.Fingerprint != plain.Fingerprint {
		t.Fatal("outcome capture (and failures) must not change the generated commands")
	}
	lines, h, tr := readOutcomeLines(t, outPath)
	if uint64(len(lines)) != g.OpsSent {
		t.Fatalf("%d outcome lines for %d operations sent", len(lines), g.OpsSent)
	}
	key := map[string]bool{}
	for _, l := range lines {
		k := fmt.Sprintf("%d/%d/%d", l.Iteration, l.Worker, l.WorkerSeq)
		if key[k] || l.Iteration != 1 || l.Seq != 0 || l.Shape == "" {
			t.Fatalf("outcome line %+v (duplicate key or missing fields)", l)
		}
		key[k] = true
		if l.Outcome.OK != (l.WorkerSeq%7 != 0) || l.Outcome.LatencyUS != int64(l.WorkerSeq) {
			t.Fatalf("outcome of %s = %+v", k, l.Outcome)
		}
	}
	for id, ops := range c.ops {
		for _, op := range ops {
			if !key[fmt.Sprintf("1/%d/%d", id, op.seq)] {
				t.Fatalf("no outcome for worker %d op %d", id, op.seq)
			}
		}
	}
	if h.Source != workloadrec.OutcomeSourceDeterministic || h.RandomSeed != 42 || h.OperationsPerWorker != 200 || h.Workers != 3 || h.CollectionsHash != g.CollectionsHash {
		t.Fatalf("outcome header = %+v", h)
	}
	if tr == nil || tr.TrafficFingerprint != g.Fingerprint || tr.TrafficStatus != "complete" || tr.Status != workloadrec.StatusComplete || tr.CommandsHash != g.CommandsHash {
		t.Fatalf("outcome trailer = %+v, fingerprint %s", tr, g.Fingerprint)
	}
}

func TestDeterministicOutcomesSpanIterationsInOneFile(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "det_outcomes.ndjson")
	cfg := detCfg()
	cfg.Iterations, cfg.OperationsPerWorker = 2, 20
	cfg.WorkloadRecordOutcomes, cfg.WorkloadDeterministicOutcomesPath = true, outPath
	ctx, finish := WithWorkloadRecordingSession(context.Background())
	var g *stats.GeneratedTrafficInfo
	for i := 0; i < 2; i++ {
		var err error
		if g, _, err = runDeterministic(t, ctx, cfg, nil); err != nil {
			t.Fatal(err)
		}
	}
	finish()
	lines, _, tr := readOutcomeLines(t, outPath)
	perIter := map[int]int{}
	for _, l := range lines {
		perIter[l.Iteration]++
	}
	if perIter[1] == 0 || perIter[2] == 0 || len(perIter) != 2 || uint64(len(lines)) != g.OpsSent {
		t.Fatalf("outcomes per iteration = %v (sent %d)", perIter, g.OpsSent)
	}
	if tr == nil || tr.TrafficFingerprint != g.Fingerprint || g.Iterations != 2 {
		t.Fatalf("trailer = %+v, traffic = %+v", tr, g)
	}
}
