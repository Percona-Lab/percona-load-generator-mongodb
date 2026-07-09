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

// ReportData is the complete, render-ready model for a run report.
type ReportData struct {
	Title           string
	GeneratedAt     time.Time
	DurationSeconds float64
	TotalOps        int64
	AvgOpsPerSec    float64

	ConfigItems        []KV
	LoadProfileItems   []KV
	PacingItems        []KV
	AccessPatternItems []KV

	Latency []LatencyRow
	Heatmap []HeatmapPoint

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
