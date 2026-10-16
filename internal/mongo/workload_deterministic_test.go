package mongo

import (
	"bytes"
	"context"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/datagen"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/loadprofile"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/workloadrec"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func detCollections() []config.CollectionDefinition {
	cols := goldenCollections()
	cols = append(cols, config.CollectionDefinition{DatabaseName: "appdb", Name: "users", Fields: map[string]config.CollectionField{
		"age":  {Type: "int"},
		"name": {Type: "string", Provider: "first_name"},
		"city": {Type: "string", Provider: "city"},
	}})
	return cols
}

func detQueries() []config.QueryDefinition {
	q := goldenQueries()
	return append(q,
		config.QueryDefinition{Name: "users_by_age_city", Database: "appdb", Collection: "users", Operation: "find", Filter: map[string]interface{}{"age": "<int>", "city": "<string>", "name": "<string>"}, DefinitionIndex: 5},
		config.QueryDefinition{Name: "users_agg", Database: "appdb", Collection: "users", Operation: "aggregate", Pipeline: []interface{}{map[string]interface{}{"$match": map[string]interface{}{"age": "<int>", "name": "<string>"}}}, DefinitionIndex: 6},
	)
}

func detCfg() *config.AppConfig {
	cfg := &config.AppConfig{
		Duration: "30s", Concurrency: 3, RandomSeed: 42, StatusRefreshRateSec: 1,
		WorkloadDeterministic: true, OperationsPerWorker: 200, DropCollections: true, DocumentsCount: 50,
		InsertBatchSize: 3, FindBatchSize: 4, FindLimit: 10, ExistingRecordHitRate: 60, RecordPoolMaxSize: 1000,
		UseTransactions: true, MaxTransactionOps: 3,
		FindPercent: 30, UpdatePercent: 15, DeletePercent: 10, InsertPercent: 15, BulkInsertPercent: 10, AggregatePercent: 10, TransactionPercent: 10,
	}
	cfg.WebUI.Enabled = true
	return cfg
}

type detCapture struct {
	mu    sync.Mutex
	lines map[int][]string
	kinds map[int]int
	ops   map[int][]*detOp
}

// runDeterministic runs RunWorkload with a nil database and the send step
// replaced: fail decides which operations fail. Any database read would
// panic on the nil database.
func runDeterministic(t *testing.T, ctx context.Context, cfg *config.AppConfig, fail func(worker int, seq uint64) bool) (*stats.GeneratedTrafficInfo, *detCapture, error) {
	t.Helper()
	t.Cleanup(func() { datagen.ConfigureSeed(0) })
	noShardingSeams(t, nil)
	c := &detCapture{lines: map[int][]string{}, kinds: map[int]int{}, ops: map[int][]*detOp{}}
	orig := deterministicExecFn
	deterministicExecFn = func(ctx context.Context, wCfg workloadConfig, w *detWorker, op *detOp, out *workloadrec.Outcome) bool {
		c.mu.Lock()
		c.lines[w.id] = append(c.lines[w.id], string(op.hashLine))
		c.kinds[op.kind]++
		c.ops[w.id] = append(c.ops[w.id], op)
		c.mu.Unlock()
		ok := fail == nil || !fail(w.id, op.seq)
		if out != nil {
			out.OK, out.LatencyUS, op.shape = ok, int64(op.seq), op.opType+"|fake"
		}
		return ok
	}
	t.Cleanup(func() { deterministicExecFn = orig })
	collector := stats.NewCollector()
	err := RunWorkload(ctx, nil, detCollections(), detQueries(), cfg, collector)
	return collector.GeneratedTraffic(), c, err
}

