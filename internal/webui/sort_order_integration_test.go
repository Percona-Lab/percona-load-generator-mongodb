//go:build integration
// +build integration

package webui

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// findSortPattern returns the first sortPattern (find layer / SBE) or $sort
// sortKey (aggregate stages) document in an explain output, in server order.
func findSortPattern(v bson.RawValue) (bson.Raw, bool) {
	var elems []bson.RawElement
	switch v.Type {
	case bson.TypeEmbeddedDocument:
		elems, _ = v.Document().Elements()
	case bson.TypeArray:
		vals, _ := v.Array().Values()
		for _, x := range vals {
			if d, ok := findSortPattern(x); ok {
				return d, true
			}
		}
		return nil, false
	default:
		return nil, false
	}
	for _, e := range elems {
		if (e.Key() == "sortPattern" || e.Key() == "sortKey") && e.Value().Type == bson.TypeEmbeddedDocument {
			return e.Value().Document(), true
		}
	}
	for _, e := range elems {
		if d, ok := findSortPattern(e.Value()); ok {
			return d, true
		}
	}
	return nil, false
}

func rawKeyOrder(d bson.Raw) string {
	elems, _ := d.Elements()
	parts := make([]string, 0, len(elems))
	for _, e := range elems {
		n, _ := e.Value().AsInt64OK()
		parts = append(parts, fmt.Sprintf("%s:%d", e.Key(), n))
	}
	return strings.Join(parts, ",")
}

func TestExplainIntegration_CompoundSortMatchesDeclaredOrder(t *testing.T) {
	_, database := explainITDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Every (a, b) pair once: the two precedences return different orders.
	coll := database.Collection("ties")
	if _, err := coll.InsertMany(ctx, []interface{}{
		bson.D{{Key: "_id", Value: 1}, {Key: "a", Value: 1}, {Key: "b", Value: 1}},
		bson.D{{Key: "_id", Value: 2}, {Key: "a", Value: 1}, {Key: "b", Value: 2}},
		bson.D{{Key: "_id", Value: 3}, {Key: "a", Value: 2}, {Key: "b", Value: 1}},
		bson.D{{Key: "_id", Value: 4}, {Key: "a", Value: 2}, {Key: "b", Value: 2}},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	type declared struct {
		order string
		ids   []int32
	}
	want := map[string]declared{
		"a_then_b": {"a:1,b:-1", []int32{2, 1, 4, 3}},
		"b_then_a": {"b:-1,a:1", []int32{2, 4, 1, 3}},
	}
	qf, err := config.ParseQueriesBytes([]byte(`{"queries":[
		{"name":"a_then_b","collection":"ties","operation":"aggregate","pipeline":[{"$sort":{"a":1,"b":-1}},{"$project":{"_id":1}}]},
		{"name":"b_then_a","collection":"ties","operation":"aggregate","pipeline":[{"$sort":{"b":-1,"a":1}},{"$project":{"_id":1}}]}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	collections := []config.CollectionDefinition{{
		DatabaseName: database.Name(), Name: "ties",
		Fields: map[string]config.CollectionField{"_id": {Type: "int"}, "a": {Type: "int"}, "b": {Type: "int"}},
	}}
	cfg := &config.AppConfig{
		Duration: "1200ms", StatusRefreshRateSec: 1, Concurrency: 2, AggregatePercent: 100, FindBatchSize: 10, InsertBatchSize: 5,
		InsightsEnabled: true, InsightsSamplingRate: 1, InsightsMaxEvents: 200, InsightsMaxGroups: 50, InsightsSlowThresholdMs: 60000,
		InsightsExplainEnabled: true, InsightsExplainTopN: 2, InsightsExplainSeverityMode: "high_only",
		InsightsExplainEfficiencyMode: config.ExplainEfficiencyModeDefault, InsightsExplainWorkers: 2, InsightsExplainMaxTimeMS: 5000,
	}

	runOnce := func() map[string]string {
		collector := stats.NewCollector()
		collector.ConfigureInsights(cfg)
		if err := runWorkloadFn(ctx, database, collections, qf.Queries, cfg, collector); err != nil {
			t.Fatalf("RunWorkload() failed: %v", err)
		}
		shapes := map[string]string{}
		for _, smp := range collector.SnapshotRepresentativeSamples() {
			if smp.Collection != "ties" || smp.Query == nil {
				continue
			}
			raw, err := bson.Marshal(bson.D{{Key: "p", Value: smp.Query.Pipeline}})
			if err != nil {
				t.Fatal(err)
			}
			captured := rawKeyOrder(bson.Raw(raw).Lookup("p", "0", "$sort").Document())
			name := ""
			for n, d := range want {
				if d.order == captured {
					name = n
				}
			}
			if name == "" {
				t.Fatalf("captured sort %q matches no declared sort", captured)
			}
			if prev, ok := shapes[name]; ok && prev != smp.ShapeID {
				t.Fatalf("one declared sort produced two shapes: %s / %s", prev, smp.ShapeID)
			}
			shapes[name] = smp.ShapeID

			// The captured pipeline is the executed one: run it and check the order.
			cur, err := coll.Aggregate(ctx, smp.Query.Pipeline)
			if err != nil {
				t.Fatalf("aggregate: %v", err)
			}
			var docs []struct {
				ID int32 `bson:"_id"`
			}
			if err := cur.All(ctx, &docs); err != nil {
				t.Fatal(err)
			}
			ids := make([]int32, len(docs))
			for i, d := range docs {
				ids[i] = d.ID
			}
			if !reflect.DeepEqual(ids, want[name].ids) {
				t.Fatalf("%s returned %v, want %v", name, ids, want[name].ids)
			}

			spec, reason := buildExplainCommandSpec(smp, 5000, "executionStats")
			if reason != "" {
				t.Fatalf("%s: explain rejected: %s", name, reason)
			}
			var out bson.Raw
			if err := database.RunCommand(ctx, spec.cmd).Decode(&out); err != nil {
				t.Fatalf("%s: explain: %v", name, err)
			}
			pattern, ok := findSortPattern(bson.RawValue{Type: bson.TypeEmbeddedDocument, Value: out})
			if !ok {
				t.Fatalf("%s: no sort pattern in explain output", name)
			}
			if got := rawKeyOrder(pattern); got != want[name].order {
				t.Fatalf("%s: explain sort pattern %s, want %s", name, got, want[name].order)
			}
		}
		if len(shapes) != 2 || shapes["a_then_b"] == shapes["b_then_a"] {
			t.Fatalf("expected two distinct shapes, got %v", shapes)
		}
		return shapes
	}

	first := runOnce()
	second := runOnce()
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("shape IDs changed between runs: %v vs %v", first, second)
	}
}
