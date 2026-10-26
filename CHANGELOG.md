# Changelog

This file is intended to mirror the release notes you publish on GitHub.
For each new release, add a new `## vX.Y.Z` section at the top. The release workflow will use the first versioned section as the GitHub Release body when you push a `v*` tag.

---

## v6.1.0 - Multi-target replay and easier set up

Multi-target replay in the Web UI is easier to set up: pick a recording from a list instead of typing its path, get the settings targets need turned on for you, leave the outcome file blank for a new name per run, and open comparison reports in a browser tab. The CLI, `config.yaml`, and environment variables are unchanged.

**Highlights**

**New: Choose a recording on the PLGM server**
- **Recording to Replay** is now a list of the recordings the PLGM server can replay, newest first, with operation count (uncompressed files), size, time, and where each came from (recordings directory, Import Traffic, or a run of this server). **Refresh** reloads it and keeps the selection while the file exists; a chosen file that disappears is cleared with a message. Empty and error states say what to do next.
- Manual entry stays available under **Enter a server path or upload a file**. Saved configurations keep their path; a path that is not in the list is shown as used.
- Listing is bounded: only regular files directly in the folder of the server's `workload_record_path` (no subfolders, no symbolic links), at most 200 inspected and 100 listed, reading only each file's header line. Form values never add folders. Files that look like recordings but cannot be replayed (unreadable, damaged header, unsupported format) are listed as disabled with the reason.
- When a replay starts, the chosen recording is checked again; a missing, unreadable, or non-recording file is rejected before anything connects.

**New: Prerequisites and conflicts for Replay Targets**
- Listing Replay Targets turns **Record Outcomes** on, with a note; removing the targets restores the previous value.
- In **Record a workload**, listed targets are kept but disabled and not sent, with a note: record on one instance, then replay that recording on the targets. The "Replay Targets cannot be combined with Record Workload" error no longer appears for this case. The API still rejects the combination, now with the same guidance.
- In a generic or custom run without Deterministic traffic, listed targets are kept but not sent, so the run uses MongoDB URI as before. The targets note says how to use them (turn on Deterministic traffic or choose **Replay a recording**), and the launch bar shows `replay targets not used`.

**New: Several servers next to the connection**
- Replay targets moved from the Advanced **Multi-Target** section to **Target Connection** in Basic settings: **Run on: One server / Several servers**. Several servers replaces the MongoDB URI field with the server list and Targets Mode; choosing it adds two empty rows. One server keeps the list but doesn't send it. Run on is shown for replays, and for generic or custom runs with Deterministic traffic; recording runs never show it.
- **Starting-Data Fingerprint** moved to the **Record** section when recording and the **Replay** section otherwise. The **Multi-Target** section and its tab are gone. Setting keys, the API, and saved configurations are unchanged.

**Improved: Seed & Deterministic**
- The **Record & replay** category is now **Seed & Deterministic** (**Outcomes & import** in a replay, which uses neither). **Deterministic traffic** comes first, then **Random Seed**. **Operations per Worker** shows only while Deterministic traffic is on, and says so right away when it is still 0.
- In a generic or custom run, **Run on** and the server list appear right under Deterministic traffic, so a multi-server deterministic run is set up in one place. They are the same controls as in Target Connection, so a change in one place shows in the other.
- Turning Deterministic traffic on fills in a Random Seed when it is 0, and the note lists only what it changed. Turning it off restores those values unless you changed them since, and says what it restored or kept. If several servers were listed, it says the run now connects only to the server in the MongoDB URI field, and keeps the list.
- The help states what deterministic traffic does not guarantee: identical latency, throughput, ordering across workers, or results.
- Replay target rows wrap on narrow screens, and their fields have accessible names.
- **One server** and **Several servers** each explain what they do in a **?** tooltip; the text no longer shows under the choice. One server connects to the MongoDB URI field; Several servers connects to each listed server with its own URI and ignores that field.
- All settings cards share one layout: title, level tag, and **?** with the description behind it, then the fields. Dividers only separate subsections, the tag pills are gone in a task, and the gap under Seed & Deterministic's title is gone.

**Improved: Seed documents switch**
- **Data & Query Definitions** has a **Seed documents** checkbox next to **Seeding & Transactions** (Basic, off by default, matching `skip_seed: true`). Check it to seed and enter **Seed Document Count**; unchecked, nothing is seeded and the count is greyed out but keeps its value. It replaces the Advanced **Skip Seed Phase** checkbox and still sets `skip_seed`, so configs, the API, and the CLI are unchanged. The bottom bar says `no seeding` when it's off.

