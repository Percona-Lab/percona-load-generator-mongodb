package config

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/accesspattern"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/loadprofile"
	"gopkg.in/yaml.v2"
)

type AppConfig struct {
	URI             string `yaml:"uri"`
	DefaultWorkload bool   `yaml:"default_workload"`
	PprofEnabled    bool   `yaml:"pprof_enabled"`
	CollectionsPath string `yaml:"collections_path"`
	QueriesPath     string `yaml:"queries_path"`
	DropCollections bool   `yaml:"drop_collections"`
	SkipSeed        bool   `yaml:"skip_seed"`
	DocumentsCount  int    `yaml:"documents_count"`
	Concurrency     int    `yaml:"concurrency"`
	Iterations      int    `yaml:"iterations"`
	IntervalDelay   string `yaml:"interval_delay"`

	Duration           string `yaml:"duration"`
	FindPercent        int    `yaml:"find_percent"`
	UpdatePercent      int    `yaml:"update_percent"`
	DeletePercent      int    `yaml:"delete_percent"`
	InsertPercent      int    `yaml:"insert_percent"`
	AggregatePercent   int    `yaml:"aggregate_percent"`
	TransactionPercent int    `yaml:"transaction_percent"`
	BulkInsertPercent  int    `yaml:"bulk_insert_percent"`
	InsertBatchSize    int    `yaml:"insert_batch_size"`
	SeedBatchSize      int    `yaml:"seed_batch_size"`
	SeedConcurrency    int    `yaml:"seed_concurrency"`
	UseTransactions    bool   `yaml:"use_transactions"`
	MaxTransactionOps  int    `yaml:"max_transaction_ops"`
	DebugMode          bool   `yaml:"debug_mode"`

	FindBatchSize             int   `yaml:"find_batch_size"`
	FindLimit                 int64 `yaml:"find_limit"`
	UseFindOneForLimitOne     bool  `yaml:"use_findone_for_limit_one"`
	InsertCacheSize           int   `yaml:"insert_cache_size"`
	StatusRefreshRateSec      int   `yaml:"status_refresh_rate_sec"`
	OpTimeoutMs               int   `yaml:"op_timeout_ms"`
	RetryAttempts             int   `yaml:"retry_attempts"`
	RetryBackoffMs            int   `yaml:"retry_backoff_ms"`
	ExistingRecordHitRate     int   `yaml:"existing_record_hit_rate"`
	RecordPoolMaxSize         int   `yaml:"record_pool_max_size"`
	RecordPoolBootstrapSample int   `yaml:"record_pool_bootstrap_sample"`

	// RandomSeed seeds data generation when set to a non-zero value.
	// Reproducibility is best-effort. The serial seed generator's random seed
	// sequence is independent of writer scheduling; concurrent workload streams
	// are seeded per-worker from this base.
	RandomSeed int64 `yaml:"random_seed"`

	// ThinkTimeMs is an optional fixed delay each worker waits between operations
	// to emulate realistic client pacing. ThinkJitterMs adds a uniform random
	// 0..ThinkJitterMs milliseconds on top of each delay. Both default to 0
	// (no pacing), preserving the historical "as fast as possible" behavior.
	// Pacing time is excluded from measured operation latency.
	ThinkTimeMs   int `yaml:"think_time_ms"`
	ThinkJitterMs int `yaml:"think_jitter_ms"`

	// LoadProfile shapes worker concurrency over the run (ramp/step/spike/sine).
	// An empty/fixed profile preserves the historical fixed-concurrency behavior
	// using Concurrency as the worker count.
	LoadProfile loadprofile.Config `yaml:"load_profile"`

	// AccessPattern controls how existing records are chosen for
	// find/update/delete targeting (uniform/zipfian/hotspot). An empty/uniform
	// pattern preserves the historical uniform-random selection.
	AccessPattern accesspattern.Config `yaml:"access_pattern"`

	ConnectionParams ConnectionParams       `yaml:"connection_params"`
	CustomParamsMap  map[string]interface{} `yaml:"custom_params"`
	Debug            bool                   `yaml:"debug"`
	WebUI            WebUIConfig            `yaml:"web_ui"`
	RawInjector      RawInjectorConfig      `yaml:"raw_injector"`

	CSVExportEnabled bool   `yaml:"csv_export_enabled"`
	CSVExportAppend  bool   `yaml:"csv_export_append"`
	CSVExportPath    string `yaml:"csv_export_path"`

	InsightsEnabled             bool    `yaml:"insights_enabled"`
	InsightsSamplingRate        float64 `yaml:"insights_sampling_rate"`
	InsightsSlowThresholdMs     int     `yaml:"insights_slow_threshold_ms"`
	InsightsMaxEvents           int     `yaml:"insights_max_events"`
	InsightsMaxGroups           int     `yaml:"insights_max_groups"`
	InsightsExplainEnabled      bool    `yaml:"insights_explain_enabled"`
	InsightsExplainTopN         int     `yaml:"insights_explain_top_n"`
	InsightsExplainMaxTimeMS    int     `yaml:"insights_explain_max_time_ms"`
	InsightsExplainVerbosity    string  `yaml:"insights_explain_verbosity"`
	InsightsExplainSeverityMode string  `yaml:"insights_explain_severity_mode"`
	// InsightsExplainEfficiencyMode adds explain slots chosen independently of
	// the slow threshold and severity filter. See NormalizeExplainEfficiencyMode.
	InsightsExplainEfficiencyMode string `yaml:"insights_explain_efficiency_mode"`
	InsightsExplainWorkers        int    `yaml:"insights_explain_workers"`
	InsightsExplainRetries        int    `yaml:"insights_explain_retries"`
	InsightsExplainBackoffMS      int    `yaml:"insights_explain_backoff_ms"`
	// Latency targets per query shape for the latency rating, in ms. 0 means
	// no target; with no target set, latency is shown but not rated.
	InsightsLatencyTargetAvgMs float64 `yaml:"insights_latency_target_avg_ms"`
	InsightsLatencyTargetP95Ms float64 `yaml:"insights_latency_target_p95_ms"`
	InsightsLatencyTargetP99Ms float64 `yaml:"insights_latency_target_p99_ms"`

	// Trend history. See NormalizeTrendSettings for defaults and bounds.
	TrendHistoryRuns int `yaml:"trend_history_runs"`
	// TrendMaxAgeDays is nil when unset (default 30); 0 means never outdated.
	TrendMaxAgeDays        *int   `yaml:"trend_max_age_days"`
	TrendBaselineStore     string `yaml:"trend_baseline_store"`
	TrendDefaultComparison string `yaml:"trend_default_comparison"`
	TrendRunLabel          string `yaml:"trend_run_label"`
	// InsightsTrendStoreQueryValues stores the literal commands of each
	// shape's samples with its trend snapshot. Off by default: trend
	// baselines then hold no query values.
	InsightsTrendStoreQueryValues bool `yaml:"insights_trend_store_query_values"`

	ShardingMode                     string `yaml:"sharding_mode"`
	ShardingSkipGenericWithoutConfig bool   `yaml:"sharding_skip_generic_without_config"`

	// Workload recording writes every operation a run sends, with its
	// literal values, to WorkloadRecordPath (NDJSON, mode 0600). Off by
	// default. See ValidateWorkloadRecordReplay. MaxBytes 0 is unlimited;
	// LoadAppConfig starts from DefaultWorkloadRecordMaxBytes.
	WorkloadRecordEnabled      bool   `yaml:"workload_record_enabled"`
	WorkloadRecordPath         string `yaml:"workload_record_path"`
	WorkloadRecordMaxBytes     int64  `yaml:"workload_record_max_bytes"`
	WorkloadRecordBackpressure string `yaml:"workload_record_backpressure"`
	WorkloadRecordCompression  string `yaml:"workload_record_compression"`

	// Workload replay executes the operations of a recording instead of
	// generating them. Off by default.
	WorkloadReplayEnabled       bool   `yaml:"workload_replay_enabled"`
	WorkloadReplayPath          string `yaml:"workload_replay_path"`
	WorkloadReplayMode          string `yaml:"workload_replay_mode"`
	WorkloadReplayAllowMismatch bool   `yaml:"workload_replay_allow_mismatch"`

	// WorkloadDeterministic makes generated traffic a function of the seed:
	// each worker sends OperationsPerWorker operations per iteration, each
	// derived from (seed, iteration, worker, sequence). Off by default. See
	// ValidateWorkloadDeterministic.
	WorkloadDeterministic bool `yaml:"workload_deterministic"`
	OperationsPerWorker   int  `yaml:"operations_per_worker"`

	// Outcome capture stores what every recorded, replayed, or
	// deterministic operation returned (latency, error code, result
	// counts, optional result checksum), so two executions can be compared.
	// Off by default. See ValidateWorkloadOutcomes.
	WorkloadRecordOutcomes            bool   `yaml:"workload_record_outcomes"`
	WorkloadRecordResultChecksums     bool   `yaml:"workload_record_result_checksums"`
	WorkloadReplayOutcomesPath        string `yaml:"workload_replay_outcomes_path"`
	WorkloadDeterministicOutcomesPath string `yaml:"workload_deterministic_outcomes_path"`

	// WorkloadReplayTargets runs one replay (or deterministic run) per
	// target instead of one against URI, then compares every target with
	// the first. Empty by default. See ValidateWorkloadTargets.
	WorkloadReplayTargets     []ReplayTarget `yaml:"workload_replay_targets"`
	WorkloadReplayTargetsMode string         `yaml:"workload_replay_targets_mode"`
	// WorkloadReplayOps is "all" (default) or "reads_only": a replay that
	// skips every write, so the targets keep their starting data.
	WorkloadReplayOps string `yaml:"workload_replay_ops"`
	// WorkloadDataFingerprint is "off" (default), "counts" (document count
	// and index list per collection), or "dbhash" (counts plus dbHash). A
	// recording stores it in its header; a replay compares every target
	// with it before sending any operation.
	WorkloadDataFingerprint string `yaml:"workload_data_fingerprint"`
}

