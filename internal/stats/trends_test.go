package stats

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var trendNow = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

func trendRun(id string, at time.Time) TrendRunContext {
	return TrendRunContext{
		RunID: id, RecordedAt: at.UTC().Format(time.RFC3339), ShapeIdentityVersion: ShapeIdentityVersion, ScoreVersion: EfficiencyVersion,
		Sampling: TrendSampling{SamplingRate: 1, SlowThresholdMs: 200, GroupLimit: 50, ExplainEnabled: true, ExplainVerbosity: "executionStats", ExplainMaxSamples: 20, CaptureStatus: "ready"},
		Server:   TrendServer{Versions: []string{"8.0.4"}, Topology: "mongod", Source: "explain"},
		Workload: TrendWorkload{Concurrency: 4, Duration: "20s"},
	}
}

func effSnap(run TrendRunContext, score int, commands int) *TrendEfficiencySnapshot {
	s := score
	return &TrendEfficiencySnapshot{
		RunID: run.RunID, RecordedAt: run.RecordedAt, ShapeIdentityVersion: run.ShapeIdentityVersion, ScoreVersion: run.ScoreVersion,
		Coverage: EffCoverageAssessed, Status: EfficiencyRated, Score: &s, Band: efficiencyBand(score), Confidence: "high",
		ExplainedCommands: commands, Evidence: "execution_stats", PlanSignatures: []string{"FETCH <- IXSCAN"}, IndexesUsed: []string{"a_1"},
		NamespaceIndexStatus: "fetched", NamespaceIndexes: []string{"_id_", "a_1"},
	}
}

func wlSnap(run TrendRunContext, ops uint64, avg float64, share float64) *TrendWorkloadSnapshot {
	return &TrendWorkloadSnapshot{
		RunID: run.RunID, RecordedAt: run.RecordedAt, ShapeIdentityVersion: run.ShapeIdentityVersion,
		OperationCount: ops, AvgMs: avg, TotalOperationMs: avg * float64(ops), ImpactShare: &share, GroupCoverageComplete: true,
	}
}

func shape(e *TrendEfficiencySnapshot, w *TrendWorkloadSnapshot) TrendShapeSnapshot {
	return TrendShapeSnapshot{ShapeID: "shape_a", Namespace: "db.c", Operation: "find", ShapeSummary: "find on fields: a", Efficiency: e, Workload: w}
}

func testCtx(cur TrendRunContext, runs ...TrendRunContext) trendCompareCtx {
	m := map[string]TrendRunContext{}
	for _, r := range runs {
		m[r.RunID] = r
	}
	return trendCompareCtx{run: cur, runs: m, now: trendNow, policy: DefaultTrendPolicy()}
}

func compareOne(t *testing.T, base, cur TrendShapeSnapshot, baseRun, curRun TrendRunContext) TrendRow {
	t.Helper()
	return compareTrendShape(cur, &base, testCtx(curRun, baseRun), "")
}

func viewOpts() TrendOptions { return TrendOptions{Policy: DefaultTrendPolicy()} }

func TestTrendEfficiencySupportedChanges(t *testing.T) {
	br, cr := trendRun("r1", trendNow.Add(-time.Hour)), trendRun("r2", trendNow)
	cases := []struct {
		base, cur int
		want      string
	}{
		{67, 100, TrendImproved},
		{67, 33, TrendRegressed},
		{67, 75, TrendNoMaterialChange},
		{0, 14, TrendNoMaterialChange},
		{0, 15, TrendImproved},
	}
	for _, tc := range cases {
		row := compareOne(t, shape(effSnap(br, tc.base, 2), nil), shape(effSnap(cr, tc.cur, 2), nil), br, cr)
		if row.Efficiency.State != tc.want {
			t.Fatalf("%d→%d: %s (%v), want %s", tc.base, tc.cur, row.Efficiency.State, row.Efficiency.Reasons, tc.want)
		}
		if row.Efficiency.Delta == nil || *row.Efficiency.Delta != float64(tc.cur-tc.base) {
			t.Fatalf("delta missing: %+v", row.Efficiency)
		}
	}
}

