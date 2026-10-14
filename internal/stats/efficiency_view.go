package stats

import (
	"fmt"
	"sort"
	"strings"
)

// Efficiency view: a display projection of the cached scan efficiency results
// and the separate latency, workload impact, execution overhead, and other
// measurements. It never rescores; every value comes from explain_coverage,
// impact, query_capture, slow_queries, metadata, and recommendations of the
// same report.
const (
	EffCoverageAssessed       = "assessed"
	EffCoveragePartial        = "partial"
	EffCoverageBudgetExcluded = "budget_excluded"
	EffCoverageUnavailable    = "unavailable"
	EffCoverageFailed         = "failed"
	EffCoverageCancelled      = "cancelled"
	EffCoverageNotSupported   = "not_supported"
	EffCoverageNotApplicable  = "not_applicable"

	EffSortEfficiency = "efficiency"
	EffSortImpact     = "impact"

	efficiencySortNote = "Sort by scan efficiency (lowest score first; at equal scores, measured scores before \"≤ N\" bounds, then higher workload impact) or by workload impact (summed operation time). Shapes without a score follow the scored ones and are never hidden."
)

var efficiencyCoverageLabels = map[string]string{
	EffCoverageAssessed:       "Assessed",
	EffCoveragePartial:        "Partial evidence",
	EffCoverageBudgetExcluded: "Not explained (budget)",
	EffCoverageUnavailable:    "Unavailable",
	EffCoverageFailed:         "Explain failed",
	EffCoverageCancelled:      "Explain cancelled",
	EffCoverageNotSupported:   "Not supported",
	EffCoverageNotApplicable:  "Not applicable",
}

// Sorts and spills are shown under execution overhead, not as scan notes.
var efficiencyObservationNotes = map[string]string{
	"covered":                   "Covered query: the index answered it without reading documents.",
	"large_work":                "More than 10,000 documents examined per call. That is volume, not a scan-efficiency problem, and it is not scored.",
	"unfiltered_scan_expected":  "Collection scan without a filter: reading every document is expected, so it is not penalized.",
	"below_work_floor":          "Fewer than 100 documents or keys examined, so the per-result ratio is not penalized.",
	"zero_result":               "The sample returned no results; the examined count is used as the ratio.",
	"aggregate_reduction":       "The pipeline reduces its input (for example $group); the ratio uses the documents read by the first stage, not the final output.",
	"matched_count_lower_bound": "The matched count is a lower bound (express update path), so the ratio may be overstated.",
}

var efficiencyDimensionNames = map[string]string{
	DimDocsPerResult: "documents examined per result",
	DimKeysPerResult: "index keys examined per result",
	DimCollscan:      "filtered collection scan",
}

var efficiencyDimensionNotes = map[string]string{
	"below_work_floor":         "under the 100-item floor",
	"zero_result":              "no results returned",
	"filtered_collection_scan": "collection scan with a filter",
	"unfiltered_scan_expected": "collection scan without a filter",
	"no_collection_scan":       "no collection scan",
}

// EfficiencyViewSample is one explained sample's data source and counters.
type EfficiencyViewSample struct {
	Role              string          `json:"role"`
	ClientLatencyMs   float64         `json:"client_latency_ms"`
	ExplainStatus     string          `json:"explain_status"`
	ExplainReason     string          `json:"explain_reason,omitempty"`
	Attempts          int             `json:"attempts"`
	SharedCommand     bool            `json:"shared_command,omitempty"`
	Evidence          string          `json:"evidence"`
	Layout            string          `json:"layout,omitempty"`
	Status            string          `json:"status,omitempty"`
	Reason            string          `json:"reason,omitempty"`
	Score             *int            `json:"score,omitempty"`
	ScoreIsUpperBound bool            `json:"score_is_upper_bound,omitempty"`
	ScoreLabel        string          `json:"score_label"`
	Band              string          `json:"band,omitempty"`
	DocsExamined      *ExplainCounter `json:"docs_examined,omitempty"`
	KeysExamined      *ExplainCounter `json:"keys_examined,omitempty"`
	Returned          *ExplainCounter `json:"returned,omitempty"`
	AccessReturned    *ExplainCounter `json:"access_returned,omitempty"`
	Matched           *ExplainCounter `json:"matched,omitempty"`
	ServerExecMs      *int64          `json:"server_exec_ms,omitempty"`
	WinningPlan       string          `json:"winning_plan,omitempty"`
	IndexesUsed       []string        `json:"indexes_used,omitempty"`
	LatencyComparison string          `json:"latency_comparison,omitempty"`
}

