//go:build integration
// +build integration

package mongo

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"go.mongodb.org/mongo-driver/v2/bson"
	driverMongo "go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func integrationMongoURI() string {
	if v := os.Getenv("PLGM_IT_MONGO_URI"); v != "" {
		return v
	}
	return "mongodb://127.0.0.1:30777"
}

func connectIntegrationMongoOrSkip(t *testing.T) *driverMongo.Client {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := driverMongo.Connect(options.Client().
		ApplyURI(integrationMongoURI()).
		SetServerSelectionTimeout(2 * time.Second))
	if err != nil {
		t.Skipf("skipping integration test: unable to create mongo client: %v", err)
	}

	if err := client.Ping(ctx, nil); err != nil {
		_ = client.Disconnect(context.Background())
		t.Skipf("skipping integration test: mongo is not reachable at %s: %v", integrationMongoURI(), err)
	}

	return client
}

func isAuthErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unauthorized") || strings.Contains(msg, "authentication")
}

func integrationBoolEnv(key string) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

func requireMongosOrSkip(t *testing.T, client *driverMongo.Client) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var hello bson.M
	if err := client.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello); err != nil {
		if isAuthErr(err) {
			t.Skipf("skipping mongos-only integration test: unable to run hello against admin: %v", err)
		}
		t.Skipf("skipping mongos-only integration test: failed hello command: %v", err)
	}
	if hello["msg"] != "isdbgrid" {
		t.Skipf("skipping mongos-only integration test: target is not mongos (msg=%v)", hello["msg"])
	}
}

func requireShardingPrivilegesOrSkip(t *testing.T, client *driverMongo.Client) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Database("admin").RunCommand(ctx, bson.D{{Key: "listShards", Value: 1}}).Err(); err != nil {
		if isAuthErr(err) {
			t.Skipf("skipping mongos-only integration test: missing listShards privileges: %v", err)
		}
		t.Skipf("skipping mongos-only integration test: unable to list shards: %v", err)
	}
}

