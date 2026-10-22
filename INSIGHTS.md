# Post-Run Insights: Slow Query and Index Analysis

This guide explains how PLGM's post-run insights work, how to configure them, and what outcomes to expect.

## What This Feature Does

After a workload fully finishes, PLGM analyzes sampled operation events and produces:
- ranked slow query groups
- affected collections and shape hotspots
- potential index issues with confidence labels
- explain-backed diagnostics
- recommended next actions

### Explain requirement

Post-Run Explain Sampling (`insights_explain_enabled`, Web UI settings tab: `Query Analysis (Explain)`) must be on to use the query analysis in full. It is on by default.

| Result | Explain on | Explain off |
|---|---|---|
| Latency, workload impact, slow query groups | Yes | Yes |
| Scan efficiency ratings | Yes | Unassessed |
| Execution overhead (in-memory sorts, disk spills, sort/spill measurements) | Yes | Not assessed |
| Plan-based index recommendations and candidate keys | Yes | No |
| Scan efficiency trends | Yes | No efficiency comparison |

When explain is off, the dashboard (Insights and Query & Index Analysis tabs), the HTML report, and Download Summary (`efficiency_view.notice`) state that it was turned off and how to enable it. Explain runs only after the workload completes; see the defaults below for its budget and cost.

Insights are available in three places:
- Dashboard panel: `POST-RUN SLOW QUERY & INDEX ANALYSIS`
- API: `GET /api/insights`
- `Download Summary` export JSON: `insights` section

All three use the same backend report model.

## Lifecycle and States

Insights are post-run by design.

`metadata.status` values:
- `inactive`: no collector/run context
- `pending`: workload still running
- `ready`: final report is available
- `empty`: run completed but no sampled events survived filters
- `disabled`: insights disabled by config

Expected behavior:
- During `pending`, the panel stays visible with a clear waiting message.
- Final findings are shown only when status becomes `ready`.
- Starting a new run clears the previous run's insights cache.

## Defaults and Recommended Starting Point

Current defaults:
- `insights_enabled: true`
- `insights_sampling_rate: 0.10`
- `insights_slow_threshold_ms: 200`
- `insights_max_events: 5000`
- `insights_max_groups: 300`
- `insights_explain_enabled: true`
- `insights_explain_top_n: 5`
- `insights_explain_max_time_ms: 1000`
- `insights_explain_verbosity: executionStats`
- `insights_explain_severity_mode: high_only`
- `insights_explain_efficiency_mode: impact_and_latency_tiers`
- `insights_explain_workers: 1`
- `insights_explain_retries: 1`
- `insights_explain_backoff_ms: 150`

Recommended baseline for most users:
- Keep explain enabled (the default): scan efficiency, sort/spill measurements, and index advice need it. Explain runs only after the workload ends (at most `2 x insights_explain_top_n` shapes, 2 samples each, `insights_explain_max_time_ms` per command, one worker) and never during the measured window. During the run, representative-sample capture adds well under 100 ns per operation (measured once with `BenchmarkRecordOperationEventWithCapture`).
- Disable it (`insights_explain_enabled: false`) when the target cluster must receive no reads beyond the workload or the user lacks explain privileges.
- Keep explain workers low (`1`) and top-N small (`3-5`).

## Configuration Reference