func TestTrendEfficiencySparseEvidenceIsNotLabeled(t *testing.T) {
	br, cr := trendRun("r1", trendNow.Add(-time.Hour)), trendRun("r2", trendNow)
	huge := wlSnap(cr, 5_000_000, 2, 0.5)
	cases := map[string]func(b, c *TrendEfficiencySnapshot){
		"one explained command in the current run": func(b, c *TrendEfficiencySnapshot) { c.ExplainedCommands = 1 },
		"one explained command in the baseline":    func(b, c *TrendEfficiencySnapshot) { b.ExplainedCommands = 1 },
		"medium confidence":                        func(b, c *TrendEfficiencySnapshot) { c.Confidence = "medium" },
		"partial baseline": func(b, c *TrendEfficiencySnapshot) {
			b.Coverage, b.Status, b.ScoreIsUpperBound = EffCoveragePartial, EfficiencyPartial, true
		},
		"budget-excluded current run": func(b, c *TrendEfficiencySnapshot) {
			c.Coverage, c.Status, c.Score, c.Reason = EffCoverageBudgetExcluded, EfficiencyUnassessed, nil, "not_selected_budget"
		},
	}
	for name, mutate := range cases {
		b, c := effSnap(br, 100, 2), effSnap(cr, 0, 2)
		mutate(b, c)
		row := compareOne(t, shape(b, wlSnap(br, 5_000_000, 2, 0.5)), shape(c, huge), br, cr)
		if row.Efficiency.State != TrendInsufficient {
			t.Fatalf("%s: state %s, want insufficient evidence", name, row.Efficiency.State)
		}
		if !strings.Contains(strings.Join(row.Efficiency.Reasons, " "), "5000000 operations do not add explain evidence") {
			t.Fatalf("%s: a large operation count must be called out as not explain evidence: %v", name, row.Efficiency.Reasons)
		}
	}
}

func TestTrendSharedExplainCommandCountsOnce(t *testing.T) {
	score := 0
	rep := InsightsReport{Metadata: InsightsMetadata{Status: "ready", ShapeIdentityVersion: ShapeIdentityVersion}, EfficiencyView: &EfficiencyView{Rows: []EfficiencyViewRow{{
		ShapeID: "shape_a", Namespace: "db.c", Operation: "find", Coverage: EffCoverageAssessed, Status: EfficiencyRated, Score: &score, Confidence: "high",
		OperationCount: 100, AvgMs: 1, Query: `{"a": "secret-value"}`,
		Samples: []EfficiencyViewSample{
			{Role: "median", ExplainStatus: "explained", Evidence: "execution_stats", WinningPlan: "COLLSCAN"},
			{Role: "p95", ExplainStatus: "explained", Evidence: "execution_stats", WinningPlan: "COLLSCAN", SharedCommand: true},
		},
	}}}}
	cur := CurrentTrendBaselines(rep, trendRun("r", trendNow))
	if cur[0].Efficiency.ExplainedCommands != 1 {
		t.Fatalf("a deduplicated explain is one command, got %d", cur[0].Efficiency.ExplainedCommands)
	}
	b, _ := json.Marshal(cur)
	if strings.Contains(string(b), "secret") {
		t.Fatalf("baseline summary must not include query text: %s", b)
	}
}

