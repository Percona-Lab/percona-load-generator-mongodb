package mongo

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/datagen"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func parseIndexes(t *testing.T, indexesJSON string) []config.IndexDefinition {
	t.Helper()
	cfg, err := config.ParseCollectionsBytes([]byte(`[{"database":"d","collection":"c","fields":{},"indexes":` + indexesJSON + `}]`))
	if err != nil {
		t.Fatal(err)
	}
	if err := config.ValidateCollectionDefinitions(cfg.Collections); err != nil {
		t.Fatal(err)
	}
	return cfg.Collections[0].Indexes
}

func resolvedIndexOptions(t *testing.T, b *options.IndexOptionsBuilder) options.IndexOptions {
	t.Helper()
	var o options.IndexOptions
	for _, set := range b.List() {
		if err := set(&o); err != nil {
			t.Fatal(err)
		}
	}
	return o
}

func TestBuildIndexModelsDeterministicKeysAndNames(t *testing.T) {
	const indexes = `[{"keys":{"origin":1,"destination":1,"flight_date":-1}},{"keys":{"b":1,"a":1}},{"keys":{"a":1,"b":1}}]`
	wantKeys := []bson.D{
		{{Key: "origin", Value: int32(1)}, {Key: "destination", Value: int32(1)}, {Key: "flight_date", Value: int32(-1)}},
		{{Key: "b", Value: int32(1)}, {Key: "a", Value: int32(1)}},
		{{Key: "a", Value: int32(1)}, {Key: "b", Value: int32(1)}},
	}
	wantNames := []string{"origin_1_destination_1_flight_date_-1", "b_1_a_1", "a_1_b_1"}
	for run := 0; run < 50; run++ {
		models, err := buildIndexModels(parseIndexes(t, indexes))
		if err != nil {
			t.Fatal(err)
		}
		for i, m := range models {
			if !reflect.DeepEqual(m.Keys, wantKeys[i]) {
				t.Fatalf("run %d index %d keys = %v, want %v", run, i, m.Keys, wantKeys[i])
			}
			raw, err := bson.Marshal(m.Keys)
			if err != nil {
				t.Fatal(err)
			}
			elems, _ := bson.Raw(raw).Elements()
			for j, e := range elems {
				if e.Key() != wantKeys[i][j].Key {
					t.Fatalf("encoded BSON key %d = %q, want %q", j, e.Key(), wantKeys[i][j].Key)
				}
			}
			if o := resolvedIndexOptions(t, m.Options); o.Name == nil || *o.Name != wantNames[i] {
				t.Fatalf("run %d index %d name = %v, want %s", run, i, o.Name, wantNames[i])
			}
		}
	}
}

func TestBuildIndexModelsPassesSupportedOptions(t *testing.T) {
	models, err := buildIndexModels(parseIndexes(t, `[
		{"keys":{"status":1,"created":-1},"name":"by_status","unique":true,"sparse":true,"hidden":true},
		{"keys":{"created":1},"expireAfterSeconds":60},
		{"keys":{"sku":1},"partialFilterExpression":{"qty":{"$gt":5},"active":true}}]`))
	if err != nil {
		t.Fatal(err)
	}
	a := resolvedIndexOptions(t, models[0].Options)
	if *a.Name != "by_status" || a.Unique == nil || !*a.Unique || a.Sparse == nil || !*a.Sparse || a.Hidden == nil || !*a.Hidden {
		t.Fatalf("options = %+v", a)
	}
	b := resolvedIndexOptions(t, models[1].Options)
	if b.ExpireAfterSeconds == nil || *b.ExpireAfterSeconds != 60 || b.Unique != nil {
		t.Fatalf("ttl options = %+v", b)
	}
	c := resolvedIndexOptions(t, models[2].Options)
	pfe, ok := c.PartialFilterExpression.(bson.D)
	if !ok || len(pfe) != 2 || pfe[0].Key != "qty" || pfe[1].Key != "active" {
		t.Fatalf("partial filter = %#v", c.PartialFilterExpression)
	}
}

