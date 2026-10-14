package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func parseOneCollection(t *testing.T, indexesJSON string) CollectionDefinition {
	t.Helper()
	cfg, err := ParseCollectionsBytes([]byte(`[{"database":"d","collection":"c","fields":{},"indexes":` + indexesJSON + `}]`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := ValidateCollectionDefinitions(cfg.Collections); err != nil {
		t.Fatalf("validate: %v", err)
	}
	return cfg.Collections[0]
}

func TestIndexKeysPreserveDeclaredOrderAndDirection(t *testing.T) {
	want := bson.D{{Key: "origin", Value: int32(1)}, {Key: "destination", Value: int32(1)}, {Key: "flight_date", Value: int32(-1)}}
	for i := 0; i < 100; i++ {
		col := parseOneCollection(t, `[{"keys":{"origin":1,"destination":1,"flight_date":-1}}]`)
		if got := col.Indexes[0].Keys.BSON(); !reflect.DeepEqual(got, want) {
			t.Fatalf("iteration %d: keys = %v, want %v", i, got, want)
		}
		if got := col.Indexes[0].EffectiveName(); got != "origin_1_destination_1_flight_date_-1" {
			t.Fatalf("name = %q", got)
		}
	}
}

func TestIndexKeysNotAlphabetized(t *testing.T) {
	col := parseOneCollection(t, `[{"keys":{"z":1,"a":-1,"m":"hashed"}}]`)
	if got := col.Indexes[0].Keys.Fields(); !reflect.DeepEqual(got, []string{"z", "a", "m"}) {
		t.Fatalf("fields = %v", got)
	}
	if got := col.Indexes[0].EffectiveName(); got != "z_1_a_-1_m_hashed" {
		t.Fatalf("name = %q", got)
	}
}

func TestIndexKeysArrayFormEquivalentToObjectForm(t *testing.T) {
	obj := parseOneCollection(t, `[{"keys":{"b":1,"a":-1}}]`)
	arr := parseOneCollection(t, `[{"keys":[{"b":1},{"a":-1}]}]`)
	if !reflect.DeepEqual(obj.Indexes[0].Keys.BSON(), arr.Indexes[0].Keys.BSON()) {
		t.Fatalf("object %v != array %v", obj.Indexes[0].Keys.BSON(), arr.Indexes[0].Keys.BSON())
	}
	if obj.Indexes[0].EffectiveName() != arr.Indexes[0].EffectiveName() {
		t.Fatalf("names differ")
	}
}

func TestIndexKeysJSONRoundTripKeepsOrder(t *testing.T) {
	col := parseOneCollection(t, `[{"keys":{"z":1,"a":-1},"name":"za","unique":true,"partialFilterExpression":{"z":{"$gt":0},"a":{"$exists":true}}}]`)
	b, err := json.Marshal(col.Indexes)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"keys":{"z":1,"a":-1}`) {
		t.Fatalf("marshalled keys lost order: %s", b)
	}
	again := parseOneCollection(t, string(b))
	if !reflect.DeepEqual(again.Indexes[0].Keys, col.Indexes[0].Keys) || again.Indexes[0].Name != "za" || !again.Indexes[0].Unique {
		t.Fatalf("round trip mismatch: %+v vs %+v", again.Indexes[0], col.Indexes[0])
	}
}

func TestIndexSupportedOptions(t *testing.T) {
	col := parseOneCollection(t, `[{"keys":{"status":1,"created":-1},"name":"by_status","unique":true,"sparse":false,"hidden":true},
		{"keys":{"created":1},"expireAfterSeconds":3600},
		{"keys":{"sku":1},"partialFilterExpression":{"qty":{"$gt":5},"active":true}}]`)
	a, b, c := col.Indexes[0], col.Indexes[1], col.Indexes[2]
	if a.EffectiveName() != "by_status" || !a.Unique || a.Sparse || !a.Hidden {
		t.Fatalf("options a: %+v", a)
	}
	if b.ExpireAfterSeconds == nil || *b.ExpireAfterSeconds != 3600 {
		t.Fatalf("ttl: %+v", b)
	}
	pfe, err := c.PartialFilterDoc()
	if err != nil {
		t.Fatal(err)
	}
	if len(pfe) != 2 || pfe[0].Key != "qty" || pfe[1].Key != "active" {
		t.Fatalf("partial filter order: %v", pfe)
	}
}

func TestDefaultCollectionsKeepDeclaredIndexOrder(t *testing.T) {
	cfg, err := LoadCollections(filepath.Join("..", "..", "resources", "collections", "default.json"), true)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, idx := range cfg.Collections[0].Indexes {
		names = append(names, idx.EffectiveName())
	}
	want := []string{
		"flight_id_1_equipment.plane_type_1",
		"flight_id_1_seats_available_1",
		"flight_id_1_duration_minutes_1_seats_available_1",
		"equipment.plane_type_1",
		"origin_1_destination_1_flight_date_-1",
		"status_1",
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
}

func TestInvalidIndexDefinitionsReported(t *testing.T) {
	tests := []struct {
		name, indexes, want string
	}{
		{"duplicate_field_object", `[{"keys":{"a":1,"a":-1}}]`, `index key "a" is declared more than once`},
		{"duplicate_field_array", `[{"keys":[{"a":1},{"a":1}]}]`, `declared more than once`},
		{"array_entry_multi_field", `[{"keys":[{"a":1,"b":1}]}]`, "exactly one field"},
		{"array_entry_not_object", `[{"keys":["a"]}]`, "single-field objects"},
		{"keys_not_object", `[{"keys":"a"}]`, "JSON object or an array"},
		{"zero_direction", `[{"keys":{"a":0}}]`, "unsupported direction"},
		{"fractional_direction", `[{"keys":{"a":1.5}}]`, "unsupported direction"},
		{"unknown_type", `[{"keys":{"a":"geoHaystack"}}]`, "unsupported index type"},
		{"bool_value", `[{"keys":{"a":true}}]`, "unsupported key value"},
		{"unknown_option", `[{"keys":{"a":1},"collation":{"locale":"en"}}]`, "unsupported index option(s) collation"},
		{"typo_option", `[{"keys":{"a":1},"uniq":true}]`, "unsupported index option(s) uniq"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseCollectionsBytes([]byte(`[{"database":"d","collection":"c","fields":{},"indexes":` + tc.indexes + `}]`))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestAmbiguousIndexDefinitionsRejectedByValidation(t *testing.T) {
	tests := []struct {
		name, indexes, want string
	}{
		{"empty_keys", `[{"keys":{}}]`, "index keys cannot be empty"},
		{"same_keys_twice", `[{"keys":{"a":1,"b":1}},{"keys":{"a":1,"b":1},"unique":true}]`, "same keys"},
		{"same_name_different_keys", `[{"keys":{"a":1},"name":"x"},{"keys":{"b":1},"name":"x"}]`, `same name "x"`},
		{"explicit_name_collides_with_generated", `[{"keys":{"a":1}},{"keys":{"b":1},"name":"a_1"}]`, `same name "a_1"`},
		{"dollar_field", `[{"keys":{"$a":1}}]`, "cannot start with '$'"},
		{"negative_ttl", `[{"keys":{"a":1},"expireAfterSeconds":-1}]`, "expireAfterSeconds"},
		{"partial_not_object", `[{"keys":{"a":1},"partialFilterExpression":5}]`, "partialFilterExpression"},
		{"partial_empty", `[{"keys":{"a":1},"partialFilterExpression":{}}]`, "must not be empty"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseCollectionsBytes([]byte(`[{"database":"d","collection":"c","fields":{},"indexes":` + tc.indexes + `}]`))
			if err == nil {
				err = ValidateCollectionDefinitions(cfg.Collections)
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestSameFieldsInDifferentOrderAreDistinctIndexes(t *testing.T) {
	col := parseOneCollection(t, `[{"keys":{"a":1,"b":1}},{"keys":{"b":1,"a":1}}]`)
	if col.Indexes[0].EffectiveName() == col.Indexes[1].EffectiveName() {
		t.Fatalf("expected distinct names")
	}
}

func TestShardKeyValidation(t *testing.T) {
	tests := []struct {
		name, key, want string
	}{
		{"descending_rejected", `{"a":-1}`, `value must be 1 or "hashed"`},
		{"two_hashed", `{"a":"hashed","b":"hashed"}`, "at most one hashed"},
		{"text_rejected", `{"a":"text"}`, `value must be 1 or "hashed"`},
		{"duplicate", `{"a":1,"a":1}`, "declared more than once"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseCollectionsBytes([]byte(`[{"database":"d","collection":"c","fields":{},"shardConfig":{"key":` + tc.key + `}}]`))
			if err == nil {
				err = ValidateCollectionDefinitions(cfg.Collections)
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
	cfg, err := ParseCollectionsBytes([]byte(`[{"database":"d","collection":"c","fields":{},"shardConfig":{"key":{"region":1,"id":"hashed"}}}]`))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateCollectionDefinitions(cfg.Collections); err != nil {
		t.Fatal(err)
	}
	if got := cfg.Collections[0].ShardConfig.Key.Fields(); !reflect.DeepEqual(got, []string{"region", "id"}) {
		t.Fatalf("shard key order = %v", got)
	}
}

func TestLoadCollectionsSurfacesIndexErrors(t *testing.T) {
	file := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(file, []byte(`{"collections":[{"database":"d","collection":"c","fields":{},"indexes":[{"keys":{"a":1},"collation":{}}]}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadCollections(file, false)
	if err == nil || !strings.Contains(err.Error(), "unsupported index option(s) collation") {
		t.Fatalf("expected explicit option error, got %v", err)
	}
}

func TestProgrammaticIndexKeysNormalized(t *testing.T) {
	cols := []CollectionDefinition{{DatabaseName: "d", Name: "c", Indexes: []IndexDefinition{{Keys: IndexKeys{{Field: "b", Value: 1}, {Field: "a", Value: float64(-1)}}}}}}
	if err := ValidateCollectionDefinitions(cols); err != nil {
		t.Fatal(err)
	}
	want := bson.D{{Key: "b", Value: int32(1)}, {Key: "a", Value: int32(-1)}}
	if got := cols[0].Indexes[0].Keys.BSON(); !reflect.DeepEqual(got, want) {
		t.Fatalf("keys = %v", got)
	}
}
