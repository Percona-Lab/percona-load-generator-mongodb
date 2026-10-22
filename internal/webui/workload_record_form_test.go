package webui

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/db"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	driverMongo "go.mongodb.org/mongo-driver/v2/mongo"
)

func TestApplyWorkloadRecordFormKeepsConfigWhenFieldsAreMissing(t *testing.T) {
	cfg := &config.AppConfig{RandomSeed: 5, WorkloadRecordEnabled: true, WorkloadRecordPath: "a.ndjson", WorkloadRecordMaxBytes: 10}
	before := *cfg
	if err := applyWorkloadRecordForm(cfg, url.Values{}); err != nil {
		t.Fatal(err)
	}
	if cfg.RandomSeed != before.RandomSeed || cfg.WorkloadRecordEnabled != before.WorkloadRecordEnabled || cfg.WorkloadRecordPath != before.WorkloadRecordPath || cfg.WorkloadRecordMaxBytes != before.WorkloadRecordMaxBytes {
		t.Fatalf("an empty form changed the config: %+v", cfg)
	}
}

func TestApplyWorkloadRecordFormAppliesFields(t *testing.T) {
	cfg := &config.AppConfig{RandomSeed: 5, WorkloadRecordEnabled: true}
	form := url.Values{
		"random_seed":                    {""},
		"workload_record_enabled":        {"false"},
		"workload_record_path":           {" r.ndjson "},
		"workload_record_max_bytes":      {"2048"},
		"workload_replay_enabled":        {"true"},
		"workload_replay_path":           {" in.ndjson "},
		"workload_replay_mode":           {"timed"},
		"workload_replay_allow_mismatch": {"on"},
	}
	if err := applyWorkloadRecordForm(cfg, form); err != nil {
		t.Fatal(err)
	}
	if cfg.RandomSeed != 0 || cfg.WorkloadRecordEnabled || cfg.WorkloadRecordPath != "r.ndjson" || cfg.WorkloadRecordMaxBytes != 2048 {
		t.Fatalf("record fields = %+v", cfg)
	}
	if !cfg.WorkloadReplayEnabled || cfg.WorkloadReplayPath != "in.ndjson" || cfg.WorkloadReplayMode != "timed" || !cfg.WorkloadReplayAllowMismatch {
		t.Fatalf("replay fields = %+v", cfg)
	}
	if err := applyWorkloadRecordForm(cfg, url.Values{"random_seed": {"-12"}}); err != nil || cfg.RandomSeed != -12 {
		t.Fatalf("seed = %d err = %v", cfg.RandomSeed, err)
	}
	for field, bad := range map[string]string{"random_seed": "1.5", "workload_record_max_bytes": "-1"} {
		if err := applyWorkloadRecordForm(cfg, url.Values{field: {bad}}); err == nil {
			t.Fatalf("%s=%q accepted", field, bad)
		}
	}
	scale := url.Values{"workload_record_max_bytes": {"0"}, "workload_record_backpressure": {" block "}, "workload_record_compression": {"zstd"}}
	if err := applyWorkloadRecordForm(cfg, scale); err != nil {
		t.Fatal(err)
	}
	if cfg.WorkloadRecordMaxBytes != 0 || cfg.WorkloadRecordBackpressure != "block" || cfg.WorkloadRecordCompression != "zstd" {
		t.Fatalf("scale fields = %d %q %q", cfg.WorkloadRecordMaxBytes, cfg.WorkloadRecordBackpressure, cfg.WorkloadRecordCompression)
	}
}

