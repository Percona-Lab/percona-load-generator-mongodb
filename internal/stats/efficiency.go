package stats

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Scan efficiency rating (docs/query-efficiency-spec.md). The constants are
// versioned product heuristics; changing any of them requires a new
// EfficiencyVersion.
const (
	EfficiencyVersion = "se-1"

	EfficiencyRated         = "rated"
	EfficiencyPartial       = "partial"
	EfficiencyUnassessed    = "unassessed"
	EfficiencyNotApplicable = "not_applicable"

	EfficiencyBandEfficient   = "efficient"
	EfficiencyBandModerate    = "moderate"
	EfficiencyBandInefficient = "inefficient"
	EfficiencyBandUnconfirmed = "unconfirmed"

	DimDocsPerResult = "docs_per_result"
	DimKeysPerResult = "keys_per_result"
	DimCollscan      = "collscan"

	// Ratio dimensions are not penalized below this much examined work.
	efficiencyWorkFloor = 100
	// Filtered collection-scan floor, Big-hole COLLSCAN_SCORE (MIT, zelmario).
	efficiencyCollscanPenalty = 0.62
	efficiencyLargeWorkDocs   = 10000
	efficiencyEfficientMin    = 75
	efficiencyModerateMin     = 50

	EfficiencyMethodNote     = "Scan efficiency, 0-100, higher is better: how much the server examined to produce each result. The score is set by the worst of three checks: documents examined per result, index keys examined per result, and a filtered collection scan (which caps the score at 38). The counts come from explain (executionStats) of up to 2 sample commands per shape, run after the workload. It is a PLGM heuristic, not a MongoDB metric and not an estimate of time saved."
	efficiencyEvidenceSource = "post_run_explain_of_representative_samples"
	efficiencyEvidenceNote   = "Explain ran after the workload finished, so cache state and concurrent load differed from the run. The counts describe the sample commands, not every execution of the shape."
)

// legacyEfficiencyVersions are earlier identifiers of the current calculation.
// "qe-1" is the same formula and constants as "se-1" under its former name.
var legacyEfficiencyVersions = map[string]string{"qe-1": EfficiencyVersion}

// CanonicalScoreVersion maps a stored score version to its current identifier
// so renamed but identical calculations stay comparable.
func CanonicalScoreVersion(v string) string {
	if c, ok := legacyEfficiencyVersions[v]; ok {
		return c
	}
	return v
}

// EfficiencyDimension is one scored signal. A nil Penalty on an applicable
// dimension means the evidence was missing; it is never read as zero.
type EfficiencyDimension struct {
	Name          string   `json:"name"`
	Applicable    bool     `json:"applicable"`
	Value         *float64 `json:"value,omitempty"`
	Penalty       *float64 `json:"penalty,omitempty"`
	MissingReason string   `json:"missing_reason,omitempty"`
	Note          string   `json:"note,omitempty"`
	Source        string   `json:"source,omitempty"`
}

// EfficiencySample is the scan efficiency assessment of one explained sample
// command.
type EfficiencySample struct {
	Role              string                `json:"role"`
	Status            string                `json:"status"`
	Reason            string                `json:"reason,omitempty"`
	Score             *int                  `json:"score,omitempty"`
	ScoreIsUpperBound bool                  `json:"score_is_upper_bound,omitempty"`
	Band              string                `json:"band,omitempty"`
	LimitingDimension string                `json:"limiting_dimension,omitempty"`
	Layout            string                `json:"layout,omitempty"`
	Dimensions        []EfficiencyDimension `json:"dimensions,omitempty"`
	Observations      []string              `json:"observations,omitempty"`
	Evidence          []string              `json:"evidence,omitempty"`
	approximate       bool
	sharded           bool
	notes             []string
}

