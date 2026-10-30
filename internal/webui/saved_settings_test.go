package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const snapshotJSON = `{"fields":{"uri":"mongodb://alice:s3cret@h1:27017/","password":"x","tls_key_password":"y","save_settings":true,"save_settings_dir":"/tmp",` +
	`"operation_mode":"generic","duration":"30s","iterations":"2","concurrency":"4","operations_per_worker":"500","workload_replay_targets":"[{\"uri\":\"mongodb://u:p@a\"}]","nested":{"a":1}},` +
	`"replay_targets":[{"label":"a","uri":"mongodb://u:p@a:1/"},{"label":"b","uri":"mongodb://b:2/"}]}`

func TestSanitizeSavedSettingsDropsSecrets(t *testing.T) {
	snap, err := sanitizeSavedSettings([]byte(snapshotJSON))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(snap)
	for _, bad := range []string{"s3cret", ":p@", "tls_key_password", `"password"`, "save_settings", "nested", "workload_replay_targets"} {
		if strings.Contains(string(out), bad) {
			t.Errorf("saved settings still contain %q: %s", bad, out)
		}
	}
	if snap.Fields["uri"] != "mongodb://alice@h1:27017/" || !snap.CredentialsRemoved {
		t.Errorf("uri = %v removed=%v", snap.Fields["uri"], snap.CredentialsRemoved)
	}
	if snap.Fields["duration"] != "30s" || len(snap.ReplayTargets) != 2 || snap.ReplayTargets[0]["uri"] != "mongodb://u@a:1/" {
		t.Errorf("unexpected snapshot: %s", out)
	}
	if _, err := sanitizeSavedSettings([]byte("{")); err == nil {
		t.Error("invalid JSON accepted")
	}
	if _, err := sanitizeSavedSettings([]byte(strings.Repeat(" ", maxSavedSettingsBytes+1))); err == nil {
		t.Error("oversized snapshot accepted")
	}
}

func TestSavedSettingsAreNeverOverwrittenAndListedNewestFirst(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "settings")
	snap, _ := sanitizeSavedSettings([]byte(snapshotJSON))
	now := time.Date(2026, 10, 30, 15, 4, 5, 0, time.Local)
	p1, err := writeSavedSettings(dir, now, snap)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := writeSavedSettings(dir, now, snap)
	if err != nil {
		t.Fatal(err)
	}
	p3, err := writeSavedSettings(dir, now.Add(time.Minute), snap)
	if err != nil {
		t.Fatal(err)
	}
	if p1 == p2 || filepath.Base(p1) != "plgm_settings_20261030-150405.json" || filepath.Base(p2) != "plgm_settings_20261030-150405-2.json" {
		t.Fatalf("names: %s %s", p1, p2)
	}
	if fi, _ := os.Stat(p1); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
	writeFile(t, filepath.Join(dir, "other.json"), "{}", time.Time{})
	writeFile(t, filepath.Join(dir, "plgm_settings_20261030-150000.json"), "not json", time.Time{})

	s := &WebServer{recordingDirs: []string{dir}}
	rec := httptest.NewRecorder()
	s.handleSavedSettings(rec, httptest.NewRequest(http.MethodGet, "/api/saved-settings?dir="+url.QueryEscape(dir), nil))
	var got struct {
		Files []savedSettingsEntry `json:"files"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Files) != 3 || got.Files[0].Name != filepath.Base(p3) || got.Files[0].Mode != "generic" || got.Files[0].Servers != 2 || !got.Files[0].CredentialsRemoved {
		t.Fatalf("files = %+v", got.Files)
	}

	rec = httptest.NewRecorder()
	s.handleSavedSettingItem(rec, httptest.NewRequest(http.MethodGet, "/api/saved-settings/item?name="+filepath.Base(p1), nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"duration": "30s"`) {
		t.Fatalf("item (default folder): %d %s", rec.Code, rec.Body.String())
	}
	for _, name := range []string{"../etc/passwd", "other.json", "plgm_settings_20261030-150000.json", "plgm_settings_20200101-000000.json"} {
		rec = httptest.NewRecorder()
		s.handleSavedSettingItem(rec, httptest.NewRequest(http.MethodGet, "/api/saved-settings/item?name="+url.QueryEscape(name), nil))
		if rec.Code < 400 {
			t.Errorf("%s: status %d", name, rec.Code)
		}
	}
	rec = httptest.NewRecorder()
	s.handleSavedSettings(rec, httptest.NewRequest(http.MethodGet, "/api/saved-settings?dir="+url.QueryEscape(filepath.Join(dir, "missing")), nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"files":[]`) {
		t.Errorf("missing folder: %d %s", rec.Code, rec.Body.String())
	}
}

func TestSaveRunSettingsHonorsCheckboxAndReportsInEvents(t *testing.T) {
	dir := t.TempDir()
	s := &WebServer{recordingDirs: []string{dir}}
	form := func(v url.Values) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/api/start", strings.NewReader(v.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return r
	}
	s.saveRunSettings(form(url.Values{"settings_snapshot": {snapshotJSON}}))
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("saved without the checkbox: %v", entries)
	}
	s.saveRunSettings(form(url.Values{"save_settings": {"true"}, "settings_snapshot": {snapshotJSON}}))
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("default folder files = %v", entries)
	}
	// The same settings again, even with another URI password, are not saved twice.
	s.saveRunSettings(form(url.Values{"save_settings": {"true"}, "settings_snapshot": {strings.Replace(snapshotJSON, "s3cret", "other", 1)}}))
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("a duplicate was saved: %v", entries)
	}
	// A changed value is a new file.
	time.Sleep(1100 * time.Millisecond)
	s.saveRunSettings(form(url.Values{"save_settings": {"true"}, "settings_snapshot": {strings.Replace(snapshotJSON, `"30s"`, `"31s"`, 1)}}))
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Fatalf("a changed setting was not saved: %v", entries)
	}
	s.saveRunSettings(form(url.Values{"save_settings": {"true"}, "settings_snapshot": {"{"}}))
	s.mu.Lock()
	defer s.mu.Unlock()
	ev := s.LifecycleEvents
	if len(ev) != 4 || !strings.Contains(ev[0].Message, "saved to") || !strings.Contains(ev[1].Message, "no new file was saved") ||
		!strings.Contains(ev[2].Message, "saved to") || !strings.Contains(ev[3].Message, "not saved") {
		t.Errorf("events = %+v", ev)
	}
}
