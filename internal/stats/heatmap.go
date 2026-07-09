package stats

import "sync"

// defaultMaxHeatmapBuckets bounds memory for very long runs; older buckets are
// dropped once the cap is exceeded.
const defaultMaxHeatmapBuckets = 3600

// LatencyBucket is one time-window of latency percentiles, used to render a
// latency-over-time heatmap and to surface tail-latency drift.
type LatencyBucket struct {
	ElapsedSec      float64 `json:"elapsedSec"`
	TimestampUnixMs int64   `json:"timestampUnixMs"`
	Count           int64   `json:"count"`
	P50             float64 `json:"p50"`
	P95             float64 `json:"p95"`
	P99             float64 `json:"p99"`
	Max             float64 `json:"max"`
}

// LatencyHeatmap is an ordered, bounded series of LatencyBuckets. It is safe for
// concurrent append/read.
type LatencyHeatmap struct {
	mu         sync.Mutex
	buckets    []LatencyBucket
	maxBuckets int
}

// NewLatencyHeatmap builds a heatmap series. A non-positive max uses the default.
func NewLatencyHeatmap(maxBuckets int) *LatencyHeatmap {
	if maxBuckets <= 0 {
		maxBuckets = defaultMaxHeatmapBuckets
	}
	return &LatencyHeatmap{maxBuckets: maxBuckets}
}

// Append adds a bucket, trimming the oldest if the cap is exceeded.
func (lh *LatencyHeatmap) Append(b LatencyBucket) {
	if lh == nil {
		return
	}
	lh.mu.Lock()
	defer lh.mu.Unlock()
	lh.buckets = append(lh.buckets, b)
	if len(lh.buckets) > lh.maxBuckets {
		overflow := len(lh.buckets) - lh.maxBuckets
		lh.buckets = lh.buckets[overflow:]
	}
}

// Snapshot returns a copy of the current series.
func (lh *LatencyHeatmap) Snapshot() []LatencyBucket {
	if lh == nil {
		return nil
	}
	lh.mu.Lock()
	defer lh.mu.Unlock()
	out := make([]LatencyBucket, len(lh.buckets))
	copy(out, lh.buckets)
	return out
}

// Len returns the number of buckets currently stored.
func (lh *LatencyHeatmap) Len() int {
	if lh == nil {
		return 0
	}
	lh.mu.Lock()
	defer lh.mu.Unlock()
	return len(lh.buckets)
}
