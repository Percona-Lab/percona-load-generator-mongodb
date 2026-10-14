package webui

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/db"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// stubExplainJobs replaces the explain runner and records every job it receives.
func stubExplainJobs(t *testing.T, result func(explainJob) explainEvidence) *[]explainJob {
	t.Helper()
	var seen []explainJob
	orig := runExplainJobsFn
	runExplainJobsFn = func(ctx context.Context, jobs []explainJob, workers int, cfg *config.AppConfig, maxTimeMs int, verbosity string, retries int, backoffMs int) []explainEvidence {
		out := make([]explainEvidence, len(jobs))
		for i, j := range jobs {
			seen = append(seen, j)
			out[i] = result(j)
		}
		return out
	}
	t.Cleanup(func() { runExplainJobsFn = orig })
	return &seen
}

func indexedFindEvidence(j explainJob) explainEvidence {
	d := &stats.ExplainDiagnostics{
		ReplayDB: j.sample.Query.Database, ReplayCollection: j.sample.Collection, Verbosity: "executionStats",
		ExecutionStatsAvailable: true, IndexScanDetected: true, FetchDetected: true, WinningPlanSummary: "FETCH <- IXSCAN",
		IndexesUsed: []string{"k_1"}, KeysExamined: 1, DocsExamined: 1, NReturned: 1,
	}
	stats.DescribeExplainSample(d, j.sample.Operation)
	return explainEvidence{status: "explained", reason: d.Diagnosis, attempts: 1, diag: d}
}

// fastShape has distinct executable samples so median and p95 picks differ.
func fastShape(key string, rank int, count uint64, avgMs float64) stats.ExplainInventoryShape {
	s := invShape("find", key, rank, count, avgMs, 0)
	s.Samples = shapeSamples("find", key, []float64{avgMs * 0.5, avgMs, avgMs * 1.5, avgMs * 4}, true)
	return s
}

func coverageShape(t *testing.T, cov stats.ExplainCoverageView, key string) stats.ExplainCoverageShape {
	t.Helper()
	id := stats.StableShapeID("find", "orders", key)
	for _, s := range cov.Shapes {
		if s.ShapeID == id {
			return s
		}
	}
	t.Fatalf("shape %q missing from coverage: %+v", key, cov.Shapes)
	return stats.ExplainCoverageShape{}
}

func readyReport(thresholdMs float64) stats.InsightsReport {
	return stats.InsightsReport{Metadata: stats.InsightsMetadata{Status: "ready", SlowThresholdMs: thresholdMs, SamplingRate: 1}}
}

func efficiencyParams(topN int) explainParams {
	p := defaultParams(topN, "high_only")
	p.efficiencyMode = config.ExplainEfficiencyModeDefault
	return p
}

func TestEnrichExplainsFastOnlyShapeWithNoSlowQueries(t *testing.T) {
	jobs := stubExplainJobs(t, indexedFindEvidence)
	rep := readyReport(200)
	inv := stats.ExplainInventory{CaptureStatus: "ready", Shapes: []stats.ExplainInventoryShape{fastShape("fast", 1, 5000, 0.4)}}

	enrichInsightsWithExplain(context.Background(), "ready", &rep, inv, &config.AppConfig{}, efficiencyParams(5))

	cov := rep.ExplainCoverage
	s := coverageShape(t, cov, "fast")
	if s.Status != "explained" || s.SelectedBy != "impact_rank" || !s.BelowSlowThreshold || s.Evidence != "execution_stats" {
		t.Fatalf("fast shape must be explained by an efficiency slot: %+v", s)
	}
	if s.Finding == nil || len(s.Samples) != 2 || s.Samples[0].Role != "median" || s.Samples[1].Role != "p95" {
		t.Fatalf("expected a finding from median and p95 samples, got finding=%+v samples=%+v", s.Finding, s.Samples)
	}
	if s.LatencyTier != "lt_1ms" || s.Selection == nil || s.Selection.Scope != stats.SampleScopeUniformReservoir {
		t.Fatalf("expected tier and sample scope disclosure, got %+v", s)
	}
	if len(*jobs) != 2 || cov.SelectedShapes != 1 || cov.CompletedShapes != 1 || cov.SelectedBelowSlowThreshold != 1 || cov.CommandsRun != 2 {
		t.Fatalf("unexpected coverage: jobs=%d %+v", len(*jobs), cov)
	}
	if len(rep.SlowQueries) != 0 {
		t.Fatalf("efficiency selection must not add shapes to the slow view")
	}
	if len(cov.UnrecordedPaths) != 2 || cov.Sampling.CaptureStatus != "ready" {
		t.Fatalf("expected unrecorded-path and capture disclosures, got %+v", cov)
	}
}

