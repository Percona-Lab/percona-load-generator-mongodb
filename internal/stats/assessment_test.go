package stats

import (
	"math"
	"testing"
	"time"
)

func latencySamples(n int, v float64) []float64 {
	s := make([]float64, n)
	for i := range s {
		s[i] = v
	}
	return s
}

func TestRateLatencyRequiresTargetsAndMeasurements(t *testing.T) {
	enough := sampleLatency(latencySamples(100, 10))
	few := sampleLatency(latencySamples(50, 10))
	if enough.P99Ms == nil || few.P95Ms == nil || few.P99Ms != nil || few.Status != "from_retained_samples" {
		t.Fatalf("p99 needs %d samples, p95 needs %d: %+v %+v", minP99Samples, minImpactPercentileSamples, enough, few)
	}
	if s := sampleLatency(latencySamples(5, 10)); s.P95Ms != nil || s.Status != "insufficient_samples" {
		t.Fatalf("too few samples must not produce percentiles: %+v", s)
	}

	cases := []struct {
		name    string
		count   uint64
		avg     float64
		sl      SampleLatency
		ok      bool
		targets LatencyTargets
		status  string
		reason  string
	}{
		{"no target is never rated", 100, 1, enough, true, LatencyTargets{}, LatencyNotAssessed, "no_latency_target"},
		{"met targets", 100, 8, enough, true, LatencyTargets{AvgMs: 9, P95Ms: 10, P99Ms: 10}, LatencyWithinTarget, ""},
		{"missed avg", 100, 12, enough, true, LatencyTargets{AvgMs: 11}, LatencyAboveTarget, ""},
		{"unmeasured p99 is not a pass", 50, 1, few, true, LatencyTargets{AvgMs: 5, P99Ms: 20}, LatencyNotAssessed, "target_statistic_not_measured"},
		{"miss wins over unmeasured", 50, 30, few, true, LatencyTargets{AvgMs: 5, P99Ms: 20}, LatencyAboveTarget, ""},
		{"no sample entry", 0, 0, SampleLatency{}, false, LatencyTargets{P95Ms: 5}, LatencyNotAssessed, "target_statistic_not_measured"},
	}
	for _, c := range cases {
		a := BuildLatencyAssessment(c.count, c.avg, c.avg, c.sl, c.ok, c.targets)
		if a.Rating.Status != c.status || a.Rating.Reason != c.reason || a.Rating.Version != LatencyRatingVersion {
			t.Fatalf("%s: got %+v", c.name, a.Rating)
		}
	}
	a := BuildLatencyAssessment(0, 0, 0, SampleLatency{}, false, LatencyTargets{})
	if a.AvgMs != nil || a.PercentileStatus != "no_samples" || a.Summary != "avg not measured · p95 not measured · p99 not measured" {
		t.Fatalf("a shape with no operations must report nothing as measured: %+v", a)
	}
}

func TestNewLatencyTargetsDropsInvalidValues(t *testing.T) {
	got := NewLatencyTargets(-1, math.Inf(1), 25)
	if got != (LatencyTargets{P99Ms: 25}) || !got.Any() || (LatencyTargets{}).Any() {
		t.Fatalf("invalid targets must be dropped: %+v", got)
	}
}

func TestBuildWorkloadImpactNeedsMeasuredDenominators(t *testing.T) {
	d := 10.0
	w := BuildWorkloadImpact(1, 500, 250, 1000, 1000, &d)
	if *w.OperationsPerSec != 50 || *w.ShareOfOperations != 0.5 || *w.ShareOfOperationTime != 0.25 ||
		w.Summary != "500 ops · 50/s · 50.0% of ops · 25.0% of op time" {
		t.Fatalf("unexpected impact: %+v", w)
	}
	none := BuildWorkloadImpact(2, 5, 1, 0, 0, nil)
	if none.OperationsPerSec != nil || none.ShareOfOperations != nil || none.ShareOfOperationTime != nil ||
		none.Summary != "5 ops · rate not measured · share of ops not measured · share of op time not measured" {
		t.Fatalf("missing denominators must stay unset: %+v", none)
	}
}

func overheadSample(role string, d ExplainDiagnostics) ExplainSample {
	return ExplainSample{Role: role, Status: "explained", Diagnostics: &d}
}

