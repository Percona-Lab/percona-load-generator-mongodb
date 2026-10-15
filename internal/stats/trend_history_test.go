package stats

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func recordRuns(t *testing.T, d TrendStoreData, p TrendPolicy, runs ...TrendRunContext) TrendStoreData {
	t.Helper()
	for _, r := range runs {
		d = MergeTrendBaselines(d, r, []TrendShapeSnapshot{shape(effSnap(r, 50, 2), wlSnap(r, 100, 5, 0.1))}, p)
	}
	return d
}

func runIDs(d TrendStoreData) []string {
	out := make([]string, 0, len(d.Runs))
	for _, r := range d.Runs {
		out = append(out, r.RunID)
	}
	return out
}

func TestTrendHistoryKeepsNewestNPlusPinned(t *testing.T) {
	p := TrendPolicy{KeepRuns: 3, MaxAge: TrendMaxBaselineAge}
	var runs []TrendRunContext
	for i := 0; i < 8; i++ {
		runs = append(runs, trendRun(fmt.Sprintf("r%d", i), trendNow.Add(time.Duration(i-10)*time.Hour)))
	}
	runs[0].Pinned = true
	d := recordRuns(t, TrendStoreData{}, p, runs...)
	if got := strings.Join(runIDs(d), ","); got != "r7,r6,r5,r0" {
		t.Fatalf("kept runs %s, want the 3 newest unpinned plus the pinned r0", got)
	}
	h := d.Shapes[0]
	if len(h.WorkloadHistory) != 4 || h.WorkloadHistory[0].RunID != "r7" || h.WorkloadHistory[3].RunID != "r0" || len(h.EfficiencyHistory) != 4 {
		t.Fatalf("history must keep one snapshot per kept run, newest first: %+v", h.WorkloadHistory)
	}
	assertSnapshotsHaveRuns(t, d)

	d = recordRuns(t, d, TrendPolicy{KeepRuns: 1}, trendRun("r8", trendNow.Add(-time.Minute)))
	if got := strings.Join(runIDs(d), ","); got != "r8,r0" {
		t.Fatalf("lowering N prunes the oldest unpinned runs and their snapshots, never the pinned run: %s", got)
	}
	if n := len(d.Shapes[0].WorkloadHistory); n != 2 {
		t.Fatalf("snapshots of pruned runs must go: %d", n)
	}
}

func TestTrendPinLimitRenameAndDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trend_baselines.json")
	s := OpenTrendStore(path)
	p := TrendPolicy{KeepRuns: 50}
	for i := 0; i < TrendMaxPinnedRuns+1; i++ {
		r := trendRun(fmt.Sprintf("r%02d", i), trendNow.Add(time.Duration(i)*time.Minute))
		if err := s.Record(r, []TrendShapeSnapshot{shape(effSnap(r, 50, 2), nil)}, p); err != nil {
			t.Fatal(err)
		}
	}
	yes := true
	for i := 0; i < TrendMaxPinnedRuns; i++ {
		if _, err := s.UpdateRun(fmt.Sprintf("r%02d", i), nil, &yes, p, trendNow); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.UpdateRun(fmt.Sprintf("r%02d", TrendMaxPinnedRuns), nil, &yes, p, trendNow); !errors.Is(err, ErrTrendPinLimit) {
		t.Fatalf("pin limit: %v", err)
	}
	if st := OpenTrendStore(path).Status(); st.PinnedRuns != TrendMaxPinnedRuns || st.MaxPinnedRuns != TrendMaxPinnedRuns {
		t.Fatalf("pinned runs must persist: %+v", st)
	}
	label := "  mongo-8.0.4\x07 tuned "
	got, err := s.UpdateRun("r00", &label, nil, p, trendNow)
	if err != nil || got.Label != "mongo-8.0.4 tuned" || !got.Pinned {
		t.Fatalf("rename must sanitize and keep the pin: %+v %v", got, err)
	}
	empty := ""
	if got, _ := s.UpdateRun("r00", &empty, nil, p, trendNow); got.Label != "mongo-8.0.4" {
		t.Fatalf("an empty label restores the default: %q", got.Label)
	}
	if _, err := s.UpdateRun("nope", &label, nil, p, trendNow); !errors.Is(err, ErrTrendRunNotFound) {
		t.Fatalf("unknown run: %v", err)
	}
	if err := s.DeleteRun("r01"); err != nil {
		t.Fatal(err)
	}
	d := OpenTrendStore(path).Snapshot()
	for _, sh := range d.Shapes {
		for _, e := range sh.EfficiencyHistory {
			if e.RunID == "r01" {
				t.Fatalf("delete must remove the run's snapshots")
			}
		}
	}
	if len(d.Runs) != TrendMaxPinnedRuns {
		t.Fatalf("runs after delete: %v", runIDs(d))
	}
	if err := DisabledTrendStore().DeleteRun("r00"); !errors.Is(err, ErrTrendStoreDisabled) {
		t.Fatalf("disabled store edits: %v", err)
	}
}

