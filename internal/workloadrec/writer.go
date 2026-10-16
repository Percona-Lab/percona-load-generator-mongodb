package workloadrec

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultQueueSize bounds the entries waiting for the writer goroutine.
const DefaultQueueSize = 1 << 16

const (
	stateRecording int32 = iota
	stateTruncated
	stateFailed
)

const (
	reasonQueueFull = "writer_queue_full: the writer could not keep up with the workload"
	reasonMaxBytes  = "max_bytes_reached: the file reached workload_record_max_bytes"
)

// Entry is one sent operation. Record encodes Command on the calling
// goroutine, so Command may change once Record returns.
type Entry struct {
	Worker   int
	Offset   time.Duration
	NS       string
	Op       string
	Path     string
	OK       bool
	Existing bool
	Query    *QueryRef
	Shape    *ShapeRef
	Command  interface{}
	// Outcome is what the operation returned; nil unless outcome capture
	// is on.
	Outcome *Outcome
	// Flags mark imported operations (Flag* constants).
	Flags []string

	cmd       []byte
	iteration int
}

// Options configure a Recorder. The zero value is an unlimited,
// uncompressed recording that fails on backpressure.
type Options struct {
	// MaxBytes caps the uncompressed bytes written; <= 0 is unlimited.
	MaxBytes int64
	// QueueSize bounds the entries waiting for the writer; <= 0 uses
	// DefaultQueueSize.
	QueueSize int
	// Backpressure is BackpressureFail (default) or BackpressureBlock.
	Backpressure string
	// Compression is CompressionNone (default), CompressionZstd, or
	// CompressionGzip.
	Compression string
}

// Policy is the backpressure policy text stated in headers and logs.
func (o Options) Policy() string {
	if o.Backpressure == BackpressureBlock {
		return BlockPolicy
	}
	return FailurePolicy
}

// Result is the state of a recording. It never holds literal values.
type Result struct {
	Path         string
	Status       string
	Reason       string
	Ops          uint64
	Bytes        int64
	CommandsHash string
	// FileBytes is the size on disk once closed; it differs from Bytes
	// when the file is compressed.
	FileBytes    int64
	Compression  string
	Backpressure string
	Iterations   int
	Waits        uint64
	WaitTotal    time.Duration
	WaitMax      time.Duration
	// Outcomes and ResultChecksums repeat the header.
	Outcomes        bool
	ResultChecksums bool
}

// Recorder appends operations to a recording file from one writer goroutine.
// When the bounded queue is full, Record either marks the recording failed
// and stops (BackpressureFail) or waits for the writer (BackpressureBlock).
type Recorder struct {
	path         string
	maxBytes     int64
	reserve      int64
	block        bool
	multi        bool
	compression  string
	outcomes     bool
	checksums    bool
	ch           chan Entry
	done         chan struct{}
	state        atomic.Int32
	ops          atomic.Uint64
	bytes        atomic.Int64
	iteration    atomic.Int32
	waits        atomic.Uint64
	waitTotal    atomic.Int64
	waitMax      atomic.Int64
	start        time.Time
	startSet     atomic.Bool
	lastIterSeen int

	mu     sync.Mutex
	reason string

	f         *os.File
	file      *countingWriter
	comp      io.WriteCloser
	w         *bufio.Writer
	buf       bytes.Buffer
	enc       *json.Encoder
	seq       uint64
	workerSeq map[int]uint64
	hashes    map[int]*StreamHash
	iterOps   map[int]uint64

	closeOnce sync.Once
	result    Result

	// beforeWrite is a test hook run on the writer goroutine.
	beforeWrite func()
}

