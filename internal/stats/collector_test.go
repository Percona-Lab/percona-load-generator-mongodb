package stats

import (
	"encoding/csv"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestConcurrencySnapshot(t *testing.T) {
	c := NewCollector()
	if target, active := c.ConcurrencySnapshot(); target != 0 || active != 0 {
		t.Fatalf("expected zero initial concurrency, got target=%d active=%d", target, active)
	}
	c.SetConcurrency(50, 37)
	if target, active := c.ConcurrencySnapshot(); target != 50 || active != 37 {
		t.Fatalf("expected target=50 active=37, got target=%d active=%d", target, active)
	}
	c.SetConcurrency(10, 10)
	if target, active := c.ConcurrencySnapshot(); target != 10 || active != 10 {
		t.Fatalf("expected target=10 active=10, got target=%d active=%d", target, active)
	}
}

func TestLatencyHistogramRecordAndStats(t *testing.T) {
	h := &LatencyHistogram{Min: math.MaxFloat64}
	h.Record(10.2)
	h.Record(20.7)
	h.RecordBatch(5.0, 2)

	if h.Count != 4 {
		t.Fatalf("expected count 4, got %d", h.Count)
	}
	avg := h.GetAverage()
	if avg <= 0 {
		t.Fatalf("expected positive average, got %f", avg)
	}

	stats := h.GetStats()
	if stats["count"] != 4 {
		t.Fatalf("expected stats count 4, got %f", stats["count"])
	}
	if stats["min"] != 5 {
		t.Fatalf("expected min 5, got %f", stats["min"])
	}
	if stats["max"] < 20 {
		t.Fatalf("expected max around 20+, got %f", stats["max"])
	}
	if p95 := h.GetPercentile(95); p95 < 20 {
		t.Fatalf("expected p95 in high bucket, got %f", p95)
	}
}

func TestLatencyHistogramEmptyAndBoundaryCases(t *testing.T) {
	h := &LatencyHistogram{Min: math.MaxFloat64}

	empty := h.GetStats()
	if empty["count"] != 0 || empty["avg"] != 0 || empty["p95"] != 0 {
		t.Fatalf("expected zero stats for empty histogram, got %+v", empty)
	}
	if p := h.GetPercentile(99); p != 0 {
		t.Fatalf("expected percentile=0 for empty histogram, got %f", p)
	}

	h.Record(-3.7)
	if h.Buckets[0] != 1 {
		t.Fatalf("expected negative latency to clamp into bucket 0")
	}

	h.Record(float64(MaxLatencyBin) + 500)
	if h.Overflow != 1 {
		t.Fatalf("expected overflow increment, got %d", h.Overflow)
	}
	if p := h.GetPercentile(99); p != float64(MaxLatencyBin) {
		t.Fatalf("expected overflow percentile to return MaxLatencyBin, got %f", p)
	}
}

func TestLatencyHistogramGetStatsAndReset(t *testing.T) {
	h := &LatencyHistogram{Min: math.MaxFloat64}
	h.Record(12)
	h.Record(18)

	stats := h.GetStatsAndReset()
	if stats["count"] != 2 {
		t.Fatalf("expected count 2 before reset, got %f", stats["count"])
	}
	if h.Count != 0 || h.Sum != 0 || h.Min != math.MaxFloat64 {
		t.Fatalf("expected histogram to reset, got count=%d sum=%f min=%f", h.Count, h.Sum, h.Min)
	}
}

func TestCollectorTrackAndAdd(t *testing.T) {
	c := NewCollector()
	c.Track("find", 10*time.Millisecond)
	c.Track("updateOne", 15*time.Millisecond)
	c.Track("deleteMany", 20*time.Millisecond)
	c.Add("insert", 3, 30*time.Millisecond)

	if got := atomic.LoadUint64(&c.FindOps); got != 1 {
		t.Fatalf("expected FindOps=1, got %d", got)
	}
	if got := atomic.LoadUint64(&c.UpdateOps); got != 1 {
		t.Fatalf("expected UpdateOps=1, got %d", got)
	}
	if got := atomic.LoadUint64(&c.DeleteOps); got != 1 {
		t.Fatalf("expected DeleteOps=1, got %d", got)
	}
	if got := atomic.LoadUint64(&c.InsertOps); got != 3 {
		t.Fatalf("expected InsertOps=3, got %d", got)
	}
	if c.TotalHist.Count != 6 {
		t.Fatalf("expected total histogram count 6, got %d", c.TotalHist.Count)
	}
}

func TestCollectorTrackUnknownOpUpdatesTotalOnly(t *testing.T) {
	c := NewCollector()
	c.Track("unknown", 5*time.Millisecond)

	if c.TotalHist.Count != 1 {
		t.Fatalf("expected total histogram updated for unknown operation")
	}
	if got := atomic.LoadUint64(&c.FindOps) + atomic.LoadUint64(&c.InsertOps) + atomic.LoadUint64(&c.UpdateOps) + atomic.LoadUint64(&c.DeleteOps) + atomic.LoadUint64(&c.AggOps) + atomic.LoadUint64(&c.TransOps) + atomic.LoadUint64(&c.UpsertOps); got != 0 {
		t.Fatalf("expected no typed op counters for unknown operation, got %d", got)
	}
}

func TestFormatHelpers(t *testing.T) {
	if got := formatLatency(999.9); !strings.Contains(got, "ms") {
		t.Fatalf("expected ms format, got %q", got)
	}
	if got := formatLatency(2000); !strings.Contains(got, "s") {
		t.Fatalf("expected seconds format, got %q", got)
	}
	if got := formatLatency(120000); !strings.Contains(got, "m") {
		t.Fatalf("expected minutes format, got %q", got)
	}

	if got := formatInt(1234567); got != "1,234,567" {
		t.Fatalf("formatInt mismatch: %q", got)
	}
	if got := formatInt(-12345); got != "-12,345" {
		t.Fatalf("formatInt negative mismatch: %q", got)
	}
}

func TestGetOverriddenEnvVarsFiltersPassword(t *testing.T) {
	t.Setenv("PLGM_ALPHA", "1")
	t.Setenv("PLGM_PASSWORD", "secret")

	vars := getOverriddenEnvVars()
	joined := strings.Join(vars, "\n")
	if strings.Contains(joined, "PLGM_PASSWORD") {
		t.Fatalf("expected password env var to be filtered")
	}
	if !strings.Contains(joined, "PLGM_ALPHA=1") {
		t.Fatalf("expected PLGM_ALPHA override to be included, got %v", vars)
	}
}

var stdoutCaptureMu sync.Mutex

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	stdoutCaptureMu.Lock()
	defer stdoutCaptureMu.Unlock()

	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("create pipe: %v", err)
	}
	os.Stdout = w

	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()

	fn()

	_ = w.Close()
	os.Stdout = old
	return <-done
}

