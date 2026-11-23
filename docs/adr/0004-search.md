# ADR 0004: Search

**Status:** Accepted  
**Date:** 2026-11-23

## Context

The first retrieval question is whether an agent sees the approved revision of the right document, not whether a vector index can paraphrase it.

## Decision

- Default retrieval is PostgreSQL full text search (`english` configuration) plus title trigram and heading text.
- Queries are filtered by workspace and by the published Git SHA before ranking.
- Draft, deprecated, superseded, and unapproved rows are excluded from the default search.
- Stale but still approved documents may be returned with a warning. A `stale_after` value that does not parse is treated as not fresh.
- Query logs used later for retrieval tuning must be redacted before they are sent to a curator.

## Consequences

The index is disposable. Rebuilding it from the published Git tree is the recovery path. Changing the published SHA is the publication step, and it happens in the same transaction as the index rebuild for that SHA.