// TestHandleStartIterationsWithRecordAndReplay checks that a recording runs
// every configured iteration, and a replay runs once whatever iterations
// says (it replays the recorded iterations itself).
func TestHandleStartIterationsWithRecordAndReplay(t *testing.T) {
	for name, tc := range map[string]struct {
		fields    map[string]string
		wantCalls int
	}{
		"record": {fields: map[string]string{"workload_record_enabled": "true", "workload_record_backpressure": "block", "workload_record_compression": "gzip"}, wantCalls: 3},
		"replay": {fields: map[string]string{"workload_replay_enabled": "true", "workload_replay_path": "in.ndjson"}, wantCalls: 1},
	} {
		t.Run(name, func(t *testing.T) {
			calls := make(chan config.AppConfig, 8)
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
					calls <- *cfg
					return nil
				},
			})
			s := NewServer(&config.AppConfig{Duration: "1s", Iterations: 1, CollectionsPath: "ignored", QueriesPath: "ignored"})
			waitForRunsOnCleanup(t, s)
			fields := map[string]string{"default_workload": "on", "documents_count": "0", "iterations": "3", "interval_delay": "0s", "workload_record_path": t.TempDir() + "/rec.ndjson"}
			for k, v := range tc.fields {
				fields[k] = v
			}
			req, rec := newMultipartRequest(t, fields, nil)
			s.handleStart(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("start status %d body=%s", rec.Code, rec.Body.String())
			}
			waitUntil(t, 5*time.Second, func() bool { return !isRunning(s) }, "run did not finish")
			s.runWG.Wait()
			if got := len(calls); got != tc.wantCalls {
				t.Fatalf("runWorkload called %d times, want %d", got, tc.wantCalls)
			}
			s.mu.Lock()
			total := s.TotalIterations
			s.mu.Unlock()
			if total != tc.wantCalls {
				t.Fatalf("TotalIterations = %d, want %d", total, tc.wantCalls)
			}
			if name == "record" {
				cfg := <-calls
				if cfg.WorkloadRecordBackpressure != "block" || cfg.WorkloadRecordCompression != "gzip" || cfg.Iterations != 3 {
					t.Fatalf("run config = %q %q %d", cfg.WorkloadRecordBackpressure, cfg.WorkloadRecordCompression, cfg.Iterations)
				}
			}
		})
	}
}

func TestApplyWorkloadRecordFormOperationsPerWorker(t *testing.T) {
	cfg := &config.AppConfig{OperationsPerWorker: 9}
	if err := applyWorkloadRecordForm(cfg, url.Values{}); err != nil || cfg.OperationsPerWorker != 9 {
		t.Fatalf("missing field changed the budget: %d %v", cfg.OperationsPerWorker, err)
	}
	if err := applyWorkloadRecordForm(cfg, url.Values{"operations_per_worker": {" 40 "}}); err != nil || cfg.OperationsPerWorker != 40 {
		t.Fatalf("budget = %d err = %v", cfg.OperationsPerWorker, err)
	}
	if err := applyWorkloadRecordForm(cfg, url.Values{"operations_per_worker": {""}}); err != nil || cfg.OperationsPerWorker != 0 {
		t.Fatalf("empty budget = %d err = %v", cfg.OperationsPerWorker, err)
	}
	for _, bad := range []string{"-1", "2.5", "x"} {
		if err := applyWorkloadRecordForm(cfg, url.Values{"operations_per_worker": {bad}}); err == nil {
			t.Fatalf("operations_per_worker=%q accepted", bad)
		}
	}
}

func TestApplyWorkloadRecordFormOutcomeFields(t *testing.T) {
	cfg := &config.AppConfig{WorkloadRecordOutcomes: true, WorkloadReplayOutcomesPath: "keep.ndjson"}
	if err := applyWorkloadRecordForm(cfg, url.Values{}); err != nil || !cfg.WorkloadRecordOutcomes || cfg.WorkloadReplayOutcomesPath != "keep.ndjson" {
		t.Fatalf("missing fields changed the config: %+v %v", cfg, err)
	}
	form := url.Values{
		"workload_record_outcomes":             {"false"},
		"workload_record_result_checksums":     {"on"},
		"workload_replay_outcomes_path":        {" r_out.ndjson "},
		"workload_deterministic_outcomes_path": {" d_out.ndjson "},
	}
	if err := applyWorkloadRecordForm(cfg, form); err != nil {
		t.Fatal(err)
	}
	if cfg.WorkloadRecordOutcomes || !cfg.WorkloadRecordResultChecksums || cfg.WorkloadReplayOutcomesPath != "r_out.ndjson" || cfg.WorkloadDeterministicOutcomesPath != "d_out.ndjson" {
		t.Fatalf("outcome fields = %+v", cfg)
	}
}

