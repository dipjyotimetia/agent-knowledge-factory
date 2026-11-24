# Agent Knowledge Factory — V2 implementation pack

**Status:** Proposed implementation contracts; no code changes in this pack.  
**Baseline:** `dipjyotimetia/agent-knowledge-factory@ffbdc79e07e368b738de51b86f9db8f41095cf60` (`main`, reviewed 24 Nov 2026).  
**Reference:** [How to build a knowledge factory](https://www.langchain.com/blog/knowledge-factory).  
**Scope:** KF-22–KF-38. Keep Go, PostgreSQL, Temporal, Git, MCP and the existing three executables. No new mandatory broker, graph database, Kubernetes, or agent framework.

## How to use this pack

Copy `specs/KF-22.md` through `specs/KF-38.md` and `specs/V2-IMPLEMENTATION.md` into the existing repository. Do not overwrite KF-01–21. The guide is normative for all new specs. Where an old spec contradicts a new security invariant (notably KF-11 filesystem traces and KF-17 global feedback consumption), the new spec supersedes that behaviour. Existing AGENTS.md remains binding. Follow the dependency order, use one branch/PR per spec, and do not implement all specs in one prompt.

Before implementation, Codex must read AGENTS.md, docs/architecture.md, docs/security.md, the guide and the chosen spec; inspect the current branch and relevant code/tests. If the baseline has moved, record what changed and adapt identifiers without changing the contract. Do not assert that proposed files, routes, or migrations already exist.

Start with KF-22 and KF-23 as a coordinated safety release; do not deploy the intermediate branch to real workspaces. See the [implementation guide](specs/V2-IMPLEMENTATION.md) for precise ordering, test gates, shared definitions and ready-to-use Codex prompts.

## Contents

| Spec | Deliverable | Dependencies |
|---|---|---|
| KF-22 | Single publication pipeline | Baseline |
| KF-23 | Stateless activities, operation ledger | 22 |
| KF-24 | Safe trace IDs, candidate isolation | 22, 23 |
| KF-25 | Durable holds, quarantine and rollback | 22–24 |
| KF-26 | Normalised experience ingestion | 24 |
| KF-27 | Proactive gap detection | 26 |
| KF-28 | Bounded evidence investigation | 23, 27 |
| KF-29 | Scoped human feedback | 26, 28 |
| KF-30 | Procedure mining | 27–29 |
| KF-31 | Incremental source discovery | 23, 28 |
| KF-32 | Data-access contracts | 28, 31 |
| KF-33 | Authority and lifecycle | 29, 31, 32 |
| KF-34 | Revision-pinned retrieval/context | 24, 32, 33 |
| KF-35 | Comparative execution replay | 25, 26, 34 |
| KF-36 | Learning metrics | 27, 35 |
| KF-37 | Remote MCP and SDK | 24, 26, 34 |
| KF-38 | Banking showcase | All preceding specs |

These are proposed requirements, not evidence that features or tests have been implemented. Verify all implementation results in CI before claiming completion.
