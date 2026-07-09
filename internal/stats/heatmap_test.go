package stats

import (
	"math"
	"testing"
	"time"
)

func TestLatencyHeatmapAppendAndBounds(t *testing.T) {
	hm := NewLatencyHeatmap(3)
	for i := 0; i < 5; i++ {
		hm.Append(LatencyBucket{ElapsedSec: float64(i), Count: int64(i)})
	}
	got := hm.Snapshot()
	if len(got) != 3 {
		t.Fatalf("expected cap of 3 buckets, got %d", len(got))
	}
	// Oldest buckets (0,1) should have been trimmed, leaving 2,3,4.
	if got[0].ElapsedSec != 2 || got[2].ElapsedSec != 4 {
		t.Fatalf("unexpected trimming, got %+v", got)
	}
}

func TestSnapshotAndResetComputesPercentiles(t *testing.T) {
	h := &LatencyHistogram{Min: math.MaxFloat64}
	for i := 1; i <= 100; i++ {
		h.Record(float64(i))
	}
	count, p50, p95, p99, max := h.SnapshotAndReset()
	if count != 100 {
		t.Fatalf("expected count 100, got %d", count)
	}
	if p50 < 49 || p50 > 51 {
		t.Fatalf("expected p50 ~50, got %v", p50)
	}
	if p95 < 94 || p95 > 96 {
		t.Fatalf("expected p95 ~95, got %v", p95)
	}
	if p99 < 98 || p99 > 100 {
		t.Fatalf("expected p99 ~99, got %v", p99)
	}
	if max != 100 {
		t.Fatalf("expected max 100, got %v", max)
	}
	// After reset, the histogram must be empty.
	if c, _, _, _, _ := h.SnapshotAndReset(); c != 0 {
		t.Fatalf("expected empty histogram after reset, got count %d", c)
	}
}

func TestCaptureLatencyIntervalBuildsSeries(t *testing.T) {
	c := NewCollector()
	c.MarkWorkloadStart()

	// First window: a couple of fast ops.
	c.Track("find", 5*time.Millisecond)
	c.Track("find", 7*time.Millisecond)
	c.CaptureLatencyInterval()

	// Second window: a slower tail to show drift.
	c.Track("find", 80*time.Millisecond)
	c.Track("find", 120*time.Millisecond)
	c.CaptureLatencyInterval()

	// An empty window should not append a bucket.
	c.CaptureLatencyInterval()

	series := c.LatencyHeatmap()
	if len(series) != 2 {
		t.Fatalf("expected 2 heatmap buckets, got %d", len(series))
	}
	if series[0].Count != 2 || series[1].Count != 2 {
		t.Fatalf("unexpected bucket counts: %+v", series)
	}
	if series[1].P99 <= series[0].P99 {
		t.Fatalf("expected tail latency drift: first p99=%v second p99=%v", series[0].P99, series[1].P99)
	}
	if series[0].TimestampUnixMs == 0 {
		t.Fatalf("expected timestamped bucket")
	}
}

func TestCaptureLatencyIntervalEveryGatesByWindow(t *testing.T) {
	c := NewCollector()
	c.MarkWorkloadStart()

	// First call always captures (no prior timestamp).
	c.Track("find", 5*time.Millisecond)
	c.CaptureLatencyIntervalEvery(time.Hour)
	if got := len(c.LatencyHeatmap()); got != 1 {
		t.Fatalf("expected 1 bucket after first capture, got %d", got)
	}

	// A second call within the window must be gated (no new bucket), and the
	// operation recorded in between must NOT be lost - it stays in the window.
	c.Track("find", 9*time.Millisecond)
	c.CaptureLatencyIntervalEvery(time.Hour)
	if got := len(c.LatencyHeatmap()); got != 1 {
		t.Fatalf("expected capture to be gated within window, got %d buckets", got)
	}

	// minWindow <= 0 forces an immediate capture, flushing the pending op.
	c.CaptureLatencyIntervalEvery(0)
	series := c.LatencyHeatmap()
	if len(series) != 2 {
		t.Fatalf("expected 2 buckets after forced capture, got %d", len(series))
	}
	if series[1].Count != 1 {
		t.Fatalf("expected the gated op to be retained in the second window, got count %d", series[1].Count)
	}
}

func TestLatencyHeatmapNilSafe(t *testing.T) {
	var hm *LatencyHeatmap
	hm.Append(LatencyBucket{})
	if hm.Snapshot() != nil {
		t.Fatalf("expected nil snapshot from nil heatmap")
	}
	if hm.Len() != 0 {
		t.Fatalf("expected len 0 from nil heatmap")
	}
}
