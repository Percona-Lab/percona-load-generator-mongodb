package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/db"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	driverMongo "go.mongodb.org/mongo-driver/v2/mongo"
)

func trendCall(t *testing.T, s *WebServer, method, target string, body []byte, contentType string) (int, map[string]interface{}, http.Header) {
	t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	switch {
	case target == "/api/trend-baselines":
		s.handleTrendBaselines(rec, req)
	case strings.HasPrefix(target, "/api/trends/compare"):
		s.handleTrendCompare(rec, req)
	default:
		s.handleTrendBaselineItem(rec, req)
	}
	var out map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, rec.Header()
}

func storedRunIDs(t *testing.T, body map[string]interface{}) []string {
	t.Helper()
	var ids []string
	for _, r := range body["runs"].([]interface{}) {
		ids = append(ids, r.(map[string]interface{})["run_id"].(string))
	}
	return ids
}

func uploadTrendFile(t *testing.T, s *WebServer, name string, contents []byte) (int, map[string]interface{}) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, _ := w.CreateFormFile("file", name)
	_, _ = part.Write(contents)
	_ = w.Close()
	code, body, _ := trendCall(t, s, http.MethodPost, "/api/trend-baselines/import", buf.Bytes(), w.FormDataContentType())
	return code, body
}

func TestTrendRunEndpointsListRenamePinDelete(t *testing.T) {
	_, s, _ := trendFixtureAPI(t)
	code, body, _ := trendCall(t, s, http.MethodGet, "/api/trend-baselines", nil, "")
	if code != http.StatusOK {
		t.Fatalf("GET: %d %v", code, body)
	}
	ids := storedRunIDs(t, body)
	settings := body["settings"].(map[string]interface{})
	defaults := settings["defaults"].(map[string]interface{})
	if len(ids) != 2 || settings["history_runs"].(float64) != 5 || settings["max_age_days"].(float64) != 30 || settings["default_comparison"] != "previous" ||
		defaults["history_runs"].(float64) != 5 || settings["limits"].(map[string]interface{})["max_history_runs"].(float64) != 50 {
		t.Fatalf("GET must list runs newest first with settings, defaults and limits: %v %v", ids, settings)
	}
	older := ids[1]

	code, body, _ = trendCall(t, s, http.MethodPatch, "/api/trend-baselines/runs/"+older, []byte(`{"label":"before index","pinned":true}`), "application/json")
	run, _ := body["run"].(map[string]interface{})
	if code != http.StatusOK || run["label"] != "before index" || run["pinned"] != true || body["store"].(map[string]interface{})["pinned_runs"].(float64) != 1 {
		t.Fatalf("PATCH rename+pin: %d %v", code, body)
	}
	for _, tc := range []struct {
		target, body string
		want         int
	}{
		{"/api/trend-baselines/runs/" + older, `{"label":"x","drop":true}`, http.StatusBadRequest},
		{"/api/trend-baselines/runs/" + older, `{}`, http.StatusBadRequest},
		{"/api/trend-baselines/runs/" + older, `{"label":"` + strings.Repeat("a", 65) + `"}`, http.StatusBadRequest},
		{"/api/trend-baselines/runs/no-such-run", `{"pinned":true}`, http.StatusNotFound},
		{"/api/trend-baselines/runs/bad%20id!", `{"pinned":true}`, http.StatusBadRequest},
	} {
		if code, body, _ := trendCall(t, s, http.MethodPatch, tc.target, []byte(tc.body), "application/json"); code != tc.want || body["error"] == nil {
			t.Fatalf("PATCH %s %s: %d %v, want %d with an error", tc.target, tc.body, code, body, tc.want)
		}
	}
	if code, _, _ := trendCall(t, s, http.MethodPost, "/api/trend-baselines/runs/"+older, nil, ""); code != http.StatusMethodNotAllowed {
		t.Fatalf("POST on a run: %d", code)
	}

	code, body, _ = trendCall(t, s, http.MethodDelete, "/api/trend-baselines/runs/"+ids[0], nil, "")
	if code != http.StatusOK || len(storedRunIDs(t, body)) != 1 || storedRunIDs(t, body)[0] != older {
		t.Fatalf("DELETE run: %d %v", code, body)
	}
	if code, _, _ := trendCall(t, s, http.MethodDelete, "/api/trend-baselines/runs/"+ids[0], nil, ""); code != http.StatusNotFound {
		t.Fatalf("deleting twice: %d", code)
	}

	off, _ := efficiencyFixtureServerWith(t, "ready", fixtureOptions{store: stats.DisabledTrendStore()})
	if code, body, _ := trendCall(t, off, http.MethodPatch, "/api/trend-baselines/runs/r1", []byte(`{"pinned":true}`), "application/json"); code != http.StatusConflict || !strings.Contains(body["error"].(string), "disabled") {
		t.Fatalf("a disabled store cannot be edited: %d %v", code, body)
	}
}

