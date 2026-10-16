package trafficimport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/mongo"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/workloadrec"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func fixture(t *testing.T, name string) Input {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return Input{Name: filepath.Join("testdata", name), R: bytes.NewReader(b)}
}

func convert(t *testing.T, opts Options, inputs ...Input) (*Report, string, []*workloadrec.OpLine, *workloadrec.FileInfo) {
	t.Helper()
	if opts.Normalize == nil {
		opts.Normalize = mongo.ImportedOperation
	}
	out := filepath.Join(t.TempDir(), "imported.ndjson")
	rep, err := Convert(inputs, out, opts)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	info, err := workloadrec.Scan(out)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	var ops []*workloadrec.OpLine
	if err := workloadrec.Stream(context.Background(), out, func(op *workloadrec.OpLine) error {
		ops = append(ops, op)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return rep, out, ops, info
}

type opSummary struct {
	Op     string
	Worker int
	Offset int64
	Flags  string
	OK     bool
}

func summarize(ops []*workloadrec.OpLine) []opSummary {
	out := make([]opSummary, len(ops))
	for i, op := range ops {
		out[i] = opSummary{op.Op, op.Worker, op.OffsetUS, strings.Join(op.Flags, ","), op.OK}
	}
	return out
}

func decode(t *testing.T, op *workloadrec.OpLine) bson.D {
	t.Helper()
	d, err := workloadrec.DecodeCommand(op.Cmd)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestConvertMongodLog(t *testing.T) {
	rep, out, ops, info := convert(t, Options{}, fixture(t, "mongod.log"))

	want := []opSummary{
		{"aggregate", 1, 0, "", true},
		{"find", 2, 100000, "", true},
		{"insertMany", 2, 1100000, "server_generated_values", true},
		{"updateOne", 1, 2100000, "split_statement", true},
		{"updateMany", 1, 2100000, "dropped_options,split_statement", true},
		{"deleteOne", 2, 3100000, "", true},
		{"updateOne", 2, 4999000, "in_transaction,server_generated_values", true},
		{"find", 1, 6089000, "failed_on_source", false},
	}
	if got := summarize(ops); !reflect.DeepEqual(got, want) {
		t.Fatalf("ops (start-time order, connection = worker):\n got %+v\nwant %+v", got, want)
	}
	for i, op := range ops {
		if op.Seq != uint64(i+1) || op.NS != "shop.orders" || op.Shape == nil || op.Shape.Key == "" {
			t.Fatalf("op %d = %+v", i, op)
		}
	}

	find := decode(t, ops[1])
	keys := []string{}
	for _, e := range find {
		keys = append(keys, e.Key)
	}
	if strings.Join(keys, ",") != "filter,projection,sort,skip,hint,limit,batchSize" {
		t.Fatalf("find keys = %v", keys)
	}
	sortDoc := find[2].Value.(bson.D)
	if sortDoc[0].Key != "ts" || sortDoc[1].Key != "_id" {
		t.Fatalf("multi-key sort must keep its order: %v", sortDoc)
	}
	if find[3].Value != int64(2) || find[5].Value != int64(10) || find[6].Value != int32(5) {
		t.Fatalf("find skip/limit/batchSize = %#v %#v %#v", find[3].Value, find[5].Value, find[6].Value)
	}
	docs := decode(t, ops[2])[0].Value.(bson.A)
	first := docs[0].(bson.D)
	if _, ok := first[0].Value.(bson.ObjectID); !ok {
		t.Fatalf("_id type = %T", first[0].Value)
	}
	if _, ok := first[2].Value.(bson.DateTime); !ok {
		t.Fatalf("date type = %T", first[2].Value)
	}
	if first[3].Value != int64(7) {
		t.Fatalf("$numberLong = %#v", first[3].Value)
	}

	h := info.Header
	if h.Import == nil || !reflect.DeepEqual(h.Import.Sources, []string{SourceMongodLog}) || h.Workers != 2 || h.CollectionsHash != "" || h.Import.Note != Note {
		t.Fatalf("header = %+v import %+v", h, h.Import)
	}
	if info.Status != workloadrec.StatusComplete || info.Ops != 8 || rep.HeaderHash != info.HeaderHash {
		t.Fatalf("scan = %+v report hash %q", info, rep.HeaderHash)
	}
	if rep.Entries != 15 || rep.NonOperations != 1 || rep.Converted != 8 || rep.Skipped != 7 || rep.Connections != 2 || rep.Workers != 2 {
		t.Fatalf("report counts = %+v", rep)
	}
	wantSkipped := map[string]uint64{SkipDuplicateWrite: 1, SkipGetMore: 1, SkipTruncated: 1, SkipUnsupported: 2, SkipSystemNamespace: 1, SkipUnparseable: 1}
	if !reflect.DeepEqual(rep.SkippedByReason, wantSkipped) {
		t.Fatalf("skipped = %v", rep.SkippedByReason)
	}
	if !reflect.DeepEqual(rep.UnsupportedCommands, map[string]uint64{"count": 1, "update (replacement document)": 1}) {
		t.Fatalf("unsupported = %v", rep.UnsupportedCommands)
	}
	if !reflect.DeepEqual(rep.DroppedOptions, map[string]uint64{"arrayFilters": 1}) {
		t.Fatalf("dropped options = %v", rep.DroppedOptions)
	}
	if rep.FirstTimestamp != "2026-10-16T10:00:00.9Z" || rep.SpanMs != 6089 {
		t.Fatalf("time range = %s .. %s (%d ms)", rep.FirstTimestamp, rep.LastTimestamp, rep.SpanMs)
	}
	if len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "mongod.log line 15") {
		t.Fatalf("warnings = %v", rep.Warnings)
	}

	raw, _ := os.ReadFile(out)
	repJSON, _ := json.Marshal(rep)
	for _, b := range [][]byte{raw, repJSON, []byte(SummaryText(rep))} {
		if bytes.Contains(b, []byte("SECRET-VALUE")) || bytes.Contains(b, []byte("MaxTimeMSExpired")) {
			t.Fatalf("error messages and unparsed lines must not be copied: %s", b)
		}
	}
	if bytes.Contains(repJSON, []byte("x1")) {
		t.Fatalf("the report must not hold literal values: %s", repJSON)
	}
	if fi, _ := os.Stat(out); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", fi.Mode().Perm())
	}
}

func TestConvertProfileArray(t *testing.T) {
	rep, _, ops, _ := convert(t, Options{}, fixture(t, "profile.json"))
	want := []opSummary{
		{"find", 1, 0, "", true},
		{"deleteMany", 2, 7000, "", true},
		{"updateMany", 1, 12000, "", true},
		{"insert", 2, 32000, "", true},
		{"aggregate", 2, 41000, "server_generated_values", true},
	}
	if got := summarize(ops); !reflect.DeepEqual(got, want) {
		t.Fatalf("ops:\n got %+v\nwant %+v", got, want)
	}
	if rep.Entries != 7 || rep.SkippedByReason[SkipGetMore] != 1 || rep.SkippedByReason[SkipTruncated] != 1 || !reflect.DeepEqual(rep.Sources, []string{SourceProfile}) {
		t.Fatalf("report = %+v", rep)
	}
	upd := decode(t, ops[2])
	if upd[0].Key != "filter" || upd[1].Key != "update" || upd[2].Value != false {
		t.Fatalf("profiler update statement = %v", upd)
	}
	doc := decode(t, ops[3])[0].Value.(bson.D)
	if _, ok := doc[2].Value.(bson.Decimal128); !ok {
		t.Fatalf("decimal type = %T", doc[2].Value)
	}
}

func TestConvertRawCommandsNeedADatabase(t *testing.T) {
	rep, _, ops, _ := convert(t, Options{DefaultDatabase: "appdb"}, fixture(t, "commands.ndjson"))
	if len(ops) != 2 || ops[0].NS != "appdb.users" || ops[1].NS != "other.users" || ops[0].Worker != 1 || ops[1].Worker != 1 {
		t.Fatalf("ops = %+v", summarize(ops))
	}
	if ops[0].Flags[0] != workloadrec.FlagNoTimestamp || ops[0].OffsetUS != 0 || rep.NonOperations != 1 || rep.Entries != 3 {
		t.Fatalf("ops %+v report %+v", summarize(ops), rep)
	}

	out := filepath.Join(t.TempDir(), "x.ndjson")
	rep, err := Convert([]Input{fixture(t, "commands.ndjson")}, out, Options{Normalize: mongo.ImportedOperation})
	if err != nil || rep.SkippedByReason[SkipNoNamespace] != 1 || rep.Converted != 1 {
		t.Fatalf("without a default database: %v %+v", err, rep)
	}
}

func TestConvertWorkersAndSkipOptions(t *testing.T) {
	rep, _, ops, info := convert(t, Options{Workers: 1, SkipServerGenerated: true, Compression: workloadrec.CompressionZstd}, fixture(t, "mongod.log"))
	for _, op := range ops {
		if op.Worker != 1 || strings.Contains(strings.Join(op.Flags, ","), workloadrec.FlagServerGenerated) {
			t.Fatalf("op = %+v", op)
		}
	}
	if len(ops) != 6 || rep.SkippedByReason[SkipServerGenerated] != 2 || !rep.FoldedConnections || info.Workers != 1 || info.Compression != workloadrec.CompressionZstd {
		t.Fatalf("report %+v info %+v", rep, info)
	}
}

func TestConvertMergesFilesInStartOrder(t *testing.T) {
	_, _, ops, info := convert(t, Options{}, fixture(t, "profile.json"), fixture(t, "mongod.log"))
	if len(ops) != 13 || ops[0].Op != "aggregate" || ops[len(ops)-1].Op != "aggregate" || info.Workers != 4 {
		t.Fatalf("merged = %+v workers %d", summarize(ops), info.Workers)
	}
	if !reflect.DeepEqual(info.Header.Import.Sources, []string{SourceMongodLog, SourceProfile}) || len(info.Header.Import.Files) != 2 {
		t.Fatalf("import = %+v", info.Header.Import)
	}
}

func TestConvertNothingWritesNoFile(t *testing.T) {
	out := filepath.Join(t.TempDir(), "x.ndjson")
	in := Input{Name: "x.log", R: strings.NewReader(`{"t":{"$date":"2026-10-16T10:00:00Z"},"ctx":"listener","msg":"Connection accepted","attr":{"connectionId":1}}` + "\n")}
	rep, err := Convert([]Input{in}, out, Options{Normalize: mongo.ImportedOperation})
	if !errors.Is(err, ErrNoOperations) || rep.NonOperations != 1 {
		t.Fatalf("err %v report %+v", err, rep)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("no file must be written: %v", err)
	}
}

func TestConvertRefusesToOverwriteOtherFiles(t *testing.T) {
	out := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(out, []byte("keep me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Convert([]Input{fixture(t, "mongod.log")}, out, Options{Normalize: mongo.ImportedOperation})
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(out); string(b) != "keep me\n" {
		t.Fatalf("file changed: %q", b)
	}
}

func TestConvertValidatesOptions(t *testing.T) {
	out := filepath.Join(t.TempDir(), "x.ndjson")
	for _, opts := range []Options{{}, {Normalize: mongo.ImportedOperation, Workers: -1}, {Normalize: mongo.ImportedOperation, Compression: "lz4"}} {
		if _, err := Convert(nil, out, opts); err == nil {
			t.Fatalf("Convert(%+v) must fail", opts)
		}
	}
}
