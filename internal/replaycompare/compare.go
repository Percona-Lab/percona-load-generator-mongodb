// Package replaycompare compares two executions of the same operations, like
// pt-upgrade: a recording with outcomes and a replay of it, two replays of one
// recording, or two deterministic runs with the same traffic fingerprint. It
// reports per query shape and overall what got faster, slower, or different.
package replaycompare

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/workloadrec"
)

const (
	FormatVersion = "plgm-replay-compare/1"

	// MatchBySeq matches the recording's seq (recording and replay files);
	// MatchByWorkerSeq matches (iteration, worker, per-worker seq).
	MatchBySeq       = "seq"
	MatchByWorkerSeq = "iteration_worker_seq"

	SideRecording     = "recording"
	SideReplay        = workloadrec.OutcomeSourceReplay
	SideDeterministic = workloadrec.OutcomeSourceDeterministic

	IdentityRecording = "recording_header_hash"
	IdentityTraffic   = "traffic_fingerprint"

	VerdictSlower       = "slower"
	VerdictFaster       = "faster"
	VerdictNoChange     = "no_material_change"
	VerdictInsufficient = "insufficient_evidence"

	DiffResultCount = "result_count_mismatch"
	DiffChecksum    = "checksum_mismatch"
	DiffErrorOnlyA  = "error_only_a"
	DiffErrorOnlyB  = "error_only_b"
	DiffErrorCode   = "error_code_differs"
	DiffMissingInA  = "missing_in_a"
	DiffMissingInB  = "missing_in_b"

	DefaultTopN = 20
	MaxTopN     = 1000

	LatencyNote = "Latency verdicts are PLGM heuristics, the same thresholds as Trends: at least 30 operations that succeeded on both sides, and the average must move by at least 1 ms and 20% (a p95 that moves materially the other way gives insufficient evidence). They are not significance tests; cache state, concurrent load, and the app host can move latency."
	FactsNote   = "Result and error differences are facts: the same operation returned a different count or checksum, failed on one side only, or failed with another error code. Writes in the workload change the data, so a difference can also mean the two targets did not start from the same data."
	LiteralNote = "The differing operations list holds literal query values (canonical Extended JSON). Treat this report like the database contents it was taken from."
	MemoryNote  = "The first file is held in memory (about 150 bytes per operation); the second file is streamed."
)

// ErrMismatch is returned when the two files are not executions of the same
// operations and AllowMismatch is off.
var ErrMismatch = errors.New("the files are not executions of the same operations")

// PlanInfo is a shape's explain evidence from one side (from Trends).
type PlanInfo struct {
	PlanSignatures []string `json:"plan_signatures,omitempty"`
	IndexesUsed    []string `json:"indexes_used,omitempty"`
}

// Options configure Compare. PlansA and PlansB map shape ids to explain
// evidence; a plan change is an annotation only.
type Options struct {
	LabelA, LabelB string
	AllowMismatch  bool
	// TopN bounds the differing operations listed (0 = DefaultTopN).
	TopN int
	// RecordingPath supplies the commands (and shape summaries) when
	// neither file is the recording itself.
	RecordingPath  string
	PlansA, PlansB map[string]PlanInfo
	Now            func() time.Time
	// Notes are copied to the report.
	Notes []string
}

// Side describes one compared file. It never holds literal values.
type Side struct {
	Label               string `json:"label"`
	File                string `json:"file"`
	Kind                string `json:"kind"`
	Status              string `json:"status"`
	Ops                 uint64 `json:"ops"`
	ServerVersion       string `json:"server_version,omitempty"`
	Topology            string `json:"topology,omitempty"`
	PLGMVersion         string `json:"plgm_version,omitempty"`
	CreatedAt           string `json:"created_at,omitempty"`
	Workers             int    `json:"workers"`
	ResultChecksums     bool   `json:"result_checksums"`
	RecordingHeaderHash string `json:"recording_header_hash,omitempty"`
	TrafficFingerprint  string `json:"traffic_fingerprint,omitempty"`
	// PlanRunID is the trend run the plan annotations came from.
	PlanRunID string `json:"plan_run_id,omitempty"`
	// OpsFilter is "reads_only" when the replay skipped the writes; Target
	// is the multi-target label the outcome file was written for.
	OpsFilter string `json:"ops_filter,omitempty"`
	Target    string `json:"target,omitempty"`
}

// Latency summarizes the latencies of operations that succeeded on both sides.
type Latency struct {
	Ops   uint64  `json:"ops"`
	AvgMs float64 `json:"avg_ms"`
	P50Ms float64 `json:"p50_ms"`
	P95Ms float64 `json:"p95_ms"`
	P99Ms float64 `json:"p99_ms"`
	MaxMs float64 `json:"max_ms"`
}

