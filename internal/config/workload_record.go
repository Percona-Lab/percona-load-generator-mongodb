package config

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

const (
	WorkloadReplayModePerWorker = "per_worker"
	WorkloadReplayModeTimed     = "timed"

	WorkloadRecordBackpressureFail  = "fail"
	WorkloadRecordBackpressureBlock = "block"

	WorkloadRecordCompressionNone = "none"
	WorkloadRecordCompressionZstd = "zstd"
	WorkloadRecordCompressionGzip = "gzip"

	DefaultWorkloadRecordPath     = "plgm_workload_record.ndjson"
	DefaultWorkloadRecordMaxBytes = int64(512 << 20)
	MaxWorkloadRecordPathBytes    = 1024
)

// NormalizeWorkloadRecordReplay trims the paths, replaces a negative size
// cap with the default (0 stays: unlimited) and, when recording is on
// without a path, applies the default path. Empty backpressure, compression,
// and replay mode become fail, none, and per_worker; other values are
// lowercased and left for ValidateWorkloadRecordReplay.
func NormalizeWorkloadRecordReplay(cfg *AppConfig) {
	if cfg == nil {
		return
	}
	cfg.WorkloadRecordPath = strings.TrimSpace(cfg.WorkloadRecordPath)
	cfg.WorkloadReplayPath = strings.TrimSpace(cfg.WorkloadReplayPath)
	cfg.WorkloadReplayOutcomesPath = strings.TrimSpace(cfg.WorkloadReplayOutcomesPath)
	cfg.WorkloadDeterministicOutcomesPath = strings.TrimSpace(cfg.WorkloadDeterministicOutcomesPath)
	if cfg.WorkloadRecordMaxBytes < 0 {
		cfg.WorkloadRecordMaxBytes = DefaultWorkloadRecordMaxBytes
	}
	cfg.WorkloadRecordBackpressure = strings.ToLower(strings.TrimSpace(cfg.WorkloadRecordBackpressure))
	if cfg.WorkloadRecordBackpressure == "" {
		cfg.WorkloadRecordBackpressure = WorkloadRecordBackpressureFail
	}
	cfg.WorkloadRecordCompression = strings.ToLower(strings.TrimSpace(cfg.WorkloadRecordCompression))
	if cfg.WorkloadRecordCompression == "" {
		cfg.WorkloadRecordCompression = WorkloadRecordCompressionNone
	}
	if cfg.WorkloadRecordEnabled && cfg.WorkloadRecordPath == "" {
		cfg.WorkloadRecordPath = DefaultWorkloadRecordPath
	}
	cfg.WorkloadReplayMode = strings.ToLower(strings.TrimSpace(cfg.WorkloadReplayMode))
	if cfg.WorkloadReplayMode == "" {
		cfg.WorkloadReplayMode = WorkloadReplayModePerWorker
	}
	normalizeWorkloadTargets(cfg)
}

