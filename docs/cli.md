# Running from the Command Line

PLGM runs headless from the CLI with the same features as the Web UI. Use it for scripted benchmarks, CI pipelines, and containers. All settings live in [`config.yaml`](../config.yaml) and can be overridden with environment variables; see the [Configuration Reference](./configuration-reference.md).

[Back to the main README](../README.md)

- [Configure](#configure)
- [Run](#run)
- [CLI Usage](#cli-usage)
- [What happens during a run](#what-happens-during-a-run)
- [Interpreting the Output](#interpreting-the-output)

---

## Configure

Download the [`config.yaml`](../config.yaml), set your connection settings and any other parameters, then choose your workload:

* **Mode 1: Default Workload (Easiest)**

    By default (`default_workload: true` in `config.yaml`), the application uses the embedded collection and query definitions. You do **not** need to create any extra folders or files.

* **Mode 2: Custom Workload**

    Set `default_workload: false` in `config.yaml` and point `collections_path` and `queries_path` at your JSON definitions. See [Workloads](./workloads.md#workload-loading-from-the-cli).

## Run

```bash
# The extracted binary will have the OS suffix
./plgm-linux-amd64

# Use a specific config file
./plgm /path/to/some/custom_config.yaml

# Override settings for one run
PLGM_CONCURRENCY=50 PLGM_DURATION=5m ./bin/plgm
```

Credentials can go in the URI (`mongodb://user:password@host:27017/`) or in `username` and `PLGM_PASSWORD`. A username and password in the URI win over the separate ones, and PLGM logs a line when it ignores them. With `--replay-target`, each target can have its own user in its URI. PLGM prompts for a username or password only when a URI without them needs one: if `PLGM_PASSWORD` is not set and a username is configured, it asks for the password.

Two subcommands support upgrade testing; see [Record and Replay](./record-and-replay.md):

```bash
./plgm compare-replays A B   # Compare two replay/deterministic outcome files
./plgm import-traffic LOG    # Convert mongod logs / system.profile exports into a recording
```

## CLI Usage

To view the full usage guide, including available flags and environment variables, run the help command:

```bash
./plgm --help

plgm: Percona Load Generator for MongoDB Clusters
Usage: ./plgm [flags] [config_file]

Examples:
  ./plgm                    # Run with default 'config.yaml'
  ./plgm my_test.yaml       # Run with specific config file
  ./plgm --help             # Show this help message
  ./plgm compare-replays A B # Compare two replay/deterministic outcome files (see compare-replays --help)
  ./plgm import-traffic LOG  # Convert mongod logs / system.profile exports into a recording (see import-traffic --help)

Flags:
  -config string
      Path to the configuration file (default "config.yaml")
  -raw-injector
      Enable Raw BSON Injector (High Performance Mode)
  -raw-injector-batch int
      Bulk batch size (ops per network round trip) (default 1000)
  -raw-injector-drop
      Drop the collection before starting
  -raw-injector-max-docs int
      Maximum number of documents to operate on (default 10000000)
  -raw-injector-size int
      Document size in bytes (default 1024)
  -raw-injector-type string
      Operation: insert, upsert, update, delete, find, mixed (default "insert")
  -replay-target value
      Multi-target replay: label=uri of one target (repeat for 2-8 targets; replaces workload_replay_targets)
  -replay-targets-mode string
      Multi-target replay: sequential (default) or concurrent
  -version
      Print version information and exit
  -webui
      Start the interactive Web UI
  -webui-port int
      Port for the Web UI (default 9999)
  -webui-tls
      Serve the Web UI over HTTPS (default: HTTP on loopback, no cert warnings)
  -webui-cert string
      Path to a TLS certificate file for the Web UI (implies --webui-tls)
  -webui-key string
      Path to the TLS private key file for the Web UI (implies --webui-tls)

Environment Variables (Overrides):
 [Connection]
  PLGM_URI                            Connection URI
  PLGM_USERNAME                       Database User
  PLGM_PASSWORD                       Database Password (Recommended: Use Prompt)
  PLGM_DIRECT_CONNECTION              Force direct connection (true/false)
  PLGM_REPLICASET_NAME                Replica Set name
  PLGM_READ_PREFERENCE                nearest

 [Web UI]
  PLGM_WEBUI_ENABLED                  Start the interactive Web UI (true/false)
  PLGM_WEBUI_PORT                     Port for the Web UI (default: 9999)
  PLGM_WEBUI_TLS                      Serve Web UI over HTTPS (true/false)
  PLGM_WEBUI_TLS_CERT                 TLS certificate file path for the Web UI
  PLGM_WEBUI_TLS_KEY                  TLS private key file path for the Web UI

 [Workload Core]
  PLGM_DEFAULT_WORKLOAD               Use built-in workload (true/false)
  PLGM_COLLECTIONS_PATH               Path to collection JSON
  PLGM_QUERIES_PATH                   Path to query JSON
  PLGM_DURATION                       Test duration (e.g. 60s, 5m)
  PLGM_ITERATIONS                     Number of times to repeat the workload
  PLGM_INTERVAL_DELAY                 Time to pause between iterations (e.g. 5s, 1m)
  PLGM_CONCURRENCY                    Number of active workers
  PLGM_DOCUMENTS_COUNT                Initial seed document count
  PLGM_DROP_COLLECTIONS               Drop collections on start (true/false)
  PLGM_SKIP_SEED                      Do not seed initial data on start (true/false)
  PLGM_SEED_BATCH_SIZE                Number of documents to insert per batch during SEED phase
  PLGM_SEED_CONCURRENCY               Number of concurrent seed writers (default: 1)
  PLGM_DEBUG_MODE                     Enable verbose logic logs (true/false)
  PLGM_PPROF_ENABLED                  Enable pprof server on localhost:6060 (true/false)
  PLGM_USE_TRANSACTIONS               Enable transactional workloads (true/false)
  PLGM_MAX_TRANSACTION_OPS            Maximum number of operations to group into a single transaction block
  PLGM_RANDOM_SEED                    Seed for reproducible data and operations (0 = random)

 [Workload Record & Replay] (Off by default)
  PLGM_WORKLOAD_RECORD_ENABLED        Record every sent operation to an NDJSON file (true/false)
  PLGM_WORKLOAD_RECORD_PATH           Recording file (default: plgm_workload_record.ndjson; holds literal values)
  PLGM_WORKLOAD_RECORD_MAX_BYTES      Size cap, uncompressed; 0 = unlimited (default: 536870912)
  PLGM_WORKLOAD_RECORD_BACKPRESSURE   fail (default: stop recording) or block (workers wait for the writer)
  PLGM_WORKLOAD_RECORD_COMPRESSION    none (default), zstd, or gzip
  PLGM_WORKLOAD_RECORD_OUTCOMES       Store each op's latency, error code, and result summary (true/false)
  PLGM_WORKLOAD_RECORD_RESULT_CHECKSUMS Add an order-sensitive result checksum to find/aggregate outcomes (true/false)
  PLGM_WORKLOAD_REPLAY_ENABLED        Replay a recording instead of generating operations (true/false)
  PLGM_WORKLOAD_REPLAY_PATH           Recording file to replay
  PLGM_WORKLOAD_REPLAY_MODE           per_worker (default) or timed (keep recorded offsets)
  PLGM_WORKLOAD_REPLAY_ALLOW_MISMATCH Replay even if definitions differ from the recording (true/false)
  PLGM_WORKLOAD_REPLAY_OUTCOMES_PATH  Replay outcome file (needs outcomes on)
  PLGM_WORKLOAD_REPLAY_TARGETS        Multi-target: label=uri;label=uri (2-8 targets; uri is then not used)
  PLGM_WORKLOAD_REPLAY_TARGETS_MODE   sequential (default) or concurrent
  PLGM_WORKLOAD_REPLAY_OPS            all (default) or reads_only (skip and count writes)
  PLGM_WORKLOAD_DATA_FINGERPRINT      off (default), counts, or dbhash: starting-data check
  PLGM_WORKLOAD_DETERMINISTIC         Same generated workload for Duration, no recording (true/false)
  PLGM_OPERATIONS_PER_WORKER          One pass per worker; the workload then continues until Duration (required when deterministic)
  PLGM_WORKLOAD_DETERMINISTIC_OUTCOMES_PATH Deterministic outcome file (needs outcomes on)

 [Raw Injector Mode] (High Performance Hardware Test)
  PLGM_INJECTOR                       Enable Raw Injector mode (true/false)
  PLGM_INJECTOR_TYPE                  Operation: insert, upsert, update, delete, find, mixed
  PLGM_INJECTOR_SIZE                  Document size in bytes
  PLGM_INJECTOR_BATCH_SIZE            Operations per network batch (default: 1000)
  PLGM_INJECTOR_MAX_DOCS              Total documents to operate on (default: 10M)
  PLGM_INJECTOR_DROP                  Drop collection on start (true/false)
  PLGM_INJECTOR_DB                    Database name
  PLGM_INJECTOR_COLLECTION            Collection name

 [Operation Ratios] (Must sum to ~100)
  PLGM_FIND_PERCENT                   % of ops that are FIND
  PLGM_UPDATE_PERCENT                 % of ops that are UPDATE
  PLGM_INSERT_PERCENT                 % of ops that are INSERT
  PLGM_DELETE_PERCENT                 % of ops that are DELETE
  PLGM_AGGREGATE_PERCENT              % of ops that are AGGREGATE
  PLGM_TRANSACTION_PERCENT            % of ops that are TRANSACTIONAL
  PLGM_BULK_INSERT_PERCENT            % of ops that are BULK INSERTS

 [Performance Optimization]
  PLGM_FIND_BATCH_SIZE                Docs returned per cursor batch
  PLGM_INSERT_BATCH_SIZE              Number of docs in batch bulk insert
  PLGM_FIND_LIMIT                     Max docs per Find query
  PLGM_INSERT_CACHE_SIZE              Generator buffer size
  PLGM_OP_TIMEOUT_MS                  Soft timeout per DB op (ms)
  PLGM_RETRY_ATTEMPTS                 Retry attempts for failures
  PLGM_RETRY_BACKOFF_MS               Wait time between retries (ms)
  PLGM_STATUS_REFRESH_RATE_SEC        Status report interval (sec)
  GOMAXPROCS                          Go Runtime CPU limit
```

## What happens during a run

When executed, PLGM performs the following steps:

1.  **Initialization:** Connects to the database and loads collection/query definitions.
2.  **Setup:**
    * Creates databases and collections defined in your JSON files.
    * Creates indexes.
    * (Optional) Seeds initial data with the number of documents defined by `documents_count` in the config.
3.  **Workload Execution:**
    * Spawns the configured number of **Active Workers**.
    * Continuously generates and executes queries (Find, Insert, Update, Delete, Aggregate, Upsert) based on your configured ratios.
    * Generates realistic BSON data for Inserts and Updates (supports recursion and complex schemas).
    * Workers pick a random collection from the provided list for every operation, then route query execution using queries bound to that same database+collection.
4.  **Reporting:**
    * Outputs a real-time status report every N seconds (configurable).
    * Prints a detailed summary table at the end of the run.
    * Exports output to CSV (off by default; see [Reports and Exports](./reports-and-exports.md)).

### Sample Output

A 10-second run of the default workload with 4 workers and 5,000 seeded documents: the banner with active environment overrides, initialization, the per-second monitor, the summary, latency distribution, and workload accuracy.

![Headless CLI output of a 10-second default workload run](../images/cli_output.png)

## Interpreting the Output

Here is a sample of the real-time monitor output and a final summary, showing what the Ops/Sec metrics represent. This data is modeled after the flights workload used when `default_workload` is true.

### Real-Time Monitor Sample
While running a workload, PLGM prints a row every second (based on `status_refresh_rate_sec`).

```bash
> Starting Workload...

 TIME    | TOTAL OPS  | SELECT   | INSERT   | UPDATE   | DELETE   | AGG    | TRANS
 -------------------------------------------------------------------------------
 00:01   |      8,300 |    5,004 |      798 |    1,650 |      848 |      0 |      0
 00:02   |      8,048 |    4,736 |      773 |    1,694 |      845 |      0 |      0
 00:03   |      8,168 |    4,728 |      824 |    1,737 |      879 |      0 |      0
```
What this represents:

* TIME: The elapsed time since the workload started (MM:SS).
* TOTAL OPS: The combined number of all operations executed across all workers in that specific 1-second interval.
* SELECT/INSERT/UPDATE/DELETE/AGG: The raw count of each specific operation type completed in that second.
* TRANS: The number of successful transaction blocks completed in that second (reusing the CRUD operations above internally).

### Final Summary and Latency Sample
At the end of the run, PLGM calculates the overall averages and the latency distribution.

```bash
> Workload Finished.

  SUMMARY
  --------------------------------------------------
  Runtime:    10.00s
  Total Ops:  81,746
  Avg Rate:   8,174 ops/sec

  LATENCY DISTRIBUTION (ms)
  --------------------------------------------------
  TYPE             AVG          MIN          MAX          P95          P99
  ----             ---          ---          ---          ---          ---
  SELECT       1.24 ms      0.45 ms     15.20 ms      4.00 ms      9.00 ms
  INSERT      12.11 ms      4.10 ms     85.00 ms     66.00 ms     73.00 ms
  UPDATE       9.71 ms      3.20 ms     78.40 ms     65.00 ms     71.00 ms
  DELETE       9.60 ms      3.05 ms     76.20 ms     65.00 ms     72.00 ms
  TRANS       25.40 ms     12.00 ms    145.00 ms     95.00 ms    112.00 ms
```

What this represents:

* Avg Rate (Ops/Sec): The total throughput of the database cluster. It is calculated by dividing Total Ops by the total Runtime.
* AVG Latency: The average time (in milliseconds) it took the MongoDB driver to receive a response for that operation.
* P95/P99 (Percentiles): These are the most important metrics for performance tuning. P99 represents the worst case for 99% of your users. For example, if P99 SELECT is 9.00ms, 99% of your flight searches completed in under 9ms, while 1% took longer.
* TRANS Latency: This is typically higher than individual operations because a single transaction block contains 1 to X grouped operations, where X is defined by `max_transaction_ops` or `PLGM_MAX_TRANSACTION_OPS`.

The summary also includes a **WORKLOAD ACCURACY** section; see [Workloads](./workloads.md#workload-accuracy-metrics).
