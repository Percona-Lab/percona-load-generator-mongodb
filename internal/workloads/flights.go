package workloads

import (
	"math/rand"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/datagen"
	"github.com/brianvoe/gofakeit/v6"
)

// flightFieldSignals are field names / provider names that indicate a
// flight/airline-style schema and trigger the consistent flight generator.
var flightFieldSignals = map[string]struct{}{
	"passengers":      {},
	"equipment":       {},
	"seats_available": {},
	"flight_code":     {},
	"gate":            {},
}

// isFlightSchema reports whether the collection should use the consistent
// flight generator. It triggers for the "flights" collection whenever the
// schema references flight-specific fields or providers, independent of the
// default_workload flag, so selecting the built-in definition from the library
// produces the same realistic data as the default workload mode.
func isFlightSchema(col config.CollectionDefinition) bool {
	if col.Name != "flights" {
		return false
	}
	for name, def := range col.Fields {
		if _, ok := flightFieldSignals[name]; ok {
			return true
		}
		if _, ok := flightFieldSignals[def.Provider]; ok {
			return true
		}
	}
	return false
}

// randomEquipment produces an equipment object (test/back-compat helper).
func randomEquipment(rng *rand.Rand) map[string]interface{} {
	equip, _ := datagen.RandomEquipment(gofakeit.New(rng.Int63()))
	return equip
}

// randomPassengers creates a list of passengers with unique seat assignments
// (test/back-compat helper delegating to the shared generator).
func randomPassengers(totalSeats int, seatsAvailable int, rng *rand.Rand) []map[string]interface{} {
	return datagen.RandomPassengers(gofakeit.New(rng.Int63()), totalSeats, seatsAvailable)
}

// GenerateDefaultDocument produces a realistic, internally-consistent flight
// document using the collection definition.
func GenerateDefaultDocument(col config.CollectionDefinition) map[string]interface{} {
	faker := datagen.NewFaker()
	rng := faker.Rand
	doc := make(map[string]interface{})

	if len(col.Fields) > 0 {
		var totalSeats int
		var seatsAvailable int
		hasSeatsAvailable := false

		for fname, fdef := range col.Fields {
			switch fname {
			case "equipment":
				equip, capacity := datagen.RandomEquipment(faker)
				doc[fname] = equip
				totalSeats = capacity
			case "seats_available":
				hasSeatsAvailable = true
				// Resolved after equipment so it can be bounded by capacity.
			case "passengers":
				// Filled at the end once seat counts are known.
			default:
				// Everything else (flight_code, gate, origin, agent_*, dates,
				// bounded ints, ...) flows through the schema-driven generator,
				// which now understands the domain providers.
				doc[fname] = datagen.RandomValueWithFaker(fdef, faker)
			}
		}

		if totalSeats <= 0 {
			_, totalSeats = datagen.RandomEquipment(faker)
		}

		if hasSeatsAvailable {
			seatsAvailable = rng.Intn(totalSeats + 1)
			doc["seats_available"] = seatsAvailable
		}

		// Logical consistency: origin and destination must differ.
		if origin, ok := doc["origin"].(string); ok {
			if dest, ok := doc["destination"].(string); ok {
				for origin == dest {
					dest = faker.City()
				}
				doc["destination"] = dest
			}
		}

		if _, ok := col.Fields["passengers"]; ok {
			doc["passengers"] = datagen.RandomPassengers(faker, totalSeats, seatsAvailable)
		}
		return doc
	}

	// Fallback if no schema is provided.
	equip, totalSeats := datagen.RandomEquipment(faker)
	doc["flight_id"] = rng.Intn(900000) + 100000
	doc["flight_code"] = datagen.RandomFlightCode(faker)
	doc["origin"] = faker.City()
	dest := faker.City()
	for dest == doc["origin"] {
		dest = faker.City()
	}
	doc["destination"] = dest
	doc["gate"] = datagen.RandomGate(faker)
	doc["duration_minutes"] = rng.Intn(871) + 30
	seatsAvailable := rng.Intn(totalSeats + 1)
	doc["seats_available"] = seatsAvailable
	doc["equipment"] = equip
	doc["passengers"] = datagen.RandomPassengers(faker, totalSeats, seatsAvailable)
	return doc
}

// GenerateDefaultUpdate returns a MongoDB update document specific to the
// flights workload, using realistic bounded values.
func GenerateDefaultUpdate(rng *rand.Rand) map[string]interface{} {
	if rng.Intn(2) == 0 {
		return map[string]interface{}{
			"$inc": map[string]interface{}{"seats_available": rng.Intn(5) + 1},
		}
	}
	return map[string]interface{}{
		"$set": map[string]interface{}{"duration_minutes": rng.Intn(871) + 30},
	}
}