Web UI settings tabs:
- `Metrics & Insights`: insights capture (`insights_enabled`, sampling, slow threshold, retention) and CSV export
- `Query Analysis (Explain)`: post-run explain (`insights_explain_*`) and query latency targets (`insights_latency_target_*`)
- `Trends & History`: trend history settings (`trend_*`, `insights_trend_store_query_values`), the stored run list (rename, pin, delete), export/import, and reset. See [docs/trends-and-history.md](./docs/trends-and-history.md) for the settings table.
- `Record & Replay`: Reproducible Workload settings, grouped as Seed & Deterministic Traffic, Record, Replay, Multi-Target Replay & Starting Data, Outcome Capture, and Import Captured Traffic: `random_seed`, `workload_record_*`, `workload_replay_*`. See [docs/record-and-replay.md](./docs/record-and-replay.md). `workload_record_backpressure`: `block` makes workers wait for the writer so every operation is recorded (throughput may drop; the waits are reported); `fail` (default) stops the recording instead. `workload_record_compression`: `zstd`/`gzip` compress the file, `none` (default) does not. `workload_record_max_bytes: 0` is unlimited. Recordings may span several iterations. Run metadata `recording` in `/api/insights` reports the policy, sizes, compression ratio, iterations, and waits; it never holds literal values. Deterministic Generated Traffic: `workload_deterministic` (default off) with `operations_per_worker` generates each operation from (seed, iteration, worker, operation number). `duration` is the length of the run: each worker sends one pass of `operations_per_worker`, then continues the same workload until `duration` ends. Run metadata `generated_traffic` holds the traffic fingerprint of that first pass, status (`complete` when every worker finished the pass, `incomplete` with a reason when `duration` ended first), planned/generated/sent counts (`ops_sent` can exceed `ops_planned`), and per-worker hashes, never values. See [docs/record-and-replay.md](./docs/record-and-replay.md#deterministic-generated-traffic). Outcome Capture: `workload_record_outcomes` (default off) keeps each operation's client latency, ok or error code and name (never the message), and result summary; `workload_record_result_checksums` (default off, needs outcomes) adds an order-sensitive sha256 of find/aggregate results; `workload_replay_outcomes_path` / `workload_deterministic_outcomes_path` (default empty) name the 0600 NDJSON outcome files. Run metadata `outcome_file` reports the path, count, status, and hashes only; outcomes never reach stats, insights, trends, or Download Summary. See [docs/record-and-replay.md](./docs/record-and-replay.md#operation-outcomes). Multi-Target Replay & Starting Data: `workload_replay_ops` (`all` default; `reads_only` skips and counts writes, so the target keeps its starting data); `workload_data_fingerprint` (`off` default; `counts` stores each collection's document count and index list in the recording, `dbhash` adds the dbHash collection hashes, which match only byte-identical copies such as a snapshot or mongodump restore; every replay of such a recording checks its target first and fails on a difference, or warns with allow mismatch); `workload_replay_targets` (empty default; 2–8 `{label, uri}` targets, only those are used, `uri` is not) and `workload_replay_targets_mode` (`sequential` default; `concurrent` runs all targets at once from one host, so latencies may be affected and each target's client-side lag is reported). Each target gets its own collector, outcome file, insights, and trend run; run metadata `target` (label, index, reference, mode) and `replay.ops_filter` / `ops_skipped` / `data_fingerprint` (match and differences: counts and index names only) carry no URIs or values. See [docs/record-and-replay.md](./docs/record-and-replay.md#multi-target-replay).
- `Query Analysis > Replay Comparison` (and `plgm compare-replays a b`): compares two outcome files of the same operations (same recording header hash, or same traffic fingerprint for deterministic runs; others are refused unless a mismatch is allowed, with a warning). Per shape and overall: operation counts, latency avg/p50/p95/p99 per side with the change, result count/checksum differences, errors on one side by code, missing operations, and the first N differing operations side by side (command and literal values only when a recording is given; the report is then marked). Result and error differences are facts; latency verdicts reuse the Trends thresholds and are heuristics. Plan changes come from two chosen trend runs and are annotations only. Reports stay in server memory (newest 10) and download as HTML or JSON. See [docs/record-and-replay.md](./docs/record-and-replay.md#compare-replays).

YAML keys and environment variables:

| YAML key | Environment variable | Purpose |
| :--- | :--- | :--- |
| `insights_enabled` | `PLGM_INSIGHTS_ENABLED` | Enable post-run insights report |
| `insights_sampling_rate` | `PLGM_INSIGHTS_SAMPLING_RATE` | Event sampling rate (`0.01-1.0`) |
| `insights_slow_threshold_ms` | `PLGM_INSIGHTS_SLOW_THRESHOLD_MS` | Slow cutoff used for filtering |
| `insights_max_events` | `PLGM_INSIGHTS_MAX_EVENTS` | Max sampled events retained in memory |
| `insights_max_groups` | `PLGM_INSIGHTS_MAX_GROUPS` | Max aggregated groups retained |
| `insights_explain_enabled` | `PLGM_INSIGHTS_EXPLAIN_ENABLED` | Enable post-run explain enrichment |
| `insights_explain_top_n` | `PLGM_INSIGHTS_EXPLAIN_TOP_N` | Max top groups eligible for explain |
| `insights_explain_max_time_ms` | `PLGM_INSIGHTS_EXPLAIN_MAX_TIME_MS` | Server `maxTimeMS` for explain command |
| `insights_explain_verbosity` | `PLGM_INSIGHTS_EXPLAIN_VERBOSITY` | `executionStats` (default) or `queryPlanner` |
| `insights_explain_severity_mode` | `PLGM_INSIGHTS_EXPLAIN_SEVERITY_MODE` | Explain eligibility by severity |
| `insights_explain_efficiency_mode` | `PLGM_INSIGHTS_EXPLAIN_EFFICIENCY_MODE` | `impact_and_latency_tiers` (default) adds Top N explain slots chosen from every tracked shape regardless of slow threshold/severity; `off` keeps severity slots only. Results are in `explain_coverage`. |
| `insights_explain_workers` | `PLGM_INSIGHTS_EXPLAIN_WORKERS` | Post-run explain worker count |
| `insights_explain_retries` | `PLGM_INSIGHTS_EXPLAIN_RETRIES` | Retries for explain timeout/failure |
| `insights_explain_backoff_ms` | `PLGM_INSIGHTS_EXPLAIN_BACKOFF_MS` | Retry backoff between explain attempts |
| `insights_latency_target_avg_ms`, `_p95_ms`, `_p99_ms` | `PLGM_INSIGHTS_LATENCY_TARGET_AVG_MS`, `_P95_MS`, `_P99_MS` | Query latency targets in ms (default 0, no target). A shape is `above_target` if a measured statistic exceeds its target and `within_target` only if every set target was measured and met; otherwise `not_assessed`. |
| `insights_trend_store_query_values` | `PLGM_INSIGHTS_TREND_STORE_QUERY_VALUES` | On: each run also stores the exact commands of each shape's explained samples (plus up to 8 retained samples), so Trends can check whether ordinary generated runs used the same query values; this never changes the improved/regressed labels. To compare instances with identical operations, use Record & Replay instead (two replays of one recording compare as `same_values` with source `same_recording`, without stored literals) and compare their outcome files with `plgm compare-replays` or Query Analysis > Replay Comparison. The baseline file, exports, Download Summary, and HTML report then contain real query values (up to 16 KiB per sample, 1 MiB per run). Off (default): only redacted shapes and metrics are stored. |

`insights_explain_severity_mode` accepted values:
- `high_only` (default): explain only `high` + `critical`
- `medium_only`: explain `medium` + `high` + `critical`
- `critical_only`: explain only `critical`
- `high_and_low`: explain all severities

## Filtering Rules (Very Important)

To keep output actionable and reduce noise:
- Items below `insights_slow_threshold_ms` are filtered out.
- Filtered-out items are not shown in dashboard findings.
- Filtered-out items are not included in exported insights.
- Severity explain slots are chosen after filtering, and severity eligibility applies to them. Efficiency explain slots ignore these filters (see below).

Transparency counters are exposed in metadata and shown in UI status line:
- `filtered_by_threshold`
- `filtered_by_severity`

## Workload Impact View

The filtering rules above apply to the severity view (`slow_queries`, `query_shapes`). The `impact` object is separate and is never filtered by threshold, severity, or explain eligibility.

- Every completed operation updates a per-shape aggregate keyed by operation, database, collection, and shape. This happens before sampling and retention, so fast operations count, and buffer overwrites and the sampling rate do not change the totals.
- Each group reports `operation_count`, `total_operation_ms`, `avg_ms`, `max_ms`, `error_count`, and `share_of_observed_time`. Groups are ranked by `total_operation_ms`.
- `total_operation_ms` is **summed concurrent operation time**: the sum of client-observed latencies. It is not wall-clock time and not server CPU time.
- `sample_count` and `sample_p95_ms` come only from retained sampled events and are separate from the full-run counts. Below 20 samples, `sample_p95_ms` is omitted and `sample_percentile_status` is `insufficient_samples`. Totals are never extrapolated from samples.
- At most `insights_max_groups` shapes are tracked. Operations in additional shapes are counted in `coverage.overflow_operations` and `coverage.overflow_operation_ms`, and they remain in observed totals and shares. In that case `group_coverage_complete` is `false`.
- The top 20 groups are listed. `coverage.not_shown_*` and `coverage.limitations` disclose the rest, a wrapped sample buffer, and sampling bias.
- `explanation` states only measured facts. It does not identify a cause and it is not index advice.

The same values appear in `/api/insights`, the dashboard's Workload Impact section, Download Summary (which embeds the API payload), and the HTML report (`/api/report`).

## Retention/Sampling vs Explain: How They Interact

Think of this as a two-stage pipeline:

1. Insights Capture (Retention & Sampling)
- Controls whether operation events are recorded at all (`insights_enabled`).
- Controls sampling rate (`insights_sampling_rate`) and retention caps (`insights_max_events`, `insights_max_groups`).
- Determines memory footprint and the amount of post-run evidence available.
- Applies slow-threshold filtering (`insights_slow_threshold_ms`) for report relevance.

2. Explain Enrichment (Post-Run Explain Analysis)
- Runs only after workload completion.
- Selects shapes in two groups of `insights_explain_top_n` slots. Severity slots take shapes from the severity-filtered slow view, as before. Efficiency slots (`insights_explain_efficiency_mode: impact_and_latency_tiers`) take shapes from every tracked shape, regardless of the slow threshold or severity filter: they alternate between the highest remaining workload impact and a round-robin over full-run average latency tiers (`lt_1ms`, `1_10ms`, `10_100ms`, `100ms_1s`, `ge_1s`), fastest first, so fast, low-impact shapes can be assessed. A shape that was not selected is unassessed, not efficient.
- Explains each selected shape from its own representative samples (the per-shape reservoir in `query_capture`), picking those nearest the samples' median and p95 latency (at most 2 per shape). Identical commands run once. `explain_selection.scope` is `all_operations`, `full_run_uniform_reservoir`, or `full_run_reservoir_with_drops`.
- `explain_coverage` reports every tracked shape with its status (`explained`, `failed`, `cancelled`, `unavailable`, `excluded`, `not_supported`), reason, selection slot, latency tier, and evidence (`execution_stats` or partial `planner_only`), plus selected/completed/failed/cancelled/unavailable/excluded counts, budgets, sampling scope counts, capture state, unrecorded paths, and limitations.
- Sort specifications loaded from query definitions keep their declared key order, so multi-key `$sort` stages are explained with the same key order that was executed. A multi-key `$sort` held in an unordered map (only possible for definitions built in Go code, which query validation now rejects) is not explained (`sort_key_order_undefined`).
- Starting a new run cancels in-flight post-run explains; unsent commands are reported as `cancelled`.
- Explains the query exactly as executed (namespace, filter, projection, limit, update/upsert, pipeline). Updates and deletes are explained with their own `update`/`delete` commands, which never apply writes; pipelines with `$out`/`$merge` are explained with `queryPlanner` only. When the executable query was not captured, the shape is marked `insufficient_metadata` with a reason instead of explaining a placeholder.
- Explain timeouts and failures are inconclusive and keep heuristic guidance. Client latency (measured during the run) is reported separately from server explain timing; a difference alone is not treated as a root cause.
- Builds `explain_finding`, index-issue evidence, and recommendations after explain, citing namespace, shape, examined/returned metrics, sort/spill signals, indexes used, and sample counts. Confidence comes from evidence quality (execution metrics, agreeing samples); COLLSCAN alone is not treated as proof of an indexing problem.

Practical dependency:
- Explain does not run during workload execution.
- Explain needs `insights_enabled`; with it off, no representative samples are kept.
- Representative samples are kept before sampling and retention, so `insights_sampling_rate` and `insights_max_events` do not limit which shapes can be explained. Only shapes tracked within `insights_max_groups` can be explained.
- A shape whose executable query was not captured is marked `insufficient_metadata`.

Common operating modes:
- Sampling ON, Explain OFF:
  - Best for lightweight post-run profiling and benchmark fidelity.
- Sampling ON, Explain ON:
  - Best for troubleshooting and actionable plan diagnostics.
- Sampling OFF, Explain ON:
  - Not useful in practice; explain has no retained evidence to enrich.
- Sampling OFF, Explain OFF:
  - Disables insights entirely.

## Explain Mode Details

Explain runs only after workloads complete.

Design guarantees:
- Explain does not execute inside the measured workload loop.
- Explain impact is classified as post-run only.
- Fallback heuristics are used when explain is unavailable.

Verbosity options:
- `executionStats` (default): richer troubleshooting evidence
- `queryPlanner`: lighter, but less actionable runtime evidence

What `executionStats` can provide when available:
- `execution_time_millis`
- `total_docs_examined`
- `total_keys_examined`
- `n_returned`
- index and scan stage signals
- examined-to-returned and keys-to-returned ratios
- stage chain and recommendation context

## Explain Support Matrix

Currently explain-supported:
- `find`
- `aggregate`
- `updateOne`
- `updateMany`
- `deleteOne`
- `deleteMany`

Currently not supported:
- `insert`
- `insertMany`
- `transaction`
- other non-query-planner operations

## Explain Status and Reason Fields

Each analyzed shape can include:
- `explain_status`
- `explain_reason`

`explain_status` values:
- `explained`
- `explain_unavailable`
- `not_supported`
- `insufficient_metadata`
- `timed_out`
- `execution_failed`

Common `explain_reason` examples:
- `explain_disabled`
- `explain_not_attempted_yet`
- `not_selected_top_n_<N>`
- `filtered_by_threshold`
- `filtered_by_severity_mode_<mode>`
- `missing_filter_sample`
- `missing_pipeline_sample`
- `max_time_exceeded`
- `connect_failed`
- `not_authorized`
- `namespace_not_found`
- `run_command_failed`

## Reading the Dashboard Output

Quick triage order:
1. Check top finding and severity badge.
2. Check explain status badge and reason text.
3. Review plan/evidence line (`index used`, `collscan`, docs/keys/returned, ratios).
4. Read interpretation and `next step` recommendation.
5. Use affected collections and iteration/timeline tabs for blast radius and timing context.

## Findings UX (Current)

The post-run findings experience is intentionally split into:

1. Main findings cards (overview surface)
- Concise, high-signal cards for slow queries and index issues.
- Focused on title, where-found context, key metrics, priority/confidence/explain status.
- Designed for quick scanning and triage.

2. Explore drawer (technical deep dive)
- Open with the **Explore** action on any finding card.
- Opens a right-side details panel.
- Always defaults to **Summary** when opened.
- Includes section toggles:
  - `Summary`
  - `Execution Plan`
  - `Recommendation`
  - `Diagnostics`
  - `Query Reference`
- Supports `Prev` / `Next` navigation across findings of the current list.
- Supports `Esc` key to close.

Each slow-query row can show:
- latency stats (`avg/p95/p99/max`) and count
- query reference block (`query_label`, `query_source`, `workload_name`, `query_file`, definition id/index)
- representative query/pipeline preview for quick identification
- shape identity
- explain diagnostics (`db`, verbosity, timeout budget, stage summary)
- plan summary and evidence ratios
- interpretation and confidence-weighted next step

The `Copy diagnostics` button copies a structured JSON payload for ticketing/triage.

## Execution Plan Visualization

Inside the Explore drawer, execution plans are rendered as a stage-flow visualization (pill/arrow chain), for example:
- `IXSCAN -> FETCH -> GROUP -> SORT -> LIMIT`

Risk cues are highlighted in the flow:
- high risk: collection-scan signals (`COLLSCAN`)
- medium risk: common bottlenecks (for example blocking sort, fetch after index)

Raw diagnostics remain available in an expandable technical section for deep troubleshooting, but are not the primary surface.

## Query Traceability (Finding the Exact Query to Optimize)

Insights now carry query-reference metadata so users can map findings back to workload definitions quickly.

For each slow-query/index entry, look for:
- `query_label`
- `query_source`
- `workload_name`
- `query_file`
- `query_definition_id`
- `query_definition_index`
- `representative_query_summary`
- `representative_pipeline_summary`

How these fields are populated:
- File-based workload queries: source file and definition index are captured.
- Uploaded UI query files: uploaded filename and definition index are captured.
- Generated runtime/fallback paths: source is marked as runtime-generated.
- If a JSON query has `id`/`label`/`name`, those are used as primary user-facing identifiers.

Important grouping behavior:
- Insights aggregate by normalized shape.
- Multiple query definitions can collapse into the same shape.
- When this happens, the UI marks it as multiple query definitions and shows a variants count.

## Download Summary Parity

`Download Summary` embeds the same insights object returned by `/api/insights` at completion.

Practical meaning:
- UI and export should match for the same completed run.
- Export contains the same filtering outcomes, explain statuses, and recommendations.
- Export includes the same query-reference metadata shown in the UI.
- Password redaction and existing summary protections remain unchanged.
- Literal query values appear only for trend runs stored with `insights_trend_store_query_values` on (marked as such). Recordings, outcome files, and replay comparison reports are never embedded; Download Summary carries only their metadata and hashes (`recording`, `replay`, `generated_traffic`, `outcome_file`).

## Practical Use Cases

1. Benchmark triage after a run
- Identify top `critical/high` slow groups quickly.

2. Index investigation backlog
- Use explain-backed findings first, heuristic findings second.

3. Regression checks across runs
- Use the Trends view (`trends` in `/api/insights`, tc-2). The store keeps a history of runs (`trend_history_runs`, default 5 unpinned, plus up to 10 pinned reference runs), each with a label (`trend_run_label`, default `mongo-<server version>` or `run-<UTC time>`). Each shape is compared separately for scan efficiency, full-run average latency, and share of observed operation time. By default (`trend_default_comparison: previous`) the baseline is the previous compatible run: for each shape and metric, the newest earlier snapshot that passes the gates (efficiency prefers assessed evidence), so the efficiency and latency baselines can come from different runs. `trends.comparison` says which mode was used and `trends.rows[].baseline` names the run(s). `pinned` compares every shape with the newest pinned run, falling back to previous with a note when none is pinned.
- Compare with another run without rerunning: `GET /api/trends/compare?baseline=previous|pinned|<run_id>` returns the same view for the last finished run. With `?runs=<id>,<id>` (up to 8) it returns a matrix (`columns`, one per run, current first; `rows[].cells` with `avg_ms`, `sample_p95_ms`, `score`, `impact_share`, and separate `efficiency`/`latency`/`impact` states). Each pair is gated on its own; a cell that is not comparable has `comparability` and `reasons` (for example `shape_identity_version_changed`, `baseline_outdated`, `not_in_baseline_run`).
- A label is given only when the baseline has the same shape identity and scan efficiency version (baselines recorded as `qe-1` use the same formula and compare with `se-1`), is not outdated (unpinned runs older than `trend_max_age_days`, default 30, `0` = never; pinned runs never expire), and both runs meet the published evidence minimums (`trends.criteria`); otherwise the state is `insufficient_evidence`, `not_comparable`, or `no_baseline` with reasons. Plan, index, server, sampling, and workload changes are annotated, not treated as causes. The thresholds are heuristics, not significance tests. Shape identity `si-3` renders multi-key sorts in declared order, so baselines recorded with `si-2` or earlier are `not_comparable` (`shape_identity_version_changed`) until a new baseline is recorded.
- Share runs between machines with export/import (Trends & History tab, `/api/trend-baselines/export` and `/import`). Imports are merged by `run_id`; duplicates are skipped and the imported runs keep their source label. Only redacted trend fields are read.

4. Reporting and incident artifacts
- Export final summary JSON with `insights` for sharing with DBAs/SREs.

5. Explain budget control
- Keep explain scoped with threshold + severity mode + top-N limits.

6. Comparing two instances (upgrade testing)
- Trends compare shapes across runs heuristically; they cannot tell whether two runs sent the same operations. For an exact A-vs-B comparison, send identical operations to both instances with Record & Replay, Deterministic Generated Traffic, or imported traffic, capture per-operation outcomes, and compare the outcome files with `plgm compare-replays` or Query Analysis > Replay Comparison ([docs/record-and-replay.md](./docs/record-and-replay.md#comparing-two-instances-upgrade-testing)).
- In Trends, each row carries a values annotation (`same_values`, `overlapping`, `different_values`, `not_recorded`). Two replays of the same recording (source `same_recording`) or two runs with the same generated-traffic fingerprint (source `same_generated_traffic`) are `same_values` without stored literals; ordinary generated runs need `insights_trend_store_query_values` on. The annotation never changes improved/regressed labels or comparability.

## Troubleshooting

If findings seem too sparse:
- Increase sampling rate.
- Increase retained events/groups.
- Ensure run duration has enough operation volume.

If many items are filtered out:
- Lower `insights_slow_threshold_ms` carefully.
- Expand severity mode from `high_only` to `medium_only` or `high_and_low`.

If explain times out:
- Increase `insights_explain_max_time_ms`.
- Keep workers low first (`1`).
- Use modest retries (`1-2`) with non-zero backoff.

If explain is unavailable:
- Confirm explain is enabled.
- Increase top-N if needed.
- Ensure metadata retention is sufficient (sampling/events).
- Verify database permissions for explain commands.

## Query Assessment: Supported Scope

The Query Assessment table shows each dimension in its own column, with its own explanation and version. They are never combined into one score. The scan efficiency, execution overhead, and `$lookup` columns require Post-Run Explain Sampling (see "Explain requirement"); query latency and workload impact do not.

- Scan efficiency (`se-1`, 0–100, higher is better): documents examined per result, index keys examined per result, and filtered collection scans, from post-run explains. Planner-only evidence (`$out`/`$merge`, `queryPlanner` verbosity) is an upper bound (`≤ N`), never `efficient`. Unselected, timed-out, failed, or uncaptured shapes are `Unassessed` with a reason.
- Query latency (`lr-1`): client-side average and maximum over every operation, p95 from at least 20 sampled operations and p99 from at least 100. It is rated only against `insights_latency_target_avg_ms`, `_p95_ms`, and `_p99_ms`: `above_target` if any measured statistic exceeds its target, `within_target` only if every set target was measured and met, otherwise `not_assessed`. With no target the rating is `not_assessed`.
- Workload impact (no rating): operations, operations per second over the workload window, share of operations, and share of summed operation time. A frequent shape is not inefficient for that reason.
- Execution overhead (`eo-1`): `spilled_to_disk` (a stage reported `usedDisk`), `in_memory_sort` (a SORT plan stage or `$sort` pipeline stage), or `none_observed`. `none_observed` needs executionStats on every explained sample; otherwise, and for unexplained shapes, the result is `not_assessed`.
- Sort and spill measurements (`execution_overhead.measurements`, shown under the Execution overhead column and in its detail panel, Download Summary, and the HTML report): per sample and per executed stage, sort bytes processed, `usedDisk`, spills, spilled records, spilled data size, spill file size on disk, memory limit, reported peak memory, and the stage time estimate, each with its explain field. They come from the same post-run explains (no extra commands) and describe that explain run, not the original operations. Sort bytes processed is not peak memory; the memory limit is not usage; spilled data size is not the on-disk file size; stage time includes input stages and is not exclusive sort time. Unreported fields say "not reported"; planner-only, failed, or unsupported explains say "unavailable" with a reason. Samples are compared, never averaged. See spec §11.
  - Verified against standalone MongoDB 8.0.4 (classic and slot-based engines, find and aggregate, in-memory and spilled sorts, `$group`, `$facet`). On 8.0.4, slot-based in-memory sorts report sort bytes as 0; this is shown as not measured. 8.0.4 `SORT` stages report no peak memory, spilled records, or spilled data size. Sharded layouts are fixture-tested only; mongos merge stages and `$unionWith` sub-pipelines are not measured (partial).
- `$lookup`: documents and keys examined in the joined collection, when the `$lookup` stage reports them (executionStats, separate pipeline stage). A `$lookup` pushed into the plan (`EQ_LOOKUP`) is `Not measured`. Lock waits and network time are not collected and are always `Not measured`.
- See `docs/query-efficiency-spec.md` for the formulas and limitations.
- Operations: find, aggregate, updateOne/Many, and deleteOne/Many are explained. Inserts are `not_applicable`. Failed transactions and the raw injector are not recorded.
- Budget: at most `2 x insights_explain_top_n` shapes (severity slots plus efficiency slots) and 2 samples per shape (default 20 explain commands). Each command uses `insights_explain_max_time_ms`, with shared retries and workers. `listIndexes` runs once per explained namespace (at most 32). Rendering and exports never rerun explains.
- Permissions: explain needs the privilege of the explained command (`find`, `update`, or `remove` on the namespace; update and delete explains do not modify data). Index advice needs `listIndexes`; without it the inventory is `unknown`, not empty.
- Verified topologies: standalone and single-node replica-set MongoDB 8.0.4 (local live tests). The CI configuration also runs the integration tests against standalone MongoDB 7.0 and 8.0. Sharded clusters (`mongos`) are covered only by hand-built explain fixtures. A replica set is reported as topology `mongod`.
- API change: `slow_queries[].trend` (an in-memory latency delta) was removed. Use `trends` in `/api/insights` and `/api/trend-baselines` instead. No compatibility field is provided.

## Limitations

- Sampling is representative, not exhaustive tracing.
- Trend history persists in `trend_baselines.json` (see [docs/trends-and-history.md](./docs/trends-and-history.md#where-history-is-stored)) and is pruned to the newest `trend_history_runs` unpinned runs plus pinned runs; efficiency comparisons rest on at most 2 explained commands per shape per run.
- Comparing with another run (`/api/trends/compare`) works only for the last finished run in this process; earlier runs can be compared with each other only as baselines of a later run.
- Explain depends on retained replay metadata and server permissions.
- Top-N explain means some shapes intentionally remain unattempted.
- Heuristic guidance is intentionally cautious and not proof of root cause.
- With `insights_trend_store_query_values` on, trend baselines, exports, Download Summary, and the HTML report contain literal query values (up to 16 KiB per sample, 1 MiB per run). The values comparison uses only the kept samples (explained plus up to 8 retained per shape), so it can miss values that ran but were not sampled.
- Replay comparison latency verdicts reuse the trend thresholds and are heuristics; result and error differences are facts, but writes change data, so they can also come from different starting data (use `workload_data_fingerprint` and `workload_replay_ops: reads_only`).
