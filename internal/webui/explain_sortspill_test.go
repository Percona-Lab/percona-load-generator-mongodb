package webui

import (
	"math"
	"os"
	"strings"
	"testing"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// testdata/explain_sortspill_mongod_8.0.4.json holds executionStats explains
// from a disposable standalone MongoDB 8.0.4 over 5,000 documents with only
// the _id index, under forceClassicEngine and trySbeEngine. The *_spill cases
// lowered internalQueryMaxBlockingSortMemoryUsageBytes and
// internalDocumentSourceGroupMaxMemoryBytes to 64 KiB on that server; the
// commands carry no allowDiskUse. rejectedPlans and the queryPlanner trees
// (which repeat memLimit) are kept.
func loadSortSpillExplains(t *testing.T) map[string]bson.M {
	t.Helper()
	data, err := os.ReadFile("testdata/explain_sortspill_mongod_8.0.4.json")
	if err != nil {
		t.Fatal(err)
	}
	var raw bson.M
	if err := bson.UnmarshalExtJSON(data, false, &raw); err != nil {
		t.Fatal(err)
	}
	out := make(map[string]bson.M, len(raw))
	for k, v := range raw {
		doc, ok := explainDoc(v)
		if !ok {
			t.Fatalf("%s is not a document", k)
		}
		out[k] = bson.M(doc)
	}
	return out
}

func counterVal(c *stats.ExplainCounter) int64 {
	if c == nil {
		return nr
	}
	return c.Value
}

func TestCollectSortSpillRealMongod(t *testing.T) {
	ex := loadSortSpillExplains(t)
	type stage struct {
		path, stage, engine                               string
		sortBytes, spills, storage, spilledData, memLimit int64
		usedDisk                                          bool
		time                                              int64
		caveat                                            bool
	}
	const es = "executionStats.executionStages"
	cases := []struct {
		key, op, status string
		stages          []stage
	}{
		{"forceClassicEngine_find_sort_mem", "find", stats.SortSpillMeasured, []stage{
			{es, "SORT", "classic", 675000, 0, 0, nr, 104857600, false, 0, false}}},
		{"forceClassicEngine_find_sort_spill", "find", stats.SortSpillMeasured, []stage{
			{es, "SORT", "classic", 675000, 23, 241585, nr, 65536, true, 6, false}}},
		// The slot-based in-memory sort reports totalDataSizeSorted 0 for 2,500 sorted documents.
		{"trySbeEngine_find_sort_mem", "find", stats.SortSpillPartial, []stage{
			{es, "sort", "sbe", 0, 0, 0, nr, 104857600, false, 0, true}}},
		{"trySbeEngine_find_sort_spill", "find", stats.SortSpillMeasured, []stage{
			{es, "sort", "sbe", 708480, 26, 244204, nr, 65536, true, 7, false}}},
		{"forceClassicEngine_agg_group_sort_mem", "aggregate", stats.SortSpillMeasured, []stage{
			{"stages[1]", "$group", "pipeline", nr, 0, 0, 0, nr, false, 4, false},
			{"stages[2]", "$sort", "pipeline", 2905000, 0, 0, nr, nr, false, 4, false}}},
		{"forceClassicEngine_agg_group_sort_spill", "aggregate", stats.SortSpillMeasured, []stage{
			{"stages[1]", "$group", "pipeline", nr, 35, 72223, 2240000, nr, true, 8, false},
			{"stages[2]", "$sort", "pipeline", 2905000, 100, 575370, nr, nr, true, 33, false}}},
		{"trySbeEngine_agg_group_sort_spill", "aggregate", stats.SortSpillMeasured, []stage{
			{es, "sort", "sbe", 1836688, 62, 485098, nr, 65536, true, 22, false},
			{es + ".inputStage.inputStage.inputStage.inputStage", "group", "sbe", nr, 0, 0, 0, nr, false, 0, false}}},
		{"forceClassicEngine_agg_facet_sort_mem", "aggregate", stats.SortSpillMeasured, []stage{
			{"stages[1].$facet.byG[1]", "$group", "pipeline", nr, 0, 0, 0, nr, false, 0, false},
			{"stages[1].$facet.top[1]", "$sort", "pipeline", 1074, 0, 0, nr, nr, false, 1, false}}},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			doc, ok := ex[tc.key]
			if !ok {
				t.Fatalf("fixture %s missing", tc.key)
			}
			s := normalizeExplainMetrics(doc, tc.op).SortSpill
			if s == nil || s.Status != tc.status {
				t.Fatalf("status %+v, want %s", s, tc.status)
			}
			// $facet itself reports no fields and is not listed.
			got, want := s.Stages, tc.stages
			if len(got) != len(want) {
				t.Fatalf("stages %d, want %d: %+v", len(got), len(want), got)
			}
			for i, w := range want {
				g := got[i]
				if g.Path != w.path || g.Stage != w.stage || g.Engine != w.engine || g.Namespace != "plgm_fixture_sortspill.c" {
					t.Fatalf("stage %d identity %+v, want %+v", i, g, w)
				}
				if strings.Contains(g.Path, "queryPlanner") || strings.Contains(g.Path, "rejectedPlans") {
					t.Fatalf("planner tree read as a measurement: %s", g.Path)
				}
				vals := [][2]int64{
					{counterVal(g.SortBytesProcessed), w.sortBytes}, {counterVal(g.Spills), w.spills},
					{counterVal(g.SpillStorageBytes), w.storage}, {counterVal(g.SpilledBytes), w.spilledData},
					{counterVal(g.MemoryLimitBytes), w.memLimit}, {counterVal(g.TimeMsEstimate), w.time},
				}
				for j, v := range vals {
					if v[0] != v[1] {
						t.Fatalf("stage %s value %d = %d, want %d: %+v", g.Path, j, v[0], v[1], g)
					}
				}
				if g.UsedDisk == nil || *g.UsedDisk != w.usedDisk {
					t.Fatalf("stage %s usedDisk %v, want %v", g.Path, g.UsedDisk, w.usedDisk)
				}
				if (len(g.Caveats) > 0) != w.caveat || (g.Sort && g.SortBytesUsable() == w.caveat) {
					t.Fatalf("stage %s caveats %v, want caveat=%v", g.Path, g.Caveats, w.caveat)
				}
				if w.caveat && (len(g.Caveats) != 1 || g.Caveats[0] != stats.CaveatSBESortBytesWithoutSpill ||
					g.SortBytesProcessed.Source != g.Path+".totalDataSizeSorted") {
					t.Fatalf("stage %s: only the raw totalDataSizeSorted 0 is flagged: %+v", g.Path, g)
				}
				if g.ServerVersion != "8.0.4" {
					t.Fatalf("stage %s server version %q", g.Path, g.ServerVersion)
				}
			}
		})
	}
	// Classic $group reports peak memory per accumulator, kept by name.
	g := normalizeExplainMetrics(ex["forceClassicEngine_agg_group_sort_spill"], "aggregate").SortSpill.Stages[0]
	if len(g.PeakMemory) != 2 || g.PeakMemory[0].Scope != "accumulator n" || g.PeakMemory[0].Value != 19992 ||
		g.PeakMemory[1].Scope != "accumulator p" || g.PeakMemory[1].Value != 43512 {
		t.Fatalf("accumulator memory: %+v", g.PeakMemory)
	}
	if g.SpilledRecords == nil || g.SpilledRecords.Value != 5000 || g.SpilledBytes.Source != "stages[1].numBytesSpilledEstimate" {
		t.Fatalf("classic $group spill fields: %+v", g)
	}
}

