package webui

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// testdata/explain_mongod_8.0.4.json holds executionStats/queryPlanner
// explains captured from a standalone MongoDB 8.0.4 over a synthetic 5,000
// document collection (indexes {a:1} and {b:1,g:1}); bulky server metadata
// was stripped. No mongos explain was captured.
func loadRealExplains(t *testing.T) map[string]bson.M {
	t.Helper()
	data, err := os.ReadFile("testdata/explain_mongod_8.0.4.json")
	if err != nil {
		t.Fatal(err)
	}
	var raw bson.M
	if err := bson.UnmarshalExtJSON(data, false, &raw); err != nil {
		t.Fatal(err)
	}
	out := make(map[string]bson.M, len(raw))
	for k, v := range raw {
		doc, ok := explainDoc(v)
		if !ok {
			t.Fatalf("%s is not a document", k)
		}
		out[k] = bson.M(doc)
	}
	return out
}

const nr = -1 // counter not reported

func counterIs(c *stats.ExplainCounter, want int64) bool {
	if want == nr {
		return c == nil
	}
	return c != nil && c.Value == want
}

func TestNormalizeExplainMetricsRealMongod(t *testing.T) {
	ex := loadRealExplains(t)
	cases := []struct {
		key, op                               string
		layout                                string
		docs, keys, returned, access, matched int64
		lowerBound                            bool
		accessSource                          string
	}{
		// FETCH(1) over IXSCAN(100): the old max-over-nodes parser reported 100 returned.
		{"find_ixscan_fetch", "find", layoutPlan, 100, 100, 1, nr, nr, false, ""},
		{"find_covered", "find", layoutPlan, 0, 10, 10, nr, nr, false, ""},
		{"find_collscan", "find", layoutPlan, 5000, 0, 1, nr, nr, false, ""},
		{"find_zero", "find", layoutPlan, 0, 0, 0, nr, nr, false, ""},
		{"find_limit", "find", layoutPlan, 5, 5, 5, nr, nr, false, ""},
		{"find_id", "find", layoutPlan, 1, 1, 1, nr, nr, false, ""},
		// SBE pushdown: the group input (1000) is the access count, the root (12) the output.
		{"agg_group_pushdown", "aggregate", layoutPlan, 1000, 1000, 12, 1000, nr, false, "[group].inputStage"},
		{"agg_count", "aggregate", layoutPipeline, 0, 1000, 1, 1000, nr, false, "stages[0].$cursor"},
		{"agg_facet", "aggregate", layoutPipeline, 1000, 1000, 1, 1000, nr, false, "stages[0].$cursor"},
		{"agg_match_only", "aggregate", layoutPlan, 100, 100, 100, 100, nr, false, ""},
		{"agg_sort_group", "aggregate", layoutPipeline, 1000, 1000, 12, 1000, nr, false, "[group].inputStage"},
		{"upd_one", "updateOne", layoutPlan, 1, 1, nr, nr, 1, false, ""},
		{"upd_many", "updateMany", layoutPlan, 5000, 0, nr, nr, 1111, false, ""},
		// EXPRESS_UPDATE has no nMatched (and reports nWouldDelete: 0).
		{"upd_id", "updateOne", layoutPlan, 1, 1, nr, nr, 1, true, ""},
		{"upd_zero", "updateMany", layoutPlan, 0, 0, nr, nr, 0, false, ""},
		{"del_one", "deleteOne", layoutPlan, 1, 1, nr, nr, 1, false, ""},
		{"del_many", "deleteMany", layoutPlan, 100, 100, nr, nr, 100, false, ""},
		{"del_id", "deleteOne", layoutPlan, 1, 1, nr, nr, 1, false, ""},
		{"find_planner", "find", layoutPlannerOnly, nr, nr, nr, nr, nr, false, ""},
		{"agg_planner", "aggregate", layoutPlannerOnly, nr, nr, nr, nr, nr, false, ""},
	}
	for _, tc := range cases {
		out, ok := ex[tc.key]
		if !ok {
			t.Fatalf("fixture %s missing", tc.key)
		}
		m := normalizeExplainMetrics(out, tc.op)
		if m.Layout != tc.layout || !counterIs(m.DocsExamined, tc.docs) || !counterIs(m.KeysExamined, tc.keys) ||
			!counterIs(m.Returned, tc.returned) || !counterIs(m.AccessReturned, tc.access) || !counterIs(m.Matched, tc.matched) {
			t.Fatalf("%s: got %+v docs=%+v keys=%+v returned=%+v access=%+v matched=%+v", tc.key, m, m.DocsExamined, m.KeysExamined, m.Returned, m.AccessReturned, m.Matched)
		}
		if m.Matched != nil && m.Matched.LowerBound != tc.lowerBound {
			t.Fatalf("%s: lower bound = %v", tc.key, m.Matched.LowerBound)
		}
		if tc.accessSource != "" && !strings.Contains(m.AccessReturned.Source, tc.accessSource) {
			t.Fatalf("%s: access source %q lacks %q", tc.key, m.AccessReturned.Source, tc.accessSource)
		}
		for _, c := range []*stats.ExplainCounter{m.DocsExamined, m.KeysExamined, m.Returned, m.AccessReturned, m.Matched} {
			if c != nil && c.Source == "" {
				t.Fatalf("%s: counter without a source path %+v", tc.key, c)
			}
		}
	}
}