func TestTrendExpiryIsConfigurableAndSkipsPinnedRuns(t *testing.T) {
	cur := trendRun("cur", trendNow)
	old := trendRun("old", trendNow.Add(-40*24*time.Hour))
	base, now := shape(effSnap(old, 67, 2), wlSnap(old, 100, 5, 0.1)), shape(effSnap(cur, 100, 2), wlSnap(cur, 100, 9, 0.1))
	compare := func(p TrendPolicy, baseRun TrendRunContext) TrendRow {
		ctx := testCtx(cur, baseRun)
		ctx.policy = p.normalized()
		return compareTrendShape(now, &base, ctx, "")
	}
	if row := compare(DefaultTrendPolicy(), old); row.Comparability != TrendOutdated || !strings.Contains(row.Efficiency.Reasons[0], "maximum 30 days") {
		t.Fatalf("40-day baseline is outdated by default: %+v", row)
	}
	pinned := old
	pinned.Pinned = true
	if row := compare(DefaultTrendPolicy(), pinned); row.Comparability != TrendComparable || row.Efficiency.State != TrendImproved {
		t.Fatalf("a pinned run never expires: %s %v", row.Comparability, row.ComparabilityReasons)
	}
	if row := compare(TrendPolicy{MaxAge: 0, KeepRuns: 5}, old); row.Comparability != TrendComparable {
		t.Fatalf("max age 0 disables expiry: %s %v", row.Comparability, row.ComparabilityReasons)
	}
	week := TrendPolicy{MaxAge: 7 * 24 * time.Hour, KeepRuns: 5}
	ten := trendRun("ten", trendNow.Add(-10*24*time.Hour))
	base = shape(effSnap(ten, 67, 2), nil)
	if row := compare(week, ten); row.Comparability != TrendOutdated || !strings.Contains(row.Efficiency.Reasons[0], "maximum 7 days") {
		t.Fatalf("a 7-day expiry outdates a 10-day baseline: %+v", row)
	}
	if c := TrendCriteriaFor(week); c.MaxBaselineAgeHours != 7*24 || !strings.Contains(c.Summary, "older than 7 days") {
		t.Fatalf("criteria must publish the configured age: %+v", c)
	}
	if c := TrendCriteriaFor(TrendPolicy{KeepRuns: 5}); c.MaxBaselineAgeHours != 0 || !strings.Contains(c.Summary, "never outdated") {
		t.Fatalf("criteria with expiry off: %s", c.Summary)
	}
	sums := trendRunSummaries(TrendStoreData{Runs: []TrendRunContext{old, pinned}}, "", trendNow, DefaultTrendPolicy())
	if !sums[0].Outdated || sums[1].Outdated {
		t.Fatalf("run list must mark only unpinned old runs outdated: %+v", sums)
	}
}