func TestCollectSortSpillBSONNumericTypes(t *testing.T) {
	src := bson.D{
		{Key: "queryPlanner", Value: bson.D{{Key: "namespace", Value: "db.c"}}},
		{Key: "executionStats", Value: bson.D{{Key: "executionStages", Value: bson.D{
			{Key: "stage", Value: "SORT"}, {Key: "nReturned", Value: int32(5)},
			{Key: "totalDataSizeSorted", Value: int64(1 << 40)},
			{Key: "spills", Value: int32(7)},
			{Key: "spilledDataStorageSize", Value: float64(4096.9)},
			{Key: "memLimit", Value: int32(104857600)},
			{Key: "usedDisk", Value: true},
			{Key: "executionTimeMillisEstimate", Value: math.NaN()},
			{Key: "spilledRecords", Value: "12"},
		}}}},
	}
	raw, err := bson.Marshal(src)
	if err != nil {
		t.Fatal(err)
	}
	var out bson.M
	if err := bson.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	s := normalizeExplainMetrics(out, "find").SortSpill
	st := s.Stages[0]
	if st.SortBytesProcessed.Value != 1<<40 || st.Spills.Value != 7 || st.SpillStorageBytes.Value != 4096 || st.MemoryLimitBytes.Value != 104857600 {
		t.Fatalf("int32/int64/double values: %+v", st)
	}
	if st.TimeMsEstimate != nil || st.SpilledRecords != nil {
		t.Fatalf("NaN and string values must be unreported, not zero: %+v", st)
	}
	if st.Namespace != "db.c" || st.SortBytesProcessed.Source != "executionStats.executionStages.totalDataSizeSorted" {
		t.Fatalf("namespace/source: %+v", st)
	}
}