// EfficiencyViewRow is one tracked shape in the efficiency view.
type EfficiencyViewRow struct {
	ShapeID             string                 `json:"shape_id"`
	Namespace           string                 `json:"namespace"`
	Operation           string                 `json:"operation"`
	ShapeSummary        string                 `json:"shape_summary"`
	Query               string                 `json:"query,omitempty"`
	QueryTruncated      bool                   `json:"query_truncated,omitempty"`
	QueryVariants       int                    `json:"query_variants,omitempty"`
	Status              string                 `json:"status"`
	Reason              string                 `json:"reason,omitempty"`
	Score               *int                   `json:"score,omitempty"`
	ScoreIsUpperBound   bool                   `json:"score_is_upper_bound,omitempty"`
	ScoreLabel          string                 `json:"score_label"`
	Band                string                 `json:"band,omitempty"`
	Confidence          string                 `json:"confidence,omitempty"`
	ConfidenceBasis     string                 `json:"confidence_basis,omitempty"`
	Coverage            string                 `json:"coverage"`
	CoverageLabel       string                 `json:"coverage_label"`
	CoverageReason      string                 `json:"coverage_reason,omitempty"`
	RatingExplanation   string                 `json:"rating_explanation"`
	LimitingDimension   string                 `json:"limiting_dimension,omitempty"`
	Dimensions          []EfficiencyDimension  `json:"dimensions,omitempty"`
	Observations        []string               `json:"observations,omitempty"`
	ObservationNotes    []string               `json:"observation_notes,omitempty"`
	Limitations         []string               `json:"limitations,omitempty"`
	OperationCount      uint64                 `json:"operation_count"`
	AvgMs               float64                `json:"avg_ms"`
	MaxMs               float64                `json:"max_ms"`
	LatencyTier         string                 `json:"latency_tier,omitempty"`
	BelowSlowThreshold  bool                   `json:"below_slow_threshold"`
	Severity            string                 `json:"severity"`
	ImpactRank          int                    `json:"impact_rank,omitempty"`
	TotalOperationMs    float64                `json:"total_operation_ms"`
	ImpactShare         *float64               `json:"impact_share,omitempty"`
	SelectedBy          string                 `json:"selected_by,omitempty"`
	SampleScope         string                 `json:"sample_scope,omitempty"`
	Selection           *ExplainSelection      `json:"selection,omitempty"`
	DeterminingSample   string                 `json:"determining_sample,omitempty"`
	SamplesDisagree     bool                   `json:"samples_disagree,omitempty"`
	EvidenceNote        string                 `json:"evidence_note,omitempty"`
	Samples             []EfficiencyViewSample `json:"samples,omitempty"`
	PlanSummary         string                 `json:"plan_summary,omitempty"`
	Diagnosis           string                 `json:"diagnosis,omitempty"`
	Interpretation      string                 `json:"interpretation,omitempty"`
	RecommendationRanks []int                  `json:"recommendation_ranks,omitempty"`
	EfficiencyOrder     int                    `json:"efficiency_order"`

	Latency           LatencyAssessment           `json:"latency"`
	WorkloadImpact    WorkloadImpactAssessment    `json:"workload_impact"`
	ExecutionOverhead ExecutionOverheadAssessment `json:"execution_overhead"`
	OtherMeasurements OtherMeasurements           `json:"other_measurements"`
}

// EfficiencyUnrecorded discloses work that no row can represent.
type EfficiencyUnrecorded struct {
	Paths               []string `json:"paths"`
	UntrackedOperations uint64   `json:"untracked_operations"`
}

// ExplainDisabledNotice is shown wherever query analysis results appear when
// post-run explain was turned off for the run.
const ExplainDisabledNotice = "Post-run explain was turned off for this run. Scan efficiency, execution overhead (sorts and disk spills), plan-based index recommendations, and efficiency trends require it, so only latency and workload impact are reported. To get the full query analysis, enable Post-Run Explain Sampling (settings tab Query Analysis (Explain), or insights_explain_enabled: true) and run the workload again. Explain runs after the workload completes and does not affect benchmark results."

