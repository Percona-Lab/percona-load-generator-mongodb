package stats

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
)

func newImpactCollector(samplingRate float64, maxEvents, maxGroups int) *Collector {
	c := NewCollector()
	c.ConfigureInsights(&config.AppConfig{
		InsightsEnabled:         true,
		InsightsSamplingRate:    samplingRate,
		InsightsSlowThresholdMs: 200,
		InsightsMaxEvents:       maxEvents,
		InsightsMaxGroups:       maxGroups,
	})
	return c
}

func recordImpactOps(c *Collector, coll, shape string, n int, d time.Duration) {
	for i := 0; i < n; i++ {
		c.RecordOperationEvent("find", "shop", coll, shape, shape+" summary", []string{"status"}, d, true, 1, nil, "", "unit_test", "test_workload", "", "", "", "", 0)
	}
}

// recordImpactFixture records 1,000 fast ops and 7 slow ops, interleaved so the
// slow shape is not confined to a single sampling block.
func recordImpactFixture(c *Collector) {
	for i := 0; i < 1000; i++ {
		recordImpactOps(c, "orders", "fast_shape", 1, 60*time.Millisecond)
		if i%150 == 0 {
			recordImpactOps(c, "audit", "slow_shape", 1, 1200*time.Millisecond)
		}
	}
}

func findImpact(t *testing.T, v ImpactView, shape string) WorkloadImpact {
	t.Helper()
	for _, g := range v.Groups {
		if g.ShapeKey == shape {
			return g
		}
	}
	t.Fatalf("impact group %q not found in %+v", shape, v.Groups)
	return WorkloadImpact{}
}

func TestImpactRanksFrequentFastShapeAboveRareSlowShape(t *testing.T) {
	c := newImpactCollector(1, 5000, 300)
	recordImpactFixture(c)

	rep := c.GetFinalInsights()
	imp := rep.Impact
	if imp.Status != "ready" || len(imp.Groups) != 2 {
		t.Fatalf("expected 2 ready impact groups, got status=%q groups=%+v", imp.Status, imp.Groups)
	}
	first, second := imp.Groups[0], imp.Groups[1]
	if first.ShapeKey != "fast_shape" || first.Rank != 1 || second.ShapeKey != "slow_shape" || second.Rank != 2 {
		t.Fatalf("expected fast_shape ranked above slow_shape, got %+v", imp.Groups)
	}
	if first.OperationCount != 1000 || first.TotalOperationMs != 60000 {
		t.Fatalf("expected 1000 ops / 60000ms, got %d / %.2f", first.OperationCount, first.TotalOperationMs)
	}
	if second.OperationCount != 7 || second.TotalOperationMs != 8400 {
		t.Fatalf("expected 7 ops / 8400ms, got %d / %.2f", second.OperationCount, second.TotalOperationMs)
	}
	if imp.Coverage.ObservedOperations != 1007 || imp.Coverage.ObservedOperationMs != 68400 {
		t.Fatalf("expected observed 1007 ops / 68400ms, got %+v", imp.Coverage)
	}
	if got, want := first.ShareOfObservedTime, 60000.0/68400.0; got < want-1e-9 || got > want+1e-9 {
		t.Fatalf("share = %v, want %v", got, want)
	}
	if first.Namespace != "shop.orders" || imp.Coverage.TimeBasis != ImpactTimeBasis {
		t.Fatalf("unexpected namespace/time basis: %q %q", first.Namespace, imp.Coverage.TimeBasis)
	}
	for _, want := range []string{"find", "shop.orders", "1000 times", "60000.00 ms", "summed concurrent operation time", "87.7%"} {
		if !strings.Contains(first.Explanation, want) {
			t.Fatalf("explanation %q missing %q", first.Explanation, want)
		}
	}
	if strings.Contains(strings.ToLower(first.Explanation), "index") {
		t.Fatalf("measured explanation must not contain index advice: %q", first.Explanation)
	}
	if !strings.Contains(imp.Coverage.TimeBasisNote, "not wall-clock") || !strings.Contains(imp.Coverage.TimeBasisNote, "not server CPU") {
		t.Fatalf("time basis note must disclaim wall-clock and CPU: %q", imp.Coverage.TimeBasisNote)
	}

	// Severity view is unchanged: the fast shape never crosses the threshold.
	if len(rep.SlowQueries) != 1 || rep.SlowQueries[0].Collection != "audit" || rep.SlowQueries[0].Severity != "high" {
		t.Fatalf("expected severity view to keep only the slow audit shape, got %+v", rep.SlowQueries)
	}
	if rep.Metadata.FilteredByThreshold != 1 {
		t.Fatalf("expected fast shape filtered by threshold in severity view, got %d", rep.Metadata.FilteredByThreshold)
	}
}

