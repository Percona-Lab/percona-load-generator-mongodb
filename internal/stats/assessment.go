package stats

import (
	"fmt"
	"math"
	"strings"
)

// Query assessment dimensions shown next to scan efficiency. Each is computed
// from its own measurements, none changes the scan efficiency score, and they
// are never combined into one score. Changing a rule requires a new version.
const (
	LatencyRatingVersion  = "lr-1"
	OverheadRatingVersion = "eo-1"

	LatencyWithinTarget = "within_target"
	LatencyAboveTarget  = "above_target"
	LatencyNotAssessed  = "not_assessed"

	LatencyCheckMet         = "met"
	LatencyCheckMissed      = "missed"
	LatencyCheckNotMeasured = "not_measured"

	OverheadNoneObserved  = "none_observed"
	OverheadInMemorySort  = "in_memory_sort"
	OverheadSpilled       = "spilled_to_disk"
	OverheadNotAssessed   = "not_assessed"
	OverheadNotApplicable = "not_applicable"

	SignalObserved    = "observed"
	SignalNotObserved = "not_observed"
	SignalNotMeasured = "not_measured"

	MeasureMeasured      = "measured"
	MeasureNotMeasured   = "not_measured"
	MeasureNotAssessed   = "not_assessed"
	MeasureNotApplicable = "not_applicable"
)

// LatencyTargets are the configured per-shape latency targets in ms; 0 means
// no target for that statistic.
type LatencyTargets struct {
	AvgMs float64 `json:"avg_ms,omitempty"`
	P95Ms float64 `json:"p95_ms,omitempty"`
	P99Ms float64 `json:"p99_ms,omitempty"`
}

// NewLatencyTargets drops non-positive and non-finite targets.
func NewLatencyTargets(avg, p95, p99 float64) LatencyTargets {
	clean := func(v float64) float64 {
		if v > 0 && !math.IsInf(v, 0) {
			return v
		}
		return 0
	}
	return LatencyTargets{AvgMs: clean(avg), P95Ms: clean(p95), P99Ms: clean(p99)}
}

// Any reports whether at least one target is set.
func (t LatencyTargets) Any() bool { return t.AvgMs > 0 || t.P95Ms > 0 || t.P99Ms > 0 }

// LatencyCheck compares one measured statistic with its target.
type LatencyCheck struct {
	Metric     string   `json:"metric"`
	TargetMs   float64  `json:"target_ms"`
	MeasuredMs *float64 `json:"measured_ms,omitempty"`
	Result     string   `json:"result"`
}

// LatencyRating is within_target only when every configured target was
// measured and met. A missed target is above_target even if another
// statistic was not measured; otherwise missing data is not_assessed.
type LatencyRating struct {
	Version string         `json:"version"`
	Status  string         `json:"status"`
	Label   string         `json:"label"`
	Reason  string         `json:"reason,omitempty"`
	Checks  []LatencyCheck `json:"checks,omitempty"`
}

// LatencyAssessment is a shape's client-measured latency and its rating.
type LatencyAssessment struct {
	AvgMs             *float64      `json:"avg_ms,omitempty"`
	MaxMs             *float64      `json:"max_ms,omitempty"`
	P95Ms             *float64      `json:"p95_ms,omitempty"`
	P99Ms             *float64      `json:"p99_ms,omitempty"`
	PercentileSamples int           `json:"percentile_samples"`
	PercentileStatus  string        `json:"percentile_status"`
	Summary           string        `json:"summary"`
	Rating            LatencyRating `json:"rating"`
}

// BuildLatencyAssessment uses the full-run average and maximum and the
// retained-sample percentiles. ok is false when the shape has no sample
// latency entry.
func BuildLatencyAssessment(count uint64, avgMs, maxMs float64, sl SampleLatency, ok bool, targets LatencyTargets) LatencyAssessment {
	a := LatencyAssessment{PercentileSamples: sl.Samples, PercentileStatus: sl.Status, P95Ms: sl.P95Ms, P99Ms: sl.P99Ms}
	if !ok {
		a.PercentileStatus = "no_samples"
	}
	if count > 0 {
		a.AvgMs, a.MaxMs = floatPtr(avgMs), floatPtr(maxMs)
	}
	part := func(name string, v *float64) string {
		if v == nil {
			return name + " not measured"
		}
		return fmt.Sprintf("%s %s ms", name, trimFloat(*v))
	}
	a.Summary = strings.Join([]string{part("avg", a.AvgMs), part("p95", a.P95Ms), part("p99", a.P99Ms)}, " · ")
	a.Rating = rateLatency(a, targets)
	return a
}

