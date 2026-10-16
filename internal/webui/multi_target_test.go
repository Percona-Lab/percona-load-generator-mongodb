package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/db"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/report"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/workloadrec"
	driverMongo "go.mongodb.org/mongo-driver/v2/mongo"
)

func TestConcurrentTargetExplainsAreAllCancelledByNewRun(t *testing.T) {
	s := &WebServer{}
	ctxA, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelA()
	defer cancelB()
	unregisterA := s.registerExplainCancel(cancelA)
	unregisterB := s.registerExplainCancel(cancelB)

	s.mu.Lock()
	s.cancelExplainsLocked()
	s.mu.Unlock()
	if ctxA.Err() == nil || ctxB.Err() == nil {
		t.Fatalf("a new run must stop both targets' explains: A=%v B=%v", ctxA.Err(), ctxB.Err())
	}

	unregisterA()
	ctxC, cancelC := context.WithCancel(context.Background())
	defer cancelC()
	unregisterC := s.registerExplainCancel(cancelC)
	unregisterB()
	s.mu.Lock()
	n := len(s.explainCancels)
	s.cancelExplainsLocked()
	s.mu.Unlock()
	if n != 1 || ctxC.Err() == nil {
		t.Fatalf("finishing one target's explain must not drop another's handle: registered=%d C=%v", n, ctxC.Err())
	}
	unregisterC()
	if len(s.explainCancels) != 0 {
		t.Fatalf("handles left after unregister: %d", len(s.explainCancels))
	}
}

func TestApplyWorkloadRecordFormMultiTargetFields(t *testing.T) {
	cfg := &config.AppConfig{WorkloadReplayTargets: []config.ReplayTarget{{Label: "keep", URI: "mongodb://k"}}, WorkloadReplayOps: "reads_only"}
	if err := applyWorkloadRecordForm(cfg, url.Values{}); err != nil || len(cfg.WorkloadReplayTargets) != 1 || cfg.WorkloadReplayOps != "reads_only" {
		t.Fatalf("missing fields changed the config: %+v %v", cfg, err)
	}
	form := url.Values{
		"workload_replay_targets":      {`[{"label":"m8","uri":"mongodb://a:1"},{"label":"m9","uri":"mongodb://b:2"}]`},
		"workload_replay_targets_mode": {" concurrent "},
		"workload_replay_ops":          {"all"},
		"workload_data_fingerprint":    {"dbhash"},
	}
	if err := applyWorkloadRecordForm(cfg, form); err != nil {
		t.Fatal(err)
	}
	if len(cfg.WorkloadReplayTargets) != 2 || cfg.WorkloadReplayTargets[1] != (config.ReplayTarget{Label: "m9", URI: "mongodb://b:2"}) ||
		cfg.WorkloadReplayTargetsMode != "concurrent" || cfg.WorkloadReplayOps != "all" || cfg.WorkloadDataFingerprint != "dbhash" {
		t.Fatalf("multi-target fields = %+v", cfg)
	}
	if err := applyWorkloadRecordForm(cfg, url.Values{"workload_replay_targets": {""}}); err != nil || cfg.WorkloadReplayTargets != nil {
		t.Fatalf("an empty list must clear the targets: %+v %v", cfg.WorkloadReplayTargets, err)
	}
	if err := applyWorkloadRecordForm(cfg, url.Values{"workload_replay_targets": {"m8=mongodb://a"}}); err == nil || !strings.Contains(err.Error(), "invalid multi-target settings") {
		t.Fatalf("a malformed list was accepted: %v", err)
	}
}

