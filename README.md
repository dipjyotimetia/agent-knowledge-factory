# Agent Knowledge Factory

Closed-loop knowledge maintenance for coding and testing agents. Agents retrieve approved knowledge, report a gap, and later read the reviewed correction. A curator can propose a change. It cannot publish one.

The banking `distributed-systems` repository is evidence, not a host. This service does not write to it.

**License:** Apache-2.0.

## What the MVP does

Epics KF-01 through KF-10 (this stack) implement:

1. Workspace-scoped control API, Postgres 18, Temporal, and CI.
2. OKF v0.3 and Agent Skills validators. Skill scripts are never executed.
3. A read-only GitHub source adapter that pins a commit SHA. Tests use recorded HTTP.
4. Full-text search of the published Git revision only.
5. MCP tools: `knowledge.search`, `knowledge.get`, `knowledge.report_gap`, `knowledge.submit_feedback`, `knowledge.get_gap_status` (stdio).
6. Gap intake with idempotency, de-duplication, audit, and secret rejection.
7. A deterministic curator, then an optional OpenAI-compatible provider adapter. Claims without pinned evidence do not proceed.
8. Pull-request creation without a merge API. Publication waits for a verified merge commit and then reindexes.
9. A 20-case evaluation set with before/after checks and no golden-answer leakage.
10. One banking learning cycle through `TriageGapWorkflow`: stale movement-event dedupe guidance, evidence from the Avro contract, reviewed change, better retrieval, and automatic `verified` when no critical case regresses.

`kfactory curate` starts a workflow only when `TEMPORAL_ADDRESS` is set; otherwise it exits with an error and does not print a workflow id. `kfactory eval` and `kfactory serve` operate the rest of the loop. `kfactory serve` records webhook deliveries and signals Temporal, including a push to the configured default branch. `kfactory audit verify` checks the hash chain. `kfactory rollback --sha` points search back at an indexed revision. `POST /v1/github/webhook` checks the GitHub HMAC signature before it signals review. Search hits include `last_reviewed`, and a secret-shaped query is stored as `[redacted]`. `factoryd` serves HTTP. `factoryd -mcp` serves that HTTP API and MCP on stdio in the same process.

KF-11 through KF-21 are in this tree: trace intake, connectors, hybrid recall, the review view, safe publication, measured improvement, suggested edits, human deltas, the curator schedule, data-access pages, and the contradiction scan. The current plan status is `docs/plan-status.md`.

## Layout

See the plan's repository map. Start with `docs/architecture.md`, `docs/security.md`, and `specs/`. Setup and day-2 operations: [`docs/operations-guide.md`](docs/operations-guide.md).

## Develop

Requirements: Go 1.27.4 for CI and the container build, PostgreSQL 18 for the factory database, golangci-lint v2.15.0. Docker is only for Compose. `go.mod` says `go 1.26.0` because that is the language version the Temporal Go SDK requires; the toolchain that compiles this repo is 1.27.4. Compose runs Temporal on its own Postgres 16 so its schema stays off the factory database.

```bash
make db-init
make test
make dev-up
curl -s localhost:8080/healthz
```

Details: `docs/runbooks/local-development.md`.

The module depends on:

- `github.com/modelcontextprotocol/go-sdk` v1.8.0 (protocol 2026-07-28, negotiates older versions the SDK supports)
- `go.temporal.io/sdk` v1.51.0
- PostgreSQL full text search and `pg_trgm` on PostgreSQL 18

Those modules arrive in the epic that uses them. KF-01 uses `pgx` v5 and Cobra.

## Manual setup before real GitHub or model traffic

Tests and CI do not need these. `.env.example` lists every variable. None of the secret values belong in Git.

1. **API database role.** Create a non-superuser, non-`BYPASSRLS` role and set `AKF_DATABASE_URL`. Keep `AKF_INSTALLER_DATABASE_URL` for migrations only.
2. **Bearer tokens.** Set `AKF_AGENT_TOKEN` and `AKF_CURATOR_TOKEN`. Leave `AKF_DEV` unset. The sample dev tokens are refused without it.
3. **Knowledge repository.** A separate Git repo with the same layout as `examples/banking/fixture-knowledge`: an OKF v0.3 bundle (`index.md` and concept files such as `events/`) plus Agent Skills under `skills/`. Add `CODEOWNERS` that names humans. Protect `workspace.default_branch` (main when that field is unset): human review required, code-owner review required, stale reviews dismissed, no bypass for the GitHub App. Set `workspace.knowledge_owner` and `workspace.knowledge_repo` to that repository. Those fields are what the publisher uses.
4. **GitHub App for that repository only.** Contents and pull requests: read/write. Metadata: read. No administration permission. Do not put the app in `CODEOWNERS`. Do not give it a workflow that merges. Set `GITHUB_APP_ID`, `GITHUB_APP_INSTALLATION_ID`, and `GITHUB_APP_PRIVATE_KEY`.
5. **Webhook.** Point pull-request and push events at `/v1/github/webhook` with `GITHUB_WEBHOOK_SECRET`. Unsigned deliveries are rejected.
6. **Banking reads.** Optional `GITHUB_READONLY_TOKEN` if the evidence repository is private. The adapter allowlist is `api.github.com` and `contracts/avro/` at the pinned SHA. The factory never writes to `distributed-systems`.
7. **Model provider, optional.** Set `AKF_MODEL_BASE_URL`, `AKF_MODEL_API_KEY`, and `AKF_MODEL_NAME` for the OpenAI-compatible adapter. Empty values keep the deterministic curator, which is what the banking demo runs.

`docs/runbooks/github-app.md` restates the GitHub steps.

## Banking demo

```bash
make test
```

`go test ./internal/demo` is the learning cycle: a stale `movementId` guide is reported by two agents, checked against the recorded Avro contract, proposed as a pull request, withheld from search until a human merge, then retrieved as `eventId`. The critical eval case improves and a rollback reindex restores the previous tree.

## Status

KF-01 through KF-10 are the MVP. KF-11 through KF-21 extend that loop and are specified under `specs/`. The model adapter is optional and tested against a fake server; the demo uses the deterministic curator.
