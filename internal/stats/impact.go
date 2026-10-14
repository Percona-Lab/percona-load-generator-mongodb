package stats

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// ImpactTimeBasis labels what TotalOperationMs measures. Operations run
// concurrently, so the sum can exceed wall-clock run time and is not CPU time.
const ImpactTimeBasis = "summed_concurrent_operation_time"

const impactTimeBasisNote = "Summed concurrent operation time: the sum of client-observed latencies of completed operations. Concurrent operations overlap, so this is not wall-clock time and not server CPU consumption."

const impactInterpretationNote = "Impact is measured from every completed operation. It shows where operation time went; it does not establish a cause, and it is not index advice."

// minImpactPercentileSamples is the minimum number of retained samples needed
// before a per-group sample p95 is reported.
const minImpactPercentileSamples = 20

// minP99Samples is the minimum number of retained samples needed before a
// per-group sample p99 is reported: fewer than 100 samples have no
// observation above the 99th percentile, so the value would be the maximum.
const minP99Samples = 100

const impactViewLimit = 20

type impactKey struct {
	op, database, collection, shape string
}

type impactAgg struct {
	shapeSummary string
	queryLabel   string
	count        uint64
	errorCount   uint64
	sumNs        int64
	maxNs        int64
	capture      *shapeCapture
}

// impactState holds full-run per-shape aggregates. It is updated for every
// completed operation, before sampling and retention, and guarded by insightsMu.
type impactState struct {
	groups      map[impactKey]*impactAgg
	totalOps    uint64
	totalNs     int64
	overflowOps uint64
	overflowNs  int64
}

func (s *impactState) reset() {
	s.groups = nil
	s.totalOps = 0
	s.totalNs = 0
	s.overflowOps = 0
	s.overflowNs = 0
}

// record returns the shape's group, or nil when the shape is not tracked.
func (s *impactState) record(op, database, collection, shapeKey, shapeSummary, queryLabel string, duration time.Duration, success bool, maxGroups int) *impactAgg {
	ns := duration.Nanoseconds()
	s.totalOps++
	s.totalNs += ns

	k := impactKey{op: op, database: database, collection: collection, shape: shapeKey}
	g, ok := s.groups[k]
	if !ok {
		if s.groups == nil {
			s.groups = make(map[impactKey]*impactAgg)
		}
		if len(s.groups) >= maxGroups {
			s.overflowOps++
			s.overflowNs += ns
			return nil
		}
		g = &impactAgg{shapeSummary: shapeSummary, queryLabel: queryLabel}
		s.groups[k] = g
	}
	g.count++
	if !success {
		g.errorCount++
	}
	g.sumNs += ns
	if ns > g.maxNs {
		g.maxNs = ns
	}
	return g
}

type ImpactCoverage struct {
	Scope                 string   `json:"scope"`
	TimeBasis             string   `json:"time_basis"`
	TimeBasisNote         string   `json:"time_basis_note"`
	InterpretationNote    string   `json:"interpretation_note"`
	ObservedOperations    uint64   `json:"observed_operations"`
	ObservedOperationMs   float64  `json:"observed_operation_ms"`
	TrackedGroups         int      `json:"tracked_groups"`
	GroupLimit            int      `json:"group_limit"`
	OverflowOperations    uint64   `json:"overflow_operations"`
	OverflowOperationMs   float64  `json:"overflow_operation_ms"`
	GroupsShown           int      `json:"groups_shown"`
	NotShownGroups        int      `json:"not_shown_groups"`
	NotShownOperations    uint64   `json:"not_shown_operations"`
	NotShownOperationMs   float64  `json:"not_shown_operation_ms"`
	GroupCoverageComplete bool     `json:"group_coverage_complete"`
	SampledInEvents       uint64   `json:"sampled_in_events"`
	RetainedSampleEvents  int      `json:"retained_sample_events"`
	SampleBufferWrapped   bool     `json:"sample_buffer_wrapped"`
	MinPercentileSamples  int      `json:"min_percentile_samples"`
	Limitations           []string `json:"limitations,omitempty"`
}

