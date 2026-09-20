# Testing Guide

This repository has two test layers:

1. Unit tests (default, fast, no MongoDB required)
2. Integration tests (optional, require a real MongoDB instance)

## Quick Start (Make targets)

The Makefile wraps the common commands so they are easy to find and run:

```bash
make test             # all unit tests (no MongoDB required)
make test-race        # unit tests with the race detector
make vet              # go vet static analysis
make fmt              # verify gofmt formatting
make check            # full local gate: fmt + vet + unit tests (run before committing)
make test-integration # integration tests (needs MongoDB; see below)
```

`make check` is the recommended pre-commit gate and mirrors what CI should enforce.

## Unit Tests

Run all unit tests:

```bash
go test ./...
```

Unit tests are the default and are intentionally independent of local MongoDB availability.

### Feature test map

Tests for the load-modeling / analysis features live next to their packages:

| Feature | Package | Notes |
| --- | --- | --- |
| Load profiles (fixed/ramp/step/spike/sine) | `internal/loadprofile` | Compile + `TargetAt` + validation |
| Load-profile wiring (peak concurrency) | `internal/mongo` | `TestRunWorkload*LoadProfile*` |
| Think time / pacing | `internal/mongo` | `TestApplyPacing*`, `TestRunWorkloadPropagatesPacingToWorkers` |
| Access-pattern skew (uniform/zipfian/hotspot) | `internal/accesspattern` | Distribution + validation; `internal/mongo` record-pool integration |
| Latency heatmap over time | `internal/stats` | `heatmap_test.go`, `SnapshotAndReset` percentiles |
| Query-log schema inference | `internal/schemainfer` | mongod logs, profiler docs, raw commands, malformed input |
| Shareable run reports | `internal/report` | Self-contained HTML + inline SVG chart |
| Web API endpoints | `internal/webui` | `/api/infer-schema`, `/api/report` |
| Parallel initial seeding | `internal/mongo`, `internal/config`, `internal/webui` | `TestInsertRandomDocuments*`, `TestSeedConcurrencyConfig`, `TestHandleStartSeedConcurrency` |

Timing-sensitive tests (pacing) assert lower bounds / ranges rather than
exact durations to avoid flakiness.

## Integration Tests

Integration tests are behind the `integration` build tag and are skipped from default runs.

Current integration smoke test:

- `internal/mongo/TestInsertRandomDocumentsIntegrationParallel` (serial/parallel seed counts, final partial batch, target database routing)
- `internal/mongo/TestInsertRandomDocumentsIntegrationDuplicateKey` (real write error propagation and partially accepted writes)
- `internal/mongo/TestRunWorkloadIntegration_OneShotExecutesFindUpdateAndSkipsInsert`
- `internal/mongo/TestRunWorkloadIntegration_OneShotMultiDBMixedShardConfig`
- `internal/mongo/TestRunWorkloadIntegration_MongosMixedShardedAndUnshardedCollections` (mongos-only, gated)

### Option A: Start MongoDB with Docker Compose (recommended)

From repository root:

```bash
docker compose -f docker-compose.integration.yml up -d
```

Then run the integration smoke test:

```bash
go test -tags=integration ./internal/mongo -run TestRunWorkloadIntegration_OneShotExecutesFindUpdateAndSkipsInsert -v
```

Run all integration tests available for your endpoint:

```bash
go test -tags=integration ./internal/mongo -v
```

Stop MongoDB when done:

```bash
docker compose -f docker-compose.integration.yml down -v
```

### Option B: Use an existing MongoDB endpoint

Set the URI via environment variable:

```bash
PLGM_IT_MONGO_URI='mongodb://127.0.0.1:30777' \
go test -tags=integration ./internal/mongo -run TestRunWorkloadIntegration_OneShotExecutesFindUpdateAndSkipsInsert -v
```

If your MongoDB requires authentication, include credentials in `PLGM_IT_MONGO_URI`.

### Mongos Mixed Sharding Integration Test (CI/profiled env)

This test validates a real mongos path with one sharded and one unsharded collection in the same run:

