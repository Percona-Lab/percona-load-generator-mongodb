package webui

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"embed"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/benchmark"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/db"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/mongo"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	"go.mongodb.org/mongo-driver/v2/bson"
	mongodrv "go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

//go:embed static
var staticFiles embed.FS

type WebServer struct {
	mu               sync.Mutex
	IsRunning        bool
	LastError        string
	CurrentStats     *stats.Collector
	ActiveCancel     context.CancelFunc
	AppConfig        *config.AppConfig
	CurrentIteration int
	TotalIterations  int
	IsWaiting        bool
	IntervalStr      string
	RunID            int64
	InsightsCache    *stats.InsightsReport
	ShapeTrendBase   map[string]float64
}

var (
	loadCollectionsFn   = config.LoadCollections
	loadQueriesFn       = config.LoadQueries
	connectFn           = db.Connect
	disconnectFn        = func(c *db.Connection, ctx context.Context) { c.Disconnect(ctx) }
	createCollectionsFn = mongo.CreateCollectionsFromConfig
	createIndexesFn     = mongo.CreateIndexesFromConfig
	insertRandomDocsFn  = mongo.InsertRandomDocuments
	runWorkloadFn       = mongo.RunWorkload
	runRawInjectorFn    = benchmark.RunRawInjector
)

func NewServer(cfg *config.AppConfig) *WebServer {
	return &WebServer{
		AppConfig:      cfg,
		ShapeTrendBase: make(map[string]float64),
	}
}

func openBrowser(url string) error {
	var cmd string
	var args []string

	switch runtime.GOOS {
	case "windows":
		cmd = "cmd"
		args = []string{"/c", "start"}
	case "darwin": // Mac
		cmd = "open"
	default: // Linux, FreeBSD, etc.
		cmd = "xdg-open"
	}

	args = append(args, url)
	return exec.Command(cmd, args...).Run()
}

func (s *WebServer) Start(port int) error {
	mux := http.NewServeMux()

	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return fmt.Errorf("failed to load static files: %v", err)
	}

	mux.Handle("/", http.FileServer(http.FS(staticFS)))
	mux.HandleFunc("/api/config", s.handleGetConfig)
	mux.HandleFunc("/api/start", s.handleStart)
	mux.HandleFunc("/api/stop", s.handleStop)
	mux.HandleFunc("/api/stats", s.handleStats)
	mux.HandleFunc("/api/insights", s.handleInsights)
	mux.HandleFunc("/api/shutdown", s.handleShutdown)

	cert, err := generateSelfSignedCert()
	if err != nil {
		return err
	}

	server := &http.Server{
		Addr:      fmt.Sprintf("127.0.0.1:%d", port),
		Handler:   mux,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
	}

	url := fmt.Sprintf("https://127.0.0.1:%d/", port)
	log.Printf("Starting SECURE Web UI on %s\n", url)

	go func() {
		time.Sleep(500 * time.Millisecond)
		openBrowser(url)
	}()

	return server.ListenAndServeTLS("", "")
}

func (s *WebServer) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	safeCfg := *s.AppConfig
	safeCfg.ConnectionParams.Password = ""
	json.NewEncoder(w).Encode(safeCfg)
}

func parseInt(val string, defaultVal int) int {
	if val == "" {
		return defaultVal
	}
	parsed, err := strconv.Atoi(val)
	if err != nil {
		return defaultVal
	}
	return parsed
}

func parseFloat(val string, defaultVal float64) float64 {
	if val == "" {
		return defaultVal
	}
	parsed, err := strconv.ParseFloat(val, 64)
	if err != nil {
		return defaultVal
	}
	return parsed
}

