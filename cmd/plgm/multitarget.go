package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/multitarget"
)

// replayTargetFlags collects repeated --replay-target label=uri flags.
type replayTargetFlags []config.ReplayTarget

func (f *replayTargetFlags) String() string {
	labels := make([]string, len(*f))
	for i, t := range *f {
		labels[i] = t.Label
	}
	return strings.Join(labels, ",")
}

func (f *replayTargetFlags) Set(v string) error {
	ts := config.ParseReplayTargets(v)
	if len(ts) != 1 || ts[0].Label == "" {
		return fmt.Errorf("want label=uri (for example m8=mongodb://host8:27017)")
	}
	*f = append(*f, ts[0])
	return nil
}

// applyReplayTargetFlags replaces the configured targets and mode with the
// flags, when given, and validates the result.
func applyReplayTargetFlags(cfg *config.AppConfig, targets replayTargetFlags, mode string) error {
	if len(targets) > 0 {
		cfg.WorkloadReplayTargets = append([]config.ReplayTarget(nil), targets...)
	}
	if mode != "" {
		cfg.WorkloadReplayTargetsMode = mode
	}
	config.NormalizeWorkloadRecordReplay(cfg)
	return config.ValidateWorkloadTargets(cfg)
}

// disableTargetCompression adds compressors=none to every target URI that
// does not choose compressors, as the CLI does for uri.
func disableTargetCompression(cfg *config.AppConfig) {
	for i := range cfg.WorkloadReplayTargets {
		u := &cfg.WorkloadReplayTargets[i].URI
		if strings.Contains(*u, "compressors=") {
			continue
		}
		if strings.Contains(*u, "?") {
			*u += "&compressors=none"
		} else {
			*u += "?compressors=none"
		}
	}
}

// runMultiTarget runs every target and writes the comparisons next to the
// outcome files. It returns 1 when a target failed or a comparison could not
// be made.
func runMultiTarget(ctx context.Context, cfg *config.AppConfig, dbName string, cols []config.CollectionDefinition, queries []config.QueryDefinition, out io.Writer, deps func(*multitarget.Plan)) int {
	plan, err := multitarget.NewPlan(cfg, dbName, cols, queries)
	if err != nil {
		log.Printf("multi-target: %v", err)
		return 1
	}
	plan.WriteReports = true
	if deps != nil {
		deps(plan)
	}
	labels := make([]string, len(plan.Targets))
	for i, t := range plan.Targets {
		labels[i] = t.Label
	}
	log.Printf("[Multi-Target] %s on %d targets (%s): %s; each is compared with %s", plan.Kind, len(labels), plan.Mode, strings.Join(labels, ", "), labels[0])
	res := plan.Run(ctx)
	fmt.Fprint(out, "\n"+res.SummaryText())
	code := 0
	for _, s := range res.Targets {
		if s.Status != multitarget.StatusComplete {
			code = 1
		}
	}
	for _, c := range res.Comparisons {
		if c.Error != "" {
			code = 1
		}
	}
	return code
}
