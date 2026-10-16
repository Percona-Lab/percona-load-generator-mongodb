// Package multitarget runs one replay (or deterministic run) against each
// instance listed in workload_replay_targets, sequentially or concurrently,
// and compares every target with the first, like pt-upgrade against several
// hosts. Each target has its own client, workers, stats collector, outcome
// file, and run metadata; only listed targets are written to.
package multitarget

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/db"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/mongo"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/replaycompare"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	mongodrv "go.mongodb.org/mongo-driver/v2/mongo"
)

// Target states.
const (
	StatusPending   = "pending"
	StatusPreparing = "preparing"
	StatusRunning   = "running"
	StatusComplete  = "complete"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
	// StatusIncomplete is a deterministic run stopped by its safety timeout,
	// or a replay of a recording that ended early.
	StatusIncomplete = "incomplete"

	KindReplay        = "replay"
	KindDeterministic = "deterministic"

	ConcurrentNote = "The targets ran concurrently from one PLGM host: they shared its CPU, memory, and network, so client-side latencies may be affected. Compare the per-target client-side lag; a target that fell far behind may be limited by the PLGM host rather than the server."
	SequentialNote = "The targets ran one after another (sequential), so they did not compete for the PLGM host."
)

// Deps are the steps of one target's run; tests replace them.
type Deps struct {
	// Connect opens a client for the target's config and returns the
	// workload database and a func that closes the client.
	Connect func(ctx context.Context, cfg *config.AppConfig, dbName string) (*mongodrv.Database, func(), error)
	// Prepare creates the collections and indexes and seeds the data.
	Prepare func(ctx context.Context, t *Target, db *mongodrv.Database) error
	// Run runs the workload iterations of the target.
	Run     func(ctx context.Context, t *Target, db *mongodrv.Database) error
	Compare func(ctx context.Context, a, b string, opts replaycompare.Options) (*replaycompare.Report, error)
	// CompareOptions, when set, adjusts the options of ref vs t (for
	// example with plan annotations from their trend runs).
	CompareOptions func(ref, t *Target, opts *replaycompare.Options)
	// OnChange is called after a target changes state.
	OnChange func(t *Target)
}

// Target is one instance of the plan.
type Target struct {
	Index        int
	Label        string
	Config       *config.AppConfig
	Collector    *stats.Collector
	OutcomesPath string
	Collections  []config.CollectionDefinition
	Queries      []config.QueryDefinition
	// TrendRunID is the trend run the Web UI recorded for the target.
	TrendRunID string

	progress mongo.TargetProgress
	mu       sync.Mutex
	status   string
	err      string
	started  time.Time
	finished time.Time
}

// TargetState is a snapshot of a target for the CLI, the dashboard, and the
// run summary. It never holds the URI.
type TargetState struct {
	Index       int      `json:"index"`
	Label       string   `json:"label"`
	Status      string   `json:"status"`
	Error       string   `json:"error,omitempty"`
	OpsDone     uint64   `json:"ops_done"`
	OpsTotal    uint64   `json:"ops_total,omitempty"`
	OpsSkipped  uint64   `json:"ops_skipped,omitempty"`
	ElapsedSec  float64  `json:"elapsed_sec"`
	LagMaxMs    *float64 `json:"lag_max_ms,omitempty"`
	LagAvgMs    *float64 `json:"lag_avg_ms,omitempty"`
	OutcomeFile string   `json:"outcome_file"`
	// TrafficFingerprint is the deterministic traffic fingerprint.
	TrafficFingerprint string `json:"traffic_fingerprint,omitempty"`
	// DataFingerprintMatch compares the starting data with the recording.
	DataFingerprintMatch *bool  `json:"data_fingerprint_match,omitempty"`
	ServerVersion        string `json:"server_version,omitempty"`
}

func (t *Target) setStatus(status, errText string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.status, t.err = status, errText
	switch status {
	case StatusRunning:
		t.started = time.Now()
	case StatusComplete, StatusFailed, StatusCancelled, StatusIncomplete:
		if !t.started.IsZero() && t.finished.IsZero() {
			t.finished = time.Now()
		}
	}
}

// Status returns the target's state.
func (t *Target) Status() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.status
}