func TestTrendStoreMigratesTB1(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trend_baselines.json")
	r1, r2 := trendRun("r1", trendNow.Add(-2*time.Hour)), trendRun("r2", trendNow.Add(-time.Hour))
	r2.Server = TrendServer{Topology: "unknown", Source: "none"}
	legacy := legacyTrendStoreData{SchemaVersion: trendLegacySchemaVersion, Runs: []TrendRunContext{r1, r2}, Shapes: []legacyTrendShape{{
		ShapeID: "shape_a", Namespace: "db.c", Operation: "find", ShapeSummary: "find on fields: a",
		Efficiency: effSnap(r1, 67, 2), Workload: wlSnap(r2, 100, 5, 0.1),
	}}}
	raw, _ := json.Marshal(legacy)
	if !strings.Contains(string(raw), `"efficiency":{`) || strings.Contains(string(raw), "history") {
		t.Fatalf("fixture must use the tb-1 layout: %s", raw)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	s := OpenTrendStore(path)
	st := s.Status()
	if st.Status != "ok" || !strings.Contains(st.Reason, "migrated from tb-1") || st.Baselines != 1 || st.Runs != 2 {
		t.Fatalf("tb-1 must load: %+v", st)
	}
	d := s.Snapshot()
	sh := d.Shapes[0]
	if len(sh.EfficiencyHistory) != 1 || sh.EfficiencyHistory[0].RunID != "r1" || len(sh.WorkloadHistory) != 1 || sh.WorkloadHistory[0].RunID != "r2" {
		t.Fatalf("each tb-1 snapshot becomes the first history entry: %+v", sh)
	}
	labels := map[string]string{}
	for _, r := range d.Runs {
		labels[r.RunID] = r.Label
	}
	if labels["r1"] != "mongo-8.0.4" || labels["r2"] != "run-"+trendNow.Add(-time.Hour).UTC().Format("20060102T150405Z") {
		t.Fatalf("migrated runs get default labels: %v", labels)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("a migrated file is not moved aside: %v", entries)
	}
	r3 := trendRun("r3", trendNow)
	if err := s.Record(r3, []TrendShapeSnapshot{shape(effSnap(r3, 100, 2), wlSnap(r3, 100, 4, 0.1))}, DefaultTrendPolicy()); err != nil {
		t.Fatal(err)
	}
	var head struct {
		SchemaVersion string `json:"schema_version"`
	}
	raw, _ = os.ReadFile(path)
	if err := json.Unmarshal(raw, &head); err != nil || head.SchemaVersion != TrendStoreSchemaVersion {
		t.Fatalf("the next save writes %s: %v %s", TrendStoreSchemaVersion, err, head.SchemaVersion)
	}
	if h := OpenTrendStore(path).Snapshot().Shapes[0]; len(h.EfficiencyHistory) != 2 || h.EfficiencyHistory[1].RunID != "r1" {
		t.Fatalf("migrated history must be kept after the next run: %+v", h.EfficiencyHistory)
	}
}

func TestTrendRunLabelDefaults(t *testing.T) {
	at := trendNow.UTC().Format(time.RFC3339)
	cases := []struct {
		label    string
		versions []string
		want     string
	}{
		{"", []string{"8.0.4"}, "mongo-8.0.4"},
		{"", []string{"7.0.12", "8.0.4"}, "mongo-7.0.12+8.0.4"},
		{"", nil, "run-20261013T120000Z"},
		{"   ", nil, "run-20261013T120000Z"},
		{" baseline\t-a ", []string{"8.0.4"}, "baseline-a"},
		{strings.Repeat("é", 40), nil, strings.Repeat("é", 32)},
	}
	for _, tc := range cases {
		if got := TrendRunLabel(tc.label, TrendServer{Versions: tc.versions}, at); got != tc.want {
			t.Fatalf("label %q versions %v: %q, want %q", tc.label, tc.versions, got, tc.want)
		}
	}
	rep := InsightsReport{Metadata: InsightsMetadata{Status: "ready"}}
	if run := NewTrendRunContext(rep, TrendWorkload{}, "id-1", "", trendNow); run.Label != "run-20261013T120000Z" {
		t.Fatalf("no explain evidence: label %q", run.Label)
	}
	if run := NewTrendRunContext(rep, TrendWorkload{}, "id-1", "mongo-8.0.4-rc", trendNow); run.Label != "mongo-8.0.4-rc" {
		t.Fatalf("configured label: %q", run.Label)
	}
}

// multiRunStore has runs r0 (other shape identity), r1 (score 33), r2
// (score 67), and an outdated r9; the current run scores 100.
func multiRunStore(t *testing.T) (TrendStoreData, TrendRunContext, InsightsReport) {
	t.Helper()
	p := TrendPolicy{KeepRuns: 10}
	r0 := trendRun("r0", trendNow.Add(-3*time.Hour))
	r0.ShapeIdentityVersion = "si-1"
	r1, r2 := trendRun("r1", trendNow.Add(-2*time.Hour)), trendRun("r2", trendNow.Add(-time.Hour))
	r9 := trendRun("r9", trendNow.Add(-45*24*time.Hour))
	d := TrendStoreData{}
	for _, x := range []struct {
		run   TrendRunContext
		score int
		avg   float64
	}{{r9, 10, 50}, {r0, 0, 40}, {r1, 33, 20}, {r2, 67, 10}} {
		snap := shape(effSnap(x.run, x.score, 2), wlSnap(x.run, 100, x.avg, 0.2))
		snap.Efficiency.ShapeIdentityVersion, snap.Workload.ShapeIdentityVersion = x.run.ShapeIdentityVersion, x.run.ShapeIdentityVersion
		d = MergeTrendBaselines(d, x.run, []TrendShapeSnapshot{snap}, p)
	}
	other := trendRun("r3", trendNow.Add(-30*time.Minute))
	only := shape(nil, wlSnap(other, 100, 1, 0.1))
	only.ShapeID = "shape_other"
	d = MergeTrendBaselines(d, other, []TrendShapeSnapshot{only}, p)
	score := 100
	rep := InsightsReport{Metadata: InsightsMetadata{Status: "ready", ShapeIdentityVersion: ShapeIdentityVersion}, EfficiencyView: &EfficiencyView{Rows: []EfficiencyViewRow{{
		ShapeID: "shape_a", Namespace: "db.c", Operation: "find", ShapeSummary: "find on fields: a", Coverage: EffCoverageAssessed, Status: EfficiencyRated,
		Score: &score, Confidence: "high", OperationCount: 100, AvgMs: 5,
		Samples: []EfficiencyViewSample{{ExplainStatus: "explained", Evidence: "execution_stats", WinningPlan: "IXSCAN"}, {ExplainStatus: "explained", Evidence: "execution_stats", WinningPlan: "IXSCAN"}},
	}}}}
	cur := trendRun("cur", trendNow)
	return d, cur, rep
}

func TestTrendViewBaselineSelection(t *testing.T) {
	d, cur, rep := multiRunStore(t)
	p := TrendPolicy{KeepRuns: 10, MaxAge: TrendMaxBaselineAge}
	view := func(opts TrendOptions) TrendRow {
		opts.Policy = p
		v := BuildTrendView(rep, cur, d, TrendStoreStatus{Status: "ok"}, trendNow, opts)
		if len(v.Rows) != 1 {
			t.Fatalf("rows: %+v", v.Rows)
		}
		return v.Rows[0]
	}
	row := view(TrendOptions{})
	if row.Baseline.EfficiencyRunID != "r2" || row.Baseline.WorkloadRunID != "r2" || row.Efficiency.State != TrendImproved || row.Baseline.EfficiencyRunLabel != "mongo-8.0.4" {
		t.Fatalf("default compares with the previous compatible run r2: %+v %+v", row.Baseline, row.Efficiency)
	}
	row = view(TrendOptions{Mode: TrendCompareRun, BaselineRunID: "r1"})
	if row.Baseline.EfficiencyRunID != "r1" || row.Efficiency.State != TrendImproved || *row.Efficiency.Delta != 67 {
		t.Fatalf("a chosen run is used for every metric: %+v", row.Efficiency)
	}
	row = view(TrendOptions{Mode: TrendCompareRun, BaselineRunID: "r0"})
	if row.Comparability != TrendIncompatible || !strings.Contains(strings.Join(row.ComparabilityReasons, " "), "shape_identity_version_changed") {
		t.Fatalf("an incompatible chosen run says why: %+v", row)
	}
	row = view(TrendOptions{Mode: TrendCompareRun, BaselineRunID: "r3"})
	if row.Comparability != TrendMissing || !strings.Contains(row.ComparabilityReasons[0], "not_in_baseline_run") {
		t.Fatalf("a run that did not record the shape: %+v", row)
	}

	v := BuildTrendView(rep, cur, d, TrendStoreStatus{Status: "ok"}, trendNow, TrendOptions{Policy: p, Mode: "pinned"})
	if v.Comparison.Mode != "previous" || v.Comparison.RequestedMode != "pinned" || !strings.Contains(v.Comparison.Note, "No pinned reference run") {
		t.Fatalf("pinned without a pinned run falls back with a note: %+v", v.Comparison)
	}
	for i := range d.Runs {
		d.Runs[i].Pinned = d.Runs[i].RunID == "r9"
	}
	v = BuildTrendView(rep, cur, d, TrendStoreStatus{Status: "ok"}, trendNow, TrendOptions{Policy: TrendPolicy{KeepRuns: 10, MaxAge: TrendMaxBaselineAge, DefaultComparison: "pinned"}})
	if v.Comparison.Mode != "pinned" || v.Comparison.BaselineRunID != "r9" || v.Rows[0].Comparability != TrendComparable || v.Rows[0].Latency.State != TrendImproved {
		t.Fatalf("the default comparison can be the pinned run, which never expires: %+v %+v", v.Comparison, v.Rows[0])
	}
	if len(v.AvailableRuns) != 5 || v.AvailableRuns[0].RunID != "r3" || !v.AvailableRuns[4].Pinned {
		t.Fatalf("available runs newest first: %+v", v.AvailableRuns)
	}
}

func TestTrendPreviousRunSkipsIncompatibleSnapshots(t *testing.T) {
	cur := trendRun("cur", trendNow)
	r1, r2 := trendRun("r1", trendNow.Add(-2*time.Hour)), trendRun("r2", trendNow.Add(-time.Hour))
	e2, w2 := effSnap(r2, 90, 2), wlSnap(r2, 100, 50, 0.1)
	e2.ScoreVersion = "qe-0"
	w2.ShapeIdentityVersion = "si-1"
	sh := TrendShapeBaseline{ShapeID: "shape_a", Namespace: "db.c", Operation: "find",
		EfficiencyHistory: []TrendEfficiencySnapshot{*e2, *effSnap(r1, 33, 2)},
		WorkloadHistory:   []TrendWorkloadSnapshot{*w2, *wlSnap(r1, 100, 5, 0.1)}}
	sel := selectPreviousBaseline(sh, testCtx(cur, r1, r2))
	if sel.Efficiency.RunID != "r1" || sel.Workload.RunID != "r1" {
		t.Fatalf("the previous compatible snapshot is the newest that passes the gates: eff %s wl %s", sel.Efficiency.RunID, sel.Workload.RunID)
	}
	only := TrendShapeBaseline{ShapeID: "shape_a", Namespace: "db.c", Operation: "find", EfficiencyHistory: []TrendEfficiencySnapshot{*e2}}
	row := compareTrendShape(shape(effSnap(cur, 100, 2), nil), selectPreviousBaseline(only, testCtx(cur, r2)), testCtx(cur, r2), "")
	if row.Efficiency.State != TrendNotComparable || !strings.Contains(row.Efficiency.Reasons[0], "score_version_changed") {
		t.Fatalf("with no compatible snapshot the newest is shown with its gate reason: %+v", row.Efficiency)
	}
}

func TestTrendMatrixColumnsAndGateReasons(t *testing.T) {
	d, cur, rep := multiRunStore(t)
	p := TrendPolicy{KeepRuns: 10, MaxAge: TrendMaxBaselineAge}
	m, err := BuildTrendMatrix(rep, cur, d, []string{"r1", "r9", "r2", "r0", "r3", "r2"}, trendNow, p)
	if err != nil {
		t.Fatal(err)
	}
	var cols []string
	for _, c := range m.Columns {
		cols = append(cols, c.RunID)
	}
	if strings.Join(cols, ",") != "cur,r3,r2,r1,r0,r9" {
		t.Fatalf("current column first, then chosen runs newest first without duplicates: %v", cols)
	}
	if len(m.Rows) != 1 || len(m.Rows[0].Cells) != len(m.Columns) {
		t.Fatalf("one cell per column: %+v", m.Rows)
	}
	cells := map[string]TrendMatrixCell{}
	for _, c := range m.Rows[0].Cells {
		cells[c.RunID] = c
	}
	c := cells["cur"]
	if !c.Current || *c.Score != 100 || *c.AvgMs != 5 || c.Comparability != "" {
		t.Fatalf("current cell: %+v", c)
	}
	if c := cells["r2"]; c.Comparability != TrendComparable || *c.Score != 67 || *c.AvgMs != 10 || *c.ImpactShare != 0.2 || c.Efficiency != TrendImproved || c.Latency != TrendImproved || c.Impact == "" || len(c.Reasons) != 0 {
		t.Fatalf("comparable column: %+v", c)
	}
	if c := cells["r0"]; c.Comparability != TrendIncompatible || !strings.Contains(strings.Join(c.Reasons, " "), "shape_identity_version_changed") || c.Efficiency != TrendNotComparable {
		t.Fatalf("incompatible column shows the gate reason: %+v", c)
	}
	if c := cells["r9"]; c.Comparability != TrendOutdated || !strings.Contains(strings.Join(c.Reasons, " "), "baseline_outdated") || c.AvgMs == nil {
		t.Fatalf("outdated column keeps its values and shows the reason: %+v", c)
	}
	if c := cells["r3"]; c.Recorded || c.Comparability != TrendMissing || !strings.Contains(c.Reasons[0], "not_in_baseline_run") {
		t.Fatalf("a run without the shape: %+v", c)
	}

	if _, err := BuildTrendMatrix(rep, cur, d, []string{"nope"}, trendNow, p); !errors.Is(err, ErrTrendRunNotFound) {
		t.Fatalf("unknown run: %v", err)
	}
	if _, err := BuildTrendMatrix(rep, cur, d, nil, trendNow, p); err == nil {
		t.Fatal("an empty selection must be rejected")
	}
	many := TrendStoreData{}
	var ids []string
	for i := 0; i <= TrendMaxMatrixRuns; i++ {
		r := trendRun(fmt.Sprintf("m%d", i), trendNow.Add(-time.Duration(i+1)*time.Minute))
		many = recordRuns(t, many, TrendPolicy{KeepRuns: 20}, r)
		ids = append(ids, r.RunID)
	}
	if _, err := BuildTrendMatrix(rep, cur, many, ids, trendNow, p); err == nil || !strings.Contains(err.Error(), "1 to 8") {
		t.Fatalf("more than %d runs must be rejected: %v", TrendMaxMatrixRuns, err)
	}
	withCur := recordRuns(t, many, TrendPolicy{KeepRuns: 20}, cur)
	if _, err := BuildTrendMatrix(rep, cur, withCur, []string{"cur"}, trendNow, p); err == nil {
		t.Fatal("the current run is not a baseline column")
	}
}

func shareStore(t *testing.T, ids ...string) (*TrendStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trend_baselines.json")
	s := OpenTrendStore(path)
	for i, id := range ids {
		r := trendRun(id, trendNow.Add(time.Duration(i-len(ids))*time.Hour))
		if err := s.Record(r, []TrendShapeSnapshot{shape(effSnap(r, 40+i, 2), wlSnap(r, 100, float64(10+i), 0.1))}, TrendPolicy{KeepRuns: 20}); err != nil {
			t.Fatal(err)
		}
	}
	return s, path
}

func TestTrendExportImportRoundTrip(t *testing.T) {
	a, _ := shareStore(t, "a1", "a2", "a3")
	yes := true
	if _, err := a.UpdateRun("a1", nil, &yes, TrendPolicy{KeepRuns: 20}, trendNow); err != nil {
		t.Fatal(err)
	}
	x, err := a.Export(nil, " lab-host\x01 ", trendNow)
	if err != nil || len(x.Runs) != 3 || x.Format != TrendExportFormat || x.SchemaVersion != TrendStoreSchemaVersion || x.Source != "lab-host" {
		t.Fatalf("export all: %+v %v", x, err)
	}
	one, _ := a.Export([]string{"a2"}, "", trendNow)
	if len(one.Runs) != 1 || len(one.Shapes) != 1 || len(one.Shapes[0].EfficiencyHistory) != 1 || one.Shapes[0].EfficiencyHistory[0].RunID != "a2" {
		t.Fatalf("export selected keeps only those runs' snapshots: %+v", one)
	}
	if _, err := a.Export([]string{"zz"}, "", trendNow); !errors.Is(err, ErrTrendRunNotFound) {
		t.Fatalf("unknown export id: %v", err)
	}
	raw, _ := json.Marshal(x)

	b, bPath := shareStore(t, "b1")
	p := TrendPolicy{KeepRuns: 20}
	res, err := b.Import(raw, "upload.json", p, trendNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Added) != 3 || len(res.Skipped) != 0 || len(res.NotKept) != 0 || res.Source != "lab-host" {
		t.Fatalf("import result: %+v", res)
	}
	for _, r := range res.Added {
		if r.Source != "lab-host" || r.ImportedAt == "" {
			t.Fatalf("imported runs carry their source: %+v", r)
		}
		if r.RunID == "a1" && !r.Pinned {
			t.Fatalf("a pinned run stays pinned: %+v", r)
		}
	}
	got := OpenTrendStore(bPath).Snapshot()
	if len(got.Runs) != 4 || len(got.Shapes[0].EfficiencyHistory) != 4 {
		t.Fatalf("imported runs and snapshots must be stored: runs %v", runIDs(got))
	}
	again, err := b.Import(raw, "upload.json", p, trendNow)
	if err != nil || len(again.Added) != 0 || len(again.Skipped) != 3 || !strings.Contains(again.Skipped[0].Reason, "duplicate") {
		t.Fatalf("re-import skips duplicates: %+v %v", again, err)
	}
	if n := len(OpenTrendStore(bPath).Snapshot().Shapes[0].EfficiencyHistory); n != 4 {
		t.Fatalf("duplicates must not add snapshots: %d", n)
	}

	unnamed := x
	unnamed.Source = ""
	raw2, _ := json.Marshal(unnamed)
	c, _ := shareStore(t)
	if res, _ := c.Import(raw2, "/tmp/team-baselines.json", p, trendNow); res.Source != "team-baselines" {
		t.Fatalf("without a source label the file name is used: %q", res.Source)
	}

	small, _ := shareStore(t, "s1", "s2")
	older := x
	for i := range older.Runs {
		older.Runs[i].Pinned = false
	}
	raw3, _ := json.Marshal(older)
	res, err = small.Import(raw3, "", TrendPolicy{KeepRuns: 2}, trendNow)
	if err != nil || len(res.Added) != 1 || res.Added[0].RunID != "a3" || len(res.NotKept) != 2 || !strings.Contains(res.NotKept[0].Reason, "newest 2 unpinned runs") {
		t.Fatalf("runs pruned by the history setting are reported as not kept: %+v %v", res, err)
	}
}

func TestTrendImportRejectsInvalidFiles(t *testing.T) {
	s, path := shareStore(t, "b1")
	before, _ := os.ReadFile(path)
	valid, _ := s.Export(nil, "", trendNow)
	tooMany := valid
	for i := 0; i < TrendMaxRuns; i++ {
		r := trendRun(fmt.Sprintf("x%02d", i), trendNow)
		tooMany.Runs = append(tooMany.Runs, r)
	}
	tooManyRaw, _ := json.Marshal(tooMany)
	cases := map[string]struct {
		body []byte
		want string
	}{
		"empty":       {[]byte("  "), "empty"},
		"not json":    {[]byte("{nope"), "invalid JSON"},
		"wrong types": {[]byte(`{"schema_version":"tb-2","runs":"x"}`), "invalid JSON"},
		"unsupported": {[]byte(`{"schema_version":"tb-1","runs":[],"shapes":[]}`), `unsupported schema "tb-1"`},
		"no schema":   {[]byte(`{"runs":[],"shapes":[]}`), `unsupported schema "missing"`},
		"format":      {[]byte(`{"format":"other","schema_version":"tb-2"}`), "format"},
		"oversized":   {append([]byte(`{"schema_version":"tb-2","pad":"`), append(make([]byte, TrendFileMaxBytes), '"', '}')...), "limit"},
		"too many":    {tooManyRaw, "the limit is 64"},
		"no runs":     {[]byte(`{"schema_version":"tb-2","runs":[{"run_id":"bad id!","recorded_at":"x"}],"shapes":[]}`), "no valid runs"},
	}
	for name, tc := range cases {
		_, err := s.Import(tc.body, "f.json", DefaultTrendPolicy(), trendNow)
		var ie *TrendImportError
		if !errors.As(err, &ie) || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: %v, want an import error containing %q", name, err, tc.want)
		}
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("a rejected import must not change the store")
	}
	if _, err := DisabledTrendStore().Import([]byte("{}"), "", DefaultTrendPolicy(), trendNow); !errors.Is(err, ErrTrendStoreDisabled) {
		t.Fatalf("disabled: %v", err)
	}
}

