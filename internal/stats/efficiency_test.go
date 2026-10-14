package stats

import (
	"encoding/json"
	"strings"
	"testing"
)

func ctr(v int64, src string) *ExplainCounter { return &ExplainCounter{Value: v, Source: src} }

// fixtureDiag builds explained diagnostics with normalized counters. A
// negative count means the counter was not reported.
type fixtureDiag struct {
	op                     string
	collscan, unfiltered   bool
	ixscan, fetch          bool
	blockingSort, usedDisk bool
	plannerOnly            bool
	docs, keys, result     int64
	final                  int64 // aggregate final output; -1 unreported
	shards                 int
}

func (f fixtureDiag) diag() *ExplainDiagnostics {
	d := &ExplainDiagnostics{
		ExecutionStatsAvailable: !f.plannerOnly,
		CollectionScanDetected:  f.collscan, UnfilteredScanExpected: f.unfiltered,
		IndexScanDetected: f.ixscan, FetchDetected: f.fetch,
		BlockingSortDetected: f.blockingSort, UsedDisk: f.usedDisk,
	}
	if f.plannerOnly {
		d.Verbosity = "queryPlanner"
		d.Metrics = &ExplainMetrics{Layout: "planner_only"}
		return d
	}
	m := &ExplainMetrics{Layout: "plan", Shards: f.shards}
	opt := func(v int64, src string) *ExplainCounter {
		if v < 0 {
			return nil
		}
		return ctr(v, src)
	}
	m.DocsExamined = opt(f.docs, "executionStats.totalDocsExamined")
	m.KeysExamined = opt(f.keys, "executionStats.totalKeysExamined")
	switch {
	case isWriteOp(f.op):
		m.Matched = opt(f.result, "executionStats.executionStages.nMatched")
	case f.op == "aggregate":
		m.AccessReturned = opt(f.result, "executionStats.executionStages[group].inputStage.nReturned")
		m.Returned = opt(f.final, "executionStats.nReturned")
	default:
		m.Returned = opt(f.result, "executionStats.nReturned")
	}
	d.Metrics = m
	return d
}

func explainedShape(op string, latencies []float64, d func() *ExplainDiagnostics) ExplainCoverageShape {
	cs := ExplainCoverageShape{ShapeID: "s1", Operation: op, Status: "explained", SampleScope: SampleScopeUniformReservoir}
	roles := []string{"median", "p95"}
	for i, l := range latencies {
		cs.Samples = append(cs.Samples, ExplainSample{Role: roles[i%2], ClientLatencyMs: l, Status: "explained", Diagnostics: d()})
	}
	return cs
}

func hasString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func scoreOf(t *testing.T, q *QueryEfficiency) int {
	t.Helper()
	if q.Score == nil {
		t.Fatalf("expected a score, got %+v", q)
	}
	return *q.Score
}

