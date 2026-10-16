package stats

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
)

// trendStoreMaxBytes bounds the baseline file and an imported file. A larger
// stored file is treated as corrupt and moved aside; a save that would exceed
// it drops the oldest unpinned runs first (see boundTrendSize).
const trendStoreMaxBytes = 32 << 20

// TrendFileMaxBytes is the largest baseline or import file accepted.
const TrendFileMaxBytes = trendStoreMaxBytes

// ValidTrendRunID reports whether id has the stored run id form.
func ValidTrendRunID(id string) bool { return trendRunIDPattern.MatchString(id) }

var (
	// ErrTrendStoreDisabled is returned by run edits, export, and import when trends are off.
	ErrTrendStoreDisabled = errors.New("trend baseline store is disabled")
	// ErrTrendPinLimit is returned when pinning would exceed TrendMaxPinnedRuns.
	ErrTrendPinLimit = fmt.Errorf("at most %d runs can be pinned; unpin one first", TrendMaxPinnedRuns)

	trendRunIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,79}$`)

	// trendLockWait and trendLockStaleAfter are variables so tests can shorten them.
	trendLockWait       = 10 * time.Second
	trendLockStaleAfter = 30 * time.Second
)

// TrendImportError is an import file that was rejected; nothing was changed.
type TrendImportError struct{ Msg string }

func (e *TrendImportError) Error() string { return "trend import rejected: " + e.Msg }

// TrendStore persists redacted per-shape snapshot histories in a JSON file
// using the same layout as the definitions store: a user-config path that can
// be overridden by config or environment and mode 0600. Several processes can
// share one file: every write takes a lock file, re-reads the file, applies
// its change to what is on disk, and replaces the file atomically. An empty
// path keeps baselines in memory only; DisabledTrendStore turns trends off.
type TrendStore struct {
	mu       sync.Mutex
	path     string
	disabled bool
	data     TrendStoreData
	status   string
	reason   string
	modTime  time.Time
	size     int64
}

// DefaultTrendStorePath returns the baseline file path, or "" with
// disabled=true when PLGM_TREND_BASELINE_STORE is "off".
func DefaultTrendStorePath() (path string, disabled bool) {
	return ResolveTrendStorePath(os.Getenv(trendStorePathEnv))
}

// ResolveTrendStorePath maps a trend_baseline_store setting to a path: empty
// is the default location next to definitions.json, "off" disables trends.
func ResolveTrendStorePath(setting string) (path string, disabled bool) {
	if override := strings.TrimSpace(setting); override != "" {
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
	return OpenTrendStoreSetting(os.Getenv(trendStorePathEnv))
}

// OpenTrendStoreSetting opens the store for a trend_baseline_store setting.
func OpenTrendStoreSetting(setting string) *TrendStore {
	path, disabled := ResolveTrendStorePath(setting)
	if disabled {
		return DisabledTrendStore()
	}
	return OpenTrendStore(path)
}

// DisabledTrendStore never loads or records baselines.
func DisabledTrendStore() *TrendStore {
	return &TrendStore{disabled: true, status: "disabled", reason: "trend_baseline_store=off (" + trendStorePathEnv + ")"}
}

// OpenTrendStore loads path. It never fails: an unreadable, oversized,
// corrupt, or unsupported file is moved aside (never deleted) and reported in
// the store status, and comparisons start without baselines. A tb-1 file is
// migrated in memory and rewritten as tb-2 on the next save.
func OpenTrendStore(path string) *TrendStore {
	s := &TrendStore{path: path, data: emptyTrendData()}
	s.load()
	return s
}

func emptyTrendData() TrendStoreData {
	return TrendStoreData{SchemaVersion: TrendStoreSchemaVersion, Runs: []TrendRunContext{}, Shapes: []TrendShapeBaseline{}}
}

// Path is the file path, "" for a memory-only or disabled store.
func (s *TrendStore) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Disabled reports whether trends are off.
func (s *TrendStore) Disabled() bool { return s == nil || s.disabled }

func (s *TrendStore) load() {
	s.status = "empty"
	if s.path == "" {
		s.reason = "memory_only"
		return
	}
	s.data, s.status, s.reason = readTrendFile(s.path)
	s.statFileLocked()
}

func (s *TrendStore) statFileLocked() {
	s.modTime, s.size = time.Time{}, 0
	if fi, err := os.Stat(s.path); err == nil {
		s.modTime, s.size = fi.ModTime(), fi.Size()
	}
}

// refreshLocked reloads the file when another process changed it.
func (s *TrendStore) refreshLocked() {
	if s.path == "" || s.disabled {
		return
	}
	fi, err := os.Stat(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if !s.modTime.IsZero() {
			s.data, s.status, s.reason = emptyTrendData(), "empty", "the baseline file was removed by another process"
			s.modTime, s.size = time.Time{}, 0
		}
		return
	case err != nil:
		return
	case fi.ModTime().Equal(s.modTime) && fi.Size() == s.size:
		return
	}
	s.data, s.status, s.reason = readTrendFile(s.path)
	s.statFileLocked()
}

// readTrendFile loads path; an unusable file is moved aside.
func readTrendFile(path string) (TrendStoreData, string, string) {
	empty := emptyTrendData()
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return empty, "empty", ""
	}
	if err != nil {
		return empty, "unavailable", "read failed: " + err.Error()
	}
	b, err := io.ReadAll(io.LimitReader(f, trendStoreMaxBytes+1))
	f.Close()
	if err != nil {
		return empty, "unavailable", "read failed: " + err.Error()
	}
	if len(b) > trendStoreMaxBytes {
		st, why := moveTrendFileAside(path, "corrupt", fmt.Sprintf("file exceeds %d bytes", trendStoreMaxBytes))
		return empty, st, why
	}
	if strings.TrimSpace(string(b)) == "" {
		return empty, "empty", ""
	}
	d, note, perr := parseTrendStoreFile(b)
	if perr != nil {
		st, why := moveTrendFileAside(path, perr.kind, perr.why)
		return empty, st, why
	}
	status := "ok"
	if len(d.Shapes) == 0 {
		status = "empty"
	}
	return d, status, note
}

type trendParseError struct{ kind, why string }

// legacyTrendShape is a tb-1 shape: one efficiency and one workload snapshot.
type legacyTrendShape struct {
	ShapeID      string                   `json:"shape_id"`
	Namespace    string                   `json:"namespace"`
	Operation    string                   `json:"operation"`
	ShapeSummary string                   `json:"shape_summary,omitempty"`
	Efficiency   *TrendEfficiencySnapshot `json:"efficiency,omitempty"`
	Workload     *TrendWorkloadSnapshot   `json:"workload,omitempty"`
}

type legacyTrendStoreData struct {
	SchemaVersion string             `json:"schema_version"`
	Runs          []TrendRunContext  `json:"runs"`
	Shapes        []legacyTrendShape `json:"shapes"`
}

// parseTrendStoreFile decodes a tb-2 file, or migrates a tb-1 file so each
// stored snapshot becomes the first history entry of its shape.
func parseTrendStoreFile(b []byte) (TrendStoreData, string, *trendParseError) {
	var head struct {
		SchemaVersion string `json:"schema_version"`
	}
	if err := json.Unmarshal(b, &head); err != nil {
		return TrendStoreData{}, "", &trendParseError{"corrupt", "invalid JSON"}
	}
	var d TrendStoreData
	var notes []string
	switch head.SchemaVersion {
	case TrendStoreSchemaVersion:
		if err := json.Unmarshal(b, &d); err != nil {
			return TrendStoreData{}, "", &trendParseError{"corrupt", "invalid baseline entries"}
		}
	case trendLegacySchemaVersion:
		var old legacyTrendStoreData
		if err := json.Unmarshal(b, &old); err != nil {
			return TrendStoreData{}, "", &trendParseError{"corrupt", "invalid baseline entries"}
		}
		d = migrateLegacyTrendData(old)
		notes = append(notes, fmt.Sprintf("migrated from %s; the file is rewritten as %s on the next save", trendLegacySchemaVersion, TrendStoreSchemaVersion))
	default:
		return TrendStoreData{}, "", &trendParseError{"unsupported", fmt.Sprintf("schema %q is not %s or %s", emptyFallback(head.SchemaVersion, "missing"), TrendStoreSchemaVersion, trendLegacySchemaVersion)}
	}
	d, dropped := sanitizeTrendData(d)
	if dropped != "" {
		notes = append(notes, dropped)
	}
	return d, strings.Join(notes, "; "), nil
}

func migrateLegacyTrendData(old legacyTrendStoreData) TrendStoreData {
	d := TrendStoreData{SchemaVersion: TrendStoreSchemaVersion, Runs: old.Runs}
	for _, s := range old.Shapes {
		sh := TrendShapeBaseline{ShapeID: s.ShapeID, Namespace: s.Namespace, Operation: s.Operation, ShapeSummary: s.ShapeSummary}
		if s.Efficiency != nil {
			sh.EfficiencyHistory = []TrendEfficiencySnapshot{*s.Efficiency}
		}
		if s.Workload != nil {
			sh.WorkloadHistory = []TrendWorkloadSnapshot{*s.Workload}
		}
		d.Shapes = append(d.Shapes, sh)
	}
	return d
}

// moveTrendFileAside keeps an unusable file for inspection.
func moveTrendFileAside(path, kind, why string) (string, string) {
	dst := fmt.Sprintf("%s.%s-%s", path, kind, time.Now().UTC().Format("20060102T150405.000000000Z"))
	if err := os.Rename(path, dst); err != nil {
		return "unavailable", fmt.Sprintf("%s (%s) and could not be moved aside: %v", kind, why, err)
	}
	return kind + "_ignored", fmt.Sprintf("%s; moved to %s", why, filepath.Base(dst))
}

// sanitizeTrendData drops entries that cannot be compared safely and
// re-applies the text and count limits, so a hand-edited or imported file
// stays bounded. Only the hard limits apply here (50 unpinned runs, 10
// pinned); a save applies the configured history setting.
func sanitizeTrendData(d TrendStoreData) (TrendStoreData, string) {
	dropped := 0
	validTime := func(t string) bool {
		_, err := time.Parse(time.RFC3339, t)
		return err == nil
	}
	validRun := func(id, at string) bool { return trendRunIDPattern.MatchString(id) && validTime(at) }
	shapes := make([]TrendShapeBaseline, 0, len(d.Shapes))
	for _, sh := range d.Shapes {
		if sh.ShapeID == "" || sh.Namespace == "" {
			dropped++
			continue
		}
		sh.ShapeID, sh.Namespace = trendText(sh.ShapeID, 64), trendText(sh.Namespace, trendMaxText)
		sh.Operation, sh.ShapeSummary = trendText(sh.Operation, 32), trendText(sh.ShapeSummary, trendMaxText)
		eff := make([]TrendEfficiencySnapshot, 0, len(sh.EfficiencyHistory))
		for _, e := range sh.EfficiencyHistory {
			if !validRun(e.RunID, e.RecordedAt) {
				dropped++
				continue
			}
			e.ScoreVersion = CanonicalScoreVersion(trendText(e.ScoreVersion, 16))
			e.ShapeIdentityVersion = trendText(e.ShapeIdentityVersion, 16)
			e.Coverage, e.Status, e.Band = trendText(e.Coverage, 32), trendText(e.Status, 32), trendText(e.Band, 32)
			e.Confidence, e.Evidence, e.LimitingDimension = trendText(e.Confidence, 32), trendText(e.Evidence, 32), trendText(e.LimitingDimension, 64)
			e.NamespaceIndexStatus = trendText(e.NamespaceIndexStatus, 32)
			e.Reason = trendText(e.Reason, 128)
			e.PlanSignatures = boundTexts(e.PlanSignatures, trendMaxPlans, trendMaxText)
			e.IndexesUsed = boundTexts(e.IndexesUsed, trendMaxIndexesUsed, 128)
			e.NamespaceIndexes = boundTexts(e.NamespaceIndexes, trendMaxNamespaceIndexes, 128)
			eff = append(eff, e)
		}
		wl := make([]TrendWorkloadSnapshot, 0, len(sh.WorkloadHistory))
		for _, w := range sh.WorkloadHistory {
			if !validRun(w.RunID, w.RecordedAt) {
				dropped++
				continue
			}
			w.ShapeIdentityVersion = trendText(w.ShapeIdentityVersion, 16)
			wl = append(wl, w)
		}
		sh.EfficiencyHistory, sh.WorkloadHistory = eff, wl
		if len(eff) == 0 && len(wl) == 0 {
			continue
		}
		if len(sh.QueryValuesHistory) > 0 {
			vals := make([]TrendQueryValues, 0, len(sh.QueryValuesHistory))
			for _, v := range sh.QueryValuesHistory {
				if !validRun(v.RunID, v.RecordedAt) {
					dropped++
					continue
				}
				vals = append(vals, sanitizeTrendQueryValues(v))
			}
			sh.QueryValuesHistory = vals
		}
		shapes = append(shapes, sh)
	}
	runs := make([]TrendRunContext, 0, len(d.Runs))
	for _, r := range d.Runs {
		if !validRun(r.RunID, r.RecordedAt) {
			dropped++
			continue
		}
		runs = append(runs, sanitizeTrendRun(r))
	}
	out := pruneTrendData(TrendStoreData{SchemaVersion: TrendStoreSchemaVersion, Runs: runs, Shapes: shapes}, TrendPolicy{KeepRuns: config.MaxTrendHistoryRuns})
	if dropped > 0 {
		return out, fmt.Sprintf("%d invalid baseline entries were ignored", dropped)
	}
	return out, ""
}

func sanitizeTrendRun(r TrendRunContext) TrendRunContext {
	r.ScoreVersion = CanonicalScoreVersion(trendText(r.ScoreVersion, 16))
	r.ShapeIdentityVersion = trendText(r.ShapeIdentityVersion, 16)
	r.Server.Versions = boundTexts(r.Server.Versions, trendMaxServerVersions, 32)
	r.Server.Topology, r.Server.Source = trendText(r.Server.Topology, 32), trendText(r.Server.Source, 32)
	r.Label = TrendRunLabel(r.Label, r.Server, r.RecordedAt)
	r.Source = trendText(config.SanitizeTrendRunLabel(r.Source), trendMaxSource)
	if _, err := time.Parse(time.RFC3339, r.ImportedAt); err != nil {
		r.ImportedAt = ""
	}
	s := &r.Sampling
	s.ExplainVerbosity, s.ExplainEfficiencyMode, s.CaptureStatus = trendText(s.ExplainVerbosity, 32), trendText(s.ExplainEfficiencyMode, 64), trendText(s.CaptureStatus, 32)
	w := &r.Workload
	w.Duration, w.OperationMix = trendText(w.Duration, 32), trendText(w.OperationMix, trendMaxText)
	w.LoadProfile, w.AccessPattern = trendText(w.LoadProfile, 32), trendText(w.AccessPattern, 32)
	if !trendHashPattern.MatchString(w.ReplayHeaderHash) {
		w.ReplayHeaderHash = ""
	}
	if !trendHashPattern.MatchString(w.GeneratedTrafficFingerprint) {
		w.GeneratedTrafficFingerprint = ""
	}
	return r
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

func cloneTrendData(d TrendStoreData) TrendStoreData {
	b, _ := json.Marshal(d)
	var out TrendStoreData
	_ = json.Unmarshal(b, &out)
	if out.Runs == nil {
		out.Runs = []TrendRunContext{}
	}
	if out.Shapes == nil {
		out.Shapes = []TrendShapeBaseline{}
	}
	out.SchemaVersion = TrendStoreSchemaVersion
	return out
}

// Snapshot returns a copy of the stored baselines, reloaded if another
// process changed the file.
func (s *TrendStore) Snapshot() TrendStoreData {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	return cloneTrendData(s.data)
}

// Status describes the store for the trend view and API.
func (s *TrendStore) Status() TrendStoreStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	return s.statusLocked()
}

func (s *TrendStore) statusLocked() TrendStoreStatus {
	st := TrendStoreStatus{
		Status: s.status, Reason: s.reason, Persistent: s.path != "" && !s.disabled, Path: s.path,
		Baselines: len(s.data.Shapes), MaxBaselines: TrendMaxShapeBaselines, Runs: len(s.data.Runs), MaxPinnedRuns: TrendMaxPinnedRuns,
	}
	for _, r := range s.data.Runs {
		if r.Pinned {
			st.PinnedRuns++
		}
	}
	note := func(t string) {
		if st.OldestRecordedAt == "" || t < st.OldestRecordedAt {
			st.OldestRecordedAt = t
		}
		if t > st.NewestRecordedAt {
			st.NewestRecordedAt = t
		}
	}
	for _, sh := range s.data.Shapes {
		for _, e := range sh.EfficiencyHistory {
			note(e.RecordedAt)
		}
		for _, w := range sh.WorkloadHistory {
			note(w.RecordedAt)
		}
	}
	return st
}

// Runs lists the stored runs, newest first.
func (s *TrendStore) Runs(p TrendPolicy, now time.Time) []TrendRunSummary {
	if s == nil || s.disabled {
		return []TrendRunSummary{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	return trendRunSummaries(s.data, "", now, p)
}

// mutate applies fn to the latest stored data and saves the result. For a
// file store it holds the lock file while it re-reads the file, so changes
// written by another process in between are kept.
func (s *TrendStore) mutate(fn func(TrendStoreData) (TrendStoreData, error)) error {
	if s == nil || s.disabled {
		return ErrTrendStoreDisabled
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" {
		next, err := fn(cloneTrendData(s.data))
		if err != nil {
			return err
		}
		next, s.reason = boundTrendSize(next, trendStoreMaxBytes)
		s.data, s.status = next, trendDataStatus(next)
		if s.reason == "" {
			s.reason = "memory_only"
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("create trend baseline directory: %w", err)
	}
	unlock, err := lockTrendFile(s.path)
	if err != nil {
		return err
	}
	defer unlock()
	disk, st, reason := readTrendFile(s.path)
	if st == "unavailable" {
		s.status, s.reason = st, reason
		return errors.New(reason)
	}
	s.data, s.status, s.reason = disk, st, reason
	s.statFileLocked()
	next, err := fn(cloneTrendData(disk))
	if err != nil {
		return err
	}
	next, trimmed := boundTrendSize(next, trendStoreMaxBytes)
	if err := writeTrendFile(s.path, next); err != nil {
		s.status, s.reason = "unavailable", err.Error()
		return err
	}
	s.data, s.status, s.reason = next, trendDataStatus(next), trimmed
	s.statFileLocked()
	return nil
}

func trendDataStatus(d TrendStoreData) string {
	if len(d.Shapes) == 0 && len(d.Runs) == 0 {
		return "empty"
	}
	return "ok"
}

// boundTrendSize keeps the encoded data within maxBytes. It first removes
// stored query values, oldest run first (never the newest run's), and marks
// those runs; then it drops the oldest unpinned runs (never the newest run),
// then the least recently recorded shapes. Without query values this only
// triggers for unusually large index inventories.
func boundTrendSize(d TrendStoreData, maxBytes int) (TrendStoreData, string) {
	b, _ := json.Marshal(d)
	runs, shapes, valueRuns := 0, 0, 0
	if len(b) > maxBytes && TrendDataHasQueryValues(d) {
		ordered := append([]TrendRunContext(nil), d.Runs...)
		sortRunsNewestFirst(ordered)
		withValues := map[string]bool{}
		for _, sh := range d.Shapes {
			for _, v := range sh.QueryValuesHistory {
				withValues[v.RunID] = true
			}
		}
		for i := len(ordered) - 1; i > 0 && len(b) > maxBytes; i-- {
			if withValues[ordered[i].RunID] {
				d = stripTrendRunValues(d, ordered[i].RunID)
				valueRuns++
				b, _ = json.Marshal(d)
			}
		}
	}
	for len(b) > maxBytes {
		ordered := append([]TrendRunContext(nil), d.Runs...)
		sortRunsNewestFirst(ordered)
		victim := ""
		for i := len(ordered) - 1; i > 0; i-- {
			if !ordered[i].Pinned {
				victim = ordered[i].RunID
				break
			}
		}
		switch {
		case victim != "":
			d = removeTrendRun(d, victim)
			runs++
		case len(d.Shapes) > 1:
			n := len(d.Shapes) * 9 / 10
			shapes += len(d.Shapes) - n
			d.Shapes = d.Shapes[:n]
		default:
			return d, ""
		}
		b, _ = json.Marshal(d)
	}
	if runs == 0 && shapes == 0 && valueRuns == 0 {
		return d, ""
	}
	if valueRuns > 0 {
		return d, fmt.Sprintf("store size limit of %d bytes reached: removed the stored query values of %d oldest runs, dropped %d oldest unpinned runs and %d least recently recorded shapes", maxBytes, valueRuns, runs, shapes)
	}
	return d, fmt.Sprintf("store size limit of %d bytes reached: dropped %d oldest unpinned runs and %d least recently recorded shapes", maxBytes, runs, shapes)
}

// lockTrendFile creates path.lock exclusively, waiting for another writer.
// A lock older than trendLockStaleAfter is treated as left by a crashed process.
func lockTrendFile(path string) (func(), error) {
	lock := path + ".lock"
	deadline := time.Now().Add(trendLockWait)
	for {
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintf(f, "pid %d at %s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
			f.Close()
			return func() { _ = os.Remove(lock) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("lock trend baselines: %w", err)
		}
		if fi, serr := os.Stat(lock); serr == nil && time.Since(fi.ModTime()) > trendLockStaleAfter {
			_ = os.Remove(lock)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("trend baselines are locked by another process (%s); remove the lock file if no PLGM instance is writing", lock)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// writeTrendFile replaces path atomically through a unique temp file.
func writeTrendFile(path string, d TrendStoreData) error {
	b, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("encode trend baselines: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("write trend baselines: %w", err)
	}
	name := tmp.Name()
	_, werr := tmp.Write(append(b, '\n'))
	if werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(name, 0o600)
	}
	if werr != nil {
		_ = os.Remove(name)
		return fmt.Errorf("write trend baselines: %w", werr)
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("replace trend baselines: %w", err)
	}
	return nil
}

// Record adds this run's snapshots to the store with policy p.
func (s *TrendStore) Record(run TrendRunContext, cur []TrendShapeSnapshot, p TrendPolicy) error {
	if s == nil || s.disabled || len(cur) == 0 {
		return nil
	}
	return s.mutate(func(d TrendStoreData) (TrendStoreData, error) {
		return MergeTrendBaselines(d, run, cur, p), nil
	})
}

// UpdateRun renames and/or pins a run. An empty label restores the default.
// Changes take effect without pruning; an unpinned run counts toward the
// history limit again at the next save.
func (s *TrendStore) UpdateRun(id string, label *string, pinned *bool, p TrendPolicy, now time.Time) (TrendRunSummary, error) {
	var out TrendRunSummary
	err := s.mutate(func(d TrendStoreData) (TrendStoreData, error) {
		i := trendRunIndex(d, id)
		if i < 0 {
			return d, fmt.Errorf("%w: %s", ErrTrendRunNotFound, id)
		}
		r := &d.Runs[i]
		if label != nil {
			r.Label = TrendRunLabel(*label, r.Server, r.RecordedAt)
		}
		if pinned != nil && *pinned && !r.Pinned {
			n := 0
			for _, x := range d.Runs {
				if x.Pinned {
					n++
				}
			}
			if n >= TrendMaxPinnedRuns {
				return d, ErrTrendPinLimit
			}
		}
		if pinned != nil {
			r.Pinned = *pinned
		}
		for _, x := range trendRunSummaries(d, "", now, p) {
			if x.RunID == id {
				out = x
			}
		}
		return d, nil
	})
	return out, err
}

// DeleteRun removes a run and its snapshots.
func (s *TrendStore) DeleteRun(id string) error {
	return s.mutate(func(d TrendStoreData) (TrendStoreData, error) {
		if trendRunIndex(d, id) < 0 {
			return d, fmt.Errorf("%w: %s", ErrTrendRunNotFound, id)
		}
		return removeTrendRun(d, id), nil
	})
}

func trendRunIndex(d TrendStoreData, id string) int {
	for i, r := range d.Runs {
		if r.RunID == id {
			return i
		}
	}
	return -1
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
		if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
			return fmt.Errorf("remove trend baselines: %w", err)
		}
		unlock, err := lockTrendFile(s.path)
		if err != nil {
			return err
		}
		defer unlock()
		if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove trend baselines: %w", err)
		}
	}
	s.data = emptyTrendData()
	s.status, s.reason = "empty", "reset at "+time.Now().UTC().Format(time.RFC3339)
	s.modTime, s.size = time.Time{}, 0
	return nil
}
