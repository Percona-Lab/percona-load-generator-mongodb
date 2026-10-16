package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkloadRecordReplayDefaultsOff(t *testing.T) {
	cfg, err := LoadAppConfig(filepath.Join(t.TempDir(), "missing.yaml"), false)
	if err != nil {
		t.Fatalf("LoadAppConfig() error = %v", err)
	}
	if cfg.WorkloadRecordEnabled || cfg.WorkloadReplayEnabled || cfg.WorkloadReplayAllowMismatch {
		t.Fatalf("record/replay must default off: %+v", cfg)
	}
	if cfg.WorkloadRecordPath != "" || cfg.WorkloadReplayPath != "" {
		t.Fatalf("paths must default empty: record=%q replay=%q", cfg.WorkloadRecordPath, cfg.WorkloadReplayPath)
	}
	if cfg.WorkloadRecordMaxBytes != DefaultWorkloadRecordMaxBytes {
		t.Fatalf("WorkloadRecordMaxBytes = %d, want %d", cfg.WorkloadRecordMaxBytes, DefaultWorkloadRecordMaxBytes)
	}
	if cfg.WorkloadReplayMode != WorkloadReplayModePerWorker {
		t.Fatalf("WorkloadReplayMode = %q, want %q", cfg.WorkloadReplayMode, WorkloadReplayModePerWorker)
	}
	if cfg.WorkloadRecordBackpressure != WorkloadRecordBackpressureFail || cfg.WorkloadRecordCompression != WorkloadRecordCompressionNone {
		t.Fatalf("backpressure/compression = %q/%q, want fail/none", cfg.WorkloadRecordBackpressure, cfg.WorkloadRecordCompression)
	}
}

