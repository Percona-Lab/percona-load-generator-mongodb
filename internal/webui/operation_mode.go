package webui

import (
	"fmt"
	"strings"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
)

// Operation modes select which settings apply to a Web UI run.
// An empty mode keeps the previous behavior: every submitted field applies.
const (
	opModeGeneric = "generic"
	opModeCustom  = "custom"
	opModeRecord  = "record"
	opModeReplay  = "replay"
	opModeRaw     = "raw"
)

// applyOperationMode forces the flags that define a mode and clears settings
// that mode cannot use, so a value left in a hidden section does not change
// the run. Compatible values (paths, mix, concurrency, and so on) stay in cfg.
func applyOperationMode(cfg *config.AppConfig, mode string) error {
	if cfg == nil {
		return nil
	}
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		return nil
	}
	switch mode {
	case opModeGeneric, opModeCustom, opModeRecord, opModeReplay, opModeRaw:
	default:
		return fmt.Errorf("operation_mode must be generic, custom, record, replay, or raw")
	}

	switch mode {
	case opModeGeneric:
		cfg.DefaultWorkload = true
		cfg.WorkloadRecordEnabled = false
		cfg.WorkloadReplayEnabled = false
		cfg.RawInjector.Enabled = false
	case opModeCustom:
		cfg.DefaultWorkload = false
		cfg.WorkloadRecordEnabled = false
		cfg.WorkloadReplayEnabled = false
		cfg.RawInjector.Enabled = false
	case opModeRecord:
		cfg.WorkloadRecordEnabled = true
		cfg.WorkloadReplayEnabled = false
		cfg.RawInjector.Enabled = false
	case opModeReplay:
		cfg.WorkloadReplayEnabled = true
		cfg.WorkloadRecordEnabled = false
		cfg.WorkloadDeterministic = false
		cfg.RawInjector.Enabled = false
	case opModeRaw:
		cfg.RawInjector.Enabled = true
		cfg.WorkloadRecordEnabled = false
		cfg.WorkloadReplayEnabled = false
		cfg.WorkloadDeterministic = false
	}

	if mode != opModeReplay && !cfg.WorkloadDeterministic {
		cfg.WorkloadReplayTargets = nil
	}
	if mode == opModeRaw {
		cfg.WorkloadReplayTargets = nil
	}
	// Replay-only choices left in the form must not fail a mode that cannot use them.
	if !cfg.WorkloadReplayEnabled && strings.EqualFold(strings.TrimSpace(cfg.WorkloadReplayOps), config.WorkloadReplayOpsReadsOnly) {
		cfg.WorkloadReplayOps = config.WorkloadReplayOpsAll
	}
	if !cfg.WorkloadRecordEnabled && !cfg.WorkloadReplayEnabled && config.DataFingerprintMode(cfg) != config.DataFingerprintOff {
		cfg.WorkloadDataFingerprint = config.DataFingerprintOff
	}
	clearIncompatibleOutcomes(cfg)
	return nil
}

// clearIncompatibleOutcomes drops outcome capture that the effective mode
// cannot write, without touching a recording path the user may switch back to.
func clearIncompatibleOutcomes(cfg *config.AppConfig) {
	if !cfg.WorkloadReplayEnabled {
		cfg.WorkloadReplayOutcomesPath = ""
	}
	if !cfg.WorkloadDeterministic {
		cfg.WorkloadDeterministicOutcomesPath = ""
	}
	if cfg.WorkloadRecordEnabled || cfg.WorkloadReplayEnabled || cfg.WorkloadDeterministic {
		return
	}
	cfg.WorkloadRecordOutcomes = false
	cfg.WorkloadRecordResultChecksums = false
}

// inferDashboardMode labels a run that did not send operation_mode.
func inferDashboardMode(cfg *config.AppConfig) string {
	if cfg == nil {
		return opModeGeneric
	}
	switch {
	case cfg.RawInjector.Enabled:
		return opModeRaw
	case cfg.WorkloadReplayEnabled:
		return opModeReplay
	case cfg.WorkloadRecordEnabled:
		return opModeRecord
	case cfg.DefaultWorkload:
		return opModeGeneric
	default:
		return opModeCustom
	}
}