func plan(stage bson.M) bson.M {
	return bson.M{"queryPlanner": bson.M{"namespace": "db.c"}, "executionStats": bson.M{"executionStages": stage}}
}

func TestCollectSortSpillMissingVersusZero(t *testing.T) {
	zero := normalizeExplainMetrics(plan(bson.M{"stage": "SORT", "nReturned": int32(3), "totalDataSizeSorted": int32(0),
		"usedDisk": false, "spills": int32(0), "executionTimeMillisEstimate": int32(0)}), "find").SortSpill
	if zero.Status != stats.SortSpillMeasured || zero.Stages[0].SortBytesProcessed == nil || zero.Stages[0].Spills.Value != 0 {
		t.Fatalf("classic reported zeros are measurements: %+v", zero)
	}
	missing := normalizeExplainMetrics(plan(bson.M{"stage": "SORT", "nReturned": int32(3)}), "find").SortSpill
	st := missing.Stages[0]
	if missing.Status != stats.SortSpillPartial || st.SortBytesProcessed != nil || st.Spills != nil || st.UsedDisk != nil ||
		strings.Join(st.Missing, ",") != "sort_bytes_processed,spill_status" {
		t.Fatalf("absent fields must stay unreported and mark the result partial: %+v %+v", missing, st)
	}
	// A slot-based sort that returned nothing really sorted nothing.
	empty := normalizeExplainMetrics(plan(bson.M{"stage": "sort", "nReturned": int32(0), "totalDataSizeSorted": int32(0),
		"usedDisk": false, "spills": int32(0)}), "find").SortSpill
	if empty.Status != stats.SortSpillMeasured || len(empty.Stages[0].Caveats) != 0 || !empty.Stages[0].SortBytesUsable() {
		t.Fatalf("empty SBE sort: %+v", empty)
	}
	// usedDisk true without any amount names the missing amount.
	noAmount := normalizeExplainMetrics(plan(bson.M{"stage": "SORT", "totalDataSizeSorted": int32(10), "usedDisk": true}), "find").SortSpill
	if !strings.Contains(strings.Join(noAmount.Stages[0].Missing, ","), "spill_amount") || noAmount.Status != stats.SortSpillPartial {
		t.Fatalf("spill without amount: %+v", noAmount.Stages[0])
	}
	// No sort and no fields: measured, with nothing to list.
	none := normalizeExplainMetrics(plan(bson.M{"stage": "COLLSCAN", "nReturned": int32(3)}), "find").SortSpill
	if none.Status != stats.SortSpillMeasured || len(none.Stages) != 0 {
		t.Fatalf("plain scan: %+v", none)
	}
}

