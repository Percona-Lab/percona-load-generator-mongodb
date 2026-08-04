# plgm: Percona Load Generator for MongoDB Clusters

**plgm** is a high-performance tool written in Go, designed to effortlessly generate data and simulate heavy workloads for both sharded and non-sharded MongoDB clusters.

It simulates real-world usage patterns by generating random data using robust BSON data types and executing standard CRUD operations (Find, Insert, Update, Delete) based on configurable ratios.

This tool is a complete refactor of the previous Python version, offering:
* **Single Binary:** No complex dependencies or Python environment setup.
* **Interactive Web UI:** A beautiful dashboard for configuring and running workloads and monitoring live telemetry.
* **High Concurrency:** Utilizes Go goroutines ("Active Workers") to generate massive load with minimal client-side resource usage.
* **Configuration as Code:** Fully configurable via a simple `config.yaml` file or Environment Variables.
* **Extensive Data Support:** Supports all standard MongoDB BSON data types (ObjectId, Decimal128, Date, Binary, etc.) and realistic data generation via `gofakeit` (supporting complex nested objects and arrays).
* **True Parallelism:** Unlike the previous Python version, this tool automatically detects and utilizes all available logical CPUs (`GOMAXPROCS`) by default to maximize hardware efficiency.

---

## Quick Start

### 1. Installation

**Option 1: Download Release**

Navigate to the Releases page and download the .tar.gz file matching your operating system.

1. Download and Extract

```bash
# Example for Linux
tar -xzvf plgm-linux-amd64.tar.gz

# Example for Mac (Apple Silicon)
tar -xzvf plgm-darwin-arm64.tar.gz
```

**Option 2: Build from Source** (Requires Go 1.25+)

This project includes a `Makefile` to simplify building and packaging.

```bash
git clone [https://github.com/Percona-Lab/percona-load-generator-mongodb.git](https://github.com/Percona-Lab/percona-load-generator-mongodb.git)
cd percona-load-generator-mongodb
go mod tidy

# Build a binary for your CURRENT machine only (no .tar.gz)
make build-local

# Run it
./bin/plgm --help
```

**Cross-Compilation (Build for different OS)**

If you are preparing binaries for other users (or other servers), use the main build command. This will compile binaries for Linux and Mac and automatically package them into .tar.gz files in the `bin/` folder.

```bash
# Generate all release packages
make build

# Output:
# bin/plgm-linux-amd64.tar.gz
# bin/plgm-darwin-amd64.tar.gz
# bin/plgm-darwin-arm64.tar.gz
```

### 2. Configuration

To run the application you need a configuration file. Depending on whether you want to run the built-in workload or your own custom, you may also need to create resource folders.

**Step A: Get the Config**

Download the [`config.yaml`](./config.yaml) and make the necessary adjustments.

### 3. Choose Your Interface

`plgm` can be run completely headless via the CLI, or via a rich interactive Web Dashboard.

* **Run via CLI (Headless):**
  ```bash
  ./bin/plgm
  ```
* **Run via Web UI:**
  ```bash
  ./bin/plgm --webui
  ```

### 4. Testing

For unit and integration test instructions (including Docker-based MongoDB setup), see [`TESTING.md`](./TESTING.md).

### 5. Post-Run Insights

PLGM includes a post-run **Slow Query and Index Analysis** layer that is configurable from the Web UI (Advanced tab).

Quick access:
* Full guide: [`INSIGHTS.md`](./INSIGHTS.md)
* Web UI path: `Advanced -> Insights Analysis`
* Output: Dashboard insights panel + `Download Summary` JSON (`insights` section)
* Includes explain support matrix, status/reason glossary, confidence model, troubleshooting, and limitations
* Includes `executionStats` vs `queryPlanner` guidance, severity-based explain eligibility, filtering counters, and expected outcomes
* Includes query traceability fields so each finding can be mapped to a concrete query source/definition
* Includes concise findings cards + Explore drawer workflow with execution-plan stage flow and technical details tabs
* Includes mixed sharded/non-sharded runtime support with per-collection auto detection and explicit sharding strategy controls in the UI

### 6. Workload Lifecycle & Timing Semantics

PLGM now exposes an explicit lifecycle so users can distinguish preparation from measured workload execution:

* `initializing`: collection setup, index creation, sharding/runtime preparation, optional seeding
* `running`: actual query workload execution
* `completed` / `failed`: terminal states

Important timing behavior:

* Initialization time is tracked separately and is **not** treated as execution runtime
* Execution timer and progress bar start from the execution phase
* During initialization, the UI shows progress context (step name, progress %, and recent activity events)

Current limitation:

* Initialization progress is structured and user-visible, but still coarse-grained (step/event based)
* It is not yet per-index/per-shard real-time streaming

## The Interactive UI

`plgm` features a completely embedded Web UI. It allows you to configure your database connection, upload custom workload schemas, adjust operation ratios, and monitor real-time throughput and latency without ever touching a YAML file. It has the same functionality as the CLI version, but with an awesome UI.

### 1. Starting the UI
To launch the UI, simply pass the `--webui` flag. The application starts a local server bound to the loopback interface (`127.0.0.1`) and automatically opens your default web browser on port 9999. You can also set a custom port instead, as shown in the example below:

```bash
./bin/plgm --webui
# Or, set a custom port:
./bin/plgm --webui --webui-port=32000
```

#### HTTP vs. HTTPS (no certificate warnings)

By default the UI is served over plain **HTTP bound to loopback** (`http://127.0.0.1:<port>/`). Because the server never listens on a public interface, traffic stays on your machine, and browsers treat `http://localhost`/`http://127.0.0.1` as a *secure context* — so you get **no "your connection is not private" / `ERR_CERT_AUTHORITY_INVALID` warnings**.

Enable HTTPS only if you intend to expose the UI beyond loopback. For a **warning-free** HTTPS endpoint, supply a certificate/key that your clients already trust:

```bash
./bin/plgm --webui --webui-cert=/path/to/tls.crt --webui-key=/path/to/tls.key
```

If you enable TLS without providing a certificate (`--webui-tls` alone), PLGM falls back to a temporary self-signed certificate — browsers will warn until it is trusted, so this is not recommended for day-to-day use.

Equivalent `config.yaml` (`web_ui.tls_enabled`, `web_ui.tls_cert_file`, `web_ui.tls_key_file`) and environment variables (`PLGM_WEBUI_TLS`, `PLGM_WEBUI_TLS_CERT`, `PLGM_WEBUI_TLS_KEY`) are also available.

#### Accessing the UI on a remote host (recommended: SSH tunnel)

When PLGM runs on a remote server (for example, in your lab or cluster), the **recommended and secure** way to reach the UI is an SSH local port-forward. Run PLGM normally on the remote host (default HTTP on loopback — no TLS needed), then from your workstation:

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

- SSH encrypts the entire network path between your workstation and the remote host — that is the only segment an attacker on the network could observe, and it is fully protected.
- PLGM binds only to `127.0.0.1` on the remote host, and `ssh -L` binds only to your workstation's loopback, so the UI is never reachable from the network on either end.

Because SSH already provides the encryption, **you do not need to enable HTTPS/TLS for this workflow** — doing so would be redundant and would reintroduce browser certificate warnings. Enabling TLS is only useful if you expose PLGM directly on a network interface without an SSH tunnel.

### PLGM UI Sample Usage

**Default Workload**

<video src="https://github.com/user-attachments/assets/1d1d9641-1b11-4b6c-b82f-ff85bd18c4d6" controls width="700"></video>

**Custom Workload**

<video src="https://github.com/user-attachments/assets/e05d08ed-77ea-4ca1-9fee-9206811295f6" controls width="700"></video>

**Raw Injector Workload**

<video src="https://github.com/user-attachments/assets/d3ff03b0-4941-4cef-88b3-df4e3c012fa2" controls width="700"></video>

**Time machine**

<video src="https://github.com/user-attachments/assets/68a57bb0-f500-4f75-99c3-c599d485444a" controls width="700"></video>

**Multiple Iterations**

