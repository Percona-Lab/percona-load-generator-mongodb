package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadQueriesDirectoryFiltering(t *testing.T) {
	dir := t.TempDir()
	defaultJSON := `[{"name":"find_flights","database":"db","collection":"flights","operation":"find","filter":{}}]`
	customJSON := `[{"name":"update_flights","database":"db","collection":"flights","operation":"updateOne","filter":{},"update":{"$set":{"a":1}}}]`

	if err := os.WriteFile(filepath.Join(dir, "default.json"), []byte(defaultJSON), 0o644); err != nil {
		t.Fatalf("write default: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "custom.json"), []byte(customJSON), 0o644); err != nil {
		t.Fatalf("write custom: %v", err)
	}

	defaultOnly, err := LoadQueries(dir, true)
	if err != nil {
		t.Fatalf("LoadQueries(default) error = %v", err)
	}
	if len(defaultOnly.Queries) != 1 || defaultOnly.Queries[0].Operation != "find" {
		t.Fatalf("expected only default query, got %+v", defaultOnly.Queries)
	}

	nonDefault, err := LoadQueries(dir, false)
	if err != nil {
		t.Fatalf("LoadQueries(non-default) error = %v", err)
	}
	if len(nonDefault.Queries) != 1 || nonDefault.Queries[0].Operation != "updateOne" {
		t.Fatalf("expected only custom query, got %+v", nonDefault.Queries)
	}
}

func TestLoadQueriesMissingPathFallsBackToEmbeddedDefault(t *testing.T) {
	cfg, err := LoadQueries(filepath.Join(t.TempDir(), "missing"), true)
	if err != nil {
		t.Fatalf("LoadQueries() error = %v", err)
	}
	if len(cfg.Queries) == 0 {
		t.Fatalf("expected embedded default queries")
	}
}

func TestLoadQueriesFromFileInvalidJSON(t *testing.T) {
	file := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(file, []byte(`{"not":"an array"}`), 0o644); err != nil {
		t.Fatalf("write bad file: %v", err)
	}

	_, err := loadQueriesFromFile(file)
	if err == nil {
		t.Fatalf("expected invalid JSON format error")
	}
}

func TestParseQueriesBytesSupportsWrappedAndArray(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantLen int
		wantErr bool
	}{
		{
			name:    "wrapped",
			input:   `{"queries":[{"name":"q1","collection":"orders","operation":"find","filter":{}}]}`,
			wantLen: 1,
		},
		{
			name:    "array",
			input:   `[{"name":"q1","collection":"orders","operation":"find","filter":{}}]`,
			wantLen: 1,
		},
		{
			name:    "invalid",
			input:   `{"queries":[]}`,
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseQueriesBytes([]byte(tc.input))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected parse error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseQueriesBytes() error = %v", err)
			}
			if len(got.Queries) != tc.wantLen {
				t.Fatalf("expected %d queries, got %d", tc.wantLen, len(got.Queries))
			}
		})
	}
}

func TestNormalizeAndValidateQueries(t *testing.T) {
	tests := []struct {
		name    string
		queries []QueryDefinition
		wantErr string
		wantOp  string
	}{
		{
			name: "normalizes_update_alias_and_generates_name",
			queries: []QueryDefinition{
				{Collection: "orders", Operation: "update", Filter: map[string]interface{}{}, Update: map[string]interface{}{"$set": map[string]interface{}{"state": "x"}}},
			},
			wantOp: "updateOne",
		},
		{
			name: "missing_collection",
			queries: []QueryDefinition{
				{Operation: "find", Filter: map[string]interface{}{}},
			},
			wantErr: "missing required field 'collection'",
		},
		{
			name: "aggregate_missing_pipeline",
			queries: []QueryDefinition{
				{Collection: "orders", Operation: "aggregate"},
			},
			wantErr: "missing required field 'pipeline'",
		},
		{
			name: "update_missing_update_doc",
			queries: []QueryDefinition{
				{Collection: "orders", Operation: "updateOne", Filter: map[string]interface{}{}},
			},
			wantErr: "missing required field 'update'",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := NormalizeAndValidateQueries(tc.queries)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeAndValidateQueries() error = %v", err)
			}
			if tc.wantOp != "" && tc.queries[0].Operation != tc.wantOp {
				t.Fatalf("expected operation %q, got %q", tc.wantOp, tc.queries[0].Operation)
			}
			if tc.queries[0].Name == "" {
				t.Fatalf("expected generated name")
			}
		})
	}
}

func TestValidateAndBindQueriesToCollections(t *testing.T) {
	collections := []CollectionDefinition{
		{DatabaseName: "db1", Name: "orders"},
		{DatabaseName: "db2", Name: "orders"},
		{DatabaseName: "db1", Name: "users"},
	}

	t.Run("binds_database_when_unambiguous", func(t *testing.T) {
		in := []QueryDefinition{
			{Name: "q1", Collection: "users", Operation: "find", Filter: map[string]interface{}{}},
		}
		if err := NormalizeAndValidateQueries(in); err != nil {
			t.Fatalf("NormalizeAndValidateQueries() error = %v", err)
		}
		got, err := ValidateAndBindQueriesToCollections(in, collections)
		if err != nil {
			t.Fatalf("ValidateAndBindQueriesToCollections() error = %v", err)
		}
		if got[0].Database != "db1" {
			t.Fatalf("expected db1 bound, got %q", got[0].Database)
		}
	})

	t.Run("fails_unknown_collection", func(t *testing.T) {
		in := []QueryDefinition{
			{Name: "q2", Collection: "missing", Operation: "find", Filter: map[string]interface{}{}},
		}
		if err := NormalizeAndValidateQueries(in); err != nil {
			t.Fatalf("NormalizeAndValidateQueries() error = %v", err)
		}
		_, err := ValidateAndBindQueriesToCollections(in, collections)
		if err == nil || !strings.Contains(err.Error(), "unknown collection") {
			t.Fatalf("expected unknown collection error, got %v", err)
		}
	})

	t.Run("fails_when_database_is_explicit_but_not_defined_for_collection", func(t *testing.T) {
		in := []QueryDefinition{
			{Name: "q_db_mismatch", Database: "db9", Collection: "orders", Operation: "find", Filter: map[string]interface{}{}},
		}
		if err := NormalizeAndValidateQueries(in); err != nil {
			t.Fatalf("NormalizeAndValidateQueries() error = %v", err)
		}
		_, err := ValidateAndBindQueriesToCollections(in, collections)
		if err == nil || !strings.Contains(err.Error(), "unknown collection") {
			t.Fatalf("expected unknown collection+database error, got %v", err)
		}
	})

	t.Run("fails_ambiguous_collection_without_database", func(t *testing.T) {
		in := []QueryDefinition{
			{Name: "q3", Collection: "orders", Operation: "find", Filter: map[string]interface{}{}},
		}
		if err := NormalizeAndValidateQueries(in); err != nil {
			t.Fatalf("NormalizeAndValidateQueries() error = %v", err)
		}
		_, err := ValidateAndBindQueriesToCollections(in, collections)
		if err == nil || !strings.Contains(err.Error(), "exists in multiple databases") {
			t.Fatalf("expected ambiguous database error, got %v", err)
		}
	})
}