**New: Use defaults / Reset to defaults**
- Each task card has a **Use defaults** button, and **Reset to defaults** sits under the task title once a task is open. Both load the settings PLGM started with (`config.yaml` and environment) instead of the last run's, selects the built-in definitions, and keeps the URI and credentials. Clicking a card itself still starts from the last run's settings. `GET /api/config?defaults=1` returns that startup configuration.

**New: Newest recording selected for you**
- Opening **Replay a recorded workload** with no recording chosen selects the newest one that can be replayed, and says so. A recording you picked, or a saved path, is kept.

**New: A new file for every recording**
- In the Web UI, a blank **Recording File** now gives each run its own file, `plgm_workload_record-<UTC time>-<random>.ndjson` (`.zst` or `.gz` when compressed), instead of reusing `plgm_workload_record.ndjson` and replacing the last recording. It's created exclusively, returned by `POST /api/start` (`recording_file`), shown on the dashboard, and listed for replay. A name you type is used as before. The CLI default is unchanged.

**New: Generated outcome file names**
- With Record Outcomes on and **Replay Outcome File** (or **Deterministic Outcome File**) blank, the Web UI server names the file for the run: `plgm_replay_outcomes-<UTC time>-<random>.ndjson` in the folder of `workload_record_path`, with the target label before the extension for each target. Files are created exclusively (mode 0600), so a generated name never replaces a file, and the next run always gets a new name. A name you enter is used as before.
- The resolved files are returned by `POST /api/start` (`outcome_files`), logged in the run events, shown on the dashboard for single runs, and listed per target in the multi-target table.
- The multi-target table lists each target's outcome file by name (full path on hover) and names the folder once below the comparisons.

**New: Open reports in the browser**
- Comparison links in the multi-target card and in **Replay Comparison** now offer **Open report** (new tab) and **Download**. **Completed Runs** has the same pair.
- `GET /api/replay-compare/<id>/report.html` and `GET /api/run-history/<id>/report` are served inline; add `?download=1` for a file. Comparison reports carry a restrictive Content-Security-Policy and `nosniff`. A report that is no longer kept returns a short page that says so.

**Fixes**
- Comparison report links downloaded the report instead of opening it.
- Replay without an outcome file path failed validation even though the path could be chosen automatically.

