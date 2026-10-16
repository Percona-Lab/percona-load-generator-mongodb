package stats

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestTrendRunContextCarriesReplayHashOnlyForCompleteReplays(t *testing.T) {
	hash := strings.Repeat("ab", 32)
	for _, tc := range []struct {
		replay *WorkloadReplayInfo
		want   string
	}{
		{replay: nil, want: ""},
		{replay: &WorkloadReplayInfo{HeaderHash: hash, Status: "cancelled"}, want: ""},
		{replay: &WorkloadReplayInfo{HeaderHash: hash, Status: "complete"}, want: hash},
	} {
		var rep InsightsReport
		rep.Metadata.Replay = tc.replay
		run := NewTrendRunContext(rep, TrendWorkload{}, "r1", "", time.Unix(0, 0))
		if run.Workload.ReplayHeaderHash != tc.want {
			t.Fatalf("replay %+v: hash = %q, want %q", tc.replay, run.Workload.ReplayHeaderHash, tc.want)
		}
	}
	raw, _ := json.Marshal(TrendWorkload{Concurrency: 4})
	if strings.Contains(string(raw), "replay_header_hash") {
		t.Fatalf("generated runs must not store a replay hash: %s", raw)
	}
}

func TestTrendAnnotatesSameRecordedWorkload(t *testing.T) {
	hash := strings.Repeat("cd", 32)
	base := TrendRunContext{Workload: TrendWorkload{Concurrency: 2, ReplayHeaderHash: hash}}
	cur := TrendRunContext{Workload: TrendWorkload{Concurrency: 2, ReplayHeaderHash: hash}}
	has := func(anns []TrendAnnotation) bool {
		for _, a := range anns {
			if a.Kind == "same_recorded_workload" {
				return strings.Contains(a.Detail, hash[:12])
			}
		}
		return false
	}
	if !has(trendWorkloadChanges(nil, nil, &base, cur)) {
		t.Fatal("missing same_recorded_workload annotation for runs replaying the same recording")
	}
	other := cur
	other.Workload.ReplayHeaderHash = strings.Repeat("ef", 32)
	generated := cur
	generated.Workload.ReplayHeaderHash = ""
	noHash := base
	noHash.Workload.ReplayHeaderHash = ""
	for name, pair := range map[string][2]TrendRunContext{"different recordings": {base, other}, "generated run": {base, generated}, "both generated": {noHash, generated}} {
		b := pair[0]
		if has(trendWorkloadChanges(nil, nil, &b, pair[1])) {
			t.Fatalf("%s: unexpected same_recorded_workload annotation", name)
		}
	}
}

func TestTrendRunContextCarriesTrafficFingerprintOnlyForCompleteRuns(t *testing.T) {
	fp := strings.Repeat("12", 32)
	for _, tc := range []struct {
		g    *GeneratedTrafficInfo
		want string
	}{
		{g: nil, want: ""},
		{g: &GeneratedTrafficInfo{Fingerprint: fp, Status: "incomplete"}, want: ""},
		{g: &GeneratedTrafficInfo{Fingerprint: fp, Status: "complete"}, want: fp},
	} {
		var rep InsightsReport
		rep.Metadata.GeneratedTraffic = tc.g
		run := NewTrendRunContext(rep, TrendWorkload{}, "r1", "", time.Unix(0, 0))
		if run.Workload.GeneratedTrafficFingerprint != tc.want {
			t.Fatalf("traffic %+v: fingerprint = %q, want %q", tc.g, run.Workload.GeneratedTrafficFingerprint, tc.want)
		}
	}
	raw, _ := json.Marshal(TrendWorkload{Concurrency: 4})
	if strings.Contains(string(raw), "generated_traffic") {
		t.Fatalf("runs without deterministic traffic must not store a fingerprint: %s", raw)
	}
	bad := sanitizeTrendRun(TrendRunContext{Workload: TrendWorkload{GeneratedTrafficFingerprint: "nope"}})
	if bad.Workload.GeneratedTrafficFingerprint != "" {
		t.Fatalf("invalid fingerprint kept: %q", bad.Workload.GeneratedTrafficFingerprint)
	}
}

func TestTrendSameGeneratedTrafficIsAnnotationAndSameValues(t *testing.T) {
	fp := strings.Repeat("34", 32)
	build := func(baseFP, curFP string) TrendView {
		r1 := trendRun("r1", trendNow.Add(-time.Hour))
		r1.Workload.GeneratedTrafficFingerprint = baseFP
		baseRep, _ := valuesReport([]interface{}{1, 2})
		d := MergeTrendBaselines(TrendStoreData{}, r1, CurrentTrendBaselines(baseRep, r1), DefaultTrendPolicy())
		cur := trendRun("cur", trendNow)
		cur.Workload.GeneratedTrafficFingerprint = curFP
		rep, _ := valuesReport([]interface{}{1, 2})
		return BuildTrendView(rep, cur, d, TrendStoreStatus{Status: "ok"}, trendNow, viewOpts())
	}
	same := build(fp, fp)
	row := same.Rows[0]
	q := row.QueryValues
	if q == nil || q.State != TrendValuesSame || q.Source != TrendValuesSourceGeneratedTraffic || q.BaselineRunID != "r1" || !strings.Contains(q.Label, fp[:12]) {
		t.Fatalf("equal fingerprints must count as same values: %+v", q)
	}
	if q.Current != nil || q.Baseline != nil || same.Counts["query_values"][TrendValuesSame] != 1 {
		t.Fatalf("no literal values may be invented: %+v", q)
	}
	found := false
	for _, a := range row.WorkloadChanges {
		found = found || a.Kind == "same_generated_traffic"
	}
	if !found {
		t.Fatalf("missing same_generated_traffic annotation: %+v", row.WorkloadChanges)
	}

	for name, v := range map[string]TrendView{"different": build(fp, strings.Repeat("56", 32)), "baseline generated": build("", fp), "none": build("", "")} {
		r := v.Rows[0]
		if r.QueryValues != nil {
			t.Fatalf("%s: unexpected values comparison %+v", name, r.QueryValues)
		}
		for _, a := range r.WorkloadChanges {
			if a.Kind == "same_generated_traffic" {
				t.Fatalf("%s: unexpected annotation", name)
			}
		}
		got := row
		got.QueryValues, got.WorkloadChanges, r.WorkloadChanges = nil, nil, nil
		if !reflect.DeepEqual(got, r) {
			t.Fatalf("%s: the fingerprint must not change labels or comparability:\n%+v\n%+v", name, got, r)
		}
	}
}