func TestTrendVersionAgeAndShapeMismatch(t *testing.T) {
	br, cr := trendRun("r1", trendNow.Add(-time.Hour)), trendRun("r2", trendNow)

	b := effSnap(br, 67, 2)
	b.ScoreVersion = "qe-0"
	row := compareOne(t, shape(b, wlSnap(br, 100, 5, 0.1)), shape(effSnap(cr, 100, 2), wlSnap(cr, 100, 5, 0.1)), br, cr)
	if row.Efficiency.State != TrendNotComparable || !strings.Contains(row.Efficiency.Reasons[0], "score_version_changed") {
		t.Fatalf("score version change must be disclosed and not compared: %+v", row.Efficiency)
	}
	if row.Latency.State == TrendNotComparable || row.Comparability != TrendPartial {
		t.Fatalf("a score version change does not block latency: %s / %s", row.Latency.State, row.Comparability)
	}

	legacyRun := trendRun("r1", trendNow.Add(-time.Hour))
	legacyRun.ScoreVersion = "qe-1"
	legacy := effSnap(legacyRun, 67, 2)
	row = compareOne(t, shape(legacy, nil), shape(effSnap(cr, 100, 2), nil), legacyRun, cr)
	if row.Efficiency.State != TrendImproved {
		t.Fatalf("a qe-1 baseline uses the same formula as %s and must stay comparable: %+v", EfficiencyVersion, row.Efficiency)
	}
	if CanonicalScoreVersion("qe-1") != EfficiencyVersion || CanonicalScoreVersion("qe-0") != "qe-0" {
		t.Fatal("only the renamed version is aliased")
	}

	oldSI := trendRun("r0", trendNow.Add(-time.Hour))
	oldSI.ShapeIdentityVersion = "si-1"
	row = compareOne(t, shape(effSnap(oldSI, 67, 2), wlSnap(oldSI, 100, 5, 0.1)), shape(effSnap(cr, 100, 2), wlSnap(cr, 100, 5, 0.1)), oldSI, cr)
	if row.Comparability != TrendIncompatible || row.Efficiency.State != TrendNotComparable || row.Latency.State != TrendNotComparable || row.Impact.State != TrendNotComparable {
		t.Fatalf("identity version change must block every metric: %+v", row)
	}

	old := trendRun("old", trendNow.Add(-31*24*time.Hour))
	row = compareOne(t, shape(effSnap(old, 67, 2), wlSnap(old, 100, 5, 0.1)), shape(effSnap(cr, 100, 2), wlSnap(cr, 100, 9, 0.1)), old, cr)
	if row.Comparability != TrendOutdated || row.Efficiency.State != TrendNotComparable || row.Latency.State != TrendNotComparable {
		t.Fatalf("outdated baseline must not be labeled: %+v", row)
	}
	if row.Baseline == nil || row.Baseline.EfficiencyAgeSeconds == nil || *row.Baseline.EfficiencyAgeSeconds < 30*24*3600 || row.Baseline.EfficiencyAge != "31 days old" {
		t.Fatalf("baseline age must be exposed: %+v", row.Baseline)
	}

	future := trendRun("f", trendNow.Add(time.Hour))
	row = compareOne(t, shape(effSnap(future, 67, 2), nil), shape(effSnap(cr, 100, 2), nil), future, cr)
	if row.Efficiency.State != TrendNotComparable {
		t.Fatalf("future baseline must not be compared: %s", row.Efficiency.State)
	}

	other := shape(effSnap(br, 67, 2), wlSnap(br, 100, 5, 0.1))
	other.Operation = "aggregate"
	row = compareOne(t, other, shape(effSnap(cr, 100, 2), wlSnap(cr, 100, 5, 0.1)), br, cr)
	if row.Comparability != TrendIncompatible || row.Efficiency.State != TrendNotComparable {
		t.Fatalf("a shape id reused by another operation is incompatible: %+v", row)
	}

	row = compareTrendShape(shape(effSnap(cr, 100, 2), wlSnap(cr, 100, 5, 0.1)), nil, testCtx(cr), "")
	if row.Comparability != TrendMissing || row.Efficiency.State != TrendNoBaseline || row.Latency.State != TrendNoBaseline || row.Impact.State != TrendNoBaseline {
		t.Fatalf("missing baseline: %+v", row)
	}
}

