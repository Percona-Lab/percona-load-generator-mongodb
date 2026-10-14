package stats

import (
	"fmt"
	"strings"
)

// Execution-overhead measurement statuses (in addition to the SortSpill ones).
const OverheadMeasurementsNotApplicable = "not_applicable"

// overheadMeasurementNotes say what each reported field means. They are part
// of every export so no value is read as something it is not.
var overheadMeasurementNotes = []string{
	"Measured by re-running each representative command with explain executionStats after the workload, not during the original operations; values describe that explain run only.",
	"Sort bytes processed (totalDataSizeSorted, or totalDataSizeSortedBytesEstimate for a $sort pipeline stage) is the amount of data the sort handled, not its peak memory.",
	"Memory limit (memLimit) is the stage's in-memory limit before it must spill, not memory used. Peak memory is shown only when a stage reports it (maxMemoryUsageBytes, memUsage, or maxAccumulatorMemoryUsageBytes per accumulator).",
	"Spilled data size (spilledBytes, or numBytesSpilledEstimate for classic $group) is the server's size of the records it wrote to temporary files; spill file size (spilledDataStorageSize) is the storage those files used. Neither is peak memory, and they are not interchangeable.",
	"On MongoDB 7.0 and 8.0 the slot-based sort counts totalDataSizeSorted only for data it spilled, so a 0 from a sort that did not spill and returned rows is shown as \"reported value unreliable\", not as no sort work. \"Not reported\" means the field was absent. Every other reported 0 is shown as 0. After a spill the field is shown as reported; by the server source it leaves out the last in-memory run (not measured).",
	"Each representative sample is shown separately; values are never averaged across samples.",
	"Stage time (executionTimeMillisEstimate) is the server's estimate for the stage including its input stages; it is not exclusive sort time and is never summed across stages.",
	"CPU time, lock waits, and network time are not measured.",
}

// OverheadMeasurements are the sort, spill, memory, and timing values the
// explained samples reported, kept per sample and per stage. Samples are
// compared, never averaged. Details are the display lines shared by the
// dashboard, Download Summary, and HTML report.
type OverheadMeasurements struct {
	Status      string                      `json:"status"`
	Reason      string                      `json:"reason,omitempty"`
	Source      string                      `json:"source"`
	Summary     string                      `json:"summary"`
	Samples     []OverheadSampleMeasurement `json:"samples,omitempty"`
	Differences []string                    `json:"differences,omitempty"`
	Details     []string                    `json:"details,omitempty"`
	Notes       []string                    `json:"notes,omitempty"`
}

// OverheadSampleMeasurement is one representative sample's measurements.
type OverheadSampleMeasurement struct {
	Role          string                `json:"role"`
	ServerVersion string                `json:"server_version,omitempty"`
	ExplainStatus string                `json:"explain_status"`
	Status        string                `json:"status"`
	Reason        string                `json:"reason,omitempty"`
	SharedCommand bool                  `json:"shared_command,omitempty"`
	Stages        []StageResourceMetric `json:"stages,omitempty"`
}

func (s OverheadSampleMeasurement) hasValues() bool {
	return s.Status == SortSpillMeasured || s.Status == SortSpillPartial
}

func buildOverheadMeasurements(op string, samples []ExplainSample, reason string) OverheadMeasurements {
	m := OverheadMeasurements{Source: "explain executionStats (post-run)"}
	if isInsertOp(op) {
		m.Status, m.Reason = OverheadMeasurementsNotApplicable, "operation_not_explainable"
		return m
	}
	m.Notes = append([]string{}, overheadMeasurementNotes...)
	measured, withValues := 0, 0
	for _, s := range samples {
		sm := sampleMeasurement(s)
		if sm.Status == SortSpillMeasured {
			measured++
		}
		if sm.hasValues() {
			withValues++
		}
		m.Samples = append(m.Samples, sm)
	}
	switch {
	case len(m.Samples) == 0:
		m.Status, m.Reason = SortSpillUnavailable, emptyFallback(reason, "explain_unavailable")
	case measured == len(m.Samples):
		m.Status = SortSpillMeasured
	case withValues > 0:
		m.Status, m.Reason = SortSpillPartial, "some_samples_incomplete_or_unavailable"
	default:
		m.Status, m.Reason = SortSpillUnavailable, m.Samples[0].Reason
	}
	for _, sm := range m.Samples {
		m.Details = append(m.Details, sampleDetailLines(sm)...)
	}
	m.Differences = sampleDifferences(m.Samples)
	m.Details = append(m.Details, m.Differences...)
	m.Summary = measurementSummary(m)
	return m
}

