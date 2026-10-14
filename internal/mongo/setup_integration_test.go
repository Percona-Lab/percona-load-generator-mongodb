//go:build integration
// +build integration

package mongo

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"go.mongodb.org/mongo-driver/v2/bson"
	driverMongo "go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func listIndexSpecs(t *testing.T, coll *driverMongo.Collection) []bson.D {
	t.Helper()
	cur, err := coll.Indexes().List(context.Background())
	if err != nil {
		t.Fatalf("listIndexes: %v", err)
	}
	var specs []bson.D
	if err := cur.All(context.Background(), &specs); err != nil {
		t.Fatalf("decode indexes: %v", err)
	}
	return specs
}

func indexNames(specs []bson.D) []string {
	var names []string
	for _, s := range specs {
		for _, e := range s {
			if e.Key == "name" {
				names = append(names, fmt.Sprint(e.Value))
			}
		}
	}
	return names
}

func TestIntegrationCreateIndexesFromConfigIsIdempotent(t *testing.T) {
	client := connectIntegrationMongoOrSkip(t)
	defer client.Disconnect(context.Background())

	dbName := fmt.Sprintf("plgm_it_s9_%d", time.Now().UnixNano())
	defer client.Database(dbName).Drop(context.Background())

	cfg, err := config.ParseCollectionsBytes([]byte(`[{"database":"` + dbName + `","collection":"flights","fields":{},"indexes":[
		{"keys":{"flight_id":1,"equipment.plane_type":1}},
		{"keys":{"flight_id":1,"duration_minutes":1,"seats_available":1}},
		{"keys":{"origin":1,"destination":1,"flight_date":-1}},
		{"keys":{"b":1,"a":1}},
		{"keys":{"status":1,"created":-1},"name":"by_status","unique":true},
		{"keys":{"created":1},"expireAfterSeconds":3600},
		{"keys":{"sku":1},"partialFilterExpression":{"qty":{"$gt":5}}}]}]`))
	if err != nil {
		t.Fatal(err)
	}
	if err := config.ValidateCollectionDefinitions(cfg.Collections); err != nil {
		t.Fatal(err)
	}
	db := client.Database(dbName)
	coll := db.Collection("flights")

	var first []bson.D
	for run := 1; run <= 5; run++ {
		if err := CreateIndexesFromConfig(context.Background(), db, cfg); err != nil {
			if isAuthErr(err) {
				skipIntegration(t, "skipping: no privileges to create indexes: %v", err)
			}
			t.Fatalf("run %d: %v", run, err)
		}
		specs := listIndexSpecs(t, coll)
		if run == 1 {
			first = specs
			want := []string{"_id_", "flight_id_1_equipment.plane_type_1", "flight_id_1_duration_minutes_1_seats_available_1",
				"origin_1_destination_1_flight_date_-1", "b_1_a_1", "by_status", "created_1", "sku_1"}
			if got := indexNames(specs); !reflect.DeepEqual(got, want) {
				t.Fatalf("index names = %v, want %v", got, want)
			}
			continue
		}
		if !reflect.DeepEqual(specs, first) {
			t.Fatalf("run %d changed the index inventory:\nfirst: %v\nnow:   %v", run, first, specs)
		}
	}

	for _, s := range first {
		m := bson.M{}
		for _, e := range s {
			m[e.Key] = e.Value
		}
		switch m["name"] {
		case "origin_1_destination_1_flight_date_-1":
			key := m["key"].(bson.D)
			if key[0].Key != "origin" || key[1].Key != "destination" || key[2].Key != "flight_date" {
				t.Fatalf("server key order = %v", key)
			}
		case "by_status":
			if m["unique"] != true {
				t.Fatalf("unique option lost: %v", s)
			}
		case "created_1":
			if fmt.Sprint(m["expireAfterSeconds"]) != "3600" {
				t.Fatalf("ttl option lost: %v", s)
			}
		case "sku_1":
			if m["partialFilterExpression"] == nil {
				t.Fatalf("partial filter lost: %v", s)
			}
		}
	}
}

