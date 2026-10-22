//go:build integration
// +build integration

package mongo

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/workloadrec"
)

// resultFields is an outcome without its latency, for comparing executions.
func resultFields(o workloadrec.Outcome) string {
	n := func(p *int64) string {
		if p == nil {
			return "-"
		}
		return fmt.Sprint(*p)
	}
	return fmt.Sprintf("ok=%t code=%d name=%s returned=%s checksum=%s inserted=%s matched=%s modified=%s upserted=%s deleted=%s tx=%s",
		o.OK, o.ErrorCode, o.ErrorName, n(o.Returned), o.Checksum, n(o.Inserted), n(o.Matched), n(o.Modified), n(o.Upserted), n(o.Deleted), o.Transaction)
}

// TestWorkloadOutcomesIntegration runs one deterministic worker twice on
// freshly seeded copies of the same data, recording outcomes with result
// checksums, then replays the recording on a third copy and once more on the
// already-changed data. With one worker every result must match exactly;
// the second replay must report duplicate-key errors by code, without the
// error message.
func TestWorkloadOutcomesIntegration(t *testing.T) {
	client := connectIntegrationMongoOrSkip(t)
	defer client.Disconnect(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	dbName := fmt.Sprintf("plgm_it_outcomes_%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = client.Database(dbName).Drop(context.Background()) })
	collections := []config.CollectionDefinition{
		{DatabaseName: dbName, Name: "orders", Fields: map[string]config.CollectionField{
			"order_id": {Type: "int"}, "status": {Type: "string", Enum: []interface{}{"new", "paid", "sent"}}, "total": {Type: "double"},
		}},
		{DatabaseName: dbName, Name: "users", Fields: map[string]config.CollectionField{
			"age": {Type: "int", Min: intPtr(18), Max: intPtr(90)}, "name": {Type: "string", Provider: "first_name"}, "joined": {Type: "date"},
		}},
	}
	queries := []config.QueryDefinition{
		{Name: "users_by_age", Database: dbName, Collection: "users", Operation: "find", Filter: map[string]interface{}{"age": map[string]interface{}{"$gte": "<int>"}}},
		{Name: "users_agg", Database: dbName, Collection: "users", Operation: "aggregate", Pipeline: []interface{}{map[string]interface{}{"$match": map[string]interface{}{"age": map[string]interface{}{"$lte": "<int>"}}}}},
		{Name: "touch_user", Database: dbName, Collection: "users", Operation: "updateOne", Filter: map[string]interface{}{"age": "<int>"}, Update: map[string]interface{}{"$inc": map[string]interface{}{"age": 1}}},
		{Name: "drop_user", Database: dbName, Collection: "users", Operation: "deleteOne", Filter: map[string]interface{}{"age": "<int>"}},
	}
	dir := t.TempDir()
	base := config.AppConfig{
		Duration: "3s", StatusRefreshRateSec: 1, Concurrency: 1, RandomSeed: 2026, DropCollections: true,
		WorkloadDeterministic: true, OperationsPerWorker: 300,
		DocumentsCount: 300, SeedBatchSize: 50, SeedConcurrency: 1,
		FindPercent: 40, AggregatePercent: 10, UpdatePercent: 20, DeletePercent: 5, InsertPercent: 15, BulkInsertPercent: 10,
		FindBatchSize: 5, FindLimit: 5, InsertBatchSize: 5, InsertCacheSize: 50, RecordPoolBootstrapSample: 50, RecordPoolMaxSize: 1000,
		ExistingRecordHitRate: 60, InsightsEnabled: true, InsightsSamplingRate: 1, InsightsMaxEvents: 1000, InsightsMaxGroups: 500,
		WorkloadRecordOutcomes: true, WorkloadRecordResultChecksums: true,
	}
	base.WebUI.Enabled = true

	runDet := func(name string, record bool) (string, string, *stats.GeneratedTrafficInfo) {
		cfg := base
		cfg.WorkloadDeterministicOutcomesPath = filepath.Join(dir, name+"_outcomes.ndjson")
		rec := ""
		if record {
			rec = filepath.Join(dir, name+"_rec.ndjson")
			cfg.WorkloadRecordEnabled, cfg.WorkloadRecordPath = true, rec
		}
		config.NormalizeWorkloadRecordReplay(&cfg)
		if err := config.ValidateWorkloadOutcomes(&cfg); err != nil {
			t.Fatal(err)
		}
		db := seedRecordReplayDB(t, ctx, client, collections, &cfg)
		c := stats.NewCollector()
		if err := RunWorkload(ctx, db, collections, queries, &cfg, c); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		md := c.GetFinalInsights().Metadata
		if md.OutcomeFile == nil || md.OutcomeFile.Status != workloadrec.StatusComplete || md.OutcomeFile.TrafficFingerprint != md.GeneratedTraffic.Fingerprint {
			t.Fatalf("%s: outcome file = %+v", name, md.OutcomeFile)
		}
		return cfg.WorkloadDeterministicOutcomesPath, rec, md.GeneratedTraffic
	}
	byKey := func(path string) map[string]workloadrec.OutcomeLine {
		lines, _, tr := readOutcomeLines(t, path)
		if tr == nil || tr.Status != workloadrec.StatusComplete {
			t.Fatalf("%s trailer = %+v", path, tr)
		}
		out := make(map[string]workloadrec.OutcomeLine, len(lines))
		for _, l := range lines {
			out[fmt.Sprintf("%d/%d/%d", l.Iteration, l.Worker, l.WorkerSeq)] = l
		}
		return out
	}

	outA, recA, gA := runDet("a", true)
	outB, _, gB := runDet("b", false)
	if gA.Fingerprint != gB.Fingerprint || gA.OpsGenerated <= gA.OpsPlanned || gB.OpsGenerated <= gB.OpsPlanned {
		t.Fatalf("outcome capture or recording changed the generated traffic: %+v %+v", gA, gB)
	}
	firstPass := func(m map[string]workloadrec.OutcomeLine) map[string]workloadrec.OutcomeLine {
		out := make(map[string]workloadrec.OutcomeLine, len(m))
		for k, l := range m {
			if l.WorkerSeq <= uint64(base.OperationsPerWorker) {
				out[k] = l
			}
		}
		return out
	}
	a, b := firstPass(byKey(outA)), firstPass(byKey(outB))
	if len(a) == 0 || len(a) != len(b) {
		t.Fatalf("first-pass outcome counts differ: %d vs %d", len(a), len(b))
	}
	for k, la := range a {
		if lb, ok := b[k]; !ok || resultFields(la.Outcome) != resultFields(lb.Outcome) || la.Shape != lb.Shape {
			t.Fatalf("op %s differs between two runs on the same data:\n a %s %s\n b %s %s", k, la.Shape, resultFields(la.Outcome), lb.Shape, resultFields(lb.Outcome))
		}
	}

	// Every op type carries its result summary, in the recording too.
	recorded := map[uint64]workloadrec.OpLine{}
	kinds := map[string]bool{}
	if err := workloadrec.Stream(ctx, recA, func(op *workloadrec.OpLine) error {
		o := op.Outcome
		if o == nil {
			return fmt.Errorf("recorded op %d has no outcome", op.Seq)
		}
		kind := op.Path + "/" + op.Op
		switch op.Op {
		case "find", "aggregate":
			if o.Returned == nil || len(o.Checksum) != 64 {
				return fmt.Errorf("%s outcome = %s", kind, resultFields(*o))
			}
		case "insert", "insertMany":
			if o.Inserted == nil {
				return fmt.Errorf("%s outcome = %s", kind, resultFields(*o))
			}
		case "updateOne":
			if o.Matched == nil || o.Modified == nil || o.Upserted == nil {
				return fmt.Errorf("%s outcome = %s", kind, resultFields(*o))
			}
		case "deleteOne":
			if o.Deleted == nil {
				return fmt.Errorf("%s outcome = %s", kind, resultFields(*o))
			}
		}
		if !o.OK {
			return fmt.Errorf("%s failed on fresh data: %s", kind, resultFields(*o))
		}
		kinds[kind] = true
		recorded[op.Seq] = *op
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{workloadrec.PathFastFind + "/find", workloadrec.PathFastInsertBatch + "/insertMany", "/find", "/aggregate", "/updateOne", "/deleteOne", "/insert"} {
		if !kinds[want] {
			t.Fatalf("recording has no %s operations with outcomes: %v", want, kinds)
		}
	}

	replay := func(name string, reseed bool) map[uint64]workloadrec.OutcomeLine {
		cfg := base
		cfg.WorkloadDeterministic, cfg.OperationsPerWorker = false, 0
		cfg.WorkloadReplayEnabled, cfg.WorkloadReplayPath = true, recA
		cfg.WorkloadReplayOutcomesPath = filepath.Join(dir, name+"_outcomes.ndjson")
		config.NormalizeWorkloadRecordReplay(&cfg)
		if err := config.ValidateWorkloadOutcomes(&cfg); err != nil {
			t.Fatal(err)
		}
		db := client.Database(dbName)
		if reseed {
			db = seedRecordReplayDB(t, ctx, client, collections, &cfg)
		}
		if err := RunWorkload(ctx, db, collections, queries, &cfg, stats.NewCollector()); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		lines, h, _ := readOutcomeLines(t, cfg.WorkloadReplayOutcomesPath)
		info, _ := workloadrec.Scan(recA)
		if h.RecordingHeaderHash != info.HeaderHash {
			t.Fatalf("%s: outcome header hash %q, recording %q", name, h.RecordingHeaderHash, info.HeaderHash)
		}
		out := map[uint64]workloadrec.OutcomeLine{}
		for _, l := range lines {
			out[l.Seq] = l
		}
		return out
	}
	same := replay("replay_fresh", true)
	if len(same) != len(recorded) {
		t.Fatalf("%d replay outcomes for %d recorded operations", len(same), len(recorded))
	}
	for seq, op := range recorded {
		if got := same[seq]; resultFields(got.Outcome) != resultFields(*op.Outcome) {
			t.Fatalf("seq %d (%s) replayed on the same data differs:\n recorded %s\n replayed %s", seq, op.Op, resultFields(*op.Outcome), resultFields(got.Outcome))
		}
	}

	again := replay("replay_again", false)
	dup := 0
	for seq, op := range recorded {
		if op.Op != "insert" {
			continue
		}
		if o := again[seq].Outcome; !o.OK && o.ErrorCode == 11000 && o.ErrorName == "DuplicateKey" && *o.Inserted == 0 {
			dup++
		}
	}
	if dup == 0 {
		t.Fatal("replaying inserts onto data that already has them must report DuplicateKey outcomes")
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "replay_again_outcomes.ndjson"))
	if strings.Contains(string(raw), "E11000") || strings.Contains(string(raw), "duplicate key error") {
		t.Fatal("the outcome file must not contain error messages")
	}
	t.Logf("%d operations compared; %d duplicate-key inserts on the second replay; kinds %v", len(recorded), dup, kinds)
}