// BuildVersion is the PLGM version, set by main.
var BuildVersion = "dev"

type WebUIConfig struct {
	Enabled bool `yaml:"enabled"`
	Port    int  `yaml:"port"`

	// TLS controls how the Web UI is served. By default the UI is served over
	// plain HTTP bound to the loopback interface (127.0.0.1), which browsers
	// treat as a secure context and therefore do NOT show certificate warnings.
	// Enable TLS only when exposing the UI beyond loopback; in that case supply
	// a trusted certificate/key via TLSCertFile/TLSKeyFile. If TLS is enabled
	// without cert/key files, an in-memory self-signed certificate is generated
	// (browsers will warn until it is trusted).
	TLSEnabled  bool   `yaml:"tls_enabled"`
	TLSCertFile string `yaml:"tls_cert_file"`
	TLSKeyFile  string `yaml:"tls_key_file"`
}

type RawInjectorConfig struct {
	Enabled        bool   `yaml:"enabled"`
	Type           string `yaml:"type"`
	DocumentSize   int    `yaml:"document_size"` // bytes
	DBName         string `yaml:"db_name"`
	CollectionName string `yaml:"collection_name"`
	MaxDocs        int64  `yaml:"max_docs"`
	BatchSize      int    `yaml:"batch_size"`
	DropCollection bool   `yaml:"drop_collection"`
}

type ConnectionParams struct {
	Username               string `yaml:"username"`
	Password               string `yaml:"-" json:"Password" mapstructure:"password"`
	AuthSource             string `yaml:"auth_source"`
	DirectConnection       bool   `yaml:"direct_connection"`
	ConnectionTimeout      int    `yaml:"connection_timeout"`
	ServerSelectionTimeout int    `yaml:"server_selection_timeout"`
	MaxPoolSize            int    `yaml:"max_pool_size"`
	MinPoolSize            int    `yaml:"min_pool_size"`
	MaxIdleTime            int    `yaml:"max_idle_time"`
	ReplicaSetName         string `yaml:"replicaset_name"`
	ReadPreference         string `yaml:"read_preference"`
	TLSCAFile              string `yaml:"tls_ca_file"`
	TLSCertificateKeyFile  string `yaml:"tlsCertificateKeyFile"`
}

