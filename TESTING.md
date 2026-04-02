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

### Option A: Start MongoDB with Docker Compose (recommended)

From repository root:

```bash
docker compose -f docker-compose.integration.yml up -d
```

Then run the integration smoke test:

```bash
go test -tags=integration ./internal/mongo -run TestRunWorkloadIntegration_OneShotExecutesFindUpdateAndSkipsInsert -v
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

## Run Everything Available

```bash
# 1) Unit tests
go test ./...

# 2) Integration smoke test(s)
go test -tags=integration ./internal/mongo -run TestRunWorkloadIntegration_OneShotExecutesFindUpdateAndSkipsInsert -v
```

## CI Notes (Optional)

- Keep `go test ./...` as the required fast gate.
- Run `-tags=integration` tests in a separate optional/extended CI job with Docker services.
