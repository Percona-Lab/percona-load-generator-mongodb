package webui

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	htmlstd "html"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/replaycompare"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
)

// maxReplayComparisons bounds the comparison reports kept in memory. They
// can hold literal values, so they are never written to disk by the server.
const maxReplayComparisons = 10

var compareReplaysFn = replaycompare.Compare

// replayComparisonSummary lists a kept report without literal values.
type replayComparisonSummary struct {
	ID                    string `json:"id"`
	GeneratedAt           string `json:"generated_at"`
	LabelA                string `json:"label_a"`
	LabelB                string `json:"label_b"`
	FileA                 string `json:"file_a"`
	FileB                 string `json:"file_b"`
	IdentityKind          string `json:"identity_kind,omitempty"`
	Identity              string `json:"identity,omitempty"`
	IdentityMatch         bool   `json:"identity_match"`
	DifferingOps          uint64 `json:"differing_ops"`
	RunA                  string `json:"run_a,omitempty"`
	RunB                  string `json:"run_b,omitempty"`
	ContainsLiteralValues bool   `json:"contains_literal_values,omitempty"`
}

func summarizeComparison(r *replaycompare.Report) replayComparisonSummary {
	return replayComparisonSummary{
		ID: r.ID, GeneratedAt: r.GeneratedAt, LabelA: r.A.Label, LabelB: r.B.Label, FileA: r.A.File, FileB: r.B.File,
		IdentityKind: r.IdentityKind, Identity: r.Identity, IdentityMatch: r.IdentityMatch, DifferingOps: r.Overall.DifferingOps,
		RunA: r.A.PlanRunID, RunB: r.B.PlanRunID, ContainsLiteralValues: r.ContainsLiteralValues,
	}
}

func newComparisonID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (s *WebServer) replayComparisons() []replayComparisonSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]replayComparisonSummary, 0, len(s.replayCompares))
	for i := len(s.replayCompares) - 1; i >= 0; i-- {
		out = append(out, summarizeComparison(s.replayCompares[i]))
	}
	return out
}

func (s *WebServer) replayComparison(id string) *replaycompare.Report {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.replayCompares {
		if r.ID == id {
			return r
		}
	}
	return nil
}

func (s *WebServer) keepReplayComparison(r *replaycompare.Report) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.replayCompares = append(s.replayCompares, r)
	if n := len(s.replayCompares); n > maxReplayComparisons {
		s.replayCompares = append([]*replaycompare.Report(nil), s.replayCompares[n-maxReplayComparisons:]...)
	}
}

// handleReplayCompare: GET lists the kept comparisons (newest first); POST
// compares two outcome files (uploads a_file/b_file or server paths
// a_path/b_path, optional recording_file/recording_path).
func (s *WebServer) handleReplayCompare(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]interface{}{"reports": s.replayComparisons(), "max_reports": maxReplayComparisons})
	case http.MethodPost:
		s.createReplayComparison(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// handleReplayCompareItem serves {id} (JSON) and {id}/report.html. The
// HTML report opens inline in a browser tab; ?download=1 sends it as an
// attachment. Only kept reports are served, by id; nothing is read from disk.
func (s *WebServer) handleReplayCompareItem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/replay-compare/"), "/")
	id, html := rest, false
	if strings.HasSuffix(rest, "/report.html") {
		id, html = strings.TrimSuffix(rest, "/report.html"), true
	}
	rep := s.replayComparison(id)
	if rep == nil {
		msg := "replay comparison not found (reports are kept in memory until the server restarts, newest " + strconv.Itoa(maxReplayComparisons) + ")"
		if html {
			writeReportNotFound(w, "This comparison report is no longer available. Reports are kept in the PLGM server's memory until it restarts, newest "+strconv.Itoa(maxReplayComparisons)+" only. Run the comparison again (Query Analysis > Replay Comparison) to recreate it.")
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"error": msg})
		return
	}
	if !html {
		writeJSON(w, http.StatusOK, map[string]interface{}{"report": rep})
		return
	}
	var buf bytes.Buffer
	if err := replaycompare.WriteHTML(&buf, rep); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	setReportHeaders(w)
	disposition := "inline"
	if r.URL.Query().Get("download") == "1" {
		disposition = "attachment"
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`%s; filename="plgm-replay-compare-%s.html"`, disposition, rep.ID))
	_, _ = w.Write(buf.Bytes())
}

// setReportHeaders marks a self-contained HTML report: no scripts, no
// outside resources, and never sniffed as another type.
func setReportHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'self'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
}

