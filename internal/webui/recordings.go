package webui

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/workloadrec"
)

const (
	// maxRecordingDirEntries bounds the directory entries read per
	// recordings directory; maxRecordingPeeks bounds the files whose header
	// is read; maxListedRecordings bounds the response.
	maxRecordingDirEntries = 2000
	maxRecordingPeeks      = 200
	maxListedRecordings    = 100
	// maxSessionRecordings bounds the recordings remembered from runs.
	maxSessionRecordings = 20
)

// recordingExts are the file name extensions a recording can have.
var recordingExts = map[string]bool{".ndjson": true, ".jsonl": true, ".json": true, ".zst": true, ".gz": true}

// recordingEntry is one recording the server can replay. Path is the value
// for workload_replay_path; nothing in it holds literal values.
type recordingEntry struct {
	Path        string `json:"path"`
	Name        string `json:"name"`
	Source      string `json:"source"`
	SizeBytes   int64  `json:"size_bytes"`
	ModifiedAt  string `json:"modified_at"`
	CreatedAt   string `json:"created_at,omitempty"`
	PLGMVersion string `json:"plgm_version,omitempty"`
	Workers     int    `json:"workers,omitempty"`
	Iterations  int    `json:"iterations,omitempty"`
	Compression string `json:"compression,omitempty"`
	Imported    bool   `json:"imported,omitempty"`
	Outcomes    bool   `json:"outcomes,omitempty"`
	// Ops and Status come from the trailer of an uncompressed recording.
	Ops    *uint64 `json:"ops,omitempty"`
	Status string  `json:"status,omitempty"`
	// Problem is set for a file that looks like a recording but cannot be
	// replayed (unreadable, damaged header, unsupported format).
	Problem string `json:"problem,omitempty"`

	modTime time.Time
}

// Recording sources.
const (
	recordingSourceDirectory = "directory"
	recordingSourceImport    = "import"
	recordingSourceRun       = "run"
)

// serverRecordingDirs are the directories listed for recordings: those of
// the recording and replay paths the server started with. Form values never
// change them.
func serverRecordingDirs(cfg *config.AppConfig) []string {
	paths := []string{config.DefaultWorkloadRecordPath}
	if cfg != nil {
		if p := strings.TrimSpace(cfg.WorkloadRecordPath); p != "" {
			paths[0] = p
		}
		if p := strings.TrimSpace(cfg.WorkloadReplayPath); p != "" {
			paths = append(paths, p)
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, p := range paths {
		dir := filepath.Clean(filepath.Dir(p))
		key := dir
		if abs, err := filepath.Abs(dir); err == nil {
			key = abs
		}
		if !seen[key] {
			seen[key] = true
			out = append(out, dir)
		}
	}
	return out
}

// rememberRecording keeps the path a record run wrote, so it is listed even
// outside the recordings directories.
func (s *WebServer) rememberRecording(path string) {
	path = strings.TrimSpace(path)
	if path == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, p := range s.sessionRecordings {
		if p == path {
			s.sessionRecordings = append(s.sessionRecordings[:i], s.sessionRecordings[i+1:]...)
			break
		}
	}
	s.sessionRecordings = append(s.sessionRecordings, path)
	if n := len(s.sessionRecordings); n > maxSessionRecordings {
		s.sessionRecordings = append([]string(nil), s.sessionRecordings[n-maxSessionRecordings:]...)
	}
}

// describeRecording reads the header (and the trailer when cheap) of path.
// ok is false when the file is not a recording at all.
func describeRecording(path, name, source string, fi os.FileInfo) (recordingEntry, bool) {
	e := recordingEntry{Path: path, Name: name, Source: source, SizeBytes: fi.Size(), ModifiedAt: fi.ModTime().UTC().Format(time.RFC3339), modTime: fi.ModTime()}
	sum, err := workloadrec.PeekRecording(path)
	if err != nil {
		if errors.Is(err, workloadrec.ErrNotRecording) {
			return e, false
		}
		e.Problem = err.Error()
		if errors.Is(err, os.ErrPermission) {
			e.Problem = "the PLGM server cannot read this file (permission denied)"
		}
		return e, true
	}
	h := sum.Header
	e.CreatedAt, e.PLGMVersion, e.Workers, e.Iterations = h.CreatedAt, h.PLGMVersion, h.Workers, h.Iterations
	e.Compression, e.Imported, e.Outcomes = sum.Compression, h.Import != nil, h.Outcomes
	if t := sum.Trailer; t != nil {
		ops := t.Ops
		e.Ops, e.Status = &ops, t.Status
	}
	return e, true
}

// scanRecordingDir lists the recordings directly in dir (no recursion).
// Symbolic links and other non-regular files are skipped.
func scanRecordingDir(dir string, peeks *int) ([]recordingEntry, bool) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, false
	}
	entries, err := f.ReadDir(maxRecordingDirEntries)
	truncated := len(entries) == maxRecordingDirEntries
	f.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false
	}
	type candidate struct {
		name string
		fi   os.FileInfo
	}
	var cands []candidate
	for _, de := range entries {
		if !de.Type().IsRegular() || !recordingExts[strings.ToLower(filepath.Ext(de.Name()))] {
			continue
		}
		fi, err := de.Info()
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		cands = append(cands, candidate{de.Name(), fi})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].fi.ModTime().After(cands[j].fi.ModTime()) })
	var out []recordingEntry
	for _, c := range cands {
		if *peeks >= maxRecordingPeeks {
			truncated = true
			break
		}
		*peeks++
		if e, ok := describeRecording(filepath.Join(dir, c.name), c.name, recordingSourceDirectory, c.fi); ok {
			out = append(out, e)
		}
	}
	return out, truncated
}

