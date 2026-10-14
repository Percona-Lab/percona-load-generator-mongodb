package stats

import (
	"fmt"
	"sort"
)

// Per-shape sample scopes: which population a shape's explain samples represent.
const (
	SampleScopeAllOperations     = "all_operations"
	SampleScopeUniformReservoir  = "full_run_uniform_reservoir"
	SampleScopeReservoirDropped  = "full_run_reservoir_with_drops"
	explainSampleScopeMethodNote = "Each selected shape is explained from its own representative samples: a uniform reservoir over every successful explainable operation of the shape in the run, plus its slowest one, independent of the slow threshold. all_operations means every such operation was retained; full_run_reservoir_with_drops means some admissions were skipped by the sample size or byte limits, so the reservoir may be biased."
)

// Latency tiers group shapes by full-run average latency for diverse selection.
var explainLatencyTiers = []struct {
	name  string
	maxMs float64
}{
	{"lt_1ms", 1},
	{"1_10ms", 10},
	{"10_100ms", 100},
	{"100ms_1s", 1000},
	{"ge_1s", 0},
}

// ExplainLatencyTiers lists tier names from fastest to slowest.
func ExplainLatencyTiers() []string {
	out := make([]string, len(explainLatencyTiers))
	for i, t := range explainLatencyTiers {
		out[i] = t.name
	}
	return out
}

// ExplainLatencyTier returns the tier index and name for an average latency.
func ExplainLatencyTier(avgMs float64) (int, string) {
	for i, t := range explainLatencyTiers {
		if t.maxMs == 0 || avgMs < t.maxMs {
			return i, t.name
		}
	}
	last := len(explainLatencyTiers) - 1
	return last, explainLatencyTiers[last].name
}

// ExplainInventoryShape is one tracked shape with full-run impact counters and
// its retained representative samples. Samples hold executable queries.
type ExplainInventoryShape struct {
	ShapeID            string
	Operation          string
	Database           string
	Collection         string
	ShapeKey           string
	ShapeSummary       string
	ImpactRank         int
	OperationCount     uint64
	ErrorCount         uint64
	EligibleOperations uint64
	TotalOperationMs   float64
	AvgMs              float64
	MaxMs              float64
	SampleScope        string
	UnavailableReason  string
	Samples            []RepresentativeSample
}

// ExplainInventory is every tracked shape (not a truncated view), ranked by
// summed operation time, plus the capture state that bounds it.
type ExplainInventory struct {
	CaptureStatus         string
	CaptureDisabledReason string
	GroupLimit            int
	SlowThresholdMs       float64
	UntrackedOperations   uint64
	UnorderedSortShapes   int
	Shapes                []ExplainInventoryShape
}

