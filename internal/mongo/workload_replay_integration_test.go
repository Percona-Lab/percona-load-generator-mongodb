//go:build integration
// +build integration

package mongo

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/workloadrec"
	"go.mongodb.org/mongo-driver/v2/bson"
	driverMongo "go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func shapeCounts(rep stats.InsightsReport) map[string]int {
	out := map[string]int{}
	for _, s := range rep.QueryShapes {
		out[s.Operation+" "+s.Collection+" "+s.ShapeKey] += s.Count
	}
	return out
}

// seedRecordReplayDB drops and seeds dbName from the seed, as a fresh
// instance with the same random_seed would be.
func seedRecordReplayDB(t *testing.T, ctx context.Context, client *driverMongo.Client, collections []config.CollectionDefinition, cfg *config.AppConfig) *driverMongo.Database {
	t.Helper()
	db := client.Database(collections[0].DatabaseName)
	if err := db.Drop(ctx); err != nil {
		t.Fatalf("drop: %v", err)
	}
	for _, col := range collections {
		if err := InsertRandomDocuments(ctx, db, col, cfg.DocumentsCount, cfg); err != nil {
			if isAuthErr(err) {
				skipIntegration(t, "skipping integration test: insufficient auth at %q: %v", integrationMongoURI(), err)
			}
			t.Fatalf("seed %s: %v", col.Name, err)
		}
	}
	return db
}

func seededIDs(t *testing.T, ctx context.Context, db *driverMongo.Database, col string) []interface{} {
	t.Helper()
	cur, err := db.Collection(col).Find(ctx, bson.D{}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}).SetProjection(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		t.Fatal(err)
	}
	var docs []bson.M
	if err := cur.All(ctx, &docs); err != nil {
		t.Fatal(err)
	}
	ids := make([]interface{}, len(docs))
	for i, d := range docs {
		ids[i] = d["_id"]
	}
	return ids
}

// TestWorkloadRecordReplayIntegration records a generated workload, reseeds
// the database from the same seed (a second instance with the same starting
// data), replays the recording there, and checks that the same commands were
// sent and every shape ran as often as in the recorded run.
func TestWorkloadRecordReplayIntegration(t *testing.T) {
	client := connectIntegrationMongoOrSkip(t)
	defer client.Disconnect(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dbName := fmt.Sprintf("plgm_it_record_replay_%d", time.Now().UnixNano())
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
	path := filepath.Join(t.TempDir(), "workload.ndjson")
	base := config.AppConfig{
		Duration: "1500ms", StatusRefreshRateSec: 1, Concurrency: 3, RandomSeed: 2026, DropCollections: true,
		DocumentsCount: 300, SeedBatchSize: 50, SeedConcurrency: 1,
		FindPercent: 50, UpdatePercent: 20, DeletePercent: 5, InsertPercent: 15, BulkInsertPercent: 10,
		FindBatchSize: 5, FindLimit: 5, InsertBatchSize: 5, InsertCacheSize: 50, RecordPoolBootstrapSample: 50, RecordPoolMaxSize: 1000,
		ExistingRecordHitRate: 50, InsightsEnabled: true, InsightsSamplingRate: 1, InsightsMaxEvents: 1000, InsightsMaxGroups: 500,
	}
	base.WebUI.Enabled = true

	recordCfg := base
	recordCfg.WorkloadRecordEnabled, recordCfg.WorkloadRecordPath = true, path
	config.NormalizeWorkloadRecordReplay(&recordCfg)
	dbA := seedRecordReplayDB(t, ctx, client, collections, &recordCfg)
	idsA := seededIDs(t, ctx, dbA, "users")
	recorded := stats.NewCollector()
	if err := RunWorkload(ctx, dbA, collections, queries, &recordCfg, recorded); err != nil {
		t.Fatalf("record run: %v", err)
	}
	recRep := recorded.GetFinalInsights()
	if recRep.Metadata.Recording == nil || recRep.Metadata.Recording.Status != workloadrec.StatusComplete || recRep.Metadata.Recording.OpsWritten == 0 {
		t.Fatalf("recording = %+v", recRep.Metadata.Recording)
	}
	info, err := workloadrec.Scan(path)
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]int{}
	if err := workloadrec.Stream(ctx, path, func(op *workloadrec.OpLine) error { paths[op.Path+"/"+op.Op]++; return nil }); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{workloadrec.PathFastInsertBatch + "/insertMany", workloadrec.PathFastFind + "/find", "/find", "/insert", "/insertMany"} {
		if paths[want] == 0 {
			t.Fatalf("recording has no %s operations: %v", want, paths)
		}
	}
	t.Logf("recorded %d operations: %v", info.Ops, paths)

	replayCfg := base
	replayCfg.WorkloadReplayEnabled, replayCfg.WorkloadReplayPath = true, path
	config.NormalizeWorkloadRecordReplay(&replayCfg)
	dbB := seedRecordReplayDB(t, ctx, client, collections, &replayCfg)
	if idsB := seededIDs(t, ctx, dbB, "users"); !reflect.DeepEqual(idsA, idsB) {
		t.Fatalf("the same seed seeded different _ids (%d vs %d documents)", len(idsA), len(idsB))
	}
	replayed := stats.NewCollector()
	if err := RunWorkload(ctx, dbB, collections, queries, &replayCfg, replayed); err != nil {
		t.Fatalf("replay run: %v", err)
	}
	rep := replayed.WorkloadReplay()
	if rep == nil || rep.Status != workloadrec.StatusComplete || rep.OpsReplayed != info.Ops || rep.OpsTotal != info.Ops {
		t.Fatalf("replay = %+v, recording ops = %d", rep, info.Ops)
	}
	if rep.CommandsHashMatch == nil || !*rep.CommandsHashMatch || rep.CommandsHash != info.Trailer.CommandsHash {
		t.Fatalf("command hashes differ: replay %q recording %q", rep.CommandsHash, info.Trailer.CommandsHash)
	}
	if got, want := shapeCounts(replayed.GetFinalInsights()), shapeCounts(recRep); !reflect.DeepEqual(got, want) {
		t.Fatalf("per-shape op counts differ:\n replay %v\n record %v", got, want)
	}
}

func intPtr(v int) *int { return &v }
