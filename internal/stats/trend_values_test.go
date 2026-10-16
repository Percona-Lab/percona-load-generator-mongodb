package stats

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func valuesQuery(v interface{}) *ExecutableQuery {
	return &ExecutableQuery{Database: "db", Filter: map[string]interface{}{"a": v}}
}

// valuesReport is a ready report with one find shape (shape_a) whose two
// explained samples query a = vals[0] and a = vals[1]; retained holds a = r
// for each extra value.
func valuesReport(vals []interface{}, extra ...interface{}) (InsightsReport, []RepresentativeSample) {
	score := 80
	rep := InsightsReport{Metadata: InsightsMetadata{Status: "ready", ShapeIdentityVersion: ShapeIdentityVersion}, EfficiencyView: &EfficiencyView{Rows: []EfficiencyViewRow{{
		ShapeID: "shape_a", Namespace: "db.c", Operation: "find", ShapeSummary: "find on fields: a", Coverage: EffCoverageAssessed, Status: EfficiencyRated,
		Score: &score, Confidence: "high", OperationCount: 100, AvgMs: 5,
		Samples: []EfficiencyViewSample{{ExplainStatus: "explained", Evidence: "execution_stats", WinningPlan: "IXSCAN"}, {ExplainStatus: "explained", Evidence: "execution_stats", WinningPlan: "IXSCAN"}},
	}}}}
	cs := ExplainCoverageShape{ShapeID: "shape_a", Namespace: "db.c", Collection: "c", Operation: "find"}
	roles := []string{"median", "p95"}
	for i, v := range vals {
		cs.Samples = append(cs.Samples, ExplainSample{Role: roles[i%2], ClientLatencyMs: float64(i + 1), Status: "explained", Query: valuesQuery(v)})
	}
	rep.ExplainCoverage.Shapes = []ExplainCoverageShape{cs}
	var retained []RepresentativeSample
	for i, v := range append(append([]interface{}{}, vals...), extra...) {
		retained = append(retained, RepresentativeSample{ShapeID: "shape_a", Operation: "find", Database: "db", Collection: "c", DurationMs: float64(i) + 0.5, Query: valuesQuery(v)})
	}
	return rep, retained
}

func valuesRun(id string, at time.Time) TrendRunContext {
	r := trendRun(id, at)
	r.QueryValuesStored = true
	return r
}

func TestTrendValuesCanonicalExtJSONKeepsTypesAndSortOrder(t *testing.T) {
	oid := bson.NewObjectIDFromTimestamp(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	dec, _ := bson.ParseDecimal128("12.50")
	q := &ExecutableQuery{Database: "shop", Pipeline: []interface{}{
		map[string]interface{}{"$match": map[string]interface{}{
			"z_id": oid, "at": bson.DateTime(1700000000000), "n32": int32(7), "n64": int64(7), "price": dec,
		}},
		map[string]interface{}{"$sort": bson.D{{Key: "b", Value: -1}, {Key: "a", Value: 1}}},
	}}
	cmd, reason := TrendExecutedCommand("aggregate", "orders", q)
	if reason != "" {
		t.Fatal(reason)
	}
	text, err := CanonicalTrendCommand(cmd)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`{"$oid":"` + oid.Hex() + `"}`, `"at":{"$date":{"$numberLong":"1700000000000"}}`,
		`"n32":{"$numberInt":"7"}`, `"n64":{"$numberLong":"7"}`, `"price":{"$numberDecimal":"12.50"}`,
		`{"$sort":{"b":{"$numberInt":"-1"},"a":{"$numberInt":"1"}}}`,
		`{"aggregate":"orders","pipeline":`, `"$db":"shop"}`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("canonical command missing %s:\n%s", want, text)
		}
	}
	if strings.Index(text, `"at"`) > strings.Index(text, `"z_id"`) {
		t.Fatalf("map keys must be sorted so encoding is deterministic: %s", text)
	}
	for i := 0; i < 20; i++ {
		again, _ := CanonicalTrendCommand(cmd)
		if again != text {
			t.Fatal("canonical encoding must be deterministic")
		}
	}
	var back bson.D
	if err := bson.UnmarshalExtJSON([]byte(text), true, &back); err != nil {
		t.Fatalf("stored command must parse back as canonical Extended JSON: %v", err)
	}

	unordered := &ExecutableQuery{Database: "shop", Pipeline: []interface{}{map[string]interface{}{"$sort": map[string]interface{}{"b": -1, "a": 1}}}}
	if _, reason := TrendExecutedCommand("aggregate", "orders", unordered); reason != "sort_key_order_undefined" {
		t.Fatalf("a multi-key $sort in a map must not be stored alphabetized, got %q", reason)
	}
	rep, retained := valuesReport([]interface{}{1})
	retained = append(retained, RepresentativeSample{ShapeID: "shape_a", Operation: "aggregate", Collection: "c", Query: unordered})
	v := BuildTrendQueryValues(rep, retained)["shape_a"]
	if !v.Truncated || v.OmittedSamples != 1 || !strings.Contains(v.TruncationNote, "no defined key order") {
		t.Fatalf("an unordered-sort sample must be counted as left out: %+v", v)
	}
}

