package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
)

var updateUIFixtures = flag.Bool("update-ui-fixtures", false, "rewrite internal/webui/testdata/ui fixtures from the current API")

type efficiencyFixtureCalls struct {
	explainRuns int
	indexLists  int
}

func counters(docs, keys, returned int64) *stats.ExplainMetrics {
	return &stats.ExplainMetrics{
		Layout:       "plan",
		DocsExamined: &stats.ExplainCounter{Value: docs, Source: "executionStats.totalDocsExamined"},
		KeysExamined: &stats.ExplainCounter{Value: keys, Source: "executionStats.totalKeysExamined"},
		Returned:     &stats.ExplainCounter{Value: returned, Source: "executionStats.nReturned"},
	}
}

// fixtureOptions vary the stubbed run. after models a later run in which an
// index on sku was added and the status shape examines more documents.
type fixtureOptions struct {
	after bool
	store *stats.TrendStore
}

// efficiencyEvidence returns stubbed explain output per shape key.
func efficiencyEvidence(j explainJob, verbosity string, after bool) explainEvidence {
	d := &stats.ExplainDiagnostics{
		ReplayDB: j.sample.Database, ReplayCollection: j.sample.Collection, Verbosity: verbosity,
		ExecutionStatsAvailable: true, ExecutionTimeMillis: 3, ServerVersion: "8.0.4",
	}
	switch key := j.sample.ShapeKey; {
	case after && key == "find|sku":
		d.IndexScanDetected, d.FetchDetected = true, true
		d.WinningPlanSummary, d.IndexesUsed = "FETCH <- IXSCAN", []string{"sku_1"}
		d.DocsExamined, d.KeysExamined, d.NReturned = 2, 2, 2
		d.Metrics = counters(2, 2, 2)
	case after && key == "find|status":
		d.IndexScanDetected, d.FetchDetected = true, true
		d.WinningPlanSummary, d.IndexesUsed = "FETCH <- IXSCAN", []string{"status_1"}
		d.DocsExamined, d.KeysExamined, d.NReturned = 10000, 10000, 100
		d.Metrics = counters(10000, 10000, 100)
	case key == "find|email":
		d.IndexScanDetected, d.FetchDetected, d.BlockingSortDetected = true, true, true
		d.WinningPlanSummary, d.IndexesUsed = "SORT <- FETCH <- IXSCAN", []string{"email_1"}
		d.DocsExamined, d.KeysExamined, d.NReturned = 1, 1, 1
		d.Metrics = counters(1, 1, 1)
		// The slower sample sorted more data; neither spilled.
		sorted := int64(172017)
		if math.Mod(j.sample.DurationMs, 1) > 0.15 {
			sorted = 5540000
		}
		path := "executionStats.executionStages"
		d.Metrics.SortSpill = &stats.SortSpillMetrics{Status: stats.SortSpillMeasured, Source: "explain executionStats", Stages: []stats.StageResourceMetric{{
			Path: path, Stage: "SORT", Sort: true, Engine: "classic", Namespace: "shop.orders", UsedDisk: new(bool),
			SortBytesProcessed: &stats.ExplainCounter{Value: sorted, Source: path + ".totalDataSizeSorted"},
			Spills:             &stats.ExplainCounter{Value: 0, Source: path + ".spills"},
			SpillStorageBytes:  &stats.ExplainCounter{Value: 0, Source: path + ".spilledDataStorageSize"},
			MemoryLimitBytes:   &stats.ExplainCounter{Value: 104857600, Source: path + ".memLimit"},
			TimeMsEstimate:     &stats.ExplainCounter{Value: 0, Source: path + ".executionTimeMillisEstimate"},
		}}}
	case key == "find|status":
		d.IndexScanDetected, d.FetchDetected = true, true
		d.WinningPlanSummary, d.IndexesUsed = "FETCH <- IXSCAN", []string{"status_1"}
		d.DocsExamined, d.KeysExamined, d.NReturned = 1000, 1000, 100
		d.Metrics = counters(1000, 1000, 100)
	case key == "find|region":
		d.ExecutionStatsAvailable, d.ExecutionTimeMillis, d.Verbosity = false, 0, "queryPlanner"
		d.CollectionScanDetected, d.WinningPlanSummary = true, "COLLSCAN"
		d.Metrics = &stats.ExplainMetrics{Layout: "planner_only"}
	case key == "aggregate|lookup":
		d.IndexScanDetected, d.WinningPlanSummary = true, "IXSCAN -> $lookup"
		d.Metrics = &stats.ExplainMetrics{
			Layout:       "pipeline",
			DocsExamined: &stats.ExplainCounter{Value: 500, Source: "executionStats.totalDocsExamined"},
			KeysExamined: &stats.ExplainCounter{Value: 500, Source: "executionStats.totalKeysExamined"},
			Returned:     &stats.ExplainCounter{Value: 10, Source: "stages.$lookup.nReturned"},
			Notes:        []string{"lookup_work_not_scored"},
			Lookup: &stats.LookupMetrics{Stages: 1,
				DocsExamined:    &stats.ExplainCounter{Value: 10, Source: "stages[1].totalDocsExamined"},
				KeysExamined:    &stats.ExplainCounter{Value: 10, Source: "stages[1].totalKeysExamined"},
				CollectionScans: &stats.ExplainCounter{Value: 0, Source: "stages[1].collectionScans"}},
		}
	case key == "find|tag":
		return explainEvidence{status: "timed_out", reason: "explain_timeout_after_1000ms", attempts: 3}
	default:
		d.CollectionScanDetected, d.WinningPlanSummary = true, "COLLSCAN"
		d.DocsExamined, d.NReturned = 50000, 2
		d.Metrics = counters(50000, 0, 2)
	}
	stats.DescribeExplainSample(d, j.sample.Operation)
	return explainEvidence{status: "explained", reason: d.Diagnosis, attempts: 1, diag: d}
}