func TestFindReorderedIndexes(t *testing.T) {
	configured := parseIndexes(t, `[{"keys":{"a":1,"b":1}},{"keys":{"x":1,"y":-1,"z":1}},{"keys":{"p":1,"q":1}},{"keys":{"q":1,"p":1}}]`)
	existing := []existingIndex{
		{Name: "_id_", Key: bson.D{{Key: "_id", Value: int32(1)}}},
		{Name: "a_1_b_1", Key: bson.D{{Key: "a", Value: int32(1)}, {Key: "b", Value: int32(1)}}},
		{Name: "b_1_a_1", Key: bson.D{{Key: "b", Value: int32(1)}, {Key: "a", Value: float64(1)}}},
		{Name: "z_1_x_1_y_-1", Key: bson.D{{Key: "z", Value: int64(1)}, {Key: "x", Value: int32(1)}, {Key: "y", Value: int32(-1)}}},
		{Name: "b_-1_a_1", Key: bson.D{{Key: "b", Value: int32(-1)}, {Key: "a", Value: int32(1)}}},
		{Name: "p_1_q_1", Key: bson.D{{Key: "p", Value: int32(1)}, {Key: "q", Value: int32(1)}}},
		{Name: "q_1_p_1", Key: bson.D{{Key: "q", Value: int32(1)}, {Key: "p", Value: int32(1)}}},
		{Name: "user_c_1_d_1", Key: bson.D{{Key: "c", Value: int32(1)}, {Key: "d", Value: int32(1)}}},
	}
	got := findReorderedIndexes(configured, existing)
	want := []reorderedIndex{
		{Existing: "b_1_a_1", Configured: "a_1_b_1"},
		{Existing: "z_1_x_1_y_-1", Configured: "x_1_y_-1_z_1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reordered = %+v, want %+v", got, want)
	}
}

func TestInsertRandomDocumentsBatching(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		count, batchSize, workers int
		wantSizes                 []int
	}{
		{"empty", 0, 3, 4, nil},
		{"negative_count", -1, 3, 4, nil},
		{"partial_batch", 2, 3, 8, []int{2}},
		{"exact_batches", 12, 3, 4, []int{3, 3, 3, 3}},
		{"remainder", 11, 3, 4, []int{2, 3, 3, 3}},
		{"serial_default", 5, 2, 0, []int{1, 2, 2}},
		{"invalid_settings", 1001, -1, -1, []int{1, 1000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var sizes []int
			err := insertRandomDocuments(context.Background(), config.CollectionDefinition{}, tc.count,
				&config.AppConfig{SeedBatchSize: tc.batchSize, SeedConcurrency: tc.workers},
				func(_ context.Context, batch []interface{}) error {
					mu.Lock()
					defer mu.Unlock()
					sizes = append(sizes, len(batch))
					return nil
				})
			if err != nil {
				t.Fatal(err)
			}
			sort.Ints(sizes)
			if !reflect.DeepEqual(sizes, tc.wantSizes) {
				t.Fatalf("batch sizes = %v, want %v", sizes, tc.wantSizes)
			}
		})
	}
}

