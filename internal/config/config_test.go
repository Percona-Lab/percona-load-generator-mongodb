package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/accesspattern"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/loadprofile"
)

func TestValidateLoadProfile(t *testing.T) {
	t.Run("valid_ramp_passes", func(t *testing.T) {
		cfg := &AppConfig{Concurrency: 4, LoadProfile: loadprofile.Config{
			Kind: "ramp", StartWorkers: 1, EndWorkers: 50, RampOver: "30s",
		}}
		if err := ValidateLoadProfile(cfg); err != nil {
			t.Fatalf("expected valid ramp, got %v", err)
		}
	})
	t.Run("empty_profile_passes", func(t *testing.T) {
		if err := ValidateLoadProfile(&AppConfig{Concurrency: 4}); err != nil {
			t.Fatalf("expected empty profile to be valid, got %v", err)
		}
	})
	t.Run("invalid_step_fails", func(t *testing.T) {
		cfg := &AppConfig{Concurrency: 4, LoadProfile: loadprofile.Config{
			Kind: "step", Steps: []loadprofile.Stage{{Workers: -5, Duration: "10s"}},
		}}
		if err := ValidateLoadProfile(cfg); err == nil {
			t.Fatalf("expected invalid step profile to fail validation")
		}
	})
}

func TestLoadAppConfigRejectsInvalidLoadProfileEnv(t *testing.T) {
	t.Setenv("PLGM_LOAD_PROFILE_KIND", "bogus")
	_, err := LoadAppConfig(filepath.Join(t.TempDir(), "missing.yaml"), false)
	if err == nil {
		t.Fatalf("expected LoadAppConfig to reject unknown load profile kind")
	}
}

func TestThinkTimeEnvOverrideAndClamp(t *testing.T) {
	t.Setenv("PLGM_THINK_TIME_MS", "75")
	t.Setenv("PLGM_THINK_JITTER_MS", "20")
	cfg, err := LoadAppConfig(filepath.Join(t.TempDir(), "missing.yaml"), false)
	if err != nil {
		t.Fatalf("LoadAppConfig() error = %v", err)
	}
	if cfg.ThinkTimeMs != 75 || cfg.ThinkJitterMs != 20 {
		t.Fatalf("expected think_time=75 jitter=20, got %d/%d", cfg.ThinkTimeMs, cfg.ThinkJitterMs)
	}

	// Negative values are clamped to 0 by applyBaseDefaults.
	clamp := &AppConfig{ThinkTimeMs: -5, ThinkJitterMs: -3}
	applyBaseDefaults(clamp)
	if clamp.ThinkTimeMs != 0 || clamp.ThinkJitterMs != 0 {
		t.Fatalf("expected negative pacing clamped to 0, got %d/%d", clamp.ThinkTimeMs, clamp.ThinkJitterMs)
	}
}

func TestValidateAccessPattern(t *testing.T) {
	valid := []*AppConfig{
		{},
		{AccessPattern: accesspattern.Config{Kind: "uniform"}},
		{AccessPattern: accesspattern.Config{Kind: "zipfian", ZipfianExponent: 1.5}},
		{AccessPattern: accesspattern.Config{Kind: "hotspot", HotspotPercent: 10, HotspotTrafficPercent: 90}},
	}
	for i, cfg := range valid {
		if err := ValidateAccessPattern(cfg); err != nil {
			t.Fatalf("valid[%d] unexpected error: %v", i, err)
		}
	}
	invalid := []*AppConfig{
		{AccessPattern: accesspattern.Config{Kind: "bogus"}},
		{AccessPattern: accesspattern.Config{Kind: "zipfian", ZipfianExponent: 0.5}},
		{AccessPattern: accesspattern.Config{Kind: "hotspot", HotspotPercent: 200}},
	}
	for i, cfg := range invalid {
		if err := ValidateAccessPattern(cfg); err == nil {
			t.Fatalf("invalid[%d] expected error, got nil", i)
		}
	}
}

func TestAccessPatternEnvOverride(t *testing.T) {
	t.Setenv("PLGM_ACCESS_PATTERN_KIND", "zipfian")
	t.Setenv("PLGM_ACCESS_PATTERN_ZIPFIAN_EXPONENT", "1.7")
	cfg, err := LoadAppConfig(filepath.Join(t.TempDir(), "missing.yaml"), false)
	if err != nil {
		t.Fatalf("LoadAppConfig() error = %v", err)
	}
	if cfg.AccessPattern.Kind != "zipfian" || cfg.AccessPattern.ZipfianExponent != 1.7 {
		t.Fatalf("expected zipfian/1.7, got %q/%g", cfg.AccessPattern.Kind, cfg.AccessPattern.ZipfianExponent)
	}
}

func TestLoadAppConfigRejectsInvalidAccessPatternEnv(t *testing.T) {
	t.Setenv("PLGM_ACCESS_PATTERN_KIND", "bogus")
	if _, err := LoadAppConfig(filepath.Join(t.TempDir(), "missing.yaml"), false); err == nil {
		t.Fatalf("expected invalid access pattern kind to fail LoadAppConfig")
	}
}

