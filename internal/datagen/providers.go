package datagen

import (
	"fmt"
	"sync/atomic"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/brianvoe/gofakeit/v6"
)

// ---------------------------------------------------------------------------
// Deterministic seeding support
//
// When a non-zero seed is configured (via config.random_seed / PLGM_RANDOM_SEED)
// NextSeed returns a reproducible sequence of seeds. Otherwise it falls back to
// time-based seeding, preserving the previous non-deterministic behavior.
// ---------------------------------------------------------------------------

var (
	seedConfigured int32
	seedBase       int64
	seedCounter    int64
)

// ConfigureSeed sets (or clears, when seed == 0) the deterministic seed base
// and resets the internal counter so the next generation run is reproducible.
func ConfigureSeed(seed int64) {
	if seed == 0 {
		atomic.StoreInt32(&seedConfigured, 0)
		return
	}
	atomic.StoreInt64(&seedBase, seed)
	atomic.StoreInt64(&seedCounter, 0)
	atomic.StoreInt32(&seedConfigured, 1)
}

// NextSeed returns the next seed for a fresh RNG/Faker instance.
func NextSeed() int64 {
	if atomic.LoadInt32(&seedConfigured) == 1 {
		return atomic.LoadInt64(&seedBase) + atomic.AddInt64(&seedCounter, 1)
	}
	return time.Now().UnixNano()
}

// NewFaker creates a Faker honoring the configured seed (if any).
func NewFaker() *gofakeit.Faker {
	return gofakeit.New(NextSeed())
}

// ---------------------------------------------------------------------------
// Domain data sets used by the built-in flight/airline workload. These are kept
// here (rather than in the workloads package) so both the schema-driven generic
// generator and the flight-specific generator share a single source of truth.
// ---------------------------------------------------------------------------

// AirlineCodes are realistic two-letter IATA carrier codes.
var AirlineCodes = []string{"AA", "DL", "UA", "WN", "AS", "B6", "NK", "F9", "HA", "G4"}

// AirportCodes are realistic three-letter IATA airport codes.
var AirportCodes = []string{
	"ATL", "LAX", "ORD", "DFW", "DEN", "JFK", "SFO", "SEA", "LAS", "MCO",
	"EWR", "MIA", "PHX", "IAH", "BOS", "MSP", "DTW", "FLL", "PHL", "LGA",
	"CLT", "BWI", "SLC", "SAN", "DCA", "TPA", "PDX", "STL", "HNL", "AUS",
}

// PlaneTypes are realistic commercial aircraft models.
var PlaneTypes = []string{
	"Boeing 737-800", "Airbus A320", "Embraer E190", "Bombardier CRJ900",
	"Boeing 777-300ER", "Airbus A350-900", "Boeing 787-9", "Airbus A321neo",
}

// planeSeatCapacity maps an aircraft model to a realistic total seat count.
var planeSeatCapacity = map[string]int{
	"Boeing 737-800":    162,
	"Airbus A320":       150,
	"Embraer E190":      100,
	"Bombardier CRJ900": 76,
	"Boeing 777-300ER":  396,
	"Airbus A350-900":   325,
	"Boeing 787-9":      290,
	"Airbus A321neo":    194,
}

// Amenities are realistic onboard amenity labels.
var Amenities = []string{"WiFi", "Seatback TV", "Power outlets", "Hot meals", "Priority boarding", "Extra legroom", "USB charging"}

// maxDetailedPassengers caps how many fully-detailed passenger sub-documents are
// generated per flight. Large wide-body aircraft can seat hundreds of people;
// generating that many sub-documents per seed document would bloat the dataset
// and slow seeding considerably, so we emit a representative bounded sample.
const maxDetailedPassengers = 25

// PlaneSeatCapacity returns a realistic seat count for the given aircraft model,
// defaulting to a narrow-body capacity for unknown models.
func PlaneSeatCapacity(planeType string) int {
	if c, ok := planeSeatCapacity[planeType]; ok {
		return c
	}
	return 150
}

// ---------------------------------------------------------------------------
// Scalar domain generators
// ---------------------------------------------------------------------------

// RandomFlightCode returns an airline-style flight code such as "DL482".
func RandomFlightCode(f *gofakeit.Faker) string {
	carrier := AirlineCodes[f.Rand.Intn(len(AirlineCodes))]
	return fmt.Sprintf("%s%d", carrier, f.Rand.Intn(900)+100)
}

// RandomGate returns a realistic gate identifier such as "B7" or "A12".
func RandomGate(f *gofakeit.Faker) string {
	letter := rune('A' + f.Rand.Intn(6))
	return fmt.Sprintf("%c%d", letter, f.Rand.Intn(50)+1)
}

// RandomAirportCode returns a realistic three-letter IATA airport code.
func RandomAirportCode(f *gofakeit.Faker) string {
	return AirportCodes[f.Rand.Intn(len(AirportCodes))]
}

// boundedInt resolves an integer honoring optional schema Min/Max overrides and
// the provided realistic defaults.
func boundedInt(f *gofakeit.Faker, def config.CollectionField, defMin, defMax int) int {
	min, max := defMin, defMax
	if def.Min != nil {
		min = *def.Min
	}
	if def.Max != nil {
		max = *def.Max
	}
	if max < min {
		min, max = max, min
	}
	span := max - min + 1
	if span <= 0 {
		return min
	}
	return f.Rand.Intn(span) + min
}