func TestInsertRandomDocumentsConcurrentWriters(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var active, peak atomic.Int32
	ready := make(chan struct{})
	err := insertRandomDocuments(ctx, config.CollectionDefinition{}, 40,
		&config.AppConfig{SeedBatchSize: 2, SeedConcurrency: 4},
		func(ctx context.Context, batch []interface{}) error {
			n := active.Add(1)
			defer active.Add(-1)
			for old := peak.Load(); n > old; old = peak.Load() {
				if peak.CompareAndSwap(old, n) {
					if n == 4 {
						close(ready)
					}
					break
				}
			}
			select {
			case <-ready:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	if err != nil {
		t.Fatal(err)
	}
	if peak.Load() != 4 || active.Load() != 0 {
		t.Fatalf("peak/remaining writers = %d/%d, want 4/0", peak.Load(), active.Load())
	}
}

func TestInsertRandomDocumentsPreservesGeneratedData(t *testing.T) {
	defer datagen.ConfigureSeed(0)
	// One seeded field avoids unrelated map iteration / wall-clock generation.
	col := config.CollectionDefinition{Fields: map[string]config.CollectionField{
		"value": {Type: "string", Template: "????????????????"},
	}}
	collect := func(workers int) []string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var mu sync.Mutex
		var values []string
		var entered atomic.Int32
		ready := make(chan struct{})
		err := insertRandomDocuments(ctx, col, 103,
			&config.AppConfig{SeedBatchSize: 7, SeedConcurrency: workers, RandomSeed: 42},
			func(ctx context.Context, batch []interface{}) error {
				// Keep early batches in use while the producer builds later ones.
				if entered.Add(1) == int32(workers) {
					close(ready)
				}
				select {
				case <-ready:
				case <-ctx.Done():
					return ctx.Err()
				}
				mu.Lock()
				defer mu.Unlock()
				for _, doc := range batch {
					values = append(values, doc.(map[string]interface{})["value"].(string))
				}
				return nil
			})
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(values)
		return values
	}
	serial, parallel := collect(1), collect(4)
	if len(parallel) != 103 || !reflect.DeepEqual(serial, parallel) {
		t.Fatal("parallel seeding lost, duplicated, or changed generated documents")
	}
}

func TestInsertRandomDocumentsFailureCancelsAndJoinsWriters(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wantErr := errors.New("insert failed")
	var calls, active atomic.Int32
	allStarted := make(chan struct{})
	err := insertRandomDocuments(ctx, config.CollectionDefinition{DatabaseName: "db", Name: "docs"}, 100,
		&config.AppConfig{SeedBatchSize: 1, SeedConcurrency: 3},
		func(ctx context.Context, _ []interface{}) error {
			active.Add(1)
			defer active.Add(-1)
			n := calls.Add(1)
			if n == 3 {
				close(allStarted)
			}
			if n == 1 {
				select {
				case <-allStarted:
					return wantErr
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			<-ctx.Done()
			return ctx.Err()
		})
	if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "db.docs") {
		t.Fatalf("error = %v, want namespaced insert failure", err)
	}
	if calls.Load() != 3 || active.Load() != 0 {
		t.Fatalf("calls/remaining writers = %d/%d, want 3/0", calls.Load(), active.Load())
	}
}

func TestInsertRandomDocumentsCancellation(t *testing.T) {
	for _, preCanceled := range []bool{false, true} {
		t.Run(fmt.Sprintf("preCanceled=%t", preCanceled), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if preCanceled {
				cancel()
			}
			var calls, active atomic.Int32
			err := insertRandomDocuments(ctx, config.CollectionDefinition{}, 100, &config.AppConfig{SeedBatchSize: 1, SeedConcurrency: 4},
				func(ctx context.Context, _ []interface{}) error {
					calls.Add(1)
					active.Add(1)
					defer active.Add(-1)
					cancel()
					<-ctx.Done()
					return ctx.Err()
				})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want cancellation", err)
			}
			if active.Load() != 0 {
				t.Fatal("writers still running after return")
			}
			if preCanceled && calls.Load() != 0 {
				t.Fatal("insert called after cancellation")
			}
		})
	}
}

func TestInsertRandomDocumentsDefaultConfig(t *testing.T) {
	calls := 0
	err := insertRandomDocuments(context.Background(), config.CollectionDefinition{}, 1, nil,
		func(_ context.Context, batch []interface{}) error { calls++; return nil })
	if err != nil || calls != 1 {
		t.Fatalf("error/calls = %v/%d, want nil/1", err, calls)
	}
}
