//go:build integration
// +build integration

package mongo

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
)

// TestWorkloadDeterministicIntegration runs the same deterministic workload
// twice, each time on a database freshly seeded from the same seed (two
// instances with the same starting data), and checks that both runs report
// the same complete traffic fingerprint and ran every shape equally often.
func TestWorkloadDeterministicIntegration(t *testing.T) {
	client := connectIntegrationMongoOrSkip(t)
	defer client.Disconnect(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	dbName := fmt.Sprintf("plgm_it_deterministic_%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = client.Database(dbName).Drop(context.Background()) })
	collections := []config.CollectionDefinition{
		{DatabaseName: dbName, Name: "orders", Fields: map[string]config.CollectionField{
			"order_id": {Type: "int"}, "status": {Type: "string", Enum: []interface{}{"new", "paid", "sent"}}, "total": {Type: "double"},
		}},
		{DatabaseName: dbName, Name: "users", Fields: map[string]config.CollectionField{
			"age": {Type: "int", Min: intPtr(18), Max: intPtr(90)}, "name": {Type: "string", Provider: "first_name"}, "joined": {Type: "date"},
		}},
	}
	queries := []config.QueryDefinition{
		{Name: "users_by_age", Database: dbName, Collection: "users", Operation: "find", Filter: map[string]interface{}{"age": map[string]interface{}{"$gte": "<int>"}}},
		{Name: "touch_user", Database: dbName, Collection: "users", Operation: "updateOne", Filter: map[string]interface{}{"age": "<int>"}, Update: map[string]interface{}{"$inc": map[string]interface{}{"age": 1}}},
		{Name: "drop_user", Database: dbName, Collection: "users", Operation: "deleteOne", Filter: map[string]interface{}{"age": "<int>"}},
	}
	cfg := config.AppConfig{
		Duration: "60s", StatusRefreshRateSec: 1, Concurrency: 3, RandomSeed: 2026, DropCollections: true,
		WorkloadDeterministic: true, OperationsPerWorker: 150,
		DocumentsCount: 300, SeedBatchSize: 50, SeedConcurrency: 1,
		FindPercent: 50, UpdatePercent: 20, DeletePercent: 5, InsertPercent: 15, BulkInsertPercent: 10,
		FindBatchSize: 5, FindLimit: 5, InsertBatchSize: 5, InsertCacheSize: 50, RecordPoolBootstrapSample: 50, RecordPoolMaxSize: 1000,
		ExistingRecordHitRate: 50, InsightsEnabled: true, InsightsSamplingRate: 1, InsightsMaxEvents: 5000, InsightsMaxGroups: 500,
	}
	cfg.WebUI.Enabled = true
	if err := config.ValidateWorkloadDeterministic(&cfg); err != nil {
		t.Fatal(err)
	}

	run := func() (*stats.GeneratedTrafficInfo, map[string]int) {
		runCfg := cfg
		db := seedRecordReplayDB(t, ctx, client, collections, &runCfg)
		c := stats.NewCollector()
		if err := RunWorkload(ctx, db, collections, queries, &runCfg, c); err != nil {
			t.Fatalf("deterministic run: %v", err)
		}
		rep := c.GetFinalInsights()
		g := rep.Metadata.GeneratedTraffic
		if g == nil || g.Status != "complete" || g.OpsSent != uint64(cfg.Concurrency*cfg.OperationsPerWorker) {
			t.Fatalf("generated traffic = %+v", g)
		}
		// Impact groups count every operation per shape; QueryShapes only
		// holds sampled slow operations.
		counts := map[string]int{}
		for _, grp := range rep.Impact.Groups {
			counts[grp.Operation+" "+grp.Namespace+" "+grp.ShapeKey] += int(grp.OperationCount)
		}
		return g, counts
	}
	a, countsA := run()
	b, countsB := run()
	if a.Fingerprint != b.Fingerprint || !reflect.DeepEqual(a.WorkerHashes, b.WorkerHashes) {
		t.Fatalf("fingerprints differ:\n %s %v\n %s %v", a.Fingerprint, a.WorkerHashes, b.Fingerprint, b.WorkerHashes)
	}
	if len(countsA) == 0 || !reflect.DeepEqual(countsA, countsB) {
		t.Fatalf("per-shape op counts differ:\n run 1 %v\n run 2 %v", countsA, countsB)
	}
	t.Logf("fingerprint %s, %d ops, shapes %v", a.Fingerprint, a.OpsSent, countsA)
}
