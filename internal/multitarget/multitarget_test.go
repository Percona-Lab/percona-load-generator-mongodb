package multitarget

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/workloadrec"
	"go.mongodb.org/mongo-driver/v2/bson"
	mongodrv "go.mongodb.org/mongo-driver/v2/mongo"
)

const fixtureOps = 12

var labels = []string{"m7", "m8", "m9"}

func sum(c byte) string { return strings.Repeat(string(c), 64) }

// recording writes a recording of fixtureOps finds on two workers.
func recording(t *testing.T, dir string) (string, string) {
	t.Helper()
	path := filepath.Join(dir, "rec.ndjson")
	r, err := workloadrec.NewRecorder(path, workloadrec.Options{}, workloadrec.Header{PLGMVersion: "test", Workers: 2, Outcomes: true, ResultChecksums: true, Namespaces: []string{"app.users"}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= fixtureOps; i++ {
		out := workloadrec.Outcome{LatencyUS: 1000, OK: true, Returned: workloadrec.Count(1), Checksum: sum('a')}
		r.Record(workloadrec.Entry{Worker: 1 + i%2, NS: "app.users", Op: "find", OK: true, Shape: &workloadrec.ShapeRef{Key: "find|k", Summary: "find on fields: k"},
			Command: bson.D{{Key: "find", Value: "users"}, {Key: "filter", Value: bson.D{{Key: "k", Value: i}}}}, Outcome: &out})
	}
	if res := r.Close(); res.Status != workloadrec.StatusComplete {
		t.Fatalf("recording: %+v", res)
	}
	info, err := workloadrec.Scan(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, info.HeaderHash
}

func baseConfig(dir, recPath string) *config.AppConfig {
	cfg := &config.AppConfig{URI: "mongodb://never-used:27017", WorkloadReplayEnabled: true, WorkloadReplayPath: recPath, WorkloadRecordOutcomes: true,
		WorkloadReplayOutcomesPath: filepath.Join(dir, "out.ndjson"), TrendRunLabel: "upgrade"}
	for i, l := range labels {
		cfg.WorkloadReplayTargets = append(cfg.WorkloadReplayTargets, config.ReplayTarget{Label: l, URI: "mongodb://host" + string(rune('a'+i)) + ":27017"})
	}
	return cfg
}

// fakes replace the server: Run writes a replay outcome file of the
// recording, with checksum c on the target labelled checksumOn.
type fakes struct {
	t                *testing.T
	recPath, hash    string
	checksumOn       string
	failOn           string
	waitForAll       *sync.WaitGroup
	mu               sync.Mutex
	events, uris     []string
	running, maxLive int
}

func (f *fakes) event(e string) {
	f.mu.Lock()
	f.events = append(f.events, e)
	f.mu.Unlock()
}

func (f *fakes) deps(p *Plan) {
	p.Deps.Connect = func(ctx context.Context, cfg *config.AppConfig, dbName string) (*mongodrv.Database, func(), error) {
		f.mu.Lock()
		f.uris = append(f.uris, cfg.URI)
		f.mu.Unlock()
		return nil, func() {}, nil
	}
	p.Deps.Prepare = func(ctx context.Context, t *Target, db *mongodrv.Database) error {
		f.event("prepare " + t.Label)
		return nil
	}
	p.Deps.Run = func(ctx context.Context, t *Target, db *mongodrv.Database) error {
		f.mu.Lock()
		f.running++
		f.maxLive = max(f.maxLive, f.running)
		f.events = append(f.events, "run "+t.Label)
		f.mu.Unlock()
		defer func() {
			f.mu.Lock()
			f.running--
			f.mu.Unlock()
		}()
		if f.waitForAll != nil {
			f.waitForAll.Done()
			done := make(chan struct{})
			go func() { f.waitForAll.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				return errors.New("the targets did not run at the same time")
			}
		}
		if t.Label == f.failOn {
			return errors.New("connection refused")
		}
		return f.writeReplay(ctx, t)
	}
}

func (f *fakes) writeReplay(ctx context.Context, t *Target) error {
	w, err := workloadrec.NewOutcomeWriter(t.OutcomesPath, workloadrec.OutcomeHeader{Source: workloadrec.OutcomeSourceReplay, PLGMVersion: "test", Workers: 2, ResultChecksums: true, RecordingFile: "rec.ndjson", RecordingHeaderHash: f.hash, Target: t.Label})
	if err != nil {
		return err
	}
	var n uint64
	lag := 2.0
	err = workloadrec.Stream(ctx, f.recPath, func(op *workloadrec.OpLine) error {
		c := sum('a')
		if t.Label == f.checksumOn {
			c = sum('b')
		}
		t.Collector.Track("find", time.Millisecond)
		w.Write(workloadrec.OutcomeLine{Seq: op.Seq, Worker: op.Worker, WorkerSeq: op.WorkerSeq, NS: op.NS, Op: op.Op, Shape: "find|k", Outcome: workloadrec.Outcome{LatencyUS: 1000, OK: true, Returned: workloadrec.Count(1), Checksum: c}})
		t.progress.Sent.Add(1)
		n++
		return nil
	})
	w.Close(workloadrec.OutcomeTrailer{}, workloadrec.StatusComplete, "")
	t.progress.Total.Store(n)
	t.Collector.SetWorkloadReplay(&stats.WorkloadReplayInfo{Status: workloadrec.StatusComplete, OpsTotal: n, OpsReplayed: n, LagMaxMs: &lag, LagAvgMs: &lag})
	return err
}

func newPlan(t *testing.T, mode string) (*Plan, *fakes) {
	t.Helper()
	dir := t.TempDir()
	rec, hash := recording(t, dir)
	cfg := baseConfig(dir, rec)
	cfg.WorkloadReplayTargetsMode = mode
	p, err := NewPlan(cfg, "app", []config.CollectionDefinition{{Name: "users", DatabaseName: "app"}}, []config.QueryDefinition{{Database: "app", Collection: "users", Operation: "find", Filter: map[string]interface{}{"k": 1}}})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakes{t: t, recPath: rec, hash: hash}
	f.deps(p)
	p.WriteReports = true
	return p, f
}

func TestNewPlanIsolatesTargets(t *testing.T) {
	p, _ := newPlan(t, "")
	if p.Kind != KindReplay || p.Mode != config.WorkloadTargetsSequential || len(p.Targets) != 3 {
		t.Fatalf("plan = %s %s %d targets", p.Kind, p.Mode, len(p.Targets))
	}
	seen := map[*stats.Collector]bool{}
	for i, tg := range p.Targets {
		c := tg.Config
		if c.URI != p.Base.WorkloadReplayTargets[i].URI || len(c.WorkloadReplayTargets) != 0 || c.TrendRunLabel != "upgrade-"+tg.Label {
			t.Fatalf("target %s config: uri %q, %d targets, trend label %q", tg.Label, c.URI, len(c.WorkloadReplayTargets), c.TrendRunLabel)
		}
		if want := config.TargetPath(p.Base.WorkloadReplayOutcomesPath, tg.Label); tg.OutcomesPath != want || c.WorkloadReplayOutcomesPath != want {
			t.Fatalf("target %s outcomes %q, want %q", tg.Label, tg.OutcomesPath, want)
		}
		if seen[tg.Collector] || tg.Collector == nil {
			t.Fatal("targets must not share a collector")
		}
		seen[tg.Collector] = true
		md := tg.Collector.GetFinalInsights().Metadata
		if md.Target == nil || md.Target.Label != tg.Label || md.Target.Index != i+1 || md.Target.Targets != 3 || md.Target.Reference != "m7" {
			t.Fatalf("target metadata = %+v", md.Target)
		}
	}
	p.Targets[0].Queries[0].Filter["k"] = 99
	if p.Targets[1].Queries[0].Filter["k"] != 1 || p.Queries[0].Filter["k"] != 1 {
		t.Fatal("targets must get their own copy of the queries")
	}
	if p.Base.URI != "mongodb://never-used:27017" || len(p.Base.WorkloadReplayTargets) != 3 {
		t.Fatal("the base config must not change")
	}
	bad := *p.Base
	bad.WorkloadReplayTargets = bad.WorkloadReplayTargets[:1]
	if _, err := NewPlan(&bad, "app", nil, nil); err == nil {
		t.Fatal("one target must be rejected")
	}
}

func TestOnlyConcurrentTargetsLabelTheirCLIOutput(t *testing.T) {
	for mode, labeled := range map[string]bool{config.WorkloadTargetsSequential: false, config.WorkloadTargetsConcurrent: true} {
		p, _ := newPlan(t, mode)
		for _, tg := range p.Targets {
			if want := map[bool]string{true: tg.Label}[labeled]; tg.Collector.OutputLabel != want {
				t.Fatalf("%s target %s output label %q, want %q", mode, tg.Label, tg.Collector.OutputLabel, want)
			}
		}
	}
}

func TestSequentialRunComparesEachTargetWithTheFirst(t *testing.T) {
	p, f := newPlan(t, config.WorkloadTargetsSequential)
	f.checksumOn = "m9"
	res := p.Run(context.Background())
	if f.maxLive != 1 {
		t.Fatalf("sequential mode ran %d targets at once", f.maxLive)
	}
	want := "prepare m7,run m7,prepare m8,run m8,prepare m9,run m9"
	if got := strings.Join(f.events, ","); got != want {
		t.Fatalf("order = %s, want %s", got, want)
	}
	for _, u := range f.uris {
		if strings.Contains(u, "never-used") {
			t.Fatal("the uri setting must not be connected to when targets are listed")
		}
	}
	if len(f.uris) != 3 {
		t.Fatalf("connected %d times", len(f.uris))
	}
	if res.Reference != "m7" || len(res.Comparisons) != 2 || res.Notes[0] != SequentialNote || res.FingerprintsMatch != nil {
		t.Fatalf("result = %+v", res)
	}
	for _, s := range res.Targets {
		if s.Status != StatusComplete || s.OpsDone != fixtureOps || s.OpsTotal != fixtureOps || s.LagMaxMs == nil {
			t.Fatalf("target state = %+v", s)
		}
	}
	m8, m9 := res.Comparisons[0], res.Comparisons[1]
	if m8.Target != "m8" || m8.Error != "" || m8.DifferingOps != 0 || m8.Report == nil || !m8.Report.IdentityMatch {
		t.Fatalf("m7 vs m8 = %+v", m8)
	}
	if m9.Target != "m9" || m9.DifferingOps != fixtureOps || m9.Report.A.Label != "m7" || m9.Report.B.Label != "m9" || m9.Report.B.Target != "m9" {
		t.Fatalf("m7 vs m9 = %+v", m9)
	}
	for _, c := range res.Comparisons {
		for _, path := range []string{c.JSONPath, c.HTMLPath} {
			st, err := os.Stat(path)
			if err != nil || st.Mode().Perm() != 0o600 || !strings.Contains(path, ".compare.m7-vs-"+c.Target) {
				t.Fatalf("report %s: %v %v", path, st, err)
			}
		}
	}
	if s := res.SummaryText(); !strings.Contains(s, "m7 vs m9: 12 differing operations") || strings.Contains(s, "mongodb://") {
		t.Fatalf("summary:\n%s", s)
	}
	if p.Run(context.Background()) != res {
		t.Fatal("a plan runs once")
	}
}

func TestConcurrentRunPreparesFirstThenRunsTogether(t *testing.T) {
	p, f := newPlan(t, config.WorkloadTargetsConcurrent)
	f.waitForAll = &sync.WaitGroup{}
	f.waitForAll.Add(len(labels))
	res := p.Run(context.Background())
	if f.maxLive != len(labels) {
		t.Fatalf("concurrent mode ran at most %d targets at once", f.maxLive)
	}
	if got := strings.Join(f.events[:3], ","); got != "prepare m7,prepare m8,prepare m9" {
		t.Fatalf("every target must be prepared before the workloads start: %v", f.events)
	}
	if len(res.Notes) != 2 || res.Notes[0] != ConcurrentNote || !strings.Contains(res.Notes[1], "m9 max 2.0 ms") {
		t.Fatalf("notes = %v", res.Notes)
	}
	for _, c := range res.Comparisons {
		if c.Error != "" || c.DifferingOps != 0 || len(c.Report.Notes) != 2 || c.Report.Notes[0] != ConcurrentNote {
			t.Fatalf("comparison = %+v", c)
		}
	}
	for _, tg := range p.Targets {
		if n := tg.Collector.FindOps; n != fixtureOps {
			t.Fatalf("target %s counted %d finds (stats must not mix targets)", tg.Label, n)
		}
	}
}

func TestFailedTargetDoesNotStopTheOthers(t *testing.T) {
	p, f := newPlan(t, "")
	f.failOn = "m8"
	res := p.Run(context.Background())
	if res.Targets[1].Status != StatusFailed || res.Targets[2].Status != StatusComplete || !strings.Contains(res.Targets[1].Error, "connection refused") {
		t.Fatalf("targets = %+v", res.Targets)
	}
	if c := res.Comparisons[0]; c.Error == "" || c.Report != nil {
		t.Fatalf("a failed target cannot be compared: %+v", c)
	}
	if c := res.Comparisons[1]; c.Error != "" || c.Report == nil {
		t.Fatalf("m7 vs m9 = %+v", c)
	}
}

func TestCancelledSequentialRunSkipsTheRest(t *testing.T) {
	p, f := newPlan(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	run := p.Deps.Run
	p.Deps.Run = func(c context.Context, tg *Target, db *mongodrv.Database) error {
		err := run(c, tg, db)
		cancel()
		return err
	}
	res := p.Run(ctx)
	if res.Targets[0].Status != StatusCancelled || res.Targets[1].Status != StatusCancelled || res.Targets[2].Status != StatusCancelled {
		t.Fatalf("targets = %+v", res.Targets)
	}
	if strings.Join(f.events, ",") != "prepare m7,run m7" {
		t.Fatalf("a cancelled run must not start more targets: %v", f.events)
	}
}

func TestDeterministicTargetsCheckFingerprints(t *testing.T) {
	for _, tc := range []struct {
		name   string
		differ string
		match  bool
	}{{"equal", "", true}, {"different", "m8", false}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := baseConfig(dir, "")
			cfg.WorkloadReplayEnabled, cfg.WorkloadReplayPath, cfg.WorkloadReplayOutcomesPath = false, "", ""
			cfg.WorkloadDeterministic, cfg.WorkloadDeterministicOutcomesPath = true, filepath.Join(dir, "det.ndjson")
			cfg.WorkloadReplayTargetsMode = config.WorkloadTargetsConcurrent
			p, err := NewPlan(cfg, "app", nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			f := &fakes{t: t}
			f.deps(p)
			p.Deps.Run = func(ctx context.Context, tg *Target, db *mongodrv.Database) error {
				fp := sum('f')
				if tg.Label == tc.differ {
					fp = sum('e')
				}
				w, err := workloadrec.NewOutcomeWriter(tg.OutcomesPath, workloadrec.OutcomeHeader{Source: workloadrec.OutcomeSourceDeterministic, PLGMVersion: "test", Workers: 1, RandomSeed: 7, OperationsPerWorker: 3})
				if err != nil {
					return err
				}
				for ws := uint64(1); ws <= 3; ws++ {
					w.Write(workloadrec.OutcomeLine{Worker: 1, WorkerSeq: ws, Iteration: 1, NS: "app.users", Op: "find", Shape: "find|k", Outcome: workloadrec.Outcome{LatencyUS: 1000, OK: true, Returned: workloadrec.Count(1), Checksum: sum('a')}})
				}
				w.Close(workloadrec.OutcomeTrailer{TrafficFingerprint: fp, TrafficStatus: "complete"}, workloadrec.StatusComplete, "")
				tg.Collector.SetGeneratedTraffic(&stats.GeneratedTrafficInfo{Status: "complete", Fingerprint: fp, OpsPlanned: 3, OpsSent: 3})
				return nil
			}
			if !strings.HasSuffix(p.Targets[1].OutcomesPath, "det.m8.ndjson") || p.Kind != KindDeterministic {
				t.Fatalf("deterministic outcome path = %s", p.Targets[1].OutcomesPath)
			}
			res := p.Run(context.Background())
			if res.FingerprintsMatch == nil || *res.FingerprintsMatch != tc.match {
				t.Fatalf("fingerprints match = %v, want %v", res.FingerprintsMatch, tc.match)
			}
			if s := res.Targets[0]; s.OpsDone != 3 || s.OpsTotal != 3 || s.TrafficFingerprint != sum('f') {
				t.Fatalf("state = %+v", s)
			}
			m8 := res.Comparisons[0]
			if tc.match && (m8.Error != "" || !m8.Report.IdentityMatch) {
				t.Fatalf("equal fingerprints must compare: %+v", m8)
			}
			if !tc.match && m8.Error == "" && m8.Report.IdentityMatch {
				t.Fatalf("different fingerprints must not compare as the same traffic: %+v", m8)
			}
		})
	}
}