// diagnosticsFor runs the production parse/normalize/describe path.
func diagnosticsFor(out bson.M, op string, unfiltered bool, verbosity string) *stats.ExplainDiagnostics {
	d := &stats.ExplainDiagnostics{Verbosity: verbosity, UnfilteredScanExpected: unfiltered}
	applyExplainSignalsToDiagnostics(d, parseExplainSignals(out))
	applyExplainMetricsToDiagnostics(d, normalizeExplainMetrics(out, op), op)
	stats.DescribeExplainSample(d, op)
	return d
}

func rateExplains(op string, outs ...*stats.ExplainDiagnostics) *stats.QueryEfficiency {
	cs := stats.ExplainCoverageShape{Operation: op, Status: "explained"}
	for i, d := range outs {
		cs.Samples = append(cs.Samples, stats.ExplainSample{Role: []string{"median", "p95"}[i%2], Status: "explained", Diagnostics: d})
	}
	return stats.BuildQueryEfficiency(cs)
}

func TestQueryEfficiencyFromRealMongodExplains(t *testing.T) {
	ex := loadRealExplains(t)
	cases := []struct {
		key, op    string
		unfiltered bool
		verbosity  string
		status     string
		score      int
		band       string
		obs        []string
	}{
		{"find_ixscan_fetch", "find", false, "executionStats", stats.EfficiencyRated, 33, "inefficient", nil},
		{"find_covered", "find", false, "executionStats", stats.EfficiencyRated, 100, "efficient", []string{"covered"}},
		{"find_collscan", "find", false, "executionStats", stats.EfficiencyRated, 0, "inefficient", nil},
		{"find_zero", "find", false, "executionStats", stats.EfficiencyRated, 100, "efficient", []string{"zero_result", "below_work_floor"}},
		// Scored on the final output (12) this would be 36; the access count gives 100.
		{"agg_group_pushdown", "aggregate", false, "executionStats", stats.EfficiencyRated, 100, "efficient", []string{"aggregate_reduction"}},
		{"agg_count", "aggregate", false, "executionStats", stats.EfficiencyRated, 100, "efficient", []string{"aggregate_reduction"}},
		{"agg_facet", "aggregate", false, "executionStats", stats.EfficiencyRated, 100, "efficient", []string{"aggregate_reduction"}},
		{"upd_many", "updateMany", false, "executionStats", stats.EfficiencyRated, 38, "inefficient", nil},
		{"upd_id", "updateOne", false, "executionStats", stats.EfficiencyRated, 100, "efficient", []string{"matched_count_lower_bound"}},
		{"find_planner", "find", false, "queryPlanner", stats.EfficiencyPartial, 38, "inefficient", nil},
		{"agg_planner", "aggregate", false, "queryPlanner", stats.EfficiencyPartial, 100, "unconfirmed", nil},
	}
	for _, tc := range cases {
		d := diagnosticsFor(ex[tc.key], tc.op, tc.unfiltered, tc.verbosity)
		q := rateExplains(tc.op, d, d)
		if q.Status != tc.status || q.Score == nil || *q.Score != tc.score || q.Band != tc.band {
			t.Fatalf("%s: got status=%s score=%v band=%s dims=%+v", tc.key, q.Status, q.Score, q.Band, q.Dimensions)
		}
		for _, o := range tc.obs {
			found := false
			for _, got := range q.Observations {
				found = found || got == o
			}
			if !found {
				t.Fatalf("%s: missing observation %q in %v", tc.key, o, q.Observations)
			}
		}
	}
	// The flat counters keep their meaning: final output for n_returned, and
	// ratios against the operation's own denominator.
	d := diagnosticsFor(ex["agg_group_pushdown"], "aggregate", false, "executionStats")
	if d.NReturned != 12 || d.DocsExamined != 1000 || d.ExaminedToReturnedRatio != 1 {
		t.Fatalf("flat aggregate counters: returned=%d docs=%d ratio=%v", d.NReturned, d.DocsExamined, d.ExaminedToReturnedRatio)
	}
	if d.Diagnosis == stats.DiagCollscanSelective || d.Diagnosis == stats.DiagIndexLowSelectivity {
		t.Fatalf("a reducing aggregate must not be diagnosed from its final output: %s", d.Diagnosis)
	}
}