// TestWorkloadRecordMaxBytesZeroIsUnlimited checks that only an explicit 0
// removes the cap: an unset value keeps the default.
func TestWorkloadRecordMaxBytesZeroIsUnlimited(t *testing.T) {
	dir := t.TempDir()
	unset := filepath.Join(dir, "unset.yaml")
	zero := filepath.Join(dir, "zero.yaml")
	if err := os.WriteFile(unset, []byte("workload_record_enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(zero, []byte("workload_record_enabled: true\nworkload_record_max_bytes: 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err := LoadAppConfig(unset, false); err != nil || cfg.WorkloadRecordMaxBytes != DefaultWorkloadRecordMaxBytes {
		t.Fatalf("unset max bytes = %v / %v, want the default", cfg, err)
	}
	if cfg, err := LoadAppConfig(zero, false); err != nil || cfg.WorkloadRecordMaxBytes != 0 {
		t.Fatalf("explicit 0 = %v / %v, want 0 (unlimited)", cfg, err)
	}
	t.Setenv("PLGM_WORKLOAD_RECORD_MAX_BYTES", "0")
	if cfg, err := LoadAppConfig(unset, false); err != nil || cfg.WorkloadRecordMaxBytes != 0 {
		t.Fatalf("env 0 = %v / %v, want 0 (unlimited)", cfg, err)
	}
	neg := &AppConfig{WorkloadRecordMaxBytes: -5}
	NormalizeWorkloadRecordReplay(neg)
	if neg.WorkloadRecordMaxBytes != DefaultWorkloadRecordMaxBytes {
		t.Fatalf("negative max bytes normalized to %d, want the default", neg.WorkloadRecordMaxBytes)
	}
}

func TestWorkloadRecordBackpressureCompressionEnv(t *testing.T) {
	t.Setenv("PLGM_WORKLOAD_RECORD_ENABLED", "true")
	t.Setenv("PLGM_WORKLOAD_RECORD_BACKPRESSURE", " BLOCK ")
	t.Setenv("PLGM_WORKLOAD_RECORD_COMPRESSION", "Zstd")
	t.Setenv("PLGM_ITERATIONS", "3")
	cfg, err := LoadAppConfig(filepath.Join(t.TempDir(), "missing.yaml"), false)
	if err != nil {
		t.Fatalf("LoadAppConfig() error = %v (recording with iterations > 1 must be accepted)", err)
	}
	if cfg.WorkloadRecordBackpressure != WorkloadRecordBackpressureBlock || cfg.WorkloadRecordCompression != WorkloadRecordCompressionZstd || cfg.Iterations != 3 {
		t.Fatalf("env not applied: %q %q %d", cfg.WorkloadRecordBackpressure, cfg.WorkloadRecordCompression, cfg.Iterations)
	}
	t.Setenv("PLGM_WORKLOAD_RECORD_COMPRESSION", "brotli")
	if _, err := LoadAppConfig(filepath.Join(t.TempDir(), "missing.yaml"), false); err == nil || !strings.Contains(err.Error(), "workload_record_compression") {
		t.Fatalf("unknown compression error = %v", err)
	}
}

func TestRunIterationsIsOneDuringReplay(t *testing.T) {
	if n := RunIterations(&AppConfig{Iterations: 3}); n != 3 {
		t.Fatalf("RunIterations = %d, want 3", n)
	}
	if n := RunIterations(&AppConfig{Iterations: 3, WorkloadReplayEnabled: true}); n != 1 {
		t.Fatalf("RunIterations(replay) = %d, want 1", n)
	}
	if n := RunIterations(&AppConfig{}); n != 1 {
		t.Fatalf("RunIterations(zero) = %d, want 1", n)
	}
}

func TestWorkloadRecordEnvOverrides(t *testing.T) {
	t.Setenv("PLGM_WORKLOAD_RECORD_ENABLED", "true")
	t.Setenv("PLGM_WORKLOAD_RECORD_PATH", "  /tmp/rec.ndjson  ")
	t.Setenv("PLGM_WORKLOAD_RECORD_MAX_BYTES", "4096")
	cfg, err := LoadAppConfig(filepath.Join(t.TempDir(), "missing.yaml"), false)
	if err != nil {
		t.Fatalf("LoadAppConfig() error = %v", err)
	}
	if !cfg.WorkloadRecordEnabled || cfg.WorkloadRecordPath != "/tmp/rec.ndjson" || cfg.WorkloadRecordMaxBytes != 4096 {
		t.Fatalf("record env not applied: enabled=%v path=%q max=%d", cfg.WorkloadRecordEnabled, cfg.WorkloadRecordPath, cfg.WorkloadRecordMaxBytes)
	}
}

func TestWorkloadReplayEnvOverrides(t *testing.T) {
	t.Setenv("PLGM_WORKLOAD_REPLAY_ENABLED", "true")
	t.Setenv("PLGM_WORKLOAD_REPLAY_PATH", "/tmp/rec.ndjson")
	t.Setenv("PLGM_WORKLOAD_REPLAY_MODE", "TIMED")
	t.Setenv("PLGM_WORKLOAD_REPLAY_ALLOW_MISMATCH", "true")
	cfg, err := LoadAppConfig(filepath.Join(t.TempDir(), "missing.yaml"), false)
	if err != nil {
		t.Fatalf("LoadAppConfig() error = %v", err)
	}
	if !cfg.WorkloadReplayEnabled || cfg.WorkloadReplayPath != "/tmp/rec.ndjson" || cfg.WorkloadReplayMode != WorkloadReplayModeTimed || !cfg.WorkloadReplayAllowMismatch {
		t.Fatalf("replay env not applied: %+v", cfg)
	}
}

func TestWorkloadRecordAndReplayTogetherRejected(t *testing.T) {
	t.Setenv("PLGM_WORKLOAD_RECORD_ENABLED", "true")
	t.Setenv("PLGM_WORKLOAD_REPLAY_ENABLED", "true")
	t.Setenv("PLGM_WORKLOAD_REPLAY_PATH", "/tmp/rec.ndjson")
	_, err := LoadAppConfig(filepath.Join(t.TempDir(), "missing.yaml"), false)
	if err == nil || !strings.Contains(err.Error(), "cannot both be on") {
		t.Fatalf("LoadAppConfig() error = %v, want record+replay rejection", err)
	}
}

func TestNormalizeWorkloadRecordDefaultPath(t *testing.T) {
	cfg := &AppConfig{WorkloadRecordEnabled: true}
	NormalizeWorkloadRecordReplay(cfg)
	if cfg.WorkloadRecordPath != DefaultWorkloadRecordPath {
		t.Fatalf("WorkloadRecordPath = %q, want %q", cfg.WorkloadRecordPath, DefaultWorkloadRecordPath)
	}
	off := &AppConfig{}
	NormalizeWorkloadRecordReplay(off)
	if off.WorkloadRecordPath != "" {
		t.Fatalf("recording off must keep an empty path, got %q", off.WorkloadRecordPath)
	}
}

func TestValidateWorkloadRecordReplay(t *testing.T) {
	cases := []struct {
		name string
		cfg  AppConfig
		want string
	}{
		{name: "defaults", cfg: AppConfig{}},
		{name: "record ok", cfg: AppConfig{WorkloadRecordEnabled: true, WorkloadRecordPath: "r.ndjson", Duration: "10s", Iterations: 1}},
		{name: "replay ok", cfg: AppConfig{WorkloadReplayEnabled: true, WorkloadReplayPath: "r.ndjson", WorkloadReplayMode: "timed"}},
		{name: "both", cfg: AppConfig{WorkloadRecordEnabled: true, WorkloadRecordPath: "r", WorkloadReplayEnabled: true, WorkloadReplayPath: "r"}, want: "cannot both be on"},
		{name: "replay without path", cfg: AppConfig{WorkloadReplayEnabled: true}, want: "workload_replay_path is required"},
		{name: "bad mode", cfg: AppConfig{WorkloadReplayEnabled: true, WorkloadReplayPath: "r", WorkloadReplayMode: "fast"}, want: "workload_replay_mode"},
		{name: "record iterations", cfg: AppConfig{WorkloadRecordEnabled: true, WorkloadRecordPath: "r", Iterations: 3, IntervalDelay: "1s"}},
		{name: "record block zstd unlimited", cfg: AppConfig{WorkloadRecordEnabled: true, WorkloadRecordPath: "r", WorkloadRecordBackpressure: "Block", WorkloadRecordCompression: "zstd"}},
		{name: "record gzip", cfg: AppConfig{WorkloadRecordEnabled: true, WorkloadRecordPath: "r", WorkloadRecordCompression: "gzip"}},
		{name: "bad backpressure", cfg: AppConfig{WorkloadRecordEnabled: true, WorkloadRecordPath: "r", WorkloadRecordBackpressure: "drop"}, want: "workload_record_backpressure"},
		{name: "bad compression", cfg: AppConfig{WorkloadRecordEnabled: true, WorkloadRecordPath: "r", WorkloadRecordCompression: "lz4"}, want: "workload_record_compression"},
		{name: "negative max bytes", cfg: AppConfig{WorkloadRecordEnabled: true, WorkloadRecordPath: "r", WorkloadRecordMaxBytes: -1}, want: "workload_record_max_bytes"},
		{name: "record zero duration", cfg: AppConfig{WorkloadRecordEnabled: true, WorkloadRecordPath: "r", Duration: "0s"}, want: "positive duration"},
		{name: "record bad path", cfg: AppConfig{WorkloadRecordEnabled: true, WorkloadRecordPath: "a\nb"}, want: "not a valid file path"},
		{name: "raw injector", cfg: AppConfig{WorkloadReplayEnabled: true, WorkloadReplayPath: "r", RawInjector: RawInjectorConfig{Enabled: true}}, want: "raw injector"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateWorkloadRecordReplay(&tc.cfg)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestWorkloadReplayWarnings(t *testing.T) {
	if w := WorkloadReplayWarnings(&AppConfig{ThinkTimeMs: 5}); w != nil {
		t.Fatalf("no warnings without replay, got %v", w)
	}
	cfg := &AppConfig{WorkloadReplayEnabled: true, ThinkTimeMs: 5, Iterations: 2}
	cfg.LoadProfile.Kind = "ramp"
	cfg.AccessPattern.Kind = "zipfian"
	w := WorkloadReplayWarnings(cfg)
	if len(w) != 4 || !strings.Contains(w[3], "iterations=2 is ignored") {
		t.Fatalf("warnings = %v, want 4 ending with the iterations warning", w)
	}
}