func TestHandleStartWiresAndValidatesOutcomeCapture(t *testing.T) {
	calls := make(chan config.AppConfig, 2)
	deterministicStartSeams(t, calls)
	s := NewServer(&config.AppConfig{Duration: "1s", Iterations: 1, CollectionsPath: "ignored", QueriesPath: "ignored"})
	waitForRunsOnCleanup(t, s)
	out := t.TempDir() + "/det_out.ndjson"
	req, rec := newMultipartRequest(t, map[string]string{"default_workload": "on", "documents_count": "0", "interval_delay": "0s",
		"workload_deterministic": "true", "operations_per_worker": "5", "random_seed": "3", "drop_collections": "true",
		"workload_record_outcomes": "true", "workload_record_result_checksums": "true", "workload_deterministic_outcomes_path": out}, nil)
	s.handleStart(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("start status %d body=%s", rec.Code, rec.Body.String())
	}
	var cfg config.AppConfig
	select {
	case cfg = <-calls:
	case <-time.After(5 * time.Second):
		t.Fatal("workload did not start")
	}
	if !cfg.WorkloadRecordOutcomes || !cfg.WorkloadRecordResultChecksums || cfg.WorkloadDeterministicOutcomesPath != out {
		t.Fatalf("run config outcomes=%v checksums=%v path=%q", cfg.WorkloadRecordOutcomes, cfg.WorkloadRecordResultChecksums, cfg.WorkloadDeterministicOutcomesPath)
	}
	waitUntil(t, 5*time.Second, func() bool { return !isRunning(s) }, "run did not finish")
	s.runWG.Wait()

	for name, tc := range map[string]struct {
		fields map[string]string
		want   string
	}{
		"checksums without outcomes":  {map[string]string{"workload_record_result_checksums": "true"}, "needs workload_record_outcomes on"},
		"outcomes with nowhere to go": {map[string]string{"workload_record_outcomes": "true"}, "needs workload_record_enabled"},
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

func deterministicStartSeams(t *testing.T, calls chan config.AppConfig) {
	t.Helper()
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
			calls <- *cfg
			return nil
		},
	})
}

func TestHandleStartWiresDeterministicTraffic(t *testing.T) {
	for name, tc := range map[string]struct {
		fields   map[string]string
		wantDet  bool
		wantOps  int
		wantSeed int64
	}{
		"default off": {fields: map[string]string{}, wantDet: false, wantOps: 0},
		"on": {fields: map[string]string{"workload_deterministic": "true", "operations_per_worker": "25", "random_seed": "11", "drop_collections": "true"},
			wantDet: true, wantOps: 25, wantSeed: 11},
	} {
		t.Run(name, func(t *testing.T) {
			calls := make(chan config.AppConfig, 2)
			deterministicStartSeams(t, calls)
			s := NewServer(&config.AppConfig{Duration: "1s", Iterations: 1, CollectionsPath: "ignored", QueriesPath: "ignored"})
			waitForRunsOnCleanup(t, s)
			fields := map[string]string{"default_workload": "on", "documents_count": "0", "interval_delay": "0s"}
			for k, v := range tc.fields {
				fields[k] = v
			}
			req, rec := newMultipartRequest(t, fields, nil)
			s.handleStart(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("start status %d body=%s", rec.Code, rec.Body.String())
			}
			var cfg config.AppConfig
			select {
			case cfg = <-calls:
			case <-time.After(5 * time.Second):
				t.Fatal("workload did not start")
			}
			if cfg.WorkloadDeterministic != tc.wantDet || cfg.OperationsPerWorker != tc.wantOps || (tc.wantDet && cfg.RandomSeed != tc.wantSeed) {
				t.Fatalf("run config deterministic=%v ops=%d seed=%d", cfg.WorkloadDeterministic, cfg.OperationsPerWorker, cfg.RandomSeed)
			}
			waitUntil(t, 5*time.Second, func() bool { return !isRunning(s) }, "run did not finish")
			s.runWG.Wait()
		})
	}
}

