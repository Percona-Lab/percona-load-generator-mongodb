package stats

import (
	"fmt"
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
)

func TestSnapshotExplainInventoryCoversEveryTrackedShapeRankedByImpact(t *testing.T) {
	c := newCaptureCollector(t, 200)
	const shapes = captureViewLimit + 20
	for s := 0; s < shapes; s++ {
		key := fmt.Sprintf("find|{s%03d:<str>}", s)
		for i := 0; i < 3; i++ {
			recordFind(c, "orders", key, time.Duration(s+1)*time.Microsecond*10, true, &ExecutableQuery{Database: "shop", Filter: map[string]interface{}{"v": i}})
		}
	}
	rep := c.GetFinalInsights()
	if len(rep.QueryCapture.Shapes) != captureViewLimit || len(rep.Impact.Groups) >= shapes {
		t.Fatalf("test needs truncated views: capture=%d impact=%d", len(rep.QueryCapture.Shapes), len(rep.Impact.Groups))
	}

	inv := c.SnapshotExplainInventory()
	if len(inv.Shapes) != shapes || inv.CaptureStatus != "ready" || inv.GroupLimit != 200 {
		t.Fatalf("inventory must hold every tracked shape: got %d, status %q", len(inv.Shapes), inv.CaptureStatus)
	}
	for i, s := range inv.Shapes {
		if s.ImpactRank != i+1 || len(s.Samples) != 3 || s.SampleScope != SampleScopeAllOperations {
			t.Fatalf("shape %d: unexpected %+v", i, s)
		}
		if i > 0 && s.TotalOperationMs > inv.Shapes[i-1].TotalOperationMs {
			t.Fatalf("inventory not ranked by summed time at %d", i)
		}
	}
	if inv.Shapes[0].ShapeKey != fmt.Sprintf("find|{s%03d:<str>}", shapes-1) {
		t.Fatalf("top-impact shape should be the slowest one, got %q", inv.Shapes[0].ShapeKey)
	}
}

func TestSnapshotExplainInventoryScopesAndUnavailableReasons(t *testing.T) {
	c := newCaptureCollector(t, 10)
	for i := 0; i < 50; i++ {
		recordFind(c, "orders", "find|many", time.Millisecond, true, &ExecutableQuery{Database: "shop", Filter: map[string]interface{}{"n": i}})
	}
	recordFind(c, "orders", "find|failed", time.Millisecond, false, &ExecutableQuery{Database: "shop"})
	recordFind(c, "orders", "find|noquery", time.Millisecond, true, nil)
	big := make([]interface{}, captureMaxSampleBytes)
	recordFind(c, "orders", "find|big", time.Millisecond, true, &ExecutableQuery{Database: "shop", Filter: map[string]interface{}{"$in": big}})

	inv := c.SnapshotExplainInventory()
	byKey := map[string]ExplainInventoryShape{}
	for _, s := range inv.Shapes {
		byKey[s.ShapeKey] = s
	}
	if s := byKey["find|many"]; s.SampleScope != SampleScopeUniformReservoir || len(s.Samples) < captureSamplesPerShape || s.EligibleOperations != 50 {
		t.Fatalf("expected a full-run reservoir: %+v", s)
	}
	for key, want := range map[string]string{"find|failed": "no_successful_operations", "find|noquery": "no_executable_query", "find|big": "command_exceeds_sample_byte_limit"} {
		if s := byKey[key]; len(s.Samples) != 0 || s.UnavailableReason != want {
			t.Fatalf("%s: reason=%q want %q", key, s.UnavailableReason, want)
		}
	}

	inv.Shapes[0].Samples[0].Query.Filter["n"] = "mutated"
	if again := c.SnapshotExplainInventory(); again.Shapes[0].Samples[0].Query.Filter["n"] == "mutated" {
		t.Fatalf("inventory must return deep copies")
	}
}

func TestSnapshotExplainInventoryDisabledAndCoverageDefaults(t *testing.T) {
	off := NewCollector()
	off.ConfigureInsights(&config.AppConfig{InsightsEnabled: true, InsightsSamplingRate: 1, InsightsMaxEvents: 10, InsightsMaxGroups: 10})
	recordFind(off, "orders", "find|x", time.Millisecond, true, &ExecutableQuery{Database: "shop"})
	inv := off.SnapshotExplainInventory()
	if inv.CaptureStatus != "disabled" || inv.CaptureDisabledReason != "explain_disabled" || len(inv.Shapes) != 1 || len(inv.Shapes[0].Samples) != 0 {
		t.Fatalf("unexpected disabled inventory: %+v", inv)
	}
	rep := off.GetFinalInsights()
	if rep.ExplainCoverage.Status != "disabled" || rep.ExplainCoverage.Reason != "explain_disabled" || len(rep.ExplainCoverage.UnrecordedPaths) != 2 {
		t.Fatalf("explain-disabled coverage must disclose its state: %+v", rep.ExplainCoverage)
	}

	on := newCaptureCollector(t, 10)
	recordFind(on, "orders", "find|x", time.Millisecond, true, &ExecutableQuery{Database: "shop"})
	if rep := on.GetFinalInsights(); rep.ExplainCoverage.Status != "pending" || rep.Metadata.ExplainEfficiencyMode != config.ExplainEfficiencyModeDefault {
		t.Fatalf("explain-enabled coverage before enrichment: %+v mode=%q", rep.ExplainCoverage, rep.Metadata.ExplainEfficiencyMode)
	}
}

func TestExplainLatencyTier(t *testing.T) {
	for ms, want := range map[float64]string{0.2: "lt_1ms", 1: "1_10ms", 99.9: "10_100ms", 100: "100ms_1s", 5000: "ge_1s"} {
		if _, got := ExplainLatencyTier(ms); got != want {
			t.Fatalf("tier(%v)=%q want %q", ms, got, want)
		}
	}
}

func TestCoverageStatusFromSamples(t *testing.T) {
	stats := &ExplainDiagnostics{ExecutionStatsAvailable: true}
	planner := &ExplainDiagnostics{Verbosity: "queryPlanner"}
	cases := []struct {
		name                 string
		samples              []ExplainSample
		status, reason, evid string
		partial              string
	}{
		{"explained", []ExplainSample{{Status: "explained", Diagnostics: stats}, {Status: "timed_out", Reason: "max_time_exceeded"}}, "explained", "", "execution_stats", ""},
		{"planner only", []ExplainSample{{Status: "explained", Diagnostics: planner}}, "explained", "", "planner_only", "verbosity_queryPlanner"},
		{"failed", []ExplainSample{{Status: "cancelled"}, {Status: "execution_failed", Reason: "not_authorized"}}, "failed", "execution_failed:not_authorized", "", ""},
		{"cancelled", []ExplainSample{{Status: "cancelled"}}, "cancelled", "explain_cancelled", "", ""},
		{"unavailable", []ExplainSample{{Status: "insufficient_metadata", Reason: "sort_key_order_undefined"}}, "unavailable", "sort_key_order_undefined", "", ""},
	}
	for _, tc := range cases {
		st, r, e, p := coverageStatusFromSamples(tc.samples)
		if st != tc.status || r != tc.reason || e != tc.evid || p != tc.partial {
			t.Fatalf("%s: got (%q,%q,%q,%q)", tc.name, st, r, e, p)
		}
	}
}