func TestEnrichUsesEachShapesOwnRepresentativeSamples(t *testing.T) {
	jobs := stubExplainJobs(t, indexedFindEvidence)
	rep := readyReport(200)
	a, b := fastShape("a", 1, 100, 5), fastShape("b", 2, 50, 50)
	inv := stats.ExplainInventory{CaptureStatus: "ready", Shapes: []stats.ExplainInventoryShape{a, b}}

	enrichInsightsWithExplain(context.Background(), "ready", &rep, inv, &config.AppConfig{}, efficiencyParams(2))

	if len(*jobs) != 4 {
		t.Fatalf("expected 2 samples for each of 2 shapes, got %d jobs", len(*jobs))
	}
	for _, j := range *jobs {
		key := j.sample.Query.Filter["k"]
		if want := stats.StableShapeID("find", "orders", key.(string)); j.sample.ShapeID != want {
			t.Fatalf("job query from shape %v was attributed to %s", key, j.sample.ShapeID)
		}
	}
	for _, s := range []stats.ExplainInventoryShape{a, b} {
		cs := coverageShape(t, rep.ExplainCoverage, s.ShapeKey)
		for _, smp := range cs.Samples {
			found := false
			for _, own := range s.Samples {
				if own.DurationMs == smp.ClientLatencyMs {
					found = true
				}
			}
			if !found {
				t.Fatalf("shape %s sample latency %.2f is not one of its own samples", s.ShapeKey, smp.ClientLatencyMs)
			}
		}
	}
}

func TestEfficiencySelectionIsIndependentOfSeverityFilter(t *testing.T) {
	stubExplainJobs(t, indexedFindEvidence)
	low := fastShape("low", 2, 100, 50)
	inv := stats.ExplainInventory{CaptureStatus: "ready", Shapes: []stats.ExplainInventoryShape{fastShape("hot", 1, 10000, 2), low}}

	var selectedBy []map[string]string
	for _, mode := range []string{"high_only", "critical_only", "medium_only", "high_and_low"} {
		rep := readyReport(10)
		rep.SlowQueries = []stats.SlowQueryInsight{{ShapeID: low.ShapeID, Operation: "find", Collection: "orders", ShapeKey: "low", Severity: "low", P95Ms: 60, P99Ms: 80}}
		p := efficiencyParams(1)
		p.severityMode = mode
		enrichInsightsWithExplain(context.Background(), "ready", &rep, inv, &config.AppConfig{}, p)

		got := map[string]string{}
		for _, s := range rep.ExplainCoverage.Shapes {
			if s.SelectedBy != "" {
				got[s.ShapeID] = s.SelectedBy
			}
		}
		selectedBy = append(selectedBy, got)
		hot := coverageShape(t, rep.ExplainCoverage, "hot")
		if hot.SelectedBy != "impact_rank" || hot.Status != "explained" {
			t.Fatalf("%s: efficiency slot must pick the top-impact shape regardless of severity: %+v", mode, hot)
		}
		if mode == "high_only" || mode == "critical_only" || mode == "medium_only" {
			if rep.SlowQueries[0].ExplainReason != "low_value_shape_filtered" {
				t.Fatalf("%s: severity filter meaning changed: %+v", mode, rep.SlowQueries[0])
			}
		}
	}
	if !jsonEqualMaps(selectedBy[0], selectedBy[1]) || !jsonEqualMaps(selectedBy[0], selectedBy[2]) {
		t.Fatalf("efficiency selection changed with severity mode: %v", selectedBy)
	}
	if selectedBy[3][low.ShapeID] != "severity" {
		t.Fatalf("high_and_low severity slot should still take the low shape: %v", selectedBy[3])
	}
}

