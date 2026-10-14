package stats

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func newCaptureCollector(t testing.TB, maxGroups int) *Collector {
	t.Helper()
	c := NewCollector()
	c.ConfigureInsights(&config.AppConfig{
		InsightsEnabled:         true,
		InsightsSamplingRate:    0.1,
		InsightsMaxEvents:       50,
		InsightsMaxGroups:       maxGroups,
		InsightsSlowThresholdMs: 200,
		InsightsExplainEnabled:  true,
	})
	return c
}

func recordFind(c *Collector, coll, shape string, d time.Duration, success bool, q *ExecutableQuery) {
	c.RecordOperationEvent("find", "shop", coll, shape, shape+" summary", []string{"status"}, d, success, 1, q, "q", "unit_test", "w", "", "", "", "", 0)
}

func captureShape(t *testing.T, rep InsightsReport, shapeKey string) CapturedShape {
	t.Helper()
	for _, s := range rep.QueryCapture.Shapes {
		if s.ShapeKey == shapeKey {
			return s
		}
	}
	t.Fatalf("shape %q not in query_capture: %+v", shapeKey, rep.QueryCapture.Shapes)
	return CapturedShape{}
}

func TestCaptureRetainsExecutableSamplesForShapeThatNeverCrossesSlowThreshold(t *testing.T) {
	c := newCaptureCollector(t, 10)
	for i := 0; i < 2000; i++ {
		q := &ExecutableQuery{Database: "shop", Filter: map[string]interface{}{"status": fmt.Sprintf("open-%d", i)}, Limit: 10}
		recordFind(c, "orders", "find|{status:<str>}|limit:10", time.Duration(1+i%5)*time.Millisecond, true, q)
	}
	rep := c.GetFinalInsights()

	for _, sq := range rep.SlowQueries {
		if sq.ShapeKey == "find|{status:<str>}|limit:10" {
			t.Fatalf("fast shape must stay out of the slow view, got %+v", sq)
		}
	}
	if rep.Metadata.FilteredByThreshold != 1 {
		t.Fatalf("expected the fast shape to be filtered by threshold, got %d", rep.Metadata.FilteredByThreshold)
	}
	s := captureShape(t, rep, "find|{status:<str>}|limit:10")
	if s.Status != "sampled" || s.EligibleOperations != 2000 || s.RecordedOperations != 2000 {
		t.Fatalf("unexpected capture row: %+v", s)
	}
	if s.RetainedSamples < captureSamplesPerShape || s.RetainedSamples > captureSamplesPerShape+1 {
		t.Fatalf("retained %d, want %d..%d", s.RetainedSamples, captureSamplesPerShape, captureSamplesPerShape+1)
	}
	if s.RetainedBelowSlowThreshold != s.RetainedSamples || !s.SlowestRetained || s.RetainedMaxMs != 5 {
		t.Fatalf("expected all samples below threshold and the slowest (5ms) retained: %+v", s)
	}

	samples := c.SnapshotRepresentativeSamples()
	if len(samples) != s.RetainedSamples {
		t.Fatalf("snapshot has %d samples, report says %d", len(samples), s.RetainedSamples)
	}
	slowest := 0
	for _, smp := range samples {
		if smp.Query == nil || smp.Query.Limit != 10 || smp.Query.Database != "shop" {
			t.Fatalf("sample is not executable: %+v", smp)
		}
		if v, ok := smp.Query.Filter["status"].(string); !ok || !strings.HasPrefix(v, "open-") {
			t.Fatalf("executable literal not preserved: %+v", smp.Query.Filter)
		}
		if smp.DurationMs >= 200 {
			t.Fatalf("sample above slow threshold: %v", smp.DurationMs)
		}
		if smp.ShapeID != s.ShapeID {
			t.Fatalf("shape ID mismatch: %s vs %s", smp.ShapeID, s.ShapeID)
		}
		if smp.Slowest {
			slowest++
		}
	}
	if slowest != 1 {
		t.Fatalf("expected exactly one slowest sample, got %d", slowest)
	}

	// Snapshots are deep copies.
	samples[0].Query.Filter["status"] = "mutated"
	for _, smp := range c.SnapshotRepresentativeSamples() {
		if smp.Query.Filter["status"] == "mutated" {
			t.Fatalf("snapshot must not alias retained samples")
		}
	}
}

