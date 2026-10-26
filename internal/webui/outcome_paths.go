package webui

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/multitarget"
)

// maxOutcomeNameAttempts bounds the retries when a generated file name is
// already taken.
const maxOutcomeNameAttempts = 8

const (
	fieldReplayOutcomes        = "workload_replay_outcomes_path"
	fieldDeterministicOutcomes = "workload_deterministic_outcomes_path"
	fieldRecordPath            = "workload_record_path"
)

var (
	outcomeNameClock  = time.Now
	outcomeNameSuffix = func() string {
		var b [4]byte
		_, _ = rand.Read(b[:])
		return hex.EncodeToString(b[:])
	}
)

// generatedFile describes a file name the server chose for a run because
// its setting was blank: an outcome file or the recording.
type generatedFile struct {
	// Field is the setting that was blank and filled in.
	Field string
	// Path is the configured value; Files are the files the run writes
	// (one per target for a multi-target run), reserved empty with mode 0600.
	Path  string
	Files []string
}

// outcomePathToGenerate names the outcome path setting a run needs but left
// blank: Record Outcomes during a replay, or in a deterministic run without
// a recording (a multi-target run never records). "" when none.
func outcomePathToGenerate(cfg *config.AppConfig) string {
	if cfg == nil || !cfg.WorkloadRecordOutcomes {
		return ""
	}
	switch {
	case cfg.WorkloadReplayEnabled:
		if strings.TrimSpace(cfg.WorkloadReplayOutcomesPath) == "" {
			return fieldReplayOutcomes
		}
	case cfg.WorkloadDeterministic:
		if strings.TrimSpace(cfg.WorkloadDeterministicOutcomesPath) == "" && (!cfg.WorkloadRecordEnabled || multitarget.Enabled(cfg)) {
			return fieldDeterministicOutcomes
		}
	}
	return ""
}

// recordPathToGenerate reports whether a recording run left its file name
// blank, so it gets a new name instead of the fixed default.
func recordPathToGenerate(cfg *config.AppConfig) bool {
	return cfg != nil && cfg.WorkloadRecordEnabled && strings.TrimSpace(cfg.WorkloadRecordPath) == ""
}

// pendingOutcomePath and pendingRecordPath stand in for generated paths
// while the settings are validated; the files are reserved only after
// validation. They differ so the outcome checks do not see them as one file.
const (
	pendingOutcomePath = "plgm_outcomes.pending.ndjson"
	pendingRecordPath  = "plgm_workload_record.pending.ndjson"
)

// setOutcomePath sets the outcome path setting named field.
func setOutcomePath(cfg *config.AppConfig, field, path string) {
	if field == fieldDeterministicOutcomes {
		cfg.WorkloadDeterministicOutcomesPath = path
	} else {
		cfg.WorkloadReplayOutcomesPath = path
	}
}

// newGeneratedName is <prefix>-<UTC time>-<random><ext> in dir.
func newGeneratedName(dir, prefix, ext string) string {
	name := fmt.Sprintf("%s-%s-%s%s", prefix, outcomeNameClock().UTC().Format("20060102T150405Z"), outcomeNameSuffix(), ext)
	if dir == "" || dir == "." {
		return name
	}
	return filepath.Join(dir, name)
}

// createExclusive creates path empty with mode 0600; it fails if the path
// exists, so a generated name never replaces a file.
func createExclusive(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

// reserveGenerated picks an unused name in dir and creates every file the
// run writes (files maps the name to them) exclusively. A taken name is
// replaced by a new one.
func reserveGenerated(dir, prefix, ext, field, what string, files func(base string) []string) (*generatedFile, error) {
	for attempt := 0; attempt < maxOutcomeNameAttempts; attempt++ {
		base := newGeneratedName(dir, prefix, ext)
		paths := files(base)
		var created []string
		var err error
		for _, f := range paths {
			if err = createExclusive(f); err != nil {
				break
			}
			created = append(created, f)
		}
		if err == nil {
			return &generatedFile{Field: field, Path: base, Files: paths}, nil
		}
		removeEmptyFiles(created)
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("cannot create %s in %s on the PLGM server: %w; set %s to a writable path", what, displayDir(dir), err, field)
		}
	}
	return nil, fmt.Errorf("could not find an unused %s name in %s; set %s", what, displayDir(dir), field)
}

