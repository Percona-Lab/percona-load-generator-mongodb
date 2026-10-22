# Web UI

PLGM ships with an embedded Web UI. You configure the connection, choose or upload your workload, set the operation mix, and watch throughput and latency live, without editing a YAML file. It has the same functionality as the CLI. Every setting shows what happens when it is on and when it is off, plus its `config.yaml` key and environment variable.

[Back to the main README](../README.md)

- [Starting the UI](#starting-the-ui)
- [Accessing the UI on a remote host](#accessing-the-ui-on-a-remote-host-recommended-ssh-tunnel)
- [Your first run](#your-first-run)
- [Settings tabs](#settings-tabs)
- [The Observability Dashboard](#the-observability-dashboard)
- [The "Time Machine" Scrubber and Drag-to-Zoom](#the-time-machine-scrubber-and-drag-to-zoom)
- [Workload Lifecycle & Timing Semantics](#workload-lifecycle--timing-semantics)
- [Shutting down](#shutting-down)

---

## Starting the UI

Pass the `--webui` flag. PLGM starts a local server bound to the loopback interface (`127.0.0.1`) and opens your default browser on port 9999. You can set a custom port:

```bash
./bin/plgm --webui
# Or, set a custom port:
./bin/plgm --webui --webui-port=32000
```

### HTTP vs. HTTPS (no certificate warnings)

By default the UI is served over plain **HTTP bound to loopback** (`http://127.0.0.1:<port>/`). Because the server never listens on a public interface, traffic stays on your machine, and browsers treat `http://localhost`/`http://127.0.0.1` as a *secure context*, so you get **no "your connection is not private" / `ERR_CERT_AUTHORITY_INVALID` warnings**.

Enable HTTPS only if you intend to expose the UI beyond loopback. For a **warning-free** HTTPS endpoint, supply a certificate/key that your clients already trust:

```bash
./bin/plgm --webui --webui-cert=/path/to/tls.crt --webui-key=/path/to/tls.key
```

If you enable TLS without providing a certificate (`--webui-tls` alone), PLGM falls back to a temporary self-signed certificate. Browsers will warn until it is trusted, so this is not recommended for day-to-day use.

Equivalent `config.yaml` (`web_ui.tls_enabled`, `web_ui.tls_cert_file`, `web_ui.tls_key_file`) and environment variables (`PLGM_WEBUI_TLS`, `PLGM_WEBUI_TLS_CERT`, `PLGM_WEBUI_TLS_KEY`) are also available.

| Config Setting | Environment Variable | Description | Example |
| :--- | :--- | :--- | :--- |
| `enabled` | `PLGM_WEBUI_ENABLED` | Force launch the Web UI | `true` |
| `port` | `PLGM_WEBUI_PORT` | Port for the Web UI | `9999` |
| `tls_enabled` | `PLGM_WEBUI_TLS` | Serve the Web UI over HTTPS (default HTTP on loopback, no cert warnings) | `false` |
| `tls_cert_file` | `PLGM_WEBUI_TLS_CERT` | TLS certificate file for the Web UI (for warning-free HTTPS) | `/etc/ssl/plgm.crt` |
| `tls_key_file` | `PLGM_WEBUI_TLS_KEY` | TLS private key file for the Web UI | `/etc/ssl/plgm.key` |

## Accessing the UI on a remote host (recommended: SSH tunnel)

When PLGM runs on a remote server (for example, in your lab or cluster), the **recommended and secure** way to reach the UI is an SSH local port-forward. Run PLGM normally on the remote host (default HTTP on loopback, no TLS needed), then from your workstation:

```bash
# Forward local port 9999 to 127.0.0.1:9999 on the remote host
ssh -L 9999:127.0.0.1:9999 youruser@remote-host
```

Then open `http://localhost:9999` in your local browser.

**This is safe and introduces no network vulnerability.** With this setup, the only unencrypted segments are the loopback interfaces on each end, which are never exposed to the network:

```
[your browser] --http--> localhost:9999 (your machine's loopback)
                              │
                              │  encrypted by SSH  ← the only network hop
                              ▼
   [sshd on remote] --http--> 127.0.0.1:9999 (remote loopback; PLGM listens here)
```

- SSH encrypts the entire network path between your workstation and the remote host. That is the only segment an attacker on the network could observe, and it is fully protected.
- PLGM binds only to `127.0.0.1` on the remote host, and `ssh -L` binds only to your workstation's loopback, so the UI is never reachable from the network on either end.

Because SSH already provides the encryption, **you do not need to enable HTTPS/TLS for this workflow**. Doing so would be redundant and would bring back browser certificate warnings. Enabling TLS is only useful if you expose PLGM directly on a network interface without an SSH tunnel.

## Your first run

1. **Execution / Runtime**: enter the **MongoDB URI**, and optionally **Username**, **Password**, **Auth Source**, **ReplicaSet Name**, and **Read Preference**. Passwords are never logged and are kept in session storage.
2. **Data / Query Mix**: keep the built-in defaults selected in the **Saved Definition Library**, or import your own collection and query files. Set **Seed Document Count** and the **Operation Mix Allocation** (the total must be 100%).
3. **Workload Configuration**: set **Concurrency (Active Workers)** and **Duration (e.g. 60s, 5m)**.
4. Click **Launch Workload** at the bottom right of the screen.
5. Watch the dashboard. Click **Stop Workload** to end the run early.
6. When the run finishes, review **Query & Index Analysis**, then click **Download Summary** or **Export HTML Report** to keep the results.

![Execution / Runtime tab: connection, TLS client files, connection pool, and runtime tuning](../images/settings_execution.png)

This walkthrough runs the bundled `query_analysis_demo` workload, which mixes index-backed queries with deliberately unindexed ones, against a local MongoDB:

1. **Configure:** set the connection, workload shape, query mix, and the insights, explain, and trend settings, then upload the demo collection and query definitions.
2. **Run:** seed 100,000 documents and run for 60 seconds while the Live Metrics and Latency Analysis dashboards update, including the latency percentile timeline, cumulative distribution, and heatmap.
3. **Analyze:** after the run, PLGM samples explain plans and opens the Query & Index Analysis views: per-shape efficiency, slow queries and index issues with a detail drawer (summary, execution plan, recommendation, diagnostics, query reference), and trends compared with a previous run. The Insights overview, per-iteration, and timeline views follow.

https://github.com/user-attachments/assets/f133fd5a-baf8-4d6f-89e1-c179b83c4bef

## Settings tabs

Settings are grouped into tabs on the left. Each tab links to the guide that explains it.

| Tab | What you set there | Guide |
| :--- | :--- | :--- |
| **Execution / Runtime** | Connection (URI, credentials, auth source, replica set, read preference), TLS client files, connection pool, batch sizes, seeding batch size and concurrency, op timeout | [Performance Tuning](./performance-tuning.md) |
| **Workload Configuration** | Load profile, concurrency, duration, iterations, think time, access pattern, sharding strategy | [Load Modeling](./load-modeling.md), [Performance Tuning](./performance-tuning.md#workload-iterations--scheduling) |
| **Data / Query Mix** | Collection and query definitions, seeding, transactions, operation mix, schema inference | [Workloads](./workloads.md) |
| **Record & Replay** | Random seed, deterministic traffic, recording, replay, multi-target, outcomes, traffic import | [Record and Replay](./record-and-replay.md) |
| **Metrics & Insights** | CSV export, post-run insights capture | [Reports and Exports](./reports-and-exports.md), [Query and Index Analysis](./query-analysis.md) |
| **Query Analysis (Explain)** | Post-run explain sampling, which shapes are explained, explain limits, latency targets | [Query and Index Analysis](./query-analysis.md) |
| **Trends & History** | Runs to keep, expiry, default comparison, run label, store path, stored runs, export and import | [Trends and History](./trends-and-history.md) |
| **Expert / Debug** | Retry controls, Raw Injector | [Raw Injector](./raw-injector.md) |

Validation errors open the tab that holds the setting they name. A screenshot of each tab is in the guide linked from its row.

## The Observability Dashboard

Once the workload begins, the UI switches to a real-time dashboard with four tabs: **Live Metrics**, **Latency Analysis**, **Query & Index Analysis**, and **Insights**.

![Live Metrics during a run: per-operation totals and rates, live throughput and latency charts, and the Workload Anatomy donut](../images/dashboard_live.png)

![Latency Analysis: percentile timeline, cumulative latency distribution, and the latency heatmap](../images/dashboard_latency.png)

* **Live Telemetry:** throughput (Ops/sec) and average latency (ms) stream in real time for each operation type.
* **Workload Anatomy:** a live donut chart shows that the database is executing the operation ratios you configured.
* **Crosshair Sync:** hovering over a spike on the Throughput chart highlights the same moment on the Latency chart.
* **Drag-to-Zoom:** click and drag across the Throughput, Latency, or Latency Percentile Timeline charts to isolate a time range. All dashboard graphs and summary values update to that window.
* **Latency Analysis:** percentile timeline, cumulative distribution, and the [latency heatmap](./load-modeling.md#latency-heatmap-over-time).
* **Record & Replay runs:** cards for the traffic fingerprint and per-target status, progress, lag, and starting-data check.
* **After the run:** [Query & Index Analysis](./query-analysis.md#read-the-results), **Insights**, **Download Summary**, and **Export HTML Report** ([Reports and Exports](./reports-and-exports.md)).

## The "Time Machine" Scrubber and Drag-to-Zoom

If you are running a long benchmark, you might miss a sudden latency spike. The UI keeps a running history buffer of the benchmark data.

Use the **Time Machine** slider above the charts to pause the live feed and scrub backward to a specific second. All line charts, sparklines, and numeric values synchronize to show the state of the database at that point in time.

To investigate a wider window, click and drag across any main time-series chart. PLGM zooms all dashboard charts to the selected range, including throughput, average latency, latency percentiles, sparklines, numeric totals, and workload distribution. This makes it easier to isolate a latency spike, throughput drop, or short workload burst without waiting for the run to finish.

Click **Reset Zoom** to return to the normal chart window. Click **Back to Live** to clear any historical view and resume real-time monitoring. Moving the Time Machine slider also clears the current zoom selection, because the slider is used for point-in-time inspection.

![Throughput dragged to zoom on 00:14 to 00:21: every chart, total, and sparkline shows that window, with Reset Zoom and Back to Live](../images/dashboard_time_machine_zoom.png)

## Workload Lifecycle & Timing Semantics

PLGM shows an explicit lifecycle so you can tell preparation apart from measured workload execution:

* `initializing`: collection setup, index creation, sharding/runtime preparation, optional seeding
* `running`: actual query workload execution
* `completed` / `failed`: terminal states

Important timing behavior:

* Initialization time is tracked separately and is **not** treated as execution runtime
* During initialization, the header clock counts preparation time and is labeled `init`. The progress bar follows the current setup step
* The execution timer and progress bar start when the workload itself starts. Preparation time is not included in that clock
* With more than one iteration and an interval delay, the total on the clock includes the pauses between runs. Five runs of 30s with 10s between them is 03:10 (02:30 of workload plus four 10s pauses; there is no pause after the last run). A short note under the clock states that once execution starts. It stays hidden during initialization, while the clock is counting preparation time. See [Workload Iterations](./performance-tuning.md#workload-iterations--scheduling)
* With deterministic traffic, Duration is the length of the measured run. Operations per Worker is one pass. When a worker finishes that pass, it continues the same workload until Duration ends. The header stays on the duration clock (`00:04 / 00:30`). **FINISHED** means the run filled that time; the note gives operations sent and the size of one pass. **INCOMPLETE** means Duration ended before every worker finished one pass. The traffic fingerprint is that first pass, so two runs can share it and still send different totals. See [Deterministic Generated Traffic](./record-and-replay.md#deterministic-generated-traffic)
* During initialization, the UI shows progress context (step name, progress %, and recent activity events). Seeding progress advances as documents are inserted, including inside one large collection
* Reloading the page during a run returns to that run and restores the live charts from samples kept for it, then continues. Starting again while one is already going opens the run in progress

Current limitation:

* Index creation and sharding progress are still per collection, not per index or shard
* Chart samples kept for a reload are capped at 3,600 points (about 30 minutes at the default 500ms refresh)

## Shutting down

Click **EXIT** in the header, then **Shut Down Backend**, to stop PLGM from the browser. Background workers are cleaned up, and converted traffic files are deleted.