func TestCaptureReservoirSpansWholeRunAndLatencyLevels(t *testing.T) {
	c := newCaptureCollector(t, 10)
	const n = 20000
	// Latency rises monotonically through the run, so a recency- or
	// block-biased sample would cluster at one end.
	for i := 0; i < n; i++ {
		q := &ExecutableQuery{Database: "shop", Filter: map[string]interface{}{"i": int64(i)}}
		recordFind(c, "orders", "find|{i:<num>}", time.Duration(i+1)*time.Microsecond*10, true, q)
	}
	var res []float64
	for _, smp := range c.SnapshotRepresentativeSamples() {
		if smp.InReservoir {
			res = append(res, smp.DurationMs)
		}
	}
	if len(res) != captureSamplesPerShape {
		t.Fatalf("reservoir size %d", len(res))
	}
	maxMs := float64(n) * 0.01
	low, high := 0, 0
	for _, ms := range res {
		if ms < maxMs/2 {
			low++
		} else {
			high++
		}
	}
	if low == 0 || high == 0 {
		t.Fatalf("reservoir does not span the run: %v", res)
	}
	rep := c.GetFinalInsights()
	s := captureShape(t, rep, "find|{i:<num>}")
	if s.RetainedMaxMs != maxMs || !s.SlowestRetained {
		t.Fatalf("slowest operation must be retained: max=%v want %v", s.RetainedMaxMs, maxMs)
	}
	if s.RetainedMedianMs < maxMs*0.15 || s.RetainedMedianMs > maxMs*0.85 {
		t.Fatalf("reservoir median %v is not representative of 0..%v", s.RetainedMedianMs, maxMs)
	}
	if rep.QueryCapture.Coverage.EvictedSamples == 0 || rep.QueryCapture.Coverage.SampledOperations <= uint64(captureSamplesPerShape) {
		t.Fatalf("expected reservoir replacement over the run: %+v", rep.QueryCapture.Coverage)
	}
}

func TestCaptureEligibilityCountersAndUnavailableReasons(t *testing.T) {
	c := newCaptureCollector(t, 10)
	q := &ExecutableQuery{Database: "shop", Filter: map[string]interface{}{"status": "x"}}
	recordFind(c, "orders", "find|ok", time.Millisecond, true, q)
	recordFind(c, "orders", "find|failed", time.Millisecond, false, q)
	recordFind(c, "orders", "find|noquery", time.Millisecond, true, nil)
	c.RecordOperationEvent("insert", "shop", "orders", "insert|generated_doc", "insert", nil, time.Millisecond, true, 1, nil, "", "", "", "", "", "", "", 0)
	c.RecordOperationEvent("transaction", "shop", "(multiple)", "transaction|batch", "tx", nil, time.Millisecond, true, 1, nil, "", "", "", "", "", "", "", 0)

	rep := c.GetFinalInsights()
	cov := rep.QueryCapture.Coverage
	if rep.QueryCapture.Status != "ready" || cov.RecordedOperations != 5 || cov.EligibleOperations != 1 || cov.IneligibleFailed != 1 || cov.IneligibleNoQuery != 1 || cov.IneligibleNotExplainable != 2 {
		t.Fatalf("unexpected coverage: %+v", cov)
	}
	if cov.TrackedShapes != 3 || cov.ShapesWithSamples != 1 || cov.RetainedSamples != 1 {
		t.Fatalf("unexpected shape counts: %+v", cov)
	}
	if got := captureShape(t, rep, "find|failed"); got.Status != "unavailable" || got.UnavailableReason != "no_successful_operations" {
		t.Fatalf("failed shape: %+v", got)
	}
	if got := captureShape(t, rep, "find|noquery"); got.Status != "unavailable" || got.UnavailableReason != "no_executable_query" {
		t.Fatalf("no-query shape: %+v", got)
	}
	if len(cov.UnrecordedPaths) != 2 || !strings.Contains(strings.Join(cov.UnrecordedPaths, " "), "raw_injector") {
		t.Fatalf("unrecorded paths must be disclosed: %v", cov.UnrecordedPaths)
	}
	if cov.ShapeIdentityVersion != ShapeIdentityVersion || rep.Metadata.ShapeIdentityVersion != ShapeIdentityVersion {
		t.Fatalf("shape identity version missing")
	}
}

