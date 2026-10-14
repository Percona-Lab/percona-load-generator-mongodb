package stats

import (
	"fmt"
	"math"
	"strings"
)

const (
	// ExplainSelectionMethod names how explain samples are picked per shape.
	ExplainSelectionMethod = "nearest_representative_median_and_p95"
	// ExplainMaxSamplesPerShape bounds explain commands per shape (median + p95).
	ExplainMaxSamplesPerShape = 2

	ExplainTimingNote = "Client latency was measured during the run; explain timings were measured afterwards by the server under different load and cache state. A difference between them does not by itself establish a root cause."

	maxExplainRecommendations = 3
	maxInsightRecommendations = 16

	// A scan is treated as selective (and an index as potentially useful) only
	// when it examined at least this many documents per returned/matched one,
	// and at least selectiveScanMinDocs in total.
	selectiveScanRatio   = 10
	selectiveScanMinDocs = 1000
)

const (
	DiagCollscanSelective   = "collscan_selective_filter"
	DiagIndexLowSelectivity = "index_low_selectivity"
	DiagSpillToDisk         = "spill_to_disk"
	DiagBlockingSort        = "blocking_sort"
	DiagCollscanUnfiltered  = "collscan_unfiltered_expected"
	DiagCollscanLowSelect   = "collscan_low_selectivity"
	DiagIndexPipelineCost   = "index_backed_pipeline_cost"
	DiagCollscanPlanOnly    = "collscan_unquantified"
	DiagIndexEfficient      = "index_supported_efficient"
	DiagIndexPlanOnly       = "index_supported_plan_only"
	DiagUnclear             = "plan_signal_unclear"
)

// diagnosisOrder is most-specific first; it breaks ties between samples.
var diagnosisOrder = []string{
	DiagCollscanSelective,
	DiagIndexLowSelectivity,
	DiagSpillToDisk,
	DiagBlockingSort,
	DiagCollscanUnfiltered,
	DiagCollscanLowSelect,
	DiagIndexPipelineCost,
	DiagCollscanPlanOnly,
	DiagIndexEfficient,
	DiagIndexPlanOnly,
	DiagUnclear,
}

// diagnosisCap is the strongest confidence a diagnosis can support regardless
// of how many samples agree.
var diagnosisCap = map[string]string{
	DiagCollscanSelective:   "high",
	DiagIndexLowSelectivity: "high",
	DiagSpillToDisk:         "high",
	DiagBlockingSort:        "medium",
	DiagCollscanUnfiltered:  "medium",
	DiagCollscanLowSelect:   "medium",
	DiagIndexPipelineCost:   "medium",
	DiagIndexEfficient:      "medium",
	DiagCollscanPlanOnly:    "low",
	DiagIndexPlanOnly:       "low",
	DiagUnclear:             "low",
}

func diagnosisRank(d string) int {
	for i, v := range diagnosisOrder {
		if v == d {
			return i
		}
	}
	return len(diagnosisOrder)
}

func isWriteOp(op string) bool {
	switch op {
	case "updateOne", "updateMany", "deleteOne", "deleteMany":
		return true
	}
	return false
}

// resultCount is what the operation produced: returned documents for finds,
// the access-stage output for aggregates, matched documents for updates/deletes.
func resultCount(d *ExplainDiagnostics, op string) int64 {
	if m := d.Metrics; m != nil {
		c, _ := efficiencyResultCounter(*m, op)
		if c == nil && op == "aggregate" {
			c = m.Returned
		}
		if c == nil {
			return 0
		}
		return c.Value
	}
	if isWriteOp(op) {
		return d.NMatched
	}
	return d.NReturned
}

// ExplainResultCount is the operation's result count used as the ratio
// denominator (0 when unreported).
func ExplainResultCount(d *ExplainDiagnostics, op string) int64 { return resultCount(d, op) }

func resultLabel(op string) string {
	if isWriteOp(op) {
		return "nMatched"
	}
	return "nReturned"
}

