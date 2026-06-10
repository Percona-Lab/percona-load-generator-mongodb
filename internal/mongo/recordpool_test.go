package mongo

import (
	"math/rand"
	"testing"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
)

func TestRecordPoolAddRandomRemove(t *testing.T) {
	pool := NewRecordPool(10)
	ns := "shop.orders"
	pool.Add(ns, map[string]interface{}{"order_id": int64(42), "status": "open"})
	pool.Add(ns, map[string]interface{}{"order_id": int64(99), "status": "closed"})

	if pool.Len(ns) != 2 {
		t.Fatalf("expected 2 records, got %d", pool.Len(ns))
	}

	rng := rand.New(rand.NewSource(1))
	rec, ok := pool.Random(ns, rng)
	if !ok {
		t.Fatalf("expected random record")
	}
	if rec["order_id"] != int64(42) && rec["order_id"] != int64(99) {
		t.Fatalf("unexpected record: %+v", rec)
	}

	pool.RemoveMatching(ns, map[string]interface{}{"order_id": int64(42)})
	if pool.Len(ns) != 1 {
		t.Fatalf("expected 1 record after delete, got %d", pool.Len(ns))
	}
	rec, ok = pool.Random(ns, rng)
	if !ok || rec["order_id"] != int64(99) {
		t.Fatalf("expected remaining record 99, got %+v ok=%v", rec, ok)
	}
}

func TestRecordPoolRespectsMaxSize(t *testing.T) {
	pool := NewRecordPool(2)
	ns := "shop.orders"
	pool.Add(ns, map[string]interface{}{"n": 1})
	pool.Add(ns, map[string]interface{}{"n": 2})
	pool.Add(ns, map[string]interface{}{"n": 3})
	if pool.Len(ns) != 2 {
		t.Fatalf("expected pool capped at 2, got %d", pool.Len(ns))
	}
}

func TestProcessFilterPlaceholdersUsesExistingRecord(t *testing.T) {
	pool := NewRecordPool(100)
	col := config.CollectionDefinition{DatabaseName: "shop", Name: "orders"}
	ns := collectionNamespace(nil, col)
	pool.Add(ns, map[string]interface{}{
		"order_id": int64(12345),
		"status":   "shipped",
	})

	filter := map[string]interface{}{
		"order_id": "<int>",
		"status":   "<string>",
	}
	rng := rand.New(rand.NewSource(7))
	processRecursiveWithPool(filter, rng, pool, ns, 100)

	if filter["order_id"] != int64(12345) {
		t.Fatalf("expected existing order_id, got %#v", filter["order_id"])
	}
	if filter["status"] != "shipped" {
		t.Fatalf("expected existing status, got %#v", filter["status"])
	}
}

func TestProcessFilterPlaceholdersFallsBackWhenPoolEmpty(t *testing.T) {
	filter := map[string]interface{}{"order_id": "<int>"}
	rng := rand.New(rand.NewSource(3))
	processRecursiveWithPool(filter, rng, NewRecordPool(10), "shop.orders", 100)
	if _, ok := filter["order_id"].(int); !ok {
		t.Fatalf("expected random int fallback, got %#v", filter["order_id"])
	}
}

func TestProcessFilterPlaceholdersAllowsNonExistingWhenHitRateZero(t *testing.T) {
	pool := NewRecordPool(10)
	ns := "shop.orders"
	pool.Add(ns, map[string]interface{}{"order_id": int64(1)})

	filter := map[string]interface{}{"order_id": "<int>"}
	rng := rand.New(rand.NewSource(4))
	processRecursiveWithPool(filter, rng, pool, ns, 0)
	if filter["order_id"] == int64(1) {
		t.Fatalf("expected random value when hit rate is 0, got existing record id")
	}
}

func TestMixedWorkloadPlaceholderTargeting(t *testing.T) {
	pool := NewRecordPool(1000)
	col := config.CollectionDefinition{DatabaseName: "app", Name: "users"}
	ns := collectionNamespace(nil, col)

	for i := 0; i < 100; i++ {
		pool.Add(ns, map[string]interface{}{
			"user_id": int64(i),
			"email":   "user@example.com",
		})
	}

	percentages := map[string]int{
		"find":   60,
		"insert": 20,
		"delete": 10,
		"update": 10,
	}
	rng := rand.New(rand.NewSource(99))
	hits := 0
	attempts := 0
	for i := 0; i < 200; i++ {
		op := selectOperation(percentages, rng)
		switch op {
		case "find", "updateOne", "updateMany", "deleteOne", "deleteMany":
			filter := map[string]interface{}{"user_id": "<int>"}
			processRecursiveWithPool(filter, rng, pool, ns, 100)
			attempts++
			switch v := filter["user_id"].(type) {
			case int64:
				if v >= 0 && v < 100 {
					hits++
				}
			case int:
				if v >= 0 && v < 100 {
					hits++
				}
			default:
				t.Fatalf("unexpected user_id type %#v", filter["user_id"])
			}
		}
	}
	if attempts == 0 {
		t.Fatalf("expected non-insert operations in mixed simulation")
	}
	if hits != attempts {
		t.Fatalf("expected all targeted operations to use existing records at hit rate 100, got %d/%d", hits, attempts)
	}
}

func TestRegisterInsertedDocumentAndDeleteRemoval(t *testing.T) {
	pool := NewRecordPool(100)
	col := config.CollectionDefinition{DatabaseName: "shop", Name: "orders"}
	doc := map[string]interface{}{"order_id": int64(555), "status": "new"}
	registerInsertedDocument(pool, nil, col, doc)

	ns := collectionNamespace(nil, col)
	if pool.Len(ns) != 1 {
		t.Fatalf("expected inserted doc in pool")
	}

	filter := map[string]interface{}{"order_id": int64(555)}
	pool.RemoveMatching(ns, filter)
	if pool.Len(ns) != 0 {
		t.Fatalf("expected deleted record removed from pool, got len=%d", pool.Len(ns))
	}
}

func TestLookupFieldNestedPath(t *testing.T) {
	record := map[string]interface{}{
		"customer": map[string]interface{}{
			"address": map[string]interface{}{
				"city": "Denver",
			},
		},
	}
	val, ok := lookupField(record, "customer.address.city")
	if !ok || val != "Denver" {
		t.Fatalf("expected nested lookup, got %#v ok=%v", val, ok)
	}
}
