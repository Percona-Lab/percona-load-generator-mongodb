package stats

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// UIChartOps is one throughput or cumulative-total sample, in operations.
type UIChartOps struct {
	Total     float64 `json:"total"`
	Find      float64 `json:"find"`
	Insert    float64 `json:"insert"`
	Update    float64 `json:"update"`
	Delete    float64 `json:"delete"`
	Aggregate float64 `json:"aggregate"`
}

// UIChartLatency is one latency sample in milliseconds. A nil field means the
// window had no operations for that series (the chart leaves a gap).
type UIChartLatency struct {
	Total       *float64 `json:"total"`
	Find        *float64 `json:"find"`
	Insert      *float64 `json:"insert"`
	Update      *float64 `json:"update"`
	Delete      *float64 `json:"delete"`
	Aggregate   *float64 `json:"aggregate"`
	Transaction *float64 `json:"transaction"`
}

// Phases of a sample when workers are not running.
const (
	// UIChartPhaseWait is the configured wait between two iterations.
	UIChartPhaseWait = "wait"
	// UIChartPhaseIdle is time without running workers outside a wait:
	// preparing the next iteration, or finishing after the last one.
	UIChartPhaseIdle = "idle"
)

// UIChartPoint is one live-dashboard sample. Seq increases for the life of the
// collector so a client can ask for samples after the last one it drew. Every
// sample of a run is kept.
type UIChartPoint struct {
	Seq        int     `json:"seq"`
	ElapsedSec float64 `json:"elapsedSec"`
	// ActiveSec is the time workers ran up to this sample; waits between
	// iterations are not counted. Average rates divide by it.
	ActiveSec float64 `json:"activeSec"`
	Iteration int     `json:"iteration,omitempty"`
	Waiting   bool    `json:"waiting,omitempty"`
	// Phase is empty while workers run, else UIChartPhaseWait or
	// UIChartPhaseIdle.
	Phase string `json:"phase,omitempty"`
	// Workers is the worker count the load profile asked for.
	Workers int `json:"workers,omitempty"`
	// Marker names the events since the previous sample, joined by "; ".
	Marker string         `json:"marker,omitempty"`
	Rates  UIChartOps     `json:"rates"`
	Totals UIChartOps     `json:"totals"`
	Avg    UIChartLatency `json:"avg"`
	P95    UIChartLatency `json:"p95"`
	P99    UIChartLatency `json:"p99"`
}

type uiLatAccum struct {
	sum   float64
	count float64
}

type uiChartSeries struct {
	mu            sync.Mutex
	points        []UIChartPoint
	nextSeq       int
	events        []string
	lastWorkers   int
	prevElapsed   float64
	havePrev      bool
	prevFind      uint64
	prevInsert    uint64
	prevUpdate    uint64
	prevDelete    uint64
	prevAgg       uint64
	prevLat       map[string]uiLatAccum
	lastIteration int
	lastWaiting   bool
}

// UIChartSample is one sample to record: seconds since the workload
// started, the iteration, whether the run waits between iterations, the
// latency of the operations since the previous sample, and whether it is the
// last sample of the run.
type UIChartSample struct {
	Now       time.Time
	Elapsed   float64
	Iteration int
	Waiting   bool
	Final     bool
	Interval  map[string]map[string]float64
}

// NoteChartEvent labels the next dashboard sample with an event, such as a
// stop request.
func (c *Collector) NoteChartEvent(text string) {
	if c == nil || strings.TrimSpace(text) == "" {
		return
	}
	c.uiChart.mu.Lock()
	c.uiChart.events = append(c.uiChart.events, text)
	c.uiChart.mu.Unlock()
}

// RecordUIChartSample appends one dashboard sample taken now.
func (c *Collector) RecordUIChartSample(elapsed float64, iteration int, waiting bool, interval map[string]map[string]float64) {
	c.AddUIChartSample(UIChartSample{Now: time.Now(), Elapsed: elapsed, Iteration: iteration, Waiting: waiting, Interval: interval})
}

