package workloadrec

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestPeekRecordingReadsHeaderAndCheapTrailer(t *testing.T) {
	for _, c := range []string{CompressionNone, CompressionZstd, CompressionGzip} {
		r, path := newRecorderWith(t, Options{Compression: c}, Header{PLGMVersion: "test", Workers: 3})
		for i := 0; i < 5; i++ {
			r.Record(Entry{Worker: 1, NS: "db.c", Op: "find", OK: true, Command: bson.D{{Key: "find", Value: "c"}}})
		}
		if res := r.Close(); res.Status != StatusComplete {
			t.Fatalf("%s: %+v", c, res)
		}
		s, err := PeekRecording(path)
		if err != nil {
			t.Fatalf("%s: %v", c, err)
		}
		if s.Header.Workers != 3 || s.Compression != c {
			t.Fatalf("%s: summary %+v", c, s)
		}
		if c == CompressionNone {
			if s.Trailer == nil || s.Trailer.Ops != 5 || s.Trailer.Status != StatusComplete {
				t.Fatalf("plain trailer = %+v", s.Trailer)
			}
		} else if s.Trailer != nil {
			t.Fatalf("%s: a compressed file is not read to its end", c)
		}
	}
}

func TestPeekRecordingRejectsOtherFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for name, content := range map[string]string{
		"empty.ndjson":   "",
		"outcome.ndjson": `{"type":"outcome_header","format":"x"}` + "\n",
		"other.json":     `{"uri":"mongodb://x"}`,
		"long.ndjson":    strings.Repeat("x", MaxPeekHeaderBytes+10),
	} {
		if _, err := PeekRecording(write(name, content)); !errors.Is(err, ErrNotRecording) {
			t.Fatalf("%s: err = %v, want ErrNotRecording", name, err)
		}
	}
	huge := `{"type":"header","format":"` + FormatVersion + `","plgm_version":"` + strings.Repeat("v", MaxPeekHeaderBytes) + `"}` + "\n"
	if _, err := PeekRecording(write("huge.ndjson", huge)); err == nil || errors.Is(err, ErrNotRecording) {
		t.Fatalf("a header longer than the limit must fail as a damaged recording: %v", err)
	}
	if _, err := PeekRecording(write("v9.ndjson", `{"type":"header","format":"plgm-workload-record/9"}`+"\n")); err == nil || !strings.Contains(err.Error(), "unsupported workload recording format") {
		t.Fatalf("unsupported format: %v", err)
	}
}
