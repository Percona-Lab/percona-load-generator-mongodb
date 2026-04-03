# Post-Run Insights: Slow Query and Index Analysis

This guide explains exactly how PLGM's post-run insights work, what they can and cannot tell you, and how to configure them safely.

The insights feature is a **post-run analytics layer** that helps you identify:
- repeated slow operation groups
- affected collections
- normalized query-shape hotspots
- possible index-related issues with confidence-aware wording
- next actions for investigation

It is intentionally designed to avoid runtime disruption during active benchmarking.

## How to Read Insights in 60 Seconds

Use this quick sequence after each completed run:

1. Check `metadata.status`
- If `pending`, wait for run completion.
- If `ready`, continue.
- If `empty`, increase sampling rate or run duration and retry.

2. Read the first entry in `slow_queries`
- Start with highest-ranked shape (`rank = 1`).
- Focus on `p95_ms`, `p99_ms`, `count`, and `severity`.

3. Check explain fields for that shape
- `explain_status` tells you whether evidence is explain-backed or heuristic.
- `explain_reason` tells you why explain did or did not run.

4. Review top `potential_index_issues`
- Prioritize entries with higher confidence first.
- Treat `high` confidence as strongest action signal (typically explain `COLLSCAN` observed).

5. Use `affected_collections` for blast radius
- Confirm whether one collection dominates slow behavior.

6. Execute next action
- Validate index/action in staging.
- Re-run workload and compare shape metrics/trends.

### Quick Example

If you see:
- `slow_queries[0]`: `find` on `orders`, `p99_ms=1850`, `severity=high`
- `explain_status=explained`, `explain_reason=collscan_observed`
- `potential_index_issues[0].confidence=high`

Interpretation:
- This is a high-priority, repeated slow shape with strong explain evidence of scan risk.

Recommended next step:
- Propose candidate index for reported filter fields.
- Validate with explain + replay.
- Re-run benchmark and confirm lower p95/p99 for same `shape_id`.

## Quick Expectations

Before you use this feature, expect the following behavior:
- Insights are not final while workload iterations are still running.
- Insights become final only when all iterations complete.
- The panel is visible during run with pending status, then fills in automatically after completion.
- Results are sampled and bounded, not a full trace of every operation.
- Index suggestions are cautious and evidence-weighted.

## Where Insights Are Available

After a run completes, insights are available in:
- Web UI panel: `POST-RUN SLOW QUERY & INDEX ANALYSIS`
- API endpoint: `GET /api/insights`
- `Download Summary` export JSON under `insights`

All three surfaces come from the same backend report model.

## Lifecycle and States

`GET /api/insights` uses these states:
- `inactive`: no active collector/run context exists
- `pending`: run is still executing
- `ready`: final report available
- `empty`: run finished but no sampled events were retained
- `disabled`: insights disabled by configuration

Important:
- No final slow-query/index conclusions are shown during `pending`.
- Final insights are cached per completed run for consistent UI/export parity.

## Data Collection Model

During workload execution, PLGM stores sampled operation events in a bounded in-memory ring buffer.

Each sampled event may include:
- operation type
- database and collection
- normalized shape key and shape summary
- extracted filter fields (when available)
- duration in milliseconds
- success/failure
- iteration number
- timestamp
- optional explain replay metadata (filter/pipeline samples) when explain mode is enabled

Boundaries:
- sampling is controlled by `insights_sampling_rate`
- total retained events are capped by `insights_max_events`
- aggregation cardinality is capped by `insights_max_groups`

This keeps memory use predictable and avoids unbounded retention.

## What the Final Report Contains

Top-level report fields:
- `summary`
- `slow_queries`
- `affected_collections`
- `query_shapes`
- `potential_index_issues`
- `recommendations`
- `per_iteration`
- `time_slices`
- `metadata`

## Shape Identity and Trends

Each shape gets a stable `shape_id` built from:
- operation
- collection
- normalized shape key

This allows shape-level continuity across runs.

Trend hints (`improved`, `worse`, `flat`) are based on in-memory baseline from prior run(s) in the same process. They do not persist across process restarts.

## Explain Mode (Optional, Off by Default)

Explain mode is post-run only and optional.

Design:
- disabled by default
- executed only after run completion
- limited to top-N slow shapes
- uses bounded post-run worker concurrency
- retries timeout-bound explains with bounded backoff
- bounded by explain max time
- never blocks active workload execution path

When enabled, explain evidence can refine index confidence messaging.

## Explain Operation Support Matrix

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

Notes:
- `update*` and `delete*` are evaluated through a safe filter-based query-planner surrogate path to inspect index/scan behavior.
- `aggregate` explain works when representative sampled pipeline metadata is retained.

## Explain Status and Reason Semantics

Each slow shape and index issue includes:
- `explain_status`
- `explain_reason`

Statuses:
- `explained`: explain successfully executed
- `explain_unavailable`: explain not attempted for this shape (for example top-N limit)
- `not_supported`: operation type is not explain-supported
- `insufficient_metadata`: required sampled replay metadata was unavailable
- `timed_out`: explain exceeded configured server/client timeout budget
- `execution_failed`: explain execution/connectivity/auth/namespace failed

Common reasons include:
- `explain_disabled`
- `explain_not_attempted_yet`
- `not_selected_top_n_<N>`
- `low_value_shape_filtered`
- `shape_event_not_retained`
- `missing_filter_sample`
- `missing_pipeline_sample`
- `connect_failed`
- `not_authorized`
- `namespace_not_found`
- `command_not_found`
- `max_time_exceeded`
- `run_command_failed`
- `collscan_observed`
- `ixscan_observed`
- `no_scan_stage_detected`

