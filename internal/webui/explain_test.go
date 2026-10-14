package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/db"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func innerExplain(t *testing.T, spec explainCommandSpec) bson.D {
	t.Helper()
	if len(spec.cmd) != 2 || spec.cmd[0].Key != "explain" || spec.cmd[1].Key != "verbosity" {
		t.Fatalf("expected {explain, verbosity} command, got %v", spec.cmd)
	}
	inner, ok := spec.cmd[0].Value.(bson.D)
	if !ok {
		t.Fatalf("expected inner command document, got %T", spec.cmd[0].Value)
	}
	return inner
}

func field(d bson.D, key string) (interface{}, bool) {
	for _, e := range d {
		if e.Key == key {
			return e.Value, true
		}
	}
	return nil, false
}

func TestBuildExplainCommandSpecPreservesFindDetails(t *testing.T) {
	filter := map[string]interface{}{"status": "open", "total": map[string]interface{}{"$gt": 100}}
	projection := map[string]interface{}{"_id": 0, "status": 1}
	ev := stats.RepresentativeSample{Operation: "find", Collection: "orders", Query: &stats.ExecutableQuery{Database: "shop", Filter: filter, Projection: projection, Limit: 25}}

	spec, reason := buildExplainCommandSpec(ev, 1500, "executionStats")
	if reason != "" {
		t.Fatalf("unexpected reason %q", reason)
	}
	inner := innerExplain(t, spec)
	if inner[0].Key != "find" || inner[0].Value != "orders" {
		t.Fatalf("expected find on orders, got %v", inner[0])
	}
	if v, _ := field(inner, "filter"); !jsonEqual(v, filter) {
		t.Fatalf("filter not preserved: %v", v)
	}
	if v, _ := field(inner, "projection"); !jsonEqual(v, projection) {
		t.Fatalf("projection not preserved: %v", v)
	}
	if v, _ := field(inner, "limit"); v != int64(25) {
		t.Fatalf("expected captured limit 25 (not a fabricated limit:1), got %v", v)
	}
	if spec.db != "shop" {
		t.Fatalf("expected captured database, got %q", spec.db)
	}
	for _, k := range []string{"sort", "skip", "collation", "allowDiskUse"} {
		if _, ok := field(inner, k); ok {
			t.Fatalf("explain must not add %s that the original query did not send", k)
		}
	}
}

func TestBuildExplainCommandSpecUnlimitedFindHasNoLimit(t *testing.T) {
	ev := stats.RepresentativeSample{Operation: "find", Collection: "orders", Query: &stats.ExecutableQuery{Database: "shop"}}
	spec, reason := buildExplainCommandSpec(ev, 1000, "executionStats")
	if reason != "" {
		t.Fatalf("unexpected reason %q", reason)
	}
	inner := innerExplain(t, spec)
	if _, ok := field(inner, "limit"); ok {
		t.Fatalf("expected no limit when none was captured")
	}
	if v, _ := field(inner, "filter"); !jsonEqual(v, map[string]interface{}{}) {
		t.Fatalf("expected captured empty filter, got %v", v)
	}
	if !spec.unfiltered {
		t.Fatalf("expected empty filter flagged as unfiltered")
	}
}

