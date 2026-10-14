package stats

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
)

func execDiag(collscan, ixscan bool, docs, keys, returned int64) *ExplainDiagnostics {
	d := &ExplainDiagnostics{
		ReplayDB:                "shop",
		ReplayCollection:        "orders",
		Verbosity:               "executionStats",
		ExecutionStatsAvailable: true,
		CollectionScanDetected:  collscan,
		IndexScanDetected:       ixscan,
		DocsExamined:            docs,
		KeysExamined:            keys,
		NReturned:               returned,
	}
	if collscan {
		d.WinningPlanSummary = "COLLSCAN"
	} else if ixscan {
		d.WinningPlanSummary = "FETCH -> IXSCAN"
		d.IndexesUsed = []string{"status_1"}
	}
	return d
}

func explainedSample(role string, clientMs float64, d *ExplainDiagnostics) ExplainSample {
	DescribeExplainSample(d, "find")
	return ExplainSample{Role: role, ClientLatencyMs: clientMs, Status: "explained", Attempts: 1, Diagnostics: d}
}

func TestClassifyExplainSample(t *testing.T) {
	planOnly := execDiag(true, false, 0, 0, 0)
	planOnly.ExecutionStatsAvailable = false
	unfiltered := execDiag(true, false, 50000, 0, 50000)
	unfiltered.UnfilteredScanExpected = true
	spill := execDiag(false, true, 200, 200, 100)
	spill.SortDetected, spill.BlockingSortDetected, spill.UsedDisk, spill.Spills = true, true, true, 3
	write := execDiag(true, false, 50000, 0, 0)
	write.NMatched = 50000

	tests := []struct {
		name string
		d    *ExplainDiagnostics
		op   string
		want string
	}{
		{"selective collscan", execDiag(true, false, 50000, 0, 3), "find", DiagCollscanSelective},
		{"collscan returning most docs", execDiag(true, false, 50000, 0, 40000), "find", DiagCollscanLowSelect},
		{"small collection collscan", execDiag(true, false, 200, 0, 1), "find", DiagCollscanLowSelect},
		{"unfiltered collscan", unfiltered, "find", DiagCollscanUnfiltered},
		{"collscan without metrics", planOnly, "find", DiagCollscanPlanOnly},
		{"low selectivity index", execDiag(false, true, 30000, 30000, 10), "find", DiagIndexLowSelectivity},
		{"efficient index", execDiag(false, true, 10, 10, 10), "find", DiagIndexEfficient},
		{"spill", spill, "find", DiagSpillToDisk},
		{"write uses matched count", write, "updateMany", DiagCollscanLowSelect},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyExplainSample(tc.d, tc.op); got != tc.want {
				t.Fatalf("ClassifyExplainSample() = %q, want %q", got, tc.want)
			}
		})
	}
}

func findingShape(samples ...ExplainSample) SlowQueryInsight {
	return SlowQueryInsight{
		ShapeID:        "shape_a",
		Operation:      "find",
		Collection:     "orders",
		ShapeSummary:   "find on fields: status",
		FilterFields:   []string{"status"},
		ExplainSamples: samples,
		ExplainSelection: &ExplainSelection{
			Method: ExplainSelectionMethod, Scope: SampleScopeUniformReservoir,
			RetainedSuccessfulSamples: 40, ExecutableSamples: 40, MedianMs: 12, P95Ms: 90,
		},
	}
}