// LatencyComparison is B against A. Delta percentages are nil when A is zero.
type LatencyComparison struct {
	A           Latency  `json:"a"`
	B           Latency  `json:"b"`
	AvgDeltaPct *float64 `json:"avg_delta_pct,omitempty"`
	P50DeltaPct *float64 `json:"p50_delta_pct,omitempty"`
	P95DeltaPct *float64 `json:"p95_delta_pct,omitempty"`
	P99DeltaPct *float64 `json:"p99_delta_pct,omitempty"`
	Verdict     string   `json:"verdict"`
	Label       string   `json:"label"`
	Reasons     []string `json:"reasons,omitempty"`
}

// Totals are the op counts and differences of a shape or of the whole file.
type Totals struct {
	OpsA                 uint64            `json:"ops_a"`
	OpsB                 uint64            `json:"ops_b"`
	Matched              uint64            `json:"matched"`
	DifferingOps         uint64            `json:"differing_ops"`
	CountMismatches      uint64            `json:"count_mismatches"`
	ChecksumMismatches   uint64            `json:"checksum_mismatches"`
	ChecksumsCompared    uint64            `json:"checksums_compared"`
	ErrorsOnlyA          map[string]uint64 `json:"errors_only_a,omitempty"`
	ErrorsOnlyB          map[string]uint64 `json:"errors_only_b,omitempty"`
	ErrorCodeDifferences uint64            `json:"error_code_differences"`
	MissingInA           uint64            `json:"missing_in_a"`
	MissingInB           uint64            `json:"missing_in_b"`
	Latency              LatencyComparison `json:"latency"`
}

// PlanChange compares a shape's explain evidence; annotation only.
type PlanChange struct {
	A       PlanInfo `json:"a"`
	B       PlanInfo `json:"b"`
	Changed bool     `json:"changed"`
	Note    string   `json:"note"`
}

// ShapeComparison is one query shape (the shape identity of the runs).
type ShapeComparison struct {
	ShapeID      string `json:"shape_id"`
	Namespace    string `json:"namespace"`
	Operation    string `json:"operation"`
	ShapeKey     string `json:"shape_key"`
	ShapeSummary string `json:"shape_summary,omitempty"`
	Totals
	Plan *PlanChange `json:"plan,omitempty"`
}

// OpDiff is one operation that differs. Command holds literal values.
type OpDiff struct {
	Seq          uint64               `json:"seq,omitempty"`
	Iteration    int                  `json:"iteration,omitempty"`
	Worker       int                  `json:"worker"`
	WorkerSeq    uint64               `json:"worker_seq"`
	Namespace    string               `json:"namespace"`
	Operation    string               `json:"operation"`
	ShapeID      string               `json:"shape_id"`
	ShapeKey     string               `json:"shape_key"`
	ShapeSummary string               `json:"shape_summary,omitempty"`
	Kinds        []string             `json:"kinds"`
	A            *workloadrec.Outcome `json:"a,omitempty"`
	B            *workloadrec.Outcome `json:"b,omitempty"`
	Command      string               `json:"command,omitempty"`
	CommandNote  string               `json:"command_note,omitempty"`
}

// Criteria are the latency thresholds (shared with Trends).
type Criteria struct {
	MinOperations           int     `json:"min_operations"`
	LatencyMaterialAbsMs    float64 `json:"latency_material_abs_ms"`
	LatencyMaterialRelative float64 `json:"latency_material_relative"`
}

// Report is the A vs B comparison.
type Report struct {
	Format        string            `json:"format"`
	ID            string            `json:"id,omitempty"`
	GeneratedAt   string            `json:"generated_at"`
	A             Side              `json:"a"`
	B             Side              `json:"b"`
	MatchedBy     string            `json:"matched_by"`
	IdentityKind  string            `json:"identity_kind,omitempty"`
	Identity      string            `json:"identity,omitempty"`
	IdentityMatch bool              `json:"identity_match"`
	Warnings      []string          `json:"warnings,omitempty"`
	Overall       Totals            `json:"overall"`
	ShapeVerdicts map[string]int    `json:"shape_verdicts"`
	Shapes        []ShapeComparison `json:"shapes"`
	DifferingOps  []OpDiff          `json:"differing_ops"`
	TopN          int               `json:"top_n"`
	// ContainsLiteralValues is set when DifferingOps hold commands.
	ContainsLiteralValues bool     `json:"contains_literal_values,omitempty"`
	Criteria              Criteria `json:"criteria"`
	LatencyNote           string   `json:"latency_note"`
	FactsNote             string   `json:"facts_note"`
	LiteralNote           string   `json:"literal_note,omitempty"`
	MemoryNote            string   `json:"memory_note"`
	// Notes describe how the executions ran (Options.Notes), for example
	// that the targets of a multi-target replay ran concurrently.
	Notes []string `json:"notes,omitempty"`
}

// opKey identifies one operation in either matching mode.
type opKey struct {
	seq       uint64
	iteration int32
	worker    int32
	workerSeq uint64
}