## Confidence and Evidence Model

PLGM intentionally separates observed evidence from recommendations:

Typical confidence mapping:
- `high`: explain observed `COLLSCAN` for representative shape
- `medium`: explain observed `IXSCAN` but performance is still poor
- `low`: heuristic-only, unsupported, unavailable, insufficient metadata, or failed explain

Evidence levels are explicit (for example `heuristic_*`, `explain_*`) and reflected in both UI and export.

Guidance language is intentionally cautious and never presents heuristics as guaranteed truth.

## UI: What You Should Expect

During run:
- Insights panel remains visible with pending status text.
- No final index/slow-shape conclusions are shown.

After run:
- Top findings summarize the strongest signals.
- Slow query rows include latency stats, shape id, and explain status/reason.
- Index issue rows include confidence, evidence level, explain status/reason, and recommended action.
- Iteration and timeline tabs provide supporting context.

## Download Summary Behavior

`Download Summary` includes `insights` from the same final model used by `/api/insights`.

This means:
- UI and export should match for completed runs.
- Password redaction behavior remains unchanged.

## Configuration

Web UI path:
- `Advanced -> Insights Analysis`

Controls:
- Enable Post-Run Insights Analysis
- Enable Post-Run Explain Sampling (Optional)
- Insights Sampling Rate
- Slow Threshold (ms)
- Max Retained Events
- Max Group Entries
- Explain Top N Shapes
- Explain Max Time (ms)
- Explain Workers (Post-Run)
- Explain Timeout Retries
- Explain Retry Backoff (ms)

YAML keys:
- `insights_enabled`
- `insights_sampling_rate`
- `insights_slow_threshold_ms`
- `insights_max_events`
- `insights_max_groups`
- `insights_explain_enabled`
- `insights_explain_top_n`
- `insights_explain_max_time_ms`
- `insights_explain_workers`
- `insights_explain_retries`
- `insights_explain_backoff_ms`

Environment variables:
- `PLGM_INSIGHTS_ENABLED`
- `PLGM_INSIGHTS_SAMPLING_RATE`
- `PLGM_INSIGHTS_SLOW_THRESHOLD_MS`
- `PLGM_INSIGHTS_MAX_EVENTS`
- `PLGM_INSIGHTS_MAX_GROUPS`
- `PLGM_INSIGHTS_EXPLAIN_ENABLED`
- `PLGM_INSIGHTS_EXPLAIN_TOP_N`
- `PLGM_INSIGHTS_EXPLAIN_MAX_TIME_MS`
- `PLGM_INSIGHTS_EXPLAIN_WORKERS`
- `PLGM_INSIGHTS_EXPLAIN_RETRIES`
- `PLGM_INSIGHTS_EXPLAIN_BACKOFF_MS`

## Recommended Baselines

General benchmarking:
- sampling rate: `0.10`
- slow threshold: `200`
- max events: `5000`
- max groups: `300`
- explain enabled: `false`
- explain workers: `1`
- explain retries: `1`
- explain backoff: `150`

Focused troubleshooting:
- sampling rate: `0.25` to `1.0`
- explain enabled: `true`
- explain top N: `3` to `5`
- explain max time: `1000` to `3000`
- explain workers: `1` to `2` (increase carefully)
- explain retries: `1` to `2`
- explain backoff: `150` to `400`

## Practical Use Cases

1. Immediate post-run triage
- Find top slow operation groups and start with the highest-severity shape.

2. Collection hotspot ranking
- Identify which collections contribute most to slow ratios.

3. Safe index investigation shortlist
- Use filter field patterns and evidence level to build DBA validation backlog.

4. Iteration drift detection
- Compare per-iteration slow ratio and p95/p99 changes.

5. Time-slice correlation
- Align latency hotspots with throughput periods using `time_slices`.

6. CI/export analysis
- Ingest `insights` JSON into automated benchmark regression reports.

## Limitations and Non-Goals

- Sampling is representative, not exhaustive.
- Heuristic index suggestions are not proof of root cause.
- Explain depends on retained replay metadata and server permissions.
- Top-N explain means some shapes will remain `explain_unavailable`.
- Trend memory is process-local and reset on restart.
- Insights are post-run only by design to protect workload performance.

## Troubleshooting

If explain stays unavailable:
- confirm explain mode is enabled
- increase sampling rate
- increase max retained events
- increase explain top N
- increase explain max time (ms)
- keep explain workers low first (to avoid extra cluster pressure)
- verify MongoDB auth allows explain commands

If many shapes show insufficient metadata:
- ensure explain mode was enabled before run start
- increase sampling rate and max events

If explain shows timeout failures:
- raise `insights_explain_max_time_ms`
- keep retries at `1` or `2` (higher values increase post-run analysis time)
- use low worker count first (`1`), then increase only if cluster capacity allows

If confidence remains low:
- expected when evidence is heuristic only
- rerun with explain enabled and targeted workload duration

If report is empty:
- increase sampling rate
- check run duration and operation volume

## Versioning Expectations

The insights schema is designed for dashboard/export use and may evolve. Consumers should:
- tolerate additional fields
- treat unknown fields as non-breaking
- rely on `metadata.status` to determine readiness

## Future Extensions

Potential additions for full dashboard evolution:
- persistent multi-run history
- deeper explain plan diagnostics
- cross-run diff/regression scoring
- richer shape drill-down and replay helpers