func TestTrendValuesHashEquality(t *testing.T) {
	hash := func(q *ExecutableQuery) string {
		s, reason := newTrendValueSample("median", true, 1, "find", "c", q)
		if reason != "" {
			t.Fatal(reason)
		}
		return s.ValuesHash
	}
	if hash(valuesQuery("x")) != hash(valuesQuery("x")) {
		t.Fatal("equal commands must hash equally")
	}
	if hash(valuesQuery("x")) == hash(valuesQuery("y")) {
		t.Fatal("different literals must hash differently")
	}
	if hash(valuesQuery(int32(5))) == hash(valuesQuery(int64(5))) {
		t.Fatal("int32 and int64 are different values in canonical Extended JSON")
	}
	twoKeys := func(order bson.D) *ExecutableQuery {
		return &ExecutableQuery{Database: "db", Pipeline: []interface{}{map[string]interface{}{"$sort": order}}}
	}
	a, _ := newTrendValueSample("median", true, 1, "aggregate", "c", twoKeys(bson.D{{Key: "a", Value: 1}, {Key: "b", Value: 1}}))
	b, _ := newTrendValueSample("median", true, 1, "aggregate", "c", twoKeys(bson.D{{Key: "b", Value: 1}, {Key: "a", Value: 1}}))
	if a.ValuesHash == b.ValuesHash {
		t.Fatal("sort precedence is part of the command")
	}
}

func TestTrendValuesRetainedSubsetIgnoresOrderAndLatency(t *testing.T) {
	extra := make([]interface{}, 0, trendValuesMaxRetained+3)
	for i := 0; i < trendValuesMaxRetained+3; i++ {
		extra = append(extra, fmt.Sprintf("r%d", i))
	}
	rep, retained := valuesReport([]interface{}{"m", "p"}, extra...)
	first := BuildTrendQueryValues(rep, retained)["shape_a"]
	shuffled := make([]RepresentativeSample, len(retained))
	for i, s := range retained {
		s.DurationMs = 7
		shuffled[len(retained)-1-i] = s
	}
	second := BuildTrendQueryValues(rep, shuffled)["shape_a"]
	if first.ValuesHash != second.ValuesHash {
		t.Fatal("the same retained values must keep the same subset regardless of order or latency ties")
	}
	if got, _ := compareTrendValues(&first, &second); got != TrendValuesSame {
		t.Fatalf("same retained values must compare as %s, got %s", TrendValuesSame, got)
	}
}

