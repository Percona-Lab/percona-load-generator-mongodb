package workloadrec

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func rawDoc(t testing.TB, d bson.D) bson.Raw {
	t.Helper()
	b, err := bson.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestResultHashIsStableAndOrderSensitive(t *testing.T) {
	a := rawDoc(t, bson.D{{Key: "_id", Value: int32(1)}, {Key: "n", Value: int64(5)}})
	b := rawDoc(t, bson.D{{Key: "_id", Value: int32(2)}, {Key: "n", Value: "x"}})
	sum := func(docs ...bson.Raw) string {
		h := NewResultHash()
		for _, d := range docs {
			if err := h.Add(d); err != nil {
				t.Fatal(err)
			}
		}
		return h.Sum()
	}
	if sum(a, b) != sum(a, b) {
		t.Fatal("the same result in the same order must hash equal")
	}
	if sum(a, b) == sum(b, a) {
		t.Fatal("the checksum must be order-sensitive")
	}
	if sum(a) == sum(a, b) || sum() == sum(a) {
		t.Fatal("a different result must hash differently")
	}
	// int32 vs int64 of the same number are different BSON types.
	c := rawDoc(t, bson.D{{Key: "_id", Value: int64(1)}, {Key: "n", Value: int64(5)}})
	if sum(a) == sum(c) {
		t.Fatal("the checksum must cover BSON types (canonical Extended JSON)")
	}
	// The same document stored with another field order (as seeding from
	// Go maps does on each instance) is the same result.
	nested := func(order bool) bson.Raw {
		inner := bson.D{{Key: "x", Value: int32(1)}, {Key: "y", Value: "z"}}
		if !order {
			inner = bson.D{{Key: "y", Value: "z"}, {Key: "x", Value: int32(1)}}
		}
		d := bson.D{{Key: "_id", Value: int32(1)}, {Key: "sub", Value: inner}, {Key: "arr", Value: bson.A{inner, int32(2)}}}
		if !order {
			d = bson.D{{Key: "arr", Value: bson.A{inner, int32(2)}}, {Key: "sub", Value: inner}, {Key: "_id", Value: int32(1)}}
		}
		return rawDoc(t, d)
	}
	if sum(nested(true)) != sum(nested(false)) {
		t.Fatal("field order inside documents must not change the checksum")
	}
	arr := func(vals ...interface{}) bson.Raw { return rawDoc(t, bson.D{{Key: "a", Value: bson.A(vals)}}) }
	if sum(arr(int32(1), int32(2))) == sum(arr(int32(2), int32(1))) {
		t.Fatal("array order must change the checksum")
	}
}

func TestRecorderWritesOutcomesOnlyWhenGiven(t *testing.T) {
	h := testHeader(1)
	h.Outcomes, h.ResultChecksums = true, true
	path := filepath.Join(t.TempDir(), "rec.ndjson")
	r, err := NewRecorder(path, Options{}, h)
	if err != nil {
		t.Fatal(err)
	}
	out := &Outcome{LatencyUS: 120, OK: true, Returned: Count(0), Checksum: strings.Repeat("c", 64)}
	r.Record(Entry{Worker: 1, NS: "db.c", Op: "find", OK: true, Command: bson.D{{Key: "filter", Value: bson.D{}}}, Outcome: out})
	r.Record(Entry{Worker: 1, NS: "db.c", Op: "insert", Command: bson.D{{Key: "document", Value: bson.D{}}}, Outcome: &Outcome{LatencyUS: 9, ErrorCode: 11000, ErrorName: "DuplicateKey", Inserted: Count(0)}})
	res := r.Close()
	if !res.Outcomes || !res.ResultChecksums {
		t.Fatalf("result = %+v, want outcomes and checksums flagged", res)
	}
	info, err := Scan(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Header.Outcomes || !info.Header.ResultChecksums {
		t.Fatalf("header = %+v", info.Header)
	}
	var got []*Outcome
	if err := Stream(context.Background(), path, func(op *OpLine) error { got = append(got, op.Outcome); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] == nil || got[0].Returned == nil || *got[0].Returned != 0 || got[0].Checksum != out.Checksum || got[0].LatencyUS != 120 || !got[0].OK {
		t.Fatalf("find outcome = %+v", got[0])
	}
	if got[1].OK || got[1].ErrorCode != 11000 || got[1].ErrorName != "DuplicateKey" || got[1].Inserted == nil || *got[1].Inserted != 0 {
		t.Fatalf("insert outcome = %+v", got[1])
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), `"returned":0`) {
		t.Fatalf("a zero count must be written, not omitted:\n%s", b)
	}
}

// TestDefaultRecordingHasNoOutcomeKeys complements the golden check in
// TestDefaultOptionsWriteThePreviousFormat for the outcome keys.
func TestDefaultRecordingHasNoOutcomeKeys(t *testing.T) {
	r, path := newTestRecorder(t, 0, 0, 1)
	r.Record(Entry{Worker: 1, NS: "db.c", Op: "find", OK: true, Command: bson.D{{Key: "filter", Value: bson.D{}}}})
	r.Close()
	b, _ := os.ReadFile(path)
	for _, key := range []string{`"outcome`, `"result_checksums"`} {
		if strings.Contains(string(b), key) {
			t.Fatalf("recording without outcomes contains %s:\n%s", key, b)
		}
	}
}

func TestOutcomeWriterRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.ndjson")
	w, err := NewOutcomeWriter(path, OutcomeHeader{Source: OutcomeSourceReplay, PLGMVersion: "test", Workers: 2, RecordingFile: "rec.ndjson", RecordingHeaderHash: strings.Repeat("h", 64), ResultChecksums: true})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for worker := 1; worker <= 2; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := uint64(1); i <= 50; i++ {
				w.Write(OutcomeLine{Seq: i*2 - uint64(2-worker), Worker: worker, WorkerSeq: i, NS: "db.c", Op: "find", Shape: "find|a", Outcome: Outcome{LatencyUS: int64(i), OK: true, Returned: Count(int64(i))}})
			}
		}(worker)
	}
	wg.Wait()
	res := w.Close(OutcomeTrailer{CommandsHash: strings.Repeat("d", 64)}, StatusComplete, "")
	if res.Status != StatusComplete || res.Ops != 100 || res.Bytes <= 0 {
		t.Fatalf("result = %+v", res)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
	last := map[int]uint64{}
	n := 0
	h, tr, err := StreamOutcomes(context.Background(), path, func(l *OutcomeLine) error {
		if l.WorkerSeq != last[l.Worker]+1 {
			t.Fatalf("worker %d seq %d after %d: per-worker order lost", l.Worker, l.WorkerSeq, last[l.Worker])
		}
		last[l.Worker] = l.WorkerSeq
		n++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if h.Source != OutcomeSourceReplay || h.RecordingHeaderHash != strings.Repeat("h", 64) || !h.ResultChecksums || h.Format != OutcomeFormatVersion {
		t.Fatalf("header = %+v", h)
	}
	if tr == nil || tr.Status != StatusComplete || tr.Ops != 100 || tr.CommandsHash != strings.Repeat("d", 64) || n != 100 {
		t.Fatalf("trailer = %+v, lines %d", tr, n)
	}
}

func TestOutcomeWriterRefusesOtherFilesAndReaderRejectsRecordings(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(other, []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewOutcomeWriter(other, OutcomeHeader{Source: OutcomeSourceReplay}); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("overwrite error = %v", err)
	}
	if b, _ := os.ReadFile(other); string(b) != "keep me\n" {
		t.Fatal("an unrelated file was changed")
	}
	r, rec := newTestRecorder(t, 0, 0, 1)
	r.Close()
	if _, err := NewOutcomeWriter(rec, OutcomeHeader{Source: OutcomeSourceReplay}); err == nil {
		t.Fatal("an outcome file must not overwrite a recording")
	}
	if _, _, err := StreamOutcomes(context.Background(), rec, nil); err == nil || !strings.Contains(err.Error(), "outcome header") {
		t.Fatalf("reading a recording as outcomes: %v", err)
	}
	path := filepath.Join(dir, "out.ndjson")
	w, err := NewOutcomeWriter(path, OutcomeHeader{Source: OutcomeSourceDeterministic})
	if err != nil {
		t.Fatal(err)
	}
	w.Close(OutcomeTrailer{TrafficFingerprint: strings.Repeat("f", 64), TrafficStatus: "incomplete"}, "incomplete", "safety timeout")
	if _, err := Scan(path); err == nil {
		t.Fatal("an outcome file must not scan as a recording")
	}
	w2, err := NewOutcomeWriter(path, OutcomeHeader{Source: OutcomeSourceDeterministic})
	if err != nil {
		t.Fatalf("an earlier outcome file must be replaceable: %v", err)
	}
	w2.Close(OutcomeTrailer{}, StatusComplete, "")
}

func TestOutcomeWithoutTrailerReportsNoTrailer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cut.ndjson")
	h, _ := json.Marshal(OutcomeHeader{Type: LineOutcomeHeader, Format: OutcomeFormatVersion, Source: OutcomeSourceReplay})
	l, _ := json.Marshal(OutcomeLine{Type: LineOutcome, Seq: 1, Worker: 1, WorkerSeq: 1, NS: "db.c", Op: "find"})
	if err := os.WriteFile(path, append(append(append(h, '\n'), l...), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	n := 0
	_, tr, err := StreamOutcomes(context.Background(), path, func(*OutcomeLine) error { n++; return nil })
	if err != nil || tr != nil || n != 1 {
		t.Fatalf("cut file: err=%v trailer=%v lines=%d", err, tr, n)
	}
}

// BenchmarkRecorderRecordWithOutcome is BenchmarkRecorderRecord with an
// outcome on the entry (the outcome is encoded by the writer goroutine).
func BenchmarkRecorderRecordWithOutcome(b *testing.B) {
	path := filepath.Join(b.TempDir(), "bench.ndjson")
	h := testHeader(1)
	h.Outcomes = true
	r, err := NewRecorder(path, Options{QueueSize: b.N + 16}, h)
	if err != nil {
		b.Fatal(err)
	}
	e := benchmarkFindEntry()
	e.Outcome = &Outcome{LatencyUS: 250, OK: true, Returned: Count(10), Checksum: strings.Repeat("c", 64)}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Record(e)
	}
	res := r.Close()
	b.StopTimer()
	if res.Ops != uint64(b.N) {
		b.Fatalf("result = %+v", res)
	}
}

// BenchmarkResultHashPerDocument is the checksum cost per returned document
// (a typical 10-field document).
func BenchmarkResultHashPerDocument(b *testing.B) {
	doc := rawDoc(b, bson.D{
		{Key: "_id", Value: bson.NewObjectID()}, {Key: "flight_code", Value: "AA1234"}, {Key: "seats", Value: int32(180)},
		{Key: "price", Value: 412.5}, {Key: "status", Value: "ontime"}, {Key: "gate", Value: "G12"},
		{Key: "departure", Value: bson.DateTime(1767225600000)}, {Key: "origin", Value: "LIS"}, {Key: "dest", Value: "JFK"},
		{Key: "tags", Value: bson.A{"a", "b"}},
	})
	h := NewResultHash()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := h.Add(doc); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkOutcomeWriterWrite is the worker's cost per outcome line in a
// replay or deterministic run, including the writer goroutine.
func BenchmarkOutcomeWriterWrite(b *testing.B) {
	w, err := NewOutcomeWriter(filepath.Join(b.TempDir(), "out.ndjson"), OutcomeHeader{Source: OutcomeSourceReplay})
	if err != nil {
		b.Fatal(err)
	}
	l := OutcomeLine{Seq: 1, Worker: 1, WorkerSeq: 1, NS: "db.flights", Op: "find", Shape: "find|flight_code|limit:10", Outcome: Outcome{LatencyUS: 250, OK: true, Returned: Count(10)}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w.Write(l)
	}
	w.Close(OutcomeTrailer{}, StatusComplete, "")
}