func (k opKey) less(o opKey) bool {
	if k.seq != o.seq {
		return k.seq < o.seq
	}
	if k.iteration != o.iteration {
		return k.iteration < o.iteration
	}
	if k.worker != o.worker {
		return k.worker < o.worker
	}
	return k.workerSeq < o.workerSeq
}

const (
	cReturned = iota
	cInserted
	cMatched
	cModified
	cUpserted
	cDeleted
	nCounts
)

const (
	sumNone = iota
	sumOK
	sumUnavailable
)

// result is an Outcome without pointers, so the first file costs little
// memory.
type result struct {
	shape     int32
	worker    int32
	iteration int32
	workerSeq uint64
	seq       uint64
	latencyUS int64
	errCode   int32
	errName   string
	ok        bool
	has       uint8
	counts    [nCounts]int64
	sumState  uint8
	sum       [32]byte
	txn       string
}

type interner map[string]string

func (in interner) get(s string) string {
	if v, ok := in[s]; ok {
		return v
	}
	in[s] = s
	return s
}

func toResult(o *workloadrec.Outcome, in interner) result {
	r := result{latencyUS: o.LatencyUS, ok: o.OK, errCode: int32(o.ErrorCode), errName: in.get(o.ErrorName), txn: in.get(o.Transaction)}
	for i, p := range []*int64{o.Returned, o.Inserted, o.Matched, o.Modified, o.Upserted, o.Deleted} {
		if p != nil {
			r.has |= 1 << i
			r.counts[i] = *p
		}
	}
	switch {
	case o.Checksum == "":
	case len(o.Checksum) == 64:
		if _, err := hex.Decode(r.sum[:], []byte(o.Checksum)); err == nil {
			r.sumState = sumOK
			break
		}
		r.sumState = sumUnavailable
	default:
		r.sumState = sumUnavailable
	}
	return r
}

func (r *result) outcome() *workloadrec.Outcome {
	o := &workloadrec.Outcome{LatencyUS: r.latencyUS, OK: r.ok, ErrorCode: int(r.errCode), ErrorName: r.errName, Transaction: r.txn}
	ptrs := []**int64{&o.Returned, &o.Inserted, &o.Matched, &o.Modified, &o.Upserted, &o.Deleted}
	for i, p := range ptrs {
		if r.has&(1<<i) != 0 {
			*p = workloadrec.Count(r.counts[i])
		}
	}
	switch r.sumState {
	case sumOK:
		o.Checksum = hex.EncodeToString(r.sum[:])
	case sumUnavailable:
		o.Checksum = "unavailable"
	}
	return o
}

// errorLabel is "<code> <name>" or the client-side category.
func (r *result) errorLabel() string {
	switch {
	case r.errCode != 0 && r.errName != "":
		return fmt.Sprintf("%d %s", r.errCode, r.errName)
	case r.errCode != 0:
		return fmt.Sprint(r.errCode)
	case r.errName != "":
		return r.errName
	}
	return "unknown_error"
}

type shapeRef struct {
	ns, op, key string
}

type shapeAcc struct {
	ref        shapeRef
	t          Totals
	latA, latB []int64
}

func newShapeAcc(ref shapeRef) *shapeAcc {
	return &shapeAcc{ref: ref, t: Totals{ErrorsOnlyA: map[string]uint64{}, ErrorsOnlyB: map[string]uint64{}}}
}

// sideFile is an opened input file before its operations are read.
type sideFile struct {
	path      string
	side      Side
	recording *workloadrec.FileInfo
	seqKeyed  bool
}

