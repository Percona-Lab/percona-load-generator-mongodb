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
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/accesspattern"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/benchmark"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/db"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/definitions"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/loadprofile"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/mongo"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/report"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/schemainfer"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	"go.mongodb.org/mongo-driver/v2/bson"
	mongodrv "go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

//go:embed static
var staticFiles embed.FS

type WebServer struct {
	mu                sync.Mutex
	IsRunning         bool
	LastError         string
	CurrentStats      *stats.Collector
	ActiveCancel      context.CancelFunc
	AppConfig         *config.AppConfig
	CurrentIteration  int
	TotalIterations   int
	IsWaiting         bool
	IntervalStr       string
	RunID             int64
	InsightsCache     *stats.InsightsReport
	ShapeTrendBase    map[string]float64
	LifecyclePhase    string
	LifecycleMessage  string
	LifecycleStep     string
	LifecycleStepIdx  int
	LifecycleStepTot  int
	LifecycleStepDone int
	LifecycleStepWork int
	InitStartedAt     time.Time
	ExecStartedAt     time.Time
	CompletedAt       time.Time
	InitDurationSec   float64
	LifecycleEvents   []lifecycleEvent
	DefinitionStore   *definitions.FileStore
}

type lifecycleEvent struct {
	At       string `json:"at"`
	Category string `json:"category"`
	Message  string `json:"message"`
}

const (
	builtinQueryDefinitionID      = "__builtin_default_queries"
	builtinCollectionDefinitionID = "__builtin_default_collections"
	builtinDefinitionTimestamp    = "1970-01-01T00:00:00Z"
)

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
	defStore, err := definitions.NewFileStore(definitions.DefaultStorePath())
	if err != nil {
		log.Printf("Warning: definition library disabled: %v", err)
	}
	return &WebServer{
		AppConfig:       cfg,
		ShapeTrendBase:  make(map[string]float64),
		LifecyclePhase:  "idle",
		DefinitionStore: defStore,
	}
}

func (s *WebServer) setLifecyclePhaseLocked(phase, message string) {
	prev := s.LifecyclePhase
	s.LifecyclePhase = phase
	s.LifecycleMessage = strings.TrimSpace(message)
	now := time.Now()
	switch phase {
	case "initializing":
		if s.InitStartedAt.IsZero() {
			s.InitStartedAt = now
		}
	case "running":
		if s.ExecStartedAt.IsZero() {
			s.ExecStartedAt = now
			if !s.InitStartedAt.IsZero() {
				s.InitDurationSec = s.ExecStartedAt.Sub(s.InitStartedAt).Seconds()
			}
		}
	case "completed", "failed":
		s.CompletedAt = now
	}
	if prev != phase && phase != "" {
		s.addLifecycleEventLocked("phase", fmt.Sprintf("Phase changed: %s", phase))
	}
}

func (s *WebServer) setLifecycleStepLocked(step string, idx, total int) {
	s.LifecycleStep = strings.TrimSpace(step)
	s.LifecycleStepIdx = idx
	s.LifecycleStepTot = total
	s.LifecycleStepDone = 0
	s.LifecycleStepWork = 0
}

func (s *WebServer) setLifecycleStepProgressLocked(done, work int) {
	if done < 0 {
		done = 0
	}
	if work < 0 {
		work = 0
	}
	s.LifecycleStepDone = done
	s.LifecycleStepWork = work
}

func (s *WebServer) addLifecycleEventLocked(category, message string) {
	category = strings.TrimSpace(category)
	message = strings.TrimSpace(message)
	if message == "" {
		return
	}
	if category == "" {
		category = "info"
	}
	s.LifecycleEvents = append(s.LifecycleEvents, lifecycleEvent{
		At:       time.Now().Format(time.RFC3339),
		Category: category,
		Message:  message,
	})
	if len(s.LifecycleEvents) > 12 {
		s.LifecycleEvents = s.LifecycleEvents[len(s.LifecycleEvents)-12:]
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
	mux.HandleFunc("/api/infer-schema", s.handleInferSchema)
	mux.HandleFunc("/api/report", s.handleReport)
	mux.HandleFunc("/api/insights", s.handleInsights)
	mux.HandleFunc("/api/shutdown", s.handleShutdown)
	mux.HandleFunc("/api/query-definitions", s.handleQueryDefinitions)
	mux.HandleFunc("/api/query-definitions/", s.handleQueryDefinitionItem)
	mux.HandleFunc("/api/collection-definitions", s.handleCollectionDefinitions)
	mux.HandleFunc("/api/collection-definitions/", s.handleCollectionDefinitionItem)

	// TLS is opt-in. The default is plain HTTP bound to loopback, which browsers
	// treat as a secure context and serve WITHOUT certificate warnings. TLS on a
	// loopback-only dev tool otherwise forces an untrusted self-signed cert and
	// the "your connection is not private" / "unknown certificate" errors.
	var tlsEnabled, tlsCertFile, tlsKeyFile = false, "", ""
	if s.AppConfig != nil {
		tlsEnabled = s.AppConfig.WebUI.TLSEnabled
		tlsCertFile = s.AppConfig.WebUI.TLSCertFile
		tlsKeyFile = s.AppConfig.WebUI.TLSKeyFile
	}

	server := &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", port),
		Handler: mux,
	}

	if !tlsEnabled {
		url := fmt.Sprintf("http://127.0.0.1:%d/", port)
		log.Printf("Starting Web UI on %s \n", url)
		go func() {
			time.Sleep(500 * time.Millisecond)
			openBrowser(url)
		}()
		return server.ListenAndServe()
	}

	url := fmt.Sprintf("https://127.0.0.1:%d/", port)

	// A user-supplied certificate/key pair is the way to get a trusted, warning
	// free HTTPS endpoint. ListenAndServeTLS loads and watches these files.
	if tlsCertFile != "" && tlsKeyFile != "" {
		log.Printf("Starting SECURE Web UI on %s (using certificate %s)\n", url, tlsCertFile)
		go func() {
			time.Sleep(500 * time.Millisecond)
			openBrowser(url)
		}()
		return server.ListenAndServeTLS(tlsCertFile, tlsKeyFile)
	}

	// TLS requested without a certificate: fall back to a self-signed cert.
	cert, err := generateSelfSignedCert()
	if err != nil {
		return err
	}
	server.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
	log.Printf("Starting SECURE Web UI on %s\n", url)
	log.Printf("Note: using a self-signed certificate. Browsers will warn until it is trusted. " +
		"To avoid warnings, either use the default HTTP mode (disable web_ui.tls_enabled) " +
		"or provide a trusted certificate via web_ui.tls_cert_file/tls_key_file.\n")

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

func (s *WebServer) handleQueryDefinitions(w http.ResponseWriter, r *http.Request) {
	s.handleDefinitionCollection(w, r, definitions.KindQuery)
}

func (s *WebServer) handleQueryDefinitionItem(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/query-definitions/")
	s.handleDefinitionItem(w, r, definitions.KindQuery, id)
}

func (s *WebServer) handleCollectionDefinitions(w http.ResponseWriter, r *http.Request) {
	s.handleDefinitionCollection(w, r, definitions.KindCollection)
}

func (s *WebServer) handleCollectionDefinitionItem(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/collection-definitions/")
	s.handleDefinitionItem(w, r, definitions.KindCollection, id)
}

func (s *WebServer) handleDefinitionCollection(w http.ResponseWriter, r *http.Request, kind definitions.Kind) {
	switch r.Method {
	case http.MethodGet:
		builtin, err := s.builtinDefinition(kind, false)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		defs := []definitions.Definition{builtin}
		if s.DefinitionStore != nil {
			stored, err := s.DefinitionStore.List(kind)
			if err != nil {
				writeAPIError(w, http.StatusBadRequest, err.Error())
				return
			}
			defs = append(defs, stored...)
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"definitions": defs})
	case http.MethodPost:
		if s.DefinitionStore == nil {
			writeAPIError(w, http.StatusServiceUnavailable, "definition storage is not available")
			return
		}
		if strings.HasSuffix(r.URL.Path, "/upload") {
			s.handleDefinitionUpload(w, r, kind)
			return
		}
		var in definitions.Input
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid JSON request body: "+err.Error())
			return
		}
		def, err := s.DefinitionStore.Create(kind, in)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, def)
	default:
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *WebServer) handleDefinitionItem(w http.ResponseWriter, r *http.Request, kind definitions.Kind, id string) {
	id = strings.Trim(id, "/")
	if id == "upload" {
		if r.Method != http.MethodPost {
			writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.handleDefinitionUpload(w, r, kind)
		return
	}
	if id == "" {
		writeAPIError(w, http.StatusBadRequest, "definition id is required")
		return
	}

	switch r.Method {
	case http.MethodGet:
		def, err := s.getDefinition(kind, id)
		if err != nil {
			writeDefinitionLookupError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, def)
	case http.MethodPut:
		if isBuiltinDefinitionID(id) {
			writeAPIError(w, http.StatusBadRequest, "built-in default definitions cannot be updated; use Save As New instead")
			return
		}
		if s.DefinitionStore == nil {
			writeAPIError(w, http.StatusServiceUnavailable, "definition storage is not available")
			return
		}
		var in definitions.Input
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid JSON request body: "+err.Error())
			return
		}
		def, err := s.DefinitionStore.Update(kind, id, in)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				writeAPIError(w, http.StatusNotFound, "definition not found")
				return
			}
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, def)
	case http.MethodDelete:
		if isBuiltinDefinitionID(id) {
			writeAPIError(w, http.StatusBadRequest, "built-in default definitions cannot be deleted")
			return
		}
		if s.DefinitionStore == nil {
			writeAPIError(w, http.StatusServiceUnavailable, "definition storage is not available")
			return
		}
		if err := s.DefinitionStore.Delete(kind, id); err != nil {
			writeDefinitionLookupError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	default:
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *WebServer) handleDefinitionUpload(w http.ResponseWriter, r *http.Request, kind definitions.Kind) {
	if s.DefinitionStore == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "definition storage is not available")
		return
	}
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		writeAPIError(w, http.StatusBadRequest, "failed to parse multipart upload: "+err.Error())
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "upload field 'file' is required")
		return
	}
	defer file.Close()

	b, err := io.ReadAll(file)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "failed to read uploaded file: "+err.Error())
		return
	}
	filename := ""
	if header != nil {
		filename = header.Filename
	}
	in := definitions.Input{
		Name:           r.FormValue("name"),
		Description:    r.FormValue("description"),
		Content:        string(b),
		Format:         "json",
		SourceFilename: filename,
	}
	def, err := s.DefinitionStore.Create(kind, in)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, def)
}