func readCSVRowsAllowVariableColumns(path string) ([][]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	reader := csv.NewReader(f)
	reader.FieldsPerRecord = -1
	return reader.ReadAll()
}

func waitForCSVRowsAtLeast(path string, want int, timeout time.Duration) ([][]string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		rows, err := readCSVRowsAllowVariableColumns(path)
		if err == nil && len(rows) >= want {
			return rows, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return readCSVRowsAllowVariableColumns(path)
}

func TestMonitorPrintsHeaderAndWritesCSV(t *testing.T) {
	origTickerFactory := monitorTickerFactory
	defer func() { monitorTickerFactory = origTickerFactory }()

	tickCh := make(chan time.Time, 1)
	monitorTickerFactory = func(d time.Duration) (<-chan time.Time, func()) {
		return tickCh, func() {}
	}

	c := NewCollector()
	csvPath := filepath.Join(t.TempDir(), "metrics.csv")
	done := make(chan struct{})
	var wg sync.WaitGroup
	output := captureStdout(t, func() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Monitor(done, 1, 2, true, false, csvPath, false)
		}()

		c.Track("find", 20*time.Millisecond)
		tickCh <- time.Now()

		if _, err := waitForCSVRowsAtLeast(csvPath, 2, 2*time.Second); err != nil {
			t.Fatalf("wait for csv row write: %v", err)
		}
		close(done)

		waitDone := make(chan struct{})
		go func() {
			wg.Wait()
			close(waitDone)
		}()
		select {
		case <-waitDone:
		case <-time.After(2 * time.Second):
			t.Fatalf("Monitor did not stop after done signal")
		}
	})

	if !strings.Contains(output, "Starting Workload") {
		t.Fatalf("expected monitor start output, got: %q", output)
	}
	if !strings.Contains(output, "TOTAL OPS") {
		t.Fatalf("expected monitor header output, got: %q", output)
	}

	rows, err := readCSVRowsAllowVariableColumns(csvPath)
	if err != nil {
		t.Fatalf("read csv rows: %v", err)
	}
	if len(rows) < 2 {
		t.Fatalf("expected header + at least one row, got %d rows", len(rows))
	}
	header := rows[0]
	if len(header) != 19 || header[0] != "Timestamp" || header[18] != "Iteration" {
		t.Fatalf("unexpected csv header: %+v", header)
	}
	last := rows[len(rows)-1]
	if len(last) != 19 {
		t.Fatalf("expected 19 csv columns, got %d", len(last))
	}
	if _, err := strconv.ParseFloat(last[2], 64); err != nil {
		t.Fatalf("expected numeric Total_OpsSec, got %q", last[2])
	}
	if last[18] != "1" {
		t.Fatalf("expected default iteration value 1, got %q", last[18])
	}
}