func TestBuildExplainCommandSpecWritesUseNativeNonMutatingExplain(t *testing.T) {
	filter := map[string]interface{}{"sku": "A-1"}
	update := map[string]interface{}{"$set": map[string]interface{}{"qty": 3}}
	tests := []struct {
		op        string
		cmdKey    string
		listKey   string
		wantStmt  map[string]interface{}
		withWrite bool
	}{
		{op: "updateOne", cmdKey: "update", listKey: "updates", wantStmt: map[string]interface{}{"q": filter, "u": update, "multi": false, "upsert": true}, withWrite: true},
		{op: "updateMany", cmdKey: "update", listKey: "updates", wantStmt: map[string]interface{}{"q": filter, "u": update, "multi": true, "upsert": true}, withWrite: true},
		{op: "deleteOne", cmdKey: "delete", listKey: "deletes", wantStmt: map[string]interface{}{"q": filter, "limit": 1}},
		{op: "deleteMany", cmdKey: "delete", listKey: "deletes", wantStmt: map[string]interface{}{"q": filter, "limit": 0}},
	}
	for _, tc := range tests {
		t.Run(tc.op, func(t *testing.T) {
			q := &stats.ExecutableQuery{Database: "shop", Filter: filter}
			if tc.withWrite {
				q.Update, q.Upsert = update, true
			}
			spec, reason := buildExplainCommandSpec(stats.RepresentativeSample{Operation: tc.op, Collection: "inventory", Query: q}, 1000, "executionStats")
			if reason != "" {
				t.Fatalf("unexpected reason %q", reason)
			}
			inner := innerExplain(t, spec)
			if inner[0].Key != tc.cmdKey || inner[0].Value != "inventory" {
				t.Fatalf("expected %s command, got %v", tc.cmdKey, inner[0])
			}
			if _, ok := field(inner, "find"); ok {
				t.Fatalf("writes must not be explained as a find surrogate")
			}
			list, _ := field(inner, tc.listKey)
			arr, ok := list.(bson.A)
			if !ok || len(arr) != 1 {
				t.Fatalf("expected one %s statement, got %v", tc.listKey, list)
			}
			if !jsonEqual(arr[0], tc.wantStmt) {
				t.Fatalf("statement mismatch: got %v want %v", arr[0], tc.wantStmt)
			}
		})
	}
}

func TestBuildExplainCommandSpecAggregate(t *testing.T) {
	pipeline := []interface{}{
		map[string]interface{}{"$match": map[string]interface{}{"status": "open"}},
		map[string]interface{}{"$group": map[string]interface{}{"_id": "$city"}},
	}
	spec, reason := buildExplainCommandSpec(stats.RepresentativeSample{Operation: "aggregate", Collection: "orders", Query: &stats.ExecutableQuery{Database: "shop", Pipeline: pipeline}}, 1000, "executionStats")
	if reason != "" {
		t.Fatalf("unexpected reason %q", reason)
	}
	inner := innerExplain(t, spec)
	if v, _ := field(inner, "pipeline"); !jsonEqual(v, pipeline) {
		t.Fatalf("pipeline not preserved: %v", v)
	}
	if _, ok := field(inner, "allowDiskUse"); ok {
		t.Fatalf("explain must not inject allowDiskUse")
	}
	if spec.verbosity != "executionStats" || spec.unfiltered {
		t.Fatalf("unexpected verbosity/unfiltered: %q %v", spec.verbosity, spec.unfiltered)
	}

	withOut := append(append([]interface{}{}, pipeline...), map[string]interface{}{"$out": "report"})
	spec, _ = buildExplainCommandSpec(stats.RepresentativeSample{Operation: "aggregate", Collection: "orders", Query: &stats.ExecutableQuery{Database: "shop", Pipeline: withOut}}, 1000, "executionStats")
	if spec.verbosity != "queryPlanner" || spec.verbosityNote == "" {
		t.Fatalf("expected $out pipeline to be explained with queryPlanner only, got %q", spec.verbosity)
	}
}

func TestBuildExplainCommandSpecMissingDetailsAreUnavailable(t *testing.T) {
	tests := []struct {
		name string
		ev   stats.RepresentativeSample
		want string
	}{
		{"no captured query", stats.RepresentativeSample{Operation: "find", Collection: "orders"}, "executable_query_not_captured"},
		{"no database", stats.RepresentativeSample{Operation: "find", Collection: "orders", Query: &stats.ExecutableQuery{}}, "database_not_captured"},
		{"no collection", stats.RepresentativeSample{Operation: "find", Query: &stats.ExecutableQuery{Database: "shop"}}, "collection_not_captured"},
		{"update without update doc", stats.RepresentativeSample{Operation: "updateOne", Collection: "orders", Query: &stats.ExecutableQuery{Database: "shop", Filter: map[string]interface{}{"a": 1}}}, "update_document_not_captured"},
		{"unsupported", stats.RepresentativeSample{Operation: "insert", Collection: "orders", Query: &stats.ExecutableQuery{Database: "shop"}}, "unsupported_operation"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, reason := buildExplainCommandSpec(tc.ev, 1000, "executionStats"); reason != tc.want {
				t.Fatalf("reason=%q want %q", reason, tc.want)
			}
		})
	}
}

