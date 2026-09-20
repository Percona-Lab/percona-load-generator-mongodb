//go:build integration

package mongo

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/datagen"
	"github.com/Percona-Lab/percona-load-generator-mongodb/resources"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/event"
	driverMongo "go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"
)

// BenchmarkInsertRandomDocumentsIntegration measures generation plus acknowledged
// insertion into a real server. Collection/index setup and count verification are
// outside the timer. Each operation seeds a fresh collection, not an existing one.
func BenchmarkInsertRandomDocumentsIntegration(b *testing.B) {
	data, err := resources.Defaults.ReadFile("collections/default.json")
	if err != nil {
		b.Fatal(err)
	}
	var defaults []config.CollectionDefinition
	if err := json.Unmarshal(data, &defaults); err != nil {
		b.Fatal(err)
	}
	defer datagen.ConfigureSeed(0)

	for _, workload := range []struct {
		name  string
		count int
		col   config.CollectionDefinition
	}{
		{"generic_256B", 100000, config.CollectionDefinition{Name: "docs", Fields: map[string]config.CollectionField{
			"value":   {Type: "int"},
			"payload": {Type: "string", Enum: []interface{}{strings.Repeat("x", 256)}},
		}}},
		{"flights", 10000, defaults[0]},
	} {
		b.Run(workload.name, func(b *testing.B) {
			for _, workers := range []int{1, 4, 8} {
				b.Run(fmt.Sprintf("writers_%d", workers), func(b *testing.B) {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
					defer cancel()
					var active, peak atomic.Int32
					monitor := &event.CommandMonitor{
						Started: func(_ context.Context, evt *event.CommandStartedEvent) {
							if evt.CommandName != "insert" {
								return
							}
							n := active.Add(1)
							for old := peak.Load(); n > old; old = peak.Load() {
								if peak.CompareAndSwap(old, n) {
									break
								}
							}
						},
						Succeeded: func(_ context.Context, evt *event.CommandSucceededEvent) {
							if evt.CommandName == "insert" {
								active.Add(-1)
							}
						},
						Failed: func(_ context.Context, evt *event.CommandFailedEvent) {
							if evt.CommandName == "insert" {
								active.Add(-1)
							}
						},
					}
					client, err := driverMongo.Connect(options.Client().ApplyURI(integrationMongoURI()).
						SetMaxPoolSize(16).SetMinPoolSize(16).SetMonitor(monitor).
						SetWriteConcern(writeconcern.W1()).SetServerSelectionTimeout(5 * time.Second))
					if err != nil {
						b.Fatal(err)
					}
					defer client.Disconnect(context.Background())
					if err := client.Ping(ctx, nil); err != nil {
						b.Fatal(err)
					}
					db := client.Database(fmt.Sprintf("plgm_seed_bench_%d", time.Now().UnixNano()))
					defer db.Drop(context.Background())
					col := workload.col
					col.DatabaseName = db.Name()
					col.ShardConfig = nil
					cfg := &config.AppConfig{SeedBatchSize: 1000, SeedConcurrency: workers, RandomSeed: 42}
					// Warm the generator, driver, and storage engine outside the timer.
					if err := InsertRandomDocuments(ctx, db, col, 1000, cfg); err != nil {
						b.Fatal(err)
					}
					peak.Store(0)
					b.ResetTimer()
					b.StopTimer()
					for i := 0; i < b.N; i++ {
						if err := db.Collection(col.Name).Drop(ctx); err != nil {
							b.Fatal(err)
						}
						if err := db.CreateCollection(ctx, col.Name); err != nil {
							b.Fatal(err)
						}
						if err := CreateIndexesFromConfig(ctx, db, &config.CollectionsFile{Collections: []config.CollectionDefinition{col}}); err != nil {
							b.Fatal(err)
						}
						b.StartTimer()
						err := InsertRandomDocuments(ctx, db, col, workload.count, cfg)
						b.StopTimer()
						if err != nil {
							b.Fatal(err)
						}
						count, err := db.Collection(col.Name).CountDocuments(ctx, bson.M{})
						if err != nil || count != int64(workload.count) {
							b.Fatalf("count/error = %d/%v, want %d/nil", count, err, workload.count)
						}
					}
					b.ReportMetric(float64(workload.count)*float64(b.N)/b.Elapsed().Seconds(), "docs/s")
					b.ReportMetric(float64(peak.Load()), "max_inflight")
				})
			}
		})
	}
}