You can configure how PLGM repeats a given workload and control the time between runs in the `Workload` tab. Set the iterations value to determine how many times the workload will run, and configure the interval to define the time between each run. See [Workload Iterations & Scheduling](#workload-iterations--scheduling) section for more details.

<video src="https://github.com/user-attachments/assets/58c51250-8acc-46ce-a756-8f897f73a2b2" controls width="700"></video>


### 2. UI Features

When running `plgm` with the `--webui` flag, you get access to a rich, browser-based dashboard that requires no external dependencies. 

* **Configuration Editor:** Edit connection pools, timeouts, and workload distributions on the fly without needing a `config.yaml` file.
* **Live Telemetry & Dashboard:** Watch operations per second (Find, Insert, Update, Delete) and latencies update in real-time with sub-second precision.
* **The "Time Machine" Scrubber and Drag-to-Zoom:** Pause the live feed, scrub backward to a specific second, or drag across a chart to zoom into a specific time range.
* **Real-Time CSV Export:** Configure and stream metrics to a local CSV file directly from the Advanced tab. Use the "Append" feature to stitch multiple benchmark runs into a single dataset.
* **Post-Run Insights Analysis:** Review slow-query groups, affected collections, potential index issues, and recommendations after all iterations complete.
* **Graceful Shutdown:** Click the **EXIT** button in the header to safely terminate close the application directly from the browser, ensuring all background workers are cleaned up properly.

### 3. Configuration
The UI provides an intuitive tabbed interface to configure every aspect of your workload. 

* **Connection:** Set your URI, read preferences, and connection pooling limits. Passwords are never logged and are safely managed via session storage.

![PLGM Configuration UI - Connection Tab](./images/connection.png)

* **Workload & Mix:** Upload your own custom `collections.json` and `queries.json` files directly through the browser, and use visual inputs to ensure your Find/Insert/Update/Delete ratios total exactly 100%. You can also run the default workload by clicking the `Use built-in Default Workload` checkbox.

![PLGM Configuration UI - Connection Tab](./images/workload.png)

![PLGM Configuration UI - Mix Tab](./images/mix.png)

* **Advanced & Raw Injector:** Fine-tune batch sizes, timeouts, or override the standard workload with the ultra-high-performance Raw Injector.

![PLGM Configuration UI - Advanced Tab](./images/advanced.png)

![PLGM Configuration UI - Raw Injector Tab](./images/raw_injector.png)

### 4. The Observability Dashboard
Once the workload begins, the UI transitions to a real-time observability dashboard.

![PLGM Observability Dashboard (Top)](./images/dashboard_top.png)
![PLGM Observability Dashboard (Bottom)](./images/dashboard_bottom.png)

* **Live Telemetry:** Watch throughput (Ops/sec) and average latency (ms) stream in real-time across four distinct operation categories.
* **Workload Anatomy:** A live-updating donut chart proves that your database is accurately executing the exact operation ratios you configured.
* **Crosshair Sync:** Hovering over a spike on the Throughput chart will instantly highlight the exact same moment in time on the Latency chart.
* **Drag-to-Zoom:** Click and drag across the Throughput, Latency, or Latency Percentile Timeline charts to isolate a specific time range. All dashboard graphs and summary values update to that same window.

### 5. The "Time Machine" Scrubber and Drag-to-Zoom
If you are running a long benchmark, you might miss a sudden latency spike. The UI stores a running history buffer of the benchmark data. 

Use the **Time Machine** slider above the charts to pause the live feed and scrub backward to a specific historical second. All line charts, sparklines, and numeric values synchronize to show the exact state of the database at that point in time.

To investigate a wider window, click and drag across any main time-series chart. PLGM will zoom all dashboard charts to the selected range, including throughput, average latency, latency percentiles, sparklines, numeric totals, and workload distribution. This makes it easier to isolate a latency spike, throughput drop, or short workload burst without waiting for the run to finish.

Click **Reset Zoom** to return to the normal chart window. Click **Back to Live** to clear any historical view and resume real-time monitoring. Moving the Time Machine slider also clears the current zoom selection because the slider is used for point-in-time inspection.

![PLGM Time Machine Feature](./images/time_machine.png)

### 6. Post-Run Slow Query & Index Analysis
Once PLGM is configured and the Insights and Analysis feature is enabled, you will receive a detailed analysis of your slow queries upon completion of the workload. Configuration and enabling this feature can be done via the `Metrics & Insights` section of the UI (or via the config file), see [`INSIGHTS.md`](./INSIGHTS.md) for further details:

![Metrics and Insights](./images/metrics_insights.png)
![Insights Configuration](./images/insights_config.png)

The output, once the workload is completed, will look like the following:

<img src="./images/insights.png" alt="post_run_insights" width="100%">
<img src="./images/finding_summary.png" alt="finding_summary" width="100%">
<img src="./images/finding_exec_plan.png" alt="finding_execution_plan" width="50%">
<img src="./images/finding_rec.png" alt="finding_recommendation" width="50%">
<img src="./images/finding_diag.png" alt="finding_diagnostics" width="50%">
<img src="./images/finding_query_ref.png" alt="finding_query_reference" width="50%">

### 7. User-Managed Query and Collection Definitions
The Web UI can save reusable query and collection definition files so they do not need to be uploaded every time a workload is started.

In the `Workload` tab, the **Data & Query Definitions** section drives the workload entirely from the **Saved Definition Library**. Each of the Collection Definitions and Query Definitions cards has a single library dropdown that is the primary way to choose what runs. From there you can:

* Select the built-in default collection and query definitions directly from the library dropdowns. The built-in defaults are pre-selected so a run is always ready without extra steps.
* See a short summary of which definition is currently selected for the run.
* Import a collection or query definition JSON file via **Save to Library**, which validates the file, stores it, and selects it for the run.
* Select previously saved definitions from the dropdowns.
* View and edit the selected query definition in the built-in JSON editor (**Create or edit a query definition**), then save edits back to the selected definition or save the edited content as a new definition. Editing a built-in default and saving creates a new custom definition, since built-ins are read-only.
* Use a one-off JSON file for a single run without saving it: choose a file in the import section and run without clicking **Save to Library**. An unsaved imported file is used for that run only and overrides the library selection.

Uploaded and edited definitions are validated before they are saved. PLGM rejects empty files, malformed JSON, invalid workload schemas, duplicate namespaces in collection definitions, duplicate query names in a query definition, duplicate saved definition names, and duplicate saved content.

Definition storage is durable application storage, separate from browser caching. By default PLGM stores definitions in a local JSON file under the user config directory:

```bash
~/.config/plgm/definitions.json
```

On macOS this typically resolves under:

```bash
~/Library/Application Support/plgm/definitions.json
```

You can override the location with:

```bash
export PLGM_DEFINITIONS_STORE=/path/to/plgm-definitions.json
```

The last selected query and collection definitions are cached in browser `localStorage` so the Web UI can restore your previous selections after refresh. This cache only stores selected IDs; the actual definition content is stored server-side in the JSON store.

Storage decision: PLGM uses local file-based JSON storage because it has no existing database dependency, usually runs as a single local or single-container Web UI process, and needs simple durable persistence with easy backup and restore. SQLite would be a reasonable future option if richer querying, heavier concurrent editing, or multi-user metadata becomes necessary. Valkey/Redis is not used because PLGM does not require distributed cache/state for this feature and adding it would increase operational complexity without improving the single-instance workflow.

## CLI 

#### 1. Configuration

Once you have configured your connection settings and any other parameters, you can choose your workload.

* **Mode 1: Default Workload (Easiest)**
    
    By default (`default_workload: true` in `config.yaml`), the application uses the embedded collection and query definitions. You do **not** need to create any extra folders or files.

* **Mode 2: Custom Workload**
    
    To run your own stress tests, you must set `default_workload: false` in `config.yaml` and provide the necessary files:

    1.  **Create Directories**: Create folders for your definitions (e.g., `resources/collections` and `resources/queries`).
    2.  **Add Files**: Place your JSON schema and query definitions inside these folders.
    3.  **Update Config**: Ensure `collections_path` and `queries_path` in your `config.yaml` point to these new directories.

    > **Important:** If you are running in Custom Mode, the application expects these folders to exist. If the folders are missing, `plgm` will revert to the embedded defaults to prevent a crash, but your custom test **will not run** until the files are in place.

#### 2. Run

Once configured, run the application:

```bash
# The extracted binary will have the OS suffix
./plgm-linux-amd64
```

#### 3. CLI Usage

To view the full usage guide, including available flags and environment variables, run the help command:

```bash
./plgm --help

plgm: Percona Load Generator for MongoDB Clusters
Usage: ./plgm [flags] [config_file]

Examples:
  ./plgm                    # Run with default 'config.yaml'
  ./plgm my_test.yaml       # Run with specific config file
  ./plgm --help             # Show this help message

Flags:
  -config string
      Path to the configuration file (default "config.yaml")
  -raw-injector
      Enable Raw BSON Injector (High Performance Mode)
  -raw-injector-batch int
      Bulk batch size (ops per network round trip) (default 1000)
  -raw-injector-drop
      Drop the collection before starting
  -raw-injector-max-docs int
      Maximum number of documents to operate on (default 10000000)
  -raw-injector-size int
      Document size in bytes (default 1024)
  -raw-injector-type string
      Operation: insert, upsert, update, delete, find, mixed (default "insert")
  -version
      Print version information and exit
  -webui
      Start the interactive Web UI
  -webui-port int
      Port for the Web UI (default 9999)
  -webui-tls
      Serve the Web UI over HTTPS (default: HTTP on loopback, no cert warnings)
  -webui-cert string
      Path to a TLS certificate file for the Web UI (implies --webui-tls)
  -webui-key string
      Path to the TLS private key file for the Web UI (implies --webui-tls)

Environment Variables (Overrides):
 [Connection]
  PLGM_URI                            Connection URI
  PLGM_USERNAME                       Database User
  PLGM_PASSWORD                       Database Password (Recommended: Use Prompt)
  PLGM_DIRECT_CONNECTION              Force direct connection (true/false)
  PLGM_REPLICASET_NAME                Replica Set name
  PLGM_READ_PREFERENCE                nearest

 [Web UI]
  PLGM_WEBUI_ENABLED                  Start the interactive Web UI (true/false)
  PLGM_WEBUI_PORT                     Port for the Web UI (default: 9999)
  PLGM_WEBUI_TLS                      Serve Web UI over HTTPS (true/false)
  PLGM_WEBUI_TLS_CERT                 TLS certificate file path for the Web UI
  PLGM_WEBUI_TLS_KEY                  TLS private key file path for the Web UI

 [Workload Core]
  PLGM_DEFAULT_WORKLOAD               Use built-in workload (true/false)
  PLGM_COLLECTIONS_PATH               Path to collection JSON
  PLGM_QUERIES_PATH                   Path to query JSON
  PLGM_DURATION                       Test duration (e.g. 60s, 5m)
  PLGM_ITERATIONS                     Number of times to repeat the workload
  PLGM_INTERVAL_DELAY                 Time to pause between iterations (e.g. 5s, 1m)
  PLGM_CONCURRENCY                    Number of active workers
  PLGM_DOCUMENTS_COUNT                Initial seed document count
  PLGM_DROP_COLLECTIONS               Drop collections on start (true/false)
  PLGM_SKIP_SEED                      Do not seed initial data on start (true/false)
  PLGM_SEED_BATCH_SIZE                Number of documents to insert per batch during SEED phase
  PLGM_DEBUG_MODE                     Enable verbose logic logs (true/false)
  PLGM_PPROF_ENABLED                  Enable pprof server on localhost:6060 (true/false)
  PLGM_USE_TRANSACTIONS               Enable transactional workloads (true/false)
  PLGM_MAX_TRANSACTION_OPS            Maximum number of operations to group into a single transaction block

 [Raw Injector Mode] (High Performance Hardware Test)
  PLGM_INJECTOR                       Enable Raw Injector mode (true/false)
  PLGM_INJECTOR_TYPE                  Operation: insert, upsert, update, delete, find, mixed
  PLGM_INJECTOR_SIZE                  Document size in bytes
  PLGM_INJECTOR_BATCH_SIZE            Operations per network batch (default: 1000)
  PLGM_INJECTOR_MAX_DOCS              Total documents to operate on (default: 10M)
  PLGM_INJECTOR_DROP                  Drop collection on start (true/false)
  PLGM_INJECTOR_DB                    Database name
  PLGM_INJECTOR_COLLECTION            Collection name

 [Operation Ratios] (Must sum to ~100)
  PLGM_FIND_PERCENT                   % of ops that are FIND
  PLGM_UPDATE_PERCENT                 % of ops that are UPDATE
  PLGM_INSERT_PERCENT                 % of ops that are INSERT
  PLGM_DELETE_PERCENT                 % of ops that are DELETE
  PLGM_AGGREGATE_PERCENT              % of ops that are AGGREGATE
  PLGM_TRANSACTION_PERCENT            % of ops that are TRANSACTIONAL
  PLGM_BULK_INSERT_PERCENT            % of ops that are BULK INSERTS

 [Performance Optimization]
  PLGM_FIND_BATCH_SIZE                Docs returned per cursor batch
  PLGM_INSERT_BATCH_SIZE              Number of docs in batch bulk insert
  PLGM_FIND_LIMIT                     Max docs per Find query
  PLGM_INSERT_CACHE_SIZE              Generator buffer size
  PLGM_OP_TIMEOUT_MS                  Soft timeout per DB op (ms)
  PLGM_RETRY_ATTEMPTS                 Retry attempts for failures
  PLGM_RETRY_BACKOFF_MS               Wait time between retries (ms)
  PLGM_STATUS_REFRESH_RATE_SEC        Status report interval (sec)
  GOMAXPROCS                          Go Runtime CPU limit
```

## Default Workload

Regardless of rather you are running PLGM from CLI or UI, it comes with a built-in default workload useful for immediate testing and get you started right away.
```bash
# Edit config.yaml to set your URI, then run:
./bin/plgm
```

**Note about default workload:** plgm comes pre-configured with a [default collection](./resources/collections/default.json) and [default queries](./resources/queries/default.json). If you do not provide any parameters and leave the configuration setting `default_workload: true`, this default workload will be used.

If you wish to use a different default workload, you can replace these two files with your own default.json files in the same paths. This allows you to define a different collection and set of queries as the default workload.

**Note on config file usage:** If you do not specify the config file name (above example), plgm will use the [config.yaml](./config.yaml) by default. You can create separate configuration files if you wish and then pass it as an argument:

```bash
./bin/plgm /path/to/some/custom_config.yaml
```

## Raw Injector Mode (High Performance Hardware Test) Workload

The **Raw Injector** is a specialized, ultra-high-performance engine built directly into `PLGM`. Instead of using the standard MongoDB driver structs (which consume CPU and memory for BSON marshaling), the Raw Injector pre-compiles raw BSON byte arrays and performs zero-allocation bitwise mutations in a tight loop.

This mode is designed exclusively to **stress-test network throughput, disk I/O, and extreme CPU limits** of the MongoDB cluster. It can saturate high-end infrastructure significantly faster than standard workloads. This functionality was developed to mimic other benchmarking tools, enabling an apples-to-apples comparison between PLGM and alternative solutions.

Unlike the other workloads PLGM supports, this mode is not configurable. It was intentionally built solely for stress testing purposes and is not intended to function as a workload simulator, but instead, a simple benchmark stress test.

***Important Note on Ops/Sec: In Raw Injector mode, the printed Ops/Sec refers to the number of documents processed, not the number of network commands. For example, if your batch size is 1,000, and plgm reports 50,000 Ops/Sec, it is executing 50 bulk network commands per second.***

### Understanding Raw Injector Throughput (Ops/Sec)

When running in **Raw Injector Mode**, the Web UI and CLI may report extremely high throughput numbers (e.g., 100,000+ Ops/Sec) that appear significantly higher than the command rates shown in Percona Monitoring and Management (PMM). This is expected behavior due to how the high-performance engine is architected.

#### Document-Level vs. Command-Level Metrics
The "Ops/Sec" reported by PLGM refers to individual **documents** processed. To achieve maximum performance, the Raw Injector bypasses standard BSON marshaling and groups thousands of operations into a single network round-trip command.

#### The "Batch Size" Discrepancy
The relationship between PLGM reporting and standard "Opcounters" in PMM is defined by your configured `batch_size`. 

* **PLGM Reporting**: Counts every individual document within a batch. For example, with a `batch_size` of **2,000**, a report of **100,000 Ops/Sec** means 100,000 records are being processed per second.
* **PMM Opcounters**: Standard monitoring counts the **network commands**. In the example scenario above, PMM Opcounters will show only **50 operations per second** (100,000 \ 2,000 = 50).

***How to Verify High Throughput in PMM: To see PMM metrics that align with the high numbers in the PLGM dashboard, you must look at **Document Operations** (storage engine activity) rather than **Opcounters** (protocol activity).***

#### Performance Note: Duplicate Keys
To maintain maximum stress-test velocity, the Raw Injector is programmed to ignore **Duplicate Key Errors** during `insert` or `mixed` workloads to prevent the generator from stalling. If you are inserting into a collection that already contains the target data, PLGM will report high "Ops/Sec" (attempts), but MongoDB's internal `inserted` counters in PMM will remain at **0** because the storage engine did not need to write new records.

### Configuration

You can configure the Raw Injector via `config.yaml` or directly via CLI flags/environment variables. These are the available options:

```yaml
# config.yaml snippet
raw_injector:
  enabled: true
  drop_collection: true
  type: "mixed"            # Options: insert, upsert, find, update, delete, mixed
  document_size: 1024      # Size of the random binary payload in bytes
  max_docs: 10000000       # Total documents to generate/query
  batch_size: 1000         # Number of operations packed into a single network round-trip
  db_name: "plgm_injector"
  collection_name: "injector_data"
```

Environment variables:

```bash
  PLGM_INJECTOR                       Enable Raw Injector mode (true/false)
  PLGM_INJECTOR_TYPE                  Operation: insert, upsert, update, delete, find, mixed
  PLGM_INJECTOR_SIZE                  Document size in bytes
  PLGM_INJECTOR_BATCH_SIZE            Operations per network batch (default: 1000)
  PLGM_INJECTOR_MAX_DOCS              Total documents to operate on (default: 10M)
  PLGM_INJECTOR_DROP                  Drop collection on start (true/false)
  PLGM_INJECTOR_DB                    Database name
  PLGM_INJECTOR_COLLECTION            Collection name
```

### Modes of Operation (type)

 * insert: Floods the database with new documents. Automatically pre-splits chunks if sharding is enabled.
 * find / update / delete: Operates on the existing data seeded by an insert run.
 * upsert: Fires upsert commands, creating documents if they don't exist.
 * mixed: Runs a randomized distribution of reads, inserts, updates, and deletes simultaneously.

### Running via CLI
You can bypass the YAML config entirely and trigger a raw injection test purely through flags:

##### Run a pure insert flood, dropping existing data, with 4KB documents

```bash
./plgm -raw-injector -raw-injector-type=insert -raw-injector-drop -raw-injector-size=4096 -raw-injector-batch=2000
```

##### Run a mixed workload against the generated data

```bash
./plgm -raw-injector -raw-injector-type=mixed
```

<details>
<summary>Sample output:</summary>

```bash
./plgm -raw-injector -raw-injector-type=insert -raw-injector-drop -raw-injector-size=4096 -raw-injector-batch=2000
  [INFO] Performance: Automatically disabled driver compression (compressors=none)
Enter Password for user 'root':
2026/02/23 11:26:45 [RawInjector] Dropping collection 'injector_data'...
2026/02/23 11:26:47 [RawInjector] Range sharding enabled on { _id: 1 }
2026/02/23 11:26:47 [RawInjector] Pre-splitting: 4 workers -> 2 shards
2026/02/23 11:26:48 >>> RAW INJECTOR START [mixed] workers=4 batch=2000 maxDocs=10000000 docSize=4096 <<<

> Starting Workload...

 TIME    | TOTAL OPS |  SELECT |  INSERT |  UPSERT |  UPDATE |  DELETE |    AGG | TRANS
 -----------------------------------------------------------------------------------------
 00:01   |    76,000 |  48,000 |  10,000 |       0 |  18,000 |       0 |      0 |     0
 00:02   |   160,000 | 108,000 |  10,000 |       0 |  30,000 |  12,000 |      0 |     0
 00:03   |   140,000 |  90,000 |  18,000 |       0 |  20,000 |  12,000 |      0 |     0
 00:04   |   142,000 |  78,000 |   8,000 |       0 |  38,000 |  18,000 |      0 |     0
 00:05   |   160,000 | 112,000 |  12,000 |       0 |  28,000 |   8,000 |      0 |     0
 00:06   |   140,000 |  78,000 |  12,000 |       0 |  34,000 |  16,000 |      0 |     0
 00:07   |   138,000 |  72,000 |   4,000 |       0 |  40,000 |  22,000 |      0 |     0
 00:08   |   156,000 |  98,000 |   4,000 |       0 |  44,000 |  10,000 |      0 |     0
 00:09   |   144,000 |  84,000 |   6,000 |       0 |  38,000 |  16,000 |      0 |     0
 00:10   |   180,000 | 126,000 |   4,000 |       0 |  34,000 |  16,000 |      0 |     0

> Workload Finished.

  SUMMARY
  --------------------------------------------------
  Runtime:    10.00s
  Total Ops:  1,444,000
  Avg Rate:   144,400 ops/sec

  LATENCY DISTRIBUTION (ms)
  --------------------------------------------------
  TYPE             AVG          MIN          MAX          P95          P99
  SELECT       0.01 ms      0.01 ms      0.03 ms      0.00 ms      0.00 ms
  INSERT       0.07 ms      0.04 ms      0.15 ms      0.00 ms      0.00 ms
  UPSERT             -            -            -            -            -
  UPDATE       0.04 ms      0.04 ms      0.07 ms      0.00 ms      0.00 ms
  DELETE       0.04 ms      0.03 ms      0.06 ms      0.00 ms      0.00 ms
  AGG                -            -            -            -            -
  TRANS              -            -            -            -            -
```
</details>

##### Run the workload configuring it via any of the available environment variables

<details>
<summary>Sample output:</summary>

```bash
export PLGM_PASSWORD=super_duper_password_here
PLGM_INJECTOR=true PLGM_INJECTOR_TYPE=mixed PLGM_DURATION=30s ./plgm
  [INFO] Performance: Automatically disabled driver compression (compressors=none)
2026/02/23 11:48:26 [RawInjector] Range sharding enabled on { _id: 1 }
2026/02/23 11:48:26 [RawInjector] Pre-splitting: 4 workers -> 2 shards
2026/02/23 11:48:27 [RawInjector] Checking for existing data to resume sequences...
2026/02/23 11:48:27 >>> RAW INJECTOR START [mixed] workers=4 batch=1000 maxDocs=10000000 docSize=200 <<<

> Starting Workload...

 TIME    | TOTAL OPS |  SELECT |  INSERT |  UPSERT |  UPDATE |  DELETE |    AGG | TRANS
 -----------------------------------------------------------------------------------------
 00:01   |    65,000 |  41,000 |   4,000 |       0 |  11,000 |   9,000 |      0 |     0
 00:02   |    79,000 |  53,000 |   3,000 |       0 |  14,000 |   9,000 |      0 |     0
 00:03   |    85,000 |  52,000 |   4,000 |       0 |  18,000 |  11,000 |      0 |     0
 00:04   |    77,000 |  44,000 |   3,000 |       0 |  21,000 |   9,000 |      0 |     0
 00:05   |    93,000 |  53,000 |   9,000 |       0 |  16,000 |  15,000 |      0 |     0
 00:06   |    98,000 |  63,000 |   4,000 |       0 |  24,000 |   7,000 |      0 |     0
 00:07   |    99,000 |  70,000 |   1,000 |       0 |  17,000 |  11,000 |      0 |     0
 00:08   |    93,000 |  54,000 |   4,000 |       0 |  26,000 |   9,000 |      0 |     0
 00:09   |   121,000 |  84,000 |   8,000 |       0 |  18,000 |  11,000 |      0 |     0
 00:10   |    96,000 |  55,000 |   6,000 |       0 |  29,000 |   6,000 |      0 |     0
 00:11   |   113,000 |  73,000 |   7,000 |       0 |  23,000 |  10,000 |      0 |     0
 00:12   |   102,000 |  67,000 |   4,000 |       0 |  18,000 |  13,000 |      0 |     0
 00:13   |   124,000 |  87,000 |   7,000 |       0 |  26,000 |   4,000 |      0 |     0
 00:14   |   114,000 |  73,000 |   6,000 |       0 |  27,000 |   8,000 |      0 |     0
 00:15   |   117,000 |  73,000 |  12,000 |       0 |  20,000 |  12,000 |      0 |     0
 00:16   |   109,000 |  67,000 |   5,000 |       0 |  31,000 |   6,000 |      0 |     0
 00:17   |   127,000 |  89,000 |   9,000 |       0 |  15,000 |  14,000 |      0 |     0
 00:18   |   121,000 |  86,000 |   2,000 |       0 |  19,000 |  14,000 |      0 |     0
 00:19   |   116,000 |  78,000 |   4,000 |       0 |  16,000 |  18,000 |      0 |     0
 00:20   |   105,000 |  71,000 |   5,000 |       0 |  18,000 |  11,000 |      0 |     0
 00:21   |   129,000 |  91,000 |   9,000 |       0 |  24,000 |   5,000 |      0 |     0
 00:22   |   121,000 |  83,000 |   3,000 |       0 |  27,000 |   8,000 |      0 |     0
 00:23   |   130,000 |  94,000 |   4,000 |       0 |  22,000 |  10,000 |      0 |     0
 00:24   |   118,000 |  82,000 |   6,000 |       0 |  18,000 |  12,000 |      0 |     0
 00:25   |   111,000 |  71,000 |   7,000 |       0 |  21,000 |  12,000 |      0 |     0
 00:26   |   109,000 |  74,000 |   5,000 |       0 |  20,000 |  10,000 |      0 |     0
 00:27   |   117,000 |  77,000 |   3,000 |       0 |  25,000 |  12,000 |      0 |     0
 00:28   |   111,000 |  72,000 |   5,000 |       0 |  28,000 |   6,000 |      0 |     0
 00:29   |   118,000 |  76,000 |   6,000 |       0 |  27,000 |   9,000 |      0 |     0
 00:30   |   111,000 |  67,000 |   6,000 |       0 |  25,000 |  13,000 |      0 |     0

> Workload Finished.

  SUMMARY
  --------------------------------------------------
  Runtime:    30.00s
  Total Ops:  3,233,000
  Avg Rate:   107,766 ops/sec

  LATENCY DISTRIBUTION (ms)
  --------------------------------------------------
  TYPE             AVG          MIN          MAX          P95          P99
  SELECT       0.02 ms      0.01 ms      0.06 ms      0.00 ms      0.00 ms
  INSERT       0.02 ms      0.01 ms      0.05 ms      0.00 ms      0.00 ms
  UPSERT             -            -            -            -            -
  UPDATE       0.07 ms      0.04 ms      0.23 ms      0.00 ms      0.00 ms
  DELETE       0.09 ms      0.05 ms      0.17 ms      0.00 ms      0.00 ms
  AGG                -            -            -            -            -
  TRANS              -            -            -            -            -
```

</details>

## Additional Workloads

You will find additional workloads that you can use as references to benchmark your environment in cases where you prefer not to provide your own collection definitions and queries. However, if your goal is to test your application accurately, we strongly recommend creating collection definitions and queries that match those used by your application.

The additional collection and query definitions can be found here:

* [collections](./resources/collections/)
* [queries](./resources/queries/)

## Load Modeling & Analysis

Beyond fixed-concurrency runs, PLGM can shape load over time, pace clients, skew
access patterns, visualize latency drift, infer a workload from logs, and export
shareable reports. All of these are configurable from the Web UI (the **Load
Modeling** and **Schema Inference** cards) and via `config.yaml` / `PLGM_*`
environment variables.

### Load Profiles (load shaping)

Instead of a constant worker count, you can ramp, step, spike, or oscillate
concurrency over the run. The worker pool is sized to the profile's peak, and a
controller gates how many workers are active at any moment. The live target and
active worker counts are exposed in `/api/stats` (`concurrencyTarget`,
`concurrencyActive`).

```yaml
# config.yaml
concurrency: 16            # used as the fixed/fallback worker count
load_profile:
  kind: ramp               # fixed | ramp | step | spike | sine
  start_workers: 1
  end_workers: 50
  ramp_over: 30s
# step example:
#   kind: step
#   steps: [{workers: 5, duration: 30s}, {workers: 20, duration: 60s}]
# spike example:
#   kind: spike
#   base_workers: 5
#   peak_workers: 50
#   spike_at: 20s
#   spike_for: 5s
# sine example:
#   kind: sine
#   min_workers: 5
#   max_workers: 50
#   period: 30s
```

Invalid profiles (bad durations, negative workers, malformed steps) are rejected
with a clear error before the run starts. An empty/`fixed` profile preserves the
historical fixed-concurrency behavior.

Env overrides: `PLGM_LOAD_PROFILE_KIND`, `PLGM_LOAD_PROFILE_WORKERS`.

#### `duration` vs. load-profile timings (`period`, `ramp_over`, `spike_at`, ...)

These are two different kinds of setting and are easy to confuse:

- **`duration`** is the **total run length** of the benchmark (per iteration) —
  the whole window the workload executes for (e.g. `120s`, `5m`).
- **Load-profile timings** describe **what happens *inside* that window**. They
  are always *sub-intervals* of `duration`, so each should be shorter than it:
  - `ramp_over` — how long the ramp from Start to End workers takes.
  - `steps[].duration` — how long each step holds its worker count.
  - `spike_at` / `spike_for` — when the spike starts and how long it lasts.
  - **`period`** (Sine only) — the length of **one full oscillation**
    (Min → Max → Min). It is *not* the run length.

**How `period` relates to `duration` (Sine):** the run completes
`duration ÷ period` oscillations. Keep `period ≤ duration`, and for whole,
symmetric waves make `duration` an integer multiple of `period`.

| `duration` | `period` | Result |
| :--- | :--- | :--- |
| `120s` | `30s` | 4 complete oscillations |
| `60s` | `60s` | Exactly 1 oscillation |
| `60s` | `120s` | Only half an oscillation (Min → Max, run ends before returning) |

The same principle applies to the other profiles: e.g. a `ramp_over: 30s` inside
a `duration: 120s` ramps for the first 30s, then holds at End workers for the
remaining 90s.

### Think Time / Pacing

Add a per-worker delay between operations to emulate realistic clients or hold a
steady, sub-saturation request rate. Pacing time is excluded from measured
operation latency.

```yaml
think_time_ms: 0     # fixed delay between operations
think_jitter_ms: 0   # extra uniform random 0..jitter ms per delay
```

Env overrides: `PLGM_THINK_TIME_MS`, `PLGM_THINK_JITTER_MS`.

### Access-Pattern Skew

Control which existing records are selected for find/update/delete targeting.
Skewed access exposes cache pressure and lock/hot-shard contention that uniform
access hides.

```yaml
access_pattern:
  kind: uniform          # uniform | zipfian | hotspot
  # zipfian:
  #   kind: zipfian
  #   zipfian_exponent: 1.5   # >= 1.0; higher = more skew
  # hotspot:
  #   kind: hotspot
  #   hotspot_percent: 20        # share of records that are "hot"
  #   hotspot_traffic_percent: 80 # share of traffic to the hot set
```

Env overrides: `PLGM_ACCESS_PATTERN_KIND`, `PLGM_ACCESS_PATTERN_ZIPFIAN_EXPONENT`,
`PLGM_ACCESS_PATTERN_HOTSPOT_PERCENT`, `PLGM_ACCESS_PATTERN_HOTSPOT_TRAFFIC_PERCENT`.

### Latency Heatmap Over Time

PLGM buckets latency into ~1&nbsp;second time windows and records p50/p95/p99/max
per window (across **all operation types combined**), so you can see
tail-latency drift over the run. The series is exposed in `/api/stats`
(`latencyHeatmap`) and rendered as an inline chart both in the live **Latency
Analysis** tab and in the exported report.

Each column in the heatmap is one time window. Two independent visual channels
encode the data:

- **Color = absolute p99 latency.** Colors map to fixed latency thresholds
  (based on common database/APM norms), *not* to the run's own maximum. This
  means a fast run stays green even if one window is relatively taller than the
  others — the heatmap will never paint a healthy 10&nbsp;ms run red just
  because it is the busiest window seen so far.

  | p99 (this window) | Color | Interpretation |
  | :--- | :--- | :--- |
  | &le; 10 ms | Green | Excellent |
  | ~25 ms | Lime | Very good |
  | ~50 ms | Yellow | Good |
  | ~100 ms | Orange | Moderate |
  | ~250 ms | Red | Slow |
  | ~500 ms | Strong red | Very slow |
  | &ge; 1 s | Dark red | Critical |

  Values between anchors are interpolated, so the color transitions smoothly.

- **Bar height = relative drift.** Height is scaled to this run's own peak p99,
  so you can always see *when* tail latency rose or fell during the run, even in
  an all-green run where every window is objectively fast.

Hover any column to see the full per-window breakdown (ops in window,
throughput, and p50/p95/p99/max). While the cursor is over the heatmap it
pauses updating so you can inspect a window; it resumes and catches up to the
latest data as soon as the cursor leaves.

> Note: because each window is short, an occasional slow operation (for example
> a heavy aggregation) can lift a single window's p99 above the cumulative p99
> shown in the distribution table. That is expected — surfacing exactly *when*
> tail latency occurred is the purpose of this view.

### Schema Inference from Query Logs

Paste mongod JSON slow-query logs, `system.profile` documents, or raw command
documents into the **Schema Inference** card (or `POST /api/infer-schema` with
`{"log": "..."}`). PLGM infers the collections touched, per-operation
filter/sort/update/projection fields, candidate queries, and a suggested
operation mix (find/insert/update/delete/aggregate percentages) that can pre-fill
the distribution fields. Malformed or unrecognized lines are skipped and reported
rather than failing the whole inference.

### Shareable Run Reports

Click **Export HTML Report** (or open `GET /api/report`) to generate a
self-contained HTML report — no external assets or JavaScript — that includes the
run summary, full configuration, load profile, pacing, access pattern, latency
table, an inline latency-over-time chart, and notable insights/warnings. Use the
browser's Print → Save as PDF to export a PDF.
Add `?download=1` to download the HTML file directly.

## CSV Metrics Export 
When CSV export is enabled, PLGM writes a new row to the file every time the `status_refresh_rate_sec` ticker fires. Because it flushes to disk continuously, your benchmark data is completely preserved even if the test is forcefully interrupted.

The CSV includes the following headers:

`Timestamp,ElapsedSec,Select_OpsSec,Insert_OpsSec,Upsert_OpsSec,Update_OpsSec,Delete_OpsSec,Agg_OpsSec,Trans_OpsSec,Total_Lat_P99,Select_Lat_P99,Insert_Lat_P99,Upsert_Lat_P99,Update_Lat_P99,Delete_Lat_P99,Agg_Lat_P99,Trans_Lat_P99,Iteration`


*Tip: Enable the **Append to existing file** option to run a series of varying workloads (e.g., ramping up concurrency from 4 to 16 to 64) and capture the entire progression in a single, unbroken CSV file for easy graphing!*

<details>
<summary>Sample output:</summary>

`cat /tmp/plgm_metrics_export.csv`

```csv
Timestamp,ElapsedSec,Select_OpsSec,Insert_OpsSec,Upsert_OpsSec,Update_OpsSec,Delete_OpsSec,Agg_OpsSec,Trans_OpsSec,Total_Lat_P99,Select_Lat_P99,Insert_Lat_P99,Upsert_Lat_P99,Update_Lat_P99,Delete_Lat_P99,Agg_Lat_P99,Trans_Lat_P99,Iteration
2026-03-25T11:40:48-04:00,1,1051.00,1570.00,0.00,363.00,177.00,0.00,0.00,5.00,12.00,1.00,0.00,6.00,12.00,0.00,0.00,1
2026-03-25T11:40:49-04:00,2,1239.00,2020.00,0.00,394.00,201.00,0.00,0.00,3.00,4.00,1.00,0.00,3.00,3.00,0.00,0.00,1
2026-03-25T11:40:50-04:00,3,1131.00,2150.00,0.00,399.00,186.00,0.00,0.00,4.00,6.00,1.00,0.00,8.00,4.00,0.00,0.00,1
2026-03-25T11:40:51-04:00,4,1200.00,2170.00,0.00,399.00,199.00,0.00,0.00,4.00,5.00,1.00,0.00,7.00,5.00,0.00,0.00,1
2026-03-25T11:40:52-04:00,5,1244.00,1810.00,0.00,403.00,223.00,0.00,0.00,4.00,4.00,1.00,0.00,4.00,4.00,0.00,0.00,1
2026-03-25T11:40:53-04:00,6,1181.00,2410.00,0.00,408.00,175.00,0.00,0.00,3.00,4.00,1.00,0.00,5.00,3.00,0.00,0.00,1
2026-03-25T11:40:54-04:00,7,1223.00,2130.00,0.00,429.00,226.00,0.00,0.00,3.00,4.00,1.00,0.00,4.00,3.00,0.00,0.00,1
2026-03-25T11:40:55-04:00,8,1220.00,2030.00,0.00,386.00,189.00,0.00,0.00,4.00,5.00,1.00,0.00,7.00,9.00,0.00,0.00,1
2026-03-25T11:40:56-04:00,9,1278.00,2100.00,0.00,428.00,201.00,0.00,0.00,3.00,4.00,0.00,0.00,4.00,5.00,0.00,0.00,1
2026-03-25T11:40:57-04:00,10,1246.00,2230.00,0.00,418.00,216.00,0.00,0.00,3.00,4.00,1.00,0.00,3.00,8.00,0.00,0.00,1
2026-03-25T11:41:03-04:00,1,1210.00,1870.00,0.00,396.00,208.00,0.00,0.00,3.00,4.00,1.00,0.00,6.00,9.00,0.00,0.00,2
2026-03-25T11:41:04-04:00,2,1212.00,2100.00,0.00,379.00,210.00,0.00,0.00,3.00,5.00,1.00,0.00,5.00,5.00,0.00,0.00,2
2026-03-25T11:41:05-04:00,3,1188.00,1740.00,0.00,420.00,206.00,0.00,0.00,5.00,6.00,1.00,0.00,7.00,7.00,0.00,0.00,2
2026-03-25T11:41:06-04:00,4,1250.00,2090.00,0.00,404.00,189.00,0.00,0.00,4.00,5.00,0.00,0.00,4.00,8.00,0.00,0.00,2
2026-03-25T11:41:07-04:00,5,1234.00,2420.00,0.00,423.00,212.00,0.00,0.00,3.00,3.00,1.00,0.00,5.00,11.00,0.00,0.00,2
2026-03-25T11:41:08-04:00,6,1154.00,2230.00,0.00,405.00,220.00,0.00,0.00,3.00,5.00,1.00,0.00,4.00,4.00,0.00,0.00,2
2026-03-25T11:41:09-04:00,7,1046.00,1770.00,0.00,372.00,200.00,0.00,0.00,6.00,9.00,0.00,0.00,7.00,7.00,0.00,0.00,2
2026-03-25T11:41:10-04:00,8,1078.00,2060.00,0.00,368.00,193.00,0.00,0.00,6.00,9.00,1.00,0.00,11.00,6.00,0.00,0.00,2
2026-03-25T11:41:11-04:00,9,1166.00,2020.00,0.00,366.00,209.00,0.00,0.00,5.00,7.00,0.00,0.00,7.00,5.00,0.00,0.00,2
2026-03-25T11:41:12-04:00,10,1192.00,2000.00,0.00,341.00,194.00,0.00,0.00,3.00,7.00,1.00,0.00,4.00,4.00,0.00,0.00,2
2026-03-25T11:41:19-04:00,1,1168.00,2080.00,0.00,364.00,192.00,0.00,0.00,3.00,5.00,1.00,0.00,4.00,5.00,0.00,0.00,3
2026-03-25T11:41:20-04:00,2,1140.00,1820.00,0.00,362.00,202.00,0.00,0.00,4.00,7.00,2.00,0.00,6.00,4.00,0.00,0.00,3
2026-03-25T11:41:21-04:00,3,1174.00,2080.00,0.00,428.00,195.00,0.00,0.00,3.00,6.00,1.00,0.00,4.00,5.00,0.00,0.00,3
2026-03-25T11:41:22-04:00,4,1137.00,1910.00,0.00,392.00,200.00,0.00,0.00,4.00,7.00,1.00,0.00,6.00,6.00,0.00,0.00,3
2026-03-25T11:41:23-04:00,5,1131.00,2020.00,0.00,393.00,181.00,0.00,0.00,4.00,7.00,1.00,0.00,9.00,11.00,0.00,0.00,3
2026-03-25T11:41:24-04:00,6,1161.00,2260.00,0.00,418.00,205.00,0.00,0.00,3.00,6.00,1.00,0.00,3.00,5.00,0.00,0.00,3
2026-03-25T11:41:25-04:00,7,1167.00,1920.00,0.00,409.00,198.00,0.00,0.00,4.00,8.00,1.00,0.00,7.00,5.00,0.00,0.00,3
2026-03-25T11:41:26-04:00,8,1143.00,1850.00,0.00,397.00,198.00,0.00,0.00,4.00,7.00,2.00,0.00,7.00,4.00,0.00,0.00,3
2026-03-25T11:41:27-04:00,9,1223.00,2260.00,0.00,431.00,210.00,0.00,0.00,3.00,4.00,1.00,0.00,9.00,3.00,0.00,0.00,3
2026-03-25T11:41:28-04:00,10,1256.00,2080.00,0.00,378.00,236.00,0.00,0.00,3.00,4.00,1.00,0.00,4.00,3.00,0.00,0.00,3
2026-03-25T11:41:34-04:00,1,1183.00,1760.00,0.00,435.00,184.00,0.00,0.00,3.00,5.00,1.00,0.00,5.00,3.00,0.00,0.00,4
2026-03-25T11:41:35-04:00,2,1222.00,1800.00,0.00,425.00,239.00,0.00,0.00,4.00,4.00,1.00,0.00,4.00,4.00,0.00,0.00,4
2026-03-25T11:41:36-04:00,3,1216.00,2120.00,0.00,430.00,203.00,0.00,0.00,3.00,4.00,1.00,0.00,5.00,5.00,0.00,0.00,4
2026-03-25T11:41:37-04:00,4,1193.00,2010.00,0.00,392.00,205.00,0.00,0.00,3.00,7.00,1.00,0.00,9.00,3.00,0.00,0.00,4
2026-03-25T11:41:38-04:00,5,1204.00,1970.00,0.00,415.00,186.00,0.00,0.00,3.00,4.00,2.00,0.00,7.00,10.00,0.00,0.00,4
2026-03-25T11:41:39-04:00,6,1222.00,2120.00,0.00,393.00,194.00,0.00,0.00,3.00,4.00,1.00,0.00,4.00,18.00,0.00,0.00,4
2026-03-25T11:41:40-04:00,7,1130.00,1910.00,0.00,398.00,180.00,0.00,0.00,4.00,8.00,1.00,0.00,8.00,8.00,0.00,0.00,4
2026-03-25T11:41:41-04:00,8,1232.00,2160.00,0.00,436.00,201.00,0.00,0.00,3.00,5.00,0.00,0.00,4.00,3.00,0.00,0.00,4
2026-03-25T11:41:42-04:00,9,1269.00,2230.00,0.00,397.00,211.00,0.00,0.00,3.00,4.00,1.00,0.00,5.00,4.00,0.00,0.00,4
2026-03-25T11:41:49-04:00,1,1192.00,2020.00,0.00,404.00,182.00,0.00,0.00,3.00,4.00,1.00,0.00,5.00,4.00,0.00,0.00,5
2026-03-25T11:41:50-04:00,2,1188.00,1940.00,0.00,411.00,203.00,0.00,0.00,4.00,7.00,1.00,0.00,5.00,6.00,0.00,0.00,5
2026-03-25T11:41:51-04:00,3,1220.00,1980.00,0.00,419.00,219.00,0.00,0.00,4.00,6.00,1.00,0.00,5.00,4.00,0.00,0.00,5
2026-03-25T11:41:52-04:00,4,1212.00,1690.00,0.00,468.00,199.00,0.00,0.00,4.00,5.00,1.00,0.00,5.00,4.00,0.00,0.00,5
2026-03-25T11:41:53-04:00,5,1272.00,1950.00,0.00,397.00,209.00,0.00,0.00,3.00,4.00,1.00,0.00,3.00,10.00,0.00,0.00,5
2026-03-25T11:41:54-04:00,6,1218.00,2010.00,0.00,420.00,214.00,0.00,0.00,3.00,3.00,0.00,0.00,3.00,2.00,0.00,0.00,5
2026-03-25T11:41:55-04:00,7,1171.00,2170.00,0.00,389.00,193.00,0.00,0.00,4.00,7.00,1.00,0.00,7.00,9.00,0.00,0.00,5
2026-03-25T11:41:56-04:00,8,1220.00,2040.00,0.00,446.00,230.00,0.00,0.00,3.00,4.00,1.00,0.00,4.00,5.00,0.00,0.00,5
2026-03-25T11:41:57-04:00,9,1244.00,2030.00,0.00,420.00,188.00,0.00,0.00,4.00,7.00,1.00,0.00,6.00,4.00,0.00,0.00,5
```
</details>

Once the csv is exported, you can script your own method to plot its data. We have provided a sample gnuplot script ([gnuplot_csv.gnu](./resources/gnuplot_csv.gnu)) so you can see how this works. All you need to do is point the script to where the csv is located:

```bash
./gnuplot_csv.gnu /tmp/plgm_metrics_export.csv
```

<img src="./resources/gnuplot.png" alt="csv_export" width="50%">

## Post-Run JSON Summary Report
If you forget to enable the real-time CSV export, or if you just want a clean summary of your final results, PLGM provides a Download Summary Report button in the Web UI that appears the moment a workload finishes.

This generates a downloadable JSON summary report that captures both the final performance metrics (total ops, average latencies, and throughput per operation type), post-run insights analysis, and the exact configuration parameters used to achieve those results. Passwords are automatically redacted from this file for safe sharing.

Example Summary Snippet:

```json
{
    "generated_at": "2026-03-12T20:32:00.409Z",
    "duration_seconds": "10.51",
    "total_operations": 2159,
    "average_throughput_ops_sec": "205.50",
    "average_throughput_per_op": {
        "find": "59.56",
        "insert": "109.42",
        "update": "23.69",
        "delete": "12.75"
    },
    "operations": { ... },
    "average_latencies_ms": { ... },
    "insights": { ... },
    "configuration": {
        "concurrency": "4",
        "find_batch_size": "10",
        "password": "********"
    }
}
```

### Workload Configuration & Loading

You can supply your own collections and queries using the `PLGM_COLLECTIONS_PATH` and `PLGM_QUERIES_PATH` environment variables (or the corresponding config file fields). 

plgm supports two loading modes:

#### 1. Single File Mode
If you point to a specific file, plgm will load **only** that file, regardless of its name and will ignore the default workload setting.

```bash
# Loads only my_custom_workload.json
export PLGM_COLLECTIONS_PATH="./resources/collections/my_custom_workload.json"
```

#### 2. Directory Mode (Multi-file)
If you point to a folder, plgm will scan and merge **all** `.json` files found in that folder. This allows you to split complex schemas across multiple files. The default workload will be ignored.

```bash
# Loads all .json files in the /custom folder
export PLGM_COLLECTIONS_PATH="./resources/custom_collections/"
```

#### Single-File Multi-Definition Format
When using a single file, plgm supports both legacy array format and wrapped multi-definition format:

- Collections:
  - `[{...}]`
  - `{"collections":[...]}`
- Queries:
  - `[{...}]`
  - `{"queries":[...]}`

For multi-collection workloads, each query must identify its target collection (and database when needed). plgm validates query-to-collection mapping before execution and fails early on unknown or ambiguous references.
Using the same collection name in different databases is supported (for example `db1.orders` and `db2.orders`).

Example files:

- [`resources/collections/multi_collections.json`](./resources/collections/multi_collections.json)
- [`resources/queries/multi_queries.json`](./resources/queries/multi_queries.json)

#### Default Workload Filtering
When using **Directory Mode**, the behavior depends on the `PLGM_DEFAULT_WORKLOAD` setting:

* **`true` (Default):** Loads **only** `default.json` (if present). It ignores all other files in the folder.
* **`false` (Custom):** Loads all JSON files **except** `default.json`. 
  * *Use Case:* Set this to `false` to run your custom workload files while keeping `default.json` in the folder for reference (it will be ignored).

---

## Kubernetes & Docker
Prefer running in a container? We have a dedicated guide for building Docker images and running performance jobs directly inside Kubernetes (recommended for accurate network latency testing).

[View the Docker & Kubernetes Guide](k8s_and_docker.md)

---

## Configuration

plgm is configured primarily through its [config.yaml](./config.yaml) file. This makes it easier to save and version-control your test scenarios.

### Environment Variable Overrides
You can override any setting in `config.yaml` using environment variables. This is useful for CI/CD pipelines, Kubernetes deployments, or quick runtime adjustments without editing the file. These are all the available ENV vars you can configure and each corresponding setting in the [config.yaml](./config.yaml) file:

| Config Setting | Environment Variable | Description | Example |
| :--- | :--- | :--- | :--- |
| **Connection** | | | |
| `uri` | `PLGM_URI` | Target MongoDB connection URI | `mongodb://user:pass@host:27017` |
| `direct_connection` | `PLGM_DIRECT_CONNECTION` | Force direct connection (bypass topology discovery) | `true` |
| `replicaset_name` | `PLGM_REPLICASET_NAME` | Replica Set name (required for sharded clusters/RS) | `rs0` |
| `read_preference` | `PLGM_READ_PREFERENCE` | By default, an application directs its read operations to the primary member in a replica set. You can specify a read preference to send read operations to secondaries. | `nearest` |
| `username` | `PLGM_USERNAME` |  Database User | `admin` |
| ***can not be set via config*** | `PLGM_PASSWORD` | Database Password (if not set, plgm will prompt) | `password123` |
| **Web UI** | | | |
| `enabled` | `PLGM_WEBUI_ENABLED` | Force launch the Web UI | `true` |
| `port` | `PLGM_WEBUI_PORT` | Port for the Web UI | `9999` |
| `tls_enabled` | `PLGM_WEBUI_TLS` | Serve the Web UI over HTTPS (default HTTP on loopback, no cert warnings) | `false` |
| `tls_cert_file` | `PLGM_WEBUI_TLS_CERT` | TLS certificate file for the Web UI (for warning-free HTTPS) | `/etc/ssl/plgm.crt` |
| `tls_key_file` | `PLGM_WEBUI_TLS_KEY` | TLS private key file for the Web UI | `/etc/ssl/plgm.key` |
| **Metrics Export** | | | |
| `csv_export_enabled` ||Continuously stream workload throughput metrics to a CSV file| `false` |
| `csv_export_append` ||If true, appends to the file. If false, overwrites it.| `false` |
| `csv_export_path` ||Path and metrics file name| `/tmp/plgm_metrics_export.csv` |
| **Post-Run Insights** | | | |
| `insights_enabled` | `PLGM_INSIGHTS_ENABLED` | Enable post-run slow-query/index analysis | `true` |
| `insights_sampling_rate` | `PLGM_INSIGHTS_SAMPLING_RATE` | Sample rate for captured operation events (`0.01`-`1.0`) | `0.10` |
| `insights_slow_threshold_ms` | `PLGM_INSIGHTS_SLOW_THRESHOLD_MS` | Latency threshold used to classify operations as slow | `200` |
| `insights_max_events` | `PLGM_INSIGHTS_MAX_EVENTS` | Max retained sampled events in memory | `5000` |
| `insights_max_groups` | `PLGM_INSIGHTS_MAX_GROUPS` | Max aggregated slow-shape groups | `300` |
| `insights_explain_enabled` | `PLGM_INSIGHTS_EXPLAIN_ENABLED` | Enable optional post-run explain sampling | `false` |
| `insights_explain_top_n` | `PLGM_INSIGHTS_EXPLAIN_TOP_N` | Number of top slow shapes to attempt explain on | `5` |
| `insights_explain_max_time_ms` | `PLGM_INSIGHTS_EXPLAIN_MAX_TIME_MS` | Max server time per explain command | `1000` |
| `insights_explain_verbosity` | `PLGM_INSIGHTS_EXPLAIN_VERBOSITY` | Explain verbosity (`executionStats` recommended, `queryPlanner` lighter) | `executionStats` |
| `insights_explain_severity_mode` | `PLGM_INSIGHTS_EXPLAIN_SEVERITY_MODE` | Explain eligibility mode (`high_only`, `medium_only`, `critical_only`, `high_and_low`) | `high_only` |
| `insights_explain_workers` | `PLGM_INSIGHTS_EXPLAIN_WORKERS` | Post-run explain worker count | `1` |
| `insights_explain_retries` | `PLGM_INSIGHTS_EXPLAIN_RETRIES` | Retry count for explain timeout/failure | `1` |
| `insights_explain_backoff_ms` | `PLGM_INSIGHTS_EXPLAIN_BACKOFF_MS` | Backoff between explain retries (ms) | `150` |
| **Sharding Strategy** | | | |
| `sharding_mode` | `PLGM_SHARDING_MODE` | Per-collection sharding behavior: `auto`, `force_on`, `force_off` | `auto` |
| `sharding_skip_generic_without_config` | `PLGM_SHARDING_SKIP_GENERIC_WITHOUT_CONFIG` | Skip generic sharding setup for collections without `shardConfig` (recommended for mixed workloads) | `true` |
| **Workload Control** | | | |
| `concurrency` | `PLGM_CONCURRENCY` | Number of active worker goroutines | `50` |
| `duration` | `PLGM_DURATION` | Test duration (Go duration string) | `5m`, `60s` |
| `iterations` | `PLGM_ITERATIONS` | Number of times to repeat the workload | `1` |
| `interval_delay` | `PLGM_INTERVAL_DELAY` | Time to pause between iterations (e.g. 5s, 1m) | `0s` |
| `default_workload` | `PLGM_DEFAULT_WORKLOAD` | Use built-in "Flights" workload (`true`/`false`) | `false` |
| `collections_path` | `PLGM_COLLECTIONS_PATH` | Path to custom collection JSON files (supports directories for multi-collection load) | `./schemas` |
| `queries_path` | `PLGM_QUERIES_PATH` | Path to custom query JSON files or directory. | `./queries` |
| `documents_count` | `PLGM_DOCUMENTS_COUNT` | Number of documents to seed initially | `10000` |
| `drop_collections` | `PLGM_DROP_COLLECTIONS` | Drop collections before starting (`true`/`false`) | `true` |
| `skip_seed` | `PLGM_SKIP_SEED` | Do not seed initial data on start (`true`/`false`) | `true` |
| `seed_batch_size` | `PLGM_SEED_BATCH_SIZE` | Number of documents to insert per batch during SEED phase | `1000` |
| `debug_mode` | `PLGM_DEBUG_MODE` | Enable verbose debug logging (`true`/`false`) | `false` |
| `pprof_enabled` | `PLGM_PPROF_ENABLED` | Enable pprof server on localhost:6060 (`true`/`false`) | `false` |
| `use_transactions` | `PLGM_USE_TRANSACTIONS` | Enable Transactional Workloads (`true`/`false`) | `false` |
| `max_transaction_ops` | `PLGM_MAX_TRANSACTION_OPS` | Maximum number of operations to group into a single transaction block | `5` |
| **Operation Ratios** | | (Must sum to ~100) | |
| `find_percent` | `PLGM_FIND_PERCENT` | Percentage of Find operations | `60` |
| `insert_percent` | `PLGM_INSERT_PERCENT` | Percentage of Insert operations (this is not related to the initial seed inserts) | `5` |
| `bulk_insert_percent ` | `PLGM_BULK_INSERT_PERCENT` | Percentage of Bulk Insert operations (this is not related to the initial seed inserts) | `5` |
| `update_percent` | `PLGM_UPDATE_PERCENT` | Percentage of Update operations | `20` |
| `delete_percent` | `PLGM_DELETE_PERCENT` | Percentage of Delete operations | `10` |
| `aggregate_percent` | `PLGM_AGGREGATE_PERCENT` | Percentage of Aggregate operations | `0` |
| `transaction_percent` | `PLGM_TRANSACTION_PERCENT` | Percentage of Transactional operations | `0` |
| **Performance Optimization** | | | |
| `find_batch_size` | `PLGM_FIND_BATCH_SIZE` | Documents returned per cursor batch | `100` |
| `insert_batch_size` | `PLGM_INSERT_BATCH_SIZE` | Number of documents per insert batch | `100` |
| `find_limit` | `PLGM_FIND_LIMIT` | Hard limit on documents per Find query | `10` |
| `insert_cache_size` | `PLGM_INSERT_CACHE_SIZE` | Size of the document generation buffer | `1000` |
| `op_timeout_ms` | `PLGM_OP_TIMEOUT_MS` | Soft timeout for individual DB operations (ms) | `500` |
| `retry_attempts` | `PLGM_RETRY_ATTEMPTS` | Number of retries for transient errors | `3` |
| `retry_backoff_ms` | `PLGM_RETRY_BACKOFF_MS` | Wait time between retries (ms) | `10` |
| `status_refresh_rate_sec` | `PLGM_STATUS_REFRESH_RATE_SEC` | How often to print stats to console (sec) | `5` |


**Example:**
```bash
PLGM_CONCURRENCY=50 PLGM_DURATION=5m ./bin/plgm
```

---

## Functionality

When executed, plgm performs the following steps:

1.  **Initialization:** Connects to the database and loads collection/query definitions.
2.  **Setup:**
    * Creates databases and collections defined in your JSON files.
    * Creates indexes.
    * (Optional) Seeds initial data with the number of documents defined by `documents_count` in the config.
3.  **Workload Execution:**
    * Spawns the configured number of **Active Workers**.
    * Continuously generates and executes queries (Find, Insert, Update, Delete, Aggregate, Upsert) based on your configured ratios.
    * Generates realistic BSON data for Inserts and Updates (supports recursion and complex schemas).
    * Workers pick a random collection from the provided list for every operation, then route query execution using queries bound to that same database+collection.
4.  **Reporting:**
    * Outputs a real-time status report every N seconds (configurable).
    * Prints a detailed summary table at the end of the run.
    * Export output to csv (off by default)

### Sample Output

![plgm](./images/plgm.gif)

### Interpreting the Output

To show how the Ops/Sec metrics are represented and what they signify, here is a sample of the real-time monitor output and a final summary. This data is modeled after the flights workload used when default_workload is true.

#### Real-Time Monitor Sample
While running a workload, plgm prints a row every second (based on `status_refresh_rate_sec`).

```bash
> Starting Workload...

 TIME    | TOTAL OPS  | SELECT   | INSERT   | UPDATE   | DELETE   | AGG    | TRANS
 -------------------------------------------------------------------------------
 00:01   |      8,300 |    5,004 |      798 |    1,650 |      848 |      0 |      0
 00:02   |      8,048 |    4,736 |      773 |    1,694 |      845 |      0 |      0
 00:03   |      8,168 |    4,728 |      824 |    1,737 |      879 |      0 |      0
```
What this represents:

* TIME: The elapsed time since the workload started (MM:SS).
* TOTAL OPS: The combined number of all operations executed across all workers in that specific 1-second interval.
* SELECT/INSERT/UPDATE/DELETE/AGG: The raw count of each specific operation type completed in that second.
* TRANS: The number of successful transaction blocks completed in that second (reusing the CRUD operations above internally).

#### Final Summary and Latency Sample
At the end of the run, plgm calculates the overall averages and the latency distribution.

```bash
> Workload Finished.

  SUMMARY
  --------------------------------------------------
  Runtime:    10.00s
  Total Ops:  81,746
  Avg Rate:   8,174 ops/sec

  LATENCY DISTRIBUTION (ms)
  --------------------------------------------------
  TYPE             AVG          MIN          MAX          P95          P99
  ----             ---          ---          ---          ---          ---
  SELECT       1.24 ms      0.45 ms     15.20 ms      4.00 ms      9.00 ms
  INSERT      12.11 ms      4.10 ms     85.00 ms     66.00 ms     73.00 ms
  UPDATE       9.71 ms      3.20 ms     78.40 ms     65.00 ms     71.00 ms
  DELETE       9.60 ms      3.05 ms     76.20 ms     65.00 ms     72.00 ms
  TRANS       25.40 ms     12.00 ms    145.00 ms     95.00 ms    112.00 ms
```

What this represents:

* Avg Rate (Ops/Sec): The total throughput of the database cluster. It is calculated by dividing Total Ops by the total Runtime.
* AVG Latency: The average time (in milliseconds) it took the MongoDB driver to receive a response for that operation.
* P95/P99 (Percentiles): These are the most critical metrics for performance tuning. P99 represents the "worst-case" scenario for 99% of your users. For example, if P99 SELECT is 9.00ms, it means 99% of your flight searches completed in under 9ms, while 1% took longer.
* TRANS Latency: This will typically be higher than individual operations because a single transaction block contains 1 to X grouped operations, where X is defined in the config file via `max_transaction_ops` or the env var `PLGM_MAX_TRANSACTION_OPS`.

---

## Custom Workloads

To run your own workload against your own schema:

1.  **Define Collection Schema:**
    Create a JSON file (e.g., `my_collection.json`) defining your schema.

    ```json
    [
      {
        "database": "ecommerce",
        "collection": "orders",
        "fields": {
          "_id": { "type": "objectid" },
          "customer_name": { "type": "string", "provider": "first_name" },
          "total": { "type": "double" },
          "created_at": { "type": "date" }
        }
      }
    ]
    ```

2.  **Define Query Patterns:**
    Create a JSON file (e.g., `my_queries.json`) defining the operations to run.

    ```json
    [
      {
        "database": "ecommerce",
        "collection": "orders",
        "operation": "find",
        "filter": { "customer_name": "<string>" },
        "limit": 10
      },
      {
        "database": "ecommerce",
        "collection": "orders",
        "operation": "updateOne",
        "filter": { "order_uuid": "<string>" },
        "update": { "$set": { "status": "processed" } },
        "upsert": true
      }
    ]
    ```

3.  **Run:**
    ```bash
    export PLGM_COLLECTIONS_PATH=./my_collection.json
    export PLGM_QUERIES_PATH=./my_queries.json
    ./bin/plgm
    ```

### Supported Data Types
* **Primitives:** `int`, `long`, `double`, `decimal128`, `bool`, `string`.
* **Time:** `date`, `timestamp`.
* **Binary/Logic:** `binary`, `uuid`, `objectid`, `regex`, `javascript`.
* **Complex:** `object`, `array`.
* **Providers:** Supports ANY gofakeit provider via reflection. Example: `beer_name`, `car_maker`, `bitcoin_address`, `credit_card`, `city`, `ssn`, etc..
* **Domain providers (airline workload):** `flight_code` (e.g. `DL482`), `gate` (e.g. `B7`), `airport_code` / `airport` (IATA), `airline`, `aircraft` / `plane_type`, `seats_available`, `duration_minutes`, `equip` (aircraft object), `passengers` (passenger list).

### Field Constraints (schema-aware bounded values)
Each field may declare optional constraints so generated data stays realistic and bounded:

| Key         | Applies to        | Description |
|-------------|-------------------|-------------|
| `min`/`max` | `int`, `double`   | Inclusive numeric range. Unbounded ints default to a large range, so set these for real-world fields. |
| `enum`      | any type          | Picks a random value from the provided list (strings or numbers). Takes precedence over `provider`/`type`. |
| `template`  | `string`          | Pattern expander: `#` → digit, `?` → uppercase letter, `^` → lowercase letter (e.g. `"??###"` → `DL482`). |
| `minLength`/`maxLength` | `string`, provider args | Length hints passed to length-aware providers. |
| `arraySize` | `array`           | Fixed element count (otherwise randomized). |
| `items`     | `array`           | Field definition for array elements. |
| `fields`    | `object`          | Nested field definitions. |

```json
{
  "duration_minutes": { "type": "int", "min": 30, "max": 900 },
  "status":           { "type": "string", "enum": ["scheduled", "boarding", "landed", "cancelled"] },
  "flight_code":      { "type": "string", "template": "??###" }
}
```

### Query Accuracy (targeting existing records)
Find/update/delete query filters use placeholders such as `<int>` and `<string>`. At runtime plgm
maintains an in-memory **record pool** of inserted (and bootstrap-sampled) documents and, for
`existing_record_hit_rate`% of operations, fills those placeholders with real values from the pool.
This ensures operations actually read/update/delete documents that exist instead of missing on random
values, so benchmark results reflect real work rather than empty matches. Tune via
`existing_record_hit_rate`, `record_pool_max_size`, and `record_pool_bootstrap_sample`.

### Workload Accuracy Metrics
The final summary includes a **WORKLOAD ACCURACY** section reporting existing-record targeting,
find match/miss rate, documents returned, update matched/modified counts, and delete hit rate — so a
fast-but-misleading run (mostly missing records) is easy to spot.

### Deterministic Generation
Set `random_seed` (or `PLGM_RANDOM_SEED`) to a non-zero value to reproduce datasets. The single-threaded
seed phase is fully reproducible; concurrent workload streams are seeded per-worker from the base
(best-effort). Leave `0` for time-based, non-deterministic generation.

---

## Performance Optimization

plgm is designed to utilize maximum system resources by default, but it can be fine-tuned to fit specific hardware constraints or testing scenarios.

### 1. CPU Utilization (`GOMAXPROCS`)

By default, plgm automatically detects and schedules work across **all available logical CPUs**. You generally do not need to configure this.

However, if you are running in a constrained environment (e.g., a shared CI runner or a container with strict CPU limits) or if you want to throttle the generator's CPU usage, you can override this via the standard Go environment variable:

```bash
# Limit plgm to use only 2 CPU cores
export GOMAXPROCS=2
./plgm
```

### 2. Configuration Optimization (`config.yaml`)

You can fine-tune plgm internal behavior by adjusting the parameters in `config.yaml`.

#### Workload Type
By default, the tool comes preconfigured with the following workload distribution:

| Operation | Percentage |
| :--- | :--- | 
| Find  | 50% | 
| Update  | 20% | 
| Delete  | 10% | 
| Insert  | 5% | 
| Bulk Inserts | 5% |
| Aggregate | 5% | 
| Transaction | 5% | 

You can modify any of the values above to run different types of workloads.

Please note:

* If `use_transactions: false`, the transaction_percent value is ignored.
* If there are no aggregation queries defined in queries.json, the aggregate_percent value is also ignored. 
* Aggregate operations will only generate activity if at least one query with "operation": "aggregate" is defined in your active JSON query files.
* The maximum number of operations within a transaction is defined in the config file via `max_transaction_ops` or the env var `PLGM_MAX_TRANSACTION_OPS`. The number of operations per transaction will be randomized, with the max number being set as explained above. 
* Multi-Collection Load: If multiple collections are defined in your collections_path, each worker will randomly select a collection for every operation. This includes operations within a transaction, allowing for cross-collection atomic updates.


#### Concurrency & Workers
* **`concurrency`**: Controls the number of "Active Workers" continuously executing operations against the database.
    * *Tip:* Increase this to generate higher load. If set too high on a weak client, you may see increased client-side latency.
    * *Default:* `4`

#### Connection Pooling
These settings control the MongoDB driver's connection pool. Proper sizing is critical to prevent the application from waiting for available connections.

* **`max_pool_size`**: The maximum number of connections allowed in the pool.
    * *Tip:* A good rule of thumb is to set this slightly higher than your `concurrency` setting so that every worker is guaranteed a connection without blocking.
    * *Default:* `1000`
* **`min_pool_size`**: The minimum number of connections to keep open.
    * *Tip:* Setting this higher helps avoid the "cold start" penalty of establishing new connections during the initial ramp-up.
    * *Default:* `20`
* **`max_idle_time`**: How long a connection can remain unused before being closed (in minutes).
    * *Tip:* Keep this high (e.g., `30`) to avoid "reconnect churn" during brief pauses in workload.

#### Operation Optimization
These settings affect the efficiency of individual database operations and memory usage.

* **`find_batch_size`**: The number of documents returned per batch in a cursor.
    * *Tip:* Higher values reduce network round-trips but increase memory usage per worker.
    * *Default:* `10`
* **`insert_batch_size`**: The number of documents to be inserted by bulk inserts.
    * *Default:* `10`   
* **`seed_batch_size`**: The number of documents grouped into a single InsertMany call during the initial data seeding phase.
    * *Tip:* Keeps memory usage stable when seeding millions of documents. A value of 1000 is recommended for performance.
    * *Default:* `1000`
* **`find_limit`**: The hard limit on documents returned for `find` operations.
    * *Default:* `5`
* **`insert_cache_size`**: The buffer size for the document generator channel.
    * *Tip:* This decouples document generation from database insertion. A larger buffer ensures workers rarely wait for data generation logic.
    * *Default:* `1000`
* **`upserts`**: Any updateOne or updateMany operation in your query JSON files can include "upsert": true. This will cause MongoDB to create the document if no match is found for the filter.  


#### Timeouts & Reliability
Control how plgm reacts to network lag or database pressure.

* **`op_timeout_ms`**: A hard timeout for individual database operations.
    * *Tip:* Lowering this allows plgm to fail fast and retry rather than hanging on stalled requests.
    * *Default:* `500` (0.5 seconds)
* **`retry_attempts`** & **`retry_backoff_ms`**: Logic for handling transient failures.
    * *Tip:* For stress testing, you might want to set `retry_attempts: 0` to see raw failure rates immediately.
    * *Default:* `2` attempts with `5ms` backoff.


#### Workload Iterations & Scheduling
Control how plgm repeats a given workload and schedules the time between runs.

* **`iterations`**: The number of times to run the defined workload duration back-to-back.
    * *Tip:* This is perfect for warming up the database cache during the first iteration and capturing true performance metrics on subsequent iterations, without having to manually restart the tool.
    * *Default:* `1`
* **`interval_delay`**: The amount of time to pause the workload between each iteration (e.g., `10s`, `1m`).
    * *Tip:* Adding a delay allows database background processes (like log flushing, checkpointing, or compaction) to catch up, simulating real-world batch-processing patterns. 
    * *Default:* `0s`


### 3. Custom Connection Parameters (`custom_params`)

In the `config.yaml`, the `custom_params` section allows you to pass arbitrary options directly to the MongoDB driver's connection string. These parameters are appended as URI query options and are critical for tuning network throughput, security, and routing. All standard MongoDB connection parameters are supported.

```yaml
custom_params:
  compressors: "zlib,snappy"
```

| Parameter | Example Value | Impact on Performance |
| :--- | :--- | :--- |
| **`compressors`** | `"snappy,zlib"` | **High Impact.** Enables network compression. <br>• **`snappy`**: Low CPU overhead, moderate compression. Good for high-throughput, low-latency. <br>• **`zlib`**: Higher CPU overhead, high compression. Good for limited bandwidth. <br>• **Empty**: No compression (saves CPU, uses max bandwidth). |
| **`readPreference`**| `"secondary"` | **Medium Impact.** (Optional) Can be added to offload read operations to replica set secondaries, keeping the primary free for writes. |

***Note: From the perspective of the MongoDB Go driver, tls and ssl are 100% synonymous. They do the exact same thing. Historically, MongoDB used the term ssl, but has transitioned to tls to reflect modern security standards. The driver supports both for backwards compatibility.***

#### Connecting to TLS/SSL-Enabled Clusters

If your target MongoDB cluster enforces TLS/SSL, configure the load generator with the TLS client fields under `connection_params`.

Example config.yaml for TLS:

```yaml
connection_params:
  tls_ca_file: "/etc/ssl/ca.crt"
  tlsCertificateKeyFile: "/etc/ssl/tls.pem" # Combined certificate/key PEM file
```

Further configuration and examples specific to [Kubernetes environments setup with TLS can be found here](k8s_and_docker.md#3-running-plgm-with-tls-on-kubernetes)

PLGM automatically sets `tls=true` in the generated MongoDB URI when both TLS client files are present, and `tls=false` when they are not. Do not duplicate `tls`, `tlsCAFile`, or `tlsCertificateKeyFile` under `custom_params`.


# Disclaimer

This application is not supported by Percona. It has been provided as a community contribution and is not covered under any Percona services agreement.
