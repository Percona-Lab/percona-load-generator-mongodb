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

	// --- VALIDATION ---
	// Ensure we didn't load an empty config due to JSON key mismatch
	for i, col := range allCollections {
		if col.DatabaseName == "" || col.Name == "" {
			return nil, fmt.Errorf("loaded collection at index %d has empty 'database' or 'collection' name. Check your JSON keys: must be 'database' and 'collection' (lowercase)", i)
		}
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
