package webui

import (
	"fmt"
	"sync/atomic"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/multitarget"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
)

// Dashboard samples are taken on the server at the refresh rate picked on the
// dashboard, whether or not a browser is open, so the charts hold every
// second of the run. A browser that was away gets the samples it missed.
const (
	defaultChartSampleInterval = 500 * time.Millisecond
	minChartSampleInterval     = 100 * time.Millisecond
	maxChartSampleInterval     = 5 * time.Second
)

// chartSampler is the sampling state kept between ticks.
type chartSampler struct {
	intervalNs atomic.Int64
	lastTick   time.Time
	// finalized are the collectors whose last sample was taken.
	finalized map[*stats.Collector]bool
}

func (s *WebServer) chartSampleInterval() time.Duration {
	if d := time.Duration(s.sampler.intervalNs.Load()); d > 0 {
		return d
	}
	return defaultChartSampleInterval
}

// setChartSampleInterval sets the sampling interval from the dashboard's
// Refresh (ms) value; out-of-range values are clamped.
func (s *WebServer) setChartSampleInterval(d time.Duration) {
	if d < minChartSampleInterval {
		d = minChartSampleInterval
	}
	if d > maxChartSampleInterval {
		d = maxChartSampleInterval
	}
	s.sampler.intervalNs.Store(int64(d))
}

// runChartSampler samples the charts until stop is closed.
func (s *WebServer) runChartSampler(stop <-chan struct{}) {
	for {
		t := time.NewTimer(s.chartSampleInterval())
		select {
		case <-stop:
			t.Stop()
			return
		case now := <-t.C:
			s.sampleCharts(now)
		}
	}
}

// sampleCharts takes one sample of the running run: the run itself, or every
// running server of a run on several servers. A finished run or server gets
// one last sample at its end. A tick that comes much later than planned (the
// machine slept or the process was suspended) labels the gap.
func (s *WebServer) sampleCharts(now time.Time) {
	interval := s.chartSampleInterval()
	s.mu.Lock()
	if s.sampler.finalized == nil {
		s.sampler.finalized = map[*stats.Collector]bool{}
	}
	gap := time.Duration(0)
	if !s.sampler.lastTick.IsZero() {
		if d := now.Sub(s.sampler.lastTick); d > 3*interval && d > 2*time.Second {
			gap = d
		}
	}
	s.sampler.lastTick = now
	s.promoteToRunningLocked()
	collector := s.CurrentStats
	phase := s.LifecyclePhase
	plan := s.multiTarget
	curIter := s.CurrentIteration
	isWait := s.IsWaiting
	execStartedAt := s.ExecStartedAt
	completedAt := s.CompletedAt
	iterations := map[*stats.Collector]int{}
	if plan != nil {
		for _, t := range plan.Targets {
			if t.Collector != nil {
				iterations[t.Collector] = t.Collector.CurrentIteration
			}
		}
	}
	finalized := s.sampler.finalized
	s.mu.Unlock()
	if collector == nil {
		return
	}
	noteGap := func(c *stats.Collector) {
		if gap > 0 {
			c.NoteChartEvent(fmt.Sprintf("No samples for %.0fs", gap.Seconds()))
		}
	}

	if plan != nil {
		for _, t := range plan.Targets {
			if t.Collector != nil && t.State().Status == multitarget.StatusRunning {
				noteGap(t.Collector)
			}
		}
		sampleRunningTargets(plan, collector, iterations, isWait, now)
		for _, t := range plan.Targets {
			c := t.Collector
			if c == nil || finalized[c] || !targetFinished(t.State().Status) {
				continue
			}
			start, ok := c.WorkloadStart()
			if !ok {
				continue
			}
			end, ok := c.WorkloadEnd()
			if !ok {
				end = now
			}
			c.CaptureLatencyIntervalEvery(0)
			c.AddUIChartSample(stats.UIChartSample{Now: end, Elapsed: end.Sub(start).Seconds(), Iteration: iterations[c], Final: true, Interval: c.GetUILatencyTimelineAndReset()})
			s.markFinalized(c)
		}
		return
	}

	start := execStartedAt
	if ws, ok := collector.WorkloadStart(); ok {
		start = ws
	}
	if start.IsZero() {
		return
	}
	switch phase {
	case "running":
		noteGap(collector)
		collector.CaptureLatencyIntervalEvery(time.Second)
		collector.AddUIChartSample(stats.UIChartSample{Now: now, Elapsed: now.Sub(start).Seconds(), Iteration: curIter, Waiting: isWait, Interval: collector.GetUILatencyTimelineAndReset()})
	case "completed", "failed":
		if finalized[collector] {
			return
		}
		end := completedAt
		if end.IsZero() {
			end = now
		}
		collector.CaptureLatencyIntervalEvery(0)
		collector.AddUIChartSample(stats.UIChartSample{Now: end, Elapsed: end.Sub(start).Seconds(), Iteration: curIter, Final: true, Interval: collector.GetUILatencyTimelineAndReset()})
		s.markFinalized(collector)
	}
}

func (s *WebServer) markFinalized(c *stats.Collector) {
	s.mu.Lock()
	s.sampler.finalized[c] = true
	s.mu.Unlock()
}

func targetFinished(status string) bool {
	switch status {
	case multitarget.StatusComplete, multitarget.StatusFailed, multitarget.StatusCancelled, multitarget.StatusIncomplete:
		return true
	}
	return false
}