func TestMonitorCSVAppendSkipsHeaderIfFileHasData(t *testing.T) {
	origTickerFactory := monitorTickerFactory
	defer func() { monitorTickerFactory = origTickerFactory }()

	tickCh := make(chan time.Time, 1)
	monitorTickerFactory = func(d time.Duration) (<-chan time.Time, func()) {
		return tickCh, func() {}
	}

	csvPath := filepath.Join(t.TempDir(), "append.csv")
	seed := "A,B\n1,2\n"
	if err := os.WriteFile(csvPath, []byte(seed), 0o644); err != nil {
		t.Fatalf("seed csv: %v", err)
	}

	c := NewCollector()
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.Monitor(done, 1, 1, true, true, csvPath, true)
	}()

	tickCh <- time.Now()

	if _, err := waitForCSVRowsAtLeast(csvPath, 3, 2*time.Second); err != nil {
		t.Fatalf("wait for appended csv row: %v", err)
	}
	close(done)
	wg.Wait()

	rows, err := readCSVRowsAllowVariableColumns(csvPath)
	if err != nil {
		t.Fatalf("read csv rows: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows (2 seeded + 1 appended), got %d", len(rows))
	}
	if !reflect.DeepEqual(rows[0], []string{"A", "B"}) {
		t.Fatalf("expected seeded header preserved, got %+v", rows[0])
	}
	if len(rows[2]) != 19 {
		t.Fatalf("expected appended monitor row with 19 columns, got %d", len(rows[2]))
	}
}

func TestPrintFinalSummaryOutputsExpectedSections(t *testing.T) {
	c := NewCollector()
	c.Track("find", 10*time.Millisecond)
	c.Track("insert", 20*time.Millisecond)

	output := captureStdout(t, func() {
		c.PrintFinalSummary(2 * time.Second)
	})

	expectedSnippets := []string{
		"Workload Finished",
		"SUMMARY",
		"Runtime:",
		"2.00s",
		"Total Ops:",
		"LATENCY DISTRIBUTION",
		"SELECT",
		"INSERT",
	}
	for _, s := range expectedSnippets {
		if !strings.Contains(output, s) {
			t.Fatalf("expected summary output to contain %q, got: %q", s, output)
		}
	}
}

func TestPrintFinalSummarySilentModeMessage(t *testing.T) {
	c := NewCollector()
	output := captureStdout(t, func() {
		c.PrintFinalSummary(1500*time.Millisecond, true)
	})
	if !strings.Contains(output, "Web UI Active") || !strings.Contains(output, "1.50s") {
		t.Fatalf("expected silent mode message with duration, got: %q", output)
	}
}

func TestAccuracyCounters(t *testing.T) {
	c := NewCollector()

	c.RecordFindResult(3)
	c.RecordFindResult(0)
	c.RecordUpdateResult(2, 1)
	c.RecordUpdateResult(0, 0)
	c.RecordDeleteResult(1)
	c.RecordTargeting(true)
	c.RecordTargeting(true)
	c.RecordTargeting(false)

	acc := c.AccuracyStats()
	if acc.FindOps != 2 || acc.FindReturned != 3 || acc.FindZero != 1 {
		t.Fatalf("unexpected find counters: %+v", acc)
	}
	if acc.UpdateOps != 2 || acc.UpdateMatched != 2 || acc.UpdateModified != 1 {
		t.Fatalf("unexpected update counters: %+v", acc)
	}
	if acc.DeleteOps != 1 || acc.DeleteDeleted != 1 {
		t.Fatalf("unexpected delete counters: %+v", acc)
	}
	if acc.TargetExisting != 2 || acc.TargetRandom != 1 {
		t.Fatalf("unexpected targeting counters: %+v", acc)
	}
}