func jsonEqualMaps(a, b map[string]string) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

func TestEfficiencySlotForSlowViewShapeAttachesSamplesToSlowQuery(t *testing.T) {
	stubExplainJobs(t, indexedFindEvidence)
	low := fastShape("low", 1, 100, 50)
	rep := readyReport(10)
	rep.SlowQueries = []stats.SlowQueryInsight{{ShapeID: low.ShapeID, Operation: "find", Collection: "orders", ShapeKey: "low", Severity: "low", P95Ms: 60, P99Ms: 80}}
	inv := stats.ExplainInventory{CaptureStatus: "ready", Shapes: []stats.ExplainInventoryShape{low}}

	enrichInsightsWithExplain(context.Background(), "ready", &rep, inv, &config.AppConfig{}, efficiencyParams(1))

	sq := rep.SlowQueries[0]
	cs := coverageShape(t, rep.ExplainCoverage, "low")
	if cs.SelectedBy != "impact_rank" || sq.ExplainStatus != "explained" || sq.ExplainFinding == nil || len(sq.ExplainSamples) != len(cs.Samples) {
		t.Fatalf("expected slow view and coverage to share the efficiency explain: sq=%+v cs=%+v", sq, cs)
	}
}

func TestEfficiencySlotsRespectBudgetAndReachFastLowImpactShapes(t *testing.T) {
	jobs := stubExplainJobs(t, indexedFindEvidence)
	rep := readyReport(200)
	inv := stats.ExplainInventory{CaptureStatus: "ready"}
	for i := 0; i < 19; i++ {
		inv.Shapes = append(inv.Shapes, fastShape(string(rune('a'+i)), i+1, uint64(1000-i), 1500))
	}
	inv.Shapes = append(inv.Shapes, fastShape("tiny", 20, 3, 0.2))

	enrichInsightsWithExplain(context.Background(), "ready", &rep, inv, &config.AppConfig{}, efficiencyParams(3))

	cov := rep.ExplainCoverage
	if cov.Budget.EfficiencySlots != 3 || cov.Budget.ImpactSlots != 2 || cov.Budget.LatencyTierSlots != 1 || cov.Budget.MaxPlannedSamples != 12 {
		t.Fatalf("unexpected budget: %+v", cov.Budget)
	}
	if cov.SelectedShapes != 3 || cov.SelectedByImpact != 2 || cov.SelectedByLatencyTier != 1 {
		t.Fatalf("expected 2 impact + 1 latency-tier selections, got %+v", cov)
	}
	if cov.ExcludedShapes != 17 || cov.ExcludedByReason["not_selected_budget"] != 17 || cov.EligibleShapes != 20 {
		t.Fatalf("expected 17 shapes excluded by budget out of 20, got %+v", cov)
	}
	if len(*jobs) > cov.Budget.MaxPlannedSamples || cov.SamplesPlanned > cov.Budget.MaxPlannedSamples {
		t.Fatalf("planned %d samples / %d jobs exceed budget %d", cov.SamplesPlanned, len(*jobs), cov.Budget.MaxPlannedSamples)
	}
	tiny := coverageShape(t, cov, "tiny")
	if tiny.SelectedBy != "latency_tier" || tiny.Status != "explained" || tiny.ImpactRank != 20 {
		t.Fatalf("the fastest, lowest-impact shape must win the latency-tier slot: %+v", tiny)
	}
	if a := coverageShape(t, cov, "a"); a.SelectedBy != "impact_rank" {
		t.Fatalf("top-impact shape must take the first slot: %+v", a)
	}
	if c := coverageShape(t, cov, "c"); c.Status != "excluded" || c.Reason != "not_selected_budget" {
		t.Fatalf("unselected shapes must be reported excluded, not assessed: %+v", c)
	}
	if !strings.Contains(strings.Join(cov.Limitations, " "), "unassessed, not efficient") {
		t.Fatalf("expected budget exclusion disclosure, got %v", cov.Limitations)
	}
}

