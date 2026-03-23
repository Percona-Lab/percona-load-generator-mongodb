package config

import (
	"fmt"
	"os"
	"strconv"

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
	UseTransactions    bool   `yaml:"use_transactions"`
	MaxTransactionOps  int    `yaml:"max_transaction_ops"`
	DebugMode          bool   `yaml:"debug_mode"`

	FindBatchSize         int   `yaml:"find_batch_size"`
	FindLimit             int64 `yaml:"find_limit"`
	UseFindOneForLimitOne bool  `yaml:"use_findone_for_limit_one"`
	InsertCacheSize       int   `yaml:"insert_cache_size"`
	StatusRefreshRateSec  int   `yaml:"status_refresh_rate_sec"`
	OpTimeoutMs           int   `yaml:"op_timeout_ms"`
	RetryAttempts         int   `yaml:"retry_attempts"`
	RetryBackoffMs        int   `yaml:"retry_backoff_ms"`

	ConnectionParams ConnectionParams       `yaml:"connection_params"`
	CustomParamsMap  map[string]interface{} `yaml:"custom_params"`
	Debug            bool                   `yaml:"debug"`
	WebUI            WebUIConfig            `yaml:"web_ui"`
	RawInjector      RawInjectorConfig      `yaml:"raw_injector"`

	CSVExportEnabled bool   `yaml:"csv_export_enabled"`
	CSVExportAppend  bool   `yaml:"csv_export_append"`
	CSVExportPath    string `yaml:"csv_export_path"`
}

type WebUIConfig struct {
	Enabled bool `yaml:"enabled"`
	Port    int  `yaml:"port"`
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
}

func LoadAppConfig(path string, isWebUI bool) (*AppConfig, error) {
	cfg := &AppConfig{}
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
	cfg.FindPercent = 50
	cfg.UpdatePercent = 20
	cfg.DeletePercent = 10
	cfg.InsertPercent = 20
	cfg.BulkInsertPercent = 0
	cfg.AggregatePercent = 0
	cfg.TransactionPercent = 0

	// --- EXPORT DEFAULTS ---
	cfg.CSVExportEnabled = false
	cfg.CSVExportAppend = false
	cfg.CSVExportPath = "plgm_metrics_export.csv"
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
	if cfg.MaxTransactionOps <= 0 {
		cfg.MaxTransactionOps = 3
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
	if v := os.Getenv("PLGM_SEED_BATCH_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.SeedBatchSize = n
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

	return overrides
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
