# Architecture

Agent Knowledge Factory maintains the knowledge that coding and testing agents retrieve. Agents report gaps, the curator proposes evidence-backed edits, and humans review and merge those edits in a separate GitHub knowledge repository. Publication makes an approved Git revision available to agent search.

[Interactive architecture diagram](diagrams/architecture.html) · [Archify source](diagrams/architecture.json)

Open the HTML file from a checkout in a browser. GitHub displays the file as source, and the [README Mermaid overview](../README.md#architecture) renders inline on GitHub.

## Runtime and data ownership

| Component | Responsibility |
|---|---|
| [`factoryd`](../cmd/factoryd/main.go) | Runs the HTTP control API and, with `-mcp`, serves workspace-bound MCP on stdio in the same process. The HTTP API offers bearer-authenticated search and operations, a read-only review view, and a signed GitHub webhook endpoint. |
| [`kfactory`](../cmd/kfactory/main.go) | Provides operator commands for workspace initialization, validation, indexing, gap intake, curation, evaluation, audit verification, and publication requests. `serve` runs the HTTP surface. |
| [Temporal and `factory-worker`](../cmd/factory-worker/main.go) | Temporal keeps durable workflow state. The worker registers `TriageGapWorkflow` and `DrainOpenWorkflow` on the configured task queue, runs activities, and reconciles Git and enforces retention on a minute tick. Workflow code orchestrates activities and waits for signals. Model and HTTP calls run outside workflow code. |
| [PostgreSQL 18](../migrations/) | Holds workspace-scoped gaps, proposals, audit events, evidence snapshots, evaluation records, and the rebuildable search index. The workspace's `published_sha` selects the default revision. The factory database and Temporal's backing database are separate in [Compose](../deployments/compose.yaml). |
| [GitHub knowledge repository](runbooks/github-app.md) | Holds the approved Git history for OKF v0.3 concepts, Agent Skills, and data-access pages. The scoped GitHub App writes proposal branches and opens PRs. Humans merge through GitHub under the configured branch protection. |

The banking evidence repository is separate from the knowledge repository. The [GitHub source adapter](../internal/ingestion/github/adapter.go) reads allowlisted paths at a pinned commit. Optional [Notion and Slack connectors](../internal/workflows/evidence.go) supply secondary evidence. Agent Skill scripts are indexed as untrusted bytes.

## Knowledge maintenance loop

1. **Retrieve and report.** MCP search and get use the workspace index. Default search returns approved, stable items at the published revision. PostgreSQL full-text search is the default. `SEARCH_MODE=hybrid` reranks full-text hits locally with hashed-token similarity. MCP or `kfactory gaps report` accepts a gap, validates it, applies idempotency and deduplication, and audits the result. Secret-bearing reports are rejected with a redacted audit record. When Temporal is configured, intake can start a workflow for the canonical gap. [Index](../internal/index/index.go) · [Gap service](../internal/gaps/service.go)
2. **Prepare a proposal.** `TriageGapWorkflow` asks the worker to curate from the knowledge tree, configured evidence, and available feedback. The deterministic curator runs first. An OpenAI-compatible provider is optional and can advise after the deterministic curator abstains. An actionable proposal needs grounded evidence before it can reach review. [Workflow](../internal/workflows/workflow.go) · [Curator](../internal/curator/curator.go)
3. **Validate and request review.** The worker validates the proposed OKF bundle and Agent Skills. When evaluation cases are configured, it checks for critical retrieval regressions before opening a PR. The workflow waits for review. A PR head update triggers revalidation, and closing without a merge rejects the proposal. The signed webhook and the role-gated approval endpoint can signal the workflow. [Validation gate](../internal/workflows/gate.go) · [Webhook](../internal/api/webhook.go) · [Approval endpoint](../internal/api/ops.go)
4. **Request a release.** The signed webhook, reviewer merge hint, worker reconciliation and `kfactory index` enqueue workspace-scoped work. The publication worker resolves GitHub identity and reads an immutable full tree, then stages a private index and compares retrieval with the pinned baseline. A passing receipt binds the data, policy, ranking and generation before promotion.

## Reconciliation and rollback

The worker rechecks Git history and fences installation with the active generation and its operation lease. Reconciliation verifies each proposal's exact PR and surviving merged changes. An explicit gap-to-case link can establish retrieval proof from a failing baseline and passing release. Follow the [publication runbook](runbooks/publication.md) for held bootstrap and recovery.

Legacy rollback is disabled pending KF-25. Production rollout remains held through KF-24 and KF-25.

## Regenerate the interactive diagram

The editable [Archify candidate](diagrams/architecture.json) records the reviewed Git commit and source line ranges. When the topology changes, update the candidate and its evidence against a committed revision. From the repository root, set `ARCHIFY_CLI` to the installed `bin/archify.mjs` path and run the command with Node and Chrome or Edge available.

```bash
archify_receipts=$(mktemp -d)
node "$ARCHIFY_CLI" finalize architecture docs/diagrams/architecture.json \
  docs/diagrams/architecture.html --repo-root . --quality showcase \
  --out-dir "$archify_receipts" --json
```

Keep the candidate and generated HTML together. Archify also writes a machine-local delivery receipt next to the HTML. Review the [security boundaries](security.md) and the [architecture decision records](adr/) for the design rationale.