// NewRecorder creates (or truncates) path with mode 0600, writes the header,
// and starts the writer. h.Iterations > 1 makes op lines carry their
// iteration (see SetIteration).
func NewRecorder(path string, opts Options, h Header) (*Recorder, error) {
	if opts.QueueSize <= 0 {
		opts.QueueSize = DefaultQueueSize
	}
	if opts.MaxBytes < 0 {
		opts.MaxBytes = 0
	}
	if opts.Backpressure == "" {
		opts.Backpressure = BackpressureFail
	}
	if opts.Backpressure != BackpressureFail && opts.Backpressure != BackpressureBlock {
		return nil, fmt.Errorf("unknown workload recording backpressure %q", opts.Backpressure)
	}
	if opts.Compression == "" {
		opts.Compression = CompressionNone
	}
	if !ValidCompression(opts.Compression) {
		return nil, fmt.Errorf("unknown workload recording compression %q", opts.Compression)
	}
	if err := checkOverwrite(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open workload recording: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return nil, fmt.Errorf("set workload recording permissions: %w", err)
	}
	cw := &countingWriter{w: f}
	comp, err := newCompressor(cw, opts.Compression)
	if err != nil {
		f.Close()
		return nil, err
	}
	h.Type, h.Format, h.FailurePolicy, h.MaxBytes = LineHeader, FormatVersion, opts.Policy(), opts.MaxBytes
	h.Backpressure = ""
	if opts.Backpressure == BackpressureBlock {
		h.Backpressure = BackpressureBlock
	}
	if h.Iterations <= 1 {
		h.Iterations, h.IntervalDelay = 0, ""
	}
	if h.CreatedAt == "" {
		h.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	r := &Recorder{
		path: path, maxBytes: opts.MaxBytes, reserve: 1024 + 200*int64(h.Workers+1) + 64*int64(h.Iterations),
		block: opts.Backpressure == BackpressureBlock, multi: h.Iterations > 1, compression: opts.Compression, outcomes: h.Outcomes, checksums: h.ResultChecksums,
		ch: make(chan Entry, opts.QueueSize), done: make(chan struct{}),
		f: f, file: cw, comp: comp, w: bufio.NewWriterSize(comp, 1<<20),
		workerSeq: map[int]uint64{}, hashes: map[int]*StreamHash{}, iterOps: map[int]uint64{},
		start: time.Now(),
	}
	r.iteration.Store(1)
	r.enc = json.NewEncoder(&r.buf)
	r.enc.SetEscapeHTML(false)
	if err := r.writeLine(h); err != nil {
		f.Close()
		return nil, fmt.Errorf("write workload recording header: %w", err)
	}
	if r.maxBytes > 0 && r.bytes.Load() > r.maxBytes {
		f.Close()
		return nil, fmt.Errorf("workload_record_max_bytes (%d) is smaller than the header", r.maxBytes)
	}
	if err := r.w.Flush(); err != nil {
		f.Close()
		return nil, fmt.Errorf("write workload recording header: %w", err)
	}
	go r.run()
	return r, nil
}

// checkOverwrite allows replacing only an empty file or an earlier
// recording (compressed or not), so a mistyped path never truncates
// unrelated data.
func checkOverwrite(path string) error {
	return checkOverwriteOf(path, headerPrefix, "workload recording")
}

// checkOverwriteOf allows replacing only an empty file or a file whose
// first line starts with prefix.
func checkOverwriteOf(path string, prefix []byte, what string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", what, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s path %s is not a regular file", what, path)
	}
	if fi.Size() == 0 {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", what, err)
	}
	defer f.Close()
	refuse := fmt.Errorf("%s path %s exists and is not a %s; refusing to overwrite it", what, path, what)
	content, _, release, err := decompress(bufio.NewReader(f))
	if err != nil {
		return refuse
	}
	defer release()
	buf := make([]byte, len(prefix))
	if _, err := io.ReadFull(content, buf); err != nil || !bytes.Equal(buf, prefix) {
		return refuse
	}
	return nil
}

