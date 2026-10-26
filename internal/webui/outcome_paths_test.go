package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/db"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	driverMongo "go.mongodb.org/mongo-driver/v2/mongo"
)

func threeTargets() []config.ReplayTarget {
	return []config.ReplayTarget{{Label: "m9", URI: "mongodb://a:1"}, {Label: "rs02", URI: "mongodb://b:2"}, {Label: "RS03", URI: "mongodb://c:3"}}
}

func TestOutcomePathToGenerate(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg  config.AppConfig
		want string
	}{
		"outcomes off":              {config.AppConfig{WorkloadReplayEnabled: true}, ""},
		"replay, blank":             {config.AppConfig{WorkloadRecordOutcomes: true, WorkloadReplayEnabled: true}, "workload_replay_outcomes_path"},
		"replay, explicit":          {config.AppConfig{WorkloadRecordOutcomes: true, WorkloadReplayEnabled: true, WorkloadReplayOutcomesPath: "mine.ndjson"}, ""},
		"deterministic, blank":      {config.AppConfig{WorkloadRecordOutcomes: true, WorkloadDeterministic: true}, "workload_deterministic_outcomes_path"},
		"deterministic + recording": {config.AppConfig{WorkloadRecordOutcomes: true, WorkloadDeterministic: true, WorkloadRecordEnabled: true}, ""},
		"recording only":            {config.AppConfig{WorkloadRecordOutcomes: true, WorkloadRecordEnabled: true}, ""},
		"generated run":             {config.AppConfig{WorkloadRecordOutcomes: true}, ""},
	} {
		cfg := tc.cfg
		if got := outcomePathToGenerate(&cfg); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}