func TestCaptureGroupLimitOversizeAndByteBudget(t *testing.T) {
	c := newCaptureCollector(t, 2)
	small := &ExecutableQuery{Database: "shop", Filter: map[string]interface{}{"a": 1.0}}
	recordFind(c, "orders", "find|a", time.Millisecond, true, small)
	ids := make([]int64, 3000)
	big := &ExecutableQuery{Database: "shop", Filter: map[string]interface{}{"_id": map[string]interface{}{"$in": ids}}}
	recordFind(c, "orders", "find|big", time.Millisecond, true, big)
	recordFind(c, "orders", "find|untracked", time.Millisecond, true, small)

	rep := c.GetFinalInsights()
	cov := rep.QueryCapture.Coverage
	if cov.UntrackedShapeOperations != 1 || cov.DroppedOversized != 1 || cov.TrackedShapes != 2 {
		t.Fatalf("unexpected coverage: %+v", cov)
	}
	if got := captureShape(t, rep, "find|big"); got.UnavailableReason != "command_exceeds_sample_byte_limit" {
		t.Fatalf("oversized shape: %+v", got)
	}
	joined := strings.Join(cov.Limitations, " | ")
	if !strings.Contains(joined, "Group limit 2") || !strings.Contains(joined, "sample limit") {
		t.Fatalf("limits must be disclosed: %s", joined)
	}
	if cov.Limits.SamplesPerShape != captureSamplesPerShape || cov.Limits.MaxGroups != 2 || cov.Limits.MaxSampleBytes != captureMaxSampleBytes {
		t.Fatalf("limits missing: %+v", cov.Limits)
	}

	b := newCaptureCollector(t, 10)
	b.insightsMu.Lock()
	b.capture.maxRetainedBytes = 2 * estimateQueryBytes(small)
	b.insightsMu.Unlock()
	recordFind(b, "orders", "find|first", time.Millisecond, true, small)
	recordFind(b, "orders", "find|second", time.Millisecond, true, small)
	brep := b.GetFinalInsights()
	if brep.QueryCapture.Coverage.DroppedByteBudget != 1 || brep.QueryCapture.Coverage.RetainedBytesEstimate > b.capture.maxRetainedBytes {
		t.Fatalf("byte budget not enforced: %+v", brep.QueryCapture.Coverage)
	}
	if got := captureShape(t, brep, "find|second"); got.UnavailableReason != "retained_byte_budget_exhausted" {
		t.Fatalf("budget shape: %+v", got)
	}
}

func TestCaptureRetainedBytesStayWithinBudgetUnderReplacement(t *testing.T) {
	c := newCaptureCollector(t, 50)
	for i := 0; i < 5000; i++ {
		filter := map[string]interface{}{"k": strings.Repeat("x", i%300)}
		recordFind(c, "orders", fmt.Sprintf("find|s%d", i%40), time.Duration(i%97)*time.Microsecond, true, &ExecutableQuery{Database: "shop", Filter: filter})
	}
	c.insightsMu.Lock()
	sum := 0
	for _, g := range c.impact.groups {
		if g.capture == nil {
			continue
		}
		for _, s := range g.capture.reservoir {
			sum += s.bytes
		}
		if g.capture.slowest != nil {
			sum += g.capture.slowest.bytes
		}
		if len(g.capture.reservoir) > captureSamplesPerShape {
			t.Errorf("reservoir exceeds limit: %d", len(g.capture.reservoir))
		}
	}
	accounted := c.capture.retainedBytes
	c.insightsMu.Unlock()
	if sum != accounted {
		t.Fatalf("byte accounting drifted: actual %d, accounted %d", sum, accounted)
	}
}

