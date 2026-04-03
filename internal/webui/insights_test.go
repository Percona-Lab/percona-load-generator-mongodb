package webui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/db"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
)

func TestHandleInsightsLifecycleAndParity(t *testing.T) {
	s := NewServer(&config.AppConfig{Duration: "2s"})
	c := stats.NewCollector()
	c.ConfigureInsights(&config.AppConfig{
		InsightsEnabled:         true,
		InsightsSamplingRate:    1,
		InsightsSlowThresholdMs: 1,
		InsightsMaxEvents:       100,
		InsightsMaxGroups:       50,
	})

	c.RecordOperationEvent(
		"find",
		"testdb",
		"flights",
		"find|{_id:<num>}",
		"find on fields: _id",
		[]string{"_id"},
		20*time.Millisecond,
		true,
		1,
		map[string]interface{}{"_id": 1},
		nil,
	)

	s.CurrentStats = c
	s.IsRunning = true

	req := httptest.NewRequest(http.MethodGet, "/api/insights", nil)
	rec := httptest.NewRecorder()
	s.handleInsights(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var pending map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &pending); err != nil {
		t.Fatalf("decode pending: %v", err)
	}
	meta, ok := pending["metadata"].(map[string]interface{})
	if !ok || meta["status"] != "pending" {
		t.Fatalf("expected pending status, got %+v", pending)
	}

	s.IsRunning = false

	rec1 := httptest.NewRecorder()
	s.handleInsights(rec1, req)
	if rec1.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec1.Code)
	}
	var ready1 map[string]interface{}
	if err := json.Unmarshal(rec1.Body.Bytes(), &ready1); err != nil {
		t.Fatalf("decode ready1: %v", err)
	}

	m1, ok := ready1["metadata"].(map[string]interface{})
	if !ok || (m1["status"] != "ready" && m1["status"] != "empty") {
		t.Fatalf("expected ready/empty status, got %+v", ready1)
	}
	if _, ok := ready1["slow_queries"].([]interface{}); !ok {
		t.Fatalf("expected slow_queries field in ready payload, got %+v", ready1)
	}

	rec2 := httptest.NewRecorder()
	s.handleInsights(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec2.Code)
	}
	var ready2 map[string]interface{}
	if err := json.Unmarshal(rec2.Body.Bytes(), &ready2); err != nil {
		t.Fatalf("decode ready2: %v", err)
	}

	if !reflect.DeepEqual(ready1, ready2) {
		t.Fatalf("expected repeated insights payload to be stable and identical")
	}
}

func TestEnrichInsightsWithExplain_ClassifiesUnsupportedAndInsufficientMetadata(t *testing.T) {
	rep := stats.InsightsReport{
		SlowQueries: []stats.SlowQueryInsight{
			{ShapeID: "s_find", Operation: "find", Collection: "orders"},
			{ShapeID: "s_agg", Operation: "aggregate", Collection: "orders"},
			{ShapeID: "s_insert", Operation: "insert", Collection: "orders"},
		},
		QueryShapes: []stats.SlowQueryInsight{
			{ShapeID: "s_find", Operation: "find", Collection: "orders"},
			{ShapeID: "s_agg", Operation: "aggregate", Collection: "orders"},
			{ShapeID: "s_insert", Operation: "insert", Collection: "orders"},
		},
		PotentialIndexIssues: []stats.IndexIssue{
			{ShapeID: "s_find", Collection: "orders", Operation: "find"},
		},
	}
	events := []stats.OperationEvent{
		{Operation: "find", Collection: "orders", ShapeKey: "k1"},
		{Operation: "aggregate", Collection: "orders", ShapeKey: "k2"},
	}
	rep.SlowQueries[0].ShapeID = stats.StableShapeID("find", "orders", "k1")
	rep.QueryShapes[0].ShapeID = rep.SlowQueries[0].ShapeID
	rep.SlowQueries[1].ShapeID = stats.StableShapeID("aggregate", "orders", "k2")
	rep.QueryShapes[1].ShapeID = rep.SlowQueries[1].ShapeID
	rep.PotentialIndexIssues[0].ShapeID = rep.SlowQueries[0].ShapeID

	enrichInsightsWithExplain("ready", &rep, events, &config.AppConfig{}, 5, 1000, "high_and_low", 1, 1, 0)

	if rep.SlowQueries[0].ExplainStatus != "insufficient_metadata" || rep.SlowQueries[0].ExplainReason != "missing_filter_sample" {
		t.Fatalf("expected find shape insufficient metadata, got status=%q reason=%q", rep.SlowQueries[0].ExplainStatus, rep.SlowQueries[0].ExplainReason)
	}
	if rep.SlowQueries[1].ExplainStatus != "insufficient_metadata" || rep.SlowQueries[1].ExplainReason != "missing_pipeline_sample" {
		t.Fatalf("expected aggregate shape insufficient metadata, got status=%q reason=%q", rep.SlowQueries[1].ExplainStatus, rep.SlowQueries[1].ExplainReason)
	}
	if rep.SlowQueries[2].ExplainStatus != "not_supported" {
		t.Fatalf("expected insert shape not_supported, got status=%q", rep.SlowQueries[2].ExplainStatus)
	}
	if rep.PotentialIndexIssues[0].ExplainStatus != "insufficient_metadata" {
		t.Fatalf("expected index issue explain status propagated, got %q", rep.PotentialIndexIssues[0].ExplainStatus)
	}
}