func TestRunWorkloadIntegration_OneShotExecutesFindUpdateAndSkipsInsert(t *testing.T) {
	client := connectIntegrationMongoOrSkip(t)
	defer client.Disconnect(context.Background())

	dbName := fmt.Sprintf("plgm_it_%d", time.Now().UnixNano())
	collName := "workload_smoke"
	db := client.Database(dbName)
	coll := db.Collection(collName)
	t.Cleanup(func() {
		_ = db.Drop(context.Background())
	})

	seedID := "seed-doc"
	if _, err := coll.InsertOne(context.Background(), map[string]interface{}{
		"_id":     seedID,
		"counter": 0,
	}); err != nil {
		if isAuthErr(err) {
			t.Skipf("skipping integration test: Mongo URI %q lacks required auth for write operations: %v", integrationMongoURI(), err)
		}
		t.Fatalf("seed document insert failed: %v", err)
	}

	cfg := &config.AppConfig{
		Duration:  "0s", // one-shot path -> runAllQueriesOnce
		DebugMode: false,
	}

	collections := []config.CollectionDefinition{
		{
			DatabaseName: dbName,
			Name:         collName,
			Fields: map[string]config.CollectionField{
				"_id": {Type: "string"},
			},
		},
	}

	queries := []config.QueryDefinition{
		{
			Database:   dbName,
			Collection: collName,
			Operation:  "find",
			Filter:     map[string]interface{}{"_id": "<string>"},
		},
		{
			Database:   dbName,
			Collection: collName,
			Operation:  "updateOne",
			Filter:     map[string]interface{}{"_id": seedID},
			Update:     map[string]interface{}{"$set": map[string]interface{}{"counter": 1}},
		},
		{
			Database:   dbName,
			Collection: collName,
			Operation:  "insert", // intentionally present: runAllQueriesOnce should skip insert/insertMany
			Filter:     map[string]interface{}{"_id": "should_not_be_inserted"},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := RunWorkload(ctx, db, collections, queries, cfg); err != nil {
		if isAuthErr(err) {
			t.Skipf("skipping integration test: insufficient auth for workload operations at %q: %v", integrationMongoURI(), err)
		}
		t.Fatalf("RunWorkload() failed: %v", err)
	}

	var updated struct {
		Counter int `bson:"counter"`
	}
	if err := coll.FindOne(context.Background(), map[string]interface{}{"_id": seedID}).Decode(&updated); err != nil {
		t.Fatalf("failed to load updated seed document: %v", err)
	}
	if updated.Counter != 1 {
		t.Fatalf("expected counter to be updated to 1, got %d", updated.Counter)
	}

	count, err := coll.CountDocuments(context.Background(), map[string]interface{}{})
	if err != nil {
		t.Fatalf("count documents failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected insert operation to be skipped in one-shot mode; got document count %d", count)
	}
}

func TestRunWorkloadIntegration_OneShotMultiDBMixedShardConfig(t *testing.T) {
	client := connectIntegrationMongoOrSkip(t)
	defer client.Disconnect(context.Background())

	dbA := fmt.Sprintf("plgm_it_a_%d", time.Now().UnixNano())
	dbB := fmt.Sprintf("plgm_it_b_%d", time.Now().UnixNano())
	collA := "orders"
	collB := "customers"

	dbAHandle := client.Database(dbA)
	dbBHandle := client.Database(dbB)
	t.Cleanup(func() {
		_ = dbAHandle.Drop(context.Background())
		_ = dbBHandle.Drop(context.Background())
	})

	if _, err := dbAHandle.Collection(collA).InsertOne(context.Background(), map[string]interface{}{"_id": "a1", "counter": 0}); err != nil {
		if isAuthErr(err) {
			t.Skipf("skipping integration test: insufficient auth for writes: %v", err)
		}
		t.Fatalf("seed dbA failed: %v", err)
	}
	if _, err := dbBHandle.Collection(collB).InsertOne(context.Background(), map[string]interface{}{"_id": "b1", "counter": 0}); err != nil {
		if isAuthErr(err) {
			t.Skipf("skipping integration test: insufficient auth for writes: %v", err)
		}
		t.Fatalf("seed dbB failed: %v", err)
	}

	cfg := &config.AppConfig{
		Duration:                         "0s",
		ShardingMode:                     "auto",
		ShardingSkipGenericWithoutConfig: true,
	}
	collections := []config.CollectionDefinition{
		{
			DatabaseName: dbA,
			Name:         collA,
			Fields:       map[string]config.CollectionField{"_id": {Type: "string"}},
			ShardConfig:  &config.ShardConfig{Key: map[string]interface{}{"_id": 1}},
		},
		{
			DatabaseName: dbB,
			Name:         collB,
			Fields:       map[string]config.CollectionField{"_id": {Type: "string"}},
		},
	}
	queries := []config.QueryDefinition{
		{Database: dbA, Collection: collA, Operation: "updateOne", Filter: map[string]interface{}{"_id": "a1"}, Update: map[string]interface{}{"$set": map[string]interface{}{"counter": 1}}},
		{Database: dbB, Collection: collB, Operation: "updateOne", Filter: map[string]interface{}{"_id": "b1"}, Update: map[string]interface{}{"$set": map[string]interface{}{"counter": 1}}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := RunWorkload(ctx, dbAHandle, collections, queries, cfg); err != nil {
		if isAuthErr(err) {
			t.Skipf("skipping integration test: insufficient auth for workload operations: %v", err)
		}
		t.Fatalf("RunWorkload() failed for multi-db mixed shardConfig scenario: %v", err)
	}

	var aDoc struct {
		Counter int `bson:"counter"`
	}
	if err := dbAHandle.Collection(collA).FindOne(context.Background(), map[string]interface{}{"_id": "a1"}).Decode(&aDoc); err != nil {
		t.Fatalf("load dbA document failed: %v", err)
	}
	var bDoc struct {
		Counter int `bson:"counter"`
	}
	if err := dbBHandle.Collection(collB).FindOne(context.Background(), map[string]interface{}{"_id": "b1"}).Decode(&bDoc); err != nil {
		t.Fatalf("load dbB document failed: %v", err)
	}
	if aDoc.Counter != 1 || bDoc.Counter != 1 {
		t.Fatalf("expected both multi-db updates applied, got dbA=%d dbB=%d", aDoc.Counter, bDoc.Counter)
	}
}

func TestRunWorkloadIntegration_MongosMixedShardedAndUnshardedCollections(t *testing.T) {
	if !integrationBoolEnv("PLGM_IT_ENABLE_MONGOS_SHARD_TEST") {
		t.Skip("skipping mongos mixed-sharding integration test: set PLGM_IT_ENABLE_MONGOS_SHARD_TEST=true to enable")
	}

	client := connectIntegrationMongoOrSkip(t)
	defer client.Disconnect(context.Background())
	requireMongosOrSkip(t, client)
	requireShardingPrivilegesOrSkip(t, client)

	dbA := fmt.Sprintf("plgm_it_mongos_a_%d", time.Now().UnixNano())
	dbB := fmt.Sprintf("plgm_it_mongos_b_%d", time.Now().UnixNano())
	collSharded := "orders"
	collUnsharded := "customers"

	dbAHandle := client.Database(dbA)
	dbBHandle := client.Database(dbB)
	t.Cleanup(func() {
		_ = dbAHandle.Drop(context.Background())
		_ = dbBHandle.Drop(context.Background())
	})

	if _, err := dbAHandle.Collection(collSharded).InsertOne(context.Background(), map[string]interface{}{"_id": "sa1", "counter": 0}); err != nil {
		if isAuthErr(err) {
			t.Skipf("skipping mongos mixed-sharding integration test: insufficient auth for seed writes: %v", err)
		}
		t.Fatalf("seed sharded collection failed: %v", err)
	}
	if _, err := dbBHandle.Collection(collUnsharded).InsertOne(context.Background(), map[string]interface{}{"_id": "ub1", "counter": 0}); err != nil {
		if isAuthErr(err) {
			t.Skipf("skipping mongos mixed-sharding integration test: insufficient auth for seed writes: %v", err)
		}
		t.Fatalf("seed unsharded collection failed: %v", err)
	}

	cfg := &config.AppConfig{
		Duration:                         "0s",
		ShardingMode:                     "auto",
		ShardingSkipGenericWithoutConfig: true,
		Concurrency:                      2,
	}
	collections := []config.CollectionDefinition{
		{
			DatabaseName: dbA,
			Name:         collSharded,
			Fields:       map[string]config.CollectionField{"_id": {Type: "string"}},
			ShardConfig:  &config.ShardConfig{Key: map[string]interface{}{"_id": 1}},
		},
		{
			DatabaseName: dbB,
			Name:         collUnsharded,
			Fields:       map[string]config.CollectionField{"_id": {Type: "string"}},
		},
	}
	queries := []config.QueryDefinition{
		{
			Database:   dbA,
			Collection: collSharded,
			Operation:  "updateOne",
			Filter:     map[string]interface{}{"_id": "sa1"},
			Update:     map[string]interface{}{"$set": map[string]interface{}{"counter": 1}},
		},
		{
			Database:   dbB,
			Collection: collUnsharded,
			Operation:  "updateOne",
			Filter:     map[string]interface{}{"_id": "ub1"},
			Update:     map[string]interface{}{"$set": map[string]interface{}{"counter": 1}},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := RunWorkload(ctx, dbAHandle, collections, queries, cfg); err != nil {
		if isAuthErr(err) {
			t.Skipf("skipping mongos mixed-sharding integration test: insufficient auth for sharding/workload operations: %v", err)
		}
		t.Fatalf("RunWorkload() failed for mongos mixed sharded/unsharded test: %v", err)
	}

	isShardedA, _, err := checkShardingStatus(context.Background(), dbAHandle, collSharded)
	if err != nil {
		t.Fatalf("checkShardingStatus for sharded target failed: %v", err)
	}
	if !isShardedA {
		t.Fatalf("expected %s.%s to be sharded in mongos mixed test", dbA, collSharded)
	}

	isShardedB, _, err := checkShardingStatus(context.Background(), dbBHandle, collUnsharded)
	if err != nil {
		t.Fatalf("checkShardingStatus for unsharded target failed: %v", err)
	}
	if isShardedB {
		t.Fatalf("expected %s.%s to remain unsharded in mongos mixed test", dbB, collUnsharded)
	}

	var shardedDoc struct {
		Counter int `bson:"counter"`
	}
	if err := dbAHandle.Collection(collSharded).FindOne(context.Background(), map[string]interface{}{"_id": "sa1"}).Decode(&shardedDoc); err != nil {
		t.Fatalf("load sharded doc failed: %v", err)
	}
	var unshardedDoc struct {
		Counter int `bson:"counter"`
	}
	if err := dbBHandle.Collection(collUnsharded).FindOne(context.Background(), map[string]interface{}{"_id": "ub1"}).Decode(&unshardedDoc); err != nil {
		t.Fatalf("load unsharded doc failed: %v", err)
	}
	if shardedDoc.Counter != 1 || unshardedDoc.Counter != 1 {
		t.Fatalf("expected both updates to apply in mixed test, got sharded=%d unsharded=%d", shardedDoc.Counter, unshardedDoc.Counter)
	}
}
