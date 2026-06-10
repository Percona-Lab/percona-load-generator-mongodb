package mongo

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"strings"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

type placeholderOptions struct {
	rng            *rand.Rand
	pool           *RecordPool
	namespace      string
	hitRate        int
	existingRecord map[string]interface{}
	useExisting    bool
}

func newPlaceholderOptions(rng *rand.Rand, pool *RecordPool, namespace string, hitRate int) *placeholderOptions {
	po := &placeholderOptions{
		rng:       rng,
		pool:      pool,
		namespace: namespace,
		hitRate:   hitRate,
	}
	if pool == nil || hitRate <= 0 || rng == nil || namespace == "" {
		return po
	}
	if rng.Intn(100) >= hitRate {
		return po
	}
	if rec, ok := pool.Random(namespace, rng); ok {
		po.useExisting = true
		po.existingRecord = rec
	}
	return po
}

func isDatatypePlaceholder(s string) bool {
	switch s {
	case "<int>", "<string>", "<boolean>", "<bool>", "<array>", "<object>", "<null>":
		return true
	default:
		return strings.HasPrefix(s, "<") && strings.HasSuffix(s, ">")
	}
}

func randomPlaceholder(placeholder string, rng *rand.Rand) interface{} {
	switch placeholder {
	case "<int>":
		return rng.Intn(1000)
	case "<string>":
		return fmt.Sprintf("val-%d", rng.Intn(1000))
	case "<boolean>", "<bool>":
		return rng.Intn(2) == 0
	case "<null>":
		return nil
	case "<array>":
		return []interface{}{rng.Intn(1000)}
	case "<object>":
		return map[string]interface{}{"k": rng.Intn(1000)}
	default:
		if strings.HasPrefix(placeholder, "<") && strings.HasSuffix(placeholder, ">") {
			return fmt.Sprintf("val-%d", rng.Intn(1000))
		}
		return placeholder
	}
}

func placeholderCompatible(placeholder string, val interface{}) bool {
	switch placeholder {
	case "<int>":
		switch val.(type) {
		case int, int32, int64, float64:
			return true
		}
	case "<string>":
		_, ok := val.(string)
		return ok
	case "<boolean>", "<bool>":
		_, ok := val.(bool)
		return ok
	case "<null>":
		return val == nil
	case "<array>":
		_, ok := val.([]interface{})
		return ok
	case "<object>":
		switch val.(type) {
		case map[string]interface{}, map[string]int:
			return true
		}
	}
	return false
}

func lookupField(record map[string]interface{}, fieldPath string) (interface{}, bool) {
	if record == nil || fieldPath == "" {
		return nil, false
	}
	parts := strings.Split(fieldPath, ".")
	var current interface{} = record
	for _, part := range parts {
		asMap, ok := current.(map[string]interface{})
		if !ok {
			return nil, false
		}
		val, ok := asMap[part]
		if !ok {
			return nil, false
		}
		current = val
	}
	return current, true
}

func (po *placeholderOptions) replacePlaceholder(fieldPath, placeholder string) interface{} {
	if po.useExisting && po.existingRecord != nil {
		if val, ok := lookupField(po.existingRecord, fieldPath); ok && placeholderCompatible(placeholder, val) {
			return val
		}
		if val, ok := po.existingRecord[fieldPath]; ok && placeholderCompatible(placeholder, val) {
			return val
		}
	}
	return randomPlaceholder(placeholder, po.rng)
}

func processFilterPlaceholders(v interface{}, po *placeholderOptions, fieldPath string) {
	if po == nil {
		return
	}
	switch t := v.(type) {
	case map[string]interface{}:
		for k, val := range t {
			path := k
			if fieldPath != "" {
				path = fieldPath + "." + k
			}
			if s, ok := val.(string); ok && isDatatypePlaceholder(s) {
				t[k] = po.replacePlaceholder(path, s)
				continue
			}
			if strings.HasPrefix(k, "$") {
				processFilterPlaceholders(val, po, fieldPath)
				continue
			}
			processFilterPlaceholders(val, po, path)
		}
	case []interface{}:
		for _, val := range t {
			processFilterPlaceholders(val, po, fieldPath)
		}
	}
}

// processRecursiveWithPool resolves placeholder values in-place and reports
// whether an existing sampled record was used to fill them (targeting quality).
func processRecursiveWithPool(v interface{}, rng *rand.Rand, pool *RecordPool, namespace string, hitRate int) bool {
	po := newPlaceholderOptions(rng, pool, namespace, hitRate)
	processFilterPlaceholders(v, po, "")
	return po.useExisting
}

func processRecursive(v interface{}, rng *rand.Rand) {
	processRecursiveWithPool(v, rng, nil, "", 0)
}

func registerInsertedDocument(pool *RecordPool, db *mongo.Database, col config.CollectionDefinition, doc map[string]interface{}) {
	if pool == nil || doc == nil {
		return
	}
	pool.Add(collectionNamespace(db, col), doc)
}

func registerInsertedDocuments(pool *RecordPool, db *mongo.Database, col config.CollectionDefinition, docs []interface{}) {
	if pool == nil {
		return
	}
	namespace := collectionNamespace(db, col)
	for _, raw := range docs {
		if doc, ok := raw.(map[string]interface{}); ok {
			pool.Add(namespace, doc)
		}
	}
}

func registerFastBatchIDs(pool *RecordPool, db *mongo.Database, col config.CollectionDefinition, patchKey string, cmdRaw []byte, offsets []int) {
	if pool == nil || patchKey == "" {
		return
	}
	namespace := collectionNamespace(db, col)
	for _, offset := range offsets {
		if offset < 0 || offset+8 > len(cmdRaw) {
			continue
		}
		id := int64(binary.LittleEndian.Uint64(cmdRaw[offset:]))
		pool.Add(namespace, map[string]interface{}{patchKey: id})
	}
}
