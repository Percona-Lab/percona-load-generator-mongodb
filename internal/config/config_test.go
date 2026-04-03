package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAppConfigWebUIDefaultsAndBaseDefaults(t *testing.T) {
	t.Setenv("PLGM_URI", "")
	cfg, err := LoadAppConfig(filepath.Join(t.TempDir(), "missing.yaml"), true)
	if err != nil {
		t.Fatalf("LoadAppConfig() error = %v", err)
	}

	if cfg.URI != "mongodb://localhost:27017" {
		t.Fatalf("expected default URI, got %q", cfg.URI)
	}
	if cfg.ConnectionParams.Username != "plgm" {
		t.Fatalf("expected UI username default, got %q", cfg.ConnectionParams.Username)
	}
	if cfg.CollectionsPath != "resources/collections" || cfg.QueriesPath != "resources/queries" {
		t.Fatalf("expected default resource paths, got collections=%q queries=%q", cfg.CollectionsPath, cfg.QueriesPath)
	}
	if cfg.WebUI.Port != 9999 {
		t.Fatalf("expected default web port 9999, got %d", cfg.WebUI.Port)
	}
	if cfg.FindPercent+cfg.UpdatePercent+cfg.DeletePercent+cfg.InsertPercent+cfg.AggregatePercent+cfg.TransactionPercent+cfg.BulkInsertPercent != 100 {
		t.Fatalf("expected normalized percentages to total 100")
	}
	if cfg.InsightsExplainWorkers != 1 || cfg.InsightsExplainRetries != 1 || cfg.InsightsExplainBackoffMS != 150 || cfg.InsightsExplainSeverityMode != "high_only" {
		t.Fatalf("expected explain defaults workers=1 retries=1 backoff=150 mode=high_only, got workers=%d retries=%d backoff=%d mode=%q", cfg.InsightsExplainWorkers, cfg.InsightsExplainRetries, cfg.InsightsExplainBackoffMS, cfg.InsightsExplainSeverityMode)
	}
}

func TestLoadAppConfigWithYAMLAndEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	content := []byte(`uri: mongodb://example:27017
connection_params:
  username: from_yaml
find_percent: 10
update_percent: 20
delete_percent: 10
insert_percent: 60
use_transactions: false
`)
	if err := os.WriteFile(cfgPath, content, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	t.Setenv("PLGM_USERNAME", "env_user")
	t.Setenv("PLGM_FIND_PERCENT", "40")
	t.Setenv("PLGM_INSERT_PERCENT", "40")
	t.Setenv("PLGM_AGGREGATE_PERCENT", "20")
	t.Setenv("PLGM_TRANSACTION_PERCENT", "15")

	cfg, err := LoadAppConfig(cfgPath, false)
	if err != nil {
		t.Fatalf("LoadAppConfig() error = %v", err)
	}

	if cfg.ConnectionParams.Username != "env_user" {
		t.Fatalf("expected env username override, got %q", cfg.ConnectionParams.Username)
	}
	if cfg.TransactionPercent != 0 {
		t.Fatalf("expected transaction percent forced to 0 when transactions disabled, got %d", cfg.TransactionPercent)
	}
	total := cfg.FindPercent + cfg.UpdatePercent + cfg.DeletePercent + cfg.InsertPercent + cfg.AggregatePercent + cfg.TransactionPercent + cfg.BulkInsertPercent
	if total != 100 {
		t.Fatalf("expected normalized total of 100, got %d", total)
	}
}

func TestApplyEnvOverridesDefaultWorkloadBehavior(t *testing.T) {
	cfg := &AppConfig{DefaultWorkload: true}

	t.Setenv("PLGM_COLLECTIONS_PATH", "/tmp/custom_collections")
	t.Setenv("PLGM_QUERIES_PATH", "/tmp/custom_queries")

	_ = applyEnvOverrides(cfg)
	if cfg.DefaultWorkload {
		t.Fatalf("expected default_workload=false when both custom paths are set without explicit override")
	}

	cfg2 := &AppConfig{DefaultWorkload: false}
	t.Setenv("PLGM_DEFAULT_WORKLOAD", "true")
	_ = applyEnvOverrides(cfg2)
	if !cfg2.DefaultWorkload {
		t.Fatalf("expected explicit PLGM_DEFAULT_WORKLOAD override to win")
	}
}