func TestBuildExplainFindingConfidenceFromEvidenceQuality(t *testing.T) {
	t.Run("two agreeing samples with metrics give high", func(t *testing.T) {
		f := BuildExplainFinding(findingShape(
			explainedSample("median", 12, execDiag(true, false, 50000, 0, 3)),
			explainedSample("p95", 90, execDiag(true, false, 50000, 0, 2)),
		))
		if f == nil || f.Confidence != "high" || f.Diagnosis != DiagCollscanSelective {
			t.Fatalf("expected high-confidence selective collscan, got %+v", f)
		}
	})
	t.Run("single sample caps at medium", func(t *testing.T) {
		f := BuildExplainFinding(findingShape(explainedSample("median+p95", 12, execDiag(true, false, 50000, 0, 3))))
		if f == nil || f.Confidence != "medium" {
			t.Fatalf("expected medium confidence for one sample, got %+v", f)
		}
	})
	t.Run("one inconclusive sample caps at medium", func(t *testing.T) {
		f := BuildExplainFinding(findingShape(
			explainedSample("median", 12, execDiag(true, false, 50000, 0, 3)),
			ExplainSample{Role: "p95", Status: "timed_out", Reason: "max_time_exceeded", Attempts: 2},
		))
		if f == nil || f.Confidence != "medium" || f.SamplesInconclusive != 1 || f.SamplesExplained != 1 {
			t.Fatalf("expected medium with one inconclusive sample, got %+v", f)
		}
	})
	t.Run("disagreeing samples give low", func(t *testing.T) {
		f := BuildExplainFinding(findingShape(
			explainedSample("median", 12, execDiag(true, false, 50000, 0, 3)),
			explainedSample("p95", 90, execDiag(false, true, 10, 10, 10)),
		))
		if f == nil || f.Confidence != "low" || !f.SamplesDisagree {
			t.Fatalf("expected low confidence on disagreement, got %+v", f)
		}
	})
	t.Run("collscan without execution metrics is low and does not claim an index problem", func(t *testing.T) {
		a, b := execDiag(true, false, 0, 0, 0), execDiag(true, false, 0, 0, 0)
		a.ExecutionStatsAvailable, b.ExecutionStatsAvailable = false, false
		f := BuildExplainFinding(findingShape(explainedSample("median", 12, a), explainedSample("p95", 90, b)))
		if f == nil || f.Confidence != "low" || f.Diagnosis != DiagCollscanPlanOnly {
			t.Fatalf("expected low-confidence plan-only collscan, got %+v", f)
		}
		if !strings.Contains(f.Recommendation, "does not prove") {
			t.Fatalf("expected cautious recommendation, got %q", f.Recommendation)
		}
	})
	t.Run("all samples failed yields no finding", func(t *testing.T) {
		f := BuildExplainFinding(findingShape(
			ExplainSample{Role: "median", Status: "timed_out"},
			ExplainSample{Role: "p95", Status: "execution_failed"},
		))
		if f != nil {
			t.Fatalf("expected nil finding when nothing was explained, got %+v", f)
		}
	})
}

func TestBuildExplainFindingCitesEvidence(t *testing.T) {
	f := BuildExplainFinding(findingShape(
		explainedSample("median", 12, execDiag(true, false, 50000, 0, 3)),
		explainedSample("p95", 90, execDiag(true, false, 50000, 0, 2)),
	))
	joined := strings.Join(f.Evidence, " | ")
	for _, want := range []string{"namespace shop.orders", "find on fields: status", "docsExamined=50000", "nReturned=3", "indexes used: none", "2 samples selected", "40 representative samples", "uniform full-run reservoir"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected evidence to contain %q, got %s", want, joined)
		}
	}
	if !strings.Contains(f.Recommendation, "[status]") || !strings.Contains(f.Recommendation, "shop.orders") {
		t.Fatalf("expected recommendation to cite filter fields and namespace, got %q", f.Recommendation)
	}
	if f.TimingNote == "" {
		t.Fatalf("expected timing note")
	}
}