func TestTrendLatencyCriteriaReplaceTinyChangeWarnings(t *testing.T) {
	br, cr := trendRun("r1", trendNow.Add(-time.Hour)), trendRun("r2", trendNow)
	p := func(v float64) *float64 { return &v }
	cases := []struct {
		name               string
		bOps, cOps         uint64
		bAvg, cAvg         float64
		bP95, cP95         *float64
		want, reasonSubstr string
	}{
		{"tiny absolute change over 1 ms of old rule", 100, 100, 50, 51.5, nil, nil, TrendNoMaterialChange, ""},
		{"large relative change below 1 ms", 100, 100, 0.2, 0.5, nil, nil, TrendNoMaterialChange, ""},
		{"material regression", 100, 100, 5, 8, nil, nil, TrendRegressed, ""},
		{"material improvement", 100, 100, 8, 5, nil, nil, TrendImproved, ""},
		{"too few operations", 12, 12, 900, 1200, nil, nil, TrendInsufficient, "12 operations"},
		{"p95 contradicts the average", 100, 100, 5, 8, p(20), p(10), TrendInsufficient, "opposite directions"},
		{"p95 agrees", 100, 100, 5, 8, p(10), p(20), TrendRegressed, ""},
	}
	for _, tc := range cases {
		b, c := wlSnap(br, tc.bOps, tc.bAvg, 0.1), wlSnap(cr, tc.cOps, tc.cAvg, 0.1)
		if tc.bP95 != nil {
			b.SampleP95Ms, c.SampleP95Ms, b.RetainedSamples, c.RetainedSamples = tc.bP95, tc.cP95, 100, 100
		}
		got := compareTrendLatency(b, c)
		if got.State != tc.want {
			t.Fatalf("%s: %s (%s), want %s", tc.name, got.State, got.Label, tc.want)
		}
		if tc.reasonSubstr != "" && !strings.Contains(strings.Join(got.Reasons, " "), tc.reasonSubstr) {
			t.Fatalf("%s: reasons %v missing %q", tc.name, got.Reasons, tc.reasonSubstr)
		}
		if tc.bP95 == nil && tc.want != TrendInsufficient && !strings.Contains(got.Secondary, "p95 not compared") {
			t.Fatalf("%s: missing p95 must be disclosed: %q", tc.name, got.Secondary)
		}
	}
}

func TestTrendImpactCriteria(t *testing.T) {
	br, cr := trendRun("r1", trendNow.Add(-time.Hour)), trendRun("r2", trendNow)
	if got := compareTrendImpact(wlSnap(br, 100, 1, 0.04), wlSnap(cr, 100, 1, 0.12)); got.State != TrendIncreased {
		t.Fatalf("4%%→12%% should be a larger share: %+v", got)
	}
	if got := compareTrendImpact(wlSnap(br, 100, 1, 0.30), wlSnap(cr, 100, 1, 0.33)); got.State != TrendNoMaterialChange {
		t.Fatalf("3 points is below the threshold: %+v", got)
	}
	inc := wlSnap(cr, 100, 1, 0.9)
	inc.GroupCoverageComplete = false
	if got := compareTrendImpact(wlSnap(br, 100, 1, 0.1), inc); got.State != TrendInsufficient {
		t.Fatalf("incomplete group coverage must be insufficient: %+v", got)
	}
}