// The spec's calibration fixtures (docs/query-efficiency-spec.md §8).
func TestQueryEfficiencyGoldenFixtures(t *testing.T) {
	cases := []struct {
		id         string
		f          fixtureDiag
		latency    []float64
		status     string
		score      int
		upper      bool
		band       string
		limiting   string
		confidence string
		obs        []string
	}{
		{"F1 fast inefficient", fixtureDiag{op: "find", collscan: true, docs: 50000, keys: 0, result: 2}, []float64{4, 4.5},
			EfficiencyRated, 0, false, EfficiencyBandInefficient, DimDocsPerResult, "high", []string{"large_work"}},
		{"F2 slow efficient", fixtureDiag{op: "find", ixscan: true, fetch: true, docs: 20000, keys: 20000, result: 20000}, []float64{1800, 2400},
			EfficiencyRated, 100, false, EfficiencyBandEfficient, "", "high", []string{"large_work"}},
		{"F3a zero-result indexed miss", fixtureDiag{op: "find", ixscan: true, fetch: true, docs: 0, keys: 1, result: 0}, []float64{1, 1},
			EfficiencyRated, 100, false, EfficiencyBandEfficient, "", "high", []string{"zero_result", "below_work_floor"}},
		{"F3b zero-result scan", fixtureDiag{op: "find", collscan: true, docs: 10000, keys: 0, result: 0}, []float64{30, 40},
			EfficiencyRated, 0, false, EfficiencyBandInefficient, DimDocsPerResult, "high", []string{"zero_result"}},
		{"F4a covered", fixtureDiag{op: "find", ixscan: true, docs: 0, keys: 10, result: 10}, []float64{1, 2},
			EfficiencyRated, 100, false, EfficiencyBandEfficient, "", "high", []string{"covered", "below_work_floor"}},
		{"F4b covered wide range", fixtureDiag{op: "find", ixscan: true, docs: 0, keys: 5000, result: 10}, []float64{3, 5},
			EfficiencyRated, 10, false, EfficiencyBandInefficient, DimKeysPerResult, "high", []string{"covered"}},
		{"F5a aggregate reduction", fixtureDiag{op: "aggregate", ixscan: true, fetch: true, docs: 5000, keys: 5000, result: 5000, final: 12}, []float64{20, 25},
			EfficiencyRated, 100, false, EfficiencyBandEfficient, "", "high", []string{"aggregate_reduction"}},
		{"F5b access count missing", fixtureDiag{op: "aggregate", ixscan: true, fetch: true, docs: 5000, keys: 5000, result: -1, final: 12}, []float64{20, 25},
			EfficiencyPartial, 100, true, EfficiencyBandUnconfirmed, "", "low", nil},
		{"F6a efficient write", fixtureDiag{op: "updateOne", ixscan: true, fetch: true, docs: 1, keys: 1, result: 1}, []float64{2, 3},
			EfficiencyRated, 100, false, EfficiencyBandEfficient, "", "high", []string{"below_work_floor"}},
		{"F6b inefficient write", fixtureDiag{op: "updateMany", collscan: true, docs: 100000, keys: 0, result: 50}, []float64{90, 120},
			EfficiencyRated, 0, false, EfficiencyBandInefficient, DimDocsPerResult, "high", []string{"large_work"}},
		{"F7a planner-only scan", fixtureDiag{op: "find", collscan: true, plannerOnly: true}, []float64{9, 9},
			EfficiencyPartial, 38, true, EfficiencyBandInefficient, DimCollscan, "low", nil},
		{"F7b planner-only index", fixtureDiag{op: "find", ixscan: true, fetch: true, plannerOnly: true}, []float64{9, 9},
			EfficiencyPartial, 100, true, EfficiencyBandUnconfirmed, "", "low", nil},
		{"F9 expected unfiltered scan", fixtureDiag{op: "find", collscan: true, unfiltered: true, docs: 1000, keys: 0, result: 1000}, []float64{8, 9},
			EfficiencyRated, 100, false, EfficiencyBandEfficient, "", "high", []string{"unfiltered_scan_expected"}},
		{"F10 moderate", fixtureDiag{op: "find", ixscan: true, fetch: true, docs: 1000, keys: 1000, result: 100}, []float64{5, 6},
			EfficiencyRated, 67, false, EfficiencyBandModerate, DimDocsPerResult, "high", nil},
		// S4 records blocking sorts as observations only (no sort penalty).
		{"F11 blocking sort only", fixtureDiag{op: "aggregate", ixscan: true, fetch: true, blockingSort: true, docs: 500, keys: 500, result: 500, final: 500}, []float64{7, 8},
			EfficiencyRated, 100, false, EfficiencyBandEfficient, "", "high", []string{"blocking_sort"}},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			q := BuildQueryEfficiency(explainedShape(tc.f.op, tc.latency, tc.f.diag))
			if q.Version != EfficiencyVersion || q.Status != tc.status || scoreOf(t, q) != tc.score || q.ScoreIsUpperBound != tc.upper ||
				q.Band != tc.band || q.LimitingDimension != tc.limiting || q.Confidence != tc.confidence {
				t.Fatalf("got status=%s score=%v upper=%v band=%s limiting=%q confidence=%s; want %s %d %v %s %q %s",
					q.Status, *q.Score, q.ScoreIsUpperBound, q.Band, q.LimitingDimension, q.Confidence,
					tc.status, tc.score, tc.upper, tc.band, tc.limiting, tc.confidence)
			}
			for _, o := range tc.obs {
				if !hasString(q.Observations, o) {
					t.Fatalf("missing observation %q in %v", o, q.Observations)
				}
			}
			if len(q.Samples) != 2 || q.SamplesRated+q.SamplesPartial != 2 {
				t.Fatalf("both samples must be kept and assessed: %+v", q.Samples)
			}
		})
	}
}

