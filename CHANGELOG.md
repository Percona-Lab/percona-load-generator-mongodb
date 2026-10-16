# Changelog

This file is intended to mirror the release notes you publish on GitHub.
For each new release, add a new `## vX.Y.Z` section at the top. The release workflow will use the first versioned section as the GitHub Release body when you push a `v*` tag.

---

## v5.0.0 - Reproducible Workloads: Record, Replay, and Compare Instances

This release makes PLGM an upgrade- and migration-testing tool. You can send the exact same operations, with the same values, to two or more MongoDB instances, then compare what each one returned and how fast, operation by operation. Every new feature is opt-in and off by default; with the defaults, runs, recordings, and stored trends are unchanged.

**Highlights**

**New: Record & Replay**
- `workload_record_enabled` writes every operation a run sends, with its exact values, to an NDJSON recording (`workload_record_path`, mode 0600; an existing file that is not a recording is never overwritten). Recording costs about 2 µs per operation.
- `workload_replay_enabled` runs a recording instead of generating operations: `per_worker` (recorded order, as fast as possible) or `timed` (recorded offsets, with lag reported). Replays check the recording header and commands hash, and fail on mismatched definitions unless `workload_replay_allow_mismatch` is on.
- `workload_record_max_bytes` caps the size (default 512 MiB, `0` = unlimited; the recording is marked truncated and the run continues). `workload_record_backpressure` chooses whether a slow writer fails the recording (default) or blocks workers. `workload_record_compression` writes `zstd` or `gzip` files (typically 10–15× smaller), detected automatically on replay and upload.
- Recordings can span several iterations; a replay runs them in order in one pass.
- `workload_replay_ops: reads_only` replays only finds and read-only aggregates, so the target keeps its starting data.
- `random_seed` now produces identical seeded documents on any instance: fixed field order, fixed reference time, seeded ObjectIds, and seeded `_id`s when collections are dropped.

**New: Deterministic generated traffic**
- `workload_deterministic` with `operations_per_worker` generates every operation from the seed, iteration, worker, and operation number. Two runs with the same settings send the same commands with the same values, without a recording file. Failures and retries do not shift later operations.
- Each run reports a traffic fingerprint (hashes only) on the dashboard, in Download Summary, and in the HTML report. Duration becomes a safety timeout, and runs that hit it are marked incomplete.

**New: Operation outcomes and replay comparison**
- `workload_record_outcomes` keeps what every operation returned: client latency, ok or error code and name (never the message), and a result summary (documents returned, inserted, matched/modified/upserted, deleted, transaction commit/abort). `workload_record_result_checksums` adds an order-sensitive sha256 of each find and aggregate result.
- Recordings store outcomes inline. Replays and deterministic runs write them to `workload_replay_outcomes_path` / `workload_deterministic_outcomes_path`.
- `plgm compare-replays a b` and **Query Analysis > Replay Comparison** in the Web UI match two executions operation by operation. Results are reported per shape and overall: latency deltas, count and checksum mismatches, errors that occur on only one side, missing operations, and the top differing operations, with commands shown when a recording is available. Optional plans per shape come from two trend runs. Exports are JSON and HTML.

**New: Multi-target replay**
- `workload_replay_targets` (2–8 `label=uri` entries, also `--replay-target` on the CLI) runs one replay or deterministic run on every target, `sequential` (default) or `concurrent` (`workload_replay_targets_mode`). Each target gets its own client, workers, stats, outcome file, insights, and trend run, and is compared automatically with the first target.
- `workload_data_fingerprint` (`counts` or `dbhash`) stores each collection's document count and indexes in the recording and checks them on every target before the first operation.

**New: Import captured traffic**
- `plgm import-traffic` and the Web UI convert mongod JSON logs (4.4+), `system.profile` exports, and raw command documents (plain, gzip, or zstd) into a normal recording. Each captured connection becomes a worker, and the original order and timing are kept.
- find, aggregate, insert, update, and delete are converted. Unsupported entries are skipped and counted, and operations that cannot be replayed exactly (server-generated values, dropped options, transactions) are flagged.

**New: Query values in trends**
- `insights_trend_store_query_values` (default off) stores the exact commands of each shape's explained and retained samples, so Trends can show whether two runs used the same query values. Runs replayed from the same recording or generated with the same deterministic fingerprint compare as "same values" without storing any literals. Improved/regressed labels never change.