func TestTrendInterventionsAreAnnotatedWithoutDiscarding(t *testing.T) {
	br, cr := trendRun("r1", trendNow.Add(-time.Hour)), trendRun("r2", trendNow)
	cr.Server.Versions = []string{"8.0.5"}
	cr.Sampling.ExplainMaxSamples = 40
	cr.Workload.Concurrency = 16
	b := effSnap(br, 0, 2)
	b.PlanSignatures, b.IndexesUsed, b.NamespaceIndexes = []string{"COLLSCAN"}, nil, []string{"_id_"}
	c := effSnap(cr, 100, 2)
	row := compareOne(t, shape(b, wlSnap(br, 100, 5, 0.1)), shape(c, wlSnap(cr, 400, 5.2, 0.1)), br, cr)
	if row.Efficiency.State != TrendImproved || row.Comparability != TrendComparable {
		t.Fatalf("intervention must not discard a comparable result: %+v", row)
	}
	kinds := map[string]string{}
	for _, a := range append(row.Interventions, row.WorkloadChanges...) {
		kinds[a.Kind] = a.Detail
	}
	for _, k := range []string{"plan_changed", "indexes_used_changed", "index_inventory_changed", "server_version_changed", "sampling_settings_changed", "workload_config_changed", "operation_count_changed"} {
		if kinds[k] == "" {
			t.Fatalf("missing annotation %s in %v", k, kinds)
		}
	}
	if !strings.Contains(kinds["index_inventory_changed"], "added a_1") || !strings.Contains(kinds["operation_count_changed"], "100 → 400") {
		t.Fatalf("annotation details: %v", kinds)
	}
	all, _ := json.Marshal(row)
	for _, claim := range []string{"because", "caused", "due to"} {
		if strings.Contains(strings.ToLower(string(all)), claim) {
			t.Fatalf("row must not claim causation (%q): %s", claim, all)
		}
	}

	row = compareTrendShape(shape(c, nil), &TrendShapeSnapshot{ShapeID: "shape_a", Namespace: "db.c", Operation: "find", Efficiency: effSnap(br, 0, 2)}, testCtx(cr), "")
	if len(row.Interventions) == 0 || row.Interventions[len(row.Interventions)-1].Kind != "run_context_unavailable" {
		t.Fatalf("missing run context must be disclosed: %v", row.Interventions)
	}
}

func TestMergeTrendBaselinesRulesAndBounds(t *testing.T) {
	r1, r2 := trendRun("r1", trendNow.Add(-2*time.Hour)), trendRun("r2", trendNow.Add(-time.Hour))
	p := DefaultTrendPolicy()
	prev := MergeTrendBaselines(TrendStoreData{}, r1, []TrendShapeSnapshot{shape(effSnap(r1, 33, 2), wlSnap(r1, 100, 5, 0.1))}, p)
	excluded := effSnap(r2, 0, 0)
	excluded.Coverage, excluded.Status, excluded.Score = EffCoverageBudgetExcluded, EfficiencyUnassessed, nil
	next := MergeTrendBaselines(prev, r2, []TrendShapeSnapshot{shape(excluded, wlSnap(r2, 200, 6, 0.2))}, p)
	got := next.Shapes[0]
	if len(got.EfficiencyHistory) != 2 || got.EfficiencyHistory[0].RunID != "r2" || got.WorkloadHistory[0].RunID != "r2" || got.WorkloadHistory[1].RunID != "r1" {
		t.Fatalf("each run keeps its own snapshot, newest first: %+v", got)
	}
	if len(next.Runs) != 2 || next.Runs[0].RunID != "r2" {
		t.Fatalf("both runs must be kept newest first, got %+v", next.Runs)
	}
	cur := trendRun("r3", trendNow)
	sel := selectPreviousBaseline(next.Shapes[0], testCtx(cur, next.Runs...))
	if sel.Efficiency.RunID != "r1" || sel.Workload.RunID != "r2" {
		t.Fatalf("a budget-excluded run must not hide the assessed efficiency baseline; workload uses the newest: eff=%s wl=%s", sel.Efficiency.RunID, sel.Workload.RunID)
	}
	again := MergeTrendBaselines(next, r2, []TrendShapeSnapshot{shape(effSnap(r2, 100, 2), nil)}, p)
	if n := len(again.Shapes[0].EfficiencyHistory); n != 2 || again.Shapes[0].EfficiencyHistory[0].Score == nil || len(again.Shapes[0].WorkloadHistory) != 1 {
		t.Fatalf("re-recording a run id replaces that run's snapshots: %+v", again.Shapes[0])
	}

	var many []TrendShapeSnapshot
	data := TrendStoreData{}
	big := TrendPolicy{KeepRuns: 50}
	for i := 0; i < TrendMaxShapeBaselines+40; i++ {
		run := trendRun(fmt.Sprintf("run%03d", i), trendNow.Add(time.Duration(i)*time.Second))
		s := shape(nil, wlSnap(run, 50, 1, 0.01))
		s.ShapeID = fmt.Sprintf("shape_%04d", i)
		many = append(many, s)
		data = MergeTrendBaselines(data, run, []TrendShapeSnapshot{s}, big)
	}
	if len(data.Shapes) > TrendMaxShapeBaselines || len(data.Runs) != 50 {
		t.Fatalf("bounds exceeded: shapes=%d runs=%d", len(data.Shapes), len(data.Runs))
	}
	if data.Shapes[0].ShapeID != many[len(many)-1].ShapeID {
		t.Fatalf("newest shapes must be kept first, got %s", data.Shapes[0].ShapeID)
	}
	assertSnapshotsHaveRuns(t, data)
}

