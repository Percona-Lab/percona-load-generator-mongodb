package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// acceptanceCase is one principal user scenario. Client latencies are
// synthetic; server evidence is the explain output recorded from a standalone
// MongoDB 8.0.4 (testdata/explain_mongod_8.0.4.json), parsed by the same code
// as a live explain.
type acceptanceCase struct {
	name, op, key, summary string
	query                  *stats.ExecutableQuery
	ms                     float64
	n                      int
	explain                string // recorded explain; "" with timeout for a timed-out explain
	timeout                bool

	coverage, band, label, observation string
	advice                             bool
}

var acceptanceCases = []acceptanceCase{
	{name: "fast broad scan, poor selectivity", op: "find", key: "find|s", summary: "find on fields: s",
		query: &stats.ExecutableQuery{Filter: map[string]interface{}{"s": "x42"}}, ms: 2, n: 300, explain: "find_collscan",
		coverage: stats.EffCoverageAssessed, band: stats.EfficiencyBandInefficient, label: "0", advice: true},
	{name: "fast selective lookup", op: "find", key: "find|_id", summary: "find on fields: _id",
		query: &stats.ExecutableQuery{Filter: map[string]interface{}{"_id": 5}}, ms: 1, n: 300, explain: "find_id",
		coverage: stats.EffCoverageAssessed, band: stats.EfficiencyBandEfficient, label: "100"},
	{name: "slow selective lookup", op: "find", key: "find|a|limit:5", summary: "find on fields: a (limit 5)",
		query: &stats.ExecutableQuery{Filter: map[string]interface{}{"a": 3}, Limit: 5}, ms: 900, n: 12, explain: "find_limit",
		coverage: stats.EffCoverageAssessed, band: stats.EfficiencyBandEfficient, label: "100"},
	{name: "covered lookup", op: "find", key: "find|b|proj", summary: "find on fields: b (covered)",
		query: &stats.ExecutableQuery{Filter: map[string]interface{}{"b": map[string]interface{}{"$gte": 10, "$lt": 20}}, Projection: map[string]interface{}{"_id": 0, "b": 1, "g": 1}},
		ms:    1, n: 100, explain: "find_covered",
		coverage: stats.EffCoverageAssessed, band: stats.EfficiencyBandEfficient, label: "100", observation: "covered"},
	{name: "inefficient indexed scan", op: "find", key: "find|a,s", summary: "find on fields: a, s",
		query: &stats.ExecutableQuery{Filter: map[string]interface{}{"a": 7, "s": "x7"}}, ms: 3, n: 100, explain: "find_ixscan_fetch",
		coverage: stats.EffCoverageAssessed, band: stats.EfficiencyBandInefficient, label: "33", advice: true},
	{name: "zero-result lookup", op: "find", key: "find|a", summary: "find on fields: a",
		query: &stats.ExecutableQuery{Filter: map[string]interface{}{"a": 999}}, ms: 1, n: 50, explain: "find_zero",
		coverage: stats.EffCoverageAssessed, band: stats.EfficiencyBandEfficient, label: "100", observation: "zero_result"},
	{name: "aggregation reduction", op: "aggregate", key: "aggregate|group", summary: "aggregate $match a, $group",
		query: &stats.ExecutableQuery{Pipeline: []interface{}{
			map[string]interface{}{"$match": map[string]interface{}{"a": map[string]interface{}{"$lt": 10}}},
			map[string]interface{}{"$group": map[string]interface{}{"_id": "$g", "n": map[string]interface{}{"$sum": 1}}},
		}}, ms: 4, n: 60, explain: "agg_group_pushdown",
		coverage: stats.EffCoverageAssessed, band: stats.EfficiencyBandEfficient, label: "100", observation: "aggregate_reduction"},
	{name: "inefficient write explain", op: "updateMany", key: "updateMany|s", summary: "updateMany on fields: s",
		query: &stats.ExecutableQuery{Filter: map[string]interface{}{"s": map[string]interface{}{"$regex": "^x1"}}, Update: map[string]interface{}{"$set": map[string]interface{}{"z": 1}}},
		ms:    6, n: 40, explain: "upd_many",
		coverage: stats.EffCoverageAssessed, band: stats.EfficiencyBandInefficient, label: "38"},
	{name: "selective write explain", op: "deleteOne", key: "deleteOne|a", summary: "deleteOne on fields: a",
		query: &stats.ExecutableQuery{Filter: map[string]interface{}{"a": 7}}, ms: 1, n: 40, explain: "del_one",
		coverage: stats.EffCoverageAssessed, band: stats.EfficiencyBandEfficient, label: "100"},
	{name: "planner-only output pipeline", op: "aggregate", key: "aggregate|out", summary: "aggregate $match a, $group, $out",
		query: &stats.ExecutableQuery{Pipeline: []interface{}{
			map[string]interface{}{"$match": map[string]interface{}{"a": 1}},
			map[string]interface{}{"$group": map[string]interface{}{"_id": "$g"}},
			map[string]interface{}{"$out": "c_report"},
		}}, ms: 5, n: 30, explain: "agg_planner",
		coverage: stats.EffCoveragePartial},
	{name: "explain timeout", op: "find", key: "find|t", summary: "find on fields: t",
		query: &stats.ExecutableQuery{Filter: map[string]interface{}{"t": "x"}}, ms: 2, n: 30, timeout: true,
		coverage: stats.EffCoverageFailed, label: "Unassessed"},
	{name: "missing evidence", op: "find", key: "find|legacy", summary: "find on fields: legacy",
		ms: 2, n: 30,
		coverage: stats.EffCoverageUnavailable, label: "Unassessed"},
	{name: "insert", op: "insert", key: "insert", summary: "insert", ms: 1, n: 30,
		coverage: stats.EffCoverageNotApplicable, label: "Not applicable"},
}