// EfficiencyView is the query assessment table plus the disclosures needed to
// read it. Version is the scan efficiency version; RatingVersions names every
// rating shown.
type EfficiencyView struct {
	Version        string            `json:"version"`
	RatingVersions map[string]string `json:"rating_versions"`
	Status         string            `json:"status"`
	Reason         string            `json:"reason,omitempty"`
	// Notice tells the reader what is missing and how to get it; set only
	// when post-run explain was turned off for the run.
	Notice         string               `json:"notice,omitempty"`
	MethodNote     string               `json:"method_note"`
	Columns        []EfficiencyColumn   `json:"columns"`
	LatencyTargets LatencyTargets       `json:"latency_targets"`
	SortOptions    []string             `json:"sort_options"`
	DefaultSort    string               `json:"default_sort"`
	SortNote       string               `json:"sort_note"`
	ShapesTotal    int                  `json:"shapes_total"`
	CoverageCounts map[string]int       `json:"coverage_counts"`
	CoverageLabels map[string]string    `json:"coverage_labels"`
	Unrecorded     EfficiencyUnrecorded `json:"unrecorded"`
	Limitations    []string             `json:"limitations,omitempty"`
	Rows           []EfficiencyViewRow  `json:"rows"`
}

// ApplyEfficiencyView attaches the efficiency view built from rep's cached
// results. It is a projection only and does not rescore or re-explain.
func ApplyEfficiencyView(rep *InsightsReport) {
	v := BuildEfficiencyView(*rep)
	rep.EfficiencyView = &v
}

