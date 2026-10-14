package stats

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// trendStoreMaxBytes bounds the baseline file read at startup; a larger file
// is treated as corrupt and moved aside.
const trendStoreMaxBytes = 8 << 20

// TrendStore persists redacted per-shape baselines in a JSON file using the
// same layout as the definitions store: a user-config path that can be
// overridden by environment, atomic temp-file replace, and mode 0600. An empty
// path keeps baselines in memory only; DisabledTrendStore turns trends off.
type TrendStore struct {
	mu       sync.Mutex
	path     string
	disabled bool
	data     TrendStoreData
	status   string
	reason   string
}

// DefaultTrendStorePath returns the baseline file path, or "" with
// disabled=true when PLGM_TREND_BASELINE_STORE is "off".
func DefaultTrendStorePath() (path string, disabled bool) {
	if override := strings.TrimSpace(os.Getenv(trendStorePathEnv)); override != "" {
		if strings.EqualFold(override, trendStoreDisabledValue) {
			return "", true
		}
		return override, false
	}
	base, err := os.UserConfigDir()
	if err != nil || strings.TrimSpace(base) == "" {
		base = "."
	}
	return filepath.Join(base, "plgm", "trend_baselines.json"), false
}

// OpenDefaultTrendStore opens the store at DefaultTrendStorePath.
func OpenDefaultTrendStore() *TrendStore {
	path, disabled := DefaultTrendStorePath()
	if disabled {
		return DisabledTrendStore()
	}
	return OpenTrendStore(path)
}

// DisabledTrendStore never loads or records baselines.
func DisabledTrendStore() *TrendStore {
	return &TrendStore{disabled: true, status: "disabled", reason: trendStorePathEnv + "=off"}
}

// OpenTrendStore loads path. It never fails: an unreadable, oversized,
// corrupt, or unsupported file is moved aside (never deleted) and reported in
// the store status, and comparisons start without baselines.
func OpenTrendStore(path string) *TrendStore {
	s := &TrendStore{path: path, data: TrendStoreData{SchemaVersion: TrendStoreSchemaVersion}}
	s.load()
	return s
}

func (s *TrendStore) load() {
	s.status = "empty"
	if s.path == "" {
		s.reason = "memory_only"
		return
	}
	f, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		s.status, s.reason = "unavailable", "read failed: "+err.Error()
		return
	}
	b, err := io.ReadAll(io.LimitReader(f, trendStoreMaxBytes+1))
	f.Close()
	if err != nil {
		s.status, s.reason = "unavailable", "read failed: "+err.Error()
		return
	}
	if len(b) > trendStoreMaxBytes {
		s.moveAside("corrupt", fmt.Sprintf("file exceeds %d bytes", trendStoreMaxBytes))
		return
	}
	if strings.TrimSpace(string(b)) == "" {
		return
	}
	var head struct {
		SchemaVersion string `json:"schema_version"`
	}
	if err := json.Unmarshal(b, &head); err != nil {
		s.moveAside("corrupt", "invalid JSON")
		return
	}
	if head.SchemaVersion != TrendStoreSchemaVersion {
		s.moveAside("unsupported", fmt.Sprintf("schema %q is not %s", emptyFallback(head.SchemaVersion, "missing"), TrendStoreSchemaVersion))
		return
	}
	var d TrendStoreData
	if err := json.Unmarshal(b, &d); err != nil {
		s.moveAside("corrupt", "invalid baseline entries")
		return
	}
	s.data, s.reason = sanitizeTrendData(d)
	s.status = "ok"
	if len(s.data.Shapes) == 0 {
		s.status = "empty"
	}
}

// moveAside keeps an unusable file for inspection and starts empty.
func (s *TrendStore) moveAside(kind, why string) {
	dst := fmt.Sprintf("%s.%s-%s", s.path, kind, time.Now().UTC().Format("20060102T150405Z"))
	if err := os.Rename(s.path, dst); err != nil {
		s.status, s.reason = "unavailable", fmt.Sprintf("%s (%s) and could not be moved aside: %v", kind, why, err)
		return
	}
	s.status = kind + "_ignored"
	s.reason = fmt.Sprintf("%s; moved to %s", why, filepath.Base(dst))
}