func openSide(path, label string) (*sideFile, error) {
	kind, err := workloadrec.DetectKind(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	sf := &sideFile{path: path, side: Side{Label: label, File: filepath.Base(path)}}
	if kind == workloadrec.KindRecording {
		info, err := workloadrec.Scan(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", sf.side.File, err)
		}
		if !info.Header.Outcomes {
			return nil, fmt.Errorf("%s is a recording without outcomes: record with workload_record_outcomes on, or compare the outcome files of two replays", sf.side.File)
		}
		h := info.Header
		sf.recording, sf.seqKeyed = info, true
		sf.side.Kind, sf.side.Status, sf.side.Ops = SideRecording, info.Status, info.Ops
		sf.side.ServerVersion, sf.side.Topology, sf.side.PLGMVersion, sf.side.CreatedAt = h.ServerVersion, h.Topology, h.PLGMVersion, h.CreatedAt
		sf.side.Workers, sf.side.ResultChecksums, sf.side.RecordingHeaderHash = info.Workers, h.ResultChecksums, info.HeaderHash
		return sf, nil
	}
	h, err := workloadrec.ReadOutcomeHeader(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", sf.side.File, err)
	}
	switch h.Source {
	case workloadrec.OutcomeSourceReplay:
		sf.seqKeyed = true
	case workloadrec.OutcomeSourceDeterministic:
	default:
		return nil, fmt.Errorf("%s: unknown outcome source %q", sf.side.File, h.Source)
	}
	sf.side.Kind, sf.side.Status = h.Source, workloadrec.StatusIncomplete
	sf.side.ServerVersion, sf.side.Topology, sf.side.PLGMVersion, sf.side.CreatedAt = h.ServerVersion, h.Topology, h.PLGMVersion, h.CreatedAt
	sf.side.Workers, sf.side.ResultChecksums, sf.side.RecordingHeaderHash = h.Workers, h.ResultChecksums, h.RecordingHeaderHash
	sf.side.OpsFilter, sf.side.Target = h.OpsFilter, h.Target
	return sf, nil
}

func (sf *sideFile) identity() (string, string) {
	if sf.seqKeyed {
		return IdentityRecording, sf.side.RecordingHeaderHash
	}
	return IdentityTraffic, sf.side.TrafficFingerprint
}

// opLine is one operation of either file kind.
type opLine struct {
	seq       uint64
	iteration int
	worker    int
	workerSeq uint64
	ns, op    string
	shape     string
	summary   string
	outcome   *workloadrec.Outcome
}

// recordingShapeKey is the shape key a replay reports for a recorded
// operation; it must match replayShapeKey in internal/mongo.
func recordingShapeKey(op *workloadrec.OpLine) string {
	switch {
	case op.Shape != nil:
		return op.Shape.Key
	case op.Path == workloadrec.PathFastInsertBatch:
		return "insert|fast_batch"
	case op.Op == "transaction":
		return "transaction|batch"
	case op.Path != "":
		return op.Op + "|" + op.Path
	}
	return op.Op
}

// each calls fn for every operation of the file and fills the side status.
func (sf *sideFile) each(ctx context.Context, fn func(*opLine) error) error {
	if sf.recording != nil {
		return workloadrec.Stream(ctx, sf.path, func(op *workloadrec.OpLine) error {
			l := &opLine{seq: op.Seq, iteration: op.Iteration, worker: op.Worker, workerSeq: op.WorkerSeq, ns: op.NS, op: op.Op, shape: recordingShapeKey(op), outcome: op.Outcome}
			if op.Shape != nil {
				l.summary = op.Shape.Summary
			}
			return fn(l)
		})
	}
	n := uint64(0)
	_, tr, err := workloadrec.StreamOutcomes(ctx, sf.path, func(o *workloadrec.OutcomeLine) error {
		n++
		out := o.Outcome
		return fn(&opLine{seq: o.Seq, iteration: o.Iteration, worker: o.Worker, workerSeq: o.WorkerSeq, ns: o.NS, op: o.Op, shape: o.Shape, outcome: &out})
	})
	if err != nil {
		return fmt.Errorf("%s: %w", sf.side.File, err)
	}
	sf.side.Ops = n
	if tr != nil {
		sf.side.Status, sf.side.TrafficFingerprint = tr.Status, tr.TrafficFingerprint
	}
	return nil
}

func (l *opLine) result(c *comparer) result {
	r := toResult(l.outcome, c.in)
	r.worker, r.iteration, r.workerSeq, r.seq = int32(l.worker), int32(l.iteration), l.workerSeq, l.seq
	return r
}

func (l *opLine) key(bySeq bool) opKey {
	if bySeq {
		return opKey{seq: l.seq}
	}
	it := l.iteration
	if it < 1 {
		it = 1
	}
	return opKey{iteration: int32(it), worker: int32(l.worker), workerSeq: l.workerSeq}
}

type comparer struct {
	topN      int
	in        interner
	shapes    []*shapeAcc
	shapeIdx  map[shapeRef]int32
	summaries map[string]string
	diffs     []OpDiff
	diffKeys  []opKey
	overall   *shapeAcc
}

func (c *comparer) shape(ns, op, key string) int32 {
	ref := shapeRef{ns: ns, op: op, key: key}
	if i, ok := c.shapeIdx[ref]; ok {
		return i
	}
	i := int32(len(c.shapes))
	c.shapes = append(c.shapes, newShapeAcc(ref))
	c.shapeIdx[ref] = i
	return i
}

func (c *comparer) addDiff(k opKey, a, b *result, kinds []string) {
	id := a
	if id == nil {
		id = b
	}
	d := OpDiff{Seq: id.seq, Iteration: int(id.iteration), Worker: int(id.worker), WorkerSeq: id.workerSeq, Kinds: kinds}
	if a != nil {
		d.A = a.outcome()
	}
	if b != nil {
		d.B = b.outcome()
	}
	ref := c.shapes[id.shape].ref
	d.Namespace, d.Operation, d.ShapeKey, d.ShapeID = ref.ns, ref.op, ref.key, shapeID(ref)
	c.diffs = append(c.diffs, d)
	c.diffKeys = append(c.diffKeys, k)
	if len(c.diffs) >= 4*c.topN+64 {
		c.trimDiffs()
	}
}

// trimDiffs keeps the topN differing operations with the lowest keys, in
// key order.
func (c *comparer) trimDiffs() {
	idx := make([]int, len(c.diffs))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(i, j int) bool { return c.diffKeys[idx[i]].less(c.diffKeys[idx[j]]) })
	if len(idx) > c.topN {
		idx = idx[:c.topN]
	}
	diffs, keys := make([]OpDiff, len(idx)), make([]opKey, len(idx))
	for i, j := range idx {
		diffs[i], keys[i] = c.diffs[j], c.diffKeys[j]
	}
	c.diffs, c.diffKeys = diffs, keys
}

