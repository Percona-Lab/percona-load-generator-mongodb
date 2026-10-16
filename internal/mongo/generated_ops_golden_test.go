package mongo

import (
	"bytes"
	"flag"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/datagen"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/workloadrec"
	"go.mongodb.org/mongo-driver/v2/bson"
)

var updateGeneratedGolden = flag.Bool("update-generated-golden", false, "rewrite testdata/generated_ops_seeded.golden from the current generators")

func goldenCollections() []config.CollectionDefinition {
	return []config.CollectionDefinition{{DatabaseName: "appdb", Name: "items", Fields: map[string]config.CollectionField{
		"item_id": {Type: "long"},
		"a":       {Type: "int"},
		"name":    {Type: "string", Provider: "first_name"},
		"at":      {Type: "date"},
		"ref":     {Type: "objectid"},
	}}}
}

func goldenQueries() []config.QueryDefinition {
	return []config.QueryDefinition{
		{Name: "by_a", Database: "appdb", Collection: "items", Operation: "find", Filter: map[string]interface{}{"a": "<int>"}, DefinitionIndex: 0},
		{Name: "by_name", Database: "appdb", Collection: "items", Operation: "find", Filter: map[string]interface{}{"name": "<string>"}, Limit: 5, DefinitionIndex: 1},
		{Name: "set_a", Database: "appdb", Collection: "items", Operation: "updateOne", Filter: map[string]interface{}{"name": "<string>"}, Update: map[string]interface{}{"$set": map[string]interface{}{"a": 5}}, DefinitionIndex: 2},
		{Name: "set_many", Database: "appdb", Collection: "items", Operation: "updateMany", Filter: map[string]interface{}{"a": "<int>"}, Update: map[string]interface{}{"$inc": map[string]interface{}{"a": 1}}, DefinitionIndex: 3},
		{Name: "agg", Database: "appdb", Collection: "items", Operation: "aggregate", Pipeline: []interface{}{map[string]interface{}{"$match": map[string]interface{}{"a": "<int>"}}, map[string]interface{}{"$limit": 3}}, DefinitionIndex: 4},
	}
}

// generatedOpsSeeded replays the generation steps of the standard worker path
// (independentWorker) for one seeded worker, without a server, and returns
// each command in its recorded form. The prebuilt insert batches of the
// optimized path come first.
func generatedOpsSeeded(t *testing.T) []byte {
	t.Helper()
	cfg := &config.AppConfig{RandomSeed: 42, InsertBatchSize: 3, ExistingRecordHitRate: 50}
	datagen.ConfigureSeed(cfg.RandomSeed)
	t.Cleanup(func() { datagen.ConfigureSeed(0) })
	InsertDocumentCache = nil
	cols := goldenCollections()
	qMap := map[string][]config.QueryDefinition{}
	for _, q := range goldenQueries() {
		qMap[q.Operation] = append(qMap[q.Operation], q)
	}
	percentages := map[string]int{"find": 30, "update": 20, "delete": 10, "insert": 20, "insertMany": 10, "aggregate": 10}

	var out bytes.Buffer
	line := func(kind string, cmd interface{}) {
		b, err := workloadrec.EncodeCommand(cmd)
		if err != nil {
			t.Fatal(err)
		}
		out.WriteString(kind + " " + string(b) + "\n")
	}

	batches, patchKey, err := prepareGenericBatchPool(cols[0], cfg, cfg.InsertBatchSize, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range batches {
		var d bson.D
		if err := bson.Unmarshal(b.CmdRaw, &d); err != nil {
			t.Fatal(err)
		}
		line("fast_batch:"+patchKey, canonicalRawDocs(d))
	}

	pool := NewRecordPool(100)
	ns := collectionNamespace(nil, cols[0])
	rng := rand.New(rand.NewSource(cfg.RandomSeed + 1*1000003))
	for i := 0; i < 300; i++ {
		col := cols[rng.Intn(len(cols))]
		opType := selectOperation(percentages, rng)
		var q config.QueryDefinition
		var docs []interface{}
		run := true
		switch opType {
		case "insert":
			q = generateInsertQuery(col, rng, cfg)
		case "insertMany":
			docs = generateInsertManyQuery(col, rng, cfg)
		default:
			q, run = selectRandomQueryByType(nil, nil, opType, qMap, col, false, rng, "item_id", cfg)
		}
		if !run {
			line("skip", bson.D{{Key: "op", Value: opType}})
			continue
		}
		req := opRequest{opType: opType, col: col, update: q.Update, projection: q.Projection, upsert: q.Upsert, findLimit: 10, findBatch: 10}
		if opType == "aggregate" {
			p, _ := deepClone(q.Pipeline).([]interface{})
			req.usedExisting = processRecursiveWithPool(p, rng, pool, ns, cfg.ExistingRecordHitRate)
			req.pipeline = p
		} else if opType != "insertMany" {
			f := cloneMap(q.Filter)
			req.usedExisting = processRecursiveWithPool(f, rng, pool, ns, cfg.ExistingRecordHitRate)
			req.filter = f
		}
		switch opType {
		case "insert":
			req.doc = q.Filter
			registerInsertedDocument(pool, nil, col, q.Filter)
		case "insertMany":
			req.docs = docs
			registerInsertedDocuments(pool, nil, col, docs)
		}
		kind := opType
		if req.usedExisting {
			kind += ":existing"
		}
		line(kind, standardCommand(&req))
	}
	return out.Bytes()
}

// canonicalRawDocs sorts the fields of each document of a decoded insert
// command; the prebuilt batches marshal Go maps, whose field order varies.
func canonicalRawDocs(cmd bson.D) bson.D {
	out := make(bson.D, len(cmd))
	for i, e := range cmd {
		out[i] = e
		if docs, ok := e.Value.(bson.A); ok {
			sorted := make(bson.A, len(docs))
			for j, d := range docs {
				m := map[string]interface{}{}
				for _, f := range d.(bson.D) {
					m[f.Key] = f.Value
				}
				sorted[j] = workloadrec.Canonical(m)
			}
			out[i].Value = sorted
		}
	}
	return out
}

// TestSeededGenerationMatchesGolden pins what a seeded run without
// workload_deterministic generates, so the deterministic mode's changes to
// the shared generators cannot change the default output.
func TestSeededGenerationMatchesGolden(t *testing.T) {
	path := filepath.Join("testdata", "generated_ops_seeded.golden")
	got := generatedOpsSeeded(t)
	if again := generatedOpsSeeded(t); !bytes.Equal(got, again) {
		t.Fatal("the seeded single-worker generation is not repeatable")
	}
	if *updateGeneratedGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update-generated-golden to create it): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("seeded generation changed; diff the output against %s", path)
	}
}