func TestQueryEfficiencyUnassessedAndNotApplicable(t *testing.T) {
	cases := []struct {
		name   string
		cs     ExplainCoverageShape
		status string
		reason string
	}{
		{"F6c insert", ExplainCoverageShape{Operation: "insertMany", Status: "not_supported", Reason: "operation_insertMany_not_supported"}, EfficiencyNotApplicable, "operation_not_explainable"},
		{"F8a timeout", ExplainCoverageShape{Operation: "find", Status: "failed", Reason: "timed_out:max_time_exceeded",
			Samples: []ExplainSample{{Role: "median", Status: "timed_out"}, {Role: "p95", Status: "timed_out"}}}, EfficiencyUnassessed, "explain_timeout"},
		{"F8b not selected", ExplainCoverageShape{Operation: "find", Status: "excluded", Reason: "not_selected_budget"}, EfficiencyUnassessed, "not_selected_budget"},
		{"cancelled", ExplainCoverageShape{Operation: "find", Status: "cancelled", Reason: "explain_cancelled"}, EfficiencyUnassessed, "explain_cancelled"},
		{"execution failure", ExplainCoverageShape{Operation: "find", Status: "failed", Reason: "execution_failed:not_authorized"}, EfficiencyUnassessed, "explain_failed"},
		{"no samples captured", ExplainCoverageShape{Operation: "find", Status: "unavailable", Reason: "no_representative_samples"}, EfficiencyUnassessed, "no_representative_samples"},
		{"unsupported op", ExplainCoverageShape{Operation: "findOneAndUpdate", Status: "not_supported", Reason: "operation_findOneAndUpdate_not_supported"}, EfficiencyUnassessed, "unsupported_operation"},
	}
	for _, tc := range cases {
		q := BuildQueryEfficiency(tc.cs)
		if q.Status != tc.status || q.Reason != tc.reason || q.Score != nil || q.Band != "" || q.Confidence != "" {
			t.Fatalf("%s: got %+v", tc.name, q)
		}
	}
}

func TestQueryEfficiencyBoundaries(t *testing.T) {
	for _, tc := range []struct {
		score int
		band  string
	}{{100, "efficient"}, {75, "efficient"}, {74, "moderate"}, {50, "moderate"}, {49, "inefficient"}, {0, "inefficient"}} {
		if got := efficiencyBand(tc.score); got != tc.band {
			t.Fatalf("band(%d) = %s, want %s", tc.score, got, tc.band)
		}
	}
	if upperBoundBand(75) != EfficiencyBandUnconfirmed || upperBoundBand(74) != EfficiencyBandModerate || upperBoundBand(38) != EfficiencyBandInefficient {
		t.Fatal("an upper bound in the efficient range must stay unconfirmed")
	}
	for _, tc := range []struct {
		ratio float64
		score int
	}{{1, 100}, {10, 67}, {100, 33}, {1000, 0}, {25000, 0}} {
		if got := efficiencyScore(ratioPenalty(tc.ratio)); got != tc.score {
			t.Fatalf("ratio %.0f scored %d, want %d", tc.ratio, got, tc.score)
		}
	}
	// Work floor: 99 examined for 1 result is not penalized, 100 is.
	for _, tc := range []struct {
		docs  int64
		score int
	}{{99, 100}, {100, 33}} {
		f := fixtureDiag{op: "find", ixscan: true, fetch: true, docs: tc.docs, keys: 1, result: 1}
		if got := scoreOf(t, BuildQueryEfficiency(explainedShape("find", []float64{1, 1}, f.diag))); got != tc.score {
			t.Fatalf("docs=%d scored %d, want %d", tc.docs, got, tc.score)
		}
	}
}

