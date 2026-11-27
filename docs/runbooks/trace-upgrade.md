# Upgrade to KF-24

Migration 010 adds workspace-scoped storage for sanitized traces and clears
every existing `trace_ref` on gap reports and agent runs. It preserves the gap
and run records. The migration treats the old values as opaque text and discards
them. Operators can resubmit a gap with a fresh idempotency key after importing
the original local fixture.

## Upgrade the database and services

1. Keep the production publication hold in place pending KF-25. Record the
   active release metadata and take a protected database backup. Handle any
   pre-upgrade backup under the existing retention policy because it can still
   contain legacy reference values.
2. Pause intake and stop the old `factoryd`, CLI automation and workers. Apply
   the embedded migrations with the installer connection. `factoryd` applies
   migrations and refreshes grants on startup when configured with
   `AKF_INSTALLER_DATABASE_URL`. Keep the application connection on the
   non-superuser `akf_app` role. Verify `010_trace_records.sql` in
   `schema_migrations` and verify both legacy columns are empty before resuming
   intake.
3. Configure optional stable actor IDs for bearer credentials and stdio.
   A credential without an actor ID can still submit trace-free reports.
   Restrict the trace import config and application database credential to
   the trusted local operator.
4. Deploy the updated worker and API, then resume intake. Production retrieval
   requires a KF-22 active release matching the workspace pointer. A workspace
   with only a legacy pointer returns empty search results and item lookups
   return 404. Follow the [publication runbook](publication.md) to stage a
   bootstrap under the production hold. Explicit revisions on HTTP, MCP and
   CLI search or retrieval must match the active SHA.

## Import and link a trace

Use a trusted workspace config with an operator actor and an explicit owner
binding for each run. The example [banking config](../../examples/banking/config.yaml)
binds `run-qe-1` to `banking-qe`.

```bash
kfactory traces import --config examples/banking/config.yaml \
  --file examples/banking/tests/trace.json --run-id run-qe-1
```

Run the command on Linux or macOS. Use a regular file under a path with no
symlink components. Use a physical path if the usual path passes through a
macOS symlink such as `/tmp`. The input limit is 1 MiB. The importer accepts a
JSON transcript with a `turns` array or an array of flat GenAI attribute maps
with `gen_ai.prompt`, `gen_ai.completion` and `gen_ai.human` entries. It discards
recognized sensitive fields, redacts detected credentials and common contact
details in text, and rejects malformed or ambiguous input. Use synthetic
traces or review text before importing it. Redaction covers known patterns.

Set a new gap's `trace_ref` to the single `trace:<uuid>` line the command prints.
Submit with the matching `agent_run_id` as the configured owner, or use a
configured trusted curator. HTTP gap intake is available at
`POST /v1/gaps` with a bearer credential. A malformed reference returns 400.
Missing, expired and out-of-scope references all return the same 404.
The canonical investigation uses sanitized human turns when the persisted
link still checks out. If it expires before preparation, the attempt records
`unavailable` and continues. The retention workflow deletes expired trace
payloads using the expiry calculated from workspace retention at import.

For a local end-to-end acceptance run with PostgreSQL and Temporal, run
`go test ./internal/demo -run Test23RestartDuringBankingReviewAndReplay -count=1`.
The fixture imports a trace, links its ID, checks the canonical attempt,
replays history and verifies the proposal through the release loop.
