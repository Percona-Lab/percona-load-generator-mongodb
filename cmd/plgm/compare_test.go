package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/workloadrec"
)

func outcomeFile(t *testing.T, path, hash string, returned int64) {
	t.Helper()
	w, err := workloadrec.NewOutcomeWriter(path, workloadrec.OutcomeHeader{Source: workloadrec.OutcomeSourceReplay, Workers: 1, RecordingHeaderHash: hash})
	if err != nil {
		t.Fatal(err)
	}
	for i := uint64(1); i <= 3; i++ {
		w.Write(workloadrec.OutcomeLine{Seq: i, Worker: 1, WorkerSeq: i, NS: "db.c", Op: "find", Shape: "find|k", Outcome: workloadrec.Outcome{LatencyUS: 100, OK: true, Returned: workloadrec.Count(returned)}})
	}
	w.Close(workloadrec.OutcomeTrailer{}, workloadrec.StatusComplete, "")
}

func TestCompareReplaysCommand(t *testing.T) {
	dir := t.TempDir()
	hash := strings.Repeat("a", 64)
	a, b, c := filepath.Join(dir, "a.ndjson"), filepath.Join(dir, "b.ndjson"), filepath.Join(dir, "c.ndjson")
	outcomeFile(t, a, hash, 1)
	outcomeFile(t, b, hash, 2)
	outcomeFile(t, c, strings.Repeat("b", 64), 1)
	jp, hp := filepath.Join(dir, "r.json"), filepath.Join(dir, "r.html")

	var out, errOut bytes.Buffer
	code := runCompareReplays([]string{"--label-a", "mongo-8", a, b, "--json", jp, "--html", hp, "--top", "2"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "count mismatches 3") || !strings.Contains(out.String(), "Wrote "+jp) {
		t.Fatalf("summary: %s", out.String())
	}
	raw, _ := os.ReadFile(jp)
	if !strings.Contains(string(raw), `"label": "mongo-8"`) || strings.Count(string(raw), `"kinds"`) != 2 {
		t.Fatalf("json report (flags after the files apply): %s", raw)
	}
	if _, err := os.Stat(hp); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	errOut.Reset()
	if code := runCompareReplays([]string{a, c, "--json", "", "--html", ""}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "not executions of the same operations") {
		t.Fatalf("mismatch: exit %d %s", code, errOut.String())
	}
	if code := runCompareReplays([]string{a, c, "--allow-mismatch", "--json", "", "--html", ""}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "WARNING: Mismatch allowed") {
		t.Fatalf("allowed mismatch: exit %d %s", code, out.String())
	}
	if code := runCompareReplays([]string{a}, &out, &errOut); code != 2 {
		t.Fatalf("one file: exit %d", code)
	}
	if code := runCompareReplays([]string{a, b, "--plans-a-run", "x"}, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "need --trend-store") {
		t.Fatalf("plans without store: exit %d", code)
	}
}