// sanitizeTrendData drops entries that cannot be compared safely and
// re-applies the text and count limits, so a hand-edited file stays bounded.
func sanitizeTrendData(d TrendStoreData) (TrendStoreData, string) {
	dropped := 0
	validTime := func(t string) bool {
		_, err := time.Parse(time.RFC3339, t)
		return err == nil
	}
	shapes := make([]TrendShapeBaseline, 0, len(d.Shapes))
	for _, sh := range d.Shapes {
		if sh.ShapeID == "" || sh.Namespace == "" {
			dropped++
			continue
		}
		sh.ShapeID, sh.Namespace = trendText(sh.ShapeID, 64), trendText(sh.Namespace, trendMaxText)
		sh.Operation, sh.ShapeSummary = trendText(sh.Operation, 32), trendText(sh.ShapeSummary, trendMaxText)
		if e := sh.Efficiency; e != nil {
			if e.RunID == "" || !validTime(e.RecordedAt) {
				sh.Efficiency, dropped = nil, dropped+1
			} else {
				e.ScoreVersion = CanonicalScoreVersion(e.ScoreVersion)
				e.Reason = trendText(e.Reason, 128)
				e.PlanSignatures = boundTexts(e.PlanSignatures, trendMaxPlans, trendMaxText)
				e.IndexesUsed = boundTexts(e.IndexesUsed, trendMaxIndexesUsed, 128)
				e.NamespaceIndexes = boundTexts(e.NamespaceIndexes, trendMaxNamespaceIndexes, 128)
			}
		}
		if w := sh.Workload; w != nil && (w.RunID == "" || !validTime(w.RecordedAt)) {
			sh.Workload, dropped = nil, dropped+1
		}
		if sh.Efficiency == nil && sh.Workload == nil {
			continue
		}
		shapes = append(shapes, sh)
	}
	runs := make([]TrendRunContext, 0, len(d.Runs))
	for _, r := range d.Runs {
		if r.RunID == "" || !validTime(r.RecordedAt) {
			dropped++
			continue
		}
		r.ScoreVersion = CanonicalScoreVersion(r.ScoreVersion)
		r.Server.Versions = boundTexts(r.Server.Versions, trendMaxServerVersions, 32)
		runs = append(runs, r)
	}
	out := pruneTrendData(TrendStoreData{SchemaVersion: TrendStoreSchemaVersion, Runs: runs, Shapes: shapes})
	if dropped > 0 {
		return out, fmt.Sprintf("%d invalid baseline entries were ignored", dropped)
	}
	return out, ""
}

func boundTexts(in []string, maxItems, maxLen int) []string {
	if len(in) > maxItems {
		in = in[:maxItems]
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, trendText(s, maxLen))
	}
	return out
}

// Snapshot returns a copy of the stored baselines.
func (s *TrendStore) Snapshot() TrendStoreData {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, _ := json.Marshal(s.data)
	var out TrendStoreData
	_ = json.Unmarshal(b, &out)
	return out
}

// Status describes the store for the trend view and API.
func (s *TrendStore) Status() TrendStoreStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := TrendStoreStatus{
		Status: s.status, Reason: s.reason, Persistent: s.path != "" && !s.disabled,
		Baselines: len(s.data.Shapes), MaxBaselines: TrendMaxShapeBaselines, Runs: len(s.data.Runs),
	}
	for _, sh := range s.data.Shapes {
		for _, t := range []string{snapshotTime(sh.Efficiency), workloadTime(sh.Workload)} {
			if t == "" {
				continue
			}
			if st.OldestRecordedAt == "" || t < st.OldestRecordedAt {
				st.OldestRecordedAt = t
			}
			if t > st.NewestRecordedAt {
				st.NewestRecordedAt = t
			}
		}
	}
	return st
}

func snapshotTime(e *TrendEfficiencySnapshot) string {
	if e == nil {
		return ""
	}
	return e.RecordedAt
}

func workloadTime(w *TrendWorkloadSnapshot) string {
	if w == nil {
		return ""
	}
	return w.RecordedAt
}

// Record merges this run's summaries into the store and persists them.
func (s *TrendStore) Record(run TrendRunContext, cur []TrendShapeBaseline) error {
	if s == nil || s.disabled || len(cur) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := MergeTrendBaselines(s.data, run, cur)
	if err := s.saveLocked(next); err != nil {
		s.status, s.reason = "unavailable", err.Error()
		return err
	}
	s.data, s.status, s.reason = next, "ok", ""
	return nil
}

// Reset removes every baseline and the baseline file. Moved-aside corrupt
// files are kept.
func (s *TrendStore) Reset() error {
	if s == nil || s.disabled {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path != "" {
		if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove trend baselines: %w", err)
		}
	}
	s.data = TrendStoreData{SchemaVersion: TrendStoreSchemaVersion}
	s.status, s.reason = "empty", "reset at "+time.Now().UTC().Format(time.RFC3339)
	return nil
}

func (s *TrendStore) saveLocked(d TrendStoreData) error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("create trend baseline directory: %w", err)
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("encode trend baselines: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("write trend baselines: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("replace trend baselines: %w", err)
	}
	return nil
}
