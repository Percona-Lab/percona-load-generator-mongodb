# Concurrency Sweep

A **concurrency sweep** runs the same workload several times, once for each worker count in a list, for example 4, 8, 16, and 32. Every run starts from the same data, so you can see how throughput and latency change as concurrency grows, and where more workers stop helping.

[Back to the main README](../README.md)

- [When to use it](#when-to-use-it)
- [Run a sweep](#run-a-sweep)
- [Settings](#settings)
- [What a sweep does](#what-a-sweep-does)
- [Read the results](#read-the-results)
- [Make the sweep fair](#make-the-sweep-fair)
- [Stopping and failures](#stopping-and-failures)
- [Sweep or Step?](#sweep-or-step)
- [Limits](#limits)
- [API](#api)

---

## When to use it

- You want to know how many workers a server can serve before latency climbs.
- You want to compare two servers, or the same server before and after a change, at several concurrency levels. Run one sweep on each and compare them in [Compare Runs](./compare-runs.md).
- You want one setting that replaces starting four runs by hand and changing **Concurrency** between them.

## Run a sweep

1. Choose **Run a generic workload** or **Run a custom workload**, then open **Workload Control** in **Basic settings**.
2. Set **Duration** to the length of **each** run, for example `2m`. It is not the length of the whole sweep.
3. Set **Load Profile** to **Sweep — one run per worker count**.
4. In **Worker counts to test**, list 2 to 8 counts separated by commas, for example `4, 8, 16, 32`. Under the field, PLGM shows how many runs that is and about how long it takes in total.
5. Keep **Drop Collections First** and **Seed documents** on, so every run starts from the same data. See [Make the sweep fair](#make-the-sweep-fair).
6. Optional: set a **Run label** in **Advanced > Trends & history**, for example `mongo-8.0`. Without one, the sweep is named `Sweep <date and time>`.
7. Click **Launch Workload**.

The dashboard shows the first run. A banner above the charts says which run of the sweep is going (`Sweep of 4 runs (4, 8, 16, 32 workers): Level 2 of 4: 8 workers`). When a run ends, the dashboard opens the next one by itself. When the last run ends, the banner shows **Compare this sweep**.

## Settings

| Setting (Web UI) | Form field | What it does |
| :--- | :--- | :--- |
| **Load Profile: Sweep** | `load_profile_kind=sweep` | Runs the workload once for each worker count. |
| **Worker counts to test** | `load_profile_sweep_workers` | 2 to 8 whole numbers from 1 to 10000, separated by commas, spaces, or semicolons. The order is the order they run. No count may be listed twice. |
| **Duration** | `duration` | Length of **each** run. |
| **Iterations** | `iterations` | Iterations inside each run. Usually leave at 1. |
| **Interval Delay** | `interval_delay` | The pause between two runs of the sweep (and between iterations inside a run). `0s` starts the next run at once. |
| **Run label** | `trend_run_label` | The base name. Each run is named `<label> - <N> workers`. A label longer than will fit in 64 bytes is shortened, never the worker count. |
| **Concurrency** | `concurrency` | Not used by a sweep; each run uses its own count from the list. |

A sweep is a Web UI feature. The command line and `config.yaml` run one worker count at a time.

## What a sweep does

Each worker count is an ordinary run:

- It prepares its data like any run: it creates the collections, drops them first if **Drop Collections First** is on, and seeds the documents.
- It has its own dashboard, report, insights, and settings, and is stored in **Completed Runs** under the name `<label> - <N> workers`.
- It has a fixed number of workers for its whole **Duration**.

The runs of one sweep share a **sweep tag**. **Completed Runs** shows it under the run (`Sweep run 2 of 4 · 8 workers`) and changes the **Compare** button to **Compare sweep**. Compare Runs uses the tag to select every run of the sweep at once and to show the results table.

The run settings are saved once, when the first run starts. The runs after it have the same settings except the worker count, so they are not saved again. Loading the saved file brings the sweep back.

While a sweep is going, no other run can be started. The Web UI says so, and **Stop** ends the whole sweep.

## Read the results

Open **Compare Runs** (the **Compare this sweep** button in the banner, **Compare sweep** in **Completed Runs**, or the **Compare Runs** tab). Above the charts, each stored sweep has a results table:

| Column | Meaning |
| :--- | :--- |
| **Workers** | The fixed worker count of the run. |
| **Throughput (ops/s)** | Operations per second over the time the workers ran. |
| **vs first** | Throughput compared with the first row (the lowest count). `×1.93` means 93% more operations per second than the first row. |
| **Avg latency (ms)** | Average latency of all operations. |
| **p95 (ms)** | 95 out of 100 operations were faster than this. |
| **Status** | `completed` or `failed`. |

Click **Show on charts** under a sweep, or use **Compare sweep** to draw its runs on the throughput, latency, and tail latency charts, one line per run. The lines all start at 0, so you can see each run at the same point in time. See [Compare Runs](./compare-runs.md#read-the-charts).

How to read the table:

- **Throughput rises about as fast as the worker count, and latency stays flat.** The server has room. Try higher counts.
- **Throughput stops rising, and latency climbs.** The server is saturated. The count before the throughput stopped growing is the useful limit.
- **Throughput drops with more workers.** The workers are competing for something, such as locks, the cache, or disk. Look at the tail latency (**p95**) and the **Insights** tab of the highest run.
- The client machine can be the limit, too. If the highest counts do not scale, check the CPU of the machine running PLGM before you blame the server.

## Make the sweep fair

The runs only compare well if they start from the same state.

- Keep **Drop Collections First** on and **Seed documents** on. Each run then drops and re-creates the data, and every worker count starts from the same documents. With **Seed documents** off, all runs use whatever is in the database, and writes of the earlier runs change what the later ones see.
- Use a **Duration** long enough for the larger counts to warm up. A 30-second run at 32 workers spends a large part of its time ramping up.
- Use **Interval Delay** (for example `30s`) to let the server settle between runs, if background work such as checkpoints or index builds from the previous run matters to you.
- Keep the same **Operation Mix** and **Documents** for the whole sweep. They are the same for every run, because the runs share one form.
- Run on similar hardware, with similar load on the server and on the client machine.

## Stopping and failures

- **Stop** ends the run that is going and the rest of the sweep. The runs that finished stay stored. The stopped run is stored too, with the part it ran.
- If a run fails (for example, the server is unreachable), the sweep stops there. The banner names the run (`level 2 (8 workers) failed: ...`). The runs before it stay stored; the runs after it do not start.
- Between two runs (during **Interval Delay**), **Stop** also works, and the next run does not start.
- Reloading the page during a sweep reattaches the dashboard to it.

## Sweep or Step?

Both change the worker count, but they answer different questions.

| | **Sweep** | **Step** |
| :--- | :--- | :--- |
| Runs | One run per worker count | One run with stages |
| Starting state | Each count starts from freshly seeded data | Later stages run on what the earlier ones changed |
| Warm-up | Each count warms up on its own | Warm-up and caches carry over from the stages before |
| Stored as | One entry per count in **Completed Runs** | One entry |
| Compare | A results table and one line per count | Stages in one chart over time |
| Time | Sum of the runs and the pauses, plus data preparation for each | The sum of the stages |

Use **Sweep** to measure each concurrency level on its own. Use [Step](./load-modeling.md#load-profiles-load-shaping) when you want to watch how one run reacts as the load changes.

## Limits

- A sweep has 2 to 8 worker counts. Compare Runs draws up to 8 lines.
- A sweep runs a **generic** or **custom** workload. It cannot be combined with **Record**, **Replay**, or **Raw Injector**. Choose **Fixed** for those.
- **Deterministic traffic** needs a fixed worker count, so it cannot be turned on with a sweep. Each worker count would send different operations anyway (the operations depend on the worker).
- Each run is stored in **Completed Runs**, so a sweep needs as many stored runs as it has counts. **Runs to keep** defaults to 5 (1 to 50) in **Advanced > Trends & history**, and older unpinned runs are removed first. The summary under **Worker counts to test** warns when **Runs to keep** is lower than the number of runs. Raise it, or **Pin** the runs you need. See [Trends and History](./trends-and-history.md#history-labels-and-pinning).
- The total time is the sum of the runs, the pauses, and the data preparation of each run. The summary under the field does not include data preparation.
- A sweep on several servers (**Run on: Several servers**) is not covered by the results table.
- The server runs one sweep at a time. Restarting PLGM during a sweep ends it; the runs that finished stay stored.

## API

- `POST /api/start` with `load_profile_kind=sweep` and `load_profile_sweep_workers=4,8,16,32` starts a sweep. The response is the response of the first run. Errors (`400`: fewer than 2 or more than 8 counts, a count that is not a whole number from 1 to 10000, a repeated count, or Record, Replay, or Raw Injector mode; `409`: a run or sweep is already going) have the same `{"status": "error", "message": ...}` body as any start.
- `GET /api/stats` has a `sweep` object while a sweep runs, and while the last run of a finished sweep is the current one: `id`, `label`, `level`, `levels`, `workers`, `current_workers`, `state` (`running`, `waiting`, `done`, `stopped`, or `failed`), `active`, `message`, and `pause_sec`.
- `GET /api/run-history` lists each run of a sweep with `sweep`: `id`, `label`, `level`, `levels`, and `workers`. Group the runs by `sweep.id` to build your own table.
- `POST /api/stop` ends a sweep, including between two runs.
