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
	"math"
	"math/big"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
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
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/multitarget"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/replaycompare"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/report"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/schemainfer"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	"go.mongodb.org/mongo-driver/v2/bson"
)

//go:embed static
var staticFiles embed.FS

type WebServer struct {
	mu               sync.Mutex
	runWG            sync.WaitGroup
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
	TrendStore       *stats.TrendStore
	// trendStoreSetting is the trend_baseline_store value TrendStore was opened with.
	trendStoreSetting string
	insightsBuildMu   sync.Mutex
	// explainCancels stop in-flight post-run explains when a new run starts;
	// concurrent multi-target runs explain several targets at once.
	explainCancels    map[uint64]context.CancelFunc
	explainCancelSeq  uint64
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
	// replayCompares are the kept replay comparison reports, oldest first.
	replayCompares []*replaycompare.Report
	// trafficImports are the kept converted recordings, oldest first.
	trafficImports []*trafficImport
	// recordingDirs are listed by /api/recordings (from the startup
	// config); sessionRecordings are files record runs wrote, oldest first.
	recordingDirs     []string
	sessionRecordings []string
	// generated are the paths the last run got because their settings were
	// blank; they are cleared from the config served to the form and from
	// the base of the next run, so every run gets new names.
	generated []*generatedFile
	// startupConfig is the configuration PLGM started with (config.yaml and
	// environment), served for "Use defaults" and "Reset to defaults".
	startupConfig config.AppConfig
	// multiTarget is the plan of the current or last multi-target run;
	// multiTargetRefInsights is its reference target's final insights.
	multiTarget            *multitarget.Plan
	multiTargetRefInsights *stats.InsightsReport
	// runHistory stores completed dashboards. It is nil in unit tests unless a
	// test assigns a temp store, so tests do not write the user config directory.
	runHistory    *RunHistory
	dashboardID   string
	dashboardMode string
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
	var history *RunHistory
	if !runningUnderTest() {
		history = OpenRunHistory(runDashboardDir(cfg))
	}
	s := &WebServer{
		AppConfig:       cfg,
		TrendStore:      openTrendStoreFor(cfg),
		LifecyclePhase:  "idle",
		DefinitionStore: defStore,
		runHistory:      history,
		recordingDirs:   serverRecordingDirs(cfg),
	}
	if cfg != nil {
		s.startupConfig = *cfg
		s.trendStoreSetting = cfg.TrendBaselineStore
	}
	return s
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

func tempNamePattern(prefix string, header *multipart.FileHeader) string {
	ext := filepath.Ext(header.Filename)
	if ext == "" {
		ext = ".pem"
	}
	return prefix + ext
}

func saveUploadedTempFile(file multipart.File, pattern string) (string, error) {
	tmp, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", err
	}
	defer tmp.Close()

	if _, err := io.Copy(tmp, file); err != nil {
		_ = os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

// removeReplayUpload deletes an uploaded replay file after its run and clears
// it from the shown config, which later runs also start from.
func (s *WebServer) removeReplayUpload(path string) {
	_ = os.Remove(path)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.AppConfig != nil && s.AppConfig.WorkloadReplayPath == path {
		cfg := *s.AppConfig
		cfg.WorkloadReplayPath = ""
		s.AppConfig = &cfg
	}
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
	mux.HandleFunc("/api/trend-baselines", s.handleTrendBaselines)
	mux.HandleFunc("/api/trend-baselines/", s.handleTrendBaselineItem)
	mux.HandleFunc("/api/trends/compare", s.handleTrendCompare)
	mux.HandleFunc("/api/replay-compare", s.handleReplayCompare)
	mux.HandleFunc("/api/replay-compare/", s.handleReplayCompareItem)
	mux.HandleFunc("/api/import-traffic", s.handleImportTraffic)
	mux.HandleFunc("/api/import-traffic/", s.handleImportTrafficItem)
	mux.HandleFunc("/api/recordings", s.handleRecordings)
	mux.HandleFunc("/api/shutdown", s.handleShutdown)
	mux.HandleFunc("/api/run-history", s.handleRunHistory)
	mux.HandleFunc("/api/run-history/", s.handleRunHistoryItem)
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
	s.mu.Lock()
	safeCfg := *s.AppConfig
	if r.URL.Query().Get("defaults") == "1" {
		safeCfg = s.startupConfig
	}
	forgetGeneratedPaths(&safeCfg, s.generated)
	s.mu.Unlock()
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
		if isBuiltinDefinitionID(strings.TrimSpace(in.SourceDefinitionID)) {
			// Built-in definitions are always in declared sort order; only the version is checked.
			source, err := s.getDefinition(kind, strings.TrimSpace(in.SourceDefinitionID))
			if errors.Is(err, os.ErrNotExist) {
				err = fmt.Errorf("%w: source %s definition %q does not exist", definitions.ErrSourceDefinitionChanged, kind, in.SourceDefinitionID)
			}
			if err == nil {
				err = definitions.CheckCopySource(source, in)
			}
			if err != nil {
				writeDefinitionCreateError(w, err)
				return
			}
			in.SourceDefinitionID, in.SourceUpdatedAt, in.SourceContentVersion = "", "", 0
		}
		def, err := s.DefinitionStore.Create(kind, in)
		if err != nil {
			writeDefinitionCreateError(w, err)
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
			if writeSortOrderReviewError(w, err) {
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
			ContentVersion: definitions.ContentVersionOrderedSorts,
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

func writeDefinitionCreateError(w http.ResponseWriter, err error) {
	if writeSortOrderReviewError(w, err) {
		return
	}
	if errors.Is(err, definitions.ErrSourceDefinitionChanged) {
		writeAPIError(w, http.StatusConflict, err.Error())
		return
	}
	writeAPIError(w, http.StatusBadRequest, err.Error())
}

// writeSortOrderReviewError writes a *definitions.SortOrderReviewError as a
// 409 with its structured review and reports whether err was one.
func writeSortOrderReviewError(w http.ResponseWriter, err error) bool {
	var reviewErr *definitions.SortOrderReviewError
	if !errors.As(err, &reviewErr) {
		return false
	}
	writeJSON(w, http.StatusConflict, map[string]interface{}{
		"status":          "error",
		"code":            definitions.SortOrderReviewErrorCode,
		"message":         reviewErr.Message,
		"sortOrderReview": reviewErr.Review,
	})
	return true
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

// applyLatencyTargetForm sets the latency targets that were submitted; a
// submitted empty, non-positive, or invalid value clears that target.
func applyLatencyTargetForm(cfg *config.AppConfig, form url.Values) {
	for field, dst := range map[string]*float64{
		"insights_latency_target_avg_ms": &cfg.InsightsLatencyTargetAvgMs,
		"insights_latency_target_p95_ms": &cfg.InsightsLatencyTargetP95Ms,
		"insights_latency_target_p99_ms": &cfg.InsightsLatencyTargetP99Ms,
	} {
		if _, ok := form[field]; ok {
			if v := parseFloat(form.Get(field), 0); v > 0 && !math.IsInf(v, 0) {
				*dst = v
			} else {
				*dst = 0
			}
		}
	}
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
	s.multiTarget, s.multiTargetRefInsights = nil, nil
	s.cancelExplainsLocked()
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
	// A name generated for an earlier run is never reused.
	forgetGeneratedPaths(&baseCfg, s.generated)
	s.mu.Unlock()

	// An uploaded replay file holds literal values; it is removed when the
	// run ends, or here when the run does not start.
	var replayUpload string
	replayHandedOff := false
	// Outcome files reserved for a blank outcome path are removed while
	// still empty if the run does not start, or when it ends.
	var generated []*generatedFile
	defer func() {
		if replayUpload != "" && !replayHandedOff {
			_ = os.Remove(replayUpload)
		}
		if !replayHandedOff {
			removeEmptyFiles(generatedPaths(generated))
		}
	}()

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
	if file, _, err := r.FormFile("tls_ca_file"); err == nil {
		uploadedPath, uploadErr := saveUploadedTempFile(file, "plgm-tls-ca-*")
		file.Close()
		if uploadErr != nil {
			s.abortRun("Failed to store TLS CA file: " + uploadErr.Error())
			writeStartError(w, http.StatusBadRequest, "failed to store TLS CA file: "+uploadErr.Error())
			return
		}
		cfg.ConnectionParams.TLSCAFile = uploadedPath
	}
	if file, header, err := r.FormFile("tlsCertificateKeyFile"); err == nil {
		uploadedPath, uploadErr := saveUploadedTempFile(file, tempNamePattern("plgm-tls-client-*", header))
		file.Close()
		if uploadErr != nil {
			s.abortRun("Failed to store TLS certificate key file: " + uploadErr.Error())
			writeStartError(w, http.StatusBadRequest, "failed to store TLS certificate key file: "+uploadErr.Error())
			return
		}
		cfg.ConnectionParams.TLSCertificateKeyFile = uploadedPath
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
	if mode := r.FormValue("insights_explain_efficiency_mode"); mode != "" {
		cfg.InsightsExplainEfficiencyMode = mode
	}
	cfg.InsightsExplainEfficiencyMode = config.NormalizeExplainEfficiencyMode(cfg.InsightsExplainEfficiencyMode)
	switch cfg.InsightsExplainSeverityMode {
	case "high_and_low", "medium_only", "critical_only", "high_only":
	default:
		cfg.InsightsExplainSeverityMode = "high_only"
	}
	cfg.InsightsExplainWorkers = parseInt(r.FormValue("insights_explain_workers"), cfg.InsightsExplainWorkers)
	cfg.InsightsExplainRetries = parseInt(r.FormValue("insights_explain_retries"), cfg.InsightsExplainRetries)
	cfg.InsightsExplainBackoffMS = parseInt(r.FormValue("insights_explain_backoff_ms"), cfg.InsightsExplainBackoffMS)
	cfg.InsightsTrendStoreQueryValues = r.FormValue("insights_trend_store_query_values") == "true" || r.FormValue("insights_trend_store_query_values") == "on"
	cfg.WorkloadDeterministic = r.FormValue("workload_deterministic") == "true" || r.FormValue("workload_deterministic") == "on"
	applyLatencyTargetForm(cfg, r.Form)
	if err := applyTrendForm(cfg, r.Form); err != nil {
		s.abortRun(err.Error())
		writeStartError(w, http.StatusBadRequest, err.Error())
		return
	}
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
	if err := applyWorkloadRecordForm(cfg, r.Form); err != nil {
		s.abortRun("Invalid workload record/replay settings: " + err.Error())
		writeStartError(w, http.StatusBadRequest, err.Error())
		return
	}
	if file, _, err := r.FormFile("workload_replay_file"); err == nil {
		uploadedPath, uploadErr := saveUploadedTempFile(file, "plgm-workload-replay-*.ndjson")
		file.Close()
		if uploadErr != nil {
			s.abortRun("Failed to store workload replay file: " + uploadErr.Error())
			writeStartError(w, http.StatusBadRequest, "failed to store workload replay file: "+uploadErr.Error())
			return
		}
		cfg.WorkloadReplayPath = uploadedPath
		replayUpload = uploadedPath
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
	cfg.SeedConcurrency = parseInt(r.FormValue("seed_concurrency"), cfg.SeedConcurrency)
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

	mode := strings.TrimSpace(r.FormValue("operation_mode"))
	if err := applyOperationMode(cfg, mode); err != nil {
		s.abortRun(err.Error())
		writeStartError(w, http.StatusBadRequest, err.Error())
		return
	}
	if mode == "" {
		mode = inferDashboardMode(cfg)
	}
	s.mu.Lock()
	s.dashboardMode = mode
	s.mu.Unlock()

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
			if err := def.CheckSortOrder(parsed.Queries); err != nil {
				s.abortRun("Ambiguous sort order in selected query definition: " + err.Error())
				if !writeSortOrderReviewError(w, err) {
					writeStartError(w, http.StatusBadRequest, err.Error())
				}
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
	if cfg.SeedConcurrency <= 0 {
		cfg.SeedConcurrency = 1
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
	if err := config.ValidateConnectionParams(cfg); err != nil {
		s.abortRun("Invalid connection settings: " + err.Error())
		writeStartError(w, http.StatusBadRequest, err.Error())
		return
	}
	// A blank recording name gets a new file per run instead of the fixed
	// default; it is reserved after validation, like generated outcome files.
	generateRecordPath := recordPathToGenerate(cfg)
	if generateRecordPath {
		cfg.WorkloadRecordPath = pendingRecordPath
	}
	config.NormalizeWorkloadRecordReplay(cfg)
	if err := config.ValidateWorkloadRecordReplay(cfg); err != nil {
		s.abortRun(err.Error())
		writeStartError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := config.ValidateWorkloadDeterministic(cfg); err != nil {
		s.abortRun(err.Error())
		writeStartError(w, http.StatusBadRequest, err.Error())
		return
	}
	// A blank outcome path the run needs gets a generated name; the files
	// are reserved only once the settings (target labels included) are valid.
	outcomeField := outcomePathToGenerate(cfg)
	if outcomeField != "" {
		setOutcomePath(cfg, outcomeField, pendingOutcomePath)
	}
	if err := config.ValidateWorkloadOutcomes(cfg); err != nil {
		s.abortRun(err.Error())
		writeStartError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := config.ValidateWorkloadTargets(cfg); err != nil {
		s.abortRun(err.Error())
		writeStartError(w, http.StatusBadRequest, err.Error())
		return
	}
	if multitarget.Enabled(cfg) && cfg.RawInjector.Enabled {
		msg := "invalid multi-target settings: workload_replay_targets cannot be combined with the raw injector"
		s.abortRun(msg)
		writeStartError(w, http.StatusBadRequest, msg)
		return
	}
	if cfg.WorkloadReplayEnabled && replayUpload == "" {
		if err := checkReplayRecording(strings.TrimSpace(cfg.WorkloadReplayPath)); err != nil {
			msg := "invalid workload record/replay settings: " + err.Error()
			s.abortRun(msg)
			writeStartError(w, http.StatusBadRequest, msg)
			return
		}
	}
	genDir := "."
	if dirs := s.recordingDirs; len(dirs) > 0 {
		genDir = dirs[0]
	}
	if generateRecordPath {
		cfg.WorkloadRecordPath = ""
		g, err := assignRecordPath(cfg, genDir)
		if err != nil {
			msg := "invalid workload record/replay settings: " + err.Error()
			s.abortRun(msg)
			writeStartError(w, http.StatusBadRequest, msg)
			return
		}
		generated = append(generated, g)
	}
	if outcomeField != "" {
		setOutcomePath(cfg, outcomeField, "")
		g, err := assignOutcomePaths(cfg, genDir)
		if err != nil {
			msg := "invalid workload outcome settings: " + err.Error()
			s.abortRun(msg)
			writeStartError(w, http.StatusBadRequest, msg)
			return
		}
		generated = append(generated, g)
	}
	if len(generated) > 0 {
		s.mu.Lock()
		s.generated = generated
		for _, g := range generated {
			s.addLifecycleEventLocked("config", g.note())
		}
		s.mu.Unlock()
	}
	if cfg.WorkloadRecordEnabled {
		s.rememberRecording(cfg.WorkloadRecordPath)
	}

	s.mu.Lock()
	s.dashboardID = newDashboardID(s.RunID)
	s.mu.Unlock()

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
		s.useTrendStoreLocked(cfg)
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

		s.runWG.Add(1)
		go func() {
			defer s.runWG.Done()
			defer s.persistDashboard()
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
			// Deferred LIFO: disconnect must finish before IsRunning is cleared,
			// otherwise a new run can start while the old connection closes.
			defer func() {
				s.mu.Lock()
				s.IsRunning = false
				if s.LastError == "" {
					s.setLifecyclePhaseLocked("completed", "Raw injector run completed")
				}
				s.mu.Unlock()
			}()
			defer disconnectFn(benchConn, context.Background())

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

	if multitarget.Enabled(cfg) {
		replayHandedOff = true
		s.startMultiTarget(w, ctx, cancel, cfg, collectionsCfg, queriesCfg, dbName, replayUpload, generated)
		return
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
	s.CurrentStats.SetCollectionsForInsights(collectionsCfg.Collections)
	s.AppConfig = cfg
	s.useTrendStoreLocked(cfg)
	s.ActiveCancel = cancel
	s.LastError = ""
	s.CurrentIteration = 1
	s.TotalIterations = config.RunIterations(cfg)
	s.IntervalStr = cfg.IntervalDelay
	s.IsWaiting = false
	s.CurrentStats.CurrentIteration = 1
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, startedResponse(generated))

	s.runWG.Add(1)
	replayHandedOff = true
	go func() {
		defer s.runWG.Done()
		defer s.persistDashboard()
		if replayUpload != "" {
			defer s.removeReplayUpload(replayUpload)
		}
		if len(generated) > 0 {
			defer removeEmptyFiles(generatedPaths(generated))
		}
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
		// Deferred LIFO: disconnect must finish before IsRunning is cleared,
		// otherwise a new run can start while the old connection closes.
		defer func() {
			s.mu.Lock()
			s.IsRunning = false
			if s.LastError == "" {
				s.setLifecyclePhaseLocked("completed", "Workload completed")
			}
			s.mu.Unlock()
		}()
		defer disconnectFn(benchConn, context.Background())

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
			// Progress is in documents, not collections, so one large collection
			// moves past 0% as batches land instead of jumping only when it finishes.
			totalDocs := len(collectionsCfg.Collections) * cfg.DocumentsCount
			if totalDocs < 1 {
				totalDocs = 1
			}
			s.mu.Lock()
			s.setLifecycleStepLocked("Seeding initial data", 5, 5)
			s.setLifecycleStepProgressLocked(0, totalDocs)
			s.addLifecycleEventLocked("init", fmt.Sprintf("Seeding up to %d documents per collection", cfg.DocumentsCount))
			s.mu.Unlock()
			doneDocs := 0
			for _, col := range collectionsCfg.Collections {
				colCtx := mongo.WithSeedProgress(ctx, func(inserted, total int) {
					if inserted < 0 {
						inserted = 0
					}
					if total > 0 && inserted > total {
						inserted = total
					}
					s.mu.Lock()
					done := doneDocs + inserted
					if done > s.LifecycleStepDone {
						s.setLifecycleStepProgressLocked(done, totalDocs)
					}
					s.mu.Unlock()
				})
				if err := insertRandomDocsFn(colCtx, benchConn.Database, col, cfg.DocumentsCount, cfg); err != nil {
					msg := fmt.Sprintf("Failed during data seeding: %v", err)
					log.Println("UI Run Error:", msg)
					s.mu.Lock()
					s.LastError = msg
					s.setLifecyclePhaseLocked("failed", "Initialization failed while seeding data")
					s.mu.Unlock()
					return
				}
				doneDocs += cfg.DocumentsCount
				s.mu.Lock()
				if doneDocs > s.LifecycleStepDone {
					s.setLifecycleStepProgressLocked(doneDocs, totalDocs)
				}
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
		iterations := config.RunIterations(cfg)
		runCtx, finishRecording := mongo.WithWorkloadRecordingSession(ctx)
		defer finishRecording()

		for i := 1; i <= iterations; i++ {
			if ctx.Err() != nil {
				break
			}

			s.mu.Lock()
			s.CurrentIteration = i
			s.IsWaiting = false
			s.CurrentStats.CurrentIteration = i
			s.mu.Unlock()

			if err := runWorkloadFn(runCtx, benchConn.Database, collectionsCfg.Collections, queriesCfg.Queries, cfg, s.CurrentStats); err != nil {
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

			if i < iterations && intervalDuration > 0 {
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
	workloadDeterministic := false
	operationsPerWorker := 0
	if s.AppConfig != nil {
		durationStr = s.AppConfig.Duration
		if k := strings.TrimSpace(s.AppConfig.LoadProfile.Kind); k != "" {
			loadProfileKind = strings.ToLower(k)
		}
		configuredConcurrency = s.AppConfig.Concurrency
		workloadDeterministic = s.AppConfig.WorkloadDeterministic
		operationsPerWorker = s.AppConfig.OperationsPerWorker
	}
	outcomeFile := runOutcomeFile(s.AppConfig)
	recordingFile := runRecordingFile(s.AppConfig)
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
	plan := s.multiTarget
	// Target iterations are written under s.mu by the run.
	targetIterations := map[*stats.Collector]int{}
	if plan != nil {
		for _, t := range plan.Targets {
			if t.Collector != nil {
				targetIterations[t.Collector] = t.Collector.CurrentIteration
			}
		}
	}
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
	// The execution clock stays at zero until the measured run starts. During
	// preparation the header still needs a clock that moves, so report time
	// since initialization began separately.
	initElapsedSec := initDurationSec
	if lifecyclePhase == "initializing" && !initStartedAt.IsZero() {
		initElapsedSec = now.Sub(initStartedAt).Seconds()
	}
	if initElapsedSec < 0 {
		initElapsedSec = 0
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
			"initializationElapsedSec":  initElapsedSec,
			"executionElapsedSec":       execElapsedSec,
			"workloadDeterministic":     workloadDeterministic,
			"operationsPerWorker":       operationsPerWorker,
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
				"initialization_elapsed_sec":  initElapsedSec,
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
	concTarget, concActive := collector.ConcurrencySnapshot()
	// Snapshot the UI interval once, keep the point, and return only samples
	// newer than chartSince so a reloaded page can redraw the run so far.
	// With replay targets, every running target is sampled on its own clock.
	var intervalLatency map[string]map[string]float64
	if plan != nil {
		intervalLatency = sampleRunningTargets(plan, collector, targetIterations, isWait, now)
	} else {
		collector.CaptureLatencyIntervalEvery(time.Second)
		intervalLatency = collector.GetUILatencyTimelineAndReset()
		if lifecyclePhase == "running" || lifecyclePhase == "completed" || lifecyclePhase == "failed" {
			collector.RecordUIChartSample(execElapsedSec, curIter, isWait, intervalLatency)
		}
	}
	chartHistory := collector.UIChartHistory(chartSinceParam(r))

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
		"initializationElapsedSec":  initElapsedSec,
		"executionElapsedSec":       execElapsedSec,
		"workloadDeterministic":     workloadDeterministic,
		"operationsPerWorker":       operationsPerWorker,
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
			"initialization_elapsed_sec":  initElapsedSec,
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
		"intervalLatency": intervalLatency,
		"chartHistory":    chartHistory,
		"latencyHeatmap":  collector.LatencyHeatmap(),
	}
	if mt := multiTargetStatus(plan); mt != nil {
		statsResp["multi_target"] = mt
		statsResp["targets"] = multiTargetSeries(plan, parseTargetSince(r))
	} else if outcomeFile != "" && lifecyclePhase != "idle" {
		statsResp["outcomeFile"] = outcomeFile
	}
	if recordingFile != "" && lifecyclePhase != "idle" {
		statsResp["recordingFile"] = recordingFile
	}
	if g := collector.GeneratedTraffic(); g != nil {
		statsResp["generatedTraffic"] = g
	}
	json.NewEncoder(w).Encode(statsResp)
}

func chartSinceParam(r *http.Request) int {
	n, err := strconv.Atoi(r.URL.Query().Get("chartSince"))
	if err != nil || n < 0 {
		return 0
	}
	return n
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

// generatedTrafficItems lists the traffic fingerprint of a deterministic run
// (hashes and counts only); nil for any other run.
func generatedTrafficItems(g *stats.GeneratedTrafficInfo) []report.KV {
	if g == nil {
		return nil
	}
	status := g.Status
	if g.Reason != "" {
		status += " (" + g.Reason + ")"
	}
	items := []report.KV{
		{Key: "Fingerprint", Value: g.Fingerprint},
		{Key: "Status", Value: status},
		{Key: "Random Seed", Value: strconv.FormatInt(g.RandomSeed, 10)},
		{Key: "Workers x Operations", Value: fmt.Sprintf("%d x %d per iteration, %d iteration(s)", g.Workers, g.OperationsPerWorker, g.Iterations)},
		{Key: "Operations planned / generated / sent", Value: fmt.Sprintf("%d / %d / %d", g.OpsPlanned, g.OpsGenerated, g.OpsSent)},
		{Key: "PLGM Version", Value: g.PLGMVersion},
		{Key: "Collections Hash", Value: g.CollectionsHash},
		{Key: "Queries Hash", Value: g.QueriesHash},
		{Key: "Commands Hash", Value: g.CommandsHash},
	}
	if g.SafetyTimeout != "" {
		items = append(items, report.KV{Key: "Duration", Value: g.SafetyTimeout})
	}
	workers := make([]int, 0, len(g.WorkerHashes))
	for k := range g.WorkerHashes {
		if n, err := strconv.Atoi(k); err == nil {
			workers = append(workers, n)
		}
	}
	sort.Ints(workers)
	for _, w := range workers {
		items = append(items, report.KV{Key: fmt.Sprintf("Worker %d Hash", w), Value: g.WorkerHashes[strconv.Itoa(w)]})
	}
	return items
}

// buildReportData snapshots the current run state into a render-ready ReportData.
func (s *WebServer) buildReportData() report.ReportData {
	s.mu.Lock()
	collector := s.CurrentStats
	cfg := s.AppConfig
	lastErr := s.LastError
	isRunning := s.IsRunning
	completedAt := s.CompletedAt
	plan := s.multiTarget
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
		data.GeneratedTrafficItems = generatedTrafficItems(collector.GeneratedTraffic())
		data.MultiTargetItems, data.MultiTargetNotes = multiTargetReportItems(plan)
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
		if !isRunning {
			if rep, _ := s.finalInsightsReport(); rep != nil {
				data.Impact = reportImpactSection(rep.Impact)
				data.Efficiency = reportEfficiencySection(rep.EfficiencyView, rep.Recommendations)
				data.Trends = reportTrendSection(rep.Trends)
				data.Recommendations = reportRecommendations(rep.Recommendations)
			}
		}
	}

	return data
}

// reportRecommendations maps insights recommendations into the HTML report
// model so the report shows the same list as /api/insights.
func reportRecommendations(recs []stats.InsightRecommendation) []report.RecommendationRow {
	out := make([]report.RecommendationRow, 0, len(recs))
	for _, r := range recs {
		out = append(out, report.RecommendationRow{
			Rank:       r.Rank,
			Priority:   r.Priority,
			Title:      r.Title,
			Details:    r.Details,
			Confidence: r.Confidence,
			Source:     r.Source,
		})
	}
	return out
}

// reportImpactSection maps the insights impact view into the HTML report model
// so the report shows the same values as /api/insights.
func reportImpactSection(v stats.ImpactView) *report.ImpactSection {
	if v.Status == "" || v.Status == "disabled" {
		return nil
	}
	sec := &report.ImpactSection{
		Status:             v.Status,
		TimeBasisNote:      v.Coverage.TimeBasisNote,
		InterpretationNote: v.Coverage.InterpretationNote,
		ObservedOps:        v.Coverage.ObservedOperations,
		ObservedMs:         v.Coverage.ObservedOperationMs,
		Limitations:        v.Coverage.Limitations,
	}
	for _, g := range v.Groups {
		p95 := g.SamplePercentileStatus
		if g.SampleP95Ms != nil {
			p95 = fmt.Sprintf("%.2f", *g.SampleP95Ms)
		}
		sec.Rows = append(sec.Rows, report.ImpactRow{
			Rank:           g.Rank,
			Namespace:      g.Namespace,
			Operation:      g.Operation,
			Shape:          g.ShapeSummary,
			OperationCount: g.OperationCount,
			TotalMs:        g.TotalOperationMs,
			SharePct:       g.ShareOfObservedTime * 100,
			AvgMs:          g.AvgMs,
			SampleCount:    g.SampleCount,
			SampleP95:      p95,
			Explanation:    g.Explanation,
		})
	}
	return sec
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
	rep, state := s.finalInsightsReport()
	w.Header().Set("Content-Type", "application/json")
	if rep == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"metadata": map[string]interface{}{"status": state},
		})
		return
	}
	json.NewEncoder(w).Encode(rep)
}

// finalInsightsReport returns the post-run insights with explain enrichment,
// computing them once per run so the API, HTML report, and exports share one
// result. It returns nil with state "inactive" or "pending" when unavailable.
func (s *WebServer) finalInsightsReport() (*stats.InsightsReport, string) {
	s.insightsBuildMu.Lock()
	defer s.insightsBuildMu.Unlock()

	s.mu.Lock()
	collector := s.CurrentStats
	running := s.IsRunning
	runID := s.RunID
	cached := s.InsightsCache
	appCfg := s.AppConfig
	trendStore := s.TrendStore
	s.mu.Unlock()

	if collector == nil {
		return nil, "inactive"
	}
	if running {
		return nil, "pending"
	}
	if cached != nil {
		return cached, "ready"
	}

	now := trendClock()
	rep, run, policy := s.buildFinalInsights(collector, appCfg, trendStore, newTrendRunID(now, runID), now)

	s.mu.Lock()
	record := !s.IsRunning && s.RunID == runID
	if record {
		s.InsightsCache = &rep
	}
	s.mu.Unlock()
	if record {
		recordTrendRun(trendStore, rep, run, policy)
		if raw, err := json.Marshal(rep); err == nil && s.runHistory != nil {
			s.mu.Lock()
			id := s.dashboardID
			s.mu.Unlock()
			if err := s.runHistory.AttachInsights(id, raw, rep.Metadata.Status); err != nil {
				logDashboard(err)
			}
		}
	}
	return &rep, "ready"
}

func recordTrendRun(trendStore *stats.TrendStore, rep stats.InsightsReport, run stats.TrendRunContext, policy stats.TrendPolicy) {
	if trendStore == nil || rep.Metadata.Status != "ready" {
		return
	}
	if err := trendStore.Record(run, stats.CurrentTrendBaselines(rep, run), policy); err != nil {
		log.Printf("Warning: trend baselines not saved: %v", err)
	}
}

// registerExplainCancel adds cancel to the explains a new run stops and
// returns the function that removes it again.
func (s *WebServer) registerExplainCancel(cancel context.CancelFunc) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.explainCancels == nil {
		s.explainCancels = map[uint64]context.CancelFunc{}
	}
	s.explainCancelSeq++
	key := s.explainCancelSeq
	s.explainCancels[key] = cancel
	return func() {
		s.mu.Lock()
		delete(s.explainCancels, key)
		s.mu.Unlock()
	}
}

// cancelExplainsLocked stops every in-flight post-run explain; s.mu is held.
func (s *WebServer) cancelExplainsLocked() {
	for _, cancel := range s.explainCancels {
		cancel()
	}
}

// buildFinalInsights computes the post-run insights of collector with
// explain enrichment against appCfg's server and the trend comparison.
func (s *WebServer) buildFinalInsights(collector *stats.Collector, appCfg *config.AppConfig, trendStore *stats.TrendStore, trendRunID string, now time.Time) (stats.InsightsReport, stats.TrendRunContext, stats.TrendPolicy) {
	rep := collector.GetFinalInsights()
	explainEnabled, explainTopN, explainMaxMs, explainVerbosity, explainSeverityMode, explainWorkers, explainRetries, explainBackoffMs := collector.GetExplainSettings()
	if explainEnabled {
		ctx, cancel := context.WithCancel(context.Background())
		unregister := s.registerExplainCancel(cancel)
		enrichInsightsWithExplain(ctx, rep.Metadata.Status, &rep, collector.SnapshotExplainInventory(), appCfg, explainParams{
			topN: explainTopN, maxTimeMs: explainMaxMs, verbosity: explainVerbosity, severityMode: explainSeverityMode,
			efficiencyMode: collector.GetExplainEfficiencyMode(), workers: explainWorkers, retries: explainRetries, backoffMs: explainBackoffMs,
		})
		unregister()
		cancel()
	}
	stats.ApplyEfficiencyView(&rep)
	if trendStore == nil {
		trendStore = stats.DisabledTrendStore()
	}
	policy := stats.TrendPolicyFromConfig(appCfg)
	label := ""
	if appCfg != nil {
		label = appCfg.TrendRunLabel
	}
	run := stats.NewTrendRunContext(rep, stats.TrendWorkloadFromConfig(appCfg), trendRunID, label, now)
	if appCfg != nil && appCfg.InsightsTrendStoreQueryValues && !trendStore.Disabled() && rep.Metadata.Status == "ready" {
		rep.TrendQueryValues = stats.BuildTrendQueryValues(rep, collector.SnapshotRepresentativeSamples())
		run.QueryValuesStored = true
	}
	stats.ApplyTrends(&rep, run, trendStore.Snapshot(), trendStore.Status(), now, stats.TrendOptions{Policy: policy})
	return rep, run, policy
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
		s.removeTrafficImports()
		os.Exit(0)
	}()
}