func addCount(m map[string]uint64, k string) { m[k]++ }

// pair compares one operation that both files ran.
func (c *comparer) pair(k opKey, a, b *result) {
	acc := c.shapes[a.shape]
	var kinds []string
	acc.t.Matched++
	switch {
	case a.ok && !b.ok:
		kinds = append(kinds, DiffErrorOnlyB)
		addCount(acc.t.ErrorsOnlyB, b.errorLabel())
	case !a.ok && b.ok:
		kinds = append(kinds, DiffErrorOnlyA)
		addCount(acc.t.ErrorsOnlyA, a.errorLabel())
	case !a.ok && !b.ok:
		if a.errCode != b.errCode || a.errName != b.errName {
			kinds = append(kinds, DiffErrorCode)
			acc.t.ErrorCodeDifferences++
		}
	default:
		acc.latA = append(acc.latA, a.latencyUS)
		acc.latB = append(acc.latB, b.latencyUS)
		if a.has != b.has || a.counts != b.counts || a.txn != b.txn {
			kinds = append(kinds, DiffResultCount)
			acc.t.CountMismatches++
		}
		if a.sumState == sumOK && b.sumState == sumOK {
			acc.t.ChecksumsCompared++
			if a.sum != b.sum {
				kinds = append(kinds, DiffChecksum)
				acc.t.ChecksumMismatches++
			}
		}
	}
	if len(kinds) > 0 {
		acc.t.DifferingOps++
		c.addDiff(k, a, b, kinds)
	}
}

func shapeID(ref shapeRef) string {
	coll := ref.ns
	if i := strings.IndexByte(ref.ns, '.'); i >= 0 {
		coll = ref.ns[i+1:]
	}
	return stats.StableShapeID(ref.op, coll, ref.key)
}