// AddUIChartSample appends one dashboard sample from the current counters
// and the interval latency snapshot. Rates are operations since the previous
// sample divided by the seconds since it, like a server's opcounters; a wait
// between iterations shows as zero. Update counts include upserts.
func (c *Collector) AddUIChartSample(in UIChartSample) {
	if c == nil {
		return
	}
	elapsed, iteration, interval := in.Elapsed, in.Iteration, in.Interval
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	active, running := c.ActiveDurationAt(now)
	_, waitingNow := c.WaitingSince()
	phase := ""
	switch {
	case in.Final:
	case in.Waiting || waitingNow:
		phase = UIChartPhaseWait
	case !running && atomic.LoadInt64(&c.workloadStartUnix) != 0:
		phase = UIChartPhaseIdle
	}
	waiting := phase == UIChartPhaseWait
	workers := int(atomic.LoadInt64(&c.concurrencyTarget))
	if elapsed < 0 {
		elapsed = 0
	}
	find := atomic.LoadUint64(&c.FindOps)
	insert := atomic.LoadUint64(&c.InsertOps)
	update := atomic.LoadUint64(&c.UpdateOps) + atomic.LoadUint64(&c.UpsertOps)
	del := atomic.LoadUint64(&c.DeleteOps)
	agg := atomic.LoadUint64(&c.AggOps)

	c.uiChart.mu.Lock()
	defer c.uiChart.mu.Unlock()
	st := &c.uiChart

	delta := elapsed
	if st.havePrev {
		delta = elapsed - st.prevElapsed
	}
	if delta < 0.001 {
		delta = 0.001
	}
	rate := func(now, prev uint64) float64 {
		d := float64(now) - float64(prev)
		if d < 0 {
			d = 0
		}
		return math.Round(d / delta)
	}
	totals := UIChartOps{
		Find:      float64(find),
		Insert:    float64(insert),
		Update:    float64(update),
		Delete:    float64(del),
		Aggregate: float64(agg),
	}
	totals.Total = totals.Find + totals.Insert + totals.Update + totals.Delete + totals.Aggregate
	rates := UIChartOps{
		Find:      rate(find, st.prevFind),
		Insert:    rate(insert, st.prevInsert),
		Update:    rate(update, st.prevUpdate),
		Delete:    rate(del, st.prevDelete),
		Aggregate: rate(agg, st.prevAgg),
	}
	rates.Total = rates.Find + rates.Insert + rates.Update + rates.Delete + rates.Aggregate

	var events []string
	if iteration > 0 && iteration != st.lastIteration {
		events = append(events, fmt.Sprintf("Iter %d", iteration))
		st.lastIteration = iteration
	} else if waiting && !st.lastWaiting {
		events = append(events, "Wait")
	}
	if phase == "" && workers > 0 && st.lastWorkers > 0 && workers != st.lastWorkers {
		events = append(events, fmt.Sprintf("Workers %d→%d", st.lastWorkers, workers))
	}
	if workers > 0 && phase == "" {
		st.lastWorkers = workers
	}
	events = append(events, st.events...)
	st.events = nil
	marker := strings.Join(events, "; ")
	st.lastWaiting = waiting
	st.prevElapsed = elapsed
	st.havePrev = true
	st.prevFind, st.prevInsert, st.prevUpdate, st.prevDelete, st.prevAgg = find, insert, update, del, agg

	avg := UIChartLatency{}
	p95 := UIChartLatency{}
	p99 := UIChartLatency{}
	sum, count := histSumCount(c.TotalHist)
	waiting = phase != ""
	setLatency(&avg, &p95, &p99, st, "total", interval["total"], sum, count, true, waiting)
	sum, count = histSumCount(c.FindHist)
	setLatency(&avg, &p95, &p99, st, "find", interval["find"], sum, count, true, waiting)
	sum, count = histSumCount(c.InsertHist)
	setLatency(&avg, &p95, &p99, st, "insert", interval["insert"], sum, count, true, waiting)
	uSum, uCount := histSumCount(c.UpdateHist)
	sSum, sCount := histSumCount(c.UpsertHist)
	setLatency(&avg, &p95, &p99, st, "update", interval["update"], uSum+sSum, uCount+sCount, true, waiting)
	sum, count = histSumCount(c.DeleteHist)
	setLatency(&avg, &p95, &p99, st, "delete", interval["delete"], sum, count, true, waiting)
	sum, count = histSumCount(c.AggHist)
	setLatency(&avg, &p95, &p99, st, "aggregate", interval["aggregate"], sum, count, true, waiting)
	sum, count = histSumCount(c.TransHist)
	setLatency(&avg, &p95, &p99, st, "transaction", interval["transaction"], sum, count, false, waiting)

	st.nextSeq++
	st.points = append(st.points, UIChartPoint{
		Seq:        st.nextSeq,
		ElapsedSec: elapsed,
		ActiveSec:  math.Round(active.Seconds()*1000) / 1000,
		Iteration:  iteration,
		Waiting:    phase == UIChartPhaseWait,
		Phase:      phase,
		Workers:    workers,
		Marker:     marker,
		Rates:      rates,
		Totals:     totals,
		Avg:        avg,
		P95:        p95,
		P99:        p99,
	})
}