func (s *WebServer) getDefinition(kind definitions.Kind, id string) (definitions.Definition, error) {
	if isBuiltinDefinitionID(id) {
		if id != builtinDefinitionIDForKind(kind) {
			return definitions.Definition{}, os.ErrNotExist
		}
		return s.builtinDefinition(kind, true)
	}
	if s.DefinitionStore == nil {
		return definitions.Definition{}, fmt.Errorf("definition storage is not available")
	}
	return s.DefinitionStore.Get(kind, id)
}

func (s *WebServer) builtinDefinition(kind definitions.Kind, includeContent bool) (definitions.Definition, error) {
	switch kind {
	case definitions.KindCollection:
		path := "resources/collections"
		if s.AppConfig != nil && strings.TrimSpace(s.AppConfig.CollectionsPath) != "" {
			path = s.AppConfig.CollectionsPath
		}
		loaded, err := loadCollectionsFn(path, true)
		if err != nil {
			return definitions.Definition{}, err
		}
		content := ""
		if includeContent {
			var err error
			content, err = definitions.MarshalCanonicalJSON(loaded)
			if err != nil {
				return definitions.Definition{}, err
			}
		}
		return definitions.Definition{
			ID:             builtinCollectionDefinitionID,
			Type:           definitions.KindCollection,
			Name:           "Built-in Default Collections",
			Description:    "PLGM embedded default collection definition.",
			Content:        content,
			Format:         "json",
			SourceFilename: "resources/collections/default.json",
			CreatedAt:      builtinDefinitionTimestamp,
			UpdatedAt:      builtinDefinitionTimestamp,
		}, nil
	case definitions.KindQuery:
		path := "resources/queries"
		if s.AppConfig != nil && strings.TrimSpace(s.AppConfig.QueriesPath) != "" {
			path = s.AppConfig.QueriesPath
		}
		loaded, err := loadQueriesFn(path, true)
		if err != nil {
			return definitions.Definition{}, err
		}
		content := ""
		if includeContent {
			var err error
			content, err = definitions.MarshalCanonicalJSON(loaded)
			if err != nil {
				return definitions.Definition{}, err
			}
		}
		return definitions.Definition{
			ID:             builtinQueryDefinitionID,
			Type:           definitions.KindQuery,
			Name:           "Built-in Default Queries",
			Description:    "PLGM embedded default query definition.",
			Content:        content,
			Format:         "json",
			SourceFilename: "resources/queries/default.json",
			CreatedAt:      builtinDefinitionTimestamp,
			UpdatedAt:      builtinDefinitionTimestamp,
		}, nil
	default:
		return definitions.Definition{}, fmt.Errorf("unsupported definition type %q", kind)
	}
}

func isBuiltinDefinitionID(id string) bool {
	return id == builtinQueryDefinitionID || id == builtinCollectionDefinitionID
}

func builtinDefinitionIDForKind(kind definitions.Kind) string {
	switch kind {
	case definitions.KindQuery:
		return builtinQueryDefinitionID
	case definitions.KindCollection:
		return builtinCollectionDefinitionID
	default:
		return ""
	}
}

func writeDefinitionLookupError(w http.ResponseWriter, err error) {
	if errors.Is(err, os.ErrNotExist) {
		writeAPIError(w, http.StatusNotFound, "definition not found")
		return
	}
	writeAPIError(w, http.StatusBadRequest, err.Error())
}

func writeAPIError(w http.ResponseWriter, statusCode int, message string) {
	writeJSON(w, statusCode, map[string]string{
		"status":  "error",
		"message": message,
	})
}

func writeJSON(w http.ResponseWriter, statusCode int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(payload)
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

func writeStartError(w http.ResponseWriter, statusCode int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":  "error",
		"message": message,
	})
}

func explainCollectionsFormatError(raw []byte, wrappedErr error, arrErr error) string {
	body := string(raw)
	if strings.Contains(body, "\"databaseName\"") || strings.Contains(body, "\"collectionName\"") {
		return "invalid collections format: use keys 'database' and 'collection' (not 'databaseName'/'collectionName')"
	}
	base := "invalid collections format: expected either [{...}] or {\"collections\":[...]} with keys 'database' and 'collection'"
	if wrappedErr != nil || arrErr != nil {
		base = fmt.Sprintf("%s (wrapper parse: %v; array parse: %v)", base, wrappedErr, arrErr)
	}
	return base
}

func explainQueriesFormatError(raw []byte, wrappedErr error, arrErr error) string {
	body := string(raw)
	base := "invalid queries format: expected either [{...}] or {\"queries\":[...]} with query definitions"
	if strings.Contains(body, "\"query\"") && !strings.Contains(body, "\"queries\"") {
		base = "invalid queries format: use top-level key 'queries' (plural) when using wrapped format"
	}
	if wrappedErr != nil || arrErr != nil {
		base = fmt.Sprintf("%s (wrapper parse: %v; array parse: %v)", base, wrappedErr, arrErr)
	}
	return base
}

