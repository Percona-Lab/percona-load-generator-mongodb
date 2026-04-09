# Testing Guide

This repository has two test layers:

1. Unit tests (default, fast, no MongoDB required)
2. Integration tests (optional, require a real MongoDB instance)

## Unit Tests

Run all unit tests:

```bash
go test ./...
```

Unit tests are the default and are intentionally independent of local MongoDB availability.

## Integration Tests

Integration tests are behind the `integration` build tag and are skipped from default runs.

Current integration smoke test:

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