// SnapshotExplainInventory returns deep copies of every tracked shape's
// representative samples. Shapes that share a shape ID across databases are
// merged, matching how the slow view groups them.
func (c *Collector) SnapshotExplainInventory() ExplainInventory {
	c.insightsMu.Lock()
	defer c.insightsMu.Unlock()
	inv := ExplainInventory{
		CaptureStatus:       "ready",
		GroupLimit:          c.insightsCfg.maxGroups,
		SlowThresholdMs:     c.insightsCfg.slowThresholdMs,
		UntrackedOperations: c.impact.overflowOps,
	}
	if !c.capture.enabled {
		inv.CaptureStatus, inv.CaptureDisabledReason = "disabled", c.capture.disabledReason
	}

	type acc struct {
		shape  ExplainInventoryShape
		sumNs  int64
		maxNs  int64
		drops  bool
		maxRes int
	}
	byID := make(map[string]*acc, len(c.impact.groups))
	for k, g := range c.impact.groups {
		id := StableShapeID(k.op, k.collection, k.shape)
		a, ok := byID[id]
		if !ok {
			a = &acc{shape: ExplainInventoryShape{
				ShapeID: id, Operation: k.op, Database: k.database, Collection: k.collection,
				ShapeKey: k.shape, ShapeSummary: g.shapeSummary,
			}}
			byID[id] = a
		}
		a.shape.OperationCount += g.count
		a.shape.ErrorCount += g.errorCount
		a.sumNs += g.sumNs
		if g.maxNs > a.maxNs {
			a.maxNs = g.maxNs
		}
		sc := g.capture
		if sc == nil {
			if a.shape.UnavailableReason == "" {
				a.shape.UnavailableReason = captureUnavailableReason(g)
			}
			continue
		}
		a.shape.EligibleOperations += sc.eligible
		if sc.oversized > 0 || sc.byteBudget > 0 {
			a.drops = true
		}
		samples, slowestIdx := sc.retained()
		if len(samples) == 0 {
			if a.shape.UnavailableReason == "" {
				a.shape.UnavailableReason = captureUnavailableReason(g)
			}
			continue
		}
		a.maxRes += len(sc.reservoir)
		for i, smp := range samples {
			a.shape.Samples = append(a.shape.Samples, RepresentativeSample{
				ShapeID: id, Operation: k.op, Database: k.database, Collection: k.collection, ShapeKey: k.shape,
				DurationMs: nsToMs(smp.durationNs), TimestampMs: smp.timestampMs,
				InReservoir: i < len(sc.reservoir), Slowest: i == slowestIdx,
				Query: cloneExecutableQuery(smp.query),
			})
		}
	}

	inv.Shapes = make([]ExplainInventoryShape, 0, len(byID))
	sums := make(map[string]int64, len(byID))
	for id, a := range byID {
		s := a.shape
		s.TotalOperationMs = round2(nsToMs(a.sumNs))
		s.MaxMs = round2(nsToMs(a.maxNs))
		if s.OperationCount > 0 {
			s.AvgMs = round2(nsToMs(a.sumNs) / float64(s.OperationCount))
		}
		if len(s.Samples) > 0 {
			s.UnavailableReason = ""
			sort.SliceStable(s.Samples, func(i, j int) bool { return s.Samples[i].DurationMs < s.Samples[j].DurationMs })
			switch {
			case a.drops:
				s.SampleScope = SampleScopeReservoirDropped
			case s.EligibleOperations <= uint64(a.maxRes):
				s.SampleScope = SampleScopeAllOperations
			default:
				s.SampleScope = SampleScopeUniformReservoir
			}
			for _, smp := range s.Samples {
				if smp.Query != nil && HasUnorderedSortSpec(smp.Query.Pipeline) {
					inv.UnorderedSortShapes++
					break
				}
			}
		}
		sums[id] = a.sumNs
		inv.Shapes = append(inv.Shapes, s)
	}
	sort.Slice(inv.Shapes, func(i, j int) bool {
		a, b := inv.Shapes[i], inv.Shapes[j]
		if sums[a.ShapeID] != sums[b.ShapeID] {
			return sums[a.ShapeID] > sums[b.ShapeID]
		}
		if a.OperationCount != b.OperationCount {
			return a.OperationCount > b.OperationCount
		}
		return a.ShapeID < b.ShapeID
	})
	for i := range inv.Shapes {
		inv.Shapes[i].ImpactRank = i + 1
	}
	return inv
}

type ExplainCoverageBudget struct {
	SeveritySlots     int `json:"severity_slots"`
	EfficiencySlots   int `json:"efficiency_slots"`
	ImpactSlots       int `json:"impact_slots"`
	LatencyTierSlots  int `json:"latency_tier_slots"`
	SamplesPerShape   int `json:"samples_per_shape"`
	MaxPlannedSamples int `json:"max_planned_samples"`
	MaxTimeMS         int `json:"max_time_ms"`
	Workers           int `json:"workers"`
	Retries           int `json:"retries"`
}

type ExplainSamplingCoverage struct {
	CaptureStatus            string   `json:"capture_status"`
	CaptureDisabledReason    string   `json:"capture_disabled_reason,omitempty"`
	ScopeNote                string   `json:"scope_note"`
	ShapesAllOperations      int      `json:"shapes_all_operations"`
	ShapesUniformReservoir   int      `json:"shapes_uniform_reservoir"`
	ShapesReservoirWithDrops int      `json:"shapes_reservoir_with_drops"`
	ShapesWithoutSamples     int      `json:"shapes_without_samples"`
	UntrackedOperations      uint64   `json:"untracked_operations"`
	LatencyTiers             []string `json:"latency_tiers"`
}

// ExplainCoverageShape is the explain outcome for one tracked shape. Status is
// explained, failed, cancelled, unavailable, excluded, or not_supported.
type ExplainCoverageShape struct {
	ShapeID            string            `json:"shape_id"`
	Namespace          string            `json:"namespace"`
	Collection         string            `json:"collection"`
	Operation          string            `json:"operation"`
	ShapeSummary       string            `json:"shape_summary"`
	ImpactRank         int               `json:"impact_rank"`
	OperationCount     uint64            `json:"operation_count"`
	TotalOperationMs   float64           `json:"total_operation_ms"`
	AvgMs              float64           `json:"avg_ms"`
	MaxMs              float64           `json:"max_ms"`
	LatencyTier        string            `json:"latency_tier"`
	BelowSlowThreshold bool              `json:"below_slow_threshold"`
	Status             string            `json:"status"`
	Reason             string            `json:"reason,omitempty"`
	SelectedBy         string            `json:"selected_by,omitempty"`
	Evidence           string            `json:"evidence,omitempty"`
	PartialReason      string            `json:"partial_reason,omitempty"`
	SampleScope        string            `json:"sample_scope,omitempty"`
	Selection          *ExplainSelection `json:"selection,omitempty"`
	Samples            []ExplainSample   `json:"samples,omitempty"`
	Finding            *ExplainFinding   `json:"finding,omitempty"`
	Efficiency         *QueryEfficiency  `json:"efficiency,omitempty"`
	Predicates         *PredicateProfile `json:"predicates,omitempty"`
}

