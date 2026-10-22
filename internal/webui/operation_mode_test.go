package webui

import (
	"testing"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
)

func TestApplyOperationModeEmptyPreservesFields(t *testing.T) {
	cfg := &config.AppConfig{}
	cfg.WorkloadRecordEnabled = true
	cfg.WorkloadReplayEnabled = true
	cfg.RawInjector.Enabled = true
	cfg.DefaultWorkload = false
	if err := applyOperationMode(cfg, ""); err != nil {
		t.Fatal(err)
	}
	if !cfg.WorkloadRecordEnabled || !cfg.WorkloadReplayEnabled || !cfg.RawInjector.Enabled || cfg.DefaultWorkload {
		t.Fatalf("empty mode changed config: %+v", cfg)
	}
}

func TestApplyOperationModeGenericStripsIncompatible(t *testing.T) {
	cfg := &config.AppConfig{}
	cfg.DefaultWorkload = false
	cfg.WorkloadRecordEnabled = true
	cfg.WorkloadRecordPath = "keep.ndjson"
	cfg.WorkloadReplayEnabled = true
	cfg.WorkloadReplayPath = "replay.ndjson"
	cfg.RawInjector.Enabled = true
	cfg.WorkloadRecordOutcomes = true
	cfg.WorkloadRecordResultChecksums = true
	cfg.WorkloadReplayOutcomesPath = "out.ndjson"
	cfg.WorkloadReplayTargets = []config.ReplayTarget{{Label: "a", URI: "mongodb://user:secret@db:27017"}}
	cfg.WorkloadReplayOps = config.WorkloadReplayOpsReadsOnly
	cfg.WorkloadDataFingerprint = config.DataFingerprintCounts
	cfg.Concurrency = 16
	cfg.Duration = "60s"
	if err := applyOperationMode(cfg, "generic"); err != nil {
		t.Fatal(err)
	}
	if !cfg.DefaultWorkload || cfg.WorkloadRecordEnabled || cfg.WorkloadReplayEnabled || cfg.RawInjector.Enabled {
		t.Fatalf("generic mode flags: %+v", cfg)
	}
	if cfg.WorkloadRecordPath != "keep.ndjson" || cfg.WorkloadReplayPath != "replay.ndjson" {
		t.Fatalf("compatible paths were cleared: %+v", cfg)
	}
	if cfg.WorkloadRecordOutcomes || cfg.WorkloadRecordResultChecksums || cfg.WorkloadReplayOutcomesPath != "" || len(cfg.WorkloadReplayTargets) != 0 {
		t.Fatalf("incompatible outcomes or targets still apply: %+v", cfg)
	}
	if cfg.WorkloadReplayOps != config.WorkloadReplayOpsAll || cfg.WorkloadDataFingerprint != config.DataFingerprintOff {
		t.Fatalf("replay-only options still apply: %+v", cfg)
	}
	if cfg.Concurrency != 16 || cfg.Duration != "60s" {
		t.Fatalf("basic settings changed: %+v", cfg)
	}
}

func TestApplyOperationModeGenericKeepsDeterministicTargets(t *testing.T) {
	cfg := &config.AppConfig{WorkloadDeterministic: true}
	cfg.WorkloadReplayTargets = []config.ReplayTarget{{Label: "a", URI: "mongodb://a:27017"}, {Label: "b", URI: "mongodb://b:27017"}}
	if err := applyOperationMode(cfg, "generic"); err != nil {
		t.Fatal(err)
	}
	if !cfg.WorkloadDeterministic || len(cfg.WorkloadReplayTargets) != 2 {
		t.Fatalf("deterministic multi-target was stripped: %+v", cfg)
	}
}

func TestApplyOperationModeRecordAndReplay(t *testing.T) {
	cfg := &config.AppConfig{WorkloadDeterministic: true, WorkloadReplayEnabled: true, RawInjector: config.RawInjectorConfig{Enabled: true}}
	if err := applyOperationMode(cfg, "record"); err != nil {
		t.Fatal(err)
	}
	if !cfg.WorkloadRecordEnabled || cfg.WorkloadReplayEnabled || cfg.RawInjector.Enabled || !cfg.WorkloadDeterministic {
		t.Fatalf("record mode: %+v", cfg)
	}

	cfg.WorkloadRecordEnabled = true
	cfg.WorkloadDeterministic = true
	if err := applyOperationMode(cfg, "replay"); err != nil {
		t.Fatal(err)
	}
	if !cfg.WorkloadReplayEnabled || cfg.WorkloadRecordEnabled || cfg.WorkloadDeterministic || cfg.RawInjector.Enabled {
		t.Fatalf("replay mode: %+v", cfg)
	}
}

func TestApplyOperationModeCustomAndRaw(t *testing.T) {
	cfg := &config.AppConfig{DefaultWorkload: true, WorkloadRecordEnabled: true}
	if err := applyOperationMode(cfg, "custom"); err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultWorkload || cfg.WorkloadRecordEnabled {
		t.Fatalf("custom mode: %+v", cfg)
	}
	cfg.WorkloadReplayTargets = []config.ReplayTarget{{Label: "a", URI: "mongodb://a"}}
	cfg.WorkloadDeterministic = true
	if err := applyOperationMode(cfg, "raw"); err != nil {
		t.Fatal(err)
	}
	if !cfg.RawInjector.Enabled || cfg.WorkloadDeterministic || cfg.WorkloadRecordEnabled || len(cfg.WorkloadReplayTargets) != 0 {
		t.Fatalf("raw mode: %+v", cfg)
	}
}

func TestApplyOperationModeRejectsUnknown(t *testing.T) {
	if err := applyOperationMode(&config.AppConfig{}, "benchmark"); err == nil {
		t.Fatal("expected an error for an unknown mode")
	}
}