// ValidateWorkloadRecordReplay rejects settings that cannot run: recording
// and replay together, replay without a file or with an unknown mode, either
// with the raw injector, and recording without a positive duration or with
// an unknown backpressure policy or compression.
func ValidateWorkloadRecordReplay(cfg *AppConfig) error {
	if cfg == nil {
		return nil
	}
	if !cfg.WorkloadRecordEnabled && !cfg.WorkloadReplayEnabled {
		return nil
	}
	if cfg.WorkloadRecordEnabled && cfg.WorkloadReplayEnabled {
		return fmt.Errorf("invalid workload record/replay settings: workload_record_enabled and workload_replay_enabled cannot both be on for the same run")
	}
	if cfg.RawInjector.Enabled {
		return fmt.Errorf("invalid workload record/replay settings: recording and replay are not supported with the raw injector")
	}
	if cfg.WorkloadRecordEnabled {
		if err := checkWorkloadPath("workload_record_path", cfg.WorkloadRecordPath); err != nil {
			return err
		}
		switch strings.ToLower(strings.TrimSpace(cfg.WorkloadRecordBackpressure)) {
		case "", WorkloadRecordBackpressureFail, WorkloadRecordBackpressureBlock:
		default:
			return fmt.Errorf("invalid workload record/replay settings: workload_record_backpressure must be %q or %q (got %q)", WorkloadRecordBackpressureFail, WorkloadRecordBackpressureBlock, cfg.WorkloadRecordBackpressure)
		}
		switch strings.ToLower(strings.TrimSpace(cfg.WorkloadRecordCompression)) {
		case "", WorkloadRecordCompressionNone, WorkloadRecordCompressionZstd, WorkloadRecordCompressionGzip:
		default:
			return fmt.Errorf("invalid workload record/replay settings: workload_record_compression must be %q, %q, or %q (got %q)", WorkloadRecordCompressionNone, WorkloadRecordCompressionZstd, WorkloadRecordCompressionGzip, cfg.WorkloadRecordCompression)
		}
		if cfg.WorkloadRecordMaxBytes < 0 {
			return fmt.Errorf("invalid workload record/replay settings: workload_record_max_bytes must be 0 (unlimited) or a positive number of bytes")
		}
		if d, err := time.ParseDuration(strings.TrimSpace(cfg.Duration)); err == nil && d <= 0 {
			return fmt.Errorf("invalid workload record/replay settings: recording needs a positive duration (duration=%s)", cfg.Duration)
		}
		return nil
	}
	if strings.TrimSpace(cfg.WorkloadReplayPath) == "" {
		return fmt.Errorf("invalid workload record/replay settings: workload_replay_path is required when workload_replay_enabled is on")
	}
	if err := checkWorkloadPath("workload_replay_path", cfg.WorkloadReplayPath); err != nil {
		return err
	}
	switch strings.ToLower(strings.TrimSpace(cfg.WorkloadReplayMode)) {
	case "", WorkloadReplayModePerWorker, WorkloadReplayModeTimed:
	default:
		return fmt.Errorf("invalid workload record/replay settings: workload_replay_mode must be %q or %q (got %q)", WorkloadReplayModePerWorker, WorkloadReplayModeTimed, cfg.WorkloadReplayMode)
	}
	return nil
}

func checkWorkloadPath(name, path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("invalid workload record/replay settings: %s is empty", name)
	}
	if len(path) > MaxWorkloadRecordPathBytes || strings.ContainsAny(path, "\x00\r\n") {
		return fmt.Errorf("invalid workload record/replay settings: %s is not a valid file path", name)
	}
	return nil
}

// ValidateWorkloadDeterministic rejects deterministic generation that could
// not reproduce its traffic: it needs a non-zero random_seed, an
// operations_per_worker budget, a positive duration (the run repeats
// until it ends),
// a fixed worker count, and freshly seeded data, because values for
// existing records are derived from the seeded documents instead of being
// read from the database. It cannot be combined with replay or the raw
// injector.
func ValidateWorkloadDeterministic(cfg *AppConfig) error {
	if cfg == nil || !cfg.WorkloadDeterministic {
		return nil
	}
	const prefix = "invalid deterministic workload settings: "
	if cfg.RandomSeed == 0 {
		return fmt.Errorf(prefix + "workload_deterministic needs a non-zero random_seed")
	}
	if cfg.OperationsPerWorker <= 0 {
		return fmt.Errorf(prefix + "workload_deterministic needs operations_per_worker (the number of operations each worker sends per iteration)")
	}
	if cfg.WorkloadReplayEnabled {
		return fmt.Errorf(prefix + "workload_deterministic and workload_replay_enabled cannot both be on: a replay sends the recorded operations instead of generating them")
	}
	if cfg.RawInjector.Enabled {
		return fmt.Errorf(prefix + "workload_deterministic is not supported with the raw injector")
	}
	if k := strings.ToLower(strings.TrimSpace(cfg.LoadProfile.Kind)); k != "" && k != "fixed" {
		return fmt.Errorf(prefix+"load_profile %q changes the worker count over time; workload_deterministic needs a fixed worker count", cfg.LoadProfile.Kind)
	}
	if !cfg.DropCollections || cfg.SkipSeed {
		return fmt.Errorf(prefix + "workload_deterministic needs freshly seeded data: turn drop_collections on and skip_seed off, so every run starts from the documents the seed generates (existing data is not supported yet)")
	}
	if d, err := time.ParseDuration(strings.TrimSpace(cfg.Duration)); err == nil && d <= 0 {
		return fmt.Errorf(prefix+"workload_deterministic needs a positive duration; the workload repeats until it ends (duration=%s)", cfg.Duration)
	}
	return nil
}