func TestTrendExportImportEndpoints(t *testing.T) {
	_, s, _ := trendFixtureAPI(t)
	_, list, _ := trendCall(t, s, http.MethodGet, "/api/trend-baselines", nil, "")
	ids := storedRunIDs(t, list)

	code, all, hdr := trendCall(t, s, http.MethodGet, "/api/trend-baselines/export?source=lab-a", nil, "")
	if code != http.StatusOK || !strings.HasPrefix(hdr.Get("Content-Disposition"), `attachment; filename="plgm-trend-runs-`) ||
		all["format"] != stats.TrendExportFormat || all["schema_version"] != stats.TrendStoreSchemaVersion || len(all["runs"].([]interface{})) != 2 || all["source"] != "lab-a" {
		t.Fatalf("export all: %d %v", code, hdr)
	}
	code, one, _ := trendCall(t, s, http.MethodGet, "/api/trend-baselines/export?runs="+ids[1], nil, "")
	if code != http.StatusOK || len(one["runs"].([]interface{})) != 1 {
		t.Fatalf("export selected: %d", code)
	}
	if code, _, _ := trendCall(t, s, http.MethodGet, "/api/trend-baselines/export?runs=missing-run", nil, ""); code != http.StatusNotFound {
		t.Fatalf("export unknown run: %d", code)
	}
	if code, _, _ := trendCall(t, s, http.MethodGet, "/api/trend-baselines/export?runs=bad!id", nil, ""); code != http.StatusBadRequest {
		t.Fatalf("export invalid id: %d", code)
	}
	raw, _ := json.Marshal(all)
	if strings.Contains(string(raw), "secret-") || strings.Contains(string(raw), "mongodb://") {
		t.Fatal("an export must hold only redacted trend fields")
	}

	other, _ := efficiencyFixtureServerWith(t, "ready", fixtureOptions{store: stats.OpenTrendStore(filepath.Join(t.TempDir(), "other.json"))})
	code, res := uploadTrendFile(t, other, "lab-a-runs.json", raw)
	if code != http.StatusOK || len(res["added"].([]interface{})) != 2 || len(res["skipped"].([]interface{})) != 0 || res["source"] != "lab-a" {
		t.Fatalf("multipart import: %d %v", code, res)
	}
	code, res, _ = trendCall(t, other, http.MethodPost, "/api/trend-baselines/import", raw, "application/json")
	if code != http.StatusOK || len(res["added"].([]interface{})) != 0 || len(res["skipped"].([]interface{})) != 2 {
		t.Fatalf("raw re-import must skip duplicates: %d %v", code, res)
	}
	_, list, _ = trendCall(t, other, http.MethodGet, "/api/trend-baselines", nil, "")
	if r := list["runs"].([]interface{})[0].(map[string]interface{}); r["source"] != "lab-a" || r["imported_at"] == nil {
		t.Fatalf("imported runs list their source: %v", r)
	}

	for name, body := range map[string][]byte{
		"invalid json": []byte("{"),
		"unsupported":  []byte(`{"schema_version":"tb-9","runs":[],"shapes":[]}`),
		"oversized":    bytes.Repeat([]byte(" "), stats.TrendFileMaxBytes+10),
	} {
		code, res := uploadTrendFile(t, other, "bad.json", body)
		if code != http.StatusBadRequest || !strings.Contains(res["error"].(string), "trend import rejected") {
			t.Fatalf("%s: %d %v", name, code, res)
		}
	}
	if code, res := uploadTrendFile(t, other, "bad.json", []byte(`{"schema_version":"tb-9"}`)); !strings.Contains(res["error"].(string), `unsupported schema "tb-9"`) || code != http.StatusBadRequest {
		t.Fatalf("unsupported schema must be named: %v", res)
	}
	var empty bytes.Buffer
	mw := multipart.NewWriter(&empty)
	_ = mw.WriteField("x", "y")
	_ = mw.Close()
	if code, _, _ := trendCall(t, other, http.MethodPost, "/api/trend-baselines/import", empty.Bytes(), mw.FormDataContentType()); code != http.StatusBadRequest {
		t.Fatalf("a multipart import without a file: %d", code)
	}
	if code, _, _ := trendCall(t, other, http.MethodGet, "/api/trend-baselines/import", nil, ""); code != http.StatusMethodNotAllowed {
		t.Fatalf("GET import: %d", code)
	}
	if code, _, _ := trendCall(t, other, http.MethodGet, "/api/trend-baselines/nope", nil, ""); code != http.StatusNotFound {
		t.Fatalf("unknown endpoint: %d", code)
	}
}