func TestTrendValuesCapsAndTruncation(t *testing.T) {
	big := strings.Repeat("x", trendValuesMaxSampleBytes)
	rep, retained := valuesReport([]interface{}{big, "small"})
	v := BuildTrendQueryValues(rep, retained)["shape_a"]
	if len(v.Samples) != 2 || !v.Samples[0].Truncated || v.Samples[0].Command != "" || v.Samples[0].CommandBytes <= trendValuesMaxSampleBytes || len(v.Samples[0].ValuesHash) != 64 {
		t.Fatalf("an oversized command keeps its hash and size only: %+v", v.Samples)
	}
	if !v.Truncated || !strings.Contains(v.TruncationNote, "hash only") || v.Samples[1].Command == "" {
		t.Fatalf("truncation must be flagged and explained: %+v", v)
	}

	extra := make([]interface{}, 0, 20)
	for i := 0; i < 20; i++ {
		extra = append(extra, i)
	}
	rep, retained = valuesReport([]interface{}{"m", "p"}, extra...)
	v = BuildTrendQueryValues(rep, retained)["shape_a"]
	explained, others := 0, 0
	for _, s := range v.Samples {
		if s.Explained {
			explained++
		} else if s.Role == TrendValueRoleRetained {
			others++
		}
	}
	if explained != 2 || others != trendValuesMaxRetained || v.Truncated {
		t.Fatalf("2 explained + at most %d retained, not explained: %d %d %+v", trendValuesMaxRetained, explained, others, v)
	}

	mid := strings.Repeat("y", 15<<10)
	extra = extra[:0]
	for i := 0; i < 8; i++ {
		extra = append(extra, mid+string(rune('a'+i)))
	}
	rep, retained = valuesReport([]interface{}{"m"}, extra...)
	v = BuildTrendQueryValues(rep, retained)["shape_a"]
	if trendValuesBytes(&v) > trendValuesMaxShapeBytes || v.OmittedSamples == 0 || !v.Truncated || !strings.Contains(v.TruncationNote, "the shape reached its limit") {
		t.Fatalf("the per-shape byte limit must be enforced and reported: %d bytes %+v", trendValuesBytes(&v), v.TruncationNote)
	}

	var b trendValuesBuilder
	b.runBytes = trendValuesMaxRunBytes - 10
	var got TrendQueryValues
	notes := map[string]bool{}
	n := 0
	s, _ := newTrendValueSample("median", true, 1, "find", "c", valuesQuery("abc"))
	b.add(&got, map[string]bool{}, s, &n, notes)
	finishTrendValues(&got, notes)
	if len(got.Samples) != 0 || got.OmittedSamples != 1 || !strings.Contains(got.TruncationNote, "the run reached its limit") {
		t.Fatalf("the per-run byte limit must be enforced and reported: %+v", got)
	}
}

func TestTrendValuesClassification(t *testing.T) {
	vals := func(cmds ...string) *TrendQueryValues {
		v := &TrendQueryValues{}
		for _, c := range cmds {
			v.Samples = append(v.Samples, TrendQueryValueSample{Command: c, ValuesHash: trendValuesHash(c)})
		}
		return v
	}
	for _, tc := range []struct {
		name      string
		cur, base *TrendQueryValues
		want      string
		match     int
	}{
		{"same", vals("a", "b"), vals("b", "a"), TrendValuesSame, 2},
		{"overlap", vals("a", "b"), vals("a", "c"), TrendValuesOverlapping, 1},
		{"subset", vals("a"), vals("a", "c"), TrendValuesOverlapping, 1},
		{"different", vals("a"), vals("c"), TrendValuesDifferent, 0},
		{"no baseline values", vals("a"), nil, TrendValuesNotRecorded, 0},
		{"empty current", vals(), vals("a"), TrendValuesNotRecorded, 0},
	} {
		if got, m := compareTrendValues(tc.cur, tc.base); got != tc.want || m != tc.match {
			t.Fatalf("%s: %s/%d, want %s/%d", tc.name, got, m, tc.want, tc.match)
		}
	}
}

// valuesStore records r1 (a = 1, 2) and returns the data plus the current
// report (a = curVals) and run.
func valuesStore(t *testing.T, baseStored bool, curVals ...interface{}) (TrendStoreData, InsightsReport, TrendRunContext) {
	t.Helper()
	r1 := trendRun("r1", trendNow.Add(-time.Hour))
	r1.QueryValuesStored = baseStored
	baseRep, baseRetained := valuesReport([]interface{}{1, 2})
	if baseStored {
		baseRep.TrendQueryValues = BuildTrendQueryValues(baseRep, baseRetained)
	}
	d := MergeTrendBaselines(TrendStoreData{}, r1, CurrentTrendBaselines(baseRep, r1), DefaultTrendPolicy())
	cur := valuesRun("cur", trendNow)
	rep, retained := valuesReport(curVals)
	rep.TrendQueryValues = BuildTrendQueryValues(rep, retained)
	return d, rep, cur
}