type WorkloadImpact struct {
	Rank                   int      `json:"rank"`
	ImpactID               string   `json:"impact_id"`
	ShapeID                string   `json:"shape_id"`
	Namespace              string   `json:"namespace"`
	Database               string   `json:"database,omitempty"`
	Collection             string   `json:"collection"`
	Operation              string   `json:"operation"`
	ShapeKey               string   `json:"shape_key"`
	ShapeSummary           string   `json:"shape_summary"`
	QueryLabel             string   `json:"query_label,omitempty"`
	OperationCount         uint64   `json:"operation_count"`
	ErrorCount             uint64   `json:"error_count"`
	TotalOperationMs       float64  `json:"total_operation_ms"`
	AvgMs                  float64  `json:"avg_ms"`
	MaxMs                  float64  `json:"max_ms"`
	ShareOfObservedTime    float64  `json:"share_of_observed_time"`
	SampleCount            int      `json:"sample_count"`
	SampleP95Ms            *float64 `json:"sample_p95_ms,omitempty"`
	SampleP99Ms            *float64 `json:"sample_p99_ms,omitempty"`
	SamplePercentileStatus string   `json:"sample_percentile_status"`
	Explanation            string   `json:"explanation"`
}

// SampleLatency is one tracked shape's latency percentiles from retained
// samples. A nil percentile had too few samples.
type SampleLatency struct {
	Samples int
	P95Ms   *float64
	P99Ms   *float64
	Status  string
}

type ImpactView struct {
	Status   string           `json:"status"`
	Coverage ImpactCoverage   `json:"coverage"`
	Groups   []WorkloadImpact `json:"groups"`
	// SampleLatency covers every tracked shape by shape id, including shapes
	// beyond the listed groups.
	SampleLatency map[string]SampleLatency `json:"-"`
}

func sampleLatency(s []float64) SampleLatency {
	out := SampleLatency{Samples: len(s)}
	switch {
	case len(s) == 0:
		out.Status = "no_samples"
		return out
	case len(s) < minImpactPercentileSamples:
		out.Status = "insufficient_samples"
		return out
	}
	out.Status = "from_retained_samples"
	p95 := round2(percentile(s, 95))
	out.P95Ms = &p95
	if len(s) >= minP99Samples {
		p99 := round2(percentile(s, 99))
		out.P99Ms = &p99
	}
	return out
}

func impactNamespace(database, collection string) string {
	coll := emptyFallback(collection, "(unknown)")
	if strings.TrimSpace(database) == "" {
		return coll
	}
	return database + "." + coll
}

func nsToMs(ns int64) float64 {
	return float64(ns) / 1e6
}