// Compare compares the outcomes of pathA (A, the reference) and pathB (B).
func Compare(ctx context.Context, pathA, pathB string, opts Options) (*Report, error) {
	if opts.TopN <= 0 {
		opts.TopN = DefaultTopN
	}
	if opts.TopN > MaxTopN {
		opts.TopN = MaxTopN
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	sa, err := openSide(pathA, emptyFallback(opts.LabelA, "A"))
	if err != nil {
		return nil, err
	}
	sb, err := openSide(pathB, emptyFallback(opts.LabelB, "B"))
	if err != nil {
		return nil, err
	}
	rep := &Report{
		Format: FormatVersion, GeneratedAt: now().UTC().Format(time.RFC3339), TopN: opts.TopN,
		Criteria:    Criteria{MinOperations: stats.TrendMinOperations, LatencyMaterialAbsMs: stats.TrendLatencyMaterialAbsMs, LatencyMaterialRelative: stats.TrendLatencyMaterialRelative},
		LatencyNote: LatencyNote, FactsNote: FactsNote, MemoryNote: MemoryNote, Notes: append([]string(nil), opts.Notes...),
	}
	bySeq := sa.seqKeyed && sb.seqKeyed
	rep.MatchedBy = MatchByWorkerSeq
	if bySeq {
		rep.MatchedBy = MatchBySeq
	}
	var mismatches []string
	checkIdentity := func() {
		mismatches = mismatches[:0]
		ka, ia := sa.identity()
		kb, ib := sb.identity()
		switch {
		case ka != kb:
			mismatches = append(mismatches, fmt.Sprintf("A is a %s file and B a %s file: a replay or recording is not the same operations as a deterministic run", sa.side.Kind, sb.side.Kind))
		case ia == "" || ib == "":
			mismatches = append(mismatches, fmt.Sprintf("the %s is missing on %s (the file did not finish)", strings.ReplaceAll(ka, "_", " "), missingSide(ia, ib)))
		case ia != ib:
			mismatches = append(mismatches, fmt.Sprintf("the %s differs: A %s, B %s", strings.ReplaceAll(ka, "_", " "), short(ia), short(ib)))
		default:
			rep.IdentityKind, rep.Identity, rep.IdentityMatch = ka, ia, true
		}
	}
	refuse := func() error {
		return fmt.Errorf("%w: %s (allow a mismatch to compare anyway)", ErrMismatch, strings.Join(mismatches, "; "))
	}
	if bySeq {
		if checkIdentity(); len(mismatches) > 0 && !opts.AllowMismatch {
			return nil, refuse()
		}
	} else if sa.seqKeyed != sb.seqKeyed && !opts.AllowMismatch {
		checkIdentity()
		return nil, refuse()
	}

	c := &comparer{topN: opts.TopN, in: interner{}, shapeIdx: map[shapeRef]int32{}, summaries: map[string]string{}}
	aOps := map[opKey]result{}
	dupA, dupB, noOutcome := 0, 0, 0
	err = sa.each(ctx, func(l *opLine) error {
		if l.summary != "" {
			c.summaries[l.shape] = l.summary
		}
		if l.outcome == nil {
			noOutcome++
			return nil
		}
		k := l.key(bySeq)
		if _, dup := aOps[k]; dup {
			dupA++
			return nil
		}
		r := l.result(c)
		r.shape = c.shape(l.ns, l.op, l.shape)
		aOps[k] = r
		c.shapes[r.shape].t.OpsA++
		return nil
	})
	if err != nil {
		return nil, err
	}
	seenB := map[opKey]struct{}{}
	preferB := sa.recording != nil && sb.recording == nil
	err = sb.each(ctx, func(l *opLine) error {
		if l.summary != "" {
			c.summaries[l.shape] = l.summary
		}
		if l.outcome == nil {
			noOutcome++
			return nil
		}
		k := l.key(bySeq)
		if _, dup := seenB[k]; dup {
			dupB++
			return nil
		}
		seenB[k] = struct{}{}
		b := l.result(c)
		a, ok := aOps[k]
		shape := c.shape(l.ns, l.op, l.shape)
		if ok && !preferB {
			shape = a.shape
		}
		b.shape = shape
		c.shapes[shape].t.OpsB++
		if !ok {
			acc := c.shapes[shape]
			acc.t.MissingInA++
			acc.t.DifferingOps++
			c.addDiff(k, nil, &b, []string{DiffMissingInA})
			return nil
		}
		if shape != a.shape {
			c.shapes[a.shape].t.OpsA--
			c.shapes[shape].t.OpsA++
			a.shape = shape
		}
		delete(aOps, k)
		c.pair(k, &a, &b)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !bySeq {
		if checkIdentity(); len(mismatches) > 0 && !opts.AllowMismatch {
			return nil, refuse()
		}
	}
	for _, m := range mismatches {
		rep.Warnings = append(rep.Warnings, "Mismatch allowed: "+m+"; operations are matched by "+strings.ReplaceAll(rep.MatchedBy, "_", ", ")+" and the comparison may be meaningless")
	}
	for k, a := range aOps {
		acc := c.shapes[a.shape]
		acc.t.MissingInB++
		acc.t.DifferingOps++
		a := a
		c.addDiff(k, &a, nil, []string{DiffMissingInB})
	}
	c.trimDiffs()

	for _, s := range []*sideFile{sa, sb} {
		if s.side.Status != workloadrec.StatusComplete {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s (%s) status is %s, not complete: operations it did not run are reported as missing", s.side.Label, s.side.File, s.side.Status))
		}
	}
	if sa.side.OpsFilter != sb.side.OpsFilter {
		for _, s := range []*sideFile{sa, sb} {
			if s.side.OpsFilter != "" {
				rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s (%s) replayed %s (workload_replay_ops): the writes it skipped are reported as missing on that side", s.side.Label, s.side.File, strings.ReplaceAll(s.side.OpsFilter, "_", " ")))
			}
		}
	}
	if sa.side.ResultChecksums != sb.side.ResultChecksums {
		rep.Warnings = append(rep.Warnings, "only one side has result checksums (workload_record_result_checksums); results are compared by counts only")
	}
	if dupA+dupB > 0 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("duplicate operation keys ignored: %d in A, %d in B", dupA, dupB))
	}
	if noOutcome > 0 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("%d recorded operations have no outcome and were skipped", noOutcome))
	}
	rep.A, rep.B = sa.side, sb.side
	rep.DifferingOps = c.diffs
	if rep.DifferingOps == nil {
		rep.DifferingOps = []OpDiff{}
	}
	if err := attachCommands(ctx, rep, c.diffKeys, sa, sb, opts.RecordingPath, bySeq, c.summaries); err != nil {
		return nil, err
	}
	for i := range rep.DifferingOps {
		d := &rep.DifferingOps[i]
		d.ShapeSummary = c.summaries[d.ShapeKey]
		if d.Command != "" {
			rep.ContainsLiteralValues, rep.LiteralNote = true, LiteralNote
		}
	}

	c.overall = newShapeAcc(shapeRef{})
	rep.ShapeVerdicts = map[string]int{}
	for _, acc := range c.shapes {
		acc.t.Latency = compareLatency(acc.latA, acc.latB)
		mergeTotals(&c.overall.t, &acc.t)
		c.overall.latA = append(c.overall.latA, acc.latA...)
		c.overall.latB = append(c.overall.latB, acc.latB...)
		sc := ShapeComparison{ShapeID: shapeID(acc.ref), Namespace: acc.ref.ns, Operation: acc.ref.op, ShapeKey: acc.ref.key, ShapeSummary: c.summaries[acc.ref.key], Totals: acc.t}
		sc.Plan = planChange(opts.PlansA[sc.ShapeID], opts.PlansB[sc.ShapeID], opts.PlansA != nil && opts.PlansB != nil)
		cleanTotals(&sc.Totals)
		rep.Shapes = append(rep.Shapes, sc)
		rep.ShapeVerdicts[sc.Latency.Verdict]++
	}
	c.overall.t.Latency = compareLatency(c.overall.latA, c.overall.latB)
	cleanTotals(&c.overall.t)
	rep.Overall = c.overall.t
	sort.SliceStable(rep.Shapes, func(i, j int) bool {
		a, b := rep.Shapes[i], rep.Shapes[j]
		if a.DifferingOps != b.DifferingOps {
			return a.DifferingOps > b.DifferingOps
		}
		if ra, rb := verdictRank(a.Latency.Verdict), verdictRank(b.Latency.Verdict); ra != rb {
			return ra < rb
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Operation != b.Operation {
			return a.Operation < b.Operation
		}
		return a.ShapeKey < b.ShapeKey
	})
	return rep, nil
}