func TestExplainCommandSummaryIsRedacted(t *testing.T) {
	ev := stats.RepresentativeSample{Operation: "updateMany", Collection: "users", Query: &stats.ExecutableQuery{
		Database: "shop",
		Filter:   map[string]interface{}{"email": "secret@example.com"},
		Update:   map[string]interface{}{"$set": map[string]interface{}{"token": "s3cr3t"}},
	}}
	spec, _ := buildExplainCommandSpec(ev, 1000, "executionStats")
	for _, s := range []string{spec.commandSummary, spec.stageSummary} {
		if strings.Contains(s, "secret") || strings.Contains(s, "s3cr3t") {
			t.Fatalf("summary leaked literal values: %q", s)
		}
	}
	if !strings.Contains(spec.commandSummary, "{email}") || !strings.Contains(spec.commandSummary, "multi=true") {
		t.Fatalf("expected key-only summary, got %q", spec.commandSummary)
	}
}

func shapeSamples(op, key string, latencies []float64, withQuery bool) []stats.RepresentativeSample {
	out := make([]stats.RepresentativeSample, 0, len(latencies))
	for i, ms := range latencies {
		smp := stats.RepresentativeSample{ShapeID: stats.StableShapeID(op, "orders", key), Operation: op, Database: "shop", Collection: "orders", ShapeKey: key, DurationMs: ms}
		if withQuery {
			smp.Query = &stats.ExecutableQuery{Database: "shop", Filter: map[string]interface{}{"k": key, "n": i}}
		}
		out = append(out, smp)
	}
	return out
}

func TestSelectRepresentativeSamplesNearestMedianAndP95(t *testing.T) {
	samples := shapeSamples("find", "k", []float64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100, 110, 120, 130, 140, 150, 160, 170, 180, 190, 200, 210}, true)

	picks, sel, reason := selectRepresentativeSamples(samples, 1000, "executionStats")
	if reason != "" {
		t.Fatalf("unexpected reason %q", reason)
	}
	if sel.RetainedSuccessfulSamples != 21 || sel.MedianMs != 110 || sel.P95Ms != 200 {
		t.Fatalf("unexpected selection stats: %+v", sel)
	}
	if len(picks) != 2 || picks[0].role != "median" || picks[0].sample.DurationMs != 110 || picks[1].role != "p95" || picks[1].sample.DurationMs != 200 {
		t.Fatalf("expected median=110 and p95=200 picks, got %+v", picks)
	}
}

func TestSelectRepresentativeSamplesPrefersExecutableNearestAndDedupes(t *testing.T) {
	samples := shapeSamples("find", "k", []float64{10, 50, 100}, false)
	samples[0].Query = &stats.ExecutableQuery{Database: "shop", Filter: map[string]interface{}{"n": 1}}
	picks, sel, reason := selectRepresentativeSamples(samples, 1000, "executionStats")
	if reason != "" || len(picks) != 1 || picks[0].role != "median+p95" || picks[0].sample.DurationMs != 10 {
		t.Fatalf("expected one deduplicated pick, got picks=%+v reason=%q", picks, reason)
	}
	if sel.RetainedSuccessfulSamples != 3 || sel.ExecutableSamples != 1 || sel.MedianMs != 50 {
		t.Fatalf("expected percentiles over all representative samples, got %+v", sel)
	}

	same := shapeSamples("find", "k", []float64{10, 100}, true)
	same[0].Query.Filter = map[string]interface{}{"a": 1, "b": "x", "c": true}
	same[1].Query.Filter = map[string]interface{}{"c": true, "b": "x", "a": 1}
	picks, _, _ = selectRepresentativeSamples(same, 1000, "executionStats")
	if len(picks) != 1 {
		t.Fatalf("identical commands must be explained once, got %d picks", len(picks))
	}
}

func TestSelectRepresentativeSamplesReasons(t *testing.T) {
	if _, _, reason := selectRepresentativeSamples(nil, 1000, "executionStats"); reason != "no_representative_samples" {
		t.Fatalf("got %q", reason)
	}
	if _, sel, reason := selectRepresentativeSamples(shapeSamples("find", "k", []float64{10, 20}, false), 1000, "executionStats"); reason != "executable_query_not_captured" || sel == nil {
		t.Fatalf("got %q sel=%v", reason, sel)
	}
}