func LoadAppConfig(path string, isWebUI bool) (*AppConfig, error) {
	cfg := &AppConfig{
		ExistingRecordHitRate:     90,
		RecordPoolMaxSize:         10000,
		RecordPoolBootstrapSample: 500,
		InsightsExplainEnabled:    true,
		WorkloadRecordMaxBytes:    DefaultWorkloadRecordMaxBytes,
	}
	configLoaded := false

	// 1. Load the YAML file if it exists
	if _, err := os.Stat(path); err == nil {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config file %s: %w", path, err)
		}
		if err := yaml.Unmarshal(b, cfg); err != nil {
			return nil, fmt.Errorf("invalid YAML format for config: %w", err)
		}
		configLoaded = true
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("error checking config file %s: %w", path, err)
	}

	// 2. Apply explicit environment variable overrides
	overriddenStats := applyEnvOverrides(cfg)

	// 3. Apply exact screenshot defaults ONLY if Web UI is requested AND no file was loaded
	if isWebUI && !configLoaded {
		applyUIDefaults(cfg)
	}

	// 4. Apply base safety limits (driver batch sizes, timeouts, etc.)
	applyBaseDefaults(cfg)
	if err := ValidateShardingConfig(cfg); err != nil {
		return nil, err
	}
	if err := ValidateLoadProfile(cfg); err != nil {
		return nil, err
	}
	if err := ValidateAccessPattern(cfg); err != nil {
		return nil, err
	}
	if err := ValidateConnectionParams(cfg); err != nil {
		return nil, err
	}
	if err := ValidateWorkloadRecordReplay(cfg); err != nil {
		return nil, err
	}
	if err := ValidateWorkloadDeterministic(cfg); err != nil {
		return nil, err
	}
	if err := ValidateWorkloadOutcomes(cfg); err != nil {
		return nil, err
	}
	if err := ValidateWorkloadTargets(cfg); err != nil {
		return nil, err
	}

	// 5. Balance the mix percentages
	normalizePercentages(cfg, overriddenStats)

	return cfg, nil
}

// applyUIDefaults forces exact visual values from screenshots when no config.yaml is found
func applyUIDefaults(cfg *AppConfig) {
	// --- CONNECTION TAB ---
	cfg.URI = "mongodb://localhost:27017"
	cfg.ConnectionParams.Username = "plgm"
	cfg.ConnectionParams.AuthSource = "admin"
	cfg.ConnectionParams.ReadPreference = "nearest"

	// --- WORKLOAD TAB ---
	cfg.Concurrency = 4
	cfg.Duration = "20s"
	cfg.DefaultWorkload = true
	cfg.SkipSeed = true
	cfg.DocumentsCount = 100000
	cfg.Iterations = 1
	cfg.IntervalDelay = "0s"

	// --- MIX TAB ---
	cfg.FindPercent = 60
	cfg.UpdatePercent = 20
	cfg.DeletePercent = 10
	cfg.InsertPercent = 5
	cfg.BulkInsertPercent = 5
	cfg.AggregatePercent = 0
	cfg.TransactionPercent = 0

	// --- EXPORT DEFAULTS ---
	cfg.CSVExportEnabled = false
	cfg.CSVExportAppend = false
	cfg.CSVExportPath = "plgm_metrics_export.csv"

	// --- INSIGHTS DEFAULTS ---
	cfg.InsightsEnabled = true
	cfg.InsightsSamplingRate = 0.10
	cfg.InsightsSlowThresholdMs = 200
	cfg.InsightsMaxEvents = 5000
	cfg.InsightsMaxGroups = 300
	cfg.InsightsExplainEnabled = true
	cfg.InsightsExplainTopN = 5
	cfg.InsightsExplainMaxTimeMS = 1000
	cfg.InsightsExplainVerbosity = "executionStats"
	cfg.InsightsExplainSeverityMode = "high_only"
	cfg.InsightsExplainEfficiencyMode = ExplainEfficiencyModeDefault
	cfg.InsightsExplainWorkers = 1
	cfg.InsightsExplainRetries = 1
	cfg.InsightsExplainBackoffMS = 150
	cfg.ShardingMode = "auto"
	cfg.ShardingSkipGenericWithoutConfig = true
}