**Web UI**
- New **Record & Replay** settings tab with browser-style sub-tabs: Seed & Determinism, Record, Replay, Multi-Target, Outcomes, and Import Traffic. Each setting is shown on one row, with the control on the left and an explanation on the right of what happens when it is on and when it is off, plus its config key and environment variable. Validation errors open the tab that holds the setting they name.
- Shape details in Efficiency and Trends open in one shared detail dialog.
- The Launch Workload button floats at the bottom-right without the full-width bar behind it.
- Dashboard cards for the traffic fingerprint and multi-target results; run reports include recording, replay, and per-target sections.

**Compatibility**
- New file formats: `plgm-workload-record/1` (recordings), `plgm-workload-outcomes/1` (outcome files), and `plgm-replay-compare/1` (comparison reports).
- Recordings, outcome files, comparison reports, and trend stores with query values enabled contain real query values. Handle them like production data.
- When `workload_replay_targets` is set, the MongoDB URI is not used. Username, password, and TLS settings apply to every target.
- Record & Replay settings moved from the Workload Configuration tab to the new Record & Replay tab.

**Fixes**
- Fixed a data race when several targets shared one set of collection definitions.

**Testing & Docs**
- Added unit, golden-file, and scale tests for the recording format, compression, backpressure, outcomes, comparison, traffic import, and deterministic generation. Golden tests confirm that the default settings produce unchanged output.
- Added integration tests for recording and replay, deterministic runs and outcomes, and multi-target replay on several mongod instances.
- Added jsdom UI tests for every new setting and its explanation, Replay Comparison, multi-target, traffic import, trend values, and the Record & Replay sub-tabs.
- Documented the features in `README.md`, including "Comparing Two Instances (Upgrade Testing)", "Record and Replay", "Multi-Target Replay", and "Import Captured Traffic". Also updated `INSIGHTS.md`, `TESTING.md`, `config.yaml`, and `plgm --help`.

## v4.9.0 - Multi-Run Trend History, Comparison, and Sharing

**Highlights**

**New: Trend history with labels and pinning**
- Each query shape keeps one workload and one efficiency snapshot per run, newest first. The store keeps the newest `trend_history_runs` unpinned runs (default 5, 1–50); pruning drops the oldest runs and their snapshots.
- Runs get a label from `trend_run_label`, or the default `mongo-<server version>` (`run-<UTC time>` when the version is unknown). Labels can be edited after the run.
- Pinned reference runs (up to 10) do not count toward the limit and are never pruned or expired. Unpinned runs older than `trend_max_age_days` (default 30, `0` = never) are reported as outdated.

**New: Comparison modes and run matrix**
- By default each shape is compared with the previous compatible run, per metric. `trend_default_comparison: pinned` uses the newest pinned run instead.
- The Trends view can compare with any stored run, or show a matrix of up to 8 runs: one column per run with average latency, p95, efficiency score, and impact share, with the current run highlighted. Each pair is gated separately, and cells that are not comparable show the reason. Efficiency, latency, and impact stay separate.
- New endpoint `GET /api/trends/compare?baseline=…|runs=…`.

**New: Export, import, and shared stores**
- Export selected or all runs as JSON (UI, `GET /api/trend-baselines/export`). Import in the UI or with `POST /api/trend-baselines/import`. Imports validate the schema and limits, merge by `run_id` (duplicates skipped), report what was added, skipped, or not kept, and keep the source label. Fields outside the redacted trend schema are ignored.
- Several instances can share one `PLGM_TREND_BASELINE_STORE` file: writes take a lock file, re-read and merge, then replace the file atomically. Run ids include 64 random bits, so they stay unique across machines.
- Run endpoints: `PATCH /api/trend-baselines/runs/<id>` (label, pinned) and `DELETE /api/trend-baselines/runs/<id>`.

**New: Trends & History settings tab**
- Runs to keep, age expiry, store path (`off` disables), default comparison, and default run label, each with a config.yaml key and a `PLGM_TREND_*` environment variable (environment over YAML over default). The tab also has the run list (rename, pin, delete), export/import, and Reset baselines.

**Compatibility**
- Store schema `tb-2` and trend version `tc-2`. A `tb-1` file is migrated on load (each snapshot becomes the first history entry) and rewritten on the next save. Unsupported files are moved aside, never deleted.
- `recorded_at` for new runs now has millisecond precision.
- The file stays under 32 MiB. Typical size is about 0.75 KB per shape per run; see README for the worst case.

**Testing & Docs**
- Added unit tests for history merge and pruning with pinning, expiry, migration from `tb-1`, label defaults, the comparison modes and matrix gate reasons, export/import (round trip, duplicates, invalid, oversized, and unsupported files, redaction), concurrent writers and lock handling, and config, env, and UI wiring.
- Added a multi-run UI fixture and jsdom tests for the Trends & History tab and the matrix view.
- Documented storage, sharing, and settings in `README.md`, `config.yaml`, and `INSIGHTS.md`.

