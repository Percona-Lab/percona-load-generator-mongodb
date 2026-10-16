package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/replaycompare"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
)

const compareReplaysUsage = `Usage: %s compare-replays [flags] A B

Compares two executions of the same operations, like pt-upgrade: A is the
reference, B the target. Each of A and B is a replay outcome file
(workload_replay_outcomes_path), a recording made with
workload_record_outcomes on, or a deterministic outcome file
(workload_deterministic_outcomes_path). Both must come from the same
recording (same header hash) or the same deterministic traffic fingerprint.

Writes a JSON and an HTML report (mode 0600). The differing operations list
holds literal query values when the commands are available.

Flags:
`

// runCompareReplays implements "plgm compare-replays" and returns the exit
// code.
func runCompareReplays(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("compare-replays", flag.ContinueOnError)
	fs.SetOutput(stderr)
	labelA := fs.String("label-a", "A", "Label of A (for example mongo-8)")
	labelB := fs.String("label-b", "B", "Label of B (for example mongo-9)")
	recording := fs.String("recording", "", "Recording file that supplies the commands when A and B are both replay outcome files")
	allow := fs.Bool("allow-mismatch", false, "Compare even if A and B are not executions of the same operations (prints a warning)")
	top := fs.Int("top", replaycompare.DefaultTopN, fmt.Sprintf("Differing operations to list (max %d)", replaycompare.MaxTopN))
	jsonOut := fs.String("json", "plgm_replay_compare.json", "JSON report path (empty: none)")
	htmlOut := fs.String("html", "plgm_replay_compare.html", "HTML report path (empty: none)")
	trendStore := fs.String("trend-store", "", "Trend baseline file to read explain plans from (with --plans-a-run/--plans-b-run)")
	plansA := fs.String("plans-a-run", "", "Trend run id whose explain plans describe A")
	plansB := fs.String("plans-b-run", "", "Trend run id whose explain plans describe B")
	fs.Usage = func() {
		fmt.Fprintf(stderr, compareReplaysUsage, os.Args[0])
		fs.PrintDefaults()
	}
	var files []string
	for {
		if err := fs.Parse(args); err != nil {
			if err == flag.ErrHelp {
				return 0
			}
			return 2
		}
		if fs.NArg() == 0 {
			break
		}
		files = append(files, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(files) != 2 {
		fs.Usage()
		return 2
	}
	opts := replaycompare.Options{LabelA: *labelA, LabelB: *labelB, RecordingPath: *recording, AllowMismatch: *allow, TopN: *top}
	if (*plansA != "" || *plansB != "") && *trendStore == "" {
		fmt.Fprintln(stderr, "compare-replays: --plans-a-run and --plans-b-run need --trend-store")
		return 2
	}
	if *trendStore != "" {
		d := stats.OpenTrendStore(*trendStore).Snapshot()
		for _, p := range []struct {
			id  string
			dst *map[string]replaycompare.PlanInfo
		}{{*plansA, &opts.PlansA}, {*plansB, &opts.PlansB}} {
			if p.id == "" {
				continue
			}
			m, ok := replaycompare.PlansFromTrendRun(d, p.id)
			if !ok {
				fmt.Fprintf(stderr, "compare-replays: trend run %q not found in %s\n", p.id, *trendStore)
				return 2
			}
			*p.dst = m
		}
	}
	rep, err := replaycompare.Compare(context.Background(), files[0], files[1], opts)
	if err != nil {
		fmt.Fprintf(stderr, "compare-replays: %v\n", err)
		return 1
	}
	rep.A.PlanRunID, rep.B.PlanRunID = *plansA, *plansB
	if err := replaycompare.WriteFiles(rep, *jsonOut, *htmlOut); err != nil {
		fmt.Fprintf(stderr, "compare-replays: write report: %v\n", err)
		return 1
	}
	fmt.Fprint(stdout, replaycompare.SummaryText(rep))
	for _, p := range []string{*jsonOut, *htmlOut} {
		if p != "" {
			fmt.Fprintf(stdout, "Wrote %s (mode 0600)\n", p)
		}
	}
	if rep.ContainsLiteralValues {
		fmt.Fprintln(stdout, "WARNING: the report contains literal query values from the recording.")
	}
	return 0
}