func assertSnapshotsHaveRuns(t *testing.T, d TrendStoreData) {
	t.Helper()
	runs := map[string]bool{}
	for _, r := range d.Runs {
		runs[r.RunID] = true
	}
	for _, s := range d.Shapes {
		for _, w := range s.WorkloadHistory {
			if !runs[w.RunID] {
				t.Fatalf("snapshot without a stored run context: %s", w.RunID)
			}
		}
		for _, e := range s.EfficiencyHistory {
			if !runs[e.RunID] {
				t.Fatalf("snapshot without a stored run context: %s", e.RunID)
			}
		}
	}
}

func TestTrendStorePersistsAcrossRestartWithBoundedRedactedText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "trend_baselines.json")
	s := OpenTrendStore(path)
	if st := s.Status(); st.Status != "empty" || !st.Persistent {
		t.Fatalf("new store: %+v", st)
	}
	run := trendRun("r1", trendNow)
	sh := shape(effSnap(run, 67, 2), wlSnap(run, 100, 5, 0.1))
	sh.ShapeSummary = strings.Repeat("x", 5000)
	sh.Efficiency.PlanSignatures = []string{"a", "b", "c", "d", "e", "f"}
	if err := s.Record(run, []TrendShapeSnapshot{sh}, DefaultTrendPolicy()); err != nil {
		t.Fatal(err)
	}
	reopened := OpenTrendStore(path)
	snap := reopened.Snapshot()
	if reopened.Status().Status != "ok" || len(snap.Shapes) != 1 || len(snap.Runs) != 1 {
		t.Fatalf("restart must reload baselines: %+v", reopened.Status())
	}
	got := snap.Shapes[0]
	if len(got.ShapeSummary) > trendMaxText || len(got.EfficiencyHistory) != 1 || *got.EfficiencyHistory[0].Score != 67 {
		t.Fatalf("persisted baseline: %+v", got)
	}
	raw, _ := os.ReadFile(path)
	var file TrendStoreData
	if err := json.Unmarshal(raw, &file); err != nil || file.SchemaVersion != TrendStoreSchemaVersion {
		t.Fatalf("file schema: %v %s", err, file.SchemaVersion)
	}

	big := historyShape(shape(effSnap(run, 67, 2), nil))
	big.EfficiencyHistory[0].PlanSignatures = []string{"a", "b", "c", "d", "e", "f"}
	big.ShapeSummary = strings.Repeat("y", 5000)
	edited, _ := json.Marshal(TrendStoreData{SchemaVersion: TrendStoreSchemaVersion, Runs: []TrendRunContext{run}, Shapes: []TrendShapeBaseline{big}})
	if err := os.WriteFile(path, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	got = OpenTrendStore(path).Snapshot().Shapes[0]
	if len(got.ShapeSummary) > trendMaxText || len(got.EfficiencyHistory[0].PlanSignatures) > trendMaxPlans {
		t.Fatalf("hand-edited file must be re-bounded on load: %d %d", len(got.ShapeSummary), len(got.EfficiencyHistory[0].PlanSignatures))
	}

	if err := reopened.Reset(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) || reopened.Status().Baselines != 0 || OpenTrendStore(path).Status().Baselines != 0 {
		t.Fatalf("reset must remove baselines on disk and in memory")
	}
}