func (s *WebServer) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.mu.Lock()
	if s.IsRunning {
		s.mu.Unlock()
		http.Error(w, "Workload is already running", http.StatusConflict)
		return
	}
	s.IsRunning = true
	s.RunID++
	s.InsightsCache = nil

	baseCfg := *s.AppConfig
	s.mu.Unlock()

	err := r.ParseMultipartForm(10 << 20)
	if err != nil {
		s.abortRun("Failed to parse form")
		http.Error(w, "Failed to parse form", http.StatusBadRequest)
		return
	}

	cfg := &baseCfg
	cfg.WebUI.Enabled = true

	if v := r.FormValue("uri"); v != "" {
		cfg.URI = v
	}
	if v := r.FormValue("username"); v != "" {
		cfg.ConnectionParams.Username = v
	}
	if v := r.FormValue("auth_source"); v != "" {
		cfg.ConnectionParams.AuthSource = v
	}
	if v := r.FormValue("replicaset_name"); v != "" {
		cfg.ConnectionParams.ReplicaSetName = v
	}
	if v := r.FormValue("read_preference"); v != "" {
		cfg.ConnectionParams.ReadPreference = v
	}
	if uiPassword := r.FormValue("password"); uiPassword != "" {
		cfg.ConnectionParams.Password = uiPassword
	}

	cfg.ConnectionParams.DirectConnection = r.FormValue("direct_connection") == "true" || r.FormValue("direct_connection") == "on"
	cfg.DefaultWorkload = r.FormValue("default_workload") == "true" || r.FormValue("default_workload") == "on"
	cfg.DropCollections = r.FormValue("drop_collections") == "true" || r.FormValue("drop_collections") == "on"
	cfg.SkipSeed = r.FormValue("skip_seed") == "true" || r.FormValue("skip_seed") == "on"
	cfg.UseTransactions = r.FormValue("use_transactions") == "true" || r.FormValue("use_transactions") == "on"
	cfg.DebugMode = r.FormValue("debug_mode") == "true" || r.FormValue("debug_mode") == "on"

	cfg.CSVExportEnabled = r.FormValue("csv_export_enabled") == "true" || r.FormValue("csv_export_enabled") == "on"
	cfg.CSVExportAppend = r.FormValue("csv_export_append") == "true" || r.FormValue("csv_export_append") == "on"
	if v := r.FormValue("csv_export_path"); v != "" {
		cfg.CSVExportPath = v
	}
	cfg.InsightsEnabled = r.FormValue("insights_enabled") == "true" || r.FormValue("insights_enabled") == "on"
	cfg.InsightsSamplingRate = parseFloat(r.FormValue("insights_sampling_rate"), cfg.InsightsSamplingRate)
	cfg.InsightsSlowThresholdMs = parseInt(r.FormValue("insights_slow_threshold_ms"), cfg.InsightsSlowThresholdMs)
	cfg.InsightsMaxEvents = parseInt(r.FormValue("insights_max_events"), cfg.InsightsMaxEvents)
	cfg.InsightsMaxGroups = parseInt(r.FormValue("insights_max_groups"), cfg.InsightsMaxGroups)
	cfg.InsightsExplainEnabled = r.FormValue("insights_explain_enabled") == "true" || r.FormValue("insights_explain_enabled") == "on"
	cfg.InsightsExplainTopN = parseInt(r.FormValue("insights_explain_top_n"), cfg.InsightsExplainTopN)
	cfg.InsightsExplainMaxTimeMS = parseInt(r.FormValue("insights_explain_max_time_ms"), cfg.InsightsExplainMaxTimeMS)

	cfg.ConnectionParams.ConnectionTimeout = parseInt(r.FormValue("connection_timeout"), cfg.ConnectionParams.ConnectionTimeout)
	cfg.ConnectionParams.ServerSelectionTimeout = parseInt(r.FormValue("server_selection_timeout"), cfg.ConnectionParams.ServerSelectionTimeout)
	cfg.ConnectionParams.MaxPoolSize = parseInt(r.FormValue("max_pool_size"), cfg.ConnectionParams.MaxPoolSize)
	cfg.ConnectionParams.MinPoolSize = parseInt(r.FormValue("min_pool_size"), cfg.ConnectionParams.MinPoolSize)
	cfg.ConnectionParams.MaxIdleTime = parseInt(r.FormValue("max_idle_time"), cfg.ConnectionParams.MaxIdleTime)

	cfg.DocumentsCount = parseInt(r.FormValue("documents_count"), cfg.DocumentsCount)
	cfg.Concurrency = parseInt(r.FormValue("concurrency"), cfg.Concurrency)
	if d := r.FormValue("duration"); d != "" {
		cfg.Duration = d
	}
	cfg.MaxTransactionOps = parseInt(r.FormValue("max_transaction_ops"), cfg.MaxTransactionOps)
	cfg.Iterations = parseInt(r.FormValue("iterations"), cfg.Iterations)
	if v := r.FormValue("interval_delay"); v != "" {
		cfg.IntervalDelay = v
	}

	cfg.FindPercent = parseInt(r.FormValue("find_percent"), cfg.FindPercent)
	cfg.UpdatePercent = parseInt(r.FormValue("update_percent"), cfg.UpdatePercent)
	cfg.DeletePercent = parseInt(r.FormValue("delete_percent"), cfg.DeletePercent)
	cfg.InsertPercent = parseInt(r.FormValue("insert_percent"), cfg.InsertPercent)
	cfg.BulkInsertPercent = parseInt(r.FormValue("bulk_insert_percent"), cfg.BulkInsertPercent)
	cfg.AggregatePercent = parseInt(r.FormValue("aggregate_percent"), cfg.AggregatePercent)
	cfg.TransactionPercent = parseInt(r.FormValue("transaction_percent"), cfg.TransactionPercent)

	cfg.FindBatchSize = parseInt(r.FormValue("find_batch_size"), cfg.FindBatchSize)
	cfg.FindLimit = int64(parseInt(r.FormValue("find_limit"), int(cfg.FindLimit)))
	cfg.UseFindOneForLimitOne = r.FormValue("use_findone_for_limit_one") == "true" || r.FormValue("use_findone_for_limit_one") == "on"
	cfg.InsertCacheSize = parseInt(r.FormValue("insert_cache_size"), cfg.InsertCacheSize)
	cfg.InsertBatchSize = parseInt(r.FormValue("insert_batch_size"), cfg.InsertBatchSize)
	cfg.SeedBatchSize = parseInt(r.FormValue("seed_batch_size"), cfg.SeedBatchSize)
	cfg.StatusRefreshRateSec = parseInt(r.FormValue("status_refresh_rate_sec"), cfg.StatusRefreshRateSec)
	cfg.OpTimeoutMs = parseInt(r.FormValue("op_timeout_ms"), cfg.OpTimeoutMs)
	cfg.RetryAttempts = parseInt(r.FormValue("retry_attempts"), cfg.RetryAttempts)
	cfg.RetryBackoffMs = parseInt(r.FormValue("retry_backoff_ms"), cfg.RetryBackoffMs)

	cfg.RawInjector.Enabled = r.FormValue("raw_injector_enabled") == "true" || r.FormValue("raw_injector_enabled") == "on"
	if t := r.FormValue("raw_injector_type"); t != "" {
		cfg.RawInjector.Type = t
	}
	cfg.RawInjector.DropCollection = r.FormValue("raw_injector_drop") == "true" || r.FormValue("raw_injector_drop") == "on"
	cfg.RawInjector.DocumentSize = parseInt(r.FormValue("raw_injector_doc_size"), cfg.RawInjector.DocumentSize)
	cfg.RawInjector.MaxDocs = int64(parseInt(r.FormValue("raw_injector_max_docs"), int(cfg.RawInjector.MaxDocs)))
	cfg.RawInjector.BatchSize = parseInt(r.FormValue("raw_injector_batch_size"), cfg.RawInjector.BatchSize)
	if dbn := r.FormValue("raw_injector_db"); dbn != "" {
		cfg.RawInjector.DBName = dbn
	}
	if coln := r.FormValue("raw_injector_coll"); coln != "" {
		cfg.RawInjector.CollectionName = coln
	}

	var customCollections *config.CollectionsFile
	var customQueries *config.QueriesFile

	if !cfg.DefaultWorkload {
		collFile, _, err := r.FormFile("collections_file")
		if err == nil {
			defer collFile.Close()
			b, _ := io.ReadAll(collFile)
			var wrapped config.CollectionsFile
			if err := json.Unmarshal(b, &wrapped); err == nil && len(wrapped.Collections) > 0 {
				customCollections = &wrapped
			} else {
				var arr []config.CollectionDefinition
				if err := json.Unmarshal(b, &arr); err == nil && len(arr) > 0 {
					customCollections = &config.CollectionsFile{Collections: arr}
				}
			}

			if customCollections != nil {
				for i, col := range customCollections.Collections {
					if col.DatabaseName == "" || col.Name == "" {
						s.abortRun(fmt.Sprintf("Loaded collection at index %d has empty 'database' or 'collection' name.", i))
						http.Error(w, "Invalid collections format: missing db or collection name", http.StatusBadRequest)
						return
					}
				}
			} else {
				s.abortRun("Failed to parse custom collections_file")
				http.Error(w, "Invalid collections format", http.StatusBadRequest)
				return
			}
		}

		queryFile, _, err := r.FormFile("queries_file")
		if err == nil {
			defer queryFile.Close()
			b, _ := io.ReadAll(queryFile)
			var defs []config.QueryDefinition
			if err := json.Unmarshal(b, &defs); err == nil && len(defs) > 0 {
				customQueries = &config.QueriesFile{Queries: defs}
			} else {
				s.abortRun("Failed to parse custom queries_file")
				http.Error(w, "Invalid queries format", http.StatusBadRequest)
				return
			}
		}
	}

	if cfg.StatusRefreshRateSec <= 0 {
		cfg.StatusRefreshRateSec = 1
	}
	if cfg.OpTimeoutMs <= 0 {
		cfg.OpTimeoutMs = 500
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	if cfg.FindBatchSize <= 0 {
		cfg.FindBatchSize = 10
	}
	if cfg.InsertBatchSize <= 0 {
		cfg.InsertBatchSize = 10
	}
	if cfg.InsertCacheSize <= 0 {
		cfg.InsertCacheSize = 1000
	}
	if cfg.SeedBatchSize <= 0 {
		cfg.SeedBatchSize = 1000
	}
	if cfg.MaxTransactionOps <= 0 {
		cfg.MaxTransactionOps = 1
	}
	if cfg.Duration == "" {
		cfg.Duration = "10s"
	}

	ctx, cancel := context.WithCancel(context.Background())

	// -----------------------------------------------------------------------
	// EXECUTION BRANCH 1: RAW INJECTOR
	// -----------------------------------------------------------------------
	if cfg.RawInjector.Enabled {
		dbName := cfg.RawInjector.DBName
		if dbName == "" {
			dbName = "plgm_injector"
		}

		benchConn, connectErr := connectFn(ctx, cfg, dbName)
		if connectErr != nil {
			s.abortRun("Database connection failed: " + connectErr.Error())
			cancel()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"status": "error", "message": connectErr.Error()})
			return
		}

		s.mu.Lock()
		s.CurrentStats = stats.NewCollector()
		s.CurrentStats.ConfigureInsights(cfg)
		s.AppConfig = cfg
		s.ActiveCancel = cancel
		s.LastError = ""
		s.CurrentIteration = 1
		s.TotalIterations = cfg.Iterations
		s.IntervalStr = cfg.IntervalDelay
		s.IsWaiting = false
		s.CurrentStats.CurrentIteration = 1
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "started"})

		go func() {
			defer disconnectFn(benchConn, context.Background())
			defer func() {
				s.mu.Lock()
				s.IsRunning = false
				s.mu.Unlock()
			}()

			intervalDuration, _ := time.ParseDuration(cfg.IntervalDelay)

			for i := 1; i <= cfg.Iterations; i++ {
				if ctx.Err() != nil {
					break
				}

				s.mu.Lock()
				s.CurrentIteration = i
				s.IsWaiting = false
				s.CurrentStats.CurrentIteration = i
				s.mu.Unlock()

				if err := runRawInjectorFn(ctx, benchConn.Database, cfg, s.CurrentStats); err != nil {
					if err != context.Canceled {
						msg := fmt.Sprintf("Injector Runtime Error: %v", err)
						log.Println("UI Run Error:", msg)
						s.mu.Lock()
						s.LastError = msg
						s.mu.Unlock()
						break
					}
				}

				if i < cfg.Iterations && intervalDuration > 0 {
					s.mu.Lock()
					s.IsWaiting = true
					s.mu.Unlock()
					select {
					case <-ctx.Done():
						return
					case <-time.After(intervalDuration):
					}
				}
			}
		}()
		return
	}

	// -----------------------------------------------------------------------
	// EXECUTION BRANCH 2: STANDARD WORKLOAD
	// -----------------------------------------------------------------------
	var collectionsCfg *config.CollectionsFile
	var loadErr error

	if customCollections != nil {
		collectionsCfg = customCollections
	} else {
		collectionsCfg, loadErr = loadCollectionsFn(cfg.CollectionsPath, cfg.DefaultWorkload)
		if loadErr != nil {
			s.abortRun("Failed to load collections: " + loadErr.Error())
			cancel()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"status": "error", "message": loadErr.Error()})
			return
		}
	}

	var queriesCfg *config.QueriesFile
	if customQueries != nil {
		queriesCfg = customQueries
	} else {
		queriesCfg, loadErr = loadQueriesFn(cfg.QueriesPath, cfg.DefaultWorkload)
		if loadErr != nil {
			s.abortRun("Failed to load queries: " + loadErr.Error())
			cancel()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"status": "error", "message": loadErr.Error()})
			return
		}
	}

	if len(collectionsCfg.Collections) == 0 {
		errMsg := "No collections found in configuration"
		s.abortRun(errMsg)
		cancel()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"status": "error", "message": errMsg})
		return
	}

	validCollections := make(map[string]bool)
	for _, col := range collectionsCfg.Collections {
		validCollections[col.Name] = true
	}
	var filteredQueries []config.QueryDefinition
	for _, q := range queriesCfg.Queries {
		if validCollections[q.Collection] {
			filteredQueries = append(filteredQueries, q)
		}
	}
	queriesCfg.Queries = filteredQueries
	dbName := collectionsCfg.Collections[0].DatabaseName

	benchConn, connectErr := connectFn(ctx, cfg, dbName)
	if connectErr != nil {
		s.abortRun("Database connection failed: " + connectErr.Error())
		cancel()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"status": "error", "message": connectErr.Error()})
		return
	}

	s.mu.Lock()
	s.CurrentStats = stats.NewCollector()
	s.CurrentStats.ConfigureInsights(cfg)
	s.CurrentStats.SetCollectionsForInsights(collectionsCfg.Collections)
	s.AppConfig = cfg
	s.ActiveCancel = cancel
	s.LastError = ""
	s.CurrentIteration = 1
	s.TotalIterations = cfg.Iterations
	s.IntervalStr = cfg.IntervalDelay
	s.IsWaiting = false
	s.CurrentStats.CurrentIteration = 1
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "started"})

	go func() {
		defer disconnectFn(benchConn, context.Background())
		defer func() {
			s.mu.Lock()
			s.IsRunning = false
			s.mu.Unlock()
		}()

		if err := createCollectionsFn(ctx, benchConn.Database, collectionsCfg, cfg.DropCollections); err != nil {
			msg := fmt.Sprintf("Failed to create collections: %v", err)
			log.Println("UI Run Error:", msg)
			s.mu.Lock()
			s.LastError = msg
			s.mu.Unlock()
			return
		}
		if err := createIndexesFn(ctx, benchConn.Database, collectionsCfg); err != nil {
			msg := fmt.Sprintf("Failed to create indexes: %v", err)
			log.Println("UI Run Error:", msg)
			s.mu.Lock()
			s.LastError = msg
			s.mu.Unlock()
			return
		}

		if !cfg.SkipSeed && cfg.DocumentsCount > 0 {
			for _, col := range collectionsCfg.Collections {
				if err := insertRandomDocsFn(ctx, benchConn.Database, col, cfg.DocumentsCount, cfg); err != nil {
					msg := fmt.Sprintf("Failed during data seeding: %v", err)
					log.Println("UI Run Error:", msg)
					s.mu.Lock()
					s.LastError = msg
					s.mu.Unlock()
					return
				}
			}
		}

		intervalDuration, _ := time.ParseDuration(cfg.IntervalDelay)

		for i := 1; i <= cfg.Iterations; i++ {
			if ctx.Err() != nil {
				break
			}

			s.mu.Lock()
			s.CurrentIteration = i
			s.IsWaiting = false
			s.CurrentStats.CurrentIteration = i
			s.mu.Unlock()

			if err := runWorkloadFn(ctx, benchConn.Database, collectionsCfg.Collections, queriesCfg.Queries, cfg, s.CurrentStats); err != nil {
				if err != context.Canceled {
					msg := fmt.Sprintf("Workload crashed: %v", err)
					log.Println("UI Run Error:", msg)
					s.mu.Lock()
					s.LastError = msg
					s.mu.Unlock()
					break
				}
			}

			if i < cfg.Iterations && intervalDuration > 0 {
				s.mu.Lock()
				s.IsWaiting = true
				s.mu.Unlock()
				select {
				case <-ctx.Done():
					return
				case <-time.After(intervalDuration):
				}
			}
		}
	}()
}