// ClassifyExplainSample assigns a diagnosis to one explained sample from its
// parsed plan and execution metrics.
func ClassifyExplainSample(d *ExplainDiagnostics, op string) string {
	if d == nil {
		return DiagUnclear
	}
	ret := resultCount(d, op)
	denom := ret
	if denom < 1 {
		denom = 1
	}
	stats := d.ExecutionStatsAvailable
	switch {
	case d.CollectionScanDetected && stats && !d.UnfilteredScanExpected &&
		d.DocsExamined >= selectiveScanMinDocs && d.DocsExamined >= denom*selectiveScanRatio:
		return DiagCollscanSelective
	case d.IndexScanDetected && !d.CollectionScanDetected && stats &&
		maxInt64(d.KeysExamined, d.DocsExamined) >= selectiveScanMinDocs &&
		maxInt64(d.KeysExamined, d.DocsExamined) >= denom*selectiveScanRatio:
		return DiagIndexLowSelectivity
	case d.UsedDisk:
		return DiagSpillToDisk
	case d.BlockingSortDetected:
		return DiagBlockingSort
	case d.CollectionScanDetected && d.UnfilteredScanExpected:
		return DiagCollscanUnfiltered
	case d.CollectionScanDetected && stats:
		return DiagCollscanLowSelect
	case d.IndexScanDetected && d.GroupDetected:
		return DiagIndexPipelineCost
	case d.CollectionScanDetected:
		return DiagCollscanPlanOnly
	case d.IndexScanDetected && stats:
		return DiagIndexEfficient
	case d.IndexScanDetected:
		return DiagIndexPlanOnly
	default:
		return DiagUnclear
	}
}

// DescribeExplainSample fills the per-sample diagnosis, interpretation,
// recommendation, confidence, and evidence fields of d.
func DescribeExplainSample(d *ExplainDiagnostics, op string) {
	if d == nil {
		return
	}
	d.Diagnosis = ClassifyExplainSample(d, op)
	ctx := findingContext{op: op, namespace: d.ReplayDB + "." + d.ReplayCollection}
	d.Interpretation, d.Recommendation = diagnosisText(d.Diagnosis, ctx, d)
	conf := diagnosisCap[d.Diagnosis]
	if confidenceScore(conf) > confidenceScore("medium") {
		conf = "medium"
	}
	d.RecommendationConfidence = conf
	d.EvidenceSummary = strings.Join(sampleEvidence("sample", d, op), "; ")
}

type findingContext struct {
	op           string
	namespace    string
	shape        string
	filterFields []string
}