func TestImpactPercentileSamplesSeparateFromRunCounts(t *testing.T) {
	c := newImpactCollector(1, 5000, 300)
	recordImpactFixture(c)
	imp := c.GetFinalInsights().Impact

	fast := findImpact(t, imp, "fast_shape")
	if fast.SampleCount != 1000 || fast.SampleP95Ms == nil || *fast.SampleP95Ms != 60 || fast.SamplePercentileStatus != "from_retained_samples" {
		t.Fatalf("expected fast shape sample p95 from 1000 samples, got %+v", fast)
	}
	slow := findImpact(t, imp, "slow_shape")
	if slow.SampleCount != 7 || slow.SampleP95Ms != nil || slow.SamplePercentileStatus != "insufficient_samples" {
		t.Fatalf("expected insufficient samples for 7-sample shape, got %+v", slow)
	}
}

func TestImpactTotalsSurviveSampleBufferOverwrite(t *testing.T) {
	c := newImpactCollector(1, 10, 300)
	recordImpactFixture(c)
	imp := c.GetFinalInsights().Impact

	fast := findImpact(t, imp, "fast_shape")
	slow := findImpact(t, imp, "slow_shape")
	if fast.OperationCount != 1000 || fast.TotalOperationMs != 60000 || slow.OperationCount != 7 || slow.TotalOperationMs != 8400 {
		t.Fatalf("totals changed by buffer overwrite: fast=%+v slow=%+v", fast, slow)
	}
	if !imp.Coverage.SampleBufferWrapped || imp.Coverage.RetainedSampleEvents != 10 || imp.Coverage.SampledInEvents != 1007 {
		t.Fatalf("expected wrapped buffer disclosure, got %+v", imp.Coverage)
	}
	if fast.SampleCount > 10 || fast.SamplePercentileStatus != "insufficient_samples" {
		t.Fatalf("expected retained-only sample count for fast shape, got %+v", fast)
	}
	if !containsSubstring(imp.Coverage.Limitations, "Sample buffer wrapped") {
		t.Fatalf("expected wrapped limitation, got %v", imp.Coverage.Limitations)
	}
}

func TestImpactIndependentOfSamplingRate(t *testing.T) {
	full := newImpactCollector(1, 5000, 300)
	sparse := newImpactCollector(0.1, 5000, 300)
	recordImpactFixture(full)
	recordImpactFixture(sparse)
	a, b := full.GetFinalInsights().Impact, sparse.GetFinalInsights().Impact

	if a.Coverage.ObservedOperations != b.Coverage.ObservedOperations || a.Coverage.ObservedOperationMs != b.Coverage.ObservedOperationMs {
		t.Fatalf("observed totals depend on sampling: %+v vs %+v", a.Coverage, b.Coverage)
	}
	if len(a.Groups) != len(b.Groups) {
		t.Fatalf("group count depends on sampling: %d vs %d", len(a.Groups), len(b.Groups))
	}
	for i := range a.Groups {
		ga, gb := a.Groups[i], b.Groups[i]
		if ga.ImpactID != gb.ImpactID || ga.OperationCount != gb.OperationCount || ga.TotalOperationMs != gb.TotalOperationMs || ga.ShareOfObservedTime != gb.ShareOfObservedTime {
			t.Fatalf("rank %d differs with sampling: %+v vs %+v", i+1, ga, gb)
		}
	}
	if b.Coverage.SampledInEvents >= b.Coverage.ObservedOperations {
		t.Fatalf("expected sparse sampling to sample fewer events than observed ops, got %+v", b.Coverage)
	}
	if !containsSubstring(b.Coverage.Limitations, "never extrapolated") {
		t.Fatalf("expected sampling limitation, got %v", b.Coverage.Limitations)
	}
}

func TestImpactGroupLimitDisclosesOverflow(t *testing.T) {
	c := newImpactCollector(1, 5000, 2)
	recordImpactOps(c, "a", "shape_a", 10, 10*time.Millisecond)
	recordImpactOps(c, "b", "shape_b", 5, 10*time.Millisecond)
	recordImpactOps(c, "c", "shape_c", 30, 100*time.Millisecond)
	imp := c.GetFinalInsights().Impact

	if len(imp.Groups) != 2 || imp.Coverage.TrackedGroups != 2 || imp.Coverage.GroupLimit != 2 {
		t.Fatalf("expected 2 tracked groups at limit 2, got %+v", imp)
	}
	cov := imp.Coverage
	if cov.GroupCoverageComplete || cov.OverflowOperations != 30 || cov.OverflowOperationMs != 3000 {
		t.Fatalf("expected overflow of 30 ops / 3000ms disclosed, got %+v", cov)
	}
	if cov.ObservedOperations != 45 || cov.ObservedOperationMs != 3150 {
		t.Fatalf("expected observed totals to include overflow, got %+v", cov)
	}
	if got, want := imp.Groups[0].ShareOfObservedTime, 100.0/3150.0; got < want-1e-9 || got > want+1e-9 {
		t.Fatalf("share must use full observed total: got %v want %v", got, want)
	}
	if !containsSubstring(cov.Limitations, "Group limit 2 reached") {
		t.Fatalf("expected group-limit limitation, got %v", cov.Limitations)
	}
}