func sampleMeasurement(s ExplainSample) OverheadSampleMeasurement {
	sm := OverheadSampleMeasurement{Role: s.Role, ExplainStatus: s.Status, SharedCommand: s.SharedCommand}
	d := s.Diagnostics
	if d != nil {
		sm.ServerVersion = d.ServerVersion
	}
	switch {
	case s.Status != "explained" || d == nil:
		sm.Status, sm.Reason = SortSpillUnavailable, "explain_"+emptyFallback(s.Status, "missing")
		if s.Reason != "" {
			sm.Reason += ": " + s.Reason
		}
	case d.Metrics == nil || d.Metrics.SortSpill == nil:
		sm.Status, sm.Reason = SortSpillUnavailable, "sort_spill_fields_not_recorded"
		if !d.ExecutionStatsAvailable {
			sm.Reason = "planner_only_no_execution_stats"
		}
	default:
		ss := d.Metrics.SortSpill
		sm.Status, sm.Reason, sm.Stages = ss.Status, ss.Reason, ss.Stages
	}
	return sm
}

func sampleLabel(sm OverheadSampleMeasurement) string {
	ctx := []string{}
	if sm.ServerVersion != "" {
		ctx = append(ctx, "MongoDB "+sm.ServerVersion)
	}
	if len(sm.Stages) > 0 && sm.Stages[0].Namespace != "" {
		ctx = append(ctx, sm.Stages[0].Namespace)
	}
	label := sm.Role + " sample"
	if len(ctx) > 0 {
		label += " (" + strings.Join(ctx, ", ") + ")"
	}
	return label
}

func sampleDetailLines(sm OverheadSampleMeasurement) []string {
	label := sampleLabel(sm)
	if sm.SharedCommand {
		label += " [same explain command as an earlier sample; not independent evidence]"
	}
	switch {
	case !sm.hasValues():
		return []string{fmt.Sprintf("%s: sort/spill measurements unavailable (%s)", label, emptyFallback(sm.Reason, "unknown"))}
	case len(sm.Stages) == 0 && sm.Status == SortSpillMeasured:
		return []string{label + ": the executed plan reported no sort stage and no spill or memory fields"}
	}
	var out []string
	if sm.Status == SortSpillPartial {
		out = append(out, fmt.Sprintf("%s: partial measurements (%s)", label, sm.Reason))
	}
	for _, st := range sm.Stages {
		out = append(out, fmt.Sprintf("%s: %s", label, StageMeasurementText(st)))
	}
	return out
}

