# ADR 0005: Orchestration

**Status:** Accepted  
**Date:** 2026-11-23

## Context

Curation waits on human review. The wait has to survive process restarts, and a retry must not open a second pull request or treat an attempted merge as success.

## Decision

- The curator workflow is a Temporal workflow (`go.temporal.io/sdk` v1.51.0).
- Model calls, GitHub calls, and database writes are activities.
- The workflow version is recorded with Temporal's `GetVersion` gate so the approval wait can evolve.
- A synchronous runner may call the same activity methods in tests and in the local demo. It is not a second policy implementation.
- Publication runs only after an activity observes the merge commit on the knowledge repository's default branch. The curator client has no merge method.

## Consequences

Local `make dev-up` starts Temporal 1.29.1 against its own Postgres. Unit and workflow tests use the Temporal Go test suite, so `make test` does not require the Temporal server. A workflow test that needs a live server is gated on `TEMPORAL_ADDRESS`.