func TestAssignOutcomePathsReservesUniqueFilesPerTarget(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.AppConfig{WorkloadRecordOutcomes: true, WorkloadReplayEnabled: true, WorkloadReplayTargets: threeTargets()}
	g, err := assignOutcomePaths(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	base := cfg.WorkloadReplayOutcomesPath
	if g.Path != base || filepath.Dir(base) != dir || !strings.HasPrefix(filepath.Base(base), "plgm_replay_outcomes-") || filepath.Ext(base) != ".ndjson" {
		t.Fatalf("generated %q", base)
	}
	if len(g.Files) != 3 {
		t.Fatalf("files %v", g.Files)
	}
	for i, tg := range threeTargets() {
		want := config.TargetPath(base, tg.Label)
		if g.Files[i] != want || !strings.Contains(filepath.Base(want), "."+tg.Label+".") {
			t.Fatalf("target %s file %q, want %q", tg.Label, g.Files[i], want)
		}
		fi, err := os.Stat(want)
		if err != nil || fi.Size() != 0 || fi.Mode().Perm() != 0o600 {
			t.Fatalf("reserved %s: %v %v", want, fi, err)
		}
	}
	if _, err := os.Stat(base); !os.IsNotExist(err) {
		t.Fatal("a multi-target run writes only the per-target files")
	}
	if err := config.ValidateWorkloadOutcomes(cfg); err != nil {
		t.Fatal(err)
	}
	if err := config.ValidateWorkloadTargets(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestAssignOutcomePathsKeepsAnExplicitPath(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.AppConfig{WorkloadRecordOutcomes: true, WorkloadReplayEnabled: true, WorkloadReplayOutcomesPath: "chosen.ndjson"}
	g, err := assignOutcomePaths(cfg, dir)
	if err != nil || g != nil || cfg.WorkloadReplayOutcomesPath != "chosen.ndjson" {
		t.Fatalf("explicit path changed: %v %v %q", g, err, cfg.WorkloadReplayOutcomesPath)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatal("nothing is reserved for an explicit path")
	}
}

func TestAssignOutcomePathsNeverReplacesAnExistingFile(t *testing.T) {
	dir := t.TempDir()
	fixed := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	suffixes := []string{"aaaa", "aaaa", "bbbb"}
	oldClock, oldSuffix := outcomeNameClock, outcomeNameSuffix
	outcomeNameClock = func() time.Time { return fixed }
	outcomeNameSuffix = func() string { s := suffixes[0]; suffixes = suffixes[1:]; return s }
	t.Cleanup(func() { outcomeNameClock, outcomeNameSuffix = oldClock, oldSuffix })

	// Target rs02's file of the first candidate already exists.
	taken := filepath.Join(dir, "plgm_replay_outcomes-20261026T120000Z-aaaa.rs02.ndjson")
	writeFile(t, taken, "keep me", time.Time{})
	cfg := &config.AppConfig{WorkloadRecordOutcomes: true, WorkloadReplayEnabled: true, WorkloadReplayTargets: threeTargets()}
	g, err := assignOutcomePaths(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(g.Path, "-bbbb.ndjson") {
		t.Fatalf("generated %q; a taken name must be replaced by a new one", g.Path)
	}
	if b, _ := os.ReadFile(taken); string(b) != "keep me" {
		t.Fatal("an existing file was overwritten")
	}
	if _, err := os.Stat(filepath.Join(dir, "plgm_replay_outcomes-20261026T120000Z-aaaa.m9.ndjson")); !os.IsNotExist(err) {
		t.Fatal("the files reserved for the abandoned name must be removed")
	}
}

func TestAssignOutcomePathsConcurrentRunsGetDistinctFiles(t *testing.T) {
	dir := t.TempDir()
	fixed := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	oldClock := outcomeNameClock
	outcomeNameClock = func() time.Time { return fixed }
	t.Cleanup(func() { outcomeNameClock = oldClock })
	const n = 16
	var wg sync.WaitGroup
	paths := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cfg := &config.AppConfig{WorkloadRecordOutcomes: true, WorkloadDeterministic: true}
			g, err := assignOutcomePaths(cfg, dir)
			errs[i] = err
			if g != nil {
				paths[i] = g.Path
			}
		}(i)
	}
	wg.Wait()
	seen := map[string]bool{}
	for i, p := range paths {
		if errs[i] != nil || p == "" || seen[p] || !strings.Contains(p, "plgm_deterministic_outcomes-") {
			t.Fatalf("run %d: %q %v (duplicate: %v)", i, p, errs[i], seen[p])
		}
		seen[p] = true
	}
}

func TestAssignOutcomePathsReportsAnUnwritableDirectory(t *testing.T) {
	cfg := &config.AppConfig{WorkloadRecordOutcomes: true, WorkloadReplayEnabled: true}
	_, err := assignOutcomePaths(cfg, filepath.Join(t.TempDir(), "missing"))
	if err == nil || !strings.Contains(err.Error(), "set workload_replay_outcomes_path") || cfg.WorkloadReplayOutcomesPath != "" {
		t.Fatalf("err = %v, path %q", err, cfg.WorkloadReplayOutcomesPath)
	}
}

func TestHandleStartRejectedRunRemovesNothingAndReservesNothing(t *testing.T) {
	dir := t.TempDir()
	s := NewServer(&config.AppConfig{Duration: "1s", WorkloadRecordPath: filepath.Join(dir, "rec.ndjson")})
	// The labels are invalid, so no file may be created for them.
	req, rec := newMultipartRequest(t, map[string]string{"default_workload": "on", "operation_mode": "replay",
		"workload_replay_path": writeTestRecording(t, t.TempDir(), "in.ndjson"), "workload_record_outcomes": "true",
		"workload_replay_targets": `[{"label":"../up","uri":"mongodb://a"},{"label":"b","uri":"mongodb://b"}]`}, nil)
	s.handleStart(rec, req)
	assertStartErrorResponse(t, rec, http.StatusBadRequest, "label")
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("a rejected run created %v", entries)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "up")); err == nil {
		t.Fatal("a target label escaped the directory")
	}
}

