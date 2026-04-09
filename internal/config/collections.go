package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Percona-Lab/percona-load-generator-mongodb/resources"
)

type CollectionField struct {
	Type      string                     `json:"type"`
	Provider  string                     `json:"provider,omitempty"`
	MaxLength int                        `json:"maxLength,omitempty"`
	MinLength int                        `json:"minLength,omitempty"`
	Min       *int                       `json:"min,omitempty"`
	Max       *int                       `json:"max,omitempty"`
	Enum      []string                   `json:"enum,omitempty"`
	Items     *CollectionField           `json:"items,omitempty"`
	Fields    map[string]CollectionField `json:"fields,omitempty"`
	ArraySize int                        `json:"arraySize,omitempty"`
}

type IndexDefinition struct {
	Keys map[string]interface{} `json:"keys"`
}

type ShardConfig struct {
	Key    map[string]interface{} `json:"key"`
	Unique bool                   `json:"unique,omitempty"`
}

type CollectionDefinition struct {
	DatabaseName string                     `json:"database"`
	Name         string                     `json:"collection"`
	Fields       map[string]CollectionField `json:"fields"`
	Indexes      []IndexDefinition          `json:"indexes,omitempty"`
	ShardConfig  *ShardConfig               `json:"shardConfig,omitempty"`
}

type CollectionsFile struct {
	Collections []CollectionDefinition `json:"collections"`
}

// LoadCollections filters files based on the 'loadDefault' flag.
// - If loadDefault is TRUE: Load ONLY 'default.json'.
// - If loadDefault is FALSE: Load ALL files EXCEPT 'default.json'.
// - Single file paths are always loaded.
func LoadCollections(path string, loadDefault bool) (*CollectionsFile, error) {
	if path == "" {
		return &CollectionsFile{}, nil
	}

	// 1. Try to access the folder on disk
	info, err := os.Stat(path)

	// 2. Fallback Logic: If folder/file not found, use Embedded Default
	if os.IsNotExist(err) {
		fmt.Printf("Warning: Collections path '%s' not found. Using embedded default.json.\n", path)
		return loadEmbeddedCollection("collections/default.json")
	}

	if err != nil {
		return nil, fmt.Errorf("stat path %s: %w", path, err)
	}

	var allCollections []CollectionDefinition

	// 3. Normal Disk Loading Logic
	if info.IsDir() {
		entries, err := os.ReadDir(path)
		if err != nil {
			return nil, fmt.Errorf("read collections dir: %w", err)
		}

		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
				continue
			}

			isDefault := strings.EqualFold(entry.Name(), "default.json")

			if loadDefault {
				if !isDefault {
					continue
				}
			} else {
				if isDefault {
					continue
				}
			}

			fullPath := filepath.Join(path, entry.Name())
			loaded, err := loadCollectionsFromFile(fullPath)
			if err != nil {
				return nil, fmt.Errorf("error loading collection file %s: %w", entry.Name(), err)
			}
			allCollections = append(allCollections, loaded.Collections...)
		}
	} else {
		// Single file path provided by user
		loaded, err := loadCollectionsFromFile(path)
		if err != nil {
			return nil, err
		}
		allCollections = append(allCollections, loaded.Collections...)
	}

	if err := ValidateCollectionDefinitions(allCollections); err != nil {
		return nil, err
	}

	return &CollectionsFile{Collections: allCollections}, nil
}

// loadEmbeddedCollection reads a specific file from the embedded FS
func loadEmbeddedCollection(embedPath string) (*CollectionsFile, error) {
	b, err := resources.Defaults.ReadFile(embedPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read embedded file %s: %w", embedPath, err)
	}
	return parseCollectionsBytes(b)
}

func loadCollectionsFromFile(path string) (*CollectionsFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read collections file: %w", err)
	}
	return parseCollectionsBytes(b)
}

func parseCollectionsBytes(b []byte) (*CollectionsFile, error) {
	var wrapped CollectionsFile
	if err := json.Unmarshal(b, &wrapped); err == nil && len(wrapped.Collections) > 0 {
		return &wrapped, nil
	}

	var arr []CollectionDefinition
	if err := json.Unmarshal(b, &arr); err == nil && len(arr) > 0 {
		return &CollectionsFile{Collections: arr}, nil
	}

	return nil, fmt.Errorf("invalid collections format")
}

func ParseCollectionsBytes(b []byte) (*CollectionsFile, error) {
	return parseCollectionsBytes(b)
}

func ValidateCollectionDefinitions(cols []CollectionDefinition) error {
	seenNamespaces := make(map[string]int, len(cols))

	for i, col := range cols {
		if strings.TrimSpace(col.DatabaseName) == "" || strings.TrimSpace(col.Name) == "" {
			return fmt.Errorf("loaded collection at index %d has empty 'database' or 'collection' name. Check your JSON keys: must be 'database' and 'collection' (lowercase)", i)
		}

		nsKey := strings.ToLower(strings.TrimSpace(col.DatabaseName)) + "." + strings.ToLower(strings.TrimSpace(col.Name))
		if prev, ok := seenNamespaces[nsKey]; ok {
			return fmt.Errorf("duplicate namespace %q at index %d (already declared at index %d)", col.DatabaseName+"."+col.Name, i, prev)
		}
		seenNamespaces[nsKey] = i

		for idxPos, idx := range col.Indexes {
			if len(idx.Keys) == 0 {
				return fmt.Errorf("collection %q has invalid index at position %d: index keys cannot be empty", col.Name, idxPos)
			}
		}
		if err := validateFieldConstraints(col.Fields, ""); err != nil {
			return fmt.Errorf("collection %q field validation failed: %w", col.DatabaseName+"."+col.Name, err)
		}
	}

	return nil
}

func validateFieldConstraints(fields map[string]CollectionField, prefix string) error {
	for name, field := range fields {
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		if field.Min != nil && field.Max != nil && *field.Min > *field.Max {
			return fmt.Errorf("field %q has invalid min/max: min (%d) is greater than max (%d)", path, *field.Min, *field.Max)
		}
		if field.MinLength > 0 && field.MaxLength > 0 && field.MinLength > field.MaxLength {
			return fmt.Errorf("field %q has invalid minLength/maxLength: minLength (%d) is greater than maxLength (%d)", path, field.MinLength, field.MaxLength)
		}
		if len(field.Fields) > 0 {
			if err := validateFieldConstraints(field.Fields, path); err != nil {
				return err
			}
		}
		if field.Items != nil {
			if err := validateFieldConstraints(map[string]CollectionField{"[]": *field.Items}, path); err != nil {
				return err
			}
		}
	}
	return nil
}
