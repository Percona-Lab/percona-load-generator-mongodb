package stats

import (
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
)

func recordShapeEvents(c *Collector, op, coll, shapeKey, shapeSummary string, filterFields []string, durs []time.Duration) {
	for _, d := range durs {
		c.RecordOperationEvent(op, "testdb", coll, shapeKey, shapeSummary, filterFields, d, true, 1, map[string]interface{}{"k": 1}, nil)
	}
}

func TestInsightsReportFiltersByThresholdAndDefaultHighSeverityOnly(t *testing.T) {
	c := NewCollector()
	c.ConfigureInsights(&config.AppConfig{
		InsightsEnabled:         true,
		InsightsSamplingRate:    1,
		InsightsSlowThresholdMs: 200,
		InsightsMaxEvents:       200,
		InsightsMaxGroups:       50,
		InsightsExplainEnabled:  true,
	})

	recordShapeEvents(c, "find", "orders", "shape_high", "high severity", []string{"customer_id"}, []time.Duration{
		900 * time.Millisecond,
		1000 * time.Millisecond,
		1100 * time.Millisecond,
		1200 * time.Millisecond,
		950 * time.Millisecond,
		980 * time.Millisecond,
	})
	recordShapeEvents(c, "find", "payments", "shape_medium", "medium severity", []string{"account_id"}, []time.Duration{
		250 * time.Millisecond,
		300 * time.Millisecond,
		280 * time.Millisecond,
	})
	recordShapeEvents(c, "find", "inventory", "shape_fast", "below threshold", []string{"sku"}, []time.Duration{
		50 * time.Millisecond,
		60 * time.Millisecond,
		70 * time.Millisecond,
	})

	rep := c.GetFinalInsights()
	if rep.Metadata.ExplainSeverityMode != "high_only" {
		t.Fatalf("expected default explain severity mode high_only, got %q", rep.Metadata.ExplainSeverityMode)
	}
	if len(rep.SlowQueries) != 1 {
		t.Fatalf("expected only high-severity group to remain, got %d", len(rep.SlowQueries))
	}
	if rep.SlowQueries[0].Collection != "orders" {
		t.Fatalf("expected orders to be kept, got %q", rep.SlowQueries[0].Collection)
	}
	if len(rep.QueryShapes) != 1 || rep.QueryShapes[0].Collection != "orders" {
		t.Fatalf("expected query shapes filtered to orders only, got %+v", rep.QueryShapes)
	}
	if len(rep.AffectedCollections) != 1 || rep.AffectedCollections[0].Collection != "orders" {
		t.Fatalf("expected affected collections filtered to orders only, got %+v", rep.AffectedCollections)
	}
	if rep.Metadata.FilteredByThreshold != 1 || rep.Metadata.FilteredBySeverity != 1 {
		t.Fatalf("expected filtered counters threshold=1 severity=1, got threshold=%d severity=%d", rep.Metadata.FilteredByThreshold, rep.Metadata.FilteredBySeverity)
	}
}

func TestInsightsReportIncludesMediumWhenSeverityModeHighAndLow(t *testing.T) {
	c := NewCollector()
	c.ConfigureInsights(&config.AppConfig{
		InsightsEnabled:             true,
		InsightsSamplingRate:        1,
		InsightsSlowThresholdMs:     200,
		InsightsMaxEvents:           200,
		InsightsMaxGroups:           50,
		InsightsExplainSeverityMode: "high_and_low",
		InsightsExplainEnabled:      true,
	})

	recordShapeEvents(c, "find", "orders", "shape_high", "high severity", []string{"customer_id"}, []time.Duration{
		900 * time.Millisecond,
		1000 * time.Millisecond,
		1100 * time.Millisecond,
		1200 * time.Millisecond,
		950 * time.Millisecond,
		980 * time.Millisecond,
	})
	recordShapeEvents(c, "find", "payments", "shape_medium", "medium severity", []string{"account_id"}, []time.Duration{
		250 * time.Millisecond,
		300 * time.Millisecond,
		280 * time.Millisecond,
	})
	recordShapeEvents(c, "find", "inventory", "shape_fast", "below threshold", []string{"sku"}, []time.Duration{
		50 * time.Millisecond,
		60 * time.Millisecond,
		70 * time.Millisecond,
	})

	rep := c.GetFinalInsights()
	if rep.Metadata.ExplainSeverityMode != "high_and_low" {
		t.Fatalf("expected explain severity mode high_and_low, got %q", rep.Metadata.ExplainSeverityMode)
	}
	if len(rep.SlowQueries) != 2 {
		t.Fatalf("expected high+medium groups above threshold, got %d", len(rep.SlowQueries))
	}
	for _, q := range rep.SlowQueries {
		if q.Collection == "inventory" {
			t.Fatalf("did not expect below-threshold collection inventory in slow queries")
		}
	}
	if rep.Metadata.FilteredByThreshold != 1 || rep.Metadata.FilteredBySeverity != 0 {
		t.Fatalf("expected filtered counters threshold=1 severity=0, got threshold=%d severity=%d", rep.Metadata.FilteredByThreshold, rep.Metadata.FilteredBySeverity)
	}
}

