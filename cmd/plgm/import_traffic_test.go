package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/workloadrec"
)

func TestImportTrafficCommand(t *testing.T) {
	dir := t.TempDir()
	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "trafficimport", "testdata", "mongod.log"))
	if err != nil {
		t.Fatal(err)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(raw)
	zw.Close()
	in := filepath.Join(dir, "mongod.log.gz")
	if err := os.WriteFile(in, gz.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	out, jp := filepath.Join(dir, "imp.ndjson.zst"), filepath.Join(dir, "imp.json")

	var stdout, stderr bytes.Buffer
	code := runImportTraffic([]string{in, "--out", out, "--compression", "zstd", "--json", jp, "--workers", "3"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Imported 8 of 15 entries") || !strings.Contains(stdout.String(), "slowms") || !strings.Contains(stdout.String(), "literal values") {
		t.Fatalf("summary: %s", stdout.String())
	}
	info, err := workloadrec.Scan(out)
	if err != nil || info.Ops != 8 || info.Compression != workloadrec.CompressionZstd || info.Header.Import == nil || info.Header.Workers != 3 {
		t.Fatalf("scan %+v err %v", info, err)
	}
	var rep map[string]interface{}
	b, _ := os.ReadFile(jp)
	if err := json.Unmarshal(b, &rep); err != nil || rep["header_hash"] != info.HeaderHash {
		t.Fatalf("json report %s: %v", b, err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := runImportTraffic(nil, &stdout, &stderr); code != 2 {
		t.Fatalf("no files: exit %d", code)
	}
	empty := filepath.Join(dir, "empty.log")
	os.WriteFile(empty, []byte("{\"ctx\":\"listener\",\"attr\":{}}\n"), 0o600)
	if code := runImportTraffic([]string{empty, "--out", filepath.Join(dir, "none.ndjson")}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "no operations") {
		t.Fatalf("nothing to import: exit %d %s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "none.ndjson")); !os.IsNotExist(err) {
		t.Fatal("no recording must be written when nothing converts")
	}
}