// efficiencyFixtureServer builds a finished run whose explains and index
// inventory are stubbed. mode is ready, failed, disabled, or empty.
func efficiencyFixtureServer(t *testing.T, mode string) (*WebServer, *efficiencyFixtureCalls) {
	t.Helper()
	return efficiencyFixtureServerWith(t, mode, fixtureOptions{})
}

func efficiencyFixtureServerWith(t *testing.T, mode string, opt fixtureOptions) (*WebServer, *efficiencyFixtureCalls) {
	t.Helper()
	if opt.store == nil {
		opt.store = stats.OpenTrendStore(filepath.Join(t.TempDir(), "trend_baselines.json"))
	}
	calls := &efficiencyFixtureCalls{}
	origRun, origList := runExplainJobsFn, listNamespaceIndexesFn
	t.Cleanup(func() { runExplainJobsFn, listNamespaceIndexesFn = origRun, origList })
	runExplainJobsFn = func(ctx context.Context, jobs []explainJob, workers int, cfg *config.AppConfig, maxTimeMs int, verbosity string, retries int, backoffMs int) []explainEvidence {
		calls.explainRuns++
		out := make([]explainEvidence, len(jobs))
		for i, j := range jobs {
			if mode == "failed" {
				out[i] = explainEvidence{status: "execution_failed", reason: "command_failed_unauthorized", attempts: 1}
				continue
			}
			out[i] = efficiencyEvidence(j, verbosity, opt.after)
		}
		return out
	}
	listNamespaceIndexesFn = func(ctx context.Context, cfg *config.AppConfig, namespaces []string) stats.IndexInventoryView {
		calls.indexLists++
		view := stats.CollectIndexInventory(ctx, namespaces, func(_ context.Context, ns string) stats.NamespaceIndexInventory {
			idx := []stats.DeployedIndex{{Name: "_id_", Type: "btree", Keys: []stats.IndexKeySpec{{Field: "_id", Direction: 1, Kind: "asc"}}}}
			if opt.after {
				idx = append(idx, stats.DeployedIndex{Name: "sku_1", Type: "btree", Keys: []stats.IndexKeySpec{{Field: "sku", Direction: 1, Kind: "asc"}}})
			}
			return stats.NamespaceIndexInventory{Status: "fetched", Indexes: idx}
		})
		view.FetchedAt = ""
		for i := range view.Namespaces {
			view.Namespaces[i].FetchedAt, view.Namespaces[i].ElapsedMS = "", 0
		}
		return view
	}

	c := stats.NewCollector()
	c.ConfigureInsights(&config.AppConfig{
		InsightsEnabled: true, InsightsSamplingRate: 1, InsightsSlowThresholdMs: 200, InsightsMaxEvents: 5000, InsightsMaxGroups: 50,
		InsightsExplainEnabled: mode != "disabled", InsightsExplainTopN: 10, InsightsExplainSeverityMode: "high_and_low",
		InsightsExplainEfficiencyMode: config.ExplainEfficiencyModeDefault,
		InsightsLatencyTargetAvgMs:    500, InsightsLatencyTargetP95Ms: 50, InsightsLatencyTargetP99Ms: 100,
	})
	find := func(field string, ms float64, n int) {
		for i := 0; i < n; i++ {
			q := &stats.ExecutableQuery{Database: "shop", Filter: map[string]interface{}{field: fmt.Sprintf("secret-%s-%d", field, i)}}
			c.RecordOperationEvent("find", "shop", "orders", "find|"+field, "find on fields: "+field, []string{field}, time.Duration((ms+float64(i%3)*0.1)*float64(time.Millisecond)), true, 1, q, "by "+field, "unit_test", "w", "q.json", "q-"+field, "", "", 0)
		}
	}
	emailMs, skuMs, statusMs, concurrency := 900.0, 2.0, 5.0, 4
	if opt.after {
		emailMs, skuMs, statusMs, concurrency = 1200, 0.5, 8, 8
	}
	if mode != "empty" {
		find("email", emailMs, 12)
		find("sku", skuMs, 200)
		find("status", statusMs, 100)
		find("region", 2, 80)
		find("tag", 4, 50)
		for i := 0; i < 60; i++ {
			q := &stats.ExecutableQuery{Database: "shop", Pipeline: []interface{}{
				map[string]interface{}{"$match": map[string]interface{}{"customer": fmt.Sprintf("secret-c-%d", i)}},
				map[string]interface{}{"$lookup": map[string]interface{}{"from": "customers", "localField": "customer", "foreignField": "_id", "as": "c"}},
			}}
			c.RecordOperationEvent("aggregate", "shop", "orders", "aggregate|lookup", "aggregate $match customer, $lookup customers", []string{"customer"}, 3*time.Millisecond, true, 1, q, "lookup", "unit_test", "w", "q.json", "q-lookup", "", "", 0)
		}
		for f := 0; f < 8; f++ {
			find(fmt.Sprintf("f%d", f), 3, 10)
		}
		for i := 0; i < 30; i++ {
			c.RecordOperationEvent("find", "shop", "orders", "find|nosample", "find on fields: legacy", []string{"legacy"}, 3*time.Millisecond, true, 1, nil, "legacy", "unit_test", "w", "q.json", "q-legacy", "", "", 0)
			c.RecordOperationEvent("insert", "shop", "orders", "insert", "insert", nil, time.Millisecond, true, 1, nil, "insert", "unit_test", "w", "q.json", "q-insert", "", "", 0)
		}
	}
	s := NewServer(&config.AppConfig{Duration: "1s", Concurrency: concurrency})
	s.TrendStore = opt.store
	s.CurrentStats = c
	return s, calls
}