func TestGetExplainSettingsIncludesSeverityMode(t *testing.T) {
	c := NewCollector()
	c.ConfigureInsights(&config.AppConfig{
		InsightsEnabled:             true,
		InsightsExplainEnabled:      true,
		InsightsExplainTopN:         7,
		InsightsExplainMaxTimeMS:    1500,
		InsightsExplainSeverityMode: "high_and_low",
		InsightsExplainWorkers:      2,
		InsightsExplainRetries:      3,
		InsightsExplainBackoffMS:    250,
	})

	enabled, topN, maxMs, mode, workers, retries, backoff := c.GetExplainSettings()
	if !enabled || topN != 7 || maxMs != 1500 || mode != "high_and_low" || workers != 2 || retries != 3 || backoff != 250 {
		t.Fatalf("unexpected explain settings: enabled=%v topN=%d maxMs=%d mode=%q workers=%d retries=%d backoff=%d", enabled, topN, maxMs, mode, workers, retries, backoff)
	}
}

func TestInsightsReportSeverityModesMediumAndCriticalOnly(t *testing.T) {
	makeCollector := func(mode string) *Collector {
		c := NewCollector()
		c.ConfigureInsights(&config.AppConfig{
			InsightsEnabled:             true,
			InsightsSamplingRate:        1,
			InsightsSlowThresholdMs:     200,
			InsightsMaxEvents:           300,
			InsightsMaxGroups:           100,
			InsightsExplainEnabled:      true,
			InsightsExplainSeverityMode: mode,
		})
		recordShapeEvents(c, "find", "critical_coll", "shape_critical", "critical severity", []string{"c"}, []time.Duration{
			2100 * time.Millisecond, 2200 * time.Millisecond, 2300 * time.Millisecond, 2400 * time.Millisecond, 2500 * time.Millisecond,
			2100 * time.Millisecond, 2200 * time.Millisecond, 2300 * time.Millisecond, 2400 * time.Millisecond, 2500 * time.Millisecond,
		})
		recordShapeEvents(c, "find", "high_coll", "shape_high", "high severity", []string{"h"}, []time.Duration{
			900 * time.Millisecond, 1000 * time.Millisecond, 1100 * time.Millisecond, 1200 * time.Millisecond, 950 * time.Millisecond, 980 * time.Millisecond,
		})
		recordShapeEvents(c, "find", "medium_coll", "shape_medium", "medium severity", []string{"m"}, []time.Duration{
			250 * time.Millisecond, 300 * time.Millisecond, 280 * time.Millisecond,
		})
		return c
	}

	repMedium := makeCollector("medium_only").GetFinalInsights()
	if repMedium.Metadata.ExplainSeverityMode != "medium_only" {
		t.Fatalf("expected medium_only mode, got %q", repMedium.Metadata.ExplainSeverityMode)
	}
	if got := len(repMedium.SlowQueries); got != 3 {
		t.Fatalf("expected 3 groups in medium_only mode, got %d", got)
	}
	if repMedium.Metadata.FilteredBySeverity != 0 {
		t.Fatalf("expected no severity filtering in medium_only for medium+, got %d", repMedium.Metadata.FilteredBySeverity)
	}

	repCritical := makeCollector("critical_only").GetFinalInsights()
	if repCritical.Metadata.ExplainSeverityMode != "critical_only" {
		t.Fatalf("expected critical_only mode, got %q", repCritical.Metadata.ExplainSeverityMode)
	}
	if got := len(repCritical.SlowQueries); got != 1 {
		t.Fatalf("expected 1 group in critical_only mode, got %d", got)
	}
	if repCritical.SlowQueries[0].Collection != "critical_coll" {
		t.Fatalf("expected critical_coll in critical_only mode, got %q", repCritical.SlowQueries[0].Collection)
	}
	if repCritical.Metadata.FilteredBySeverity != 2 {
		t.Fatalf("expected 2 severity-filtered groups in critical_only mode, got %d", repCritical.Metadata.FilteredBySeverity)
	}
}
