package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/db"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	driverMongo "go.mongodb.org/mongo-driver/v2/mongo"
)

func TestParseSweepWorkers(t *testing.T) {
	got, err := parseSweepWorkers("4, 8 16;32\n")
	if err != nil || len(got) != 4 || got[0] != 4 || got[3] != 32 {
		t.Fatalf("got %v, %v", got, err)
	}
	for in, want := range map[string]string{
		"4":                             "at least 2",
		"":                              "at least 2",
		"4,8,16,32,64,128,256,512,1024": "at most 8",
		"4,x":                           "not a worker count",
		"4,0":                           "not a worker count",
		"4,-2":                          "not a worker count",
		"4,20000":                       "not a worker count",
		"4,8,4":                         "listed twice",
	} {
		if _, err := parseSweepWorkers(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error %v, want it to mention %q", in, err, want)
		}
	}
}

func TestSweepLevelLabelKeepsTheWorkerCount(t *testing.T) {
	if got := sweepLevelLabel("mongo-8.0", 16); got != "mongo-8.0 - 16 workers" {
		t.Fatalf("got %q", got)
	}
	long := strings.Repeat("é", 60)
	got := sweepLevelLabel(long, 128)
	if len(got) > config.MaxTrendRunLabelBytes || !strings.HasSuffix(got, " - 128 workers") {
		t.Fatalf("label %q (%d bytes) lost its worker count or is too long", got, len(got))
	}
}

// sweepSeams runs every level instantly and records the worker count and the
// label each level started with.
func sweepSeams(t *testing.T, mu *sync.Mutex, seen *[]int, labels *[]string, onRun func(level int, ctx context.Context) error) {
	withWebUISeams(t, webuiSeams{
		loadCollections: func(string, bool) (*config.CollectionsFile, error) {
			return &config.CollectionsFile{Collections: []config.CollectionDefinition{{Name: "c", DatabaseName: "d"}}}, nil
		},
		loadQueries: func(string, bool) (*config.QueriesFile, error) {
			return &config.QueriesFile{Queries: []config.QueryDefinition{{Name: "q", Database: "d", Collection: "c", Operation: "find", Filter: map[string]interface{}{}}}}, nil
		},
		connect:           func(context.Context, *config.AppConfig, string) (*db.Connection, error) { return &db.Connection{}, nil },
		disconnect:        func(*db.Connection, context.Context) {},
		createCollections: func(context.Context, *driverMongo.Database, *config.CollectionsFile, bool) error { return nil },
		createIndexes:     func(context.Context, *driverMongo.Database, *config.CollectionsFile) error { return nil },
		insertRandomDocs: func(context.Context, *driverMongo.Database, config.CollectionDefinition, int, *config.AppConfig) error {
			return nil
		},
		runWorkload: func(ctx context.Context, _ *driverMongo.Database, _ []config.CollectionDefinition, _ []config.QueryDefinition, cfg *config.AppConfig, _ ...*stats.Collector) error {
			mu.Lock()
			*seen = append(*seen, cfg.Concurrency)
			*labels = append(*labels, cfg.TrendRunLabel)
			n := len(*seen)
			mu.Unlock()
			if cfg.LoadProfile.Kind != "fixed" {
				t.Errorf("level %d ran with load profile %q, want fixed", n, cfg.LoadProfile.Kind)
			}
			if onRun != nil {
				return onRun(n, ctx)
			}
			return nil
		},
	})
}

func startSweep(t *testing.T, s *WebServer, fields map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req, rec := newMultipartRequest(t, fields, nil)
	s.handleStart(rec, req)
	return rec
}

func sweepState(s *WebServer) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sweep == nil {
		return ""
	}
	return s.sweep.State
}

func TestSweepRunsEveryLevelAsItsOwnStoredRun(t *testing.T) {
	var mu sync.Mutex
	var seen []int
	var labels []string
	sweepSeams(t, &mu, &seen, &labels, nil)
	s := NewServer(&config.AppConfig{Duration: "1s", Iterations: 1, CollectionsPath: "ignored", QueriesPath: "ignored"})
	s.runHistory = OpenRunHistory(t.TempDir())
	waitForRunsOnCleanup(t, s)

	rec := startSweep(t, s, map[string]string{
		"default_workload": "on", "load_profile_kind": "sweep", "load_profile_sweep_workers": "4, 8, 16", "trend_run_label": "mongo-8.0",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", rec.Code, rec.Body.String())
	}
	waitUntil(t, 5*time.Second, func() bool { return sweepState(s) == sweepDone }, "the sweep did not finish")

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 || seen[0] != 4 || seen[1] != 8 || seen[2] != 16 {
		t.Fatalf("levels ran with %v workers, want [4 8 16]", seen)
	}
	if labels[0] != "mongo-8.0 - 4 workers" || labels[2] != "mongo-8.0 - 16 workers" {
		t.Fatalf("labels %v", labels)
	}
	runs := s.runHistory.List(time.Now(), stats.TrendPolicy{KeepRuns: 50})
	if len(runs) != 3 {
		t.Fatalf("%d stored runs, want 3", len(runs))
	}
	ids, got := map[string]bool{}, map[int]bool{}
	for _, r := range runs {
		if r.Sweep == nil || r.Sweep.Levels != 3 {
			t.Fatalf("run %s has no sweep tag: %+v", r.ID, r.Sweep)
		}
		ids[r.Sweep.ID] = true
		got[r.Sweep.Workers] = true
	}
	if len(ids) != 1 || !got[4] || !got[8] || !got[16] {
		t.Fatalf("sweep tags %v workers %v", ids, got)
	}
	if s.sweep.tag() != nil {
		t.Fatal("a finished sweep must not tag later runs")
	}
}

func TestSweepStopsWhenALevelFails(t *testing.T) {
	var mu sync.Mutex
	var seen []int
	var labels []string
	sweepSeams(t, &mu, &seen, &labels, func(level int, _ context.Context) error {
		if level == 2 {
			return context.DeadlineExceeded
		}
		return nil
	})
	s := NewServer(&config.AppConfig{Duration: "1s", Iterations: 1, CollectionsPath: "ignored", QueriesPath: "ignored"})
	waitForRunsOnCleanup(t, s)
	startSweep(t, s, map[string]string{"default_workload": "on", "load_profile_kind": "sweep", "load_profile_sweep_workers": "2,4,8"})
	waitUntil(t, 5*time.Second, func() bool { return sweepState(s) == sweepFailed }, "the sweep did not stop at the failed level")
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("levels run: %v; the third must not start after the second failed", seen)
	}
	if !strings.Contains(s.sweep.Message, "level 2 (4 workers) failed") {
		t.Fatalf("message %q", s.sweep.Message)
	}
}

