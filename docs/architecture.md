# Architecture

The Agent Knowledge Factory is a closed loop:

1. An agent retrieves approved knowledge through MCP.
2. The agent reports a gap. The report is validated, de-duplicated, and audited.
3. A curator gathers pinned, allowlisted source evidence and proposes a minimal change.
4. Validators and a before/after evaluation run. Failures do not publish.
5. A GitHub pull request is opened on the knowledge repository. A human merges it.
6. A verified merge reindexes that commit. The next retrieval sees the new revision.

Git is the source of truth for approved knowledge (OKF v0.3 concepts, Agent Skills, and data-access pages). PostgreSQL holds operational state and a rebuildable full-text index. Temporal runs `TriageGapWorkflow` and the scheduled `DrainOpenWorkflow`. The control API and the MCP server are Go processes (`factoryd`, `kfactory`, `factory-worker`).

KF-01 through KF-10 are the MVP. KF-11 through KF-21 cover trace intake, Notion and Slack reads, hybrid recall, the read-only review view, and the publication and curation gates. The collector file under `deployments/` is optional. The factory imports a trace file; it does not export spans.

See `docs/adr/` for the scope, storage, security, search, and orchestration decisions.