// BuildEfficiencyView projects one report into the efficiency view.
func BuildEfficiencyView(rep InsightsReport) EfficiencyView {
	cov := rep.ExplainCoverage
	v := EfficiencyView{
		Version: EfficiencyVersion, Status: cov.Status, Reason: cov.Reason,
		RatingVersions: map[string]string{"scan_efficiency": EfficiencyVersion, "latency": LatencyRatingVersion, "execution_overhead": OverheadRatingVersion},
		MethodNote:     EfficiencyMethodNote, Columns: efficiencyColumns(), LatencyTargets: rep.Metadata.LatencyTargets,
		SortOptions: []string{EffSortEfficiency, EffSortImpact}, DefaultSort: EffSortEfficiency, SortNote: efficiencySortNote,
		CoverageCounts: map[string]int{}, CoverageLabels: efficiencyCoverageLabels,
		Unrecorded: EfficiencyUnrecorded{Paths: append([]string{}, cov.UnrecordedPaths...), UntrackedOperations: cov.Sampling.UntrackedOperations},
		Rows:       []EfficiencyViewRow{},
	}
	if v.Status == "" {
		v.Status = "unavailable"
	}
	if cov.Reason == "explain_disabled" {
		v.Notice = ExplainDisabledNotice
	}
	if len(v.Unrecorded.Paths) == 0 {
		v.Unrecorded.Paths = append([]string{}, captureUnrecordedPaths...)
	}
	if rep.EfficiencyCoverage != nil {
		v.Limitations = append(v.Limitations, rep.EfficiencyCoverage.Limitations...)
		if rep.EfficiencyCoverage.UntrackedOperations > v.Unrecorded.UntrackedOperations {
			v.Unrecorded.UntrackedOperations = rep.EfficiencyCoverage.UntrackedOperations
		}
	}
	if rep.Impact.Coverage.OverflowOperations > v.Unrecorded.UntrackedOperations {
		v.Unrecorded.UntrackedOperations = rep.Impact.Coverage.OverflowOperations
	}

	severity := make(map[string]string, len(rep.SlowQueries))
	for _, sq := range rep.SlowQueries {
		severity[sq.ShapeID] = sq.Severity
	}
	captured := make(map[string]CapturedShape, len(rep.QueryCapture.Shapes))
	for _, cs := range rep.QueryCapture.Shapes {
		captured[cs.ShapeID] = cs
	}
	querySummary := make(map[string]string, len(rep.QueryShapes))
	for _, qs := range append(append([]SlowQueryInsight{}, rep.SlowQueries...), rep.QueryShapes...) {
		if _, ok := querySummary[qs.ShapeID]; ok {
			continue
		}
		if q := firstNonEmpty(qs.QuerySummary, qs.PipelineSummary); q != "" {
			querySummary[qs.ShapeID] = q
		}
	}
	recRanks := make(map[string][]int)
	for _, r := range rep.Recommendations {
		ids := append([]string{}, r.ShapeIDs...)
		if r.ShapeID != "" {
			ids = append(ids, r.ShapeID)
		}
		seen := map[string]bool{}
		for _, id := range ids {
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			recRanks[id] = append(recRanks[id], r.Rank)
		}
	}
	observedMs := rep.Impact.Coverage.ObservedOperationMs

	if cov.Status == "ready" {
		for _, cs := range cov.Shapes {
			v.Rows = append(v.Rows, efficiencyRowFromCoverage(cs))
		}
	} else {
		reason := "explain_" + v.Status
		if cov.Reason != "" {
			reason = cov.Reason
		}
		for _, g := range rep.Impact.Groups {
			row := EfficiencyViewRow{
				ShapeID: g.ShapeID, Namespace: g.Namespace, Operation: g.Operation, ShapeSummary: g.ShapeSummary,
				OperationCount: g.OperationCount, AvgMs: g.AvgMs, MaxMs: g.MaxMs, ImpactRank: g.Rank,
				TotalOperationMs: g.TotalOperationMs, BelowSlowThreshold: g.MaxMs < rep.Metadata.SlowThresholdMs,
				Status: EfficiencyUnassessed, Reason: reason, Coverage: EffCoverageUnavailable, CoverageReason: reason,
			}
			if isInsertOp(g.Operation) {
				row.Status, row.Reason, row.Coverage, row.CoverageReason = EfficiencyNotApplicable, "operation_not_explainable", EffCoverageNotApplicable, "operation_not_explainable"
			}
			row.ExecutionOverhead = BuildExecutionOverhead(g.Operation, nil, reason)
			row.OtherMeasurements = BuildOtherMeasurements(g.Operation, nil)
			v.Rows = append(v.Rows, row)
		}
		if len(rep.Impact.Groups) > 0 {
			v.Limitations = append(v.Limitations, fmt.Sprintf("Explain results are %s (%s), so no shape has a scan efficiency score or plan-based measurement. Rows are the shapes listed in Workload Impact.", v.Status, reason))
		}
		if rep.Impact.Coverage.NotShownGroups > 0 {
			v.Limitations = append(v.Limitations, fmt.Sprintf("%d tracked shapes are not in the impact view and are not listed.", rep.Impact.Coverage.NotShownGroups))
		}
	}

	for i := range v.Rows {
		r := &v.Rows[i]
		r.ScoreLabel = efficiencyScoreLabel(r.Status, r.Score, r.ScoreIsUpperBound)
		r.CoverageLabel = efficiencyCoverageLabels[r.Coverage]
		if r.RatingExplanation == "" {
			r.RatingExplanation = efficiencyRatingExplanation(*r)
		}
		if sev, ok := severity[r.ShapeID]; ok && sev != "" {
			r.Severity = sev
		} else if r.BelowSlowThreshold {
			r.Severity = "below_threshold"
		} else {
			r.Severity = "not_listed"
		}
		if c, ok := captured[r.ShapeID]; ok && c.Descriptor != "" {
			r.Query, r.QueryTruncated, r.QueryVariants = c.Descriptor, c.DescriptorTruncated, c.DescriptorVariants
		} else if q := querySummary[r.ShapeID]; q != "" {
			r.Query = q
		}
		if observedMs > 0 {
			share := roundTo(r.TotalOperationMs/observedMs, 4)
			r.ImpactShare = &share
		}
		sl, ok := rep.Impact.SampleLatency[r.ShapeID]
		r.Latency = BuildLatencyAssessment(r.OperationCount, r.AvgMs, r.MaxMs, sl, ok, rep.Metadata.LatencyTargets)
		r.WorkloadImpact = BuildWorkloadImpact(r.ImpactRank, r.OperationCount, r.TotalOperationMs, rep.Impact.Coverage.ObservedOperations, observedMs, rep.Metadata.WorkloadDurationSec)
		r.RecommendationRanks = recRanks[r.ShapeID]
		v.CoverageCounts[r.Coverage]++
	}
	v.ShapesTotal = len(v.Rows)
	SortEfficiencyRows(v.Rows, EffSortEfficiency)
	for i := range v.Rows {
		v.Rows[i].EfficiencyOrder = i + 1
	}
	return v
}

