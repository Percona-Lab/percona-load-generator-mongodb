package mongo

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/datagen"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/logger"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/workloads"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func InsertRandomDocuments(ctx context.Context, db *mongo.Database, col config.CollectionDefinition, count int, cfg *config.AppConfig) error {
	// Configure deterministic generation (no-op when random_seed == 0). The seed
	// phase is single-threaded, so the generated dataset is fully reproducible.
	if cfg != nil {
		datagen.ConfigureSeed(cfg.RandomSeed)
	}
	logger.Info("Seeding %d documents into '%s.%s'...", count, col.DatabaseName, col.Name)

	// 1. Configure Batch Size
	batchSize := cfg.SeedBatchSize
	if batchSize <= 0 {
		batchSize = 1000
	}

	// 2. Configure Progress Reporting
	// Calculate 10% interval
	modu := int(float32(count) * 0.1)
	if modu < 1 {
		modu = 1
	}
	nextLogTarget := modu

	logger.Debug("Inserting documents in batches of %d", batchSize)
	logger.Debug("Progress reporting every %d documents", modu)

	targetDB := db.Client().Database(col.DatabaseName)
	collection := targetDB.Collection(col.Name)

	// Pre-allocate batch slice
	batch := make([]interface{}, 0, batchSize)
	totalInserted := 0

	for i := 0; i < count; i++ {
		// Generate document
		batch = append(batch, workloads.GenerateDocument(col, cfg))

		// If batch is full, InsertMany
		if len(batch) >= batchSize {
			if _, err := collection.InsertMany(ctx, batch); err != nil {
				return fmt.Errorf("insert batch into %s.%s: %w", col.DatabaseName, col.Name, err)
			}

			totalInserted += len(batch)
			batch = batch[:0] // Reset batch, keep capacity

			// Check if we crossed the 10% threshold
			if totalInserted >= nextLogTarget {
				logger.Info("-- Inserted %d documents...", totalInserted)
				// Advance target to next 10% marker
				for totalInserted >= nextLogTarget {
					nextLogTarget += modu
				}
			}
		}
	}

	// Insert any remaining documents
	if len(batch) > 0 {
		if _, err := collection.InsertMany(ctx, batch); err != nil {
			return fmt.Errorf("insert remaining documents into %s.%s: %w", col.DatabaseName, col.Name, err)
		}
		totalInserted += len(batch)
		logger.Info("-- Inserted %d documents (Final)...", totalInserted)
	}

	logger.Debug("Document generation and seeding complete")
	return nil
}

// CreateCollectionsFromConfig creates collections and applies sharding if configured.
func CreateCollectionsFromConfig(ctx context.Context, db *mongo.Database, cfg *config.CollectionsFile, drop bool) error {
	adminDB := db.Client().Database("admin")

	// 1. Check if the cluster is actually sharded
	var helloResult bson.M
	isShardedCluster := false
	if err := adminDB.RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&helloResult); err == nil {
		if msg, ok := helloResult["msg"].(string); ok && msg == "isdbgrid" {
			isShardedCluster = true
		}
	}

	for _, col := range cfg.Collections {
		// If we are NOT dropping, check if the collection is already populated (chunks > 1).
		if !drop {
			isSharded, chunks, _ := checkShardingStatus(ctx, db, col.Name)
			if isSharded && chunks > 1 {
				logger.Info("Collection '%s' already set up (Sharded, %d chunks). Skipping creation.", col.Name, chunks)
				continue
			}
		}

		targetDB := db.Client().Database(col.DatabaseName)

		// 2. Drop if requested
		if drop {
			_ = targetDB.Collection(col.Name).Drop(ctx)
		}

		// 3. Create Collection
		if err := targetDB.CreateCollection(ctx, col.Name); err != nil {
			if drop {
				return fmt.Errorf("create collection %s.%s: %w", col.DatabaseName, col.Name, err)
			}
		}

		// 4. Configure Sharding
		if col.ShardConfig != nil {
			if !isShardedCluster {
				logger.Info("Skipping sharding for '%s': Cluster is not sharded (Replica Set)", col.Name)
				continue
			}

			_ = adminDB.RunCommand(ctx, bson.D{{Key: "enableSharding", Value: col.DatabaseName}})

			shardKeyDoc := buildOrderedShardKey(col.ShardConfig.Key)

			cmd := bson.D{
				{Key: "shardCollection", Value: fmt.Sprintf("%s.%s", col.DatabaseName, col.Name)},
				{Key: "key", Value: shardKeyDoc},
			}
			if col.ShardConfig.Unique {
				cmd = append(cmd, bson.E{Key: "unique", Value: true})
			}

			if err := adminDB.RunCommand(ctx, cmd).Err(); err != nil {
				logger.Info("Warning: Failed to shard collection '%s': %v", col.Name, err)
			} else {
				logger.Info("Sharding configured for '%s' (Key: %s)", col.Name, col.ShardConfig.Key.Signature())
			}
		}
	}
	return nil
}