// invShape builds an inventory shape with n executable samples of latency ms.
func invShape(op, key string, rank int, count uint64, avgMs float64, n int) stats.ExplainInventoryShape {
	lat := make([]float64, n)
	for i := range lat {
		lat[i] = avgMs
	}
	return stats.ExplainInventoryShape{
		ShapeID: stats.StableShapeID(op, "orders", key), Operation: op, Database: "shop", Collection: "orders", ShapeKey: key,
		ShapeSummary: "find on " + key, ImpactRank: rank, OperationCount: count, EligibleOperations: count,
		TotalOperationMs: avgMs * float64(count), AvgMs: avgMs, MaxMs: avgMs, SampleScope: stats.SampleScopeUniformReservoir,
		Samples: shapeSamples(op, key, lat, true),
	}
}

func explainReport(shapes int, perShape int) (stats.InsightsReport, stats.ExplainInventory) {
	rep := stats.InsightsReport{Metadata: stats.InsightsMetadata{Status: "ready", SlowThresholdMs: 1, SamplingRate: 1}}
	inv := stats.ExplainInventory{CaptureStatus: "ready", GroupLimit: 50, SlowThresholdMs: 1}
	for s := 0; s < shapes; s++ {
		key := string(rune('a' + s))
		id := stats.StableShapeID("find", "orders", key)
		rep.SlowQueries = append(rep.SlowQueries, stats.SlowQueryInsight{ShapeID: id, Operation: "find", Collection: "orders", ShapeKey: key, Severity: "high", P95Ms: 100, P99Ms: 100})
		rep.PotentialIndexIssues = append(rep.PotentialIndexIssues, stats.IndexIssue{ShapeID: id, Collection: "orders", Operation: "find"})
		is := invShape("find", key, s+1, uint64(perShape), 100, 0)
		lat := make([]float64, perShape)
		for i := range lat {
			lat[i] = float64(10 * (i + 1))
		}
		is.Samples = shapeSamples("find", key, lat, true)
		inv.Shapes = append(inv.Shapes, is)
	}
	return rep, inv
}

func defaultParams(topN int, mode string) explainParams {
	return explainParams{topN: topN, maxTimeMs: 1000, verbosity: "executionStats", severityMode: mode, efficiencyMode: config.ExplainEfficiencyModeOff, workers: 3, retries: 2}
}

func TestEnrichInsightsWithExplainEnforcesTopNAndSampleLimits(t *testing.T) {
	var connects int32
	origConnect := connectFn
	connectFn = func(ctx context.Context, cfg *config.AppConfig, dbName string) (*db.Connection, error) {
		atomic.AddInt32(&connects, 1)
		return nil, context.DeadlineExceeded
	}
	defer func() { connectFn = origConnect }()

	rep, inv := explainReport(3, 20)
	enrichInsightsWithExplain(context.Background(), "ready", &rep, inv, &config.AppConfig{}, defaultParams(2, "high_only"))

	if got := atomic.LoadInt32(&connects); got != 4 {
		t.Fatalf("expected 2 shapes x 2 samples = 4 explain jobs, got %d", got)
	}
	if rep.Metadata.ExplainSamplesPlanned != 4 || rep.Metadata.ExplainCommandsRun != 0 || rep.Metadata.ExplainMaxSamples != 4 {
		t.Fatalf("unexpected limit metadata: %+v", rep.Metadata)
	}
	if rep.Metadata.ExplainSelectionScope != "per_shape_representative_samples" || rep.Metadata.ExplainTimingNote == "" {
		t.Fatalf("expected selection scope and timing note, got %+v", rep.Metadata)
	}
	for i := 0; i < 2; i++ {
		sq := rep.SlowQueries[i]
		if sq.ExplainStatus != "execution_failed" || sq.ExplainFinding != nil || len(sq.ExplainSamples) != 2 {
			t.Fatalf("shape %d: expected inconclusive failure with 2 samples, got %+v", i, sq)
		}
		if sq.ExplainSelection == nil || sq.ExplainSelection.Scope != stats.SampleScopeUniformReservoir {
			t.Fatalf("shape %d: expected per-shape reservoir scope, got %+v", i, sq.ExplainSelection)
		}
		if rep.PotentialIndexIssues[i].EvidenceLevel != "heuristic_explain_inconclusive" || rep.PotentialIndexIssues[i].Confidence != "low" {
			t.Fatalf("shape %d: expected inconclusive issue, got %+v", i, rep.PotentialIndexIssues[i])
		}
	}
	if rep.SlowQueries[2].ExplainReason != "not_selected_top_n_2" || len(rep.SlowQueries[2].ExplainSamples) != 0 {
		t.Fatalf("expected third shape filtered by top N, got %+v", rep.SlowQueries[2])
	}
	cov := rep.ExplainCoverage
	if cov.SelectedShapes != 2 || cov.FailedShapes != 2 || cov.ExcludedShapes != 1 || cov.ExcludedByReason["not_selected_efficiency_mode_off"] != 1 {
		t.Fatalf("unexpected coverage counts: %+v", cov)
	}
}