func TestTrendSameRecordingIsSameValuesWithoutLiterals(t *testing.T) {
	hash := strings.Repeat("7c", 32)
	build := func(baseHash, curHash, curFP string) TrendView {
		r1 := trendRun("r1", trendNow.Add(-time.Hour))
		r1.Workload.ReplayHeaderHash = baseHash
		baseRep, _ := valuesReport([]interface{}{1, 2})
		d := MergeTrendBaselines(TrendStoreData{}, r1, CurrentTrendBaselines(baseRep, r1), DefaultTrendPolicy())
		cur := trendRun("cur", trendNow)
		cur.Workload.ReplayHeaderHash, cur.Workload.GeneratedTrafficFingerprint = curHash, curFP
		rep, _ := valuesReport([]interface{}{3, 4})
		return BuildTrendView(rep, cur, d, TrendStoreStatus{Status: "ok"}, trendNow, viewOpts())
	}
	same := build(hash, hash, "")
	row := same.Rows[0]
	q := row.QueryValues
	if q == nil || q.State != TrendValuesSame || q.Source != TrendValuesSourceRecording || q.ReplayHeaderHash != hash || q.BaselineRunID != "r1" || !strings.Contains(q.Label, hash[:12]) {
		t.Fatalf("two replays of one recording must count as same values: %+v", q)
	}
	if q.Current != nil || q.Baseline != nil || same.Counts["query_values"][TrendValuesSame] != 1 {
		t.Fatalf("no literal values may be invented: %+v", q)
	}
	found := false
	for _, a := range row.WorkloadChanges {
		found = found || a.Kind == "same_recorded_workload"
	}
	if !found {
		t.Fatalf("missing same_recorded_workload annotation: %+v", row.WorkloadChanges)
	}
	for name, v := range map[string]TrendView{
		"different recordings": build(hash, strings.Repeat("8d", 32), ""),
		"replay vs generated":  build(hash, "", strings.Repeat("9e", 32)),
		"generated vs replay":  build("", hash, ""),
	} {
		r := v.Rows[0]
		if r.QueryValues != nil {
			t.Fatalf("%s: keeps the stored-values behavior (none stored): %+v", name, r.QueryValues)
		}
		got := row
		got.QueryValues, got.WorkloadChanges, r.WorkloadChanges = nil, nil, nil
		if !reflect.DeepEqual(got, r) {
			t.Fatalf("%s: the recording hash must not change labels or comparability:\n%+v\n%+v", name, got, r)
		}
	}
}

func TestSanitizeTrendRunDropsInvalidReplayHash(t *testing.T) {
	run := TrendRunContext{Workload: TrendWorkload{ReplayHeaderHash: "not-a-hash"}}
	if got := sanitizeTrendRun(run); got.Workload.ReplayHeaderHash != "" {
		t.Fatalf("invalid hash kept: %q", got.Workload.ReplayHeaderHash)
	}
	ok := strings.Repeat("0a", 32)
	run.Workload.ReplayHeaderHash = ok
	if got := sanitizeTrendRun(run); got.Workload.ReplayHeaderHash != ok {
		t.Fatalf("valid hash dropped: %q", got.Workload.ReplayHeaderHash)
	}
}

func TestInsightsMetadataCarriesRecordAndReplayInfo(t *testing.T) {
	c := NewCollector()
	c.SetWorkloadRecording(&WorkloadRecordingInfo{Path: "r.ndjson", OpsWritten: 3, Bytes: 100, Status: "complete"})
	match := true
	c.SetWorkloadReplay(&WorkloadReplayInfo{FileName: "r.ndjson", Status: "complete", OpsTotal: 3, OpsReplayed: 3, CommandsHashMatch: &match, Warnings: []string{"w"}})
	md := c.GetFinalInsights().Metadata
	if md.Recording == nil || md.Recording.OpsWritten != 3 || md.Replay == nil || md.Replay.OpsReplayed != 3 {
		t.Fatalf("metadata = %+v / %+v", md.Recording, md.Replay)
	}
	empty := NewCollector().GetFinalInsights().Metadata
	raw, _ := json.Marshal(empty)
	if strings.Contains(string(raw), `"recording"`) || strings.Contains(string(raw), `"replay"`) {
		t.Fatalf("default metadata must omit recording/replay: %s", raw)
	}
}
