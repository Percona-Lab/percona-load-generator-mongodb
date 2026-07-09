package report

import (
	"strings"
	"testing"
	"time"
)

func sampleData() ReportData {
	return ReportData{
		Title:              "Test Run",
		GeneratedAt:        time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		DurationSeconds:    30,
		TotalOps:           12345,
		AvgOpsPerSec:       411.5,
		ConfigItems:        []KV{{Key: "URI", Value: "mongodb://localhost"}, {Key: "Concurrency", Value: "16"}},
		LoadProfileItems:   []KV{{Key: "Kind", Value: "ramp"}},
		PacingItems:        []KV{{Key: "Think Time", Value: "50ms"}},
		AccessPatternItems: []KV{{Key: "Access Pattern", Value: "zipfian"}},
		Latency: []LatencyRow{
			{Type: "find", Count: 1000, AvgMs: 1.2, MinMs: 0.3, MaxMs: 40, P95Ms: 5, P99Ms: 12},
		},
		Heatmap: []HeatmapPoint{
			{ElapsedSec: 1, Count: 100, P50: 1, P95: 4, P99: 10, Max: 20},
			{ElapsedSec: 2, Count: 120, P50: 2, P95: 8, P99: 30, Max: 60},
		},
		Insights: []string{"Throughput stable across the run."},
		Warnings: []string{"Example warning."},
	}
}

func TestRenderProducesSelfContainedHTML(t *testing.T) {
	html, err := RenderBytes(sampleData())
	if err != nil {
		t.Fatalf("RenderBytes error: %v", err)
	}
	s := string(html)

	// Self-contained: no external asset references.
	for _, bad := range []string{"http://", "https://", "<script"} {
		if strings.Contains(s, bad) {
			t.Fatalf("report should be self-contained, found %q", bad)
		}
	}

	wantSubstrings := []string{
		"Test Run",
		"2026-01-02 03:04:05 UTC",
		"12345", // total ops
		"mongodb://localhost",
		"ramp",    // load profile
		"zipfian", // access pattern
		"<svg",    // inline chart
		"Example warning.",
		"Throughput stable",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(s, want) {
			t.Fatalf("expected report to contain %q", want)
		}
	}
}

func TestRenderWithoutOptionalSections(t *testing.T) {
	d := ReportData{
		Title:           "Minimal",
		DurationSeconds: 5,
		TotalOps:        10,
		// No heatmap, no warnings/insights.
	}
	html, err := RenderBytes(d)
	if err != nil {
		t.Fatalf("RenderBytes error: %v", err)
	}
	s := string(html)
	if !strings.Contains(s, "No latency-over-time data") {
		t.Fatalf("expected empty-heatmap message")
	}
	if !strings.Contains(s, "Fixed concurrency.") {
		t.Fatalf("expected fixed-concurrency fallback for empty load profile")
	}
}

func TestLatencyChartEmpty(t *testing.T) {
	out := string(latencyChartSVG(nil))
	if !strings.Contains(out, "No latency-over-time data") {
		t.Fatalf("expected empty-state message, got %q", out)
	}
}

func TestLatencyChartRendersPolylines(t *testing.T) {
	out := string(latencyChartSVG([]HeatmapPoint{
		{ElapsedSec: 0, P50: 1, P95: 2, P99: 3, Max: 4},
		{ElapsedSec: 10, P50: 2, P95: 5, P99: 9, Max: 12},
	}))
	for _, cls := range []string{`class="p50"`, `class="p95"`, `class="p99"`} {
		if !strings.Contains(out, cls) {
			t.Fatalf("expected polyline %s in chart", cls)
		}
	}
}
