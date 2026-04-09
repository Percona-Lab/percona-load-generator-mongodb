package mongo

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math/rand"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

type mongoSeams struct {
	ensureGenericSharding func(ctx context.Context, db *mongo.Database, col config.CollectionDefinition, cfg *config.AppConfig) error
	runAllQueriesOnce     func(ctx context.Context, db *mongo.Database, queries []config.QueryDefinition, debug bool) error
	runContinuousWorkload func(ctx context.Context, wCfg workloadConfig) error
	getPrimaryFilterField func(ctx context.Context, db *mongo.Database, col config.CollectionDefinition) string
	executeQueryOnce      func(ctx context.Context, database *mongo.Database, q config.QueryDefinition, filter map[string]interface{})
}

func withMongoSeams(t *testing.T, s mongoSeams) {
	t.Helper()
	origEnsure := ensureGenericShardingFn
	origRunOnce := runAllQueriesOnceFn
	origRunContinuous := runContinuousWorkloadFn
	origPrimary := getPrimaryFilterFieldFn
	origExec := executeQueryOnceFn

	if s.ensureGenericSharding != nil {
		ensureGenericShardingFn = s.ensureGenericSharding
	}
	if s.runAllQueriesOnce != nil {
		runAllQueriesOnceFn = s.runAllQueriesOnce
	}
	if s.runContinuousWorkload != nil {
		runContinuousWorkloadFn = s.runContinuousWorkload
	}
	if s.getPrimaryFilterField != nil {
		getPrimaryFilterFieldFn = s.getPrimaryFilterField
	}
	if s.executeQueryOnce != nil {
		executeQueryOnceFn = s.executeQueryOnce
	}

	t.Cleanup(func() {
		ensureGenericShardingFn = origEnsure
		runAllQueriesOnceFn = origRunOnce
		runContinuousWorkloadFn = origRunContinuous
		getPrimaryFilterFieldFn = origPrimary
		executeQueryOnceFn = origExec
	})
}

func TestBuildRawInt64Array(t *testing.T) {
	ids := []int64{5, 9, 42}
	raw := buildRawInt64Array(ids)
	if len(raw) < 5 {
		t.Fatalf("raw array too short")
	}
	declaredLen := int(binary.LittleEndian.Uint32(raw[:4]))
	if declaredLen != len(raw) {
		t.Fatalf("declared len %d != actual len %d", declaredLen, len(raw))
	}

	var decoded map[string]int64
	if err := bson.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal raw array doc: %v", err)
	}
	if decoded["0"] != 5 || decoded["1"] != 9 || decoded["2"] != 42 {
		t.Fatalf("unexpected decoded values: %+v", decoded)
	}
}

func TestBuildRawBatch(t *testing.T) {
	template := []byte{0x01, 0x02, 0x03, 0x04}
	magic := []byte{0x02, 0x03}

	raw, offsets, err := buildRawBatch(template, 3, magic)
	if err != nil {
		t.Fatalf("buildRawBatch() error = %v", err)
	}
	if len(offsets) != 3 {
		t.Fatalf("expected 3 offsets, got %d", len(offsets))
	}
	for _, off := range offsets {
		if off < 0 || off+len(magic) > len(raw) {
			t.Fatalf("invalid offset %d for raw length %d", off, len(raw))
		}
		if !bytes.Equal(raw[off:off+len(magic)], magic) {
			t.Fatalf("offset %d does not point to magic bytes", off)
		}
	}

	_, _, err = buildRawBatch(template, 2, []byte{0xAA})
	if err == nil {
		t.Fatalf("expected error when magic marker is missing")
	}

	raw, offsets, err = buildRawBatch(template, 0, magic)
	if err != nil {
		t.Fatalf("buildRawBatch(count=0) error = %v", err)
	}
	if len(offsets) != 0 {
		t.Fatalf("expected empty offsets, got %d", len(offsets))
	}
	if len(raw) == 0 {
		t.Fatalf("expected valid BSON array bytes for empty batch")
	}
}