// ExplainCoverageView reports which shapes were explained, which were not, and
// why. An unselected shape is not assessed; that is not a claim it is efficient.
type ExplainCoverageView struct {
	Status                     string                  `json:"status"`
	Reason                     string                  `json:"reason,omitempty"`
	EfficiencyMode             string                  `json:"efficiency_mode,omitempty"`
	SelectionMethod            string                  `json:"selection_method,omitempty"`
	SelectionNote              string                  `json:"selection_note,omitempty"`
	Budget                     ExplainCoverageBudget   `json:"budget"`
	TrackedShapes              int                     `json:"tracked_shapes"`
	EligibleShapes             int                     `json:"eligible_shapes"`
	SelectedShapes             int                     `json:"selected_shapes"`
	SelectedBySeverity         int                     `json:"selected_by_severity"`
	SelectedByImpact           int                     `json:"selected_by_impact"`
	SelectedByLatencyTier      int                     `json:"selected_by_latency_tier"`
	SelectedBelowSlowThreshold int                     `json:"selected_below_slow_threshold"`
	CompletedShapes            int                     `json:"completed_shapes"`
	PartialShapes              int                     `json:"partial_shapes"`
	FailedShapes               int                     `json:"failed_shapes"`
	CancelledShapes            int                     `json:"cancelled_shapes"`
	UnavailableShapes          int                     `json:"unavailable_shapes"`
	ExcludedShapes             int                     `json:"excluded_shapes"`
	NotSupportedShapes         int                     `json:"not_supported_shapes"`
	UnavailableByReason        map[string]int          `json:"unavailable_by_reason,omitempty"`
	ExcludedByReason           map[string]int          `json:"excluded_by_reason,omitempty"`
	SamplesPlanned             int                     `json:"samples_planned"`
	CommandsDeduplicated       int                     `json:"commands_deduplicated"`
	CommandsRun                int                     `json:"commands_run"`
	Sampling                   ExplainSamplingCoverage `json:"sampling"`
	UnrecordedPaths            []string                `json:"unrecorded_paths"`
	Limitations                []string                `json:"limitations,omitempty"`
	Shapes                     []ExplainCoverageShape  `json:"shapes"`
}

// ExplainSamplingScopeNote describes what explain samples represent.
func ExplainSamplingScopeNote() string { return explainSampleScopeMethodNote }

// NewExplainCoverage returns a view with the disclosures every state carries.
func NewExplainCoverage(status, reason string) ExplainCoverageView {
	return ExplainCoverageView{
		Status:          status,
		Reason:          reason,
		Sampling:        ExplainSamplingCoverage{ScopeNote: explainSampleScopeMethodNote, LatencyTiers: ExplainLatencyTiers()},
		UnrecordedPaths: append([]string(nil), captureUnrecordedPaths...),
		Shapes:          []ExplainCoverageShape{},
	}
}