func TestEnrichInsightsWithExplain_RespectsTopNSelection(t *testing.T) {
	origConnect := connectFn
	connectFn = func(ctx context.Context, cfg *config.AppConfig, dbName string) (*db.Connection, error) {
		return nil, context.DeadlineExceeded
	}
	defer func() { connectFn = origConnect }()

	shape1 := stats.StableShapeID("find", "orders", "k1")
	shape2 := stats.StableShapeID("find", "orders", "k2")
	rep := stats.InsightsReport{
		SlowQueries: []stats.SlowQueryInsight{
			{ShapeID: shape1, Operation: "find", Collection: "orders"},
			{ShapeID: shape2, Operation: "find", Collection: "orders"},
		},
		QueryShapes: []stats.SlowQueryInsight{
			{ShapeID: shape1, Operation: "find", Collection: "orders"},
			{ShapeID: shape2, Operation: "find", Collection: "orders"},
		},
	}
	events := []stats.OperationEvent{
		{Operation: "find", Database: "testdb", Collection: "orders", ShapeKey: "k1", FilterSample: map[string]interface{}{"a": 1}},
		{Operation: "find", Database: "testdb", Collection: "orders", ShapeKey: "k2", FilterSample: map[string]interface{}{"b": 1}},
	}

	enrichInsightsWithExplain("ready", &rep, events, &config.AppConfig{}, 1, 1000, "high_and_low", 1, 1, 0)

	if rep.SlowQueries[0].ExplainStatus != "execution_failed" {
		t.Fatalf("expected first shape selected for explain and failed via stubbed connect, got %q", rep.SlowQueries[0].ExplainStatus)
	}
	if rep.SlowQueries[1].ExplainStatus != "explain_unavailable" || rep.SlowQueries[1].ExplainReason != "not_selected_top_n_1" {
		t.Fatalf("expected second shape to be topN filtered, got status=%q reason=%q", rep.SlowQueries[1].ExplainStatus, rep.SlowQueries[1].ExplainReason)
	}
}

