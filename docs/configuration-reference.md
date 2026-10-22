# Configuration Reference

PLGM is configured through its [`config.yaml`](../config.yaml) file, which makes it easy to save and version-control your test scenarios. Every setting can also be set in the Web UI (see the guide for each feature) or overridden with an environment variable.

**Precedence:** environment variable, then `config.yaml`, then the built-in default. Values set in the Web UI apply to the run they start.

[Back to the main README](../README.md)

## Environment Variable Overrides

Environment variables are useful for CI/CD pipelines, Kubernetes deployments, or quick runtime adjustments without editing the file:

```bash
PLGM_CONCURRENCY=50 PLGM_DURATION=5m ./bin/plgm
```

The table lists every environment variable and the matching `config.yaml` setting. For the Web UI label of each setting, see the feature guides:
[Web UI](./web-ui.md), [Workloads](./workloads.md), [Load Modeling](./load-modeling.md), [Query and Index Analysis](./query-analysis.md), [Trends and History](./trends-and-history.md), [Record and Replay](./record-and-replay.md), [Reports and Exports](./reports-and-exports.md), [Raw Injector](./raw-injector.md), and [Performance Tuning](./performance-tuning.md).

| Config Setting | Environment Variable | Description | Example |
| :--- | :--- | :--- | :--- |
| **Connection** | | | |
| `uri` | `PLGM_URI` | Target MongoDB connection URI | `mongodb://user:pass@host:27017` |
| `direct_connection` | `PLGM_DIRECT_CONNECTION` | Force direct connection (bypass topology discovery) | `true` |
| `replicaset_name` | `PLGM_REPLICASET_NAME` | Replica Set name (required for sharded clusters/RS) | `rs0` |
| `read_preference` | `PLGM_READ_PREFERENCE` | By default, an application directs its read operations to the primary member in a replica set. You can specify a read preference to send read operations to secondaries. | `nearest` |
| `username` | `PLGM_USERNAME` |  Database User | `admin` |
| ***can not be set via config*** | `PLGM_PASSWORD` | Database Password (if not set, plgm will prompt) | `password123` |
| **Web UI** | | | |
| `enabled` | `PLGM_WEBUI_ENABLED` | Force launch the Web UI | `true` |
| `port` | `PLGM_WEBUI_PORT` | Port for the Web UI | `9999` |
| `tls_enabled` | `PLGM_WEBUI_TLS` | Serve the Web UI over HTTPS (default HTTP on loopback, no cert warnings) | `false` |
| `tls_cert_file` | `PLGM_WEBUI_TLS_CERT` | TLS certificate file for the Web UI (for warning-free HTTPS) | `/etc/ssl/plgm.crt` |
| `tls_key_file` | `PLGM_WEBUI_TLS_KEY` | TLS private key file for the Web UI | `/etc/ssl/plgm.key` |
| **Metrics Export** | | | |
| `csv_export_enabled` ||Continuously stream workload throughput metrics to a CSV file| `false` |
| `csv_export_append` ||If true, appends to the file. If false, overwrites it.| `false` |
| `csv_export_path` ||Path and metrics file name| `/tmp/plgm_metrics_export.csv` |
| **Post-Run Insights** | | | |
| `insights_enabled` | `PLGM_INSIGHTS_ENABLED` | Enable post-run slow-query/index analysis | `true` |
| `insights_sampling_rate` | `PLGM_INSIGHTS_SAMPLING_RATE` | Sample rate for captured operation events (`0.01`-`1.0`) | `0.10` |
| `insights_slow_threshold_ms` | `PLGM_INSIGHTS_SLOW_THRESHOLD_MS` | Latency threshold used to classify operations as slow | `200` |
| `insights_max_events` | `PLGM_INSIGHTS_MAX_EVENTS` | Max retained sampled events in memory | `5000` |
| `insights_max_groups` | `PLGM_INSIGHTS_MAX_GROUPS` | Max aggregated slow-shape groups | `300` |
| `insights_explain_enabled` | `PLGM_INSIGHTS_EXPLAIN_ENABLED` | Enable post-run explain sampling (needed for scan efficiency and sort/spill measurements; runs after the workload) | `true` |
| `insights_explain_top_n` | `PLGM_INSIGHTS_EXPLAIN_TOP_N` | Number of top slow shapes to attempt explain on | `5` |
| `insights_explain_max_time_ms` | `PLGM_INSIGHTS_EXPLAIN_MAX_TIME_MS` | Max server time per explain command | `1000` |
| `insights_explain_verbosity` | `PLGM_INSIGHTS_EXPLAIN_VERBOSITY` | Explain verbosity (`executionStats` recommended, `queryPlanner` lighter) | `executionStats` |
| `insights_explain_severity_mode` | `PLGM_INSIGHTS_EXPLAIN_SEVERITY_MODE` | Explain eligibility mode (`high_only`, `medium_only`, `critical_only`, `high_and_low`) | `high_only` |
| `insights_explain_efficiency_mode` | `PLGM_INSIGHTS_EXPLAIN_EFFICIENCY_MODE` | Extra Top N explain slots chosen by workload impact and latency tier, independent of slow threshold and severity (`impact_and_latency_tiers`, `off`) | `impact_and_latency_tiers` |
| `insights_explain_workers` | `PLGM_INSIGHTS_EXPLAIN_WORKERS` | Post-run explain worker count | `1` |
| `insights_explain_retries` | `PLGM_INSIGHTS_EXPLAIN_RETRIES` | Retry count for explain timeout/failure | `1` |
| `insights_explain_backoff_ms` | `PLGM_INSIGHTS_EXPLAIN_BACKOFF_MS` | Backoff between explain retries (ms) | `150` |
| `insights_latency_target_avg_ms`, `_p95_ms`, `_p99_ms` | `PLGM_INSIGHTS_LATENCY_TARGET_AVG_MS`, `_P95_MS`, `_P99_MS` | Per-shape client latency targets used only for the Query latency rating; 0 means no target (latency is shown but not rated) | `0` |
| **Trend History** | | | |
| `trend_history_runs` | `PLGM_TREND_HISTORY_RUNS` | Unpinned runs to keep per store (1-50); pinned runs (max 10) are extra | `5` |
| `trend_max_age_days` | `PLGM_TREND_MAX_AGE_DAYS` | Unpinned runs older than this are reported as outdated, not compared (`0` = never, max 3650) | `30` |
| `trend_baseline_store` | `PLGM_TREND_BASELINE_STORE` | Trend store file; empty = `<user config dir>/plgm/trend_baselines.json`, `off` disables history | `""` |
| `trend_default_comparison` | `PLGM_TREND_DEFAULT_COMPARISON` | Baseline for each shape: `previous` (previous compatible run) or `pinned` (newest pinned run) | `previous` |
| `trend_run_label` | `PLGM_TREND_RUN_LABEL` | Label for runs started with this config (max 64 bytes); empty = `mongo-<server version>`, or `run-<UTC time>` when unknown | `""` |
| `insights_trend_store_query_values` | `PLGM_INSIGHTS_TREND_STORE_QUERY_VALUES` | On: each run also stores the exact commands of each shape's explained samples (plus up to 8 retained samples), so Trends can check whether ordinary generated runs used the same query values; labels never change. To compare instances with identical operations, use [Record and Replay](./record-and-replay.md) and [Compare Replays](./record-and-replay.md#compare-replays) instead. The baseline file, exports, Download Summary, and HTML report then contain real query values. Off (default): only redacted shapes and metrics are stored | `false` |
| **Reproducible Workload (Record & Replay)** | | See [Record and Replay](./record-and-replay.md) | |
| `random_seed` | `PLGM_RANDOM_SEED` | Set (non-zero): the same seed, documents count, and collection definitions seed identical documents on any instance. 0 (default): time-based values that differ every run | `0` |
| `workload_record_enabled` | `PLGM_WORKLOAD_RECORD_ENABLED` | On: writes every operation sent, with its exact values, to the recording file (real query values; about 2 µs per operation; every iteration goes into the one file). Off (default): nothing is recorded | `false` |
| `workload_record_path` | `PLGM_WORKLOAD_RECORD_PATH` | NDJSON recording file (mode 0600; an existing non-recording file is never overwritten). Empty (default): `plgm_workload_record.ndjson` when recording is on | `""` |
| `workload_record_max_bytes` | `PLGM_WORKLOAD_RECORD_MAX_BYTES` | When the uncompressed size reaches this many bytes the recording stops and is marked truncated; the run continues. 0: unlimited. Unset (default): 512 MiB | `536870912` |
| `workload_record_backpressure` | `PLGM_WORKLOAD_RECORD_BACKPRESSURE` | `block`: workers wait for the writer, so every operation is recorded; throughput may drop, and the total and longest wait are reported. `fail` (default): the recording stops and is marked failed; the run continues at full speed | `fail` |
| `workload_record_compression` | `PLGM_WORKLOAD_RECORD_COMPRESSION` | `zstd` / `gzip`: compresses the file (typically 10–15x smaller; zstd is faster); replay and upload detect it. `none` (default): plain NDJSON | `none` |
| `workload_replay_enabled` | `PLGM_WORKLOAD_REPLAY_ENABLED` | On: no operations are generated; the recorded operations run on the recorded worker count until the file ends (duration, load profile, access pattern, think time, and iterations are ignored; every recorded iteration is replayed in order). Off (default): operations are generated | `false` |
| `workload_replay_path` | `PLGM_WORKLOAD_REPLAY_PATH` | Recording to replay; required when replay is on. Empty (default): nothing to replay | `""` |
| `workload_replay_mode` | `PLGM_WORKLOAD_REPLAY_MODE` | `timed`: each operation at its recorded offset, lag reported. `per_worker` (default): each worker's operations in order, as fast as possible | `per_worker` |
| `workload_replay_allow_mismatch` | `PLGM_WORKLOAD_REPLAY_ALLOW_MISMATCH` | On: replay even when definitions or namespaces differ from the recording, or it did not complete; results may not be comparable. Off (default): such a replay fails before any operation is sent | `false` |
| `workload_deterministic` | `PLGM_WORKLOAD_DETERMINISTIC` | On: every operation is generated from (seed, iteration, worker, operation number), so two runs with the same settings send the same commands and values without a recording; reports a traffic fingerprint. Needs a non-zero `random_seed`, `operations_per_worker`, `drop_collections` on, `skip_seed` off, and a fixed worker count. `duration` is how long the run lasts: workers repeat the workload until it ends, and a run that does not finish one pass is incomplete. Off (default): generated as usual until `duration` ends | `false` |
| `operations_per_worker` | `PLGM_OPERATIONS_PER_WORKER` | One pass of a deterministic workload. Each worker sends this many operations, then continues the same workload until `duration` ends. Required when deterministic traffic is on. 0 (default): no pass; workers run until `duration` ends | `0` |
| `workload_record_outcomes` | `PLGM_WORKLOAD_RECORD_OUTCOMES` | On: keeps each operation's client latency, ok or error code and name (never the message), and result summary, in the recording or an outcome file; commands, stats, and trends are unchanged. Off (default): no outcomes | `false` |
| `workload_record_result_checksums` | `PLGM_WORKLOAD_RECORD_RESULT_CHECKSUMS` | On: adds an order-sensitive sha256 of each find/aggregate result to its outcome (about 5 µs of worker CPU per returned document); needs outcomes on. Off (default): counts only | `false` |
| `workload_replay_outcomes_path` | `PLGM_WORKLOAD_REPLAY_OUTCOMES_PATH` | Set: a replay writes its outcomes to this NDJSON file (0600), keyed by the recording's sequence numbers; required for outcomes during a replay. Empty (default): none | `""` |
| `workload_deterministic_outcomes_path` | `PLGM_WORKLOAD_DETERMINISTIC_OUTCOMES_PATH` | Set: a deterministic run writes its outcomes to this NDJSON file (0600), keyed by (iteration, worker, operation number) and stamped with the traffic fingerprint. Empty (default): none | `""` |
| `workload_replay_ops` | `PLGM_WORKLOAD_REPLAY_OPS` | `reads_only`: replays finds and read-only aggregates and skips every write (inserts, updates, deletes, transactions with a write, aggregates with `$out`/`$merge`), so the target keeps its starting data; skipped writes are counted and reported, and the commands hash is not compared. `all` (default): replays every recorded operation, including writes | `all` |
| `workload_data_fingerprint` | `PLGM_WORKLOAD_DATA_FINGERPRINT` | `counts` / `dbhash`: a recording stores each collection's document count and index list (plus the `dbHash` collection hash) before the first operation; every replay of it compares them with its target first and fails on a difference (warns with allow mismatch). `dbhash` needs the dbHash privilege, does not run through mongos, and matches only byte-identical copies (snapshot or mongodump restore). A replay always checks a recording that has a fingerprint. `off` (default): nothing stored or checked | `off` |
| `workload_replay_targets` | `PLGM_WORKLOAD_REPLAY_TARGETS` (`label=uri;label=uri`), CLI `--replay-target label=uri` (repeat) | Listed (2–8 targets): the replay or deterministic run runs once on every listed target, and only on those (`uri` is not used); each gets its own client, workers, stats, outcome file (`outcomes.<label>.ndjson`), insights, and trend run, and is compared with the first target. Needs replay or deterministic traffic, and outcomes with an outcome file. Empty (default): a single run on `uri` | `[]` |
| `workload_replay_targets_mode` | `PLGM_WORKLOAD_REPLAY_TARGETS_MODE`, CLI `--replay-targets-mode` | `concurrent`: targets are prepared one at a time, then all run at once from this host; they share its CPU, memory, and network, so latencies may be affected, and the report shows each target's client-side lag. `sequential` (default): one target after another | `sequential` |
| **Sharding Strategy** | | | |
| `sharding_mode` | `PLGM_SHARDING_MODE` | Per-collection sharding behavior: `auto`, `force_on`, `force_off` | `auto` |
| `sharding_skip_generic_without_config` | `PLGM_SHARDING_SKIP_GENERIC_WITHOUT_CONFIG` | Skip generic sharding setup for collections without `shardConfig` (recommended for mixed workloads) | `true` |
| **Workload Control** | | | |
| `concurrency` | `PLGM_CONCURRENCY` | Number of active worker goroutines | `50` |
| `duration` | `PLGM_DURATION` | Test duration (Go duration string) | `5m`, `60s` |
| `iterations` | `PLGM_ITERATIONS` | Number of times to repeat the workload | `1` |
| `interval_delay` | `PLGM_INTERVAL_DELAY` | Pause after each iteration except the last (e.g. 5s, 1m). The header total includes these pauses: 5 × 30s with 10s between runs is 03:10 | `0s` |
| `default_workload` | `PLGM_DEFAULT_WORKLOAD` | Use built-in "Flights" workload (`true`/`false`) | `false` |
| `collections_path` | `PLGM_COLLECTIONS_PATH` | Path to custom collection JSON files (supports directories for multi-collection load) | `./schemas` |
| `queries_path` | `PLGM_QUERIES_PATH` | Path to custom query JSON files or directory. | `./queries` |
| `documents_count` | `PLGM_DOCUMENTS_COUNT` | Number of documents to seed initially | `10000` |
| `drop_collections` | `PLGM_DROP_COLLECTIONS` | Drop collections before starting (`true`/`false`) | `true` |
| `skip_seed` | `PLGM_SKIP_SEED` | Do not seed initial data on start (`true`/`false`) | `true` |
| `seed_batch_size` | `PLGM_SEED_BATCH_SIZE` | Number of documents to insert per batch during SEED phase | `1000` |
| `seed_concurrency` | `PLGM_SEED_CONCURRENCY` | Concurrent seed writers, independent of workload concurrency | `1` |
| `debug_mode` | `PLGM_DEBUG_MODE` | Enable verbose debug logging (`true`/`false`) | `false` |
| `pprof_enabled` | `PLGM_PPROF_ENABLED` | Enable pprof server on localhost:6060 (`true`/`false`) | `false` |
| `use_transactions` | `PLGM_USE_TRANSACTIONS` | Enable Transactional Workloads (`true`/`false`) | `false` |
| `max_transaction_ops` | `PLGM_MAX_TRANSACTION_OPS` | Maximum number of operations to group into a single transaction block | `5` |
| **Operation Ratios** | | (Must sum to ~100) | |
| `find_percent` | `PLGM_FIND_PERCENT` | Percentage of Find operations | `60` |
| `insert_percent` | `PLGM_INSERT_PERCENT` | Percentage of Insert operations (this is not related to the initial seed inserts) | `5` |
| `bulk_insert_percent` | `PLGM_BULK_INSERT_PERCENT` | Percentage of Bulk Insert operations (this is not related to the initial seed inserts) | `5` |
| `update_percent` | `PLGM_UPDATE_PERCENT` | Percentage of Update operations | `20` |
| `delete_percent` | `PLGM_DELETE_PERCENT` | Percentage of Delete operations | `10` |
| `aggregate_percent` | `PLGM_AGGREGATE_PERCENT` | Percentage of Aggregate operations | `0` |
| `transaction_percent` | `PLGM_TRANSACTION_PERCENT` | Percentage of Transactional operations | `0` |
| **Performance Optimization** | | | |
| `find_batch_size` | `PLGM_FIND_BATCH_SIZE` | Documents returned per cursor batch | `100` |
| `insert_batch_size` | `PLGM_INSERT_BATCH_SIZE` | Number of documents per insert batch | `100` |
| `find_limit` | `PLGM_FIND_LIMIT` | Hard limit on documents per Find query | `10` |
| `insert_cache_size` | `PLGM_INSERT_CACHE_SIZE` | Size of the document generation buffer | `1000` |
| `op_timeout_ms` | `PLGM_OP_TIMEOUT_MS` | Soft timeout for individual DB operations (ms) | `500` |
| `retry_attempts` | `PLGM_RETRY_ATTEMPTS` | Number of retries for transient errors | `3` |
| `retry_backoff_ms` | `PLGM_RETRY_BACKOFF_MS` | Wait time between retries (ms) | `10` |
| `status_refresh_rate_sec` | `PLGM_STATUS_REFRESH_RATE_SEC` | How often to print stats to console (sec) | `5` |