func (s *WebServer) abortRun(reason string) {
	log.Println("Run aborted:", reason)
	s.mu.Lock()
	s.IsRunning = false
	s.mu.Unlock()
}

func (s *WebServer) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.IsRunning && s.ActiveCancel != nil {
		s.ActiveCancel()
		s.IsRunning = false
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "stopped"})
}

func (s *WebServer) handleStats(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	collector := s.CurrentStats
	running := s.IsRunning
	lastErr := s.LastError
	durationStr := "0s"
	if s.AppConfig != nil {
		durationStr = s.AppConfig.Duration
	}
	curIter := s.CurrentIteration
	totIter := s.TotalIterations
	isWait := s.IsWaiting
	intStr := s.IntervalStr
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if collector == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"isRunning": false})
		return
	}

	statsResp := map[string]interface{}{
		"isRunning":        running,
		"lastError":        lastErr,
		"duration":         durationStr,
		"currentIteration": curIter,
		"totalIterations":  totIter,
		"isWaiting":        isWait,
		"intervalDelay":    intStr,

		"findOps":   atomic.LoadUint64(&collector.FindOps),
		"insertOps": atomic.LoadUint64(&collector.InsertOps),
		"upsertOps": atomic.LoadUint64(&collector.UpsertOps),
		"updateOps": atomic.LoadUint64(&collector.UpdateOps),
		"deleteOps": atomic.LoadUint64(&collector.DeleteOps),
		"aggOps":    atomic.LoadUint64(&collector.AggOps),

		"findLatAvg":   collector.FindHist.GetAverage(),
		"insertLatAvg": collector.InsertHist.GetAverage(),
		"updateLatAvg": collector.UpdateHist.GetAverage(),
		"deleteLatAvg": collector.DeleteHist.GetAverage(),
		"aggLatAvg":    collector.AggHist.GetAverage(),

		"distFind":        collector.FindHist.GetStats(),
		"distInsert":      collector.InsertHist.GetStats(),
		"distUpsert":      collector.UpsertHist.GetStats(),
		"distUpdate":      collector.UpdateHist.GetStats(),
		"distDelete":      collector.DeleteHist.GetStats(),
		"distAgg":         collector.AggHist.GetStats(),
		"distTrans":       collector.TransHist.GetStats(),
		"distTotal":       collector.TotalHist.GetStats(),
		"intervalLatency": collector.GetUILatencyTimelineAndReset(),
	}
	json.NewEncoder(w).Encode(statsResp)
}

