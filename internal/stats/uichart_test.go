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
	if all[2].Marker != "Wait" || !all[2].Waiting || all[2].P99.Find == nil || *all[2].P99.Find != 0 || all[2].Avg.Find == nil || *all[2].Avg.Find != 0 {
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

func TestUIChartHistoryDropsOldestAndKeepsSeq(t *testing.T) {
	c := NewCollector()
	c.uiChart.max = 2
	for i := 1; i <= 3; i++ {
		c.FindOps = uint64(i)
		c.RecordUIChartSample(float64(i), 1, false, nil)
	}
	got := c.UIChartHistory(0)
	if len(got) != 2 || got[0].Seq != 2 || got[1].Seq != 3 {
		t.Fatalf("capped history = %+v", got)
	}
	if newer := c.UIChartHistory(2); len(newer) != 1 || newer[0].Seq != 3 {
		t.Fatalf("since 2 = %+v", newer)
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
