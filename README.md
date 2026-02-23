# plgm: Percona Load Generator for MongoDB Clusters

**plgm** is a high-performance tool written in Go, designed to effortlessly generate data and simulate heavy workloads for both sharded and non-sharded MongoDB clusters.

It simulates real-world usage patterns by generating random data using robust BSON data types and executing standard CRUD operations (Find, Insert, Update, Delete) based on configurable ratios.

This tool is a complete refactor of the previous Python version, offering:
* **Single Binary:** No complex dependencies or Python environment setup.
* **High Concurrency:** Utilizes Go goroutines ("Active Workers") to generate massive load with minimal client-side resource usage.
* **Configuration as Code:** Fully configurable via a simple `config.yaml` file or Environment Variables.
* **Extensive Data Support:** Supports all standard MongoDB BSON data types (ObjectId, Decimal128, Date, Binary, etc.) and realistic data generation via `gofakeit` (supporting complex nested objects and arrays).
* **True Parallelism:** Unlike the previous Python version, this tool automatically detects and utilizes all available logical CPUs (`GOMAXPROCS`) by default to maximize hardware efficiency.

---

## Quick Start

### 1. Installation

**Option 1: Download Release**

Navigate to the [Releases] page and download the .tar.gz file matching your operating system.

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
git clone https://github.com/Percona-Lab/percona-load-generator-mongodb.git
cd percona-load-generator-mongodb
go mod tidy

# Build a binary for your CURRENT machine only (no .tar.gz)
make build-local

# Run it
./bin/plgm --help
```

### 2. Configuration & Resources

To run the application, you need a configuration file. Depending on whether you want to run the built-in test or your own custom workload, you may also need to create resource folders.

**Step A: Get the Config**

Download the [`config.yaml`](./config.yaml) and adjust the `uri` to point to your MongoDB instance.

**Step B: Choose Your Workload**

* **Mode 1: Default Workload (Easiest)**
    
    By default (`default_workload: true` in `config.yaml`), the application uses the embedded collection and query definitions. You do **not** need to create any extra folders or files.

* **Mode 2: Custom Workload**
    
    To run your own stress tests, you must set `default_workload: false` in `config.yaml` and provide the necessary files:

    1.  **Create Directories**: Create folders for your definitions (e.g., `resources/collections` and `resources/queries`).
    2.  **Add Files**: Place your JSON schema and query definitions inside these folders.
    3.  **Update Config**: Ensure `collections_path` and `queries_path` in your `config.yaml` point to these new directories.

    > **Important:** If you are running in Custom Mode, the application expects these folders to exist. If the folders are missing, `plgm` will revert to the embedded defaults to prevent a crash, but your custom test **will not run** until the files are in place.

### 3. Run

Once configured, run the application:

```bash
# The extracted binary will have the OS suffix
./plgm-linux-amd64
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

#### Usage

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

Environment Variables (Overrides):
 [Connection]
  PLGM_URI                            Connection URI
  PLGM_USERNAME                       Database User
  PLGM_PASSWORD                       Database Password (Recommended: Use Prompt)
  PLGM_DIRECT_CONNECTION              Force direct connection (true/false)
  PLGM_REPLICASET_NAME                Replica Set name
  PLGM_READ_PREFERENCE                nearest

 [Workload Core]
  PLGM_DEFAULT_WORKLOAD               Use built-in workload (true/false)
  PLGM_COLLECTIONS_PATH               Path to collection JSON
  PLGM_QUERIES_PATH                   Path to query JSON
  PLGM_DURATION                       Test duration (e.g. 60s, 5m)
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

### 2. Default Workload
plgm comes with a built-in default workload useful for immediate testing and get you started right away.
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

### 3. Raw Injector Mode (High Performance Hardware Test) Workload

The **Raw Injector** is a specialized, ultra-high-performance engine built directly into `PLGM`. Instead of using the standard MongoDB driver structs (which consume CPU and memory for BSON marshaling), the Raw Injector pre-compiles raw BSON byte arrays and performs zero-allocation bitwise mutations in a tight loop.

This mode is designed exclusively to **stress-test network throughput, disk I/O, and extreme CPU limits** of the MongoDB cluster. It can saturate high-end infrastructure significantly faster than standard workloads. This functionality was developed to mimic other benchmarking tools, enabling an apples-to-apples comparison between PLGM and alternative solutions.

Unlike the other workloads PLGM supports, this mode is not configurable. It was intentionally built solely for stress testing purposes and is not intended to function as a workload simulator, but instead, a simple benchmark stress test.

***Important Note on Ops/Sec: In Raw Injector mode, the printed Ops/Sec refers to the number of documents processed, not the number of network commands. For example, if your batch size is 1,000, and plgm reports 50,000 Ops/Sec, it is executing 50 bulk network commands per second.***

#### Configuration

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

