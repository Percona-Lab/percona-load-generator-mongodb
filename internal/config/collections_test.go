package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseCollectionsBytes(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantLen int
		wantErr bool
	}{
		{
			name:    "wrapped_format",
			input:   `{"collections":[{"database":"db1","collection":"c1","fields":{}}]}`,
			wantLen: 1,
		},
		{
			name:    "array_format",
			input:   `[{"database":"db2","collection":"c2","fields":{}}]`,
			wantLen: 1,
		},
		{
			name:    "invalid_format",
			input:   `{"collections":[]}`,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCollectionsBytes([]byte(tc.input))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseCollectionsBytes() error = %v", err)
			}
			if len(got.Collections) != tc.wantLen {
				t.Fatalf("expected %d collections, got %d", tc.wantLen, len(got.Collections))
			}
		})
	}
}

func TestLoadCollectionsDirectoryFiltering(t *testing.T) {
	dir := t.TempDir()
	defaultJSON := `{"collections":[{"database":"db","collection":"default_col","fields":{}}]}`
	customJSON := `{"collections":[{"database":"db","collection":"custom_col","fields":{}}]}`

	if err := os.WriteFile(filepath.Join(dir, "default.json"), []byte(defaultJSON), 0o644); err != nil {
		t.Fatalf("write default: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "custom.json"), []byte(customJSON), 0o644); err != nil {
		t.Fatalf("write custom: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ignore.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write ignore: %v", err)
	}

	defaultOnly, err := LoadCollections(dir, true)
	if err != nil {
		t.Fatalf("LoadCollections(default) error = %v", err)
	}
	if len(defaultOnly.Collections) != 1 || defaultOnly.Collections[0].Name != "default_col" {
		t.Fatalf("expected only default collection, got %+v", defaultOnly.Collections)
	}

	nonDefault, err := LoadCollections(dir, false)
	if err != nil {
		t.Fatalf("LoadCollections(non-default) error = %v", err)
	}
	if len(nonDefault.Collections) != 1 || nonDefault.Collections[0].Name != "custom_col" {
		t.Fatalf("expected only custom collection, got %+v", nonDefault.Collections)
	}
}

func TestLoadCollectionsMissingPathFallsBackToEmbeddedDefault(t *testing.T) {
	cfg, err := LoadCollections(filepath.Join(t.TempDir(), "does-not-exist"), true)
	if err != nil {
		t.Fatalf("LoadCollections() error = %v", err)
	}
	if len(cfg.Collections) == 0 {
		t.Fatalf("expected embedded default collections")
	}
}

func TestLoadCollectionsValidationErrorForMissingNames(t *testing.T) {
	file := filepath.Join(t.TempDir(), "bad.json")
	bad := `{"collections":[{"database":"","collection":"","fields":{}}]}`
	if err := os.WriteFile(file, []byte(bad), 0o644); err != nil {
		t.Fatalf("write bad file: %v", err)
	}

	_, err := LoadCollections(file, false)
	if err == nil {
		t.Fatalf("expected validation error for empty names")
	}
}

func TestValidateCollectionDefinitionsRejectsDuplicates(t *testing.T) {
	tests := []struct {
		name string
		cols []CollectionDefinition
		want string
	}{
		{
			name: "duplicate_namespace",
			cols: []CollectionDefinition{
				{DatabaseName: "db1", Name: "orders"},
				{DatabaseName: "db1", Name: "Orders"},
			},
			want: "duplicate namespace",
		},
		{
			name: "invalid_empty_index_keys",
			cols: []CollectionDefinition{
				{
					DatabaseName: "db1",
					Name:         "orders",
					Indexes:      []IndexDefinition{{Keys: map[string]interface{}{}}},
				},
			},
			want: "index keys cannot be empty",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCollectionDefinitions(tc.cols)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestValidateCollectionDefinitionsAllowsSameCollectionNameInDifferentDatabases(t *testing.T) {
	cols := []CollectionDefinition{
		{DatabaseName: "db1", Name: "orders"},
		{DatabaseName: "db2", Name: "orders"},
	}
	if err := ValidateCollectionDefinitions(cols); err != nil {
		t.Fatalf("expected same collection name across databases to be valid, got %v", err)
	}
}

func TestValidateCollectionDefinitionsRejectsInvalidFieldRanges(t *testing.T) {
	min, max := 10, 1
	cols := []CollectionDefinition{
		{
			DatabaseName: "db1",
			Name:         "orders",
			Fields: map[string]CollectionField{
				"amount": {Type: "int", Min: &min, Max: &max},
			},
		},
	}
	err := ValidateCollectionDefinitions(cols)
	if err == nil || !strings.Contains(err.Error(), "invalid min/max") {
		t.Fatalf("expected invalid min/max error, got %v", err)
	}
}

func TestValidateCollectionDefinitionsRejectsNestedInvalidRanges(t *testing.T) {
	minLen, maxLen := 50, 5
	cols := []CollectionDefinition{
		{
			DatabaseName: "db1",
			Name:         "orders",
			Fields: map[string]CollectionField{
				"items": {
					Type: "array",
					Items: &CollectionField{
						Type: "object",
						Fields: map[string]CollectionField{
							"sku": {
								Type:      "string",
								MinLength: minLen,
								MaxLength: maxLen,
							},
						},
					},
				},
			},
		},
	}
	err := ValidateCollectionDefinitions(cols)
	if err == nil || !strings.Contains(err.Error(), "invalid minLength/maxLength") {
		t.Fatalf("expected nested minLength/maxLength validation error, got %v", err)
	}
}
