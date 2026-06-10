package mongo

import (
	"context"
	"fmt"
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

			shardKeyDoc := buildOrderedShardKey(col, col.ShardConfig.Key, false)
			// Check for 'hashed' type in config to set flag correctly
			for _, v := range col.ShardConfig.Key {
				if s, ok := v.(string); ok && s == "hashed" {
					shardKeyDoc = buildOrderedShardKey(col, col.ShardConfig.Key, true)
					break
				}
			}

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
				logger.Info("Sharding configured for '%s' (Key: %v)", col.Name, col.ShardConfig.Key)
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
		models := make([]mongo.IndexModel, 0, len(col.Indexes))

		for _, idx := range col.Indexes {
			keysDoc := bson.D{}
			for key, val := range idx.Keys {
				var indexValue interface{}
				if f, ok := val.(float64); ok {
					indexValue = int32(f)
				} else {
					indexValue = val
				}
				keysDoc = append(keysDoc, bson.E{Key: key, Value: indexValue})
			}
			models = append(models, mongo.IndexModel{Keys: keysDoc})
		}

		ctxCreate, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		opts := options.CreateIndexes()

		if _, err := collection.Indexes().CreateMany(ctxCreate, models, opts); err != nil {
			return fmt.Errorf("create indexes on %s.%s: %w", col.DatabaseName, col.Name, err)
		}

		logger.Info("Created %d indexes on '%s.%s'", len(models), col.DatabaseName, col.Name)
	}
	return nil
}