func TestCaptureResetClearsSamplesAndCounters(t *testing.T) {
	c := newCaptureCollector(t, 10)
	q := &ExecutableQuery{Database: "shop", Filter: map[string]interface{}{"a": 1.0}}
	for i := 0; i < 50; i++ {
		recordFind(c, "orders", "find|a", time.Millisecond, true, q)
	}
	recordFind(c, "orders", "find|a", time.Millisecond, false, q)
	c.ResetInsights()
	if got := c.SnapshotRepresentativeSamples(); len(got) != 0 {
		t.Fatalf("samples survived reset: %d", len(got))
	}
	cov := c.GetFinalInsights().QueryCapture.Coverage
	if cov.EligibleOperations != 0 || cov.IneligibleFailed != 0 || cov.SampledOperations != 0 || cov.RetainedSamples != 0 || cov.RetainedBytesEstimate != 0 || cov.TrackedShapes != 0 {
		t.Fatalf("counters survived reset: %+v", cov)
	}
	recordFind(c, "orders", "find|a", time.Millisecond, true, q)
	if got := c.GetFinalInsights().QueryCapture.Coverage; got.EligibleOperations != 1 || got.RetainedSamples != 1 {
		t.Fatalf("capture must resume after reset: %+v", got)
	}
}

func TestCaptureDisabledModesAreVisible(t *testing.T) {
	off := NewCollector()
	off.ConfigureInsights(&config.AppConfig{InsightsEnabled: true, InsightsSamplingRate: 1, InsightsMaxEvents: 10, InsightsMaxGroups: 10})
	recordFind(off, "orders", "find|a", time.Millisecond, true, &ExecutableQuery{Database: "shop", Filter: map[string]interface{}{"a": 1.0}})
	rep := off.GetFinalInsights()
	if rep.QueryCapture.Status != "disabled" || rep.QueryCapture.DisabledReason != "explain_disabled" || len(off.SnapshotRepresentativeSamples()) != 0 {
		t.Fatalf("explain-disabled capture must be visible and empty: %+v", rep.QueryCapture)
	}
	if rep.Impact.Coverage.ObservedOperations != 1 {
		t.Fatalf("impact must still count operations")
	}

	none := NewCollector()
	none.ConfigureInsights(&config.AppConfig{InsightsEnabled: false, InsightsExplainEnabled: true})
	if r := none.GetFinalInsights(); r.QueryCapture.Status != "disabled" || r.QueryCapture.DisabledReason != "insights_disabled" {
		t.Fatalf("insights-disabled capture must be visible: %+v", r.QueryCapture)
	}
}