// State returns a snapshot of the target.
func (t *Target) State() TargetState {
	t.mu.Lock()
	s := TargetState{Index: t.Index, Label: t.Label, Status: t.status, Error: t.err, OutcomeFile: t.OutcomesPath}
	if !t.started.IsZero() {
		end := t.finished
		if end.IsZero() {
			end = time.Now()
		}
		s.ElapsedSec = end.Sub(t.started).Seconds()
	}
	t.mu.Unlock()
	s.OpsDone, s.OpsSkipped, s.OpsTotal = t.progress.Sent.Load(), t.progress.Skipped.Load(), t.progress.Total.Load()
	if rep := t.Collector.WorkloadReplay(); rep != nil {
		s.LagMaxMs, s.LagAvgMs = rep.LagMaxMs, rep.LagAvgMs
		if rep.DataFingerprint != nil {
			m := rep.DataFingerprint.Match
			s.DataFingerprintMatch = &m
		}
	}
	if g := t.Collector.GeneratedTraffic(); g != nil {
		s.TrafficFingerprint = g.Fingerprint
		s.OpsTotal = g.OpsPlanned
		if s.OpsDone == 0 {
			s.OpsDone = g.OpsSent
		}
	}
	return s
}

// Comparison is one target compared with the reference (target 1).
type Comparison struct {
	Reference string `json:"reference"`
	Target    string `json:"target"`
	// Report holds literal values when the commands are available; it is
	// kept in memory (Web UI) or written to the files below (CLI).
	Report       *replaycompare.Report `json:"-"`
	ReportID     string                `json:"report_id,omitempty"`
	JSONPath     string                `json:"json_path,omitempty"`
	HTMLPath     string                `json:"html_path,omitempty"`
	DifferingOps uint64                `json:"differing_ops"`
	Verdict      string                `json:"latency_verdict,omitempty"`
	Error        string                `json:"error,omitempty"`
}

// Result is the outcome of a multi-target run.
type Result struct {
	Kind        string        `json:"kind"`
	Mode        string        `json:"mode"`
	Reference   string        `json:"reference"`
	Targets     []TargetState `json:"targets"`
	Comparisons []Comparison  `json:"comparisons"`
	// FingerprintsMatch is set for deterministic runs: every complete
	// target sent the same traffic as the reference.
	FingerprintsMatch *bool    `json:"fingerprints_match,omitempty"`
	Notes             []string `json:"notes,omitempty"`
}

// Plan is a multi-target run.
type Plan struct {
	Kind, Mode  string
	DBName      string
	Base        *config.AppConfig
	Collections []config.CollectionDefinition
	Queries     []config.QueryDefinition
	Targets     []*Target
	Deps        Deps
	// WriteReports writes each comparison as JSON and HTML (mode 0600) next
	// to the outcome files; otherwise reports are only kept in the Result.
	WriteReports bool

	mu     sync.Mutex
	result *Result
	ran    atomic.Bool
}

// Enabled reports whether cfg lists replay targets.
func Enabled(cfg *config.AppConfig) bool {
	return cfg != nil && len(cfg.WorkloadReplayTargets) > 0
}

// NewPlan builds the per-target configs and collectors. base must pass
// config.ValidateWorkloadTargets.
func NewPlan(base *config.AppConfig, dbName string, collections []config.CollectionDefinition, queries []config.QueryDefinition) (*Plan, error) {
	if err := config.ValidateWorkloadTargets(base); err != nil {
		return nil, err
	}
	if !Enabled(base) {
		return nil, errors.New("no workload_replay_targets")
	}
	p := &Plan{Kind: KindReplay, Mode: config.WorkloadTargetsSequential, DBName: dbName, Base: base, Collections: collections, Queries: queries,
		Deps: Deps{Connect: connect, Prepare: DefaultPrepare, Run: DefaultRun, Compare: replaycompare.Compare}}
	if !base.WorkloadReplayEnabled {
		p.Kind = KindDeterministic
	}
	if config.ConcurrentTargets(base) {
		p.Mode = config.WorkloadTargetsConcurrent
	}
	for i, tg := range base.WorkloadReplayTargets {
		cfg := *base
		cfg.URI = tg.URI
		cfg.WorkloadReplayTargets = nil
		if p.Kind == KindReplay {
			cfg.WorkloadReplayOutcomesPath = config.TargetPath(base.WorkloadReplayOutcomesPath, tg.Label)
		} else {
			cfg.WorkloadDeterministicOutcomesPath = config.TargetPath(base.WorkloadDeterministicOutcomesPath, tg.Label)
		}
		if cfg.CSVExportEnabled {
			cfg.CSVExportPath = config.TargetPath(base.CSVExportPath, tg.Label)
		}
		cfg.TrendRunLabel = tg.Label
		if l := strings.TrimSpace(base.TrendRunLabel); l != "" {
			cfg.TrendRunLabel = config.SanitizeTrendRunLabel(l + "-" + tg.Label)
		}
		t := &Target{Index: i + 1, Label: tg.Label, Config: &cfg, status: StatusPending,
			Collections: config.CloneCollections(collections), Queries: config.CloneQueries(queries)}
		t.OutcomesPath = cfg.WorkloadReplayOutcomesPath
		if p.Kind == KindDeterministic {
			t.OutcomesPath = cfg.WorkloadDeterministicOutcomesPath
		}
		t.Collector = stats.NewCollector()
		if p.Mode == config.WorkloadTargetsConcurrent {
			t.Collector.OutputLabel = tg.Label
		}
		t.Collector.ConfigureInsights(&cfg)
		t.Collector.SetCollectionsForInsights(t.Collections)
		t.Collector.SetWorkloadTarget(&stats.WorkloadTargetInfo{Label: tg.Label, Index: i + 1, Targets: len(base.WorkloadReplayTargets), Mode: p.Mode, Reference: base.WorkloadReplayTargets[0].Label})
		p.Targets = append(p.Targets, t)
	}
	return p, nil
}