func TestIntegrationReorderedLeftoverIsKeptAndReported(t *testing.T) {
	client := connectIntegrationMongoOrSkip(t)
	defer client.Disconnect(context.Background())

	dbName := fmt.Sprintf("plgm_it_s9l_%d", time.Now().UnixNano())
	defer client.Database(dbName).Drop(context.Background())
	db := client.Database(dbName)
	coll := db.Collection("c")
	if _, err := coll.Indexes().CreateOne(context.Background(), driverMongo.IndexModel{Keys: bson.D{{Key: "b", Value: 1}, {Key: "a", Value: 1}}}); err != nil {
		if isAuthErr(err) {
			skipIntegration(t, "skipping: no privileges to create indexes: %v", err)
		}
		t.Fatal(err)
	}
	cfg, err := config.ParseCollectionsBytes([]byte(`[{"database":"` + dbName + `","collection":"c","fields":{},"indexes":[{"keys":{"a":1,"b":1}}]}]`))
	if err != nil {
		t.Fatal(err)
	}
	for run := 0; run < 2; run++ {
		if err := CreateIndexesFromConfig(context.Background(), db, cfg); err != nil {
			t.Fatal(err)
		}
	}
	specs := listIndexSpecs(t, coll)
	if got := indexNames(specs); !reflect.DeepEqual(got, []string{"_id_", "b_1_a_1", "a_1_b_1"}) {
		t.Fatalf("indexes = %v (the leftover must not be dropped and no extra index may be added)", got)
	}
	var existing []existingIndex
	for _, s := range specs {
		raw, _ := bson.Marshal(s)
		var e existingIndex
		_ = bson.Unmarshal(raw, &e)
		existing = append(existing, e)
	}
	if got := findReorderedIndexes(cfg.Collections[0].Indexes, existing); len(got) != 1 || got[0].Existing != "b_1_a_1" || got[0].Configured != "a_1_b_1" {
		t.Fatalf("reordered = %+v", got)
	}
}

func TestIntegrationIncompatibleExistingIndexIsReported(t *testing.T) {
	client := connectIntegrationMongoOrSkip(t)
	defer client.Disconnect(context.Background())

	tests := []struct {
		name     string
		existing driverMongo.IndexModel
		config   string
	}{
		{
			name:     "same_name_different_options",
			existing: driverMongo.IndexModel{Keys: bson.D{{Key: "a", Value: 1}}, Options: options.Index().SetName("by_a")},
			config:   `{"keys":{"a":1},"name":"by_a","unique":true}`,
		},
		{
			name:     "same_keys_different_name",
			existing: driverMongo.IndexModel{Keys: bson.D{{Key: "a", Value: 1}, {Key: "b", Value: 1}}, Options: options.Index().SetName("custom")},
			config:   `{"keys":{"a":1,"b":1}}`,
		},
		{
			name:     "same_name_different_keys",
			existing: driverMongo.IndexModel{Keys: bson.D{{Key: "b", Value: 1}, {Key: "a", Value: 1}}, Options: options.Index().SetName("a_1_b_1")},
			config:   `{"keys":{"a":1,"b":1}}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dbName := fmt.Sprintf("plgm_it_s9c_%d", time.Now().UnixNano())
			defer client.Database(dbName).Drop(context.Background())
			db := client.Database(dbName)
			if _, err := db.Collection("c").Indexes().CreateOne(context.Background(), tc.existing); err != nil {
				if isAuthErr(err) {
					skipIntegration(t, "skipping: no privileges to create indexes: %v", err)
				}
				t.Fatal(err)
			}
			before := listIndexSpecs(t, db.Collection("c"))
			cfg, err := config.ParseCollectionsBytes([]byte(`[{"database":"` + dbName + `","collection":"c","fields":{},"indexes":[` + tc.config + `]}]`))
			if err != nil {
				t.Fatal(err)
			}
			err = CreateIndexesFromConfig(context.Background(), db, cfg)
			if err == nil || !strings.Contains(err.Error(), "create indexes on "+dbName+".c") {
				t.Fatalf("expected the conflict to be reported, got %v", err)
			}
			if after := listIndexSpecs(t, db.Collection("c")); !reflect.DeepEqual(after, before) {
				t.Fatalf("existing indexes changed: %v -> %v", before, after)
			}
		})
	}
}