// finalizeExplainCoverage derives each selected shape's status from its
// samples, attaches findings, and recomputes the coverage counters.
func finalizeExplainCoverage(rep *InsightsReport, bySlow map[string]*SlowQueryInsight) {
	cov := &rep.ExplainCoverage
	if cov.Status != "ready" {
		return
	}
	cov.SelectedShapes, cov.SelectedBySeverity, cov.SelectedByImpact, cov.SelectedByLatencyTier = 0, 0, 0, 0
	cov.SelectedBelowSlowThreshold, cov.CompletedShapes, cov.PartialShapes, cov.FailedShapes = 0, 0, 0, 0
	cov.CancelledShapes, cov.UnavailableShapes, cov.ExcludedShapes, cov.NotSupportedShapes = 0, 0, 0, 0
	cov.UnavailableByReason, cov.ExcludedByReason = map[string]int{}, map[string]int{}
	cov.TrackedShapes, cov.EligibleShapes = len(cov.Shapes), 0
	cov.Sampling.ShapesAllOperations, cov.Sampling.ShapesUniformReservoir = 0, 0
	cov.Sampling.ShapesReservoirWithDrops, cov.Sampling.ShapesWithoutSamples = 0, 0

	for i := range cov.Shapes {
		s := &cov.Shapes[i]
		if s.Status != "not_supported" {
			cov.EligibleShapes++
			switch s.SampleScope {
			case SampleScopeAllOperations:
				cov.Sampling.ShapesAllOperations++
			case SampleScopeUniformReservoir:
				cov.Sampling.ShapesUniformReservoir++
			case SampleScopeReservoirDropped:
				cov.Sampling.ShapesReservoirWithDrops++
			default:
				cov.Sampling.ShapesWithoutSamples++
			}
		}
		if s.SelectedBy != "" && len(s.Samples) > 0 {
			if sq, ok := bySlow[s.ShapeID]; ok && len(sq.ExplainSamples) > 0 {
				s.Samples, s.Finding = sq.ExplainSamples, sq.ExplainFinding
			} else {
				for j := range s.Samples {
					s.Samples[j].LatencyComparison = compareLatency(s.Samples[j])
				}
				s.Finding = BuildExplainFinding(SlowQueryInsight{
					ShapeID: s.ShapeID, Operation: s.Operation, Collection: s.Collection,
					ShapeSummary: s.ShapeSummary, ExplainSelection: s.Selection, ExplainSamples: s.Samples,
				})
			}
			s.Status, s.Reason, s.Evidence, s.PartialReason = coverageStatusFromSamples(s.Samples)
		}
		if s.SelectedBy != "" {
			cov.SelectedShapes++
			switch s.SelectedBy {
			case "severity":
				cov.SelectedBySeverity++
			case "impact_rank":
				cov.SelectedByImpact++
			case "latency_tier":
				cov.SelectedByLatencyTier++
			}
			if s.BelowSlowThreshold {
				cov.SelectedBelowSlowThreshold++
			}
		}
		switch s.Status {
		case "explained":
			cov.CompletedShapes++
			if s.Evidence == "planner_only" {
				cov.PartialShapes++
			}
		case "failed":
			cov.FailedShapes++
		case "cancelled":
			cov.CancelledShapes++
		case "unavailable":
			cov.UnavailableShapes++
			cov.UnavailableByReason[s.Reason]++
		case "excluded":
			cov.ExcludedShapes++
			cov.ExcludedByReason[s.Reason]++
		case "not_supported":
			cov.NotSupportedShapes++
		}
	}
	if cov.ExcludedShapes > 0 {
		cov.Limitations = append(cov.Limitations, fmt.Sprintf(
			"%d eligible shapes were not explained because of the explain budget; they are unassessed, not efficient.", cov.ExcludedShapes))
	}
	if cov.PartialShapes > 0 {
		cov.Limitations = append(cov.Limitations, fmt.Sprintf(
			"%d explained shapes have planner-only evidence (no execution statistics), so their evidence is partial.", cov.PartialShapes))
	}
	if cov.Sampling.ShapesReservoirWithDrops > 0 {
		cov.Limitations = append(cov.Limitations, fmt.Sprintf(
			"%d shapes skipped some sample admissions because of the size or byte limits; their samples may be biased.", cov.Sampling.ShapesReservoirWithDrops))
	}
	if cov.CancelledShapes > 0 {
		cov.Limitations = append(cov.Limitations, fmt.Sprintf(
			"Explain was cancelled before %d selected shapes completed.", cov.CancelledShapes))
	}
}

// coverageStatusFromSamples summarizes a selected shape's sample outcomes.
func coverageStatusFromSamples(samples []ExplainSample) (status, reason, evidence, partial string) {
	explained, cancelled, inconclusive := 0, 0, 0
	hasStats := false
	firstFail, firstOther := "", ""
	for _, s := range samples {
		switch {
		case s.Status == "explained" && s.Diagnostics != nil:
			explained++
			if s.Diagnostics.ExecutionStatsAvailable {
				hasStats = true
			} else if partial == "" {
				partial = s.Diagnostics.VerbosityNote
				if partial == "" {
					partial = "verbosity_" + s.Diagnostics.Verbosity
				}
			}
		case s.Status == "cancelled":
			cancelled++
		case isInconclusiveStatus(s.Status):
			inconclusive++
			if firstFail == "" {
				firstFail = s.Status + ":" + s.Reason
			}
		default:
			if firstOther == "" {
				firstOther = s.Reason
			}
		}
	}
	switch {
	case explained > 0:
		if hasStats {
			return "explained", "", "execution_stats", ""
		}
		return "explained", "", "planner_only", partial
	case inconclusive > 0:
		return "failed", firstFail, "", ""
	case cancelled > 0:
		return "cancelled", "explain_cancelled", "", ""
	default:
		return "unavailable", emptyFallback(firstOther, "explain_unavailable"), "", ""
	}
}
