package webui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
)

func impactFixtureServer(t *testing.T, maxEvents int) *WebServer {
	t.Helper()
	c := stats.NewCollector()
	c.ConfigureInsights(&config.AppConfig{
		InsightsEnabled:         true,
		InsightsSamplingRate:    1,
		InsightsSlowThresholdMs: 200,
		InsightsMaxEvents:       maxEvents,
		InsightsMaxGroups:       50,
	})
	for i := 0; i < 1000; i++ {
		c.RecordOperationEvent("find", "shop", "orders", "find|{status:<str>}", "find on fields: status", []string{"status"}, 60*time.Millisecond, true, 1, nil, "", "unit_test", "w", "", "", "", "", 0)
		if i%150 == 0 {
			c.RecordOperationEvent("find", "shop", "audit", "find|{actor:<str>}", "find on fields: actor", []string{"actor"}, 1200*time.Millisecond, true, 1, nil, "", "unit_test", "w", "", "", "", "", 0)
		}
	}
	s := NewServer(&config.AppConfig{Duration: "1s"})
	s.CurrentStats = c
	return s
}

func getInsightsJSON(t *testing.T, s *WebServer) map[string]interface{} {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleInsights(rec, httptest.NewRequest(http.MethodGet, "/api/insights", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode insights: %v", err)
	}
	return out
}

func getReportHTML(t *testing.T, s *WebServer) string {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleReport(rec, httptest.NewRequest(http.MethodGet, "/api/report", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 from report, got %d", rec.Code)
	}
	return rec.Body.String()
}

func TestImpactConsistentAcrossAPIReportAndExport(t *testing.T) {
	s := impactFixtureServer(t, 5000)
	api := getInsightsJSON(t, s)

	impact, ok := api["impact"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected impact object in /api/insights, got %+v", api)
	}
	groups, _ := impact["groups"].([]interface{})
	if len(groups) != 2 {
		t.Fatalf("expected 2 impact groups, got %+v", impact)
	}
	first := groups[0].(map[string]interface{})
	second := groups[1].(map[string]interface{})
	if first["namespace"] != "shop.orders" || first["operation_count"] != float64(1000) || first["total_operation_ms"] != float64(60000) {
		t.Fatalf("unexpected top impact group: %+v", first)
	}
	if second["namespace"] != "shop.audit" || second["operation_count"] != float64(7) || second["total_operation_ms"] != float64(8400) {
		t.Fatalf("unexpected second impact group: %+v", second)
	}
	if second["sample_percentile_status"] != "insufficient_samples" {
		t.Fatalf("expected insufficient_samples for 7-op shape, got %+v", second)
	}
	if _, has := second["sample_p95_ms"]; has {
		t.Fatalf("sample_p95_ms must be omitted when samples are insufficient: %+v", second)
	}
	cov := impact["coverage"].(map[string]interface{})
	if cov["time_basis"] != stats.ImpactTimeBasis {
		t.Fatalf("expected time basis %q, got %+v", stats.ImpactTimeBasis, cov)
	}
	slow, _ := api["slow_queries"].([]interface{})
	if len(slow) != 1 || slow[0].(map[string]interface{})["collection"] != "audit" {
		t.Fatalf("expected severity view unchanged (only audit), got %+v", slow)
	}

	// JSON export ("Download Summary") embeds the /api/insights payload.
	b, err := json.Marshal(map[string]interface{}{"insights": api})
	if err != nil {
		t.Fatal(err)
	}
	var rt map[string]interface{}
	if err := json.Unmarshal(b, &rt); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rt["insights"].(map[string]interface{})["impact"], impact) {
		t.Fatalf("export impact payload differs from /api/insights")
	}

	// HTML report renders the same values.
	html := getReportHTML(t, s)
	for _, want := range []string{
		"Workload Impact (summed concurrent operation time)",
		"not wall-clock time",
		"shop.orders", "60000.00", "87.72",
		"shop.audit", "8400.00", "12.28", "insufficient_samples",
		"Observed: 1007 operations, 68400.00 ms",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("HTML report missing %q", want)
		}
	}
	if strings.Index(html, "shop.orders") > strings.Index(html, "shop.audit") {
		t.Fatalf("HTML report must keep impact rank order")
	}
}

func TestImpactWrappedBufferDisclosedInAPIAndReport(t *testing.T) {
	s := impactFixtureServer(t, 10)
	impact := getInsightsJSON(t, s)["impact"].(map[string]interface{})
	cov := impact["coverage"].(map[string]interface{})
	if cov["sample_buffer_wrapped"] != true {
		t.Fatalf("expected sample_buffer_wrapped=true, got %+v", cov)
	}
	lims, _ := cov["limitations"].([]interface{})
	if len(lims) == 0 || !strings.Contains(fmt.Sprint(lims...), "Sample buffer wrapped") {
		t.Fatalf("expected wrapped limitation in API, got %+v", lims)
	}
	first := impact["groups"].([]interface{})[0].(map[string]interface{})
	if first["total_operation_ms"] != float64(60000) {
		t.Fatalf("totals must not depend on retention, got %+v", first)
	}
	if html := getReportHTML(t, s); !strings.Contains(html, "Sample buffer wrapped") {
		t.Fatalf("HTML report must disclose wrapped sample buffer")
	}
}

func TestImpactOmittedFromReportWhileRunning(t *testing.T) {
	s := impactFixtureServer(t, 5000)
	s.IsRunning = true
	if html := getReportHTML(t, s); strings.Contains(html, "Workload Impact") {
		t.Fatalf("impact must not be rendered for an in-progress run")
	}
}

func TestStaticUIRendersImpactAPIFields(t *testing.T) {
	page, err := staticFiles.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("read embedded index.html: %v", err)
	}
	impact := getInsightsJSON(t, impactFixtureServer(t, 5000))["impact"].(map[string]interface{})
	group := impact["groups"].([]interface{})[0].(map[string]interface{})
	cov := impact["coverage"].(map[string]interface{})

	fields := map[string]map[string]interface{}{
		"rank": group, "operation": group, "namespace": group, "shape_summary": group,
		"operation_count": group, "total_operation_ms": group, "share_of_observed_time": group,
		"avg_ms": group, "error_count": group, "sample_count": group, "sample_p95_ms": group,
		"sample_percentile_status": group, "explanation": group,
		"observed_operations": cov, "observed_operation_ms": cov, "time_basis_note": cov,
		"interpretation_note": cov,
	}
	for field, obj := range fields {
		if _, ok := obj[field]; !ok {
			t.Fatalf("API impact payload lacks %q", field)
		}
		if !strings.Contains(string(page), "."+field) {
			t.Fatalf("UI does not render API field %q", field)
		}
	}
	if !strings.Contains(string(page), "cov.limitations") || !strings.Contains(string(page), "rep.impact") {
		t.Fatalf("UI must render impact limitations from rep.impact")
	}
}