func connect(ctx context.Context, cfg *config.AppConfig, dbName string) (*mongodrv.Database, func(), error) {
	conn, err := db.Connect(ctx, cfg, dbName)
	if err != nil {
		return nil, nil, err
	}
	return conn.Database, func() { conn.Disconnect(context.Background()) }, nil
}

// DefaultPrepare creates the collections and indexes of a target and seeds
// it, as a single-target run does.
func DefaultPrepare(ctx context.Context, t *Target, database *mongodrv.Database) error {
	cfg := t.Config
	file := &config.CollectionsFile{Collections: t.Collections}
	if err := mongo.CreateCollectionsFromConfig(ctx, database, file, cfg.DropCollections); err != nil {
		return fmt.Errorf("create collections: %w", err)
	}
	if err := mongo.CreateIndexesFromConfig(ctx, database, file); err != nil {
		return fmt.Errorf("create indexes: %w", err)
	}
	if !cfg.SkipSeed && cfg.DocumentsCount > 0 {
		for _, col := range t.Collections {
			if err := mongo.InsertRandomDocuments(ctx, database, col, cfg.DocumentsCount, cfg); err != nil {
				return fmt.Errorf("seed %s: %w", col.Name, err)
			}
		}
	}
	return nil
}

// DefaultRun runs the target's iterations in one recording session, as a
// single-target run does.
func DefaultRun(ctx context.Context, t *Target, database *mongodrv.Database) error {
	cfg := t.Config
	iterations := config.RunIterations(cfg)
	interval, _ := time.ParseDuration(cfg.IntervalDelay)
	runCtx, finish := mongo.WithWorkloadRecordingSession(ctx)
	defer finish()
	for i := 1; i <= iterations; i++ {
		if err := mongo.RunWorkload(runCtx, database, t.Collections, t.Queries, cfg, t.Collector); err != nil {
			return err
		}
		if i < iterations && interval > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(interval):
			}
		}
	}
	return nil
}

func (p *Plan) changed(t *Target) {
	if p.Deps.OnChange != nil {
		p.Deps.OnChange(t)
	}
}

func (p *Plan) fail(t *Target, err error) {
	status := StatusFailed
	if errors.Is(err, context.Canceled) {
		status = StatusCancelled
	}
	t.setStatus(status, err.Error())
	log.Printf("[Multi-Target] target %s: %s: %v", t.Label, status, err)
	p.changed(t)
}

type conn struct {
	db    *mongodrv.Database
	close func()
}

// prepare connects to a target and prepares its data.
func (p *Plan) prepare(ctx context.Context, t *Target) (*conn, bool) {
	if err := ctx.Err(); err != nil {
		p.fail(t, err)
		return nil, false
	}
	t.setStatus(StatusPreparing, "")
	p.changed(t)
	log.Printf("[Multi-Target] target %d of %d (%s): connecting and preparing data", t.Index, len(p.Targets), t.Label)
	database, closeFn, err := p.Deps.Connect(ctx, t.Config, p.DBName)
	if err != nil {
		p.fail(t, fmt.Errorf("connect: %w", err))
		return nil, false
	}
	if err := p.Deps.Prepare(ctx, t, database); err != nil {
		closeFn()
		p.fail(t, err)
		return nil, false
	}
	return &conn{db: database, close: closeFn}, true
}