func rateLatency(a LatencyAssessment, t LatencyTargets) LatencyRating {
	r := LatencyRating{Version: LatencyRatingVersion}
	if !t.Any() {
		r.Status, r.Label, r.Reason = LatencyNotAssessed, "Not assessed (no target set)", "no_latency_target"
		return r
	}
	var missed, unmeasured []string
	add := func(metric string, target float64, measured *float64) {
		if target <= 0 {
			return
		}
		c := LatencyCheck{Metric: metric, TargetMs: target, MeasuredMs: measured}
		switch {
		case measured == nil:
			c.Result = LatencyCheckNotMeasured
			unmeasured = append(unmeasured, metric)
		case *measured > target:
			c.Result = LatencyCheckMissed
			missed = append(missed, fmt.Sprintf("%s %s ms > %s ms", metric, trimFloat(*measured), trimFloat(target)))
		default:
			c.Result = LatencyCheckMet
		}
		r.Checks = append(r.Checks, c)
	}
	add("avg", t.AvgMs, a.AvgMs)
	add("p95", t.P95Ms, a.P95Ms)
	add("p99", t.P99Ms, a.P99Ms)
	switch {
	case len(missed) > 0:
		r.Status, r.Label = LatencyAboveTarget, "Above target ("+strings.Join(missed, ", ")+")"
	case len(unmeasured) > 0:
		r.Status, r.Reason = LatencyNotAssessed, "target_statistic_not_measured"
		r.Label = "Not assessed (" + strings.Join(unmeasured, ", ") + " not measured)"
	default:
		r.Status, r.Label = LatencyWithinTarget, "Within target"
	}
	return r
}

// WorkloadImpactAssessment is how much of the run a shape accounts for. It has
// no rating: a frequent or time-heavy shape is not inefficient for that reason.
type WorkloadImpactAssessment struct {
	Rank                 int      `json:"rank,omitempty"`
	OperationCount       uint64   `json:"operation_count"`
	OperationsPerSec     *float64 `json:"operations_per_sec,omitempty"`
	ShareOfOperations    *float64 `json:"share_of_operations,omitempty"`
	TotalOperationMs     float64  `json:"total_operation_ms"`
	ShareOfOperationTime *float64 `json:"share_of_operation_time,omitempty"`
	Summary              string   `json:"summary"`
}

// BuildWorkloadImpact divides by the measured workload window and the observed
// totals; a missing denominator leaves the value unset.
func BuildWorkloadImpact(rank int, count uint64, totalMs float64, observedOps uint64, observedMs float64, durationSec *float64) WorkloadImpactAssessment {
	w := WorkloadImpactAssessment{Rank: rank, OperationCount: count, TotalOperationMs: totalMs}
	if durationSec != nil && *durationSec > 0 {
		w.OperationsPerSec = floatPtr(roundTo(float64(count) / *durationSec, 2))
	}
	if observedOps > 0 {
		w.ShareOfOperations = floatPtr(roundTo(float64(count)/float64(observedOps), 4))
	}
	if observedMs > 0 {
		w.ShareOfOperationTime = floatPtr(roundTo(totalMs/observedMs, 4))
	}
	parts := []string{fmt.Sprintf("%d ops", count)}
	if w.OperationsPerSec != nil {
		parts = append(parts, trimFloat(*w.OperationsPerSec)+"/s")
	} else {
		parts = append(parts, "rate not measured")
	}
	pct := func(v *float64, what string) string {
		if v == nil {
			return "share of " + what + " not measured"
		}
		return fmt.Sprintf("%.1f%% of %s", *v*100, what)
	}
	parts = append(parts, pct(w.ShareOfOperations, "ops"), pct(w.ShareOfOperationTime, "op time"))
	w.Summary = strings.Join(parts, " · ")
	return w
}

// OverheadSignal is one execution-overhead check with the evidence for it.
type OverheadSignal struct {
	Status   string   `json:"status"`
	Reason   string   `json:"reason,omitempty"`
	Evidence []string `json:"evidence,omitempty"`
}

// ExecutionOverheadAssessment classifies the sort and spill work seen in the
// explained plans: spilled_to_disk, then in_memory_sort, then none_observed.
// none_observed needs executionStats on every explained sample, because a
// spill is only reported there.
type ExecutionOverheadAssessment struct {
	Version string         `json:"version"`
	Status  string         `json:"status"`
	Label   string         `json:"label"`
	Reason  string         `json:"reason,omitempty"`
	Sort    OverheadSignal `json:"sort"`
	Spill   OverheadSignal `json:"spill"`
	// Measurements are the reported sort/spill values; they do not change
	// Status.
	Measurements OverheadMeasurements `json:"measurements"`
}

