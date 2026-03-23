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
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/benchmark"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/db"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/mongo"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
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
}

func NewServer(cfg *config.AppConfig) *WebServer {
	return &WebServer{
		AppConfig: cfg,
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

	var uploadedTempDir string

	if !cfg.DefaultWorkload {
		tempDir, _ := os.MkdirTemp("", "plgm-ui-workload-*")
		uploadedTempDir = tempDir

		collFile, _, err := r.FormFile("collections_file")
		if err == nil {
			defer collFile.Close()
			collPath := filepath.Join(tempDir, "collections.json")
			dst, _ := os.Create(collPath)
			io.Copy(dst, collFile)
			dst.Close()
			cfg.CollectionsPath = collPath
		}
		queryFile, _, err := r.FormFile("queries_file")
		if err == nil {
			defer queryFile.Close()
			queryPath := filepath.Join(tempDir, "queries.json")
			dst, _ := os.Create(queryPath)
			io.Copy(dst, queryFile)
			dst.Close()
			cfg.QueriesPath = queryPath
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

		benchConn, connectErr := db.Connect(ctx, cfg, dbName)
		if connectErr != nil {
			s.abortRun("Database connection failed: " + connectErr.Error())
			cancel()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"status": "error", "message": connectErr.Error()})
			return
		}

		s.mu.Lock()
		s.CurrentStats = stats.NewCollector()
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
			defer func() {
				if uploadedTempDir != "" {
					os.RemoveAll(uploadedTempDir)
				}
			}()
			defer benchConn.Disconnect(context.Background())
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

				if err := benchmark.RunRawInjector(ctx, benchConn.Database, cfg, s.CurrentStats); err != nil {
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
	collectionsCfg, loadErr := config.LoadCollections(cfg.CollectionsPath, cfg.DefaultWorkload)
	if loadErr != nil {
		s.abortRun("Failed to load collections: " + loadErr.Error())
		cancel()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"status": "error", "message": loadErr.Error()})
		return
	}

	queriesCfg, loadErr := config.LoadQueries(cfg.QueriesPath, cfg.DefaultWorkload)
	if loadErr != nil {
		s.abortRun("Failed to load queries: " + loadErr.Error())
		cancel()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"status": "error", "message": loadErr.Error()})
		return
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

	benchConn, connectErr := db.Connect(ctx, cfg, dbName)
	if connectErr != nil {
		s.abortRun("Database connection failed: " + connectErr.Error())
		cancel()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"status": "error", "message": connectErr.Error()})
		return
	}

	s.mu.Lock()
	s.CurrentStats = stats.NewCollector()
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
		defer func() {
			if uploadedTempDir != "" {
				os.RemoveAll(uploadedTempDir)
			}
		}()
		defer benchConn.Disconnect(context.Background())
		defer func() {
			s.mu.Lock()
			s.IsRunning = false
			s.mu.Unlock()
		}()

		if err := mongo.CreateCollectionsFromConfig(ctx, benchConn.Database, collectionsCfg, cfg.DropCollections); err != nil {
			msg := fmt.Sprintf("Failed to create collections: %v", err)
			log.Println("UI Run Error:", msg)
			s.mu.Lock()
			s.LastError = msg
			s.mu.Unlock()
			return
		}
		if err := mongo.CreateIndexesFromConfig(ctx, benchConn.Database, collectionsCfg); err != nil {
			msg := fmt.Sprintf("Failed to create indexes: %v", err)
			log.Println("UI Run Error:", msg)
			s.mu.Lock()
			s.LastError = msg
			s.mu.Unlock()
			return
		}

		if !cfg.SkipSeed && cfg.DocumentsCount > 0 {
			for _, col := range collectionsCfg.Collections {
				if err := mongo.InsertRandomDocuments(ctx, benchConn.Database, col, cfg.DocumentsCount, cfg); err != nil {
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

			if err := mongo.RunWorkload(ctx, benchConn.Database, collectionsCfg.Collections, queriesCfg.Queries, cfg, s.CurrentStats); err != nil {
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

		"distFind":   collector.FindHist.GetStats(),
		"distInsert": collector.InsertHist.GetStats(),
		"distUpsert": collector.UpsertHist.GetStats(),
		"distUpdate": collector.UpdateHist.GetStats(),
		"distDelete": collector.DeleteHist.GetStats(),
		"distAgg":    collector.AggHist.GetStats(),
		"distTrans":  collector.TransHist.GetStats(),
		"distTotal":  collector.TotalHist.GetStats(),
	}
	json.NewEncoder(w).Encode(statsResp)
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
