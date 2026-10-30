package webui

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
)

const (
	savedSettingsVersion = 1
	savedSettingsPrefix  = "plgm_settings_"
	// maxSavedSettingsBytes bounds a snapshot sent by the page and a file read back.
	maxSavedSettingsBytes = 1 << 20
	// maxSavedSettingsScan bounds the directory entries read and maxSavedSettingsListed the response.
	maxSavedSettingsScan   = 2000
	maxSavedSettingsListed = 100
)

var (
	// savedSettingsName is the only file name the server writes and reads:
	// the user chooses the folder, never the name, so a saved file is not overwritten.
	savedSettingsName = regexp.MustCompile(`^` + savedSettingsPrefix + `[0-9]{8}-[0-9]{6}(-[0-9]+)?\.json$`)
	// uriPassword matches the password of a URI with credentials.
	uriPassword = regexp.MustCompile(`(mongodb(?:\+srv)?://[^/@\s:"]*):[^/@\s"]*@`)
	// secretField matches setting names that are never saved.
	secretField = regexp.MustCompile(`(?i)password|passwd|secret|token`)
)

// savedSettings is the file written when a run starts. Fields are the
// values of the settings form by field name, as the page restores them.
type savedSettings struct {
	Version            int                    `json:"plgm_settings_version"`
	SavedAt            string                 `json:"saved_at"`
	PLGMVersion        string                 `json:"plgm_version,omitempty"`
	Fields             map[string]interface{} `json:"fields"`
	ReplayTargets      []map[string]string    `json:"replay_targets,omitempty"`
	CredentialsRemoved bool                   `json:"credentials_removed,omitempty"`
}