// threeRunServer finishes the ready, after-index, and ready fixtures in turn on
// one store; the returned server holds the third (current) run.
func threeRunServer(t *testing.T) (*WebServer, map[string]interface{}) {
	t.Helper()
	steppedTrendClock(t)
	path := filepath.Join(t.TempDir(), "trend_baselines.json")
	first, _ := efficiencyFixtureServerWith(t, "ready", fixtureOptions{store: stats.OpenTrendStore(path)})
	getInsightsJSON(t, first)
	second, _ := efficiencyFixtureServerWith(t, "ready", fixtureOptions{after: true, store: stats.OpenTrendStore(path)})
	getInsightsJSON(t, second)
	third, _ := efficiencyFixtureServerWith(t, "ready", fixtureOptions{store: stats.OpenTrendStore(path)})
	return third, getInsightsJSON(t, third)
}

// TestUIFixturesMatchAPITrendHistory writes the multi-run fixture used by the
// browser tests: insights, the run list, a chosen-run view, and a matrix. Run
// ids are replaced by stable fixture-run-N names (1 = oldest).
func TestUIFixturesMatchAPITrendHistory(t *testing.T) {
	s, api := threeRunServer(t)
	_, list, _ := trendCall(t, s, http.MethodGet, "/api/trend-baselines", nil, "")
	ids := storedRunIDs(t, list)
	label, pinned := "before index", true
	if _, err := s.TrendStore.UpdateRun(ids[2], &label, &pinned, stats.DefaultTrendPolicy(), time.Now()); err != nil {
		t.Fatal(err)
	}
	_, list, _ = trendCall(t, s, http.MethodGet, "/api/trend-baselines", nil, "")
	_, view, _ := trendCall(t, s, http.MethodGet, "/api/trends/compare?baseline="+ids[2], nil, "")
	_, matrix, _ := trendCall(t, s, http.MethodGet, "/api/trends/compare?runs="+ids[1]+","+ids[2], nil, "")
	raw, _ := json.Marshal(map[string]interface{}{"insights": api, "baselines": list, "compare_run": view, "matrix": matrix})
	out := string(raw)
	for i, id := range ids {
		out = strings.ReplaceAll(out, id, fmt.Sprintf("fixture-run-%d", len(ids)-i))
	}
	var fixture map[string]interface{}
	if err := json.Unmarshal([]byte(out), &fixture); err != nil {
		t.Fatal(err)
	}
	checkUIFixture(t, "trend_history.json", fixture)
}

func TestTrendCompareEndpoint(t *testing.T) {
	third, api := threeRunServer(t)
	_, list, _ := trendCall(t, third, http.MethodGet, "/api/trend-baselines", nil, "")
	ids := storedRunIDs(t, list)
	if len(ids) != 3 {
		t.Fatalf("three runs stored: %v", ids)
	}
	curID := api["trends"].(map[string]interface{})["current_run"].(map[string]interface{})["run_id"].(string)
	if curID != ids[0] {
		t.Fatalf("the newest run is the current run")
	}
	_, rows := trendRows(t, api)
	if got := metricState(rows["find on fields: sku"], "efficiency"); got != stats.TrendRegressed {
		t.Fatalf("by default run 3 (no index) compares with run 2 (index): %s %v ids %v", got, rows["find on fields: sku"], ids)
	}

	code, body, _ := trendCall(t, third, http.MethodGet, "/api/trends/compare?baseline="+ids[2], nil, "")
	if code != http.StatusOK {
		t.Fatalf("compare with run 1: %d %v", code, body)
	}
	_, rows = trendRows(t, map[string]interface{}{"trends": body["view"]})
	sku := rows["find on fields: sku"]
	cmp := body["view"].(map[string]interface{})["comparison"].(map[string]interface{})
	if cmp["mode"] != stats.TrendCompareRun || cmp["baseline_run_id"] != ids[2] || metricState(sku, "efficiency") != stats.TrendNoMaterialChange ||
		sku["baseline"].(map[string]interface{})["efficiency_run_id"] != ids[2] {
		t.Fatalf("a chosen run is the baseline of every shape: %v %v", cmp, sku["efficiency"])
	}

	code, body, _ = trendCall(t, third, http.MethodGet, "/api/trends/compare?runs="+ids[2]+","+ids[1], nil, "")
	if code != http.StatusOK {
		t.Fatalf("matrix: %d %v", code, body)
	}
	m := body["matrix"].(map[string]interface{})
	cols := m["columns"].([]interface{})
	if len(cols) != 3 || m["current_run_id"] != curID || cols[0].(map[string]interface{})["run_id"] != curID || cols[1].(map[string]interface{})["run_id"] != ids[1] {
		t.Fatalf("matrix columns: current first, then newest first: %v", cols)
	}

	for _, tc := range []struct {
		q    string
		want int
	}{
		{"baseline=missing-run", http.StatusNotFound},
		{"baseline=" + curID, http.StatusNotFound},
		{"baseline=bad!id", http.StatusNotFound},
		{"runs=missing-run", http.StatusNotFound},
		{"runs=" + curID, http.StatusBadRequest},
	} {
		if code, body, _ := trendCall(t, third, http.MethodGet, "/api/trends/compare?"+tc.q, nil, ""); code != tc.want || body["error"] == nil {
			t.Fatalf("%s: %d %v, want %d", tc.q, code, body, tc.want)
		}
	}
	fresh := NewServer(&config.AppConfig{})
	if code, _, _ := trendCall(t, fresh, http.MethodGet, "/api/trends/compare", nil, ""); code != http.StatusConflict {
		t.Fatalf("compare before any run: %d", code)
	}
}

