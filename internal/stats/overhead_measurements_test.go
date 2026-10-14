package stats

import (
	"reflect"
	"strings"
	"testing"
)

func sortStage(sorted, spills, storage int64, disk bool, ms int64) StageResourceMetric {
	p := "executionStats.executionStages"
	return StageResourceMetric{Path: p, Stage: "SORT", Sort: true, Engine: "classic", Namespace: "shop.orders",
		SortBytesProcessed: ctr(sorted, p+".totalDataSizeSorted"), UsedDisk: &disk,
		Spills: ctr(spills, p+".spills"), SpillStorageBytes: ctr(storage, p+".spilledDataStorageSize"),
		MemoryLimitBytes: ctr(104857600, p+".memLimit"), TimeMsEstimate: ctr(ms, p+".executionTimeMillisEstimate")}
}

func measuredSample(role string, status string, stages ...StageResourceMetric) ExplainSample {
	return ExplainSample{Role: role, Status: "explained", Diagnostics: &ExplainDiagnostics{
		ExecutionStatsAvailable: true, ServerVersion: "8.0.4", BlockingSortDetected: true,
		Metrics: &ExplainMetrics{Layout: "plan", SortSpill: &SortSpillMetrics{Status: status, Source: "explain", Stages: stages}},
	}}
}

func TestOverheadMeasurementsComparesSamplesWithoutAveraging(t *testing.T) {
	m := BuildExecutionOverhead("find", []ExplainSample{
		measuredSample("median", SortSpillMeasured, sortStage(1000, 0, 0, false, 1)),
		measuredSample("p95", SortSpillMeasured, sortStage(3000000, 12, 2048, true, 40)),
	}, "").Measurements
	if m.Status != SortSpillMeasured || len(m.Samples) != 2 || m.Samples[0].ServerVersion != "8.0.4" {
		t.Fatalf("measurements: %+v", m)
	}
	if !strings.Contains(m.Summary, "1000 bytes–2.9 MiB (differs by sample)") || !strings.Contains(m.Summary, "0–12 (differs by sample) spills") {
		t.Fatalf("summary must show the range, not an average: %q", m.Summary)
	}
	if strings.Contains(m.Summary, "1.4 MiB") || strings.Contains(m.Summary, "1500000") {
		t.Fatalf("summary averaged the samples: %q", m.Summary)
	}
	diff := strings.Join(m.Differences, "\n")
	for _, want := range []string{"usedDisk false vs true", "sort bytes processed 1000 bytes vs 2.9 MiB", "spills 0 vs 12",
		"spill file size 0 bytes vs 2.0 KiB", "stage time estimate (ms) 1 vs 40"} {
		if !strings.Contains(diff, want) {
			t.Fatalf("differences missing %q: %s", want, diff)
		}
	}
	line := m.Details[1]
	for _, want := range []string{"p95 sample (MongoDB 8.0.4, shop.orders)", "SORT (classic engine) at executionStats.executionStages",
		"sort processed 3,000,000 bytes (2.9 MiB)", "wrote to disk (usedDisk true)", "spills 12", "spill file size on disk 2,048 bytes (2.0 KiB)",
		"memory limit 104,857,600 bytes (100.0 MiB)", "stage time estimate 40 ms (includes input stages)", "not reported: spilled records, spilled data size"} {
		if !strings.Contains(line, want) {
			t.Fatalf("detail missing %q: %s", want, line)
		}
	}
	if strings.Contains(strings.ToLower(strings.Join(m.Details, " ")), "peak memory") {
		t.Fatalf("a memory limit must not be shown as peak memory: %v", m.Details)
	}
	same := BuildExecutionOverhead("find", []ExplainSample{
		measuredSample("median", SortSpillMeasured, sortStage(10, 0, 0, false, 0)),
		measuredSample("p95", SortSpillMeasured, sortStage(10, 0, 0, false, 0)),
	}, "").Measurements
	if len(same.Differences) != 1 || !strings.Contains(same.Differences[0], "reported the same") || strings.Contains(same.Summary, "differs") {
		t.Fatalf("equal samples: %+v", same)
	}
}

func TestOverheadMeasurementsSharedCommandIsNotCompared(t *testing.T) {
	b := measuredSample("p95", SortSpillMeasured, sortStage(99, 0, 0, false, 9))
	b.SharedCommand = true
	m := BuildExecutionOverhead("find", []ExplainSample{measuredSample("median", SortSpillMeasured, sortStage(10, 0, 0, false, 0)), b}, "").Measurements
	if len(m.Differences) != 0 || !strings.Contains(m.Details[1], "not independent evidence") {
		t.Fatalf("a shared command is not a second measurement: %+v", m)
	}
}

