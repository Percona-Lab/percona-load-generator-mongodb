package webui

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
)

// A concurrency sweep runs the same workload once for each worker count in a
// list (for example 4, 8, 16, 32). Every level is an ordinary run: it
// prepares its data like any run, is stored in Completed Runs, and gets a
// label that names its worker count. The runs carry a shared sweep tag so
// Compare Runs can select them together and show one results table.
const (
	loadProfileSweep = "sweep"
	sweepMinLevels   = 2
	// sweepMaxLevels matches the 8 lines Compare Runs draws at once.
	sweepMaxLevels  = 8
	sweepMaxWorkers = 10000

	sweepRunning = "running"
	sweepWaiting = "waiting"
	sweepDone    = "done"
	sweepStopped = "stopped"
	sweepFailed  = "failed"
)

// SweepTag is stored with every run of a sweep.
type SweepTag struct {
	ID      string `json:"id"`
	Label   string `json:"label,omitempty"`
	Level   int    `json:"level"`
	Levels  int    `json:"levels"`
	Workers int    `json:"workers"`
}

// sweepRun is the state of the current or last sweep. All fields are guarded
// by WebServer.mu.
type sweepRun struct {
	ID        string
	Label     string
	Workers   []int
	Level     int // 1-based level that is running or ran last
	State     string
	Message   string
	Cancelled bool
	Pause     time.Duration
	// LastDash is the stored run of the last level that ran; the dashboard
	// shows the sweep while that run is the one on screen.
	LastDash string
}

func (sw *sweepRun) active() bool {
	return sw != nil && (sw.State == sweepRunning || sw.State == sweepWaiting)
}

func (sw *sweepRun) tag() *SweepTag {
	if !sw.active() || sw.Level < 1 || sw.Level > len(sw.Workers) {
		return nil
	}
	return &SweepTag{ID: sw.ID, Label: sw.Label, Level: sw.Level, Levels: len(sw.Workers), Workers: sw.Workers[sw.Level-1]}
}

// view is the sweep section of /api/stats.
func (sw *sweepRun) view() map[string]any {
	if sw == nil {
		return nil
	}
	cur := 0
	if sw.Level >= 1 && sw.Level <= len(sw.Workers) {
		cur = sw.Workers[sw.Level-1]
	}
	return map[string]any{
		"id": sw.ID, "label": sw.Label, "level": sw.Level, "levels": len(sw.Workers),
		"workers": sw.Workers, "current_workers": cur, "state": sw.State, "active": sw.active(),
		"message": sw.Message, "pause_sec": sw.Pause.Seconds(),
	}
}

// sweepViewLocked is the sweep section of /api/stats: the sweep while it
// runs, and a finished one while its last run is still the current one.
func (s *WebServer) sweepViewLocked() map[string]any {
	sw := s.sweep
	if sw == nil || (!sw.active() && sw.LastDash != s.dashboardID) {
		return nil
	}
	return sw.view()
}

// parseSweepWorkers reads a list such as "4, 8, 16 32" into worker counts.
func parseSweepWorkers(raw string) ([]int, error) {
	fields := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' })
	if len(fields) < sweepMinLevels {
		return nil, fmt.Errorf("a sweep needs at least %d worker counts, for example 4, 8, 16, 32", sweepMinLevels)
	}
	if len(fields) > sweepMaxLevels {
		return nil, fmt.Errorf("a sweep has at most %d worker counts; Compare Runs draws up to %d lines", sweepMaxLevels, sweepMaxLevels)
	}
	seen := map[int]bool{}
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil || n < 1 || n > sweepMaxWorkers {
			return nil, fmt.Errorf("%q is not a worker count; use whole numbers from 1 to %d", f, sweepMaxWorkers)
		}
		if seen[n] {
			return nil, fmt.Errorf("worker count %d is listed twice", n)
		}
		seen[n] = true
		out = append(out, n)
	}
	return out, nil
}

// sweepLevelLabel names one level. The worker count is at the end, so the
// 64-byte limit shortens the base label, never the count.
func sweepLevelLabel(base string, workers int) string {
	suffix := fmt.Sprintf(" - %d workers", workers)
	base = strings.TrimSpace(base)
	room := config.MaxTrendRunLabelBytes - len(suffix)
	if room < 0 {
		room = 0
	}
	if len(base) > room {
		cut := room
		for cut > 0 && base[cut]&0xC0 == 0x80 {
			cut--
		}
		base = strings.TrimSpace(base[:cut])
	}
	return config.SanitizeTrendRunLabel(base + suffix)
}

