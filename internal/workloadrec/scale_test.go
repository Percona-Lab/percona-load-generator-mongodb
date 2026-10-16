package workloadrec

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func newRecorderWith(t *testing.T, opts Options, h Header) (*Recorder, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rec.ndjson")
	r, err := NewRecorder(path, opts, h)
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	return r, path
}

// everyOpTypeEntries is one entry per recorded command form.
func everyOpTypeEntries(t *testing.T) []Entry {
	t.Helper()
	oid := bson.NewObjectID()
	doc := map[string]interface{}{"_id": oid, "i32": int32(7), "i64": int64(1 << 40), "dec": mustDecimal(t, "9.99"), "at": time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)}
	sortSpec := bson.D{{Key: "z", Value: int32(-1)}, {Key: "a", Value: int32(1)}}
	raw, err := bson.Marshal(bson.D{{Key: "insert", Value: "c"}, {Key: "documents", Value: bson.A{bson.D{{Key: "_id", Value: oid}, {Key: "seq", Value: int64(1)}}}}})
	if err != nil {
		t.Fatal(err)
	}
	return []Entry{
		{Op: "find", Command: bson.D{{Key: "filter", Value: map[string]interface{}{"i32": map[string]interface{}{"$gte": int32(3)}}}, {Key: "limit", Value: int64(5)}, {Key: "batchSize", Value: int32(10)}}},
		{Op: "aggregate", Command: bson.D{{Key: "pipeline", Value: []interface{}{map[string]interface{}{"$match": map[string]interface{}{"b": true}}, map[string]interface{}{"$sort": sortSpec}}}}},
		{Op: "updateOne", Command: bson.D{{Key: "filter", Value: map[string]interface{}{"_id": oid}}, {Key: "update", Value: map[string]interface{}{"$set": doc}}, {Key: "upsert", Value: true}}},
		{Op: "updateMany", Command: bson.D{{Key: "filter", Value: map[string]interface{}{}}, {Key: "update", Value: map[string]interface{}{"$inc": map[string]interface{}{"i32": int32(1)}}}, {Key: "upsert", Value: false}}},
		{Op: "deleteOne", Command: bson.D{{Key: "filter", Value: map[string]interface{}{"i64": int64(3)}}}},
		{Op: "deleteMany", Command: bson.D{{Key: "filter", Value: map[string]interface{}{"dec": mustDecimal(t, "1.0")}}}},
		{Op: "insert", Command: bson.D{{Key: "document", Value: doc}}},
		{Op: "insertMany", Command: bson.D{{Key: "documents", Value: []interface{}{doc, doc}}}},
		{Op: "transaction", NS: "db.(multiple)", Command: bson.D{{Key: "ops", Value: bson.A{bson.D{{Key: "op", Value: "insert"}, {Key: "ns", Value: "db.c"}, {Key: "args", Value: bson.D{{Key: "document", Value: doc}}}}}}}},
		{Op: "find", Path: PathFastFind, Command: bson.D{{Key: "find", Value: "c"}, {Key: "filter", Value: bson.D{{Key: "seq", Value: bson.D{{Key: "$in", Value: bson.A{int64(1), int64(2)}}}}}}, {Key: "limit", Value: int32(2)}}},
		{Op: "insertMany", Path: PathFastInsertBatch, Command: bson.Raw(raw)},
	}
}

func recordAll(r *Recorder, entries []Entry) {
	for _, e := range entries {
		if e.NS == "" {
			e.NS = "db.c"
		}
		e.Worker, e.OK = 1, true
		r.Record(e)
	}
}

