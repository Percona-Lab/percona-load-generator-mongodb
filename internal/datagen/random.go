package datagen

import (
	"fmt"
	"reflect"
	"strings"
	"time"
	"unicode"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/brianvoe/gofakeit/v6"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// toCamelCase converts snake_case (e.g. "first_name") to CamelCase (e.g. "FirstName")
func toCamelCase(s string) string {
	parts := strings.Split(s, "_")
	for i := range parts {
		if len(parts[i]) > 0 {
			r := []rune(parts[i])
			r[0] = unicode.ToUpper(r[0])
			parts[i] = string(r)
		}
	}
	return strings.Join(parts, "")
}

// expandTemplate replaces pattern characters in a template string:
//
//	'#' -> random digit, '?' -> uppercase letter, '^' -> lowercase letter.
//
// All other characters are emitted literally.
func expandTemplate(faker *gofakeit.Faker, template string) string {
	rng := faker.Rand
	out := make([]rune, 0, len(template))
	for _, c := range template {
		switch c {
		case '#':
			out = append(out, rune('0'+rng.Intn(10)))
		case '?':
			out = append(out, rune('A'+rng.Intn(26)))
		case '^':
			out = append(out, rune('a'+rng.Intn(26)))
		default:
			out = append(out, c)
		}
	}
	return string(out)
}

// RandomValueWithFaker uses an existing Faker instance to generate values.
// This is much faster than creating a new Faker for every field.
func RandomValueWithFaker(def config.CollectionField, faker *gofakeit.Faker) interface{} {
	// Use the RNG inside the faker instance for raw math operations
	rng := faker.Rand

	// 0. Enum values take precedence over everything else: when a field
	// enumerates allowed values we always pick one of them.
	if len(def.Enum) > 0 {
		return def.Enum[rng.Intn(len(def.Enum))]
	}

	// 0b. Template/pattern values for structured string fields (e.g. "??###").
	if def.Template != "" {
		return expandTemplate(faker, def.Template)
	}

	// 1. Provider lookup. Domain-specific providers (flight_code, gate, ...)
	// take precedence so semantic names always yield realistic data; otherwise
	// fall back to gofakeit reflection.
	if def.Provider != "" {
		if fn, ok := domainProvider(strings.ToLower(def.Provider)); ok {
			return fn(faker, def)
		}

		methodName := toCamelCase(def.Provider)
		fakerVal := reflect.ValueOf(faker)
		method := fakerVal.MethodByName(methodName)

		if method.IsValid() {
			// Check argument count to prevent panics
			numArgs := method.Type().NumIn()

			if numArgs == 0 {
				// e.g. Name(), Email() - No args required
				results := method.Call(nil)
				if len(results) > 0 {
					return results[0].Interface()
				}
			} else if numArgs == 1 && method.Type().In(0).Kind() == reflect.Int {
				// e.g. Sentence(wordCount) or Paragraph(paragraphCount)
				// Smart logic: Use configuration constraints if available
				argVal := 5 // Default fallback

				if def.MaxLength > 0 {
					argVal = def.MaxLength
				} else if def.ArraySize > 0 {
					argVal = def.ArraySize
				}

				results := method.Call([]reflect.Value{reflect.ValueOf(argVal)})
				if len(results) > 0 {
					return results[0].Interface()
				}
			}
			// Note: Methods requiring >1 args or non-int args are skipped here
			// and will fall through to the switch/default below.
		}

		// Fallback for special providers (handling cases reflection might miss or specific overrides)
		switch strings.ToLower(def.Provider) {
		case "uuid":
			return faker.UUID()
		case "ssn":
			return faker.SSN()
		}
	}

	// 2. Handle All MongoDB Data Types
	switch strings.ToLower(def.Type) {
	// --- Numbers ---
	case "int", "integer", "int32":
		min := 0
		max := 2147483647
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
			return int32(min)
		}
		return int32(rng.Intn(span) + min)

	case "long", "int64":
		return rng.Int63()

	case "double", "float":
		min := 0.0
		max := 1000.0
		if def.Min != nil {
			min = float64(*def.Min)
		}
		if def.Max != nil {
			max = float64(*def.Max)
		}
		return min + rng.Float64()*(max-min)

	case "decimal", "decimal128":
		val := fmt.Sprintf("%d.%d", rng.Intn(1000), rng.Intn(100))
		d, _ := bson.ParseDecimal128(val)
		return d

	// --- Strings & Boolean ---
	case "string":
		if def.Provider == "" {
			return fmt.Sprintf("str-%d", rng.Intn(100000))
		}
		// If provider was invalid or skipped in reflection, we land here.
		return "val"

	case "bool", "boolean":
		return rng.Intn(2) == 0

	// --- Dates & Times ---
	case "date", "datetime":
		return time.Now().Add(-time.Duration(rng.Intn(365*24)) * time.Hour)
	case "timestamp":
		return bson.Timestamp{T: uint32(time.Now().Unix()), I: uint32(rng.Intn(100))}

	// --- Identifiers ---
	case "objectid":
		return bson.NewObjectID()

	// --- Complex Structures (Recursion uses the SAME faker instance) ---
	case "object", "document":
		if len(def.Fields) > 0 {
			doc := make(bson.D, 0, len(def.Fields))
			for key, fieldDef := range def.Fields {
				val := RandomValueWithFaker(fieldDef, faker)
				doc = append(doc, bson.E{Key: key, Value: val})
			}
			return doc
		}
		return bson.D{{Key: "nested_random", Value: rng.Intn(100)}}

	case "array":
		size := def.ArraySize
		if size <= 0 {
			size = rng.Intn(5) + 1
		}
		arr := make(bson.A, size)

		if def.Items != nil {
			for i := 0; i < size; i++ {
				arr[i] = RandomValueWithFaker(*def.Items, faker)
			}
		} else {
			for i := 0; i < size; i++ {
				arr[i] = rng.Intn(1000)
			}
		}
		return arr

	default:
		return fmt.Sprintf("unknown-%s", def.Type)
	}
}

// RandomValue convenience wrapper (slower, creates new faker)
func RandomValue(def config.CollectionField) interface{} {
	return RandomValueWithFaker(def, NewFaker())
}
