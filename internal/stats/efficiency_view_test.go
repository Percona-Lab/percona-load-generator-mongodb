package stats

import (
	"strings"
	"testing"
)

func intPtr(v int) *int { return &v }

func viewTestReport() InsightsReport {
	rated := func(id string, score int, rank int, below bool) ExplainCoverageShape {
		return ExplainCoverageShape{
			ShapeID: id, Namespace: "shop.orders", Operation: "find", ShapeSummary: id, ImpactRank: rank,
			TotalOperationMs: float64(100 * rank), BelowSlowThreshold: below, Status: "explained", Evidence: "execution_stats",
			Efficiency: &QueryEfficiency{Version: EfficiencyVersion, Status: EfficiencyRated, Score: intPtr(score), Band: efficiencyBand(score),
				Confidence: "high", LimitingDimension: DimDocsPerResult,
				Dimensions: []EfficiencyDimension{{Name: DimDocsPerResult, Applicable: true, Value: floatPtr(25000), Penalty: floatPtr(1 - float64(score)/100)}}},
		}
	}
	partial := rated("partial", 38, 5, true)
	partial.Efficiency.Status, partial.Efficiency.ScoreIsUpperBound, partial.Efficiency.Band, partial.Efficiency.Reason = EfficiencyPartial, true, EfficiencyBandInefficient, "execution_stats_unavailable"
	partial.Evidence, partial.PartialReason = "planner_only", "verbosity_queryPlanner"
	slowEff := rated("slow-100", 100, 1, false)
	slowEff.Efficiency.Observations = []string{"blocking_sort"}
	return InsightsReport{
		Impact: ImpactView{Status: "ready", Coverage: ImpactCoverage{ObservedOperationMs: 1000}},
		ExplainCoverage: ExplainCoverageView{Status: "ready", UnrecordedPaths: captureUnrecordedPaths, Shapes: []ExplainCoverageShape{
			slowEff,
			rated("fast-0", 0, 4, true),
			rated("tie-38", 38, 6, true),
			partial,
			{ShapeID: "excluded", Operation: "find", ImpactRank: 2, Status: "excluded", Reason: "not_selected_budget"},
			{ShapeID: "timeout", Operation: "find", ImpactRank: 3, Status: "failed", Reason: "timed_out:x"},
			{ShapeID: "insert", Operation: "insert", ImpactRank: 7, Status: "not_supported", Reason: "operation_insert_not_supported"},
		}},
		SlowQueries:     []SlowQueryInsight{{ShapeID: "slow-100", Severity: "high"}},
		QueryCapture:    QueryCaptureView{Shapes: []CapturedShape{{ShapeID: "fast-0", Descriptor: "{sku: <string>}"}}},
		Recommendations: []InsightRecommendation{{Rank: 3, ShapeIDs: []string{"fast-0", "tie-38"}}, {Rank: 4, ShapeID: "fast-0"}},
	}
}

