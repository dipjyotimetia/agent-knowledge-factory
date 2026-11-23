# GitHub App setup

The factory does not ship a private key. Create the app in the GitHub organization that owns the knowledge repository, then put the secrets in the process environment.

## Knowledge repository app (write, no merge)

Install the app on the knowledge repository only.

- Permissions: Contents (read and write), Pull requests (read and write), Metadata (read).
- Do not grant Administration.
- Do not add the app to `CODEOWNERS`.
- Branch protection on the default branch: require a human review, require the code owner review, dismiss stale reviews, and do not allow the app to bypass the rules.
- Leave "Allow auto-merge" off for the app's pull requests if the repository enables auto-merge globally, and do not grant the app a workflow that merges.

Environment:

- `GITHUB_APP_ID`
- `GITHUB_APP_INSTALLATION_ID`
- `GITHUB_APP_PRIVATE_KEY` (PEM contents; the config file stores `env:GITHUB_APP_PRIVATE_KEY`)

The worker mints an installation token from these three values. It does not read a pre-issued bearer token unless `GITHUB_APP_ID` is unset and `GITHUB_APP_TOKEN` is set for a local fake. The publisher refuses HTTP calls whose path merges a pull request. Merge happens in the GitHub UI, and publish indexes the tree at the verified merge commit.

`POST /v1/github/webhook` requires `GITHUB_WEBHOOK_SECRET`. GitHub signs the raw body as `X-Hub-Signature-256`. A repeated `X-GitHub-Delivery` is ignored. A merged curator pull request signals the workflow. A `synchronize` event revalidates the head. A push to `main` reconciles a missed webhook or force-push.

A live GitHub App run is opt-in. Set the repository variable `RUN_GITHUB_SANDBOX` to `true` and the secrets `GITHUB_APP_ID`, `GITHUB_APP_INSTALLATION_ID`, `GITHUB_APP_PRIVATE_KEY`, and `GITHUB_WEBHOOK_SECRET`. CI only checks that those values are present. It does not call GitHub with them.

## Banking source app or token (read only)

The banking `distributed-systems` repository is never written.

- Fine-grained PAT or GitHub App with Contents: read, on that repository only.
- `GITHUB_READONLY_TOKEN`
- Config reference: `env:GITHUB_READONLY_TOKEN`

Public reads of the pinned SHA work without a token. A token is only needed for a private repository or for a higher rate limit. Tests use recorded HTTP fixtures and do not call GitHub.

## Webhook

- Payload URL: `https://<factory>/v1/github/webhook`
- Secret: `GITHUB_WEBHOOK_SECRET`
- Events: Pull requests, Pushes.
- The endpoint rejects unsigned deliveries unless a test explicitly enables insecure mode. Compose does not enable that mode.
