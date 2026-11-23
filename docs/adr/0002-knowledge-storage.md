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
- No vector database and no graph database in v1. Trigram similarity is available as a lexical aid. Embeddings wait for a measured recall gap (KF-13, out of MVP scope).

## Consequences

Validators follow the published OKF v0.3 and Agent Skills specifications, including the rules those specs leave optional. Scripts inside a skill are indexed as untrusted files and are never executed by the factory.
