package workloads

import (
	"math/rand"
	"sort"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/datagen"
	"github.com/brianvoe/gofakeit/v6"
)

// GenerateDocument creates a single document.
func GenerateDocument(col config.CollectionDefinition, cfg *config.AppConfig) map[string]interface{} {
	return GenerateDocumentWithFaker(col, cfg, datagen.NewFaker())
}

// GenerateDocumentWithFaker creates a single document from faker's random
// stream, so the caller controls the seed.
func GenerateDocumentWithFaker(col config.CollectionDefinition, cfg *config.AppConfig, faker *gofakeit.Faker) map[string]interface{} {
	if isFlightSchema(col) {
		return generateDefaultDocument(col, faker)
	}
	return generateGenericDocument(col, faker)
}

// GenerateFallbackUpdateWithFaker is GenerateFallbackUpdate drawing every
// value from faker's random stream, with the field picked from the sorted
// field names, so the same stream always yields the same update.
func GenerateFallbackUpdateWithFaker(col config.CollectionDefinition, faker *gofakeit.Faker) map[string]interface{} {
	if isFlightSchema(col) {
		return GenerateDefaultUpdate(faker.Rand)
	}
	if len(col.Fields) == 0 {
		return map[string]interface{}{"$set": map[string]interface{}{"updated_at": faker.Rand.Int63()}}
	}
	keys := make([]string, 0, len(col.Fields))
	for k := range col.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	field := keys[faker.Rand.Intn(len(keys))]
	return map[string]interface{}{"$set": map[string]interface{}{field: datagen.RandomValueWithFaker(col.Fields[field], faker)}}
}

// GenerateFallbackUpdate creates an update document when no configured query is found.
func GenerateFallbackUpdate(col config.CollectionDefinition, cfg *config.AppConfig, rng *rand.Rand) map[string]interface{} {
	if isFlightSchema(col) {
		return GenerateDefaultUpdate(rng)
	}
	return generateGenericUpdate(col, rng)
}

// generateGenericDocument uses ONE faker instance per document.
func generateGenericDocument(col config.CollectionDefinition, faker *gofakeit.Faker) map[string]interface{} {
	doc := make(map[string]interface{})
	for _, fieldName := range datagen.FieldNames(col.Fields) {
		// Pass the faker instance to reuse RNG
		doc[fieldName] = datagen.RandomValueWithFaker(col.Fields[fieldName], faker)
	}
	return doc
}

func generateGenericUpdate(col config.CollectionDefinition, rng *rand.Rand) map[string]interface{} {
	if len(col.Fields) == 0 {
		return map[string]interface{}{
			"$set": map[string]interface{}{"updated_at": rng.Int63()},
		}
	}

	keys := make([]string, 0, len(col.Fields))
	for k := range col.Fields {
		keys = append(keys, k)
	}
	randomField := keys[rng.Intn(len(keys))]
	fieldDef := col.Fields[randomField]

	// For single field updates, we can create a temporary faker wrapping the existing RNG
	// or just make a new one (updates are less frequent than inserts usually)
	val := datagen.RandomValue(fieldDef)

	return map[string]interface{}{
		"$set": map[string]interface{}{randomField: val},
	}
}
