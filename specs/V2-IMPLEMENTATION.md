# V2 implementation contract and sequencing

Normative for KF-22–38. Read this before each spec. Baseline commit: `ffbdc79e07e368b738de51b86f9db8f41095cf60`. Existing specs and AGENTS.md apply except where explicitly superseded. All new APIs and structures below are proposed, not existing interfaces.

## 1. Scope and non-negotiable architecture

- Git owns approved knowledge; PostgreSQL stores durable operational state, evidence and rebuildable indexes. Human-controlled Git merges only. Never write to `dipjyotimetia/distributed-systems`. `publish.Publisher` must not acquire merge or branch-protection bypass capabilities.
- Retain `factoryd`, `factory-worker`, `kfactory`. Logical modules are packages, not required new services. Use Go standard library, existing SDKs and pgx. Add dependencies only with an ADR and licence/security review. No mandatory paid model, remote service or pgvector.
- Model responses, traces, retrieved documents, suggested edits and tool outputs are untrusted. No direct tool execution, shell, arbitrary URL fetching or skill script execution by curator. Treat authority metadata as server-assigned, not document text.
- Every network/model/database side effect is an activity, service or HTTP handler, never Temporal workflow code. Workflows carry bounded identifiers, versioned inputs and results, not corpus or trace bodies. Retain replay compatibility with `workflow.GetVersion` or explicit new workflow names; document draining legacy workers.
- Production must fail closed on missing policy/evaluation configuration. Local fixtures may use explicit `AKF_DEV=1` and a deterministic publisher/evaluator; no bypass flag in production. A local bootstrap without Git may exist only in dev and cannot be exported as an approved production release.

## 2. Canonical terms and invariants

| Term | Meaning |
|---|---|
| workspace | Trusted principal/config boundary, never inferred from arbitrary request text |
| source revision | Immutable Git commit, provider revision or content digest of a fetched source |
| candidate | Private complete tree/index identified by candidate UUID and tree digest; not a published SHA |
| release | A previously validated Git commit and recorded policy/evaluation receipts |
| active revision | `workspaces.published_sha`, the only default agent-visible release |
| publication generation | Monotonically increasing workspace fence for promotion, hold, rollback and resume |
| gap | Investigation; independent of PR status and release status |
| verified gap | The linked proposal was merged, its effect remains present in the release, and explicit targeted evaluation evidence proves improvement |
| experience | Sanitised immutable signal from a task run; not authoritative evidence |
| evidence | Immutable snapshot from allowed source with server-assigned classification, access and provenance |
| learning candidate | A ranked hypothesis linked to experiences; not approved knowledge |

Gap lifecycle remains compatible with `internal/domain/state.go`: `new → triaged → investigating → proposal-ready → awaiting-review → accepted → published → verified`. `verified` is historical proof at a particular release, not a promise it remains effective forever. Use linked recurrence gaps on regression. Keep insufficient-evidence/rejected/cancelled/failed/duplicate terminal; retry via a distinct attempt or linked gap, not illegal state rewind. Record outcome reason codes separately. Approval does not equal publication. Publication does not equal verification. Release promotion may proceed with unresolved noncritical gaps; do not mark them verified.

## 3. Shared schema / identifiers

Use additive forward migrations numbered next available after `007_data_access.sql`; allocate migration IDs at implementation time (parallel branches may conflict). Every new workspace table has `workspace_id NOT NULL`, `ENABLE` and `FORCE ROW LEVEL SECURITY` plus `USING`/`WITH CHECK` on `current_setting('akf.workspace_id', true)`. Cross-table links use composite workspace foreign keys or transactional ownership checks plus uniqueness; never trust UUID alone. Use UTC `timestamptz`, UUIDs, immutable SHA-256 content digests and explicit schema versions. Prefer typed columns for query/filter keys; bounded JSONB for extensions. No migration rewrites prior migrations. Upgrade tests from baseline and fresh install required. Default deny for unknown legacy state. Do not grandfather arbitrary indexed candidates as releases; controlled bootstrap verification needed.

Canonical idempotency: unique `(workspace_id, operation_kind, idempotency_key)`; same key/same canonical payload returns same receipt, same key/different payload = conflict. Canonical JSON sorted map keys, preserved array order, UTF-8; exclude timestamps generated server side. Stable IDs for workflow/snapshot/PR attempt; store external IDs and reconcile ambiguous outcomes before retry. Do not promise exactly-once external side effects; implement effectively-once via deduplication and recovery.