// assignOutcomePaths fills a blank outcome path that the run needs with a
// unique name in dir and reserves every file the run will write (the
// per-target variants for a multi-target run). The name is chosen once per
// run. It returns nil when the configured paths are used as they are.
func assignOutcomePaths(cfg *config.AppConfig, dir string) (*generatedFile, error) {
	field := outcomePathToGenerate(cfg)
	if field == "" {
		return nil, nil
	}
	kind := "replay"
	if field == fieldDeterministicOutcomes {
		kind = "deterministic"
	}
	g, err := reserveGenerated(dir, "plgm_"+kind+"_outcomes", ".ndjson", field, "an outcome file", func(base string) []string {
		if !multitarget.Enabled(cfg) {
			return []string{base}
		}
		files := make([]string, 0, len(cfg.WorkloadReplayTargets))
		for _, t := range cfg.WorkloadReplayTargets {
			files = append(files, config.TargetPath(base, strings.TrimSpace(t.Label)))
		}
		return files
	})
	if err != nil {
		return nil, err
	}
	setOutcomePath(cfg, field, g.Path)
	return g, nil
}

// assignRecordPath gives a recording run with a blank file name a new file
// in dir, plgm_workload_record-<UTC time>-<random>.ndjson, with .zst or .gz
// added for a compressed recording, so a later run never replaces it.
func assignRecordPath(cfg *config.AppConfig, dir string) (*generatedFile, error) {
	if !recordPathToGenerate(cfg) {
		return nil, nil
	}
	ext := ".ndjson"
	switch strings.ToLower(strings.TrimSpace(cfg.WorkloadRecordCompression)) {
	case config.WorkloadRecordCompressionZstd:
		ext += ".zst"
	case config.WorkloadRecordCompressionGzip:
		ext += ".gz"
	}
	g, err := reserveGenerated(dir, "plgm_workload_record", ext, fieldRecordPath, "a recording file", func(base string) []string { return []string{base} })
	if err != nil {
		return nil, err
	}
	cfg.WorkloadRecordPath = g.Path
	return g, nil
}

func displayDir(dir string) string { return absPath(dir) }

// removeEmptyFiles deletes reserved files that a run never wrote.
func removeEmptyFiles(paths []string) {
	for _, p := range paths {
		if fi, err := os.Lstat(p); err == nil && fi.Mode().IsRegular() && fi.Size() == 0 {
			_ = os.Remove(p)
		}
	}
}

// generatedPaths lists every file reserved for a run.
func generatedPaths(gs []*generatedFile) []string {
	var out []string
	for _, g := range gs {
		out = append(out, g.Files...)
	}
	return out
}

// note is the lifecycle message naming the generated files.
func (g *generatedFile) note() string {
	abs := make([]string, len(g.Files))
	for i, f := range g.Files {
		abs[i] = absPath(f)
	}
	what := "Outcome file"
	if g.Field == fieldRecordPath {
		what = "Recording file"
	}
	return what + " (generated because the path was blank): " + strings.Join(abs, ", ")
}

// startedResponse is the /api/start reply; it names generated files.
func startedResponse(gs []*generatedFile) map[string]interface{} {
	out := map[string]interface{}{"status": "started"}
	for _, g := range gs {
		if g.Field == fieldRecordPath {
			out["recording_file"] = g.Path
			continue
		}
		out["outcome_files"] = g.Files
		out["outcome_files_generated"] = true
	}
	return out
}

// runOutcomeFile is the absolute outcome file a single-target run writes,
// or "".
func runOutcomeFile(cfg *config.AppConfig) string {
	if cfg == nil || !cfg.WorkloadRecordOutcomes {
		return ""
	}
	p := ""
	if cfg.WorkloadReplayEnabled {
		p = strings.TrimSpace(cfg.WorkloadReplayOutcomesPath)
	} else if cfg.WorkloadDeterministic {
		p = strings.TrimSpace(cfg.WorkloadDeterministicOutcomesPath)
	}
	return absPath(p)
}

// runRecordingFile is the absolute recording a record run writes, or "".
func runRecordingFile(cfg *config.AppConfig) string {
	if cfg == nil || !cfg.WorkloadRecordEnabled {
		return ""
	}
	return absPath(strings.TrimSpace(cfg.WorkloadRecordPath))
}

// absPath is p made absolute on the server; "" stays "".
func absPath(p string) string {
	if p == "" {
		return ""
	}
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// forgetGeneratedPaths blanks the paths the server generated for the last
// run, so the form and later runs see those settings as the user left them:
// blank, and the next run gets new names.
func forgetGeneratedPaths(cfg *config.AppConfig, gs []*generatedFile) {
	if cfg == nil {
		return
	}
	for _, g := range gs {
		switch {
		case g.Field == fieldReplayOutcomes && cfg.WorkloadReplayOutcomesPath == g.Path:
			cfg.WorkloadReplayOutcomesPath = ""
		case g.Field == fieldDeterministicOutcomes && cfg.WorkloadDeterministicOutcomesPath == g.Path:
			cfg.WorkloadDeterministicOutcomesPath = ""
		case g.Field == fieldRecordPath && cfg.WorkloadRecordPath == g.Path:
			cfg.WorkloadRecordPath = ""
		}
	}
}