// UIChartHistory returns samples with Seq greater than since, in order.
func (c *Collector) UIChartHistory(since int) []UIChartPoint {
	if c == nil {
		return []UIChartPoint{}
	}
	c.uiChart.mu.Lock()
	defer c.uiChart.mu.Unlock()
	out := make([]UIChartPoint, 0)
	for _, p := range c.uiChart.points {
		if p.Seq > since {
			out = append(out, p)
		}
	}
	return out
}

func histSumCount(h *LatencyHistogram) (sum, count float64) {
	if h == nil {
		return 0, 0
	}
	st := h.GetStats()
	return st["sum"], st["count"]
}

func setLatency(avg, p95, p99 *UIChartLatency, st *uiChartSeries, op string, bucket map[string]float64, sum, count float64, useFallback, waiting bool) {
	var avgV, p95V, p99V *float64
	if v, ok := intervalMetric(bucket, "avg"); ok {
		avgV = f64p(v)
	} else if useFallback {
		if v, ok := st.avgDelta(op, sum, count); ok {
			avgV = f64p(v)
		}
	}
	if v, ok := intervalMetric(bucket, "p95"); ok {
		p95V = f64p(v)
	}
	if v, ok := intervalMetric(bucket, "p99"); ok {
		p99V = f64p(v)
	}
	// Without running workers there is no latency; the chart leaves a gap.
	if waiting {
		avgV, p95V, p99V = nil, nil, nil
	}
	switch op {
	case "total":
		avg.Total, p95.Total, p99.Total = avgV, p95V, p99V
	case "find":
		avg.Find, p95.Find, p99.Find = avgV, p95V, p99V
	case "insert":
		avg.Insert, p95.Insert, p99.Insert = avgV, p95V, p99V
	case "update":
		avg.Update, p95.Update, p99.Update = avgV, p95V, p99V
	case "delete":
		avg.Delete, p95.Delete, p99.Delete = avgV, p95V, p99V
	case "aggregate":
		avg.Aggregate, p95.Aggregate, p99.Aggregate = avgV, p95V, p99V
	case "transaction":
		avg.Transaction, p95.Transaction, p99.Transaction = avgV, p95V, p99V
	}
}

func intervalMetric(bucket map[string]float64, name string) (float64, bool) {
	if len(bucket) == 0 || bucket["count"] <= 0 {
		return 0, false
	}
	v, ok := bucket[name]
	return v, ok
}

// avgDelta is the average latency of the operations since the previous
// sample; false when there were none.
func (st *uiChartSeries) avgDelta(op string, sum, count float64) (float64, bool) {
	if st.prevLat == nil {
		st.prevLat = map[string]uiLatAccum{}
	}
	prev := st.prevLat[op]
	st.prevLat[op] = uiLatAccum{sum: sum, count: count}
	if count-prev.count > 0 {
		return (sum - prev.sum) / (count - prev.count), true
	}
	return 0, false
}

func f64p(v float64) *float64 {
	p := v
	return &p
}