func (s *WebServer) handleInsights(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	collector := s.CurrentStats
	running := s.IsRunning
	runID := s.RunID
	cached := s.InsightsCache
	appCfg := s.AppConfig
	baseTrends := make(map[string]float64, len(s.ShapeTrendBase))
	for k, v := range s.ShapeTrendBase {
		baseTrends[k] = v
	}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if collector == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"metadata": map[string]interface{}{"status": "inactive"},
		})
		return
	}
	if running {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"metadata": map[string]interface{}{"status": "pending"},
		})
		return
	}
	if cached != nil {
		json.NewEncoder(w).Encode(cached)
		return
	}

	rep := collector.GetFinalInsights()
	events := collector.SnapshotOperationEvents()
	explainEnabled, explainTopN, explainMaxMs := collector.GetExplainSettings()
	if explainEnabled {
		enrichInsightsWithExplain(rep.Metadata.Status, &rep, events, appCfg, explainTopN, explainMaxMs)
	}
	applyTrends(&rep, baseTrends)

	s.mu.Lock()
	if !s.IsRunning && s.RunID == runID {
		s.InsightsCache = &rep
		updateTrendBase(&s.ShapeTrendBase, rep)
	}
	s.mu.Unlock()

	json.NewEncoder(w).Encode(rep)
}