func TestNormalizePercentagesPinnedOver100(t *testing.T) {
	cfg := &AppConfig{
		UseTransactions:  true,
		FindPercent:      70,
		UpdatePercent:    50,
		DeletePercent:    10,
		InsertPercent:    5,
		AggregatePercent: 0,
	}
	pinned := map[string]bool{
		"FindPercent":   true,
		"UpdatePercent": true,
	}

	normalizePercentages(cfg, pinned)

	if cfg.DeletePercent != 0 || cfg.InsertPercent != 0 || cfg.AggregatePercent != 0 || cfg.TransactionPercent != 0 || cfg.BulkInsertPercent != 0 {
		t.Fatalf("expected unpinned percentages zeroed when pinned total >=100")
	}
	total := cfg.FindPercent + cfg.UpdatePercent + cfg.DeletePercent + cfg.InsertPercent + cfg.AggregatePercent + cfg.TransactionPercent + cfg.BulkInsertPercent
	if total != 100 {
		t.Fatalf("expected final total 100, got %d", total)
	}
}

func TestApplyBaseDefaultsPreservesExplicitValues(t *testing.T) {
	cfg := &AppConfig{}
	cfg.ConnectionParams.MaxPoolSize = 300
	cfg.ConnectionParams.MinPoolSize = 5
	cfg.RawInjector.Type = "insert"
	cfg.CSVExportEnabled = true
	cfg.CSVExportPath = ""

	applyBaseDefaults(cfg)

	if cfg.ConnectionParams.MaxPoolSize != 300 || cfg.ConnectionParams.MinPoolSize != 5 {
		t.Fatalf("expected explicit pool sizes to be preserved")
	}
	if cfg.RawInjector.Type != "insert" {
		t.Fatalf("expected explicit raw injector type to be preserved")
	}
	if cfg.CSVExportPath == "" {
		t.Fatalf("expected csv path default when export enabled")
	}
}

func TestApplyEnvOverridesInsightsExplainRuntimeSettings(t *testing.T) {
	cfg := &AppConfig{}
	t.Setenv("PLGM_INSIGHTS_EXPLAIN_WORKERS", "3")
	t.Setenv("PLGM_INSIGHTS_EXPLAIN_RETRIES", "2")
	t.Setenv("PLGM_INSIGHTS_EXPLAIN_BACKOFF_MS", "250")
	t.Setenv("PLGM_INSIGHTS_EXPLAIN_SEVERITY_MODE", "medium_only")

	_ = applyEnvOverrides(cfg)
	applyBaseDefaults(cfg)

	if cfg.InsightsExplainWorkers != 3 {
		t.Fatalf("expected explain workers override 3, got %d", cfg.InsightsExplainWorkers)
	}
	if cfg.InsightsExplainRetries != 2 {
		t.Fatalf("expected explain retries override 2, got %d", cfg.InsightsExplainRetries)
	}
	if cfg.InsightsExplainBackoffMS != 250 {
		t.Fatalf("expected explain backoff override 250, got %d", cfg.InsightsExplainBackoffMS)
	}
	if cfg.InsightsExplainSeverityMode != "medium_only" {
		t.Fatalf("expected explain severity mode override medium_only, got %q", cfg.InsightsExplainSeverityMode)
	}
}

func TestApplyBaseDefaultsNormalizesUnknownExplainSeverityMode(t *testing.T) {
	cfg := &AppConfig{InsightsExplainSeverityMode: "unexpected_mode"}
	applyBaseDefaults(cfg)
	if cfg.InsightsExplainSeverityMode != "high_only" {
		t.Fatalf("expected invalid explain severity mode to normalize to high_only, got %q", cfg.InsightsExplainSeverityMode)
	}
}
