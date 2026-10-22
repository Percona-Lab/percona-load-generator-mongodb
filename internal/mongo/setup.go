package mongo

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/datagen"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/logger"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/workloads"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// SeedProgressFunc reports how many documents of one collection have been
// inserted. inserted is between 1 and total and may arrive out of order when
// several writers finish batches together. total is that collection's count.
type SeedProgressFunc func(inserted, total int)

type seedProgressKey struct{}

// WithSeedProgress attaches a per-collection insert callback to ctx.
func WithSeedProgress(ctx context.Context, fn SeedProgressFunc) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, seedProgressKey{}, fn)
}

// SeedProgressFrom returns the callback attached by WithSeedProgress, or nil.
func SeedProgressFrom(ctx context.Context) SeedProgressFunc {
	if ctx == nil {
		return nil
	}
	fn, _ := ctx.Value(seedProgressKey{}).(SeedProgressFunc)
	return fn
}

func InsertRandomDocuments(ctx context.Context, db *mongo.Database, col config.CollectionDefinition, count int, cfg *config.AppConfig) error {
	collection := db.Client().Database(col.DatabaseName).Collection(col.Name)
	return insertRandomDocuments(ctx, col, count, cfg, func(ctx context.Context, batch []interface{}) error {
		_, err := collection.InsertMany(ctx, batch)
		return err
	})
}

// insertRandomDocuments keeps generation serial and dispatches owned batches to
// concurrent writers. insertBatch must not retain a batch after returning.
func insertRandomDocuments(ctx context.Context, col config.CollectionDefinition, count int, cfg *config.AppConfig, insertBatch func(context.Context, []interface{}) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if count <= 0 {
		return nil
	}
	if cfg == nil {
		cfg = &config.AppConfig{}
	}
	datagen.ConfigureSeed(cfg.RandomSeed)
	batchSize := cfg.SeedBatchSize
	if batchSize <= 0 {
		batchSize = 1000
	}
	batchSize = min(batchSize, count)
	workers := cfg.SeedConcurrency
	if workers <= 0 {
		workers = 1
	}
	workers = min(workers, 1+(count-1)/batchSize)
	logger.Info("Seeding %d documents into '%s.%s' with %d writer(s)...", count, col.DatabaseName, col.Name, workers)

	// With a seed and freshly dropped collections the _ids are derived from
	// the seed too, so two seeded runs start from identical documents.
	// Without a drop a repeated seed would collide with the earlier _ids.
	seededIDs := cfg.RandomSeed != 0 && cfg.DropCollections
	ns := col.DatabaseName + "." + col.Name

	modu := max(1, count/10)
	nextLogTarget := modu
	totalInserted := 0
	onProgress := SeedProgressFrom(ctx)
	var progressMu sync.Mutex
	var wg sync.WaitGroup

	workCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	// No queued batches: at most one batch per writer plus the producer's batch
	// is held in memory, independent of documents_count.
	batches := make(chan []interface{})
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for batch := range batches {
				if workCtx.Err() != nil {
					return
				}
				if err := insertBatch(workCtx, batch); err != nil {
					cancel(fmt.Errorf("insert batch into %s.%s: %w", col.DatabaseName, col.Name, err))
					return
				}
				var inserted int
				progressMu.Lock()
				totalInserted += len(batch)
				inserted = totalInserted
				if totalInserted >= nextLogTarget || totalInserted == count {
					logger.Info("-- Inserted %d documents...", totalInserted)
					nextLogTarget = totalInserted + modu
				}
				progressMu.Unlock()
				if onProgress != nil {
					onProgress(inserted, count)
				}
			}
		}()
	}

generate:
	for generated := 0; generated < count; {
		// Each dispatched batch owns its backing array; never reuse it while a
		// writer may still be serializing its documents.
		size := min(batchSize, count-generated)
		batch := make([]interface{}, 0, size)
		for i := 0; i < size; i++ {
			if workCtx.Err() != nil {
				break generate
			}
			doc := workloads.GenerateDocument(col, cfg)
			if seededIDs {
				if _, ok := doc["_id"]; !ok {
					doc["_id"] = seededDocumentID(cfg.RandomSeed, ns, generated+i)
				}
			}
			batch = append(batch, doc)
		}
		select {
		case batches <- batch:
			generated += size
		case <-workCtx.Done():
			break generate
		}
	}
	close(batches)
	wg.Wait()
	if err := context.Cause(workCtx); err != nil {
		return err
	}

	logger.Debug("Document generation and seeding complete")
	return nil
}

// seededDocumentID is the _id of the index-th seeded document of ns: the
// seeded reference time, a hash of the seed and namespace, and the index.
func seededDocumentID(seed int64, ns string, index int) bson.ObjectID {
	h := fnv.New32a()
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(seed))
	h.Write(b[:])
	h.Write([]byte(ns))
	var id bson.ObjectID
	binary.BigEndian.PutUint32(id[0:4], uint32(datagen.SeededReferenceTime.Unix()))
	binary.BigEndian.PutUint32(id[4:8], h.Sum32())
	binary.BigEndian.PutUint32(id[8:12], uint32(index))
	return id
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
