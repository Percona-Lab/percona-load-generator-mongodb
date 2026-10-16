package mongo

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/trafficimport"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/workloadrec"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func importFixture(t *testing.T) string {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "trafficimport", "testdata", "mongod.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := filepath.Join(t.TempDir(), "imported.ndjson")
	if _, err := trafficimport.Convert([]trafficimport.Input{{Name: "mongod.log", R: f}}, out, trafficimport.Options{Normalize: ImportedOperation}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestReplayOfImportedTraffic(t *testing.T) {
	path := importFixture(t)
	var mu sync.Mutex
	got := map[int][]string{}
	var finds []*opRequest
	// Decode and re-encode exactly as executeReplayOp does, without a server.
	withReplayExec(t, func(ctx context.Context, wCfg workloadConfig, workerID int, op *workloadrec.OpLine, cols replayCollections, out *workloadrec.Outcome) (bool, []byte, error) {
		cmd, err := workloadrec.DecodeCommand(op.Cmd)
		if err != nil {
			return false, nil, err
		}
		req, err := decodeStandard(op, cmd, cols.lookup(op.NS))
		if err != nil {
			return false, nil, err
		}
		sent, err := workloadrec.EncodeCommand(standardCommand(req))
		mu.Lock()
		got[workerID] = append(got[workerID], op.Op)
		if op.Op == "find" {
			finds = append(finds, req)
		}
		mu.Unlock()
		return true, sent, err
	})
	noShardingSeams(t, func(ctx context.Context, wCfg workloadConfig) error {
		t.Fatal("replay must not generate operations")
		return nil
	})
	cols, queries := recordReplayFixtures()
	cfg := replayCfg(path)
	cfg.DropCollections = true
	collector := stats.NewCollector()
	if err := RunWorkload(context.Background(), nil, cols, queries, cfg, collector); err != nil {
		t.Fatalf("an imported recording replays without matching definitions: %v", err)
	}
	want := map[int][]string{
		1: {"aggregate", "updateOne", "updateMany", "find"},
		2: {"find", "insertMany", "deleteOne", "updateOne"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("per-worker order:\n got %v\nwant %v", got, want)
	}
	rep := collector.WorkloadReplay()
	if rep.Status != workloadrec.StatusComplete || rep.OpsReplayed != 8 || len(rep.Mismatches) != 0 {
		t.Fatalf("replay = %+v", rep)
	}
	if rep.CommandsHashMatch == nil || !*rep.CommandsHashMatch {
		t.Fatalf("the replay must send exactly the imported commands: %+v", rep)
	}
	if !reflect.DeepEqual(rep.ImportedFrom, []string{trafficimport.SourceMongodLog}) {
		t.Fatalf("imported_from = %v", rep.ImportedFrom)
	}
	warn := strings.Join(rep.Warnings, "\n")
	for _, w := range []string{"imported from captured traffic", "drop_collections or seeding was on", "server-generated values"} {
		if !strings.Contains(warn, w) {
			t.Fatalf("warnings miss %q: %s", w, warn)
		}
	}
	var sorted *opRequest
	for _, r := range finds {
		if r.findSort != nil {
			sorted = r
		}
	}
	if sorted == nil || sorted.findSkip != 2 || sorted.findBatch != 5 || sorted.col.DatabaseName != "shop" || sorted.col.Name != "orders" {
		t.Fatalf("imported find = %+v", sorted)
	}
	if s := sorted.findSort.(bson.D); s[0].Key != "ts" || s[1].Key != "_id" {
		t.Fatalf("sort order = %v", s)
	}
}

func TestImportedOperationKeepsGeneratedCommandsUnchanged(t *testing.T) {
	// A generated find has no sort/skip/hint: its recorded form is unchanged.
	req := &opRequest{opType: "find", filter: map[string]interface{}{"a": 1}, findLimit: 5, findBatch: 10}
	got, _ := workloadrec.EncodeCommand(standardCommand(req))
	if want := `{"filter":{"a":{"$numberInt":"1"}},"limit":{"$numberLong":"5"},"batchSize":{"$numberInt":"10"}}`; string(got) != want {
		t.Fatalf("generated find = %s", got)
	}
	agg := &opRequest{opType: "aggregate", pipeline: bson.A{}}
	if got, _ := workloadrec.EncodeCommand(standardCommand(agg)); string(got) != `{"pipeline":[]}` {
		t.Fatalf("generated aggregate = %s", got)
	}

	cmd, shape, err := ImportedOperation("db.c", "find", bson.D{{Key: "filter", Value: bson.D{}}, {Key: "hint", Value: "a_1"}, {Key: "limit", Value: int64(0)}, {Key: "batchSize", Value: int64(0)}})
	if err != nil || shape == nil || !strings.HasPrefix(shape.Key, "find|") {
		t.Fatalf("ImportedOperation = %v %+v %v", cmd, shape, err)
	}
	if _, _, err := ImportedOperation("db.c", "find", bson.D{{Key: "filter", Value: bson.D{}}, {Key: "skip", Value: int64(-1)}, {Key: "limit", Value: int64(0)}, {Key: "batchSize", Value: int64(0)}}); err == nil {
		t.Fatal("a negative skip must be rejected")
	}
	if _, _, err := ImportedOperation("db.c", "count", bson.D{}); err == nil {
		t.Fatal("an unsupported operation must be rejected")
	}
}