func TestNewTrendRunIDIsUniqueAndValid(t *testing.T) {
	seen := map[string]bool{}
	now := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 2000; i++ {
		id := newTrendRunID(now, 1)
		if !stats.ValidTrendRunID(id) || !strings.HasPrefix(id, "20261015T090000Z-1-") || seen[id] {
			t.Fatalf("run id %q must be valid, timestamped, and unique even with the same time and counter", id)
		}
		seen[id] = true
	}
}

func TestApplyTrendForm(t *testing.T) {
	cfg := &config.AppConfig{TrendHistoryRuns: 9, TrendRunLabel: "yaml"}
	config.NormalizeTrendSettings(cfg)
	if err := applyTrendForm(cfg, map[string][]string{"trend_history_runs": {""}, "trend_max_age_days": {" "}}); err != nil || cfg.TrendHistoryRuns != 9 || *cfg.TrendMaxAgeDays != 30 {
		t.Fatalf("empty numeric fields keep the loaded values: %v %+v", err, cfg)
	}
	if err := applyTrendForm(cfg, map[string][]string{
		"trend_history_runs": {"12"}, "trend_max_age_days": {"0"}, "trend_default_comparison": {"Pinned"},
		"trend_run_label": {"  after index "}, "trend_baseline_store": {" /tmp/shared.json "},
	}); err != nil {
		t.Fatal(err)
	}
	if cfg.TrendHistoryRuns != 12 || *cfg.TrendMaxAgeDays != 0 || cfg.TrendDefaultComparison != "pinned" || cfg.TrendRunLabel != "after index" || cfg.TrendBaselineStore != "/tmp/shared.json" {
		t.Fatalf("form overrides: %+v", cfg)
	}
	if err := applyTrendForm(cfg, map[string][]string{"trend_run_label": {""}, "trend_baseline_store": {""}}); err != nil || cfg.TrendRunLabel != "" || cfg.TrendBaselineStore != "" {
		t.Fatalf("empty label and store select the defaults: %v %+v", err, cfg)
	}
	for field, v := range map[string]string{
		"trend_history_runs": "0", "trend_max_age_days": "-1", "trend_default_comparison": "latest",
		"trend_run_label": strings.Repeat("x", 65), "trend_baseline_store": "/a\n/b",
	} {
		c := &config.AppConfig{}
		if err := applyTrendForm(c, map[string][]string{field: {v}}); err == nil || !strings.Contains(err.Error(), field) {
			t.Fatalf("%s=%q must be rejected naming the field: %v", field, v, err)
		}
	}
	for _, v := range []string{"51", "1.5", "x"} {
		if err := applyTrendForm(&config.AppConfig{}, map[string][]string{"trend_history_runs": {v}}); err == nil {
			t.Fatalf("trend_history_runs=%q must be rejected", v)
		}
	}
	if err := applyTrendForm(&config.AppConfig{}, map[string][]string{"trend_max_age_days": {"3651"}}); err == nil {
		t.Fatal("trend_max_age_days above the maximum must be rejected")
	}
}