func TestTrendImportKeepsOnlyRedactedFields(t *testing.T) {
	s, path := shareStore(t)
	file := `{"format":"plgm-trend-export","schema_version":"tb-2","source":"evil","uri":"mongodb://user:pw@host",
"runs":[{"run_id":"imp-1","recorded_at":"2026-10-13T11:00:00Z","label":"x","query":"secret-literal","server":{"versions":["8.0.4"],"topology":"mongod","source":"explain","host":"secret-host"},"shape_identity_version":"si-3","score_version":"se-1"}],
"shapes":[{"shape_id":"shape_a","namespace":"db.c","operation":"find","filter":{"ssn":"secret-123"},
"workload_history":[{"run_id":"imp-1","recorded_at":"2026-10-13T11:00:00Z","shape_identity_version":"si-3","operation_count":100,"avg_ms":2,"command":"secret-cmd"}]}]}`
	res, err := s.Import([]byte(file), "", DefaultTrendPolicy(), trendNow)
	if err != nil || len(res.Added) != 1 {
		t.Fatalf("import: %+v %v", res, err)
	}
	if !strings.Contains(strings.Join(res.Warnings, " "), "fields outside the trend schema were ignored") {
		t.Fatalf("ignored fields must be reported: %v", res.Warnings)
	}
	raw, _ := os.ReadFile(path)
	for _, leak := range []string{"secret", "mongodb://", "pw@", `"filter"`, `"query"`, `"command"`, `"uri"`} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("store must hold only redacted trend fields, found %q in %s", leak, raw)
		}
	}
}