#### Modes of Operation (type)

 * insert: Floods the database with new documents. Automatically pre-splits chunks if sharding is enabled.
 * find / update / delete: Operates on the existing data seeded by an insert run.
 * upsert: Fires upsert commands, creating documents if they don't exist.
 * mixed: Runs a randomized distribution of reads, inserts, updates, and deletes simultaneously.


#### Running via CLI
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

### 4. Additional Workloads

You will find additional workloads that you can use as references to benchmark your environment in cases where you prefer not to provide your own collection definitions and queries. However, if your goal is to test your application accurately, we strongly recommend creating collection definitions and queries that match those used by your application.

The additional collection and query definitions can be found here:

* [collections](./resources/collections/)
* [queries](./resources/queries/)


### 5. Workload Configuration & Loading

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
| `username` | `PLGM_USERNAME` |	Database User | `admin` |
| ***can not be set via config*** | `PLGM_PASSWORD` |	Database Password (if not set, plgm will prompt) | `password123` |
| **Workload Control** | | | |
| `concurrency` | `PLGM_CONCURRENCY` | Number of active worker goroutines | `50` |
| `duration` | `PLGM_DURATION` | Test duration (Go duration string) | `5m`, `60s` |
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
| `find_percent` | `PLGM_FIND_PERCENT` | Percentage of Find operations | `50` |
| `insert_percent` | `PLGM_INSERT_PERCENT` | Percentage of Insert operations (this is not related to the initial seed inserts) | `10` |
| `bulk_insert_percent ` | `PLGM_BULK_INSERT_PERCENT` | Percentage of Bulk Insert operations (this is not related to the initial seed inserts) | `10` |
| `update_percent` | `PLGM_UPDATE_PERCENT` | Percentage of Update operations | `10` |
| `delete_percent` | `PLGM_DELETE_PERCENT` | Percentage of Delete operations | `10` |
| `aggregate_percent` | `PLGM_AGGREGATE_PERCENT` | Percentage of Aggregate operations | `5` |
| `transaction_percent` | `PLGM_TRANSACTION_PERCENT` | Percentage of Transactional operations | `5` |
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
    * Workers pick a random collection from the provided list for every operation.
4.  **Reporting:**
    * Outputs a real-time status report every N seconds (configurable).
    * Prints a detailed summary table at the end of the run.

### Sample Output

![plgm](./plgm.gif)

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

| Operation |	Percentage |
| :--- | :--- | 
| Find	| 50% | 
| Update	| 20% | 
| Delete	| 10% | 
| Insert	| 5% | 
| Bulk Inserts | 5% |
| Aggregate	| 5% | 
| Transaction	| 5% | 

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

### 3. Custom Connection Parameters (`custom_params`)

In the `config.yaml`, the `custom_params` section allows you to pass arbitrary options directly to the MongoDB driver's connection string. These parameters are appended as URI query options and are critical for tuning network throughput, security, and routing. All standard MongoDB connection parameters are supported.

```yaml
custom_params:
  compressors: "zlib,snappy"
  tls: false
```

| Parameter | Example Value | Impact on Performance |
| :--- | :--- | :--- |
| **`compressors`** | `"snappy,zlib"` | **High Impact.** Enables network compression. <br>• **`snappy`**: Low CPU overhead, moderate compression. Good for high-throughput, low-latency. <br>• **`zlib`**: Higher CPU overhead, high compression. Good for limited bandwidth. <br>• **Empty**: No compression (saves CPU, uses max bandwidth). |
| **`tls`** | `false` | **Low/Medium Impact.** Disabling TLS (`false`) saves the CPU overhead of TLS handshakes and encryption, useful for local testing or secured private networks. |
| **`readPreference`**| `"secondary"` | **Medium Impact.** (Optional) Can be added to offload read operations to replica set secondaries, keeping the primary free for writes. |

***Note: From the perspective of the MongoDB Go driver, tls and ssl are 100% synonymous. They do the exact same thing. Historically, MongoDB used the term ssl, but has transitioned to tls to reflect modern security standards. The driver supports both for backwards compatibility.***

#### Connecting to TLS/SSL-Enabled Clusters

If your target MongoDB cluster enforces TLS/SSL, you must configure the load generator to use secure connections and present a valid client certificate.

Because custom_params are directly injected into the MongoDB connection string, you can easily pass your TLS configuration directly through the config.yaml.

Example config.yaml for TLS:

```YAML
custom_params:
  compressors: "none"
  tls: true
  tlsInsecure: true # Bypasses strict Certificate Authority (CA) validation
  tlsCertificateKeyFile: "/etc/ssl/tls.pem" # Path to the combined certificate/key PEM file
```

Further configuration and examples specific to [Kubernetes environments setup with TLS can be found here](k8s_and_docker.md#3-running-plgm-with-tls-on-kubernetes)

***Note: Ensure you remove tls: false from your URI if you are using tls: true in custom_params, as the MongoDB Go driver will return a fatal error if conflicting security parameters are provided.***


