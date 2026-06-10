package workloads

import (
	"regexp"
	"testing"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
)

func intPtr(v int) *int { return &v }

func defaultFlightCollection() config.CollectionDefinition {
	return config.CollectionDefinition{
		DatabaseName: "airline",
		Name:         "flights",
		Fields: map[string]config.CollectionField{
			"flight_id":        {Type: "int", Min: intPtr(100000), Max: intPtr(999999)},
			"flight_code":      {Type: "string", Provider: "flight_code"},
			"gate":             {Type: "string", Provider: "gate"},
			"origin":           {Type: "string", Provider: "city"},
			"destination":      {Type: "string", Provider: "city"},
			"status":           {Type: "string", Enum: []interface{}{"scheduled", "boarding", "landed"}},
			"duration_minutes": {Type: "int", Provider: "duration_minutes", Min: intPtr(30), Max: intPtr(900)},
			"seats_available":  {Type: "int", Provider: "seats_available"},
			"equipment":        {Type: "object", Provider: "equip"},
			"passengers":       {Type: "array", Provider: "passengers"},
		},
	}
}

// TestDefaultDocumentIsRealisticAndConsistent verifies the reported issues are
// fixed: no "val" placeholders, bounded numeric fields, and consistent seats.
func TestDefaultDocumentIsRealisticAndConsistent(t *testing.T) {
	gateRe := regexp.MustCompile(`^[A-F][0-9]{1,2}$`)
	flightCodeRe := regexp.MustCompile(`^[A-Z0-9]{2}[0-9]{3}$`)
	col := defaultFlightCollection()

	for i := 0; i < 100; i++ {
		doc := GenerateDefaultDocument(col)

		gate, _ := doc["gate"].(string)
		if !gateRe.MatchString(gate) {
			t.Fatalf("gate %q is not realistic", gate)
		}
		code, _ := doc["flight_code"].(string)
		if !flightCodeRe.MatchString(code) {
			t.Fatalf("flight_code %q is not realistic", code)
		}

		dur, ok := doc["duration_minutes"].(int)
		if !ok || dur < 30 || dur > 900 {
			t.Fatalf("duration_minutes out of range: %v", doc["duration_minutes"])
		}

		fid, ok := doc["flight_id"].(int32)
		if !ok || fid < 100000 || fid > 999999 {
			t.Fatalf("flight_id out of expected range: %v", doc["flight_id"])
		}

		equip, ok := doc["equipment"].(map[string]interface{})
		if !ok {
			t.Fatalf("expected equipment object, got %T", doc["equipment"])
		}
		totalSeats, _ := equip["total_seats"].(int)
		if totalSeats <= 0 {
			t.Fatalf("expected positive total_seats, got %v", equip["total_seats"])
		}
		seats, ok := doc["seats_available"].(int)
		if !ok || seats < 0 || seats > totalSeats {
			t.Fatalf("seats_available %v inconsistent with total_seats %d", doc["seats_available"], totalSeats)
		}

		passengers, ok := doc["passengers"].([]map[string]interface{})
		if !ok || len(passengers) == 0 {
			t.Fatalf("expected passenger list, got %T", doc["passengers"])
		}
		if len(passengers) > 25 {
			t.Fatalf("expected bounded passenger detail (<=25), got %d", len(passengers))
		}
		seenSeats := map[string]bool{}
		for _, p := range passengers {
			seat, _ := p["seat_number"].(string)
			if seat == "" || seenSeats[seat] {
				t.Fatalf("invalid or duplicate seat assignment: %q", seat)
			}
			seenSeats[seat] = true
		}

		status, _ := doc["status"].(string)
		if status != "scheduled" && status != "boarding" && status != "landed" {
			t.Fatalf("unexpected status enum value %q", status)
		}
	}
}

// TestGenerateDocumentRealisticRegardlessOfDefaultWorkloadFlag is the core
// regression guard: selecting the built-in flight schema (DefaultWorkload=false)
// must still yield realistic data instead of the generic placeholder values.
func TestGenerateDocumentRealisticRegardlessOfDefaultWorkloadFlag(t *testing.T) {
	col := defaultFlightCollection()
	cfg := &config.AppConfig{DefaultWorkload: false}

	for i := 0; i < 50; i++ {
		doc := GenerateDocument(col, cfg)
		if doc["gate"] == "val" || doc["flight_code"] == "val" {
			t.Fatalf("got placeholder 'val' in flight document: %#v", doc)
		}
		if _, ok := doc["passengers"].([]map[string]interface{}); !ok {
			t.Fatalf("expected structured passengers, got %T", doc["passengers"])
		}
	}
}
