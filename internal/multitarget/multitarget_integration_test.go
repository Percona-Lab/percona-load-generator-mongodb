//go:build integration
// +build integration

package multitarget

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/datagen"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/db"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/mongo"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/workloadrec"
)

// integrationTargets returns the instances A (recording), B, and C. B and C
// must be separate mongod instances: every target uses the same database.
func integrationTargets(t *testing.T) (string, string, string) {
	t.Helper()
	a := os.Getenv("PLGM_IT_MONGO_URI")
	if a == "" {
		a = "mongodb://127.0.0.1:30777"
	}
	b, c := os.Getenv("PLGM_IT_MONGO_URI_B"), os.Getenv("PLGM_IT_MONGO_URI_C")
	if b == "" || c == "" {
		t.Skip("skipping multi-target integration test: set PLGM_IT_MONGO_URI_B and PLGM_IT_MONGO_URI_C to two more MongoDB instances")
	}
	for _, u := range []string{a, b, c} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		conn, err := db.Connect(ctx, &config.AppConfig{URI: u}, "admin")
		if err == nil {
			err = conn.Client.Ping(ctx, nil)
			conn.Disconnect(context.Background())
		}
		cancel()
		if err != nil {
			t.Skipf("skipping multi-target integration test: %s unreachable: %v", u, err)
		}
	}
	return a, b, c
}

func intPtr(v int) *int { return &v }

