package webui

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
)

// applyWorkloadRecordForm applies the random seed and the workload
// record/replay fields. A field missing from the form keeps the configured
// value.
func applyWorkloadRecordForm(cfg *config.AppConfig, form url.Values) error {
	boolField := func(name string, dst *bool) {
		if _, ok := form[name]; ok {
			v := form.Get(name)
			*dst = v == "true" || v == "on"
		}
	}
	stringField := func(name string, dst *string) {
		if _, ok := form[name]; ok {
			*dst = strings.TrimSpace(form.Get(name))
		}
	}
	if _, ok := form["random_seed"]; ok {
		v := strings.TrimSpace(form.Get("random_seed"))
		cfg.RandomSeed = 0
		if v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return fmt.Errorf("random_seed must be a whole number (0 or empty for random)")
			}
			cfg.RandomSeed = n
		}
	}
	boolField("workload_record_enabled", &cfg.WorkloadRecordEnabled)
	stringField("workload_record_path", &cfg.WorkloadRecordPath)
	if _, ok := form["workload_record_max_bytes"]; ok {
		if v := strings.TrimSpace(form.Get("workload_record_max_bytes")); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				return fmt.Errorf("workload_record_max_bytes must be 0 (unlimited) or a positive number of bytes")
			}
			cfg.WorkloadRecordMaxBytes = n
		}
	}
	stringField("workload_record_backpressure", &cfg.WorkloadRecordBackpressure)
	stringField("workload_record_compression", &cfg.WorkloadRecordCompression)
	boolField("workload_replay_enabled", &cfg.WorkloadReplayEnabled)
	stringField("workload_replay_path", &cfg.WorkloadReplayPath)
	stringField("workload_replay_mode", &cfg.WorkloadReplayMode)
	boolField("workload_replay_allow_mismatch", &cfg.WorkloadReplayAllowMismatch)
	boolField("workload_record_outcomes", &cfg.WorkloadRecordOutcomes)
	boolField("workload_record_result_checksums", &cfg.WorkloadRecordResultChecksums)
	stringField("workload_replay_outcomes_path", &cfg.WorkloadReplayOutcomesPath)
	stringField("workload_deterministic_outcomes_path", &cfg.WorkloadDeterministicOutcomesPath)
	if _, ok := form["operations_per_worker"]; ok {
		cfg.OperationsPerWorker = 0
		if v := strings.TrimSpace(form.Get("operations_per_worker")); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return fmt.Errorf("operations_per_worker must be 0 (off) or a positive whole number")
			}
			cfg.OperationsPerWorker = n
		}
	}
	if _, ok := form["workload_replay_targets"]; ok {
		cfg.WorkloadReplayTargets = nil
		if v := strings.TrimSpace(form.Get("workload_replay_targets")); v != "" {
			if err := json.Unmarshal([]byte(v), &cfg.WorkloadReplayTargets); err != nil {
				return fmt.Errorf("invalid multi-target settings: workload_replay_targets must be a list of {\"label\", \"uri\"} targets")
			}
		}
	}
	stringField("workload_replay_targets_mode", &cfg.WorkloadReplayTargetsMode)
	stringField("workload_replay_ops", &cfg.WorkloadReplayOps)
	stringField("workload_data_fingerprint", &cfg.WorkloadDataFingerprint)
	return nil
}
