# Changelog

This file is intended to mirror the release notes you publish on GitHub.
For each new release, add a new `## vX.Y.Z` section at the top. The release workflow will use the first versioned section as the GitHub Release body when you push a `v*` tag.

---

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
