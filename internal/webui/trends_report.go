package webui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/report"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
)

// reportTrendSection maps the cached trend view into the HTML report with the
// same labels, reasons, and annotations as /api/insights.
func reportTrendSection(v *stats.TrendView) *report.TrendSection {
	if v == nil {
		return nil
	}
	sec := &report.TrendSection{
		Version: v.Version, Status: v.Status, Reason: v.Reason, MethodNote: v.MethodNote,
		CausationNote: v.CausationNote, EvidenceNote: v.EvidenceNote,
		CriteriaSummary: v.Criteria.Summary, CriteriaNote: v.Criteria.Note, Limitations: v.Limitations,
	}
	st := v.Store
	sec.Store = []report.KV{
		{Key: "Baseline store", Value: st.Status + reasonSuffix(st.Reason)},
		{Key: "Persistent", Value: fmt.Sprint(st.Persistent)},
		{Key: "Stored shape baselines", Value: fmt.Sprintf("%d (max %d)", st.Baselines, st.MaxBaselines)},
		{Key: "Oldest / newest baseline", Value: emptyFallback(st.OldestRecordedAt, "none") + " / " + emptyFallback(st.NewestRecordedAt, "none")},
	}
	for _, metric := range []string{"comparability", "efficiency", "latency", "impact"} {
		sec.Counts = append(sec.Counts, report.KV{Key: "By " + metric, Value: trendCountsText(v.Counts[metric])})
	}
	for _, r := range v.Rows {
		sec.Rows = append(sec.Rows, report.TrendRow{
			ShapeID: r.ShapeID, Namespace: r.Namespace, Operation: r.Operation, Shape: r.ShapeSummary,
			Comparability: r.Comparability, BaselineAge: trendBaselineAgeText(r.Baseline),
			Efficiency: r.Efficiency.Label, Latency: r.Latency.Label, Impact: r.Impact.Label,
			Details: trendRowDetails(r),
		})
	}
	return sec
}

func trendCountsText(m map[string]int) string {
	if len(m) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s: %d", k, m[k]))
	}
	return strings.Join(parts, " | ")
}

func trendBaselineAgeText(b *stats.TrendBaselineInfo) string {
	if b == nil {
		return ""
	}
	var parts []string
	if b.EfficiencyAge != "" {
		parts = append(parts, "scan efficiency "+b.EfficiencyAge)
	}
	if b.WorkloadAge != "" {
		parts = append(parts, "latency/impact "+b.WorkloadAge)
	}
	return strings.Join(parts, ", ")
}

func trendEvidenceText(name string, s stats.TrendEvidenceSide) string {
	if s.OperationCount == nil && s.ExplainedCommands == nil {
		return name + ": no evidence"
	}
	var parts []string
	if s.OperationCount != nil {
		parts = append(parts, fmt.Sprintf("%d operations (full run)", *s.OperationCount))
	}
	if s.RetainedLatencySamples != nil {
		parts = append(parts, fmt.Sprintf("%d retained latency samples", *s.RetainedLatencySamples))
	}
	if s.ExplainedCommands != nil {
		parts = append(parts, fmt.Sprintf("%d distinct explained commands (%s, confidence %s, coverage %s)",
			*s.ExplainedCommands, emptyFallback(s.EfficiencyEvidence, "none"), emptyFallback(s.EfficiencyConfidence, "none"), emptyFallback(s.Coverage, "unknown")))
	}
	return name + ": " + strings.Join(parts, ", ")
}

func trendRowDetails(r stats.TrendRow) []string {
	out := make([]string, 0, 8)
	for _, reason := range r.ComparabilityReasons {
		out = append(out, "Comparability: "+reason)
	}
	for _, c := range []struct {
		name string
		cmp  stats.TrendComparison
	}{{"Scan efficiency", r.Efficiency}, {"Latency", r.Latency}, {"Impact", r.Impact}} {
		for _, reason := range c.cmp.Reasons {
			out = append(out, fmt.Sprintf("%s (%s): %s", c.name, c.cmp.State, reason))
		}
		if c.cmp.Secondary != "" {
			out = append(out, c.name+": "+c.cmp.Secondary)
		}
	}
	out = append(out, trendEvidenceText("Evidence, baseline", r.Evidence.Baseline), trendEvidenceText("Evidence, current", r.Evidence.Current))
	for _, a := range r.Interventions {
		out = append(out, fmt.Sprintf("Intervention (%s): %s", a.Kind, a.Detail))
	}
	for _, a := range r.WorkloadChanges {
		out = append(out, fmt.Sprintf("Workload change (%s): %s", a.Kind, a.Detail))
	}
	return out
}
