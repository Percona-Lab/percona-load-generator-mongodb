package datagen

import (
	"testing"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/brianvoe/gofakeit/v6"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestToCamelCase(t *testing.T) {
	tests := map[string]string{
		"first_name":   "FirstName",
		"uuid":         "Uuid",
		"alreadyCamel": "AlreadyCamel",
		"":             "",
	}
	for in, want := range tests {
		if got := toCamelCase(in); got != want {
			t.Fatalf("toCamelCase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRandomValueWithFakerTypesAndProviders(t *testing.T) {
	min, max := 10, 20
	faker := gofakeit.New(42)
	nowBefore := time.Now()

	tests := []struct {
		name  string
		def   config.CollectionField
		check func(t *testing.T, got interface{})
	}{
		{
			name: "provider_method_no_args",
			def:  config.CollectionField{Type: "string", Provider: "email"},
			check: func(t *testing.T, got interface{}) {
				s, ok := got.(string)
				if !ok || s == "" {
					t.Fatalf("expected non-empty string, got %T %v", got, got)
				}
			},
		},
		{
			name: "int_range",
			def:  config.CollectionField{Type: "int", Min: &min, Max: &max},
			check: func(t *testing.T, got interface{}) {
				v, ok := got.(int32)
				if !ok {
					t.Fatalf("expected int32, got %T", got)
				}
				if int(v) < min || int(v) > max {
					t.Fatalf("int out of range: %d", v)
				}
			},
		},
		{
			name: "array_with_items",
			def: config.CollectionField{
				Type:      "array",
				ArraySize: 3,
				Items:     &config.CollectionField{Type: "string"},
			},
			check: func(t *testing.T, got interface{}) {
				arr, ok := got.(bson.A)
				if !ok || len(arr) != 3 {
					t.Fatalf("expected bson.A of len 3, got %T len=%d", got, len(arr))
				}
			},
		},
		{
			name: "object_with_fields",
			def: config.CollectionField{
				Type: "object",
				Fields: map[string]config.CollectionField{
					"name": {Type: "string"},
					"age":  {Type: "int"},
				},
			},
			check: func(t *testing.T, got interface{}) {
				doc, ok := got.(bson.D)
				if !ok || len(doc) != 2 {
					t.Fatalf("expected bson.D with 2 fields, got %T len=%d", got, len(doc))
				}
			},
		},
		{
			name: "date_in_recent_window",
			def:  config.CollectionField{Type: "date"},
			check: func(t *testing.T, got interface{}) {
				tm, ok := got.(time.Time)
				if !ok {
					t.Fatalf("expected time.Time, got %T", got)
				}
				nowAfter := time.Now()
				if tm.Before(nowBefore.Add(-366*24*time.Hour)) || tm.After(nowAfter) {
					t.Fatalf("date out of expected window: %v", tm)
				}
			},
		},
		{
			name: "unknown_type",
			def:  config.CollectionField{Type: "mystery"},
			check: func(t *testing.T, got interface{}) {
				s, ok := got.(string)
				if !ok || s != "unknown-mystery" {
					t.Fatalf("expected unknown marker, got %T %v", got, got)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := RandomValueWithFaker(tc.def, faker)
			tc.check(t, got)
		})
	}
}

func TestRandomValueWithInvalidStringProviderFallsBackToVal(t *testing.T) {
	faker := gofakeit.New(1)
	got := RandomValueWithFaker(config.CollectionField{Type: "string", Provider: "does_not_exist"}, faker)
	if got != "val" {
		t.Fatalf("expected fallback val, got %v", got)
	}
}

func TestRandomValueWithFakerIntRangeHandlesInvertedBounds(t *testing.T) {
	min, max := 20, 10
	faker := gofakeit.New(7)

	got := RandomValueWithFaker(config.CollectionField{
		Type: "int",
		Min:  &min,
		Max:  &max,
	}, faker)

	v, ok := got.(int32)
	if !ok {
		t.Fatalf("expected int32, got %T", got)
	}
	if int(v) < 10 || int(v) > 20 {
		t.Fatalf("expected value to be normalized to swapped range [10..20], got %d", v)
	}
}