func TestRetryExplainEvidenceCountsAttemptsAndDoublesBudget(t *testing.T) {
	var budgets []int
	got := retryExplainEvidence(context.Background(), 500, 2, 0, func(maxTimeMs int) explainEvidence {
		budgets = append(budgets, maxTimeMs)
		return explainEvidence{status: "timed_out", reason: "max_time_exceeded"}
	})
	if got.status != "timed_out" || got.attempts != 3 {
		t.Fatalf("expected 3 timed-out attempts, got %+v", got)
	}
	if len(budgets) != 3 || budgets[0] != 500 || budgets[1] != 1000 || budgets[2] != 2000 {
		t.Fatalf("unexpected budgets %v", budgets)
	}
}

func TestParseExplainSignalsIgnoresRejectedPlansAndQueryOperators(t *testing.T) {
	out := bson.M{
		"queryPlanner": bson.M{
			"parsedQuery": bson.M{"$and": bson.A{bson.M{"a": bson.M{"$gt": 1}}}},
			"winningPlan": bson.M{"stage": "FETCH", "inputStage": bson.M{"stage": "IXSCAN", "indexName": "a_1"}},
			"rejectedPlans": bson.A{
				bson.M{"stage": "SORT", "inputStage": bson.M{"stage": "COLLSCAN"}},
			},
		},
		"command":        bson.M{"find": "orders", "filter": bson.M{"a": bson.M{"$gt": 1}}},
		"executionStats": bson.M{"nReturned": 5, "totalDocsExamined": 5, "totalKeysExamined": 5},
	}
	sig := parseExplainSignals(out)
	if sig.collectionScan || sig.blockingSort {
		t.Fatalf("rejected plan stages must not leak into the diagnosis: %+v", sig.planStages)
	}
	for _, st := range sig.planStages {
		if st == "AND" || st == "GT" {
			t.Fatalf("query operators must not be reported as plan stages: %v", sig.planStages)
		}
	}
	if !sig.indexScan || !sig.executionStats {
		t.Fatalf("expected index scan with execution stats, got %+v", sig)
	}
	if m := normalizeExplainMetrics(out, "find"); m.Returned == nil || m.Returned.Value != 5 {
		t.Fatalf("expected nReturned=5 from executionStats, got %+v", m)
	}
}

func TestParseExplainSignalsWriteMatchedCount(t *testing.T) {
	out := bson.M{
		"queryPlanner":   bson.M{"winningPlan": bson.M{"stage": "UPDATE", "inputStage": bson.M{"stage": "COLLSCAN"}}},
		"executionStats": bson.M{"nReturned": 0, "totalDocsExamined": 5000, "executionStages": bson.M{"stage": "UPDATE", "nMatched": 4000, "nWouldModify": 4000}},
	}
	sig := parseExplainSignals(out)
	m := normalizeExplainMetrics(out, "updateMany")
	if !sig.collectionScan || m.Matched == nil || m.Matched.Value != 4000 || m.DocsExamined == nil || m.DocsExamined.Value != 5000 {
		t.Fatalf("expected matched count from update stage, got %+v %+v", sig, m)
	}
}