// TestMultiTargetReplayIntegration records a workload on instance A, then
// replays it on B and C, sequentially and concurrently, from the same seeded
// starting data, and runs a deterministic workload on B and C. Every
// comparison of C with B must show identical results.
func TestMultiTargetReplayIntegration(t *testing.T) {
	uriA, uriB, uriC := integrationTargets(t)
	t.Cleanup(func() { datagen.ConfigureSeed(0) })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	dbName := fmt.Sprintf("plgm_it_multi_target_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		for _, u := range []string{uriA, uriB, uriC} {
			if conn, err := db.Connect(context.Background(), &config.AppConfig{URI: u}, dbName); err == nil {
				_ = conn.Database.Drop(context.Background())
				conn.Disconnect(context.Background())
			}
		}
	})
	collections := []config.CollectionDefinition{
		{DatabaseName: dbName, Name: "users", Fields: map[string]config.CollectionField{
			"age": {Type: "int", Min: intPtr(18), Max: intPtr(90)}, "name": {Type: "string", Provider: "first_name"},
		}, Indexes: []config.IndexDefinition{{Keys: config.IndexKeys{{Field: "age", Value: int32(1)}}}}},
	}
	queries := []config.QueryDefinition{
		{Name: "users_by_age", Database: dbName, Collection: "users", Operation: "find", Filter: map[string]interface{}{"age": map[string]interface{}{"$gte": "<int>"}}},
		{Name: "touch_user", Database: dbName, Collection: "users", Operation: "updateOne", Filter: map[string]interface{}{"age": "<int>"}, Update: map[string]interface{}{"$inc": map[string]interface{}{"age": 1}}},
		{Name: "drop_user", Database: dbName, Collection: "users", Operation: "deleteOne", Filter: map[string]interface{}{"age": "<int>"}},
	}
	dir := t.TempDir()
	// One worker keeps the replayed writes in one order, so B and C must
	// return identical results.
	base := config.AppConfig{
		Duration: "1s", StatusRefreshRateSec: 1, Concurrency: 1, RandomSeed: 2026, DropCollections: true,
		DocumentsCount: 200, SeedBatchSize: 50, SeedConcurrency: 1,
		FindPercent: 60, UpdatePercent: 20, DeletePercent: 5, InsertPercent: 15,
		FindBatchSize: 5, FindLimit: 5, InsertBatchSize: 5, InsertCacheSize: 50, RecordPoolBootstrapSample: 50, RecordPoolMaxSize: 1000,
		ExistingRecordHitRate: 50, InsightsEnabled: true, InsightsSamplingRate: 1, InsightsMaxEvents: 1000, InsightsMaxGroups: 500,
		WorkloadRecordOutcomes: true, WorkloadRecordResultChecksums: true,
	}
	base.WebUI.Enabled = true

	recPath := filepath.Join(dir, "rec.ndjson")
	recCfg := base
	recCfg.URI, recCfg.WorkloadRecordEnabled, recCfg.WorkloadRecordPath, recCfg.WorkloadDataFingerprint = uriA, true, recPath, config.DataFingerprintCounts
	config.NormalizeWorkloadRecordReplay(&recCfg)
	connA, err := db.Connect(ctx, &recCfg, dbName)
	if err != nil {
		t.Fatal(err)
	}
	defer connA.Disconnect(context.Background())
	if err := DefaultPrepare(ctx, &Target{Config: &recCfg, Collections: collections}, connA.Database); err != nil {
		t.Fatalf("prepare A: %v", err)
	}
	recCtx, finish := mongo.WithWorkloadRecordingSession(ctx)
	recorded := stats.NewCollector()
	err = mongo.RunWorkload(recCtx, connA.Database, collections, config.CloneQueries(queries), &recCfg, recorded)
	finish()
	if err != nil {
		t.Fatalf("record on A: %v", err)
	}
	info, err := workloadrec.Scan(recPath)
	if err != nil || info.Ops == 0 || info.Header.DataFingerprint == nil {
		t.Fatalf("recording: %+v %v", info, err)
	}

	for _, mode := range []string{config.WorkloadTargetsSequential, config.WorkloadTargetsConcurrent} {
		t.Run("replay "+mode, func(t *testing.T) {
			cfg := base
			cfg.WorkloadReplayEnabled, cfg.WorkloadReplayPath = true, recPath
			cfg.WorkloadReplayOutcomesPath = filepath.Join(t.TempDir(), "out.ndjson")
			cfg.WorkloadReplayTargets = []config.ReplayTarget{{Label: "b", URI: uriB}, {Label: "c", URI: uriC}}
			cfg.WorkloadReplayTargetsMode = mode
			config.NormalizeWorkloadRecordReplay(&cfg)
			p, err := NewPlan(&cfg, dbName, collections, queries)
			if err != nil {
				t.Fatal(err)
			}
			p.WriteReports = true
			res := p.Run(ctx)
			t.Log(res.SummaryText())
			for _, s := range res.Targets {
				if s.Status != StatusComplete || s.OpsDone != info.Ops || s.DataFingerprintMatch == nil || !*s.DataFingerprintMatch {
					t.Fatalf("target %s = %+v", s.Label, s)
				}
			}
			if mode == config.WorkloadTargetsConcurrent && res.Targets[1].LagMaxMs == nil {
				t.Fatal("a concurrent replay must report client-side lag")
			}
			c := res.Comparisons[0]
			if c.Error != "" || c.Report == nil || !c.Report.IdentityMatch || c.DifferingOps != 0 || c.Report.Overall.Matched != info.Ops || c.Report.Overall.ChecksumsCompared == 0 {
				t.Fatalf("b vs c = %+v overall %+v", c, c.Report.Overall)
			}
			for _, path := range []string{c.JSONPath, c.HTMLPath, p.Targets[0].OutcomesPath, p.Targets[1].OutcomesPath} {
				if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
					t.Fatalf("%s: %v %v", path, st, err)
				}
			}
		})
	}

	t.Run("deterministic concurrent", func(t *testing.T) {
		cfg := base
		cfg.Duration = "60s"
		cfg.WorkloadDeterministic, cfg.OperationsPerWorker = true, 200
		cfg.WorkloadDeterministicOutcomesPath = filepath.Join(t.TempDir(), "det.ndjson")
		cfg.WorkloadReplayTargets = []config.ReplayTarget{{Label: "b", URI: uriB}, {Label: "c", URI: uriC}}
		cfg.WorkloadReplayTargetsMode = config.WorkloadTargetsConcurrent
		config.NormalizeWorkloadRecordReplay(&cfg)
		p, err := NewPlan(&cfg, dbName, collections, queries)
		if err != nil {
			t.Fatal(err)
		}
		res := p.Run(ctx)
		t.Log(res.SummaryText())
		if res.FingerprintsMatch == nil || !*res.FingerprintsMatch || res.Targets[0].TrafficFingerprint == "" {
			t.Fatalf("fingerprints: %+v", res.Targets)
		}
		if c := res.Comparisons[0]; c.Error != "" || c.DifferingOps != 0 || c.Report.Overall.Matched != 200 {
			t.Fatalf("b vs c = %+v", c)
		}
	})
}