func streamCmds(t *testing.T, path string) [][]byte {
	t.Helper()
	var out [][]byte
	if err := Stream(context.Background(), path, func(op *OpLine) error {
		out = append(out, append([]byte(nil), op.Cmd...))
		return nil
	}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	return out
}

func TestCompressedRecordingsRoundTripEveryOperationType(t *testing.T) {
	entries := everyOpTypeEntries(t)
	want := make([][]byte, len(entries))
	for i, e := range entries {
		b, err := EncodeCommand(e.Command)
		if err != nil {
			t.Fatal(err)
		}
		want[i] = b
	}
	magic := map[string][]byte{CompressionZstd: zstdMagic, CompressionGzip: gzipMagic}
	var hashes []string
	for _, c := range []string{CompressionNone, CompressionZstd, CompressionGzip} {
		t.Run(c, func(t *testing.T) {
			r, path := newRecorderWith(t, Options{Compression: c}, testHeader(1))
			recordAll(r, entries)
			res := r.Close()
			if res.Status != StatusComplete || res.Ops != uint64(len(entries)) || res.Compression != c {
				t.Fatalf("result = %+v", res)
			}
			fi, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if res.FileBytes != fi.Size() || fi.Mode().Perm() != 0o600 {
				t.Fatalf("file bytes %d (stat %d) mode %v", res.FileBytes, fi.Size(), fi.Mode().Perm())
			}
			b, _ := os.ReadFile(path)
			if m := magic[c]; m != nil && !bytes.HasPrefix(b, m) {
				t.Fatalf("%s file does not start with its magic bytes: % x", c, b[:4])
			}
			if c == CompressionNone && res.FileBytes != res.Bytes {
				t.Fatalf("uncompressed file bytes %d != bytes %d", res.FileBytes, res.Bytes)
			}
			got := streamCmds(t, path)
			if len(got) != len(want) {
				t.Fatalf("streamed %d ops, want %d", len(got), len(want))
			}
			for i := range want {
				if !bytes.Equal(got[i], want[i]) {
					t.Fatalf("op %d (%s): recorded %s, want %s", i, entries[i].Op, got[i], want[i])
				}
			}
			info, err := Scan(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Compression != c || info.Status != StatusComplete || info.Ops != uint64(len(entries)) || info.Trailer.Bytes >= res.Bytes {
				t.Fatalf("scan = %+v", info)
			}
			hashes = append(hashes, info.Trailer.CommandsHash)
			r2, err := NewRecorder(path, Options{}, testHeader(1))
			if err != nil {
				t.Fatalf("overwriting a %s recording: %v", c, err)
			}
			r2.Close()
		})
	}
	if len(hashes) != 3 || hashes[0] != hashes[1] || hashes[1] != hashes[2] {
		t.Fatalf("commands hash differs by compression: %v", hashes)
	}
}

// A recording whose writer was killed has no trailer and, when compressed,
// ends mid-frame and usually mid-line. Readers must keep every complete
// operation, drop the cut-off line, and report the file as incomplete.
func TestCutOffRecordingsReadAsIncomplete(t *testing.T) {
	const ops = 3000
	for _, c := range []string{CompressionNone, CompressionZstd, CompressionGzip} {
		r, path := newRecorderWith(t, Options{Compression: c}, testHeader(1))
		for i := 0; i < ops; i++ {
			r.Record(Entry{Worker: 1, NS: "db.c", Op: "find", OK: true, Command: bson.D{{Key: "filter", Value: bson.D{{Key: "i", Value: int64(i)}, {Key: "pad", Value: strings.Repeat(fmt.Sprint(i%10), 40)}}}}})
		}
		if res := r.Close(); res.Status != StatusComplete {
			t.Fatalf("%s: result = %+v", c, res)
		}
		full, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		want := streamCmds(t, path)
		for _, frac := range []float64{0.3, 0.75, 0.97} {
			t.Run(fmt.Sprintf("%s/%.0f%%", c, frac*100), func(t *testing.T) {
				cut := filepath.Join(t.TempDir(), "cut.ndjson")
				n := int(float64(len(full)) * frac)
				if c == CompressionNone {
					n = bytes.LastIndexByte(full[:n], '\n') + 20
				}
				if err := os.WriteFile(cut, full[:n], 0o600); err != nil {
					t.Fatal(err)
				}
				info, err := Scan(cut)
				if err != nil {
					t.Fatalf("Scan: %v", err)
				}
				if info.Status != StatusIncomplete || info.Trailer != nil || info.Ops == 0 || info.Ops >= ops {
					t.Fatalf("scan status %q ops %d trailer %v, want incomplete with a prefix of %d ops", info.Status, info.Ops, info.Trailer, ops)
				}
				got := streamCmds(t, cut)
				if uint64(len(got)) != info.Ops {
					t.Fatalf("Stream returned %d ops, Scan counted %d", len(got), info.Ops)
				}
				for i := range got {
					if !bytes.Equal(got[i], want[i]) {
						t.Fatalf("op %d differs from the complete recording", i+1)
					}
				}
			})
		}
	}
}

func TestCompressedRecordingsReportTruncatedAndFailed(t *testing.T) {
	for _, c := range []string{CompressionZstd, CompressionGzip} {
		t.Run(c+"/truncated", func(t *testing.T) {
			const maxBytes = 6000
			r, path := newRecorderWith(t, Options{MaxBytes: maxBytes, Compression: c}, testHeader(1))
			for i := 0; i < 200; i++ {
				r.Record(Entry{Worker: 1, NS: "db.c", Op: "insert", Command: bson.D{{Key: "document", Value: map[string]interface{}{"i": int64(i), "pad": strings.Repeat("x", 80)}}}})
			}
			res := r.Close()
			if res.Status != StatusTruncated || res.Ops == 0 || res.Ops >= 200 || res.Bytes > maxBytes {
				t.Fatalf("result = %+v, want truncated within %d uncompressed bytes", res, maxBytes)
			}
			info, err := Scan(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Status != StatusTruncated || info.Ops != res.Ops || info.Trailer.Ops != res.Ops {
				t.Fatalf("scan = %+v", info)
			}
		})
		t.Run(c+"/failed", func(t *testing.T) {
			r, path := newRecorderWith(t, Options{QueueSize: 1, Compression: c}, testHeader(1))
			release := make(chan struct{})
			r.beforeWrite = func() { <-release }
			for i := 0; i < 4 && r.Active(); i++ {
				r.Record(Entry{Worker: 1, NS: "db.c", Op: "find", Command: bson.D{{Key: "i", Value: int32(i)}}})
			}
			close(release)
			res := r.Close()
			if res.Status != StatusFailed || !strings.Contains(res.Reason, "writer_queue_full") {
				t.Fatalf("result = %+v", res)
			}
			info, err := Scan(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Status != StatusFailed || info.Trailer.Ops != info.Ops {
				t.Fatalf("scan = %+v", info)
			}
		})
	}
}

// TestBlockBackpressureRecordsEveryOperationInOrder runs concurrent workers
// against a slow writer with a one-entry queue: nothing is dropped, every
// worker's stream is in order, and the waits are reported.
func TestBlockBackpressureRecordsEveryOperationInOrder(t *testing.T) {
	const workers, perWorker = 6, 150
	h := testHeader(workers)
	r, path := newRecorderWith(t, Options{QueueSize: 1, Backpressure: BackpressureBlock, Compression: CompressionZstd}, h)
	var n int
	r.beforeWrite = func() {
		if n++; n%10 == 0 {
			time.Sleep(200 * time.Microsecond)
		}
	}
	var wg sync.WaitGroup
	for w := 1; w <= workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				r.Record(Entry{Worker: w, NS: "db.c", Op: "find", OK: true, Command: bson.D{{Key: "i", Value: int64(i)}}})
			}
		}(w)
	}
	wg.Wait()
	res := r.Close()
	if res.Status != StatusComplete || res.Ops != workers*perWorker {
		t.Fatalf("result = %+v, want every operation recorded", res)
	}
	if res.Backpressure != BackpressureBlock || res.Waits == 0 || res.WaitTotal <= 0 || res.WaitMax <= 0 || res.WaitMax > res.WaitTotal {
		t.Fatalf("waits = %d total %s max %s, want reported waits", res.Waits, res.WaitTotal, res.WaitMax)
	}
	next := map[int]int64{}
	if err := Stream(context.Background(), path, func(op *OpLine) error {
		cmd, err := DecodeCommand(op.Cmd)
		if err != nil {
			return err
		}
		if i := cmd[0].Value.(int64); i != next[op.Worker] {
			return fmt.Errorf("worker %d: got i=%d, want %d", op.Worker, i, next[op.Worker])
		}
		next[op.Worker]++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	info, err := Scan(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Header.Backpressure != BackpressureBlock || info.Header.FailurePolicy != BlockPolicy {
		t.Fatalf("header backpressure = %q / %q", info.Header.Backpressure, info.Header.FailurePolicy)
	}
	tr := info.Trailer
	if tr.Waits != res.Waits || tr.WaitTotalUS != res.WaitTotal.Microseconds() || tr.WaitMaxUS != res.WaitMax.Microseconds() {
		t.Fatalf("trailer waits = %+v, result %+v", tr, res)
	}
}

// TestDefaultOptionsWriteThePreviousFormat is the golden check: with the
// default options and one iteration, no new key appears and every line is
// byte-identical to the single-iteration, fail-on-backpressure layout.
func TestDefaultOptionsWriteThePreviousFormat(t *testing.T) {
	h := testHeader(1)
	h.CreatedAt = "2026-01-01T00:00:00Z"
	r, path := newRecorderWith(t, Options{MaxBytes: 4096}, h)
	r.Record(Entry{Worker: 1, NS: "db.c", Op: "find", OK: true, Command: bson.D{{Key: "filter", Value: bson.D{{Key: "a", Value: int32(1)}}}}})
	r.Close()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %d", len(lines))
	}
	wantHeader := `{"type":"header","format":"plgm-workload-record/1","plgm_version":"test","created_at":"2026-01-01T00:00:00Z","random_seed":7,"workers":1,"collections_hash":"` + strings.Repeat("a", 64) + `","queries_hash":"` + strings.Repeat("b", 64) + `","namespaces":["db.c"],"config":{"concurrency":0},"failure_policy":"` + FailurePolicy + `","max_bytes":4096}`
	if lines[0] != wantHeader {
		t.Fatalf("header changed:\n got %s\nwant %s", lines[0], wantHeader)
	}
	wantOp := `{"type":"op","seq":1,"worker":1,"worker_seq":1,"offset_us":0,"ns":"db.c","op":"find","ok":true,"cmd":{"filter":{"a":{"$numberInt":"1"}}}}`
	if lines[1] != wantOp {
		t.Fatalf("op line changed:\n got %s\nwant %s", lines[1], wantOp)
	}
	for _, key := range []string{`"iteration`, `"waits"`, `"wait_`, `"backpressure"`, `"interval_delay"`, `"outcome`, `"result_checksums"`} {
		if strings.Contains(string(b), key) {
			t.Fatalf("default recording contains new key %s:\n%s", key, b)
		}
	}
}

func TestUnlimitedMaxBytesNeverTruncates(t *testing.T) {
	big := strings.Repeat("x", 4096)
	write := func(maxBytes int64) Result {
		r, _ := newRecorderWith(t, Options{MaxBytes: maxBytes}, testHeader(1))
		for i := 0; i < 600; i++ {
			r.Record(Entry{Worker: 1, NS: "db.c", Op: "insert", Command: bson.D{{Key: "document", Value: bson.D{{Key: "i", Value: int64(i)}, {Key: "pad", Value: big}}}}})
		}
		return r.Close()
	}
	if res := write(1 << 20); res.Status != StatusTruncated {
		t.Fatalf("1 MiB cap: %+v, want truncated", res)
	}
	res := write(0)
	if res.Status != StatusComplete || res.Ops != 600 || res.Bytes <= 2<<20 {
		t.Fatalf("unlimited: %+v, want all 600 ops (> 2 MiB)", res)
	}
}

func TestMultiIterationRecordingKeepsIterationOrderAndCounts(t *testing.T) {
	h := testHeader(2)
	h.Iterations, h.IntervalDelay = 3, "50ms"
	r, path := newRecorderWith(t, Options{Compression: CompressionGzip}, h)
	start := time.Now()
	r.SetStart(start)
	counts := []int{3, 0, 2}
	for it, n := range counts {
		r.SetIteration(it + 1)
		r.SetStart(start.Add(time.Hour))
		for i := 0; i < n; i++ {
			r.Record(Entry{Worker: 1 + i%2, NS: "db.c", Op: "find", Offset: r.Offset(start.Add(time.Duration(it) * time.Second)), Command: bson.D{{Key: "i", Value: int64(i)}}})
		}
	}
	res := r.Close()
	if res.Status != StatusComplete || res.Ops != 5 || res.Iterations != 3 {
		t.Fatalf("result = %+v", res)
	}
	var iters []int
	var offsets []int64
	if err := Stream(context.Background(), path, func(op *OpLine) error {
		iters = append(iters, op.Iteration)
		offsets = append(offsets, op.OffsetUS)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(iters) != "[1 1 1 3 3]" {
		t.Fatalf("iterations = %v", iters)
	}
	if offsets[0] != 0 || offsets[3] != (2*time.Second).Microseconds() {
		t.Fatalf("offsets = %v: later SetStart calls must not move the start", offsets)
	}
	info, err := Scan(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Header.Iterations != 3 || info.Header.IntervalDelay != "50ms" || info.Iterations != 3 || info.IterationOps[1] != 3 || info.IterationOps[3] != 2 {
		t.Fatalf("scan = %+v", info)
	}
	if tr := info.Trailer.IterationOps; tr["1"] != 3 || tr["3"] != 2 || len(tr) != 2 {
		t.Fatalf("trailer iteration ops = %v", tr)
	}

	hh := testHeader(1)
	hh.Type, hh.Format, hh.Iterations = LineHeader, FormatVersion, 2
	op := func(seq uint64, it int) OpLine {
		return OpLine{Type: LineOp, Seq: seq, Worker: 1, WorkerSeq: seq, Iteration: it, NS: "db.c", Op: "find", Cmd: []byte(`{"filter":{}}`)}
	}
	if _, err := Scan(writeLines(t, hh, op(1, 2), op(2, 1))); err == nil || !strings.Contains(err.Error(), "iteration 1 after iteration 2") {
		t.Fatalf("decreasing iteration error = %v", err)
	}
	if _, err := Scan(writeLines(t, hh, op(1, 3))); err == nil || !strings.Contains(err.Error(), "exceeds the recorded 2 iterations") {
		t.Fatalf("iteration beyond header error = %v", err)
	}
}

func TestNewRecorderRejectsUnknownOptions(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewRecorder(filepath.Join(dir, "a.ndjson"), Options{Backpressure: "drop"}, testHeader(1)); err == nil {
		t.Fatal("unknown backpressure accepted")
	}
	if _, err := NewRecorder(filepath.Join(dir, "b.ndjson"), Options{Compression: "lz4"}, testHeader(1)); err == nil {
		t.Fatal("unknown compression accepted")
	}
}

// realisticEntries are varied find, update, and insert commands with random
// literals (ObjectIds, codes, dates, numbers), so compression ratios are
// not inflated by identical lines.
func realisticEntries(n int) []Entry {
	rng := rand.New(rand.NewSource(1))
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	out := make([]Entry, n)
	for i := range out {
		code := fmt.Sprintf("%c%c%04d", 'A'+rng.Intn(26), 'A'+rng.Intn(26), rng.Intn(10000))
		at := base.Add(time.Duration(rng.Int63n(int64(365 * 24 * time.Hour))))
		var cmd bson.D
		op := "find"
		switch i % 4 {
		case 0, 1:
			cmd = bson.D{{Key: "filter", Value: map[string]interface{}{"flight_code": code, "seats": map[string]interface{}{"$gte": int32(rng.Intn(300))}}}, {Key: "limit", Value: int64(10)}, {Key: "batchSize", Value: int32(10)}}
		case 2:
			op = "updateOne"
			cmd = bson.D{{Key: "filter", Value: map[string]interface{}{"_id": bson.NewObjectID()}}, {Key: "update", Value: map[string]interface{}{"$set": map[string]interface{}{"status": []string{"ontime", "delayed", "cancelled"}[rng.Intn(3)], "updated_at": at}}}, {Key: "upsert", Value: false}}
		case 3:
			op = "insert"
			cmd = bson.D{{Key: "document", Value: map[string]interface{}{"_id": bson.NewObjectID(), "flight_code": code, "departure": at, "seats": int32(rng.Intn(300)), "price": rng.Float64() * 900, "gate": fmt.Sprintf("G%d", rng.Intn(60))}}}
		}
		out[i] = Entry{Worker: 1, NS: "db.flights", Op: op, OK: true, Shape: &ShapeRef{Key: op + "|flight_code"}, Command: cmd}
	}
	return out
}

func benchmarkRecorderWith(b *testing.B, opts Options, includeWriter bool) {
	path := filepath.Join(b.TempDir(), "bench.ndjson")
	opts.QueueSize = b.N + 16
	r, err := NewRecorder(path, opts, testHeader(1))
	if err != nil {
		b.Fatal(err)
	}
	entries := realisticEntries(min(b.N, 1<<17))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Record(entries[i%len(entries)])
	}
	if !includeWriter {
		b.StopTimer()
	}
	res := r.Close()
	b.StopTimer()
	if res.Status != StatusComplete || res.Ops != uint64(b.N) {
		b.Fatalf("result = %+v", res)
	}
	if res.FileBytes > 0 {
		b.ReportMetric(float64(res.Bytes)/float64(res.FileBytes), "ratio")
	}
}

// BenchmarkRecorderEndToEnd{None,Zstd,Gzip}: encode on the worker plus the
// writer, with varied commands and each compression.
func BenchmarkRecorderEndToEndNone(b *testing.B) {
	benchmarkRecorderWith(b, Options{}, true)
}

func BenchmarkRecorderEndToEndZstd(b *testing.B) {
	benchmarkRecorderWith(b, Options{Compression: CompressionZstd}, true)
}

func BenchmarkRecorderEndToEndGzip(b *testing.B) {
	benchmarkRecorderWith(b, Options{Compression: CompressionGzip}, true)
}

// benchmarkWriter times only the writer goroutine: entries are encoded and
// queued first, then the writer drains them. ns/op is the writer's cost per
// operation, so 1e9/ns_per_op is the ops/s one recording sustains.
func benchmarkWriter(b *testing.B, compression string) {
	path := filepath.Join(b.TempDir(), "bench.ndjson")
	r, err := NewRecorder(path, Options{QueueSize: b.N + 16, Compression: compression}, testHeader(1))
	if err != nil {
		b.Fatal(err)
	}
	release := make(chan struct{})
	r.beforeWrite = func() { <-release }
	entries := realisticEntries(min(b.N, 1<<17))
	for i := 0; i < b.N; i++ {
		r.Record(entries[i%len(entries)])
	}
	b.ReportAllocs()
	b.ResetTimer()
	close(release)
	res := r.Close()
	b.StopTimer()
	if res.Status != StatusComplete || res.Ops != uint64(b.N) {
		b.Fatalf("result = %+v", res)
	}
	if res.FileBytes > 0 {
		b.ReportMetric(float64(res.Bytes)/float64(res.FileBytes), "ratio")
	}
}

func BenchmarkRecorderWriterNone(b *testing.B) { benchmarkWriter(b, CompressionNone) }
func BenchmarkRecorderWriterZstd(b *testing.B) { benchmarkWriter(b, CompressionZstd) }
func BenchmarkRecorderWriterGzip(b *testing.B) { benchmarkWriter(b, CompressionGzip) }