func TestEfficiencyViewOrderLabelsAndCoverage(t *testing.T) {
	v := BuildEfficiencyView(viewTestReport())
	var order []string
	for _, r := range v.Rows {
		order = append(order, r.ShapeID)
	}
	want := []string{"fast-0", "tie-38", "partial", "slow-100", "excluded", "timeout", "insert"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("efficiency order %v, want %v", order, want)
	}
	byID := map[string]EfficiencyViewRow{}
	for _, r := range v.Rows {
		byID[r.ShapeID] = r
	}
	checks := map[string][2]string{
		"fast-0":   {"0", EffCoverageAssessed},
		"partial":  {"≤ 38", EffCoveragePartial},
		"slow-100": {"100", EffCoverageAssessed},
		"excluded": {"Unassessed", EffCoverageBudgetExcluded},
		"timeout":  {"Unassessed", EffCoverageFailed},
		"insert":   {"Not applicable", EffCoverageNotApplicable},
	}
	for id, c := range checks {
		if r := byID[id]; r.ScoreLabel != c[0] || r.Coverage != c[1] {
			t.Fatalf("%s: label %q coverage %q, want %v", id, r.ScoreLabel, r.Coverage, c)
		}
	}
	if r := byID["excluded"]; r.Score != nil || !strings.HasPrefix(r.RatingExplanation, "No score:") || !strings.Contains(r.RatingExplanation, "not_selected_budget") {
		t.Fatalf("unassessed row: %+v", r)
	}
	if r := byID["fast-0"]; r.Severity != "below_threshold" || r.Query != "{sku: <string>}" || len(r.RecommendationRanks) != 2 ||
		r.ImpactShare == nil || *r.ImpactShare != 0.4 || !strings.Contains(r.RatingExplanation, "Limited by documents examined per result: 25000") {
		t.Fatalf("fast row: %+v", r)
	}
	if r := byID["slow-100"]; r.Severity != "high" || len(r.ObservationNotes) != 0 || len(r.Observations) != 1 || r.Observations[0] != "blocking_sort" {
		t.Fatalf("a sort stays an observation and is not a scan note on a 100 score: %+v", r)
	}
	if v.CoverageCounts[EffCoverageAssessed] != 3 || v.CoverageCounts[EffCoveragePartial] != 1 || v.CoverageCounts[EffCoverageBudgetExcluded] != 1 || v.ShapesTotal != 7 {
		t.Fatalf("coverage counts %+v", v.CoverageCounts)
	}
	if len(v.Columns) != 5 || len(v.Unrecorded.Paths) != 2 {
		t.Fatalf("view must explain every column and disclose unrecorded paths: %+v", v)
	}

	SortEfficiencyRows(v.Rows, EffSortImpact)
	order = order[:0]
	for _, r := range v.Rows {
		order = append(order, r.ShapeID)
	}
	if strings.Join(order, ",") != "slow-100,excluded,timeout,fast-0,partial,tie-38,insert" {
		t.Fatalf("impact order %v", order)
	}
}

func TestEfficiencyViewNotReadyListsImpactShapesUnassessed(t *testing.T) {
	rep := InsightsReport{
		Impact: ImpactView{Status: "ready", Coverage: ImpactCoverage{ObservedOperationMs: 0, NotShownGroups: 2}, Groups: []WorkloadImpact{
			{Rank: 1, ShapeID: "a", Operation: "find", Namespace: "shop.a"},
			{Rank: 2, ShapeID: "b", Operation: "insert", Namespace: "shop.b"},
		}},
		ExplainCoverage: NewExplainCoverage("disabled", "explain_disabled"),
	}
	v := BuildEfficiencyView(rep)
	if v.Status != "disabled" || len(v.Rows) != 2 {
		t.Fatalf("view %+v", v)
	}
	for _, r := range v.Rows {
		if r.Score != nil || r.ImpactShare != nil {
			t.Fatalf("no score or share without evidence: %+v", r)
		}
	}
	if v.Rows[0].ScoreLabel != "Unassessed" || v.Rows[0].CoverageReason != "explain_disabled" || v.Rows[1].ScoreLabel != "Not applicable" {
		t.Fatalf("rows %+v", v.Rows)
	}
	if !strings.Contains(strings.Join(v.Limitations, " "), "2 tracked shapes are not in the impact view") {
		t.Fatalf("truncated impact listing must be disclosed: %v", v.Limitations)
	}
	if v.Notice != ExplainDisabledNotice {
		t.Fatalf("a run without explain must say how to enable it: %q", v.Notice)
	}
	// Other unavailable states (failed, not run) are not "turned off" and get no notice.
	for _, cov := range []ExplainCoverageView{NewExplainCoverage("failed", "explain_failed"), NewExplainCoverage("ready", "")} {
		rep.ExplainCoverage = cov
		if n := BuildEfficiencyView(rep).Notice; n != "" {
			t.Fatalf("%s: unexpected notice %q", cov.Status, n)
		}
	}
}
