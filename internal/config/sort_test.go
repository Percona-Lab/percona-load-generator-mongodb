package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func parseOneQuery(t *testing.T, js string) QueryDefinition {
	t.Helper()
	qf, err := ParseQueriesBytes([]byte(js))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := NormalizeAndValidateQueries(qf.Queries); err != nil {
		t.Fatalf("validate: %v", err)
	}
	return qf.Queries[0]
}

func stageValue(t *testing.T, stage interface{}, name string) interface{} {
	t.Helper()
	m, ok := stage.(map[string]interface{})
	if !ok {
		t.Fatalf("stage is %T, want map", stage)
	}
	return m[name]
}

func TestSortSpecKeepsDeclaredOrderAndDirection(t *testing.T) {
	q := parseOneQuery(t, `[{"collection":"c","operation":"aggregate","pipeline":[
		{"$match":{"x":1}},
		{"$sort":{"zeta":1,"alpha":-1,"mid":1.0}}
	]}]`)
	got := stageValue(t, q.Pipeline[1], "$sort")
	want := bson.D{{Key: "zeta", Value: int32(1)}, {Key: "alpha", Value: int32(-1)}, {Key: "mid", Value: int32(1)}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("$sort = %#v, want %#v", got, want)
	}
	// Non-sort numbers keep the encoding/json float64 decoding.
	if v := stageValue(t, q.Pipeline[0], "$match").(map[string]interface{})["x"]; v != float64(1) {
		t.Fatalf("$match literal = %#v, want float64(1)", v)
	}
}

func TestSortSpecBSONEncodingIsDeterministic(t *testing.T) {
	q := parseOneQuery(t, `[{"collection":"c","operation":"aggregate","pipeline":[{"$sort":{"b":1,"a":-1}}]}]`)
	first, err := bson.Marshal(bson.D{{Key: "pipeline", Value: q.Pipeline}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		b, _ := bson.Marshal(bson.D{{Key: "pipeline", Value: q.Pipeline}})
		if !bytes.Equal(b, first) {
			t.Fatalf("encoding %d differs: %s vs %s", i, bson.Raw(b), bson.Raw(first))
		}
	}
	var keys []string
	elems, _ := bson.Raw(first).Lookup("pipeline", "0", "$sort").Document().Elements()
	for _, e := range elems {
		keys = append(keys, e.Key())
	}
	if strings.Join(keys, ",") != "b,a" {
		t.Fatalf("encoded sort keys %v, want b,a", keys)
	}
}

func TestSortSpecSupportedForms(t *testing.T) {
	q := parseOneQuery(t, `[{"collection":"c","operation":"aggregate","pipeline":[
		{"$facet":{"f":[{"$sort":{"y":1,"x":1}}]}},
		{"$lookup":{"from":"o","as":"j","pipeline":[{"$sort":{"q":-1,"p":1}}]}},
		{"$unionWith":{"coll":"u","pipeline":[{"$sort":{"n":1,"m":-1}}]}},
		{"$setWindowFields":{"sortBy":{"t":1,"s":-1},"output":{"r":{"$rank":{}}}}},
		{"$group":{"_id":null,"top":{"$topN":{"n":2,"sortBy":{"k":-1,"j":1},"output":"$k"}}}},
		{"$project":{"arr":{"$sortArray":{"input":"$arr","sortBy":{"w":1,"v":-1}}},"nums":{"$sortArray":{"input":"$nums","sortBy":-1}}}},
		{"$sort":{"score":{"$meta":"textScore"},"b":1}}
	]}]`)
	facet := stageValue(t, q.Pipeline[0], "$facet").(map[string]interface{})["f"].([]interface{})
	lookup := stageValue(t, q.Pipeline[1], "$lookup").(map[string]interface{})["pipeline"].([]interface{})
	union := stageValue(t, q.Pipeline[2], "$unionWith").(map[string]interface{})["pipeline"].([]interface{})
	window := stageValue(t, q.Pipeline[3], "$setWindowFields").(map[string]interface{})["sortBy"]
	topN := stageValue(t, q.Pipeline[4], "$group").(map[string]interface{})["top"].(map[string]interface{})["$topN"].(map[string]interface{})["sortBy"]
	proj := stageValue(t, q.Pipeline[5], "$project").(map[string]interface{})
	sortArr := proj["arr"].(map[string]interface{})["$sortArray"].(map[string]interface{})["sortBy"]
	scalarArr := proj["nums"].(map[string]interface{})["$sortArray"].(map[string]interface{})["sortBy"]
	meta := stageValue(t, q.Pipeline[6], "$sort")

	cases := []struct {
		name string
		got  interface{}
		want bson.D
	}{
		{"facet", stageValue(t, facet[0], "$sort"), bson.D{{Key: "y", Value: int32(1)}, {Key: "x", Value: int32(1)}}},
		{"lookup", stageValue(t, lookup[0], "$sort"), bson.D{{Key: "q", Value: int32(-1)}, {Key: "p", Value: int32(1)}}},
		{"unionWith", stageValue(t, union[0], "$sort"), bson.D{{Key: "n", Value: int32(1)}, {Key: "m", Value: int32(-1)}}},
		{"setWindowFields", window, bson.D{{Key: "t", Value: int32(1)}, {Key: "s", Value: int32(-1)}}},
		{"topN", topN, bson.D{{Key: "k", Value: int32(-1)}, {Key: "j", Value: int32(1)}}},
		{"sortArray", sortArr, bson.D{{Key: "w", Value: int32(1)}, {Key: "v", Value: int32(-1)}}},
		{"meta", meta, bson.D{{Key: "score", Value: bson.D{{Key: "$meta", Value: "textScore"}}}, {Key: "b", Value: int32(1)}}},
	}
	for _, c := range cases {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s: got %#v, want %#v", c.name, c.got, c.want)
		}
	}
	if scalarArr != int32(-1) {
		t.Errorf("$sortArray scalar sortBy = %#v, want int32(-1)", scalarArr)
	}
}