func TestHandleStartRejectsInvalidDeterministicTraffic(t *testing.T) {
	for name, tc := range map[string]struct {
		fields map[string]string
		want   string
	}{
		"no seed":   {map[string]string{"operations_per_worker": "5", "drop_collections": "true"}, "non-zero random_seed"},
		"no budget": {map[string]string{"random_seed": "3", "drop_collections": "true"}, "operations_per_worker"},
		"no drop":   {map[string]string{"random_seed": "3", "operations_per_worker": "5"}, "drop_collections"},
	} {
		t.Run(name, func(t *testing.T) {
			s := NewServer(&config.AppConfig{Duration: "1s"})
			fields := map[string]string{"default_workload": "on", "workload_deterministic": "on"}
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

func TestGeneratedTrafficReportSection(t *testing.T) {
	if generatedTrafficItems(nil) != nil {
		t.Fatal("runs without deterministic traffic have no fingerprint section")
	}
	g := &stats.GeneratedTrafficInfo{Fingerprint: strings.Repeat("ab", 32), Status: "incomplete", Reason: "the safety timeout was reached", RandomSeed: 7, Workers: 2, OperationsPerWorker: 10, Iterations: 1,
		OpsPlanned: 20, OpsGenerated: 15, OpsSent: 14, WorkerHashes: map[string]string{"10": "h10", "2": "h2"}, SafetyTimeout: "5s"}
	items := generatedTrafficItems(g)
	joined := ""
	for _, kv := range items {
		joined += kv.Key + "=" + kv.Value + "\n"
	}
	for _, want := range []string{"Fingerprint=" + g.Fingerprint, "Status=incomplete (the safety timeout was reached)", "20 / 15 / 14", "Duration=5s"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in\n%s", want, joined)
		}
	}
	if strings.Index(joined, "Worker 2 Hash") > strings.Index(joined, "Worker 10 Hash") {
		t.Fatalf("worker hashes must be in numeric order:\n%s", joined)
	}
}

func TestHandleStartRejectsRecordAndReplayTogether(t *testing.T) {
	s := NewServer(&config.AppConfig{})
	req, rec := newMultipartRequest(t, map[string]string{
		"default_workload":        "on",
		"workload_record_enabled": "true",
		"workload_replay_enabled": "true",
		"workload_replay_path":    "x.ndjson",
	}, nil)
	s.handleStart(rec, req)
	assertStartErrorResponse(t, rec, http.StatusBadRequest, "cannot both be on")
	if isRunning(s) {
		t.Fatal("run still marked running after a rejected start")
	}
}

func TestHandleStartUploadsReplayFileForTheRunAndRemovesIt(t *testing.T) {
	type seen struct {
		cfg      config.AppConfig
		contents string
	}
	got := make(chan seen, 1)
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
			b, _ := os.ReadFile(cfg.WorkloadReplayPath)
			got <- seen{cfg: *cfg, contents: string(b)}
			return nil
		},
	})
	s := NewServer(&config.AppConfig{Duration: "1s", Iterations: 1, CollectionsPath: "ignored", QueriesPath: "ignored"})
	waitForRunsOnCleanup(t, s)
	req, rec := newMultipartRequest(t, map[string]string{
		"default_workload":        "on",
		"documents_count":         "0",
		"random_seed":             "77",
		"workload_replay_enabled": "true",
		"workload_replay_mode":    "timed",
	}, map[string]string{"workload_replay_file": `{"type":"header"}`})
	s.handleStart(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("start status %d body=%s", rec.Code, rec.Body.String())
	}
	var run seen
	select {
	case run = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("workload did not start")
	}
	if !run.cfg.WorkloadReplayEnabled || run.cfg.WorkloadReplayMode != "timed" || run.cfg.RandomSeed != 77 {
		t.Fatalf("run config = %+v", run.cfg)
	}
	if !strings.Contains(run.cfg.WorkloadReplayPath, "plgm-workload-replay-") || run.contents != `{"type":"header"}` {
		t.Fatalf("uploaded replay file = %q (%q)", run.cfg.WorkloadReplayPath, run.contents)
	}
	waitUntil(t, 5*time.Second, func() bool { return !isRunning(s) }, "run did not finish")
	s.runWG.Wait()
	if _, err := os.Stat(run.cfg.WorkloadReplayPath); !os.IsNotExist(err) {
		t.Fatalf("uploaded replay file kept after the run: %v", err)
	}
	s.mu.Lock()
	shown := s.AppConfig.WorkloadReplayPath
	s.mu.Unlock()
	if shown != "" {
		t.Fatalf("/api/config still shows the deleted upload as workload_replay_path: %q", shown)
	}
}