func TestLoadProfileEnvOverride(t *testing.T) {
	t.Setenv("PLGM_LOAD_PROFILE_KIND", "fixed")
	t.Setenv("PLGM_LOAD_PROFILE_WORKERS", "12")
	cfg, err := LoadAppConfig(filepath.Join(t.TempDir(), "missing.yaml"), false)
	if err != nil {
		t.Fatalf("LoadAppConfig() error = %v", err)
	}
	if cfg.LoadProfile.Kind != "fixed" || cfg.LoadProfile.Workers != 12 {
		t.Fatalf("expected env-driven fixed/12 load profile, got %+v", cfg.LoadProfile)
	}
}

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
	if cfg.WebUI.TLSEnabled {
		t.Fatalf("expected TLS disabled by default (HTTP on loopback)")
	}
	if cfg.FindPercent+cfg.UpdatePercent+cfg.DeletePercent+cfg.InsertPercent+cfg.AggregatePercent+cfg.TransactionPercent+cfg.BulkInsertPercent != 100 {
		t.Fatalf("expected normalized percentages to total 100")
	}
	if cfg.InsightsExplainWorkers != 1 || cfg.InsightsExplainRetries != 1 || cfg.InsightsExplainBackoffMS != 150 || cfg.InsightsExplainSeverityMode != "high_only" || cfg.InsightsExplainVerbosity != "executionStats" {
		t.Fatalf("expected explain defaults workers=1 retries=1 backoff=150 mode=high_only verbosity=executionStats, got workers=%d retries=%d backoff=%d mode=%q verbosity=%q", cfg.InsightsExplainWorkers, cfg.InsightsExplainRetries, cfg.InsightsExplainBackoffMS, cfg.InsightsExplainSeverityMode, cfg.InsightsExplainVerbosity)
	}
	if cfg.ShardingMode != "auto" || !cfg.ShardingSkipGenericWithoutConfig {
		t.Fatalf("expected sharding defaults mode=auto skip_without_config=true, got mode=%q skip=%v", cfg.ShardingMode, cfg.ShardingSkipGenericWithoutConfig)
	}
}

func TestLoadAppConfigWebUITLSEnvOverrides(t *testing.T) {
	t.Setenv("PLGM_URI", "")
	t.Setenv("PLGM_WEBUI_TLS", "true")
	t.Setenv("PLGM_WEBUI_TLS_CERT", "/etc/ssl/plgm.crt")
	t.Setenv("PLGM_WEBUI_TLS_KEY", "/etc/ssl/plgm.key")

	cfg, err := LoadAppConfig(filepath.Join(t.TempDir(), "missing.yaml"), true)
	if err != nil {
		t.Fatalf("LoadAppConfig() error = %v", err)
	}
	if !cfg.WebUI.TLSEnabled {
		t.Fatalf("expected TLS enabled via PLGM_WEBUI_TLS")
	}
	if cfg.WebUI.TLSCertFile != "/etc/ssl/plgm.crt" {
		t.Fatalf("expected cert file override, got %q", cfg.WebUI.TLSCertFile)
	}
	if cfg.WebUI.TLSKeyFile != "/etc/ssl/plgm.key" {
		t.Fatalf("expected key file override, got %q", cfg.WebUI.TLSKeyFile)
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
	t.Setenv("PLGM_INSIGHTS_EXPLAIN_VERBOSITY", "queryPlanner")

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
	if cfg.InsightsExplainVerbosity != "queryPlanner" {
		t.Fatalf("expected explain verbosity override queryPlanner, got %q", cfg.InsightsExplainVerbosity)
	}
}

func TestApplyBaseDefaultsNormalizesUnknownExplainSeverityMode(t *testing.T) {
	cfg := &AppConfig{InsightsExplainSeverityMode: "unexpected_mode"}
	applyBaseDefaults(cfg)
	if cfg.InsightsExplainSeverityMode != "high_only" {
		t.Fatalf("expected invalid explain severity mode to normalize to high_only, got %q", cfg.InsightsExplainSeverityMode)
	}
}

func TestApplyBaseDefaultsNormalizesUnknownExplainVerbosity(t *testing.T) {
	cfg := &AppConfig{InsightsExplainVerbosity: "unexpected_mode"}
	applyBaseDefaults(cfg)
	if cfg.InsightsExplainVerbosity != "executionStats" {
		t.Fatalf("expected invalid explain verbosity to normalize to executionStats, got %q", cfg.InsightsExplainVerbosity)
	}
}

func TestNormalizeShardingMode(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "", want: "auto"},
		{in: "AUTO", want: "auto"},
		{in: "force_on", want: "force_on"},
		{in: "FORCE_OFF", want: "force_off"},
		{in: "unexpected", want: "auto"},
	}

	for _, tc := range tests {
		if got := NormalizeShardingMode(tc.in); got != tc.want {
			t.Fatalf("NormalizeShardingMode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestValidateShardingConfig(t *testing.T) {
	err := ValidateShardingConfig(&AppConfig{ShardingMode: "force_on", ShardingSkipGenericWithoutConfig: true})
	if err == nil {
		t.Fatalf("expected conflict error for force_on + skip_generic_without_config")
	}

	if err := ValidateShardingConfig(&AppConfig{ShardingMode: "auto", ShardingSkipGenericWithoutConfig: true}); err != nil {
		t.Fatalf("unexpected error for valid sharding config: %v", err)
	}
}

func TestApplyEnvOverridesShardingSettings(t *testing.T) {
	cfg := &AppConfig{}
	t.Setenv("PLGM_SHARDING_MODE", "force_off")
	t.Setenv("PLGM_SHARDING_SKIP_GENERIC_WITHOUT_CONFIG", "true")

	_ = applyEnvOverrides(cfg)
	applyBaseDefaults(cfg)

	if cfg.ShardingMode != "force_off" {
		t.Fatalf("expected sharding mode force_off from env, got %q", cfg.ShardingMode)
	}
	if !cfg.ShardingSkipGenericWithoutConfig {
		t.Fatalf("expected sharding skip-without-config true from env")
	}
}