// buildImpactView ranks full-run aggregates by summed operation time. Retained
// events are used only for per-group sample percentiles, never for totals.
func buildImpactView(st *impactState, events []OperationEvent, maxGroups int, sampledIn uint64) ImpactView {
	view := ImpactView{
		Status: "ready",
		Coverage: ImpactCoverage{
			Scope:                "full_run_all_completed_operations",
			TimeBasis:            ImpactTimeBasis,
			TimeBasisNote:        impactTimeBasisNote,
			InterpretationNote:   impactInterpretationNote,
			ObservedOperations:   st.totalOps,
			ObservedOperationMs:  round2(nsToMs(st.totalNs)),
			TrackedGroups:        len(st.groups),
			GroupLimit:           maxGroups,
			OverflowOperations:   st.overflowOps,
			OverflowOperationMs:  round2(nsToMs(st.overflowNs)),
			SampledInEvents:      sampledIn,
			RetainedSampleEvents: len(events),
			SampleBufferWrapped:  sampledIn > uint64(len(events)),
			MinPercentileSamples: minImpactPercentileSamples,
		},
		Groups: []WorkloadImpact{},
	}
	cov := &view.Coverage
	cov.GroupCoverageComplete = st.overflowOps == 0

	if st.totalOps == 0 {
		view.Status = "empty"
		return view
	}

	samples := make(map[impactKey][]float64, len(st.groups))
	for _, ev := range events {
		k := impactKey{op: ev.Operation, database: ev.Database, collection: ev.Collection, shape: ev.ShapeKey}
		if _, ok := st.groups[k]; ok {
			samples[k] = append(samples[k], ev.DurationMs)
		}
	}

	type ranked struct {
		key impactKey
		agg *impactAgg
		id  string
	}
	all := make([]ranked, 0, len(st.groups))
	// Shapes that share a shape id across databases are merged, as in the
	// explain inventory.
	byShape := make(map[string][]float64, len(st.groups))
	for k, g := range st.groups {
		all = append(all, ranked{key: k, agg: g, id: StableShapeID(k.op, impactNamespace(k.database, k.collection), k.shape)})
		shapeID := StableShapeID(emptyFallback(k.op, "(unknown)"), emptyFallback(k.collection, "(unknown)"), emptyFallback(k.shape, "(shape unavailable)"))
		byShape[shapeID] = append(byShape[shapeID], samples[k]...)
	}
	view.SampleLatency = make(map[string]SampleLatency, len(byShape))
	for id, s := range byShape {
		view.SampleLatency[id] = sampleLatency(s)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].agg.sumNs != all[j].agg.sumNs {
			return all[i].agg.sumNs > all[j].agg.sumNs
		}
		if all[i].agg.count != all[j].agg.count {
			return all[i].agg.count > all[j].agg.count
		}
		return all[i].id < all[j].id
	})

	shown := all
	if len(shown) > impactViewLimit {
		shown = all[:impactViewLimit]
		for _, r := range all[impactViewLimit:] {
			cov.NotShownGroups++
			cov.NotShownOperations += r.agg.count
			cov.NotShownOperationMs += nsToMs(r.agg.sumNs)
		}
		cov.NotShownOperationMs = round2(cov.NotShownOperationMs)
	}
	cov.GroupsShown = len(shown)

	for i, r := range shown {
		k, g := r.key, r.agg
		op := emptyFallback(k.op, "(unknown)")
		coll := emptyFallback(k.collection, "(unknown)")
		shape := emptyFallback(k.shape, "(shape unavailable)")
		ns := impactNamespace(k.database, k.collection)
		totalMs := nsToMs(g.sumNs)
		share := 0.0
		if st.totalNs > 0 {
			share = float64(g.sumNs) / float64(st.totalNs)
		}
		row := WorkloadImpact{
			Rank:                i + 1,
			ImpactID:            r.id,
			ShapeID:             StableShapeID(op, coll, shape),
			Namespace:           ns,
			Database:            k.database,
			Collection:          coll,
			Operation:           op,
			ShapeKey:            shape,
			ShapeSummary:        emptyFallback(g.shapeSummary, "(shape unavailable)"),
			QueryLabel:          g.queryLabel,
			OperationCount:      g.count,
			ErrorCount:          g.errorCount,
			TotalOperationMs:    round2(totalMs),
			AvgMs:               round2(totalMs / float64(g.count)),
			MaxMs:               round2(nsToMs(g.maxNs)),
			ShareOfObservedTime: share,
		}
		sl := sampleLatency(samples[k])
		row.SampleCount, row.SampleP95Ms, row.SampleP99Ms, row.SamplePercentileStatus = sl.Samples, sl.P95Ms, sl.P99Ms, sl.Status
		row.Explanation = impactExplanation(row)
		view.Groups = append(view.Groups, row)
	}

	if !cov.GroupCoverageComplete {
		cov.Limitations = append(cov.Limitations, fmt.Sprintf(
			"Group limit %d reached: %d operations (%.2f ms) belong to shapes that were not tracked individually. They are included in observed totals and shares but cannot be ranked; the number of untracked shapes is unknown.",
			maxGroups, st.overflowOps, cov.OverflowOperationMs))
	}
	if cov.NotShownGroups > 0 {
		cov.Limitations = append(cov.Limitations, fmt.Sprintf(
			"Only the top %d of %d tracked groups are listed; %d operations (%.2f ms) in the remaining groups are not shown.",
			cov.GroupsShown, cov.TrackedGroups, cov.NotShownOperations, cov.NotShownOperationMs))
	}
	if cov.SampleBufferWrapped {
		cov.Limitations = append(cov.Limitations, fmt.Sprintf(
			"Sample buffer wrapped: %d of %d sampled events retained (most recent only). Sample percentiles reflect the end of the run; counts and totals are unaffected.",
			cov.RetainedSampleEvents, cov.SampledInEvents))
	}
	if sampledIn < st.totalOps {
		cov.Limitations = append(cov.Limitations, "Sample percentiles come from a subset of operations taken in consecutive blocks, so they may be biased. They are never extrapolated to full-run totals.")
	}
	return view
}

func impactExplanation(r WorkloadImpact) string {
	label := r.ShapeSummary
	if strings.TrimSpace(r.QueryLabel) != "" {
		label = r.QueryLabel
	}
	return fmt.Sprintf("%s on %s (%s) ran %d times for %.2f ms of summed concurrent operation time, %.1f%% of all observed operation time (avg %.2f ms).",
		r.Operation, r.Namespace, label, r.OperationCount, r.TotalOperationMs, r.ShareOfObservedTime*100, r.AvgMs)
}
