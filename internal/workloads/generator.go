package workloads

import (
	"math/rand"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/datagen"
)

// GenerateDocument creates a single document.
func GenerateDocument(col config.CollectionDefinition, cfg *config.AppConfig) map[string]interface{} {
	if isFlightSchema(col) {
		return GenerateDefaultDocument(col)
	}
	return generateGenericDocument(col)
}

// GenerateFallbackUpdate creates an update document when no configured query is found.
func GenerateFallbackUpdate(col config.CollectionDefinition, cfg *config.AppConfig, rng *rand.Rand) map[string]interface{} {
	if isFlightSchema(col) {
		return GenerateDefaultUpdate(rng)
	}
	return generateGenericUpdate(col, rng)
}

func generateGenericDocument(col config.CollectionDefinition) map[string]interface{} {
	// Optimization: Create ONE faker instance per document (honors seed config).
	faker := datagen.NewFaker()

	doc := make(map[string]interface{})
	for fieldName, fieldDef := range col.Fields {
		// Pass the faker instance to reuse RNG
		doc[fieldName] = datagen.RandomValueWithFaker(fieldDef, faker)
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