// sanitizeSavedSettings keeps scalar field values, drops secrets and the
// save controls themselves, and removes the password from every URI.
func sanitizeSavedSettings(raw []byte) (*savedSettings, error) {
	if len(raw) > maxSavedSettingsBytes {
		return nil, errors.New("the settings are too large to save")
	}
	var in struct {
		Fields        map[string]interface{} `json:"fields"`
		ReplayTargets []map[string]string    `json:"replay_targets"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("the settings are not valid JSON: %w", err)
	}
	out := &savedSettings{Version: savedSettingsVersion, Fields: map[string]interface{}{}}
	scrub := func(s string) string {
		if !uriPassword.MatchString(s) {
			return s
		}
		out.CredentialsRemoved = true
		return uriPassword.ReplaceAllString(s, "$1@")
	}
	for name, v := range in.Fields {
		if secretField.MatchString(name) || strings.HasPrefix(name, "save_settings") {
			continue
		}
		switch t := v.(type) {
		case string:
			out.Fields[name] = scrub(t)
		case bool, float64:
			out.Fields[name] = t
		}
	}
	for _, t := range in.ReplayTargets {
		out.ReplayTargets = append(out.ReplayTargets, map[string]string{"label": t["label"], "uri": scrub(t["uri"])})
	}
	// The hidden targets field repeats the list with passwords in it.
	if len(out.ReplayTargets) > 0 {
		delete(out.Fields, "workload_replay_targets")
	}
	return out, nil
}

// settingsDigest identifies the settings themselves: the same values give the
// same digest, whatever the time or the file they were saved in.
func (snap *savedSettings) settingsDigest() string {
	b, _ := json.Marshal(struct {
		Fields        map[string]interface{} `json:"fields"`
		ReplayTargets []map[string]string    `json:"replay_targets"`
	}{snap.Fields, snap.ReplayTargets})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// findSavedDuplicate returns the name of a saved settings file in dir that
// holds exactly these settings, or "".
func findSavedDuplicate(dir string, snap *savedSettings) string {
	f, err := os.Open(dir)
	if err != nil {
		return ""
	}
	entries, _ := f.ReadDir(maxSavedSettingsScan)
	f.Close()
	var names []string
	for _, de := range entries {
		if de.Type().IsRegular() && savedSettingsName.MatchString(de.Name()) {
			names = append(names, de.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	want := snap.settingsDigest()
	for _, name := range names {
		if old, _, err := readSavedSettingsFile(dir, name); err == nil && old.settingsDigest() == want {
			return name
		}
	}
	return ""
}

// savedSettingsDir is the folder the user chose, or the folder of the
// recording path the server started with when it is blank.
func (s *WebServer) savedSettingsDir(dir string) string {
	if dir = strings.TrimSpace(dir); dir != "" {
		return filepath.Clean(dir)
	}
	if len(s.recordingDirs) > 0 {
		return s.recordingDirs[0]
	}
	return "."
}

func absDir(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

// writeSavedSettings writes the snapshot to a new file named with the
// current time in dir and returns its path. A file is never overwritten.
func writeSavedSettings(dir string, now time.Time, snap *savedSettings) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	snap.SavedAt = now.UTC().Format(time.RFC3339)
	snap.PLGMVersion = config.BuildVersion
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return "", err
	}
	stamp := now.Format("20060102-150405")
	for i := 0; i < 100; i++ {
		name := savedSettingsPrefix + stamp + ".json"
		if i > 0 {
			name = fmt.Sprintf("%s%s-%d.json", savedSettingsPrefix, stamp, i+1)
		}
		path := filepath.Join(dir, name)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, werr := f.Write(data)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			_ = os.Remove(path)
			return "", werr
		}
		return path, nil
	}
	return "", errors.New("could not find a free file name")
}

// saveRunSettings saves the settings of a run that is starting when the
// page asks for it. A failure is reported in the run's events and never
// stops the run.
func (s *WebServer) saveRunSettings(r *http.Request) {
	if r.FormValue("save_settings") != "true" {
		return
	}
	report := func(msg string) {
		s.mu.Lock()
		s.addLifecycleEventLocked("config", msg)
		s.mu.Unlock()
	}
	snap, err := sanitizeSavedSettings([]byte(r.FormValue("settings_snapshot")))
	if err != nil {
		report("Run settings were not saved: " + err.Error())
		return
	}
	dir := s.savedSettingsDir(r.FormValue("save_settings_dir"))
	if name := findSavedDuplicate(dir, snap); name != "" {
		report("Run settings are the same as " + name + " in " + absDir(dir) + "; no new file was saved")
		return
	}
	path, err := writeSavedSettings(dir, time.Now(), snap)
	if err != nil {
		report("Run settings were not saved: " + err.Error())
		return
	}
	note := "Run settings saved to " + absDir(path)
	if snap.CredentialsRemoved {
		note += " (passwords in URIs were left out)"
	}
	report(note)
}

type savedSettingsEntry struct {
	Name               string `json:"name"`
	SavedAt            string `json:"saved_at"`
	Mode               string `json:"mode,omitempty"`
	Duration           string `json:"duration,omitempty"`
	Iterations         string `json:"iterations,omitempty"`
	Concurrency        string `json:"concurrency,omitempty"`
	Servers            int    `json:"servers,omitempty"`
	CredentialsRemoved bool   `json:"credentials_removed,omitempty"`
}

func readSavedSettingsFile(dir, name string) (*savedSettings, []byte, error) {
	if !savedSettingsName.MatchString(name) {
		return nil, nil, errors.New("not a saved settings file name")
	}
	path := filepath.Join(dir, name)
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !fi.Mode().IsRegular() || fi.Size() > maxSavedSettingsBytes {
		return nil, nil, errors.New("not a saved settings file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxSavedSettingsBytes))
	if err != nil {
		return nil, nil, err
	}
	var snap savedSettings
	if err := json.Unmarshal(data, &snap); err != nil || snap.Version != savedSettingsVersion || snap.Fields == nil {
		return nil, nil, errors.New("not a PLGM saved settings file")
	}
	return &snap, data, nil
}

func fieldText(m map[string]interface{}, k string) string {
	switch v := m[k].(type) {
	case string:
		return v
	case float64:
		return fmt.Sprint(v)
	}
	return ""
}

// handleSavedSettings lists the saved settings files of ?dir= (blank: the default folder), newest first.
func (s *WebServer) handleSavedSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	dir := s.savedSettingsDir(r.URL.Query().Get("dir"))
	resp := map[string]interface{}{"directory": absDir(dir), "default_directory": absDir(s.savedSettingsDir(""))}
	list := []savedSettingsEntry{}
	f, err := os.Open(dir)
	if err != nil {
		// A folder that does not exist yet simply holds nothing.
		if !errors.Is(err, os.ErrNotExist) {
			resp["error"] = "the PLGM server cannot read this folder: " + err.Error()
		}
		resp["files"] = list
		writeJSON(w, http.StatusOK, resp)
		return
	}
	entries, _ := f.ReadDir(maxSavedSettingsScan)
	f.Close()
	type cand struct {
		name string
		mod  time.Time
	}
	var cands []cand
	for _, de := range entries {
		if !de.Type().IsRegular() || !savedSettingsName.MatchString(de.Name()) {
			continue
		}
		if fi, err := de.Info(); err == nil {
			cands = append(cands, cand{de.Name(), fi.ModTime()})
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].name > cands[j].name })
	for _, c := range cands {
		if len(list) >= maxSavedSettingsListed {
			resp["truncated"] = true
			break
		}
		snap, _, err := readSavedSettingsFile(dir, c.name)
		if err != nil {
			continue
		}
		list = append(list, savedSettingsEntry{
			Name: c.name, SavedAt: snap.SavedAt, Mode: fieldText(snap.Fields, "operation_mode"),
			Duration: fieldText(snap.Fields, "duration"), Iterations: fieldText(snap.Fields, "iterations"),
			Concurrency: fieldText(snap.Fields, "concurrency"), Servers: len(snap.ReplayTargets),
			CredentialsRemoved: snap.CredentialsRemoved,
		})
	}
	resp["files"] = list
	writeJSON(w, http.StatusOK, resp)
}

// handleSavedSettingItem returns one saved settings file: ?dir= and ?name=.
func (s *WebServer) handleSavedSettingItem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	_, data, err := readSavedSettingsFile(s.savedSettingsDir(r.URL.Query().Get("dir")), r.URL.Query().Get("name"))
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, os.ErrNotExist) {
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}