func TestTrendStoreConcurrentWritersKeepEveryRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared", "trend_baselines.json")
	p := TrendPolicy{KeepRuns: 50}
	stores := []*TrendStore{OpenTrendStore(path), OpenTrendStore(path), OpenTrendStore(path)}
	const perWriter = 8
	var wg sync.WaitGroup
	errs := make(chan error, len(stores)*perWriter)
	for w, s := range stores {
		wg.Add(1)
		go func(w int, s *TrendStore) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				r := trendRun(fmt.Sprintf("w%d-%02d", w, i), trendNow.Add(time.Duration(w*perWriter+i)*time.Second))
				sh := shape(nil, wlSnap(r, 10, 1, 0.1))
				sh.ShapeID = fmt.Sprintf("shape_w%d", w)
				errs <- s.Record(r, []TrendShapeSnapshot{sh}, p)
			}
		}(w, s)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	d := OpenTrendStore(path).Snapshot()
	if len(d.Runs) != len(stores)*perWriter || len(d.Shapes) != len(stores) {
		t.Fatalf("every writer's runs must survive: %d runs, %d shapes", len(d.Runs), len(d.Shapes))
	}
	assertSnapshotsHaveRuns(t, d)
	if got := stores[0].Snapshot(); len(got.Runs) != len(d.Runs) {
		t.Fatalf("a reader must see other writers' runs: %d", len(got.Runs))
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("no lock or temp files may remain: %v", entries)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
}