func TestTrendValuesRowsAreAnnotationsOnly(t *testing.T) {
	for _, tc := range []struct {
		name       string
		baseStored bool
		cur        []interface{}
		want       string
	}{
		{"same", true, []interface{}{1, 2}, TrendValuesSame},
		{"overlap", true, []interface{}{1, 3}, TrendValuesOverlapping},
		{"different", true, []interface{}{7, 8}, TrendValuesDifferent},
		{"baseline off", false, []interface{}{1, 2}, TrendValuesNotRecorded},
	} {
		d, rep, cur := valuesStore(t, tc.baseStored, tc.cur...)
		v := BuildTrendView(rep, cur, d, TrendStoreStatus{Status: "ok"}, trendNow, viewOpts())
		row := v.Rows[0]
		if row.QueryValues == nil || row.QueryValues.State != tc.want || row.QueryValues.BaselineRunID != "r1" || v.Counts["query_values"][tc.want] != 1 {
			t.Fatalf("%s: values annotation %+v", tc.name, row.QueryValues)
		}
		if tc.want == TrendValuesNotRecorded && !strings.Contains(row.QueryValues.BaselineReason, "values not recorded for the baseline run") {
			t.Fatalf("%s: reason %q", tc.name, row.QueryValues.BaselineReason)
		}
		if v.QueryValuesNotice == "" || !strings.Contains(strings.Join(v.Limitations, " "), "Annotation only") {
			t.Fatalf("%s: the view must say it carries query values and that they are an annotation", tc.name)
		}

		plain := rep
		plain.TrendQueryValues = nil
		plainRun := cur
		plainRun.QueryValuesStored = false
		dPlain := removeTrendValues(d)
		pv := BuildTrendView(plain, plainRun, dPlain, TrendStoreStatus{Status: "ok"}, trendNow, viewOpts())
		got, want := row, pv.Rows[0]
		got.QueryValues = nil
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: query values must not change labels or comparability:\n%+v\n%+v", tc.name, got, want)
		}
	}
}

func removeTrendValues(d TrendStoreData) TrendStoreData {
	d = cloneTrendData(d)
	for i := range d.Shapes {
		d.Shapes[i].QueryValuesHistory = nil
	}
	for i := range d.Runs {
		d.Runs[i].QueryValuesStored = false
	}
	return d
}

func TestTrendValuesOffKeepsOutputAndFilesUnchanged(t *testing.T) {
	rep, retained := valuesReport([]interface{}{1, 2})
	run := trendRun("r1", trendNow)
	plain := BuildTrendView(rep, run, TrendStoreData{}, TrendStoreStatus{Status: "empty"}, trendNow, viewOpts())
	plainStore := MergeTrendBaselines(TrendStoreData{}, run, CurrentTrendBaselines(rep, run), DefaultTrendPolicy())

	rep.TrendQueryValues = BuildTrendQueryValues(rep, retained)
	if len(rep.TrendQueryValues) == 0 {
		t.Fatal("fixture must have values")
	}
	off := BuildTrendView(rep, run, TrendStoreData{}, TrendStoreStatus{Status: "empty"}, trendNow, viewOpts())
	offStore := MergeTrendBaselines(TrendStoreData{}, run, CurrentTrendBaselines(rep, run), DefaultTrendPolicy())
	a, _ := json.Marshal(plain)
	b, _ := json.Marshal(off)
	sa, _ := json.Marshal(plainStore)
	sb, _ := json.Marshal(offStore)
	if string(a) != string(b) || string(sa) != string(sb) {
		t.Fatal("with the setting off the view and the stored data must be byte-identical")
	}
	for _, out := range []string{string(b), string(sb)} {
		if strings.Contains(out, "query_values") || strings.Contains(out, "numberInt") {
			t.Fatalf("no query values field may appear with the setting off: %s", out)
		}
	}
	rj, _ := json.Marshal(rep)
	if strings.Contains(string(rj), "numberInt") || strings.Contains(string(rj), "query_values") {
		t.Fatal("the report never serializes its literal values")
	}
}

