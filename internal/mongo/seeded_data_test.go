package mongo

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/datagen"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func seededCollection() config.CollectionDefinition {
	return config.CollectionDefinition{DatabaseName: "appdb", Name: "events", Fields: map[string]config.CollectionField{
		"name":    {Type: "string", Provider: "first_name"},
		"age":     {Type: "int"},
		"score":   {Type: "double"},
		"amount":  {Type: "decimal"},
		"active":  {Type: "bool"},
		"at":      {Type: "date"},
		"ts":      {Type: "timestamp"},
		"ref":     {Type: "objectid"},
		"tags":    {Type: "array", Items: &config.CollectionField{Type: "string", Provider: "word"}},
		"profile": {Type: "object", Fields: map[string]config.CollectionField{"city": {Type: "string", Provider: "city"}, "zip": {Type: "int"}, "since": {Type: "date"}}},
	}}
}

func seedDocs(t *testing.T, cfg *config.AppConfig, count int) []map[string]interface{} {
	t.Helper()
	var mu sync.Mutex
	var out []map[string]interface{}
	err := insertRandomDocuments(context.Background(), seededCollection(), count, cfg, func(ctx context.Context, batch []interface{}) error {
		mu.Lock()
		defer mu.Unlock()
		for _, d := range batch {
			out = append(out, d.(map[string]interface{}))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSeededDataIsIdenticalAcrossRuns(t *testing.T) {
	t.Cleanup(func() { datagen.ConfigureSeed(0) })
	cfg := &config.AppConfig{RandomSeed: 42, DropCollections: true, SeedBatchSize: 7, SeedConcurrency: 1}
	first := seedDocs(t, cfg, 50)
	second := seedDocs(t, cfg, 50)
	if len(first) != 50 || !reflect.DeepEqual(first, second) {
		t.Fatal("the same seed, count, and collection definition produced different documents")
	}
	seen := map[bson.ObjectID]bool{}
	for i, d := range first {
		id, ok := d["_id"].(bson.ObjectID)
		if !ok {
			t.Fatalf("document %d has no seeded _id: %v", i, d["_id"])
		}
		if id != seededDocumentID(42, "appdb.events", i) || seen[id] {
			t.Fatalf("document %d _id = %v, want the seeded id for index %d", i, id, i)
		}
		seen[id] = true
	}

	other := seedDocs(t, &config.AppConfig{RandomSeed: 43, DropCollections: true, SeedBatchSize: 7}, 50)
	if reflect.DeepEqual(first, other) {
		t.Fatal("a different seed produced the same documents")
	}
}

func TestUnseededDataKeepsDriverGeneratedIDs(t *testing.T) {
	t.Cleanup(func() { datagen.ConfigureSeed(0) })
	for _, cfg := range []*config.AppConfig{{DropCollections: true}, {RandomSeed: 42}} {
		for _, d := range seedDocs(t, cfg, 5) {
			if _, ok := d["_id"]; ok {
				t.Fatalf("seed=%d drop=%v: generated an _id; the driver must assign it", cfg.RandomSeed, cfg.DropCollections)
			}
		}
	}
}