func (p *Plan) run(ctx context.Context, t *Target, c *conn) {
	defer c.close()
	t.setStatus(StatusRunning, "")
	p.changed(t)
	log.Printf("[Multi-Target] target %d of %d (%s): running the %s", t.Index, len(p.Targets), t.Label, p.Kind)
	runCtx := mongo.WithWorkloadTarget(ctx, t.Label, p.Mode == config.WorkloadTargetsConcurrent, &t.progress)
	if err := p.Deps.Run(runCtx, t, c.db); err != nil {
		p.fail(t, err)
		return
	}
	status := StatusComplete
	if rep := t.Collector.WorkloadReplay(); rep != nil && rep.Status != StatusComplete {
		status = rep.Status
	}
	if g := t.Collector.GeneratedTraffic(); g != nil && g.Status != StatusComplete {
		status = StatusIncomplete
	}
	if ctx.Err() != nil && status == StatusComplete {
		status = StatusCancelled
	}
	t.setStatus(status, "")
	log.Printf("[Multi-Target] target %s: %s", t.Label, status)
	p.changed(t)
}

// Run runs every target and compares each with the first. Sequential runs
// prepare and run one target after another. Concurrent runs prepare every
// target first, one at a time (seeding uses a process-wide seed sequence),
// then start all workloads together. A failed target does not stop the
// others.
func (p *Plan) Run(ctx context.Context) *Result {
	if !p.ran.CompareAndSwap(false, true) {
		return p.Result()
	}
	if strings.TrimSpace(p.Base.URI) != "" {
		log.Printf("[Multi-Target] the uri setting is not used: operations go only to the %d listed targets", len(p.Targets))
	}
	if p.Mode == config.WorkloadTargetsConcurrent {
		log.Printf("[Multi-Target] %d targets run concurrently; the per-interval CLI lines of the targets interleave, each prefixed with its [label]", len(p.Targets))
		conns := make([]*conn, len(p.Targets))
		for i, t := range p.Targets {
			conns[i], _ = p.prepare(ctx, t)
		}
		var wg sync.WaitGroup
		for i, t := range p.Targets {
			if conns[i] == nil {
				continue
			}
			wg.Add(1)
			go func(t *Target, c *conn) {
				defer wg.Done()
				defer func() {
					if r := recover(); r != nil {
						c.close()
						p.fail(t, fmt.Errorf("panic: %v", r))
					}
				}()
				p.run(ctx, t, c)
			}(t, conns[i])
		}
		wg.Wait()
	} else {
		for _, t := range p.Targets {
			if c, ok := p.prepare(ctx, t); ok {
				p.run(ctx, t, c)
			}
		}
	}
	res := p.compare(ctx)
	p.mu.Lock()
	p.result = res
	p.mu.Unlock()
	return res
}

// Result is the finished result, or nil while the plan runs.
func (p *Plan) Result() *Result {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.result
}

// States returns a snapshot of every target.
func (p *Plan) States() []TargetState {
	out := make([]TargetState, len(p.Targets))
	for i, t := range p.Targets {
		out[i] = t.State()
	}
	return out
}

func lagText(s TargetState) string {
	if s.LagMaxMs == nil {
		return "not measured"
	}
	return fmt.Sprintf("max %.1f ms, avg %.1f ms", *s.LagMaxMs, *s.LagAvgMs)
}

func (p *Plan) notes(states []TargetState) []string {
	if p.Mode != config.WorkloadTargetsConcurrent {
		return []string{SequentialNote}
	}
	parts := make([]string, 0, len(states))
	for _, s := range states {
		if p.Kind == KindReplay {
			parts = append(parts, fmt.Sprintf("%s %s", s.Label, lagText(s)))
		} else {
			parts = append(parts, fmt.Sprintf("%s %.1f s", s.Label, s.ElapsedSec))
		}
	}
	what := "Client-side lag behind the recorded timeline"
	if p.Kind == KindDeterministic {
		what = "Elapsed time (deterministic runs have no recorded timeline)"
	}
	return []string{ConcurrentNote, what + ": " + strings.Join(parts, "; ") + "."}
}