func TestTrendValuesStoreRoundTripOldFilesAndReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trend_baselines.json")
	s := OpenTrendStore(path)
	d, rep, cur := valuesStore(t, true, 1, 3)
	if err := s.mutate(func(TrendStoreData) (TrendStoreData, error) { return d, nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.Record(cur, CurrentTrendBaselines(rep, cur), DefaultTrendPolicy()); err != nil {
		t.Fatal(err)
	}
	reopened := OpenTrendStore(path).Snapshot()
	vh := reopened.Shapes[0].QueryValuesHistory
	if len(vh) != 2 || vh[0].RunID != "cur" || vh[1].RunID != "r1" || len(vh[0].Samples) == 0 || !strings.Contains(vh[0].Samples[0].Command, `"$numberInt":"1"`) {
		t.Fatalf("values must persist per run, newest first: %+v", vh)
	}
	runs := OpenTrendStore(path).Runs(DefaultTrendPolicy(), trendNow)
	if !runs[0].QueryValues || !runs[1].QueryValues {
		t.Fatalf("runs with values are listed as such: %+v", runs)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	if err := s.DeleteRun("r1"); err != nil {
		t.Fatal(err)
	}
	if vh := s.Snapshot().Shapes[0].QueryValuesHistory; len(vh) != 1 || vh[0].RunID != "cur" {
		t.Fatalf("deleting a run deletes its values: %+v", vh)
	}
	if err := s.Reset(); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(path); err == nil || TrendDataHasQueryValues(OpenTrendStore(path).Snapshot()) {
		t.Fatalf("reset must delete stored values: %s", raw)
	}

	old := `{"schema_version":"tb-2","runs":[{"run_id":"r1","recorded_at":"2026-10-13T11:00:00Z","shape_identity_version":"si-3","score_version":"eff-3","sampling":{"sampling_rate":1,"slow_threshold_ms":200,"group_limit":50,"explain_enabled":true,"explain_max_samples":20},"server":{"topology":"mongod","source":"explain"},"workload":{"concurrency":4,"observed_operations":100}}],"shapes":[{"shape_id":"shape_a","namespace":"db.c","operation":"find","workload_history":[{"run_id":"r1","recorded_at":"2026-10-13T11:00:00Z","shape_identity_version":"si-3","operation_count":100,"avg_ms":5,"max_ms":9,"retained_samples":10,"total_operation_ms":500,"group_coverage_complete":true}]}]}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	o := OpenTrendStore(path)
	if st := o.Status(); st.Status != "ok" || st.Reason != "" || TrendDataHasQueryValues(o.Snapshot()) {
		t.Fatalf("a file without values loads unchanged: %+v", st)
	}
	rep2, retained := valuesReport([]interface{}{1})
	rep2.TrendQueryValues = BuildTrendQueryValues(rep2, retained)
	v := BuildTrendView(rep2, valuesRun("cur2", trendNow), o.Snapshot(), o.Status(), trendNow, viewOpts())
	if qv := v.Rows[0].QueryValues; qv == nil || qv.State != TrendValuesNotRecorded || !strings.Contains(qv.BaselineReason, "insights_trend_store_query_values was off") {
		t.Fatalf("an old baseline shows values not recorded for that run: %+v", qv)
	}
}

func TestTrendValuesSanitizeRejectsTamperedEntries(t *testing.T) {
	good := `{"find":"c","filter":{"a":{"$numberInt":"1"}}}`
	v := sanitizeTrendQueryValues(TrendQueryValues{RunID: "r1", RecordedAt: "2026-10-13T11:00:00Z", ValuesHash: "x", Samples: []TrendQueryValueSample{
		{Role: "median", Explained: true, Command: good, ValuesHash: strings.Repeat("0", 64)},
		{Role: "p95", Command: "{not json", ValuesHash: strings.Repeat("1", 64)},
		{Role: "retained", ValuesHash: "short"},
		{Role: "retained", ValuesHash: strings.Repeat("a", 64), CommandBytes: 99999},
		{Role: "retained", Command: strings.Repeat(" ", trendValuesMaxSampleBytes+1)},
	}})
	if len(v.Samples) != 2 || v.Samples[0].ValuesHash != trendValuesHash(good) || !v.Samples[1].Truncated || v.OmittedSamples != 3 || !v.Truncated {
		t.Fatalf("hashes are recomputed and invalid samples dropped with a count: %+v", v)
	}
	if v.ValuesHash == "x" || len(v.ValuesHash) != 64 {
		t.Fatalf("the snapshot hash is recomputed: %q", v.ValuesHash)
	}
}

func TestTrendValuesExportImportRoundTrip(t *testing.T) {
	a := OpenTrendStore(filepath.Join(t.TempDir(), "a.json"))
	d, rep, cur := valuesStore(t, true, 1, 3)
	_ = a.mutate(func(TrendStoreData) (TrendStoreData, error) { return d, nil })
	if err := a.Record(cur, CurrentTrendBaselines(rep, cur), DefaultTrendPolicy()); err != nil {
		t.Fatal(err)
	}
	x, err := a.Export(nil, "lab", trendNow)
	if err != nil || !x.ContainsQueryValues {
		t.Fatalf("an export with stored values says so: %v %v", err, x.ContainsQueryValues)
	}
	raw, _ := json.Marshal(x)
	b := OpenTrendStore(filepath.Join(t.TempDir(), "b.json"))
	res, err := b.Import(raw, "lab.json", DefaultTrendPolicy(), trendNow)
	if err != nil || len(res.Added) != 2 || !strings.Contains(strings.Join(res.Warnings, " "), "contains literal query values") {
		t.Fatalf("import keeps values and warns: %v %+v", err, res)
	}
	got := b.Snapshot().Shapes[0].QueryValuesHistory
	want := a.Snapshot().Shapes[0].QueryValuesHistory
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("values must survive export/import:\n%+v\n%+v", got, want)
	}

	plain, _ := shareStore(t, "p1")
	px, _ := plain.Export(nil, "", trendNow)
	pr, _ := json.Marshal(px)
	if px.ContainsQueryValues || strings.Contains(string(pr), "query_values") || strings.Contains(string(pr), "contains_query_values") {
		t.Fatalf("an export without stored values is unchanged: %s", pr)
	}
	if _, warns, err := ParseTrendImport(pr); err != nil || strings.Contains(strings.Join(warns, " "), "query values") {
		t.Fatalf("no values warning without values: %v %v", warns, err)
	}
}

func TestTrendSizeBoundRemovesValuesBeforeRuns(t *testing.T) {
	d := TrendStoreData{}
	p := TrendPolicy{KeepRuns: 10}
	for i := 0; i < 4; i++ {
		r := valuesRun("v"+string(rune('0'+i)), trendNow.Add(time.Duration(i-4)*time.Hour))
		snap := shape(effSnap(r, 50, 2), wlSnap(r, 100, 5, 0.1))
		snap.QueryValues = &TrendQueryValues{RunID: r.RunID, RecordedAt: r.RecordedAt, Samples: []TrendQueryValueSample{{Role: "median", Command: `"` + strings.Repeat("q", 8000) + `"`, ValuesHash: strings.Repeat("0", 64)}}}
		d = MergeTrendBaselines(d, r, []TrendShapeSnapshot{snap}, p)
	}
	full, _ := json.Marshal(d)
	out, reason := boundTrendSize(d, len(full)-10000)
	if len(out.Runs) != 4 || !strings.Contains(reason, "removed the stored query values of 2 oldest runs") {
		t.Fatalf("values go before runs, oldest first: %d runs, %q", len(out.Runs), reason)
	}
	dropped := map[string]bool{}
	for _, r := range out.Runs {
		dropped[r.RunID] = r.QueryValuesDropped
	}
	if !dropped["v0"] || !dropped["v1"] || dropped["v2"] || dropped["v3"] {
		t.Fatalf("the oldest runs are marked: %v", dropped)
	}
	ctx := testCtx(valuesRun("cur", trendNow), out.Runs...)
	if r := ctx.runs["v0"]; trendValuesReason("baseline", &r, nil) == "" || !strings.Contains(trendValuesReason("baseline", &r, nil), "size limit") {
		t.Fatal("a run whose values were removed says why")
	}
}
