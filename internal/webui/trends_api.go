package webui

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
)

var trendClock = time.Now

// newTrendRunID is unique across machines: a UTC timestamp, this server's run
// counter, and 64 random bits.
func newTrendRunID(now time.Time, runID int64) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s-%d-%s", now.UTC().Format("20060102T150405Z"), runID, hex.EncodeToString(b[:]))
}

// openTrendStoreFor opens the store named by cfg.TrendBaselineStore, or the
// PLGM_TREND_BASELINE_STORE / default location when it is empty.
func openTrendStoreFor(cfg *config.AppConfig) *stats.TrendStore {
	if cfg != nil && strings.TrimSpace(cfg.TrendBaselineStore) != "" {
		return stats.OpenTrendStoreSetting(cfg.TrendBaselineStore)
	}
	return stats.OpenDefaultTrendStore()
}

// useTrendStoreLocked reopens the store when a run changes trend_baseline_store.
func (s *WebServer) useTrendStoreLocked(cfg *config.AppConfig) {
	if cfg == nil || cfg.TrendBaselineStore == s.trendStoreSetting {
		return
	}
	s.TrendStore = openTrendStoreFor(cfg)
	s.trendStoreSetting = cfg.TrendBaselineStore
}

// applyTrendForm validates and applies the trend history fields of a start
// request. A submitted empty label or store path selects the default.
func applyTrendForm(cfg *config.AppConfig, form url.Values) error {
	intField := func(name string, min, max int) (int, bool, error) {
		if _, ok := form[name]; !ok {
			return 0, false, nil
		}
		v := strings.TrimSpace(form.Get(name))
		if v == "" {
			return 0, false, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < min || n > max {
			return 0, false, fmt.Errorf("%s must be a whole number from %d to %d", name, min, max)
		}
		return n, true, nil
	}
	if n, ok, err := intField("trend_history_runs", config.MinTrendHistoryRuns, config.MaxTrendHistoryRuns); err != nil {
		return err
	} else if ok {
		cfg.TrendHistoryRuns = n
	}
	if n, ok, err := intField("trend_max_age_days", 0, config.MaxTrendMaxAgeDays); err != nil {
		return err
	} else if ok {
		cfg.TrendMaxAgeDays = &n
	}
	if _, ok := form["trend_default_comparison"]; ok {
		switch v := strings.ToLower(strings.TrimSpace(form.Get("trend_default_comparison"))); v {
		case "":
		case config.TrendComparisonPrevious, config.TrendComparisonPinned:
			cfg.TrendDefaultComparison = v
		default:
			return fmt.Errorf("trend_default_comparison must be %q or %q", config.TrendComparisonPrevious, config.TrendComparisonPinned)
		}
	}
	if _, ok := form["trend_run_label"]; ok {
		v := strings.TrimSpace(form.Get("trend_run_label"))
		if len(v) > config.MaxTrendRunLabelBytes {
			return fmt.Errorf("trend_run_label must be at most %d bytes", config.MaxTrendRunLabelBytes)
		}
		cfg.TrendRunLabel = v
	}
	if _, ok := form["trend_baseline_store"]; ok {
		v := strings.TrimSpace(form.Get("trend_baseline_store"))
		if len(v) > config.MaxTrendBaselineStorePath || strings.ContainsAny(v, "\x00\r\n") {
			return fmt.Errorf("trend_baseline_store must be a file path of at most %d bytes, empty for the default location, or %q", config.MaxTrendBaselineStorePath, "off")
		}
		cfg.TrendBaselineStore = v
	}
	config.NormalizeTrendSettings(cfg)
	return nil
}

func (s *WebServer) trendContext() (*stats.TrendStore, *config.AppConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	store := s.TrendStore
	if store == nil {
		store = stats.DisabledTrendStore()
	}
	return store, s.AppConfig
}

func writeTrendError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	var importErr *stats.TrendImportError
	switch {
	case errors.Is(err, stats.ErrTrendStoreDisabled), errors.Is(err, stats.ErrTrendPinLimit):
		code = http.StatusConflict
	case errors.Is(err, stats.ErrTrendRunNotFound):
		code = http.StatusNotFound
	case errors.As(err, &importErr):
		code = http.StatusBadRequest
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func trendSettingsPayload(cfg *config.AppConfig, store *stats.TrendStore) map[string]interface{} {
	c := config.AppConfig{}
	if cfg != nil {
		c = *cfg
	}
	config.NormalizeTrendSettings(&c)
	return map[string]interface{}{
		"history_runs":       c.TrendHistoryRuns,
		"max_age_days":       *c.TrendMaxAgeDays,
		"default_comparison": c.TrendDefaultComparison,
		"run_label":          c.TrendRunLabel,
		"baseline_store":     c.TrendBaselineStore,
		"store_path":         store.Path(),
		"disabled":           store.Disabled(),
		"defaults": map[string]interface{}{
			"history_runs": config.DefaultTrendHistoryRuns, "max_age_days": config.DefaultTrendMaxAgeDays,
			"default_comparison": config.TrendComparisonPrevious, "baseline_store": "", "run_label": "",
			"run_label_auto": "mongo-<server version>, or run-<UTC time> when the version is unknown",
		},
		"limits": map[string]interface{}{
			"min_history_runs": config.MinTrendHistoryRuns, "max_history_runs": config.MaxTrendHistoryRuns,
			"max_age_days": config.MaxTrendMaxAgeDays, "max_pinned_runs": stats.TrendMaxPinnedRuns,
			"max_matrix_runs": stats.TrendMaxMatrixRuns, "max_label_bytes": config.MaxTrendRunLabelBytes,
			"max_shapes": stats.TrendMaxShapeBaselines, "max_file_bytes": stats.TrendFileMaxBytes,
		},
	}
}

func (s *WebServer) trendBaselinesPayload(store *stats.TrendStore, cfg *config.AppConfig) map[string]interface{} {
	policy := stats.TrendPolicyFromConfig(cfg)
	return map[string]interface{}{
		"store":    store.Status(),
		"criteria": stats.TrendCriteriaFor(policy),
		"runs":     store.Runs(policy, time.Now()),
		"settings": trendSettingsPayload(cfg, store),
	}
}

// handleTrendBaselines: GET lists the store, runs, and settings; DELETE resets.
func (s *WebServer) handleTrendBaselines(w http.ResponseWriter, r *http.Request) {
	store, cfg := s.trendContext()
	switch r.Method {
	case http.MethodGet:
	case http.MethodDelete:
		if err := store.Reset(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	default:
		w.Header().Set("Allow", "GET, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, s.trendBaselinesPayload(store, cfg))
}

// handleTrendBaselineItem serves export, import, and runs/{id} (PATCH, DELETE).
func (s *WebServer) handleTrendBaselineItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/trend-baselines/"), "/")
	switch {
	case rest == "export":
		s.handleTrendExport(w, r)
	case rest == "import":
		s.handleTrendImport(w, r)
	case strings.HasPrefix(rest, "runs/"):
		s.handleTrendRun(w, r, strings.TrimPrefix(rest, "runs/"))
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown trend baseline endpoint"})
	}
}

func splitTrendIDs(v string) []string {
	var out []string
	for _, id := range strings.Split(v, ",") {
		if id = strings.TrimSpace(id); id != "" {
			out = append(out, id)
		}
	}
	return out
}

func (s *WebServer) handleTrendExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	store, _ := s.trendContext()
	ids := splitTrendIDs(r.URL.Query().Get("runs"))
	for _, id := range ids {
		if !stats.ValidTrendRunID(id) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid run id: " + clipID(id, 80)})
			return
		}
	}
	now := time.Now()
	x, err := store.Export(ids, r.URL.Query().Get("source"), now)
	if err != nil {
		writeTrendError(w, err)
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="plgm-trend-runs-%s.json"`, now.UTC().Format("20060102T150405Z")))
	writeJSON(w, http.StatusOK, x)
}

func (s *WebServer) handleTrendImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	store, cfg := s.trendContext()
	limit := int64(stats.TrendFileMaxBytes)
	r.Body = http.MaxBytesReader(w, r.Body, limit+1<<20)
	tooLarge := fmt.Sprintf("trend import rejected: file exceeds the limit of %d bytes", limit)
	var b []byte
	name := ""
	var err error
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		if err = r.ParseMultipartForm(8 << 20); err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": tooLarge})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to parse multipart upload: " + err.Error()})
			return
		}
		file, header, ferr := r.FormFile("file")
		if ferr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "upload field 'file' is required"})
			return
		}
		defer file.Close()
		b, err = io.ReadAll(io.LimitReader(file, limit+1))
		name = header.Filename
	} else {
		b, err = io.ReadAll(io.LimitReader(r.Body, limit+1))
	}
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": tooLarge})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read import: " + err.Error()})
		return
	}
	res, err := store.Import(b, name, stats.TrendPolicyFromConfig(cfg), time.Now())
	if err != nil {
		writeTrendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *WebServer) handleTrendRun(w http.ResponseWriter, r *http.Request, id string) {
	if !stats.ValidTrendRunID(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid run id"})
		return
	}
	store, cfg := s.trendContext()
	policy := stats.TrendPolicyFromConfig(cfg)
	switch r.Method {
	case http.MethodPatch:
		var body struct {
			Label  *string `json:"label"`
			Pinned *bool   `json:"pinned"`
		}
		dec := json.NewDecoder(io.LimitReader(r.Body, 4<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: expected {\"label\": string, \"pinned\": bool}"})
			return
		}
		if body.Label == nil && body.Pinned == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "nothing to change: send label and/or pinned"})
			return
		}
		if body.Label != nil && len(strings.TrimSpace(*body.Label)) > config.MaxTrendRunLabelBytes {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("label must be at most %d bytes", config.MaxTrendRunLabelBytes)})
			return
		}
		run, err := store.UpdateRun(id, body.Label, body.Pinned, policy, time.Now())
		if err != nil {
			writeTrendError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"run": run, "store": store.Status()})
	case http.MethodDelete:
		if err := store.DeleteRun(id); err != nil {
			writeTrendError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, s.trendBaselinesPayload(store, cfg))
	default:
		w.Header().Set("Allow", "PATCH, DELETE")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// handleTrendCompare recomputes the last run's trends against another
// baseline: ?baseline=previous|pinned|<run id>, or a matrix with ?runs=a,b.
func (s *WebServer) handleTrendCompare(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	s.mu.Lock()
	rep := s.InsightsCache
	s.mu.Unlock()
	store, cfg := s.trendContext()
	if rep == nil || rep.Trends == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "no finished run with trends; complete a run first"})
		return
	}
	if store.Disabled() {
		writeTrendError(w, stats.ErrTrendStoreDisabled)
		return
	}
	policy := stats.TrendPolicyFromConfig(cfg)
	run := rep.Trends.CurrentRun
	base := store.Snapshot()
	for _, sr := range base.Runs {
		if sr.RunID == run.RunID {
			run.Label, run.Pinned = sr.Label, sr.Pinned
		}
	}
	now, err := time.Parse(time.RFC3339, run.RecordedAt)
	if err != nil {
		now = time.Now()
	}
	q := r.URL.Query()
	if v := q.Get("runs"); v != "" {
		m, err := stats.BuildTrendMatrix(*rep, run, base, splitTrendIDs(v), now, policy)
		if err != nil {
			if errors.Is(err, stats.ErrTrendRunNotFound) {
				writeTrendError(w, err)
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"matrix": m})
		return
	}
	opts := stats.TrendOptions{Policy: policy}
	switch b := strings.TrimSpace(q.Get("baseline")); b {
	case "", "default":
	case config.TrendComparisonPrevious, config.TrendComparisonPinned:
		opts.Mode = b
	default:
		found := false
		for _, sr := range base.Runs {
			found = found || (sr.RunID == b && b != run.RunID)
		}
		if !stats.ValidTrendRunID(b) || !found {
			writeTrendError(w, fmt.Errorf("%w: %s", stats.ErrTrendRunNotFound, clipID(b, 80)))
			return
		}
		opts.Mode, opts.BaselineRunID = stats.TrendCompareRun, b
	}
	v := stats.BuildTrendView(*rep, run, base, store.Status(), now, opts)
	writeJSON(w, http.StatusOK, map[string]interface{}{"view": v})
}

func clipID(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