func TestInsightsAPIReportAndExportShareExplainRecommendations(t *testing.T) {
	var runs int32
	origRun := runExplainJobsFn
	runExplainJobsFn = func(ctx context.Context, jobs []explainJob, workers int, cfg *config.AppConfig, maxTimeMs int, verbosity string, retries int, backoffMs int) []explainEvidence {
		atomic.AddInt32(&runs, 1)
		out := make([]explainEvidence, len(jobs))
		for i := range jobs {
			d := &stats.ExplainDiagnostics{
				ReplayDB: jobs[i].sample.Query.Database, ReplayCollection: jobs[i].sample.Collection, Verbosity: verbosity,
				ExecutionStatsAvailable: true, CollectionScanDetected: true, WinningPlanSummary: "COLLSCAN",
				DocsExamined: 50000, NReturned: 2, ExecutionTimeMillis: 40,
			}
			stats.DescribeExplainSample(d, jobs[i].sample.Operation)
			out[i] = explainEvidence{status: "explained", reason: d.Diagnosis, attempts: 1, diag: d}
		}
		return out
	}
	defer func() { runExplainJobsFn = origRun }()

	c := stats.NewCollector()
	c.ConfigureInsights(&config.AppConfig{
		InsightsEnabled: true, InsightsSamplingRate: 1, InsightsSlowThresholdMs: 200, InsightsMaxEvents: 200, InsightsMaxGroups: 50,
		InsightsExplainEnabled: true, InsightsExplainTopN: 5, InsightsExplainSeverityMode: "high_and_low",
	})
	for i := 0; i < 12; i++ {
		q := &stats.ExecutableQuery{Database: "shop", Filter: map[string]interface{}{"email": fmt.Sprintf("secret-value-123-%d", i)}, Limit: 10}
		c.RecordOperationEvent("find", "shop", "users", "find|{email:<str>}", "find on fields: email", []string{"email"}, time.Duration(900+i*10)*time.Millisecond, true, 1, q, "users by email", "unit_test", "w", "q.json", "q1", "{email:<str>}", "", 0)
	}
	s := NewServer(&config.AppConfig{Duration: "1s"})
	s.CurrentStats = c

	rec := httptest.NewRecorder()
	s.handleInsights(rec, httptest.NewRequest(http.MethodGet, "/api/insights", nil))
	body := rec.Body.String()
	if strings.Contains(body, "secret-value-123") {
		t.Fatalf("API payload leaked executable query values")
	}
	var api stats.InsightsReport
	if err := json.Unmarshal(rec.Body.Bytes(), &api); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var explainRec *stats.InsightRecommendation
	for i := range api.Recommendations {
		if api.Recommendations[i].Source == "explain" {
			explainRec = &api.Recommendations[i]
		}
	}
	if explainRec == nil || explainRec.Diagnosis != stats.DiagCollscanSelective {
		t.Fatalf("expected explain-backed recommendation, got %+v", api.Recommendations)
	}
	sq := api.SlowQueries[0]
	if sq.ExplainFinding == nil || sq.ExplainFinding.Confidence != explainRec.Confidence || len(api.PotentialIndexIssues) == 0 || api.PotentialIndexIssues[0].Confidence != explainRec.Confidence {
		t.Fatalf("finding, issue, and recommendation confidence must agree: finding=%+v issues=%+v rec=%+v", sq.ExplainFinding, api.PotentialIndexIssues, explainRec)
	}
	if sq.ExplainSelection == nil || sq.ExplainSelection.Scope != stats.SampleScopeUniformReservoir || len(sq.ExplainSamples) != 2 {
		t.Fatalf("expected two representative samples with scope, got %+v %+v", sq.ExplainSelection, sq.ExplainSamples)
	}

	htmlRec := httptest.NewRecorder()
	s.handleReport(htmlRec, httptest.NewRequest(http.MethodGet, "/api/report", nil))
	html := htmlRec.Body.String()
	if !strings.Contains(html, "Recommendations") || !strings.Contains(html, explainRec.Title) {
		t.Fatalf("HTML report must include the same explain recommendation title %q", explainRec.Title)
	}
	if strings.Contains(html, "secret-value-123") {
		t.Fatalf("HTML report leaked executable query values")
	}
	if got := atomic.LoadInt32(&runs); got != 1 {
		t.Fatalf("expected explains to run once per run across API and report, got %d", got)
	}
}

func jsonEqual(a, b interface{}) bool {
	ab, err1 := bson.MarshalExtJSON(bson.M{"v": a}, false, false)
	bb, err2 := bson.MarshalExtJSON(bson.M{"v": b}, false, false)
	if err1 != nil || err2 != nil {
		return false
	}
	var av, bv interface{}
	_ = json.Unmarshal(ab, &av)
	_ = json.Unmarshal(bb, &bv)
	aj, _ := json.Marshal(av)
	bj, _ := json.Marshal(bv)
	return string(aj) == string(bj)
}