// RandomEquipment builds a realistic aircraft/equipment sub-document and returns
// the document along with the aircraft's total seat capacity.
func RandomEquipment(f *gofakeit.Faker) (map[string]interface{}, int) {
	planeType := PlaneTypes[f.Rand.Intn(len(PlaneTypes))]
	capacity := PlaneSeatCapacity(planeType)

	numAmenities := f.Rand.Intn(4) + 2
	perm := f.Rand.Perm(len(Amenities))
	picked := make([]string, 0, numAmenities)
	for i := 0; i < numAmenities && i < len(perm); i++ {
		picked = append(picked, Amenities[perm[i]])
	}

	return map[string]interface{}{
		"plane_type":  planeType,
		"total_seats": capacity,
		"amenities":   picked,
	}, capacity
}

// RandomPassengers builds a bounded list of passenger sub-documents with unique
// seat assignments. The number of detailed entries is min(booked, cap).
func RandomPassengers(f *gofakeit.Faker, totalSeats, seatsAvailable int) []map[string]interface{} {
	if totalSeats <= 0 {
		totalSeats = 150
	}
	booked := totalSeats - seatsAvailable
	if booked < 1 {
		booked = 1
	}
	if booked > totalSeats {
		booked = totalSeats
	}
	count := booked
	if count > maxDetailedPassengers {
		count = maxDetailedPassengers
	}

	// Build a deck of unique seats (rows 1..N, columns A..F) and shuffle it.
	seatLetters := []string{"A", "B", "C", "D", "E", "F"}
	allSeats := make([]string, 0, totalSeats)
	row := 1
	for len(allSeats) < totalSeats {
		for _, letter := range seatLetters {
			if len(allSeats) >= totalSeats {
				break
			}
			allSeats = append(allSeats, fmt.Sprintf("%d%s", row, letter))
		}
		row++
	}
	f.Rand.Shuffle(len(allSeats), func(i, j int) { allSeats[i], allSeats[j] = allSeats[j], allSeats[i] })

	passengers := make([]map[string]interface{}, count)
	for i := 0; i < count; i++ {
		passengers[i] = map[string]interface{}{
			"passenger_id":  i + 1,
			"name":          fmt.Sprintf("%s %s", f.FirstName(), f.LastName()),
			"ticket_number": fmt.Sprintf("TCK-%08d", f.Rand.Intn(99999999)+1),
			"seat_number":   allSeats[i],
		}
	}
	return passengers
}

// ---------------------------------------------------------------------------
// Provider registry
// ---------------------------------------------------------------------------

type providerFunc func(f *gofakeit.Faker, def config.CollectionField) interface{}

// domainProviders maps schema provider names to realistic, domain-specific
// generators. These take precedence over gofakeit reflection so that semantic
// provider names (e.g. "gate", "flight_code") always produce meaningful data
// instead of falling back to placeholder values.
var domainProviders = map[string]providerFunc{
	"flight_code":  func(f *gofakeit.Faker, _ config.CollectionField) interface{} { return RandomFlightCode(f) },
	"gate":         func(f *gofakeit.Faker, _ config.CollectionField) interface{} { return RandomGate(f) },
	"airport_code": func(f *gofakeit.Faker, _ config.CollectionField) interface{} { return RandomAirportCode(f) },
	"airport":      func(f *gofakeit.Faker, _ config.CollectionField) interface{} { return RandomAirportCode(f) },
	"airline": func(f *gofakeit.Faker, _ config.CollectionField) interface{} {
		return AirlineCodes[f.Rand.Intn(len(AirlineCodes))]
	},
	"airline_code": func(f *gofakeit.Faker, _ config.CollectionField) interface{} {
		return AirlineCodes[f.Rand.Intn(len(AirlineCodes))]
	},
	"aircraft": func(f *gofakeit.Faker, _ config.CollectionField) interface{} {
		return PlaneTypes[f.Rand.Intn(len(PlaneTypes))]
	},
	"plane_type": func(f *gofakeit.Faker, _ config.CollectionField) interface{} {
		return PlaneTypes[f.Rand.Intn(len(PlaneTypes))]
	},
	"aircraft_model": func(f *gofakeit.Faker, _ config.CollectionField) interface{} {
		return PlaneTypes[f.Rand.Intn(len(PlaneTypes))]
	},
	"seats_available":  func(f *gofakeit.Faker, def config.CollectionField) interface{} { return boundedInt(f, def, 0, 400) },
	"duration_minutes": func(f *gofakeit.Faker, def config.CollectionField) interface{} { return boundedInt(f, def, 30, 900) },
	"passenger_count":  func(f *gofakeit.Faker, def config.CollectionField) interface{} { return boundedInt(f, def, 0, 400) },
	"equip":            equipmentProvider,
	"equipment":        equipmentProvider,
	"passengers": func(f *gofakeit.Faker, _ config.CollectionField) interface{} {
		_, total := RandomEquipment(f)
		available := f.Rand.Intn(total + 1)
		return RandomPassengers(f, total, available)
	},
}

func equipmentProvider(f *gofakeit.Faker, _ config.CollectionField) interface{} {
	equip, _ := RandomEquipment(f)
	return equip
}

// domainProvider returns the registered generator for a provider name and
// whether one exists.
func domainProvider(name string) (providerFunc, bool) {
	fn, ok := domainProviders[name]
	return fn, ok
}
