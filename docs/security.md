# Security

Read ADR 0003 before enabling a real organization.

## Credentials

| Use | Where it lives | What it must not do |
|---|---|---|
| API bearer token | `AKF_*_TOKEN` environment variables | Cross a workspace boundary |
| Installer database URL | `AKF_INSTALLER_DATABASE_URL` | Serve HTTP requests |
| API database URL | `AKF_DATABASE_URL` as a non-superuser role | Bypass row-level security |
| GitHub read token | `env:GITHUB_READONLY_TOKEN` reference | Write to any repository |
| GitHub App private key | `env:GITHUB_APP_PRIVATE_KEY` reference | Merge, or write outside the knowledge repository |

The sample Compose password `akf` is a local default. Do not reuse it, and do not commit a filled-in `.env`.

## Gates that must stay red on failure

- Secret-bearing gap text is rejected and the audit payload is redacted.
- Unapproved pull-request content is absent from default search.
- Evidence that was not fetched through the allowlist stays `unverified` or `forged`.
- A proposal whose new claims do not resolve to pinned evidence is not publishable.
- Skill scripts are never executed.
- Webhook deliveries are idempotent. Publish checks the merge commit, not the webhook alone.

## MCP

v1 serves MCP over stdio. The process is bound to the workspace in its config. HTTP MCP is not exposed, so the remote MCP authorization profile is not required yet. The control API uses bearer tokens on `/v1/*`.