func TestLatencyDisagreementDoesNotChangeDiagnosis(t *testing.T) {
	fast := execDiag(false, true, 10, 10, 10)
	fast.ExecutionTimeMillis = 1
	slow := execDiag(false, true, 10, 10, 10)
	slow.ExecutionTimeMillis = 1
	rep := InsightsReport{SlowQueries: []SlowQueryInsight{findingShape(
		explainedSample("median", 2, fast),
		explainedSample("p95", 900, slow),
	)}}
	ApplyExplainFindings(&rep)
	sq := rep.SlowQueries[0]
	if sq.ExplainSamples[1].LatencyComparison != "client_slower_than_explain" || sq.ExplainSamples[0].LatencyComparison != "similar" {
		t.Fatalf("unexpected latency comparison: %+v", sq.ExplainSamples)
	}
	if sq.ExplainFinding.Diagnosis != DiagIndexEfficient || sq.ExplainFinding.SamplesDisagree {
		t.Fatalf("latency disagreement must not alter the plan diagnosis, got %+v", sq.ExplainFinding)
	}
}

func TestApplyExplainFindingsRebuildsIssuesAndRecommendations(t *testing.T) {
	low := execDiag(false, true, 30000, 30000, 10)
	low2 := execDiag(false, true, 30000, 30000, 12)
	shape := findingShape(explainedSample("median", 12, low), explainedSample("p95", 90, low2))
	other := SlowQueryInsight{ShapeID: "shape_b", Operation: "find", Collection: "payments"}
	impact := ImpactView{Status: "ready", Groups: []WorkloadImpact{{Rank: 1, ShapeID: "shape_b"}, {Rank: 2, ShapeID: "shape_a"}}}
	rep := InsightsReport{
		Impact:      impact,
		SlowQueries: []SlowQueryInsight{shape, other},
		QueryShapes: []SlowQueryInsight{{ShapeID: "shape_a", Operation: "find", Collection: "orders"}},
		PotentialIndexIssues: []IndexIssue{
			{ShapeID: "shape_a", Collection: "orders", Operation: "find", Confidence: "low", Message: "heuristic", Recommendation: "heuristic reco"},
			{ShapeID: "shape_b", Collection: "payments", Operation: "find", Confidence: "medium", Recommendation: "check payments"},
		},
	}
	rep.SlowQueries[1].ExplainStatus = "explain_unavailable"
	rep.SlowQueries[1].ExplainReason = "not_selected_top_n_1"

	ApplyExplainFindings(&rep)

	issue := rep.PotentialIndexIssues[0]
	if issue.Diagnosis != DiagIndexLowSelectivity || issue.EvidenceLevel != "explain_index_low_selectivity" || issue.Confidence != "high" {
		t.Fatalf("expected specific diagnosis on issue, got %+v", issue)
	}
	if strings.Contains(issue.Message, "COLLSCAN") || !strings.Contains(issue.Message, "status_1") {
		t.Fatalf("expected index-specific message, got %q", issue.Message)
	}
	if rep.QueryShapes[0].ExplainFinding == nil || rep.QueryShapes[0].ExplainStatus != "explained" {
		t.Fatalf("expected query shapes to mirror the finding")
	}
	if rep.Metadata.EvidenceLevel != "heuristic_plus_explain_samples" {
		t.Fatalf("expected explain evidence level, got %q", rep.Metadata.EvidenceLevel)
	}
	if !reflect.DeepEqual(rep.Impact, impact) {
		t.Fatalf("impact ranking must not change")
	}

	var explainRec, heuristicRec *InsightRecommendation
	for i := range rep.Recommendations {
		r := &rep.Recommendations[i]
		switch r.Source {
		case "explain":
			explainRec = r
		case "heuristic":
			heuristicRec = r
		}
	}
	if explainRec == nil || explainRec.ShapeID != "shape_a" || explainRec.Confidence != issue.Confidence || explainRec.Diagnosis != issue.Diagnosis {
		t.Fatalf("expected explain recommendation consistent with issue, got %+v", rep.Recommendations)
	}
	if !strings.Contains(explainRec.Details, "keysExamined=30000") || !strings.Contains(explainRec.Details, "status_1") {
		t.Fatalf("expected recommendation to cite metrics and index, got %q", explainRec.Details)
	}
	if heuristicRec == nil || heuristicRec.ShapeID != "shape_b" {
		t.Fatalf("expected heuristic recommendation only for the unexplained shape, got %+v", rep.Recommendations)
	}
	for i, r := range rep.Recommendations {
		if r.Rank != i+1 {
			t.Fatalf("expected sequential ranks, got %+v", rep.Recommendations)
		}
	}
}