- `TestRunWorkloadIntegration_MongosMixedShardedAndUnshardedCollections`

It is intentionally gated and will skip unless explicitly enabled:

```bash
PLGM_IT_MONGO_URI='mongodb://<mongos-host>:27017' \
PLGM_IT_ENABLE_MONGOS_SHARD_TEST=true \
go test -tags=integration ./internal/mongo \
  -run TestRunWorkloadIntegration_MongosMixedShardedAndUnshardedCollections \
  -v -count=1
```

Recommended CI/profiled environment requirements:

- URI must point to a real `mongos` router
- test user must have privileges for:
  - `listShards`
  - `enableSharding`
  - `shardCollection`
  - normal read/write on test databases
- ideally run in a dedicated test cluster/profile to avoid interference with production workloads

## Seed Throughput Benchmark

The optional benchmark exercises the actual generator and MongoDB driver with
1, 4, and 8 seed writers. Each iteration creates a fresh collection and verifies
its final document count. It fails if MongoDB is unavailable.

```bash
PLGM_IT_MONGO_URI='mongodb://127.0.0.1:30777' GOMAXPROCS=4 \
go test -tags=integration ./internal/mongo -run '^$' \
  -bench '^BenchmarkInsertRandomDocumentsIntegration$' -benchtime=1x -count=5
```

The two workloads are 100,000 generic documents (a random integer and a constant
256-byte payload, with only the `_id` index) and 10,000 built-in flight documents
(including their configured secondary indexes). Both use batches of 1,000,
`random_seed: 42`, a 16-connection pool, and explicit `w: 1`. The timer includes
document generation and acknowledged inserts, but excludes warm-up,
collection/index setup, and the final count query. `docs/s` is the measured
throughput; `max_inflight` is the peak simultaneous insert commands observed by
the driver's command monitor. It may be lower than the configured writer count.

Report the server version, topology, resource limits, and repeated results when
comparing runs. More writers need not improve throughput when serial generation
or server contention is the bottleneck.

For a controlled latency experiment, use a disposable MongoDB instance started
with `--setParameter enableTestCommands=true`, and enable this failpoint before
running the benchmark:

```javascript
db.getSiblingDB("admin").runCommand({
  configureFailPoint: "failCommand", mode: "alwaysOn",
  data: {failCommands: ["insert"], blockConnection: true, blockTimeMS: 50}
})
// Disable after the experiment:
db.getSiblingDB("admin").runCommand({configureFailPoint: "failCommand", mode: "off"})
```

Label those results as artificial server-side delay, separately from ordinary
loopback measurements. The failpoint delays real inserts; it is not a substitute
for measuring a deployment's actual network/storage latency.

## Run Everything Available

```bash
# 1) Unit tests
go test ./...

# 2) Integration smoke test(s)
go test -tags=integration ./internal/mongo -v

# 3) Optional: mongos-only mixed sharding coverage
PLGM_IT_ENABLE_MONGOS_SHARD_TEST=true \
go test -tags=integration ./internal/mongo -run TestRunWorkloadIntegration_MongosMixedShardedAndUnshardedCollections -v -count=1
```

## Insights Feature Test Focus (Unit)

If you are changing Post-Run Insights or explain-analysis behavior, these focused tests are useful:

```bash
go test ./internal/stats -run Insights -v
go test ./internal/webui -run Insights -v
```

These cover filtering/severity behavior, `/api/insights` lifecycle, and export payload parity with insights output.

## Workload Lifecycle Observability

`/api/stats` now includes explicit lifecycle state and structured initialization progress fields:

- `lifecyclePhase` (`initializing`, `running`, `completed`, `failed`)
- `lifecycleStep`, `lifecycleStepIndex`, `lifecycleStepTotal`
- `lifecycleStepDone`, `lifecycleStepWork`, `lifecycleStepProgressPct`
- `lifecycleRecentEvents` (bounded recent activity list)
- `initializationDurationSec` and `executionElapsedSec`

This enables UI/automation to distinguish preparation time from measured execution time.
