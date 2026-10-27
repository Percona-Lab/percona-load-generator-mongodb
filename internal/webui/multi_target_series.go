package webui

import (
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/multitarget"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
)

// targetCursor is where a dashboard left off for one target: the last chart
// sample and the last latency heatmap window it drew.
type targetCursor struct {
	chartSeq int
	heatMs   int64
}

// parseTargetSince reads targetSince=index:seq:ms,... from /api/stats. Bad
// entries are ignored, so that target is sent in full.
func parseTargetSince(r *http.Request) map[int]targetCursor {
	out := map[int]targetCursor{}
	for _, part := range strings.Split(r.URL.Query().Get("targetSince"), ",") {
		f := strings.Split(strings.TrimSpace(part), ":")
		if len(f) != 3 {
			continue
		}
		idx, err1 := strconv.Atoi(f[0])
		seq, err2 := strconv.Atoi(f[1])
		ms, err3 := strconv.ParseInt(f[2], 10, 64)
		if err1 != nil || err2 != nil || err3 != nil || seq < 0 || ms < 0 {
			continue
		}
		out[idx] = targetCursor{chartSeq: seq, heatMs: ms}
	}
	return out
}

// sampleRunningTargets records a chart sample and a ~1 s heatmap window for
// every target that is running, with its elapsed time measured from its own
// workload start, so the servers line up on "seconds since the server
// started". A concurrent run gets samples for every target, not only the
// one the dashboard follows. It returns the interval latency it took from
// current, for the single-server fields of /api/stats.
func sampleRunningTargets(plan *multitarget.Plan, current *stats.Collector, iterations map[*stats.Collector]int, waiting bool, now time.Time) map[string]map[string]float64 {
	var currentInterval map[string]map[string]float64
	for _, t := range plan.Targets {
		c := t.Collector
		if c == nil || t.State().Status != multitarget.StatusRunning {
			continue
		}
		start, ok := c.WorkloadStart()
		if !ok {
			continue
		}
		c.CaptureLatencyIntervalEvery(time.Second)
		interval := c.GetUILatencyTimelineAndReset()
		isCurrent := c == current
		if isCurrent {
			currentInterval = interval
		}
		c.RecordUIChartSample(now.Sub(start).Seconds(), iterations[c], waiting && isCurrent, interval)
	}
	return currentInterval
}

// multiTargetSeries is the per-target data behind the several-servers
// dashboard: totals, latency distribution per operation type (and per
// transaction), and the chart
// samples and heatmap windows after each target's cursor (all of them for a
// stored run). It holds labels, never URIs.
func multiTargetSeries(plan *multitarget.Plan, since map[int]targetCursor) []map[string]any {
	if plan == nil {
		return nil
	}
	out := make([]map[string]any, 0, len(plan.Targets))
	for _, t := range plan.Targets {
		c := t.Collector
		if c == nil {
			continue
		}
		st := t.State()
		cur := since[t.Index]
		find := atomic.LoadUint64(&c.FindOps)
		insert := atomic.LoadUint64(&c.InsertOps)
		update := atomic.LoadUint64(&c.UpdateOps) + atomic.LoadUint64(&c.UpsertOps)
		del := atomic.LoadUint64(&c.DeleteOps)
		agg := atomic.LoadUint64(&c.AggOps)
		heat := []stats.LatencyBucket{}
		for _, b := range c.LatencyHeatmap() {
			if b.TimestampUnixMs > cur.heatMs {
				heat = append(heat, b)
			}
		}
		out = append(out, map[string]any{
			"index":      t.Index,
			"label":      t.Label,
			"status":     st.Status,
			"elapsedSec": st.ElapsedSec,
			"ops": map[string]uint64{
				"total": find + insert + update + del + agg, "find": find, "insert": insert,
				"update": update, "delete": del, "aggregate": agg,
			},
			"dist": map[string]map[string]float64{
				"total": c.TotalHist.GetStats(), "find": c.FindHist.GetStats(), "insert": c.InsertHist.GetStats(),
				"update": c.UpdateHist.GetStats(), "delete": c.DeleteHist.GetStats(), "aggregate": c.AggHist.GetStats(),
				"transaction": c.TransHist.GetStats(),
			},
			"chartHistory":   c.UIChartHistory(cur.chartSeq),
			"latencyHeatmap": heat,
		})
	}
	return out
}