// More examined work for the same results can never raise the score, and
// latency or frequency never change it.
func TestQueryEfficiencyMonotonicAndLatencyIndependent(t *testing.T) {
	prev := 101
	for _, docs := range []int64{0, 50, 100, 500, 1000, 5000, 20000, 100000, 1000000} {
		f := fixtureDiag{op: "find", ixscan: true, fetch: true, docs: docs, keys: docs, result: 10}
		got := scoreOf(t, BuildQueryEfficiency(explainedShape("find", []float64{1, 1}, f.diag)))
		if got > prev {
			t.Fatalf("docs=%d scored %d, above %d for less work", docs, got, prev)
		}
		prev = got
	}
	f := fixtureDiag{op: "find", ixscan: true, fetch: true, docs: 1000, keys: 1000, result: 100}
	fast := explainedShape("find", []float64{0.2, 0.3}, f.diag)
	slow := explainedShape("find", []float64{4000, 9000}, f.diag)
	slow.OperationCount, slow.ImpactRank, slow.AvgMs = 1_000_000, 1, 5000
	if a, b := scoreOf(t, BuildQueryEfficiency(fast)), scoreOf(t, BuildQueryEfficiency(slow)); a != b || a != 67 {
		t.Fatalf("latency/frequency changed the score: %d vs %d", a, b)
	}
}

func TestQueryEfficiencySamplesDisagreeKeepsEachScore(t *testing.T) {
	cs := ExplainCoverageShape{Operation: "find", Status: "explained", Samples: []ExplainSample{
		{Role: "median", Status: "explained", Diagnostics: fixtureDiag{op: "find", ixscan: true, fetch: true, docs: 10, keys: 10, result: 10}.diag()},
		{Role: "p95", Status: "explained", Diagnostics: fixtureDiag{op: "find", ixscan: true, fetch: true, docs: 1000, keys: 1000, result: 10}.diag()},
	}}
	q := BuildQueryEfficiency(cs)
	if scoreOf(t, q) != 33 || q.DeterminingSample != "p95" || !q.SamplesDisagree || q.Confidence != "medium" || q.Status != EfficiencyRated {
		t.Fatalf("worst sample must set the score with disagreement disclosed: %+v", q)
	}
	if *q.Samples[0].Score != 100 || *q.Samples[1].Score != 33 {
		t.Fatalf("per-sample scores must be kept: %+v", q.Samples)
	}
}

