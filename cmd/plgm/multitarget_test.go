package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/multitarget"
	mongodrv "go.mongodb.org/mongo-driver/v2/mongo"
)

func TestReplayTargetFlags(t *testing.T) {
	var targets replayTargetFlags
	fs := flag.NewFlagSet("plgm", flag.ContinueOnError)
	fs.Var(&targets, "replay-target", "")
	mode := fs.String("replay-targets-mode", "", "")
	if err := fs.Parse([]string{"--replay-target", "m8=mongodb://a:1/?replicaSet=rs0", "--replay-target", "m9=mongodb://b:2", "--replay-targets-mode", "concurrent"}); err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 || targets[0].URI != "mongodb://a:1/?replicaSet=rs0" || targets.String() != "m8,m9" {
		t.Fatalf("targets = %+v", targets)
	}
	if err := targets.Set("mongodb://no-label"); err == nil {
		t.Fatal("a target without a label must be rejected")
	}

	cfg := &config.AppConfig{WorkloadReplayEnabled: true, WorkloadReplayPath: "rec.ndjson", WorkloadRecordOutcomes: true, WorkloadReplayOutcomesPath: "out.ndjson",
		WorkloadReplayTargets: []config.ReplayTarget{{Label: "yaml", URI: "mongodb://y:1"}}}
	if err := applyReplayTargetFlags(cfg, targets, *mode); err != nil {
		t.Fatal(err)
	}
	if len(cfg.WorkloadReplayTargets) != 2 || cfg.WorkloadReplayTargets[0].Label != "m8" || cfg.WorkloadReplayTargetsMode != config.WorkloadTargetsConcurrent {
		t.Fatalf("flags must replace the configured targets: %+v %s", cfg.WorkloadReplayTargets, cfg.WorkloadReplayTargetsMode)
	}
	disableTargetCompression(cfg)
	if cfg.WorkloadReplayTargets[0].URI != "mongodb://a:1/?replicaSet=rs0&compressors=none" || cfg.WorkloadReplayTargets[1].URI != "mongodb://b:2?compressors=none" {
		t.Fatalf("uris = %+v", cfg.WorkloadReplayTargets)
	}
	if err := applyReplayTargetFlags(cfg, nil, "parallel"); err == nil || !strings.Contains(err.Error(), "workload_replay_targets_mode") {
		t.Fatalf("bad mode error = %v", err)
	}
}

func TestRunMultiTargetReportsFailedTargets(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.AppConfig{WorkloadReplayEnabled: true, WorkloadReplayPath: filepath.Join(dir, "rec.ndjson"), WorkloadRecordOutcomes: true, WorkloadReplayOutcomesPath: filepath.Join(dir, "out.ndjson"),
		WorkloadReplayTargets: []config.ReplayTarget{{Label: "m8", URI: "mongodb://user:secret@a:1"}, {Label: "m9", URI: "mongodb://user:secret@b:2"}}}
	var out bytes.Buffer
	code := runMultiTarget(context.Background(), cfg, "app", nil, nil, &out, func(p *multitarget.Plan) {
		p.Deps.Connect = func(ctx context.Context, c *config.AppConfig, dbName string) (*mongodrv.Database, func(), error) {
			return nil, nil, errors.New("server selection timeout")
		}
	})
	if code != 1 || !strings.Contains(out.String(), "m9") || !strings.Contains(out.String(), "server selection timeout") || strings.Contains(out.String(), "secret") {
		t.Fatalf("code %d, summary:\n%s", code, out.String())
	}
}
