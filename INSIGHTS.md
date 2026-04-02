# Post-Run Insights: Slow Query and Index Analysis

This document explains PLGM's post-run insights layer in detail.

The feature provides a structured analysis after benchmark completion, including:
- slow operation groups
- affected collections
- normalized query-shape groupings
- cautious, evidence-based index guidance
- export-ready JSON data for downstream dashboards

## What It Is

The insights layer is a **foundational analytics pass** designed to run after all iterations are complete.

It is intentionally separated from real-time charts to keep runtime overhead bounded and predictable.

## Where It Appears

After completion, insights are available in:
- Web UI dashboard panel: `POST-RUN SLOW QUERY & INDEX ANALYSIS`
- API endpoint: `GET /api/insights`
- `Download Summary` JSON export under the `insights` section

## When It Runs

Insights are finalized only after workloads finish.

- While a run is active, `GET /api/insights` returns `metadata.status = pending`.
- Once complete, the endpoint returns final analysis (`ready` / `empty` / `disabled`).

This behavior avoids presenting partial or misleading findings during execution.

## Data Collection Model

PLGM captures sampled operation events during workload execution, with bounded retention.

Each sampled event may include:
- operation type
- database and collection
- normalized shape key and shape summary
- extracted filter fields (when applicable)
- duration
- success/failure
- iteration index
- timestamp

Retention characteristics:
- sampled (configurable sampling rate)
- bounded ring buffer (`insights_max_events`)
- bounded aggregation cardinality (`insights_max_groups`)

This prevents unbounded memory growth while preserving useful signal.

## What Insights Contains

Top-level sections in the final report:
- `summary`
- `slow_queries`
- `affected_collections`
- `query_shapes`
- `potential_index_issues`
- `recommendations`
- `per_iteration`
- `time_slices`
- `metadata`

## Stable Shape IDs and Cross-Run Trends

Each shape group has a stable `shape_id` derived from:
- operation
- collection
- normalized shape key

This enables consistent identity across runs.

PLGM also keeps a lightweight in-memory baseline to show trend hints (for matching shapes), e.g.:
- improved
- worse
- flat

## Optional Explain Sampling (Off by Default)

An optional post-run explain mode can enrich evidence for top slow shapes.

Important design choices:
- disabled by default
- runs only post-run
- limited to top-N shapes
- bounded by max explain execution time
- falls back to heuristic messaging if explain is unavailable

If explain sampling is enabled, index issue messages may be upgraded when evidence is observed (for example, explain indicating `COLLSCAN`).

## Index Advice Philosophy

PLGM uses confidence-aware wording and does not overstate certainty.

Possible evidence levels:
- heuristic
- heuristic with index-overlap/no-overlap signals
- explain-based evidence (when enabled and successful)

Typical language intentionally uses cautious terms like:
- "possible missing index"
- "collection scan is possible"
- "validate with explain"

## Web UI Configuration

Path: `Advanced -> Insights Analysis`

Available controls:
- Enable Post-Run Insights Analysis
- Enable Post-Run Explain Sampling (Optional)
- Insights Sampling Rate
- Slow Threshold (ms)
- Max Retained Events
- Max Group Entries
- Explain Top N Shapes
- Explain Max Time (ms)

All settings are applied per run and included in exported summary config.

## API Contract

`GET /api/insights`

Typical states:
- `inactive`: no collector/run context
- `pending`: run still active
- `ready`: completed report available
- `empty`: no sampled events in buffer
- `disabled`: insights disabled via configuration

The payload is read-only and designed for UI or future dashboard consumers.

## Export Contract

`Download Summary` includes:
- final benchmark summary fields
- `insights` object identical to post-run API/UI model
- redacted password handling preserved

## Configuration Reference

Config file keys:
- `insights_enabled`
- `insights_sampling_rate`
- `insights_slow_threshold_ms`
- `insights_max_events`
- `insights_max_groups`
- `insights_explain_enabled`
- `insights_explain_top_n`
- `insights_explain_max_time_ms`

Environment overrides:
- `PLGM_INSIGHTS_ENABLED`
- `PLGM_INSIGHTS_SAMPLING_RATE`
- `PLGM_INSIGHTS_SLOW_THRESHOLD_MS`
- `PLGM_INSIGHTS_MAX_EVENTS`
- `PLGM_INSIGHTS_MAX_GROUPS`
- `PLGM_INSIGHTS_EXPLAIN_ENABLED`
- `PLGM_INSIGHTS_EXPLAIN_TOP_N`
- `PLGM_INSIGHTS_EXPLAIN_MAX_TIME_MS`

## Recommended Starting Values

For general usage:
- sampling rate: `0.10`
- slow threshold: `200ms`
- max events: `5000`
- max groups: `300`
- explain sampling: disabled

For deeper troubleshooting (short test windows):
- sampling rate: `0.25` to `1.0`
- explain sampling: enabled
- top N shapes: `3` to `5`
- explain max time: `1000` to `3000`

## Use Cases

1. Fast post-run triage
- Identify top slow groups immediately after completion.

2. Collection hotspot detection
- Detect which collections account for most slow patterns.

3. Safe index investigation shortlist
- Generate candidate fields/patterns to validate with DBA workflows.

4. Iteration and timeline context
- Compare behavior across iterations and time slices.

5. CI / automated benchmarking exports
- Consume structured `insights` JSON for pipelines/reports.

## Known Limitations

- Sampling means results are representative, not exhaustive.
- Heuristic index advice is not a guarantee of missing index root cause.
- Explain enrichment depends on representative sample availability and access.
- Trend persistence is in-memory; it does not survive process restarts.
- Explanations are intentionally post-run only to protect active benchmark performance.

## Future Enhancements

Potential next steps for a full insights dashboard:
- persistent historical run storage for long-term trend analysis
- richer explain-plan capture and comparison views
- cross-run diff reports and regression alerts
- deeper per-shape drill-down and filter playback tools