func applyTrends(rep *stats.InsightsReport, base map[string]float64) {
	for i := range rep.SlowQueries {
		cur := rep.SlowQueries[i]
		prev, ok := base[cur.ShapeID]
		if !ok {
			continue
		}
		delta := cur.P95Ms - prev
		dir := "flat"
		if delta > 1 {
			dir = "worse"
		} else if delta < -1 {
			dir = "improved"
		}
		rep.SlowQueries[i].Trend = &stats.ShapeTrend{
			PreviousP95Ms: prev,
			CurrentP95Ms:  cur.P95Ms,
			DeltaP95Ms:    delta,
			Direction:     dir,
		}
	}
}

func updateTrendBase(base *map[string]float64, rep stats.InsightsReport) {
	if *base == nil {
		*base = make(map[string]float64)
	}
	for _, s := range rep.SlowQueries {
		(*base)[s.ShapeID] = s.P95Ms
	}
}

type explainEvidence struct {
	collscan bool
	ixscan   bool
	err      string
}

func enrichInsightsWithExplain(status string, rep *stats.InsightsReport, events []stats.OperationEvent, cfg *config.AppConfig, topN int, maxTimeMs int) {
	if status != "ready" || cfg == nil || topN <= 0 || len(rep.SlowQueries) == 0 {
		return
	}

	candidates := make(map[string]stats.OperationEvent, topN)
	for _, sq := range rep.SlowQueries {
		if len(candidates) >= topN {
			break
		}
		for _, ev := range events {
			if stats.StableShapeID(ev.Operation, ev.Collection, ev.ShapeKey) != sq.ShapeID {
				continue
			}
			if len(ev.FilterSample) == 0 && len(ev.PipelineSample) == 0 {
				continue
			}
			candidates[sq.ShapeID] = ev
			break
		}
	}
	if len(candidates) == 0 {
		return
	}

	rep.Metadata.EvidenceLevel = "heuristic_plus_explain_samples"
	conns := make(map[string]*db.Connection)
	defer func() {
		for _, c := range conns {
			disconnectFn(c, context.Background())
		}
	}()

	results := make(map[string]explainEvidence, len(candidates))
	for shapeID, ev := range candidates {
		dbName := ev.Database
		if dbName == "" {
			dbName = "admin"
		}
		conn, ok := conns[dbName]
		if !ok {
			ctxConn, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			newConn, err := connectFn(ctxConn, cfg, dbName)
			cancel()
			if err != nil {
				results[shapeID] = explainEvidence{err: err.Error()}
				continue
			}
			conn = newConn
			conns[dbName] = conn
		}
		evidence := runExplainForEvent(conn.Database, ev, maxTimeMs)
		results[shapeID] = evidence
	}

	for i := range rep.PotentialIndexIssues {
		issue := &rep.PotentialIndexIssues[i]
		ev, ok := results[issue.ShapeID]
		if !ok {
			continue
		}
		if ev.collscan {
			issue.EvidenceLevel = "explain_collscan_observed"
			issue.Confidence = "high"
			issue.Message = "Representative explain sample showed COLLSCAN. This is strong evidence for index investigation."
			issue.Recommendation = "Create and test a candidate index for these filter fields, then compare explain and latency before rollout."
			continue
		}
		if ev.ixscan {
			issue.EvidenceLevel = "explain_index_scan_observed"
			issue.Confidence = "low"
			issue.Message = "Representative explain sample used an index scan. Index presence was observed, but query/index fit may still be suboptimal."
			issue.Recommendation = "Review index key order/selectivity and validate with additional representative explain samples."
			continue
		}
		if ev.err != "" {
			issue.EvidenceLevel = "explain_unavailable"
			issue.Message = "Explain sampling was enabled, but this shape could not be explained automatically. Keeping heuristic guidance."
		}
	}
}