// SetStart sets the time operation offsets are measured from. Only the
// first call counts, so the offsets of a multi-iteration recording keep
// the gaps between iterations.
func (r *Recorder) SetStart(t time.Time) {
	if r != nil && r.startSet.CompareAndSwap(false, true) {
		r.start = t
	}
}

// SetIteration sets the 1-based iteration of the operations recorded next.
// Call it while no worker is recording.
func (r *Recorder) SetIteration(n int) {
	if r != nil && n >= 1 {
		r.iteration.Store(int32(n))
	}
}

// Offset is t relative to the start of the workload.
func (r *Recorder) Offset(t time.Time) time.Duration { return t.Sub(r.start) }

// Active reports whether operations are still being recorded.
func (r *Recorder) Active() bool { return r != nil && r.state.Load() == stateRecording }

// Record queues one operation. It must not be called after Close.
func (r *Recorder) Record(e Entry) {
	if r == nil || r.state.Load() != stateRecording {
		return
	}
	cmd, err := EncodeCommand(e.Command)
	if err != nil {
		r.stop(stateFailed, "encode_error: "+err.Error())
		return
	}
	e.cmd, e.Command = cmd, nil
	if r.multi {
		e.iteration = int(r.iteration.Load())
	}
	select {
	case r.ch <- e:
		return
	default:
	}
	if !r.block {
		r.stop(stateFailed, reasonQueueFull)
		return
	}
	// The writer keeps draining after a stop, so this send always completes.
	t0 := time.Now()
	r.ch <- e
	r.addWait(time.Since(t0))
}

func (r *Recorder) addWait(d time.Duration) {
	r.waits.Add(1)
	r.waitTotal.Add(int64(d))
	for {
		cur := r.waitMax.Load()
		if int64(d) <= cur || r.waitMax.CompareAndSwap(cur, int64(d)) {
			return
		}
	}
}

// Fail marks the recording failed; later operations are not recorded.
func (r *Recorder) Fail(reason string) {
	if r != nil {
		r.stop(stateFailed, reason)
	}
}

func (r *Recorder) policy() string {
	if r.block {
		return BlockPolicy
	}
	return FailurePolicy
}

func (r *Recorder) stop(state int32, reason string) {
	if !r.state.CompareAndSwap(stateRecording, state) {
		return
	}
	r.mu.Lock()
	r.reason = reason
	r.mu.Unlock()
	status := StatusFailed
	if state == stateTruncated {
		status = StatusTruncated
	}
	log.Printf("[Workload Record] recording %s (%s); later operations are not recorded. Policy: %s", status, reason, r.policy())
}

func (r *Recorder) run() {
	defer close(r.done)
	for e := range r.ch {
		if r.state.Load() != stateRecording {
			continue
		}
		r.write(e)
	}
}

func (r *Recorder) write(e Entry) {
	if r.beforeWrite != nil {
		r.beforeWrite()
	}
	cmd := e.cmd
	r.seq++
	r.workerSeq[e.Worker]++
	line := OpLine{
		Type: LineOp, Seq: r.seq, Worker: e.Worker, WorkerSeq: r.workerSeq[e.Worker], OffsetUS: e.Offset.Microseconds(), Iteration: e.iteration,
		NS: e.NS, Op: e.Op, Path: e.Path, OK: e.OK, Existing: e.Existing, Query: e.Query, Shape: e.Shape, Cmd: cmd, Outcome: e.Outcome, Flags: e.Flags,
	}
	r.buf.Reset()
	if err := r.enc.Encode(line); err != nil {
		r.seq--
		r.workerSeq[e.Worker]--
		r.stop(stateFailed, "encode_error: "+err.Error())
		return
	}
	if r.maxBytes > 0 && r.bytes.Load()+int64(r.buf.Len())+r.reserve > r.maxBytes {
		r.seq--
		r.workerSeq[e.Worker]--
		r.stop(stateTruncated, reasonMaxBytes)
		return
	}
	if _, err := r.w.Write(r.buf.Bytes()); err != nil {
		r.stop(stateFailed, "write_error: "+err.Error())
		return
	}
	r.bytes.Add(int64(r.buf.Len()))
	r.ops.Add(1)
	if r.multi {
		r.iterOps[e.iteration]++
		if e.iteration > r.lastIterSeen {
			r.lastIterSeen = e.iteration
		}
	}
	hs := r.hashes[e.Worker]
	if hs == nil {
		hs = NewStreamHash()
		r.hashes[e.Worker] = hs
	}
	hs.Add(cmd)
}