// TestHandleStartThreeTargetSequentialReplay runs one recording on three
// targets in order with a blank outcome path: every target writes its own
// generated outcome file, the dashboard lists them, and each comparison with
// the first target is kept and opens inline or downloads.
func TestHandleStartThreeTargetSequentialReplay(t *testing.T) {
	fx := writeReplayFixture(t)
	dir, recording := fx.dir, fx.recording
	var mu sync.Mutex
	var order []string
	running := 0
	overlap := false
	withWebUISeams(t, webuiSeams{
		loadCollections: func(path string, loadDefault bool) (*config.CollectionsFile, error) {
			return &config.CollectionsFile{Collections: []config.CollectionDefinition{{Name: "flights", DatabaseName: "benchdb"}}}, nil
		},
		loadQueries: func(path string, loadDefault bool) (*config.QueriesFile, error) {
			return &config.QueriesFile{Queries: []config.QueryDefinition{{Name: "f", Database: "benchdb", Collection: "flights", Operation: "find", Filter: map[string]interface{}{}}}}, nil
		},
		connect: func(ctx context.Context, cfg *config.AppConfig, dbName string) (*db.Connection, error) {
			return &db.Connection{}, nil
		},
		disconnect: func(c *db.Connection, ctx context.Context) {},
		createCollections: func(ctx context.Context, database *driverMongo.Database, cfg *config.CollectionsFile, drop bool) error {
			return nil
		},
		createIndexes: func(ctx context.Context, database *driverMongo.Database, cfg *config.CollectionsFile) error {
			return nil
		},
		runWorkload: func(ctx context.Context, database *driverMongo.Database, collections []config.CollectionDefinition, queries []config.QueryDefinition, cfg *config.AppConfig, uiCollector ...*stats.Collector) error {
			mu.Lock()
			running++
			overlap = overlap || running > 1
			order = append(order, cfg.URI)
			mu.Unlock()
			defer func() { mu.Lock(); running--; mu.Unlock() }()
			if cfg.WorkloadReplayPath != recording {
				return fmt.Errorf("replayed %q", cfg.WorkloadReplayPath)
			}
			// Every target returns the fixture's outcomes (side A).
			b, err := os.ReadFile(fx.a)
			if err != nil {
				return err
			}
			uiCollector[0].Track("find", time.Millisecond)
			return os.WriteFile(cfg.WorkloadReplayOutcomesPath, b, 0o600)
		},
	})
	s := NewServer(&config.AppConfig{Duration: "1s", Iterations: 1, CollectionsPath: "ignored", QueriesPath: "ignored", TrendBaselineStore: "off", WorkloadRecordPath: recording})
	waitForRunsOnCleanup(t, s)
	targets, _ := json.Marshal(threeTargets())
	req, rec := newMultipartRequest(t, map[string]string{"default_workload": "on", "documents_count": "0", "operation_mode": "replay",
		"workload_replay_path": recording, "workload_record_outcomes": "true", "workload_replay_outcomes_path": "",
		"workload_replay_targets": string(targets)}, nil)
	s.handleStart(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("start status %d body=%s", rec.Code, rec.Body.String())
	}
	var started struct {
		Status       string   `json:"status"`
		OutcomeFiles []string `json:"outcome_files"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil || started.Status != "started" || len(started.OutcomeFiles) != 3 {
		t.Fatalf("start response %s", rec.Body.String())
	}
	waitUntil(t, 5*time.Second, func() bool { return !isRunning(s) }, "run did not finish")
	s.runWG.Wait()

	if overlap || strings.Join(order, ",") != "mongodb://a:1,mongodb://b:2,mongodb://c:3" {
		t.Fatalf("targets ran %v (overlap %v); sequential runs one target at a time in order", order, overlap)
	}
	plan := s.multiTarget
	res := plan.Result()
	if s.LastError != "" || res == nil || len(res.Comparisons) != 2 {
		t.Fatalf("last error %q result %+v", s.LastError, res)
	}
	for i, f := range started.OutcomeFiles {
		fi, err := os.Stat(f)
		if err != nil || fi.Size() == 0 || filepath.Dir(f) != dir || plan.Targets[i].OutcomesPath != f {
			t.Fatalf("target %d outcome %q: %v %v (plan %q)", i+1, f, fi, err, plan.Targets[i].OutcomesPath)
		}
	}

	statsRec := httptest.NewRecorder()
	s.handleStats(statsRec, httptest.NewRequest(http.MethodGet, "/api/stats", nil))
	var payload struct {
		MultiTarget struct {
			Targets     []map[string]interface{} `json:"targets"`
			Comparisons []map[string]interface{} `json:"comparisons"`
		} `json:"multi_target"`
	}
	if err := json.Unmarshal(statsRec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	for i, tg := range payload.MultiTarget.Targets {
		abs, _ := filepath.Abs(started.OutcomeFiles[i])
		if tg["outcome_file"] != abs || tg["status"] != "complete" {
			t.Fatalf("target %d = %v", i+1, tg)
		}
	}
	for _, c := range payload.MultiTarget.Comparisons {
		id, _ := c["report_id"].(string)
		if c["reference"] != "m9" || id == "" {
			t.Fatalf("comparison %v", c)
		}
		if c["differing_ops"] != float64(0) {
			t.Fatalf("identical outcomes must not differ: %v", c)
		}
		code, _, page := replayCall(t, s, http.MethodGet, "/api/replay-compare/"+id+"/report.html", nil, "")
		body := page.Body.String()
		if code != http.StatusOK || !strings.HasPrefix(page.Header().Get("Content-Disposition"), "inline") || !strings.Contains(body, "m9") || strings.Contains(body, "mongodb://") {
			t.Fatalf("report %s: %d %v", id, code, page.Header())
		}
		code, _, dl := replayCall(t, s, http.MethodGet, "/api/replay-compare/"+id+"/report.html?download=1", nil, "")
		if code != http.StatusOK || !strings.HasPrefix(dl.Header().Get("Content-Disposition"), "attachment") {
			t.Fatalf("download %s: %d %v", id, code, dl.Header())
		}
	}
	if strings.Contains(statsRec.Body.String(), "mongodb://") {
		t.Fatal("/api/stats must not expose target URIs")
	}
}

func TestComparisonReportPageSecurityAndMissingReports(t *testing.T) {
	s := NewServer(&config.AppConfig{})
	for _, path := range []string{
		"/api/replay-compare/0123456789abcdef/report.html",
		"/api/replay-compare/../../etc/passwd/report.html",
		"/api/replay-compare/%2e%2e%2fsecret/report.html",
	} {
		code, _, rec := replayCall(t, s, http.MethodGet, path, nil, "")
		body := rec.Body.String()
		if code != http.StatusNotFound || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") ||
			!strings.Contains(body, "no longer available") || !strings.Contains(body, "Run the comparison again") {
			t.Fatalf("%s: %d %q", path, code, body)
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "default-src 'none'") {
			t.Fatalf("%s: headers %v", path, rec.Header())
		}
	}
	if code, out, _ := replayCall(t, s, http.MethodGet, "/api/replay-compare/0123456789abcdef", nil, ""); code != http.StatusNotFound || out["error"] == nil {
		t.Fatalf("JSON lookups still answer JSON: %d %v", code, out)
	}
	if code, _, _ := replayCall(t, s, http.MethodPost, "/api/replay-compare/x/report.html", nil, ""); code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", code)
	}
}

// TestGeneratedOutcomeNamesAreNotReusedByLaterRuns: after a run with a
// blank outcome path, the form sees the setting blank again and the next
// run (even an API call that omits the field) gets a new file.
func TestGeneratedOutcomeNamesAreNotReusedByLaterRuns(t *testing.T) {
	fx := writeReplayFixture(t)
	withWebUISeams(t, webuiSeams{
		loadCollections: func(path string, loadDefault bool) (*config.CollectionsFile, error) {
			return &config.CollectionsFile{Collections: []config.CollectionDefinition{{Name: "flights", DatabaseName: "benchdb"}}}, nil
		},
		loadQueries: func(path string, loadDefault bool) (*config.QueriesFile, error) {
			return &config.QueriesFile{Queries: []config.QueryDefinition{{Name: "f", Database: "benchdb", Collection: "flights", Operation: "find", Filter: map[string]interface{}{}}}}, nil
		},
		connect: func(ctx context.Context, cfg *config.AppConfig, dbName string) (*db.Connection, error) {
			return &db.Connection{}, nil
		},
		disconnect: func(c *db.Connection, ctx context.Context) {},
		createCollections: func(ctx context.Context, database *driverMongo.Database, cfg *config.CollectionsFile, drop bool) error {
			return nil
		},
		createIndexes: func(ctx context.Context, database *driverMongo.Database, cfg *config.CollectionsFile) error {
			return nil
		},
		runWorkload: func(ctx context.Context, database *driverMongo.Database, collections []config.CollectionDefinition, queries []config.QueryDefinition, cfg *config.AppConfig, uiCollector ...*stats.Collector) error {
			b, err := os.ReadFile(fx.a)
			if err != nil {
				return err
			}
			return os.WriteFile(cfg.WorkloadReplayOutcomesPath, b, 0o600)
		},
	})
	s := NewServer(&config.AppConfig{Duration: "1s", Iterations: 1, CollectionsPath: "ignored", QueriesPath: "ignored", TrendBaselineStore: "off", WorkloadRecordPath: fx.recording})
	waitForRunsOnCleanup(t, s)
	run := func(fields map[string]string) string {
		t.Helper()
		req, rec := newMultipartRequest(t, fields, nil)
		s.handleStart(rec, req)
		var out struct {
			OutcomeFiles []string `json:"outcome_files"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || len(out.OutcomeFiles) != 1 {
			t.Fatalf("start %d %s", rec.Code, rec.Body.String())
		}
		waitUntil(t, 5*time.Second, func() bool { return !isRunning(s) }, "run did not finish")
		s.runWG.Wait()
		return out.OutcomeFiles[0]
	}
	base := map[string]string{"default_workload": "on", "operation_mode": "replay", "workload_replay_path": fx.recording, "workload_record_outcomes": "true"}
	first := run(map[string]string{"default_workload": "on", "operation_mode": "replay", "workload_replay_path": fx.recording, "workload_record_outcomes": "true", "workload_replay_outcomes_path": ""})

	cfgRec := httptest.NewRecorder()
	s.handleGetConfig(cfgRec, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	var served config.AppConfig
	if err := json.Unmarshal(cfgRec.Body.Bytes(), &served); err != nil || served.WorkloadReplayOutcomesPath != "" {
		t.Fatalf("the form must see the outcome path blank again, got %q (%v)", served.WorkloadReplayOutcomesPath, err)
	}
	firstContent, _ := os.ReadFile(first)
	second := run(base) // the field is omitted, as an API client might
	if second == first {
		t.Fatalf("both runs wrote %s", first)
	}
	if now, _ := os.ReadFile(first); string(now) != string(firstContent) || len(now) == 0 {
		t.Fatal("the first run's outcome file was changed by the second run")
	}
	if s.statsOutcomeFileForTest() != absPath(second) {
		t.Fatalf("/api/stats outcomeFile = %q, want %q", s.statsOutcomeFileForTest(), absPath(second))
	}
}

func (s *WebServer) statsOutcomeFileForTest() string {
	rec := httptest.NewRecorder()
	s.handleStats(rec, httptest.NewRequest(http.MethodGet, "/api/stats", nil))
	var out struct {
		OutcomeFile string `json:"outcomeFile"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out.OutcomeFile
}

func TestAssignRecordPathNamesEachRecordingAndKeepsExplicitNames(t *testing.T) {
	dir := t.TempDir()
	for comp, ext := range map[string]string{"": ".ndjson", "none": ".ndjson", "zstd": ".ndjson.zst", "gzip": ".ndjson.gz"} {
		cfg := &config.AppConfig{WorkloadRecordEnabled: true, WorkloadRecordCompression: comp}
		g, err := assignRecordPath(cfg, dir)
		if err != nil {
			t.Fatal(err)
		}
		base := filepath.Base(cfg.WorkloadRecordPath)
		if g.Path != cfg.WorkloadRecordPath || !strings.HasPrefix(base, "plgm_workload_record-") || !strings.HasSuffix(base, ext) || strings.HasSuffix(strings.TrimSuffix(base, ext), ".ndjson") {
			t.Fatalf("compression %q: generated %q", comp, cfg.WorkloadRecordPath)
		}
		if fi, err := os.Stat(cfg.WorkloadRecordPath); err != nil || fi.Size() != 0 || fi.Mode().Perm() != 0o600 {
			t.Fatalf("reserved %s: %v %v", cfg.WorkloadRecordPath, fi, err)
		}
	}
	cfg := &config.AppConfig{WorkloadRecordEnabled: true, WorkloadRecordPath: "mine.ndjson"}
	if g, err := assignRecordPath(cfg, dir); g != nil || err != nil || cfg.WorkloadRecordPath != "mine.ndjson" {
		t.Fatalf("explicit name changed: %v %v %q", g, err, cfg.WorkloadRecordPath)
	}
	if g, _ := assignRecordPath(&config.AppConfig{}, dir); g != nil {
		t.Fatal("a run that does not record gets no recording file")
	}
}

// TestRecordRunsGetNewRecordingFiles: two record runs with a blank file
// name write two recordings, both listed for replay, and the form sees the
// name blank again; /api/config?defaults=1 still returns the startup values.
func TestRecordRunsGetNewRecordingFiles(t *testing.T) {
	dir := t.TempDir()
	withWebUISeams(t, webuiSeams{
		loadCollections: func(path string, loadDefault bool) (*config.CollectionsFile, error) {
			return &config.CollectionsFile{Collections: []config.CollectionDefinition{{Name: "flights", DatabaseName: "benchdb"}}}, nil
		},
		loadQueries: func(path string, loadDefault bool) (*config.QueriesFile, error) {
			return &config.QueriesFile{Queries: []config.QueryDefinition{{Name: "f", Database: "benchdb", Collection: "flights", Operation: "find", Filter: map[string]interface{}{}}}}, nil
		},
		connect: func(ctx context.Context, cfg *config.AppConfig, dbName string) (*db.Connection, error) {
			return &db.Connection{}, nil
		},
		disconnect: func(c *db.Connection, ctx context.Context) {},
		createCollections: func(ctx context.Context, database *driverMongo.Database, cfg *config.CollectionsFile, drop bool) error {
			return nil
		},
		createIndexes: func(ctx context.Context, database *driverMongo.Database, cfg *config.CollectionsFile) error {
			return nil
		},
		runWorkload: func(ctx context.Context, database *driverMongo.Database, collections []config.CollectionDefinition, queries []config.QueryDefinition, cfg *config.AppConfig, uiCollector ...*stats.Collector) error {
			// Stand-in for the recorder: a header line in the reserved file.
			return os.WriteFile(cfg.WorkloadRecordPath, []byte(`{"type":"header","format":"plgm-workload-record/1","workers":1}`+"\n"), 0o600)
		},
	})
	s := NewServer(&config.AppConfig{Duration: "1s", Concurrency: 7, Iterations: 1, CollectionsPath: "ignored", QueriesPath: "ignored", TrendBaselineStore: "off", WorkloadRecordPath: filepath.Join(dir, "default.ndjson")})
	s.recordingDirs = []string{dir}
	waitForRunsOnCleanup(t, s)
	run := func() string {
		t.Helper()
		req, rec := newMultipartRequest(t, map[string]string{"default_workload": "on", "operation_mode": "record", "concurrency": "12", "workload_record_path": "", "workload_record_compression": "zstd"}, nil)
		s.handleStart(rec, req)
		var out struct {
			RecordingFile string `json:"recording_file"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.RecordingFile == "" {
			t.Fatalf("start %d %s", rec.Code, rec.Body.String())
		}
		waitUntil(t, 5*time.Second, func() bool { return !isRunning(s) }, "run did not finish")
		s.runWG.Wait()
		return out.RecordingFile
	}
	first, second := run(), run()
	if first == second || filepath.Dir(first) != dir || !strings.HasSuffix(first, ".ndjson.zst") {
		t.Fatalf("recordings %q and %q", first, second)
	}
	if _, list := getRecordings(t, s); len(list) != 2 {
		t.Fatalf("both recordings are listed for replay: %+v", list)
	}
	if got := s.statsRecordingFileForTest(); got != absPath(second) {
		t.Fatalf("/api/stats recordingFile = %q", got)
	}
	served := func(target string) config.AppConfig {
		rec := httptest.NewRecorder()
		s.handleGetConfig(rec, httptest.NewRequest(http.MethodGet, target, nil))
		var c config.AppConfig
		if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	if c := served("/api/config"); c.WorkloadRecordPath != "" || c.Concurrency != 12 {
		t.Fatalf("last-run config: record path %q, concurrency %d", c.WorkloadRecordPath, c.Concurrency)
	}
	if c := served("/api/config?defaults=1"); c.Concurrency != 7 || c.WorkloadRecordEnabled || c.WorkloadRecordPath != filepath.Join(dir, "default.ndjson") {
		t.Fatalf("defaults: concurrency %d, record %v %q", c.Concurrency, c.WorkloadRecordEnabled, c.WorkloadRecordPath)
	}
}

func (s *WebServer) statsRecordingFileForTest() string {
	rec := httptest.NewRecorder()
	s.handleStats(rec, httptest.NewRequest(http.MethodGet, "/api/stats", nil))
	var out struct {
		RecordingFile string `json:"recordingFile"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out.RecordingFile
}