// sweepFile is an uploaded file of the start form, kept in memory because
// the request that carried it ends before the later levels start.
type sweepFile struct {
	field, name string
	header      textproto.MIMEHeader
	data        []byte
}

// sweepForm is the start form of a sweep, replayed for every level.
type sweepForm struct {
	values map[string][]string
	files  []sweepFile
}

func captureSweepForm(r *http.Request) (*sweepForm, error) {
	f := &sweepForm{values: map[string][]string{}}
	if r.MultipartForm == nil {
		return nil, fmt.Errorf("the start form was not received")
	}
	for k, vs := range r.MultipartForm.Value {
		f.values[k] = append([]string(nil), vs...)
	}
	for field, headers := range r.MultipartForm.File {
		for _, h := range headers {
			file, err := h.Open()
			if err != nil {
				return nil, fmt.Errorf("reading the uploaded file %s: %w", h.Filename, err)
			}
			data, err := io.ReadAll(file)
			file.Close()
			if err != nil {
				return nil, fmt.Errorf("reading the uploaded file %s: %w", h.Filename, err)
			}
			f.files = append(f.files, sweepFile{field: field, name: h.Filename, header: h.Header, data: data})
		}
	}
	return f, nil
}

// request builds the start request of one level: the same form with a fixed
// worker count and the level's label.
func (f *sweepForm) request(workers int, label string) (*http.Request, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	overridden := map[string]bool{"load_profile_kind": true, "concurrency": true, "trend_run_label": true, "load_profile_sweep_workers": true}
	for k, vs := range f.values {
		if overridden[k] {
			continue
		}
		for _, v := range vs {
			if err := mw.WriteField(k, v); err != nil {
				return nil, err
			}
		}
	}
	for k, v := range map[string]string{"load_profile_kind": "fixed", "concurrency": strconv.Itoa(workers), "trend_run_label": label} {
		if err := mw.WriteField(k, v); err != nil {
			return nil, err
		}
	}
	for _, file := range f.files {
		part, err := mw.CreatePart(file.header)
		if err != nil {
			return nil, err
		}
		if _, err := part.Write(file.data); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, "/api/start", &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req, nil
}

// statusWriter records the status of a response it passes through.
type statusWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.body.Len() < 2048 {
		w.body.Write(b)
	}
	return w.ResponseWriter.Write(b)
}

// discardWriter swallows the response of a level the browser did not start.
type discardWriter struct{ h http.Header }

func (d *discardWriter) Header() http.Header {
	if d.h == nil {
		d.h = http.Header{}
	}
	return d.h
}
func (d *discardWriter) Write(b []byte) (int, error) { return len(b), nil }
func (d *discardWriter) WriteHeader(int)             {}

// handleStart starts a run, or a sweep when the load profile is Sweep.
func (s *WebServer) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		if s.startSweep(w, r) {
			return
		}
	}
	s.handleStartRun(w, r)
}

// startSweep handles a start request that belongs to a sweep and reports
// whether it answered it. A request that is not a sweep, and not blocked by
// a running sweep, is left to handleStartRun.
func (s *WebServer) startSweep(w http.ResponseWriter, r *http.Request) bool {
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		return false
	}
	isSweep := strings.EqualFold(strings.TrimSpace(r.FormValue("load_profile_kind")), loadProfileSweep)

	s.mu.Lock()
	busy := s.sweep.active()
	s.mu.Unlock()
	if busy {
		writeStartError(w, http.StatusConflict, "a concurrency sweep is in progress; stop it before starting another run")
		return true
	}
	if !isSweep {
		return false
	}

	switch mode := strings.ToLower(strings.TrimSpace(r.FormValue("operation_mode"))); mode {
	case opModeRecord, opModeReplay, opModeRaw:
		writeStartError(w, http.StatusBadRequest, "a concurrency sweep runs a generic or custom workload; it cannot be combined with Record, Replay, or Raw Injector")
		return true
	}
	// Deterministic traffic is allowed: every level is a fixed-count run with
	// its own fingerprint. One outcome file path would be reused by every level.
	if strings.EqualFold(strings.TrimSpace(r.FormValue("workload_deterministic")), "true") &&
		strings.TrimSpace(r.FormValue("workload_deterministic_outcomes_path")) != "" {
		writeStartError(w, http.StatusBadRequest, "a sweep with Deterministic traffic needs a blank Deterministic Outcome File, so every worker count writes its own file")
		return true
	}
	workers, err := parseSweepWorkers(r.FormValue("load_profile_sweep_workers"))
	if err != nil {
		writeStartError(w, http.StatusBadRequest, err.Error())
		return true
	}
	form, err := captureSweepForm(r)
	if err != nil {
		writeStartError(w, http.StatusBadRequest, err.Error())
		return true
	}
	base := config.SanitizeTrendRunLabel(r.FormValue("trend_run_label"))
	if base == "" {
		base = "Sweep " + time.Now().UTC().Format("2006-01-02 15:04")
	}
	pause, _ := time.ParseDuration(strings.TrimSpace(r.FormValue("interval_delay")))
	if pause < 0 {
		pause = 0
	}

	s.mu.Lock()
	if s.IsRunning {
		s.mu.Unlock()
		writeStartError(w, http.StatusConflict, "workload is already running")
		return true
	}
	sw := &sweepRun{ID: newSweepID(), Label: base, Workers: workers, Level: 1, State: sweepRunning, Pause: pause,
		Message: fmt.Sprintf("Level 1 of %d: %d workers", len(workers), workers[0])}
	s.sweep = sw
	s.mu.Unlock()

	req, err := form.request(workers[0], sweepLevelLabel(base, workers[0]))
	if err != nil {
		s.endSweep(sw, sweepFailed, err.Error())
		writeStartError(w, http.StatusInternalServerError, err.Error())
		return true
	}
	sr := &statusWriter{ResponseWriter: w}
	s.handleStartRun(sr, req)
	if sr.status != http.StatusOK {
		s.endSweep(sw, sweepFailed, "level 1 did not start")
		return true
	}
	go s.runSweep(sw, form)
	return true
}