// The unreliable-zero rule covers only totalDataSizeSorted 0 of a slot-based
// "sort" stage on a 7.0/8.0 server that did not spill and returned rows (or
// did not report nReturned). Every other zero stays a measured zero.
func TestCollectSortSpillUnreliableZeroScope(t *testing.T) {
	sbeSort := func(extra bson.M) bson.M {
		st := bson.M{"stage": "sort", "nReturned": int32(5), "totalDataSizeSorted": int32(0), "usedDisk": false,
			"spills": int32(0), "spilledDataStorageSize": int32(0), "memLimit": int32(104857600)}
		for k, v := range extra {
			if v == nil {
				delete(st, k)
			} else {
				st[k] = v
			}
		}
		return st
	}
	withVersion := func(v string, stage bson.M) bson.M {
		root := plan(stage)
		if v != "" {
			root["serverInfo"] = bson.M{"version": v}
		}
		return root
	}
	cases := []struct {
		name    string
		root    bson.M
		op      string
		caveat  bool
		version string
	}{
		{"8.0.4 sbe rows no spill", withVersion("8.0.4", sbeSort(nil)), "find", true, "8.0.4"},
		{"7.0.14 sbe rows no spill", withVersion("7.0.14", sbeSort(nil)), "find", true, "7.0.14"},
		{"8.0.4 sbe nReturned absent", withVersion("8.0.4", sbeSort(bson.M{"nReturned": nil})), "find", true, "8.0.4"},
		{"8.0.4 sbe spills absent usedDisk false", withVersion("8.0.4", sbeSort(bson.M{"spills": nil})), "find", true, "8.0.4"},
		{"8.0.4 sbe empty sort", withVersion("8.0.4", sbeSort(bson.M{"nReturned": int32(0)})), "find", false, "8.0.4"},
		{"8.0.4 sbe spilled", withVersion("8.0.4", sbeSort(bson.M{"spills": int32(3), "usedDisk": true})), "find", false, "8.0.4"},
		{"8.0.4 sbe nonzero", withVersion("8.0.4", sbeSort(bson.M{"totalDataSizeSorted": int32(4096)})), "find", false, "8.0.4"},
		{"8.2.0 sbe not checked", withVersion("8.2.0", sbeSort(nil)), "find", false, "8.2.0"},
		{"6.0.0 sbe not checked", withVersion("6.0.0", sbeSort(nil)), "find", false, "6.0.0"},
		{"version absent", withVersion("", sbeSort(nil)), "find", false, ""},
		{"8.0.4 classic SORT zero", withVersion("8.0.4", sbeSort(bson.M{"stage": "SORT"})), "find", false, "8.0.4"},
		{"8.0.4 $sort stage zero", bson.M{"serverInfo": bson.M{"version": "8.0.4"}, "stages": bson.A{
			bson.M{"$cursor": bson.M{"queryPlanner": bson.M{"namespace": "db.c"}, "executionStats": bson.M{"executionStages": bson.M{"stage": "COLLSCAN"}}}},
			bson.M{"$sort": bson.M{"sortKey": bson.M{"a": 1}}, "nReturned": int64(5), "totalDataSizeSortedBytesEstimate": int64(0),
				"usedDisk": false, "spills": int64(0)},
		}}, "aggregate", false, "8.0.4"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := normalizeExplainMetrics(tc.root, tc.op).SortSpill
			if len(s.Stages) != 1 {
				t.Fatalf("stages: %+v", s.Stages)
			}
			st := s.Stages[0]
			if (len(st.Caveats) > 0) != tc.caveat || st.SortBytesUsable() == tc.caveat || st.ServerVersion != tc.version {
				t.Fatalf("caveat=%v version %q, want caveat=%v version %q: %+v", len(st.Caveats) > 0, st.ServerVersion, tc.caveat, tc.version, st)
			}
			if st.SortBytesProcessed == nil {
				t.Fatalf("the reported sort bytes value must be kept: %+v", st)
			}
			wantStatus := stats.SortSpillMeasured
			if tc.caveat {
				wantStatus = stats.SortSpillPartial
			}
			if s.Status != wantStatus {
				t.Fatalf("status %s, want %s (%s)", s.Status, wantStatus, s.Reason)
			}
		})
	}
	// A slot-based group returning rows keeps its zero spill counters as measurements.
	g := normalizeExplainMetrics(withVersion("8.0.4", bson.M{"stage": "group", "nReturned": int32(5000), "spilledBytes": int64(0),
		"spilledDataStorageSize": int64(0), "spilledRecords": int64(0), "spills": int64(0), "usedDisk": false}), "aggregate").SortSpill
	if g.Status != stats.SortSpillMeasured || len(g.Stages[0].Caveats) != 0 || g.Stages[0].SpilledBytes == nil || g.Stages[0].SpilledBytes.Value != 0 {
		t.Fatalf("SBE group zeros are measurements: %+v", g)
	}
	// A shard's own serverInfo decides for its stages; the router version is the fallback.
	shardPipe := func(info bson.M) bson.M {
		d := bson.M{"stages": bson.A{bson.M{"$cursor": bson.M{"queryPlanner": bson.M{"namespace": "db.c"},
			"executionStats": bson.M{"executionStages": sbeSort(nil)}}}}}
		if info != nil {
			d["serverInfo"] = info
		}
		return d
	}
	sh := normalizeExplainMetrics(bson.M{"serverInfo": bson.M{"version": "8.0.4"}, "shards": bson.M{
		"s0": shardPipe(bson.M{"version": "8.2.0"}), "s1": shardPipe(nil)}}, "aggregate").SortSpill
	if len(sh.Stages) != 2 || sh.Stages[0].ServerVersion != "8.2.0" || len(sh.Stages[0].Caveats) != 0 ||
		sh.Stages[1].ServerVersion != "8.0.4" || len(sh.Stages[1].Caveats) != 1 {
		t.Fatalf("per-shard versions: %+v", sh.Stages)
	}
}

