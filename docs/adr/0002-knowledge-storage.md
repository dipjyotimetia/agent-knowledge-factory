# ADR 0002: Knowledge storage

**Status:** Accepted  
**Date:** 2026-11-23

## Context

Agents need a diffable, reviewable corpus. Inventing a private file format would split the ecosystem. OKF v0.3 already models provenance, trust, freshness, lifecycle, and attestation. Agent Skills already model procedures as directories with `SKILL.md` frontmatter.

## Decision

- Concepts are Open Knowledge Format v0.3 Markdown.
- Procedures are Agent Skills (a directory containing `SKILL.md`, optional `scripts/`, `references/`, and `assets/`).
- Git is the source of truth for approved content and its history.
- PostgreSQL 18 stores operational state (gaps, proposals, audit) and a full-text index that can be rebuilt from Git.
- No graph database in v1. Trigram similarity is a lexical aid. KF-13 adds an optional local hash rerank (`SEARCH_MODE=hybrid`). pgvector stays an opt-in script and is not installed by the migrations.

## Consequences

Validators follow the published OKF v0.3 and Agent Skills specifications, including the rules those specs leave optional. Scripts inside a skill are indexed as untrusted files and are never executed by the factory.