func TestEfficiencyModeOffKeepsOnlySeveritySlots(t *testing.T) {
	jobs := stubExplainJobs(t, indexedFindEvidence)
	rep := readyReport(200)
	inv := stats.ExplainInventory{CaptureStatus: "ready", Shapes: []stats.ExplainInventoryShape{fastShape("fast", 1, 100, 1)}}
	enrichInsightsWithExplain(context.Background(), "ready", &rep, inv, &config.AppConfig{}, defaultParams(5, "high_only"))
	cov := rep.ExplainCoverage
	if len(*jobs) != 0 || cov.SelectedShapes != 0 || cov.ExcludedByReason["not_selected_efficiency_mode_off"] != 1 || rep.Metadata.ExplainMaxSamples != 10 {
		t.Fatalf("efficiency mode off must not explain fast shapes: jobs=%d %+v", len(*jobs), cov)
	}
}

func TestEnrichDeduplicatesIdenticalCommandsAcrossShapes(t *testing.T) {
	jobs := stubExplainJobs(t, indexedFindEvidence)
	rep := readyReport(200)
	a, b := fastShape("a", 1, 100, 5), fastShape("b", 2, 90, 5)
	for _, s := range []*stats.ExplainInventoryShape{&a, &b} {
		for i := range s.Samples {
			s.Samples[i].Query.Filter = map[string]interface{}{"x": 1, "y": "same"}
		}
	}
	b.Samples[0].Query.Filter = map[string]interface{}{"y": "same", "x": 1}
	inv := stats.ExplainInventory{CaptureStatus: "ready", Shapes: []stats.ExplainInventoryShape{a, b}}

	enrichInsightsWithExplain(context.Background(), "ready", &rep, inv, &config.AppConfig{}, efficiencyParams(2))

	cov := rep.ExplainCoverage
	if len(*jobs) != 1 || cov.SamplesPlanned != 2 || cov.CommandsDeduplicated != 1 || cov.CommandsRun != 1 {
		t.Fatalf("identical commands must run once: jobs=%d planned=%d dedup=%d run=%d", len(*jobs), cov.SamplesPlanned, cov.CommandsDeduplicated, cov.CommandsRun)
	}
	if coverageShape(t, cov, "a").Status != "explained" || coverageShape(t, cov, "b").Status != "explained" {
		t.Fatalf("both shapes must receive the shared result: %+v", cov.Shapes)
	}
	if sa, sb := coverageShape(t, cov, "a").Samples, coverageShape(t, cov, "b").Samples; sa[0].SharedCommand || !sb[0].SharedCommand {
		t.Fatalf("only the reused sample is marked as a shared command: a=%v b=%v", sa[0].SharedCommand, sb[0].SharedCommand)
	}
}

