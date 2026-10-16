package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/mongo"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/trafficimport"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/workloadrec"
)

const importTrafficUsage = `Usage: %s import-traffic [flags] FILE...

Converts captured MongoDB traffic into a workload recording that
workload_replay_path replays (per_worker or timed, multi-target, outcomes,
compare-replays), like pt-upgrade replaying logs. Each FILE is a mongod JSON
log (4.4+), a mongoexport of system.profile (NDJSON or --jsonArray), or raw
command documents; plain, gzip, or zstd.

Operations keep their order and timing (start = logged time minus duration);
each connection (log ctx, profiler session id or client) becomes a worker.
find, aggregate, insert, update, and delete are converted. getMore, truncated
commands, other commands, replacement updates, and system namespaces are
skipped and counted; operations with server-generated values, options the
replay does not send, or that ran in a transaction are kept and flagged.

Logs and the profiler capture only operations slower than slowms (default
100 ms), sampled by slowOpSampleRate, unless profiling level 2 or slowms 0
was set while capturing.

The recording (mode 0600) holds literal values from the captured traffic.

Flags:
`

// runImportTraffic implements "plgm import-traffic" and returns the exit code.
func runImportTraffic(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("import-traffic", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "plgm_imported_traffic.ndjson", "Recording to write (an existing file that is not a recording is never overwritten)")
	workers := fs.Int("workers", 0, fmt.Sprintf("Workers to map connections onto (0: one per connection, up to %d)", trafficimport.MaxWorkers))
	db := fs.String("db", "", "Database of raw command documents without $db")
	compression := fs.String("compression", workloadrec.CompressionNone, "Recording compression: none, zstd, or gzip")
	skipGenerated := fs.Bool("skip-server-generated", false, "Skip operations with server-generated values instead of flagging them")
	jsonOut := fs.String("json", "", "Also write the import report as JSON to this path")
	fs.Usage = func() {
		fmt.Fprintf(stderr, importTrafficUsage, os.Args[0])
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
	if len(files) == 0 {
		fs.Usage()
		return 2
	}
	var inputs []trafficimport.Input
	for _, p := range files {
		f, err := os.Open(p)
		if err != nil {
			fmt.Fprintf(stderr, "import-traffic: %v\n", err)
			return 1
		}
		defer f.Close()
		r, release, err := workloadrec.Decompress(f)
		if err != nil {
			fmt.Fprintf(stderr, "import-traffic: %s: %v\n", p, err)
			return 1
		}
		defer release()
		inputs = append(inputs, trafficimport.Input{Name: p, R: r})
	}
	rep, err := trafficimport.Convert(inputs, *out, trafficimport.Options{
		Workers: *workers, DefaultDatabase: *db, Compression: *compression, SkipServerGenerated: *skipGenerated, Normalize: mongo.ImportedOperation,
	})
	if rep != nil {
		fmt.Fprint(stdout, trafficimport.SummaryText(rep))
	}
	if err != nil {
		fmt.Fprintf(stderr, "import-traffic: %v\n", err)
		return 1
	}
	if *jsonOut != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(*jsonOut, append(b, '\n'), 0o600); err != nil {
			fmt.Fprintf(stderr, "import-traffic: write report: %v\n", err)
			return 1
		}
	}
	fmt.Fprintf(stdout, "Wrote %s (mode 0600, %d operations, %d workers). It contains literal values from the captured traffic.\n", *out, rep.Converted, rep.Workers)
	fmt.Fprintf(stdout, "Replay it with workload_replay_enabled: true and workload_replay_path: %s, on targets restored from a copy of the captured data (drop_collections off, skip_seed on).\n", *out)
	return 0
}