func TestHandleStartRejectsInvalidMultiTargetSettings(t *testing.T) {
	for name, tc := range map[string]struct {
		fields map[string]string
		want   string
	}{
		"one target":     {map[string]string{"workload_replay_targets": `[{"label":"a","uri":"mongodb://a"}]`, "workload_deterministic": "true", "operations_per_worker": "5", "random_seed": "3", "drop_collections": "true"}, "needs 2 to 8 targets"},
		"no outcomes":    {map[string]string{"workload_replay_targets": `[{"label":"a","uri":"mongodb://a"},{"label":"b","uri":"mongodb://b"}]`, "workload_deterministic": "true", "operations_per_worker": "5", "random_seed": "3", "drop_collections": "true"}, "needs workload_record_outcomes on"},
		"reads_only":     {map[string]string{"workload_replay_ops": "reads_only"}, "needs workload_replay_enabled"},
		"bad mode":       {map[string]string{"workload_replay_targets_mode": "parallel"}, "workload_replay_targets_mode"},
		"bad dataprint":  {map[string]string{"workload_replay_enabled": "true", "workload_replay_path": "x.ndjson", "workload_data_fingerprint": "md5"}, "workload_data_fingerprint"},
		"malformed list": {map[string]string{"workload_replay_targets": "not json"}, "invalid multi-target settings"},
	} {
		t.Run(name, func(t *testing.T) {
			s := NewServer(&config.AppConfig{Duration: "1s"})
			fields := map[string]string{"default_workload": "on"}
			for k, v := range tc.fields {
				fields[k] = v
			}
			req, rec := newMultipartRequest(t, fields, nil)
			s.handleStart(rec, req)
			assertStartErrorResponse(t, rec, http.StatusBadRequest, tc.want)
			if isRunning(s) {
				t.Fatal("run still marked running after a rejected start")
			}
		})
	}
}

