# Plan status

Updated against Implementation Plan v1.0 after the post-MVP gap work. `main` at the start of this work was `01f1267`.

## In scope and implemented in this stack

- Gap reports from MCP and `kfactory gaps report` start `TriageGapWorkflow` when `TEMPORAL_ADDRESS` is set. `kfactory curate` refuses to start, and does not print a workflow id, when that variable is unset. The workflow id is `curate-<workspace>-<gap>` and a second start is ignored.
- `POST /v1/github/webhook` checks `X-Hub-Signature-256` with HMAC-SHA256 in constant time, records the delivery id once, signals `review` with the merge SHA, revalidates on `synchronize`, and reconciles a push to `workspace.default_branch`. `factoryd` and `kfactory serve` both wire that signal and the delivery store. Reconcile uses the publisher built from the workspace repository. The worker also reconciles every minute.
- Temporal Cloud API keys (`TEMPORAL_API_KEY`) and mTLS (`TEMPORAL_TLS_CERT`, `TEMPORAL_TLS_KEY`, `TEMPORAL_TLS_CA`) are accepted by `factoryd`, `kfactory`, and `factory-worker`.
- Source snapshots, evidence links, agent runs, validation runs, and evaluation cases/runs are written in Postgres. Proposal rows store the diff, source revisions, conflicts, and token counts. Snapshot text is not written to Git.
- `Open` validates OKF and Agent Skills and refuses a critical retrieval regression before creating a pull request. `Publish` moves the gap to `verified` when the published index does not regress. The previous Git SHA is marked `superseded_by`.
- Authenticated routes: `GET /v1/gaps`, `GET /v1/gaps/{id}`, `GET /v1/proposals`, `GET /v1/evaluations`.
- Search results include `last_reviewed`. Secret-shaped queries are logged as `[redacted]` and are not searched.
- Curation budgets load from config and the source-fetch cap is enforced. `kfactory retain` deletes old query logs and snapshots. `kfactory export` omits snapshot bodies and refuses a secret-shaped payload.
- Audit rows are hash-chained.
- Missing procedures can create one skill from a contract sentence. `model.Gate` runs only after the deterministic curator abstains.
- CLI: `kfactory curate`, `eval`, `serve`, `retain`, `export`, `audit verify`, and `rollback`.
- `make dev-up` starts `factory-worker` and passes `TEMPORAL_ADDRESS` to `factoryd`. The image includes `/factory-worker`. `factoryd -mcp` serves HTTP and stdio MCP together. Compose service `factoryd-mcp` is the attached MCP process.
- CI adds gitleaks, gosec, an SPDX SBOM, a Temporal plus Postgres loop, and an opt-in GitHub App job. Release blobs are signed with keyless cosign.

Specs for this section are `specs/KF-11.md` through `specs/KF-21.md`.

## Added after the fundamentals review

- create-skill proposals persist, webhooks retry after a failed signal, and a candidate index is scored before it is published.
- `verified` requires a targeted `expects_change` case to improve. Decision cases run in the same gate.
- Gap reports can carry `suggested_edit`. The curator reads observed, expected, unread feedback, and a `trace_ref` file.
- A human-only line on the merged tree becomes a follow-up gap.
- `DrainOpenWorkflow` refetches evidence on `CURATION_INTERVAL` (default 24h).
- `data-access` is a third item type. The banking fixture has `events/movement-query.md`.
- Trace import accepts JSON and OpenTelemetry GenAI attributes, with secret and simple PII redaction.
- The cadence pass scans the corpus for conflicting claim patterns.
- Notion and Slack are read-only allowlisted adapters. `publish.Directory` is a second writer. GitHub still opens pull requests.
- `SEARCH_MODE=hybrid` reranks full-text hits. pgvector itself is `docs/sql/pgvector.sql` and is not installed in CI.
- `GET /v1/review` is a read-only authenticated view. It does not merge.

## Still deferred

Streamable HTTP MCP, Kubernetes, a graph database, model training, and billing. Live Notion or Slack workspaces and a pgvector extension are opt-in and are not required for CI.

## Owner setup for a live GitHub App

Set repository variable `RUN_GITHUB_SANDBOX` to `true` and these secrets: `GITHUB_APP_ID`, `GITHUB_APP_INSTALLATION_ID`, `GITHUB_APP_PRIVATE_KEY`, `GITHUB_WEBHOOK_SECRET`. The knowledge repository also needs human CODEOWNERS and branch protection that the app cannot bypass. See `docs/runbooks/github-app.md`.
