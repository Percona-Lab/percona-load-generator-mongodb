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
	"go.mongodb.org/mongo-driver/v2/bson"
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
		&stats.ExecutableQuery{Database: "testdb", Filter: map[string]interface{}{"_id": 1}},
		"find flights by _id",
		"unit_test",
		"default_workload",
		"queries_test.json",
		"q_find_1",
		"{_id:<num>}",
		"",
		0,
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
	inv := stats.ExplainInventory{CaptureStatus: "ready", Shapes: []stats.ExplainInventoryShape{
		{ShapeID: stats.StableShapeID("find", "orders", "k1"), Operation: "find", Collection: "orders", ShapeKey: "k1", ImpactRank: 1, OperationCount: 1,
			Samples: []stats.RepresentativeSample{{Operation: "find", Collection: "orders", ShapeKey: "k1"}}},
		{ShapeID: stats.StableShapeID("aggregate", "orders", "k2"), Operation: "aggregate", Collection: "orders", ShapeKey: "k2", ImpactRank: 2, OperationCount: 1,
			Samples: []stats.RepresentativeSample{{Operation: "aggregate", Collection: "orders", ShapeKey: "k2"}}},
	}}
	rep.SlowQueries[0].ShapeID = stats.StableShapeID("find", "orders", "k1")
	rep.QueryShapes[0].ShapeID = rep.SlowQueries[0].ShapeID
	rep.SlowQueries[1].ShapeID = stats.StableShapeID("aggregate", "orders", "k2")
	rep.QueryShapes[1].ShapeID = rep.SlowQueries[1].ShapeID
	rep.PotentialIndexIssues[0].ShapeID = rep.SlowQueries[0].ShapeID

	enrichInsightsWithExplain(context.Background(), "ready", &rep, inv, &config.AppConfig{}, explainParams{topN: 5, maxTimeMs: 1000, verbosity: "executionStats", severityMode: "high_and_low", workers: 1, retries: 1})

	if rep.SlowQueries[0].ExplainStatus != "insufficient_metadata" || rep.SlowQueries[0].ExplainReason != "executable_query_not_captured" {
		t.Fatalf("expected find shape insufficient metadata, got status=%q reason=%q", rep.SlowQueries[0].ExplainStatus, rep.SlowQueries[0].ExplainReason)
	}
	if rep.SlowQueries[1].ExplainStatus != "insufficient_metadata" || rep.SlowQueries[1].ExplainReason != "executable_query_not_captured" {
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
	inv := stats.ExplainInventory{CaptureStatus: "ready", Shapes: []stats.ExplainInventoryShape{
		{ShapeID: shape1, Operation: "find", Database: "testdb", Collection: "orders", ShapeKey: "k1", ImpactRank: 1, OperationCount: 1,
			Samples: []stats.RepresentativeSample{{ShapeID: shape1, Operation: "find", Database: "testdb", Collection: "orders", ShapeKey: "k1", Query: &stats.ExecutableQuery{Database: "testdb", Filter: map[string]interface{}{"a": 1}}}}},
		{ShapeID: shape2, Operation: "find", Database: "testdb", Collection: "orders", ShapeKey: "k2", ImpactRank: 2, OperationCount: 1,
			Samples: []stats.RepresentativeSample{{ShapeID: shape2, Operation: "find", Database: "testdb", Collection: "orders", ShapeKey: "k2", Query: &stats.ExecutableQuery{Database: "testdb", Filter: map[string]interface{}{"b": 1}}}}},
	}}
	enrichInsightsWithExplain(context.Background(), "ready", &rep, inv, &config.AppConfig{}, explainParams{topN: 1, maxTimeMs: 1000, verbosity: "executionStats", severityMode: "high_and_low", efficiencyMode: config.ExplainEfficiencyModeOff, workers: 1, retries: 1})

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
	got := retryExplainEvidence(context.Background(), 500, 2, 0, func(maxTimeMs int) explainEvidence {
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
	got := retryExplainEvidence(context.Background(), 500, 3, 0, func(maxTimeMs int) explainEvidence {
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
			c.RecordOperationEvent("find", "testdb", "orders", "shape_high", "high", []string{"customer_id"}, 950*time.Millisecond, true, 1, nil, "orders high query", "unit_test", "test_workload", "queries_high.json", "q_high", "{customer_id:<num>}", "", 0)
		}
		for i := 0; i < 3; i++ {
			c.RecordOperationEvent("find", "testdb", "payments", "shape_medium", "medium", []string{"account_id"}, 280*time.Millisecond, true, 1, nil, "payments medium query", "unit_test", "test_workload", "queries_medium.json", "q_medium", "{account_id:<num>}", "", 1)
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

func TestParseExplainSignalsAggregateNestedPlan(t *testing.T) {
	out := bson.M{
		"queryPlanner": bson.M{
			"winningPlan": bson.M{
				"queryPlan": bson.M{
					"stage": "GROUP",
					"inputStage": bson.M{
						"stage": "SORT",
						"inputStage": bson.M{
							"stage":     "FETCH",
							"indexName": "is_expedited_shipping_1_status_1",
							"inputStage": bson.M{
								"stage": "IXSCAN",
							},
						},
					},
				},
			},
		},
		"executionStats": bson.M{
			"executionTimeMillis": 691,
			"totalDocsExamined":   152865,
			"totalKeysExamined":   152865,
			"nReturned":           5,
		},
		"indexesUsed":     bson.A{"is_expedited_shipping_1_status_1"},
		"collectionScans": 0,
	}
	sig := parseExplainSignals(out)
	if !sig.indexScan {
		t.Fatalf("expected index scan signal")
	}
	if sig.collectionScan {
		t.Fatalf("expected no collection scan signal")
	}
	if !sig.fetch || !sig.group || !sig.sort {
		t.Fatalf("expected fetch/group/sort signals, got fetch=%v group=%v sort=%v", sig.fetch, sig.group, sig.sort)
	}
	if len(sig.indexesUsed) == 0 || sig.indexesUsed[0] != "is_expedited_shipping_1_status_1" {
		t.Fatalf("expected indexesUsed parsed, got %+v", sig.indexesUsed)
	}
	m := normalizeExplainMetrics(out, "aggregate")
	if m.DocsExamined == nil || m.DocsExamined.Value != 152865 || m.KeysExamined == nil || m.KeysExamined.Value != 152865 || m.Returned == nil || m.Returned.Value != 5 {
		t.Fatalf("expected examined/returned metrics parsed, got %+v", m)
	}
	// The plan has a pushed-down GROUP but no execution tree, so the
	// pre-group count is unknown rather than the final output.
	if m.AccessReturned != nil {
		t.Fatalf("access-stage count must be unavailable without executionStages, got %+v", m.AccessReturned)
	}
}

func TestParseExplainSignalsAggregateShardedCursorExecutionStats(t *testing.T) {
	out := bson.M{
		"shards": bson.M{
			"lab-shard0": bson.D{
				{Key: "stages", Value: bson.A{
					bson.D{{Key: "$cursor", Value: bson.D{
						{Key: "queryPlanner", Value: bson.D{
							{Key: "winningPlan", Value: bson.D{
								{Key: "queryPlan", Value: bson.D{
									{Key: "stage", Value: "GROUP"},
									{Key: "inputStage", Value: bson.D{
										{Key: "stage", Value: "FETCH"},
										{Key: "inputStage", Value: bson.D{
											{Key: "stage", Value: "IXSCAN"},
											{Key: "indexName", Value: "is_expedited_shipping_1_status_1"},
										}},
									}},
								}},
							}},
						}},
						{Key: "executionStats", Value: bson.D{
							{Key: "nReturned", Value: int64(98)},
							{Key: "executionTimeMillis", Value: int64(691)},
							{Key: "totalKeysExamined", Value: int64(152865)},
							{Key: "totalDocsExamined", Value: int64(152865)},
							{Key: "executionStages", Value: bson.D{
								{Key: "stage", Value: "nlj"},
								{Key: "collectionScans", Value: int64(0)},
								{Key: "indexSeeks", Value: int64(1)},
								{Key: "indexesUsed", Value: bson.A{"is_expedited_shipping_1_status_1"}},
								{Key: "inputStage", Value: bson.D{
									{Key: "stage", Value: "group"},
									{Key: "usedDisk", Value: false},
									{Key: "spills", Value: int64(0)},
								}},
							}},
						}},
					}}},
					bson.D{{Key: "$sort", Value: bson.D{{Key: "usedDisk", Value: false}, {Key: "spills", Value: int64(0)}}}},
				}},
			},
		},
	}

	sig := parseExplainSignals(out)
	if sig.winningPlanSummary == "stage_chain_unavailable" {
		t.Fatalf("expected stage chain to be parsed from sharded cursor plan")
	}
	if !sig.indexScan || sig.collectionScan {
		t.Fatalf("expected ixscan true and collscan false, got ixscan=%v collscan=%v", sig.indexScan, sig.collectionScan)
	}
	if !sig.fetch || !sig.group || !sig.sort {
		t.Fatalf("expected fetch/group/sort detection from nested plan, got fetch=%v group=%v sort=%v", sig.fetch, sig.group, sig.sort)
	}
	if len(sig.indexesUsed) == 0 || sig.indexesUsed[0] != "is_expedited_shipping_1_status_1" {
		t.Fatalf("expected indexesUsed from executionStages, got %+v", sig.indexesUsed)
	}
	m := normalizeExplainMetrics(out, "aggregate")
	if m.Layout != layoutShardedPipeline || m.Shards != 1 || m.DocsExamined == nil || m.DocsExamined.Value != 152865 || m.KeysExamined == nil || m.KeysExamined.Value != 152865 {
		t.Fatalf("expected executionStats counters, got %+v", m)
	}
	if m.AccessReturned != nil {
		t.Fatalf("group input count is not reported in this fixture; got %+v", m.AccessReturned)
	}
	if sig.executionTimeMS != 691 {
		t.Fatalf("expected executionTimeMillis=691, got %d", sig.executionTimeMS)
	}
}

func TestParseExplainSignalsDetectsIndexSeekWithoutIndexesUsedArray(t *testing.T) {
	out := bson.M{
		"executionStats": bson.M{
			"executionStages": bson.M{
				"stage":      "nlj",
				"indexSeeks": int64(2),
				"inputStage": bson.M{
					"stage": "ixseek",
				},
			},
			"nReturned": int64(10),
		},
	}

	sig := parseExplainSignals(out)
	if !sig.indexScan {
		t.Fatalf("expected index scan detection via ixseek/indexSeeks")
	}
	if sig.collectionScan {
		t.Fatalf("expected no collection scan")
	}
}
