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
)

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
