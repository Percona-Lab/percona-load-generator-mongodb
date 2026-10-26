package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/workloadrec"
)

// writeTestRecording writes a recording with a header, two operations, and
// a trailer to dir/name and returns its path.
func writeTestRecording(t *testing.T, dir, name string) string {
	t.Helper()
	h, _ := json.Marshal(workloadrec.Header{Type: workloadrec.LineHeader, Format: workloadrec.FormatVersion, PLGMVersion: "test", CreatedAt: "2026-10-01T10:00:00Z", Workers: 2})
	content := string(h) + "\n" +
		`{"type":"op","seq":1,"worker":1,"worker_seq":1}` + "\n" +
		`{"type":"op","seq":2,"worker":2,"worker_seq":1}` + "\n" +
		`{"type":"trailer","status":"complete","ops":2,"bytes":100}` + "\n"
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeFile(t *testing.T, path, content string, mod time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if !mod.IsZero() {
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
}

func getRecordings(t *testing.T, s *WebServer) (dirs []string, list []recordingEntry) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleRecordings(rec, httptest.NewRequest(http.MethodGet, "/api/recordings", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/recordings: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Directories []string         `json:"directories"`
		Recordings  []recordingEntry `json:"recordings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Directories, out.Recordings
}

func TestRecordingsListsOnlyRecordingsInTheServerDirectories(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-time.Hour)
	writeTestRecording(t, dir, "older.ndjson")
	_ = os.Chtimes(filepath.Join(dir, "older.ndjson"), old, old)
	writeTestRecording(t, dir, "newer.ndjson")
	writeFile(t, filepath.Join(dir, "outcomes.ndjson"), `{"type":"outcome_header","format":"x"}`+"\n", time.Time{})
	writeFile(t, filepath.Join(dir, "config.json"), `{"uri":"mongodb://secret"}`, time.Time{})
	writeFile(t, filepath.Join(dir, "empty.ndjson"), "", time.Time{})
	writeFile(t, filepath.Join(dir, "notes.txt"), `{"type":"header"}`, time.Time{})
	writeFile(t, filepath.Join(dir, "future.ndjson"), `{"type":"header","format":"plgm-workload-record/9"}`+"\n", old.Add(-time.Hour))
	sub := filepath.Join(dir, "nested")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestRecording(t, sub, "deep.ndjson")
	outside := t.TempDir()
	target := writeTestRecording(t, outside, "elsewhere.ndjson")
	if err := os.Symlink(target, filepath.Join(dir, "link.ndjson")); err != nil {
		t.Fatal(err)
	}

	s := NewServer(&config.AppConfig{Duration: "1s", WorkloadRecordPath: filepath.Join(dir, "plgm_workload_record.ndjson")})
	dirs, list := getRecordings(t, s)
	abs, _ := filepath.Abs(dir)
	if len(dirs) != 1 || dirs[0] != abs {
		t.Fatalf("directories = %v, want [%s]", dirs, abs)
	}
	var names []string
	for _, e := range list {
		names = append(names, e.Name)
	}
	if strings.Join(names, ",") != "newer.ndjson,older.ndjson,future.ndjson" {
		t.Fatalf("listed %v; want recordings only, newest first, no outcome files, nested files, or symlinks", names)
	}
	first := list[0]
	if first.Path != filepath.Join(dir, "newer.ndjson") || first.Source != recordingSourceDirectory || first.Workers != 2 || first.Ops == nil || *first.Ops != 2 ||
		first.Status != "complete" || first.CreatedAt != "2026-10-01T10:00:00Z" || first.SizeBytes == 0 || first.Problem != "" {
		t.Fatalf("entry = %+v", first)
	}
	if p := list[2].Problem; !strings.Contains(p, "unsupported workload recording format") {
		t.Fatalf("an unsupported recording is listed with its problem, got %q", p)
	}
}

func TestRecordingsIncludeImportsAndRecordedFilesButFormValuesCannotAddDirectories(t *testing.T) {
	dir, other := t.TempDir(), t.TempDir()
	s := NewServer(&config.AppConfig{Duration: "1s", WorkloadRecordPath: filepath.Join(dir, "rec.ndjson")})
	imported := writeTestRecording(t, other, "plgm-traffic-import-1.ndjson")
	s.keepTrafficImport(&trafficImport{ID: "i1", FileName: "app.log.ndjson", ServerPath: imported})
	recorded := writeTestRecording(t, other, "run.ndjson")
	s.rememberRecording(recorded)
	writeTestRecording(t, other, "unlisted.ndjson")
	// A later run's form values replace AppConfig; the listed directories stay.
	s.AppConfig = &config.AppConfig{WorkloadRecordPath: filepath.Join(other, "x.ndjson")}

	_, list := getRecordings(t, s)
	got := map[string]string{}
	for _, e := range list {
		got[e.Name] = e.Source
	}
	if len(got) != 2 || got["app.log.ndjson"] != recordingSourceImport || got["run.ndjson"] != recordingSourceRun {
		t.Fatalf("listed %v", got)
	}
	if err := os.Remove(recorded); err != nil {
		t.Fatal(err)
	}
	if _, list = getRecordings(t, s); len(list) != 1 {
		t.Fatalf("a deleted recording must drop out of the list: %+v", list)
	}
}

func TestRecordingsReportUnreadableFiles(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("file permissions are not enforced")
	}
	dir := t.TempDir()
	p := writeTestRecording(t, dir, "locked.ndjson")
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o600) })
	s := NewServer(&config.AppConfig{WorkloadRecordPath: filepath.Join(dir, "r.ndjson")})
	_, list := getRecordings(t, s)
	if len(list) != 1 || !strings.Contains(list[0].Problem, "permission denied") {
		t.Fatalf("list = %+v", list)
	}
}

func TestRecordingsListIsBounded(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < maxListedRecordings+5; i++ {
		writeTestRecording(t, dir, "r"+strings.Repeat("x", i%3)+time.Duration(i).String()+".ndjson")
	}
	s := NewServer(&config.AppConfig{WorkloadRecordPath: filepath.Join(dir, "r.ndjson")})
	rec := httptest.NewRecorder()
	s.handleRecordings(rec, httptest.NewRequest(http.MethodGet, "/api/recordings", nil))
	var out struct {
		Recordings []recordingEntry `json:"recordings"`
		Truncated  bool             `json:"truncated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Recordings) != maxListedRecordings || !out.Truncated {
		t.Fatalf("listed %d, truncated %v", len(out.Recordings), out.Truncated)
	}
}

func TestRecordingsRejectOtherMethods(t *testing.T) {
	s := NewServer(&config.AppConfig{})
	rec := httptest.NewRecorder()
	s.handleRecordings(rec, httptest.NewRequest(http.MethodPost, "/api/recordings", strings.NewReader("path=/etc")))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", rec.Code)
	}
}

func TestCheckReplayRecordingRevalidatesAtStart(t *testing.T) {
	dir := t.TempDir()
	ok := writeTestRecording(t, dir, "ok.ndjson")
	if err := checkReplayRecording(ok); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.ndjson")
	writeFile(t, out, `{"type":"outcome_header","format":"x"}`+"\n", time.Time{})
	for path, want := range map[string]string{
		filepath.Join(dir, "gone.ndjson"): "no longer exists on the PLGM server",
		out:                               "cannot be replayed: not a workload recording: this is an outcome file",
		dir:                               "is not a regular file",
	} {
		if err := checkReplayRecording(path); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: err = %v, want %q", path, err, want)
		}
	}

	s := NewServer(&config.AppConfig{Duration: "1s"})
	req, rec := newMultipartRequest(t, map[string]string{"default_workload": "on", "operation_mode": "replay", "workload_replay_path": filepath.Join(dir, "gone.ndjson")}, nil)
	s.handleStart(rec, req)
	assertStartErrorResponse(t, rec, http.StatusBadRequest, "refresh the recording list")
	if isRunning(s) {
		t.Fatal("run still marked running after a rejected start")
	}
}
