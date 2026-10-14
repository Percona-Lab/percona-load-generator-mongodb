package webui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/report"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
)

// reportEfficiencySection maps the cached efficiency view into the HTML report
// model so the report shows the same rows, order, and labels as /api/insights.
func reportEfficiencySection(v *stats.EfficiencyView, recs []stats.InsightRecommendation) *report.EfficiencySection {
	if v == nil {
		return nil
	}
	sec := &report.EfficiencySection{
		Status: v.Status, Reason: v.Reason, Notice: v.Notice, RatingVersions: ratingVersionsText(v.RatingVersions),
		LatencyTargets: latencyTargetsText(v.LatencyTargets), SortNote: v.SortNote,
		Limitations: v.Limitations,
	}
	for _, c := range v.Columns {
		sec.Columns = append(sec.Columns, report.KV{Key: c.Title, Value: c.Explanation})
	}
	sec.Coverage = append(sec.Coverage, report.KV{Key: "Tracked shapes", Value: fmt.Sprint(v.ShapesTotal)})
	keys := make([]string, 0, len(v.CoverageCounts))
	for k := range v.CoverageCounts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		sec.Coverage = append(sec.Coverage, report.KV{Key: emptyFallback(v.CoverageLabels[k], k), Value: fmt.Sprint(v.CoverageCounts[k])})
	}
	sec.Unrecorded = append(sec.Unrecorded, v.Unrecorded.Paths...)
	if v.Unrecorded.UntrackedOperations > 0 {
		sec.Unrecorded = append(sec.Unrecorded, fmt.Sprintf("untracked_shapes: %d operations belong to shapes beyond the group limit and have no row", v.Unrecorded.UntrackedOperations))
	}
	byRank := make(map[int]stats.InsightRecommendation, len(recs))
	for _, r := range recs {
		byRank[r.Rank] = r
	}
	for _, r := range v.Rows {
		row := report.EfficiencyRow{
			Order: r.EfficiencyOrder, ShapeID: r.ShapeID, Namespace: r.Namespace, Operation: r.Operation,
			Shape: r.ShapeSummary, Query: r.Query, ScoreLabel: r.ScoreLabel, Band: r.Band, Confidence: r.Confidence,
			Coverage: r.CoverageLabel, CoverageReason: r.CoverageReason,
			Latency: r.Latency.Summary, LatencyRating: r.Latency.Rating.Label, LatencyTier: r.LatencyTier, Severity: r.Severity,
			Impact:   impactText(r),
			Overhead: r.ExecutionOverhead.Label, OverheadEvidence: overheadDetails(r.ExecutionOverhead),
			OverheadSummary: r.ExecutionOverhead.Measurements.Summary, OverheadMeasurementStatus: r.ExecutionOverhead.Measurements.Status,
			OverheadMeasurements: r.ExecutionOverhead.Measurements.Details, OverheadNotes: r.ExecutionOverhead.Measurements.Notes,
			Lookup: r.OtherMeasurements.Lookup.Label, Locks: r.OtherMeasurements.Locks.Label, Network: r.OtherMeasurements.Network.Label,
			OtherDetails:      otherDetails(r.OtherMeasurements),
			RatingExplanation: r.RatingExplanation, PlanSummary: r.PlanSummary,
			Observations: r.ObservationNotes, Limitations: r.Limitations,
		}
		for _, d := range r.Dimensions {
			row.Components = append(row.Components, efficiencyComponentText(d))
		}
		for _, s := range r.Samples {
			row.Samples = append(row.Samples, efficiencySampleText(s))
		}
		row.DataSource = efficiencyDataSource(r)
		for _, rank := range r.RecommendationRanks {
			rec, ok := byRank[rank]
			if !ok {
				continue
			}
			row.Recommendations = append(row.Recommendations, report.EfficiencyRecommendation{
				Rank: rec.Rank, Title: rec.Title, CandidateKeys: rec.CandidateKeys, Rationale: rec.Rationale,
				Tradeoffs: rec.Tradeoffs, ValidationStep: rec.ValidationStep, Limitations: rec.Limitations,
			})
		}
		sec.Rows = append(sec.Rows, row)
	}
	return sec
}

func impactText(r stats.EfficiencyViewRow) string {
	if r.ImpactRank == 0 {
		return "not ranked · " + r.WorkloadImpact.Summary
	}
	return fmt.Sprintf("#%d · %s", r.ImpactRank, r.WorkloadImpact.Summary)
}

func ratingVersionsText(v map[string]string) string {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, strings.ReplaceAll(k, "_", " ")+" "+v[k])
	}
	return strings.Join(parts, ", ")
}

func latencyTargetsText(t stats.LatencyTargets) string {
	var parts []string
	for _, p := range []struct {
		name string
		v    float64
	}{{"avg", t.AvgMs}, {"p95", t.P95Ms}, {"p99", t.P99Ms}} {
		if p.v > 0 {
			parts = append(parts, fmt.Sprintf("%s ≤ %s ms", p.name, trimNumber(p.v)))
		}
	}
	if len(parts) == 0 {
		return "none set (latency is shown but not rated)"
	}
	return strings.Join(parts, ", ")
}