func TestSortSpecInUpdatePushModifier(t *testing.T) {
	q := parseOneQuery(t, `[{"collection":"c","operation":"updateOne","filter":{"_id":1},
		"update":{"$push":{"scores":{"$each":[{"s":1,"t":2}],"$sort":{"t":-1,"s":1}},"tags":{"$each":["x"],"$sort":1}}}}]`)
	push := q.Update["$push"].(map[string]interface{})
	got := push["scores"].(map[string]interface{})["$sort"]
	want := bson.D{{Key: "t", Value: int32(-1)}, {Key: "s", Value: int32(1)}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("$push $sort = %#v, want %#v", got, want)
	}
	if v := push["tags"].(map[string]interface{})["$sort"]; v != int32(1) {
		t.Fatalf("scalar $push $sort = %#v, want int32(1)", v)
	}
	// $each elements are documents, not sort specifications.
	each := push["scores"].(map[string]interface{})["$each"].([]interface{})
	if _, ok := each[0].(map[string]interface{}); !ok {
		t.Fatalf("$each element must stay a map, got %T", each[0])
	}
}

func TestSortSpecJSONRoundTripKeepsOrder(t *testing.T) {
	src := `{"queries":[{"name":"q","collection":"c","operation":"aggregate","pipeline":[{"$match":{"s":"<string>"}},{"$sort":{"b":-1,"a":1,"c":{"$meta":"searchScore"}}}]}]}`
	qf, err := ParseQueriesBytes([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if err := NormalizeAndValidateQueries(qf.Queries); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(qf); err != nil {
		t.Fatal(err)
	}
	out := buf.Bytes()
	if !strings.Contains(string(out), `"$sort":{"b":-1,"a":1,"c":{"$meta":"searchScore"}}`) {
		t.Fatalf("marshaled sort lost order: %s", out)
	}
	if !strings.Contains(string(out), `<string>`) {
		t.Fatalf("placeholders must not be HTML-escaped: %s", out)
	}
	again, err := ParseQueriesBytes(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := NormalizeAndValidateQueries(again.Queries); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again.Queries[0].Pipeline, qf.Queries[0].Pipeline) {
		t.Fatalf("round trip changed pipeline:\n%#v\n%#v", again.Queries[0].Pipeline, qf.Queries[0].Pipeline)
	}
}

func TestLoadQueriesFromFileKeepsSortOrder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "q.json")
	if err := os.WriteFile(path, []byte(`[{"collection":"c","operation":"aggregate","pipeline":[{"$sort":{"b":1,"a":1}}]}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	qf, err := LoadQueries(path, false)
	if err != nil {
		t.Fatal(err)
	}
	got := stageValue(t, qf.Queries[0].Pipeline[0], "$sort")
	if !reflect.DeepEqual(got, bson.D{{Key: "b", Value: int32(1)}, {Key: "a", Value: int32(1)}}) {
		t.Fatalf("loaded sort = %#v", got)
	}
}

func TestEmbeddedDefaultQueriesSortsAreOrdered(t *testing.T) {
	qf, err := loadEmbeddedQuery("queries/default.json")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, q := range qf.Queries {
		for _, st := range q.Pipeline {
			if v, ok := st.(map[string]interface{})["$sort"]; ok {
				seen++
				if _, ok := v.(bson.D); !ok {
					t.Fatalf("%s: $sort is %T, want bson.D", q.Name, v)
				}
			}
		}
	}
	if seen == 0 {
		t.Fatal("expected default queries to contain $sort stages")
	}
}

func TestSortSpecRejectsMalformedAndAmbiguous(t *testing.T) {
	cases := []struct {
		name, pipeline, want string
	}{
		{"duplicate key", `[{"$sort":{"a":1,"b":1,"a":-1}}]`, `"a" is declared more than once`},
		{"empty", `[{"$sort":{}}]`, "at least one key"},
		{"bad direction", `[{"$sort":{"a":2}}]`, "unsupported sort value"},
		{"string direction", `[{"$sort":{"a":"asc"}}]`, "unsupported sort value"},
		{"bad meta", `[{"$sort":{"a":{"$meta":"indexKey"}}}]`, "unsupported sort value"},
		{"operator key", `[{"$sort":{"$a":1}}]`, "cannot start with '$'"},
		{"empty key", `[{"$sort":{"":1}}]`, "cannot be empty"},
		{"scalar stage", `[{"$sort":1}]`, "must be an object"},
		{"array stage", `[{"$sort":[{"a":1}]}]`, "must be an object"},
		{"window duplicate", `[{"$setWindowFields":{"sortBy":{"t":1,"t":1},"output":{}}}]`, `"t" is declared more than once`},
		{"nested facet bad", `[{"$facet":{"f":[{"$sort":{"a":0}}]}}]`, "pipeline[0].$facet.f[0].$sort"},
		{"pipeline not array", `{"$sort":{"a":1}}`, "must be an array of stages"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			js := `[{"name":"bad","collection":"c","operation":"aggregate","pipeline":` + c.pipeline + `}]`
			qf, err := ParseQueriesBytes([]byte(js))
			if err == nil {
				err = NormalizeAndValidateQueries(qf.Queries)
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got error %v, want it to contain %q", err, c.want)
			}
		})
	}
}

func TestSortSpecRejectsUnorderedMultiKeyMap(t *testing.T) {
	defs := []QueryDefinition{{Collection: "c", Operation: "aggregate", Pipeline: []interface{}{
		map[string]interface{}{"$sort": map[string]interface{}{"a": 1, "b": -1}},
	}}}
	err := NormalizeAndValidateQueries(defs)
	if err == nil || !strings.Contains(err.Error(), "precedence is ambiguous") {
		t.Fatalf("multi-key map sort must be reported as ambiguous, got %v", err)
	}
	upd := []QueryDefinition{{Collection: "c", Operation: "updateOne", Filter: map[string]interface{}{}, Update: map[string]interface{}{
		"$push": map[string]interface{}{"x": map[string]interface{}{"$each": []interface{}{}, "$sort": map[string]interface{}{"a": 1, "b": 1}}},
	}}}
	if err := NormalizeAndValidateQueries(upd); err == nil || !strings.Contains(err.Error(), "precedence is ambiguous") {
		t.Fatalf("multi-key map $push $sort must be ambiguous, got %v", err)
	}

	single := []QueryDefinition{{Collection: "c", Operation: "aggregate", Pipeline: []interface{}{
		map[string]interface{}{"$sort": map[string]interface{}{"a": -1}},
	}}}
	if err := NormalizeAndValidateQueries(single); err != nil {
		t.Fatalf("single-key map sort is unambiguous: %v", err)
	}
	if got := single[0].Pipeline[0].(map[string]interface{})["$sort"]; !reflect.DeepEqual(got, bson.D{{Key: "a", Value: int32(-1)}}) {
		t.Fatalf("single-key map sort normalized to %#v", got)
	}
	ordered := []QueryDefinition{{Collection: "c", Operation: "aggregate", Pipeline: []interface{}{
		map[string]interface{}{"$sort": bson.D{{Key: "b", Value: 1}, {Key: "a", Value: -1}}},
	}}}
	if err := NormalizeAndValidateQueries(ordered); err != nil {
		t.Fatalf("bson.D sort is ordered: %v", err)
	}
	if got := ordered[0].Pipeline[0].(map[string]interface{})["$sort"]; !reflect.DeepEqual(got, bson.D{{Key: "b", Value: int32(1)}, {Key: "a", Value: int32(-1)}}) {
		t.Fatalf("bson.D sort normalized to %#v", got)
	}
}

func TestHasMultiKeySortSpec(t *testing.T) {
	q := parseOneQuery(t, `[{"collection":"c","operation":"aggregate","pipeline":[{"$facet":{"f":[{"$sort":{"b":1,"a":1}}]}}]}]`)
	if !HasMultiKeySortSpec(q.Pipeline) {
		t.Fatal("nested multi-key sort not detected")
	}
	one := parseOneQuery(t, `[{"collection":"c","operation":"aggregate","pipeline":[{"$sort":{"a":1}},{"$match":{"b":1,"c":2}}]}]`)
	if HasMultiKeySortSpec(one.Pipeline) {
		t.Fatal("single-key sort and multi-key filters are not multi-key sorts")
	}
}