// QueryEfficiency is the shape-level scan efficiency assessment: the worst assessed
// sample decides the score, and every sample's assessment is kept.
type QueryEfficiency struct {
	Version           string                `json:"version"`
	Status            string                `json:"status"`
	Reason            string                `json:"reason,omitempty"`
	Score             *int                  `json:"score,omitempty"`
	ScoreIsUpperBound bool                  `json:"score_is_upper_bound,omitempty"`
	Band              string                `json:"band,omitempty"`
	Confidence        string                `json:"confidence,omitempty"`
	ConfidenceBasis   string                `json:"confidence_basis,omitempty"`
	LimitingDimension string                `json:"limiting_dimension,omitempty"`
	DeterminingSample string                `json:"determining_sample,omitempty"`
	SamplesSelected   int                   `json:"samples_selected"`
	SamplesRated      int                   `json:"samples_rated"`
	SamplesPartial    int                   `json:"samples_partial"`
	SamplesDisagree   bool                  `json:"samples_disagree"`
	Dimensions        []EfficiencyDimension `json:"dimensions,omitempty"`
	Observations      []string              `json:"observations,omitempty"`
	Samples           []EfficiencySample    `json:"samples,omitempty"`
	EvidenceSource    string                `json:"evidence_source,omitempty"`
	EvidenceNote      string                `json:"evidence_note,omitempty"`
	SampleScope       string                `json:"sample_scope,omitempty"`
	Limitations       []string              `json:"limitations,omitempty"`
}

// EfficiencyCoverageView is the report-level scan efficiency summary over every tracked
// shape. Unassessed shapes are not efficient; they have no score.
type EfficiencyCoverageView struct {
	Version             string         `json:"version"`
	Status              string         `json:"status"`
	Reason              string         `json:"reason,omitempty"`
	Method              string         `json:"method"`
	ShapesTotal         int            `json:"shapes_total"`
	ShapesRated         int            `json:"shapes_rated"`
	ShapesPartial       int            `json:"shapes_partial"`
	ShapesUnassessed    int            `json:"shapes_unassessed"`
	ShapesNotApplicable int            `json:"shapes_not_applicable"`
	ShapesByBand        map[string]int `json:"shapes_by_band,omitempty"`
	UnassessedByReason  map[string]int `json:"unassessed_by_reason,omitempty"`
	UntrackedOperations uint64         `json:"untracked_operations"`
	Limitations         []string       `json:"limitations,omitempty"`
}

// NewEfficiencyCoverage returns the coverage view for a report state.
func NewEfficiencyCoverage(status, reason string) *EfficiencyCoverageView {
	return &EfficiencyCoverageView{Version: EfficiencyVersion, Status: status, Reason: reason, Method: EfficiencyMethodNote}
}

// ratioPenalty is Big-hole's ratioScore (MIT): 0 at or below 1:1, 1 at 1000:1.
func ratioPenalty(r float64) float64 {
	if r <= 1 {
		return 0
	}
	return math.Min(1, math.Log10(r)/3)
}

func efficiencyScore(penalty float64) int {
	return int(math.Round(100 * (1 - penalty)))
}

func efficiencyBand(score int) string {
	switch {
	case score >= efficiencyEfficientMin:
		return EfficiencyBandEfficient
	case score >= efficiencyModerateMin:
		return EfficiencyBandModerate
	default:
		return EfficiencyBandInefficient
	}
}

// upperBoundBand names a band only when every score at or below the bound
// shares it short of efficient; a bound in the efficient range is unconfirmed.
func upperBoundBand(bound int) string {
	if bound >= efficiencyEfficientMin {
		return EfficiencyBandUnconfirmed
	}
	return efficiencyBand(bound)
}