func efficiencyViewFromAPI(t *testing.T, api map[string]interface{}) (map[string]interface{}, []map[string]interface{}) {
	t.Helper()
	v, ok := api["efficiency_view"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected efficiency_view in /api/insights, got keys %v", reflect.ValueOf(api).MapKeys())
	}
	raw, _ := v["rows"].([]interface{})
	rows := make([]map[string]interface{}, len(raw))
	for i, r := range raw {
		rows[i] = r.(map[string]interface{})
	}
	return v, rows
}

func rowBySummary(t *testing.T, rows []map[string]interface{}, summary string) map[string]interface{} {
	t.Helper()
	for _, r := range rows {
		if r["shape_summary"] == summary {
			return r
		}
	}
	t.Fatalf("no efficiency row %q", summary)
	return nil
}

func dig(m map[string]interface{}, path ...string) interface{} {
	var cur interface{} = m
	for _, k := range path {
		next, ok := cur.(map[string]interface{})
		if !ok {
			return nil
		}
		cur = next[k]
	}
	return cur
}

func TestEfficiencyViewAPIReportExportConsistency(t *testing.T) {
	s, calls := efficiencyFixtureServer(t, "ready")
	api := getInsightsJSON(t, s)
	v, rows := efficiencyViewFromAPI(t, api)

	cov := api["explain_coverage"].(map[string]interface{})
	covShapes := cov["shapes"].([]interface{})
	if len(rows) != len(covShapes) || v["shapes_total"] != float64(len(covShapes)) {
		t.Fatalf("every tracked shape must have a row: rows=%d coverage=%d", len(rows), len(covShapes))
	}
	byID := map[string]map[string]interface{}{}
	for _, raw := range covShapes {
		cs := raw.(map[string]interface{})
		byID[cs["shape_id"].(string)] = cs
	}
	for i, r := range rows {
		if r["efficiency_order"] != float64(i+1) {
			t.Fatalf("rows must be delivered in efficiency order: %v at %d", r["efficiency_order"], i)
		}
		eff := byID[r["shape_id"].(string)]["efficiency"].(map[string]interface{})
		if !reflect.DeepEqual(r["score"], eff["score"]) || r["status"] != eff["status"] || r["band"] != eff["band"] {
			t.Fatalf("row must reuse the cached scan efficiency result, row=%v eff=%v", r, eff)
		}
		if _, has := r["score"]; !has {
			if r["score_label"] != "Unassessed" && r["score_label"] != "Not applicable" {
				t.Fatalf("missing score must not render as a number: %v", r)
			}
		}
	}

	fast := rowBySummary(t, rows, "find on fields: sku")
	if fast["score"] != float64(0) || fast["band"] != "inefficient" || fast["below_slow_threshold"] != true ||
		fast["severity"] != "below_threshold" || fast["coverage"] != stats.EffCoverageAssessed {
		t.Fatalf("fast inefficient shape must be visible and rated: %v", fast)
	}
	if rows[0]["score"] != float64(0) {
		t.Fatalf("worst efficiency must sort first, got %v", rows[0])
	}
	if q, _ := fast["query"].(string); q == "" || strings.Contains(q, "secret-") {
		t.Fatalf("detail must carry a redacted query, got %q", q)
	}
	slow := rowBySummary(t, rows, "find on fields: email")
	if slow["score"] != float64(100) || dig(slow, "execution_overhead", "status") != stats.OverheadInMemorySort ||
		dig(slow, "execution_overhead", "spill", "status") != stats.SignalNotObserved {
		t.Fatalf("a sort must be reported as execution overhead beside a 100 score: %v", slow)
	}
	if dig(slow, "latency", "rating", "status") != stats.LatencyAboveTarget || dig(slow, "latency", "percentile_status") != "insufficient_samples" {
		t.Fatalf("12 samples cannot give p95, but the measured avg still misses its target: %v", slow["latency"])
	}
	if dig(fast, "latency", "rating", "status") != stats.LatencyWithinTarget || dig(fast, "execution_overhead", "status") != stats.OverheadNoneObserved {
		t.Fatalf("a fast unsorted shape is within target with no overhead, independent of its scan score: %v", fast)
	}
	if share, _ := dig(fast, "workload_impact", "share_of_operations").(float64); share <= 0 || dig(fast, "workload_impact", "operations_per_sec") != nil {
		t.Fatalf("impact shares must come from observed totals and the rate needs a workload window: %v", fast["workload_impact"])
	}
	lookup := rowBySummary(t, rows, "aggregate $match customer, $lookup customers")
	if lookup["coverage"] != stats.EffCoveragePartial || lookup["score_is_upper_bound"] != true ||
		!strings.Contains(fmt.Sprint(lookup["limitations"]), "$lookup") || !strings.HasPrefix(lookup["score_label"].(string), "≤ ") {
		t.Fatalf("partial $lookup must be an upper bound with its limitation: %v", lookup)
	}
	if dig(lookup, "other_measurements", "lookup", "status") != stats.MeasureMeasured || dig(lookup, "other_measurements", "locks", "status") != stats.MeasureNotMeasured {
		t.Fatalf("$lookup work is measured separately; locks are not collected: %v", lookup["other_measurements"])
	}
	for _, r := range rows {
		explained := r["coverage"] == stats.EffCoverageAssessed || r["coverage"] == stats.EffCoveragePartial
		if !explained && dig(r, "execution_overhead", "status") == stats.OverheadNoneObserved {
			t.Fatalf("missing explain evidence must not report a clean overhead result: %v", r)
		}
	}
	if vers := v["rating_versions"].(map[string]interface{}); vers["scan_efficiency"] != stats.EfficiencyVersion ||
		vers["latency"] != stats.LatencyRatingVersion || vers["execution_overhead"] != stats.OverheadRatingVersion {
		t.Fatalf("each rating must carry its own version: %v", vers)
	}
	want := map[string]string{
		"find on fields: region": stats.EffCoveragePartial,
		"find on fields: tag":    stats.EffCoverageFailed,
		"find on fields: legacy": stats.EffCoverageUnavailable,
		"insert":                 stats.EffCoverageNotApplicable,
	}
	for summary, coverage := range want {
		if r := rowBySummary(t, rows, summary); r["coverage"] != coverage {
			t.Fatalf("%s: coverage %v, want %s", summary, r["coverage"], coverage)
		}
	}
	counts := v["coverage_counts"].(map[string]interface{})
	if counts[stats.EffCoverageBudgetExcluded] == nil || counts[stats.EffCoverageBudgetExcluded].(float64) < 1 {
		t.Fatalf("expected budget-excluded shapes, got %v", counts)
	}
	if paths := v["unrecorded"].(map[string]interface{})["paths"].([]interface{}); len(paths) != 2 {
		t.Fatalf("unrecorded paths must be disclosed: %v", v["unrecorded"])
	}
	cols, _ := v["columns"].([]interface{})
	if _, has := v["scope_note"]; has || len(cols) != 5 {
		t.Fatalf("each column needs its own explanation instead of one blanket note: %v", v["columns"])
	}
	recs := api["recommendations"].([]interface{})
	if len(recs) <= 8 {
		t.Fatalf("fixture needs more than 8 recommendations for pagination, got %d", len(recs))
	}
	if ranks, _ := fast["recommendation_ranks"].([]interface{}); len(ranks) == 0 {
		t.Fatalf("fast inefficient shape should link its index advice: %v", fast)
	}

	body, _ := json.Marshal(api)
	if strings.Contains(string(body), "secret-") {
		t.Fatalf("API leaked a query literal")
	}

	// Download Summary embeds the /api/insights payload unchanged.
	exported, _ := json.Marshal(map[string]interface{}{"insights": api})
	var rt map[string]interface{}
	if err := json.Unmarshal(exported, &rt); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rt["insights"].(map[string]interface{})["efficiency_view"], v) {
		t.Fatalf("export efficiency_view differs from /api/insights")
	}

	page := html.UnescapeString(getReportHTML(t, s))
	_ = getInsightsJSON(t, s)
	_ = getReportHTML(t, s)
	if calls.explainRuns != 1 || calls.indexLists != 1 {
		t.Fatalf("API, report, and repeated renders must reuse cached analysis: explains=%d listIndexes=%d", calls.explainRuns, calls.indexLists)
	}
	if strings.Contains(page, "secret-") {
		t.Fatalf("HTML report leaked a query literal")
	}
	for _, wantText := range []string{"Query Assessment (status: ready", "Latency targets: avg ≤ 500 ms, p95 ≤ 50 ms, p99 ≤ 100 ms",
		"Not recorded: failed_transactions", "Validation:", "Index candidate (ordered keys)"} {
		if !strings.Contains(page, wantText) {
			t.Fatalf("HTML report missing %q", wantText)
		}
	}
	if strings.Contains(strings.ToLower(page), "qe-1") || strings.Contains(strings.ToLower(page), "provenance") {
		t.Fatalf("HTML report must not use the internal version name or provenance")
	}
	section := page[strings.Index(page, `id="efficiency"`):]
	for _, raw := range cols {
		c := raw.(map[string]interface{})
		if want := fmt.Sprintf("<dt class=\"k\">%s</dt><dd>%s</dd>", c["title"], c["explanation"]); !strings.Contains(section, want) {
			t.Fatalf("HTML report missing the %v column explanation", c["key"])
		}
	}
	pos := 0
	for _, r := range rows {
		cells := []string{
			fmt.Sprintf("<td>%s</td><td class=\"num\"><strong>%s</strong></td><td>", r["shape_summary"], r["score_label"]),
			fmt.Sprintf("<td>%s<br><span class=\"muted\">%s", dig(r, "latency", "summary"), dig(r, "latency", "rating", "label")),
			fmt.Sprintf("%s</td><td>%s</td><td>$lookup: %s<br>locks: %s<br>network: %s</td>", dig(r, "workload_impact", "summary"),
				overheadCellHTML(r), dig(r, "other_measurements", "lookup", "label"),
				dig(r, "other_measurements", "locks", "label"), dig(r, "other_measurements", "network", "label")),
		}
		for _, cell := range cells {
			idx := strings.Index(section[pos:], cell)
			if idx < 0 {
				t.Fatalf("HTML report row %v (%q) missing or out of API order", r["efficiency_order"], cell)
			}
			pos += idx + len(cell)
		}
	}
	measuredRows := 0
	for _, r := range rows {
		details, _ := dig(r, "execution_overhead", "measurements", "details").([]interface{})
		if len(details) == 0 {
			continue
		}
		head := fmt.Sprintf("Sort and spill measurements (%s)", dig(r, "execution_overhead", "measurements", "status"))
		if !strings.Contains(section, head) {
			t.Fatalf("HTML report missing %q", head)
		}
		for _, d := range details {
			if !strings.Contains(section, "<li>"+d.(string)+"</li>") {
				t.Fatalf("HTML report missing measurement line %q", d)
			}
		}
		if s, _ := dig(r, "execution_overhead", "measurements", "summary").(string); strings.Contains(s, "differs by sample") {
			measuredRows++
		}
	}
	if measuredRows == 0 {
		t.Fatalf("fixture needs a shape whose samples report different sort values")
	}
}