func diagnosisText(diag string, c findingContext, d *ExplainDiagnostics) (interpretation, recommendation string) {
	shape := c.shape
	if shape == "" {
		shape = c.op
	}
	ret := resultCount(d, c.op)
	label := resultLabel(c.op)
	idx := "none"
	if len(d.IndexesUsed) > 0 {
		idx = strings.Join(d.IndexesUsed, ", ")
	}
	fields := "this query's predicate fields"
	if len(c.filterFields) > 0 {
		fields = "the filter fields [" + strings.Join(c.filterFields, ", ") + "]"
	}
	switch diag {
	case DiagCollscanSelective:
		interpretation = fmt.Sprintf("The explained plan for %s on %s used COLLSCAN and examined %d documents for %s=%d, so the filter is selective but unsupported by an index.", shape, c.namespace, d.DocsExamined, label, ret)
		recommendation = fmt.Sprintf("Evaluate an index that supports %s on %s, then re-run explain on the same samples and compare docsExamined and latency before rollout.", fields, c.namespace)
	case DiagIndexLowSelectivity:
		interpretation = fmt.Sprintf("The explained plan for %s on %s used index %s but examined %d keys and %d documents for %s=%d.", shape, c.namespace, idx, d.KeysExamined, d.DocsExamined, label, ret)
		recommendation = fmt.Sprintf("Review whether index %s leads with the most selective of %s and in a suitable order; compare alternatives with explain before changing indexes.", idx, fields)
	case DiagSpillToDisk:
		stage := "a memory-heavy stage"
		if d.SortDetected {
			stage = "SORT"
		} else if d.GroupDetected {
			stage = "GROUP"
		}
		interpretation = fmt.Sprintf("The explained plan for %s on %s spilled to disk during %s (usedDisk=true, spills=%d).", shape, c.namespace, stage, d.Spills)
		recommendation = fmt.Sprintf("Reduce the memory needed by %s on %s: filter earlier, project fewer fields, or provide the sort order from an index; confirm with explain that spills drop to 0.", stage, c.namespace)
	case DiagBlockingSort:
		interpretation = fmt.Sprintf("The winning plan for %s on %s contains an in-memory SORT stage (indexes used: %s).", shape, c.namespace, idx)
		recommendation = fmt.Sprintf("If the sort order is required, evaluate whether an index on %s can provide it, and verify with explain that the SORT stage disappears.", c.namespace)
	case DiagCollscanUnfiltered:
		interpretation = fmt.Sprintf("%s on %s has no filter predicate, so a COLLSCAN is expected.", shape, c.namespace)
		recommendation = "An index would not help this query as written. Add a predicate or limit the result set if full scans are not intended."
	case DiagCollscanLowSelect:
		interpretation = fmt.Sprintf("The explained plan for %s on %s used COLLSCAN, but it examined %d documents for %s=%d, so the scan is small or the filter is not selective.", shape, c.namespace, d.DocsExamined, label, ret)
		recommendation = fmt.Sprintf("An index is unlikely to reduce work much for %s; investigate result size, document size, and concurrency before adding one.", c.namespace)
	case DiagIndexPipelineCost:
		interpretation = fmt.Sprintf("The explained plan for %s on %s is index-backed (%s) and then runs GROUP/SORT work after the match.", shape, c.namespace, idx)
		recommendation = "Focus on post-match work: tighten early $match/$project stages, review group cardinality, and check sort pressure."
	case DiagCollscanPlanOnly:
		interpretation = fmt.Sprintf("The plan for %s on %s shows COLLSCAN, but explain ran without execution metrics, so selectivity is unknown.", shape, c.namespace)
		recommendation = "Re-run explain with executionStats verbosity before deciding on an index; COLLSCAN alone does not prove an indexing problem."
	case DiagIndexEfficient:
		interpretation = fmt.Sprintf("The explained plan for %s on %s used index %s and examined %d keys and %d documents for %s=%d, which is efficient.", shape, c.namespace, idx, d.KeysExamined, d.DocsExamined, label, ret)
		recommendation = "The plan does not account for the measured latency. Investigate contention, document size, network, or server load during the run."
	case DiagIndexPlanOnly:
		interpretation = fmt.Sprintf("The plan for %s on %s uses index %s, but explain ran without execution metrics.", shape, c.namespace, idx)
		recommendation = "Re-run explain with executionStats verbosity to quantify examined keys and documents."
	default:
		interpretation = fmt.Sprintf("Explain completed for %s on %s, but no clear scan or index signal was found in the winning plan.", shape, c.namespace)
		recommendation = "Inspect the winning plan directly before making index changes."
	}
	return interpretation, recommendation
}

func diagnosisTitle(diag, namespace string) string {
	switch diag {
	case DiagCollscanSelective:
		return "Collection scan on a selective filter in " + namespace
	case DiagIndexLowSelectivity:
		return "Index used but scans many entries in " + namespace
	case DiagSpillToDisk:
		return "Query stage spills to disk in " + namespace
	case DiagBlockingSort:
		return "In-memory sort in " + namespace
	case DiagCollscanUnfiltered:
		return "Unfiltered full scan in " + namespace
	case DiagCollscanLowSelect:
		return "Collection scan with low selectivity in " + namespace
	case DiagIndexPipelineCost:
		return "Post-match pipeline cost in " + namespace
	case DiagCollscanPlanOnly:
		return "Collection scan without execution metrics in " + namespace
	case DiagIndexEfficient:
		return "Efficient plan does not explain latency in " + namespace
	case DiagIndexPlanOnly:
		return "Index-backed plan without execution metrics in " + namespace
	default:
		return "Explain plan inconclusive in " + namespace
	}
}