func newSweepID() string {
	return "sweep-" + time.Now().UTC().Format("20060102T150405")
}

func (s *WebServer) endSweep(sw *sweepRun, state, msg string) {
	s.mu.Lock()
	sw.State, sw.Message = state, msg
	s.mu.Unlock()
}

// sleepOrCancel waits for d and reports false when the sweep was stopped.
func (s *WebServer) sleepOrCancel(sw *sweepRun, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		s.mu.Lock()
		stopped := sw.Cancelled
		s.mu.Unlock()
		if stopped {
			return false
		}
		left := time.Until(deadline)
		if left <= 0 {
			return true
		}
		if left > 200*time.Millisecond {
			left = 200 * time.Millisecond
		}
		time.Sleep(left)
	}
}

// runSweep follows the sweep after level 1 has started: it waits for each
// level to finish and starts the next. It stops at the first failed level
// or when the user stops the sweep.
func (s *WebServer) runSweep(sw *sweepRun, form *sweepForm) {
	total := len(sw.Workers)
	for i := 0; i < total; i++ {
		if i > 0 {
			s.mu.Lock()
			sw.State = sweepWaiting
			sw.Message = fmt.Sprintf("Level %d of %d (%d workers) starts next", i+1, total, sw.Workers[i])
			s.mu.Unlock()
			if sw.Pause > 0 && !s.sleepOrCancel(sw, sw.Pause) {
				s.endSweep(sw, sweepStopped, "Sweep stopped by user")
				return
			}
			s.mu.Lock()
			if sw.Cancelled {
				s.mu.Unlock()
				s.endSweep(sw, sweepStopped, "Sweep stopped by user")
				return
			}
			sw.Level, sw.State = i+1, sweepRunning
			sw.Message = fmt.Sprintf("Level %d of %d: %d workers", i+1, total, sw.Workers[i])
			s.mu.Unlock()
			req, err := form.request(sw.Workers[i], sweepLevelLabel(sw.Label, sw.Workers[i]))
			if err != nil {
				s.endSweep(sw, sweepFailed, err.Error())
				return
			}
			sr := &statusWriter{ResponseWriter: &discardWriter{}}
			s.handleStartRun(sr, req)
			if sr.status != http.StatusOK {
				msg := strings.TrimSpace(sr.body.String())
				s.endSweep(sw, sweepFailed, fmt.Sprintf("level %d (%d workers) did not start: %s", i+1, sw.Workers[i], msg))
				return
			}
		}
		// The run goroutine stores the dashboard before it ends.
		s.runWG.Wait()
		s.mu.Lock()
		sw.LastDash = s.dashboardID
		cancelled := sw.Cancelled
		failed := s.LifecyclePhase == "failed" || s.LastError != ""
		reason := s.LastError
		if reason == "" {
			reason = s.LifecycleMessage
		}
		s.mu.Unlock()
		if cancelled {
			s.endSweep(sw, sweepStopped, "Sweep stopped by user")
			return
		}
		if failed {
			s.endSweep(sw, sweepFailed, fmt.Sprintf("level %d (%d workers) failed: %s", i+1, sw.Workers[i], reason))
			return
		}
	}
	s.endSweep(sw, sweepDone, fmt.Sprintf("Sweep complete: %d levels", total))
}