func (p *Plan) compare(ctx context.Context) *Result {
	states := p.States()
	ref := p.Targets[0]
	res := &Result{Kind: p.Kind, Mode: p.Mode, Reference: ref.Label, Targets: states, Comparisons: []Comparison{}, Notes: p.notes(states)}
	if p.Kind == KindDeterministic {
		match := true
		for _, s := range states[1:] {
			if states[0].TrafficFingerprint == "" || s.TrafficFingerprint != states[0].TrafficFingerprint {
				match = false
			}
		}
		res.FingerprintsMatch = &match
		if !match {
			res.Notes = append(res.Notes, "Not every target reported the reference's traffic fingerprint: those targets did not send the same generated operations (or did not finish).")
		}
	}
	opts := replaycompare.Options{Notes: res.Notes}
	if p.Kind == KindReplay {
		opts.RecordingPath = p.Base.WorkloadReplayPath
	}
	for _, t := range p.Targets[1:] {
		c := Comparison{Reference: ref.Label, Target: t.Label}
		if err := p.compareOne(ctx, ref, t, opts, &c); err != nil {
			c.Error = err.Error()
			log.Printf("[Multi-Target] %s vs %s: comparison not available: %v", ref.Label, t.Label, err)
		}
		res.Comparisons = append(res.Comparisons, c)
	}
	return res
}

func (p *Plan) compareOne(ctx context.Context, ref, t *Target, opts replaycompare.Options, c *Comparison) error {
	for _, x := range []*Target{ref, t} {
		if _, err := os.Stat(x.OutcomesPath); err != nil {
			return fmt.Errorf("target %s has no outcome file (%s)", x.Label, x.Status())
		}
	}
	opts.LabelA, opts.LabelB = ref.Label, t.Label
	if p.Deps.CompareOptions != nil {
		p.Deps.CompareOptions(ref, t, &opts)
	}
	rep, err := p.Deps.Compare(ctx, ref.OutcomesPath, t.OutcomesPath, opts)
	if err != nil {
		return err
	}
	c.Report, c.DifferingOps, c.Verdict = rep, rep.Overall.DifferingOps, rep.Overall.Latency.Verdict
	if !p.WriteReports {
		return nil
	}
	base := p.Base.WorkloadReplayOutcomesPath
	if p.Kind == KindDeterministic {
		base = p.Base.WorkloadDeterministicOutcomesPath
	}
	base = strings.TrimSuffix(base, filepath.Ext(base))
	stem := fmt.Sprintf("%s.compare.%s-vs-%s", base, ref.Label, t.Label)
	c.JSONPath, c.HTMLPath = stem+".json", stem+".html"
	return replaycompare.WriteFiles(rep, c.JSONPath, c.HTMLPath)
}

// SummaryText is the CLI summary of a finished multi-target run.
func (r *Result) SummaryText() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Multi-target %s (%s): reference %s\n", r.Kind, r.Mode, r.Reference)
	for _, s := range r.Targets {
		fmt.Fprintf(&b, "  %d. %-16s %-10s ops %d", s.Index, s.Label, s.Status, s.OpsDone)
		if s.OpsTotal > 0 {
			fmt.Fprintf(&b, "/%d", s.OpsTotal)
		}
		if s.OpsSkipped > 0 {
			fmt.Fprintf(&b, " (%d writes skipped)", s.OpsSkipped)
		}
		fmt.Fprintf(&b, ", %.1f s", s.ElapsedSec)
		if s.LagMaxMs != nil {
			fmt.Fprintf(&b, ", lag %s", lagText(s))
		}
		if s.DataFingerprintMatch != nil {
			fmt.Fprintf(&b, ", starting data matches recording: %t", *s.DataFingerprintMatch)
		}
		if s.TrafficFingerprint != "" {
			fmt.Fprintf(&b, ", traffic %s", s.TrafficFingerprint[:min(12, len(s.TrafficFingerprint))])
		}
		if s.Error != "" {
			fmt.Fprintf(&b, ", error: %s", s.Error)
		}
		b.WriteString("\n")
	}
	if r.FingerprintsMatch != nil {
		fmt.Fprintf(&b, "Traffic fingerprints match the reference: %t\n", *r.FingerprintsMatch)
	}
	for _, c := range r.Comparisons {
		if c.Error != "" {
			fmt.Fprintf(&b, "%s vs %s: not compared (%s)\n", c.Reference, c.Target, c.Error)
			continue
		}
		fmt.Fprintf(&b, "%s vs %s: %d differing operations, latency %s (heuristic)", c.Reference, c.Target, c.DifferingOps, strings.ReplaceAll(c.Verdict, "_", " "))
		if c.HTMLPath != "" {
			fmt.Fprintf(&b, "; report %s, %s (mode 0600)", c.JSONPath, c.HTMLPath)
		}
		b.WriteString("\n")
	}
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "NOTE: %s\n", n)
	}
	return b.String()
}