func efficiencyRowFromCoverage(cs ExplainCoverageShape) EfficiencyViewRow {
	r := EfficiencyViewRow{
		ShapeID: cs.ShapeID, Namespace: cs.Namespace, Operation: cs.Operation, ShapeSummary: cs.ShapeSummary,
		OperationCount: cs.OperationCount, AvgMs: cs.AvgMs, MaxMs: cs.MaxMs, LatencyTier: cs.LatencyTier,
		BelowSlowThreshold: cs.BelowSlowThreshold, ImpactRank: cs.ImpactRank, TotalOperationMs: cs.TotalOperationMs,
		SelectedBy: cs.SelectedBy, SampleScope: cs.SampleScope, Selection: cs.Selection,
	}
	e := cs.Efficiency
	if e == nil {
		e = BuildQueryEfficiency(cs)
	}
	r.Status, r.Reason, r.Score, r.ScoreIsUpperBound = e.Status, e.Reason, e.Score, e.ScoreIsUpperBound
	r.Band, r.Confidence, r.ConfidenceBasis = e.Band, e.Confidence, e.ConfidenceBasis
	r.LimitingDimension, r.Dimensions, r.Observations = e.LimitingDimension, e.Dimensions, e.Observations
	r.Limitations, r.DeterminingSample, r.SamplesDisagree, r.EvidenceNote = e.Limitations, e.DeterminingSample, e.SamplesDisagree, e.EvidenceNote
	for _, o := range e.Observations {
		if n, ok := efficiencyObservationNotes[o]; ok {
			r.ObservationNotes = append(r.ObservationNotes, n)
		}
	}
	switch e.Status {
	case EfficiencyRated:
		r.Coverage = EffCoverageAssessed
	case EfficiencyPartial:
		r.Coverage, r.CoverageReason = EffCoveragePartial, firstNonEmpty(cs.PartialReason, e.Reason)
	case EfficiencyNotApplicable:
		r.Coverage, r.CoverageReason = EffCoverageNotApplicable, e.Reason
	default:
		r.CoverageReason = firstNonEmpty(cs.Reason, e.Reason)
		switch cs.Status {
		case "excluded":
			r.Coverage = EffCoverageBudgetExcluded
		case "failed":
			r.Coverage = EffCoverageFailed
		case "cancelled":
			r.Coverage = EffCoverageCancelled
		case "not_supported":
			r.Coverage = EffCoverageNotSupported
		default:
			r.Coverage = EffCoverageUnavailable
		}
	}
	if f := cs.Finding; f != nil {
		r.Diagnosis, r.Interpretation = f.Diagnosis, f.Interpretation
	}
	r.ExecutionOverhead = BuildExecutionOverhead(cs.Operation, cs.Samples, firstNonEmpty(r.CoverageReason, e.Reason))
	r.OtherMeasurements = BuildOtherMeasurements(cs.Operation, cs.Samples)
	byRole := make(map[string]EfficiencySample, len(e.Samples))
	for _, es := range e.Samples {
		byRole[es.Role] = es
	}
	for _, s := range cs.Samples {
		vs := EfficiencyViewSample{
			Role: s.Role, ClientLatencyMs: s.ClientLatencyMs, ExplainStatus: s.Status, ExplainReason: s.Reason,
			Attempts: s.Attempts, SharedCommand: s.SharedCommand, Evidence: "none", LatencyComparison: s.LatencyComparison,
		}
		if es, ok := byRole[s.Role]; ok {
			vs.Status, vs.Reason, vs.Score, vs.ScoreIsUpperBound, vs.Band, vs.Layout = es.Status, es.Reason, es.Score, es.ScoreIsUpperBound, es.Band, es.Layout
		}
		vs.ScoreLabel = efficiencyScoreLabel(vs.Status, vs.Score, vs.ScoreIsUpperBound)
		if d := s.Diagnostics; d != nil && s.Status == "explained" {
			vs.Evidence = "planner_only"
			if d.ExecutionStatsAvailable {
				vs.Evidence = "execution_stats"
				ms := d.ExecutionTimeMillis
				vs.ServerExecMs = &ms
			}
			vs.WinningPlan, vs.IndexesUsed = d.WinningPlanSummary, d.IndexesUsed
			if m := d.Metrics; m != nil {
				vs.DocsExamined, vs.KeysExamined, vs.Returned, vs.AccessReturned, vs.Matched = m.DocsExamined, m.KeysExamined, m.Returned, m.AccessReturned, m.Matched
			}
			if r.PlanSummary == "" || s.Role == r.DeterminingSample {
				r.PlanSummary = d.WinningPlanSummary
			}
		}
		r.Samples = append(r.Samples, vs)
	}
	return r
}