func TestCollectSortSpillFieldVariantsAndNesting(t *testing.T) {
	root := plan(bson.M{"stage": "LIMIT", "inputStage": bson.M{"stage": "OR", "inputStages": bson.A{
		bson.M{"stage": "SORT", "memUsage": int32(900), "totalDataSizeSorted": int32(900), "usedDisk": false},
		bson.M{"stage": "FETCH", "inputStage": bson.M{"stage": "SORT", "peakTrackedMemBytes": int64(2048),
			"maxMemoryUsageBytes": int64(4096), "totalDataSizeSorted": int32(10), "spills": int32(0)}},
	}}})
	s := normalizeExplainMetrics(root, "find").SortSpill
	if len(s.Stages) != 2 {
		t.Fatalf("nested SORT stages: %+v", s.Stages)
	}
	a, b := s.Stages[0], s.Stages[1]
	if a.Path != "executionStats.executionStages.inputStage.inputStages[0]" || len(a.PeakMemory) != 1 || a.PeakMemory[0].Source != a.Path+".memUsage" {
		t.Fatalf("legacy memUsage: %+v", a)
	}
	if b.Path != "executionStats.executionStages.inputStage.inputStages[1].inputStage" || len(b.PeakMemory) != 2 ||
		b.PeakMemory[0].Source != b.Path+".maxMemoryUsageBytes" || b.PeakMemory[1].Source != b.Path+".peakTrackedMemBytes" {
		t.Fatalf("each peak memory field is kept with its own source: %+v", b.PeakMemory)
	}
	if b.MemoryLimitBytes != nil {
		t.Fatalf("peak memory must not be read as a limit: %+v", b)
	}
	// spilledBytes (SBE) and numBytesSpilledEstimate (classic) both map to spilled data size;
	// spilledDataStorageSize stays separate.
	sbe := normalizeExplainMetrics(plan(bson.M{"stage": "group", "spilledBytes": int64(500), "spilledDataStorageSize": int64(120),
		"spilledRecords": int64(9), "usedDisk": true, "spills": int64(2)}), "aggregate").SortSpill.Stages[0]
	if sbe.SpilledBytes.Value != 500 || sbe.SpillStorageBytes.Value != 120 || sbe.SpilledRecords.Value != 9 || sbe.Sort {
		t.Fatalf("SBE group spill fields: %+v", sbe)
	}
}

func TestCollectSortSpillIgnoresPlannerTreesAndCommand(t *testing.T) {
	sortStage := bson.M{"stage": "SORT", "memLimit": int32(100), "totalDataSizeSorted": int32(999), "usedDisk": true, "spills": int32(9)}
	root := bson.M{
		"queryPlanner": bson.M{"namespace": "db.c", "winningPlan": sortStage, "rejectedPlans": bson.A{sortStage}},
		"executionStats": bson.M{"executionStages": bson.M{"stage": "COLLSCAN"},
			"allPlansExecution": bson.A{bson.M{"executionStages": sortStage}}},
		"command": bson.M{"find": "c", "sort": bson.M{"a": 1}, "usedDisk": true, "spills": int32(4)},
	}
	if s := normalizeExplainMetrics(root, "find").SortSpill; s.Status != stats.SortSpillMeasured || len(s.Stages) != 0 {
		t.Fatalf("only the executed winning plan may contribute: %+v", s)
	}
	pipe := bson.M{"stages": bson.A{
		bson.M{"$cursor": bson.M{"queryPlanner": bson.M{"namespace": "db.c", "winningPlan": sortStage},
			"executionStats": bson.M{"executionStages": bson.M{"stage": "COLLSCAN"}}}},
		bson.M{"$sort": bson.M{"sortKey": bson.M{"spills": int32(1)}}, "totalDataSizeSortedBytesEstimate": int64(70), "usedDisk": false, "spills": int64(0)},
	}}
	s := normalizeExplainMetrics(pipe, "aggregate").SortSpill
	if len(s.Stages) != 1 || s.Stages[0].Stage != "$sort" || s.Stages[0].Spills.Value != 0 || s.Stages[0].Namespace != "db.c" {
		t.Fatalf("a stage specification is not a measurement: %+v", s.Stages)
	}
}