func TestTrendStoreLockWaitAndStaleLock(t *testing.T) {
	oldWait, oldStale := trendLockWait, trendLockStaleAfter
	t.Cleanup(func() { trendLockWait, trendLockStaleAfter = oldWait, oldStale })
	trendLockWait, trendLockStaleAfter = 50*time.Millisecond, time.Hour
	s, path := shareStore(t, "r1")
	before, _ := os.ReadFile(path)
	lock := path + ".lock"
	if err := os.WriteFile(lock, []byte("pid 1"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := trendRun("r2", trendNow)
	err := s.Record(r, []TrendShapeSnapshot{shape(nil, wlSnap(r, 10, 1, 0.1))}, DefaultTrendPolicy())
	if err == nil || !strings.Contains(err.Error(), "locked by another process") {
		t.Fatalf("a held lock must time out with a clear error: %v", err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("the file must not change while another process holds the lock")
	}
	old := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(lock, old, old)
	if err := s.Record(r, []TrendShapeSnapshot{shape(nil, wlSnap(r, 10, 1, 0.1))}, DefaultTrendPolicy()); err != nil {
		t.Fatalf("a stale lock is taken over: %v", err)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatal("the lock must be released")
	}
}

func TestTrendSizeBoundDropsOldestUnpinnedRuns(t *testing.T) {
	d := TrendStoreData{}
	p := TrendPolicy{KeepRuns: 50}
	for i := 0; i < 10; i++ {
		r := trendRun(fmt.Sprintf("r%d", i), trendNow.Add(time.Duration(i)*time.Minute))
		r.Pinned = i == 0
		d = recordRuns(t, d, p, r)
	}
	full, _ := json.Marshal(d)
	got, note := boundTrendSize(d, len(full)/2)
	b, _ := json.Marshal(got)
	if len(b) > len(full)/2 || !strings.Contains(note, "dropped") {
		t.Fatalf("size bound: %d bytes (limit %d), %q", len(b), len(full)/2, note)
	}
	ids := strings.Join(runIDs(got), ",")
	if !strings.HasPrefix(ids, "r9,") || !strings.Contains(ids, "r0") || strings.Contains(ids, "r1,") {
		t.Fatalf("the newest and pinned runs stay, the oldest unpinned go first: %s", ids)
	}
	if same, note := boundTrendSize(d, len(full)); note != "" || len(same.Runs) != 10 {
		t.Fatalf("data within the limit is unchanged: %q", note)
	}
}

func TestTrendStoreSeesOtherProcessWritesAndReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trend_baselines.json")
	a, b := OpenTrendStore(path), OpenTrendStore(path)
	r := trendRun("r1", trendNow)
	if err := a.Record(r, []TrendShapeSnapshot{shape(nil, wlSnap(r, 10, 1, 0.1))}, DefaultTrendPolicy()); err != nil {
		t.Fatal(err)
	}
	if st := b.Status(); st.Runs != 1 || st.Baselines != 1 {
		t.Fatalf("b must reload a's write: %+v", st)
	}
	if err := a.Reset(); err != nil {
		t.Fatal(err)
	}
	if st := b.Status(); st.Runs != 0 || !strings.Contains(st.Reason, "removed by another process") {
		t.Fatalf("b must see a's reset: %+v", st)
	}
}

func TestResolveTrendStorePath(t *testing.T) {
	if p, off := ResolveTrendStorePath("OFF"); !off || p != "" {
		t.Fatalf("off: %q %v", p, off)
	}
	if p, off := ResolveTrendStorePath(" /tmp/x.json "); off || p != "/tmp/x.json" {
		t.Fatalf("explicit: %q", p)
	}
	if p, _ := ResolveTrendStorePath(""); !strings.HasSuffix(p, filepath.Join("plgm", "trend_baselines.json")) {
		t.Fatalf("default location: %q", p)
	}
	if s := OpenTrendStoreSetting("off"); !s.Disabled() || s.Status().Status != "disabled" {
		t.Fatalf("disabled store: %+v", s.Status())
	}
}