## v4.8.0 - Parallel Initial Data Seeding

**Highlights**

**New: Parallel Seed Writers**
- Added `seed_concurrency` / `PLGM_SEED_CONCURRENCY` to run concurrent `InsertMany` calls within each collection during the SEED phase. Defaults to `1` (serial) and is independent of workload `concurrency`.
- The Web UI exposes **Seed Concurrency** next to **Seed Batch Size** in the Execution / Runtime tab.
- Document generation stays serial, so the `random_seed` sequence is unchanged; batches may finish out of order.
- Memory stays bounded to about `(writers + 1) * seed_batch_size` documents regardless of `documents_count`.
- On the first insert error or cancellation, remaining writers are cancelled and the underlying error is returned. Already accepted writes are not rolled back.

**Testing & Docs**
- Added unit tests for batching, writer concurrency, generated-data integrity, failure, and cancellation, plus config and Web UI wiring tests.
- Added integration tests for serial/parallel seed counts, database routing, and duplicate-key error propagation, and a `BenchmarkInsertRandomDocumentsIntegration` throughput benchmark.
- Documented usage and limitations in `README.md`, `config.yaml`, and `TESTING.md`.

Thanks to @FranzLi347 for the contribution ([#3](https://github.com/Percona-Lab/percona-load-generator-mongodb/pull/3)).

## v4.7.0 - Add post-run query and index analysis, explain settings tab, and trends

This release adds post-run query analysis. Beyond telling you that a query is slow, PLGM now shows what the server did for each query shape: how much it scanned, whether it sorted in memory or spilled to disk, and which index would help.

**Highlights**

**Query analysis**
- Run explain after the workload on representative samples of every
  tracked query shape, including fast ones, within a fixed budget
  (2 x Top N shapes, at most 2 samples each). Updates, deletes and
  $out/$merge pipelines are explained without being applied.
- Add the Query & Index Analysis tab with separate columns: scan
  efficiency (se-1, 0-100), query latency (avg/max/p95/p99, optional
  targets), workload impact, execution overhead (in-memory sort, spill,
  per-sample sort/spill measurements), and $lookup work. Rows open a
  detail panel when clicked.
- Add index recommendations with ordered candidate keys (equality, sort,
  range), checked against listIndexes, with tradeoffs and validation steps.
- Add explain coverage reporting; unexplained shapes are "unassessed",
  never "efficient".
- Add run-to-run trends (tc-1) with persisted redacted baselines,
  /api/trend-baselines, and a Trends tab.
- Include the efficiency view, coverage, and trends in /api/insights,
  Download Summary, and the HTML report.

**Settings and UI**
- Turn post-run explain on by default (insights_explain_enabled: true)
  and move its settings to a new Query Analysis (Explain) tab.
- Add latency targets (insights_latency_target_{avg,p95,p99}_ms) and
  explain efficiency slots (insights_explain_efficiency_mode).
- Compact settings and dashboard layout; live operation-mix total.

**Ordering and identity**
- Keep declared key order for compound indexes, shard keys, and sort
  specifications from loading through execution and explain.
- Shape identity si-3: projection, limit, upsert, and ordered sorts are
  part of the shape.
- Require review of saved definitions with multi-key sorts created by
  earlier versions (contentVersion 2, reviewedSorts confirmations).

**Other**
- Add the query_analysis_demo workload.
- Add CI workflow (make check, test-race, test-ui, integration on
  MongoDB 7.0/8.0) and jsdom UI tests.
- Remove slow_queries[].trend from the insights API.

## v4.6.0 - TLS Client Configuration & Metrics Accuracy

**Highlights**

**New: TLS Client Configuration**
- Added first-class MongoDB TLS client settings for `tlsCAFile` and `tlsCertificateKeyFile`.
- TLS is now enabled automatically when both TLS client files are provided, and disabled when they are not.
- The Web UI now uses file picker inputs for selecting TLS certificate files instead of plain text fields.
- Uploaded certificate files are saved to temporary paths and wired into the active connection configuration.

**Fixes**
- Fixed the live throughput graph so it uses actual elapsed time between samples instead of assuming perfectly timed polling.
- This removes inflated spikes in the final chart when browser timing drifts near the end of a run.

**Testing & Docs**
- Added tests covering TLS config validation, MongoDB URI generation, and TLS file upload handling.
- Updated `config.yaml` and `README.md` to document the new TLS fields and usage.

---

## Release Template

Copy the structure below for the next release and replace the version/title:

## vX.Y.Z - Release Title

**Highlights**

**New: Feature Area**
- Bullet one.
- Bullet two.

**Fixes**
- Bullet one.

**Testing & Docs**
- Bullet one.