func (s *WebServer) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeStartError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	s.mu.Lock()
	if s.IsRunning {
		s.mu.Unlock()
		writeStartError(w, http.StatusConflict, "workload is already running")
		return
	}
	s.IsRunning = true
	s.RunID++
	s.InsightsCache = nil
	s.LastError = ""
	s.InitStartedAt = time.Now()
	s.ExecStartedAt = time.Time{}
	s.CompletedAt = time.Time{}
	s.InitDurationSec = 0
	s.LifecycleEvents = nil
	s.setLifecyclePhaseLocked("initializing", "Preparing workload resources")
	s.setLifecycleStepLocked("Reading configuration", 1, 5)
	s.setLifecycleStepProgressLocked(1, 1)
	s.addLifecycleEventLocked("phase", "Initialization started")
	s.addLifecycleEventLocked("config", "Reading run configuration and validating inputs")

	baseCfg := *s.AppConfig
	s.mu.Unlock()

	err := r.ParseMultipartForm(10 << 20)
	if err != nil {
		s.abortRun("Failed to parse form")
		writeStartError(w, http.StatusBadRequest, fmt.Sprintf("failed to parse multipart form: %v", err))
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
	if v := r.FormValue("insights_explain_verbosity"); v != "" {
		cfg.InsightsExplainVerbosity = v
	}
	switch cfg.InsightsExplainVerbosity {
	case "queryPlanner", "executionStats":
	default:
		cfg.InsightsExplainVerbosity = "executionStats"
	}
	if mode := r.FormValue("insights_explain_severity_mode"); mode != "" {
		cfg.InsightsExplainSeverityMode = mode
	}
	switch cfg.InsightsExplainSeverityMode {
	case "high_and_low", "medium_only", "critical_only", "high_only":
	default:
		cfg.InsightsExplainSeverityMode = "high_only"
	}
	cfg.InsightsExplainWorkers = parseInt(r.FormValue("insights_explain_workers"), cfg.InsightsExplainWorkers)
	cfg.InsightsExplainRetries = parseInt(r.FormValue("insights_explain_retries"), cfg.InsightsExplainRetries)
	cfg.InsightsExplainBackoffMS = parseInt(r.FormValue("insights_explain_backoff_ms"), cfg.InsightsExplainBackoffMS)
	if mode := r.FormValue("sharding_mode"); mode != "" {
		cfg.ShardingMode = config.NormalizeShardingMode(mode)
	} else {
		cfg.ShardingMode = config.NormalizeShardingMode(cfg.ShardingMode)
	}
	if _, ok := r.Form["sharding_skip_generic_without_config"]; ok {
		cfg.ShardingSkipGenericWithoutConfig = r.FormValue("sharding_skip_generic_without_config") == "true" || r.FormValue("sharding_skip_generic_without_config") == "on"
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
	cfg.ThinkTimeMs = parseInt(r.FormValue("think_time_ms"), cfg.ThinkTimeMs)
	cfg.ThinkJitterMs = parseInt(r.FormValue("think_jitter_ms"), cfg.ThinkJitterMs)
	applyAccessPatternForm(cfg, r)
	cfg.Iterations = parseInt(r.FormValue("iterations"), cfg.Iterations)
	if v := r.FormValue("interval_delay"); v != "" {
		cfg.IntervalDelay = v
	}
	applyLoadProfileForm(cfg, r)

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
	cfg.ExistingRecordHitRate = parseInt(r.FormValue("existing_record_hit_rate"), cfg.ExistingRecordHitRate)
	cfg.RecordPoolMaxSize = parseInt(r.FormValue("record_pool_max_size"), cfg.RecordPoolMaxSize)
	cfg.RecordPoolBootstrapSample = parseInt(r.FormValue("record_pool_bootstrap_sample"), cfg.RecordPoolBootstrapSample)

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
		if defID := strings.TrimSpace(r.FormValue("collection_definition_id")); defID != "" {
			def, err := s.getDefinition(definitions.KindCollection, defID)
			if err != nil {
				s.abortRun("Failed to load selected collection definition: " + err.Error())
				writeStartError(w, http.StatusBadRequest, "failed to load selected collection definition: "+err.Error())
				return
			}
			parsed, err := config.ParseCollectionsBytes([]byte(def.Content))
			if err != nil {
				s.abortRun("Failed to parse selected collection definition: " + err.Error())
				writeStartError(w, http.StatusBadRequest, "failed to parse selected collection definition: "+err.Error())
				return
			}
			customCollections = parsed
		}

		collFile, _, err := r.FormFile("collections_file")
		if err == nil && customCollections == nil {
			defer collFile.Close()
			b, readErr := io.ReadAll(collFile)
			if readErr != nil {
				s.abortRun("Failed to read custom collections_file: " + readErr.Error())
				writeStartError(w, http.StatusBadRequest, "failed to read uploaded collections_file: "+readErr.Error())
				return
			}
			parsed, parseErr := config.ParseCollectionsBytes(b)
			if parseErr != nil {
				var wrapped config.CollectionsFile
				wrappedErr := json.Unmarshal(b, &wrapped)
				var arr []config.CollectionDefinition
				arrErr := json.Unmarshal(b, &arr)
				parseMsg := explainCollectionsFormatError(b, wrappedErr, arrErr)
				s.abortRun("Failed to parse custom collections_file: " + parseMsg)
				writeStartError(w, http.StatusBadRequest, parseMsg)
				return
			}
			customCollections = parsed

			if err := config.ValidateCollectionDefinitions(customCollections.Collections); err != nil {
				msg := err.Error()
				body := string(b)
				if strings.Contains(body, "\"databaseName\"") || strings.Contains(body, "\"collectionName\"") {
					msg = "invalid collections format: use keys 'database' and 'collection' (not 'databaseName'/'collectionName')"
				}
				s.abortRun("Failed to validate custom collections_file: " + msg)
				writeStartError(w, http.StatusBadRequest, msg)
				return
			}
		}

		if defID := strings.TrimSpace(r.FormValue("query_definition_id")); defID != "" {
			def, err := s.getDefinition(definitions.KindQuery, defID)
			if err != nil {
				s.abortRun("Failed to load selected query definition: " + err.Error())
				writeStartError(w, http.StatusBadRequest, "failed to load selected query definition: "+err.Error())
				return
			}
			parsed, err := config.ParseQueriesBytes([]byte(def.Content))
			if err != nil {
				s.abortRun("Failed to parse selected query definition: " + err.Error())
				writeStartError(w, http.StatusBadRequest, "failed to parse selected query definition: "+err.Error())
				return
			}
			for i := range parsed.Queries {
				parsed.Queries[i].SourceType = "stored_definition"
				parsed.Queries[i].SourceFile = def.SourceFilename
				if parsed.Queries[i].SourceFile == "" {
					parsed.Queries[i].SourceFile = def.Name
				}
				parsed.Queries[i].WorkloadName = "custom_workload"
				parsed.Queries[i].DefinitionIndex = i
			}
			if err := config.NormalizeAndValidateQueries(parsed.Queries); err != nil {
				s.abortRun("Failed to validate selected query definition: " + err.Error())
				writeStartError(w, http.StatusBadRequest, err.Error())
				return
			}
			customQueries = parsed
		}

		queryFile, queryHeader, err := r.FormFile("queries_file")
		if err == nil && customQueries == nil {
			defer queryFile.Close()
			b, readErr := io.ReadAll(queryFile)
			if readErr != nil {
				s.abortRun("Failed to read custom queries_file: " + readErr.Error())
				writeStartError(w, http.StatusBadRequest, "failed to read uploaded queries_file: "+readErr.Error())
				return
			}
			parsed, parseErr := config.ParseQueriesBytes(b)
			if parseErr == nil {
				sourceFile := "uploaded_queries.json"
				if queryHeader != nil && strings.TrimSpace(queryHeader.Filename) != "" {
					sourceFile = queryHeader.Filename
				}
				for i := range parsed.Queries {
					parsed.Queries[i].SourceType = "uploaded_file"
					parsed.Queries[i].SourceFile = sourceFile
					parsed.Queries[i].WorkloadName = "custom_workload"
					parsed.Queries[i].DefinitionIndex = i
				}
				if err := config.NormalizeAndValidateQueries(parsed.Queries); err != nil {
					s.abortRun("Failed to validate custom queries_file: " + err.Error())
					writeStartError(w, http.StatusBadRequest, err.Error())
					return
				}
				customQueries = parsed
			} else {
				var wrapped config.QueriesFile
				wrappedErr := json.Unmarshal(b, &wrapped)
				var arr []config.QueryDefinition
				arrErr := json.Unmarshal(b, &arr)
				msg := explainQueriesFormatError(b, wrappedErr, arrErr)
				s.abortRun("Failed to parse custom queries_file: " + msg)
				writeStartError(w, http.StatusBadRequest, msg)
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
	if cfg.ThinkTimeMs < 0 {
		cfg.ThinkTimeMs = 0
	}
	if cfg.ThinkJitterMs < 0 {
		cfg.ThinkJitterMs = 0
	}
	if cfg.Duration == "" {
		cfg.Duration = "10s"
	}
	if err := config.ValidateShardingConfig(cfg); err != nil {
		s.abortRun("Invalid sharding settings: " + err.Error())
		writeStartError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := config.ValidateLoadProfile(cfg); err != nil {
		s.abortRun("Invalid load profile: " + err.Error())
		writeStartError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := config.ValidateAccessPattern(cfg); err != nil {
		s.abortRun("Invalid access pattern: " + err.Error())
		writeStartError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithCancel(context.Background())

	// -----------------------------------------------------------------------
	// EXECUTION BRANCH 1: RAW INJECTOR
	// -----------------------------------------------------------------------
	if cfg.RawInjector.Enabled {
		s.mu.Lock()
		s.setLifecycleStepLocked("Connecting raw injector", 1, 1)
		s.mu.Unlock()
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
			defer func() {
				if r := recover(); r != nil {
					msg := fmt.Sprintf("Raw Injector crashed unexpectedly: %v", r)
					log.Println("UI Run Panic:", msg)
					s.mu.Lock()
					s.LastError = msg
					s.IsRunning = false
					s.setLifecyclePhaseLocked("failed", "Raw injector crashed unexpectedly")
					s.mu.Unlock()
				}
			}()
			defer disconnectFn(benchConn, context.Background())
			defer func() {
				s.mu.Lock()
				s.IsRunning = false
				if s.LastError == "" {
					s.setLifecyclePhaseLocked("completed", "Raw injector run completed")
				}
				s.mu.Unlock()
			}()

			s.mu.Lock()
			s.setLifecyclePhaseLocked("initializing", "Preparing raw injector execution")
			s.setLifecycleStepLocked("Starting injector workers", 1, 1)
			s.mu.Unlock()

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
						s.setLifecyclePhaseLocked("failed", "Raw injector execution failed")
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
	s.mu.Lock()
	s.setLifecycleStepLocked("Loading collections", 1, 5)
	s.setLifecycleStepProgressLocked(0, 1)
	s.addLifecycleEventLocked("init", "Loading collection definitions")
	s.mu.Unlock()

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
	s.mu.Lock()
	s.setLifecycleStepLocked("Loading queries", 2, 5)
	s.setLifecycleStepProgressLocked(0, 1)
	s.addLifecycleEventLocked("init", "Loading query definitions")
	s.mu.Unlock()
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

	if err := config.ValidateCollectionDefinitions(collectionsCfg.Collections); err != nil {
		s.abortRun("Collection validation failed: " + err.Error())
		cancel()
		writeStartError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := config.NormalizeAndValidateQueries(queriesCfg.Queries); err != nil {
		s.abortRun("Query validation failed: " + err.Error())
		cancel()
		writeStartError(w, http.StatusBadRequest, err.Error())
		return
	}

	boundQueries, bindErr := config.ValidateAndBindQueriesToCollections(queriesCfg.Queries, collectionsCfg.Collections)
	if bindErr != nil {
		s.abortRun("Query-to-collection validation failed: " + bindErr.Error())
		cancel()
		writeStartError(w, http.StatusBadRequest, bindErr.Error())
		return
	}
	queriesCfg.Queries = boundQueries
	dbName := collectionsCfg.Collections[0].DatabaseName
	s.mu.Lock()
	s.setLifecycleStepLocked("Connecting to database", 3, 5)
	s.setLifecycleStepProgressLocked(0, 1)
	s.addLifecycleEventLocked("init", fmt.Sprintf("Connecting to database %q", dbName))
	s.mu.Unlock()

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
		defer func() {
			if r := recover(); r != nil {
				msg := fmt.Sprintf("Workload crashed unexpectedly: %v", r)
				log.Println("UI Run Panic:", msg)
				s.mu.Lock()
				s.LastError = msg
				s.IsRunning = false
				s.setLifecyclePhaseLocked("failed", "Initialization or workload execution crashed unexpectedly")
				s.mu.Unlock()
			}
		}()
		defer disconnectFn(benchConn, context.Background())
		defer func() {
			s.mu.Lock()
			s.IsRunning = false
			if s.LastError == "" {
				s.setLifecyclePhaseLocked("completed", "Workload completed")
			}
			s.mu.Unlock()
		}()

		s.mu.Lock()
		s.setLifecycleStepLocked("Preparing collections", 4, 5)
		s.setLifecycleStepProgressLocked(0, len(collectionsCfg.Collections))
		s.addLifecycleEventLocked("init", fmt.Sprintf("Preparing %d collections", len(collectionsCfg.Collections)))
		s.mu.Unlock()
		if err := createCollectionsFn(ctx, benchConn.Database, collectionsCfg, cfg.DropCollections); err != nil {
			msg := fmt.Sprintf("Failed to create collections: %v", err)
			log.Println("UI Run Error:", msg)
			s.mu.Lock()
			s.LastError = msg
			s.setLifecyclePhaseLocked("failed", "Initialization failed while creating collections")
			s.mu.Unlock()
			return
		}
		s.mu.Lock()
		s.setLifecycleStepProgressLocked(len(collectionsCfg.Collections), len(collectionsCfg.Collections))
		s.addLifecycleEventLocked("init", fmt.Sprintf("Collections ready: %d", len(collectionsCfg.Collections)))
		s.mu.Unlock()

		s.mu.Lock()
		s.setLifecycleStepLocked("Creating indexes", 5, 5)
		totalIndexes := 0
		for _, col := range collectionsCfg.Collections {
			totalIndexes += len(col.Indexes)
		}
		if totalIndexes <= 0 {
			totalIndexes = 1
		}
		s.setLifecycleStepProgressLocked(0, totalIndexes)
		s.addLifecycleEventLocked("init", fmt.Sprintf("Starting index creation across %d collections", len(collectionsCfg.Collections)))
		s.mu.Unlock()
		if err := createIndexesFn(ctx, benchConn.Database, collectionsCfg); err != nil {
			msg := fmt.Sprintf("Failed to create indexes: %v", err)
			log.Println("UI Run Error:", msg)
			s.mu.Lock()
			s.LastError = msg
			s.setLifecyclePhaseLocked("failed", "Initialization failed while creating indexes")
			s.mu.Unlock()
			return
		}
		s.mu.Lock()
		totalIndexes = 0
		for _, col := range collectionsCfg.Collections {
			totalIndexes += len(col.Indexes)
		}
		if totalIndexes <= 0 {
			totalIndexes = 1
		}
		s.setLifecycleStepProgressLocked(totalIndexes, totalIndexes)
		s.addLifecycleEventLocked("init", "Index creation finished")
		s.mu.Unlock()

		if !cfg.SkipSeed && cfg.DocumentsCount > 0 {
			s.mu.Lock()
			s.setLifecycleStepLocked("Seeding initial data", 5, 5)
			s.setLifecycleStepProgressLocked(0, len(collectionsCfg.Collections))
			s.addLifecycleEventLocked("init", fmt.Sprintf("Seeding up to %d documents per collection", cfg.DocumentsCount))
			s.mu.Unlock()
			seeded := 0
			for _, col := range collectionsCfg.Collections {
				if err := insertRandomDocsFn(ctx, benchConn.Database, col, cfg.DocumentsCount, cfg); err != nil {
					msg := fmt.Sprintf("Failed during data seeding: %v", err)
					log.Println("UI Run Error:", msg)
					s.mu.Lock()
					s.LastError = msg
					s.setLifecyclePhaseLocked("failed", "Initialization failed while seeding data")
					s.mu.Unlock()
					return
				}
				seeded++
				s.mu.Lock()
				s.setLifecycleStepProgressLocked(seeded, len(collectionsCfg.Collections))
				s.mu.Unlock()
			}
			s.mu.Lock()
			s.addLifecycleEventLocked("init", "Data seeding completed")
			s.mu.Unlock()
		}

		s.mu.Lock()
		s.setLifecyclePhaseLocked("initializing", "Finalizing execution setup")
		s.setLifecycleStepLocked("Applying sharding and runtime setup", 5, 5)
		s.setLifecycleStepProgressLocked(0, len(collectionsCfg.Collections))
		s.addLifecycleEventLocked("init", fmt.Sprintf("Applying sharding/runtime setup for %d collections", len(collectionsCfg.Collections)))
		s.mu.Unlock()

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
					s.setLifecyclePhaseLocked("failed", "Execution failed")
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

// applyLoadProfileForm populates the load profile from the start form. An empty
// load_profile_kind leaves the config untouched (fixed concurrency default).
// Step stages are accepted as a JSON array, e.g.
// [{"workers":5,"duration":"30s"},{"workers":20,"duration":"60s"}].
func applyLoadProfileForm(cfg *config.AppConfig, r *http.Request) {
	kind := strings.TrimSpace(r.FormValue("load_profile_kind"))
	if kind == "" || strings.EqualFold(kind, "fixed") {
		// Preserve fixed behavior but still honor an explicit fixed worker count.
		if kind == "" {
			return
		}
	}
	lp := loadprofile.Config{
		Kind:         kind,
		Workers:      parseInt(r.FormValue("load_profile_workers"), 0),
		StartWorkers: parseInt(r.FormValue("load_profile_start_workers"), 0),
		EndWorkers:   parseInt(r.FormValue("load_profile_end_workers"), 0),
		RampOver:     strings.TrimSpace(r.FormValue("load_profile_ramp_over")),
		BaseWorkers:  parseInt(r.FormValue("load_profile_base_workers"), 0),
		PeakWorkers:  parseInt(r.FormValue("load_profile_peak_workers"), 0),
		SpikeAt:      strings.TrimSpace(r.FormValue("load_profile_spike_at")),
		SpikeFor:     strings.TrimSpace(r.FormValue("load_profile_spike_for")),
		MinWorkers:   parseInt(r.FormValue("load_profile_min_workers"), 0),
		MaxWorkers:   parseInt(r.FormValue("load_profile_max_workers"), 0),
		Period:       strings.TrimSpace(r.FormValue("load_profile_period")),
	}
	if raw := strings.TrimSpace(r.FormValue("load_profile_steps")); raw != "" {
		var steps []loadprofile.Stage
		if err := json.Unmarshal([]byte(raw), &steps); err == nil {
			lp.Steps = steps
		}
	}
	cfg.LoadProfile = lp
}

// applyAccessPatternForm populates the access-pattern config from the start
// form. An empty access_pattern_kind leaves the config untouched (uniform).
func applyAccessPatternForm(cfg *config.AppConfig, r *http.Request) {
	kind := strings.TrimSpace(r.FormValue("access_pattern_kind"))
	if kind == "" {
		return
	}
	cfg.AccessPattern = accesspattern.Config{
		Kind:                  kind,
		ZipfianExponent:       parseFloat(r.FormValue("access_pattern_zipfian_exponent"), 0),
		HotspotPercent:        parseInt(r.FormValue("access_pattern_hotspot_percent"), 0),
		HotspotTrafficPercent: parseInt(r.FormValue("access_pattern_hotspot_traffic_percent"), 0),
	}
}

func (s *WebServer) abortRun(reason string) {
	log.Println("Run aborted:", reason)
	s.mu.Lock()
	s.LastError = reason
	s.IsRunning = false
	s.setLifecyclePhaseLocked("failed", reason)
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
		s.setLifecyclePhaseLocked("failed", "Run stopped by user")
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
	loadProfileKind := "fixed"
	configuredConcurrency := 0
	if s.AppConfig != nil {
		durationStr = s.AppConfig.Duration
		if k := strings.TrimSpace(s.AppConfig.LoadProfile.Kind); k != "" {
			loadProfileKind = strings.ToLower(k)
		}
		configuredConcurrency = s.AppConfig.Concurrency
	}
	curIter := s.CurrentIteration
	totIter := s.TotalIterations
	isWait := s.IsWaiting
	intStr := s.IntervalStr
	lifecyclePhase := s.LifecyclePhase
	lifecycleMessage := s.LifecycleMessage
	lifecycleStep := s.LifecycleStep
	lifecycleStepIdx := s.LifecycleStepIdx
	lifecycleStepTot := s.LifecycleStepTot
	lifecycleStepDone := s.LifecycleStepDone
	lifecycleStepWork := s.LifecycleStepWork
	initStartedAt := s.InitStartedAt
	execStartedAt := s.ExecStartedAt
	completedAt := s.CompletedAt
	initDurationSec := s.InitDurationSec
	lifecycleEvents := append([]lifecycleEvent(nil), s.LifecycleEvents...)
	s.mu.Unlock()

	now := time.Now()
	findOps := uint64(0)
	insertOps := uint64(0)
	upsertOps := uint64(0)
	updateOps := uint64(0)
	deleteOps := uint64(0)
	aggOps := uint64(0)
	if collector != nil {
		findOps = atomic.LoadUint64(&collector.FindOps)
		insertOps = atomic.LoadUint64(&collector.InsertOps)
		upsertOps = atomic.LoadUint64(&collector.UpsertOps)
		updateOps = atomic.LoadUint64(&collector.UpdateOps)
		deleteOps = atomic.LoadUint64(&collector.DeleteOps)
		aggOps = atomic.LoadUint64(&collector.AggOps)
	}
	totalOps := findOps + insertOps + upsertOps + updateOps + deleteOps + aggOps
	if running && lifecyclePhase == "initializing" && totalOps > 0 {
		s.mu.Lock()
		if s.LifecyclePhase == "initializing" {
			s.setLifecyclePhaseLocked("running", "Executing workload")
			s.LifecycleStep = ""
			s.LifecycleStepIdx = 0
			s.LifecycleStepTot = 0
			s.LifecycleStepDone = 0
			s.LifecycleStepWork = 0
			s.addLifecycleEventLocked("phase", "Execution phase started")
			lifecyclePhase = s.LifecyclePhase
			lifecycleMessage = s.LifecycleMessage
			lifecycleStep = s.LifecycleStep
			lifecycleStepIdx = s.LifecycleStepIdx
			lifecycleStepTot = s.LifecycleStepTot
			lifecycleStepDone = s.LifecycleStepDone
			lifecycleStepWork = s.LifecycleStepWork
			lifecycleEvents = append([]lifecycleEvent(nil), s.LifecycleEvents...)
			execStartedAt = s.ExecStartedAt
			initDurationSec = s.InitDurationSec
		}
		s.mu.Unlock()
	}
	// Prefer the precise workload-start timestamp recorded by the runner (set
	// when the measured window actually begins, excluding preparation time).
	// Fall back to the lifecycle ExecStartedAt when it is unavailable.
	workloadStart := execStartedAt
	if collector != nil {
		if ws, ok := collector.WorkloadStart(); ok {
			workloadStart = ws
		}
	}
	execElapsedSec := 0.0
	switch lifecyclePhase {
	case "running":
		if !workloadStart.IsZero() {
			execElapsedSec = now.Sub(workloadStart).Seconds()
		}
	case "completed", "failed":
		if !workloadStart.IsZero() {
			end := completedAt
			if end.IsZero() {
				end = now
			}
			execElapsedSec = end.Sub(workloadStart).Seconds()
		}
	}
	if execElapsedSec < 0 {
		execElapsedSec = 0
	}
	if initDurationSec <= 0 && !initStartedAt.IsZero() && !execStartedAt.IsZero() {
		initDurationSec = execStartedAt.Sub(initStartedAt).Seconds()
	}
	stepProgressPct := 0.0
	if lifecycleStepWork > 0 {
		stepProgressPct = (float64(lifecycleStepDone) / float64(lifecycleStepWork)) * 100.0
		if stepProgressPct > 100 {
			stepProgressPct = 100
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if collector == nil {
		if lifecyclePhase == "" {
			lifecyclePhase = "idle"
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"isRunning":                 running,
			"lastError":                 lastErr,
			"loadProfileKind":           loadProfileKind,
			"configuredConcurrency":     configuredConcurrency,
			"concurrencyTarget":         0,
			"concurrencyActive":         0,
			"lifecyclePhase":            lifecyclePhase,
			"lifecycleMessage":          lifecycleMessage,
			"lifecycleStep":             lifecycleStep,
			"lifecycleStepIndex":        lifecycleStepIdx,
			"lifecycleStepTotal":        lifecycleStepTot,
			"lifecycleStepDone":         lifecycleStepDone,
			"lifecycleStepWork":         lifecycleStepWork,
			"lifecycleStepProgressPct":  stepProgressPct,
			"lifecycleRecentEvents":     lifecycleEvents,
			"initializationDurationSec": initDurationSec,
			"executionElapsedSec":       execElapsedSec,
			"lifecycle": map[string]interface{}{
				"phase":                       lifecyclePhase,
				"message":                     lifecycleMessage,
				"step":                        lifecycleStep,
				"step_index":                  lifecycleStepIdx,
				"step_total":                  lifecycleStepTot,
				"step_done":                   lifecycleStepDone,
				"step_work":                   lifecycleStepWork,
				"step_progress_pct":           stepProgressPct,
				"initialization_duration_sec": initDurationSec,
				"execution_elapsed_sec":       execElapsedSec,
				"recent_events":               lifecycleEvents,
			},
		})
		return
	}

	// collector is guaranteed non-nil here (the nil case returned above).
	// Capture a latency-over-time heatmap bucket on a stable ~1s window rather
	// than the (sub-second) UI polling cadence. Tying the window to the poll
	// rate produced tiny samples where the per-window p99 collapsed onto the
	// single slowest op, badly overstating tail latency versus the cumulative
	// distribution. A 1s window keeps each bucket statistically meaningful.
	collector.CaptureLatencyIntervalEvery(time.Second)
	concTarget, concActive := collector.ConcurrencySnapshot()

	statsResp := map[string]interface{}{
		"isRunning":                 running,
		"lastError":                 lastErr,
		"duration":                  durationStr,
		"loadProfileKind":           loadProfileKind,
		"configuredConcurrency":     configuredConcurrency,
		"concurrencyTarget":         concTarget,
		"concurrencyActive":         concActive,
		"currentIteration":          curIter,
		"totalIterations":           totIter,
		"isWaiting":                 isWait,
		"intervalDelay":             intStr,
		"lifecyclePhase":            lifecyclePhase,
		"lifecycleMessage":          lifecycleMessage,
		"lifecycleStep":             lifecycleStep,
		"lifecycleStepIndex":        lifecycleStepIdx,
		"lifecycleStepTotal":        lifecycleStepTot,
		"lifecycleStepDone":         lifecycleStepDone,
		"lifecycleStepWork":         lifecycleStepWork,
		"lifecycleStepProgressPct":  stepProgressPct,
		"lifecycleRecentEvents":     lifecycleEvents,
		"initializationDurationSec": initDurationSec,
		"executionElapsedSec":       execElapsedSec,
		"lifecycle": map[string]interface{}{
			"phase":                       lifecyclePhase,
			"message":                     lifecycleMessage,
			"step":                        lifecycleStep,
			"step_index":                  lifecycleStepIdx,
			"step_total":                  lifecycleStepTot,
			"step_done":                   lifecycleStepDone,
			"step_work":                   lifecycleStepWork,
			"step_progress_pct":           stepProgressPct,
			"initialization_duration_sec": initDurationSec,
			"execution_elapsed_sec":       execElapsedSec,
			"recent_events":               lifecycleEvents,
		},

		"findOps":   findOps,
		"insertOps": insertOps,
		"upsertOps": upsertOps,
		"updateOps": updateOps,
		"deleteOps": deleteOps,
		"aggOps":    aggOps,

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
		"latencyHeatmap":  collector.LatencyHeatmap(),
	}
	json.NewEncoder(w).Encode(statsResp)
}

// handleReport renders a self-contained HTML report for the current/last run.
// Pass ?download=1 to receive it as a file attachment; otherwise it renders
// inline (and can be printed to PDF from the browser).
func (s *WebServer) handleReport(w http.ResponseWriter, r *http.Request) {
	data := s.buildReportData()
	html, err := report.RenderBytes(data)
	if err != nil {
		writeStartError(w, http.StatusInternalServerError, "failed to render report: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if r.URL.Query().Get("download") == "1" {
		filename := fmt.Sprintf("plgm-report-%s.html", time.Now().Format("20060102-150405"))
		w.Header().Set("Content-Disposition", "attachment; filename=\""+filename+"\"")
	}
	w.Write(html)
}

// buildReportData snapshots the current run state into a render-ready ReportData.
func (s *WebServer) buildReportData() report.ReportData {
	s.mu.Lock()
	collector := s.CurrentStats
	cfg := s.AppConfig
	lastErr := s.LastError
	isRunning := s.IsRunning
	completedAt := s.CompletedAt
	s.mu.Unlock()

	data := report.ReportData{
		Title:       "MongoDB Benchmark Report",
		GeneratedAt: time.Now(),
		Warnings:    []string{},
		Insights:    []string{},
	}
	if lastErr != "" {
		data.Warnings = append(data.Warnings, lastErr)
	}

	if cfg != nil {
		data.ConfigItems = []report.KV{
			{Key: "Target URI", Value: maskMongoURI(cfg.URI)},
			{Key: "Duration", Value: cfg.Duration},
			{Key: "Configured Concurrency", Value: strconv.Itoa(cfg.Concurrency)},
			{Key: "Find %", Value: strconv.Itoa(cfg.FindPercent)},
			{Key: "Insert %", Value: strconv.Itoa(cfg.InsertPercent)},
			{Key: "Update %", Value: strconv.Itoa(cfg.UpdatePercent)},
			{Key: "Delete %", Value: strconv.Itoa(cfg.DeletePercent)},
			{Key: "Aggregate %", Value: strconv.Itoa(cfg.AggregatePercent)},
			{Key: "Transaction %", Value: strconv.Itoa(cfg.TransactionPercent)},
		}

		if sched, err := loadprofile.Compile(cfg.LoadProfile, cfg.Concurrency); err == nil {
			if strings.TrimSpace(cfg.LoadProfile.Kind) != "" && !strings.EqualFold(cfg.LoadProfile.Kind, "fixed") {
				data.LoadProfileItems = []report.KV{
					{Key: "Kind", Value: cfg.LoadProfile.Kind},
					{Key: "Summary", Value: sched.Summary()},
					{Key: "Peak Workers", Value: strconv.Itoa(sched.MaxWorkers())},
				}
			}
		}

		data.PacingItems = []report.KV{
			{Key: "Think Time", Value: strconv.Itoa(cfg.ThinkTimeMs) + " ms"},
			{Key: "Think Jitter", Value: strconv.Itoa(cfg.ThinkJitterMs) + " ms"},
		}
		apSummary := "uniform"
		if sel, err := accesspattern.Compile(cfg.AccessPattern); err == nil {
			apSummary = sel.Summary()
		}
		data.AccessPatternItems = []report.KV{{Key: "Access Pattern", Value: apSummary}}

		data.DurationSeconds = parseDurationSeconds(cfg.Duration)
	}

	if collector != nil {
		data.Latency = buildLatencyRows(collector)
		var total int64
		for _, row := range data.Latency {
			if row.Type == "total" {
				total = row.Count
			}
		}
		data.TotalOps = total
		if elapsed, ok := collector.WorkloadStart(); ok {
			secs := time.Since(elapsed).Seconds()
			if !isRunning && !completedAt.IsZero() {
				secs = completedAt.Sub(elapsed).Seconds()
			}
			if secs > 0 {
				data.DurationSeconds = secs
			}
		}
		if data.DurationSeconds > 0 {
			data.AvgOpsPerSec = float64(total) / data.DurationSeconds
		}
		for _, hp := range collector.LatencyHeatmap() {
			data.Heatmap = append(data.Heatmap, report.HeatmapPoint{
				ElapsedSec: hp.ElapsedSec, Count: hp.Count, P50: hp.P50, P95: hp.P95, P99: hp.P99, Max: hp.Max,
			})
		}
		acc := collector.AccuracyStats()
		if acc.TargetExisting+acc.TargetRandom > 0 {
			rate := 100 * float64(acc.TargetExisting) / float64(acc.TargetExisting+acc.TargetRandom)
			data.Insights = append(data.Insights, fmt.Sprintf("Existing-record targeting: %.1f%% of filters hit known records.", rate))
		}
	}

	return data
}

// buildLatencyRows converts the collector's per-type histograms into report rows.
func buildLatencyRows(c *stats.Collector) []report.LatencyRow {
	type entry struct {
		name string
		hist *stats.LatencyHistogram
	}
	entries := []entry{
		{"find", c.FindHist},
		{"insert", c.InsertHist},
		{"upsert", c.UpsertHist},
		{"update", c.UpdateHist},
		{"delete", c.DeleteHist},
		{"aggregate", c.AggHist},
		{"transaction", c.TransHist},
		{"total", c.TotalHist},
	}
	var rows []report.LatencyRow
	for _, e := range entries {
		if e.hist == nil {
			continue
		}
		st := e.hist.GetStats()
		if st["count"] == 0 && e.name != "total" {
			continue
		}
		rows = append(rows, report.LatencyRow{
			Type:  e.name,
			Count: int64(st["count"]),
			AvgMs: st["avg"],
			MinMs: st["min"],
			MaxMs: st["max"],
			P95Ms: st["p95"],
			P99Ms: st["p99"],
		})
	}
	return rows
}

// maskMongoURI hides any password embedded in a MongoDB connection string.
func maskMongoURI(uri string) string {
	u, err := url.Parse(uri)
	if err == nil && u.User != nil {
		if p, has := u.User.Password(); has && p != "" {
			return strings.Replace(uri, p, "******", 1)
		}
	}
	return uri
}

// parseDurationSeconds parses a duration string like "30s" into seconds, 0 on error.
func parseDurationSeconds(s string) float64 {
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return d.Seconds()
}

// handleInferSchema parses a pasted/uploaded MongoDB query log and returns an
// inferred workload model (collections, operation patterns, suggested mix, and
// candidate queries) the user can review and use to pre-fill a benchmark.
func (s *WebServer) handleInferSchema(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeStartError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	defer r.Body.Close()

	var logText string
	contentType := r.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(contentType, "application/json"):
		var payload struct {
			Log string `json:"log"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			writeStartError(w, http.StatusBadRequest, "invalid JSON payload: "+err.Error())
			return
		}
		logText = payload.Log
	case strings.HasPrefix(contentType, "multipart/form-data"):
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			writeStartError(w, http.StatusBadRequest, "invalid form upload: "+err.Error())
			return
		}
		logText = r.FormValue("log")
		if logText == "" {
			if file, _, err := r.FormFile("logfile"); err == nil {
				defer file.Close()
				data, _ := io.ReadAll(io.LimitReader(file, 64<<20))
				logText = string(data)
			}
		}
	default:
		data, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
		if err != nil {
			writeStartError(w, http.StatusBadRequest, "failed to read body: "+err.Error())
			return
		}
		logText = string(data)
	}

	if strings.TrimSpace(logText) == "" {
		writeStartError(w, http.StatusBadRequest, "no query log content provided")
		return
	}

	result := schemainfer.Infer(logText)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
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
	explainEnabled, explainTopN, explainMaxMs, explainVerbosity, explainSeverityMode, explainWorkers, explainRetries, explainBackoffMs := collector.GetExplainSettings()
	if explainEnabled {
		enrichInsightsWithExplain(rep.Metadata.Status, &rep, events, appCfg, explainTopN, explainMaxMs, explainVerbosity, explainSeverityMode, explainWorkers, explainRetries, explainBackoffMs)
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
	status   string
	reason   string
	diag     *stats.ExplainDiagnostics
}

type explainCommandSpec struct {
	cmd           bson.D
	verbosity     string
	serverMaxTime int
	clientTimeout time.Duration
	commandType   string
	stageSummary  string
}

type explainSignals struct {
	planStages         []string
	indexesUsed        []string
	collectionScan     bool
	indexScan          bool
	fetch              bool
	group              bool
	sort               bool
	limit              bool
	blockingSort       bool
	usedDisk           bool
	docsExamined       int64
	keysExamined       int64
	nReturned          int64
	collectionScans    int64
	indexSeeks         int64
	executionTimeMS    int64
	spills             int64
	winningPlanSummary string
	shardSummary       string
}

func enrichInsightsWithExplain(status string, rep *stats.InsightsReport, events []stats.OperationEvent, cfg *config.AppConfig, topN int, maxTimeMs int, explainVerbosity string, explainSeverityMode string, workers int, retries int, backoffMs int) {
	if status != "ready" || cfg == nil || topN <= 0 || len(rep.SlowQueries) == 0 {
		return
	}
	if workers <= 0 {
		workers = 1
	}
	if retries < 0 {
		retries = 0
	}
	if backoffMs < 0 {
		backoffMs = 0
	}
	rep.Metadata.ExplainWorkers = workers
	rep.Metadata.ExplainRetries = retries
	rep.Metadata.ExplainBackoffMS = backoffMs
	if explainVerbosity == "queryPlanner" {
		rep.Metadata.ExplainVerbosity = "queryPlanner"
	} else {
		rep.Metadata.ExplainVerbosity = "executionStats"
	}

	indexIssueShapes := make(map[string]struct{}, len(rep.PotentialIndexIssues))
	for _, issue := range rep.PotentialIndexIssues {
		indexIssueShapes[issue.ShapeID] = struct{}{}
	}

	results := make(map[string]explainEvidence, len(rep.SlowQueries))
	candidates := make(map[string]stats.OperationEvent, topN)
	for _, sq := range rep.SlowQueries {
		if !supportsExplainOperation(sq.Operation) {
			results[sq.ShapeID] = explainEvidence{
				status: "not_supported",
				reason: fmt.Sprintf("operation_%s_not_supported", sq.Operation),
			}
			continue
		}
		if !isExplainCandidatePriorityShape(sq, indexIssueShapes, rep.Metadata.SlowThresholdMs, explainSeverityMode) {
			results[sq.ShapeID] = explainEvidence{
				status: "explain_unavailable",
				reason: "low_value_shape_filtered",
			}
			continue
		}

		if len(candidates) >= topN {
			results[sq.ShapeID] = explainEvidence{
				status: "explain_unavailable",
				reason: fmt.Sprintf("not_selected_top_n_%d", topN),
			}
			continue
		}

		candidate, reason, ok := findExplainCandidate(events, sq)
		if !ok {
			results[sq.ShapeID] = explainEvidence{
				status: "insufficient_metadata",
				reason: reason,
			}
			continue
		}

		candidates[sq.ShapeID] = candidate
		results[sq.ShapeID] = explainEvidence{
			status: "explain_unavailable",
			reason: "candidate_selected_explain_pending",
		}
	}

	shapeIDs := make([]string, 0, len(candidates))
	for shapeID := range candidates {
		shapeIDs = append(shapeIDs, shapeID)
	}
	sort.Strings(shapeIDs)
	applyExplainCandidatesConcurrently(results, candidates, shapeIDs, workers, cfg, maxTimeMs, explainVerbosity, retries, backoffMs)

	appliedExplain := false
	for _, ev := range results {
		if ev.status == "explained" {
			appliedExplain = true
			break
		}
	}
	if appliedExplain {
		rep.Metadata.EvidenceLevel = "heuristic_plus_explain_samples"
	}

	applyExplainToSlowQueries(rep, results)

	for i := range rep.PotentialIndexIssues {
		issue := &rep.PotentialIndexIssues[i]
		ev, ok := results[issue.ShapeID]
		if !ok {
			issue.ExplainStatus = "explain_unavailable"
			issue.ExplainReason = "shape_not_in_top_slow_queries"
			issue.EvidenceLevel = "heuristic_explain_unavailable"
			issue.Confidence = "low"
			issue.Message = "No representative explain sample was available for this shape. Keeping heuristic guidance."
			continue
		}
		issue.ExplainStatus = ev.status
		issue.ExplainReason = ev.reason
		if ev.diag != nil && ev.status == "explained" {
			issue.Recommendation = ev.diag.Recommendation
			issue.Confidence = ev.diag.RecommendationConfidence
			issue.Message = ev.diag.Interpretation
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
			issue.Confidence = "medium"
			issue.Message = "Representative explain sample used an index scan. Index presence was observed, but query/index fit may still be suboptimal."
			issue.Recommendation = "Review index key order/selectivity and validate with additional representative explain samples."
			continue
		}
		switch ev.status {
		case "explained":
			issue.EvidenceLevel = "explain_structured_plan_signal"
			if ev.diag == nil || strings.TrimSpace(ev.diag.Interpretation) == "" {
				issue.Confidence = "low"
				issue.Message = "Explain succeeded, but parsed plan evidence was limited. Keep guidance cautious and validate with additional explain samples."
			}
		case "not_supported":
			issue.EvidenceLevel = "heuristic_not_supported"
			issue.Confidence = "low"
			issue.Message = "This operation type is not currently explain-supported in post-run analysis. Keeping heuristic guidance."
		case "insufficient_metadata":
			issue.EvidenceLevel = "heuristic_insufficient_metadata"
			issue.Confidence = "low"
			issue.Message = "Explain could not run because required sampled filter/pipeline metadata was unavailable."
		case "timed_out":
			issue.EvidenceLevel = "heuristic_explain_timed_out"
			issue.Confidence = "low"
			issue.Message = "Explain timed out for this representative shape. Guidance remains heuristic unless a higher explain budget succeeds."
		case "execution_failed":
			issue.EvidenceLevel = "heuristic_explain_execution_failed"
			issue.Confidence = "low"
			issue.Message = "Explain execution failed for this representative shape; guidance remains heuristic."
		default:
			issue.EvidenceLevel = "heuristic_explain_unavailable"
			issue.Confidence = "low"
			issue.Message = "Explain sampling was enabled, but this shape was not selected for explain in this run. Keeping heuristic guidance."
		}
	}
}

func isExplainCandidatePriorityShape(sq stats.SlowQueryInsight, indexIssueShapes map[string]struct{}, slowThresholdMs float64, explainSeverityMode string) bool {
	switch explainSeverityMode {
	case "high_and_low":
	case "medium_only":
		switch sq.Severity {
		case "critical", "high", "medium":
		default:
			return false
		}
	case "critical_only":
		if sq.Severity != "critical" {
			return false
		}
	default: // high_only
		switch sq.Severity {
		case "critical", "high":
		default:
			return false
		}
	}
	if _, ok := indexIssueShapes[sq.ShapeID]; ok {
		return true
	}
	if sq.P95Ms >= slowThresholdMs || sq.P99Ms >= slowThresholdMs {
		return true
	}
	switch sq.Severity {
	case "critical", "high":
		return true
	default:
		return false
	}
}

func applyExplainCandidatesConcurrently(
	results map[string]explainEvidence,
	candidates map[string]stats.OperationEvent,
	shapeIDs []string,
	workers int,
	cfg *config.AppConfig,
	maxTimeMs int,
	explainVerbosity string,
	retries int,
	backoffMs int,
) {
	type job struct {
		shapeID string
		event   stats.OperationEvent
	}
	type out struct {
		shapeID  string
		evidence explainEvidence
	}

	jobs := make(chan job)
	outs := make(chan out, len(shapeIDs))

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conns := make(map[string]*db.Connection)
			defer func() {
				for _, c := range conns {
					disconnectFn(c, context.Background())
				}
			}()

			for j := range jobs {
				dbName := j.event.Database
				if dbName == "" {
					dbName = "admin"
				}
				conn, ok := conns[dbName]
				if !ok {
					ctxConn, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					newConn, err := connectFn(ctxConn, cfg, dbName)
					cancel()
					if err != nil {
						outs <- out{
							shapeID: j.shapeID,
							evidence: explainEvidence{
								status: "execution_failed",
								reason: "connect_failed",
							},
						}
						continue
					}
					conn = newConn
					conns[dbName] = conn
				}
				evidence := runExplainForEventWithRetry(conn.Database, j.event, maxTimeMs, explainVerbosity, retries, backoffMs)
				outs <- out{shapeID: j.shapeID, evidence: evidence}
			}
		}()
	}

	go func() {
		for _, shapeID := range shapeIDs {
			jobs <- job{shapeID: shapeID, event: candidates[shapeID]}
		}
		close(jobs)
		wg.Wait()
		close(outs)
	}()

	for o := range outs {
		results[o.shapeID] = o.evidence
	}
}

func runExplainForEventWithRetry(database *mongodrv.Database, ev stats.OperationEvent, baseMaxTimeMs int, explainVerbosity string, retries int, backoffMs int) explainEvidence {
	return retryExplainEvidence(baseMaxTimeMs, retries, backoffMs, func(maxTimeMs int) explainEvidence {
		return runExplainForEvent(database, ev, maxTimeMs, explainVerbosity)
	})
}

func retryExplainEvidence(baseMaxTimeMs int, retries int, backoffMs int, run func(maxTimeMs int) explainEvidence) explainEvidence {
	if baseMaxTimeMs <= 0 {
		baseMaxTimeMs = 1000
	}
	if retries < 0 {
		retries = 0
	}
	maxTime := baseMaxTimeMs
	var last explainEvidence
	for attempt := 0; attempt <= retries; attempt++ {
		last = run(maxTime)
		if last.status != "timed_out" {
			return last
		}
		if attempt == retries {
			return last
		}
		if backoffMs > 0 {
			time.Sleep(time.Duration(backoffMs*(attempt+1)) * time.Millisecond)
		}
		if maxTime < 60000 {
			maxTime *= 2
			if maxTime > 60000 {
				maxTime = 60000
			}
		}
	}
	return last
}

func runExplainForEvent(database *mongodrv.Database, ev stats.OperationEvent, maxTimeMs int, explainVerbosity string) explainEvidence {
	spec, reason := buildExplainCommandSpec(ev, maxTimeMs, explainVerbosity)
	if reason != "" {
		if reason == "unsupported_operation" {
			return explainEvidence{status: "not_supported", reason: reason}
		}
		return explainEvidence{status: "insufficient_metadata", reason: reason}
	}

	ctx, cancel := context.WithTimeout(context.Background(), spec.clientTimeout)
	defer cancel()
	start := time.Now()
	baseDiag := &stats.ExplainDiagnostics{
		ReplayDB:         nonEmptyOrFallback(ev.Database, database.Name()),
		ReplayCollection: ev.Collection,
		Verbosity:        spec.verbosity,
		ServerMaxTimeMS:  spec.serverMaxTime,
		ClientTimeoutMS:  int(spec.clientTimeout / time.Millisecond),
		StageSummary:     spec.stageSummary,
	}

	var out bson.M
	if err := database.RunCommand(ctx, spec.cmd, options.RunCmd()).Decode(&out); err != nil {
		reason := classifyExplainCommandError(err)
		baseDiag.ElapsedMS = int(time.Since(start) / time.Millisecond)
		log.Printf("[Insights Explain] op=%s db=%s coll=%s shape=%s mode=%s server_max_ms=%d client_timeout_ms=%d stages=%s result=%s elapsed_ms=%d",
			ev.Operation,
			baseDiag.ReplayDB,
			ev.Collection,
			stats.StableShapeID(ev.Operation, ev.Collection, ev.ShapeKey),
			spec.verbosity,
			spec.serverMaxTime,
			int(spec.clientTimeout/time.Millisecond),
			spec.stageSummary,
			reason,
			int(time.Since(start)/time.Millisecond),
		)
		if reason == "max_time_exceeded" || reason == "client_deadline_exceeded" {
			return explainEvidence{
				status: "timed_out",
				reason: reason,
				diag:   baseDiag,
			}
		}
		return explainEvidence{
			status: "execution_failed",
			reason: reason,
			diag:   baseDiag,
		}
	}
	reasonSignal := "no_scan_stage_detected"
	signals := parseExplainSignals(out)
	if signals.collectionScan {
		reasonSignal = "collscan_observed"
	} else if signals.indexScan {
		reasonSignal = "ixscan_observed"
	}
	baseDiag.ElapsedMS = int(time.Since(start) / time.Millisecond)
	applyExplainSignalsToDiagnostics(baseDiag, signals)
	log.Printf("[Insights Explain] op=%s db=%s coll=%s shape=%s mode=%s server_max_ms=%d client_timeout_ms=%d stages=%s result=explained elapsed_ms=%d",
		ev.Operation,
		baseDiag.ReplayDB,
		ev.Collection,
		stats.StableShapeID(ev.Operation, ev.Collection, ev.ShapeKey),
		spec.verbosity,
		spec.serverMaxTime,
		int(spec.clientTimeout/time.Millisecond),
		spec.stageSummary,
		int(time.Since(start)/time.Millisecond),
	)
	return explainEvidence{
		collscan: signals.collectionScan,
		ixscan:   signals.indexScan,
		status:   "explained",
		reason:   reasonSignal,
		diag:     baseDiag,
	}
}

func buildExplainCommandSpec(ev stats.OperationEvent, maxTimeMs int, explainVerbosity string) (explainCommandSpec, string) {
	if maxTimeMs <= 0 {
		maxTimeMs = 1000
	}
	if explainVerbosity != "queryPlanner" {
		explainVerbosity = "executionStats"
	}
	spec := explainCommandSpec{
		verbosity:     explainVerbosity,
		serverMaxTime: maxTimeMs,
		clientTimeout: time.Duration(maxTimeMs+2000) * time.Millisecond,
		commandType:   ev.Operation,
	}
	if spec.clientTimeout < 3*time.Second {
		spec.clientTimeout = 3 * time.Second
	}

	switch ev.Operation {
	case "find", "updateOne", "updateMany", "deleteOne", "deleteMany":
		if len(ev.FilterSample) == 0 {
			return explainCommandSpec{}, "missing_filter_sample"
		}
		inner := bson.D{
			{Key: "find", Value: ev.Collection},
			{Key: "filter", Value: ev.FilterSample},
			{Key: "limit", Value: 1},
			{Key: "maxTimeMS", Value: maxTimeMs},
		}
		spec.cmd = bson.D{{Key: "explain", Value: inner}, {Key: "verbosity", Value: spec.verbosity}}
		spec.stageSummary = summarizeFilterForLog(ev.FilterSample)
		return spec, ""
	case "aggregate":
		if len(ev.PipelineSample) == 0 {
			return explainCommandSpec{}, "missing_pipeline_sample"
		}
		inner := bson.D{
			{Key: "aggregate", Value: ev.Collection},
			{Key: "pipeline", Value: ev.PipelineSample},
			{Key: "cursor", Value: bson.M{}},
			{Key: "allowDiskUse", Value: true},
			{Key: "maxTimeMS", Value: maxTimeMs},
		}
		spec.cmd = bson.D{{Key: "explain", Value: inner}, {Key: "verbosity", Value: spec.verbosity}}
		spec.stageSummary = summarizePipelineForLog(ev.PipelineSample)
		return spec, ""
	default:
		return explainCommandSpec{}, "unsupported_operation"
	}
}

func classifyExplainCommandError(err error) string {
	if err == nil {
		return "run_command_failed"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "client_deadline_exceeded"
	}
	if errors.Is(err, context.Canceled) {
		return "client_context_canceled"
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "not authorized"):
		return "not_authorized"
	case strings.Contains(msg, "ns does not exist"), strings.Contains(msg, "namespace does not exist"):
		return "namespace_not_found"
	case strings.Contains(msg, "command not found"), strings.Contains(msg, "commandnotfound"):
		return "command_not_found"
	case strings.Contains(msg, "max time ms expired"), strings.Contains(msg, "maxtimems"):
		return "max_time_exceeded"
	default:
		return "run_command_failed"
	}
}

func supportsExplainOperation(op string) bool {
	switch op {
	case "find", "aggregate", "updateOne", "updateMany", "deleteOne", "deleteMany":
		return true
	default:
		return false
	}
}

func findExplainCandidate(events []stats.OperationEvent, sq stats.SlowQueryInsight) (stats.OperationEvent, string, bool) {
	shapeSeen := false
	var bestSuccess *stats.OperationEvent
	var bestAny *stats.OperationEvent
	for _, ev := range events {
		if stats.StableShapeID(ev.Operation, ev.Collection, ev.ShapeKey) != sq.ShapeID {
			continue
		}
		shapeSeen = true
		hasMetadata := false
		switch sq.Operation {
		case "aggregate":
			hasMetadata = len(ev.PipelineSample) > 0
		case "find", "updateOne", "updateMany", "deleteOne", "deleteMany":
			hasMetadata = len(ev.FilterSample) > 0
		}
		if !hasMetadata {
			continue
		}

		candidate := ev
		if bestAny == nil || candidate.DurationMs < bestAny.DurationMs {
			bestAny = &candidate
		}
		if candidate.Success && (bestSuccess == nil || candidate.DurationMs < bestSuccess.DurationMs) {
			bestSuccess = &candidate
		}
	}
	if bestSuccess != nil {
		return *bestSuccess, "", true
	}
	if bestAny != nil {
		return *bestAny, "", true
	}
	if !shapeSeen {
		return stats.OperationEvent{}, "shape_event_not_retained", false
	}
	if sq.Operation == "aggregate" {
		return stats.OperationEvent{}, "missing_pipeline_sample", false
	}
	return stats.OperationEvent{}, "missing_filter_sample", false
}

func summarizePipelineForLog(pipeline []interface{}) string {
	if len(pipeline) == 0 {
		return "[]"
	}
	stages := make([]string, 0, len(pipeline))
	for _, raw := range pipeline {
		stage, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		keys := make([]string, 0, len(stage))
		for k := range stage {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		stages = append(stages, strings.Join(keys, "+"))
	}
	if len(stages) == 0 {
		return "unknown_stages"
	}
	return strings.Join(stages, " -> ")
}

func summarizeFilterForLog(filter map[string]interface{}) string {
	if len(filter) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(filter))
	for k := range filter {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return "{" + strings.Join(keys, ",") + "}"
}

func parseExplainSignals(out bson.M) explainSignals {
	s := explainSignals{}
	stageSeen := make(map[string]struct{})
	indexSeen := make(map[string]struct{})

	addStage := func(stage string) {
		stage = strings.ToUpper(strings.TrimSpace(stage))
		if stage == "" {
			return
		}
		if _, ok := stageSeen[stage]; ok {
			return
		}
		stageSeen[stage] = struct{}{}
		s.planStages = append(s.planStages, stage)
	}
	addIndex := func(idx string) {
		idx = strings.TrimSpace(idx)
		if idx == "" {
			return
		}
		if _, ok := indexSeen[idx]; ok {
			return
		}
		indexSeen[idx] = struct{}{}
		s.indexesUsed = append(s.indexesUsed, idx)
	}
	updateMax := func(dst *int64, v int64) {
		if v > *dst {
			*dst = v
		}
	}
	processKey := func(k string, val interface{}) {
		if strings.HasPrefix(k, "$") && len(k) > 1 {
			addStage(strings.TrimPrefix(k, "$"))
		}
		kl := strings.ToLower(k)
		switch kl {
		case "stage", "stagename", "nodetype":
			if sv, ok := val.(string); ok {
				addStage(sv)
			}
		case "indexname":
			if sv, ok := val.(string); ok {
				addIndex(sv)
			}
		case "indexesused":
			for _, iv := range toStringSlice(val) {
				addIndex(iv)
			}
		case "totaldocsexamined":
			updateMax(&s.docsExamined, toInt64(val))
		case "totalkeysexamined":
			updateMax(&s.keysExamined, toInt64(val))
		case "nreturned":
			updateMax(&s.nReturned, toInt64(val))
		case "collectionscans":
			updateMax(&s.collectionScans, toInt64(val))
		case "seeks", "indexseeks", "totalseeks":
			updateMax(&s.indexSeeks, toInt64(val))
		case "executiontimemillis", "executiontimemillisestimate":
			updateMax(&s.executionTimeMS, toInt64(val))
		case "useddisk":
			if bv, ok := val.(bool); ok && bv {
				s.usedDisk = true
			}
		case "spills":
			sp := toInt64(val)
			if sp > 0 {
				updateMax(&s.spills, sp)
				s.usedDisk = true
			}
		}
	}

	var walk func(v interface{}, path string)
	walk = func(v interface{}, path string) {
		switch t := v.(type) {
		case bson.M:
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				val := t[k]
				processKey(k, val)
				walk(val, path+"."+k)
			}
		case map[string]interface{}:
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				val := t[k]
				processKey(k, val)
				walk(val, path+"."+k)
			}
		case bson.D:
			for _, elem := range t {
				processKey(elem.Key, elem.Value)
				walk(elem.Value, path+"."+elem.Key)
			}
		case []bson.E:
			for _, elem := range t {
				processKey(elem.Key, elem.Value)
				walk(elem.Value, path+"."+elem.Key)
			}
		case []interface{}:
			for i := range t {
				walk(t[i], fmt.Sprintf("%s[%d]", path, i))
			}
		case bson.A:
			for i := range t {
				walk(t[i], fmt.Sprintf("%s[%d]", path, i))
			}
		}
	}
	walk(out, "root")

	s.collectionScan = hasPlanStage(s.planStages, "COLLSCAN") || s.collectionScans > 0
	s.indexScan = hasAnyPlanStage(s.planStages, "IXSCAN", "IXSEEK", "DISTINCT_SCAN", "COUNT_SCAN") || len(s.indexesUsed) > 0 || s.indexSeeks > 0
	s.fetch = hasPlanStage(s.planStages, "FETCH")
	s.group = hasPlanStage(s.planStages, "GROUP")
	s.sort = hasPlanStage(s.planStages, "SORT")
	s.limit = hasPlanStage(s.planStages, "LIMIT")
	s.blockingSort = s.sort && !s.indexScan

	s.winningPlanSummary = strings.Join(s.planStages, " -> ")
	if s.winningPlanSummary == "" {
		s.winningPlanSummary = "stage_chain_unavailable"
	}
	s.shardSummary = buildShardSummary(out)
	return s
}

func applyExplainSignalsToDiagnostics(diag *stats.ExplainDiagnostics, sig explainSignals) {
	if diag == nil {
		return
	}
	diag.WinningPlanSummary = sig.winningPlanSummary
	diag.PlanStages = sig.planStages
	diag.IndexesUsed = sig.indexesUsed
	diag.CollectionScanDetected = sig.collectionScan
	diag.IndexScanDetected = sig.indexScan
	diag.FetchDetected = sig.fetch
	diag.GroupDetected = sig.group
	diag.SortDetected = sig.sort
	diag.LimitDetected = sig.limit
	diag.BlockingSortDetected = sig.blockingSort
	diag.UsedDisk = sig.usedDisk
	diag.DocsExamined = sig.docsExamined
	diag.KeysExamined = sig.keysExamined
	diag.NReturned = sig.nReturned
	diag.CollectionScans = sig.collectionScans
	diag.IndexSeeks = sig.indexSeeks
	diag.ExecutionTimeMillis = sig.executionTimeMS
	diag.Spills = sig.spills
	if sig.nReturned > 0 {
		diag.ExaminedToReturnedRatio = float64(sig.docsExamined) / float64(sig.nReturned)
		diag.KeysToReturnedRatio = float64(sig.keysExamined) / float64(sig.nReturned)
	}
	diag.ShardDetailsSummary = sig.shardSummary
	diag.Interpretation, diag.Recommendation, diag.RecommendationConfidence, diag.EvidenceSummary = buildExplainRecommendation(sig)
}

func buildExplainRecommendation(sig explainSignals) (interpretation, recommendation, confidence, evidence string) {
	evidenceParts := make([]string, 0, 8)
	if sig.indexScan {
		evidenceParts = append(evidenceParts, "IXSCAN detected")
	} else {
		evidenceParts = append(evidenceParts, "IXSCAN not detected")
	}
	if sig.collectionScan {
		evidenceParts = append(evidenceParts, "COLLSCAN detected")
	} else {
		evidenceParts = append(evidenceParts, "COLLSCAN not detected")
	}
	if sig.docsExamined > 0 {
		evidenceParts = append(evidenceParts, fmt.Sprintf("docsExamined=%d", sig.docsExamined))
	}
	if sig.keysExamined > 0 {
		evidenceParts = append(evidenceParts, fmt.Sprintf("keysExamined=%d", sig.keysExamined))
	}
	if sig.nReturned > 0 {
		evidenceParts = append(evidenceParts, fmt.Sprintf("nReturned=%d", sig.nReturned))
	}
	if len(sig.indexesUsed) > 0 {
		evidenceParts = append(evidenceParts, "indexesUsed="+strings.Join(sig.indexesUsed, ","))
	}
	evidence = strings.Join(evidenceParts, "; ")

	highFanout := sig.nReturned > 0 && sig.docsExamined > sig.nReturned*20
	switch {
	case sig.collectionScan:
		interpretation = "Explain indicates a collection scan path. Query appears under-indexed for its filter shape."
		recommendation = "Add or validate a selective index on the match/filter fields, then compare docsExamined and latency before rollout."
		confidence = "high"
	case sig.indexScan && highFanout:
		interpretation = "An index is used, but examined-document volume is still high relative to rows returned."
		recommendation = "Refine index selectivity (for example, a more targeted compound index) and review predicate/cardinality to reduce scanned documents."
		confidence = "high"
	case sig.indexScan && (sig.group || sig.sort):
		interpretation = "Query is index-backed, but aggregation/sort stages are likely contributing most of the runtime cost."
		recommendation = "Focus on reducing post-match workload: tighten early match/project stages, review group cardinality, and evaluate sort pressure."
		confidence = "medium"
	case sig.indexScan:
		interpretation = "Index-backed plan detected with no collection scan signal."
		recommendation = "Treat this as index-supported; investigate residual latency via cardinality, document size, and pipeline/operator cost."
		confidence = "medium"
	default:
		interpretation = "Explain completed, but a clear index/scan signal was not detected in parsed plan fields."
		recommendation = "Capture additional explain samples and verify winning plan details directly before making index changes."
		confidence = "low"
	}

	if sig.usedDisk {
		recommendation += " Explain reports disk/spill usage; review memory-heavy sort/group stages."
	}
	return interpretation, recommendation, confidence, evidence
}

func hasPlanStage(stages []string, target string) bool {
	target = strings.ToUpper(strings.TrimSpace(target))
	for _, s := range stages {
		if strings.EqualFold(s, target) {
			return true
		}
	}
	return false
}

func hasAnyPlanStage(stages []string, targets ...string) bool {
	for _, target := range targets {
		if hasPlanStage(stages, target) {
			return true
		}
	}
	return false
}

func toInt64(v interface{}) int64 {
	switch t := v.(type) {
	case int:
		return int64(t)
	case int32:
		return int64(t)
	case int64:
		return t
	case float32:
		return int64(t)
	case float64:
		return int64(t)
	case json.Number:
		i, _ := t.Int64()
		return i
	default:
		return 0
	}
}

func toStringSlice(v interface{}) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []interface{}:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case bson.A:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func buildShardSummary(out bson.M) string {
	shardsRaw, ok := out["shards"]
	if !ok {
		return ""
	}
	var shardsMap map[string]interface{}
	switch t := shardsRaw.(type) {
	case map[string]interface{}:
		shardsMap = t
	case bson.M:
		shardsMap = map[string]interface{}(t)
	default:
		return ""
	}
	if len(shardsMap) == 0 {
		return ""
	}
	names := make([]string, 0, len(shardsMap))
	for name := range shardsMap {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		sig := explainSignals{}
		switch t := shardsMap[name].(type) {
		case map[string]interface{}:
			b, _ := json.Marshal(t)
			var m bson.M
			_ = json.Unmarshal(b, &m)
			sig = parseExplainSignals(m)
		case bson.M:
			sig = parseExplainSignals(t)
		default:
			continue
		}
		parts = append(parts, fmt.Sprintf("%s docs=%d keys=%d returned=%d", name, sig.docsExamined, sig.keysExamined, sig.nReturned))
	}
	return strings.Join(parts, "; ")
}

func nonEmptyOrFallback(v, fb string) string {
	if strings.TrimSpace(v) == "" {
		return fb
	}
	return v
}

func applyExplainToSlowQueries(rep *stats.InsightsReport, results map[string]explainEvidence) {
	for i := range rep.SlowQueries {
		s := &rep.SlowQueries[i]
		if ev, ok := results[s.ShapeID]; ok {
			s.ExplainStatus = ev.status
			s.ExplainReason = ev.reason
			s.ExplainDiag = ev.diag
			continue
		}
		if supportsExplainOperation(s.Operation) {
			s.ExplainStatus = "explain_unavailable"
			s.ExplainReason = "shape_not_in_explain_results"
		} else {
			s.ExplainStatus = "not_supported"
			s.ExplainReason = fmt.Sprintf("operation_%s_not_supported", s.Operation)
		}
	}

	byShape := make(map[string]explainEvidence, len(results))
	for k, v := range results {
		byShape[k] = v
	}
	for i := range rep.QueryShapes {
		s := &rep.QueryShapes[i]
		if ev, ok := byShape[s.ShapeID]; ok {
			s.ExplainStatus = ev.status
			s.ExplainReason = ev.reason
			s.ExplainDiag = ev.diag
			continue
		}
		if supportsExplainOperation(s.Operation) {
			s.ExplainStatus = "explain_unavailable"
			s.ExplainReason = "shape_not_in_explain_results"
		} else {
			s.ExplainStatus = "not_supported"
			s.ExplainReason = fmt.Sprintf("operation_%s_not_supported", s.Operation)
		}
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
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate serial: %w", err)
	}

	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{Organization: []string{"PLGM Web UI"}, CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour * 24 * 365),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		// Cover the loopback names the UI is reachable by so hostname
		// verification passes for both http://localhost and http://127.0.0.1.
		DNSNames:    []string{"localhost"},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create certificate: %w", err)
	}
	certPem := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("marshal key: %w", err)
	}
	keyPem := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
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