// StageMeasurementText describes one stage's reported fields with units and
// names the fields that were not reported.
func StageMeasurementText(st StageResourceMetric) string {
	where := st.Stage
	switch st.Engine {
	case "sbe":
		where += " (slot-based engine)"
	case "classic":
		where += " (classic engine)"
	case "pipeline":
		where += " (aggregation pipeline stage)"
	}
	if st.Shard != "" {
		where += " on shard " + st.Shard
	}
	where += " at " + st.Path
	var parts, missing []string
	if st.Sort {
		switch {
		case st.SortBytesUsable():
			parts = append(parts, "sort processed "+bytesText(st.SortBytesProcessed.Value))
		case st.SortBytesProcessed != nil:
			server := "the server"
			if st.ServerVersion != "" {
				server = "MongoDB " + st.ServerVersion
			}
			parts = append(parts, fmt.Sprintf("sort bytes processed: reported value unreliable (%s reported %d in %s; this slot-based sort counts bytes only when it spills)",
				server, st.SortBytesProcessed.Value, st.SortBytesProcessed.Source))
		default:
			missing = append(missing, "sort bytes processed")
		}
	}
	switch {
	case st.UsedDisk != nil && *st.UsedDisk:
		parts = append(parts, "wrote to disk (usedDisk true)")
	case st.UsedDisk != nil:
		parts = append(parts, "did not write to disk (usedDisk false)")
	default:
		missing = append(missing, "usedDisk")
	}
	countPart := func(c *ExplainCounter, what string, bytes bool) {
		switch {
		case c == nil:
			missing = append(missing, what)
		case bytes:
			parts = append(parts, what+" "+bytesText(c.Value))
		default:
			parts = append(parts, fmt.Sprintf("%s %s", what, groupDigits(c.Value)))
		}
	}
	countPart(st.Spills, "spills", false)
	countPart(st.SpilledRecords, "spilled records", false)
	countPart(st.SpilledBytes, "spilled data size", true)
	countPart(st.SpillStorageBytes, "spill file size on disk", true)
	if st.MemoryLimitBytes != nil {
		parts = append(parts, "memory limit "+bytesText(st.MemoryLimitBytes.Value))
	}
	for _, pm := range st.PeakMemory {
		parts = append(parts, fmt.Sprintf("peak memory (%s) %s", pm.Scope, bytesText(pm.Value)))
	}
	if st.TimeMsEstimate != nil {
		parts = append(parts, fmt.Sprintf("stage time estimate %d ms (includes input stages)", st.TimeMsEstimate.Value))
	} else {
		missing = append(missing, "stage time estimate")
	}
	s := where + ": " + strings.Join(parts, "; ")
	if len(missing) > 0 {
		s += "; not reported: " + strings.Join(missing, ", ")
	}
	return s
}

// stageValues are the comparable values of one stage, in display order.
func stageValues(st StageResourceMetric) []struct {
	name  string
	c     *ExplainCounter
	bytes bool
} {
	return []struct {
		name  string
		c     *ExplainCounter
		bytes bool
	}{
		{"sort bytes processed", st.SortBytesProcessed, true},
		{"spills", st.Spills, false},
		{"spilled records", st.SpilledRecords, false},
		{"spilled data size", st.SpilledBytes, true},
		{"spill file size", st.SpillStorageBytes, true},
		{"memory limit", st.MemoryLimitBytes, true},
		{"stage time estimate (ms)", st.TimeMsEstimate, false},
	}
}

func counterText(c *ExplainCounter, bytes bool) string {
	switch {
	case c == nil:
		return "not reported"
	case bytes:
		return compactBytes(c.Value)
	}
	return groupDigits(c.Value)
}

// sampleDifferences compares each independent sample with the first one,
// stage by stage (matched by path), and lists every differing value.
func sampleDifferences(samples []OverheadSampleMeasurement) []string {
	var ref *OverheadSampleMeasurement
	var out []string
	for i := range samples {
		s := &samples[i]
		if !s.hasValues() {
			continue
		}
		if ref == nil {
			ref = s
			continue
		}
		if s.SharedCommand {
			continue
		}
		prefix := fmt.Sprintf("Samples differ (%s vs %s): ", ref.Role, s.Role)
		before := len(out)
		byPath := map[string]StageResourceMetric{}
		for _, st := range s.Stages {
			byPath[st.Path] = st
		}
		seen := map[string]bool{}
		for _, a := range ref.Stages {
			b, ok := byPath[a.Path]
			if !ok {
				out = append(out, fmt.Sprintf("%s%s at %s reported only by the %s sample", prefix, a.Stage, a.Path, ref.Role))
				continue
			}
			seen[a.Path] = true
			if (a.UsedDisk == nil) != (b.UsedDisk == nil) || (a.UsedDisk != nil && *a.UsedDisk != *b.UsedDisk) {
				out = append(out, fmt.Sprintf("%s%s usedDisk %s vs %s", prefix, a.Stage, boolText(a.UsedDisk), boolText(b.UsedDisk)))
			}
			av, bv := stageValues(a), stageValues(b)
			for k := range av {
				ac, bc := av[k].c, bv[k].c
				if (ac == nil) == (bc == nil) && (ac == nil || ac.Value == bc.Value) {
					continue
				}
				out = append(out, fmt.Sprintf("%s%s %s %s vs %s", prefix, a.Stage, av[k].name, counterText(ac, av[k].bytes), counterText(bc, av[k].bytes)))
			}
		}
		for _, b := range s.Stages {
			if !seen[b.Path] {
				out = append(out, fmt.Sprintf("%s%s at %s reported only by the %s sample", prefix, b.Stage, b.Path, s.Role))
			}
		}
		if len(out) == before {
			out = append(out, fmt.Sprintf("The %s and %s samples reported the same sort/spill values.", ref.Role, s.Role))
		}
	}
	return out
}