New API errors: HTTP 400 invalid, 401 unauthenticated, 403 forbidden, 404 missing or cross-workspace opaque object, 409 conflicting/idempotency or stale generation, 413 size limit, 429 quota, 503 dependency/config unavailable. JSON `{ "code": "stable_machine_code", "message": "safe message", "request_id": "..." }`; preserve legacy error field where clients need it. Do not reveal paths, tokens, source payloads or other workspace IDs. MCP translates domain errors to tool errors without secrets. All new lists use stable `(created_at,id)` cursor, maximum page 100. Responses include revision/receipt IDs as relevant.

## 4. Security, retention and observability

- All authenticated operations derive workspace and actor from trusted credentials, not request fields. Local operator CLI uses explicit config and audited role; no anonymous or default reviewer privilege. Agent read/submit only; curator propose only; reviewer records decisions; separate operator authority for publication hold/resume (local CLI until a secure operator API exists). Human Git approval is independently verified.
- Preserve source permissions; first release is workspace-level only. A source can feed publishable knowledge only when explicitly classified as workspace-shareable. Restricted content must not enter a workspace-wide index/model; use separate workspaces rather than pretending item ACLs exist. Permission loss invalidates affected candidates and withdraws affected active content until a safe release; KF-31/33 define mechanics.
- Redact before persistence, logging, model calls and workflow history. Secrets reject or quarantine; no raw production prompts, customer records, or executable payloads. Use bounded memory/bytes, cancellation, no uncontrolled decompression/redirects. Record reasons not sensitive material. Tenant-specific retention defaults to existing workspace retention; raw data is never stored. Retain minimal non-content receipts and digests for audit subject to deletion policy; do not claim digests are anonymous. Protect database backups with the same retention controls.
- Structured audit for privilege changes, publication, evidence rejection and policy decisions; use existing hash chain. Metrics low-cardinality (outcome/kind, not URI/run ID/user); trace attributes contain IDs only, not bodies. No source data or tokens in stdout, PR descriptions or telemetry. Metrics are not proof of causality.

## 5. Delivery order and compatibility

| Wave | Sequence | Gate to next wave |
|---|---|---|
| A — safety | 22 → 23 → 24 → 25 | All four integrated; no staging leak; rollback survives restart; concurrent publication tests pass |
| B — learning | 26 → 27 → 28 → 29 → 30 | Synthetic traces produce scoped hypotheses and evidence-backed PRs; no execution or direct publish |
| C — knowledge | 31 → 32 → 33 → 34 | Incremental drift, revocation, structured contracts and pinned retrieval tested |
| D — proof & integration | 35 → 36 → 37 → 38 | Reproducible execution replay, remote security conformance and banking demo |

All wave-A changes must land before production enablement. KF-22 may introduce a publication package before KF-23 removes all shared state; both must be reviewed together. KF-24 tightens legacy revision/trace behaviour intentionally; provide migration guide and update fixtures. Later specs build on these interfaces; keep flags default-off for new ingestion/remote services until configured. Existing stdio tools and local workflows remain supported. Bootstrap existing installations under a hold, verify currently active Git tree and rebuild release receipts, then enable normal reconciliation explicitly. Do not infer approval from legacy `approved=true` rows. Do not label legacy verified gaps as newly proven; record legacy-unverified provenance in an additive receipt or separate status field.

## 6. Shared test/PR gates (mandatory for every spec)

1. Add failing tests for the specific baseline defect, or contract tests for a new feature. Use table-driven unit tests, deterministic clocks and fake providers; use PostgreSQL integration tests under the non-superuser RLS app role. Test empty/fresh and migrated DBs when touching schema. No network in default tests. No `time.Sleep` as concurrency assertion; barriers/channels and bounded deadlines.
2. Cover cross-workspace access, malformed input, cancellation, retry/idempotency, crash boundaries and secret leakage whenever relevant. Use Temporal test environment and live Temporal/Postgres CI for replay/restart. Tests must inspect actual persisted state, not just HTTP status.
3. Run `make check`, `make build`, existing banking `go test ./internal/demo -run '^TestBankingLearningCycle$' -count=1` with configured DB. `make test` uses race detector; do not skip integration tests silently. Record exact commands and environment, failures/skips and why. `go mod tidy` only if dependencies change. Update schemas, docs/security.md, architecture.md, operations guide, plan-status and example config where behaviour changes. Mark implemented only after acceptance tests pass.
4. Use one branch/PR per spec, reference KF ID and acceptance IDs in tests/PR. PR includes migration/rollback plan, code paths changed, risk and compatibility notes, test output and known limitations. Never push to default branch. No test weakening, dummy success path or blanket suppression. No unrelated refactor.