Additional settings not listed above:

- Load profiles, think time, and access patterns: [Load Modeling](./load-modeling.md) (`load_profile`, `think_time_ms`, `think_jitter_ms`, `access_pattern`, and their `PLGM_LOAD_PROFILE_*`, `PLGM_THINK_*`, `PLGM_ACCESS_PATTERN_*` overrides).
- Connection pool and transport: [Performance Tuning](./performance-tuning.md#connection-pooling) (`max_pool_size`, `min_pool_size`, `max_idle_time`).
- Driver URI options and TLS client files: [Performance Tuning](./performance-tuning.md#custom-connection-parameters-custom_params) (`custom_params`, `connection_params`).
- Raw Injector: [Raw Injector](./raw-injector.md) (`raw_injector`, `PLGM_INJECTOR_*`).
- Record pool tuning: [Workloads](./workloads.md#query-accuracy-targeting-existing-records) (`existing_record_hit_rate`, `record_pool_max_size`, `record_pool_bootstrap_sample`).
- Definition store location: `PLGM_DEFINITIONS_STORE` ([Workloads](./workloads.md#where-definitions-are-stored)).
- CPU limit: `GOMAXPROCS` ([Performance Tuning](./performance-tuning.md#cpu-utilization-gomaxprocs)).

For the full list of CLI flags, run `./plgm --help` or see [Running from the Command Line](./cli.md#cli-usage).
