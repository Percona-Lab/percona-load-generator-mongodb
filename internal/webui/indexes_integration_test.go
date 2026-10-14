//go:build integration
// +build integration

package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	driverMongo "go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func TestListIndexesReadsDeployedDefinitions(t *testing.T) {
	uri := os.Getenv("PLGM_IT_MONGO_URI")
	if uri == "" {
		uri = "mongodb://127.0.0.1:30777"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := driverMongo.Connect(options.Client().ApplyURI(uri).SetServerSelectionTimeout(2 * time.Second))
	if err != nil {
		skipIntegration(t, "skipping integration test: unable to create mongo client: %v", err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		_ = client.Disconnect(context.Background())
		skipIntegration(t, "skipping integration test: mongo is not reachable at %s: %v", uri, err)
	}
	database := client.Database(fmt.Sprintf("plgm_it_indexes_%d", time.Now().UnixNano()))
	t.Cleanup(func() {
		_ = database.Drop(context.Background())
		_ = client.Disconnect(context.Background())
	})
	coll := database.Collection("advice_meta")
	if _, err := coll.InsertOne(ctx, bson.D{{Key: "_id", Value: 1}, {Key: "status", Value: "open"}, {Key: "qty", Value: 3}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	hidden := true
	models := []driverMongo.IndexModel{
		{Keys: bson.D{{Key: "status", Value: int32(1)}, {Key: "qty", Value: int32(-1)}}},
		{Keys: bson.D{{Key: "sku", Value: int32(1)}}, Options: options.Index().SetPartialFilterExpression(bson.D{{Key: "status", Value: "open-secret"}})},
		{Keys: bson.D{{Key: "legacy", Value: int32(1)}}, Options: options.Index().SetSparse(true)},
		{Keys: bson.D{{Key: "region", Value: int32(1)}}, Options: options.Index().SetHidden(hidden).SetCollation(&options.Collation{Locale: "en"})},
	}
	if _, err := coll.Indexes().CreateMany(ctx, models); err != nil {
		t.Fatalf("create indexes: %v", err)
	}

	inv := listCollectionIndexes(ctx, client, database.Name(), "advice_meta", database.Name()+".advice_meta")
	if inv.Status != "fetched" || inv.Reason != "" {
		t.Fatalf("inventory %+v", inv)
	}
	byName := map[string]int{}
	for i, idx := range inv.Indexes {
		byName[idx.Name] = i
	}
	compound, ok := byName["status_1_qty_-1"]
	if !ok {
		t.Fatalf("missing compound index in %+v", inv.Indexes)
	}
	keys := inv.Indexes[compound].Keys
	if len(keys) != 2 || keys[0].Field != "status" || keys[0].Direction != 1 || keys[1].Field != "qty" || keys[1].Direction != -1 {
		t.Fatalf("compound key order %+v", keys)
	}
	sku := inv.Indexes[byName["sku_1"]]
	if sku.Sparse || !sku.Partial || sku.PartialSummary != "status:$eq" {
		t.Fatalf("partial index %+v", sku)
	}
	if legacy := inv.Indexes[byName["legacy_1"]]; !legacy.Sparse || legacy.Partial {
		t.Fatalf("sparse index %+v", legacy)
	}
	if body, _ := json.Marshal(inv); strings.Contains(string(body), "open-secret") {
		t.Fatalf("serialized inventory leaked a partial-filter literal: %s", body)
	}
	region := inv.Indexes[byName["region_1"]]
	if !region.Hidden || region.Collation != "en" || region.Multikey != nil {
		t.Fatalf("hidden collated index %+v", region)
	}
}
