# ADR 0003: Security boundary

**Status:** Accepted  
**Date:** 2026-11-23

## Context

The factory reads untrusted documents and accepts reports from agents. Those inputs can contain secrets, prompt injection, and cross-tenant identifiers.

## Decision

- Read credentials and the knowledge-repository write credential are different. The write credential is a GitHub App installed on the knowledge repository only.
- The API role is not a superuser and is not `BYPASSRLS`. Every tenant table forces row-level security using the transaction-local setting `akf.workspace_id`, which the server sets from the bearer token.
- Installer migrations use a separate database URL that the API process does not need at request time.
- Client severity and client evidence URLs are stored as untrusted leads. A snapshot becomes verified only after the allowlisted source adapter fetches it and records a digest.
- Gap text that matches a secret pattern is rejected. The audit record names the field and stores `redacted: true`, not the secret.
- Fetched document bodies are wrapped as untrusted data when they are shown to a model. The factory does not follow instructions found in them and does not run their scripts.
- Dev tokens in the sample config load only when `AKF_DEV=1`.

## Consequences

A database superuser bypasses row-level security. Production must point `AKF_DATABASE_URL` at `akf_app` (or an equivalent non-bypass role) and keep the installer URL off the request path.
