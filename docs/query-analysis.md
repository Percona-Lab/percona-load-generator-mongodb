# Query and Index Analysis

Latency tells you that a query is slow. It doesn't tell you why, and it says nothing about the query that is fast today only because the collection is still small. After the workload finishes, PLGM runs explain on samples of every query shape it executed (fast ones included) and shows what the server actually did: how much it scanned, whether it sorted in memory or spilled to disk, and which index would help.

This page covers setup and the views. The full reference (explain support matrix, status and reason glossary, confidence model, troubleshooting, and limitations) is in [`INSIGHTS.md`](../INSIGHTS.md).

[Back to the main README](../README.md)

- [Set up the analysis in the Web UI](#set-up-the-analysis-in-the-web-ui)
- [Read the results](#read-the-results)
- [Try it with the demo workload](#try-it-with-the-demo-workload)
- [Settings reference](#settings-reference)
- [What it does not do](#what-it-does-not-do)

---

## Set up the analysis in the Web UI

It's on by default. To check or change it:

1. Choose a task and turn on **Advanced** (and **Expert** for the limits and latency targets).
2. **Metrics & reporting > Insights Capture**: keep **Enable Post-Run Insights Analysis** on. **Slow Threshold (ms)** decides which operations count as slow (default 200). **Insights Sampling Rate** is the share of operations kept for analysis (default 0.10).

   ![Metrics & reporting: CSV export and Insights Capture](../images/settings_metrics_insights.png)

3. **Query analysis > Post-Run Explain**: keep **Enable Post-Run Explain Sampling** on. Without it you get latency and workload impact, but no scan efficiency, sort and spill details, or index advice.

   ![Query analysis category: explain on, which queries are explained, limits, and latency targets](../images/settings_query_analysis.png)

4. Optional (Expert): set **Latency Target Avg / p95 / p99 (ms)** to have each query rated against your targets. Left empty, latency is shown but not rated.
5. Click **Launch Workload**.

Explain runs after the workload ends, so it doesn't affect your benchmark numbers.

## Read the results

When the run ends, open **Query & Index Analysis**. It has four views.

### Efficiency

One row per query shape, worst first.

![Efficiency view: the unindexed updateOne scores 0, the rest score 100](../images/query_efficiency.png)

- **Scan efficiency** (0 to 100, higher is better) compares documents and index keys examined with documents returned. 100 means about one document examined per result. 0 means a thousand or more, or a collection scan where an index should be used. It's a PLGM rule of thumb, not a MongoDB metric.
- **Band** sums it up: efficient, moderate, or inefficient. Shapes that weren't explained show "no score"; inserts show "not applicable".
- **Query latency**: avg, p95, and p99. Rated only if you set targets.
- **Workload impact**: how often the shape ran and its share of total operation time. A fast query run thousands of times a second can cost more than a slow report run twice.
- **Execution overhead**: in-memory sorts and disk spills.

Click a row (or **Details**) for the full story of one shape:

![Shape details: the updateOne on customer_email scans 146,183 documents and matches none](../images/query_shape_details.png)

Here the explained plan is `UPDATE -> COLLSCAN`: every update by email reads the whole collection.

### Slow Queries & Indexes

Slow queries, likely index problems, and a ranked list of things to try. **Explore** opens the details.

![Slow Queries & Indexes: the critical updateOne, its collection scan, and recommendations](../images/query_findings.png)

The **Recommendation** tab says what PLGM saw, what to try, and how sure it is:

![Finding Details, Recommendation tab](../images/query_finding_drawer.png)

Suggested index keys follow equality, then sort, then range, and are checked against your existing indexes. Test them on a non-production copy first.

### Trends

Compares each shape with the same shape in earlier runs. See [Trends and History](./trends-and-history.md).

### Replay Comparison

Compares two replays operation by operation. See [Record and Replay](./record-and-replay.md#read-the-results).

### Insights tab

**Overview** ranks shapes by their share of total operation time and lists the affected collections. **Iterations** and **Timeline** split the run by iteration and by time.

![Insights Overview: workload impact ranked by operation time](../images/insights_overview.png)

The same analysis is in **Download Summary** (JSON) and **Export HTML Report**. See [Reports and Exports](./reports-and-exports.md).

## Try it with the demo workload

The repo includes a demo with one deliberately unindexed query: [`resources/collections/query_analysis_demo.json`](../resources/collections/query_analysis_demo.json) and [`resources/queries/query_analysis_demo.json`](../resources/queries/query_analysis_demo.json). The screenshots above come from it.

1. Choose **Run a custom workload**. In **Data & Query Definitions**, import both files with **Save to Library**. They're selected for you.
2. Set **Seed Document Count** to `100000` and **Duration** to `60s`.
3. Optional: set **Slow Threshold** to `10` ms so the slow update is listed under Slow Queries.
4. Click **Launch Workload**, then open **Query & Index Analysis** when it finishes.

You should see `updateOne on fields: customer_email` at the top: scan efficiency 0, a collection scan, and most of the operation time. Add an index on `customer_email` to the collection definition, run again with the same **Run label**, and **Trends** shows it go from 0 to 100.

For a quick look at the whole UI, including the analysis view after a run, see the walkthrough video:

https://github.com/user-attachments/assets/daf25637-7d0d-4f3a-9d97-eeaa3b4835e4

## Settings reference

| Config Setting | Environment Variable | Description | Default |
| :--- | :--- | :--- | :--- |
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

[`INSIGHTS.md`](../INSIGHTS.md) also covers `executionStats` vs `queryPlanner` guidance, severity-based explain eligibility, filtering counters, expected outcomes, and query traceability fields.

## What it does not do

- Explain runs after the workload ends, not during it, so your benchmark numbers are not affected.
- It looks at samples, not every operation. Each shape gets at most two explained samples (one near the median, one near p95), within a budget of shapes. Shapes outside the budget are marked "unassessed", never "efficient".
- Missing data is shown as missing ("not measured"), never as zero.
- Lock waits, network time, and CPU are not measured.
