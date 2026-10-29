package webui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/report"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
)

const (
	runDashboardSchema  = "rd-1"
	analysisUnavailable = "Query and index analysis was not stored for this run."
	durationUnavailable = "Execution duration was not recorded because the workload did not start."
)

var runIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,80}$`)

// RunSummary is the Completed Runs list row. Pointer fields are omitted when
// the measurement was not stored; a stored zero is a real zero.
type RunSummary struct {
	ID          string   `json:"id"`
	Schema      string   `json:"schema,omitempty"`
	Label       string   `json:"label,omitempty"`
	Status      string   `json:"status"`
	Mode        string   `json:"mode,omitempty"`
	StartedAt   string   `json:"started_at,omitempty"`
	CompletedAt string   `json:"completed_at,omitempty"`
	DurationSec *float64 `json:"duration_sec,omitempty"`
	// ActiveSec is the time workers ran, without the waits between
	// iterations; average throughput divides by it.
	ActiveSec    *float64 `json:"active_sec,omitempty"`
	TotalOps     *uint64  `json:"total_ops,omitempty"`
	AvgLatencyMs *float64 `json:"avg_latency_ms,omitempty"`
	P95LatencyMs *float64 `json:"p95_latency_ms,omitempty"`
	Error        string   `json:"error,omitempty"`
	Pinned       bool     `json:"pinned,omitempty"`
	Outdated     bool     `json:"outdated,omitempty"`
	// ChartTruncated is set by older versions, which kept only the newest
	// chart samples.
	ChartTruncated bool     `json:"chart_truncated,omitempty"`
	InsightsStatus string   `json:"insights_status,omitempty"`
	Unavailable    []string `json:"unavailable,omitempty"`
}

// RunDashboard is one completed run, enough to reopen its dashboard without
// a live database. Credentials are not stored.
type RunDashboard struct {
	RunSummary
	Config   map[string]any  `json:"config,omitempty"`
	Stats    map[string]any  `json:"stats,omitempty"`
	Insights json.RawMessage `json:"insights,omitempty"`
}

type pendingInsight struct {
	raw    json.RawMessage
	status string
}

// RunHistory stores completed-run dashboards as one JSON file per run.
type RunHistory struct {
	mu      sync.Mutex
	dir     string
	pending map[string]pendingInsight
}

// OpenRunHistory remembers dir. The directory is created on the first save.
func OpenRunHistory(dir string) *RunHistory {
	return &RunHistory{dir: dir, pending: map[string]pendingInsight{}}
}

// Dir is the dashboard directory.
func (h *RunHistory) Dir() string {
	if h == nil {
		return ""
	}
	return h.dir
}

func runDashboardDir(cfg *config.AppConfig) string {
	setting := ""
	if cfg != nil {
		setting = cfg.TrendBaselineStore
	}
	path, disabled := stats.ResolveTrendStorePath(setting)
	if disabled || strings.TrimSpace(path) == "" {
		path, _ = stats.ResolveTrendStorePath("")
	}
	return filepath.Join(filepath.Dir(path), "run_dashboards")
}

func (h *RunHistory) path(id string) string {
	return filepath.Join(h.dir, id+".json")
}

func checkRunID(id string) error {
	if !runIDPattern.MatchString(id) {
		return fmt.Errorf("invalid run id")
	}
	return nil
}

func (h *RunHistory) readLocked(id string) (*RunDashboard, error) {
	if err := checkRunID(id); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(h.path(id))
	if err != nil {
		return nil, err
	}
	var snap RunDashboard
	if err := json.Unmarshal(b, &snap); err != nil {
		return &RunDashboard{RunSummary: RunSummary{
			ID: id, Status: "damaged",
			Error: "This run file is damaged and could not be read. Other runs are unaffected.",
		}}, nil
	}
	if snap.ID == "" {
		snap.ID = id
	}
	if snap.Schema != runDashboardSchema {
		snap.Status = "unsupported"
		if snap.Error == "" {
			shown := snap.Schema
			if shown == "" {
				shown = "unknown"
			}
			snap.Error = fmt.Sprintf("This run uses storage format %q and cannot be opened as a dashboard.", shown)
		}
		snap.Stats = nil
		snap.Insights = nil
		snap.Config = nil
	}
	return &snap, nil
}

func (h *RunHistory) writeLocked(snap *RunDashboard) error {
	if err := os.MkdirAll(h.dir, 0o755); err != nil {
		return err
	}
	if err := checkRunID(snap.ID); err != nil {
		return err
	}
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	tmp := h.path(snap.ID) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, h.path(snap.ID))
}

// Save writes snap, keeps insights or a pin already stored for the same id,
// and prunes unpinned runs beyond policy.KeepRuns.
func (h *RunHistory) Save(snap *RunDashboard, policy stats.TrendPolicy) error {
	if h == nil || snap == nil || snap.ID == "" {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if existing, err := h.readLocked(snap.ID); err == nil && existing != nil && existing.Schema == runDashboardSchema && existing.Status != "damaged" {
		if len(snap.Insights) == 0 && len(existing.Insights) > 0 {
			snap.Insights = existing.Insights
			snap.InsightsStatus = existing.InsightsStatus
			snap.Unavailable = dropNotice(snap.Unavailable, analysisUnavailable)
		}
		if existing.Pinned {
			snap.Pinned = true
		}
	}
	if p, ok := h.pending[snap.ID]; ok && len(snap.Insights) == 0 {
		snap.Insights = p.raw
		snap.InsightsStatus = p.status
		snap.Unavailable = dropNotice(snap.Unavailable, analysisUnavailable)
		delete(h.pending, snap.ID)
	}
	if err := h.writeLocked(snap); err != nil {
		return err
	}
	return h.pruneLocked(policy)
}

// AttachInsights adds the post-run report to a dashboard. If the dashboard
// file is not written yet, the report is kept until Save.
func (h *RunHistory) AttachInsights(id string, raw []byte, status string) error {
	if h == nil || id == "" || len(raw) == 0 {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pending == nil {
		h.pending = map[string]pendingInsight{}
	}
	h.pending[id] = pendingInsight{raw: append(json.RawMessage(nil), raw...), status: status}
	snap, err := h.readLocked(id)
	if err != nil || snap == nil || snap.Schema != runDashboardSchema || snap.Status == "damaged" || snap.Status == "unsupported" {
		return nil
	}
	snap.Insights = h.pending[id].raw
	snap.InsightsStatus = status
	if snap.InsightsStatus == "" {
		snap.InsightsStatus = "ready"
	}
	snap.Unavailable = dropNotice(snap.Unavailable, analysisUnavailable)
	delete(h.pending, id)
	return h.writeLocked(snap)
}

// SetPinned pins or unpins a stored run. Pinned runs are not pruned.
func (h *RunHistory) SetPinned(id string, pinned bool, policy stats.TrendPolicy) error {
	if h == nil {
		return fmt.Errorf("run history is not available")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	snap, err := h.readLocked(id)
	if err != nil {
		return err
	}
	if snap.Status == "damaged" || snap.Status == "unsupported" || snap.Schema != runDashboardSchema {
		return fmt.Errorf("%s", snap.Error)
	}
	if pinned && !snap.Pinned {
		n := 0
		for _, other := range h.summariesLocked(time.Now(), policy) {
			if other.Pinned && other.ID != id && other.Schema == runDashboardSchema {
				n++
			}
		}
		if n >= stats.TrendMaxPinnedRuns {
			return stats.ErrTrendPinLimit
		}
	}
	snap.Pinned = pinned
	if err := h.writeLocked(snap); err != nil {
		return err
	}
	return h.pruneLocked(policy)
}

func (h *RunHistory) summariesLocked(now time.Time, policy stats.TrendPolicy) []RunSummary {
	entries, err := os.ReadDir(h.dir)
	if err != nil {
		return nil
	}
	_, maxAge := normalizeRetention(policy)
	var out []RunSummary
	for _, ent := range entries {
		name := ent.Name()
		if ent.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".tmp") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		snap, err := h.readLocked(id)
		if err != nil || snap == nil {
			continue
		}
		sum := snap.RunSummary
		sum.Outdated = false
		if sum.Schema == runDashboardSchema && !sum.Pinned && maxAge > 0 && sum.StartedAt != "" {
			if started, err := time.Parse(time.RFC3339, sum.StartedAt); err == nil && now.Sub(started) > maxAge {
				sum.Outdated = true
			}
		}
		out = append(out, sum)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartedAt > out[j].StartedAt
	})
	return out
}

// List returns stored runs, newest first, including damaged files.
func (h *RunHistory) List(now time.Time, policy stats.TrendPolicy) []RunSummary {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.summariesLocked(now, policy)
}

// Read returns the full dashboard. Damaged and unsupported files are returned
// with an error message and no measurements.
func (h *RunHistory) Read(id string) (*RunDashboard, error) {
	if h == nil {
		return nil, os.ErrNotExist
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.readLocked(id)
}

func (h *RunHistory) pruneLocked(policy stats.TrendPolicy) error {
	keep, _ := normalizeRetention(policy)
	entries, err := os.ReadDir(h.dir)
	if err != nil {
		return nil
	}
	type item struct {
		id      string
		started string
		pinned  bool
	}
	var runs []item
	for _, ent := range entries {
		name := ent.Name()
		if ent.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".tmp") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		snap, err := h.readLocked(id)
		if err != nil || snap == nil || snap.Schema != runDashboardSchema || snap.Status == "damaged" || snap.Status == "unsupported" {
			continue
		}
		runs = append(runs, item{id: id, started: snap.StartedAt, pinned: snap.Pinned})
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].started > runs[j].started })
	unpinned := 0
	for _, run := range runs {
		if run.pinned {
			continue
		}
		unpinned++
		if unpinned <= keep {
			continue
		}
		_ = os.Remove(h.path(run.id))
	}
	return nil
}

func normalizeRetention(policy stats.TrendPolicy) (keep int, maxAge time.Duration) {
	keep = policy.KeepRuns
	if keep < config.MinTrendHistoryRuns {
		keep = config.DefaultTrendHistoryRuns
	}
	if keep > config.MaxTrendHistoryRuns {
		keep = config.MaxTrendHistoryRuns
	}
	if policy.MaxAge < 0 {
		return keep, 0
	}
	return keep, policy.MaxAge
}

func dropNotice(items []string, notice string) []string {
	out := items[:0]
	for _, item := range items {
		if item != notice {
			out = append(out, item)
		}
	}
	return out
}

func runningUnderTest() bool {
	base := filepath.Base(os.Args[0])
	return strings.HasSuffix(base, ".test") || strings.HasSuffix(base, ".test.exe")
}

func newDashboardID(runID int64) string {
	return fmt.Sprintf("run-%s-%d", time.Now().UTC().Format("20060102T150405Z"), runID)
}

func redactMongoURI(uri string) string {
	u, err := url.Parse(strings.TrimSpace(uri))
	if err != nil || u == nil || u.User == nil {
		if i := strings.Index(uri, "://"); i >= 0 {
			rest := uri[i+3:]
			if at := strings.Index(rest, "@"); at >= 0 {
				return uri[:i+3] + rest[at+1:]
			}
		}
		return uri
	}
	u.User = nil
	return u.String()
}

func (s *WebServer) persistDashboard() {
	if s == nil || s.runHistory == nil {
		return
	}
	s.mu.Lock()
	snap := s.captureDashboardLocked()
	cfg := s.AppConfig
	s.mu.Unlock()
	if snap == nil {
		return
	}
	if err := s.runHistory.Save(snap, stats.TrendPolicyFromConfig(cfg)); err != nil {
		logDashboard(err)
	}
}

func logDashboard(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "plgm: run dashboard not saved: %v\n", err)
	}
}

func (s *WebServer) captureDashboardLocked() *RunDashboard {
	if s.runHistory == nil || s.dashboardID == "" || s.CurrentStats == nil {
		return nil
	}
	if s.LifecyclePhase != "completed" && s.LifecyclePhase != "failed" {
		return nil
	}
	cfg := s.AppConfig
	mode := s.dashboardMode
	if mode == "" {
		mode = inferDashboardMode(cfg)
	}
	started := s.InitStartedAt
	if started.IsZero() {
		started = s.ExecStartedAt
	}
	completed := s.CompletedAt
	if completed.IsZero() {
		completed = time.Now()
	}
	var duration *float64
	unavailable := []string{}
	if !s.ExecStartedAt.IsZero() {
		sec := completed.Sub(s.ExecStartedAt).Seconds()
		if sec < 0 {
			sec = 0
		}
		duration = &sec
	} else {
		unavailable = append(unavailable, durationUnavailable)
	}
	collector := s.CurrentStats
	findOps := atomic.LoadUint64(&collector.FindOps)
	insertOps := atomic.LoadUint64(&collector.InsertOps)
	upsertOps := atomic.LoadUint64(&collector.UpsertOps)
	updateOps := atomic.LoadUint64(&collector.UpdateOps)
	deleteOps := atomic.LoadUint64(&collector.DeleteOps)
	aggOps := atomic.LoadUint64(&collector.AggOps)
	total := findOps + insertOps + upsertOps + updateOps + deleteOps + aggOps
	distTotal := collector.TotalHist.GetStats()
	var avg, p95 *float64
	if distTotal["count"] > 0 {
		a, p := distTotal["avg"], distTotal["p95"]
		avg, p95 = &a, &p
	}
	charts := collector.UIChartHistory(0)
	var activeSec *float64
	if d := collector.ActiveDuration(); d > 0 {
		sec := d.Seconds()
		activeSec = &sec
	}
	insightsStatus := "unavailable"
	if s.InsightsCache == nil {
		unavailable = append(unavailable, analysisUnavailable)
	} else {
		insightsStatus = s.InsightsCache.Metadata.Status
		if insightsStatus == "" {
			insightsStatus = "ready"
		}
	}
	status := s.LifecyclePhase
	label := dashboardLabel(cfg, mode, started)
	snap := &RunDashboard{
		RunSummary: RunSummary{
			ID: s.dashboardID, Schema: runDashboardSchema, Label: label, Status: status, Mode: mode,
			StartedAt: started.UTC().Format(time.RFC3339), CompletedAt: completed.UTC().Format(time.RFC3339),
			DurationSec: duration, ActiveSec: activeSec, TotalOps: &total, AvgLatencyMs: avg, P95LatencyMs: p95,
			Error: s.LastError, InsightsStatus: insightsStatus, Unavailable: unavailable,
		},
		Config: redactRunConfig(cfg, mode),
		Stats:  dashboardStats(s, collector, charts, duration, total),
	}
	if s.InsightsCache != nil {
		if raw, err := json.Marshal(s.InsightsCache); err == nil {
			snap.Insights = raw
		}
	}
	return snap
}

func dashboardLabel(cfg *config.AppConfig, mode string, started time.Time) string {
	if cfg != nil && strings.TrimSpace(cfg.TrendRunLabel) != "" {
		return strings.TrimSpace(cfg.TrendRunLabel)
	}
	name := map[string]string{
		opModeGeneric: "Generic workload",
		opModeCustom:  "Custom workload",
		opModeRecord:  "Record",
		opModeReplay:  "Replay",
		opModeRaw:     "Raw injector",
	}[mode]
	if name == "" {
		name = "Run"
	}
	if started.IsZero() {
		return name
	}
	return name + " " + started.UTC().Format("2006-01-02 15:04")
}

func redactRunConfig(cfg *config.AppConfig, mode string) map[string]any {
	out := map[string]any{"mode": mode}
	if cfg == nil {
		return out
	}
	out["uri"] = redactMongoURI(cfg.URI)
	out["duration"] = cfg.Duration
	out["concurrency"] = cfg.Concurrency
	out["documents_count"] = cfg.DocumentsCount
	out["iterations"] = cfg.Iterations
	out["default_workload"] = cfg.DefaultWorkload
	out["load_profile"] = cfg.LoadProfile.Kind
	out["find_percent"] = cfg.FindPercent
	out["update_percent"] = cfg.UpdatePercent
	out["delete_percent"] = cfg.DeletePercent
	out["insert_percent"] = cfg.InsertPercent
	out["bulk_insert_percent"] = cfg.BulkInsertPercent
	out["aggregate_percent"] = cfg.AggregatePercent
	out["transaction_percent"] = cfg.TransactionPercent
	out["record_enabled"] = cfg.WorkloadRecordEnabled
	out["replay_enabled"] = cfg.WorkloadReplayEnabled
	out["replay_mode"] = cfg.WorkloadReplayMode
	out["deterministic"] = cfg.WorkloadDeterministic
	out["raw_injector"] = cfg.RawInjector.Enabled
	out["explain_enabled"] = cfg.InsightsExplainEnabled
	out["run_label"] = cfg.TrendRunLabel
	out["sharding_mode"] = cfg.ShardingMode
	if n := len(cfg.WorkloadReplayTargets); n > 0 {
		labels := make([]string, 0, n)
		for _, t := range cfg.WorkloadReplayTargets {
			if t.Label != "" {
				labels = append(labels, t.Label)
			}
		}
		out["target_labels"] = labels
		out["target_count"] = n
	}
	return out
}

func dashboardStats(s *WebServer, collector *stats.Collector, charts []stats.UIChartPoint, duration *float64, total uint64) map[string]any {
	statsOut := map[string]any{
		"historical":       true,
		"opsPresent":       true,
		"isRunning":        false,
		"metricsCaptured":  true,
		"lifecyclePhase":   s.LifecyclePhase,
		"lifecycleMessage": s.LifecycleMessage,
		"lastError":        s.LastError,
		"findOps":          atomic.LoadUint64(&collector.FindOps),
		"insertOps":        atomic.LoadUint64(&collector.InsertOps),
		"upsertOps":        atomic.LoadUint64(&collector.UpsertOps),
		"updateOps":        atomic.LoadUint64(&collector.UpdateOps),
		"deleteOps":        atomic.LoadUint64(&collector.DeleteOps),
		"aggOps":           atomic.LoadUint64(&collector.AggOps),
		"totalOps":         total,
		"distFind":         collector.FindHist.GetStats(),
		"distInsert":       collector.InsertHist.GetStats(),
		"distUpsert":       collector.UpsertHist.GetStats(),
		"distUpdate":       collector.UpdateHist.GetStats(),
		"distDelete":       collector.DeleteHist.GetStats(),
		"distAgg":          collector.AggHist.GetStats(),
		"distTrans":        collector.TransHist.GetStats(),
		"distTotal":        collector.TotalHist.GetStats(),
		"chartHistory":     charts,
		"activeSec":        collector.ActiveDuration().Seconds(),
		"latencyHeatmap":   collector.LatencyHeatmap(),
		"currentIteration": s.CurrentIteration,
		"totalIterations":  s.TotalIterations,
	}
	if duration != nil {
		statsOut["executionElapsedSec"] = *duration
	} else {
		statsOut["executionElapsedSec"] = nil
	}
	if s.AppConfig != nil {
		statsOut["duration"] = s.AppConfig.Duration
		statsOut["configuredConcurrency"] = s.AppConfig.Concurrency
		kind := strings.TrimSpace(s.AppConfig.LoadProfile.Kind)
		if kind == "" {
			kind = "fixed"
		}
		statsOut["loadProfileKind"] = kind
		statsOut["workloadDeterministic"] = s.AppConfig.WorkloadDeterministic
		statsOut["operationsPerWorker"] = s.AppConfig.OperationsPerWorker
	}
	if g := collector.GeneratedTraffic(); g != nil {
		statsOut["generatedTraffic"] = g
	}
	if mt := multiTargetStatus(s.multiTarget); mt != nil {
		statsOut["multi_target"] = mt
		statsOut["targets"] = multiTargetSeries(s.multiTarget, nil)
	}
	return statsOut
}

func (s *WebServer) handleRunHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.runHistory == nil {
		writeJSON(w, http.StatusOK, map[string]any{"runs": []RunSummary{}, "dir": "", "disabled": true})
		return
	}
	s.mu.Lock()
	cfg := s.AppConfig
	s.mu.Unlock()
	policy := stats.TrendPolicyFromConfig(cfg)
	keep, maxAge := normalizeRetention(policy)
	days := 0
	if maxAge > 0 {
		days = int(maxAge / (24 * time.Hour))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"dir":          s.runHistory.Dir(),
		"keep_runs":    keep,
		"max_age_days": days,
		"max_pinned":   stats.TrendMaxPinnedRuns,
		"runs":         s.runHistory.List(time.Now(), policy),
	})
}

func (s *WebServer) handleRunHistoryItem(w http.ResponseWriter, r *http.Request) {
	if s.runHistory == nil {
		writeAPIError(w, http.StatusNotFound, "run history is not available")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/run-history/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeAPIError(w, http.StatusNotFound, "run not found")
		return
	}
	id := parts[0]
	if len(parts) == 2 && parts[1] == "report" {
		if r.Method != http.MethodGet {
			writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.handleRunHistoryReport(w, id, r.URL.Query().Get("download") == "1")
		return
	}
	if len(parts) != 1 {
		writeAPIError(w, http.StatusNotFound, "run not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		snap, err := s.runHistory.Read(id)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				writeAPIError(w, http.StatusNotFound, "run not found")
				return
			}
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		if snap.Status == "damaged" || snap.Status == "unsupported" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"id": snap.ID, "status": snap.Status, "error": snap.Error,
			})
			return
		}
		writeJSON(w, http.StatusOK, snap)
	case http.MethodPatch:
		var body struct {
			Pinned *bool `json:"pinned"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Pinned == nil {
			writeAPIError(w, http.StatusBadRequest, "send {\"pinned\": true} or {\"pinned\": false}")
			return
		}
		s.mu.Lock()
		cfg := s.AppConfig
		s.mu.Unlock()
		if err := s.runHistory.SetPinned(id, *body.Pinned, stats.TrendPolicyFromConfig(cfg)); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				writeAPIError(w, http.StatusNotFound, "run not found")
				return
			}
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "pinned": *body.Pinned})
	default:
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleRunHistoryReport serves a stored run's HTML report inline, so it
// opens in a browser tab; download sends it as an attachment.
func (s *WebServer) handleRunHistoryReport(w http.ResponseWriter, id string, download bool) {
	snap, err := s.runHistory.Read(id)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeAPIError(w, http.StatusNotFound, "run not found")
			return
		}
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if snap.Status == "damaged" || snap.Status == "unsupported" {
		writeAPIError(w, http.StatusUnprocessableEntity, snap.Error)
		return
	}
	html, err := report.RenderBytes(reportFromDashboard(snap))
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "failed to render report")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	disposition := "inline"
	if download {
		disposition = "attachment"
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`%s; filename="plgm-report-%s.html"`, disposition, id))
	_, _ = w.Write(html)
}