func TestCollectSortSpillShardedNoDuplicates(t *testing.T) {
	shardSort := func(n int32) bson.M {
		return bson.M{"stage": "SORT", "totalDataSizeSorted": n, "usedDisk": false, "spills": int32(0)}
	}
	// The router merge node repeats aggregated fields; only shard stages count.
	root := bson.M{"queryPlanner": bson.M{}, "executionStats": bson.M{"executionStages": bson.M{
		"stage": "SHARD_MERGE_SORT", "totalDataSizeSorted": int32(300), "usedDisk": false, "spills": int32(0),
		"shards": bson.A{
			bson.M{"shardName": "s0", "executionStages": shardSort(100)},
			bson.M{"shardName": "s1", "executionStages": shardSort(200)},
		}}}}
	s := normalizeExplainMetrics(root, "find").SortSpill
	if len(s.Stages) != 2 || s.Stages[0].Shard != "s0" || s.Stages[1].Shard != "s1" ||
		s.Stages[0].SortBytesProcessed.Value != 100 || s.Stages[1].SortBytesProcessed.Value != 200 {
		t.Fatalf("router counters must not be added to shard counters: %+v", s.Stages)
	}
	pipe := bson.M{"splitPipeline": bson.M{}, "shards": bson.M{
		"s1": bson.M{"stages": bson.A{bson.M{"$cursor": bson.M{"queryPlanner": bson.M{"namespace": "db.c"},
			"executionStats": bson.M{"executionStages": shardSort(5)}}}}},
		"s0": bson.M{"queryPlanner": bson.M{}},
	}}
	ps := normalizeExplainMetrics(pipe, "aggregate").SortSpill
	if ps.Status != stats.SortSpillPartial || !strings.Contains(ps.Reason, "shard_execution_stats_missing") ||
		!strings.Contains(ps.Reason, "merge_stages_not_measured") || len(ps.Stages) != 1 || ps.Stages[0].Shard != "s1" {
		t.Fatalf("sharded pipeline with a planner-only shard and router merge: %+v", ps)
	}
}

func TestCollectSortSpillUnavailableAndPartial(t *testing.T) {
	planner := normalizeExplainMetrics(bson.M{"queryPlanner": bson.M{"winningPlan": bson.M{"stage": "SORT", "memLimit": int32(1)}}}, "find").SortSpill
	if planner.Status != stats.SortSpillUnavailable || planner.Reason != "planner_only_no_execution_stats" || len(planner.Stages) != 0 {
		t.Fatalf("planner-only: %+v", planner)
	}
	unsupported := normalizeExplainMetrics(bson.M{"stages": bson.A{bson.M{"$match": bson.M{}}}}, "aggregate").SortSpill
	if unsupported.Status != stats.SortSpillUnavailable || !strings.HasPrefix(unsupported.Reason, "unsupported_explain_layout") {
		t.Fatalf("unsupported: %+v", unsupported)
	}
	union := normalizeExplainMetrics(bson.M{"stages": bson.A{
		bson.M{"$cursor": bson.M{"queryPlanner": bson.M{"namespace": "db.c"}, "executionStats": bson.M{"executionStages": bson.M{"stage": "COLLSCAN"}}}},
		bson.M{"$unionWith": bson.M{"coll": "d"}},
	}}, "aggregate").SortSpill
	if union.Status != stats.SortSpillPartial || union.Reason != "union_with_subpipeline_not_measured" {
		t.Fatalf("an unwalked $unionWith sub-pipeline must not read as no sort: %+v", union)
	}
}
