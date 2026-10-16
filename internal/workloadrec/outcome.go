package workloadrec

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

const (
	// OutcomeFormatVersion identifies the outcome file layout.
	OutcomeFormatVersion = "plgm-workload-outcomes/1"

	LineOutcomeHeader  = "outcome_header"
	LineOutcome        = "outcome"
	LineOutcomeTrailer = "outcome_trailer"

	// Sources of an outcome file. Replay lines carry the recording's seq;
	// deterministic lines are keyed by (iteration, worker, worker_seq).
	OutcomeSourceReplay        = "replay"
	OutcomeSourceDeterministic = "deterministic"

	TransactionCommit = "commit"
	TransactionAbort  = "abort"

	// Error names for client-side failures, which have no server code.
	ErrorNameClientTimeout = "client_timeout"
	ErrorNameCancelled     = "cancelled"
	ErrorNameNetwork       = "network_error"
	ErrorNameClient        = "client_error"
)

// Outcome is what one execution of an operation returned: latency, error
// code and name (never the message text), and a result summary for its
// operation type. Counts are pointers so a zero count is kept.
type Outcome struct {
	LatencyUS int64  `json:"latency_us"`
	OK        bool   `json:"ok"`
	ErrorCode int    `json:"error_code,omitempty"`
	ErrorName string `json:"error_name,omitempty"`
	// Returned counts the documents a find or aggregate returned; Checksum
	// is the order-sensitive sha256 of their canonical Extended JSON.
	Returned *int64 `json:"returned,omitempty"`
	Checksum string `json:"checksum,omitempty"`
	Inserted *int64 `json:"inserted,omitempty"`
	Matched  *int64 `json:"matched,omitempty"`
	Modified *int64 `json:"modified,omitempty"`
	Upserted *int64 `json:"upserted,omitempty"`
	Deleted  *int64 `json:"deleted,omitempty"`
	// Transaction is TransactionCommit or TransactionAbort.
	Transaction string `json:"transaction,omitempty"`
}

// Count returns a pointer to n, for the Outcome counts.
func Count(n int64) *int64 { return &n }

// ResultHash is the order-sensitive checksum of a result: the sha256 of
// each document's canonical Extended JSON, one per line, in the order the
// server returned them. Keys inside documents are sorted first, because
// the same seeded data can store its fields in a different order on each
// instance; array order and BSON types count.
type ResultHash struct {
	h hash.Hash
}

func NewResultHash() *ResultHash { return &ResultHash{h: sha256.New()} }

func (r *ResultHash) Add(doc bson.Raw) error {
	d, err := sortedDocument(doc)
	if err != nil {
		return err
	}
	b, err := bson.MarshalExtJSON(d, true, false)
	if err != nil {
		return err
	}
	_, _ = r.h.Write(b)
	_, _ = r.h.Write([]byte{'\n'})
	return nil
}

