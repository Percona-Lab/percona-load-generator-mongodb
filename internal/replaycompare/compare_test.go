package replaycompare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/workloadrec"
	"go.mongodb.org/mongo-driver/v2/bson"
)

const fixtureOps = 40

var ctx = context.Background()

func sum(c byte) string { return strings.Repeat(string(c), 64) }

func baseOutcome(seq uint64) workloadrec.Outcome {
	return workloadrec.Outcome{LatencyUS: 1000, OK: true, Returned: workloadrec.Count(1), Checksum: sum("abcdef"[seq%6])}
}

// recording writes a recording with outcomes: fixtureOps finds on two
// workers, each with a literal value in its filter.
func recording(t *testing.T, dir string) (string, string) { return recordingOn(t, dir, "8.0.4") }

func recordingOn(t *testing.T, dir, version string) (string, string) {
	t.Helper()
	path := filepath.Join(dir, "rec.ndjson")
	r, err := workloadrec.NewRecorder(path, workloadrec.Options{}, workloadrec.Header{PLGMVersion: "test", ServerVersion: version, Topology: "replica_set", Workers: 2, Outcomes: true, ResultChecksums: true, Namespaces: []string{"app.users"}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= fixtureOps; i++ {
		out := baseOutcome(uint64(i))
		r.Record(workloadrec.Entry{Worker: 1 + i%2, NS: "app.users", Op: "find", OK: true, Shape: &workloadrec.ShapeRef{Key: "find|k", Summary: "find on fields: k"},
			Command: bson.D{{Key: "find", Value: "users"}, {Key: "filter", Value: bson.D{{Key: "k", Value: "secret-" + string(rune('a'+i%26))}}}}, Outcome: &out})
	}
	if res := r.Close(); res.Status != workloadrec.StatusComplete {
		t.Fatalf("recording: %+v", res)
	}
	info, err := workloadrec.Scan(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, info.HeaderHash
}

type recOp struct {
	seq, workerSeq uint64
	worker         int
}

func recordedOps(t *testing.T, path string) []recOp {
	t.Helper()
	var ops []recOp
	if err := workloadrec.Stream(ctx, path, func(op *workloadrec.OpLine) error {
		ops = append(ops, recOp{seq: op.Seq, worker: op.Worker, workerSeq: op.WorkerSeq})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return ops
}

// replayFile writes a replay outcome file of the recording's ops; edit
// changes an outcome and returns false to leave the op out.
func replayFile(t *testing.T, dir, name, hash, version string, ops []recOp, edit func(seq uint64, o *workloadrec.Outcome) bool) string {
	t.Helper()
	path := filepath.Join(dir, name)
	w, err := workloadrec.NewOutcomeWriter(path, workloadrec.OutcomeHeader{Source: workloadrec.OutcomeSourceReplay, PLGMVersion: "test", ServerVersion: version, Topology: "replica_set", Workers: 2, ResultChecksums: true, RecordingFile: "rec.ndjson", RecordingHeaderHash: hash})
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		o := baseOutcome(op.seq)
		if edit != nil && !edit(op.seq, &o) {
			continue
		}
		w.Write(workloadrec.OutcomeLine{Seq: op.seq, Worker: op.worker, WorkerSeq: op.workerSeq, NS: "app.users", Op: "find", Shape: "find|k", Outcome: o})
	}
	w.Close(workloadrec.OutcomeTrailer{}, workloadrec.StatusComplete, "")
	return path
}

func deterministicFile(t *testing.T, dir, name, fp string, edit func(ws uint64, o *workloadrec.Outcome)) string {
	t.Helper()
	path := filepath.Join(dir, name)
	w, err := workloadrec.NewOutcomeWriter(path, workloadrec.OutcomeHeader{Source: workloadrec.OutcomeSourceDeterministic, PLGMVersion: "test", Workers: 1, RandomSeed: 7, OperationsPerWorker: fixtureOps})
	if err != nil {
		t.Fatal(err)
	}
	for ws := uint64(1); ws <= fixtureOps; ws++ {
		o := baseOutcome(ws)
		if edit != nil {
			edit(ws, &o)
		}
		w.Write(workloadrec.OutcomeLine{Worker: 1, WorkerSeq: ws, Iteration: 1, NS: "app.users", Op: "find", Shape: "find|k", Outcome: o})
	}
	w.Close(workloadrec.OutcomeTrailer{TrafficFingerprint: fp, TrafficStatus: "complete"}, workloadrec.StatusComplete, "")
	return path
}

func compare(t *testing.T, a, b string, opts Options) *Report {
	t.Helper()
	r, err := Compare(ctx, a, b, opts)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestIdenticalReplaysHaveNoDifferences(t *testing.T) {
	dir := t.TempDir()
	rec, hash := recording(t, dir)
	ops := recordedOps(t, rec)
	a := replayFile(t, dir, "a.ndjson", hash, "8.0.4", ops, nil)
	b := replayFile(t, dir, "b.ndjson", hash, "9.0.0", ops, nil)
	r := compare(t, a, b, Options{LabelA: "mongo-8", LabelB: "mongo-9"})
	if !r.IdentityMatch || r.Identity != hash || r.IdentityKind != IdentityRecording || r.MatchedBy != MatchBySeq {
		t.Fatalf("identity: %+v", r)
	}
	o := r.Overall
	if o.Matched != fixtureOps || o.DifferingOps != 0 || o.ChecksumsCompared != fixtureOps || len(r.DifferingOps) != 0 || r.ContainsLiteralValues {
		t.Fatalf("overall: %+v diffs %v", o, r.DifferingOps)
	}
	if o.Latency.Verdict != VerdictNoChange || len(r.Shapes) != 1 || r.Shapes[0].ShapeID != stats.StableShapeID("find", "users", "find|k") {
		t.Fatalf("shapes: %+v", r.Shapes)
	}
	if r.A.ServerVersion != "8.0.4" || r.B.ServerVersion != "9.0.0" || r.A.Topology != "replica_set" || r.A.Label != "mongo-8" || len(r.Warnings) != 0 {
		t.Fatalf("sides: %+v %+v %v", r.A, r.B, r.Warnings)
	}
}

func TestReadsOnlySideAndNotesAreReported(t *testing.T) {
	dir := t.TempDir()
	rec, hash := recording(t, dir)
	ops := recordedOps(t, rec)
	path := filepath.Join(dir, "reads.ndjson")
	w, err := workloadrec.NewOutcomeWriter(path, workloadrec.OutcomeHeader{Source: workloadrec.OutcomeSourceReplay, PLGMVersion: "test", Workers: 2, ResultChecksums: true, RecordingHeaderHash: hash, OpsFilter: "reads_only", Target: "m9"})
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		w.Write(workloadrec.OutcomeLine{Seq: op.seq, Worker: op.worker, WorkerSeq: op.workerSeq, NS: "app.users", Op: "find", Shape: "find|k", Outcome: baseOutcome(op.seq)})
	}
	w.Close(workloadrec.OutcomeTrailer{}, workloadrec.StatusComplete, "")

	r := compare(t, rec, path, Options{Notes: []string{"targets ran concurrently"}})
	if r.B.OpsFilter != "reads_only" || r.B.Target != "m9" || len(r.Notes) != 1 || !strings.Contains(strings.Join(r.Warnings, " "), "replayed reads only") {
		t.Fatalf("reads-only side: %+v warnings %v notes %v", r.B, r.Warnings, r.Notes)
	}
	var html strings.Builder
	if err := WriteHTML(&html, r); err != nil || !strings.Contains(html.String(), "targets ran concurrently") || !strings.Contains(SummaryText(r), "NOTE: targets ran concurrently") {
		t.Fatalf("notes missing from the HTML or summary: %v", err)
	}
	same := compare(t, path, path, Options{})
	if len(same.Warnings) != 0 || len(same.Notes) != 0 {
		t.Fatalf("two reads-only replays need no warning: %v %v", same.Warnings, same.Notes)
	}
}

func TestSlowerResultMismatchErrorsAndMissingOps(t *testing.T) {
	dir := t.TempDir()
	rec, hash := recording(t, dir)
	ops := recordedOps(t, rec)
	a := replayFile(t, dir, "a.ndjson", hash, "8.0.4", ops, nil)
	b := replayFile(t, dir, "b.ndjson", hash, "9.0.0", ops, func(seq uint64, o *workloadrec.Outcome) bool {
		o.LatencyUS = 3000
		switch seq {
		case 5:
			o.Returned = workloadrec.Count(2)
		case 6:
			o.Checksum = sum('9')
		case 7:
			*o = workloadrec.Outcome{LatencyUS: 50, ErrorCode: 11000, ErrorName: "DuplicateKey"}
		case 9:
			return false
		}
		return true
	})
	r := compare(t, a, b, Options{})
	o := r.Overall
	if o.Latency.Verdict != VerdictSlower || o.Latency.AvgDeltaPct == nil || *o.Latency.AvgDeltaPct != 200 || !strings.HasPrefix(o.Latency.Label, "Slower on B") {
		t.Fatalf("latency: %+v", o.Latency)
	}
	if o.Latency.A.Ops != fixtureOps-2 {
		t.Fatalf("latency covers ops ok on both sides only: %+v", o.Latency.A)
	}
	if o.CountMismatches != 1 || o.ChecksumMismatches != 1 || o.ErrorsOnlyB["11000 DuplicateKey"] != 1 || o.MissingInB != 1 || o.MissingInA != 0 || o.DifferingOps != 4 {
		t.Fatalf("differences: %+v", o)
	}
	want := map[uint64]string{5: DiffResultCount, 6: DiffChecksum, 7: DiffErrorOnlyB, 9: DiffMissingInB}
	if len(r.DifferingOps) != 4 {
		t.Fatalf("differing ops: %+v", r.DifferingOps)
	}
	for i, d := range r.DifferingOps {
		if i > 0 && d.Seq < r.DifferingOps[i-1].Seq {
			t.Fatalf("differing ops are listed by seq: %+v", r.DifferingOps)
		}
		if len(d.Kinds) != 1 || d.Kinds[0] != want[d.Seq] {
			t.Fatalf("seq %d kinds %v, want %s", d.Seq, d.Kinds, want[d.Seq])
		}
		if d.Command != "" || !strings.Contains(d.CommandNote, "give the recording") {
			t.Fatalf("no commands without the recording: %+v", d)
		}
	}
	if d := r.DifferingOps[3]; d.A == nil || d.B != nil || d.Worker == 0 || d.WorkerSeq == 0 {
		t.Fatalf("a missing op keeps A's outcome and identity: %+v", d)
	}
	if b7 := r.DifferingOps[2].B; b7 == nil || b7.ErrorCode != 11000 || b7.OK {
		t.Fatalf("error outcome: %+v", b7)
	}
	if r.ShapeVerdicts[VerdictSlower] != 1 {
		t.Fatalf("verdicts: %v", r.ShapeVerdicts)
	}

	// B has an op A does not (A stopped early).
	short := replayFile(t, dir, "short.ndjson", hash, "8.0.4", ops, func(seq uint64, o *workloadrec.Outcome) bool { return seq != 3 })
	r = compare(t, short, a, Options{})
	if r.Overall.MissingInA != 1 || r.DifferingOps[0].Kinds[0] != DiffMissingInA || r.DifferingOps[0].Seq != 3 || r.DifferingOps[0].A != nil {
		t.Fatalf("missing on A: %+v %+v", r.Overall, r.DifferingOps)
	}
}

func TestBothErrorsWithDifferentCodes(t *testing.T) {
	dir := t.TempDir()
	rec, hash := recording(t, dir)
	ops := recordedOps(t, rec)
	errAt := func(code int, name string) func(uint64, *workloadrec.Outcome) bool {
		return func(seq uint64, o *workloadrec.Outcome) bool {
			if seq == 2 {
				*o = workloadrec.Outcome{LatencyUS: 10, ErrorCode: code, ErrorName: name}
			}
			return true
		}
	}
	a := replayFile(t, dir, "a.ndjson", hash, "", ops, errAt(11000, "DuplicateKey"))
	same := replayFile(t, dir, "same.ndjson", hash, "", ops, errAt(11000, "DuplicateKey"))
	other := replayFile(t, dir, "other.ndjson", hash, "", ops, errAt(50, "MaxTimeMSExpired"))
	if r := compare(t, a, same, Options{}); r.Overall.DifferingOps != 0 {
		t.Fatalf("the same error on both sides is not a difference: %+v", r.Overall)
	}
	r := compare(t, a, other, Options{})
	if r.Overall.ErrorCodeDifferences != 1 || r.DifferingOps[0].Kinds[0] != DiffErrorCode || r.Overall.ErrorsOnlyA != nil {
		t.Fatalf("different codes: %+v", r.Overall)
	}
}

func TestDifferentRecordingIsRefusedUnlessAllowed(t *testing.T) {
	dir := t.TempDir()
	rec, hash := recording(t, dir)
	ops := recordedOps(t, rec)
	a := replayFile(t, dir, "a.ndjson", hash, "", ops, nil)
	b := replayFile(t, dir, "b.ndjson", strings.Repeat("9", 64), "", ops, nil)
	_, err := Compare(ctx, a, b, Options{})
	if !errors.Is(err, ErrMismatch) || !strings.Contains(err.Error(), "recording header hash differs") {
		t.Fatalf("err = %v", err)
	}
	r := compare(t, a, b, Options{AllowMismatch: true})
	if r.IdentityMatch || len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "Mismatch allowed") || r.Overall.Matched != fixtureOps {
		t.Fatalf("allowed mismatch: %+v %v", r.Overall, r.Warnings)
	}

	det := deterministicFile(t, dir, "det.ndjson", strings.Repeat("1", 64), nil)
	if _, err := Compare(ctx, a, det, Options{}); !errors.Is(err, ErrMismatch) || !strings.Contains(err.Error(), "deterministic") {
		t.Fatalf("replay vs deterministic: %v", err)
	}
	r = compare(t, a, det, Options{AllowMismatch: true})
	if r.MatchedBy != MatchByWorkerSeq || len(r.Warnings) == 0 {
		t.Fatalf("allowed: %+v %v", r.MatchedBy, r.Warnings)
	}
}

func TestRecordingWithoutOutcomesIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plain.ndjson")
	r, _ := workloadrec.NewRecorder(path, workloadrec.Options{}, workloadrec.Header{PLGMVersion: "test", Workers: 1})
	r.Record(workloadrec.Entry{Worker: 1, NS: "app.users", Op: "find", OK: true, Command: bson.D{{Key: "find", Value: "users"}}})
	r.Close()
	if _, err := Compare(ctx, path, path, Options{}); err == nil || !strings.Contains(err.Error(), "without outcomes") {
		t.Fatalf("err = %v", err)
	}
	other := filepath.Join(t.TempDir(), "x.json")
	_ = os.WriteFile(other, []byte(`{"a":1}`+"\n"), 0o600)
	if _, err := Compare(ctx, other, other, Options{}); err == nil || !strings.Contains(err.Error(), "not a workload recording or outcome file") {
		t.Fatalf("err = %v", err)
	}
}

func TestRecordingSideSuppliesCommandsAndSummaries(t *testing.T) {
	dir := t.TempDir()
	rec, hash := recording(t, dir)
	ops := recordedOps(t, rec)
	b := replayFile(t, dir, "b.ndjson", hash, "9.0.0", ops, func(seq uint64, o *workloadrec.Outcome) bool {
		if seq == 4 {
			o.Returned = workloadrec.Count(0)
		}
		return true
	})
	r := compare(t, rec, b, Options{})
	if r.A.Kind != SideRecording || r.B.Kind != SideReplay || !r.IdentityMatch || r.A.ServerVersion != "8.0.4" {
		t.Fatalf("sides: %+v %+v", r.A, r.B)
	}
	if len(r.DifferingOps) != 1 {
		t.Fatalf("diffs: %+v", r.DifferingOps)
	}
	d := r.DifferingOps[0]
	if !strings.Contains(d.Command, `"find":"users"`) || !strings.Contains(d.Command, "secret-") || d.ShapeSummary != "find on fields: k" || !r.ContainsLiteralValues || r.LiteralNote == "" {
		t.Fatalf("command and summary from the recording: %+v", d)
	}
	if r.Shapes[0].ShapeSummary != "find on fields: k" {
		t.Fatalf("shape summary: %+v", r.Shapes[0])
	}

	// Two replays: the commands come from the given recording, if it matches.
	a := replayFile(t, dir, "a.ndjson", hash, "8.0.4", ops, nil)
	r = compare(t, a, b, Options{RecordingPath: rec})
	if !strings.Contains(r.DifferingOps[0].Command, "secret-") || r.Shapes[0].ShapeSummary != "find on fields: k" {
		t.Fatalf("commands from the recording path: %+v", r.DifferingOps[0])
	}
	other, _ := recordingOn(t, t.TempDir(), "7.0.0")
	r = compare(t, a, b, Options{RecordingPath: other})
	if strings.Contains(r.DifferingOps[0].Command, "secret-") || !strings.Contains(strings.Join(r.Warnings, " "), "is not the one these files replayed") {
		t.Fatalf("a recording with another header must not supply commands")
	}
}

func TestDeterministicRunsMatchByWorkerSeqAndFingerprint(t *testing.T) {
	dir := t.TempDir()
	fp := strings.Repeat("f", 64)
	a := deterministicFile(t, dir, "a.ndjson", fp, nil)
	b := deterministicFile(t, dir, "b.ndjson", fp, func(ws uint64, o *workloadrec.Outcome) {
		if ws == 10 {
			o.Returned = workloadrec.Count(5)
		}
	})
	r := compare(t, a, b, Options{})
	if r.MatchedBy != MatchByWorkerSeq || r.IdentityKind != IdentityTraffic || r.Identity != fp || r.Overall.CountMismatches != 1 {
		t.Fatalf("deterministic: %+v", r)
	}
	d := r.DifferingOps[0]
	if d.Seq != 0 || d.Iteration != 1 || d.Worker != 1 || d.WorkerSeq != 10 || !strings.Contains(d.CommandNote, "regenerate") || OpKeyText(d) != "iteration 1, worker 1 #10" {
		t.Fatalf("diff key: %+v", d)
	}
	c := deterministicFile(t, dir, "c.ndjson", strings.Repeat("e", 64), nil)
	if _, err := Compare(ctx, a, c, Options{}); !errors.Is(err, ErrMismatch) || !strings.Contains(err.Error(), "traffic fingerprint differs") {
		t.Fatalf("fingerprint mismatch: %v", err)
	}
}

func TestTopNKeepsTheFirstDifferingOps(t *testing.T) {
	dir := t.TempDir()
	rec, hash := recording(t, dir)
	ops := recordedOps(t, rec)
	a := replayFile(t, dir, "a.ndjson", hash, "", ops, nil)
	b := replayFile(t, dir, "b.ndjson", hash, "", ops, func(seq uint64, o *workloadrec.Outcome) bool {
		o.Returned = workloadrec.Count(9)
		return true
	})
	r := compare(t, a, b, Options{TopN: 3})
	if r.Overall.DifferingOps != fixtureOps || len(r.DifferingOps) != 3 || r.TopN != 3 {
		t.Fatalf("top n: %d listed of %d", len(r.DifferingOps), r.Overall.DifferingOps)
	}
	for i, d := range r.DifferingOps {
		if d.Seq != ops[i].seq {
			t.Fatalf("listed %d: seq %d, want %d", i, d.Seq, ops[i].seq)
		}
	}
}

func TestInsufficientEvidenceAndPlanAnnotation(t *testing.T) {
	dir := t.TempDir()
	rec, hash := recording(t, dir)
	ops := recordedOps(t, rec)[:10]
	a := replayFile(t, dir, "a.ndjson", hash, "", ops, nil)
	b := replayFile(t, dir, "b.ndjson", hash, "", ops, func(seq uint64, o *workloadrec.Outcome) bool { o.LatencyUS = 9000; return true })
	id := stats.StableShapeID("find", "users", "find|k")
	r := compare(t, a, b, Options{
		PlansA: map[string]PlanInfo{id: {PlanSignatures: []string{"IXSCAN { k: 1 }"}}},
		PlansB: map[string]PlanInfo{id: {PlanSignatures: []string{"COLLSCAN"}}},
	})
	s := r.Shapes[0]
	if s.Latency.Verdict != VerdictInsufficient || !strings.Contains(s.Latency.Reasons[0], "minimum 30") {
		t.Fatalf("fewer than 30 ops: %+v", s.Latency)
	}
	if s.Plan == nil || !s.Plan.Changed || !strings.Contains(s.Plan.Note, "annotation only") {
		t.Fatalf("plan: %+v", s.Plan)
	}
	if r := compare(t, a, b, Options{}); r.Shapes[0].Plan != nil {
		t.Fatal("no plan annotation without explain data")
	}
}

func TestReportFiles(t *testing.T) {
	dir := t.TempDir()
	rec, hash := recording(t, dir)
	ops := recordedOps(t, rec)
	b := replayFile(t, dir, "b.ndjson", hash, "9.0.0", ops, func(seq uint64, o *workloadrec.Outcome) bool {
		if seq == 1 {
			o.Returned = workloadrec.Count(3)
		}
		return true
	})
	r := compare(t, rec, b, Options{LabelA: "mongo-8", LabelB: "mongo-9"})
	jp, hp := filepath.Join(dir, "cmp.json"), filepath.Join(dir, "cmp.html")
	if err := WriteFiles(r, jp, hp); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{jp, hp} {
		if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v", p, fi.Mode().Perm())
		}
	}
	raw, _ := os.ReadFile(jp)
	var back Report
	if err := json.Unmarshal(raw, &back); err != nil || back.Overall.CountMismatches != 1 || back.Format != FormatVersion {
		t.Fatalf("json: %v %+v", err, back.Overall)
	}
	page, _ := os.ReadFile(hp)
	for _, s := range []string{"Replay comparison: mongo-8 vs mongo-9", "Contains literal query values", "8.0.4", "9.0.0", "replica_set", "result_count_mismatch", "secret-", "returned 3", LatencyNote[:40], "find on fields: k"} {
		if !bytes.Contains(page, []byte(htmlEscape(s))) {
			t.Fatalf("html report lacks %q", s)
		}
	}
	if !strings.Contains(SummaryText(r), "count mismatches 1") {
		t.Fatalf("summary: %s", SummaryText(r))
	}
}

func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;", "'", "&#39;", "+", "&#43;").Replace(s)
}