func fetchShard(name string, docs, keys, returned int64) bson.M {
	return bson.M{
		"shardName": name, "executionSuccess": true, "nReturned": returned,
		"totalDocsExamined": docs, "totalKeysExamined": keys,
		"executionStages": bson.M{"stage": "SHARDING_FILTER", "nReturned": returned, "inputStage": bson.M{
			"stage": "FETCH", "nReturned": returned, "docsExamined": docs,
			"inputStage": bson.M{"stage": "IXSCAN", "nReturned": keys, "keysExamined": keys, "indexName": "a_1"},
		}},
	}
}

// shardedFind is a mongos find explain equivalent to find_ixscan_fetch
// (100 keys, 100 docs, 1 result) split across two shards.
func shardedFind(routerTotals bool) bson.M {
	stages := bson.M{"stage": "SHARD_MERGE", "nReturned": int64(1), "shards": bson.A{fetchShard("sh1", 60, 60, 1), fetchShard("sh2", 40, 40, 0)}}
	es := bson.M{"nReturned": int64(1), "executionStages": stages}
	if routerTotals {
		es["totalDocsExamined"], es["totalKeysExamined"] = int64(100), int64(100)
		stages["totalDocsExamined"], stages["totalKeysExamined"] = int64(100), int64(100)
	}
	return bson.M{
		"queryPlanner": bson.M{"mongosPlannerVersion": 1, "winningPlan": bson.M{"stage": "SHARD_MERGE", "shards": bson.A{
			bson.M{"shardName": "sh1", "winningPlan": bson.M{"stage": "SHARDING_FILTER", "inputStage": bson.M{"stage": "FETCH", "inputStage": bson.M{"stage": "IXSCAN", "indexName": "a_1"}}}},
			bson.M{"shardName": "sh2", "winningPlan": bson.M{"stage": "SHARDING_FILTER", "inputStage": bson.M{"stage": "FETCH", "inputStage": bson.M{"stage": "IXSCAN", "indexName": "a_1"}}}},
		}}},
		"executionStats": es,
	}
}

func groupPushdownShard(docs, groupInput, out int64) bson.M {
	return bson.M{
		"explainVersion": "2",
		"queryPlanner":   bson.M{"winningPlan": bson.M{"queryPlan": bson.M{"stage": "GROUP", "inputStage": bson.M{"stage": "FETCH", "inputStage": bson.M{"stage": "IXSCAN", "indexName": "a_1"}}}}},
		"executionStats": bson.M{
			"nReturned": out, "totalDocsExamined": docs, "totalKeysExamined": docs,
			"executionStages": bson.M{"stage": "project", "nReturned": out, "inputStage": bson.M{
				"stage": "group", "nReturned": out, "inputStage": bson.M{"stage": "nlj", "nReturned": groupInput},
			}},
		},
	}
}