func boolText(b *bool) string {
	if b == nil {
		return "not reported"
	}
	return fmt.Sprintf("%t", *b)
}

// measurementSummary is the short column text. Per-sample values are shown as
// a range when the samples differ; they are never averaged.
func measurementSummary(m OverheadMeasurements) string {
	if m.Status == SortSpillUnavailable {
		return "Sort/spill measurements unavailable"
	}
	var sortBytes, spills, storage []int64
	anySort, anyStage, unreliableSort := false, false, false
	for _, s := range m.Samples {
		if !s.hasValues() {
			continue
		}
		var sb, sp, sz *int64
		for _, st := range s.Stages {
			anyStage = true
			if st.Sort {
				anySort = true
			}
			if st.SortBytesUsable() {
				sb = maxCounter(sb, st.SortBytesProcessed)
			} else if st.SortBytesProcessed != nil {
				unreliableSort = true
			}
			sp = maxCounter(sp, st.Spills)
			sz = maxCounter(sz, st.SpillStorageBytes)
		}
		if sb != nil {
			sortBytes = append(sortBytes, *sb)
		}
		if sp != nil {
			spills = append(spills, *sp)
		}
		if sz != nil {
			storage = append(storage, *sz)
		}
	}
	var parts []string
	if len(sortBytes) > 0 {
		parts = append(parts, "sort processed "+rangeText(sortBytes, compactBytes))
	} else if unreliableSort {
		parts = append(parts, "sort bytes reported value unreliable")
	} else if anySort {
		parts = append(parts, "sort bytes not reported")
	}
	switch {
	case len(spills) > 0 && maxOf(spills) > 0:
		p := rangeText(spills, groupDigits) + " spills"
		if len(storage) > 0 {
			p += ", " + rangeText(storage, compactBytes) + " spill files"
		}
		parts = append(parts, p)
	case len(spills) > 0:
		parts = append(parts, "0 spills")
	}
	if !anyStage {
		parts = append(parts, "no sort or spill fields reported")
	}
	s := strings.Join(parts, "; ")
	if len(sortBytes) > 0 || len(spills) > 0 {
		s = "Largest stage values: " + s
	}
	if m.Status == SortSpillPartial {
		s += " (partial)"
	}
	return s
}

func maxCounter(cur *int64, c *ExplainCounter) *int64 {
	if c == nil || (cur != nil && *cur >= c.Value) {
		return cur
	}
	v := c.Value
	return &v
}

func maxOf(v []int64) int64 {
	m := v[0]
	for _, x := range v[1:] {
		if x > m {
			m = x
		}
	}
	return m
}

func rangeText(v []int64, f func(int64) string) string {
	lo, hi := v[0], v[0]
	for _, x := range v[1:] {
		if x < lo {
			lo = x
		}
		if x > hi {
			hi = x
		}
	}
	if lo == hi {
		return f(lo)
	}
	return f(lo) + "–" + f(hi) + " (differs by sample)"
}

// bytesText is the exact byte count plus a binary-unit approximation.
func bytesText(v int64) string {
	if v < 1024 && v > -1024 {
		return fmt.Sprintf("%s bytes", groupDigits(v))
	}
	return fmt.Sprintf("%s bytes (%s)", groupDigits(v), compactBytes(v))
}

func compactBytes(v int64) string {
	f := float64(v)
	for _, u := range []string{"bytes", "KiB", "MiB", "GiB"} {
		if f < 1024 && f > -1024 || u == "GiB" {
			if u == "bytes" {
				return fmt.Sprintf("%d bytes", v)
			}
			return fmt.Sprintf("%.1f %s", f, u)
		}
		f /= 1024
	}
	return fmt.Sprintf("%d bytes", v)
}

func groupDigits(v int64) string {
	s := fmt.Sprintf("%d", v)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}