// writeReportNotFound answers a report link that no longer resolves with a
// readable page instead of JSON, since it is opened in a browser tab.
func writeReportNotFound(w http.ResponseWriter, message string) {
	setReportHeaders(w)
	w.WriteHeader(http.StatusNotFound)
	_, _ = fmt.Fprintf(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Report not available</title><style>body{font-family:system-ui,sans-serif;max-width:40rem;margin:4rem auto;padding:0 1rem;color:#1f2937;line-height:1.5}h1{font-size:1.25rem}</style></head><body><h1>Report not available</h1><p>%s</p></body></html>`, htmlstd.EscapeString(message))
}

// comparisonInput returns the path of field's upload (copied to a temp file
// that cleanup removes) and its file name, or the server path in pathField.
func comparisonInput(r *http.Request, field, pathField string, cleanup *[]string) (string, string, error) {
	if file, header, err := r.FormFile(field); err == nil {
		p, err := saveUploadedTempFile(file, "plgm-replay-compare-*.ndjson")
		file.Close()
		if err != nil {
			return "", "", fmt.Errorf("store %s: %w", field, err)
		}
		*cleanup = append(*cleanup, p)
		return p, filepath.Base(header.Filename), nil
	}
	p := strings.TrimSpace(r.FormValue(pathField))
	if strings.ContainsAny(p, "\x00\r\n") {
		return "", "", fmt.Errorf("%s is not a valid path", pathField)
	}
	return p, "", nil
}

func (s *WebServer) createReplayComparison(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil && !errors.Is(err, http.ErrNotMultipart) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to parse the form: " + err.Error()})
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	var cleanup []string
	defer func() {
		for _, p := range cleanup {
			_ = os.Remove(p)
		}
	}()
	a, nameA, err := comparisonInput(r, "a_file", "a_path", &cleanup)
	if err == nil && a == "" {
		err = errors.New("side A is required: upload a file or give a server path")
	}
	var b, nameB, rec string
	if err == nil {
		b, nameB, err = comparisonInput(r, "b_file", "b_path", &cleanup)
	}
	if err == nil && b == "" {
		err = errors.New("side B is required: upload a file or give a server path")
	}
	if err == nil {
		rec, _, err = comparisonInput(r, "recording_file", "recording_path", &cleanup)
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	opts := replaycompare.Options{
		LabelA: clipID(strings.TrimSpace(r.FormValue("label_a")), 64), LabelB: clipID(strings.TrimSpace(r.FormValue("label_b")), 64),
		AllowMismatch: r.FormValue("allow_mismatch") == "true" || r.FormValue("allow_mismatch") == "on", RecordingPath: rec,
	}
	if v := strings.TrimSpace(r.FormValue("top_n")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > replaycompare.MaxTopN {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("top_n must be a whole number from 1 to %d", replaycompare.MaxTopN)})
			return
		}
		opts.TopN = n
	}
	runA, runB := strings.TrimSpace(r.FormValue("run_a")), strings.TrimSpace(r.FormValue("run_b"))
	if runA != "" || runB != "" {
		store, _ := s.trendContext()
		d := store.Snapshot()
		for _, p := range []struct {
			id  string
			dst *map[string]replaycompare.PlanInfo
		}{{runA, &opts.PlansA}, {runB, &opts.PlansB}} {
			if p.id == "" {
				continue
			}
			m, ok := replaycompare.PlansFromTrendRun(d, p.id)
			if !stats.ValidTrendRunID(p.id) || !ok {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "trend run not found: " + clipID(p.id, 80)})
				return
			}
			*p.dst = m
		}
	}
	rep, err := compareReplaysFn(r.Context(), a, b, opts)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, replaycompare.ErrMismatch) {
			code = http.StatusConflict
		}
		msg := err.Error()
		for _, u := range [][2]string{{a, nameA}, {b, nameB}} {
			if u[1] != "" {
				msg = strings.ReplaceAll(msg, filepath.Base(u[0]), u[1])
			}
		}
		writeJSON(w, code, map[string]string{"error": msg})
		return
	}
	if nameA != "" {
		rep.A.File = nameA
	}
	if nameB != "" {
		rep.B.File = nameB
	}
	rep.A.PlanRunID, rep.B.PlanRunID = runA, runB
	rep.ID = newComparisonID()
	s.keepReplayComparison(rep)
	writeJSON(w, http.StatusOK, map[string]interface{}{"report": rep})
}