func TestImpactListsTopGroupsAndDisclosesRest(t *testing.T) {
	c := newImpactCollector(1, 5000, 300)
	for i := 0; i < impactViewLimit+5; i++ {
		recordImpactOps(c, "orders", fmt.Sprintf("shape_%02d", i), i+1, time.Millisecond)
	}
	imp := c.GetFinalInsights().Impact
	cov := imp.Coverage
	if len(imp.Groups) != impactViewLimit || cov.GroupsShown != impactViewLimit || cov.NotShownGroups != 5 {
		t.Fatalf("expected %d shown and 5 not shown, got %+v", impactViewLimit, cov)
	}
	if cov.NotShownOperations != 15 || cov.NotShownOperationMs != 15 {
		t.Fatalf("expected 15 not-shown ops / 15ms (shapes with 1..5 ops), got %+v", cov)
	}
	if !cov.GroupCoverageComplete {
		t.Fatalf("listing truncation must not be reported as group overflow")
	}
	if !containsSubstring(cov.Limitations, "Only the top 20 of 25") {
		t.Fatalf("expected not-shown limitation, got %v", cov.Limitations)
	}
}

func TestImpactEmptyRun(t *testing.T) {
	c := newImpactCollector(1, 5000, 300)
	rep := c.GetFinalInsights()
	if rep.Impact.Status != "empty" || rep.Impact.Groups == nil || len(rep.Impact.Groups) != 0 || rep.Impact.Coverage.ObservedOperations != 0 {
		t.Fatalf("expected empty impact view, got %+v", rep.Impact)
	}
	b, err := json.Marshal(rep.Impact)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"groups":[]`) {
		t.Fatalf("expected empty groups array in JSON, got %s", b)
	}
}

func TestImpactDisabled(t *testing.T) {
	c := NewCollector()
	c.ConfigureInsights(&config.AppConfig{InsightsEnabled: false})
	recordImpactOps(c, "orders", "s", 5, time.Millisecond)
	imp := c.GetFinalInsights().Impact
	if imp.Status != "disabled" || len(imp.Groups) != 0 || imp.Groups == nil {
		t.Fatalf("expected disabled impact view, got %+v", imp)
	}
}

func TestImpactResetBetweenRunsButNotBetweenIterations(t *testing.T) {
	c := newImpactCollector(1, 5000, 300)
	recordImpactOps(c, "orders", "s", 5, 10*time.Millisecond)

	// The runner reconfigures the shared collector at every iteration; this
	// must not discard full-run aggregates.
	c.ConfigureInsights(&config.AppConfig{InsightsEnabled: true, InsightsSamplingRate: 1, InsightsMaxEvents: 5000, InsightsMaxGroups: 300})
	recordImpactOps(c, "orders", "s", 5, 10*time.Millisecond)
	if got := c.GetFinalInsights().Impact.Coverage.ObservedOperations; got != 10 {
		t.Fatalf("expected aggregates to span iterations, got %d ops", got)
	}

	c.ResetInsights()
	imp := c.GetFinalInsights().Impact
	if imp.Status != "empty" || imp.Coverage.ObservedOperations != 0 || imp.Coverage.TrackedGroups != 0 {
		t.Fatalf("expected reset to clear aggregates, got %+v", imp)
	}
	recordImpactOps(c, "orders", "s", 3, 10*time.Millisecond)
	if g := c.GetFinalInsights().Impact.Groups; len(g) != 1 || g[0].OperationCount != 3 || g[0].TotalOperationMs != 30 {
		t.Fatalf("expected only post-reset ops, got %+v", g)
	}
}

func TestImpactConcurrentRecording(t *testing.T) {
	c := newImpactCollector(0.1, 100, 300)
	const workers, perWorker = 8, 500
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			shape := fmt.Sprintf("shape_%d", w%2)
			for i := 0; i < perWorker; i++ {
				recordImpactOps(c, "orders", shape, 1, 2*time.Millisecond)
				if i%100 == 0 {
					_ = c.GetFinalInsights()
				}
			}
		}(w)
	}
	wg.Wait()

	imp := c.GetFinalInsights().Impact
	if imp.Coverage.ObservedOperations != workers*perWorker || imp.Coverage.ObservedOperationMs != workers*perWorker*2 {
		t.Fatalf("lost updates under concurrency: %+v", imp.Coverage)
	}
	for _, g := range imp.Groups {
		if g.OperationCount != workers/2*perWorker {
			t.Fatalf("expected %d ops per shape, got %+v", workers/2*perWorker, g)
		}
	}
}

func containsSubstring(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func BenchmarkRecordOperationEvent(b *testing.B) {
	c := newImpactCollector(0.1, 5000, 300)
	shapes := []string{"shape_a", "shape_b", "shape_c", "shape_d"}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			c.RecordOperationEvent("find", "shop", "orders", shapes[i%len(shapes)], "summary", []string{"status"}, 5*time.Millisecond, true, 1, nil, "q", "unit_test", "w", "", "", "", "", 0)
			i++
		}
	})
}
