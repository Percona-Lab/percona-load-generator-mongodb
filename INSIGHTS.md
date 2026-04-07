# Post-Run Insights: Slow Query and Index Analysis

This guide explains how PLGM's post-run insights work, how to configure them, and what outcomes to expect.

## What This Feature Does

After a workload fully finishes, PLGM analyzes sampled operation events and produces:
- ranked slow query groups
- affected collections and shape hotspots
- potential index issues with confidence labels
- explain-backed diagnostics (optional)
- recommended next actions

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
- `insights_explain_enabled: false`
- `insights_explain_top_n: 5`
- `insights_explain_max_time_ms: 1000`
- `insights_explain_verbosity: executionStats`
- `insights_explain_severity_mode: high_only`
- `insights_explain_workers: 1`
- `insights_explain_retries: 1`
- `insights_explain_backoff_ms: 150`

Recommended baseline for most users:
- Keep explain disabled for pure benchmark fidelity runs.
- Enable explain only when doing troubleshooting.
- Keep explain workers low (`1`) and top-N small (`3-5`).

## Configuration Reference

Web UI path:
- `Metrics & Insights -> Explain Analysis`

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
| `insights_explain_workers` | `PLGM_INSIGHTS_EXPLAIN_WORKERS` | Post-run explain worker count |
| `insights_explain_retries` | `PLGM_INSIGHTS_EXPLAIN_RETRIES` | Retries for explain timeout/failure |
| `insights_explain_backoff_ms` | `PLGM_INSIGHTS_EXPLAIN_BACKOFF_MS` | Retry backoff between explain attempts |

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
- Explain candidate selection runs after filtering.
- Severity eligibility is applied before explain selection.

Transparency counters are exposed in metadata and shown in UI status line:
- `filtered_by_threshold`
- `filtered_by_severity`

## Retention/Sampling vs Explain: How They Interact

Think of this as a two-stage pipeline:

1. Insights Capture (Retention & Sampling)
- Controls whether operation events are recorded at all (`insights_enabled`).
- Controls sampling rate (`insights_sampling_rate`) and retention caps (`insights_max_events`, `insights_max_groups`).
- Determines memory footprint and the amount of post-run evidence available.
- Applies slow-threshold filtering (`insights_slow_threshold_ms`) for report relevance.

2. Explain Enrichment (Post-Run Explain Analysis)
- Runs only after workload completion.
- Uses retained sampled events to find representative explain candidates.
- Requires retained filter/pipeline metadata for supported operation types.
- Adds deeper plan/evidence signals and recommendation confidence.

Practical dependency:
- Explain does not run during workload execution.
- Explain depends on what was retained by capture/sampling.
- If capture is disabled, explain cannot enrich anything meaningful.
- If capture is too sparse, explain may return `insufficient_metadata` or leave shapes unattempted.

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
- shape identity and trend
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

## Practical Use Cases

1. Benchmark triage after a run
- Identify top `critical/high` slow groups quickly.

2. Index investigation backlog
- Use explain-backed findings first, heuristic findings second.

3. Regression checks across iterations
- Track trend direction and p95/p99 deltas by shape.

4. Reporting and incident artifacts
- Export final summary JSON with `insights` for sharing with DBAs/SREs.

5. Explain budget control
- Keep explain scoped with threshold + severity mode + top-N limits.

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

## Limitations

- Sampling is representative, not exhaustive tracing.
- Trend baseline is process-local and resets on restart.
- Explain depends on retained replay metadata and server permissions.
- Top-N explain means some shapes intentionally remain unattempted.
- Heuristic guidance is intentionally cautious and not proof of root cause.