func runExplainForEvent(database *mongodrv.Database, ev stats.OperationEvent, maxTimeMs int) explainEvidence {
	coll := database.Collection(ev.Collection)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var cmd bson.D
	switch ev.Operation {
	case "find", "updateOne", "updateMany", "deleteOne", "deleteMany":
		if len(ev.FilterSample) == 0 {
			return explainEvidence{err: "missing_filter_sample"}
		}
		inner := bson.D{{Key: "find", Value: coll.Name()}, {Key: "filter", Value: ev.FilterSample}, {Key: "limit", Value: 1}}
		cmd = bson.D{{Key: "explain", Value: inner}, {Key: "verbosity", Value: "queryPlanner"}, {Key: "maxTimeMS", Value: maxTimeMs}}
	case "aggregate":
		if len(ev.PipelineSample) == 0 {
			return explainEvidence{err: "missing_pipeline_sample"}
		}
		inner := bson.D{{Key: "aggregate", Value: coll.Name()}, {Key: "pipeline", Value: ev.PipelineSample}, {Key: "cursor", Value: bson.M{}}}
		cmd = bson.D{{Key: "explain", Value: inner}, {Key: "verbosity", Value: "queryPlanner"}, {Key: "maxTimeMS", Value: maxTimeMs}}
	default:
		return explainEvidence{err: "unsupported_operation"}
	}

	var out bson.M
	if err := database.RunCommand(ctx, cmd, options.RunCmd()).Decode(&out); err != nil {
		return explainEvidence{err: err.Error()}
	}
	return explainEvidence{
		collscan: containsStage(out, "COLLSCAN"),
		ixscan:   containsStage(out, "IXSCAN"),
	}
}

