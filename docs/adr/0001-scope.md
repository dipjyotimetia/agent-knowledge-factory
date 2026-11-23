# ADR 0001: Scope and source-of-truth boundary

**Status:** Accepted  
**Date:** 2026-11-23

## Context

The factory is an independent service. The banking `distributed-systems` repository already owns event contracts, service code, and its own AGENTS.md scope. Mixing the factory, or planted defects, into that repository would make curation look like a change to the system under test.

## Decision

- This repository contains the factory services, orchestrator, connectors, MCP server, CLI, fixtures, and deployment.
- Approved knowledge lives in a separate Git repository (for the demo, the fixture under `examples/banking/fixture-knowledge` stands in for that repository).
- The banking repository is read-only evidence. Adapters pin a commit SHA. Nothing in this project writes to it.
- Task agents cannot mutate Git. The curator credential can open a pull request on the knowledge repository and cannot merge it.
- An open pull request is not published knowledge. Search returns only the verified published SHA.

## Consequences

Demos copy contract text into recorded HTTP fixtures. They do not fork or patch `distributed-systems`. Operators who point the read adapter at the live repository still use a read-only credential.