## 7. Codex execution template

```
Implement specs/KF-22.md, following specs/V2-IMPLEMENTATION.md and AGENTS.md.
Read the baseline code and relevant tests first. Confirm prerequisites are merged.
Produce a short plan mapping acceptance IDs to code and tests, then implement.
Use the smallest compatible changes that satisfy the contracts. Add the required
migrations and tests; run the shared gates and spec-specific tests. Do not modify
the banking source repository, weaken trust gates, auto-merge, or claim skipped
tests passed. Update documentation and report exact test results. Stop only if
a requirement is genuinely contradictory or requires unavailable credentials;
otherwise implement against deterministic fakes. Work on a feature branch and
prepare a PR summary; do not merge.
```

For subsequent specs replace the ID. For review: `Review the diff for KF-XX against every acceptance ID and the shared security invariants. Find missing negative tests, bypass paths and retry races; fix them before reporting completion.`

## 8. Baseline evidence map

- Publication: `internal/workflows/gate.go` (vetProposal, commitCandidate, verifyOpenGaps); `steps.go` (Prepare, Open, Publish); `internal/runtime/wire.go` (NewReviews); `internal/api/webhook.go`; `internal/index/index.go` (Stage, Promote, Reindex, Get, Rollback); `internal/publish`.
- Learning: `internal/traces/trace.go`, `internal/gaps/service.go`, `internal/curator/curator.go`, `scan.go`, `internal/model/openai.go`, `internal/workflows/cadence.go`, `human.go`, `evidence.go`, `record.go`.
- Retrieval/evaluation: `internal/index/hybrid.go`, `internal/evaluations/eval.go`, `examples/banking/tests/cases.json`, `internal/demo/banking_test.go`, `internal/mcpserver/server.go`.
- Baseline contracts: `migrations/001_init.sql` through `007_data_access.sql`, `schemas/`, `docs/security.md`, `docs/architecture.md`, `AGENTS.md`, `Makefile`.

