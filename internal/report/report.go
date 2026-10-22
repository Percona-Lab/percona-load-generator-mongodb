// Package report renders a self-contained, shareable HTML report for a completed
// benchmark run. The output embeds its own CSS and inline SVG charts (no external
// assets or JavaScript), so it can be saved and shared as a single file and
// printed to PDF directly from the browser.
//
// The package is deliberately decoupled from the live collector/sampler types:
// callers map their data into the plain ReportData structs below, which keeps
// rendering pure and trivially unit-testable.
package report

import (
	"bytes"
	"fmt"
	"html/template"
	"io"
	"time"
)

// KV is a labeled configuration value displayed in the report.
type KV struct {
	Key   string
	Value string
}

// LatencyRow is one operation type's latency summary (milliseconds).
type LatencyRow struct {
	Type  string
	Count int64
	AvgMs float64
	MinMs float64
	MaxMs float64
	P95Ms float64
	P99Ms float64
}

// HeatmapPoint is one time-window of latency percentiles.
type HeatmapPoint struct {
	ElapsedSec float64
	Count      int64
	P50        float64
	P95        float64
	P99        float64
	Max        float64
}

// ImpactRow is one query shape ranked by summed concurrent operation time.
type ImpactRow struct {
	Rank           int
	Namespace      string
	Operation      string
	Shape          string
	OperationCount uint64
	TotalMs        float64
	SharePct       float64
	AvgMs          float64
	SampleCount    int
	SampleP95      string
	Explanation    string
}

// ImpactSection is the workload-impact view: full-run totals per shape plus
// the coverage notes needed to read them correctly.
type ImpactSection struct {
	Status             string
	TimeBasisNote      string
	InterpretationNote string
	ObservedOps        uint64
	ObservedMs         float64
	Rows               []ImpactRow
	Limitations        []string
}

// RecommendationRow is one post-run recommendation, in insights rank order.
type RecommendationRow struct {
	Rank       int
	Priority   string
	Title      string
	Details    string
	Confidence string
	Source     string
}

// EfficiencyRecommendation is a recommendation linked to an efficiency row.
type EfficiencyRecommendation struct {
	Rank           int
	Title          string
	CandidateKeys  []string
	Rationale      string
	Tradeoffs      string
	ValidationStep string
	Limitations    []string
}

// EfficiencyRow is one shape of the efficiency view, in the view's order.
type EfficiencyRow struct {
	Order            int
	ShapeID          string
	Namespace        string
	Operation        string
	Shape            string
	Query            string
	ScoreLabel       string
	Band             string
	Confidence       string
	Coverage         string
	CoverageReason   string
	Latency          string
	LatencyRating    string
	LatencyTier      string
	Severity         string
	Impact           string
	Overhead         string
	OverheadEvidence []string
	// OverheadSummary, OverheadMeasurements, and OverheadNotes are the
	// measured sort/spill values as shown in the dashboard.
	OverheadSummary           string
	OverheadMeasurementStatus string
	OverheadMeasurements      []string
	OverheadNotes             []string
	Lookup                    string
	Locks                     string
	Network                   string
	OtherDetails              []string
	RatingExplanation         string
	Components                []string
	Observations              []string
	Limitations               []string
	DataSource                []string
	Samples                   []string
	PlanSummary               string
	Recommendations           []EfficiencyRecommendation
}

// EfficiencySection is the query assessment view: scan efficiency, latency,
// workload impact, execution overhead, and other measurements, each with its
// column explanation.
type EfficiencySection struct {
	Status         string
	Reason         string
	Notice         string
	RatingVersions string
	LatencyTargets string
	Columns        []KV
	SortNote       string
	Coverage       []KV
	Unrecorded     []string
	Limitations    []string
	Rows           []EfficiencyRow
}

// TrendRow is one shape's comparison with its persisted baseline.
type TrendRow struct {
	ShapeID       string
	Namespace     string
	Operation     string
	Shape         string
	Comparability string
	BaselineAge   string
	Efficiency    string
	Latency       string
	Impact        string
	Details       []string
	// QueryValues is the query values annotation; empty unless this run or
	// its baseline run stored literal values. QueryValueDetails hold them.
	QueryValues       string
	QueryValueDetails []string
	// QueryValuesFromTraffic is set when QueryValues comes from equal
	// deterministic traffic fingerprints rather than stored literals.
	QueryValuesFromTraffic bool
	// QueryValuesFromRecording is set when both runs replayed the same
	// recording.
	QueryValuesFromRecording bool
}

// TrendSection is the baseline comparison with its criteria and disclosures.
type TrendSection struct {
	QueryValuesNotice string
	Version           string
	Status            string
	Reason            string
	MethodNote        string
	CausationNote     string
	EvidenceNote      string
	CriteriaSummary   string
	CriteriaNote      string
	Store             []KV
	Counts            []KV
	Limitations       []string
	Rows              []TrendRow
}

