# plgm: Percona Load Generator for MongoDB Clusters

**plgm** is a load generator and benchmark tool for sharded and non-sharded MongoDB clusters, written in Go. It tests your database with your own schema and your own queries instead of synthetic data, so the numbers you get reflect how your application will behave.

Use it to:

* **Benchmark your own workload.** Define your collections and queries, seed realistic data with every standard BSON type, and run find, insert, update, delete, aggregate, and transactional operations at the ratios you choose.
* **Model real traffic.** Ramp, step, spike, or oscillate the worker count, add client think time, and send most traffic to a hot set of documents.
* **Find out why a query is slow.** After the run, PLGM explains every query shape it executed and reports scan efficiency, in-memory sorts and disk spills, and index recommendations.
* **Track changes between runs.** Keep a history of runs, pin reference runs, and compare up to eight side by side.
* **Test upgrades and migrations.** Record a workload once and replay the exact same operations on two or more MongoDB versions, then compare results and latency operation by operation. You can also replay traffic captured from your mongod logs.

It ships as a single binary with an embedded Web UI, uses all available CPUs by default (`GOMAXPROCS`), and is fully configurable through `config.yaml`, environment variables, or the browser.

![PLGM Web UI live dashboard during a run](./images/dashboard_live.png)

---

## Quick Start

### 1. Install

**Option 1: Download a release**

Go to the Releases page, download the `.tar.gz` file for your operating system, and extract it:

```bash
# Example for Linux
tar -xzvf plgm-linux-amd64.tar.gz

# Example for Mac (Apple Silicon)
tar -xzvf plgm-darwin-arm64.tar.gz
```

**Option 2: Build from source** (requires Go 1.25+)

```bash
git clone https://github.com/Percona-Lab/percona-load-generator-mongodb.git
cd percona-load-generator-mongodb
go mod tidy

# Build a binary for your CURRENT machine only (no .tar.gz)
make build-local

# Run it
./bin/plgm --help
```

To build release packages for Linux and Mac, run `make build`. It writes `bin/plgm-linux-amd64.tar.gz`, `bin/plgm-darwin-amd64.tar.gz`, and `bin/plgm-darwin-arm64.tar.gz`.

### 2. Configure

Download [`config.yaml`](./config.yaml) and set your connection string. The built-in default workload runs without any other files. If you use the Web UI, you can set everything in the browser instead.

### 3. Run

```bash
# Web UI (recommended): opens http://127.0.0.1:9999 in your browser
./bin/plgm --webui

# Headless CLI
./bin/plgm
```

In the UI, choose **Run a generic workload**, enter your **MongoDB URI**, and click **Launch Workload**. See [The Web UI](./docs/web-ui.md#your-first-run) for a step-by-step first run.

---

## Documentation

Each guide explains a feature, shows how to set it up in the Web UI step by step, and lists the matching `config.yaml` settings and environment variables.

| Guide | What it covers |
| :--- | :--- |
| [Web UI](./docs/web-ui.md) | Starting the UI, HTTPS, remote access over SSH, your first run, the settings categories, the live dashboard, and the Time Machine |
| [Running from the Command Line](./docs/cli.md) | CLI configuration, flags, `--help` output, and how to read the console output |
| [Workloads](./docs/workloads.md) | Default and custom workloads, the saved definition library, data types, indexes and shard keys, seeding, operation mix, transactions, and sharding |
| [Load Modeling](./docs/load-modeling.md) | Load profiles, think time, access-pattern skew, the latency heatmap, and schema inference from query logs |
| [Query and Index Analysis](./docs/query-analysis.md) | Post-run explain, scan efficiency, execution overhead, index recommendations, and latency targets |
| [Trends and History](./docs/trends-and-history.md) | Run history, labels and pinning, comparing runs, the run matrix, sharing, and where history is stored |
| [Record and Replay](./docs/record-and-replay.md) | Upgrade testing: recording, replaying, deterministic traffic, outcomes, comparing replays, multi-target replay, and importing captured traffic |
| [Reports and Exports](./docs/reports-and-exports.md) | CSV metrics export, the JSON summary, and the HTML run report |
| [Raw Injector](./docs/raw-injector.md) | High-throughput hardware stress test mode |
| [Performance Tuning](./docs/performance-tuning.md) | CPU, concurrency, connection pooling, batch sizes, timeouts, iterations, driver options, and TLS |
| [Configuration Reference](./docs/configuration-reference.md) | Every `config.yaml` setting and environment variable in one table |

More references:

* [`INSIGHTS.md`](./INSIGHTS.md): the full query analysis reference (explain support matrix, statuses, confidence model, troubleshooting, and limitations).
* [Docker & Kubernetes Guide](./k8s_and_docker.md): building images and running PLGM jobs inside Kubernetes (recommended for accurate network latency testing).
* [`TESTING.md`](./TESTING.md): unit and integration tests, including a Docker-based MongoDB setup.
* [`CHANGELOG.md`](./CHANGELOG.md): release notes.

---

# Disclaimer

This application is not supported by Percona. It has been provided as a community contribution and is not covered under any Percona services agreement.