func TestTrendStoreOldCorruptAndInvalidFiles(t *testing.T) {
	cases := []struct {
		name, content, status string
	}{
		{"corrupt json", "{not json", "corrupt_ignored"},
		{"legacy p95 map without schema", `{"shape_abc": 12.5}`, "unsupported_ignored"},
		{"future schema", `{"schema_version": "tb-9", "shapes": []}`, "unsupported_ignored"},
		{"wrong field types", `{"schema_version": "tb-1", "shapes": "nope"}`, "corrupt_ignored"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "trend_baselines.json")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			s := OpenTrendStore(path)
			st := s.Status()
			if st.Status != tc.status || st.Baselines != 0 || st.Reason == "" {
				t.Fatalf("status %+v, want %s", st, tc.status)
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "trend_baselines.json.") {
				t.Fatalf("unusable file must be moved aside, not deleted: %v", entries)
			}
			moved, _ := os.ReadFile(filepath.Join(dir, entries[0].Name()))
			if string(moved) != tc.content {
				t.Fatalf("moved file must keep its content")
			}
			run := trendRun("r1", trendNow)
			if err := s.Record(run, []TrendShapeSnapshot{shape(effSnap(run, 67, 2), nil)}, DefaultTrendPolicy()); err != nil || OpenTrendStore(path).Status().Baselines != 1 {
				t.Fatalf("store must recover and record: %v", err)
			}
			v := BuildTrendView(InsightsReport{Metadata: InsightsMetadata{Status: "ready"}, EfficiencyView: &EfficiencyView{}}, run, TrendStoreData{}, st, trendNow, viewOpts())
			if !strings.Contains(strings.Join(v.Limitations, " "), "Baseline store "+tc.status) {
				t.Fatalf("store problem must be disclosed: %v", v.Limitations)
			}
		})
	}

	t.Run("invalid entries dropped", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "trend_baselines.json")
		run := trendRun("r1", trendNow)
		bad := historyShape(shape(effSnap(run, 67, 2), nil))
		bad.EfficiencyHistory[0].RecordedAt = "yesterday"
		noID := historyShape(shape(nil, wlSnap(run, 10, 1, 0.1)))
		noID.ShapeID = ""
		good := historyShape(shape(nil, wlSnap(run, 10, 1, 0.1)))
		good.ShapeID = "shape_good"
		b, _ := json.Marshal(TrendStoreData{SchemaVersion: TrendStoreSchemaVersion, Runs: []TrendRunContext{run}, Shapes: []TrendShapeBaseline{bad, noID, good}})
		_ = os.WriteFile(path, b, 0o600)
		s := OpenTrendStore(path)
		if st := s.Status(); st.Baselines != 1 || !strings.Contains(st.Reason, "2 invalid baseline entries") {
			t.Fatalf("invalid entries: %+v", st)
		}
	})

	t.Run("oversized file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "trend_baselines.json")
		_ = os.WriteFile(path, make([]byte, trendStoreMaxBytes+1), 0o600)
		if st := OpenTrendStore(path).Status(); st.Status != "corrupt_ignored" {
			t.Fatalf("oversized file: %+v", st)
		}
	})

	t.Run("disabled store records nothing", func(t *testing.T) {
		s := DisabledTrendStore()
		run := trendRun("r1", trendNow)
		if err := s.Record(run, []TrendShapeSnapshot{shape(effSnap(run, 67, 2), nil)}, DefaultTrendPolicy()); err != nil || s.Status().Baselines != 0 || s.Status().Status != "disabled" {
			t.Fatalf("disabled store: %+v", s.Status())
		}
	})
}

