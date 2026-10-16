package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkloadOutcomesDefaultOff(t *testing.T) {
	cfg, err := LoadAppConfig(filepath.Join(t.TempDir(), "missing.yaml"), false)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkloadRecordOutcomes || cfg.WorkloadRecordResultChecksums || cfg.WorkloadReplayOutcomesPath != "" || cfg.WorkloadDeterministicOutcomesPath != "" {
		t.Fatalf("outcome settings must default off/empty: %+v", cfg)
	}
}

func TestWorkloadOutcomesEnv(t *testing.T) {
	t.Setenv("PLGM_WORKLOAD_REPLAY_ENABLED", "true")
	t.Setenv("PLGM_WORKLOAD_REPLAY_PATH", "rec.ndjson")
	t.Setenv("PLGM_WORKLOAD_RECORD_OUTCOMES", "true")
	t.Setenv("PLGM_WORKLOAD_RECORD_RESULT_CHECKSUMS", "1")
	t.Setenv("PLGM_WORKLOAD_REPLAY_OUTCOMES_PATH", "  replay_outcomes.ndjson ")
	cfg, err := LoadAppConfig(filepath.Join(t.TempDir(), "missing.yaml"), false)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.WorkloadRecordOutcomes || !cfg.WorkloadRecordResultChecksums || cfg.WorkloadReplayOutcomesPath != "replay_outcomes.ndjson" {
		t.Fatalf("env not applied: %v %v %q", cfg.WorkloadRecordOutcomes, cfg.WorkloadRecordResultChecksums, cfg.WorkloadReplayOutcomesPath)
	}
	t.Setenv("PLGM_WORKLOAD_REPLAY_OUTCOMES_PATH", "")
	if _, err := LoadAppConfig(filepath.Join(t.TempDir(), "missing.yaml"), false); err == nil || !strings.Contains(err.Error(), "workload_replay_outcomes_path") {
		t.Fatalf("replay outcomes without a path: %v", err)
	}

	t.Setenv("PLGM_WORKLOAD_REPLAY_ENABLED", "false")
	t.Setenv("PLGM_WORKLOAD_DETERMINISTIC_OUTCOMES_PATH", "det.ndjson")
	t.Setenv("PLGM_WORKLOAD_DETERMINISTIC", "true")
	t.Setenv("PLGM_RANDOM_SEED", "5")
	t.Setenv("PLGM_OPERATIONS_PER_WORKER", "10")
	t.Setenv("PLGM_DROP_COLLECTIONS", "true")
	cfg, err = LoadAppConfig(filepath.Join(t.TempDir(), "missing.yaml"), false)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkloadDeterministicOutcomesPath != "det.ndjson" {
		t.Fatalf("deterministic outcomes path = %q", cfg.WorkloadDeterministicOutcomesPath)
	}
}

func TestValidateWorkloadOutcomes(t *testing.T) {
	det := func(c AppConfig) AppConfig {
		c.WorkloadDeterministic, c.RandomSeed, c.OperationsPerWorker, c.DropCollections = true, 1, 10, true
		return c
	}
	cases := []struct {
		name string
		cfg  AppConfig
		want string
	}{
		{name: "defaults"},
		{name: "record", cfg: AppConfig{WorkloadRecordOutcomes: true, WorkloadRecordEnabled: true, WorkloadRecordPath: "r"}},
		{name: "record with checksums", cfg: AppConfig{WorkloadRecordOutcomes: true, WorkloadRecordResultChecksums: true, WorkloadRecordEnabled: true}},
		{name: "replay", cfg: AppConfig{WorkloadRecordOutcomes: true, WorkloadReplayEnabled: true, WorkloadReplayPath: "r", WorkloadReplayOutcomesPath: "o"}},
		{name: "deterministic file", cfg: det(AppConfig{WorkloadRecordOutcomes: true, WorkloadDeterministicOutcomesPath: "o"})},
		{name: "deterministic recording", cfg: det(AppConfig{WorkloadRecordOutcomes: true, WorkloadRecordEnabled: true})},
		{name: "checksums without outcomes", cfg: AppConfig{WorkloadRecordResultChecksums: true, WorkloadRecordEnabled: true}, want: "workload_record_result_checksums needs workload_record_outcomes"},
		{name: "replay path without outcomes", cfg: AppConfig{WorkloadReplayEnabled: true, WorkloadReplayOutcomesPath: "o"}, want: "workload_record_outcomes is off"},
		{name: "deterministic path without outcomes", cfg: det(AppConfig{WorkloadDeterministicOutcomesPath: "o"}), want: "workload_record_outcomes is off"},
		{name: "nowhere to write", cfg: AppConfig{WorkloadRecordOutcomes: true}, want: "needs workload_record_enabled"},
		{name: "replay without outcome file", cfg: AppConfig{WorkloadRecordOutcomes: true, WorkloadReplayEnabled: true, WorkloadReplayPath: "r"}, want: "needs workload_replay_outcomes_path"},
		{name: "deterministic without destination", cfg: det(AppConfig{WorkloadRecordOutcomes: true}), want: "workload_deterministic_outcomes_path or workload_record_enabled"},
		{name: "replay path without replay", cfg: AppConfig{WorkloadRecordOutcomes: true, WorkloadRecordEnabled: true, WorkloadReplayOutcomesPath: "o"}, want: "workload_replay_enabled is off"},
		{name: "deterministic path without deterministic", cfg: AppConfig{WorkloadRecordOutcomes: true, WorkloadRecordEnabled: true, WorkloadDeterministicOutcomesPath: "o"}, want: "workload_deterministic is off"},
		{name: "outcome file is the recording", cfg: AppConfig{WorkloadRecordOutcomes: true, WorkloadReplayEnabled: true, WorkloadReplayPath: "./rec.ndjson", WorkloadReplayOutcomesPath: "rec.ndjson"}, want: "must not be the recording file"},
		{name: "bad path", cfg: AppConfig{WorkloadRecordOutcomes: true, WorkloadReplayEnabled: true, WorkloadReplayPath: "r", WorkloadReplayOutcomesPath: "a\nb"}, want: "not a valid file path"},
	}
	for _, tc := range cases {
		cfg := tc.cfg
		err := ValidateWorkloadOutcomes(&cfg)
		if tc.want == "" {
			if err != nil {
				t.Fatalf("%s: unexpected error %v", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error = %v, want %q", tc.name, err, tc.want)
		}
	}
}