func sampleEvidence(role string, d *ExplainDiagnostics, op string) []string {
	out := make([]string, 0, 4)
	plan := d.WinningPlanSummary
	if plan == "" {
		plan = "stage_chain_unavailable"
	}
	if d.ExecutionStatsAvailable {
		out = append(out, fmt.Sprintf("%s plan %s: docsExamined=%d keysExamined=%d %s=%d", role, plan, d.DocsExamined, d.KeysExamined, resultLabel(op), resultCount(d, op)))
	} else {
		out = append(out, fmt.Sprintf("%s plan %s (verbosity %s, no execution metrics)", role, plan, emptyFallback(d.Verbosity, "queryPlanner")))
	}
	if len(d.IndexesUsed) > 0 {
		out = append(out, "indexes used: "+strings.Join(d.IndexesUsed, ", "))
	} else {
		out = append(out, "indexes used: none")
	}
	if d.BlockingSortDetected {
		out = append(out, "in-memory SORT stage present")
	}
	if d.UsedDisk {
		out = append(out, fmt.Sprintf("usedDisk=true spills=%d", d.Spills))
	}
	return out
}

func compareLatency(s ExplainSample) string {
	d := s.Diagnostics
	if s.Status != "explained" || d == nil || !d.ExecutionStatsAvailable {
		return "not_comparable"
	}
	client := s.ClientLatencyMs
	server := float64(d.ExecutionTimeMillis)
	switch {
	case client-server > 5 && client >= 2*math.Max(server, 1):
		return "client_slower_than_explain"
	case server-client > 5 && server >= 2*math.Max(client, 1):
		return "explain_slower_than_client"
	default:
		return "similar"
	}
}

func isInconclusiveStatus(status string) bool {
	return status == "timed_out" || status == "execution_failed"
}