func TestSweepStopEndsItBetweenLevels(t *testing.T) {
	var mu sync.Mutex
	var seen []int
	var labels []string
	sweepSeams(t, &mu, &seen, &labels, nil)
	s := NewServer(&config.AppConfig{Duration: "1s", Iterations: 1, CollectionsPath: "ignored", QueriesPath: "ignored"})
	waitForRunsOnCleanup(t, s)
	startSweep(t, s, map[string]string{"default_workload": "on", "load_profile_kind": "sweep", "load_profile_sweep_workers": "2,4,8", "interval_delay": "30s"})
	waitUntil(t, 5*time.Second, func() bool { return sweepState(s) == sweepWaiting }, "the sweep never reached the wait between levels")

	// Another run cannot start in the wait.
	rec := startSweep(t, s, map[string]string{"default_workload": "on"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("a run started during a sweep: status %d", rec.Code)
	}
	stop := httptest.NewRecorder()
	s.handleStop(stop, httptest.NewRequest(http.MethodPost, "/api/stop", nil))
	waitUntil(t, 5*time.Second, func() bool { return sweepState(s) == sweepStopped }, "stop did not end the sweep")
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 {
		t.Fatalf("levels run: %v; only the first should have run", seen)
	}
}

func TestSweepRejectsWhatItCannotRun(t *testing.T) {
	s := NewServer(&config.AppConfig{Duration: "1s", Iterations: 1})
	for name, f := range map[string]map[string]string{
		"one level":    {"load_profile_kind": "sweep", "load_profile_sweep_workers": "4"},
		"record":       {"load_profile_kind": "sweep", "load_profile_sweep_workers": "4,8", "operation_mode": "record"},
		"replay":       {"load_profile_kind": "sweep", "load_profile_sweep_workers": "4,8", "operation_mode": "replay"},
		"raw injector": {"load_profile_kind": "sweep", "load_profile_sweep_workers": "4,8", "operation_mode": "raw"},
		"missing list": {"load_profile_kind": "sweep"},
	} {
		rec := startSweep(t, s, f)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400 (%s)", name, rec.Code, rec.Body.String())
			continue
		}
		var resp map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		if resp["status"] != "error" || resp["message"] == "" {
			t.Errorf("%s: response %v", name, resp)
		}
	}
	if s.IsRunning || s.sweep != nil {
		t.Fatal("a rejected sweep must leave no run or sweep behind")
	}
}

func TestSweepAllowsDeterministicTrafficButNotAFixedOutcomeFile(t *testing.T) {
	var mu sync.Mutex
	var seen []int
	var labels []string
	sweepSeams(t, &mu, &seen, &labels, nil)
	s := NewServer(&config.AppConfig{Duration: "1s", Iterations: 1, CollectionsPath: "ignored", QueriesPath: "ignored"})
	s.runHistory = OpenRunHistory(t.TempDir())
	waitForRunsOnCleanup(t, s)

	rec := startSweep(t, s, map[string]string{
		"default_workload": "on", "load_profile_kind": "sweep", "load_profile_sweep_workers": "4, 8",
		"workload_deterministic": "true", "workload_deterministic_outcomes_path": "/tmp/one.ndjson",
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Deterministic Outcome File") {
		t.Fatalf("fixed outcome path: status %d body=%s", rec.Code, rec.Body.String())
	}
	rec = startSweep(t, s, map[string]string{
		"default_workload": "on", "load_profile_kind": "sweep", "load_profile_sweep_workers": "4, 8",
		"workload_deterministic": "true", "random_seed": "7", "operations_per_worker": "10",
		"drop_collections": "true",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", rec.Code, rec.Body.String())
	}
	waitUntil(t, 5*time.Second, func() bool { return sweepState(s) == sweepDone }, "the sweep did not finish")
}