func CreateIndexesFromConfig(ctx context.Context, db *mongo.Database, cfg *config.CollectionsFile) error {
	for _, col := range cfg.Collections {
		if len(col.Indexes) == 0 {
			continue
		}

		isSharded, chunks, _ := checkShardingStatus(ctx, db, col.Name)
		if isSharded && chunks > 1 {
			logger.Info("Collection '%s' already set up. Skipping index creation.", col.Name)
			continue
		}

		targetDB := db.Client().Database(col.DatabaseName)
		collection := targetDB.Collection(col.Name)
		models, err := buildIndexModels(col.Indexes)
		if err != nil {
			return fmt.Errorf("index definitions for %s.%s: %w", col.DatabaseName, col.Name, err)
		}

		ctxCreate, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		opts := options.CreateIndexes()

		if _, err := collection.Indexes().CreateMany(ctxCreate, models, opts); err != nil {
			return fmt.Errorf("create indexes on %s.%s: %w", col.DatabaseName, col.Name, err)
		}

		logger.Info("Ensured %d indexes on '%s.%s'", len(models), col.DatabaseName, col.Name)
		warnReorderedIndexes(ctxCreate, collection, col)
	}
	return nil
}

// buildIndexModels converts index definitions into createIndexes models with
// keys in declared order and an explicit name, so repeated runs send identical
// specs and the server treats them as no-ops.
func buildIndexModels(indexes []config.IndexDefinition) ([]mongo.IndexModel, error) {
	models := make([]mongo.IndexModel, 0, len(indexes))
	for i, idx := range indexes {
		if len(idx.Keys) == 0 {
			return nil, fmt.Errorf("index at position %d has no keys", i)
		}
		o := options.Index().SetName(idx.EffectiveName())
		if idx.Unique {
			o.SetUnique(true)
		}
		if idx.Sparse {
			o.SetSparse(true)
		}
		if idx.Hidden {
			o.SetHidden(true)
		}
		if idx.ExpireAfterSeconds != nil {
			o.SetExpireAfterSeconds(*idx.ExpireAfterSeconds)
		}
		pfe, err := idx.PartialFilterDoc()
		if err != nil {
			return nil, fmt.Errorf("index %q: %w", idx.EffectiveName(), err)
		}
		if pfe != nil {
			o.SetPartialFilterExpression(pfe)
		}
		models = append(models, mongo.IndexModel{Keys: idx.Keys.BSON(), Options: o})
	}
	return models, nil
}

type existingIndex struct {
	Name string `bson:"name"`
	Key  bson.D `bson:"key"`
}

// reorderedIndex is a deployed index with the same fields and directions as a
// configured index but in a different order.
type reorderedIndex struct {
	Existing   string
	Configured string
}

func findReorderedIndexes(configured []config.IndexDefinition, existing []existingIndex) []reorderedIndex {
	exact := make(map[string]bool, len(configured))
	bySet := make(map[string]string, len(configured))
	for _, idx := range configured {
		exact[idx.Keys.Signature()] = true
		bySet[keySetSignature(idx.Keys.BSON())] = idx.EffectiveName()
	}
	var out []reorderedIndex
	for _, ex := range existing {
		if len(ex.Key) < 2 {
			continue
		}
		if exact[existingKeys(ex.Key).Signature()] {
			continue
		}
		if name, ok := bySet[keySetSignature(ex.Key)]; ok {
			out = append(out, reorderedIndex{Existing: ex.Name, Configured: name})
		}
	}
	return out
}

func existingKeys(d bson.D) config.IndexKeys {
	k := make(config.IndexKeys, 0, len(d))
	for _, e := range d {
		k = append(k, config.IndexKey{Field: e.Key, Value: normalizeKeyValue(e.Value)})
	}
	return k
}

func keySetSignature(d bson.D) string {
	parts := make([]string, 0, len(d))
	for _, e := range d {
		parts = append(parts, fmt.Sprintf("%s:%v", e.Key, normalizeKeyValue(e.Value)))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// normalizeKeyValue maps numeric directions of any BSON numeric type to int32
// so listIndexes output compares equal to configured keys.
func normalizeKeyValue(v interface{}) interface{} {
	switch n := v.(type) {
	case int32:
		return n
	case int64:
		return int32(n)
	case int:
		return int32(n)
	case float64:
		return int32(n)
	}
	return v
}

// warnReorderedIndexes logs deployed indexes that look like leftovers of the
// old unordered key handling. They are never dropped automatically.
func warnReorderedIndexes(ctx context.Context, collection *mongo.Collection, col config.CollectionDefinition) {
	cur, err := collection.Indexes().List(ctx)
	if err != nil {
		logger.Debug("Could not list indexes on '%s.%s' to check key order: %v", col.DatabaseName, col.Name, err)
		return
	}
	var existing []existingIndex
	if err := cur.All(ctx, &existing); err != nil {
		logger.Debug("Could not read indexes on '%s.%s' to check key order: %v", col.DatabaseName, col.Name, err)
		return
	}
	for _, r := range findReorderedIndexes(col.Indexes, existing) {
		logger.Info("Warning: index '%s' on '%s.%s' has the same fields as configured index '%s' in a different key order. Earlier plgm versions could create such indexes from unordered keys. It was not dropped; review it and drop it manually if unused.", r.Existing, col.DatabaseName, col.Name, r.Configured)
	}
}