func explainedSamples(samples []ExplainSample) []ExplainSample {
	var out []ExplainSample
	for _, s := range samples {
		if s.Status == "explained" && s.Diagnostics != nil {
			out = append(out, s)
		}
	}
	return out
}

// BuildExecutionOverhead assesses sorts and spills from explained samples.
// reason explains why there is no evidence when no sample was explained.
func BuildExecutionOverhead(op string, samples []ExplainSample, reason string) ExecutionOverheadAssessment {
	o := classifyExecutionOverhead(op, samples, reason)
	o.Measurements = buildOverheadMeasurements(op, samples, emptyFallback(o.Reason, reason))
	return o
}

func classifyExecutionOverhead(op string, samples []ExplainSample, reason string) ExecutionOverheadAssessment {
	o := ExecutionOverheadAssessment{Version: OverheadRatingVersion}
	if isInsertOp(op) {
		o.Status, o.Label, o.Reason = OverheadNotApplicable, "Not applicable", "operation_not_explainable"
		o.Sort, o.Spill = OverheadSignal{Status: SignalNotMeasured}, OverheadSignal{Status: SignalNotMeasured}
		return o
	}
	ex := explainedSamples(samples)
	if len(ex) == 0 {
		o.Status, o.Label, o.Reason = OverheadNotAssessed, "Not assessed (no explain)", emptyFallback(reason, "explain_unavailable")
		o.Sort = OverheadSignal{Status: SignalNotMeasured, Reason: o.Reason}
		o.Spill = OverheadSignal{Status: SignalNotMeasured, Reason: o.Reason}
		return o
	}
	o.Sort.Status, o.Spill.Status = SignalNotObserved, SignalNotObserved
	allStats := true
	for _, s := range ex {
		d := s.Diagnostics
		switch {
		case d.BlockingSortDetected:
			o.Sort.Evidence = append(o.Sort.Evidence, s.Role+" sample: SORT stage in the winning plan (results sorted in memory, not read in index order)")
		case d.SortDetected:
			o.Sort.Evidence = append(o.Sort.Evidence, s.Role+" sample: $sort pipeline stage (results sorted in memory)")
		}
		if d.UsedDisk {
			e := s.Role + " sample: usedDisk true"
			if d.Spills > 0 {
				e += fmt.Sprintf(", spills %d", d.Spills)
			}
			o.Spill.Evidence = append(o.Spill.Evidence, e)
		}
		allStats = allStats && d.ExecutionStatsAvailable
	}
	if len(o.Sort.Evidence) > 0 {
		o.Sort.Status = SignalObserved
	}
	switch {
	case len(o.Spill.Evidence) > 0:
		o.Spill.Status = SignalObserved
	case !allStats:
		o.Spill.Status, o.Spill.Reason = SignalNotMeasured, "execution_stats_unavailable"
	}
	switch {
	case o.Spill.Status == SignalObserved:
		o.Status, o.Label = OverheadSpilled, "Spilled to disk"
	case o.Sort.Status == SignalObserved && o.Spill.Status == SignalNotMeasured:
		o.Status, o.Label = OverheadInMemorySort, "In-memory sort (spill not measured)"
	case o.Sort.Status == SignalObserved:
		o.Status, o.Label = OverheadInMemorySort, "In-memory sort"
	case o.Spill.Status == SignalNotMeasured:
		o.Status, o.Label, o.Reason = OverheadNotAssessed, "Not assessed (spill not measured)", o.Spill.Reason
	default:
		o.Status, o.Label = OverheadNoneObserved, "No in-memory sort or spill"
	}
	return o
}

// Measurement is a dimension that is shown only when measured.
type Measurement struct {
	Status   string   `json:"status"`
	Label    string   `json:"label"`
	Reason   string   `json:"reason,omitempty"`
	Evidence []string `json:"evidence,omitempty"`
}

// OtherMeasurements are $lookup, lock, and network costs. Only $lookup can be
// measured, from explain; locks and network time are not collected.
type OtherMeasurements struct {
	Lookup  Measurement `json:"lookup"`
	Locks   Measurement `json:"locks"`
	Network Measurement `json:"network"`
}

const (
	locksNotMeasuredReason   = "Lock waits are not collected: explain does not report them, and PLGM does not read the profiler, slow-query log, or currentOp."
	networkNotMeasuredReason = "Client latency includes network round trips, but network time is not measured separately."
)

// BuildOtherMeasurements reports joined-collection work for $lookup when the
// explained $lookup stages report it.
func BuildOtherMeasurements(op string, samples []ExplainSample) OtherMeasurements {
	m := OtherMeasurements{
		Locks:   Measurement{Status: MeasureNotMeasured, Label: "Not measured", Reason: locksNotMeasuredReason},
		Network: Measurement{Status: MeasureNotMeasured, Label: "Not measured", Reason: networkNotMeasuredReason},
	}
	m.Lookup = lookupMeasurement(op, samples)
	return m
}