func TestQueryEfficiencyPartialAndInconclusiveSamples(t *testing.T) {
	// One sample timed out: still rated from the other, at medium confidence.
	cs := explainedShape("find", []float64{1}, fixtureDiag{op: "find", ixscan: true, fetch: true, docs: 5, keys: 5, result: 5}.diag)
	cs.Samples = append(cs.Samples, ExplainSample{Role: "p95", Status: "timed_out", Reason: "max_time_exceeded"})
	q := BuildQueryEfficiency(cs)
	if q.Status != EfficiencyRated || q.Confidence != "medium" || q.SamplesRated != 1 || q.Samples[1].Reason != "explain_timeout" || len(q.Limitations) == 0 {
		t.Fatalf("an inconclusive sample must lower confidence and be disclosed: %+v", q)
	}

	// A rated efficient sample next to a partial one cannot claim efficient.
	cs = ExplainCoverageShape{Operation: "find", Status: "explained", Samples: []ExplainSample{
		{Role: "median", Status: "explained", Diagnostics: fixtureDiag{op: "find", ixscan: true, fetch: true, docs: 5, keys: 5, result: 5}.diag()},
		{Role: "p95", Status: "explained", Diagnostics: fixtureDiag{op: "find", ixscan: true, fetch: true, docs: -1, keys: 5, result: 5}.diag()},
	}}
	q = BuildQueryEfficiency(cs)
	if q.Status != EfficiencyPartial || !q.ScoreIsUpperBound || q.Band != EfficiencyBandUnconfirmed || q.Confidence != "low" {
		t.Fatalf("missing docsExamined must not yield an efficient rating: %+v", q)
	}
	if d := q.Samples[1].Dimensions[0]; d.Penalty != nil || d.MissingReason != "docs_examined_not_reported" {
		t.Fatalf("a missing counter must not become zero: %+v", d)
	}

	// Diagnostics without normalized metrics are partial, never rated from
	// the flat counters' zero values.
	cs = explainedShape("find", []float64{1, 1}, func() *ExplainDiagnostics {
		return &ExplainDiagnostics{ExecutionStatsAvailable: true, IndexScanDetected: true}
	})
	if q = BuildQueryEfficiency(cs); q.Status != EfficiencyPartial || q.Band != EfficiencyBandUnconfirmed || q.Reason != "normalized_metrics_unavailable" {
		t.Fatalf("unnormalized diagnostics must be partial: %+v", q)
	}

	// Unsupported layouts are partial with the layout reason.
	cs = explainedShape("aggregate", []float64{1, 1}, func() *ExplainDiagnostics {
		return &ExplainDiagnostics{ExecutionStatsAvailable: true, Metrics: &ExplainMetrics{Layout: "unsupported", UnsupportedReason: "pipeline_without_cursor_stage"}}
	})
	if q = BuildQueryEfficiency(cs); q.Status != EfficiencyPartial || q.Reason != "pipeline_without_cursor_stage" || q.Band != EfficiencyBandUnconfirmed {
		t.Fatalf("unsupported layout must be partial: %+v", q)
	}
}

// No combination of partial evidence produces an efficient band.
func TestQueryEfficiencyPartialNeverEfficient(t *testing.T) {
	for _, op := range []string{"find", "aggregate", "updateMany", "deleteOne"} {
		for _, f := range []fixtureDiag{
			{op: op, ixscan: true, plannerOnly: true},
			{op: op, ixscan: true, docs: -1, keys: -1, result: -1, final: -1},
			{op: op, ixscan: true, docs: 1, keys: 1, result: -1, final: 1},
		} {
			q := BuildQueryEfficiency(explainedShape(op, []float64{1, 1}, f.diag))
			if q.Status != EfficiencyPartial || q.Band == EfficiencyBandEfficient || q.Confidence != "low" {
				t.Fatalf("%s %+v: partial evidence rated %+v", op, f, q)
			}
		}
	}
}

func TestQueryEfficiencyApproximateAndShardedSourcesLowerConfidence(t *testing.T) {
	lower := func() *ExplainDiagnostics {
		d := fixtureDiag{op: "updateOne", ixscan: true, docs: 1, keys: 1, result: 1}.diag()
		d.Metrics.Matched.LowerBound = true
		return d
	}
	q := BuildQueryEfficiency(explainedShape("updateOne", []float64{1, 1}, lower))
	if q.Status != EfficiencyRated || q.Confidence != "medium" || !hasString(q.Observations, "matched_count_lower_bound") {
		t.Fatalf("lower-bound result count must be disclosed: %+v", q)
	}
	sharded := fixtureDiag{op: "find", ixscan: true, fetch: true, docs: 5, keys: 5, result: 5, shards: 2}
	if q = BuildQueryEfficiency(explainedShape("find", []float64{1, 1}, sharded.diag)); q.Confidence != "medium" || scoreOf(t, q) != 100 {
		t.Fatalf("sharded evidence: %+v", q)
	}
}

func TestQueryEfficiencySortAndSpillAreObservationsOnly(t *testing.T) {
	f := fixtureDiag{op: "aggregate", ixscan: true, fetch: true, blockingSort: true, usedDisk: true, docs: 500, keys: 500, result: 500, final: 500}
	q := BuildQueryEfficiency(explainedShape("aggregate", []float64{1, 1}, f.diag))
	if scoreOf(t, q) != 100 || !hasString(q.Observations, "blocking_sort") || !hasString(q.Observations, "spill") {
		t.Fatalf("sort/spill must be recorded but not scored: %+v", q)
	}
	for _, d := range q.Dimensions {
		if d.Name != DimDocsPerResult && d.Name != DimKeysPerResult && d.Name != DimCollscan {
			t.Fatalf("unexpected scored dimension %q", d.Name)
		}
	}
}

