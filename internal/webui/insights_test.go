package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
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
	if m1["status"] == "ready" {
		if arr, ok := ready1["slow_queries"].([]interface{}); !ok || len(arr) == 0 {
			t.Fatalf("expected slow_queries in ready payload, got %+v", ready1)
		}
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
