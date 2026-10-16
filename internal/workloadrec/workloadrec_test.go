package workloadrec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func testHeader(workers int) Header {
	return Header{PLGMVersion: "test", RandomSeed: 7, Workers: workers, CollectionsHash: strings.Repeat("a", 64), QueriesHash: strings.Repeat("b", 64), Namespaces: []string{"db.c"}}
}

func newTestRecorder(t *testing.T, maxBytes int64, queue int, workers int) (*Recorder, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rec.ndjson")
	r, err := NewRecorder(path, Options{MaxBytes: maxBytes, QueueSize: queue}, testHeader(workers))
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	return r, path
}

func mustDecimal(t *testing.T, s string) bson.Decimal128 {
	t.Helper()
	d, err := bson.ParseDecimal128(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestEncodeCommandRoundTripsEveryOperationType checks that each recorded
// command form keeps its BSON types and the order of ordered documents.
func TestEncodeCommandRoundTripsEveryOperationType(t *testing.T) {
	oid := bson.NewObjectID()
	when := time.Date(2025, 3, 4, 5, 6, 7, 8_000_000, time.UTC)
	doc := map[string]interface{}{
		"_id": oid, "i32": int32(7), "i64": int64(1 << 40), "f": 1.5, "dec": mustDecimal(t, "12.34"),
		"b": true, "s": "<a&b>", "at": when, "nested": map[string]interface{}{"z": int32(1), "a": "x"},
		"arr": []interface{}{int32(1), "two", map[string]interface{}{"k": int64(3)}},
		"ts":  bson.Timestamp{T: 10, I: 2},
	}
	sortSpec := bson.D{{Key: "z", Value: int32(-1)}, {Key: "a", Value: int32(1)}}
	cmds := map[string]interface{}{
		"find":       bson.D{{Key: "filter", Value: map[string]interface{}{"i32": map[string]interface{}{"$gte": int32(3)}}}, {Key: "projection", Value: map[string]interface{}{"s": int32(1)}}, {Key: "limit", Value: int64(5)}, {Key: "batchSize", Value: int32(10)}},
		"aggregate":  bson.D{{Key: "pipeline", Value: []interface{}{map[string]interface{}{"$match": map[string]interface{}{"b": true}}, map[string]interface{}{"$sort": sortSpec}}}},
		"updateOne":  bson.D{{Key: "filter", Value: map[string]interface{}{"_id": oid}}, {Key: "update", Value: map[string]interface{}{"$set": map[string]interface{}{"at": when}}}, {Key: "upsert", Value: true}},
		"deleteMany": bson.D{{Key: "filter", Value: map[string]interface{}{"dec": mustDecimal(t, "1.0")}}},
		"insert":     bson.D{{Key: "document", Value: doc}},
		"insertMany": bson.D{{Key: "documents", Value: []interface{}{doc, doc}}},
		"transaction": bson.D{{Key: "ops", Value: bson.A{
			bson.D{{Key: "op", Value: "insert"}, {Key: "ns", Value: "db.c"}, {Key: "args", Value: bson.D{{Key: "document", Value: doc}}}},
		}}},
		"fast_find": bson.D{{Key: "find", Value: "c"}, {Key: "filter", Value: bson.D{{Key: "k", Value: bson.D{{Key: "$in", Value: []int64{1, 2}}}}}}, {Key: "limit", Value: 2}},
	}
	for name, cmd := range cmds {
		t.Run(name, func(t *testing.T) {
			enc, err := EncodeCommand(cmd)
			if err != nil {
				t.Fatal(err)
			}
			dec, err := DecodeCommand(enc)
			if err != nil {
				t.Fatal(err)
			}
			again, err := EncodeCommand(dec)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(enc, again) {
				t.Fatalf("re-encoding changed the command:\n%s\n%s", enc, again)
			}
		})
	}

	enc, _ := EncodeCommand(bson.D{{Key: "document", Value: doc}})
	dec, _ := DecodeCommand(enc)
	got, _ := dec[0].Value.(bson.D)
	want := map[string]string{"_id": "bson.ObjectID", "i32": "int32", "i64": "int64", "f": "float64", "dec": "bson.Decimal128", "b": "bool", "s": "string", "at": "bson.DateTime", "nested": "bson.D", "arr": "bson.A", "ts": "bson.Timestamp"}
	keys := make([]string, 0, len(got))
	for _, e := range got {
		keys = append(keys, e.Key)
		if w, ok := want[e.Key]; ok && fmt.Sprintf("%T", e.Value) != w {
			t.Errorf("%s decoded as %T, want %s", e.Key, e.Value, w)
		}
	}
	if strings.Join(keys, ",") != "_id,arr,at,b,dec,f,i32,i64,nested,s,ts" {
		t.Fatalf("map keys not sorted: %v", keys)
	}
	if v := got[0].Value.(bson.ObjectID); v != oid {
		t.Fatalf("_id = %v, want %v", v, oid)
	}

	aggEnc, _ := EncodeCommand(cmds["aggregate"])
	aggDec, _ := DecodeCommand(aggEnc)
	stage := aggDec[0].Value.(bson.A)[1].(bson.D)
	spec := stage[0].Value.(bson.D)
	if spec[0].Key != "z" || spec[1].Key != "a" {
		t.Fatalf("$sort order changed: %v", spec)
	}
}

func TestRecorderKeepsCommandBytesAndHashes(t *testing.T) {
	r, path := newTestRecorder(t, 0, 0, 1)
	cmd := bson.D{{Key: "filter", Value: map[string]interface{}{"s": "<a&b>"}}}
	r.Record(Entry{Worker: 1, NS: "db.c", Op: "find", OK: true, Command: cmd})
	res := r.Close()
	if res.Status != StatusComplete || res.Ops != 1 {
		t.Fatalf("result = %+v", res)
	}
	want, _ := EncodeCommand(cmd)
	var got []byte
	if err := Stream(context.Background(), path, func(op *OpLine) error { got = append([]byte(nil), op.Cmd...); return nil }); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("recorded cmd = %s, want %s", got, want)
	}
	h := NewStreamHash()
	h.Add(want)
	if res.CommandsHash != CombineHashes(map[int]string{1: h.Sum()}) {
		t.Fatal("commands hash does not cover the recorded bytes")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestRecorderPreservesPerWorkerOrderUnderConcurrency(t *testing.T) {
	const workers, perWorker = 8, 400
	r, path := newTestRecorder(t, 0, 0, workers)
	r.SetStart(time.Now())
	var wg sync.WaitGroup
	for w := 1; w <= workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				r.Record(Entry{Worker: w, NS: "db.c", Op: "find", OK: true, Offset: r.Offset(time.Now()), Command: bson.D{{Key: "i", Value: int64(i)}}})
			}
		}(w)
	}
	wg.Wait()
	res := r.Close()
	if res.Status != StatusComplete || res.Ops != workers*perWorker {
		t.Fatalf("result = %+v", res)
	}
	next := map[int]int64{}
	var seq uint64
	err := Stream(context.Background(), path, func(op *OpLine) error {
		seq++
		if op.Seq != seq {
			return fmt.Errorf("global seq %d, want %d", op.Seq, seq)
		}
		cmd, err := DecodeCommand(op.Cmd)
		if err != nil {
			return err
		}
		if i := cmd[0].Value.(int64); i != next[op.Worker] || op.WorkerSeq != uint64(i+1) {
			return fmt.Errorf("worker %d: got i=%d worker_seq=%d, want i=%d", op.Worker, i, op.WorkerSeq, next[op.Worker])
		}
		next[op.Worker]++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	info, err := Scan(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusComplete || info.Ops != workers*perWorker || info.Workers != workers {
		t.Fatalf("scan = %+v", info)
	}
}

func TestRecorderFailsWhenQueueIsFull(t *testing.T) {
	r, path := newTestRecorder(t, 0, 1, 1)
	release := make(chan struct{})
	r.beforeWrite = func() { <-release }
	for i := 0; i < 4 && r.Active(); i++ {
		r.Record(Entry{Worker: 1, NS: "db.c", Op: "find", Command: bson.D{{Key: "i", Value: int32(i)}}})
	}
	if r.Active() {
		t.Fatal("recording still active after the queue filled")
	}
	close(release)
	res := r.Close()
	if res.Status != StatusFailed || !strings.Contains(res.Reason, "writer_queue_full") {
		t.Fatalf("result = %+v, want failed/writer_queue_full", res)
	}
	info, err := Scan(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusFailed || info.Trailer == nil || info.Trailer.Ops != info.Ops {
		t.Fatalf("scan = %+v", info)
	}
}

func TestRecorderTruncatesAtMaxBytes(t *testing.T) {
	const maxBytes = 6000
	r, path := newTestRecorder(t, maxBytes, 0, 1)
	for i := 0; i < 200; i++ {
		r.Record(Entry{Worker: 1, NS: "db.c", Op: "insert", Command: bson.D{{Key: "document", Value: map[string]interface{}{"i": int64(i), "pad": strings.Repeat("x", 80)}}}})
	}
	res := r.Close()
	if res.Status != StatusTruncated || res.Ops == 0 || res.Ops >= 200 {
		t.Fatalf("result = %+v, want truncated with a prefix", res)
	}
	fi, _ := os.Stat(path)
	if fi.Size() > maxBytes {
		t.Fatalf("file size %d exceeds max %d", fi.Size(), maxBytes)
	}
	info, err := Scan(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusTruncated || info.Ops != res.Ops {
		t.Fatalf("scan = %+v", info)
	}
}

func TestRecorderRefusesToOverwriteOtherFiles(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(other, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRecorder(other, Options{}, testHeader(1)); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("NewRecorder error = %v, want refusal", err)
	}
	if b, _ := os.ReadFile(other); string(b) != "keep me" {
		t.Fatal("unrelated file was changed")
	}
	prev := filepath.Join(dir, "prev.ndjson")
	r, err := NewRecorder(prev, Options{}, testHeader(1))
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	r2, err := NewRecorder(prev, Options{}, testHeader(1))
	if err != nil {
		t.Fatalf("overwriting an earlier recording: %v", err)
	}
	r2.Close()
}

func writeLines(t *testing.T, lines ...interface{}) string {
	t.Helper()
	var buf bytes.Buffer
	for _, l := range lines {
		switch v := l.(type) {
		case string:
			buf.WriteString(v)
		default:
			b, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			buf.Write(b)
		}
		buf.WriteByte('\n')
	}
	path := filepath.Join(t.TempDir(), "f.ndjson")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestScanReportsBrokenFiles(t *testing.T) {
	h := testHeader(1)
	h.Type, h.Format = LineHeader, FormatVersion
	op := func(seq, wseq uint64) OpLine {
		return OpLine{Type: LineOp, Seq: seq, Worker: 1, WorkerSeq: wseq, NS: "db.c", Op: "find", Cmd: json.RawMessage(`{"filter":{}}`)}
	}
	tr := Trailer{Type: LineTrailer, Status: StatusComplete, Ops: 1}
	cases := map[string]struct {
		lines []interface{}
		want  string
	}{
		"empty":            {lines: nil, want: "empty"},
		"no header":        {lines: []interface{}{op(1, 1)}, want: "does not start with a header"},
		"wrong format":     {lines: []interface{}{`{"type":"header","format":"other/9"}`}, want: "unsupported workload recording format"},
		"out of order":     {lines: []interface{}{h, op(1, 2)}, want: "out of order"},
		"after trailer":    {lines: []interface{}{h, op(1, 1), tr, op(2, 2)}, want: "after the trailer"},
		"count mismatch":   {lines: []interface{}{h, op(1, 1), op(2, 2), tr}, want: "trailer counts 1"},
		"unknown op":       {lines: []interface{}{h, `{"type":"op","seq":1,"worker":1,"worker_seq":1,"ns":"db.c","op":"drop","cmd":{}}`}, want: "unknown operation"},
		"missing worker":   {lines: []interface{}{h, `{"type":"op","seq":1,"worker":0,"worker_seq":1,"ns":"db.c","op":"find","cmd":{}}`}, want: "worker must be at least 1"},
		"corrupt json":     {lines: []interface{}{h, `{"type":"op",`}, want: "line 2"},
		"unknown fastpath": {lines: []interface{}{h, `{"type":"op","seq":1,"worker":1,"worker_seq":1,"ns":"db.c","op":"find","path":"x","cmd":{}}`}, want: "unknown path"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Scan(writeLines(t, tc.lines...))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Scan error = %v, want containing %q", err, tc.want)
			}
		})
	}

	info, err := Scan(writeLines(t, h, op(1, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusIncomplete || info.Trailer != nil || info.Ops != 1 {
		t.Fatalf("file without trailer: %+v, want incomplete", info)
	}
}

func benchmarkFindEntry() Entry {
	return Entry{Worker: 1, NS: "db.flights", Op: "find", OK: true,
		Query: &QueryRef{Name: "by_code", Index: 3}, Shape: &ShapeRef{Key: "find|flight_code|limit:10", Fields: []string{"flight_code"}},
		Command: bson.D{{Key: "filter", Value: map[string]interface{}{"flight_code": "AA1234", "seats": map[string]interface{}{"$gte": int32(4)}}}, {Key: "limit", Value: int64(10)}, {Key: "batchSize", Value: int32(10)}}}
}

// BenchmarkEncodeCommand is the per-operation encoding work of the writer.
func BenchmarkEncodeCommand(b *testing.B) {
	cmd := benchmarkFindEntry().Command
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := EncodeCommand(cmd); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkRecorder(b *testing.B, includeWriter bool) {
	path := filepath.Join(b.TempDir(), "bench.ndjson")
	r, err := NewRecorder(path, Options{QueueSize: b.N + 16}, testHeader(1))
	if err != nil {
		b.Fatal(err)
	}
	e := benchmarkFindEntry()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Record(e)
	}
	if !includeWriter {
		b.StopTimer()
	}
	res := r.Close()
	b.StopTimer()
	if res.Status != StatusComplete || res.Ops != uint64(b.N) {
		b.Fatalf("result = %+v", res)
	}
}

// BenchmarkRecorderRecord is the cost a worker pays per recorded operation:
// encoding the command and a non-blocking enqueue.
func BenchmarkRecorderRecord(b *testing.B) { benchmarkRecorder(b, false) }

// BenchmarkRecorderEndToEnd adds the writer goroutine's framing, hashing,
// and file write; its ns/op bounds the operations per second one recording
// sustains before the queue fills.
func BenchmarkRecorderEndToEnd(b *testing.B) { benchmarkRecorder(b, true) }