func TestEfficiencyJSONKeepsNullableScore(t *testing.T) {
	rated := BuildQueryEfficiency(explainedShape("find", []float64{1, 1}, fixtureDiag{op: "find", collscan: true, docs: 50000, keys: 0, result: 2}.diag))
	unassessed := BuildQueryEfficiency(ExplainCoverageShape{Operation: "find", Status: "excluded", Reason: "not_selected_budget"})
	rb, _ := json.Marshal(rated)
	ub, _ := json.Marshal(unassessed)
	if !strings.Contains(string(rb), `"score":0,`) || !strings.Contains(string(rb), `"version":"`+EfficiencyVersion+`"`) {
		t.Fatalf("a rated zero must serialize its score: %s", rb)
	}
	if strings.Contains(string(ub), `"score"`) || !strings.Contains(string(ub), `"status":"unassessed"`) {
		t.Fatalf("an unassessed shape must omit its score: %s", ub)
	}
	if strings.Contains(string(rb), "approximate") || strings.Contains(string(rb), "client_latency") {
		t.Fatalf("internal flags and latency must not be part of the efficiency object: %s", rb)
	}
}

func TestApplyExplainFindingsBuildsEfficiencyCoverage(t *testing.T) {
	rep := &InsightsReport{ExplainCoverage: NewExplainCoverage("ready", "")}
	rep.ExplainCoverage.Sampling.UntrackedOperations = 7
	rated := explainedShape("find", []float64{1, 1}, fixtureDiag{op: "find", collscan: true, docs: 50000, keys: 0, result: 2}.diag)
	rated.ShapeID, rated.SelectedBy = "a", "impact_rank"
	partial := explainedShape("find", []float64{1, 1}, fixtureDiag{op: "find", ixscan: true, plannerOnly: true}.diag)
	partial.ShapeID, partial.SelectedBy = "b", "latency_tier"
	rep.ExplainCoverage.Shapes = []ExplainCoverageShape{
		rated, partial,
		{ShapeID: "c", Operation: "find", Status: "excluded", Reason: "not_selected_budget", SampleScope: SampleScopeAllOperations},
		{ShapeID: "d", Operation: "insertOne", Status: "not_supported", Reason: "operation_insertOne_not_supported"},
	}
	ApplyExplainFindings(rep)
	ec := rep.EfficiencyCoverage
	if ec == nil || ec.Status != "ready" || ec.ShapesTotal != 4 || ec.ShapesRated != 1 || ec.ShapesPartial != 1 ||
		ec.ShapesUnassessed != 1 || ec.ShapesNotApplicable != 1 || ec.UnassessedByReason["not_selected_budget"] != 1 ||
		ec.ShapesByBand[EfficiencyBandInefficient] != 1 || ec.ShapesByBand[EfficiencyBandUnconfirmed] != 1 || ec.UntrackedOperations != 7 {
		t.Fatalf("unexpected efficiency coverage: %+v", ec)
	}
	for _, s := range rep.ExplainCoverage.Shapes {
		if s.Efficiency == nil || s.Efficiency.Version != EfficiencyVersion {
			t.Fatalf("shape %s missing efficiency", s.ShapeID)
		}
	}
	if !strings.Contains(ec.Method, "PLGM heuristic, not a MongoDB metric") || !strings.Contains(ec.Method, "higher is better") {
		t.Fatalf("method must state direction and that the score is a heuristic: %s", ec.Method)
	}

	off := &InsightsReport{ExplainCoverage: NewExplainCoverage("disabled", "explain_disabled")}
	ApplyExplainFindings(off)
	if off.EfficiencyCoverage == nil || off.EfficiencyCoverage.Status != "disabled" || off.EfficiencyCoverage.ShapesTotal != 0 {
		t.Fatalf("disabled explain must disable efficiency coverage: %+v", off.EfficiencyCoverage)
	}
}