func TestIdentifyPatchKey(t *testing.T) {
	tests := []struct {
		name string
		col  config.CollectionDefinition
		want string
	}{
		{
			name: "shard_int_key_preferred",
			col: config.CollectionDefinition{
				Name: "c1",
				Fields: map[string]config.CollectionField{
					"user_id": {Type: "int"},
				},
				ShardConfig: &config.ShardConfig{Key: map[string]interface{}{"user_id": 1}},
			},
			want: "user_id",
		},
		{
			name: "id_fallback_for_int_like_id",
			col: config.CollectionDefinition{
				Fields: map[string]config.CollectionField{"_id": {Type: "long"}},
			},
			want: "_id",
		},
		{
			name: "heuristic_match_for_id_substring",
			col: config.CollectionDefinition{
				Fields: map[string]config.CollectionField{"accountId": {Type: "int"}},
			},
			want: "accountId",
		},
		{
			name: "shard_key_fallback_without_int_type",
			col: config.CollectionDefinition{
				Fields:      map[string]config.CollectionField{"name": {Type: "string"}},
				ShardConfig: &config.ShardConfig{Key: map[string]interface{}{"name": 1}},
			},
			want: "name",
		},
		{
			name: "no_candidate_returns_empty_string",
			col: config.CollectionDefinition{
				Fields: map[string]config.CollectionField{"name": {Type: "string"}},
			},
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := identifyPatchKey(tc.col); got != tc.want {
				t.Fatalf("identifyPatchKey() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSelectOperation(t *testing.T) {
	rng := rand.New(rand.NewSource(11))

	if got := selectOperation(nil, rng); got != "find" {
		t.Fatalf("expected fallback find for nil map, got %q", got)
	}

	findOnly := map[string]int{"find": 100}
	for i := 0; i < 10; i++ {
		if got := selectOperation(findOnly, rng); got != "find" {
			t.Fatalf("expected find only, got %q", got)
		}
	}

	updateOnly := map[string]int{"update": 100}
	for i := 0; i < 10; i++ {
		if got := selectOperation(updateOnly, rng); !strings.HasPrefix(got, "update") {
			t.Fatalf("expected update variant, got %q", got)
		}
	}

	deleteOnly := map[string]int{"delete": 100}
	for i := 0; i < 10; i++ {
		if got := selectOperation(deleteOnly, rng); !strings.HasPrefix(got, "delete") {
			t.Fatalf("expected delete variant, got %q", got)
		}
	}
}

func TestCloneMapAndProcessRecursive(t *testing.T) {
	orig := map[string]interface{}{
		"a": "<int>",
		"nested": map[string]interface{}{
			"b": "<string>",
			"c": []interface{}{map[string]interface{}{"d": "<int>"}},
		},
	}
	clone := cloneMap(orig)
	if clone == nil {
		t.Fatalf("expected non-nil clone")
	}

	rng := rand.New(rand.NewSource(21))
	processRecursive(clone, rng)

	if clone["a"] == "<int>" {
		t.Fatalf("expected int placeholder replacement")
	}
	nested, _ := clone["nested"].(map[string]interface{})
	if nested["b"] == "<string>" {
		t.Fatalf("expected string placeholder replacement")
	}

	// original should remain unchanged
	if orig["a"] != "<int>" {
		t.Fatalf("expected original map to remain unchanged")
	}
}

func TestGenerateFallbackQuery(t *testing.T) {
	col := config.CollectionDefinition{
		Name: "flights",
		Fields: map[string]config.CollectionField{
			"flight_id": {Type: "int"},
		},
	}
	rng := rand.New(rand.NewSource(5))
	cfg := &config.AppConfig{}

	q, ok := generateFallbackQuery(nil, nil, "updateOne", col, rng, "flight_id", cfg)
	if !ok || q.Operation != "updateOne" || q.Filter["flight_id"] != "<int>" {
		t.Fatalf("unexpected update fallback query: %+v, ok=%v", q, ok)
	}
	if q.Update == nil {
		t.Fatalf("expected update payload for update operation")
	}

	q, ok = generateFallbackQuery(nil, nil, "deleteMany", col, rng, "_id", cfg)
	if !ok || q.Filter["_id"] != "<string>" {
		t.Fatalf("unexpected delete fallback query: %+v, ok=%v", q, ok)
	}

	_, ok = generateFallbackQuery(nil, nil, "aggregate", col, rng, "flight_id", cfg)
	if ok {
		t.Fatalf("expected unsupported op to return ok=false")
	}
}

func TestSelectRandomQueryByType(t *testing.T) {
	rng := rand.New(rand.NewSource(13))
	col := config.CollectionDefinition{
		DatabaseName: "appdb",
		Name:         "users",
		Fields: map[string]config.CollectionField{
			"user_id": {Type: "int"},
		},
	}
	cfg := &config.AppConfig{}

	queryMap := map[string][]config.QueryDefinition{
		"find": {
			{Database: "appdb", Collection: "users", Operation: "find", Filter: map[string]interface{}{"a": 1}},
			{Database: "otherdb", Collection: "users", Operation: "find", Filter: map[string]interface{}{"a": 2}},
			{Database: "appdb", Collection: "orders", Operation: "find", Filter: map[string]interface{}{"a": 3}},
		},
	}

	got, ok := selectRandomQueryByType(nil, nil, "find", queryMap, col, false, rng, "user_id", cfg)
	if !ok || got.Operation != "find" {
		t.Fatalf("expected a configured find query, got %+v ok=%v", got, ok)
	}
	if !reflect.DeepEqual(got.Filter, map[string]interface{}{"a": 1}) {
		t.Fatalf("expected query matched by db+collection, got %+v", got)
	}

	fallback, ok := selectRandomQueryByType(nil, nil, "updateOne", map[string][]config.QueryDefinition{}, col, false, rng, "user_id", cfg)
	if !ok || fallback.Operation != "updateOne" {
		t.Fatalf("expected updateOne fallback query, got %+v ok=%v", fallback, ok)
	}
	if fallback.Filter["user_id"] != "<int>" {
		t.Fatalf("unexpected fallback filter: %+v", fallback.Filter)
	}

	_, ok = selectRandomQueryByType(nil, nil, "insertMany", map[string][]config.QueryDefinition{}, col, false, rng, "user_id", cfg)
	if ok {
		t.Fatalf("expected no fallback for insertMany with empty query map")
	}
}

func TestGenerateInsertQueryAndInsertManyQueryFromCache(t *testing.T) {
	InsertDocumentCache = make(chan map[string]interface{}, 4)
	InsertDocumentCache <- map[string]interface{}{"_id": 1, "name": "cached-1"}
	InsertDocumentCache <- map[string]interface{}{"_id": 2, "name": "cached-2"}
	InsertDocumentCache <- map[string]interface{}{"_id": 3, "name": "cached-3"}

	col := config.CollectionDefinition{
		Name: "users",
		Fields: map[string]config.CollectionField{
			"name": {Type: "string"},
		},
	}
	cfg := &config.AppConfig{InsertBatchSize: 2}
	rng := rand.New(rand.NewSource(1))

	q := generateInsertQuery(col, rng, cfg)
	if q.Operation != "insert" || q.Collection != "users" {
		t.Fatalf("unexpected insert query metadata: %+v", q)
	}
	if q.Filter["name"] != "cached-1" {
		t.Fatalf("expected cached insert doc to be used first, got %+v", q.Filter)
	}

	docs := generateInsertManyQuery(col, rng, cfg)
	if len(docs) != 2 {
		t.Fatalf("expected 2 docs, got %d", len(docs))
	}
	doc0, ok := docs[0].(map[string]interface{})
	if !ok || doc0["name"] != "cached-2" {
		t.Fatalf("expected first insertMany doc from cache, got %#v", docs[0])
	}
	doc1, ok := docs[1].(map[string]interface{})
	if !ok || doc1["name"] != "cached-3" {
		t.Fatalf("expected second insertMany doc from cache, got %#v", docs[1])
	}
}

func TestBuildOrderedShardKey(t *testing.T) {
	m := map[string]interface{}{"z": 1, "a": 1}
	ordered := buildOrderedShardKey(config.CollectionDefinition{}, m, false)
	if len(ordered) != 2 || ordered[0].Key != "a" || ordered[1].Key != "z" {
		t.Fatalf("expected sorted keys, got %+v", ordered)
	}
	if ordered[0].Value != 1 || ordered[1].Value != 1 {
		t.Fatalf("expected range shard values to be 1")
	}

	hashed := buildOrderedShardKey(config.CollectionDefinition{}, m, true)
	if hashed[0].Value != "hashed" || hashed[1].Value != "hashed" {
		t.Fatalf("expected hashed values, got %+v", hashed)
	}
}

func TestResolveShardKeyDoc(t *testing.T) {
	t.Run("defaults_to_patch_key_when_shard_config_missing", func(t *testing.T) {
		doc, isHashed, err := resolveShardKeyDoc(config.CollectionDefinition{Name: "orders"}, "order_id")
		if err != nil {
			t.Fatalf("resolveShardKeyDoc() error = %v", err)
		}
		if isHashed {
			t.Fatalf("expected range shard key by default")
		}
		if len(doc) != 1 || doc[0].Key != "order_id" || doc[0].Value != 1 {
			t.Fatalf("unexpected default shard key doc: %+v", doc)
		}
	})

	t.Run("supports_hashed_shard_config", func(t *testing.T) {
		col := config.CollectionDefinition{
			Name: "orders",
			ShardConfig: &config.ShardConfig{
				Key: map[string]interface{}{"order_id": "hashed"},
			},
		}
		doc, isHashed, err := resolveShardKeyDoc(col, "order_id")
		if err != nil {
			t.Fatalf("resolveShardKeyDoc() error = %v", err)
		}
		if !isHashed {
			t.Fatalf("expected hashed flag")
		}
		if len(doc) != 1 || doc[0].Key != "order_id" || doc[0].Value != "hashed" {
			t.Fatalf("unexpected hashed shard key doc: %+v", doc)
		}
	})

	t.Run("errors_when_patch_key_empty", func(t *testing.T) {
		_, _, err := resolveShardKeyDoc(config.CollectionDefinition{Name: "orders"}, "")
		if err == nil {
			t.Fatalf("expected error for empty patch key")
		}
	})
}

func TestEnsureGenericShardingRejectsNilDatabase(t *testing.T) {
	err := ensureGenericSharding(context.Background(), nil, config.CollectionDefinition{Name: "orders"}, &config.AppConfig{})
	if err == nil || !strings.Contains(err.Error(), "database handle is nil") {
		t.Fatalf("expected nil db error, got %v", err)
	}
}

func TestShouldApplyGenericSharding(t *testing.T) {
	colWithConfig := config.CollectionDefinition{
		DatabaseName: "bench",
		Name:         "orders",
		ShardConfig:  &config.ShardConfig{Key: map[string]interface{}{"order_id": 1}},
	}
	colWithoutConfig := config.CollectionDefinition{
		DatabaseName: "bench",
		Name:         "events",
	}

	tests := []struct {
		name       string
		mode       string
		skipNoCfg  bool
		isMongos   bool
		col        config.CollectionDefinition
		wantApply  bool
		wantErrHas string
	}{
		{name: "force_off_always_skips", mode: "force_off", skipNoCfg: false, isMongos: true, col: colWithConfig, wantApply: false},
		{name: "auto_skips_non_mongos", mode: "auto", skipNoCfg: false, isMongos: false, col: colWithConfig, wantApply: false},
		{name: "auto_applies_on_mongos_when_config_present", mode: "auto", skipNoCfg: true, isMongos: true, col: colWithConfig, wantApply: true},
		{name: "auto_skips_when_no_config_and_skip_enabled", mode: "auto", skipNoCfg: true, isMongos: true, col: colWithoutConfig, wantApply: false},
		{name: "auto_applies_when_no_config_and_skip_disabled", mode: "auto", skipNoCfg: false, isMongos: true, col: colWithoutConfig, wantApply: true},
		{name: "force_on_requires_mongos", mode: "force_on", skipNoCfg: false, isMongos: false, col: colWithConfig, wantErrHas: "requires mongos topology"},
		{name: "force_on_conflict_with_skip_without_config", mode: "force_on", skipNoCfg: true, isMongos: true, col: colWithoutConfig, wantErrHas: "conflicts with sharding_skip_generic_without_config=true"},
		{name: "force_on_applies_on_mongos", mode: "force_on", skipNoCfg: false, isMongos: true, col: colWithConfig, wantApply: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotApply, err := shouldApplyGenericSharding(tc.mode, tc.skipNoCfg, tc.isMongos, tc.col)
			if tc.wantErrHas != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrHas) {
					t.Fatalf("expected error containing %q, got %v", tc.wantErrHas, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotApply != tc.wantApply {
				t.Fatalf("apply=%v, want %v", gotApply, tc.wantApply)
			}
		})
	}
}

func TestRunWorkloadDurationNonPositiveRunsQueriesOnce(t *testing.T) {
	ensureCalls := 0
	runOnceCalled := false
	withMongoSeams(t, mongoSeams{
		ensureGenericSharding: func(ctx context.Context, db *mongo.Database, col config.CollectionDefinition, cfg *config.AppConfig) error {
			ensureCalls++
			if col.Name == "warn_col" {
				return errors.New("simulated sharding warning")
			}
			return nil
		},
		runAllQueriesOnce: func(ctx context.Context, db *mongo.Database, queries []config.QueryDefinition, debug bool) error {
			runOnceCalled = true
			if len(queries) != 1 || queries[0].Operation != "find" {
				t.Fatalf("unexpected queries passed to runAllQueriesOnceFn: %+v", queries)
			}
			return nil
		},
		runContinuousWorkload: func(ctx context.Context, wCfg workloadConfig) error {
			t.Fatalf("runContinuousWorkloadFn should not be called when duration <= 0")
			return nil
		},
	})

	cfg := &config.AppConfig{
		Duration:  "0s",
		DebugMode: true,
	}
	cols := []config.CollectionDefinition{
		{Name: "warn_col", DatabaseName: "benchdb"},
		{Name: "ok_col", DatabaseName: "benchdb"},
	}
	queries := []config.QueryDefinition{{Name: "find_warn_col", Database: "benchdb", Collection: "warn_col", Operation: "find", Filter: map[string]interface{}{}}}

	if err := RunWorkload(context.Background(), nil, cols, queries, cfg); err != nil {
		t.Fatalf("RunWorkload() error = %v", err)
	}
	if ensureCalls != len(cols) {
		t.Fatalf("expected ensureGenericSharding called %d times, got %d", len(cols), ensureCalls)
	}
	if !runOnceCalled {
		t.Fatalf("expected runAllQueriesOnceFn to be called")
	}
}

func TestRunWorkloadForceOnTreatsShardingErrorsAsFatal(t *testing.T) {
	withMongoSeams(t, mongoSeams{
		ensureGenericSharding: func(ctx context.Context, db *mongo.Database, col config.CollectionDefinition, cfg *config.AppConfig) error {
			return errors.New("simulated force_on failure")
		},
		runAllQueriesOnce: func(ctx context.Context, db *mongo.Database, queries []config.QueryDefinition, debug bool) error {
			t.Fatalf("runAllQueriesOnce should not execute when force_on sharding setup fails")
			return nil
		},
	})

	cfg := &config.AppConfig{
		Duration:     "0s",
		ShardingMode: "force_on",
	}
	cols := []config.CollectionDefinition{{Name: "orders", DatabaseName: "bench"}}
	queries := []config.QueryDefinition{{Name: "q1", Database: "bench", Collection: "orders", Operation: "find", Filter: map[string]interface{}{}}}

	err := RunWorkload(context.Background(), nil, cols, queries, cfg)
	if err == nil || !strings.Contains(err.Error(), "sharding setup failed") {
		t.Fatalf("expected fatal sharding error for force_on, got %v", err)
	}
}

func TestRunWorkloadDurationPositiveBuildsWorkloadConfig(t *testing.T) {
	captured := workloadConfig{}
	withMongoSeams(t, mongoSeams{
		ensureGenericSharding: func(ctx context.Context, db *mongo.Database, col config.CollectionDefinition, cfg *config.AppConfig) error {
			return nil
		},
		runAllQueriesOnce: func(ctx context.Context, db *mongo.Database, queries []config.QueryDefinition, debug bool) error {
			t.Fatalf("runAllQueriesOnceFn should not be called for positive duration")
			return nil
		},
		getPrimaryFilterField: func(ctx context.Context, db *mongo.Database, col config.CollectionDefinition) string {
			return "tenant_id"
		},
		runContinuousWorkload: func(ctx context.Context, wCfg workloadConfig) error {
			captured = wCfg
			return nil
		},
	})

	cfg := &config.AppConfig{
		Duration:           "2s",
		Concurrency:        4,
		FindBatchSize:      0, // should default to 10 inside RunWorkload
		FindLimit:          0, // should default to 10 inside RunWorkload
		InsertCacheSize:    50,
		FindPercent:        40,
		UpdatePercent:      20,
		DeletePercent:      10,
		InsertPercent:      20,
		BulkInsertPercent:  5,
		AggregatePercent:   3,
		TransactionPercent: 2,
	}
	cols := []config.CollectionDefinition{{Name: "users", DatabaseName: "appdb"}}
	queries := []config.QueryDefinition{
		{Name: "find_users", Database: "appdb", Operation: "find", Collection: "users", Filter: map[string]interface{}{}},
		{Name: "update_users", Database: "appdb", Operation: "updateOne", Collection: "users", Filter: map[string]interface{}{}, Update: map[string]interface{}{"$set": map[string]interface{}{"a": 1}}},
	}

	if err := RunWorkload(context.Background(), nil, cols, queries, cfg); err != nil {
		t.Fatalf("RunWorkload() error = %v", err)
	}

	if captured.concurrency != 4 {
		t.Fatalf("expected concurrency=4, got %d", captured.concurrency)
	}
	if captured.findBatchSize != 10 || captured.findLimit != 10 {
		t.Fatalf("expected default find batch/limit to be 10/10, got %d/%d", captured.findBatchSize, captured.findLimit)
	}
	if captured.primaryFilterField != "tenant_id" {
		t.Fatalf("expected primary filter field from seam, got %q", captured.primaryFilterField)
	}
	if captured.percentages["find"] != 40 || captured.percentages["insertMany"] != 5 || captured.percentages["transaction"] != 2 {
		t.Fatalf("unexpected percentages map: %+v", captured.percentages)
	}
	if len(captured.queryMap["find"]) != 1 || len(captured.queryMap["updateOne"]) != 1 {
		t.Fatalf("expected query map to be grouped by operation, got %+v", captured.queryMap)
	}
}

func TestRunWorkloadInvalidDurationReturnsError(t *testing.T) {
	cfg := &config.AppConfig{Duration: "not-a-duration"}
	err := RunWorkload(context.Background(), nil, nil, nil, cfg)
	if err == nil {
		t.Fatalf("expected duration parse error")
	}
}

func TestRunWorkloadFailsWhenQueryReferencesUnknownCollection(t *testing.T) {
	cfg := &config.AppConfig{Duration: "0s"}
	collections := []config.CollectionDefinition{
		{DatabaseName: "db1", Name: "orders"},
	}
	queries := []config.QueryDefinition{
		{Name: "find_missing", Collection: "missing", Operation: "find", Filter: map[string]interface{}{}},
	}

	err := RunWorkload(context.Background(), nil, collections, queries, cfg)
	if err == nil || !strings.Contains(err.Error(), "unknown collection") {
		t.Fatalf("expected unknown collection validation error, got %v", err)
	}
}

func TestRunContinuousWorkloadReturnsErrorWhenWorkerPanics(t *testing.T) {
	wCfg := workloadConfig{
		appConfig: &config.AppConfig{
			StatusRefreshRateSec: 1,
			WebUI:                config.WebUIConfig{Enabled: true},
		},
		concurrency:    1,
		duration:       100 * time.Millisecond,
		collections:    nil, // triggers panic path in worker (random selection from empty slice)
		maxInsertCache: 8,
		collector:      stats.NewCollector(),
	}

	err := runContinuousWorkload(context.Background(), wCfg)
	if err == nil {
		t.Fatalf("expected panic-derived error from worker path")
	}
	if !strings.Contains(err.Error(), "panic in worker") {
		t.Fatalf("expected worker panic context in error, got %v", err)
	}
}

func TestRunAllQueriesOnceSkipsInsertOperationsAndProcessesPlaceholders(t *testing.T) {
	type observed struct {
		op     string
		filter map[string]interface{}
	}
	var seen []observed
	withMongoSeams(t, mongoSeams{
		executeQueryOnce: func(ctx context.Context, database *mongo.Database, q config.QueryDefinition, filter map[string]interface{}) {
			cloned := cloneMap(filter)
			seen = append(seen, observed{op: q.Operation, filter: cloned})
		},
	})

	queries := []config.QueryDefinition{
		{Operation: "insert", Filter: map[string]interface{}{"a": "<int>"}},
		{Operation: "insertMany", Filter: map[string]interface{}{"a": "<int>"}},
		{Operation: "find", Filter: map[string]interface{}{"a": "<int>", "b": "<string>"}},
		{Operation: "updateOne", Filter: map[string]interface{}{"nested": map[string]interface{}{"x": "<int>"}}},
	}

	if err := runAllQueriesOnce(context.Background(), nil, queries, false); err != nil {
		t.Fatalf("runAllQueriesOnce() error = %v", err)
	}

	if len(seen) != 2 {
		t.Fatalf("expected only non-insert operations to be processed, got %d", len(seen))
	}
	if seen[0].op != "find" || seen[1].op != "updateOne" {
		t.Fatalf("unexpected operation order: %+v", seen)
	}

	if _, ok := seen[0].filter["a"].(int); !ok {
		t.Fatalf("expected <int> placeholder replacement in find filter, got %#v", seen[0].filter["a"])
	}
	if s, ok := seen[0].filter["b"].(string); !ok || !strings.HasPrefix(s, "val-") {
		t.Fatalf("expected <string> placeholder replacement in find filter, got %#v", seen[0].filter["b"])
	}

	nested, ok := seen[1].filter["nested"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected nested map in updateOne filter, got %#v", seen[1].filter["nested"])
	}
	if _, ok := nested["x"].(int); !ok {
		t.Fatalf("expected nested <int> placeholder replacement, got %#v", nested["x"])
	}
}

func TestQueryWorkerOnceHandlesEmptyTaskChannel(t *testing.T) {
	calls := 0
	withMongoSeams(t, mongoSeams{
		executeQueryOnce: func(ctx context.Context, database *mongo.Database, q config.QueryDefinition, filter map[string]interface{}) {
			calls++
		},
	})

	tasks := make(chan *queryTask)
	close(tasks)

	var wg sync.WaitGroup
	wg.Add(1)
	go queryWorkerOnce(context.Background(), 1, tasks, &wg)

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("queryWorkerOnce did not exit for closed task channel")
	}

	if calls != 0 {
		t.Fatalf("expected no executeQueryOnceFn calls, got %d", calls)
	}
}

func TestRunAllQueriesOnceNoQueriesReturnsNil(t *testing.T) {
	called := false
	withMongoSeams(t, mongoSeams{
		executeQueryOnce: func(ctx context.Context, database *mongo.Database, q config.QueryDefinition, filter map[string]interface{}) {
			called = true
		},
	})

	if err := runAllQueriesOnce(context.Background(), nil, nil, false); err != nil {
		t.Fatalf("runAllQueriesOnce() error = %v", err)
	}
	if called {
		t.Fatalf("expected no execution calls for empty queries")
	}
}