func sortedDocument(raw bson.Raw) (bson.D, error) {
	elems, err := raw.Elements()
	if err != nil {
		return nil, err
	}
	out := make(bson.D, 0, len(elems))
	for _, e := range elems {
		v, err := sortedValue(e.Value())
		if err != nil {
			return nil, err
		}
		out = append(out, bson.E{Key: e.Key(), Value: v})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func sortedValue(v bson.RawValue) (interface{}, error) {
	switch v.Type {
	case bson.TypeEmbeddedDocument:
		return sortedDocument(v.Document())
	case bson.TypeArray:
		vals, err := v.Array().Values()
		if err != nil {
			return nil, err
		}
		out := make(bson.A, len(vals))
		for i, x := range vals {
			if out[i], err = sortedValue(x); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	return v, nil
}

func (r *ResultHash) Sum() string { return hex.EncodeToString(r.h.Sum(nil)) }

// OutcomeHeader is the first line of an outcome file.
type OutcomeHeader struct {
	Type            string `json:"type"`
	Format          string `json:"format"`
	Source          string `json:"source"`
	PLGMVersion     string `json:"plgm_version"`
	CreatedAt       string `json:"created_at"`
	ServerVersion   string `json:"server_version,omitempty"`
	Topology        string `json:"topology,omitempty"`
	Workers         int    `json:"workers"`
	ResultChecksums bool   `json:"result_checksums,omitempty"`
	// RecordingFile and RecordingHeaderHash name the replayed recording.
	RecordingFile       string `json:"recording_file,omitempty"`
	RecordingHeaderHash string `json:"recording_header_hash,omitempty"`
	// Deterministic runs: what the traffic fingerprint covers besides the
	// commands. The fingerprint itself is in the trailer.
	CollectionsHash     string `json:"collections_hash,omitempty"`
	QueriesHash         string `json:"queries_hash,omitempty"`
	RandomSeed          int64  `json:"random_seed,omitempty"`
	OperationsPerWorker int    `json:"operations_per_worker,omitempty"`
	// OpsFilter is "reads_only" when a replay skipped the writes.
	OpsFilter string `json:"ops_filter,omitempty"`
	// Target is the label of a multi-target replay's target.
	Target string `json:"target,omitempty"`
}

// OutcomeLine is the outcome of one operation.
type OutcomeLine struct {
	Type string `json:"type"`
	// Seq is the recording's seq (replay only).
	Seq       uint64 `json:"seq,omitempty"`
	Worker    int    `json:"worker"`
	WorkerSeq uint64 `json:"worker_seq"`
	// Iteration is the recorded iteration (replay; 0 for one iteration)
	// or the 1-based iteration (deterministic).
	Iteration int     `json:"iteration,omitempty"`
	NS        string  `json:"ns"`
	Op        string  `json:"op"`
	Shape     string  `json:"shape,omitempty"`
	Outcome   Outcome `json:"outcome"`
}

// OutcomeTrailer is the last line of a finished outcome file.
type OutcomeTrailer struct {
	Type   string `json:"type"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
	Ops    uint64 `json:"ops"`
	// CommandsHash covers the commands sent, per worker in order.
	CommandsHash string `json:"commands_hash,omitempty"`
	// TrafficFingerprint and TrafficStatus stamp a deterministic run.
	TrafficFingerprint string `json:"traffic_fingerprint,omitempty"`
	TrafficStatus      string `json:"traffic_status,omitempty"`
	Waits              uint64 `json:"waits,omitempty"`
	WaitTotalUS        int64  `json:"wait_total_us,omitempty"`
	WaitMaxUS          int64  `json:"wait_max_us,omitempty"`
}

// OutcomeResult is the state of an outcome file.
type OutcomeResult struct {
	Path      string
	Status    string
	Reason    string
	Ops       uint64
	Bytes     int64
	Waits     uint64
	WaitTotal time.Duration
	WaitMax   time.Duration
}

var (
	outcomeHeaderPrefix  = []byte(`{"type":"outcome_header",`)
	outcomePrefix        = []byte(`{"type":"outcome",`)
	outcomeTrailerPrefix = []byte(`{"type":"outcome_trailer",`)
)

// OutcomeWriter appends outcome lines from one writer goroutine. When its
// bounded queue is full, Write waits for the writer, so no outcome is
// dropped; the waits are reported in the trailer.
type OutcomeWriter struct {
	path      string
	ch        chan OutcomeLine
	done      chan struct{}
	ops       atomic.Uint64
	bytes     atomic.Int64
	failed    atomic.Bool
	waits     atomic.Uint64
	waitTotal atomic.Int64
	waitMax   atomic.Int64

	mu     sync.Mutex
	reason string

	f   *os.File
	w   *bufio.Writer
	buf bytes.Buffer
	enc *json.Encoder

	closeOnce sync.Once
	result    OutcomeResult
}

// NewOutcomeWriter creates (or truncates) path with mode 0600 and writes
// the header. An existing file that is not empty and not an outcome file
// is never overwritten.
func NewOutcomeWriter(path string, h OutcomeHeader) (*OutcomeWriter, error) {
	if err := checkOverwriteOf(path, outcomeHeaderPrefix, "workload outcome file"); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open workload outcome file: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return nil, fmt.Errorf("set workload outcome file permissions: %w", err)
	}
	h.Type, h.Format = LineOutcomeHeader, OutcomeFormatVersion
	if h.CreatedAt == "" {
		h.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	w := &OutcomeWriter{path: path, ch: make(chan OutcomeLine, DefaultQueueSize), done: make(chan struct{}), f: f, w: bufio.NewWriterSize(f, 1<<20)}
	w.enc = json.NewEncoder(&w.buf)
	w.enc.SetEscapeHTML(false)
	err = w.writeLine(h)
	if err == nil {
		err = w.w.Flush()
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("write workload outcome header: %w", err)
	}
	go w.run()
	return w, nil
}

// Write queues one outcome. It must not be called after Close.
func (w *OutcomeWriter) Write(l OutcomeLine) {
	if w == nil {
		return
	}
	l.Type = LineOutcome
	select {
	case w.ch <- l:
		return
	default:
	}
	t0 := time.Now()
	w.ch <- l
	d := time.Since(t0)
	w.waits.Add(1)
	w.waitTotal.Add(int64(d))
	for {
		cur := w.waitMax.Load()
		if int64(d) <= cur || w.waitMax.CompareAndSwap(cur, int64(d)) {
			return
		}
	}
}

func (w *OutcomeWriter) run() {
	defer close(w.done)
	for l := range w.ch {
		if w.failed.Load() {
			continue
		}
		if err := w.writeLine(l); err != nil {
			w.fail("write_error: " + err.Error())
			continue
		}
		w.ops.Add(1)
	}
}

func (w *OutcomeWriter) fail(reason string) {
	if w.failed.CompareAndSwap(false, true) {
		w.mu.Lock()
		w.reason = reason
		w.mu.Unlock()
	}
}

func (w *OutcomeWriter) writeLine(v interface{}) error {
	w.buf.Reset()
	if err := w.enc.Encode(v); err != nil {
		return err
	}
	if _, err := w.w.Write(w.buf.Bytes()); err != nil {
		return err
	}
	w.bytes.Add(int64(w.buf.Len()))
	return nil
}

func (w *OutcomeWriter) baseResult() OutcomeResult {
	return OutcomeResult{Path: w.path, Ops: w.ops.Load(), Bytes: w.bytes.Load(), Waits: w.waits.Load(), WaitTotal: time.Duration(w.waitTotal.Load()), WaitMax: time.Duration(w.waitMax.Load())}
}

// Snapshot is the state while outcomes are being written.
func (w *OutcomeWriter) Snapshot() OutcomeResult {
	if w == nil {
		return OutcomeResult{}
	}
	res := w.baseResult()
	res.Status = StatusRecording
	if w.failed.Load() {
		w.mu.Lock()
		res.Status, res.Reason = StatusFailed, w.reason
		w.mu.Unlock()
	}
	return res
}

// Close drains the queue, writes t as the trailer (Status and Reason are
// set from status and reason unless the file failed), and closes the file.
// Call it once every worker has stopped calling Write.
func (w *OutcomeWriter) Close(t OutcomeTrailer, status, reason string) OutcomeResult {
	if w == nil {
		return OutcomeResult{}
	}
	w.closeOnce.Do(func() {
		close(w.ch)
		<-w.done
		if w.failed.Load() {
			w.mu.Lock()
			status, reason = StatusFailed, w.reason
			w.mu.Unlock()
		}
		res := w.baseResult()
		t.Type, t.Status, t.Reason, t.Ops = LineOutcomeTrailer, status, reason, res.Ops
		t.Waits, t.WaitTotalUS, t.WaitMaxUS = res.Waits, res.WaitTotal.Microseconds(), res.WaitMax.Microseconds()
		err := w.writeLine(t)
		if err == nil {
			err = w.w.Flush()
		}
		if err == nil {
			err = w.f.Sync()
		}
		if cerr := w.f.Close(); err == nil {
			err = cerr
		}
		if err != nil && status != StatusFailed {
			status, reason = StatusFailed, "write_error: "+err.Error()
		}
		res.Status, res.Reason, res.Bytes = status, reason, w.bytes.Load()
		w.result = res
	})
	return w.result
}

// ReadOutcomeHeader reads only the header of an outcome file.
func ReadOutcomeHeader(path string) (*OutcomeHeader, error) {
	f, lr, err := open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readOutcomeHeader(lr)
}

func readOutcomeHeader(lr *lineReader) (*OutcomeHeader, error) {
	b, err := lr.next()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("workload outcome file is empty")
		}
		return nil, err
	}
	if !bytes.HasPrefix(b, outcomeHeaderPrefix) {
		return nil, fmt.Errorf("workload outcome file does not start with an outcome header line")
	}
	var h OutcomeHeader
	if err := json.Unmarshal(b, &h); err != nil {
		return nil, fmt.Errorf("workload outcome header: %w", err)
	}
	if h.Format != OutcomeFormatVersion {
		return nil, fmt.Errorf("unsupported workload outcome format %q (expected %q)", h.Format, OutcomeFormatVersion)
	}
	return &h, nil
}

// StreamOutcomes reads an outcome file: it returns the header and the
// trailer (nil when the writer did not finish) and calls fn for every
// outcome line in file order. Compressed files are detected.
func StreamOutcomes(ctx context.Context, path string, fn func(*OutcomeLine) error) (*OutcomeHeader, *OutcomeTrailer, error) {
	f, lr, err := open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	hp, err := readOutcomeHeader(lr)
	if err != nil {
		return nil, nil, err
	}
	h := *hp
	var tr *OutcomeTrailer
	for {
		if err := ctx.Err(); err != nil {
			return &h, tr, err
		}
		b, err := lr.next()
		if errors.Is(err, io.EOF) {
			return &h, tr, nil
		}
		if err != nil {
			return &h, tr, err
		}
		if tr != nil {
			return &h, tr, fmt.Errorf("line %d: data after the trailer", lr.line)
		}
		switch {
		case bytes.HasPrefix(b, outcomePrefix):
			var l OutcomeLine
			if err := json.Unmarshal(b, &l); err != nil {
				return &h, tr, fmt.Errorf("line %d: %w", lr.line, err)
			}
			if fn != nil {
				if err := fn(&l); err != nil {
					return &h, tr, err
				}
			}
		case bytes.HasPrefix(b, outcomeTrailerPrefix):
			tr = &OutcomeTrailer{}
			if err := json.Unmarshal(b, tr); err != nil {
				return &h, nil, fmt.Errorf("line %d: trailer: %w", lr.line, err)
			}
		default:
			return &h, tr, fmt.Errorf("line %d: unexpected line in a workload outcome file", lr.line)
		}
	}
}