func (r *Recorder) writeLine(v interface{}) error {
	r.buf.Reset()
	if err := r.enc.Encode(v); err != nil {
		return err
	}
	if _, err := r.w.Write(r.buf.Bytes()); err != nil {
		return err
	}
	r.bytes.Add(int64(r.buf.Len()))
	return nil
}

func (r *Recorder) baseResult() Result {
	res := Result{Path: r.path, Ops: r.ops.Load(), Bytes: r.bytes.Load(), Compression: r.compression, Backpressure: BackpressureFail, Outcomes: r.outcomes, ResultChecksums: r.checksums}
	if r.block {
		res.Backpressure = BackpressureBlock
		res.Waits, res.WaitTotal, res.WaitMax = r.waits.Load(), time.Duration(r.waitTotal.Load()), time.Duration(r.waitMax.Load())
	}
	return res
}

// Snapshot is the state while the recording runs.
func (r *Recorder) Snapshot() Result {
	if r == nil {
		return Result{}
	}
	res := r.baseResult()
	res.Status, res.Reason = r.status()
	return res
}

func (r *Recorder) status() (string, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch r.state.Load() {
	case stateTruncated:
		return StatusTruncated, r.reason
	case stateFailed:
		return StatusFailed, r.reason
	}
	return StatusRecording, ""
}

// Close drains the queue, writes the trailer, and closes the file. Call it
// once every worker has stopped calling Record.
func (r *Recorder) Close() Result {
	if r == nil {
		return Result{}
	}
	r.closeOnce.Do(func() {
		close(r.ch)
		<-r.done
		status, reason := r.status()
		if status == StatusRecording {
			status = StatusComplete
		}
		t := Trailer{Type: LineTrailer, Status: status, Reason: reason, Ops: r.ops.Load(), WorkerOps: map[string]uint64{}, WorkerHashes: map[string]string{}}
		sums := make(map[int]string, len(r.hashes))
		for id, h := range r.hashes {
			sums[id] = h.Sum()
			t.WorkerHashes[strconv.Itoa(id)] = sums[id]
			t.WorkerOps[strconv.Itoa(id)] = r.workerSeq[id]
		}
		t.CommandsHash = CombineHashes(sums)
		if r.multi {
			t.IterationOps = make(map[string]uint64, len(r.iterOps))
			for it, n := range r.iterOps {
				t.IterationOps[strconv.Itoa(it)] = n
			}
		}
		res := r.baseResult()
		if r.block {
			t.Waits, t.WaitTotalUS, t.WaitMaxUS = res.Waits, res.WaitTotal.Microseconds(), res.WaitMax.Microseconds()
		}
		t.Bytes = r.bytes.Load()
		err := r.writeLine(t)
		if err == nil {
			err = r.w.Flush()
		}
		if cerr := r.comp.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = r.f.Sync()
		}
		if cerr := r.f.Close(); err == nil {
			err = cerr
		}
		if err != nil && status == StatusComplete {
			status, reason = StatusFailed, "write_error: "+err.Error()
		}
		res.Status, res.Reason, res.Ops, res.Bytes, res.CommandsHash = status, reason, t.Ops, r.bytes.Load(), t.CommandsHash
		res.FileBytes = r.file.n
		if r.multi {
			res.Iterations = r.lastIterSeen
		}
		r.result = res
	})
	return r.result
}