func reportFromDashboard(snap *RunDashboard) report.ReportData {
	data := report.ReportData{
		Title:       "MongoDB Benchmark Report",
		GeneratedAt: time.Now(),
		Warnings:    append([]string{}, snap.Unavailable...),
	}
	if snap.Error != "" {
		data.Warnings = append([]string{snap.Error}, data.Warnings...)
	}
	if snap.DurationSec != nil {
		data.DurationSeconds = *snap.DurationSec
	} else {
		data.DurationLabel = "not recorded"
	}
	if snap.TotalOps != nil {
		data.TotalOps = int64(*snap.TotalOps)
		if snap.ActiveSec != nil && *snap.ActiveSec > 0 {
			data.AvgOpsPerSec = float64(*snap.TotalOps) / *snap.ActiveSec
		} else if snap.DurationSec != nil && *snap.DurationSec > 0 {
			data.AvgOpsPerSec = float64(*snap.TotalOps) / *snap.DurationSec
		}
	} else {
		data.OpsLabel = "not recorded"
		data.ThroughputLabel = "not recorded"
	}
	if snap.Config != nil {
		for _, kv := range []struct{ key, label string }{
			{"mode", "Mode"}, {"uri", "Target"}, {"duration", "Configured duration"},
			{"concurrency", "Concurrency"}, {"documents_count", "Seed documents"},
			{"load_profile", "Load profile"},
		} {
			if v, ok := snap.Config[kv.key]; ok && fmt.Sprint(v) != "" {
				data.ConfigItems = append(data.ConfigItems, report.KV{Key: kv.label, Value: fmt.Sprint(v)})
			}
		}
	}
	if len(snap.Insights) == 0 {
		if !containsNotice(data.Warnings, analysisUnavailable) {
			data.Warnings = append(data.Warnings, analysisUnavailable)
		}
		return data
	}
	var rep stats.InsightsReport
	if err := json.Unmarshal(snap.Insights, &rep); err != nil {
		data.Warnings = append(data.Warnings, "Stored query analysis could not be read.")
		return data
	}
	data.Impact = reportImpactSection(rep.Impact)
	data.Efficiency = reportEfficiencySection(rep.EfficiencyView, rep.Recommendations)
	data.Trends = reportTrendSection(rep.Trends)
	data.Recommendations = reportRecommendations(rep.Recommendations)
	return data
}

func containsNotice(items []string, notice string) bool {
	for _, item := range items {
		if item == notice {
			return true
		}
	}
	return false
}