func roundTo(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

func floatPtr(v float64) *float64 { return &v }

func isInsertOp(op string) bool {
	switch op {
	case "insert", "insertOne", "insertMany":
		return true
	}
	return false
}

// efficiencyResultCounter is the denominator: returned documents for find,
// the access-stage output for aggregate, and matched documents for writes.
func efficiencyResultCounter(m ExplainMetrics, op string) (*ExplainCounter, string) {
	switch {
	case isWriteOp(op):
		return m.Matched, "matched_count_not_reported"
	case op == "aggregate":
		return m.AccessReturned, "access_stage_count_unavailable"
	default:
		return m.Returned, "returned_count_not_reported"
	}
}

func ratioDimension(name string, examined, result *ExplainCounter, missing, role string) EfficiencyDimension {
	dim := EfficiencyDimension{Name: name, Applicable: true}
	switch {
	case missing != "":
		dim.MissingReason = missing
		return dim
	case examined == nil:
		dim.MissingReason = strings.TrimSuffix(name, "_per_result") + "_examined_not_reported"
		return dim
	case result == nil:
		return dim
	}
	dim.Source = examined.Source + " / " + result.Source + "@" + role
	var ratio float64
	if result.Value == 0 {
		// Zero results: the examined count itself is the work spent per empty
		// answer, so the ratio reads as examined-per-one rather than dividing by 0.
		ratio = float64(examined.Value)
		dim.Note = "zero_result"
	} else {
		ratio = float64(examined.Value) / float64(result.Value)
	}
	dim.Value = floatPtr(roundTo(ratio, 2))
	penalty := ratioPenalty(ratio)
	if examined.Value < efficiencyWorkFloor {
		penalty = 0
		dim.Note = "below_work_floor"
	}
	dim.Penalty = floatPtr(roundTo(penalty, 4))
	return dim
}

func counterEvidence(label string, c *ExplainCounter) string {
	if c == nil {
		return label + "=not_reported"
	}
	s := fmt.Sprintf("%s=%d (%s)", label, c.Value, c.Source)
	if c.LowerBound {
		s += " lower bound"
	}
	return s
}

func appendUnique(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

// scoreEfficiencySample applies scan efficiency to one sample. Missing evidence makes the
// sample partial with an upper-bound score; it never fills in zeros.
func scoreEfficiencySample(s ExplainSample, op string) EfficiencySample {
	out := EfficiencySample{Role: s.Role}
	d := s.Diagnostics
	if s.Status != "explained" || d == nil {
		out.Status, out.Reason = EfficiencyUnassessed, sampleUnassessedReason(s.Status, s.Reason)
		return out
	}
	var m ExplainMetrics
	metricsMissing := ""
	if d.Metrics != nil {
		m = *d.Metrics
	} else {
		metricsMissing = "normalized_metrics_unavailable"
	}
	out.Layout = m.Layout
	out.sharded = m.Shards > 0
	out.notes = append(out.notes, m.Notes...)

	coll := EfficiencyDimension{Name: DimCollscan, Applicable: true, Source: "queryPlanner.winningPlan@" + s.Role}
	switch {
	case d.CollectionScanDetected && !d.UnfilteredScanExpected:
		coll.Penalty, coll.Note = floatPtr(efficiencyCollscanPenalty), "filtered_collection_scan"
	case d.CollectionScanDetected:
		coll.Penalty, coll.Note = floatPtr(0), "unfiltered_scan_expected"
		out.Observations = append(out.Observations, "unfiltered_scan_expected")
	default:
		coll.Penalty, coll.Note = floatPtr(0), "no_collection_scan"
	}

	ratioMissing := metricsMissing
	if ratioMissing == "" && !d.ExecutionStatsAvailable {
		ratioMissing = "execution_stats_unavailable"
	}
	if ratioMissing == "" && m.UnsupportedReason != "" {
		ratioMissing = m.UnsupportedReason
	}
	result, resultMissing := efficiencyResultCounter(m, op)
	if ratioMissing == "" && result == nil {
		ratioMissing = resultMissing
	}
	docs := ratioDimension(DimDocsPerResult, m.DocsExamined, result, ratioMissing, s.Role)
	keys := ratioDimension(DimKeysPerResult, m.KeysExamined, result, ratioMissing, s.Role)
	out.Dimensions = []EfficiencyDimension{docs, keys, coll}

	if d.ExecutionStatsAvailable && ratioMissing == "" {
		out.Evidence = append(out.Evidence,
			counterEvidence("docsExamined", m.DocsExamined),
			counterEvidence("keysExamined", m.KeysExamined),
			counterEvidence(resultLabel(op), result))
		if op == "aggregate" {
			out.Evidence = append(out.Evidence, counterEvidence("final pipeline output", m.Returned))
		}
	}
	if result != nil && result.LowerBound {
		out.approximate = true
		out.Observations = append(out.Observations, "matched_count_lower_bound")
	}
	if d.ExecutionStatsAvailable && ratioMissing == "" && result != nil {
		if result.Value == 0 {
			out.Observations = append(out.Observations, "zero_result")
		}
		if !isWriteOp(op) && m.DocsExamined != nil && m.DocsExamined.Value == 0 && m.KeysExamined != nil &&
			d.IndexScanDetected && !d.FetchDetected && !d.CollectionScanDetected {
			out.Observations = append(out.Observations, "covered")
		}
		if op == "aggregate" && m.Returned != nil && m.Returned.Value < result.Value {
			out.Observations = append(out.Observations, "aggregate_reduction")
		}
	}
	if docs.Note == "below_work_floor" || keys.Note == "below_work_floor" {
		out.Observations = append(out.Observations, "below_work_floor")
	}
	if m.DocsExamined != nil && m.DocsExamined.Value > efficiencyLargeWorkDocs {
		out.Observations = append(out.Observations, "large_work")
	}
	if d.BlockingSortDetected {
		out.Observations = append(out.Observations, "blocking_sort")
	}
	if d.UsedDisk {
		out.Observations = append(out.Observations, "spill")
	}

	worst, limiting, missing := 0.0, "", false
	for _, dim := range out.Dimensions {
		if !dim.Applicable {
			continue
		}
		if dim.Penalty == nil {
			missing = true
			continue
		}
		if *dim.Penalty > worst {
			worst, limiting = *dim.Penalty, dim.Name
		}
	}
	score := efficiencyScore(worst)
	out.Score, out.LimitingDimension = &score, limiting
	if missing {
		out.Status, out.ScoreIsUpperBound, out.Band = EfficiencyPartial, true, upperBoundBand(score)
		out.Reason = ratioMissing
	} else {
		out.Status, out.Band = EfficiencyRated, efficiencyBand(score)
	}
	return out
}

func sampleUnassessedReason(status, reason string) string {
	switch status {
	case "timed_out":
		return "explain_timeout"
	case "execution_failed":
		return "explain_failed"
	case "cancelled":
		return "explain_cancelled"
	case "not_supported":
		return "unsupported_operation"
	case "insufficient_metadata":
		return "insufficient_metadata"
	}
	return emptyFallback(reason, "explain_unavailable")
}

// coverageUnassessedReason maps an unexplained coverage status to a reason.
func coverageUnassessedReason(cs ExplainCoverageShape) string {
	switch cs.Status {
	case "excluded", "unavailable":
		return emptyFallback(cs.Reason, "explain_unavailable")
	case "cancelled":
		return "explain_cancelled"
	case "failed":
		if strings.HasPrefix(cs.Reason, "timed_out") {
			return "explain_timeout"
		}
		return "explain_failed"
	case "not_supported":
		return "unsupported_operation"
	case "pending":
		return "explain_pending"
	}
	return emptyFallback(cs.Reason, "explain_unavailable")
}

// BuildQueryEfficiency rates one tracked shape from its explain coverage entry.
func BuildQueryEfficiency(cs ExplainCoverageShape) *QueryEfficiency {
	q := &QueryEfficiency{Version: EfficiencyVersion, SamplesSelected: len(cs.Samples)}
	if isInsertOp(cs.Operation) {
		q.Status, q.Reason = EfficiencyNotApplicable, "operation_not_explainable"
		return q
	}
	if cs.Status != "explained" {
		q.Status, q.Reason = EfficiencyUnassessed, coverageUnassessedReason(cs)
		return q
	}
	q.EvidenceSource, q.EvidenceNote, q.SampleScope = efficiencyEvidenceSource, efficiencyEvidenceNote, cs.SampleScope

	var det *EfficiencySample
	bands := map[string]struct{}{}
	unassessed, approximate, sharded := 0, false, false
	var notes []string
	for _, s := range cs.Samples {
		es := scoreEfficiencySample(s, cs.Operation)
		q.Samples = append(q.Samples, es)
		switch es.Status {
		case EfficiencyRated:
			q.SamplesRated++
		case EfficiencyPartial:
			q.SamplesPartial++
		default:
			unassessed++
			continue
		}
		bands[es.Band] = struct{}{}
		approximate = approximate || es.approximate
		sharded = sharded || es.sharded
		for _, n := range es.notes {
			notes = appendUnique(notes, n)
		}
		for _, o := range es.Observations {
			q.Observations = appendUnique(q.Observations, o)
		}
	}
	for i := range q.Samples {
		es := &q.Samples[i]
		if es.Score == nil {
			continue
		}
		if det == nil || *es.Score < *det.Score || (*es.Score == *det.Score && det.Status == EfficiencyRated && es.Status == EfficiencyPartial) {
			det = es
		}
	}
	if det == nil {
		q.Status = EfficiencyUnassessed
		q.Reason = "explain_unavailable"
		if len(q.Samples) > 0 {
			q.Reason = q.Samples[0].Reason
		}
		return q
	}

	score := *det.Score
	q.Score, q.LimitingDimension, q.DeterminingSample = &score, det.LimitingDimension, det.Role
	q.Dimensions = append([]EfficiencyDimension(nil), det.Dimensions...)
	q.SamplesDisagree = len(bands) > 1
	if q.SamplesPartial > 0 {
		q.Status, q.ScoreIsUpperBound, q.Band = EfficiencyPartial, true, upperBoundBand(score)
		q.Reason = firstPartialReason(q.Samples)
		q.Confidence = "low"
		q.ConfidenceBasis = "explain returned only some counts, so the score is the highest it could be"
	} else {
		q.Status, q.Band = EfficiencyRated, efficiencyBand(score)
		q.Confidence, q.ConfidenceBasis = "high", fmt.Sprintf("%d explained samples agree", q.SamplesRated)
		var basis []string
		if q.SamplesRated < 2 {
			basis = append(basis, fmt.Sprintf("%d rated sample", q.SamplesRated))
		}
		if q.SamplesDisagree {
			basis = append(basis, "samples fall in different bands")
		}
		if sharded {
			basis = append(basis, "sharded explain: shards can choose different plans")
		}
		if approximate {
			basis = append(basis, "a result count is a lower bound")
		}
		if len(basis) > 0 {
			q.Confidence, q.ConfidenceBasis = "medium", strings.Join(basis, "; ")
		}
	}

	if unassessed > 0 {
		q.Limitations = append(q.Limitations, fmt.Sprintf("Explain did not finish for %d of %d sample commands (timeout, failure, or cancellation); the score uses the others.", unassessed, len(cs.Samples)))
	}
	if q.SamplesDisagree {
		q.Limitations = append(q.Limitations, "The sample commands fall in different bands; the lowest score is shown and each sample's score is kept.")
	}
	if cs.SampleScope == SampleScopeReservoirDropped {
		q.Limitations = append(q.Limitations, "Some operations were too large to keep as samples, so the sample commands may not represent the whole shape.")
	}
	for _, n := range notes {
		switch n {
		case "lookup_work_not_scored":
			q.Limitations = append(q.Limitations, "Documents examined by $lookup in the joined collection are not part of this score; see the $lookup measurement.")
		}
	}
	return q
}

func firstPartialReason(samples []EfficiencySample) string {
	for _, s := range samples {
		if s.Status == EfficiencyPartial && s.Reason != "" {
			return s.Reason
		}
	}
	return "partial_evidence"
}

// applyQueryEfficiency rates every coverage shape and rebuilds the report-level
// efficiency coverage.
func applyQueryEfficiency(rep *InsightsReport) {
	cov := &rep.ExplainCoverage
	if cov.Status != "ready" {
		rep.EfficiencyCoverage = NewEfficiencyCoverage(cov.Status, cov.Reason)
		return
	}
	ec := NewEfficiencyCoverage("ready", "")
	rep.EfficiencyCoverage = ec
	ec.ShapesByBand, ec.UnassessedByReason = map[string]int{}, map[string]int{}
	ec.UntrackedOperations = cov.Sampling.UntrackedOperations
	for i := range cov.Shapes {
		e := BuildQueryEfficiency(cov.Shapes[i])
		cov.Shapes[i].Efficiency = e
		ec.ShapesTotal++
		switch e.Status {
		case EfficiencyRated:
			ec.ShapesRated++
			ec.ShapesByBand[e.Band]++
		case EfficiencyPartial:
			ec.ShapesPartial++
			ec.ShapesByBand[e.Band]++
		case EfficiencyNotApplicable:
			ec.ShapesNotApplicable++
		default:
			ec.ShapesUnassessed++
			ec.UnassessedByReason[e.Reason]++
		}
	}
	ec.Limitations = append(ec.Limitations, efficiencyEvidenceNote)
	if ec.ShapesUnassessed > 0 {
		reasons := make([]string, 0, len(ec.UnassessedByReason))
		for r := range ec.UnassessedByReason {
			reasons = append(reasons, r)
		}
		sort.Strings(reasons)
		ec.Limitations = append(ec.Limitations, fmt.Sprintf("%d shapes have no scan efficiency score because they were not explained (%s).", ec.ShapesUnassessed, strings.Join(reasons, ", ")))
	}
	if ec.ShapesPartial > 0 {
		ec.Limitations = append(ec.Limitations, fmt.Sprintf("%d shapes have only some explain counts; their score (≤ N) is the highest it could be.", ec.ShapesPartial))
	}
	if ec.UntrackedOperations > 0 {
		ec.Limitations = append(ec.Limitations, fmt.Sprintf("%d operations belong to shapes beyond insights_max_groups and have no row.", ec.UntrackedOperations))
	}
}