// efficiencyScoreLabel is the display text for a nullable score. A missing
// score is never rendered as a number.
func efficiencyScoreLabel(status string, score *int, upper bool) string {
	switch {
	case status == EfficiencyNotApplicable:
		return "Not applicable"
	case score == nil:
		return "Unassessed"
	case upper:
		return fmt.Sprintf("≤ %d", *score)
	}
	return fmt.Sprintf("%d", *score)
}

func efficiencyRatingExplanation(r EfficiencyViewRow) string {
	switch r.Status {
	case EfficiencyNotApplicable:
		return "Not applicable: this operation is not explained, so there is no scan to rate (" + emptyFallback(r.Reason, "not_applicable") + ")."
	case EfficiencyUnassessed, "":
		return fmt.Sprintf("No score: %s (%s).",
			strings.ToLower(emptyFallback(efficiencyCoverageLabels[r.Coverage], "unavailable")), emptyFallback(r.Reason, "explain_unavailable"))
	}
	var b strings.Builder
	if r.Status == EfficiencyPartial {
		fmt.Fprintf(&b, "At most %s: explain returned only some counts (%s), and the missing ones can only lower the score.", strings.TrimPrefix(r.ScoreLabel, "≤ "), emptyFallback(r.Reason, "partial_evidence"))
	} else {
		fmt.Fprintf(&b, "Score %s (%s).", r.ScoreLabel, r.Band)
	}
	if r.LimitingDimension != "" {
		for _, d := range r.Dimensions {
			if d.Name != r.LimitingDimension || d.Penalty == nil {
				continue
			}
			fmt.Fprintf(&b, " Limited by %s", emptyFallback(efficiencyDimensionNames[d.Name], d.Name))
			if d.Value != nil {
				fmt.Fprintf(&b, ": %s", trimFloat(*d.Value))
			}
			fmt.Fprintf(&b, " (penalty %.2f", *d.Penalty)
			if d.Note != "" {
				fmt.Fprintf(&b, ", %s", emptyFallback(efficiencyDimensionNotes[d.Note], d.Note))
			}
			b.WriteString(").")
		}
	} else {
		b.WriteString(" No check lowered the score.")
	}
	if r.DeterminingSample != "" {
		fmt.Fprintf(&b, " Lowest-scoring sample: %s.", r.DeterminingSample)
	}
	return b.String()
}

// EfficiencyDimensionLabel is the display name of a scan efficiency check.
func EfficiencyDimensionLabel(name string) string {
	return emptyFallback(efficiencyDimensionNames[name], name)
}

func trimFloat(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return s
}

// SortEfficiencyRows orders rows by worst efficiency or by workload impact.
func SortEfficiencyRows(rows []EfficiencyViewRow, by string) {
	impactLess := func(a, b EfficiencyViewRow) bool {
		ra, rb := a.ImpactRank, b.ImpactRank
		if ra == 0 {
			ra = 1 << 30
		}
		if rb == 0 {
			rb = 1 << 30
		}
		if ra != rb {
			return ra < rb
		}
		return a.ShapeID < b.ShapeID
	}
	if by == EffSortImpact {
		sort.SliceStable(rows, func(i, j int) bool { return impactLess(rows[i], rows[j]) })
		return
	}
	group := func(r EfficiencyViewRow) int {
		switch {
		case r.Score != nil:
			return 0
		case r.Status == EfficiencyNotApplicable:
			return 2
		}
		return 1
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if ga, gb := group(a), group(b); ga != gb {
			return ga < gb
		}
		if a.Score != nil && b.Score != nil {
			if *a.Score != *b.Score {
				return *a.Score < *b.Score
			}
			if a.ScoreIsUpperBound != b.ScoreIsUpperBound {
				return !a.ScoreIsUpperBound
			}
		}
		return impactLess(a, b)
	})
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
