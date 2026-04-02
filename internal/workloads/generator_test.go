package workloads

import (
	"math/rand"
	"testing"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
)

func TestGenerateDocumentSelectsWorkloadPath(t *testing.T) {
	cfgDefault := &config.AppConfig{DefaultWorkload: true}
	colFlights := config.CollectionDefinition{
		Name: "flights",
		Fields: map[string]config.CollectionField{
			"origin":          {Type: "string"},
			"destination":     {Type: "string"},
			"seats_available": {Type: "int"},
			"equipment":       {Type: "object"},
			"passengers":      {Type: "array"},
		},
	}
	doc := GenerateDocument(colFlights, cfgDefault)
	if _, ok := doc["passengers"]; !ok {
		t.Fatalf("expected default flights document to include passengers")
	}

	cfgGeneric := &config.AppConfig{DefaultWorkload: false}
	colGeneric := config.CollectionDefinition{
		Name: "users",
		Fields: map[string]config.CollectionField{
			"name": {Type: "string"},
			"age":  {Type: "int"},
		},
	}
	genericDoc := GenerateDocument(colGeneric, cfgGeneric)
	if len(genericDoc) != 2 {
		t.Fatalf("expected generic document with 2 fields, got %d", len(genericDoc))
	}
}

func TestGenerateFallbackUpdate(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	flights := config.CollectionDefinition{Name: "flights"}
	cfgDefault := &config.AppConfig{DefaultWorkload: true}
	update := GenerateFallbackUpdate(flights, cfgDefault, rng)
	if _, hasInc := update["$inc"]; !hasInc {
		if _, hasSet := update["$set"]; !hasSet {
			t.Fatalf("expected default flights update to contain $inc or $set")
		}
	}

	cfgGeneric := &config.AppConfig{DefaultWorkload: false}
	genericCol := config.CollectionDefinition{Name: "users", Fields: map[string]config.CollectionField{"name": {Type: "string"}}}
	genericUpdate := GenerateFallbackUpdate(genericCol, cfgGeneric, rng)
	setPart, ok := genericUpdate["$set"].(map[string]interface{})
	if !ok || len(setPart) != 1 {
		t.Fatalf("expected generic fallback update with single $set field, got %#v", genericUpdate)
	}
}

func TestGenerateGenericUpdateEmptyFields(t *testing.T) {
	rng := rand.New(rand.NewSource(9))
	update := generateGenericUpdate(config.CollectionDefinition{}, rng)
	setPart, ok := update["$set"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected $set map, got %#v", update)
	}
	if _, ok := setPart["updated_at"]; !ok {
		t.Fatalf("expected updated_at in fallback update")
	}
}

func TestRandomPassengersSeatUniqueness(t *testing.T) {
	rng := rand.New(rand.NewSource(99))
	passengers := randomPassengers(30, 20, rng)
	if len(passengers) != 10 {
		t.Fatalf("expected 10 passengers, got %d", len(passengers))
	}

	seenSeats := make(map[string]bool)
	for _, p := range passengers {
		seat, _ := p["seat_number"].(string)
		if seat == "" {
			t.Fatalf("expected seat number")
		}
		if seenSeats[seat] {
			t.Fatalf("duplicate seat assignment detected: %s", seat)
		}
		seenSeats[seat] = true
	}
}