External protocol references for implementation: [MCP specification](https://modelcontextprotocol.io/specification/), [OTel GenAI conventions](https://opentelemetry.io/docs/specs/semconv/gen-ai/), [Temporal Go SDK](https://docs.temporal.io/develop/go), [Agent Skills](https://agentskills.io/specification), [GitHub REST API](https://docs.github.com/en/rest). Pin supported versions in implementation ADRs; do not assume future protocol features or silently upgrade dependencies.

## 9. Cross-spec integration ownership and decisions

Use the following ownership boundaries to avoid duplicate implementations. A later spec extends the owning module and its tests rather than creating a second service.

| Boundary / owner | Consuming specs | Rule |
|---|---|---|
| Release receipts, candidate namespace, publication outbox (22) | 23, 24, 25, 31, 33, 34, 35 | Only publication can promote; retrieval never mutates pointer |
| Attempt and operation ledger (23) | 26–31, 35 | Persist reservations and fenced outputs; no mutable per-attempt worker state |
| Principal and trace access, public/private reader split (24) | 26, 29, 31–35, 37 | A public API cannot hold candidate-reader capability |
| Publication controls, generation and quarantine (25) | 31, 33, 35 | Trust change invalidates outstanding receipts; hold survives restart |
| Experience event ingestion and run reducer (26) | 27, 29, 30, 35, 36, 37 | Append immutable normalised events; all event consumers dedup |
| Candidate detector and checkpoint (27) | 30, 31, 33, 36 | Single candidate→gap mechanism, different detector kinds |
| Curator tools, claims, budgets (28) | 29–33 | All proposals use same validators and evidence policy |
| Feedback classification (29) | 30, 33 | Feedback is an observation, never a credential or source authority |
| Source registry/access state (31) | 32–35 | One permission decision used by indexing, serving, evaluation, rollback |
| Knowledge metadata/authority/withdrawals (33) | 34–38 | Withdrawal evaluated before retrieval, including replay permission checks |
| Retrieval and context (34) | 35, 37 | Same ranking/config for authorised candidate and baseline |
| Replay evaluation (35) | 36, 38 | Actual receipts, immutable explicit case links, no golden answer leakage |
| Learning metrics (36) | 38 | Reports derive from stored events/receipts, not illustrative targets |

### Required implementation decisions before code

- **Actor identity (KF-24 onward):** baseline `auth.Principal` exposes workspace and role only. Extend config with stable non-secret `actor_id` and derive it from configured token identity; never hash raw token as public ID. Trusted stdio gets configured actor ID; operator imports use explicit operator identity and delegated run ownership, audited. Legacy tokens may retain their existing workspace-scoped search/report privileges but cannot claim ownership of other actors' traces/events. For new experience/trace submissions require a stable actor mapping; do not infer identity from run ID or client fields. Distinct tokens for same actor intentionally share actor scope; rotation retains stable actor ID. KF-37 maps verified external identity to this same contract.
- **Candidate ID versus Git SHA (KF-22, 24):** candidate UUID is private; full tree digest is content identity; actual merged SHA is release identity. Do not make a candidate by mutating rows at an already active SHA. Pre-PR and post-merge candidates may have different tree digests. Re-use evaluation receipt only on exact tree, base, policy, dataset, evaluator and generation match. Candidate staging ownership and expiration recorded; do not delete while active evaluation lease exists. Historical evaluation read is a separate internal capability, not a public revision parameter.
- **Source revocation versus index rebuild (KF-31, 33):** current trust decision is authoritative even if an approved Git commit contains old material. Store reverse source links by release+URI. If a source is revoked before legacy links are complete, hold the workspace and disable affected retrieval (disable entire workspace if affected items cannot be identified). Never silently reconstruct old content from Git as a way around revocation. Remediation requires authorised source or reviewed removal. Make read/permission check transactionally coherent; no promise to retract already delivered text.
- **Bootstrap (KF-22, 25):** initial workspace without release may evaluate against an explicitly empty baseline with mandatory absolute safety/critical checks; no claim of improvement. Existing installations start held with previously served revision treated as legacy until verified against authoritative default branch and configured gates. A safe legacy migration path must not expose a staged revision. If no proof/safe release, return unavailable. Use deterministic fixture-only exceptions under dev config, not general production bypass.
- **Release gate minimum (KF-22, 35):** structural, secret, evidence/access and required retrieval checks always mandatory; KF-35 extends policy with execution replay and explicit safety checks. Until KF-35 lands, `verified` means retrieval-level targeted proof only, labelled `proof_kind=retrieval`; after KF-35 require configured task-level proof for execution claims and distinguish legacy proof. A published gap without proof remains published. Do not report all historical gaps as execution-verified.
- **Feedback/experience retention:** if source event content expires, keep receipt without content where policy permits; don't fabricate evidence or resurrect deleted material. Pending attempts with expired required evidence must re-fetch authorised source or abstain. Metrics report missing/expired coverage. All deletion endpoints and retention activities are workspace-scoped and audit safe metadata.
- **Fixture versus production:** default deterministic CI uses fake providers and actual PostgreSQL RLS; dev-only merge simulation is never added to production Publisher. A successful fixture run demonstrates that the implementation works for defined cases, not general live-model learning performance. Optional live tests require explicit configuration and safe isolated resources.

## 10. Final release checklist

Before merging KF-38 or labelling V2 ready: confirm all 17 spec PRs and their acceptance IDs, no alternative publication pointer writes, no candidate reads from public routes, no trace path opens in worker, no global feedback consumption, no shared activity state, no unchecked source fetching, no bypass on rollback/revocation, no unbounded model/tool call loop, no scripts executed, no live source writes, no fabricated evaluation metrics. Check fresh install and upgrade from baseline, PostgreSQL app-role RLS, race detector, live Temporal restart, deterministic demo and documentation. Record remaining opt-in integrations, benchmark limitations, security review and any skipped tests; do not claim production readiness for unverified environments.
