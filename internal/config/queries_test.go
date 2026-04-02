package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadQueriesDirectoryFiltering(t *testing.T) {
	dir := t.TempDir()
	defaultJSON := `[{"database":"db","collection":"flights","operation":"find","filter":{}}]`
	customJSON := `[{"database":"db","collection":"flights","operation":"updateOne","filter":{},"update":{"$set":{"a":1}}}]`

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