func TestClassifyExplainCommandError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "not authorized", err: errors.New("not authorized on admin to execute command"), want: "not_authorized"},
		{name: "namespace missing", err: errors.New("ns does not exist"), want: "namespace_not_found"},
		{name: "command missing", err: errors.New("CommandNotFound: explain"), want: "command_not_found"},
		{name: "timeout", err: errors.New("MaxTimeMSExpired"), want: "max_time_exceeded"},
		{name: "fallback", err: errors.New("some other failure"), want: "run_command_failed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyExplainCommandError(tt.err)
			if got != tt.want {
				t.Fatalf("classifyExplainCommandError() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRetryExplainEvidence_RetriesTimeoutAndEventuallySucceeds(t *testing.T) {
	var calls int32
	got := retryExplainEvidence(500, 2, 0, func(maxTimeMs int) explainEvidence {
		c := atomic.AddInt32(&calls, 1)
		if c < 3 {
			return explainEvidence{status: "timed_out", reason: "max_time_exceeded"}
		}
		return explainEvidence{status: "explained", reason: "ixscan_observed"}
	})
	if got.status != "explained" {
		t.Fatalf("expected explained after retries, got status=%q reason=%q", got.status, got.reason)
	}
	if calls != 3 {
		t.Fatalf("expected 3 attempts, got %d", calls)
	}
}

func TestRetryExplainEvidence_DoesNotRetryNonTimeoutFailures(t *testing.T) {
	var calls int32
	got := retryExplainEvidence(500, 3, 0, func(maxTimeMs int) explainEvidence {
		atomic.AddInt32(&calls, 1)
		return explainEvidence{status: "execution_failed", reason: "not_authorized"}
	})
	if got.reason != "not_authorized" {
		t.Fatalf("expected original non-timeout reason, got %q", got.reason)
	}
	if calls != 1 {
		t.Fatalf("expected single attempt for non-timeout failure, got %d", calls)
	}
}

func TestIsExplainCandidatePriorityShape(t *testing.T) {
	shape := stats.SlowQueryInsight{ShapeID: "s1", Severity: "low", P95Ms: 5, P99Ms: 10}
	indexSet := map[string]struct{}{"s1": {}}
	if !isExplainCandidatePriorityShape(shape, indexSet, 200, "high_and_low") {
		t.Fatalf("expected index issue shapes to be prioritized for explain")
	}
	shape2 := stats.SlowQueryInsight{ShapeID: "s2", Severity: "low", P95Ms: 5, P99Ms: 10}
	if isExplainCandidatePriorityShape(shape2, map[string]struct{}{}, 200, "high_and_low") {
		t.Fatalf("expected low-value non-index shape to be filtered")
	}
	shape3 := stats.SlowQueryInsight{ShapeID: "s3", Severity: "high", P95Ms: 20, P99Ms: 30}
	if !isExplainCandidatePriorityShape(shape3, map[string]struct{}{}, 200, "high_only") {
		t.Fatalf("expected high severity shape to be explain candidate")
	}
	shape4 := stats.SlowQueryInsight{ShapeID: "s4", Severity: "medium", P95Ms: 900, P99Ms: 1200}
	if isExplainCandidatePriorityShape(shape4, map[string]struct{}{}, 200, "high_only") {
		t.Fatalf("expected medium severity to be filtered in high_only mode")
	}
	shape5 := stats.SlowQueryInsight{ShapeID: "s5", Severity: "medium", P95Ms: 900, P99Ms: 1200}
	if !isExplainCandidatePriorityShape(shape5, map[string]struct{}{}, 200, "medium_only") {
		t.Fatalf("expected medium severity to be eligible in medium_only mode")
	}
	shape6 := stats.SlowQueryInsight{ShapeID: "s6", Severity: "high", P95Ms: 1200, P99Ms: 1500}
	if isExplainCandidatePriorityShape(shape6, map[string]struct{}{}, 200, "critical_only") {
		t.Fatalf("expected high severity to be filtered in critical_only mode")
	}
	shape7 := stats.SlowQueryInsight{ShapeID: "s7", Severity: "critical", P95Ms: 2200, P99Ms: 2500}
	if !isExplainCandidatePriorityShape(shape7, map[string]struct{}{}, 200, "critical_only") {
		t.Fatalf("expected critical severity to be eligible in critical_only mode")
	}
}

func TestInsightsEndpointAndExportParityBySeverityMode(t *testing.T) {
	buildCollector := func(mode string) *stats.Collector {
		c := stats.NewCollector()
		c.ConfigureInsights(&config.AppConfig{
			InsightsEnabled:             true,
			InsightsSamplingRate:        1,
			InsightsSlowThresholdMs:     200,
			InsightsMaxEvents:           200,
			InsightsMaxGroups:           50,
			InsightsExplainSeverityMode: mode,
			InsightsExplainEnabled:      false,
		})
		for i := 0; i < 6; i++ {
			c.RecordOperationEvent("find", "testdb", "orders", "shape_high", "high", []string{"customer_id"}, 950*time.Millisecond, true, 1, nil, nil)
		}
		for i := 0; i < 3; i++ {
			c.RecordOperationEvent("find", "testdb", "payments", "shape_medium", "medium", []string{"account_id"}, 280*time.Millisecond, true, 1, nil, nil)
		}
		return c
	}

	tests := []struct {
		name      string
		mode      string
		wantCount int
	}{
		{name: "high_only", mode: "high_only", wantCount: 1},
		{name: "high_and_low", mode: "high_and_low", wantCount: 2},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer(&config.AppConfig{Duration: "1s"})
			s.CurrentStats = buildCollector(tc.mode)
			s.IsRunning = false

			req := httptest.NewRequest(http.MethodGet, "/api/insights", nil)
			rec := httptest.NewRecorder()
			s.handleInsights(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", rec.Code)
			}

			var insights map[string]interface{}
			if err := json.Unmarshal(rec.Body.Bytes(), &insights); err != nil {
				t.Fatalf("decode insights: %v", err)
			}

			slow, ok := insights["slow_queries"].([]interface{})
			if !ok {
				t.Fatalf("expected slow_queries array in /api/insights response, got %+v", insights)
			}
			if len(slow) != tc.wantCount {
				t.Fatalf("expected %d slow queries for mode %s, got %d", tc.wantCount, tc.mode, len(slow))
			}

			exportPayload := map[string]interface{}{
				"generated_at": "2026-04-03T00:00:00Z",
				"insights":     insights,
			}
			b, err := json.Marshal(exportPayload)
			if err != nil {
				t.Fatalf("marshal export payload: %v", err)
			}

			var roundTrip map[string]interface{}
			if err := json.Unmarshal(b, &roundTrip); err != nil {
				t.Fatalf("unmarshal export payload: %v", err)
			}
			exportInsights, ok := roundTrip["insights"].(map[string]interface{})
			if !ok {
				t.Fatalf("expected insights object in export payload")
			}
			if !reflect.DeepEqual(insights, exportInsights) {
				t.Fatalf("expected export insights payload to match /api/insights response exactly")
			}
		})
	}
}