func TestBuildExecutionOverhead(t *testing.T) {
	cases := []struct {
		name    string
		op      string
		samples []ExplainSample
		status  string
		sort    string
		spill   string
	}{
		{"spill wins", "aggregate", []ExplainSample{
			overheadSample("median", ExplainDiagnostics{ExecutionStatsAvailable: true, SortDetected: true}),
			overheadSample("p95", ExplainDiagnostics{ExecutionStatsAvailable: true, SortDetected: true, UsedDisk: true, Spills: 3}),
		}, OverheadSpilled, SignalObserved, SignalObserved},
		{"blocking sort", "find", []ExplainSample{overheadSample("median", ExplainDiagnostics{ExecutionStatsAvailable: true, BlockingSortDetected: true})},
			OverheadInMemorySort, SignalObserved, SignalNotObserved},
		{"sort without executionStats", "find", []ExplainSample{overheadSample("median", ExplainDiagnostics{BlockingSortDetected: true})},
			OverheadInMemorySort, SignalObserved, SignalNotMeasured},
		{"planner only is not a clean result", "find", []ExplainSample{overheadSample("median", ExplainDiagnostics{})},
			OverheadNotAssessed, SignalNotObserved, SignalNotMeasured},
		{"mixed stats is not a clean result", "find", []ExplainSample{
			overheadSample("median", ExplainDiagnostics{ExecutionStatsAvailable: true}),
			overheadSample("p95", ExplainDiagnostics{}),
		}, OverheadNotAssessed, SignalNotObserved, SignalNotMeasured},
		{"clean", "find", []ExplainSample{overheadSample("median", ExplainDiagnostics{ExecutionStatsAvailable: true})},
			OverheadNoneObserved, SignalNotObserved, SignalNotObserved},
		{"failed explain", "find", []ExplainSample{{Role: "median", Status: "failed"}}, OverheadNotAssessed, SignalNotMeasured, SignalNotMeasured},
		{"insert", "insertOne", nil, OverheadNotApplicable, SignalNotMeasured, SignalNotMeasured},
	}
	for _, c := range cases {
		o := BuildExecutionOverhead(c.op, c.samples, "")
		if o.Status != c.status || o.Sort.Status != c.sort || o.Spill.Status != c.spill || o.Version != OverheadRatingVersion {
			t.Fatalf("%s: got %+v", c.name, o)
		}
	}
	o := BuildExecutionOverhead("aggregate", []ExplainSample{overheadSample("p95", ExplainDiagnostics{ExecutionStatsAvailable: true, UsedDisk: true, Spills: 3})}, "")
	if len(o.Spill.Evidence) != 1 || o.Spill.Evidence[0] != "p95 sample: usedDisk true, spills 3" {
		t.Fatalf("spill evidence must name the sample and the counts: %+v", o.Spill)
	}
}

func lookupSample(role string, m *ExplainMetrics) ExplainSample {
	return overheadSample(role, ExplainDiagnostics{ExecutionStatsAvailable: true, Metrics: m})
}

func TestBuildOtherMeasurements(t *testing.T) {
	c := func(v int64) *ExplainCounter { return &ExplainCounter{Value: v, Source: "stages[1].x"} }
	measured := BuildOtherMeasurements("aggregate", []ExplainSample{
		lookupSample("median", &ExplainMetrics{Lookup: &LookupMetrics{Stages: 1, DocsExamined: c(40), KeysExamined: c(40), CollectionScans: c(0)}}),
		lookupSample("p95", &ExplainMetrics{Lookup: &LookupMetrics{Stages: 1, DocsExamined: c(90), KeysExamined: c(80), CollectionScans: c(0)}}),
	})
	if measured.Lookup.Status != MeasureMeasured || measured.Lookup.Label != "90 docs, 80 keys examined in joined collection (largest sample)" || len(measured.Lookup.Evidence) != 2 {
		t.Fatalf("measured $lookup: %+v", measured.Lookup)
	}
	if measured.Locks.Status != MeasureNotMeasured || measured.Network.Status != MeasureNotMeasured {
		t.Fatalf("locks and network are never collected: %+v", measured)
	}
	pushed := BuildOtherMeasurements("aggregate", []ExplainSample{lookupSample("median", &ExplainMetrics{Notes: []string{"lookup_pushdown_access_count_ambiguous"}})})
	unreported := BuildOtherMeasurements("aggregate", []ExplainSample{lookupSample("median", &ExplainMetrics{Notes: []string{"lookup_work_not_scored"}, Lookup: &LookupMetrics{Stages: 1}})})
	if pushed.Lookup.Status != MeasureNotMeasured || unreported.Lookup.Status != MeasureNotMeasured {
		t.Fatalf("$lookup without counts must be not measured: %+v %+v", pushed.Lookup, unreported.Lookup)
	}
	if got := BuildOtherMeasurements("aggregate", nil).Lookup.Status; got != MeasureNotAssessed {
		t.Fatalf("an unexplained aggregate is not assessed, got %s", got)
	}
	if got := BuildOtherMeasurements("find", nil).Lookup.Status; got != MeasureNotApplicable {
		t.Fatalf("$lookup does not apply to find, got %s", got)
	}
}

func TestWorkloadDurationNeedsStartAndEnd(t *testing.T) {
	c := &Collector{}
	if _, ok := c.WorkloadDuration(); ok {
		t.Fatal("no window before the workload starts")
	}
	c.MarkWorkloadStart()
	if _, ok := c.WorkloadDuration(); ok {
		t.Fatal("no window before the workload ends")
	}
	time.Sleep(2 * time.Millisecond)
	c.MarkWorkloadEnd()
	first, ok := c.WorkloadDuration()
	if !ok || first < time.Millisecond {
		t.Fatalf("window must cover the run: %v %v", first, ok)
	}
	time.Sleep(2 * time.Millisecond)
	c.MarkWorkloadEnd()
	if again, _ := c.WorkloadDuration(); again != first {
		t.Fatalf("only the first end mark counts: %v != %v", again, first)
	}
}
