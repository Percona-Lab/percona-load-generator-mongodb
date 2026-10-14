package mongo

import (
	"reflect"
	"testing"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func parseIndexes(t *testing.T, indexesJSON string) []config.IndexDefinition {
	t.Helper()
	cfg, err := config.ParseCollectionsBytes([]byte(`[{"database":"d","collection":"c","fields":{},"indexes":` + indexesJSON + `}]`))
	if err != nil {
		t.Fatal(err)
	}
	if err := config.ValidateCollectionDefinitions(cfg.Collections); err != nil {
		t.Fatal(err)
	}
	return cfg.Collections[0].Indexes
}

func resolvedIndexOptions(t *testing.T, b *options.IndexOptionsBuilder) options.IndexOptions {
	t.Helper()
	var o options.IndexOptions
	for _, set := range b.List() {
		if err := set(&o); err != nil {
			t.Fatal(err)
		}
	}
	return o
}

func TestBuildIndexModelsDeterministicKeysAndNames(t *testing.T) {
	const indexes = `[{"keys":{"origin":1,"destination":1,"flight_date":-1}},{"keys":{"b":1,"a":1}},{"keys":{"a":1,"b":1}}]`
	wantKeys := []bson.D{
		{{Key: "origin", Value: int32(1)}, {Key: "destination", Value: int32(1)}, {Key: "flight_date", Value: int32(-1)}},
		{{Key: "b", Value: int32(1)}, {Key: "a", Value: int32(1)}},
		{{Key: "a", Value: int32(1)}, {Key: "b", Value: int32(1)}},
	}
	wantNames := []string{"origin_1_destination_1_flight_date_-1", "b_1_a_1", "a_1_b_1"}
	for run := 0; run < 50; run++ {
		models, err := buildIndexModels(parseIndexes(t, indexes))
		if err != nil {
			t.Fatal(err)
		}
		for i, m := range models {
			if !reflect.DeepEqual(m.Keys, wantKeys[i]) {
				t.Fatalf("run %d index %d keys = %v, want %v", run, i, m.Keys, wantKeys[i])
			}
			raw, err := bson.Marshal(m.Keys)
			if err != nil {
				t.Fatal(err)
			}
			elems, _ := bson.Raw(raw).Elements()
			for j, e := range elems {
				if e.Key() != wantKeys[i][j].Key {
					t.Fatalf("encoded BSON key %d = %q, want %q", j, e.Key(), wantKeys[i][j].Key)
				}
			}
			if o := resolvedIndexOptions(t, m.Options); o.Name == nil || *o.Name != wantNames[i] {
				t.Fatalf("run %d index %d name = %v, want %s", run, i, o.Name, wantNames[i])
			}
		}
	}
}

func TestBuildIndexModelsPassesSupportedOptions(t *testing.T) {
	models, err := buildIndexModels(parseIndexes(t, `[
		{"keys":{"status":1,"created":-1},"name":"by_status","unique":true,"sparse":true,"hidden":true},
		{"keys":{"created":1},"expireAfterSeconds":60},
		{"keys":{"sku":1},"partialFilterExpression":{"qty":{"$gt":5},"active":true}}]`))
	if err != nil {
		t.Fatal(err)
	}
	a := resolvedIndexOptions(t, models[0].Options)
	if *a.Name != "by_status" || a.Unique == nil || !*a.Unique || a.Sparse == nil || !*a.Sparse || a.Hidden == nil || !*a.Hidden {
		t.Fatalf("options = %+v", a)
	}
	b := resolvedIndexOptions(t, models[1].Options)
	if b.ExpireAfterSeconds == nil || *b.ExpireAfterSeconds != 60 || b.Unique != nil {
		t.Fatalf("ttl options = %+v", b)
	}
	c := resolvedIndexOptions(t, models[2].Options)
	pfe, ok := c.PartialFilterExpression.(bson.D)
	if !ok || len(pfe) != 2 || pfe[0].Key != "qty" || pfe[1].Key != "active" {
		t.Fatalf("partial filter = %#v", c.PartialFilterExpression)
	}
}

func TestFindReorderedIndexes(t *testing.T) {
	configured := parseIndexes(t, `[{"keys":{"a":1,"b":1}},{"keys":{"x":1,"y":-1,"z":1}},{"keys":{"p":1,"q":1}},{"keys":{"q":1,"p":1}}]`)
	existing := []existingIndex{
		{Name: "_id_", Key: bson.D{{Key: "_id", Value: int32(1)}}},
		{Name: "a_1_b_1", Key: bson.D{{Key: "a", Value: int32(1)}, {Key: "b", Value: int32(1)}}},
		{Name: "b_1_a_1", Key: bson.D{{Key: "b", Value: int32(1)}, {Key: "a", Value: float64(1)}}},
		{Name: "z_1_x_1_y_-1", Key: bson.D{{Key: "z", Value: int64(1)}, {Key: "x", Value: int32(1)}, {Key: "y", Value: int32(-1)}}},
		{Name: "b_-1_a_1", Key: bson.D{{Key: "b", Value: int32(-1)}, {Key: "a", Value: int32(1)}}},
		{Name: "p_1_q_1", Key: bson.D{{Key: "p", Value: int32(1)}, {Key: "q", Value: int32(1)}}},
		{Name: "q_1_p_1", Key: bson.D{{Key: "q", Value: int32(1)}, {Key: "p", Value: int32(1)}}},
		{Name: "user_c_1_d_1", Key: bson.D{{Key: "c", Value: int32(1)}, {Key: "d", Value: int32(1)}}},
	}
	got := findReorderedIndexes(configured, existing)
	want := []reorderedIndex{
		{Existing: "b_1_a_1", Configured: "a_1_b_1"},
		{Existing: "z_1_x_1_y_-1", Configured: "x_1_y_-1_z_1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reordered = %+v, want %+v", got, want)
	}
}
