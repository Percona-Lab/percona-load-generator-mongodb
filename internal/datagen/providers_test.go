package datagen

import (
	"regexp"
	"testing"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/brianvoe/gofakeit/v6"
)

var (
	flightCodeRe = regexp.MustCompile(`^[A-Z0-9]{2}[0-9]{3}$`)
	gateRe       = regexp.MustCompile(`^[A-F][0-9]{1,2}$`)
)

func TestDomainProvidersProduceRealisticValues(t *testing.T) {
	faker := gofakeit.New(123)

	for i := 0; i < 200; i++ {
		code, _ := RandomValueWithFaker(config.CollectionField{Type: "string", Provider: "flight_code"}, faker).(string)
		if !flightCodeRe.MatchString(code) {
			t.Fatalf("flight_code %q is not airline-style", code)
		}

		gate, _ := RandomValueWithFaker(config.CollectionField{Type: "string", Provider: "gate"}, faker).(string)
		if !gateRe.MatchString(gate) {
			t.Fatalf("gate %q is not realistic", gate)
		}

		seats, ok := RandomValueWithFaker(config.CollectionField{Type: "int", Provider: "seats_available"}, faker).(int)
		if !ok || seats < 0 || seats > 400 {
			t.Fatalf("seats_available out of range: %v", seats)
		}

		dur, ok := RandomValueWithFaker(config.CollectionField{Type: "int", Provider: "duration_minutes"}, faker).(int)
		if !ok || dur < 30 || dur > 900 {
			t.Fatalf("duration_minutes out of range: %v", dur)
		}

		ac, _ := RandomValueWithFaker(config.CollectionField{Type: "string", Provider: "airport_code"}, faker).(string)
		if len(ac) != 3 {
			t.Fatalf("airport_code %q not a 3-letter code", ac)
		}
	}
}

func TestDomainProviderBoundedOverrides(t *testing.T) {
	faker := gofakeit.New(7)
	min, max := 50, 60
	for i := 0; i < 100; i++ {
		v, ok := RandomValueWithFaker(config.CollectionField{Type: "int", Provider: "seats_available", Min: &min, Max: &max}, faker).(int)
		if !ok || v < 50 || v > 60 {
			t.Fatalf("expected bounded seats_available in [50,60], got %v", v)
		}
	}
}

func TestEquipmentProviderRealistic(t *testing.T) {
	faker := gofakeit.New(1)
	equip, total := RandomEquipment(faker)
	if total <= 0 {
		t.Fatalf("expected positive seat capacity, got %d", total)
	}
	pt, _ := equip["plane_type"].(string)
	found := false
	for _, p := range PlaneTypes {
		if p == pt {
			found = true
		}
	}
	if !found {
		t.Fatalf("plane_type %q not in known list", pt)
	}
	if _, ok := equip["amenities"].([]string); !ok {
		t.Fatalf("expected amenities slice, got %T", equip["amenities"])
	}
}

func TestEnumSelection(t *testing.T) {
	faker := gofakeit.New(2)
	def := config.CollectionField{Type: "string", Enum: []interface{}{"scheduled", "boarding", "landed"}}
	allowed := map[string]bool{"scheduled": true, "boarding": true, "landed": true}
	for i := 0; i < 50; i++ {
		v, _ := RandomValueWithFaker(def, faker).(string)
		if !allowed[v] {
			t.Fatalf("enum produced unexpected value %q", v)
		}
	}
}

func TestTemplateExpansion(t *testing.T) {
	faker := gofakeit.New(3)
	def := config.CollectionField{Type: "string", Template: "??###"}
	re := regexp.MustCompile(`^[A-Z]{2}[0-9]{3}$`)
	for i := 0; i < 50; i++ {
		v, _ := RandomValueWithFaker(def, faker).(string)
		if !re.MatchString(v) {
			t.Fatalf("template produced %q which does not match pattern", v)
		}
	}
}

func TestUnknownStringProviderStillFallsBack(t *testing.T) {
	faker := gofakeit.New(4)
	got := RandomValueWithFaker(config.CollectionField{Type: "string", Provider: "does_not_exist"}, faker)
	if got != "val" {
		t.Fatalf("expected fallback val for unknown provider, got %v", got)
	}
}

func TestSeedDeterminism(t *testing.T) {
	gen := func() []int64 {
		ConfigureSeed(42)
		out := make([]int64, 5)
		for i := range out {
			out[i] = NextSeed()
		}
		return out
	}
	a := gen()
	b := gen()
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("seed sequence not reproducible at %d: %d != %d", i, a[i], b[i])
		}
	}

	// Clearing the seed should restore non-deterministic (time-based) seeding.
	ConfigureSeed(0)
	if NextSeed() == 0 {
		t.Fatalf("expected non-zero time-based seed when seed cleared")
	}
}