// applyBaseDefaults sets low-level engine safety limits & remaining UI limits
func applyBaseDefaults(cfg *AppConfig) {
	// --- HIDDEN WORKLOAD PATHS ---
	// Required to find the default JSON files when no config.yaml is present
	if cfg.CollectionsPath == "" {
		cfg.CollectionsPath = "resources/collections"
	}
	if cfg.QueriesPath == "" {
		cfg.QueriesPath = "resources/queries"
	}

	if cfg.CSVExportEnabled && cfg.CSVExportPath == "" {
		cfg.CSVExportPath = "plgm_metrics_export.csv"
	}

	if cfg.InsightsSamplingRate <= 0 || cfg.InsightsSamplingRate > 1 {
		cfg.InsightsSamplingRate = 0.10
	}
	if cfg.InsightsSlowThresholdMs <= 0 {
		cfg.InsightsSlowThresholdMs = 200
	}
	if cfg.InsightsMaxEvents <= 0 {
		cfg.InsightsMaxEvents = 5000
	}
	if cfg.InsightsMaxGroups <= 0 {
		cfg.InsightsMaxGroups = 300
	}
	if cfg.InsightsExplainTopN <= 0 {
		cfg.InsightsExplainTopN = 5
	}
	if cfg.InsightsExplainMaxTimeMS <= 0 {
		cfg.InsightsExplainMaxTimeMS = 1000
	}
	switch cfg.InsightsExplainVerbosity {
	case "queryPlanner", "executionStats":
	default:
		cfg.InsightsExplainVerbosity = "executionStats"
	}
	switch cfg.InsightsExplainSeverityMode {
	case "high_and_low", "medium_only", "critical_only", "high_only":
	default:
		cfg.InsightsExplainSeverityMode = "high_only"
	}
	cfg.InsightsExplainEfficiencyMode = NormalizeExplainEfficiencyMode(cfg.InsightsExplainEfficiencyMode)
	if cfg.InsightsExplainWorkers <= 0 {
		cfg.InsightsExplainWorkers = 1
	}
	if cfg.InsightsExplainRetries < 0 {
		cfg.InsightsExplainRetries = 0
	}
	if cfg.InsightsExplainBackoffMS < 0 {
		cfg.InsightsExplainBackoffMS = 0
	}
	for _, t := range []*float64{&cfg.InsightsLatencyTargetAvgMs, &cfg.InsightsLatencyTargetP95Ms, &cfg.InsightsLatencyTargetP99Ms} {
		if *t < 0 || math.IsNaN(*t) || math.IsInf(*t, 0) {
			*t = 0
		}
	}
	cfg.ShardingMode = NormalizeShardingMode(cfg.ShardingMode)
	NormalizeTrendSettings(cfg)
	NormalizeWorkloadRecordReplay(cfg)

	// Web UI Port
	if cfg.WebUI.Port <= 0 {
		cfg.WebUI.Port = 9999 // default if not specified via flag
	}

	// --- CONNECTION POOLS ---
	if cfg.ConnectionParams.MaxPoolSize <= 0 {
		cfg.ConnectionParams.MaxPoolSize = 200
	}
	if cfg.ConnectionParams.MinPoolSize <= 0 {
		cfg.ConnectionParams.MinPoolSize = 10
	}
	if cfg.ConnectionParams.MaxIdleTime <= 0 {
		cfg.ConnectionParams.MaxIdleTime = 30
	}
	if cfg.ConnectionParams.ConnectionTimeout <= 0 {
		cfg.ConnectionParams.ConnectionTimeout = 30
	}
	if cfg.ConnectionParams.ServerSelectionTimeout <= 0 {
		cfg.ConnectionParams.ServerSelectionTimeout = 15
	}

	// --- ADVANCED TAB ---
	if cfg.FindBatchSize <= 0 {
		cfg.FindBatchSize = 10
	}
	if cfg.FindLimit <= 0 {
		cfg.FindLimit = 5
	}
	if cfg.InsertBatchSize <= 0 {
		cfg.InsertBatchSize = 10
	}
	if cfg.SeedBatchSize <= 0 {
		cfg.SeedBatchSize = 1000
	}
	if cfg.SeedConcurrency <= 0 {
		cfg.SeedConcurrency = 1
	}
	if cfg.InsertCacheSize <= 0 {
		cfg.InsertCacheSize = 1000
	}
	if cfg.OpTimeoutMs <= 0 {
		cfg.OpTimeoutMs = 500
	}
	if cfg.RetryAttempts <= 0 {
		cfg.RetryAttempts = 2
	}
	if cfg.RetryBackoffMs <= 0 {
		cfg.RetryBackoffMs = 5
	}
	if cfg.ExistingRecordHitRate < 0 {
		cfg.ExistingRecordHitRate = 0
	}
	if cfg.ExistingRecordHitRate > 100 {
		cfg.ExistingRecordHitRate = 100
	}
	if cfg.RecordPoolMaxSize <= 0 {
		cfg.RecordPoolMaxSize = 10000
	}
	if cfg.RecordPoolBootstrapSample < 0 {
		cfg.RecordPoolBootstrapSample = 0
	}
	if cfg.MaxTransactionOps <= 0 {
		cfg.MaxTransactionOps = 3
	}
	if cfg.ThinkTimeMs < 0 {
		cfg.ThinkTimeMs = 0
	}
	if cfg.ThinkJitterMs < 0 {
		cfg.ThinkJitterMs = 0
	}

	// --- RAW INJECTOR TAB ---
	if cfg.RawInjector.Type == "" {
		cfg.RawInjector.Type = "mixed"
	}
	if cfg.RawInjector.DocumentSize <= 0 {
		cfg.RawInjector.DocumentSize = 1024
	}
	if cfg.RawInjector.DBName == "" {
		cfg.RawInjector.DBName = "plgm_injector"
	}
	if cfg.RawInjector.CollectionName == "" {
		cfg.RawInjector.CollectionName = "injector_data"
	}
	if cfg.RawInjector.MaxDocs <= 0 {
		cfg.RawInjector.MaxDocs = 10000000
	}
	if cfg.RawInjector.BatchSize <= 0 {
		cfg.RawInjector.BatchSize = 1000
	}
}

