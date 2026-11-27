# Local development

## Dependencies

- Go 1.27.4 (the module language version is 1.26.0, which is the Temporal SDK floor).
- PostgreSQL 18 for `make test`.
- Docker, only for `make dev-up`.
- golangci-lint v2.15.0 for `make lint`.

## Database for tests

`make test` applies migrations as a superuser and then connects as `akf_app`, which is not a superuser and does not bypass row-level security.

```bash
make db-init
export AKF_TEST_ADMIN_DSN="postgres:///akf?host=/var/run/postgresql&sslmode=disable"
make test
```

GitHub Actions sets `AKF_TEST_ADMIN_DSN` to the Postgres 18 service started by the workflow. No Temporal server is required for `make test`.

## Compose

```bash
make dev-up
curl -s localhost:8080/healthz
make dev-down
```

`make dev-up` starts Postgres 18, Temporal 1.29.1 (on its own Postgres 16), the Temporal UI on port 8088, `factoryd` on port 8080, and `factory-worker`. Compose sets `TEMPORAL_ADDRESS=temporal:7233` on `factoryd` and the worker.

`factoryd` serves the HTTP API. `factoryd -mcp` serves that same HTTP API and the MCP server on stdio in one process. Compose keeps those as two services so a detached HTTP process does not take stdin:

```bash
docker compose -f deployments/compose.yaml run --rm -i --service-ports factoryd-mcp
```

`factoryd-mcp` listens on port 8081. The OpenTelemetry collector is optional:

```bash
make dev-up-otel
```

Development tokens in `examples/banking/config.yaml` are loaded only when `AKF_DEV=1`, which Compose sets. Do not set `AKF_DEV` in a shared environment.

## Initialize a workspace

```bash
export AKF_INSTALLER_DATABASE_URL="postgres:///akf?host=/var/run/postgresql&sslmode=disable"
export AKF_CONFIG=examples/banking/config.yaml
go run ./cmd/kfactory init --workspace banking-demo
```

The production index command queues a held default-branch bootstrap. The local
banking fixture runs entirely against its test forge when you run
`go test ./internal/demo -run TestBanking`. See [publication and bootstrap](publication.md)
for the GitHub-backed worker.