// ValidateWorkloadOutcomes rejects outcome settings that would capture
// nothing or write over another file: checksums need outcome capture, an
// outcome file path needs outcome capture and its mode, and outcome capture
// needs somewhere to write: the recording, the replay outcome file, or the
// deterministic outcome file.
func ValidateWorkloadOutcomes(cfg *AppConfig) error {
	if cfg == nil {
		return nil
	}
	const prefix = "invalid workload outcome settings: "
	replayPath, detPath := strings.TrimSpace(cfg.WorkloadReplayOutcomesPath), strings.TrimSpace(cfg.WorkloadDeterministicOutcomesPath)
	if !cfg.WorkloadRecordOutcomes {
		switch {
		case cfg.WorkloadRecordResultChecksums:
			return fmt.Errorf(prefix + "workload_record_result_checksums needs workload_record_outcomes on")
		case replayPath != "":
			return fmt.Errorf(prefix + "workload_replay_outcomes_path is set but workload_record_outcomes is off")
		case detPath != "":
			return fmt.Errorf(prefix + "workload_deterministic_outcomes_path is set but workload_record_outcomes is off")
		}
		return nil
	}
	if replayPath != "" && !cfg.WorkloadReplayEnabled {
		return fmt.Errorf(prefix + "workload_replay_outcomes_path is set but workload_replay_enabled is off")
	}
	if detPath != "" && !cfg.WorkloadDeterministic {
		return fmt.Errorf(prefix + "workload_deterministic_outcomes_path is set but workload_deterministic is off")
	}
	switch {
	case cfg.WorkloadReplayEnabled:
		if replayPath == "" {
			return fmt.Errorf(prefix + "workload_record_outcomes during a replay needs workload_replay_outcomes_path (the replay writes its outcomes there)")
		}
	case cfg.WorkloadDeterministic:
		if detPath == "" && !cfg.WorkloadRecordEnabled {
			return fmt.Errorf(prefix + "workload_record_outcomes in a deterministic run needs workload_deterministic_outcomes_path or workload_record_enabled")
		}
	case !cfg.WorkloadRecordEnabled:
		return fmt.Errorf(prefix + "workload_record_outcomes needs workload_record_enabled, a replay with workload_replay_outcomes_path, or a deterministic run with workload_deterministic_outcomes_path")
	}
	for _, f := range [][2]string{{"workload_replay_outcomes_path", replayPath}, {"workload_deterministic_outcomes_path", detPath}} {
		name, p := f[0], f[1]
		if p == "" {
			continue
		}
		if len(p) > MaxWorkloadRecordPathBytes || strings.ContainsAny(p, "\x00\r\n") {
			return fmt.Errorf(prefix+"%s is not a valid file path", name)
		}
		for _, other := range []string{cfg.WorkloadReplayPath, cfg.WorkloadRecordPath} {
			if other != "" && filepath.Clean(strings.TrimSpace(other)) == filepath.Clean(p) {
				return fmt.Errorf(prefix+"%s must not be the recording file (%s)", name, p)
			}
		}
	}
	return nil
}

// WorkloadReplayWarnings lists the settings a replay ignores because the
// recorded operations already fixed them.
func WorkloadReplayWarnings(cfg *AppConfig) []string {
	if cfg == nil || !cfg.WorkloadReplayEnabled {
		return nil
	}
	var out []string
	if k := strings.ToLower(strings.TrimSpace(cfg.LoadProfile.Kind)); k != "" && k != "fixed" {
		out = append(out, fmt.Sprintf("load_profile %q is ignored during replay: the recorded operations run on the recorded worker count", cfg.LoadProfile.Kind))
	}
	if k := strings.ToLower(strings.TrimSpace(cfg.AccessPattern.Kind)); k != "" && k != "uniform" {
		out = append(out, fmt.Sprintf("access_pattern %q is ignored during replay: the recorded operations already hold their values", cfg.AccessPattern.Kind))
	}
	if cfg.ThinkTimeMs > 0 || cfg.ThinkJitterMs > 0 {
		out = append(out, "think_time_ms and think_jitter_ms are ignored during replay; use workload_replay_mode=timed to keep the recorded pacing")
	}
	if cfg.Iterations > 1 {
		out = append(out, fmt.Sprintf("iterations=%d is ignored during replay: the replay runs once and replays every iteration in the recording", cfg.Iterations))
	}
	return out
}

// RunIterations is the number of times a run calls the workload: the
// configured iterations, or 1 during replay, which replays every recorded
// iteration in one call.
func RunIterations(cfg *AppConfig) int {
	if cfg == nil || cfg.Iterations < 1 {
		return 1
	}
	if cfg.WorkloadReplayEnabled {
		return 1
	}
	return cfg.Iterations
}