func applyEnvOverrides(cfg *AppConfig) map[string]bool {
	overrides := make(map[string]bool)

	// Credentials & Basics
	if v := os.Getenv("PLGM_USERNAME"); v != "" {
		cfg.ConnectionParams.Username = v
	}
	if v := os.Getenv("PLGM_PASSWORD"); v != "" {
		cfg.ConnectionParams.Password = v
	}

	// Default Workload (Explicit Override)
	explicitDefault := false
	if v := os.Getenv("PLGM_DEFAULT_WORKLOAD"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.DefaultWorkload = b
			explicitDefault = true
		}
	}

	if envDebug := os.Getenv("PLGM_DEBUG_MODE"); envDebug != "" {
		if b, err := strconv.ParseBool(envDebug); err == nil {
			cfg.DebugMode = b
		}
	}

	if envPprof := os.Getenv("PLGM_PPROF_ENABLED"); envPprof != "" {
		if b, err := strconv.ParseBool(envPprof); err == nil {
			cfg.PprofEnabled = b
		}
	}

	if envURI := os.Getenv("PLGM_URI"); envURI != "" {
		cfg.URI = envURI
	}
	if v := os.Getenv("PLGM_DIRECT_CONNECTION"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.ConnectionParams.DirectConnection = b
		}
	}
	if v, exists := os.LookupEnv("PLGM_REPLICASET_NAME"); exists {
		cfg.ConnectionParams.ReplicaSetName = v
	}
	if v, exists := os.LookupEnv("PLGM_READ_PREFERENCE"); exists {
		cfg.ConnectionParams.ReadPreference = v
	}
	if v := os.Getenv("PLGM_TLS_CA_FILE"); v != "" {
		cfg.ConnectionParams.TLSCAFile = v
	}
	if v := os.Getenv("PLGM_TLS_CERTIFICATE_KEY_FILE"); v != "" {
		cfg.ConnectionParams.TLSCertificateKeyFile = v
	}

	// Paths
	hasCustomColl := false
	if envCollectionsPath := os.Getenv("PLGM_COLLECTIONS_PATH"); envCollectionsPath != "" {
		cfg.CollectionsPath = envCollectionsPath
		hasCustomColl = true
	}

	hasCustomQuery := false
	if envQueriesPath := os.Getenv("PLGM_QUERIES_PATH"); envQueriesPath != "" {
		cfg.QueriesPath = envQueriesPath
		hasCustomQuery = true
	}

	if !explicitDefault && hasCustomColl && hasCustomQuery {
		cfg.DefaultWorkload = false
	}

	// Booleans
	if envDrop := os.Getenv("PLGM_DROP_COLLECTIONS"); envDrop != "" {
		if b, err := strconv.ParseBool(envDrop); err == nil {
			cfg.DropCollections = b
		}
	}
	if envSkip := os.Getenv("PLGM_SKIP_SEED"); envSkip != "" {
		if b, err := strconv.ParseBool(envSkip); err == nil {
			cfg.SkipSeed = b
		}
	}
	if envTx := os.Getenv("PLGM_USE_TRANSACTIONS"); envTx != "" {
		if b, err := strconv.ParseBool(envTx); err == nil {
			cfg.UseTransactions = b
		}
	}

	// Integers
	if v := os.Getenv("PLGM_MAX_TRANSACTION_OPS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.MaxTransactionOps = n
		}
	}
	if envDocs := os.Getenv("PLGM_DOCUMENTS_COUNT"); envDocs != "" {
		if n, err := strconv.Atoi(envDocs); err == nil && n >= 0 {
			cfg.DocumentsCount = n
		}
	}
	if envConcurrency := os.Getenv("PLGM_CONCURRENCY"); envConcurrency != "" {
		if n, err := strconv.Atoi(envConcurrency); err == nil && n > 0 {
			cfg.Concurrency = n
		}
	}
	if envDuration := os.Getenv("PLGM_DURATION"); envDuration != "" {
		cfg.Duration = envDuration
	}

	if envIterations := os.Getenv("PLGM_ITERATIONS"); envIterations != "" {
		if n, err := strconv.Atoi(envIterations); err == nil && n > 0 {
			cfg.Iterations = n
		}
	}
	if envInterval := os.Getenv("PLGM_INTERVAL_DELAY"); envInterval != "" {
		cfg.IntervalDelay = envInterval
	}

	// Percentages
	if p := os.Getenv("PLGM_FIND_PERCENT"); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n >= 0 {
			cfg.FindPercent = n
			overrides["FindPercent"] = true
		}
	}
	if p := os.Getenv("PLGM_UPDATE_PERCENT"); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n >= 0 {
			cfg.UpdatePercent = n
			overrides["UpdatePercent"] = true
		}
	}
	if p := os.Getenv("PLGM_DELETE_PERCENT"); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n >= 0 {
			cfg.DeletePercent = n
			overrides["DeletePercent"] = true
		}
	}
	if p := os.Getenv("PLGM_INSERT_PERCENT"); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n >= 0 {
			cfg.InsertPercent = n
			overrides["InsertPercent"] = true
		}
	}
	if p := os.Getenv("PLGM_AGGREGATE_PERCENT"); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n >= 0 {
			cfg.AggregatePercent = n
			overrides["AggregatePercent"] = true
		}
	}
	if p := os.Getenv("PLGM_TRANSACTION_PERCENT"); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n >= 0 {
			cfg.TransactionPercent = n
			overrides["TransactionPercent"] = true
		}
	}
	if p := os.Getenv("PLGM_BULK_INSERT_PERCENT"); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n >= 0 {
			cfg.BulkInsertPercent = n
			overrides["BulkInsertPercent"] = true
		}
	}

	// Advanced Tuning
	if v := os.Getenv("PLGM_FIND_BATCH_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.FindBatchSize = n
		}
	}
	if v := os.Getenv("PLGM_FIND_LIMIT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.FindLimit = int64(n)
		}
	}
	if v := os.Getenv("PLGM_INSERT_CACHE_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.InsertCacheSize = n
		}
	}
	if v := os.Getenv("PLGM_OP_TIMEOUT_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.OpTimeoutMs = n
		}
	}
	if v := os.Getenv("PLGM_RETRY_ATTEMPTS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.RetryAttempts = n
		}
	}
	if v := os.Getenv("PLGM_RETRY_BACKOFF_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.RetryBackoffMs = n
		}
	}
	if v := os.Getenv("PLGM_STATUS_REFRESH_RATE_SEC"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.StatusRefreshRateSec = n
		}
	}
	if v := os.Getenv("PLGM_INSERT_BATCH_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.InsertBatchSize = n
		}
	}
	if v := os.Getenv("PLGM_SEED_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.SeedConcurrency = n
		}
	}
	if v := os.Getenv("PLGM_SEED_BATCH_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.SeedBatchSize = n
		}
	}
	if v := os.Getenv("PLGM_EXISTING_RECORD_HIT_RATE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 100 {
			cfg.ExistingRecordHitRate = n
		}
	}
	if v := os.Getenv("PLGM_RECORD_POOL_MAX_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.RecordPoolMaxSize = n
		}
	}
	if v := os.Getenv("PLGM_RECORD_POOL_BOOTSTRAP_SAMPLE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.RecordPoolBootstrapSample = n
		}
	}
	if v := os.Getenv("PLGM_RANDOM_SEED"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.RandomSeed = n
		}
	}
	if v := os.Getenv("PLGM_THINK_TIME_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.ThinkTimeMs = n
		}
	}
	if v := os.Getenv("PLGM_THINK_JITTER_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.ThinkJitterMs = n
		}
	}
	if v := os.Getenv("PLGM_ACCESS_PATTERN_KIND"); v != "" {
		cfg.AccessPattern.Kind = v
	}
	if v := os.Getenv("PLGM_ACCESS_PATTERN_ZIPFIAN_EXPONENT"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.AccessPattern.ZipfianExponent = f
		}
	}
	if v := os.Getenv("PLGM_ACCESS_PATTERN_HOTSPOT_PERCENT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.AccessPattern.HotspotPercent = n
		}
	}
	if v := os.Getenv("PLGM_ACCESS_PATTERN_HOTSPOT_TRAFFIC_PERCENT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.AccessPattern.HotspotTrafficPercent = n
		}
	}
	// --- Load Profile Overrides ---
	// Full profile shaping is configured via YAML or the web UI; the env knobs
	// cover the most common CI use case (selecting a kind and fixed worker count).
	if v := os.Getenv("PLGM_LOAD_PROFILE_KIND"); v != "" {
		cfg.LoadProfile.Kind = v
	}
	if v := os.Getenv("PLGM_LOAD_PROFILE_WORKERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.LoadProfile.Workers = n
		}
	}

	// --- RawInjector Overrides ---
	if v := os.Getenv("PLGM_INJECTOR"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.RawInjector.Enabled = b
		}
	}
	if v := os.Getenv("PLGM_INJECTOR_TYPE"); v != "" {
		cfg.RawInjector.Type = v
	}
	if v := os.Getenv("PLGM_INJECTOR_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.RawInjector.DocumentSize = n
		}
	}
	if v := os.Getenv("PLGM_INJECTOR_DB"); v != "" {
		cfg.RawInjector.DBName = v
	}
	if v := os.Getenv("PLGM_INJECTOR_COLLECTION"); v != "" {
		cfg.RawInjector.CollectionName = v
	}
	if v := os.Getenv("PLGM_INJECTOR_MAX_DOCS"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			cfg.RawInjector.MaxDocs = n
		}
	}
	if v := os.Getenv("PLGM_INJECTOR_BATCH_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.RawInjector.BatchSize = n
		}
	}
	if v := os.Getenv("PLGM_INJECTOR_DROP"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.RawInjector.DropCollection = b
		}
	}

	// --- Web UI Overrides ---
	if v := os.Getenv("PLGM_WEBUI_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.WebUI.Enabled = b
		}
	}
	if v := os.Getenv("PLGM_WEBUI_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.WebUI.Port = n
		}
	}
	if v := os.Getenv("PLGM_WEBUI_TLS"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.WebUI.TLSEnabled = b
		}
	}
	if v := os.Getenv("PLGM_WEBUI_TLS_CERT"); v != "" {
		cfg.WebUI.TLSCertFile = v
	}
	if v := os.Getenv("PLGM_WEBUI_TLS_KEY"); v != "" {
		cfg.WebUI.TLSKeyFile = v
	}

	// --- Insights Overrides ---
	if v := os.Getenv("PLGM_INSIGHTS_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.InsightsEnabled = b
		}
	}
	if v := os.Getenv("PLGM_INSIGHTS_SAMPLING_RATE"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && f <= 1 {
			cfg.InsightsSamplingRate = f
		}
	}
	if v := os.Getenv("PLGM_INSIGHTS_SLOW_THRESHOLD_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.InsightsSlowThresholdMs = n
		}
	}
	if v := os.Getenv("PLGM_INSIGHTS_MAX_EVENTS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.InsightsMaxEvents = n
		}
	}
	if v := os.Getenv("PLGM_INSIGHTS_MAX_GROUPS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.InsightsMaxGroups = n
		}
	}
	if v := os.Getenv("PLGM_INSIGHTS_EXPLAIN_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.InsightsExplainEnabled = b
		}
	}
	if v := os.Getenv("PLGM_INSIGHTS_EXPLAIN_TOP_N"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.InsightsExplainTopN = n
		}
	}
	if v := os.Getenv("PLGM_INSIGHTS_EXPLAIN_MAX_TIME_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.InsightsExplainMaxTimeMS = n
		}
	}
	if v := os.Getenv("PLGM_INSIGHTS_EXPLAIN_VERBOSITY"); v != "" {
		cfg.InsightsExplainVerbosity = v
	}
	if v := os.Getenv("PLGM_INSIGHTS_EXPLAIN_SEVERITY_MODE"); v != "" {
		cfg.InsightsExplainSeverityMode = v
	}
	if v := os.Getenv("PLGM_INSIGHTS_EXPLAIN_EFFICIENCY_MODE"); v != "" {
		cfg.InsightsExplainEfficiencyMode = v
	}
	if v := os.Getenv("PLGM_INSIGHTS_EXPLAIN_WORKERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.InsightsExplainWorkers = n
		}
	}
	if v := os.Getenv("PLGM_INSIGHTS_EXPLAIN_RETRIES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.InsightsExplainRetries = n
		}
	}
	if v := os.Getenv("PLGM_INSIGHTS_EXPLAIN_BACKOFF_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.InsightsExplainBackoffMS = n
		}
	}
	for env, dst := range map[string]*float64{
		"PLGM_INSIGHTS_LATENCY_TARGET_AVG_MS": &cfg.InsightsLatencyTargetAvgMs,
		"PLGM_INSIGHTS_LATENCY_TARGET_P95_MS": &cfg.InsightsLatencyTargetP95Ms,
		"PLGM_INSIGHTS_LATENCY_TARGET_P99_MS": &cfg.InsightsLatencyTargetP99Ms,
	} {
		if v := os.Getenv(env); v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
				*dst = f
			}
		}
	}
	// --- Trend History Overrides ---
	if v := os.Getenv("PLGM_TREND_HISTORY_RUNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= MinTrendHistoryRuns {
			cfg.TrendHistoryRuns = n
		}
	}
	if v := os.Getenv("PLGM_TREND_MAX_AGE_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.TrendMaxAgeDays = &n
		}
	}
	if v := os.Getenv("PLGM_TREND_BASELINE_STORE"); v != "" {
		cfg.TrendBaselineStore = v
	}
	if v := os.Getenv("PLGM_TREND_DEFAULT_COMPARISON"); v != "" {
		cfg.TrendDefaultComparison = v
	}
	if v := os.Getenv("PLGM_TREND_RUN_LABEL"); v != "" {
		cfg.TrendRunLabel = v
	}
	if v := os.Getenv("PLGM_INSIGHTS_TREND_STORE_QUERY_VALUES"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.InsightsTrendStoreQueryValues = b
		}
	}
	if v := os.Getenv("PLGM_WORKLOAD_RECORD_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.WorkloadRecordEnabled = b
		}
	}
	if v := os.Getenv("PLGM_WORKLOAD_RECORD_PATH"); v != "" {
		cfg.WorkloadRecordPath = v
	}
	if v := os.Getenv("PLGM_WORKLOAD_RECORD_MAX_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			cfg.WorkloadRecordMaxBytes = n
		}
	}
	if v := os.Getenv("PLGM_WORKLOAD_RECORD_BACKPRESSURE"); v != "" {
		cfg.WorkloadRecordBackpressure = v
	}
	if v := os.Getenv("PLGM_WORKLOAD_RECORD_COMPRESSION"); v != "" {
		cfg.WorkloadRecordCompression = v
	}
	if v := os.Getenv("PLGM_WORKLOAD_REPLAY_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.WorkloadReplayEnabled = b
		}
	}
	if v := os.Getenv("PLGM_WORKLOAD_REPLAY_PATH"); v != "" {
		cfg.WorkloadReplayPath = v
	}
	if v := os.Getenv("PLGM_WORKLOAD_REPLAY_MODE"); v != "" {
		cfg.WorkloadReplayMode = v
	}
	if v := os.Getenv("PLGM_WORKLOAD_REPLAY_ALLOW_MISMATCH"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.WorkloadReplayAllowMismatch = b
		}
	}
	if v := os.Getenv("PLGM_WORKLOAD_DETERMINISTIC"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.WorkloadDeterministic = b
		}
	}
	if v := os.Getenv("PLGM_OPERATIONS_PER_WORKER"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.OperationsPerWorker = n
		}
	}
	if v := os.Getenv("PLGM_WORKLOAD_RECORD_OUTCOMES"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.WorkloadRecordOutcomes = b
		}
	}
	if v := os.Getenv("PLGM_WORKLOAD_RECORD_RESULT_CHECKSUMS"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.WorkloadRecordResultChecksums = b
		}
	}
	if v := os.Getenv("PLGM_WORKLOAD_REPLAY_OUTCOMES_PATH"); v != "" {
		cfg.WorkloadReplayOutcomesPath = v
	}
	if v := os.Getenv("PLGM_WORKLOAD_DETERMINISTIC_OUTCOMES_PATH"); v != "" {
		cfg.WorkloadDeterministicOutcomesPath = v
	}
	if v := os.Getenv("PLGM_WORKLOAD_REPLAY_TARGETS"); v != "" {
		cfg.WorkloadReplayTargets = ParseReplayTargets(v)
	}
	if v := os.Getenv("PLGM_WORKLOAD_REPLAY_TARGETS_MODE"); v != "" {
		cfg.WorkloadReplayTargetsMode = v
	}
	if v := os.Getenv("PLGM_WORKLOAD_REPLAY_OPS"); v != "" {
		cfg.WorkloadReplayOps = v
	}
	if v := os.Getenv("PLGM_WORKLOAD_DATA_FINGERPRINT"); v != "" {
		cfg.WorkloadDataFingerprint = v
	}
	if v := os.Getenv("PLGM_SHARDING_MODE"); v != "" {
		cfg.ShardingMode = v
	}
	if v := os.Getenv("PLGM_SHARDING_SKIP_GENERIC_WITHOUT_CONFIG"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.ShardingSkipGenericWithoutConfig = b
		}
	}

	return overrides
}