type acceptanceRun struct {
	server       *WebServer
	explainCalls int
	listCalls    int
	commands     map[string]explainCommandSpec
}

// acceptanceServer records every case through the collector and stubs only
// the network round trip: commands are built from the captured samples and the
// recorded explain output goes through the live parsing path.
func acceptanceServer(t *testing.T) *acceptanceRun {
	t.Helper()
	recorded := loadRealExplains(t)
	byKey := map[string]acceptanceCase{}
	for _, tc := range acceptanceCases {
		byKey[tc.key] = tc
	}
	run := &acceptanceRun{commands: map[string]explainCommandSpec{}}
	origRun, origList := runExplainJobsFn, listNamespaceIndexesFn
	t.Cleanup(func() { runExplainJobsFn, listNamespaceIndexesFn = origRun, origList })
	runExplainJobsFn = func(ctx context.Context, jobs []explainJob, workers int, cfg *config.AppConfig, maxTimeMs int, verbosity string, retries int, backoffMs int) []explainEvidence {
		run.explainCalls++
		out := make([]explainEvidence, len(jobs))
		for i, j := range jobs {
			tc := byKey[j.sample.ShapeKey]
			spec, reason := buildExplainCommandSpec(j.sample, maxTimeMs, verbosity)
			if reason != "" {
				t.Fatalf("%s: a captured sample must build an explain command: %s", tc.name, reason)
			}
			run.commands[tc.key] = spec
			if tc.timeout {
				out[i] = explainEvidence{status: "timed_out", reason: "max_time_exceeded", attempts: retries + 1}
				continue
			}
			doc, ok := recorded[tc.explain]
			if !ok {
				t.Fatalf("%s: no recorded explain %q", tc.name, tc.explain)
			}
			diag := &stats.ExplainDiagnostics{
				ReplayDB: spec.db, ReplayCollection: j.sample.Collection, Verbosity: spec.verbosity, VerbosityNote: spec.verbosityNote,
				ServerMaxTimeMS: spec.serverMaxTime, ClientTimeoutMS: int(spec.clientTimeout / time.Millisecond),
				StageSummary: spec.stageSummary, CommandSummary: spec.commandSummary, UnfilteredScanExpected: spec.unfiltered,
			}
			diag.ServerVersion = explainServerVersion(doc)
			applyExplainSignalsToDiagnostics(diag, parseExplainSignals(doc))
			applyExplainMetricsToDiagnostics(diag, normalizeExplainMetrics(doc, j.sample.Operation), j.sample.Operation)
			stats.DescribeExplainSample(diag, j.sample.Operation)
			out[i] = explainEvidence{status: "explained", reason: diag.Diagnosis, attempts: 1, diag: diag}
		}
		return out
	}
	listNamespaceIndexesFn = func(ctx context.Context, cfg *config.AppConfig, namespaces []string) stats.IndexInventoryView {
		run.listCalls++
		view := stats.CollectIndexInventory(ctx, namespaces, func(_ context.Context, ns string) stats.NamespaceIndexInventory {
			return stats.NamespaceIndexInventory{Status: "fetched", Indexes: []stats.DeployedIndex{
				{Name: "_id_", Type: "btree", Keys: []stats.IndexKeySpec{{Field: "_id", Direction: 1, Kind: "asc"}}},
				{Name: "a_1", Type: "btree", Keys: []stats.IndexKeySpec{{Field: "a", Direction: 1, Kind: "asc"}}},
				{Name: "b_1_g_1", Type: "btree", Keys: []stats.IndexKeySpec{{Field: "b", Direction: 1, Kind: "asc"}, {Field: "g", Direction: 1, Kind: "asc"}}},
			}}
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
		InsightsExplainEnabled: true, InsightsExplainTopN: 12, InsightsExplainSeverityMode: "high_and_low",
		InsightsExplainEfficiencyMode: config.ExplainEfficiencyModeDefault,
	})
	for _, tc := range acceptanceCases {
		for i := 0; i < tc.n; i++ {
			var q *stats.ExecutableQuery
			if tc.query != nil {
				cp := *tc.query
				cp.Database = "s4"
				q = &cp
			}
			d := time.Duration((tc.ms + float64(i%3)*0.1) * float64(time.Millisecond))
			c.RecordOperationEvent(tc.op, "s4", "c", tc.key, tc.summary, nil, d, true, 1, q, tc.name, "acceptance", "w", "q.json", "q-"+tc.key, "", "", 0)
		}
	}
	s := NewServer(&config.AppConfig{Duration: "1s", Concurrency: 4})
	s.TrendStore = stats.OpenTrendStore(filepath.Join(t.TempDir(), "trend_baselines.json"))
	s.CurrentStats = c
	run.server = s
	return run
}

// jsonValue normalizes a BSON value to plain JSON types for comparison.
func jsonValue(t *testing.T, v interface{}) interface{} {
	t.Helper()
	b, err := bson.MarshalExtJSON(bson.D{{Key: "v", Value: canonicalCommandValue(v)}}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out["v"]
}

// containsJSON reports whether every field of want is present and equal in got.
func containsJSON(want, got interface{}) bool {
	switch w := want.(type) {
	case map[string]interface{}:
		g, ok := got.(map[string]interface{})
		if !ok {
			return false
		}
		for k, wv := range w {
			if !containsJSON(wv, g[k]) {
				return false
			}
		}
		return true
	case []interface{}:
		g, ok := got.([]interface{})
		if !ok || len(g) != len(w) {
			return false
		}
		for i := range w {
			if !containsJSON(w[i], g[i]) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(want, got)
	}
}

func TestAcceptanceCaptureToUIConsistency(t *testing.T) {
	run := acceptanceServer(t)
	api := getInsightsJSON(t, run.server)
	body, _ := json.Marshal(api)
	var rep stats.InsightsReport
	if err := json.Unmarshal(body, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.EfficiencyView == nil || rep.EfficiencyView.Status != "ready" {
		t.Fatalf("efficiency view not ready: %+v", rep.EfficiencyView)
	}
	recorded := loadRealExplains(t)
	rows := map[string]stats.EfficiencyViewRow{}
	for _, r := range rep.EfficiencyView.Rows {
		rows[r.ShapeSummary] = r
	}
	coverage := map[string]stats.ExplainCoverageShape{}
	for _, cs := range rep.ExplainCoverage.Shapes {
		coverage[cs.ShapeID] = cs
	}
	recsByShape := map[string][]stats.InsightRecommendation{}
	recsByRank := map[int]stats.InsightRecommendation{}
	for _, r := range rep.Recommendations {
		recsByRank[r.Rank] = r
		ids := r.ShapeIDs
		if !containsString(ids, r.ShapeID) {
			ids = append(ids, r.ShapeID)
		}
		for _, id := range ids {
			if id != "" && r.Source == "index_advice" {
				recsByShape[id] = append(recsByShape[id], r)
			}
		}
	}
	slow := map[string]bool{}
	for _, sq := range rep.SlowQueries {
		slow[sq.ShapeID] = true
	}

	for _, tc := range acceptanceCases {
		row, ok := rows[tc.summary]
		if !ok {
			t.Fatalf("%s: no efficiency row", tc.name)
		}
		if row.ShapeID != stats.StableShapeID(tc.op, "c", tc.key) {
			t.Fatalf("%s: row shape id %s does not match the captured shape", tc.name, row.ShapeID)
		}
		if row.Coverage != tc.coverage {
			t.Fatalf("%s: coverage %s, want %s (%+v)", tc.name, row.Coverage, tc.coverage, row)
		}
		if tc.label != "" && row.ScoreLabel != tc.label {
			t.Fatalf("%s: score label %q, want %q (%+v)", tc.name, row.ScoreLabel, tc.label, row.Dimensions)
		}
		if tc.band != "" && row.Band != tc.band {
			t.Fatalf("%s: band %q, want %q", tc.name, row.Band, tc.band)
		}
		if tc.observation != "" && !containsString(row.Observations, tc.observation) {
			t.Fatalf("%s: observations %v, want %s", tc.name, row.Observations, tc.observation)
		}
		if (tc.ms < 200) != row.BelowSlowThreshold || slow[row.ShapeID] == row.BelowSlowThreshold {
			t.Fatalf("%s: latency severity must follow the slow threshold only (below=%v, in slow view=%v)", tc.name, row.BelowSlowThreshold, slow[row.ShapeID])
		}

		// Rating: the row is a projection of the cached explain coverage.
		cs, ok := coverage[row.ShapeID]
		if !ok {
			t.Fatalf("%s: shape missing from explain_coverage", tc.name)
		}
		if e := cs.Efficiency; e == nil || e.Status != row.Status || !reflect.DeepEqual(e.Score, row.Score) || e.Band != row.Band {
			t.Fatalf("%s: row %+v differs from explain_coverage efficiency %+v", tc.name, row, cs.Efficiency)
		}
		if tc.coverage == stats.EffCoveragePartial && (!row.ScoreIsUpperBound || row.Band == stats.EfficiencyBandEfficient || !strings.HasPrefix(row.ScoreLabel, "≤ ")) {
			t.Fatalf("%s: planner-only evidence must be an upper bound, never efficient: %+v", tc.name, row)
		}
		if row.Score == nil && tc.coverage != stats.EffCoverageAssessed && tc.coverage != stats.EffCoveragePartial && row.CoverageReason == "" {
			t.Fatalf("%s: unscored row must carry a reason", tc.name)
		}

		// Selection and explain command: the command is the captured query.
		spec, explained := run.commands[tc.key]
		if tc.query == nil || tc.op == "insert" {
			if explained {
				t.Fatalf("%s: no explain may be sent without a captured explainable query", tc.name)
			}
			continue
		}
		if !explained || cs.SelectedBy == "" {
			t.Fatalf("%s: shape must be selected and explained (selected_by=%q)", tc.name, cs.SelectedBy)
		}
		if row.BelowSlowThreshold && cs.SelectedBy == "severity" {
			t.Fatalf("%s: a below-threshold shape can only come from an efficiency slot", tc.name)
		}
		inner := jsonValue(t, spec.cmd).(map[string]interface{})["explain"].(map[string]interface{})
		if tc.explain != "" {
			want := jsonValue(t, recorded[tc.explain]["command"]).(map[string]interface{})
			delete(want, "$db")
			if tc.coverage == stats.EffCoveragePartial {
				// The recorded planner explain lacks $out; compare the leading stages.
				got := inner["pipeline"].([]interface{})
				want["pipeline"] = append(want["pipeline"].([]interface{}), got[len(got)-1])
				if spec.verbosity != "queryPlanner" || got[len(got)-1].(map[string]interface{})["$out"] == nil {
					t.Fatalf("%s: $out pipeline must be explained with queryPlanner, got %s", tc.name, spec.verbosity)
				}
			}
			if !containsJSON(want, inner) {
				t.Fatalf("%s: explain command %v does not carry the captured query %v", tc.name, inner, want)
			}
		}

		// Advice: index candidates for inefficient reads, none for efficient shapes.
		recs := recsByShape[row.ShapeID]
		if tc.advice && len(recs) == 0 {
			t.Fatalf("%s: expected index advice, got none", tc.name)
		}
		if tc.band == stats.EfficiencyBandEfficient && len(recs) > 0 {
			t.Fatalf("%s: efficient shape must not get index advice: %+v", tc.name, recs)
		}
		for _, rank := range row.RecommendationRanks {
			r, ok := recsByRank[rank]
			if !ok || (r.ShapeID != row.ShapeID && !containsString(r.ShapeIDs, row.ShapeID)) {
				t.Fatalf("%s: linked recommendation #%d is not about this shape: %+v", tc.name, rank, r)
			}
		}
	}

	if fast := rows["find on fields: s"]; len(recsByShape[fast.ShapeID]) == 0 || !reflect.DeepEqual(recsByShape[fast.ShapeID][0].CandidateKeys, []string{"s:1"}) {
		t.Fatalf("fast collection scan advice must propose an index on s: %+v", recsByShape[fast.ShapeID])
	}
	if strings.Contains(string(body), "x42") || strings.Contains(string(body), "x7\"") {
		t.Fatalf("API leaked a captured query literal")
	}
	if rep.Impact.Coverage.TrackedGroups != len(acceptanceCases) {
		t.Fatalf("impact must track every shape regardless of explain: %+v", rep.Impact.Coverage)
	}

	// HTML report and export reuse the same cached report.
	page := html.UnescapeString(getReportHTML(t, run.server))
	for _, r := range rep.EfficiencyView.Rows {
		if cell := fmt.Sprintf("<td>%s</td><td class=\"num\"><strong>%s</strong></td>", r.ShapeSummary, r.ScoreLabel); !strings.Contains(page, cell) {
			t.Fatalf("HTML report missing row %q", cell)
		}
	}
	if again := getInsightsJSON(t, run.server); !reflect.DeepEqual(again, api) {
		t.Fatalf("repeated /api/insights must return the cached report")
	}
	if run.explainCalls != 1 || run.listCalls != 1 {
		t.Fatalf("API, report, and export must not rerun explain or listIndexes: explains=%d listIndexes=%d", run.explainCalls, run.listCalls)
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
