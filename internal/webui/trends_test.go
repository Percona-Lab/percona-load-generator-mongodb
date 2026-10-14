package webui

import (
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// trendFixtureAPI runs the ready fixture, then "restarts" (a new server and a
// store reopened from the same file) and runs the after-intervention fixture.
func trendFixtureAPI(t *testing.T) (map[string]interface{}, *WebServer, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trend_baselines.json")
	first, _ := efficiencyFixtureServerWith(t, "ready", fixtureOptions{store: stats.OpenTrendStore(path)})
	api1 := getInsightsJSON(t, first)
	if tr := api1["trends"].(map[string]interface{}); tr["status"] != stats.TrendNoBaseline {
		t.Fatalf("first run must have no baseline, got %v", tr["status"])
	}
	second, _ := efficiencyFixtureServerWith(t, "ready", fixtureOptions{after: true, store: stats.OpenTrendStore(path)})
	return getInsightsJSON(t, second), second, path
}

func trendRows(t *testing.T, api map[string]interface{}) (map[string]interface{}, map[string]map[string]interface{}) {
	t.Helper()
	v, ok := api["trends"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected trends in /api/insights")
	}
	out := map[string]map[string]interface{}{}
	for _, raw := range v["rows"].([]interface{}) {
		r := raw.(map[string]interface{})
		out[r["shape_summary"].(string)] = r
	}
	return v, out
}

func metricState(r map[string]interface{}, metric string) string {
	return r[metric].(map[string]interface{})["state"].(string)
}

func TestTrendsPersistAcrossRestartAndCompareSeparately(t *testing.T) {
	api, s, path := trendFixtureAPI(t)
	v, rows := trendRows(t, api)
	if v["status"] != "ready" || v["version"] != stats.TrendVersion {
		t.Fatalf("second run must compare against the persisted baseline: %v %v", v["status"], v["reason"])
	}
	store := v["store"].(map[string]interface{})
	if store["status"] != "ok" || store["persistent"] != true || store["baselines"].(float64) < 10 {
		t.Fatalf("store must reload baselines after restart: %v", store)
	}

	sku := rows["find on fields: sku"]
	if metricState(sku, "efficiency") != stats.TrendImproved || metricState(sku, "latency") != stats.TrendImproved {
		t.Fatalf("index intervention: sku efficiency and latency should improve: %v", sku)
	}
	kinds := map[string]bool{}
	for _, a := range sku["interventions"].([]interface{}) {
		kinds[a.(map[string]interface{})["kind"].(string)] = true
	}
	for _, k := range []string{"plan_changed", "indexes_used_changed", "index_inventory_changed"} {
		if !kinds[k] {
			t.Fatalf("sku must annotate %s, got %v", k, kinds)
		}
	}
	if sku["comparability"] != stats.TrendComparable {
		t.Fatalf("a plan/index intervention must not discard the comparison: %v", sku["comparability"])
	}

	status := rows["find on fields: status"]
	if metricState(status, "efficiency") != stats.TrendRegressed || metricState(status, "latency") != stats.TrendRegressed {
		t.Fatalf("status shape should regress on efficiency and latency: %v", status)
	}

	email := rows["find on fields: email"]
	if got := metricState(email, "latency"); got != stats.TrendInsufficient {
		t.Fatalf("12 operations must not produce a latency label, got %s", got)
	}
	region := rows["find on fields: region"]
	if got := metricState(region, "efficiency"); got != stats.TrendInsufficient {
		t.Fatalf("partial evidence must not produce an efficiency label, got %s", got)
	}
	tag := rows["find on fields: tag"]
	if got := metricState(tag, "efficiency"); got != stats.TrendInsufficient {
		t.Fatalf("failed explains must not produce an efficiency label, got %s", got)
	}
	insert := rows["insert"]
	if got := metricState(insert, "efficiency"); got != stats.TrendNotComparable {
		t.Fatalf("not-applicable operations are not compared, got %s", got)
	}
	wl := sku["workload_changes"].([]interface{})
	if len(wl) == 0 || !strings.Contains(wl[0].(map[string]interface{})["detail"].(string), "concurrency 4 → 8") {
		t.Fatalf("workload config change must be annotated: %v", wl)
	}
	if !strings.Contains(v["causation_note"].(string), "not evidence of cause") {
		t.Fatalf("causation note missing")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret-") || strings.Contains(string(raw), "\"query\"") {
		t.Fatalf("baseline file must not contain query text or literals")
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("baseline file mode %v, want 0600", fi.Mode().Perm())
	}

	page := html.UnescapeString(getReportHTML(t, s))
	if !strings.Contains(page, "Trends vs Baseline (tc-1, status: ready") || !strings.Contains(page, sku["efficiency"].(map[string]interface{})["label"].(string)) ||
		!strings.Contains(page, "Intervention (plan_changed)") || !strings.Contains(page, "not evidence of cause") {
		t.Fatalf("HTML report must show the same trend labels and annotations")
	}
	if strings.Contains(page, "secret-") {
		t.Fatalf("HTML report leaked a literal")
	}
	again := getInsightsJSON(t, s)
	if !reflect.DeepEqual(again["trends"], api["trends"]) {
		t.Fatalf("trends must be computed once per run and cached")
	}
}

func TestTrendBaselinesEndpointStatusAndReset(t *testing.T) {
	_, s, path := trendFixtureAPI(t)
	call := func(method string) (int, map[string]interface{}) {
		rec := httptest.NewRecorder()
		s.handleTrendBaselines(rec, httptest.NewRequest(method, "/api/trend-baselines", nil))
		var body map[string]interface{}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}
	code, body := call(http.MethodGet)
	if code != http.StatusOK || body["store"].(map[string]interface{})["baselines"].(float64) == 0 {
		t.Fatalf("GET must report stored baselines: %d %v", code, body)
	}
	if code, _ := call(http.MethodPost); code != http.StatusMethodNotAllowed {
		t.Fatalf("POST must be rejected, got %d", code)
	}
	code, body = call(http.MethodDelete)
	st := body["store"].(map[string]interface{})
	if code != http.StatusOK || st["baselines"].(float64) != 0 || st["status"] != "empty" {
		t.Fatalf("DELETE must clear baselines: %d %v", code, body)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("reset must remove the baseline file: %v", err)
	}
	if reopened := stats.OpenTrendStore(path); reopened.Status().Baselines != 0 {
		t.Fatalf("reset must survive a restart")
	}
}

func TestTrendsDisabledStoreNeverLabels(t *testing.T) {
	s, _ := efficiencyFixtureServerWith(t, "ready", fixtureOptions{store: stats.DisabledTrendStore()})
	v, _ := trendRows(t, getInsightsJSON(t, s))
	if v["status"] != "disabled" || len(v["rows"].([]interface{})) != 0 {
		t.Fatalf("disabled store must not compare: %v", v["status"])
	}
}

func TestExplainServerVersion(t *testing.T) {
	cases := []struct {
		name string
		out  bson.M
		want string
	}{
		{"mongod", bson.M{"serverInfo": bson.M{"host": "h", "version": "8.0.4"}}, "8.0.4"},
		{"driver nested bson.D", bson.M{"serverInfo": bson.D{{Key: "host", Value: "h"}, {Key: "version", Value: "8.0.4"}}}, "8.0.4"},
		{"mongos shard bson.D", bson.M{"queryPlanner": bson.D{{Key: "winningPlan", Value: bson.D{{Key: "shards", Value: bson.A{bson.D{{Key: "serverInfo", Value: bson.D{{Key: "version", Value: "7.0.12"}}}}}}}}}}, "7.0.12"},
		{"mongos shard", bson.M{"queryPlanner": bson.M{"winningPlan": bson.M{"shards": bson.A{bson.M{"serverInfo": bson.M{"version": "7.0.12"}}}}}}, "7.0.12"},
		{"missing", bson.M{"queryPlanner": bson.M{}}, ""},
	}
	for _, tc := range cases {
		if got := explainServerVersion(tc.out); got != tc.want {
			t.Fatalf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}