func TestCaptureConcurrentRecordSnapshotAndReset(t *testing.T) {
	c := newCaptureCollector(t, 20)
	const workers, perWorker = 8, 2000
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				q := &ExecutableQuery{Database: "shop", Filter: map[string]interface{}{"w": int64(w), "i": int64(i)}}
				recordFind(c, "orders", fmt.Sprintf("find|s%d", i%5), time.Duration(i%50)*time.Microsecond, true, q)
			}
		}(w)
	}
	stop := make(chan struct{})
	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = c.GetFinalInsights()
			for _, s := range c.SnapshotRepresentativeSamples() {
				s.Query.Filter["w"] = int64(-1)
			}
		}
	}()
	wg.Wait()
	close(stop)
	readers.Wait()

	rep := c.GetFinalInsights()
	cov := rep.QueryCapture.Coverage
	if cov.EligibleOperations != workers*perWorker || cov.RecordedOperations != workers*perWorker {
		t.Fatalf("lost operations under concurrency: %+v", cov)
	}
	if cov.ShapesWithSamples != 5 || cov.RetainedSamples > 5*(captureSamplesPerShape+1) {
		t.Fatalf("unexpected retention: %+v", cov)
	}
	for _, s := range c.SnapshotRepresentativeSamples() {
		if s.Query.Filter["w"] == int64(-1) {
			t.Fatalf("reader mutation leaked into retained samples")
		}
	}

	// Reset racing with recorders must leave consistent state.
	var rw sync.WaitGroup
	for w := 0; w < 4; w++ {
		rw.Add(1)
		go func() {
			defer rw.Done()
			for i := 0; i < 500; i++ {
				recordFind(c, "orders", "find|r", time.Microsecond, true, &ExecutableQuery{Database: "shop", Filter: map[string]interface{}{"a": 1.0}})
				if i%100 == 0 {
					c.ResetInsights()
				}
			}
		}()
	}
	rw.Wait()
	c.insightsMu.Lock()
	sum := 0
	for _, g := range c.impact.groups {
		if g.capture != nil {
			for _, s := range g.capture.reservoir {
				sum += s.bytes
			}
			if g.capture.slowest != nil {
				sum += g.capture.slowest.bytes
			}
		}
	}
	if sum != c.capture.retainedBytes {
		t.Errorf("byte accounting drifted across reset: %d vs %d", sum, c.capture.retainedBytes)
	}
	c.insightsMu.Unlock()
}

func TestDescribeExecutableQueryRedactsLiteralsAndKeepsStructure(t *testing.T) {
	oid := bson.NewObjectID()
	find := &ExecutableQuery{
		Database: "shop",
		Filter: map[string]interface{}{
			"email":   "alice@example.com",
			"age":     map[string]interface{}{"$gte": 42.0},
			"owner":   oid,
			"created": time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
			"n":       int32(7),
			"_id":     map[string]interface{}{"$in": []int64{11, 22, 33}},
			"$expr":   map[string]interface{}{"$gt": []interface{}{"$spent", "$budget"}},
		},
		Projection: map[string]interface{}{"_id": 0, "name": 1.0, "tags": map[string]interface{}{"$slice": 99.0}},
		Limit:      10,
	}
	d, truncated := DescribeExecutableQuery("find", find)
	if truncated {
		t.Fatalf("unexpected truncation")
	}
	for _, want := range []string{
		"email:<string>", "age:{$gte:<double>}", "owner:<objectId>", "created:<date>", "n:<int32>",
		"_id:{$in:[<int64> x3]}", `$expr:{$gt:["$spent","$budget"]}`,
		"projection={_id:0,name:1,tags:{$slice:<double>}}", "limit=10",
	} {
		if !strings.Contains(d, want) {
			t.Fatalf("descriptor %q missing %q", d, want)
		}
	}
	for _, leak := range []string{"alice", "42", oid.Hex(), "2026", "11", "22", "33", "99"} {
		if strings.Contains(d, leak) {
			t.Fatalf("descriptor leaks %q: %s", leak, d)
		}
	}

	upd := &ExecutableQuery{Database: "shop", Filter: map[string]interface{}{"sku": "SKU-SECRET"}, Update: map[string]interface{}{"$set": map[string]interface{}{"token": "s3cret"}, "$inc": map[string]interface{}{"n": 5.0}}, Upsert: true}
	ud, _ := DescribeExecutableQuery("updateOne", upd)
	if !strings.Contains(ud, "update={$inc:{n:<double>},$set:{token:<string>}}") || !strings.Contains(ud, "upsert=true") || strings.Contains(ud, "s3cret") || strings.Contains(ud, "SKU-SECRET") {
		t.Fatalf("update descriptor: %s", ud)
	}

	agg := &ExecutableQuery{Database: "shop", Pipeline: []interface{}{
		map[string]interface{}{"$match": map[string]interface{}{"status": "$secret-literal", "amount": map[string]interface{}{"$gt": 1000.0}}},
		map[string]interface{}{"$sort": map[string]interface{}{"amount": -1.0}},
		map[string]interface{}{"$limit": 5.0},
		map[string]interface{}{"$group": map[string]interface{}{"_id": "$status", "total": map[string]interface{}{"$sum": "$amount"}, "bonus": map[string]interface{}{"$sum": 777.0}}},
		map[string]interface{}{"$project": map[string]interface{}{"total": 1.0, "label": map[string]interface{}{"$literal": "hidden-label"}}},
		map[string]interface{}{"$addFields": map[string]interface{}{"flag": "flag-literal"}},
		map[string]interface{}{"$lookup": map[string]interface{}{"from": "other", "as": "x", "pipeline": []interface{}{map[string]interface{}{"$match": map[string]interface{}{"k": "lookup-literal"}}, map[string]interface{}{"$limit": 3.0}}}},
	}}
	ad, _ := DescribeExecutableQuery("aggregate", agg)
	for _, want := range []string{
		"{$match:{amount:{$gt:<double>},status:<string>}}", "{$sort:{amount:-1}}", "{$limit:5}",
		`_id:"$status"`, `total:{$sum:"$amount"}`, "bonus:{$sum:<double>}",
		"{$project:{label:{$literal:<string>},total:1}}", "{$addFields:{flag:<string>}}",
		"pipeline:[{$match:{k:<string>}},{$limit:3}]",
	} {
		if !strings.Contains(ad, want) {
			t.Fatalf("aggregate descriptor %q missing %q", ad, want)
		}
	}
	for _, leak := range []string{"secret-literal", "1000", "777", "hidden-label", "flag-literal", "lookup-literal", "other"} {
		if strings.Contains(ad, leak) {
			t.Fatalf("aggregate descriptor leaks %q: %s", leak, ad)
		}
	}

	long := &ExecutableQuery{Database: "shop", Filter: map[string]interface{}{}}
	for i := 0; i < 200; i++ {
		long.Filter[fmt.Sprintf("field_with_a_long_name_%03d", i)] = "v"
	}
	ld, lt := DescribeExecutableQuery("find", long)
	if !lt || len(ld) > captureMaxDescriptorBytes+len("...(truncated)") || !strings.HasSuffix(ld, "...(truncated)") {
		t.Fatalf("descriptor must be bounded: truncated=%v len=%d", lt, len(ld))
	}
}