// BuildExplainFinding builds the shape-level finding from explained samples.
// It returns nil when no sample produced a usable plan.
func BuildExplainFinding(sq SlowQueryInsight) *ExplainFinding {
	explained := make([]ExplainSample, 0, len(sq.ExplainSamples))
	inconclusive := 0
	for _, s := range sq.ExplainSamples {
		if s.Status == "explained" && s.Diagnostics != nil {
			explained = append(explained, s)
		} else if isInconclusiveStatus(s.Status) {
			inconclusive++
		}
	}
	if len(explained) == 0 {
		return nil
	}

	primary := explained[0]
	disagree := false
	allStats := true
	for _, s := range explained {
		if s.Diagnostics.Diagnosis != primary.Diagnostics.Diagnosis {
			disagree = true
		}
		if diagnosisRank(s.Diagnostics.Diagnosis) < diagnosisRank(primary.Diagnostics.Diagnosis) {
			primary = s
		}
		if !s.Diagnostics.ExecutionStatsAvailable {
			allStats = false
		}
	}
	diag := primary.Diagnostics.Diagnosis

	namespace := sq.Collection
	if db := strings.TrimSpace(primary.Diagnostics.ReplayDB); db != "" {
		namespace = db + "." + sq.Collection
	}
	ctx := findingContext{op: sq.Operation, namespace: namespace, shape: sq.ShapeSummary, filterFields: sq.FilterFields}
	interp, reco := diagnosisText(diag, ctx, primary.Diagnostics)

	quality, basis := "low", ""
	switch {
	case disagree:
		basis = "explained samples disagree on the diagnosis"
	case !allStats && needsExecutionStats(diag):
		basis = "no execution metrics (queryPlanner verbosity)"
	case len(explained) >= 2 && inconclusive == 0:
		quality = "high"
		basis = fmt.Sprintf("%d distinct samples agree", len(explained))
	default:
		quality = "medium"
		basis = fmt.Sprintf("%d of %d selected samples explained", len(explained), len(sq.ExplainSamples))
		if inconclusive > 0 {
			basis += fmt.Sprintf("; %d inconclusive", inconclusive)
		}
	}
	conf := quality
	if capConf := diagnosisCap[diag]; confidenceScore(capConf) < confidenceScore(conf) {
		conf = capConf
		basis += "; diagnosis type caps confidence at " + capConf
	}

	evidence := []string{fmt.Sprintf("namespace %s, shape %s", namespace, emptyFallback(sq.ShapeSummary, sq.ShapeKey))}
	if sel := sq.ExplainSelection; sel != nil {
		evidence = append(evidence, fmt.Sprintf("%d samples selected near median %.2fms and p95 %.2fms of %d representative samples; %d explained, %d inconclusive",
			len(sq.ExplainSamples), sel.MedianMs, sel.P95Ms, sel.RetainedSuccessfulSamples, len(explained), inconclusive))
		switch sel.Scope {
		case SampleScopeAllOperations:
		case SampleScopeReservoirDropped:
			evidence = append(evidence, "samples are a full-run reservoir with skipped admissions, so they may be biased")
		default:
			evidence = append(evidence, "samples are a uniform full-run reservoir, not every operation")
		}
	}
	seenIdx := make(map[string]struct{})
	indexes := make([]string, 0)
	for _, s := range explained {
		for _, e := range sampleEvidence(s.Role+" sample", s.Diagnostics, sq.Operation) {
			if strings.HasPrefix(e, "indexes used:") {
				continue
			}
			evidence = append(evidence, e)
		}
		for _, idx := range s.Diagnostics.IndexesUsed {
			if _, ok := seenIdx[idx]; !ok {
				seenIdx[idx] = struct{}{}
				indexes = append(indexes, idx)
			}
		}
	}
	if len(indexes) > 0 {
		evidence = append(evidence, "indexes used: "+strings.Join(indexes, ", "))
	} else {
		evidence = append(evidence, "indexes used: none")
	}
	if disagree {
		evidence = append(evidence, "samples produced different diagnoses")
	}

	return &ExplainFinding{
		Diagnosis:           diag,
		Title:               diagnosisTitle(diag, namespace),
		Namespace:           namespace,
		Interpretation:      interp,
		Recommendation:      reco,
		Confidence:          conf,
		ConfidenceBasis:     basis,
		Evidence:            evidence,
		IndexesUsed:         indexes,
		SamplesSelected:     len(sq.ExplainSamples),
		SamplesExplained:    len(explained),
		SamplesInconclusive: inconclusive,
		SamplesDisagree:     disagree,
		TimingNote:          ExplainTimingNote,
	}
}

func needsExecutionStats(diag string) bool {
	switch diag {
	case DiagBlockingSort, DiagCollscanUnfiltered, DiagIndexPipelineCost:
		return false
	}
	return true
}

func explainFindingPriority(f *ExplainFinding) string {
	switch f.Diagnosis {
	case DiagCollscanSelective, DiagIndexLowSelectivity, DiagSpillToDisk:
		if f.Confidence == "low" {
			return "medium"
		}
		return "high"
	case DiagBlockingSort, DiagIndexPipelineCost:
		return "medium"
	default:
		return "low"
	}
}