func TestHandleStartAppliesTrendSettingsAndStore(t *testing.T) {
	startWith := func(t *testing.T, s *WebServer, fields map[string]string) (*httptest.ResponseRecorder, chan *config.AppConfig) {
		got := make(chan *config.AppConfig, 1)
		finished := make(chan struct{})
		withWebUISeams(t, webuiSeams{
			connect:           func(context.Context, *config.AppConfig, string) (*db.Connection, error) { return &db.Connection{}, nil },
			disconnect:        func(*db.Connection, context.Context) { close(finished) },
			createCollections: func(context.Context, *driverMongo.Database, *config.CollectionsFile, bool) error { return nil },
			createIndexes:     func(context.Context, *driverMongo.Database, *config.CollectionsFile) error { return nil },
			insertRandomDocs: func(context.Context, *driverMongo.Database, config.CollectionDefinition, int, *config.AppConfig) error {
				return nil
			},
			runWorkload: func(_ context.Context, _ *driverMongo.Database, _ []config.CollectionDefinition, _ []config.QueryDefinition, cfg *config.AppConfig, _ ...*stats.Collector) error {
				got <- cfg
				return nil
			},
		})
		waitForRunsOnCleanup(t, s)
		fields["documents_count"] = "1"
		req, rec := newMultipartRequest(t, fields, map[string]string{
			"collections_file": `{"collections":[{"database":"db","collection":"docs","fields":{}}]}`,
			"queries_file":     `{"queries":[{"name":"find_docs","collection":"docs","operation":"find","filter":{}}]}`,
		})
		s.handleStart(rec, req)
		if rec.Code == http.StatusOK {
			select {
			case <-finished:
			case <-time.After(5 * time.Second):
				t.Fatal("run did not finish")
			}
		}
		return rec, got
	}

	t.Run("applies_form_and_switches_store", func(t *testing.T) {
		s := NewServer(&config.AppConfig{Duration: "1s", Iterations: 1, TrendHistoryRuns: 7})
		store := filepath.Join(t.TempDir(), "team.json")
		rec, got := startWith(t, s, map[string]string{
			"trend_history_runs": "3", "trend_max_age_days": "0", "trend_default_comparison": "pinned",
			"trend_run_label": "after index", "trend_baseline_store": store,
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
		}
		cfg := <-got
		if cfg.TrendHistoryRuns != 3 || *cfg.TrendMaxAgeDays != 0 || cfg.TrendDefaultComparison != "pinned" || cfg.TrendRunLabel != "after index" {
			t.Fatalf("run config: %+v", cfg)
		}
		s.mu.Lock()
		path := s.TrendStore.Path()
		s.mu.Unlock()
		if path != store {
			t.Fatalf("the run must use the chosen store: %q", path)
		}
		_, body, _ := trendCall(t, s, http.MethodGet, "/api/trend-baselines", nil, "")
		if st := body["settings"].(map[string]interface{}); st["history_runs"].(float64) != 3 || st["store_path"] != store {
			t.Fatalf("settings must reflect the last run: %v", st)
		}
	})

	t.Run("off_disables_store", func(t *testing.T) {
		s := NewServer(&config.AppConfig{Duration: "1s", Iterations: 1})
		rec, _ := startWith(t, s, map[string]string{"trend_baseline_store": "off"})
		if rec.Code != http.StatusOK {
			t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
		}
		s.mu.Lock()
		disabled := s.TrendStore.Disabled()
		s.mu.Unlock()
		if !disabled {
			t.Fatal("trend_baseline_store=off must disable the store")
		}
	})

	t.Run("query_values_checkbox", func(t *testing.T) {
		for value, want := range map[string]bool{"on": true, "true": true, "": false} {
			s := NewServer(&config.AppConfig{Duration: "1s", Iterations: 1, InsightsTrendStoreQueryValues: true})
			fields := map[string]string{}
			if value != "" {
				fields["insights_trend_store_query_values"] = value
			}
			rec, got := startWith(t, s, fields)
			if rec.Code != http.StatusOK {
				t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
			}
			if cfg := <-got; cfg.InsightsTrendStoreQueryValues != want {
				t.Fatalf("insights_trend_store_query_values=%q: got %v, want %v", value, cfg.InsightsTrendStoreQueryValues, want)
			}
		}
	})

	t.Run("invalid_value_aborts", func(t *testing.T) {
		s := NewServer(&config.AppConfig{Duration: "1s", Iterations: 1})
		rec, _ := startWith(t, s, map[string]string{"trend_history_runs": "99"})
		assertStartErrorResponse(t, rec, http.StatusBadRequest, "trend_history_runs")
		if isRunning(s) {
			t.Fatal("an invalid trend setting must not start a run")
		}
	})
}