// overheadCellHTML is the report's Execution overhead cell for an API row.
func overheadCellHTML(r map[string]interface{}) string {
	cell := fmt.Sprint(dig(r, "execution_overhead", "label"))
	if sum, _ := dig(r, "execution_overhead", "measurements", "summary").(string); sum != "" {
		cell += `<br><span class="muted">` + sum + `</span>`
	}
	return cell
}

func TestEfficiencyViewNotReadyStates(t *testing.T) {
	cases := []struct {
		mode, status string
		rows         int
	}{
		{"disabled", "disabled", -1},
		{"empty", "not_run", 0},
		{"failed", "ready", -1},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			s, _ := efficiencyFixtureServer(t, tc.mode)
			api := getInsightsJSON(t, s)
			v, rows := efficiencyViewFromAPI(t, api)
			if v["status"] != tc.status {
				t.Fatalf("status %v, want %s", v["status"], tc.status)
			}
			if tc.rows >= 0 && len(rows) != tc.rows {
				t.Fatalf("rows %d, want %d", len(rows), tc.rows)
			}
			for _, r := range rows {
				if _, has := r["score"]; has {
					t.Fatalf("%s: no row may have a score: %v", tc.mode, r)
				}
				if r["score_label"] != "Unassessed" && r["score_label"] != "Not applicable" {
					t.Fatalf("%s: label %v", tc.mode, r["score_label"])
				}
			}
			if tc.mode == "failed" {
				if c := v["coverage_counts"].(map[string]interface{}); c[stats.EffCoverageFailed] == nil {
					t.Fatalf("failed explains must be disclosed: %v", c)
				}
			}
			if tc.mode == "disabled" && (len(rows) == 0 || rows[0]["coverage_reason"] != "explain_disabled") {
				t.Fatalf("disabled explain must list impact shapes as unassessed: %v", rows)
			}
			page := html.UnescapeString(getReportHTML(t, s))
			if !strings.Contains(page, "Query Assessment (status: "+tc.status) {
				t.Fatalf("HTML report must show the efficiency status %s", tc.status)
			}
			if hasNotice := strings.Contains(page, stats.ExplainDisabledNotice); hasNotice != (tc.mode == "disabled") || (v["notice"] != nil) != (tc.mode == "disabled") {
				t.Fatalf("%s: explain-off notice in report %v, in API %v", tc.mode, hasNotice, v["notice"])
			}
		})
	}
}