// listRecordings returns the recordings the server can replay, newest
// first: those in the recordings directories, converted traffic imports,
// and files recorded by runs of this server.
func (s *WebServer) listRecordings() (dirs []string, out []recordingEntry, truncated bool) {
	s.mu.Lock()
	dirs = append([]string(nil), s.recordingDirs...)
	imports := append([]*trafficImport(nil), s.trafficImports...)
	session := append([]string(nil), s.sessionRecordings...)
	s.mu.Unlock()
	if len(dirs) == 0 {
		dirs = []string{"."}
	}
	seen := map[string]bool{}
	key := func(p string) string {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
		return filepath.Clean(p)
	}
	peeks := 0
	for _, dir := range dirs {
		list, more := scanRecordingDir(dir, &peeks)
		truncated = truncated || more
		for _, e := range list {
			seen[key(e.Path)] = true
			out = append(out, e)
		}
	}
	add := func(path, name, source string) {
		if seen[key(path)] {
			return
		}
		fi, err := os.Lstat(path)
		if err != nil || !fi.Mode().IsRegular() {
			return
		}
		seen[key(path)] = true
		if e, ok := describeRecording(path, name, source, fi); ok {
			out = append(out, e)
		}
	}
	for i := len(imports) - 1; i >= 0; i-- {
		add(imports[i].ServerPath, imports[i].FileName, recordingSourceImport)
	}
	for i := len(session) - 1; i >= 0; i-- {
		add(session[i], filepath.Base(session[i]), recordingSourceRun)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].modTime.After(out[j].modTime) })
	if len(out) > maxListedRecordings {
		out, truncated = out[:maxListedRecordings], true
	}
	return dirs, out, truncated
}

// handleRecordings lists the recordings available on the PLGM server.
func (s *WebServer) handleRecordings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	dirs, list, truncated := s.listRecordings()
	shown := make([]string, len(dirs))
	for i, d := range dirs {
		shown[i] = d
		if abs, err := filepath.Abs(d); err == nil {
			shown[i] = abs
		}
	}
	if list == nil {
		list = []recordingEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"directories": shown, "recordings": list, "truncated": truncated, "max_recordings": maxListedRecordings})
}

// checkReplayRecording revalidates a server-path recording when a run
// starts, so a file deleted or replaced since it was listed fails with an
// actionable message before anything connects.
func checkReplayRecording(path string) error {
	fi, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return errors.New("the recording " + path + " no longer exists on the PLGM server: refresh the recording list and choose another one")
	case err != nil:
		return errors.New("the PLGM server cannot read the recording " + path + ": " + err.Error())
	case !fi.Mode().IsRegular():
		return errors.New("the recording " + path + " is not a regular file")
	}
	if _, err := workloadrec.PeekRecording(path); err != nil {
		return errors.New("the file " + path + " cannot be replayed: " + err.Error())
	}
	return nil
}