func ValidateShardingConfig(cfg *AppConfig) error {
	if cfg == nil {
		return nil
	}
	if cfg.ShardingMode == "force_on" && cfg.ShardingSkipGenericWithoutConfig {
		return fmt.Errorf("invalid sharding configuration: sharding_mode=force_on conflicts with sharding_skip_generic_without_config=true")
	}
	return nil
}

// ValidateLoadProfile compiles the configured load profile to surface invalid
// durations, negative worker counts, or malformed step definitions early.
func ValidateLoadProfile(cfg *AppConfig) error {
	if cfg == nil {
		return nil
	}
	fallback := cfg.Concurrency
	if fallback < 1 {
		fallback = 1
	}
	if _, err := loadprofile.Compile(cfg.LoadProfile, fallback); err != nil {
		return fmt.Errorf("invalid load profile: %w", err)
	}
	return nil
}

// ValidateAccessPattern compiles the configured access pattern to surface
// invalid distribution kinds or out-of-range parameters early.
func ValidateAccessPattern(cfg *AppConfig) error {
	if cfg == nil {
		return nil
	}
	if _, err := accesspattern.Compile(cfg.AccessPattern); err != nil {
		return fmt.Errorf("invalid access pattern: %w", err)
	}
	return nil
}

// ValidateConnectionParams catches incomplete TLS client configuration before
// we attempt to build the MongoDB URI or open a connection.
func ValidateConnectionParams(cfg *AppConfig) error {
	if cfg == nil {
		return nil
	}
	hasCA := strings.TrimSpace(cfg.ConnectionParams.TLSCAFile) != ""
	hasCert := strings.TrimSpace(cfg.ConnectionParams.TLSCertificateKeyFile) != ""
	if hasCA != hasCert {
		return fmt.Errorf("invalid TLS connection configuration: tls_ca_file and tlsCertificateKeyFile must be set together")
	}
	return nil
}