func TestNormalizeExplainMetricsShardedLayouts(t *testing.T) {
	standalone := normalizeExplainMetrics(loadRealExplains(t)["find_ixscan_fetch"], "find")

	for _, routerTotals := range []bool{true, false} {
		m := normalizeExplainMetrics(shardedFind(routerTotals), "find")
		// Router totals and shard totals cover the same work; adding them would
		// give 200, and taking the max would give 60.
		if m.Layout != layoutShardedPlan || m.Shards != 2 || !counterIs(m.DocsExamined, 100) || !counterIs(m.KeysExamined, 100) || !counterIs(m.Returned, 1) {
			t.Fatalf("routerTotals=%v: %+v docs=%+v keys=%+v returned=%+v", routerTotals, m, m.DocsExamined, m.KeysExamined, m.Returned)
		}
		if routerTotals == strings.HasPrefix(m.DocsExamined.Source, "sum(") {
			t.Fatalf("routerTotals=%v: unexpected source %q", routerTotals, m.DocsExamined.Source)
		}
		if *m.DocsExamined != (stats.ExplainCounter{Value: standalone.DocsExamined.Value, Source: m.DocsExamined.Source}) {
			t.Fatalf("sharded docs differ from standalone")
		}
	}

	// A shard missing its total makes the sum unknown, not an undercount.
	partial := shardedFind(false)
	shards := partial["executionStats"].(bson.M)["executionStages"].(bson.M)["shards"].(bson.A)
	delete(shards[1].(bson.M), "totalDocsExamined")
	if m := normalizeExplainMetrics(partial, "find"); m.DocsExamined != nil || !counterIs(m.KeysExamined, 100) {
		t.Fatalf("incomplete shard counters must be unknown: docs=%+v", m.DocsExamined)
	}

	// Sharded aggregate: per-shard access counts and work are summed.
	agg := bson.M{"mergeType": "mongos", "splitPipeline": bson.M{"shardsPart": bson.A{}, "mergerPart": bson.A{}}, "shards": bson.M{
		"sh1": groupPushdownShard(600, 600, 12), "sh2": groupPushdownShard(400, 400, 12),
	}}
	m := normalizeExplainMetrics(agg, "aggregate")
	if m.Layout != layoutShardedPipeline || !counterIs(m.DocsExamined, 1000) || !counterIs(m.AccessReturned, 1000) || m.Returned != nil {
		t.Fatalf("sharded aggregate: %+v docs=%+v access=%+v returned=%+v", m, m.DocsExamined, m.AccessReturned, m.Returned)
	}

	// Sharded write: nMatched is summed from each shard's write stage.
	upd := bson.M{"queryPlanner": bson.M{"winningPlan": bson.M{"stage": "SHARD_WRITE"}}, "executionStats": bson.M{
		"nReturned": int64(0), "totalDocsExamined": int64(10), "totalKeysExamined": int64(10),
		"executionStages": bson.M{"stage": "SHARD_WRITE", "shards": bson.A{
			bson.M{"shardName": "sh1", "executionStages": bson.M{"stage": "UPDATE", "nMatched": int64(3), "inputStage": bson.M{"stage": "FETCH"}}},
			bson.M{"shardName": "sh2", "executionStages": bson.M{"stage": "UPDATE", "nMatched": int64(7), "inputStage": bson.M{"stage": "FETCH"}}},
		}},
	}}
	if m := normalizeExplainMetrics(upd, "updateMany"); !counterIs(m.Matched, 10) || !counterIs(m.DocsExamined, 10) {
		t.Fatalf("sharded write: matched=%+v docs=%+v", m.Matched, m.DocsExamined)
	}
}

// Equivalent work gives the same score on a standalone and a sharded layout.
func TestQueryEfficiencyEquivalentStandaloneAndSharded(t *testing.T) {
	ex := loadRealExplains(t)
	single := rateExplains("find", diagnosticsFor(ex["find_ixscan_fetch"], "find", false, "executionStats"))
	for _, routerTotals := range []bool{true, false} {
		sh := rateExplains("find", diagnosticsFor(shardedFind(routerTotals), "find", false, "executionStats"))
		if *sh.Score != *single.Score || sh.Band != single.Band || sh.LimitingDimension != single.LimitingDimension {
			t.Fatalf("routerTotals=%v: sharded %d/%s vs standalone %d/%s", routerTotals, *sh.Score, sh.Band, *single.Score, single.Band)
		}
	}

	singleAgg := rateExplains("aggregate", diagnosticsFor(groupPushdownShard(1000, 1000, 12), "aggregate", false, "executionStats"))
	shAgg := rateExplains("aggregate", diagnosticsFor(bson.M{"shards": bson.M{
		"sh1": groupPushdownShard(600, 600, 12), "sh2": groupPushdownShard(400, 400, 12),
	}}, "aggregate", false, "executionStats"))
	if *singleAgg.Score != 100 || *shAgg.Score != *singleAgg.Score || shAgg.Status != stats.EfficiencyRated {
		t.Fatalf("aggregate: sharded %+v vs standalone %+v", shAgg, singleAgg)
	}
}

