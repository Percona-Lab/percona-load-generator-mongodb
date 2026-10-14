//go:build integration
// +build integration

package webui

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/db"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	"go.mongodb.org/mongo-driver/v2/bson"
	driverMongo "go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const explainITDocs = 2000

func explainITDatabase(t *testing.T) (*driverMongo.Client, *driverMongo.Database) {
	t.Helper()
	uri := os.Getenv("PLGM_IT_MONGO_URI")
	if uri == "" {
		uri = "mongodb://127.0.0.1:30777"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := driverMongo.Connect(options.Client().ApplyURI(uri).SetServerSelectionTimeout(2 * time.Second))
	if err != nil {
		skipIntegration(t, "skipping integration test: unable to create mongo client: %v", err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		_ = client.Disconnect(context.Background())
		skipIntegration(t, "skipping integration test: mongo is not reachable at %s: %v", uri, err)
	}
	database := client.Database(fmt.Sprintf("plgm_it_explain_%d", time.Now().UnixNano()))
	t.Cleanup(func() {
		_ = database.Drop(context.Background())
		_ = client.Disconnect(context.Background())
	})

	docs := make([]interface{}, 0, explainITDocs)
	for i := 0; i < explainITDocs; i++ {
		status := "closed"
		if i%4 == 0 {
			status = "open"
		}
		docs = append(docs, bson.D{{Key: "_id", Value: i}, {Key: "status", Value: status}, {Key: "sku", Value: fmt.Sprintf("sku-%d", i)}, {Key: "qty", Value: i}})
	}
	coll := database.Collection("orders")
	if _, err := coll.InsertMany(ctx, docs); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := coll.Indexes().CreateOne(ctx, driverMongo.IndexModel{Keys: bson.D{{Key: "status", Value: 1}}}); err != nil {
		t.Fatalf("index: %v", err)
	}
	return client, database
}

func collectionSnapshot(t *testing.T, database *driverMongo.Database) []bson.Raw {
	t.Helper()
	ctx := context.Background()
	var out []bson.Raw
	cur, err := database.Collection("orders").Find(ctx, bson.D{}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if err := cur.All(ctx, &out); err != nil {
		t.Fatalf("snapshot decode: %v", err)
	}
	return out
}

func explainITEvent(database *driverMongo.Database, op string, q stats.ExecutableQuery) stats.RepresentativeSample {
	q.Database = database.Name()
	return stats.RepresentativeSample{ShapeID: stats.StableShapeID(op, "orders", op), Operation: op, Database: database.Name(), Collection: "orders", ShapeKey: op, DurationMs: 50, Query: &q}
}

func TestExplainIntegration_FindPreservesFilterProjectionAndLimit(t *testing.T) {
	_, database := explainITDatabase(t)

	unindexed := runExplainForEvent(context.Background(), database, explainITEvent(database, "find", stats.ExecutableQuery{
		Filter: map[string]interface{}{"sku": "sku-7"},
	}), 5000, "executionStats")
	if unindexed.status != "explained" || unindexed.diag == nil {
		t.Fatalf("expected explained find, got %+v", unindexed)
	}
	d := unindexed.diag
	if !d.CollectionScanDetected || !d.ExecutionStatsAvailable || d.DocsExamined != explainITDocs || d.NReturned != 1 {
		t.Fatalf("expected COLLSCAN over %d docs returning 1, got %+v", explainITDocs, d)
	}
	if d.Diagnosis != stats.DiagCollscanSelective {
		t.Fatalf("expected selective collscan diagnosis, got %q", d.Diagnosis)
	}
	if d.ServerVersion == "" {
		t.Fatalf("expected serverInfo.version from a real explain, got none")
	}

	limited := runExplainForEvent(context.Background(), database, explainITEvent(database, "find", stats.ExecutableQuery{
		Filter:     map[string]interface{}{"status": "open"},
		Projection: map[string]interface{}{"_id": 0, "status": 1},
		Limit:      5,
	}), 5000, "executionStats")
	if limited.status != "explained" {
		t.Fatalf("expected explained find, got %+v", limited)
	}
	ld := limited.diag
	if !ld.IndexScanDetected || ld.CollectionScanDetected || ld.NReturned != 5 || !ld.LimitDetected {
		t.Fatalf("expected index-backed limited plan returning 5, got %+v", ld)
	}
	if len(ld.IndexesUsed) == 0 || ld.IndexesUsed[0] != "status_1" {
		t.Fatalf("expected status_1 index, got %v", ld.IndexesUsed)
	}
}

func TestExplainIntegration_WriteExplainsDoNotMutateData(t *testing.T) {
	_, database := explainITDatabase(t)
	before := collectionSnapshot(t, database)

	update := map[string]interface{}{"$set": map[string]interface{}{"qty": -1, "touched": true}}
	cases := []struct {
		op          string
		q           stats.ExecutableQuery
		wantMatched int64
	}{
		{op: "updateOne", q: stats.ExecutableQuery{Filter: map[string]interface{}{"status": "open"}, Update: update}, wantMatched: 1},
		{op: "updateMany", q: stats.ExecutableQuery{Filter: map[string]interface{}{"status": "open"}, Update: update}, wantMatched: explainITDocs / 4},
		{op: "updateOne", q: stats.ExecutableQuery{Filter: map[string]interface{}{"sku": "does-not-exist"}, Update: update, Upsert: true}, wantMatched: 0},
		{op: "deleteOne", q: stats.ExecutableQuery{Filter: map[string]interface{}{"status": "closed"}}, wantMatched: 1},
		{op: "deleteMany", q: stats.ExecutableQuery{Filter: map[string]interface{}{"status": "closed"}}, wantMatched: explainITDocs * 3 / 4},
	}
	for _, tc := range cases {
		res := runExplainForEvent(context.Background(), database, explainITEvent(database, tc.op, tc.q), 5000, "executionStats")
		if res.status != "explained" || res.diag == nil {
			t.Fatalf("%s: expected explained, got %+v", tc.op, res)
		}
		if !res.diag.ExecutionStatsAvailable || res.diag.NMatched != tc.wantMatched {
			t.Fatalf("%s: expected nMatched=%d with execution stats, got %+v", tc.op, tc.wantMatched, res.diag)
		}
		if !res.diag.IndexScanDetected && tc.q.Filter["status"] != nil {
			t.Fatalf("%s: expected status_1 index for status filter, got %+v", tc.op, res.diag)
		}
	}

	after := collectionSnapshot(t, database)
	if len(after) != len(before) {
		t.Fatalf("explain changed document count: before=%d after=%d", len(before), len(after))
	}
	for i := range before {
		if !bytes.Equal(before[i], after[i]) {
			t.Fatalf("explain changed document %d: before=%v after=%v", i, before[i], after[i])
		}
	}
}

func TestExplainIntegration_AggregateWithOutUsesQueryPlannerAndWritesNothing(t *testing.T) {
	_, database := explainITDatabase(t)
	pipeline := []interface{}{
		map[string]interface{}{"$match": map[string]interface{}{"status": "open"}},
		map[string]interface{}{"$group": map[string]interface{}{"_id": "$status", "n": map[string]interface{}{"$sum": 1}}},
		map[string]interface{}{"$out": "orders_report"},
	}
	res := runExplainForEvent(context.Background(), database, explainITEvent(database, "aggregate", stats.ExecutableQuery{Pipeline: pipeline}), 5000, "executionStats")
	if res.status != "explained" || res.diag.Verbosity != "queryPlanner" || res.diag.ExecutionStatsAvailable {
		t.Fatalf("expected queryPlanner-only explain for $out, got %+v", res)
	}
	names, err := database.ListCollectionNames(context.Background(), bson.D{{Key: "name", Value: "orders_report"}})
	if err != nil {
		t.Fatalf("list collections: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("explain of $out pipeline must not create the output collection")
	}

	plain := runExplainForEvent(context.Background(), database, explainITEvent(database, "aggregate", stats.ExecutableQuery{Pipeline: pipeline[:2]}), 5000, "executionStats")
	if plain.status != "explained" || !plain.diag.ExecutionStatsAvailable || !plain.diag.IndexScanDetected {
		t.Fatalf("expected executionStats index-backed aggregate explain, got %+v", plain.diag)
	}
	// $group reduces docs/4 matched inputs to one output: the access-stage count
	// is the denominator, not the final output.
	m := plain.diag.Metrics
	if m == nil || m.AccessReturned == nil || m.AccessReturned.Value != explainITDocs/4 || m.Returned == nil || m.Returned.Value != 1 {
		t.Fatalf("expected access count %d and final output 1, got %+v", explainITDocs/4, m)
	}
	q := rateExplains("aggregate", plain.diag)
	if q.Status != stats.EfficiencyRated || q.Score == nil || *q.Score < 75 {
		t.Fatalf("index-backed reducing aggregate must not be penalized for its final output: %+v", q)
	}
	if out := rateExplains("aggregate", res.diag); out.Status != stats.EfficiencyPartial || out.Band == stats.EfficiencyBandEfficient {
		t.Fatalf("planner-only $out pipeline must be partial: %+v", out)
	}
}

func TestExplainIntegration_EnrichEndToEnd(t *testing.T) {
	client, database := explainITDatabase(t)
	origConnect, origDisconnect := connectFn, disconnectFn
	connectFn = func(ctx context.Context, cfg *config.AppConfig, dbName string) (*db.Connection, error) {
		return &db.Connection{Client: client, Database: client.Database(dbName)}, nil
	}
	disconnectFn = func(*db.Connection, context.Context) {}
	defer func() { connectFn, disconnectFn = origConnect, origDisconnect }()

	c := stats.NewCollector()
	c.ConfigureInsights(&config.AppConfig{
		InsightsEnabled: true, InsightsSamplingRate: 1, InsightsSlowThresholdMs: 100, InsightsMaxEvents: 500, InsightsMaxGroups: 50,
		InsightsExplainEnabled: true, InsightsExplainTopN: 5, InsightsExplainSeverityMode: "high_and_low", InsightsExplainWorkers: 2,
	})
	for i := 0; i < 20; i++ {
		q := &stats.ExecutableQuery{Database: database.Name(), Filter: map[string]interface{}{"sku": fmt.Sprintf("sku-%d", i*7)}}
		c.RecordOperationEvent("find", database.Name(), "orders", "find|{sku:<str>}", "find on fields: sku", []string{"sku"}, time.Duration(800+i*20)*time.Millisecond, true, 1, q, "orders by sku", "integration", "w", "", "", "", "", 0)
	}
	rep := c.GetFinalInsights()
	enrichInsightsWithExplain(context.Background(), rep.Metadata.Status, &rep, c.SnapshotExplainInventory(), &config.AppConfig{}, explainParams{
		topN: 5, maxTimeMs: 5000, verbosity: "executionStats", severityMode: "high_and_low", efficiencyMode: config.ExplainEfficiencyModeDefault, workers: 2, retries: 1,
	})

	sq := rep.SlowQueries[0]
	if sq.ExplainFinding == nil || sq.ExplainFinding.Diagnosis != stats.DiagCollscanSelective || sq.ExplainFinding.Confidence != "high" {
		t.Fatalf("expected high-confidence selective collscan from two real samples, got %+v", sq.ExplainFinding)
	}
	if len(sq.ExplainSamples) != 2 || rep.Metadata.ExplainCommandsRun != 2 {
		t.Fatalf("expected two explain commands, got samples=%d commands=%d", len(sq.ExplainSamples), rep.Metadata.ExplainCommandsRun)
	}
	found := false
	for _, r := range rep.Recommendations {
		if r.Source == "explain" && r.ShapeID == sq.ShapeID {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected explain-backed recommendation, got %+v", rep.Recommendations)
	}
	for _, cs := range rep.ExplainCoverage.Shapes {
		if cs.ShapeID != sq.ShapeID {
			continue
		}
		e := cs.Efficiency
		if e == nil || e.Status != stats.EfficiencyRated || e.Score == nil || *e.Score > 38 || e.Band != stats.EfficiencyBandInefficient || e.Confidence != "high" || len(e.Samples) != 2 {
			t.Fatalf("expected a high-confidence inefficient rating for the selective scan, got %+v", e)
		}
	}
	if ec := rep.EfficiencyCoverage; ec == nil || ec.Status != "ready" || ec.ShapesRated == 0 {
		t.Fatalf("unexpected efficiency coverage: %+v", ec)
	}
}

func TestExplainIntegration_FastFindFromRealRunIsExplained(t *testing.T) {
	client, database := explainITDatabase(t)
	origConnect, origDisconnect := connectFn, disconnectFn
	connectFn = func(ctx context.Context, cfg *config.AppConfig, dbName string) (*db.Connection, error) {
		return &db.Connection{Client: client, Database: client.Database(dbName)}, nil
	}
	disconnectFn = func(*db.Connection, context.Context) {}
	defer func() { connectFn, disconnectFn = origConnect, origDisconnect }()

	const batchSize = 5
	cfg := &config.AppConfig{
		Duration: "1500ms", StatusRefreshRateSec: 1, Concurrency: 2, FindPercent: 100, FindBatchSize: batchSize, InsertBatchSize: 5,
		InsightsEnabled: true, InsightsSamplingRate: 0.1, InsightsMaxEvents: 100, InsightsMaxGroups: 50, InsightsSlowThresholdMs: 60000,
		InsightsExplainEnabled: true, InsightsExplainTopN: 2, InsightsExplainSeverityMode: "high_only",
		InsightsExplainEfficiencyMode: config.ExplainEfficiencyModeDefault, InsightsExplainWorkers: 2, InsightsExplainMaxTimeMS: 5000,
	}
	collections := []config.CollectionDefinition{{
		DatabaseName: database.Name(), Name: "fast_find",
		Fields: map[string]config.CollectionField{"_id": {Type: "int"}, "name": {Type: "string"}},
	}}
	collector := stats.NewCollector()
	collector.ConfigureInsights(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := runWorkloadFn(ctx, database, collections, nil, cfg, collector); err != nil {
		t.Fatalf("RunWorkload() failed: %v", err)
	}
	fastColl := database.Collection("fast_find")
	before, err := fastColl.CountDocuments(ctx, bson.D{})
	if err != nil {
		t.Fatalf("count: %v", err)
	}

	rep := collector.GetFinalInsights()
	wantKey := fmt.Sprintf("find|fast_in_patch_key|limit:%d", batchSize)
	wantID := stats.StableShapeID("find", "fast_find", wantKey)
	for _, sq := range rep.SlowQueries {
		if sq.ShapeID == wantID {
			t.Fatalf("fast find must stay out of the slow view: %+v", sq)
		}
	}
	enrichInsightsWithExplain(ctx, rep.Metadata.Status, &rep, collector.SnapshotExplainInventory(), cfg, explainParams{
		topN: 2, maxTimeMs: 5000, verbosity: "executionStats", severityMode: "high_only", efficiencyMode: cfg.InsightsExplainEfficiencyMode, workers: 2, retries: 1,
	})

	var shape *stats.ExplainCoverageShape
	for i := range rep.ExplainCoverage.Shapes {
		if rep.ExplainCoverage.Shapes[i].ShapeID == wantID {
			shape = &rep.ExplainCoverage.Shapes[i]
		}
	}
	if shape == nil {
		t.Fatalf("fast find shape missing from explain coverage: %+v", rep.ExplainCoverage)
	}
	if shape.Status != "explained" || shape.SelectedBy == "" || shape.SelectedBy == "severity" || !shape.BelowSlowThreshold || shape.Evidence != "execution_stats" {
		t.Fatalf("expected the fast find to be explained by an efficiency slot: %+v", *shape)
	}
	for _, smp := range shape.Samples {
		d := smp.Diagnostics
		if smp.Status != "explained" || d == nil || !d.ExecutionStatsAvailable || d.NReturned > batchSize || d.CollectionScanDetected {
			t.Fatalf("expected an _id-indexed plan returning at most %d docs, got %+v %+v", batchSize, smp, d)
		}
		if !strings.Contains(d.CommandSummary, fmt.Sprintf("limit=%d", batchSize)) {
			t.Fatalf("explain must keep the executed limit, got %q", d.CommandSummary)
		}
	}
	if cov := rep.ExplainCoverage; cov.SelectedBelowSlowThreshold == 0 || cov.CompletedShapes == 0 || cov.Sampling.CaptureStatus != "ready" {
		t.Fatalf("unexpected coverage: %+v", cov)
	}
	if e := shape.Efficiency; e == nil || e.Status != stats.EfficiencyRated || e.Band != stats.EfficiencyBandEfficient || e.Version != stats.EfficiencyVersion {
		t.Fatalf("expected the fast _id-indexed find to be rated efficient, got %+v", e)
	}
	if after, err := fastColl.CountDocuments(ctx, bson.D{}); err != nil || after != before {
		t.Fatalf("explain changed document count: %d -> %d (%v)", before, after, err)
	}
}

func sortSpillStage(t *testing.T, res explainEvidence) stats.StageResourceMetric {
	t.Helper()
	if res.status != "explained" || res.diag == nil || res.diag.Metrics == nil || res.diag.Metrics.SortSpill == nil {
		t.Fatalf("expected an executionStats explain with sort/spill fields, got %+v", res)
	}
	for _, st := range res.diag.Metrics.SortSpill.Stages {
		if st.Sort {
			return st
		}
	}
	t.Fatalf("no sort stage reported: %+v", res.diag.Metrics.SortSpill)
	return stats.StageResourceMetric{}
}

// The spill case lowers internalQueryMaxBlockingSortMemoryUsageBytes on the
// disposable test server (restored afterwards); the explained command itself
// is unchanged and carries no allowDiskUse.
func TestExplainIntegration_SortSpillMeasurements(t *testing.T) {
	client, database := explainITDatabase(t)
	ctx := context.Background()
	pipeline := []interface{}{
		map[string]interface{}{"$match": map[string]interface{}{"qty": map[string]interface{}{"$gte": 0}}},
		map[string]interface{}{"$sort": bson.D{{Key: "sku", Value: -1}, {Key: "qty", Value: 1}}},
	}
	before := fmt.Sprintf("%v", pipeline)
	event := func() stats.RepresentativeSample {
		return explainITEvent(database, "aggregate", stats.ExecutableQuery{Pipeline: pipeline})
	}

	mem := runExplainForEvent(ctx, database, event(), 5000, "executionStats")
	st := sortSpillStage(t, mem)
	if st.UsedDisk == nil || *st.UsedDisk || st.Spills == nil || st.Spills.Value != 0 || st.MemoryLimitBytes == nil || st.MemoryLimitBytes.Value <= 0 {
		t.Fatalf("in-memory sort must report usedDisk false, spills 0, and a memory limit: %+v", st)
	}
	if st.SortBytesProcessed == nil || (st.SortBytesUsable() && st.SortBytesProcessed.Value <= 0) {
		t.Fatalf("sort bytes processed must be positive or flagged: %+v", st)
	}
	if st.Namespace != database.Name()+".orders" || mem.diag.ServerVersion == "" {
		t.Fatalf("namespace %q / server version %q", st.Namespace, mem.diag.ServerVersion)
	}

	var hello bson.M
	if err := client.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello); err == nil && hello["msg"] == "isdbgrid" {
		t.Log("NOT RUN: spill case needs the shard's parameter; connected to mongos")
		return
	}
	admin := client.Database("admin")
	var cur bson.M
	if err := admin.RunCommand(ctx, bson.D{{Key: "getParameter", Value: 1}, {Key: "internalQueryMaxBlockingSortMemoryUsageBytes", Value: 1}}).Decode(&cur); err != nil {
		skipIntegration(t, "skipping spill case: getParameter not permitted: %v", err)
	}
	orig := cur["internalQueryMaxBlockingSortMemoryUsageBytes"]
	if err := admin.RunCommand(ctx, bson.D{{Key: "setParameter", Value: 1}, {Key: "internalQueryMaxBlockingSortMemoryUsageBytes", Value: int32(16384)}}).Err(); err != nil {
		skipIntegration(t, "skipping spill case: setParameter not permitted: %v", err)
	}
	t.Cleanup(func() {
		_ = admin.RunCommand(context.Background(), bson.D{{Key: "setParameter", Value: 1}, {Key: "internalQueryMaxBlockingSortMemoryUsageBytes", Value: orig}}).Err()
	})

	spill := runExplainForEvent(ctx, database, event(), 5000, "executionStats")
	st = sortSpillStage(t, spill)
	if st.UsedDisk == nil || !*st.UsedDisk || st.Spills == nil || st.Spills.Value <= 0 ||
		st.SpillStorageBytes == nil || st.SpillStorageBytes.Value <= 0 || st.MemoryLimitBytes == nil || st.MemoryLimitBytes.Value != 16384 {
		t.Fatalf("spilled sort must report usedDisk, spills, spill file size, and the lowered limit: %+v", st)
	}
	if !st.SortBytesUsable() || st.SortBytesProcessed.Value <= 0 {
		t.Fatalf("a spilled sort reports the bytes it processed: %+v", st)
	}
	if spill.diag.Metrics.SortSpill.Status != stats.SortSpillMeasured {
		t.Fatalf("status %+v", spill.diag.Metrics.SortSpill)
	}

	samples := []stats.ExplainSample{
		{Role: "median", Status: "explained", Diagnostics: mem.diag},
		{Role: "p95", Status: "explained", Diagnostics: spill.diag},
	}
	o := stats.BuildExecutionOverhead("aggregate", samples, "")
	if o.Status != stats.OverheadSpilled || !strings.Contains(o.Measurements.Summary, "differs by sample") ||
		!strings.Contains(strings.Join(o.Measurements.Differences, "\n"), "usedDisk false vs true") {
		t.Fatalf("overhead/measurements: %s %+v", o.Status, o.Measurements)
	}
	if after := fmt.Sprintf("%v", pipeline); after != before {
		t.Fatalf("explain changed the executed pipeline: %s -> %s", before, after)
	}
	t.Logf("in-memory: %s", stats.StageMeasurementText(sortSpillStage(t, mem)))
	t.Logf("spill: %s", stats.StageMeasurementText(st))
}