const (
	// ExplainEfficiencyModeDefault adds insights_explain_top_n explain slots
	// filled alternately by workload impact and by round-robin over latency tiers.
	ExplainEfficiencyModeDefault = "impact_and_latency_tiers"
	// ExplainEfficiencyModeOff keeps only the severity-filtered explain slots.
	ExplainEfficiencyModeOff = "off"
)

func NormalizeExplainEfficiencyMode(mode string) string {
	if strings.ToLower(strings.TrimSpace(mode)) == ExplainEfficiencyModeOff {
		return ExplainEfficiencyModeOff
	}
	return ExplainEfficiencyModeDefault
}

const (
	DefaultTrendHistoryRuns   = 5
	MinTrendHistoryRuns       = 1
	MaxTrendHistoryRuns       = 50
	DefaultTrendMaxAgeDays    = 30
	MaxTrendMaxAgeDays        = 3650
	TrendComparisonPrevious   = "previous"
	TrendComparisonPinned     = "pinned"
	MaxTrendRunLabelBytes     = 64
	MaxTrendBaselineStorePath = 1024
)

// NormalizeTrendSettings applies the trend history defaults and bounds: an
// unset or non-positive run count uses the default and a larger one is capped;
// a negative or unset age uses the default (0 = never outdated); an unknown
// comparison is "previous".
func NormalizeTrendSettings(cfg *AppConfig) {
	switch {
	case cfg.TrendHistoryRuns < MinTrendHistoryRuns:
		cfg.TrendHistoryRuns = DefaultTrendHistoryRuns
	case cfg.TrendHistoryRuns > MaxTrendHistoryRuns:
		cfg.TrendHistoryRuns = MaxTrendHistoryRuns
	}
	days := DefaultTrendMaxAgeDays
	if cfg.TrendMaxAgeDays != nil && *cfg.TrendMaxAgeDays >= 0 {
		days = *cfg.TrendMaxAgeDays
		if days > MaxTrendMaxAgeDays {
			days = MaxTrendMaxAgeDays
		}
	}
	cfg.TrendMaxAgeDays = &days
	cfg.TrendDefaultComparison = NormalizeTrendComparison(cfg.TrendDefaultComparison)
	cfg.TrendRunLabel = SanitizeTrendRunLabel(cfg.TrendRunLabel)
	cfg.TrendBaselineStore = strings.TrimSpace(cfg.TrendBaselineStore)
	if len(cfg.TrendBaselineStore) > MaxTrendBaselineStorePath || strings.ContainsAny(cfg.TrendBaselineStore, "\x00\r\n") {
		cfg.TrendBaselineStore = ""
	}
}

