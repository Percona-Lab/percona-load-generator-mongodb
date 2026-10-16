package webui

import (
	"encoding/json"
	"html"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
)

// trendValuesFixtureAPI runs the ready fixture and then the after-index
// fixture with query values on; the second run changes all status literals
// and half of the tag literals.
func trendValuesFixtureAPI(t *testing.T) (map[string]interface{}, *WebServer, string) {
	t.Helper()
	steppedTrendClock(t)
	path := filepath.Join(t.TempDir(), "trend_baselines.json")
	first, _ := efficiencyFixtureServerWith(t, "ready", fixtureOptions{store: stats.OpenTrendStore(path), values: true})
	getInsightsJSON(t, first)
	second, _ := efficiencyFixtureServerWith(t, "ready", fixtureOptions{after: true, store: stats.OpenTrendStore(path), values: true, changed: "status", partial: "tag"})
	return getInsightsJSON(t, second), second, path
}

func valuesState(r map[string]interface{}) string {
	qv, _ := r["query_values"].(map[string]interface{})
	if qv == nil {
		return ""
	}
	return qv["state"].(string)
}

func TestTrendQueryValuesEndToEnd(t *testing.T) {
	api, s, path := trendValuesFixtureAPI(t)
	plainAPI, _, _ := trendFixtureAPI(t)
	v, rows := trendRows(t, api)
	_, plainRows := trendRows(t, plainAPI)

	for summary, want := range map[string]string{
		"find on fields: sku":    stats.TrendValuesSame,
		"find on fields: email":  stats.TrendValuesSame,
		"find on fields: status": stats.TrendValuesDifferent,
		"find on fields: tag":    stats.TrendValuesOverlapping,
		"insert":                 stats.TrendValuesNotRecorded,
	} {
		if got := valuesState(rows[summary]); got != want {
			t.Fatalf("%s: values %q, want %q (%v)", summary, got, want, rows[summary]["query_values"])
		}
	}
	for summary, r := range rows {
		p := plainRows[summary]
		for _, k := range []string{"efficiency", "latency", "impact"} {
			if metricState(r, k) != metricState(p, k) || r[k].(map[string]interface{})["label"] != p[k].(map[string]interface{})["label"] {
				t.Fatalf("%s %s: query values must not change trend labels", summary, k)
			}
		}
		if r["comparability"] != p["comparability"] {
			t.Fatalf("%s: query values must not change comparability", summary)
		}
		if p["query_values"] != nil {
			t.Fatalf("%s: rows without stored values have no query_values field", summary)
		}
	}
	if v["query_values_notice"] == nil || v["counts"].(map[string]interface{})["query_values"] == nil {
		t.Fatalf("the view must mark that it carries literal values: %v", v["counts"])
	}

	sku := rows["find on fields: sku"]["query_values"].(map[string]interface{})
	cur := sku["current"].(map[string]interface{})
	samples := cur["samples"].([]interface{})
	first := samples[0].(map[string]interface{})
	if first["explained"] != true || !strings.Contains(first["command"].(string), `"find":"orders"`) || !strings.Contains(first["command"].(string), "secret-sku-") {
		t.Fatalf("current values hold the executed command: %v", first)
	}
	sawRetained := false
	for _, raw := range samples {
		if raw.(map[string]interface{})["explained"] == false {
			sawRetained = true
		}
	}
	if !sawRetained || sku["baseline"] == nil || sku["matching_samples"].(float64) != float64(len(samples)) {
		t.Fatalf("explained and retained samples of both runs are listed: %v", sku)
	}

	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "secret-sku-") || !strings.Contains(string(raw), "query_values_history") {
		t.Fatal("the store keeps values when the setting is on")
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("baseline file mode %v, want 0600", fi.Mode().Perm())
	}

	page := html.UnescapeString(getReportHTML(t, s))
	if !strings.Contains(page, stats.TrendQueryValuesNotice) || !strings.Contains(page, "Query values (literal, recorded with insights_trend_store_query_values on): different_values") ||
		!strings.Contains(page, "secret-changed-status-") || !strings.Contains(page, "[matches the other run]") {
		t.Fatal("HTML report must include the marked values")
	}

	code, exp, _ := trendCall(t, s, http.MethodGet, "/api/trend-baselines/export", nil, "")
	if code != http.StatusOK || exp["contains_query_values"] != true {
		t.Fatalf("export with values: %d %v", code, exp["contains_query_values"])
	}
	expRaw, _ := json.Marshal(exp)
	other, _ := efficiencyFixtureServerWith(t, "ready", fixtureOptions{store: stats.OpenTrendStore(filepath.Join(t.TempDir(), "other.json"))})
	code, res := uploadTrendFile(t, other, "lab.json", expRaw)
	if code != http.StatusOK || !strings.Contains(strings.Join(toStrings(res["warnings"]), " "), "contains literal query values") {
		t.Fatalf("import warns about values: %d %v", code, res)
	}
	_, list, _ := trendCall(t, other, http.MethodGet, "/api/trend-baselines", nil, "")
	if r := list["runs"].([]interface{})[0].(map[string]interface{}); r["query_values_stored"] != true {
		t.Fatalf("imported runs keep their values: %v", r)
	}

	if code, _, _ := trendCall(t, s, http.MethodDelete, "/api/trend-baselines", nil, ""); code != http.StatusOK {
		t.Fatal("reset failed")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("reset must delete the stored values")
	}
}

func toStrings(v interface{}) []string {
	var out []string
	for _, x := range v.([]interface{}) {
		out = append(out, x.(string))
	}
	return out
}

func TestTrendQueryValuesOffByDefaultInStoreAndExport(t *testing.T) {
	_, s, path := trendFixtureAPI(t)
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "query_values") || strings.Contains(string(raw), "secret-") {
		t.Fatal("with the setting off the store has no values")
	}
	_, exp, _ := trendCall(t, s, http.MethodGet, "/api/trend-baselines/export", nil, "")
	if _, ok := exp["contains_query_values"]; ok {
		t.Fatal("an export without values has no contains_query_values field")
	}
}

// TestUIFixturesMatchAPITrendValues writes the fixture used by the browser
// tests for query values in trends.
func TestUIFixturesMatchAPITrendValues(t *testing.T) {
	api, _, _ := trendValuesFixtureAPI(t)
	checkUIFixture(t, "insights_trend_values.json", api)
}
