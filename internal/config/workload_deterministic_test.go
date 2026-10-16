package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/loadprofile"
)

func TestWorkloadDeterministicDefaultsOff(t *testing.T) {
	cfg, err := LoadAppConfig(filepath.Join(t.TempDir(), "missing.yaml"), false)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkloadDeterministic || cfg.OperationsPerWorker != 0 {
		t.Fatalf("deterministic generation must default off: deterministic=%v operations_per_worker=%d", cfg.WorkloadDeterministic, cfg.OperationsPerWorker)
	}
}

func TestWorkloadDeterministicEnvAndYAML(t *testing.T) {
	t.Setenv("PLGM_WORKLOAD_DETERMINISTIC", "true")
	t.Setenv("PLGM_OPERATIONS_PER_WORKER", "250")
	t.Setenv("PLGM_RANDOM_SEED", "7")
	t.Setenv("PLGM_DROP_COLLECTIONS", "true")
	t.Setenv("PLGM_SKIP_SEED", "false")
	cfg, err := LoadAppConfig(filepath.Join(t.TempDir(), "missing.yaml"), false)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.WorkloadDeterministic || cfg.OperationsPerWorker != 250 {
		t.Fatalf("env not applied: deterministic=%v operations_per_worker=%d", cfg.WorkloadDeterministic, cfg.OperationsPerWorker)
	}

	t.Setenv("PLGM_RANDOM_SEED", "0")
	if _, err := LoadAppConfig(filepath.Join(t.TempDir(), "missing.yaml"), false); err == nil || !strings.Contains(err.Error(), "random_seed") {
		t.Fatalf("deterministic without a seed must fail to load: %v", err)
	}

	for _, k := range []string{"PLGM_WORKLOAD_DETERMINISTIC", "PLGM_OPERATIONS_PER_WORKER", "PLGM_RANDOM_SEED", "PLGM_DROP_COLLECTIONS", "PLGM_SKIP_SEED"} {
		os.Unsetenv(k)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "workload_deterministic: true\noperations_per_worker: 99\nrandom_seed: 5\ndrop_collections: true\nskip_seed: false\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadAppConfig(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.WorkloadDeterministic || cfg.OperationsPerWorker != 99 || cfg.RandomSeed != 5 {
		t.Fatalf("yaml not applied: %v %d %d", cfg.WorkloadDeterministic, cfg.OperationsPerWorker, cfg.RandomSeed)
	}
}

func TestValidateWorkloadDeterministic(t *testing.T) {
	valid := func() *AppConfig {
		return &AppConfig{WorkloadDeterministic: true, RandomSeed: 1, OperationsPerWorker: 10, DropCollections: true, Duration: "10s"}
	}
	if err := ValidateWorkloadDeterministic(valid()); err != nil {
		t.Fatalf("valid settings rejected: %v", err)
	}
	off := &AppConfig{}
	if err := ValidateWorkloadDeterministic(off); err != nil {
		t.Fatalf("off must not be validated: %v", err)
	}
	cases := map[string]struct {
		mutate func(*AppConfig)
		want   string
	}{
		"no seed":         {func(c *AppConfig) { c.RandomSeed = 0 }, "non-zero random_seed"},
		"no budget":       {func(c *AppConfig) { c.OperationsPerWorker = 0 }, "operations_per_worker"},
		"replay":          {func(c *AppConfig) { c.WorkloadReplayEnabled = true }, "cannot both be on"},
		"raw injector":    {func(c *AppConfig) { c.RawInjector.Enabled = true }, "raw injector"},
		"load profile":    {func(c *AppConfig) { c.LoadProfile = loadprofile.Config{Kind: "step"} }, "fixed worker count"},
		"no drop":         {func(c *AppConfig) { c.DropCollections = false }, "drop_collections"},
		"skip seed":       {func(c *AppConfig) { c.SkipSeed = true }, "skip_seed off"},
		"no duration":     {func(c *AppConfig) { c.Duration = "0s" }, "safety timeout"},
		"fixed is fine":   {func(c *AppConfig) { c.LoadProfile = loadprofile.Config{Kind: "fixed"} }, ""},
		"record is fine":  {func(c *AppConfig) { c.WorkloadRecordEnabled = true }, ""},
		"think is fine":   {func(c *AppConfig) { c.ThinkTimeMs, c.ThinkJitterMs = 5, 5 }, ""},
		"pattern is fine": {func(c *AppConfig) { c.AccessPattern.Kind = "zipfian" }, ""},
	}
	for name, tc := range cases {
		cfg := valid()
		tc.mutate(cfg)
		err := ValidateWorkloadDeterministic(cfg)
		if tc.want == "" {
			if err != nil {
				t.Fatalf("%s: unexpected error %v", name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.HasPrefix(err.Error(), "invalid deterministic workload settings") {
			t.Fatalf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}
