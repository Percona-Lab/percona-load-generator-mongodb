package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/db"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	driverMongo "go.mongodb.org/mongo-driver/v2/mongo"
)

func TestRunHistorySurvivesReopenAndRedactsSecrets(t *testing.T) {
	dir := t.TempDir()
	h := OpenRunHistory(dir)
	s := &WebServer{
		runHistory:     h,
		dashboardID:    "run-test-1",
		dashboardMode:  "generic",
		LifecyclePhase: "completed",
		CurrentStats:   stats.NewCollector(),
		AppConfig: &config.AppConfig{
			URI:           "mongodb://admin:s3cret@localhost:27017/admin",
			Duration:      "30s",
			Concurrency:   4,
			TrendRunLabel: "baseline",
			ConnectionParams: config.ConnectionParams{
				Username: "admin", Password: "s3cret", TLSCAFile: "/secret/ca.pem",
			},
		},
		InitStartedAt: time.Now().Add(-time.Minute),
		ExecStartedAt: time.Now().Add(-30 * time.Second),
		CompletedAt:   time.Now(),
	}
	s.CurrentStats.FindOps = 12
	s.persistDashboard()

	raw, err := os.ReadFile(filepath.Join(dir, "run-test-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, secret := range []string{"s3cret", "admin:s3cret", "/secret/ca.pem"} {
		if strings.Contains(text, secret) {
			t.Fatalf("dashboard stored %q", secret)
		}
	}
	reopened := OpenRunHistory(dir)
	list := reopened.List(time.Now(), stats.DefaultTrendPolicy())
	if len(list) != 1 || list[0].Label != "baseline" || list[0].Status != "completed" || list[0].Mode != "generic" {
		t.Fatalf("list: %+v", list)
	}
	if list[0].TotalOps == nil || *list[0].TotalOps != 12 {
		t.Fatalf("total ops: %+v", list[0].TotalOps)
	}
	if list[0].AvgLatencyMs != nil {
		t.Fatalf("latency with no samples should be omitted, got %v", *list[0].AvgLatencyMs)
	}
	got, err := reopened.Read("run-test-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Stats["historical"] != true || got.Stats["isRunning"] != false {
		t.Fatalf("stats: %+v", got.Stats)
	}
	if _, ok := got.Config["uri"]; !ok || strings.Contains(fmtSprint(got.Config["uri"]), "s3cret") {
		t.Fatalf("uri: %v", got.Config["uri"])
	}
}

func TestRunHistoryDamagedFileDoesNotHideOthers(t *testing.T) {
	dir := t.TempDir()
	h := OpenRunHistory(dir)
	good := &RunDashboard{RunSummary: RunSummary{ID: "run-good", Schema: runDashboardSchema, Status: "completed", StartedAt: time.Now().UTC().Format(time.RFC3339), Mode: "custom"}}
	if err := h.Save(good, stats.DefaultTrendPolicy()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run-bad.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run-old.json"), []byte(`{"id":"run-old","schema":"rd-0","label":"old"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	list := h.List(time.Now(), stats.DefaultTrendPolicy())
	var statuses = map[string]string{}
	for _, row := range list {
		statuses[row.ID] = row.Status
	}
	if statuses["run-good"] != "completed" || statuses["run-bad"] != "damaged" || statuses["run-old"] != "unsupported" {
		t.Fatalf("statuses: %+v", statuses)
	}
	got, err := h.Read("run-good")
	if err != nil || got.Status != "completed" {
		t.Fatalf("good run: %v %+v", err, got)
	}
	bad, err := h.Read("run-bad")
	if err != nil || bad.Status != "damaged" || bad.Stats != nil || bad.Error == "" {
		t.Fatalf("damaged run: %v %+v", err, bad)
	}
	old, err := h.Read("run-old")
	if err != nil || old.Status != "unsupported" || old.Stats != nil {
		t.Fatalf("old run: %v %+v", err, old)
	}
}

func TestRunHistoryPruneKeepsPinned(t *testing.T) {
	dir := t.TempDir()
	h := OpenRunHistory(dir)
	policy := stats.TrendPolicy{KeepRuns: 1}
	older := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	newer := time.Now().UTC().Format(time.RFC3339)
	for _, snap := range []*RunDashboard{
		{RunSummary: RunSummary{ID: "run-old", Schema: runDashboardSchema, Status: "completed", StartedAt: older, Pinned: true}},
		{RunSummary: RunSummary{ID: "run-mid", Schema: runDashboardSchema, Status: "failed", StartedAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}},
		{RunSummary: RunSummary{ID: "run-new", Schema: runDashboardSchema, Status: "completed", StartedAt: newer}},
	} {
		if err := h.Save(snap, policy); err != nil {
			t.Fatal(err)
		}
	}
	list := h.List(time.Now(), policy)
	ids := map[string]bool{}
	for _, row := range list {
		if row.Schema == runDashboardSchema {
			ids[row.ID] = true
		}
	}
	if !ids["run-old"] || !ids["run-new"] || ids["run-mid"] {
		t.Fatalf("prune kept %+v", ids)
	}
}

func TestRunHistoryHTTPReportsMissingAndDamaged(t *testing.T) {
	dir := t.TempDir()
	s := &WebServer{runHistory: OpenRunHistory(dir), AppConfig: &config.AppConfig{}}
	if err := os.WriteFile(filepath.Join(dir, "run-bad.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.handleRunHistoryItem(rec, httptest.NewRequest(http.MethodGet, "/api/run-history/missing", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	s.handleRunHistoryItem(rec, httptest.NewRequest(http.MethodGet, "/api/run-history/run-bad", nil))
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "damaged") {
		t.Fatalf("damaged: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	s.handleRunHistory(rec, httptest.NewRequest(http.MethodGet, "/api/run-history", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "run-bad") || !strings.Contains(rec.Body.String(), dir) {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
}

func TestHandleStartOperationModeIgnoresHiddenReplay(t *testing.T) {
	var seen config.AppConfig
	withWebUISeams(t, webuiSeams{
		loadCollections: func(path string, loadDefault bool) (*config.CollectionsFile, error) {
			if !loadDefault {
				t.Fatalf("generic mode should load the built-in workload")
			}
			return &config.CollectionsFile{Collections: []config.CollectionDefinition{{DatabaseName: "shop", Name: "orders"}}}, nil
		},
		loadQueries: func(path string, loadDefault bool) (*config.QueriesFile, error) {
			return &config.QueriesFile{Queries: []config.QueryDefinition{{Name: "find_orders", Collection: "orders", Operation: "find", Filter: map[string]interface{}{}}}}, nil
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
			seen = *cfg
			return nil
		},
	})
	dir := t.TempDir()
	s := NewServer(&config.AppConfig{Duration: "1s", Iterations: 1, DocumentsCount: 0})
	s.runHistory = OpenRunHistory(dir)
	waitForRunsOnCleanup(t, s)
	req, rec := newMultipartRequest(t, map[string]string{
		"operation_mode":          "generic",
		"uri":                     "mongodb://user:secret@localhost:27017",
		"workload_record_enabled": "true",
		"workload_replay_enabled": "true",
		"workload_replay_path":    "replay.ndjson",
		"raw_injector_enabled":    "true",
		"concurrency":             "8",
		"duration":                "5s",
		"documents_count":         "0",
		"interval_delay":          "0s",
	}, nil)
	s.handleStart(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	s.runWG.Wait()
	if seen.WorkloadRecordEnabled || seen.WorkloadReplayEnabled || seen.RawInjector.Enabled || !seen.DefaultWorkload {
		t.Fatalf("effective config: record=%v replay=%v raw=%v default=%v", seen.WorkloadRecordEnabled, seen.WorkloadReplayEnabled, seen.RawInjector.Enabled, seen.DefaultWorkload)
	}
	if seen.Concurrency != 8 || seen.Duration != "5s" {
		t.Fatalf("compatible settings: %+v", seen)
	}
	list := s.runHistory.List(time.Now(), stats.DefaultTrendPolicy())
	if len(list) != 1 || list[0].Status != "completed" || list[0].Mode != "generic" {
		t.Fatalf("saved dashboard: %+v", list)
	}
	raw, err := os.ReadFile(filepath.Join(dir, list[0].ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret") {
		t.Fatal("saved dashboard contains the URI password")
	}
}

func fmtSprint(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestRunHistoryReportOpensInlineOrDownloads(t *testing.T) {
	dir := t.TempDir()
	s := &WebServer{runHistory: OpenRunHistory(dir), AppConfig: &config.AppConfig{}}
	snap := &RunDashboard{RunSummary: RunSummary{ID: "run-1", Schema: runDashboardSchema, Status: "completed", StartedAt: time.Now().UTC().Format(time.RFC3339), Mode: "replay"}}
	if err := s.runHistory.Save(snap, stats.DefaultTrendPolicy()); err != nil {
		t.Fatal(err)
	}
	for target, want := range map[string]string{
		"/api/run-history/run-1/report":            `inline; filename="plgm-report-run-1.html"`,
		"/api/run-history/run-1/report?download=1": `attachment; filename="plgm-report-run-1.html"`,
	} {
		rec := httptest.NewRecorder()
		s.handleRunHistoryItem(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Disposition") != want || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("%s: %d %v", target, rec.Code, rec.Header())
		}
	}
}
