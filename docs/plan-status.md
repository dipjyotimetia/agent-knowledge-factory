# Plan status

Updated against Implementation Plan v1.0 after the post-MVP gap work. `main` at the start of this work was `01f1267`.

## In scope and implemented in this stack

- Gap reports from MCP and `kfactory gaps report` / `kfactory curate` start `TriageGapWorkflow` when `TEMPORAL_ADDRESS` is set. The workflow id is `curate-<workspace>-<gap>` and a second start is ignored.
- `POST /v1/github/webhook` checks `X-Hub-Signature-256` with HMAC-SHA256 in constant time, records the delivery id once, signals `review` with the merge SHA, revalidates on `synchronize`, and reconciles a push to `main`. The worker also reconciles every minute.
- Source snapshots, evidence links, agent runs, validation runs, and evaluation cases/runs are written in Postgres. Proposal rows store the diff, source revisions, conflicts, and token counts. Snapshot text is not written to Git.
- `Open` validates OKF and Agent Skills and refuses a critical retrieval regression before creating a pull request. `Publish` moves the gap to `verified` when the published index does not regress. The previous Git SHA is marked `superseded_by`.
- Authenticated routes: `GET /v1/gaps`, `GET /v1/gaps/{id}`, `GET /v1/proposals`, `GET /v1/evaluations`.
- Search results include `last_reviewed`. Secret-shaped queries are logged as `[redacted]` and are not searched.
- Curation budgets load from config and the source-fetch cap is enforced. `kfactory retain` deletes old query logs and snapshots. `kfactory export` omits snapshot bodies and refuses a secret-shaped payload.
- Audit rows are hash-chained.
- Missing procedures can create one skill from a contract sentence. `model.Gate` runs only after the deterministic curator abstains.
- CLI: `kfactory curate`, `eval`, `serve`, `retain`, `export`.
- CI adds gitleaks, gosec, an SPDX SBOM, a Temporal plus Postgres loop, and an opt-in GitHub App job. Release blobs are signed with keyless cosign.

## Still deferred

KF-11 OpenTelemetry intake, KF-12 extra connectors, KF-13 embeddings, KF-14 review UI, streamable HTTP MCP, Kubernetes, vector and graph databases, model training, and billing.

## Owner setup for a live GitHub App

Set repository variable `RUN_GITHUB_SANDBOX` to `true` and these secrets: `GITHUB_APP_ID`, `GITHUB_APP_INSTALLATION_ID`, `GITHUB_APP_PRIVATE_KEY`, `GITHUB_WEBHOOK_SECRET`. The knowledge repository also needs human CODEOWNERS and branch protection that the app cannot bypass. See `docs/runbooks/github-app.md`.