func TestBuildTrendViewStates(t *testing.T) {
	run := trendRun("r2", trendNow)
	score := 50
	rep := InsightsReport{Metadata: InsightsMetadata{Status: "ready", ShapeIdentityVersion: ShapeIdentityVersion}, EfficiencyView: &EfficiencyView{Rows: []EfficiencyViewRow{{
		ShapeID: "shape_a", Namespace: "db.c", Operation: "find", Coverage: EffCoverageAssessed, Status: EfficiencyRated, Score: &score, Confidence: "high", OperationCount: 40, AvgMs: 1,
	}}}}
	v := BuildTrendView(rep, run, TrendStoreData{}, TrendStoreStatus{Status: "empty"}, trendNow, viewOpts())
	if v.Status != TrendNoBaseline || len(v.Rows) != 1 || v.Rows[0].Efficiency.State != TrendNoBaseline || v.Counts["comparability"][TrendMissing] != 1 {
		t.Fatalf("no baseline view: %+v", v)
	}
	if v.Criteria.Summary == "" || !strings.Contains(v.Criteria.Note, "not statistical significance tests") {
		t.Fatalf("criteria must be published as heuristics: %+v", v.Criteria)
	}
	rep.Metadata.Status = "disabled"
	if v := BuildTrendView(rep, run, TrendStoreData{}, TrendStoreStatus{Status: "ok"}, trendNow, viewOpts()); v.Status != "not_run" || len(v.Rows) != 0 {
		t.Fatalf("insights not ready: %+v", v)
	}
	if v := BuildTrendView(rep, run, TrendStoreData{}, TrendStoreStatus{Status: "disabled"}, trendNow, viewOpts()); v.Status != "disabled" {
		t.Fatalf("disabled: %+v", v)
	}
	rep.Metadata.Status = "ready"
	old := trendRun("r1", trendNow.Add(-time.Hour))
	other := historyShape(shape(nil, wlSnap(old, 10, 1, 0.1)))
	other.ShapeID = "shape_gone"
	v = BuildTrendView(rep, run, TrendStoreData{Runs: []TrendRunContext{old}, Shapes: []TrendShapeBaseline{other}}, TrendStoreStatus{Status: "ok"}, trendNow, viewOpts())
	if !strings.Contains(strings.Join(v.Limitations, " "), "1 baseline shapes were not seen") {
		t.Fatalf("unseen baselines must be disclosed: %v", v.Limitations)
	}
	self := historyShape(shape(nil, wlSnap(run, 10, 1, 0.1)))
	v = BuildTrendView(rep, run, TrendStoreData{Runs: []TrendRunContext{run}, Shapes: []TrendShapeBaseline{self}}, TrendStoreStatus{Status: "ok"}, trendNow, viewOpts())
	if v.Status != TrendNoBaseline || len(v.AvailableRuns) != 0 {
		t.Fatalf("the current run's own snapshots are never a baseline: %s %v", v.Status, v.AvailableRuns)
	}
}

// historyShape turns a one-run snapshot into a persisted shape.
func historyShape(s TrendShapeSnapshot) TrendShapeBaseline {
	b := TrendShapeBaseline{ShapeID: s.ShapeID, Namespace: s.Namespace, Operation: s.Operation, ShapeSummary: s.ShapeSummary}
	if s.Efficiency != nil {
		b.EfficiencyHistory = []TrendEfficiencySnapshot{*s.Efficiency}
	}
	if s.Workload != nil {
		b.WorkloadHistory = []TrendWorkloadSnapshot{*s.Workload}
	}
	return b
}