func TestEnrichCoverageCountsStatusesAndReasons(t *testing.T) {
	stubExplainJobs(t, func(j explainJob) explainEvidence {
		switch j.sample.ShapeKey {
		case "fails":
			return explainEvidence{status: "timed_out", reason: "max_time_exceeded", attempts: 2}
		case "out":
			d := &stats.ExplainDiagnostics{ReplayDB: "shop", Verbosity: "queryPlanner", VerbosityNote: "pipeline_has_out_or_merge_explained_with_queryPlanner", IndexScanDetected: true, WinningPlanSummary: "IXSCAN"}
			stats.DescribeExplainSample(d, "aggregate")
			return explainEvidence{status: "explained", attempts: 1, diag: d}
		}
		return indexedFindEvidence(j)
	})
	rep := readyReport(200)
	ok := fastShape("ok", 1, 100, 2)
	fails := fastShape("fails", 2, 90, 2)
	noQuery := invShape("find", "noquery", 3, 80, 2, 0)
	noQuery.Samples, noQuery.UnavailableReason, noQuery.SampleScope = nil, "no_executable_query", ""
	insert := invShape("insert", "ins", 4, 70, 2, 0)
	insert.Samples = nil
	agg := func(key string, stage map[string]interface{}) stats.ExplainInventoryShape {
		s := invShape("aggregate", key, 5, 60, 2, 0)
		s.Samples = shapeSamples("aggregate", key, []float64{1, 2}, true)
		for i := range s.Samples {
			s.Samples[i].Query = &stats.ExecutableQuery{Database: "shop", Pipeline: []interface{}{
				map[string]interface{}{"$match": map[string]interface{}{"n": i}}, stage,
			}}
		}
		return s
	}
	unordered := agg("unordered", map[string]interface{}{"$sort": map[string]interface{}{"a": 1, "b": -1}})
	out := agg("out", map[string]interface{}{"$out": "report"})
	inv := stats.ExplainInventory{CaptureStatus: "ready", UnorderedSortShapes: 1, UntrackedOperations: 7, GroupLimit: 6,
		Shapes: []stats.ExplainInventoryShape{ok, fails, noQuery, insert, unordered, out}}

	enrichInsightsWithExplain(context.Background(), "ready", &rep, inv, &config.AppConfig{}, efficiencyParams(5))

	cov := rep.ExplainCoverage
	if cov.TrackedShapes != 6 || cov.EligibleShapes != 5 || cov.NotSupportedShapes != 1 {
		t.Fatalf("unexpected inventory counts: %+v", cov)
	}
	if cov.SelectedShapes != 3 || cov.CompletedShapes != 2 || cov.PartialShapes != 1 || cov.FailedShapes != 1 || cov.UnavailableShapes != 2 {
		t.Fatalf("unexpected outcome counts: %+v", cov)
	}
	if cov.UnavailableByReason["no_executable_query"] != 1 || cov.UnavailableByReason["sort_key_order_undefined"] != 1 {
		t.Fatalf("explicit unavailable reasons must be preserved: %v", cov.UnavailableByReason)
	}
	if s := coverageShape(t, cov, "fails"); s.Status != "failed" || s.Reason != "timed_out:max_time_exceeded" {
		t.Fatalf("expected failed shape with reason, got %+v", s)
	}
	var outShape stats.ExplainCoverageShape
	for _, s := range cov.Shapes {
		if s.Operation == "aggregate" && s.Status == "explained" {
			outShape = s
		}
	}
	if outShape.Evidence != "planner_only" || outShape.PartialReason != "pipeline_has_out_or_merge_explained_with_queryPlanner" {
		t.Fatalf("planner-only evidence must stay partial with its reason: %+v", outShape)
	}
	joined := strings.Join(cov.Limitations, " | ")
	for _, want := range []string{"sort_key_order_undefined", "7 operations belong to untracked shapes", "planner-only"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected limitation %q, got %s", want, joined)
		}
	}
	if cov.Sampling.ShapesUniformReservoir != 4 || cov.Sampling.ShapesWithoutSamples != 1 || cov.Sampling.UntrackedOperations != 7 {
		t.Fatalf("unexpected sampling coverage: %+v", cov.Sampling)
	}
}