func missingSide(a, b string) string {
	switch {
	case a == "" && b == "":
		return "both sides"
	case a == "":
		return "A"
	}
	return "B"
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func emptyFallback(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}

func verdictRank(v string) int {
	switch v {
	case VerdictSlower:
		return 0
	case VerdictFaster:
		return 1
	case VerdictNoChange:
		return 2
	}
	return 3
}

func mergeTotals(dst, src *Totals) {
	dst.OpsA += src.OpsA
	dst.OpsB += src.OpsB
	dst.Matched += src.Matched
	dst.DifferingOps += src.DifferingOps
	dst.CountMismatches += src.CountMismatches
	dst.ChecksumMismatches += src.ChecksumMismatches
	dst.ChecksumsCompared += src.ChecksumsCompared
	dst.ErrorCodeDifferences += src.ErrorCodeDifferences
	dst.MissingInA += src.MissingInA
	dst.MissingInB += src.MissingInB
	for k, v := range src.ErrorsOnlyA {
		dst.ErrorsOnlyA[k] += v
	}
	for k, v := range src.ErrorsOnlyB {
		dst.ErrorsOnlyB[k] += v
	}
}

func cleanTotals(t *Totals) {
	if len(t.ErrorsOnlyA) == 0 {
		t.ErrorsOnlyA = nil
	}
	if len(t.ErrorsOnlyB) == 0 {
		t.ErrorsOnlyB = nil
	}
}

func planChange(a, b PlanInfo, have bool) *PlanChange {
	if !have || (len(a.PlanSignatures) == 0 && len(b.PlanSignatures) == 0) {
		return nil
	}
	pc := &PlanChange{A: a, B: b}
	switch {
	case len(a.PlanSignatures) == 0 || len(b.PlanSignatures) == 0:
		pc.Note = "explain evidence on one side only; plans not compared"
	case !sameSet(a.PlanSignatures, b.PlanSignatures):
		pc.Changed = true
		pc.Note = "the winning plan differs between the two runs (annotation only; not evidence of cause)"
	default:
		pc.Note = "same winning plan on both runs"
	}
	return pc
}

func sameSet(a, b []string) bool {
	x, y := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	return strings.Join(x, "\x00") == strings.Join(y, "\x00")
}

func latencyOf(us []int64) Latency {
	n := len(us)
	if n == 0 {
		return Latency{}
	}
	s := append([]int64(nil), us...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	sum := 0.0
	for _, v := range s {
		sum += float64(v)
	}
	pct := func(p float64) float64 {
		return ms(s[int(math.Round(p/100*float64(n-1)))])
	}
	return Latency{Ops: uint64(n), AvgMs: round3(sum / float64(n) / 1000), P50Ms: pct(50), P95Ms: pct(95), P99Ms: pct(99), MaxMs: ms(s[n-1])}
}

func ms(us int64) float64 { return round3(float64(us) / 1000) }

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

func deltaPct(a, b float64) *float64 {
	if a <= 0 {
		return nil
	}
	v := math.Round((b-a)/a*1000) / 10
	return &v
}

func material(a, b float64) (bool, float64, float64) {
	d := b - a
	rel := 0.0
	if a > 0 {
		rel = d / a
	}
	return math.Abs(d) >= stats.TrendLatencyMaterialAbsMs && math.Abs(rel) >= stats.TrendLatencyMaterialRelative, d, rel
}

// compareLatency gives the latency verdict of B against A over operations
// that succeeded on both sides.
func compareLatency(a, b []int64) LatencyComparison {
	c := LatencyComparison{A: latencyOf(a), B: latencyOf(b)}
	c.AvgDeltaPct, c.P50DeltaPct = deltaPct(c.A.AvgMs, c.B.AvgMs), deltaPct(c.A.P50Ms, c.B.P50Ms)
	c.P95DeltaPct, c.P99DeltaPct = deltaPct(c.A.P95Ms, c.B.P95Ms), deltaPct(c.A.P99Ms, c.B.P99Ms)
	n := len(a)
	switch {
	case n < stats.TrendMinOperations:
		c.Verdict, c.Label = VerdictInsufficient, "Insufficient evidence"
		c.Reasons = []string{fmt.Sprintf("%d operations succeeded on both sides (minimum %d)", n, stats.TrendMinOperations)}
		return c
	case c.A.AvgMs <= 0:
		c.Verdict, c.Label = VerdictInsufficient, "Insufficient evidence"
		c.Reasons = []string{"the average latency on A is zero"}
		return c
	}
	avgMat, d, rel := material(c.A.AvgMs, c.B.AvgMs)
	p95Mat, pd, _ := material(c.A.P95Ms, c.B.P95Ms)
	text := fmt.Sprintf("avg %.2f → %.2f ms (%+.2f ms, %+.0f%%), p95 %.2f → %.2f ms", c.A.AvgMs, c.B.AvgMs, d, rel*100, c.A.P95Ms, c.B.P95Ms)
	switch {
	case avgMat && p95Mat && (d > 0) != (pd > 0):
		c.Verdict, c.Label = VerdictInsufficient, "Insufficient evidence"
		c.Reasons = []string{"the average and the p95 moved materially in opposite directions"}
	case avgMat && d > 0:
		c.Verdict, c.Label = VerdictSlower, "Slower on B: "+text
	case avgMat:
		c.Verdict, c.Label = VerdictFaster, "Faster on B: "+text
	default:
		c.Verdict = VerdictNoChange
		c.Label = fmt.Sprintf("No material change: %s; a verdict needs at least %.0f ms and %.0f%%", text, stats.TrendLatencyMaterialAbsMs, stats.TrendLatencyMaterialRelative*100)
	}
	return c
}

// attachCommands adds the recorded command of each listed differing
// operation, from whichever file holds the commands, and the recorded
// shape summaries.
func attachCommands(ctx context.Context, rep *Report, keys []opKey, sa, sb *sideFile, recordingPath string, bySeq bool, summaries map[string]string) error {
	if len(rep.DifferingOps) == 0 && (sa.recording != nil || sb.recording != nil || recordingPath == "") {
		return nil
	}
	var path, note string
	switch {
	case sa.recording != nil:
		path = sa.path
	case sb.recording != nil:
		path = sb.path
	case !bySeq:
		note = "not stored: deterministic runs regenerate their commands from the seed; record the run (workload_record_enabled) to keep them"
	case recordingPath == "":
		note = "not shown: give the recording file to show the commands"
	default:
		info, err := workloadrec.Scan(recordingPath)
		if err != nil {
			return fmt.Errorf("recording %s: %w", filepath.Base(recordingPath), err)
		}
		if info.HeaderHash != rep.A.RecordingHeaderHash && info.HeaderHash != rep.B.RecordingHeaderHash {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("the recording %s (header hash %s) is not the one these files replayed; commands are not shown", filepath.Base(recordingPath), short(info.HeaderHash)))
			note = "not shown: the given recording does not match"
			break
		}
		path = recordingPath
	}
	if path == "" {
		for i := range rep.DifferingOps {
			rep.DifferingOps[i].CommandNote = note
		}
		return nil
	}
	want := make(map[opKey]int, len(keys))
	for i, k := range keys {
		want[k] = i
	}
	err := workloadrec.Stream(ctx, path, func(op *workloadrec.OpLine) error {
		if op.Shape != nil && op.Shape.Summary != "" {
			summaries[op.Shape.Key] = op.Shape.Summary
		}
		l := opLine{seq: op.Seq, iteration: op.Iteration, worker: op.Worker, workerSeq: op.WorkerSeq}
		if i, ok := want[l.key(bySeq)]; ok {
			rep.DifferingOps[i].Command = string(op.Cmd)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("read commands from %s: %w", filepath.Base(path), err)
	}
	for i := range rep.DifferingOps {
		if rep.DifferingOps[i].Command == "" {
			rep.DifferingOps[i].CommandNote = "not in the recording"
		} else {
			rep.DifferingOps[i].CommandNote = "recorded command, sent unchanged on both sides"
		}
	}
	return nil
}
