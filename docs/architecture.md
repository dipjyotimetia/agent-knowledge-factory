# Architecture

The Agent Knowledge Factory is a closed loop:

1. An agent retrieves approved knowledge through MCP.
2. The agent reports a gap. The report is validated, de-duplicated, and audited.
3. A curator gathers pinned, allowlisted source evidence and proposes a minimal change.
4. Validators and a before/after evaluation run. Failures do not publish.
5. A GitHub pull request is opened on the knowledge repository. A human merges it.
6. A verified merge reindexes that commit. The next retrieval sees the new revision.

Git is the source of truth for approved knowledge (OKF v0.3 concepts and Agent Skills). PostgreSQL holds operational state and a rebuildable full-text index. Temporal runs the curator workflow. The control API and the MCP server are Go processes (`factoryd`, `kfactory`, `factory-worker`).

Components land in epic order KF-01 through KF-10. Sprint 5 (OpenTelemetry intake) is not part of this MVP. The collector file under `deployments/` is optional and unused by the factory.

See `docs/adr/` for the scope, storage, security, search, and orchestration decisions.
