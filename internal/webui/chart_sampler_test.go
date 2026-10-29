package webui

import (
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
)

// The server samples the charts by itself, so a run with no browser open
// still has every sample; a late tick labels the gap, and a finished run gets
// exactly one last sample at its end.
func TestChartSamplerRecordsWithoutABrowser(t *testing.T) {
	s := NewServer(&config.AppConfig{Duration: "30s"})
	c := stats.NewCollector()
	c.MarkWorkloadStart()
	c.Track("find", time.Millisecond)
	start, _ := c.WorkloadStart()
	s.CurrentStats = c
	s.IsRunning = true
	s.LifecyclePhase = "running"
	s.CurrentIteration = 1

	s.sampleCharts(start.Add(time.Second))
	s.sampleCharts(start.Add(1500 * time.Millisecond))
	s.sampleCharts(start.Add(10 * time.Second)) // the machine slept
	got := c.UIChartHistory(0)
	if len(got) != 3 || got[0].Marker != "Iter 1" || got[1].Marker != "" || got[2].Marker != "No samples for 8s" {
		t.Fatalf("samples = %+v", got)
	}
	if got[2].ElapsedSec != 10 {
		t.Fatalf("elapsed = %v, want seconds since the workload start", got[2].ElapsedSec)
	}

	c.MarkWorkloadEnd()
	s.mu.Lock()
	s.IsRunning = false
	s.setLifecyclePhaseLocked("completed", "done")
	s.CompletedAt = start.Add(12 * time.Second)
	s.mu.Unlock()
	s.sampleCharts(start.Add(13 * time.Second))
	s.sampleCharts(start.Add(14 * time.Second))
	got = c.UIChartHistory(0)
	if len(got) != 4 || got[3].ElapsedSec != 12 || got[3].Phase != "" {
		t.Fatalf("after the end: %d samples, last %+v; want one final sample at the end", len(got), got[len(got)-1])
	}
}

func TestChartSampleIntervalFollowsTheRefreshRate(t *testing.T) {
	s := NewServer(&config.AppConfig{Duration: "30s"})
	if s.chartSampleInterval() != 500*time.Millisecond {
		t.Fatalf("default = %v", s.chartSampleInterval())
	}
	for in, want := range map[time.Duration]time.Duration{
		250 * time.Millisecond: 250 * time.Millisecond,
		10 * time.Millisecond:  100 * time.Millisecond,
		time.Minute:            5 * time.Second,
	} {
		s.setChartSampleInterval(in)
		if got := s.chartSampleInterval(); got != want {
			t.Fatalf("set %v: got %v, want %v", in, got, want)
		}
	}
}