// ReportData is the complete, render-ready model for a run report.
type ReportData struct {
	Title           string
	GeneratedAt     time.Time
	DurationSeconds float64
	TotalOps        int64
	AvgOpsPerSec    float64
	// DurationLabel, OpsLabel, and ThroughputLabel replace the numeric cards
	// when a stored run does not have that measurement. Empty means show the numbers.
	DurationLabel   string
	OpsLabel        string
	ThroughputLabel string

	ConfigItems        []KV
	LoadProfileItems   []KV
	PacingItems        []KV
	AccessPatternItems []KV
	// GeneratedTrafficItems is the traffic fingerprint of a deterministic
	// run (hashes only); empty for any other run.
	GeneratedTrafficItems []KV
	// MultiTargetItems lists the targets of a multi-target replay (labels,
	// status, client-side lag); MultiTargetNotes says how they shared the
	// PLGM host. Empty for a single-target run.
	MultiTargetItems []KV
	MultiTargetNotes []string

	Latency []LatencyRow
	Heatmap []HeatmapPoint

	Impact          *ImpactSection
	Efficiency      *EfficiencySection
	Trends          *TrendSection
	Recommendations []RecommendationRow

	Insights []string
	Warnings []string
}

// RenderBytes renders the report to a byte slice.
func RenderBytes(d ReportData) ([]byte, error) {
	var buf bytes.Buffer
	if err := Render(&buf, d); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Render writes the HTML report to w.
func Render(w io.Writer, d ReportData) error {
	if d.Title == "" {
		d.Title = "MongoDB Benchmark Report"
	}
	if d.GeneratedAt.IsZero() {
		d.GeneratedAt = time.Now()
	}
	tmpl, err := template.New("report").Funcs(template.FuncMap{
		"latencyChart": latencyChartSVG,
		"fmtFloat":     func(f float64) string { return fmt.Sprintf("%.2f", f) },
		"fmtTime":      func(t time.Time) string { return t.Format("2006-01-02 15:04:05 MST") },
	}).Parse(reportTemplate)
	if err != nil {
		return err
	}
	return tmpl.Execute(w, d)
}

// latencyChartSVG builds a self-contained inline SVG line chart of p50/p95/p99
// latency over time. It returns an empty fragment when there is no data.
func latencyChartSVG(points []HeatmapPoint) template.HTML {
	if len(points) == 0 {
		return template.HTML(`<p class="muted">No latency-over-time data was captured for this run.</p>`)
	}

	const (
		width  = 920.0
		height = 280.0
		padL   = 50.0
		padR   = 20.0
		padT   = 20.0
		padB   = 40.0
		plotW  = width - padL - padR
		plotH  = height - padT - padB
	)

	maxLat := 0.0
	maxElapsed := 0.0
	for _, p := range points {
		if p.Max > maxLat {
			maxLat = p.Max
		}
		if p.P99 > maxLat {
			maxLat = p.P99
		}
		if p.ElapsedSec > maxElapsed {
			maxElapsed = p.ElapsedSec
		}
	}
	if maxLat <= 0 {
		maxLat = 1
	}
	if maxElapsed <= 0 {
		maxElapsed = 1
	}

	x := func(elapsed float64) float64 { return padL + (elapsed/maxElapsed)*plotW }
	y := func(lat float64) float64 { return padT + plotH - (lat/maxLat)*plotH }

	poly := func(sel func(HeatmapPoint) float64) string {
		var b bytes.Buffer
		for i, p := range points {
			if i > 0 {
				b.WriteByte(' ')
			}
			fmt.Fprintf(&b, "%.1f,%.1f", x(p.ElapsedSec), y(sel(p)))
		}
		return b.String()
	}

	var b bytes.Buffer
	fmt.Fprintf(&b, `<svg viewBox="0 0 %.0f %.0f" class="chart" role="img" aria-label="Latency over time">`, width, height)
	// Axes.
	fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="axis"/>`, padL, padT, padL, padT+plotH)
	fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="axis"/>`, padL, padT+plotH, padL+plotW, padT+plotH)
	// Y gridlines / labels (0, 50%, 100% of maxLat).
	for _, frac := range []float64{0, 0.5, 1} {
		yy := padT + plotH - frac*plotH
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="grid"/>`, padL, yy, padL+plotW, yy)
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" class="tick">%.0f</text>`, padL-8, yy+4, frac*maxLat)
	}
	// Lines.
	fmt.Fprintf(&b, `<polyline points="%s" class="p50"/>`, poly(func(p HeatmapPoint) float64 { return p.P50 }))
	fmt.Fprintf(&b, `<polyline points="%s" class="p95"/>`, poly(func(p HeatmapPoint) float64 { return p.P95 }))
	fmt.Fprintf(&b, `<polyline points="%s" class="p99"/>`, poly(func(p HeatmapPoint) float64 { return p.P99 }))
	// X axis labels (start and end elapsed seconds).
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" class="tick">0s</text>`, padL, padT+plotH+20)
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" class="tick" text-anchor="end">%.0fs</text>`, padL+plotW, padT+plotH+20, maxElapsed)
	b.WriteString(`</svg>`)
	b.WriteString(`<div class="legend"><span class="k p50">p50</span><span class="k p95">p95</span><span class="k p99">p99</span></div>`)

	return template.HTML(b.String())
}