func TestEnrichWithCaptureDisabledReportsUnavailable(t *testing.T) {
	jobs := stubExplainJobs(t, indexedFindEvidence)
	rep := readyReport(200)
	s := fastShape("fast", 1, 10, 1)
	s.Samples = nil
	inv := stats.ExplainInventory{CaptureStatus: "disabled", CaptureDisabledReason: "explain_disabled", Shapes: []stats.ExplainInventoryShape{s}}
	enrichInsightsWithExplain(context.Background(), "ready", &rep, inv, &config.AppConfig{}, efficiencyParams(5))
	cov := rep.ExplainCoverage
	if len(*jobs) != 0 || cov.UnavailableByReason["capture_disabled_explain_disabled"] != 1 || cov.Sampling.CaptureStatus != "disabled" || cov.Sampling.CaptureDisabledReason != "explain_disabled" {
		t.Fatalf("capture-disabled state must be disclosed: %+v", cov)
	}
}

func TestEnrichNotReadyKeepsDisclosures(t *testing.T) {
	jobs := stubExplainJobs(t, indexedFindEvidence)
	rep := readyReport(200)
	rep.Metadata.Status = "empty"
	inv := stats.ExplainInventory{CaptureStatus: "ready", Shapes: []stats.ExplainInventoryShape{fastShape("fast", 1, 10, 1)}}
	enrichInsightsWithExplain(context.Background(), "empty", &rep, inv, &config.AppConfig{}, efficiencyParams(5))
	if len(*jobs) != 0 || rep.ExplainCoverage.Status != "not_run" || rep.ExplainCoverage.Reason != "insights_status_empty" || len(rep.ExplainCoverage.UnrecordedPaths) != 2 {
		t.Fatalf("unexpected not-ready coverage: %+v", rep.ExplainCoverage)
	}
}

func TestRunExplainJobsCancelledContextSendsNoCommands(t *testing.T) {
	var connects int32
	origConnect := connectFn
	connectFn = func(ctx context.Context, cfg *config.AppConfig, dbName string) (*db.Connection, error) {
		atomic.AddInt32(&connects, 1)
		return nil, context.DeadlineExceeded
	}
	defer func() { connectFn = origConnect }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	jobs := []explainJob{{sample: fastShape("a", 1, 1, 1).Samples[0]}, {sample: fastShape("b", 1, 1, 1).Samples[0]}, {sample: fastShape("c", 1, 1, 1).Samples[0]}}
	res := runExplainJobsConcurrently(ctx, jobs, 2, &config.AppConfig{}, 1000, "executionStats", 1, 0)
	if got := atomic.LoadInt32(&connects); got != 0 {
		t.Fatalf("cancelled explain must not connect, got %d connects", got)
	}
	for i, r := range res {
		if r.status != "cancelled" || r.reason != "explain_cancelled" || r.attempts != 0 {
			t.Fatalf("job %d: expected cancelled with no attempts, got %+v", i, r)
		}
	}
}

func TestRunExplainJobsStopsAfterMidRunCancellation(t *testing.T) {
	var connects int32
	ctx, cancel := context.WithCancel(context.Background())
	origConnect := connectFn
	connectFn = func(c context.Context, cfg *config.AppConfig, dbName string) (*db.Connection, error) {
		atomic.AddInt32(&connects, 1)
		cancel()
		return nil, c.Err()
	}
	defer func() { connectFn = origConnect }()

	jobs := make([]explainJob, 5)
	for i := range jobs {
		jobs[i] = explainJob{sample: fastShape(string(rune('a'+i)), 1, 1, 1).Samples[0]}
	}
	res := runExplainJobsConcurrently(ctx, jobs, 1, &config.AppConfig{}, 1000, "executionStats", 1, 0)
	if got := atomic.LoadInt32(&connects); got != 1 {
		t.Fatalf("expected one connect attempt before cancellation, got %d", got)
	}
	for i, r := range res {
		if r.status != "cancelled" {
			t.Fatalf("job %d: expected cancelled, got %+v", i, r)
		}
	}
}