**Testing & Docs**
- `docs/web-ui.md`, `docs/record-and-replay.md`, `docs/query-analysis.md`, `docs/trends-and-history.md`, `docs/reports-and-exports.md`, and `docs/load-modeling.md` were rewritten around short steps and how to read each result. `docs/load-modeling.md` no longer says the dashboard shows active worker counts (they're in `/api/stats` and the report).

## v6.0.0 - Revamped Web UI, Settings Categories, and Completed Run Dashboards

This release redesigns Web UI setup around what you want to do. You pick a task first (generic workload, custom workload, stress test, record, or replay), and the page shows only the settings that task uses. Basic settings are always on screen; Advanced and Expert settings are grouped into categories, so you no longer scroll through one long page. Launch checks every setting the run uses, including ones that are not on screen, and takes you to the field that needs fixing. Finished runs are now saved, so you can reopen their dashboards and HTML reports after the browser or PLGM is closed.

The CLI, `config.yaml`, and environment variables are unchanged. 

**Highlights**

**New: Choose a task first**
- Setup starts by asking what to run: **Run a generic workload**, **Run a custom workload**, **Stress test** (raw injector), **Record a workload**, or **Replay a recorded workload**. Each task is a card with an icon and a one-sentence description. A sixth tile opens completed runs.
- After a task is chosen, the cards shrink to a compact switcher above the settings. Selecting another card keeps the values that still apply. The cards lock while a workload is running.
- Each task shows only the settings it uses. Choosing **Record a workload**, **Replay a recorded workload**, or **Stress test** turns on Record Workload, Replay Workload, or Raw Injector Mode for you. Settings the task cannot use are not applied to the run, and they are restored if you switch back.
- When recording, **Workload to record** chooses the built-in workload or your own definitions.

**New: Settings levels and categories**
- **Settings level** shows **Basic** settings. Turn on **Advanced** and/or **Expert** to add more. Each added section carries a small level marker. Values you change keep applying when a level is turned off again.
- With **Advanced** or **Expert** on, a **Categories** list appears beside the settings: **Basic settings**, **Connection**, **Workload execution**, **Record & replay**, **Data & queries**, **Metrics & reporting**, **Query analysis**, and **Trends & history**. Only the selected category is shown. Only categories with settings for the current task and levels are listed. Opening a level moves to the first category it adds. If the selected category disappears, **Basic settings** is selected. Switching categories, levels, or tasks never changes a value.
- The category list is a keyboard tablist (arrow keys, Home, End). On narrow screens it wraps into rows above the settings.
- Settings that depend on another category link to it. **Post-Run Explain** says when it needs **Post-Run Insights** on and links there. The note under **Deterministic traffic** links to the Basic settings it switches.

**New: Validation that finds the field**
- **Launch Workload** checks every setting the run uses, including ones in closed levels and other categories. Each problem appears under its field, and each category that holds one gets a red dot (never a count). The levels that hold problems open, and the first field is selected and focused. Fixing a field removes its message and, when it was the last one, its category's dot.
- If a problem is in a section the selected task does not use (for example a trend setting during a stress test), that section is shown with a note until the next launch or task change. The value is still checked, as before, so it can now be corrected.

**New: Completed run dashboards**
- Finished runs are saved as `rd-1` JSON files in `run_dashboards`, beside the trend baseline file (`~/.config/plgm/run_dashboards` on Linux, `~/Library/Application Support/plgm/run_dashboards` on macOS, or next to a custom `trend_baseline_store`). Multi-target runs are saved too.
- **Completed Runs** in the header lists each run with its name, start time, duration, mode, status, and a short throughput and latency summary. **Open dashboard** shows the run's saved charts, latency, and configuration after the browser is closed and PLGM is started again with that storage. Opening one does not start a workload and does not need a live database. **HTML report** downloads the stored report, and **Pin** keeps a run when older ones are pruned.
- Retention uses `trend_history_runs`, `trend_max_age_days`, and the 10-run pin limit. Up to 3600 chart samples are kept per run. Passwords, usernames, TLS paths, and URI userinfo are not stored. A measurement that was not stored is shown as unavailable, never as zero. A damaged file is listed with an error, and the other runs still open. A container must mount this directory; see `k8s_and_docker.md`.

**Web UI**
- A sticky launch bar at the bottom of the settings shows the configuration that will run, any validation message, and **Launch Workload**. It is the only launch control.
- The changed-settings badges ("2 changed") and the "N settings changed" text are gone. Changed values are still kept and sent.
- Long setting explanations open from a **?** button placed right after the label's last word (also when the label wraps), with a name like "Help: Runs to keep". Hover, keyboard focus, click, or tap opens it. Validation errors and execution warnings stay inline.
- Inputs are sized by the values they accept (compact counts, durations, names, wide URIs and paths, full-width editors) instead of all being full width. Labels written without `for` are now tied to their control.
- One token-based stylesheet covers setup, dashboards, completed runs, and dialogs (type scale, spacing, radii, buttons, tables, and focus, disabled, and invalid states). The Google Fonts request was removed: the UI uses the system font stack and loads nothing from a CDN.
- Setup, the live dashboard, and completed runs share one visual system: a light app bar, one filled primary action, and quieter secondary controls.
- **Sharding Strategy** is now shown for replay runs, which set up sharding for their collections before replaying.

**API**
- `POST /api/start` accepts `operation_mode` (`generic`, `custom`, `record`, `replay`, or `raw`). It forces the flags that define the task and clears settings the task cannot use. Omitting it keeps the previous behavior: every submitted field applies. There is no `config.yaml` key and no migration. `record_workload_source` is not stored; it only chooses built-in or custom definitions while recording.
- `GET /api/run-history` lists completed runs and the storage settings. `GET /api/run-history/<id>` returns one run, `GET /api/run-history/<id>/report` returns its HTML report, and `PATCH /api/run-history/<id>` with `{"pinned": true|false}` pins or unpins it.

**Fixes**
- Browser checks (minimum, maximum, required) on a setting that was hidden could block **Launch Workload** silently, with no message. They are now reported under the field like any other validation error.
- **MongoDB URI** was marked required in the browser even when replay targets were listed, which blocked a multi-target launch without a message. The URI is now required only when no targets are listed, as the server already did.
- Validation errors no longer turn on every settings level to show one field.

**Testing & Docs**
- New `docs/web-ui-design-system.md` with the tokens, field widths, label and help component, settings categories, and field-error conventions.
- `docs/web-ui.md` describes task selection, settings levels, the settings categories and what each holds, cross-category validation, and completed runs. The other guides (`docs/` workloads, load modeling, performance tuning, record and replay, query analysis, trends and history, reports and exports, raw injector, plus README and INSIGHTS) now write each setting's place as **Category > Section** with the level that adds it, and their Web UI steps start from the task cards.
- `docs/trends-and-history.md` and `k8s_and_docker.md` cover the completed-run directory, its retention, and the volume a container needs.
- New Go tests for `operation_mode` and run history storage. New jsdom tests cover setup modes, help placement and keyboard use, field widths, settings categories (per task and level, keyboard navigation, values kept across categories, levels, and tasks, cross-category validation and focus, dependency links, and the section shown for an error the task does not use), and completed runs.

## v5.1.0 - Runs Last for Duration, and the Web UI Stays With the Run

This release keeps a workload going for the time you configured, and keeps the Web UI attached to that run. Deterministic traffic no longer stops when the first pass of operations finishes. Checking that box also sets Drop Collections, Skip Seed, and a fixed load profile for you. The header clock moves during seeding, explains a total that includes pauses between iterations, and a reload returns you to the run that is already going.

**Highlights**

**Changed: Deterministic traffic runs until Duration**
- `duration` is how long the measured run lasts. `operations_per_worker` is one pass, not a reason to stop. When a worker finishes that pass, it continues the same workload with the next operation numbers until Duration ends. A run that ends before every worker finishes one pass is marked incomplete, with the reason.
- The traffic fingerprint still identifies that first pass only (hashes, never values). Two runs with the same fingerprint sent the same first-pass commands per worker. A faster instance can send more operations after that pass, so **sent** can be higher than **planned**. Planned is workers × Operations per Worker × iterations.
- Reports label that configured time as Duration. In v5.0.0 the same field was called a safety timeout, and the run stopped when the pass finished.

**New: Checking Deterministic traffic configures the required settings**
- Turning **Deterministic traffic** on sets the settings that mode requires: **Drop Collections First** on, **Skip Seed Phase** off, and **Load Profile** to Fixed. A note under the checkbox states what changed. Turning the checkbox off restores the values those fields had before.
- The CLI and config file are unchanged. `workload_deterministic` still requires those settings itself; the Web UI now applies them when you check the box.

**Web UI**
- During initialization the header clock counts preparation time and is labeled `init` (for example `00:12 init`). Seeding progress advances as documents are inserted, including inside one large collection. The execution clock starts when the workload itself starts, and preparation time is not included.
- With more than one iteration and an interval delay, the clock total includes the pauses between runs. Five runs of 30s with 10s between them is 03:10 (02:30 of workload plus four 10s pauses; there is no pause after the last run). A short note under the clock states that once execution starts. The note stays hidden during initialization.
- Reloading the page during a run returns to that run, with **Stop Workload** still available, and restores the live charts from samples kept for it (about 30 minutes at the default 500ms refresh). Starting again while a workload is already running opens that run instead of reporting "workload is already running".

**Fixes**
- Fixed a hang on the fast-find path when pooled records had no integer key for the lookup field. Sampling now stops after a bounded number of draws, and when the run is canceled.
- Fixed the Stored Runs list on Trends & History, which did not appear because its container reused the id of the "Runs to keep" field.

**Testing & Docs**
- README is now a short overview, quick start, and index. Feature guides with Web UI steps are in `docs/` (web UI, CLI, workloads, load modeling, query analysis, trends and history, record and replay, reports and exports, raw injector, performance tuning, and the configuration reference).
- Documented the deterministic duration behavior and the checkbox defaults in `docs/record-and-replay.md`, `docs/web-ui.md`, `docs/workloads.md`, `docs/cli.md`, `docs/performance-tuning.md`, and `docs/configuration-reference.md`: how long a run lasts, how to read FINISHED and INCOMPLETE, operation tiles versus command counts, the first-pass fingerprint, and that checking **Deterministic traffic** sets Drop Collections, Skip Seed, and a fixed load profile. Also documented the header total when iterations have an interval delay.
- Extended unit and jsdom tests for the initialization clock, reload and reattach, deterministic duration, seeding progress, the fast-find bound, and the iteration-total note.

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