func TestDeterministicRunsSendIdenticalTraffic(t *testing.T) {
	a, ca, err := runDeterministic(t, context.Background(), detCfg(), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, cb, err := runDeterministic(t, context.Background(), detCfg(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if a == nil || a.Status != "complete" || a.OpsGenerated != 600 || a.OpsPlanned != 600 || a.OpsSent == 0 || a.OpsSent > 600 {
		t.Fatalf("traffic info = %+v", a)
	}
	if a.Fingerprint != b.Fingerprint || a.CommandsHash != b.CommandsHash || !reflect.DeepEqual(a.WorkerHashes, b.WorkerHashes) || len(a.WorkerHashes) != 3 {
		t.Fatalf("same seed and config gave different fingerprints:\n%+v\n%+v", a, b)
	}
	if !reflect.DeepEqual(ca.lines, cb.lines) {
		t.Fatal("same seed and config sent different commands")
	}
	for _, kind := range []int{detStandard, detFastInsert, detFastFind, detTransaction} {
		if ca.kinds[kind] == 0 {
			t.Fatalf("no operation of kind %d was generated: %v", kind, ca.kinds)
		}
	}
	for id, lines := range ca.lines {
		h := workloadrec.NewStreamHash()
		for _, l := range lines {
			h.Add([]byte(l))
		}
		if got := h.Sum(); got != a.WorkerHashes[itoa(id)] {
			t.Fatalf("worker %d hash %s does not cover the commands sent (%s)", id, a.WorkerHashes[itoa(id)], got)
		}
	}
	if a.CollectionsHash == "" || a.QueriesHash == "" || a.PLGMVersion == "" || a.Workers != 3 || a.OperationsPerWorker != 200 || a.Iterations != 1 {
		t.Fatalf("fingerprint header fields missing: %+v", a)
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

func TestDeterministicFingerprintChangesWithSeedAndWorkers(t *testing.T) {
	base, _, err := runDeterministic(t, context.Background(), detCfg(), nil)
	if err != nil {
		t.Fatal(err)
	}
	seed := detCfg()
	seed.RandomSeed = 43
	other, _, err := runDeterministic(t, context.Background(), seed, nil)
	if err != nil {
		t.Fatal(err)
	}
	if other.Fingerprint == base.Fingerprint || other.WorkerHashes["1"] == base.WorkerHashes["1"] {
		t.Fatal("a different seed gave the same traffic")
	}
	workers := detCfg()
	workers.Concurrency = 2
	fewer, _, err := runDeterministic(t, context.Background(), workers, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fewer.Fingerprint == base.Fingerprint || fewer.CommandsHash == base.CommandsHash {
		t.Fatal("a different worker count gave the same fingerprint")
	}
}

func TestDeterministicFailuresDoNotShiftLaterOps(t *testing.T) {
	_, clean, err := runDeterministic(t, context.Background(), detCfg(), nil)
	if err != nil {
		t.Fatal(err)
	}
	failed, faulty, err := runDeterministic(t, context.Background(), detCfg(), func(worker int, seq uint64) bool {
		return worker == 1 && seq >= 5 && seq <= 40 || worker == 2 && seq%3 == 0
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(clean.lines, faulty.lines) {
		t.Fatal("failed operations changed the commands of later operations")
	}
	if failed.Status != "complete" {
		t.Fatalf("status = %s", failed.Status)
	}
}

func TestDeterministicTransactionIsGeneratedOnceForRetries(t *testing.T) {
	_, c, err := runDeterministic(t, context.Background(), detCfg(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, ops := range c.ops {
		for _, op := range ops {
			if op.kind != detTransaction {
				continue
			}
			if len(op.txOps) == 0 {
				continue
			}
			// The ops handed to runTransaction (and reused by every retry of
			// its callback) are the ones the fingerprint and recording hold.
			want := bson.A{}
			for i := range op.txOps {
				want = append(want, txInnerCommand(nil, &op.txOps[i]))
			}
			got, _ := workloadrec.EncodeCommand(op.command)
			wantJSON, _ := workloadrec.EncodeCommand(bson.D{{Key: "ops", Value: want}})
			if !bytes.Equal(got, wantJSON) || !bytes.HasSuffix(op.hashLine, got) {
				t.Fatal("the fingerprint does not cover the pre-built transaction ops")
			}
			return
		}
	}
	t.Fatal("no transaction with operations was generated")
}

func TestDeterministicExistingRecordsAreRegeneratedSeededDocuments(t *testing.T) {
	t.Cleanup(func() { datagen.ConfigureSeed(0) })
	cfg := detCfg()
	cfg.DocumentsCount = 20
	col := detCollections()[1]
	var seeded []map[string]interface{}
	err := insertRandomDocuments(context.Background(), col, cfg.DocumentsCount, cfg, func(ctx context.Context, batch []interface{}) error {
		for _, d := range batch {
			seeded = append(seeded, d.(map[string]interface{}))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	traffic, err := newTrafficFingerprint([]config.CollectionDefinition{col}, nil, cfg, 1)
	if err != nil {
		t.Fatal(err)
	}
	run, err := newDeterministicRun(context.Background(), []config.CollectionDefinition{col}, nil, cfg, 1)
	if err != nil {
		t.Fatal(err)
	}
	run.traffic = traffic
	w := newDetWorker(1, workloadConfig{appConfig: cfg, collections: []config.CollectionDefinition{col}, det: run, concurrency: 1})
	ns := collectionNamespace(nil, col)
	distinct := map[int]bool{}
	for i := 0; i < 300; i++ {
		w.reseed(uint64(i + 1))
		rec, ok := w.pickExisting(col, ns)
		if !ok {
			t.Fatal("no existing record with 20 seeded documents")
		}
		found := -1
		for k, d := range seeded {
			if reflect.DeepEqual(rec, d) {
				found = k
			}
		}
		if found < 0 {
			t.Fatalf("picked record %v is not one of the seeded documents", rec)
		}
		distinct[found] = true
	}
	if len(distinct) < 10 {
		t.Fatalf("only %d of 20 seeded documents were picked", len(distinct))
	}
}

func TestDeterministicOffLeavesGenerationUnchanged(t *testing.T) {
	t.Cleanup(func() { datagen.ConfigureSeed(0) })
	var got workloadConfig
	noShardingSeams(t, func(ctx context.Context, wCfg workloadConfig) error {
		got = wCfg
		return nil
	})
	cfg := detCfg()
	cfg.WorkloadDeterministic = false
	collector := stats.NewCollector()
	if err := RunWorkload(context.Background(), nil, detCollections(), detQueries(), cfg, collector); err != nil {
		t.Fatal(err)
	}
	if got.det != nil || collector.GeneratedTraffic() != nil {
		t.Fatal("deterministic generation is on although workload_deterministic is off")
	}
}

func TestDeterministicIterationsShareOneFingerprint(t *testing.T) {
	cfg := detCfg()
	cfg.Iterations = 2
	if _, _, err := runDeterministic(t, context.Background(), cfg, nil); err == nil || !strings.Contains(err.Error(), "WithWorkloadRecordingSession") {
		t.Fatalf("2 iterations without a session: err = %v", err)
	}
	twoIterations := func() (*stats.GeneratedTrafficInfo, *detCapture, *detCapture) {
		ctx, finish := WithWorkloadRecordingSession(context.Background())
		defer finish()
		first, c1, err := runDeterministic(t, ctx, cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		if first.Iterations != 1 || first.OpsPlanned != 600 {
			t.Fatalf("after iteration 1: %+v", first)
		}
		second, c2, err := runDeterministic(t, ctx, cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		return second, c1, c2
	}
	a, a1, a2 := twoIterations()
	if a.Iterations != 2 || a.OpsPlanned != 1200 || a.OpsGenerated != 1200 || a.Status != "complete" {
		t.Fatalf("after iteration 2: %+v", a)
	}
	if reflect.DeepEqual(a1.lines, a2.lines) {
		t.Fatal("both iterations sent the same commands; the iteration must be part of the derivation")
	}
	b, _, _ := twoIterations()
	if a.Fingerprint != b.Fingerprint {
		t.Fatal("repeating a two-iteration run changed the fingerprint")
	}
}

func TestDeterministicSafetyTimeoutAndStopAreIncomplete(t *testing.T) {
	cfg := detCfg()
	cfg.Duration = "100ms"
	cfg.OperationsPerWorker = 1000000
	slow := func(int, uint64) bool { time.Sleep(time.Millisecond); return false }
	start := time.Now()
	info, _, err := runDeterministic(t, context.Background(), cfg, slow)
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != "incomplete" || !strings.Contains(info.Reason, "safety timeout (duration=100ms)") || info.OpsGenerated >= info.OpsPlanned {
		t.Fatalf("timed out run: %+v", info)
	}
	if time.Since(start) < 100*time.Millisecond {
		t.Fatal("the run ended before its safety timeout")
	}

	cfg.Duration = "30s"
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	info, _, err = runDeterministic(t, ctx, cfg, slow)
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != "incomplete" || !strings.Contains(info.Reason, "stopped") {
		t.Fatalf("stopped run: %+v", info)
	}
}

func TestDeterministicRunWorkloadRejectsInvalidSettings(t *testing.T) {
	noShardingSeams(t, nil)
	t.Cleanup(func() { datagen.ConfigureSeed(0) })
	for name, mutate := range map[string]func(*config.AppConfig){
		"no seed":      func(c *config.AppConfig) { c.RandomSeed = 0 },
		"no budget":    func(c *config.AppConfig) { c.OperationsPerWorker = 0 },
		"load profile": func(c *config.AppConfig) { c.LoadProfile = loadprofile.Config{Kind: "ramp"} },
		"no drop":      func(c *config.AppConfig) { c.DropCollections = false },
	} {
		cfg := detCfg()
		mutate(cfg)
		err := RunWorkload(context.Background(), nil, detCollections(), detQueries(), cfg, stats.NewCollector())
		if err == nil || !strings.Contains(err.Error(), "invalid deterministic workload settings") {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}
