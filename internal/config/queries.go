package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Percona-Lab/percona-load-generator-mongodb/resources"
)

type QueryDefinition struct {
	Database    string                 `json:"database" yaml:"database"`
	Collection  string                 `json:"collection" yaml:"collection"`
	Operation   string                 `json:"operation" yaml:"operation"`
	ID          string                 `json:"id,omitempty" yaml:"id,omitempty"`
	Name        string                 `json:"name,omitempty" yaml:"name,omitempty"`
	Label       string                 `json:"label,omitempty" yaml:"label,omitempty"`
	Description string                 `json:"description,omitempty" yaml:"description,omitempty"`
	Filter      map[string]interface{} `json:"filter" yaml:"filter"`
	Pipeline    []interface{}          `json:"pipeline,omitempty" yaml:"pipeline,omitempty"`
	Projection  map[string]interface{} `json:"projection,omitempty" yaml:"projection,omitempty"`
	Limit       int64                  `json:"limit,omitempty" yaml:"limit,omitempty"`
	Update      map[string]interface{} `json:"update,omitempty" yaml:"update,omitempty"`
	Upsert      bool                   `json:"upsert,omitempty" yaml:"upsert,omitempty"`

	SourceFile      string `json:"-" yaml:"-"`
	SourceType      string `json:"-" yaml:"-"`
	WorkloadName    string `json:"-" yaml:"-"`
	DefinitionIndex int    `json:"-" yaml:"-"`
}

type QueriesFile struct {
	Queries []QueryDefinition
}

// LoadQueries filters files based on the 'loadDefault' flag.
// - If loadDefault is TRUE: Load ONLY 'default.json'.
// - If loadDefault is FALSE: Load ALL files EXCEPT 'default.json'.
// - Single file paths are always loaded.
func LoadQueries(path string, loadDefault bool) (*QueriesFile, error) {
	if path == "" {
		return &QueriesFile{}, nil
	}

	// 1. Try to access the folder on disk
	info, err := os.Stat(path)

	// 2. Fallback Logic
	if os.IsNotExist(err) {
		fmt.Printf("Warning: Queries path '%s' not found. Using embedded default.json.\n", path)
		return loadEmbeddedQuery("queries/default.json")
	}

	if err != nil {
		return nil, fmt.Errorf("stat path %s: %w", path, err)
	}

	var allQueries []QueryDefinition

	// 3. Normal Disk Loading Logic
	if info.IsDir() {
		entries, err := os.ReadDir(path)
		if err != nil {
			return nil, fmt.Errorf("read queries dir: %w", err)
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
			loaded, err := loadQueriesFromFile(fullPath)
			if err != nil {
				return nil, fmt.Errorf("error loading query file %s: %w", entry.Name(), err)
			}
			workload := "custom_workload"
			if loadDefault {
				workload = "default_workload"
			}
			annotateQueryMetadata(loaded.Queries, "file", entry.Name(), workload)
			allQueries = append(allQueries, loaded.Queries...)
		}
	} else {
		loaded, err := loadQueriesFromFile(path)
		if err != nil {
			return nil, err
		}
		workload := "custom_workload"
		if loadDefault {
			workload = "default_workload"
		}
		annotateQueryMetadata(loaded.Queries, "file", filepath.Base(path), workload)
		allQueries = append(allQueries, loaded.Queries...)
	}

	return &QueriesFile{Queries: allQueries}, nil
}

// loadEmbeddedQuery reads from embedded FS
func loadEmbeddedQuery(embedPath string) (*QueriesFile, error) {
	b, err := resources.Defaults.ReadFile(embedPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read embedded file %s: %w", embedPath, err)
	}

	var defs []QueryDefinition
	if err := json.Unmarshal(b, &defs); err != nil {
		return nil, fmt.Errorf("invalid JSON format for embedded queries: %w", err)
	}
	annotateQueryMetadata(defs, "embedded_default", embedPath, "default_workload")
	return &QueriesFile{Queries: defs}, nil
}

func loadQueriesFromFile(path string) (*QueriesFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read queries file: %w", err)
	}

	var defs []QueryDefinition
	if err := json.Unmarshal(b, &defs); err != nil {
		return nil, fmt.Errorf("invalid JSON format for queries: %w", err)
	}

	return &QueriesFile{Queries: defs}, nil
}

func annotateQueryMetadata(defs []QueryDefinition, sourceType, sourceFile, workloadName string) {
	for i := range defs {
		defs[i].SourceType = sourceType
		defs[i].SourceFile = sourceFile
		defs[i].WorkloadName = workloadName
		defs[i].DefinitionIndex = i
	}
}
