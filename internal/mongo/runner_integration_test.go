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