func TestApplyExplainFindingsInconclusiveKeepsHeuristicGuidance(t *testing.T) {
	shape := findingShape(
		ExplainSample{Role: "median", Status: "timed_out", Reason: "max_time_exceeded", Attempts: 2},
		ExplainSample{Role: "p95", Status: "timed_out", Reason: "max_time_exceeded", Attempts: 2},
	)
	rep := InsightsReport{
		SlowQueries:          []SlowQueryInsight{shape},
		PotentialIndexIssues: []IndexIssue{{ShapeID: "shape_a", Collection: "orders", Operation: "find", Confidence: "medium", Recommendation: "heuristic reco"}},
	}
	ApplyExplainFindings(&rep)
	sq := rep.SlowQueries[0]
	if sq.ExplainFinding != nil || sq.ExplainStatus != "timed_out" {
		t.Fatalf("expected inconclusive timed_out shape, got status=%q finding=%+v", sq.ExplainStatus, sq.ExplainFinding)
	}
	issue := rep.PotentialIndexIssues[0]
	if issue.EvidenceLevel != "heuristic_explain_inconclusive" || issue.Confidence != "low" || issue.Recommendation != "heuristic reco" {
		t.Fatalf("expected inconclusive heuristic issue, got %+v", issue)
	}
	for _, r := range rep.Recommendations {
		if r.Source == "explain" {
			t.Fatalf("timeouts must not produce explain-backed recommendations: %+v", r)
		}
	}
}

func TestRecordOperationEventCapturesExecutableQueryOnlyWhenExplainEnabled(t *testing.T) {
	q := &ExecutableQuery{Database: "shop", Filter: map[string]interface{}{"email": "secret@example.com"}, Limit: 7}

	off := NewCollector()
	off.ConfigureInsights(&config.AppConfig{InsightsEnabled: true, InsightsSamplingRate: 1, InsightsMaxEvents: 10, InsightsMaxGroups: 10})
	if off.CapturesExecutableQueries() {
		t.Fatalf("capture must be off when explain is disabled")
	}
	off.RecordOperationEvent("find", "shop", "users", "k", "s", nil, time.Millisecond, true, 1, q, "", "", "", "", "", "", "", 0)
	if got := off.SnapshotRepresentativeSamples(); len(got) != 0 {
		t.Fatalf("expected no executable query without explain, got %d samples", len(got))
	}

	on := NewCollector()
	on.ConfigureInsights(&config.AppConfig{InsightsEnabled: true, InsightsSamplingRate: 1, InsightsMaxEvents: 10, InsightsMaxGroups: 10, InsightsExplainEnabled: true})
	if !on.CapturesExecutableQueries() {
		t.Fatalf("capture must be on when explain is enabled")
	}
	on.RecordOperationEvent("find", "shop", "users", "k", "s", nil, time.Millisecond, true, 1, q, "", "", "", "", "", "", "", 0)
	q.Filter["email"] = "mutated"
	smp := on.SnapshotRepresentativeSamples()[0]
	if smp.Query == nil || smp.Query.Filter["email"] != "secret@example.com" || smp.Query.Limit != 7 || smp.Query.Database != "shop" {
		t.Fatalf("expected deep-cloned executable query, got %+v", smp.Query)
	}
	b, _ := json.Marshal(smp)
	if strings.Contains(string(b), "secret@example.com") {
		t.Fatalf("executable query values must not be serialized: %s", b)
	}
}