func TestRetryExplainEvidenceStopsOnCancellationDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	start := time.Now()
	got := retryExplainEvidence(ctx, 500, 3, 10000, func(int) explainEvidence {
		calls++
		cancel()
		return explainEvidence{status: "timed_out", reason: "max_time_exceeded"}
	})
	if calls != 1 || got.status != "cancelled" || got.attempts != 1 {
		t.Fatalf("expected cancellation after 1 attempt, got calls=%d %+v", calls, got)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("backoff must not wait after cancellation")
	}
}

func TestEnrichCancelledReportsCancelledShapes(t *testing.T) {
	var connects int32
	origConnect := connectFn
	connectFn = func(ctx context.Context, cfg *config.AppConfig, dbName string) (*db.Connection, error) {
		atomic.AddInt32(&connects, 1)
		return nil, context.DeadlineExceeded
	}
	defer func() { connectFn = origConnect }()

	rep, inv := explainReport(2, 4)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	enrichInsightsWithExplain(ctx, "ready", &rep, inv, &config.AppConfig{}, defaultParams(2, "high_only"))
	cov := rep.ExplainCoverage
	if atomic.LoadInt32(&connects) != 0 || cov.CancelledShapes != 2 || cov.CommandsRun != 0 {
		t.Fatalf("expected 2 cancelled shapes and no commands, got %+v", cov)
	}
	if rep.SlowQueries[0].ExplainStatus != "cancelled" || rep.PotentialIndexIssues[0].EvidenceLevel != "heuristic_explain_cancelled" {
		t.Fatalf("cancellation must be disclosed on the slow view and issues: %+v %+v", rep.SlowQueries[0], rep.PotentialIndexIssues[0])
	}
}

func TestBuildExplainCommandSpecRejectsUndefinedSortOrder(t *testing.T) {
	agg := func(pipeline []interface{}) stats.RepresentativeSample {
		return stats.RepresentativeSample{Operation: "aggregate", Collection: "orders", Query: &stats.ExecutableQuery{Database: "shop", Pipeline: pipeline}}
	}
	cases := []struct {
		name     string
		pipeline []interface{}
		want     string
	}{
		{"multi-key map sort", []interface{}{map[string]interface{}{"$sort": map[string]interface{}{"a": 1, "b": -1}}}, "sort_key_order_undefined"},
		{"nested facet sort", []interface{}{map[string]interface{}{"$facet": map[string]interface{}{"f": []interface{}{map[string]interface{}{"$sort": map[string]interface{}{"a": 1, "b": 1}}}}}}, "sort_key_order_undefined"},
		{"lookup pipeline sort", []interface{}{map[string]interface{}{"$lookup": map[string]interface{}{"from": "x", "as": "y", "pipeline": []interface{}{map[string]interface{}{"$sort": map[string]interface{}{"a": 1, "b": 1}}}}}}, "sort_key_order_undefined"},
		{"single-key map sort", []interface{}{map[string]interface{}{"$sort": map[string]interface{}{"a": 1}}}, ""},
		{"ordered sort", []interface{}{map[string]interface{}{"$sort": bson.D{{Key: "b", Value: -1}, {Key: "a", Value: 1}}}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, reason := buildExplainCommandSpec(agg(tc.pipeline), 1000, "executionStats")
			if reason != tc.want {
				t.Fatalf("reason=%q want %q", reason, tc.want)
			}
			if tc.want == "" {
				if v, _ := field(innerExplain(t, spec), "pipeline"); !jsonEqual(v, tc.pipeline) {
					t.Fatalf("pipeline not preserved: %v", v)
				}
			}
		})
	}
	spec, _ := buildExplainCommandSpec(agg([]interface{}{map[string]interface{}{"$sort": bson.D{{Key: "b", Value: -1}, {Key: "a", Value: 1}}}}), 1000, "executionStats")
	b, _ := bson.MarshalExtJSON(spec.cmd, false, false)
	if !strings.Contains(string(b), `{"b":-1,"a":1}`) {
		t.Fatalf("ordered sort keys must keep their order, got %s", b)
	}
}
