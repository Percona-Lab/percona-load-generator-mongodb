package mongo

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/accesspattern"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// RecordPool tracks documents known to exist per collection namespace so find/update/delete
// operations can target realistic filters instead of random unmatched values.
//
// selector controls which pooled record is chosen for a given operation,
// enabling uniform, zipfian, or hotspot access patterns. It is stateless and
// safe to share across worker goroutines.
type RecordPool struct {
	mu       sync.RWMutex
	maxSize  int
	pools    map[string][]map[string]interface{}
	selector accesspattern.Selector
}

func NewRecordPool(maxSize int) *RecordPool {
	if maxSize <= 0 {
		maxSize = 10000
	}
	sel, _ := accesspattern.Compile(accesspattern.Config{})
	return &RecordPool{
		maxSize:  maxSize,
		pools:    make(map[string][]map[string]interface{}),
		selector: sel,
	}
}

// SetSelector overrides the access-pattern selector. A nil selector resets to
// uniform selection.
func (rp *RecordPool) SetSelector(sel accesspattern.Selector) {
	if rp == nil {
		return
	}
	rp.mu.Lock()
	defer rp.mu.Unlock()
	if sel == nil {
		sel, _ = accesspattern.Compile(accesspattern.Config{})
	}
	rp.selector = sel
}

func collectionNamespace(db *mongo.Database, col config.CollectionDefinition) string {
	dbName := strings.TrimSpace(col.DatabaseName)
	if dbName == "" && db != nil {
		dbName = db.Name()
	}
	if dbName == "" {
		dbName = "unknown"
	}
	return dbName + "." + col.Name
}

func snapshotDocument(doc map[string]interface{}) map[string]interface{} {
	if doc == nil {
		return nil
	}
	snap := make(map[string]interface{}, len(doc))
	for k, v := range doc {
		snap[k] = v
	}
	return snap
}

func (rp *RecordPool) Add(namespace string, doc map[string]interface{}) {
	snap := snapshotDocument(doc)
	if snap == nil || namespace == "" {
		return
	}
	rp.mu.Lock()
	defer rp.mu.Unlock()
	rp.pools[namespace] = append(rp.pools[namespace], snap)
	if len(rp.pools[namespace]) > rp.maxSize {
		overflow := len(rp.pools[namespace]) - rp.maxSize
		rp.pools[namespace] = rp.pools[namespace][overflow:]
	}
}

func (rp *RecordPool) Random(namespace string, rng *rand.Rand) (map[string]interface{}, bool) {
	if rp == nil || rng == nil || namespace == "" {
		return nil, false
	}
	rp.mu.RLock()
	records := rp.pools[namespace]
	if len(records) == 0 {
		rp.mu.RUnlock()
		return nil, false
	}
	var idx int
	if rp.selector != nil {
		idx = rp.selector.Index(len(records), rng)
		if idx < 0 || idx >= len(records) {
			idx = rng.Intn(len(records))
		}
	} else {
		idx = rng.Intn(len(records))
	}
	snap := snapshotDocument(records[idx])
	rp.mu.RUnlock()
	if snap == nil {
		return nil, false
	}
	return snap, true
}

func (rp *RecordPool) Len(namespace string) int {
	if rp == nil {
		return 0
	}
	rp.mu.RLock()
	defer rp.mu.RUnlock()
	return len(rp.pools[namespace])
}

func (rp *RecordPool) RemoveMatching(namespace string, filter map[string]interface{}) {
	if rp == nil || namespace == "" || len(filter) == 0 {
		return
	}
	rp.mu.Lock()
	defer rp.mu.Unlock()
	records := rp.pools[namespace]
	if len(records) == 0 {
		return
	}
	remaining := records[:0]
	for _, rec := range records {
		if !recordMatchesFilter(rec, filter) {
			remaining = append(remaining, rec)
		}
	}
	rp.pools[namespace] = remaining
}

func recordMatchesFilter(record, filter map[string]interface{}) bool {
	for k, expected := range filter {
		if strings.HasPrefix(k, "$") {
			continue
		}
		actual, ok := record[k]
		if !ok {
			return false
		}
		if !valuesEqual(actual, expected) {
			return false
		}
	}
	return true
}

func valuesEqual(a, b interface{}) bool {
	switch av := a.(type) {
	case int:
		switch bv := b.(type) {
		case int:
			return av == bv
		case int32:
			return av == int(bv)
		case int64:
			return int64(av) == bv
		case float64:
			return float64(av) == bv
		}
	case int32:
		switch bv := b.(type) {
		case int:
			return int(av) == bv
		case int32:
			return av == bv
		case int64:
			return int64(av) == bv
		case float64:
			return float64(av) == bv
		}
	case int64:
		switch bv := b.(type) {
		case int:
			return av == int64(bv)
		case int32:
			return av == int64(bv)
		case int64:
			return av == bv
		case float64:
			return float64(av) == bv
		}
	case float64:
		switch bv := b.(type) {
		case int, int32, int64:
			return av == toFloat64(bv)
		case float64:
			return av == bv
		}
	case string:
		if bv, ok := b.(string); ok {
			return av == bv
		}
	case bool:
		if bv, ok := b.(bool); ok {
			return av == bv
		}
	case bson.ObjectID:
		switch bv := b.(type) {
		case bson.ObjectID:
			return av == bv
		case string:
			return av.Hex() == bv
		}
	}
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

func toFloat64(v interface{}) float64 {
	switch t := v.(type) {
	case int:
		return float64(t)
	case int32:
		return float64(t)
	case int64:
		return float64(t)
	case float64:
		return t
	default:
		return 0
	}
}

func bootstrapRecordPool(ctx context.Context, db *mongo.Database, collections []config.CollectionDefinition, pool *RecordPool, sampleSize int) {
	if pool == nil || db == nil || sampleSize <= 0 {
		return
	}
	for _, col := range collections {
		namespace := collectionNamespace(db, col)
		coll := getCollectionHandle(db, col)
		sampleCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		cursor, err := coll.Find(sampleCtx, bson.M{}, options.Find().SetLimit(int64(sampleSize)))
		cancel()
		if err != nil {
			continue
		}
		for cursor.Next(ctx) {
			var doc bson.M
			if err := cursor.Decode(&doc); err != nil {
				continue
			}
			pool.Add(namespace, map[string]interface{}(doc))
		}
		_ = cursor.Close(ctx)
	}
}