// ApplyExplainFindings derives shape status, findings, index-issue evidence,
// and recommendations from explain samples attached to SlowQueries. Call it
// after explain enrichment so every view reflects the same evidence.
func ApplyExplainFindings(rep *InsightsReport) {
	byShape := make(map[string]*SlowQueryInsight, len(rep.SlowQueries))
	anyExplained := false
	for i := range rep.SlowQueries {
		sq := &rep.SlowQueries[i]
		byShape[sq.ShapeID] = sq
		if len(sq.ExplainSamples) == 0 {
			continue
		}
		for j := range sq.ExplainSamples {
			sq.ExplainSamples[j].LatencyComparison = compareLatency(sq.ExplainSamples[j])
		}
		sq.ExplainFinding = BuildExplainFinding(*sq)
		sq.ExplainDiag = primarySampleDiagnostics(sq.ExplainSamples)
		if f := sq.ExplainFinding; f != nil {
			anyExplained = true
			sq.ExplainStatus = "explained"
			sq.ExplainReason = f.Diagnosis
			continue
		}
		first := sq.ExplainSamples[0]
		sq.ExplainStatus = first.Status
		sq.ExplainReason = first.Reason
	}

	finalizeExplainCoverage(rep, byShape)
	applyQueryEfficiency(rep)

	for i := range rep.QueryShapes {
		qs := &rep.QueryShapes[i]
		if sq, ok := byShape[qs.ShapeID]; ok {
			qs.ExplainStatus = sq.ExplainStatus
			qs.ExplainReason = sq.ExplainReason
			qs.ExplainDiag = sq.ExplainDiag
			qs.ExplainSelection = sq.ExplainSelection
			qs.ExplainSamples = sq.ExplainSamples
			qs.ExplainFinding = sq.ExplainFinding
		}
	}

	for i := range rep.PotentialIndexIssues {
		applyFindingToIssue(&rep.PotentialIndexIssues[i], byShape[rep.PotentialIndexIssues[i].ShapeID])
	}

	if anyExplained {
		rep.Metadata.EvidenceLevel = "heuristic_plus_explain_samples"
	}
	rep.Recommendations = buildRecommendations(*rep)
}

// primarySampleDiagnostics prefers the explained p95 sample, then any explained
// sample, then the first attempted sample.
func primarySampleDiagnostics(samples []ExplainSample) *ExplainDiagnostics {
	var fallback *ExplainDiagnostics
	for _, s := range samples {
		if s.Status != "explained" || s.Diagnostics == nil {
			continue
		}
		if strings.Contains(s.Role, "p95") {
			return s.Diagnostics
		}
		if fallback == nil {
			fallback = s.Diagnostics
		}
	}
	if fallback != nil {
		return fallback
	}
	for _, s := range samples {
		if s.Diagnostics != nil {
			return s.Diagnostics
		}
	}
	return nil
}

func applyFindingToIssue(issue *IndexIssue, sq *SlowQueryInsight) {
	if sq == nil {
		issue.ExplainStatus = "explain_unavailable"
		issue.ExplainReason = "shape_not_in_top_slow_queries"
		issue.EvidenceLevel = "heuristic_explain_unavailable"
		issue.Confidence = "low"
		issue.Message = "No representative explain sample was available for this shape. Keeping heuristic guidance."
		return
	}
	issue.ExplainStatus = sq.ExplainStatus
	issue.ExplainReason = sq.ExplainReason
	if f := sq.ExplainFinding; f != nil {
		issue.EvidenceLevel = "explain_" + f.Diagnosis
		issue.Diagnosis = f.Diagnosis
		issue.Confidence = f.Confidence
		issue.Message = f.Interpretation
		issue.Recommendation = f.Recommendation
		issue.Evidence = append([]string(nil), f.Evidence...)
		return
	}
	issue.Confidence = "low"
	switch sq.ExplainStatus {
	case "timed_out", "execution_failed":
		issue.EvidenceLevel = "heuristic_explain_inconclusive"
		issue.Message = fmt.Sprintf("Explain %s for the selected samples (%s), so plan evidence is inconclusive. Keeping heuristic guidance.", strings.ReplaceAll(sq.ExplainStatus, "_", " "), sq.ExplainReason)
	case "cancelled":
		issue.EvidenceLevel = "heuristic_explain_cancelled"
		issue.Message = "Explain was cancelled before this shape's samples ran. Keeping heuristic guidance."
	case "not_supported":
		issue.EvidenceLevel = "heuristic_not_supported"
		issue.Message = "This operation type is not currently explain-supported in post-run analysis. Keeping heuristic guidance."
	case "insufficient_metadata":
		issue.EvidenceLevel = "heuristic_insufficient_metadata"
		issue.Message = fmt.Sprintf("Explain could not run because the executable query was unavailable (%s). Keeping heuristic guidance.", sq.ExplainReason)
	default:
		issue.EvidenceLevel = "heuristic_explain_unavailable"
		issue.Message = "Explain sampling was enabled, but this shape was not selected for explain in this run. Keeping heuristic guidance."
	}
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
