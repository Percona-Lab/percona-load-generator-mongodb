package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/replaycompare"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/workloadrec"
	"go.mongodb.org/mongo-driver/v2/bson"
)

const replayFixtureOps = 80

type replayFixture struct {
	dir, recording, hash, a, b string
}

func replayFixtureShape(seq uint64) (string, string) {
	if seq%2 == 1 {
		return "find|sku", "find on fields: sku"
	}
	return "find|status", "find on fields: status"
}

func replayFixtureOutcome(seq uint64) workloadrec.Outcome {
	return workloadrec.Outcome{LatencyUS: 1000, OK: true, Returned: workloadrec.Count(1), Checksum: strings.Repeat(string("abcdef"[seq%6]), 64)}
}

// writeReplayFixture writes a recording of finds on two shapes and two
// replays of it: B (MongoDB 9.0) is 3x slower on find|sku, returns two
// documents for seq 3, a different checksum for seq 5, fails seq 7, and
// misses seq 9.
func writeReplayFixture(t *testing.T) replayFixture {
	t.Helper()
	dir := t.TempDir()
	f := replayFixture{dir: dir, recording: filepath.Join(dir, "rec.ndjson")}
	r, err := workloadrec.NewRecorder(f.recording, workloadrec.Options{}, workloadrec.Header{
		PLGMVersion: "test", CreatedAt: "2026-10-16T08:00:00Z", ServerVersion: "8.0.4", Topology: "replica_set",
		Workers: 2, Outcomes: true, ResultChecksums: true, Namespaces: []string{"shop.orders"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for seq := uint64(1); seq <= replayFixtureOps; seq++ {
		key, summary := replayFixtureShape(seq)
		field := strings.TrimPrefix(key, "find|")
		out := replayFixtureOutcome(seq)
		r.Record(workloadrec.Entry{Worker: 1 + int(seq%2), NS: "shop.orders", Op: "find", OK: true, Shape: &workloadrec.ShapeRef{Key: key, Summary: summary},
			Command: bson.D{{Key: "find", Value: "orders"}, {Key: "filter", Value: bson.D{{Key: field, Value: fmt.Sprintf("secret-%s-%d", field, seq)}}}}, Outcome: &out})
	}
	if res := r.Close(); res.Status != workloadrec.StatusComplete {
		t.Fatalf("recording: %+v", res)
	}
	info, err := workloadrec.Scan(f.recording)
	if err != nil {
		t.Fatal(err)
	}
	f.hash = info.HeaderHash
	f.a = writeReplayOutcomes(t, f, "replay-a.ndjson", f.hash, "8.0.4", "replica_set", nil)
	f.b = writeReplayOutcomes(t, f, "replay-b.ndjson", f.hash, "9.0.0", "sharded", func(seq uint64, o *workloadrec.Outcome) bool {
		if seq%2 == 1 {
			o.LatencyUS = 3000
		}
		switch seq {
		case 3:
			o.Returned = workloadrec.Count(2)
		case 5:
			o.Checksum = strings.Repeat("9", 64)
		case 7:
			*o = workloadrec.Outcome{LatencyUS: 50000, ErrorCode: 50, ErrorName: "MaxTimeMSExpired"}
		case 9:
			return false
		}
		return true
	})
	return f
}

func writeReplayOutcomes(t *testing.T, f replayFixture, name, hash, version, topology string, edit func(uint64, *workloadrec.Outcome) bool) string {
	t.Helper()
	path := filepath.Join(f.dir, name)
	w, err := workloadrec.NewOutcomeWriter(path, workloadrec.OutcomeHeader{
		Source: workloadrec.OutcomeSourceReplay, PLGMVersion: "test", CreatedAt: "2026-10-16T09:00:00Z", ServerVersion: version, Topology: topology,
		Workers: 2, ResultChecksums: true, RecordingFile: "rec.ndjson", RecordingHeaderHash: hash,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := workloadrec.Stream(context.Background(), f.recording, func(op *workloadrec.OpLine) error {
		o := replayFixtureOutcome(op.Seq)
		if edit != nil && !edit(op.Seq, &o) {
			return nil
		}
		w.Write(workloadrec.OutcomeLine{Seq: op.Seq, Worker: op.Worker, WorkerSeq: op.WorkerSeq, NS: op.NS, Op: op.Op, Shape: op.Shape.Key, Outcome: o})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w.Close(workloadrec.OutcomeTrailer{}, workloadrec.StatusComplete, "")
	return path
}

// replayForm builds a multipart body; files maps a field to {upload name, path}.
func replayForm(t *testing.T, fields map[string]string, files map[string][2]string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	for field, f := range files {
		b, err := os.ReadFile(f[1])
		if err != nil {
			t.Fatal(err)
		}
		part, _ := mw.CreateFormFile(field, f[0])
		_, _ = part.Write(b)
	}
	_ = mw.Close()
	return buf.Bytes(), mw.FormDataContentType()
}

func replayCall(t *testing.T, s *WebServer, method, target string, body []byte, contentType string) (int, map[string]interface{}, *httptest.ResponseRecorder) {
	t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	if target == "/api/replay-compare" {
		s.handleReplayCompare(rec, req)
	} else {
		s.handleReplayCompareItem(rec, req)
	}
	var out map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, rec
}

func postReplayCompare(t *testing.T, s *WebServer, fields map[string]string, files map[string][2]string) (int, map[string]interface{}) {
	t.Helper()
	body, ct := replayForm(t, fields, files)
	code, out, _ := replayCall(t, s, http.MethodPost, "/api/replay-compare", body, ct)
	return code, out
}

func TestReplayCompareAPIUploadsListAndDownload(t *testing.T) {
	f := writeReplayFixture(t)
	s := NewServer(nil)
	code, out := postReplayCompare(t, s, map[string]string{"label_a": "MongoDB 8.0", "label_b": "MongoDB 9.0", "top_n": "3"}, map[string][2]string{
		"a_file": {"replay-8.0.ndjson", f.a}, "b_file": {"replay-9.0.ndjson", f.b}, "recording_file": {"rec.ndjson", f.recording},
	})
	if code != http.StatusOK {
		t.Fatalf("POST: %d %v", code, out)
	}
	rep := out["report"].(map[string]interface{})
	a, b := rep["a"].(map[string]interface{}), rep["b"].(map[string]interface{})
	overall := rep["overall"].(map[string]interface{})
	if a["file"] != "replay-8.0.ndjson" || b["file"] != "replay-9.0.ndjson" || a["label"] != "MongoDB 8.0" || b["server_version"] != "9.0.0" || b["topology"] != "sharded" ||
		rep["identity"] != f.hash || rep["identity_match"] != true || rep["contains_literal_values"] != true ||
		overall["differing_ops"].(float64) != 4 || overall["missing_in_b"].(float64) != 1 || len(rep["differing_ops"].([]interface{})) != 3 {
		t.Fatalf("report = %v", rep)
	}
	first := rep["differing_ops"].([]interface{})[0].(map[string]interface{})
	if !strings.Contains(first["command"].(string), "secret-sku-3") {
		t.Fatalf("differing op must carry its recorded command: %v", first)
	}
	for _, p := range []string{os.TempDir()} {
		matches, _ := filepath.Glob(filepath.Join(p, "plgm-replay-compare-*.ndjson"))
		if len(matches) != 0 {
			t.Fatalf("uploads must be removed after the comparison: %v", matches)
		}
	}

	id := rep["id"].(string)
	code, list, _ := replayCall(t, s, http.MethodGet, "/api/replay-compare", nil, "")
	reports := list["reports"].([]interface{})
	if code != http.StatusOK || len(reports) != 1 || list["max_reports"].(float64) != maxReplayComparisons {
		t.Fatalf("GET list: %d %v", code, list)
	}
	summary := reports[0].(map[string]interface{})
	if summary["id"] != id || summary["identity"] != f.hash || summary["differing_ops"].(float64) != 4 || summary["file_b"] != "replay-9.0.ndjson" {
		t.Fatalf("summary = %v", summary)
	}
	if _, ok := summary["differing_ops_detail"]; ok || strings.Contains(fmt.Sprint(summary), "secret") {
		t.Fatalf("the list must not carry literal values: %v", summary)
	}
	code, item, _ := replayCall(t, s, http.MethodGet, "/api/replay-compare/"+id, nil, "")
	if code != http.StatusOK || item["report"].(map[string]interface{})["id"] != id {
		t.Fatalf("GET item: %d %v", code, item)
	}
	code, _, rec := replayCall(t, s, http.MethodGet, "/api/replay-compare/"+id+"/report.html", nil, "")
	html := rec.Body.String()
	if code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") ||
		!strings.Contains(rec.Header().Get("Content-Disposition"), "attachment") || !strings.Contains(html, "Contains literal query values") || !strings.Contains(html, "MongoDB 9.0") {
		t.Fatalf("HTML download: %d %v", code, rec.Header())
	}
	if code, _, _ := replayCall(t, s, http.MethodGet, "/api/replay-compare/unknown", nil, ""); code != http.StatusNotFound {
		t.Fatalf("unknown id: %d", code)
	}
	if code, _, _ := replayCall(t, s, http.MethodDelete, "/api/replay-compare/"+id, nil, ""); code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE: %d", code)
	}
}

func TestReplayCompareAPIPathsErrorsAndMismatch(t *testing.T) {
	f := writeReplayFixture(t)
	s := NewServer(nil)
	if code, out := postReplayCompare(t, s, map[string]string{"a_path": f.a, "b_path": f.b}, nil); code != http.StatusOK ||
		out["report"].(map[string]interface{})["contains_literal_values"] == true {
		t.Fatalf("server paths without a recording: %d %v", code, out)
	}
	for name, fields := range map[string]map[string]string{
		"missing B":   {"a_path": f.a},
		"missing A":   {"b_path": f.b},
		"bad top":     {"a_path": f.a, "b_path": f.b, "top_n": "0"},
		"bad path":    {"a_path": f.a + "\x00x", "b_path": f.b},
		"not a file":  {"a_path": filepath.Join(f.dir, "nope.ndjson"), "b_path": f.b},
		"unknown run": {"a_path": f.a, "b_path": f.b, "run_a": "no-such-run"},
	} {
		code, out := postReplayCompare(t, s, fields, nil)
		want := http.StatusBadRequest
		if name == "unknown run" {
			want = http.StatusNotFound
		}
		if code != want || out["error"] == "" {
			t.Fatalf("%s: %d %v", name, code, out)
		}
	}

	other := writeReplayOutcomes(t, f, "replay-other.ndjson", strings.Repeat("e", 64), "9.0.0", "sharded", nil)
	code, out := postReplayCompare(t, s, nil, map[string][2]string{"a_file": {"mine.ndjson", f.a}, "b_file": {"theirs.ndjson", other}})
	if code != http.StatusConflict || strings.Contains(out["error"].(string), "plgm-replay-compare-") {
		t.Fatalf("different recordings must be refused without temp file names: %d %v", code, out)
	}
	code, out = postReplayCompare(t, s, map[string]string{"a_path": f.a, "b_path": other, "allow_mismatch": "true"}, nil)
	rep, _ := out["report"].(map[string]interface{})
	if code != http.StatusOK || rep["identity_match"] != false || len(rep["warnings"].([]interface{})) == 0 {
		t.Fatalf("allow_mismatch must compare with a warning: %d %v", code, out)
	}
	if code, _, _ := replayCall(t, s, http.MethodPut, "/api/replay-compare", nil, ""); code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT: %d", code)
	}
}

func TestReplayCompareKeepsTheNewestReports(t *testing.T) {
	s := NewServer(nil)
	for i := 0; i < maxReplayComparisons+2; i++ {
		s.keepReplayComparison(&replaycompare.Report{ID: fmt.Sprintf("r%d", i)})
	}
	_, list, _ := replayCall(t, s, http.MethodGet, "/api/replay-compare", nil, "")
	reports := list["reports"].([]interface{})
	if len(reports) != maxReplayComparisons || reports[0].(map[string]interface{})["id"] != fmt.Sprintf("r%d", maxReplayComparisons+1) ||
		reports[len(reports)-1].(map[string]interface{})["id"] != "r2" || s.replayComparison("r1") != nil {
		t.Fatalf("expected the newest %d reports, newest first: %v", maxReplayComparisons, reports)
	}
}

// sameRecordingTrendServers runs two trend runs (before and after the sku
// index) into one store, both replaying hash when it is set.
func sameRecordingTrendServers(t *testing.T, hash string) (*WebServer, map[string]interface{}) {
	t.Helper()
	steppedTrendClock(t)
	path := filepath.Join(t.TempDir(), "trend_baselines.json")
	first, _ := efficiencyFixtureServerWith(t, "ready", fixtureOptions{store: stats.OpenTrendStore(path), replayHash: hash})
	getInsightsJSON(t, first)
	second, _ := efficiencyFixtureServerWith(t, "ready", fixtureOptions{after: true, store: stats.OpenTrendStore(path), replayHash: hash})
	return second, getInsightsJSON(t, second)
}

func TestReplayedSameRecordingTrendRowsAreSameValuesWithUnchangedLabels(t *testing.T) {
	hash := strings.Repeat("ab", 32)
	_, replayed := sameRecordingTrendServers(t, hash)
	_, generated := sameRecordingTrendServers(t, "")
	_, rows := trendRows(t, replayed)
	_, plain := trendRows(t, generated)
	if len(rows) == 0 || len(rows) != len(plain) {
		t.Fatalf("rows: %d vs %d", len(rows), len(plain))
	}
	for summary, row := range rows {
		qv := row["query_values"].(map[string]interface{})
		if qv["state"] != stats.TrendValuesSame || qv["source"] != stats.TrendValuesSourceRecording || qv["replay_header_hash"] != hash || qv["current"] != nil || qv["baseline"] != nil {
			t.Fatalf("%s: same-recording row must be same_values without literals: %v", summary, qv)
		}
		if strings.Contains(fmt.Sprint(row), "secret-") {
			t.Fatalf("%s: literal values leaked: %v", summary, row)
		}
		other := plain[summary]
		// workload_changes notes the same recorded workload (PHASE 5).
		for _, k := range []string{"query_values", "workload_changes"} {
			delete(row, k)
			delete(other, k)
		}
		normalizeVolatile(row)
		normalizeVolatile(other)
		if !reflect.DeepEqual(row, other) {
			t.Fatalf("%s: tc-1 trend labels must not change for a replay:\n%v\n%v", summary, row, other)
		}
	}
}

// TestUIFixturesMatchAPIReplayCompare writes the replay comparison fixture:
// two trend runs that replayed the fixture recording (insights) and the
// comparison of its two replays with plans from those runs.
func TestUIFixturesMatchAPIReplayCompare(t *testing.T) {
	f := writeReplayFixture(t)
	s, api := sameRecordingTrendServers(t, f.hash)
	_, baselines, _ := trendCall(t, s, http.MethodGet, "/api/trend-baselines", nil, "")
	ids := storedRunIDs(t, baselines)
	code, out := postReplayCompare(t, s, map[string]string{"label_a": "MongoDB 8.0", "label_b": "MongoDB 9.0", "run_a": ids[1], "run_b": ids[0]}, map[string][2]string{
		"a_file": {"replay-8.0.ndjson", f.a}, "b_file": {"replay-9.0.ndjson", f.b}, "recording_file": {"rec.ndjson", f.recording},
	})
	if code != http.StatusOK {
		t.Fatalf("POST: %d %v", code, out)
	}
	rep := out["report"].(map[string]interface{})
	var plan map[string]interface{}
	for _, raw := range rep["shapes"].([]interface{}) {
		if sh := raw.(map[string]interface{}); sh["shape_key"] == "find|sku" {
			plan, _ = sh["plan"].(map[string]interface{})
		}
	}
	if plan == nil || plan["changed"] != true {
		t.Fatalf("find|sku must carry the plan change of the chosen trend runs: %v", rep["shapes"])
	}
	_, list, _ := replayCall(t, s, http.MethodGet, "/api/replay-compare", nil, "")
	raw, _ := json.Marshal(map[string]interface{}{"insights": api, "list": list, "report": rep})
	text := strings.ReplaceAll(string(raw), rep["id"].(string), "fixture-compare-1")
	for i, id := range ids {
		text = strings.ReplaceAll(text, id, fmt.Sprintf("fixture-run-%d", len(ids)-i))
	}
	var fixture map[string]interface{}
	if err := json.Unmarshal([]byte(text), &fixture); err != nil {
		t.Fatal(err)
	}
	checkUIFixture(t, "replay_compare.json", fixture)
}
