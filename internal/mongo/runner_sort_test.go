package mongo

import (
	"bytes"
	"math/rand"
	"strconv"
	"strings"
	"testing"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func loadSortQuery(t *testing.T, pipeline string) config.QueryDefinition {
	t.Helper()
	qf, err := config.ParseQueriesBytes([]byte(`[{"name":"q","collection":"c","operation":"aggregate","pipeline":` + pipeline + `}]`))
	if err != nil {
		t.Fatal(err)
	}
	if err := config.NormalizeAndValidateQueries(qf.Queries); err != nil {
		t.Fatal(err)
	}
	return qf.Queries[0]
}

// executedPipeline prepares the pipeline the way the worker loop does.
func executedPipeline(q config.QueryDefinition) []interface{} {
	pipeline := deepClone(q.Pipeline).([]interface{})
	processRecursiveWithPool(pipeline, rand.New(rand.NewSource(1)), nil, "db.c", 0)
	return pipeline
}

func sortKeyOrder(t *testing.T, pipeline []interface{}, stage int) string {
	t.Helper()
	raw, err := bson.Marshal(bson.D{{Key: "p", Value: pipeline}})
	if err != nil {
		t.Fatal(err)
	}
	elems, err := bson.Raw(raw).Lookup("p", strconv.Itoa(stage), "$sort").Document().Elements()
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(elems))
	for _, e := range elems {
		keys = append(keys, e.Key()+":"+e.Value().String())
	}
	return strings.Join(keys, ",")
}

func TestExecutedAndCapturedPipelinesKeepDeclaredSort(t *testing.T) {
	q := loadSortQuery(t, `[{"$match":{"status":"<string>"}},{"$sort":{"b":-1,"a":1}}]`)
	pipeline := executedPipeline(q)
	if got := sortKeyOrder(t, pipeline, 1); got != `b:{"$numberInt":"-1"},a:{"$numberInt":"1"}` {
		t.Fatalf("executed sort = %s", got)
	}
	exec := buildExecutableQuery("aggregate", "db", q, nil, pipeline, 0)
	sent, _ := bson.Marshal(bson.D{{Key: "pipeline", Value: pipeline}})
	captured, _ := bson.Marshal(bson.D{{Key: "pipeline", Value: exec.Pipeline}})
	if !bytes.Equal(sent, captured) {
		t.Fatalf("captured pipeline differs from executed:\n%s\n%s", bson.Raw(sent), bson.Raw(captured))
	}
	// Cloning for the next operation must not share or reorder the spec.
	again := executedPipeline(q)
	if sortKeyOrder(t, again, 1) != sortKeyOrder(t, pipeline, 1) {
		t.Fatal("repeated preparation changed sort order")
	}
	again[1].(map[string]interface{})["$sort"].(bson.D)[0].Value = int32(1)
	if sortKeyOrder(t, pipeline, 1) != `b:{"$numberInt":"-1"},a:{"$numberInt":"1"}` {
		t.Fatal("deepClone must copy sort specifications")
	}
}

func TestShapeIdentityFollowsSortPrecedence(t *testing.T) {
	shape := func(pipeline string) string {
		q := loadSortQuery(t, pipeline)
		key, _, _ := buildOperationShape("aggregate", nil, executedPipeline(q), shapeOptions{})
		return key
	}
	ab := shape(`[{"$sort":{"a":1,"b":-1}}]`)
	for i := 0; i < 50; i++ {
		if got := shape(`[{"$sort":{"a":1,"b":-1}}]`); got != ab {
			t.Fatalf("identical sort produced different shape keys: %s vs %s", got, ab)
		}
	}
	if !strings.Contains(ab, "$sort:{a:1,b:-1}") {
		t.Fatalf("shape key must render declared order: %s", ab)
	}
	ba := shape(`[{"$sort":{"b":-1,"a":1}}]`)
	if ba == ab {
		t.Fatalf("different precedence must give different shapes: %s", ab)
	}
	if dir := shape(`[{"$sort":{"a":-1,"b":-1}}]`); dir == ab {
		t.Fatal("different direction must give a different shape")
	}
	if stats.StableShapeID("aggregate", "c", ab) == stats.StableShapeID("aggregate", "c", ba) {
		t.Fatal("shape IDs must differ for different precedence")
	}

	w1 := shape(`[{"$setWindowFields":{"sortBy":{"t":1,"s":-1},"output":{"r":{"$rank":{}}}}}]`)
	w2 := shape(`[{"$setWindowFields":{"sortBy":{"s":-1,"t":1},"output":{"r":{"$rank":{}}}}}]`)
	if w1 == w2 || !strings.Contains(w1, "sortBy:{t:1,s:-1}") {
		t.Fatalf("sortBy precedence must be part of the shape: %s / %s", w1, w2)
	}
	a1 := shape(`[{"$project":{"x":{"$sortArray":{"input":"$x","sortBy":1}}}}]`)
	a2 := shape(`[{"$project":{"x":{"$sortArray":{"input":"$x","sortBy":-1}}}}]`)
	if a1 == a2 {
		t.Fatalf("$sortArray direction must be part of the shape: %s", a1)
	}
}

func TestPipelinePreviewKeepsSortOrder(t *testing.T) {
	q := loadSortQuery(t, `[{"$sort":{"b":-1,"a":1}}]`)
	if got := summarizePipelinePreview(executedPipeline(q), 256); got != "[{$sort:{b:<num>,a:<num>}}]" {
		t.Fatalf("preview = %s", got)
	}
}