func TestNormalizeExplainMetricsUnsupportedAndAmbiguousLayouts(t *testing.T) {
	noCursor := bson.M{"stages": bson.A{bson.M{"$collStats": bson.M{}, "nReturned": int64(1)}}}
	if m := normalizeExplainMetrics(noCursor, "aggregate"); m.Layout != layoutUnsupported || m.UnsupportedReason != "pipeline_without_cursor_stage" {
		t.Fatalf("pipeline without $cursor: %+v", m)
	}
	lookup := groupPushdownShard(100, 100, 100)
	lookup["queryPlanner"] = bson.M{"winningPlan": bson.M{"queryPlan": bson.M{"stage": "EQ_LOOKUP", "inputStage": bson.M{"stage": "COLLSCAN"}}}}
	m := normalizeExplainMetrics(lookup, "aggregate")
	if m.AccessReturned != nil || len(m.Notes) == 0 || m.Notes[0] != "lookup_pushdown_access_count_ambiguous" {
		t.Fatalf("lookup pushdown must leave the access count unknown: %+v", m)
	}
	// Rejected plans never affect the access-count decision.
	rejected := groupPushdownShard(100, 100, 100)
	rejected["queryPlanner"].(bson.M)["rejectedPlans"] = bson.A{bson.M{"stage": "EQ_LOOKUP"}}
	if m := normalizeExplainMetrics(rejected, "aggregate"); !counterIs(m.AccessReturned, 100) {
		t.Fatalf("rejected plan leaked: %+v", m)
	}
	if q := rateExplains("aggregate", diagnosticsFor(lookup, "aggregate", false, "executionStats")); q.Status != stats.EfficiencyPartial || q.Band == stats.EfficiencyBandEfficient {
		t.Fatalf("ambiguous access count must be partial: %+v", q)
	}
}

func TestNormalizeExplainMetricsLookupStages(t *testing.T) {
	withLookups := func(lookups ...bson.M) bson.M {
		out := loadRealExplains(t)["agg_count"]
		stages, ok := out["stages"].(bson.A)
		if !ok {
			t.Fatalf("agg_count fixture has no stages array: %T", out["stages"])
		}
		for _, l := range lookups {
			stages = append(stages, l)
		}
		out["stages"] = stages
		return out
	}
	one := bson.M{"$lookup": bson.M{"from": "customers"}, "totalDocsExamined": int64(40), "totalKeysExamined": int64(40), "collectionScans": int64(0)}
	m := normalizeExplainMetrics(withLookups(one), "aggregate")
	if m.Lookup == nil || m.Lookup.Stages != 1 || !counterIs(m.Lookup.DocsExamined, 40) || !counterIs(m.Lookup.KeysExamined, 40) ||
		!counterIs(m.Lookup.CollectionScans, 0) || m.Lookup.DocsExamined.Source != fmt.Sprintf("stages[%d].totalDocsExamined", len(loadRealExplains(t)["agg_count"]["stages"].(bson.A))) {
		t.Fatalf("one $lookup stage: %+v docs=%+v keys=%+v scans=%+v", m.Lookup, m.Lookup.DocsExamined, m.Lookup.KeysExamined, m.Lookup.CollectionScans)
	}
	if !counterIs(m.DocsExamined, 0) || !counterIs(m.KeysExamined, 1000) {
		t.Fatalf("$lookup work must not change the scored outer counts: docs=%+v keys=%+v", m.DocsExamined, m.KeysExamined)
	}
	partial := bson.M{"$lookup": bson.M{"from": "orders"}, "totalDocsExamined": int64(5)}
	m = normalizeExplainMetrics(withLookups(one, partial), "aggregate")
	if m.Lookup.Stages != 2 || !counterIs(m.Lookup.DocsExamined, 45) || m.Lookup.KeysExamined != nil ||
		!strings.Contains(m.Lookup.DocsExamined.Source, "over 2 stages") {
		t.Fatalf("a stage missing a counter makes only that sum unknown: %+v", m.Lookup)
	}
	if m := normalizeExplainMetrics(loadRealExplains(t)["agg_count"], "aggregate"); m.Lookup != nil {
		t.Fatalf("no $lookup stage, no $lookup metrics: %+v", m.Lookup)
	}
}