func containsStage(v interface{}, stage string) bool {
	switch t := v.(type) {
	case bson.M:
		for k, val := range t {
			if strings.EqualFold(k, "stage") {
				if s, ok := val.(string); ok && strings.EqualFold(s, stage) {
					return true
				}
			}
			if containsStage(val, stage) {
				return true
			}
		}
	case map[string]interface{}:
		for k, val := range t {
			if strings.EqualFold(k, "stage") {
				if s, ok := val.(string); ok && strings.EqualFold(s, stage) {
					return true
				}
			}
			if containsStage(val, stage) {
				return true
			}
		}
	case []interface{}:
		for _, item := range t {
			if containsStage(item, stage) {
				return true
			}
		}
	}
	return false
}

func generateSelfSignedCert() (tls.Certificate, error) {
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{Organization: []string{"PLGM Web UI"}},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour * 24 * 365),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	derBytes, _ := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	certPem := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	b, _ := x509.MarshalECPrivateKey(priv)
	keyPem := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: b})
	return tls.X509KeyPair(certPem, keyPem)
}

func (s *WebServer) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "shutting_down"})

	go func() {
		time.Sleep(500 * time.Millisecond)
		log.Println("Shutdown requested via Web UI. Exiting application...")
		os.Exit(0)
	}()
}