// TestUIFixturesMatchAPI keeps the browser-test fixtures equal to the real API
// output; regenerate with -update-ui-fixtures.
func TestUIFixturesMatchAPI(t *testing.T) {
	for _, mode := range []string{"ready", "failed", "disabled", "empty", "trends", "acceptance"} {
		var api map[string]interface{}
		switch mode {
		case "trends":
			api, _, _ = trendFixtureAPI(t)
		case "acceptance":
			api = getInsightsJSON(t, acceptanceServer(t).server)
		default:
			s, _ := efficiencyFixtureServer(t, mode)
			api = getInsightsJSON(t, s)
		}
		checkUIFixture(t, "insights_"+mode+".json", api)
	}
}

func checkUIFixture(t *testing.T, name string, api map[string]interface{}) {
	t.Helper()
	normalizeVolatile(api)
	got, err := json.MarshalIndent(api, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", "ui", name)
	if *updateUIFixtures {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s (run go test ./internal/webui -run TestUIFixturesMatchAPI -update-ui-fixtures): %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is stale; regenerate with -update-ui-fixtures", path)
	}
}

func normalizeVolatile(v interface{}) {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, val := range t {
			if s, ok := val.(string); ok && strings.HasPrefix(s, "fixture-run-") {
				continue
			}
			switch k {
			case "generated_at", "fetched_at", "timestamp_ms", "recorded_at", "run_id", "efficiency_run_id", "workload_run_id",
				"efficiency_recorded_at", "workload_recorded_at", "oldest_recorded_at", "newest_recorded_at", "efficiency_age", "workload_age",
				"baseline_run_id", "imported_at", "age":
				t[k] = "normalized"
			case "elapsed_ms", "efficiency_age_seconds", "workload_age_seconds", "age_seconds":
				t[k] = 0
			case "path", "store_path":
				if s, ok := val.(string); ok && strings.HasSuffix(s, ".json") {
					t[k] = "normalized"
				}
			case "label", "baseline_label", "efficiency_run_label", "workload_run_label":
				if s, ok := val.(string); ok && strings.HasPrefix(s, "run-2") {
					t[k] = "run-normalized"
				}
			default:
				normalizeVolatile(val)
			}
		}
	case []interface{}:
		for _, val := range t {
			normalizeVolatile(val)
		}
	}
}