func TestOverheadMeasurementsStatusAndReasons(t *testing.T) {
	planner := ExplainSample{Role: "median", Status: "explained", Diagnostics: &ExplainDiagnostics{Metrics: &ExplainMetrics{Layout: "planner_only",
		SortSpill: &SortSpillMetrics{Status: SortSpillUnavailable, Reason: "planner_only_no_execution_stats"}}}}
	failed := ExplainSample{Role: "p95", Status: "timed_out", Reason: "explain_timeout_after_1000ms"}
	cases := []struct {
		name, op string
		samples  []ExplainSample
		status   string
		reason   string
		summary  string
	}{
		{"insert", "insertOne", nil, OverheadMeasurementsNotApplicable, "operation_not_explainable", ""},
		{"no samples", "find", nil, SortSpillUnavailable, "budget_excluded", "Sort/spill measurements unavailable"},
		{"planner only", "find", []ExplainSample{planner}, SortSpillUnavailable, "planner_only_no_execution_stats", "Sort/spill measurements unavailable"},
		{"timed out", "find", []ExplainSample{failed}, SortSpillUnavailable, "explain_timed_out: explain_timeout_after_1000ms", "Sort/spill measurements unavailable"},
		{"one sample failed", "find", []ExplainSample{measuredSample("median", SortSpillMeasured, sortStage(10, 0, 0, false, 0)), failed},
			SortSpillPartial, "some_samples_incomplete_or_unavailable", "Largest stage values: sort processed 10 bytes; 0 spills (partial)"},
		{"stage partial", "find", []ExplainSample{measuredSample("median", SortSpillPartial, StageResourceMetric{Path: "x", Stage: "SORT", Sort: true, Missing: []string{"sort_bytes_processed", "spill_status"}})},
			SortSpillPartial, "some_samples_incomplete_or_unavailable", "sort bytes not reported (partial)"},
		{"no sort", "find", []ExplainSample{measuredSample("median", SortSpillMeasured)}, SortSpillMeasured, "", "no sort or spill fields reported"},
	}
	for _, c := range cases {
		reason := ""
		if c.name == "no samples" {
			reason = "budget_excluded"
		}
		m := BuildExecutionOverhead(c.op, c.samples, reason).Measurements
		if m.Status != c.status || m.Reason != c.reason || m.Summary != c.summary {
			t.Fatalf("%s: got status %q reason %q summary %q", c.name, m.Status, m.Reason, m.Summary)
		}
		if c.status == SortSpillUnavailable && (len(m.Details) == 0 && len(m.Samples) > 0) {
			t.Fatalf("%s: unavailable samples need a reason line: %+v", c.name, m)
		}
	}
	m := BuildExecutionOverhead("find", []ExplainSample{failed}, "").Measurements
	if !strings.Contains(m.Details[0], "p95 sample: sort/spill measurements unavailable (explain_timed_out: explain_timeout_after_1000ms)") {
		t.Fatalf("unavailable detail: %v", m.Details)
	}
}

func TestOverheadMeasurementsZeroVersusMissingAndCaveat(t *testing.T) {
	missing := StageResourceMetric{Path: "p", Stage: "SORT", Sort: true, Engine: "classic"}
	txt := StageMeasurementText(missing)
	if !strings.Contains(txt, "not reported: sort bytes processed, usedDisk, spills, spilled records, spilled data size, spill file size on disk, stage time estimate") ||
		strings.Contains(txt, "spills 0") {
		t.Fatalf("absent fields must be named as not reported: %s", txt)
	}
	zero := sortStage(0, 0, 0, false, 0)
	if txt := StageMeasurementText(zero); !strings.Contains(txt, "sort processed 0 bytes") || !strings.Contains(txt, "spills 0") {
		t.Fatalf("reported zeros must be shown as zero: %s", txt)
	}
	sbe := sortStage(0, 0, 0, false, 0)
	sbe.Engine, sbe.Stage, sbe.ServerVersion, sbe.Caveats = "sbe", "sort", "8.0.4", []string{CaveatSBESortBytesWithoutSpill}
	txt = StageMeasurementText(sbe)
	if strings.Contains(txt, "sort processed 0") || strings.Contains(txt, "not reported: sort bytes") ||
		!strings.Contains(txt, "sort bytes processed: reported value unreliable (MongoDB 8.0.4 reported 0 in executionStats.executionStages.totalDataSizeSorted;") {
		t.Fatalf("an unreliable SBE zero keeps its raw value and source and is not shown as absent: %s", txt)
	}
	// The caveat covers only sort bytes; the stage's other reported zeros stay zeros.
	for _, want := range []string{"did not write to disk (usedDisk false)", "spills 0", "spill file size on disk 0 bytes", "stage time estimate 0 ms"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("other zeros of the same stage must stay measured (%q): %s", want, txt)
		}
	}
	m := BuildExecutionOverhead("find", []ExplainSample{measuredSample("median", SortSpillPartial, sbe)}, "").Measurements
	if m.Summary != "Largest stage values: sort bytes reported value unreliable; 0 spills (partial)" {
		t.Fatalf("summary: %q", m.Summary)
	}
	// Nonzero SBE sort bytes without a caveat are a measurement.
	nz := sortStage(708480, 26, 244204, true, 7)
	nz.Engine, nz.Stage = "sbe", "sort"
	if txt := StageMeasurementText(nz); !strings.Contains(txt, "sort processed 708,480 bytes") {
		t.Fatalf("nonzero SBE value: %s", txt)
	}
}

func TestOverheadMeasurementsDoNotChangeClassification(t *testing.T) {
	plain := overheadSample("median", ExplainDiagnostics{ExecutionStatsAvailable: true, BlockingSortDetected: true})
	with := measuredSample("median", SortSpillMeasured, sortStage(3000000, 12, 2048, true, 40))
	a, b := BuildExecutionOverhead("find", []ExplainSample{plain}, ""), BuildExecutionOverhead("find", []ExplainSample{with}, "")
	a.Measurements, b.Measurements = OverheadMeasurements{}, OverheadMeasurements{}
	if !reflect.DeepEqual(a, b) || a.Status != OverheadInMemorySort {
		t.Fatalf("measured values must not change the eo-1 status: %+v vs %+v", a, b)
	}
}