func overheadDetails(o stats.ExecutionOverheadAssessment) []string {
	out := append([]string{}, o.Sort.Evidence...)
	out = append(out, o.Spill.Evidence...)
	if o.Spill.Status == stats.SignalNotMeasured && o.Status != stats.OverheadNotApplicable {
		out = append(out, "spill not measured ("+emptyFallback(o.Spill.Reason, "execution_stats_unavailable")+")")
	}
	return out
}

func otherDetails(m stats.OtherMeasurements) []string {
	var out []string
	for _, x := range []struct {
		name string
		m    stats.Measurement
	}{{"$lookup", m.Lookup}, {"locks", m.Locks}, {"network", m.Network}} {
		for _, e := range x.m.Evidence {
			out = append(out, x.name+": "+e)
		}
		if x.m.Status != stats.MeasureMeasured && x.m.Status != stats.MeasureNotApplicable && x.m.Reason != "" {
			out = append(out, x.name+": "+x.m.Reason)
		}
	}
	return out
}

func efficiencyComponentText(d stats.EfficiencyDimension) string {
	name := stats.EfficiencyDimensionLabel(d.Name)
	if !d.Applicable {
		return name + ": not applicable"
	}
	var b strings.Builder
	b.WriteString(name + ": ")
	if d.Penalty == nil {
		b.WriteString("not measured (" + emptyFallback(d.MissingReason, "evidence_missing") + ")")
	} else {
		if d.Value != nil {
			fmt.Fprintf(&b, "value %s, ", trimNumber(*d.Value))
		}
		fmt.Fprintf(&b, "penalty %.2f", *d.Penalty)
	}
	if d.Note != "" {
		b.WriteString(" (" + d.Note + ")")
	}
	if d.Source != "" {
		b.WriteString(" [" + d.Source + "]")
	}
	return b.String()
}

func effCounterText(label string, c *stats.ExplainCounter) string {
	if c == nil {
		return label + " not reported"
	}
	s := fmt.Sprintf("%s %d", label, c.Value)
	if c.LowerBound {
		s += " (lower bound)"
	}
	return s
}

func sampleEvidenceText(e string) string {
	switch e {
	case "execution_stats":
		return "with executionStats counts"
	case "planner_only":
		return "plan only, no counts"
	}
	return "no explain data"
}

func efficiencySampleText(s stats.EfficiencyViewSample) string {
	parts := []string{
		fmt.Sprintf("%s sample: %.2f ms in the run", s.Role, s.ClientLatencyMs),
		"explain " + s.ExplainStatus + reasonSuffix(s.ExplainReason) + ", " + sampleEvidenceText(s.Evidence),
		"scan efficiency " + s.ScoreLabel + reasonSuffix(s.Band),
	}
	if s.Evidence == "execution_stats" {
		parts = append(parts, strings.Join([]string{
			effCounterText("documents examined", s.DocsExamined), effCounterText("index keys examined", s.KeysExamined),
			effCounterText("documents returned", s.Returned),
		}, ", "))
		if s.AccessReturned != nil {
			parts = append(parts, effCounterText("documents read by the first pipeline stage", s.AccessReturned))
		}
		if s.Matched != nil {
			parts = append(parts, effCounterText("documents matched", s.Matched))
		}
		if s.ServerExecMs != nil {
			parts = append(parts, fmt.Sprintf("server time %d ms during explain (separate from run latency)", *s.ServerExecMs))
		}
	}
	if s.WinningPlan != "" {
		parts = append(parts, "plan "+s.WinningPlan)
	}
	if len(s.IndexesUsed) > 0 {
		parts = append(parts, "indexes used "+strings.Join(s.IndexesUsed, ", "))
	}
	return strings.Join(parts, "; ")
}

// efficiencyDataSource describes how the measurements were collected.
func efficiencyDataSource(r stats.EfficiencyViewRow) []string {
	var out []string
	if r.SelectedBy != "" {
		out = append(out, "Chosen for explain by: "+r.SelectedBy)
	}
	if sel := r.Selection; sel != nil {
		out = append(out, fmt.Sprintf("Sample commands picked by %s from %d kept operations (%d replayable; scope %s), median %.2f ms, p95 %.2f ms",
			sel.Method, sel.RetainedSuccessfulSamples, sel.ExecutableSamples, emptyFallback(sel.Scope, r.SampleScope), sel.MedianMs, sel.P95Ms))
	} else if r.SampleScope != "" {
		out = append(out, "Sample scope: "+r.SampleScope)
	}
	if r.DeterminingSample != "" {
		out = append(out, "Lowest-scoring sample: "+r.DeterminingSample)
	}
	if r.SamplesDisagree {
		out = append(out, "The sample commands fall in different bands")
	}
	if r.QueryVariants > 1 {
		out = append(out, fmt.Sprintf("%d redacted query variants were captured; one is shown", r.QueryVariants))
	}
	if r.EvidenceNote != "" {
		out = append(out, r.EvidenceNote)
	}
	return out
}

func reasonSuffix(s string) string {
	if s == "" {
		return ""
	}
	return " (" + s + ")"
}

func trimNumber(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	return strings.TrimRight(strings.TrimRight(s, "0"), ".")
}
