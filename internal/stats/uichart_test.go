package stats

import (
	"testing"
	"time"
)

func TestUIChartHistoryRatesMarkersAndSince(t *testing.T) {
	c := NewCollector()
	c.FindOps = 10
	c.InsertOps = 5
	c.RecordUIChartSample(2, 1, false, map[string]map[string]float64{
		"find":  {"avg": 4, "p95": 6, "p99": 9, "count": 10},
		"total": {"avg": 3, "p95": 5, "p99": 8, "count": 15},
	})
	c.FindOps = 30
	c.RecordUIChartSample(3, 1, false, map[string]map[string]float64{
		"find": {"avg": 2, "p95": 3, "p99": 4, "count": 20},
	})
	c.RecordUIChartSample(4, 1, true, map[string]map[string]float64{
		"find": {"avg": 50, "p95": 50, "p99": 50, "count": 1},
	})

	all := c.UIChartHistory(0)
	if len(all) != 3 {
		t.Fatalf("samples = %d, want 3", len(all))
	}
	if all[0].Seq != 1 || all[0].Marker != "Iter 1" || all[0].Rates.Find != 5 || all[0].Rates.Insert != 3 || all[0].Rates.Total != 8 {
		t.Fatalf("first sample = %+v", all[0])
	}
	if all[0].Totals.Find != 10 || all[0].Totals.Total != 15 || all[0].Avg.Find == nil || *all[0].Avg.Find != 4 || all[0].P99.Find == nil || *all[0].P99.Find != 9 {
		t.Fatalf("first latency/totals = %+v avg=%v p99=%v", all[0].Totals, all[0].Avg.Find, all[0].P99.Find)
	}
	if all[1].Marker != "" || all[1].Rates.Find != 20 || all[1].Totals.Find != 30 {
		t.Fatalf("second sample = %+v", all[1])
	}
	// Nothing runs during a wait: zero throughput and no latency (a gap,
	// not 0 ms).
	if all[2].Marker != "Wait" || !all[2].Waiting || all[2].Phase != UIChartPhaseWait || all[2].Rates.Total != 0 || all[2].P99.Find != nil || all[2].Avg.Find != nil {
		t.Fatalf("waiting sample = %+v p99=%v", all[2], all[2].P99.Find)
	}
	if all[1].P99.Insert != nil {
		t.Fatalf("insert p99 = %v, want a gap when the window had no inserts", *all[1].P99.Insert)
	}

	rest := c.UIChartHistory(1)
	if len(rest) != 2 || rest[0].Seq != 2 || rest[1].Seq != 3 {
		t.Fatalf("history since 1 = %+v", rest)
	}
	if len(c.UIChartHistory(3)) != 0 {
		t.Fatal("history since the last seq should be empty")
	}
}

// Average rates divide by the time workers ran: the wait between two
// iterations counts in the elapsed time but not in the active time, and the
// samples say which phase they were taken in.
func TestUIChartActiveTimeLeavesOutWaits(t *testing.T) {
	c := NewCollector()
	t0 := time.Now()
	c.MarkWorkloadStart()
	c.Track("find", time.Millisecond)
	c.AddUIChartSample(UIChartSample{Now: time.Now(), Elapsed: 0.5, Iteration: 1})
	time.Sleep(30 * time.Millisecond)
	c.MarkWorkloadEnd()
	c.SetWaiting(true)
	time.Sleep(60 * time.Millisecond)
	c.AddUIChartSample(UIChartSample{Now: time.Now(), Elapsed: time.Since(t0).Seconds(), Iteration: 1})
	c.SetWaiting(false)
	c.AddUIChartSample(UIChartSample{Now: time.Now(), Elapsed: time.Since(t0).Seconds(), Iteration: 2})
	c.MarkWorkloadStart()
	time.Sleep(30 * time.Millisecond)
	c.NoteChartEvent("Stopped by user")
	c.AddUIChartSample(UIChartSample{Now: time.Now(), Elapsed: time.Since(t0).Seconds(), Iteration: 2})
	c.MarkWorkloadEnd()

	got := c.UIChartHistory(0)
	if len(got) != 4 {
		t.Fatalf("samples = %d", len(got))
	}
	if got[0].Phase != "" || got[1].Phase != UIChartPhaseWait || got[1].Marker != "Wait" || got[2].Phase != UIChartPhaseIdle || got[3].Phase != "" {
		t.Fatalf("phases = %q %q %q %q, markers %q", got[0].Phase, got[1].Phase, got[2].Phase, got[3].Phase, got[1].Marker)
	}
	if got[2].Marker != "Iter 2" || got[3].Marker != "Stopped by user" {
		t.Fatalf("markers = %q, %q", got[2].Marker, got[3].Marker)
	}
	// The wait of 60 ms is in the elapsed time, not the active time.
	if got[1].ActiveSec != got[2].ActiveSec || got[1].ActiveSec < 0.025 || got[1].ActiveSec > 0.055 {
		t.Fatalf("active during the wait = %v then %v, want the ~30 ms of iteration 1", got[1].ActiveSec, got[2].ActiveSec)
	}
	if got[1].ElapsedSec-got[1].ActiveSec < 0.05 {
		t.Fatalf("elapsed %v should include the wait, active %v should not", got[1].ElapsedSec, got[1].ActiveSec)
	}
	active, ok := c.WorkloadDuration()
	if !ok || active < 55*time.Millisecond || active > 120*time.Millisecond {
		t.Fatalf("WorkloadDuration = %v, want both iterations without the wait", active)
	}
}

func TestUIChartWorkerChangesAreMarked(t *testing.T) {
	c := NewCollector()
	c.SetConcurrency(4, 4)
	c.RecordUIChartSample(1, 1, false, nil)
	c.SetConcurrency(8, 8)
	c.RecordUIChartSample(2, 1, false, nil)
	got := c.UIChartHistory(0)
	if got[0].Workers != 4 || got[1].Workers != 8 || got[1].Marker != "Workers 4→8" {
		t.Fatalf("samples = %+v", got)
	}
}

func TestUIChartHistoryKeepsEverySample(t *testing.T) {
	c := NewCollector()
	const n = 20000 // well past the old 3,600-sample limit
	for i := 1; i <= n; i++ {
		c.FindOps = uint64(i)
		c.RecordUIChartSample(float64(i)/2, 1, false, nil)
	}
	got := c.UIChartHistory(0)
	if len(got) != n || got[0].Seq != 1 || got[0].ElapsedSec != 0.5 || got[n-1].Seq != n {
		t.Fatalf("history has %d samples, first %+v", len(got), got[0])
	}
	if newer := c.UIChartHistory(n - 1); len(newer) != 1 || newer[0].Seq != n {
		t.Fatalf("since %d = %+v", n-1, newer)
	}
}

func TestUIChartSampleUsesCumulativeFallbackWhenIntervalIsEmpty(t *testing.T) {
	c := NewCollector()
	c.Track("find", 10*time.Millisecond)
	c.RecordUIChartSample(1, 0, false, nil)
	got := c.UIChartHistory(0)
	if len(got) != 1 || got[0].Marker != "" || got[0].Avg.Find == nil || *got[0].Avg.Find != 10 || got[0].P95.Find != nil {
		t.Fatalf("fallback sample = %+v avg=%v p95=%v", got, got[0].Avg.Find, got[0].P95.Find)
	}
	if got[0].Rates.Find != 1 {
		t.Fatalf("find rate = %v, want 1", got[0].Rates.Find)
	}
}