func TestQueryCaptureReportDoesNotLeakLiterals(t *testing.T) {
	c := newCaptureCollector(t, 10)
	for i := 0; i < 30; i++ {
		recordFind(c, "users", "find|{email:<str>}|limit:1", time.Millisecond, true, &ExecutableQuery{Database: "shop", Filter: map[string]interface{}{"email": fmt.Sprintf("person%d@secret.example", i)}, Limit: 1})
		c.RecordOperationEvent("aggregate", "shop", "orders", "aggregate|x", "agg", nil, time.Millisecond, true, 1,
			&ExecutableQuery{Database: "shop", Pipeline: []interface{}{map[string]interface{}{"$match": map[string]interface{}{"tenant": "tenant-secret"}}}}, "", "", "", "", "", "", "", 0)
	}
	b, err := json.Marshal(c.GetFinalInsights())
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	for _, leak := range []string{"secret.example", "tenant-secret"} {
		if strings.Contains(out, leak) {
			t.Fatalf("report leaks %q", leak)
		}
	}
	var decoded InsightsReport
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	if got := captureShape(t, decoded, "find|{email:<str>}|limit:1"); got.Descriptor != "find filter={email:<string>} limit=1" {
		t.Fatalf("report missing redacted capture descriptor: %q", got.Descriptor)
	}
}

func BenchmarkRecordOperationEventWithCapture(b *testing.B) {
	c := newCaptureCollector(b, 300)
	shapes := []string{"find|{a:<num>}", "find|{b:<num>}", "find|{c:<num>}", "find|{d:<num>}"}
	q := &ExecutableQuery{Database: "shop", Filter: map[string]interface{}{"a": map[string]interface{}{"$in": []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}}}, Limit: 10}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			recordFind(c, "orders", shapes[i%len(shapes)], time.Duration(i%1000)*time.Microsecond, true, q)
			i++
		}
	})
}