func NormalizeTrendComparison(mode string) string {
	if strings.ToLower(strings.TrimSpace(mode)) == TrendComparisonPinned {
		return TrendComparisonPinned
	}
	return TrendComparisonPrevious
}

// SanitizeTrendRunLabel trims a run label, drops control characters, and
// bounds it to MaxTrendRunLabelBytes without splitting a UTF-8 character.
func SanitizeTrendRunLabel(label string) string {
	label = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(label))
	if len(label) <= MaxTrendRunLabelBytes {
		return strings.TrimSpace(label)
	}
	cut := MaxTrendRunLabelBytes
	for cut > 0 && (label[cut]&0xC0) == 0x80 {
		cut--
	}
	return strings.TrimSpace(label[:cut])
}

func NormalizeShardingMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "force_on":
		return "force_on"
	case "force_off":
		return "force_off"
	default:
		return "auto"
	}
}

func normalizePercentages(cfg *AppConfig, pinned map[string]bool) {
	if !cfg.UseTransactions {
		cfg.TransactionPercent = 0
		delete(pinned, "TransactionPercent")
	}

	pinnedTotal := 0
	if pinned["FindPercent"] {
		pinnedTotal += cfg.FindPercent
	}
	if pinned["UpdatePercent"] {
		pinnedTotal += cfg.UpdatePercent
	}
	if pinned["DeletePercent"] {
		pinnedTotal += cfg.DeletePercent
	}
	if pinned["InsertPercent"] {
		pinnedTotal += cfg.InsertPercent
	}
	if pinned["AggregatePercent"] {
		pinnedTotal += cfg.AggregatePercent
	}
	if pinned["TransactionPercent"] {
		pinnedTotal += cfg.TransactionPercent
	}
	if pinned["BulkInsertPercent"] {
		pinnedTotal += cfg.BulkInsertPercent
	}

	if pinnedTotal >= 100 {
		if !pinned["FindPercent"] {
			cfg.FindPercent = 0
		}
		if !pinned["UpdatePercent"] {
			cfg.UpdatePercent = 0
		}
		if !pinned["DeletePercent"] {
			cfg.DeletePercent = 0
		}
		if !pinned["InsertPercent"] {
			cfg.InsertPercent = 0
		}
		if !pinned["AggregatePercent"] {
			cfg.AggregatePercent = 0
		}
		if !pinned["TransactionPercent"] {
			cfg.TransactionPercent = 0
		}
		if !pinned["BulkInsertPercent"] {
			cfg.BulkInsertPercent = 0
		}

		if pinnedTotal > 100 {
			factor := 100.0 / float64(pinnedTotal)
			if pinned["FindPercent"] {
				cfg.FindPercent = int(float64(cfg.FindPercent) * factor)
			}
			if pinned["UpdatePercent"] {
				cfg.UpdatePercent = int(float64(cfg.UpdatePercent) * factor)
			}
			if pinned["DeletePercent"] {
				cfg.DeletePercent = int(float64(cfg.DeletePercent) * factor)
			}
			if pinned["InsertPercent"] {
				cfg.InsertPercent = int(float64(cfg.InsertPercent) * factor)
			}
			if pinned["AggregatePercent"] {
				cfg.AggregatePercent = int(float64(cfg.AggregatePercent) * factor)
			}
			if pinned["TransactionPercent"] {
				cfg.TransactionPercent = int(float64(cfg.TransactionPercent) * factor)
			}
			if pinned["BulkInsertPercent"] {
				cfg.BulkInsertPercent = int(float64(cfg.BulkInsertPercent) * factor)
			}
		}

	} else {
		remaining := 100 - pinnedTotal
		unpinnedTotal := 0
		if !pinned["FindPercent"] {
			unpinnedTotal += cfg.FindPercent
		}
		if !pinned["UpdatePercent"] {
			unpinnedTotal += cfg.UpdatePercent
		}
		if !pinned["DeletePercent"] {
			unpinnedTotal += cfg.DeletePercent
		}
		if !pinned["InsertPercent"] {
			unpinnedTotal += cfg.InsertPercent
		}
		if !pinned["AggregatePercent"] {
			unpinnedTotal += cfg.AggregatePercent
		}
		if !pinned["TransactionPercent"] {
			unpinnedTotal += cfg.TransactionPercent
		}
		if !pinned["BulkInsertPercent"] {
			unpinnedTotal += cfg.BulkInsertPercent
		}

		if unpinnedTotal > 0 {
			factor := float64(remaining) / float64(unpinnedTotal)

			if !pinned["FindPercent"] {
				cfg.FindPercent = int(float64(cfg.FindPercent) * factor)
			}
			if !pinned["UpdatePercent"] {
				cfg.UpdatePercent = int(float64(cfg.UpdatePercent) * factor)
			}
			if !pinned["DeletePercent"] {
				cfg.DeletePercent = int(float64(cfg.DeletePercent) * factor)
			}
			if !pinned["InsertPercent"] {
				cfg.InsertPercent = int(float64(cfg.InsertPercent) * factor)
			}
			if !pinned["AggregatePercent"] {
				cfg.AggregatePercent = int(float64(cfg.AggregatePercent) * factor)
			}
			if !pinned["TransactionPercent"] {
				cfg.TransactionPercent = int(float64(cfg.TransactionPercent) * factor)
			}
			if !pinned["BulkInsertPercent"] {
				cfg.BulkInsertPercent = int(float64(cfg.BulkInsertPercent) * factor)
			}
		} else {
			cfg.FindPercent += remaining
		}
	}

	finalTotal := cfg.FindPercent + cfg.UpdatePercent + cfg.DeletePercent + cfg.InsertPercent + cfg.AggregatePercent + cfg.TransactionPercent + cfg.BulkInsertPercent
	if finalTotal != 100 {
		cfg.FindPercent += (100 - finalTotal)
	}
}