// TestHandleStartMultiTargetRunsEachTargetAndKeepsComparisons runs a
// concurrent deterministic plan over two fake targets: each target gets its
// own connection, collector, and outcome file; uri is never dialed; the
// comparison is kept in memory and linked to the targets' trend runs.
func TestHandleStartMultiTargetRunsEachTargetAndKeepsComparisons(t *testing.T) {
	var mu sync.Mutex
	var dialed []string
	collectors := map[*stats.Collector]string{}
	withWebUISeams(t, webuiSeams{
		loadCollections: func(path string, loadDefault bool) (*config.CollectionsFile, error) {
			return &config.CollectionsFile{Collections: []config.CollectionDefinition{{Name: "flights", DatabaseName: "benchdb"}}}, nil
		},
		loadQueries: func(path string, loadDefault bool) (*config.QueriesFile, error) {
			return &config.QueriesFile{Queries: []config.QueryDefinition{{Name: "f", Database: "benchdb", Collection: "flights", Operation: "find", Filter: map[string]interface{}{}}}}, nil
		},
		connect: func(ctx context.Context, cfg *config.AppConfig, dbName string) (*db.Connection, error) {
			mu.Lock()
			dialed = append(dialed, cfg.URI)
			mu.Unlock()
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
			c := uiCollector[0]
			mu.Lock()
			collectors[c] = cfg.URI
			mu.Unlock()
			w, err := workloadrec.NewOutcomeWriter(cfg.WorkloadDeterministicOutcomesPath, workloadrec.OutcomeHeader{Source: workloadrec.OutcomeSourceDeterministic, PLGMVersion: "test", Workers: 1, RandomSeed: cfg.RandomSeed, OperationsPerWorker: 2})
			if err != nil {
				return err
			}
			for ws := uint64(1); ws <= 2; ws++ {
				c.Track("find", time.Millisecond)
				w.Write(workloadrec.OutcomeLine{Worker: 1, WorkerSeq: ws, Iteration: 1, NS: "benchdb.flights", Op: "find", Shape: "find|f", Outcome: workloadrec.Outcome{LatencyUS: 1000, OK: true, Returned: workloadrec.Count(1)}})
			}
			fp := strings.Repeat("f", 64)
			w.Close(workloadrec.OutcomeTrailer{TrafficFingerprint: fp, TrafficStatus: "complete"}, workloadrec.StatusComplete, "")
			c.SetGeneratedTraffic(&stats.GeneratedTrafficInfo{Status: "complete", Fingerprint: fp, OpsPlanned: 2, OpsSent: 2})
			return nil
		},
	})
	s := NewServer(&config.AppConfig{Duration: "1s", Iterations: 1, CollectionsPath: "ignored", QueriesPath: "ignored", TrendBaselineStore: "off", URI: "mongodb://not-listed:27017"})
	waitForRunsOnCleanup(t, s)
	out := filepath.Join(t.TempDir(), "det.ndjson")
	req, rec := newMultipartRequest(t, map[string]string{"default_workload": "on", "documents_count": "0", "interval_delay": "0s", "uri": "mongodb://not-listed:27017",
		"workload_deterministic": "true", "operations_per_worker": "2", "random_seed": "3", "drop_collections": "true",
		"workload_record_outcomes": "true", "workload_deterministic_outcomes_path": out, "trend_run_label": "upgrade",
		"workload_replay_targets":      `[{"label":"m8","uri":"mongodb://user:secret@a:1"},{"label":"m9","uri":"mongodb://user:secret@b:2"}]`,
		"workload_replay_targets_mode": "concurrent"}, nil)
	s.handleStart(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("start status %d body=%s", rec.Code, rec.Body.String())
	}
	waitUntil(t, 5*time.Second, func() bool { return !isRunning(s) }, "run did not finish")
	s.runWG.Wait()

	if len(dialed) != 2 || len(collectors) != 2 {
		t.Fatalf("dialed %v with %d collectors", dialed, len(collectors))
	}
	for _, u := range dialed {
		if strings.Contains(u, "not-listed") {
			t.Fatal("a multi-target run dialed the uri setting, which is not a listed target")
		}
	}
	s.mu.Lock()
	plan, lastErr, phase, cfg := s.multiTarget, s.LastError, s.LifecyclePhase, s.AppConfig
	s.mu.Unlock()
	if plan == nil || plan.Result() == nil || lastErr != "" || phase != "completed" || len(cfg.WorkloadReplayTargets) != 2 {
		t.Fatalf("plan %v, last error %q, phase %q, config targets %d", plan, lastErr, phase, len(cfg.WorkloadReplayTargets))
	}
	res := plan.Result()
	if res.FingerprintsMatch == nil || !*res.FingerprintsMatch || len(res.Comparisons) != 1 {
		t.Fatalf("result = %+v", res)
	}
	c := res.Comparisons[0]
	kept := s.replayComparison(c.ReportID)
	if c.Error != "" || kept == nil || kept.A.Label != "m8" || kept.B.Label != "m9" || kept.Notes == nil {
		t.Fatalf("comparison = %+v kept %v", c, kept)
	}
	if a, b := plan.Targets[0].TrendRunID, plan.Targets[1].TrendRunID; a == "" || a == b || kept.A.PlanRunID != a || kept.B.PlanRunID != b {
		t.Fatalf("trend runs %q %q, linked %q %q", a, b, kept.A.PlanRunID, kept.B.PlanRunID)
	}
	if plan.Targets[1].Config.TrendRunLabel != "upgrade-m9" || !strings.HasSuffix(plan.Targets[1].OutcomesPath, "det.m9.ndjson") {
		t.Fatalf("target 2: label %q, outcomes %q", plan.Targets[1].Config.TrendRunLabel, plan.Targets[1].OutcomesPath)
	}

	statsRec := httptest.NewRecorder()
	s.handleStats(statsRec, httptest.NewRequest(http.MethodGet, "/api/stats", nil))
	if strings.Contains(statsRec.Body.String(), "secret") || strings.Contains(statsRec.Body.String(), "mongodb://") {
		t.Fatal("/api/stats must not expose target URIs")
	}
	var payload struct {
		MultiTarget struct {
			Mode        string `json:"mode"`
			Reference   string `json:"reference"`
			Targets     []map[string]interface{}
			Comparisons []map[string]interface{}
		} `json:"multi_target"`
	}
	if err := json.Unmarshal(statsRec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	mt := payload.MultiTarget
	if mt.Mode != "concurrent" || mt.Reference != "m8" || len(mt.Targets) != 2 || mt.Targets[1]["status"] != "complete" || len(mt.Comparisons) != 1 || mt.Comparisons[0]["report_id"] != c.ReportID {
		t.Fatalf("multi_target = %+v", mt)
	}
	b, err := report.RenderBytes(s.buildReportData())
	html := string(b)
	if err != nil || !strings.Contains(html, `id="multi-target"`) || !strings.Contains(html, "m8 vs m9") || !strings.Contains(html, "latencies may be affected") || strings.Contains(html, "secret") {
		t.Fatalf("run report multi-target section missing or leaks a URI: %v", err)
	}
}

func TestSingleTargetStatsHaveNoMultiTargetSection(t *testing.T) {
	s := NewServer(&config.AppConfig{Duration: "1s"})
	s.mu.Lock()
	s.CurrentStats = stats.NewCollector()
	s.mu.Unlock()
	rec := httptest.NewRecorder()
	s.handleStats(rec, httptest.NewRequest(http.MethodGet, "/api/stats", nil))
	if strings.Contains(rec.Body.String(), "multi_target") {
		t.Fatal("a single-target run must not add multi_target to /api/stats")
	}
	if items, notes := multiTargetReportItems(nil); items != nil || notes != nil {
		t.Fatal("no plan, no report section")
	}
}
