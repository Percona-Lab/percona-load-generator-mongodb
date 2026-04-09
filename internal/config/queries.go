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
	Queries []QueryDefinition `json:"queries"`
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

	parsed, err := parseQueriesBytes(b)
	if err != nil {
		return nil, fmt.Errorf("invalid JSON format for embedded queries: %w", err)
	}
	annotateQueryMetadata(parsed.Queries, "embedded_default", embedPath, "default_workload")
	if err := NormalizeAndValidateQueries(parsed.Queries); err != nil {
		return nil, err
	}
	return parsed, nil
}

func loadQueriesFromFile(path string) (*QueriesFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read queries file: %w", err)
	}

	parsed, err := parseQueriesBytes(b)
	if err != nil {
		return nil, fmt.Errorf("invalid JSON format for queries: %w", err)
	}
	if err := NormalizeAndValidateQueries(parsed.Queries); err != nil {
		return nil, err
	}
	return parsed, nil
}

func parseQueriesBytes(b []byte) (*QueriesFile, error) {
	var wrapped QueriesFile
	if err := json.Unmarshal(b, &wrapped); err == nil && len(wrapped.Queries) > 0 {
		return &wrapped, nil
	}

	var defs []QueryDefinition
	if err := json.Unmarshal(b, &defs); err == nil && len(defs) > 0 {
		return &QueriesFile{Queries: defs}, nil
	}

	return nil, fmt.Errorf("expected either [{...}] or {\"queries\":[...]} with at least one query definition")
}

func annotateQueryMetadata(defs []QueryDefinition, sourceType, sourceFile, workloadName string) {
	for i := range defs {
		defs[i].SourceType = sourceType
		defs[i].SourceFile = sourceFile
		defs[i].WorkloadName = workloadName
		defs[i].DefinitionIndex = i
	}
}

func ParseQueriesBytes(b []byte) (*QueriesFile, error) {
	return parseQueriesBytes(b)
}

func NormalizeAndValidateQueries(defs []QueryDefinition) error {
	for i := range defs {
		q := &defs[i]
		rawOperation := q.Operation

		q.Operation = normalizeQueryOperation(q.Operation)
		if q.Operation == "" {
			return fmt.Errorf("invalid query operation at index %d: %q", i, rawOperation)
		}
		if strings.TrimSpace(q.Collection) == "" {
			return fmt.Errorf("query at index %d is missing required field 'collection'", i)
		}

		switch q.Operation {
		case "find", "deleteOne", "deleteMany", "updateOne", "updateMany":
			if q.Filter == nil {
				return fmt.Errorf("query at index %d (%s) is missing required field 'filter'", i, q.Operation)
			}
		}
		switch q.Operation {
		case "updateOne", "updateMany":
			if len(q.Update) == 0 {
				return fmt.Errorf("query at index %d (%s) is missing required field 'update'", i, q.Operation)
			}
		case "aggregate":
			if len(q.Pipeline) == 0 {
				return fmt.Errorf("query at index %d (%s) is missing required field 'pipeline'", i, q.Operation)
			}
		}

		if strings.TrimSpace(q.Name) == "" {
			q.Name = fmt.Sprintf("%s_%s_%d", q.Operation, sanitizeQueryNamePart(q.Collection), i+1)
		}
		if strings.TrimSpace(q.Label) == "" {
			q.Label = q.Name
		}
	}
	return nil
}

func ValidateAndBindQueriesToCollections(defs []QueryDefinition, cols []CollectionDefinition) ([]QueryDefinition, error) {
	if len(defs) == 0 {
		return nil, nil
	}

	byNamespace := make(map[string]CollectionDefinition, len(cols))
	byName := make(map[string][]CollectionDefinition, len(cols))
	for _, col := range cols {
		nsKey := namespaceKey(col.DatabaseName, col.Name)
		byNamespace[nsKey] = col
		nameKey := strings.ToLower(strings.TrimSpace(col.Name))
		byName[nameKey] = append(byName[nameKey], col)
	}

	out := make([]QueryDefinition, len(defs))
	for i := range defs {
		q := defs[i]
		if strings.TrimSpace(q.Database) != "" {
			if _, ok := byNamespace[namespaceKey(q.Database, q.Collection)]; !ok {
				return nil, fmt.Errorf("query %q at index %d references unknown collection %q in database %q", displayQueryName(q, i), i, q.Collection, q.Database)
			}
		} else {
			candidates := byName[strings.ToLower(strings.TrimSpace(q.Collection))]
			switch len(candidates) {
			case 0:
				return nil, fmt.Errorf("query %q at index %d references unknown collection %q", displayQueryName(q, i), i, q.Collection)
			case 1:
				q.Database = candidates[0].DatabaseName
			default:
				return nil, fmt.Errorf("query %q at index %d references collection %q without a database, but it exists in multiple databases; set query.database explicitly", displayQueryName(q, i), i, q.Collection)
			}
		}
		out[i] = q
	}

	return out, nil
}

func normalizeQueryOperation(op string) string {
	switch strings.ToLower(strings.TrimSpace(op)) {
	case "find":
		return "find"
	case "aggregate":
		return "aggregate"
	case "update":
		return "updateOne"
	case "updateone":
		return "updateOne"
	case "updatemany":
		return "updateMany"
	case "delete":
		return "deleteOne"
	case "deleteone":
		return "deleteOne"
	case "deletemany":
		return "deleteMany"
	case "insert":
		return "insert"
	case "insertmany":
		return "insertMany"
	default:
		return ""
	}
}

func namespaceKey(dbName, collectionName string) string {
	return strings.ToLower(strings.TrimSpace(dbName)) + "." + strings.ToLower(strings.TrimSpace(collectionName))
}

func sanitizeQueryNamePart(v string) string {
	val := strings.TrimSpace(strings.ToLower(v))
	if val == "" {
		return "query"
	}
	val = strings.ReplaceAll(val, " ", "_")
	return val
}

func displayQueryName(q QueryDefinition, index int) string {
	if strings.TrimSpace(q.Name) != "" {
		return q.Name
	}
	if strings.TrimSpace(q.Label) != "" {
		return q.Label
	}
	return fmt.Sprintf("query_%d", index+1)
}