func lookupMeasurement(op string, samples []ExplainSample) Measurement {
	if op != "aggregate" {
		return Measurement{Status: MeasureNotApplicable, Label: "Not applicable", Reason: "not_an_aggregation"}
	}
	ex := explainedSamples(samples)
	if len(ex) == 0 {
		return Measurement{Status: MeasureNotAssessed, Label: "Not assessed (no explain)", Reason: "explain_unavailable"}
	}
	var maxDocs, maxKeys *int64
	var evidence []string
	pushedDown, unreported := false, false
	keep := func(dst **int64, c *ExplainCounter) {
		if c != nil && (*dst == nil || c.Value > **dst) {
			v := c.Value
			*dst = &v
		}
	}
	for _, s := range ex {
		mt := s.Diagnostics.Metrics
		if mt == nil {
			continue
		}
		for _, n := range mt.Notes {
			switch n {
			case "lookup_pushdown_access_count_ambiguous":
				pushedDown = true
			case "lookup_work_not_scored":
				unreported = unreported || mt.Lookup == nil
			}
		}
		lk := mt.Lookup
		if lk == nil {
			continue
		}
		if lk.DocsExamined == nil && lk.KeysExamined == nil {
			unreported = true
			continue
		}
		keep(&maxDocs, lk.DocsExamined)
		keep(&maxKeys, lk.KeysExamined)
		evidence = append(evidence, fmt.Sprintf("%s sample, %d $lookup stage(s): %s, %s, %s",
			s.Role, lk.Stages, counterEvidence("docsExamined", lk.DocsExamined), counterEvidence("keysExamined", lk.KeysExamined), counterEvidence("collectionScans", lk.CollectionScans)))
	}
	if maxDocs != nil || maxKeys != nil {
		count := func(v *int64, what string) string {
			if v == nil {
				return what + " not reported"
			}
			return fmt.Sprintf("%d %s", *v, what)
		}
		return Measurement{Status: MeasureMeasured, Evidence: evidence,
			Label: count(maxDocs, "docs") + ", " + count(maxKeys, "keys") + " examined in joined collection (largest sample)"}
	}
	switch {
	case pushedDown:
		return Measurement{Status: MeasureNotMeasured, Label: "Not measured", Reason: "The $lookup ran inside the query plan (EQ_LOOKUP), which does not report joined-collection work separately."}
	case unreported:
		return Measurement{Status: MeasureNotMeasured, Label: "Not measured", Reason: "The $lookup stage did not report examined counts (they need executionStats verbosity)."}
	}
	return Measurement{Status: MeasureNotApplicable, Label: "No $lookup", Reason: "no_lookup_stage_in_plan"}
}

// EfficiencyColumn explains one column of the assessment table.
type EfficiencyColumn struct {
	Key         string `json:"key"`
	Title       string `json:"title"`
	Explanation string `json:"explanation"`
}

func efficiencyColumns() []EfficiencyColumn {
	return []EfficiencyColumn{
		{Key: "scan_efficiency", Title: "Scan efficiency", Explanation: "0-100, higher is better. Set by the worst of documents examined per result, index keys examined per result, and a filtered collection scan (caps at 38). Bands: 75+ efficient, 50-74 moderate, below 50 inefficient. From explain of up to 2 sample commands after the run. \"≤ N\" means explain returned only some counts; \"Unassessed\" means the shape was not explained."},
		{Key: "latency", Title: "Query latency", Explanation: "Client-side time per operation, including network. Avg and max cover every operation; p95 needs 20 sampled operations and p99 needs 100. Rated only against the insights_latency_target_* settings; with no target it is Not assessed."},
		{Key: "workload_impact", Title: "Workload impact", Explanation: "Operations, operations per second over the workload window, share of all operations, and share of summed operation time. It shows where time went; a high share or rate is not by itself a sign of an inefficient query."},
		{Key: "execution_overhead", Title: "Execution overhead", Explanation: "From the explained plans: a SORT or $sort stage means results were sorted in memory instead of read in index order; a spill means a stage wrote temporary data to disk (usedDisk). Spills are reported only with executionStats, so without them the result is Not assessed. Below the label: the largest sort bytes processed and spill count/file size reported by a single stage, as a range when the samples differ. Details list every stage's reported sort, spill, memory-limit, and stage-time values per sample; these come from the post-run explain, not the original operations."},
		{Key: "other", Title: "$lookup / locks / network", Explanation: "$lookup: documents and keys examined in the joined collection, when the $lookup stage reports them. Lock waits and network time are not collected by PLGM."},
	}
}
