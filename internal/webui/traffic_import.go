package webui

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/mongo"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/trafficimport"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/workloadrec"
)

// maxTrafficImports bounds the converted recordings the server keeps. They
// hold literal values, so older ones are deleted and all are deleted when
// the server shuts down from the UI.
const maxTrafficImports = 5

// trafficImport is one converted recording kept as a 0600 temp file.
type trafficImport struct {
	ID          string                `json:"id"`
	CreatedAt   string                `json:"created_at"`
	FileName    string                `json:"file_name"`
	ServerPath  string                `json:"server_path"`
	DownloadURL string                `json:"download_url"`
	Report      *trafficimport.Report `json:"report"`
}

func (s *WebServer) trafficImportList() []*trafficImport {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*trafficImport, 0, len(s.trafficImports))
	for i := len(s.trafficImports) - 1; i >= 0; i-- {
		out = append(out, s.trafficImports[i])
	}
	return out
}

func (s *WebServer) keepTrafficImport(t *trafficImport) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trafficImports = append(s.trafficImports, t)
	for len(s.trafficImports) > maxTrafficImports {
		_ = os.Remove(s.trafficImports[0].ServerPath)
		s.trafficImports = s.trafficImports[1:]
	}
}

func (s *WebServer) removeTrafficImports() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.trafficImports {
		_ = os.Remove(t.ServerPath)
	}
	s.trafficImports = nil
}

// handleImportTraffic: GET lists the kept imports (newest first); POST
// converts uploaded traffic_file(s) into a recording.
func (s *WebServer) handleImportTraffic(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]interface{}{"imports": s.trafficImportList(), "max_imports": maxTrafficImports})
	case http.MethodPost:
		s.createTrafficImport(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// handleImportTrafficItem serves {id}/recording as a download.
func (s *WebServer) handleImportTrafficItem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	id := strings.TrimSuffix(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/import-traffic/"), "/"), "/recording")
	var found *trafficImport
	for _, t := range s.trafficImportList() {
		if t.ID == id {
			found = t
		}
	}
	if found == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "import not found (the server keeps the newest " + strconv.Itoa(maxTrafficImports) + " until it restarts)"})
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, found.FileName))
	http.ServeFile(w, r, found.ServerPath)
}

func (s *WebServer) createTrafficImport(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "upload one or more captured-traffic files (traffic_file): " + err.Error()})
		return
	}
	defer r.MultipartForm.RemoveAll()
	files := r.MultipartForm.File["traffic_file"]
	if len(files) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "upload one or more captured-traffic files (traffic_file)"})
		return
	}
	opts := trafficimport.Options{
		DefaultDatabase:     strings.TrimSpace(r.FormValue("default_db")),
		Compression:         strings.ToLower(strings.TrimSpace(r.FormValue("compression"))),
		SkipServerGenerated: r.FormValue("skip_server_generated") == "true" || r.FormValue("skip_server_generated") == "on",
		Normalize:           mongo.ImportedOperation,
	}
	if v := strings.TrimSpace(r.FormValue("workers")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > trafficimport.MaxWorkers {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("workers must be a whole number from 0 (one per connection) to %d", trafficimport.MaxWorkers)})
			return
		}
		opts.Workers = n
	}
	var inputs []trafficimport.Input
	for _, fh := range files {
		f, err := fh.Open()
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read " + fh.Filename + ": " + err.Error()})
			return
		}
		defer f.Close()
		content, release, err := workloadrec.Decompress(f)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": filepath.Base(fh.Filename) + ": " + err.Error()})
			return
		}
		defer release()
		inputs = append(inputs, trafficimport.Input{Name: fh.Filename, R: content})
	}
	ext := ".ndjson"
	switch opts.Compression {
	case workloadrec.CompressionZstd:
		ext = ".ndjson.zst"
	case workloadrec.CompressionGzip:
		ext = ".ndjson.gz"
	}
	tmp, err := os.CreateTemp("", "plgm-imported-traffic-*"+ext)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	path := tmp.Name()
	tmp.Close()
	rep, err := trafficimport.Convert(inputs, path, opts)
	if err != nil {
		_ = os.Remove(path)
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": strings.ReplaceAll(err.Error(), path, "the recording"), "report": rep})
		return
	}
	id := newComparisonID()
	rep.Path = ""
	t := &trafficImport{
		ID: id, CreatedAt: time.Now().UTC().Format(time.RFC3339), FileName: "plgm_imported_traffic_" + id + ext,
		ServerPath: path, DownloadURL: "/api/import-traffic/" + id + "/recording", Report: rep,
	}
	s.keepTrafficImport(t)
	writeJSON(w, http.StatusOK, map[string]interface{}{"import": t})
}
