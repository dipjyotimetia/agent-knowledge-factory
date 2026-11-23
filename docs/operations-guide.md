# Operations guide

This is how to install and run the Agent Knowledge Factory as the code in this repository behaves. Commands, routes, flags, and variable names below are the ones the programs actually read. Shorter notes live in `docs/runbooks/local-development.md`, `docs/runbooks/github-app.md`, and `docs/security.md`.

## 1. Overview and architecture

Git is the source of truth for approved knowledge. Postgres holds operational state and a rebuildable full-text index. Temporal runs `TriageGapWorkflow`. Three binaries do the work:

| Binary | Path | Role |
|---|---|---|
| `factoryd` | `cmd/factoryd` | HTTP control API. With `-mcp`, the same process also serves MCP on stdio. |
| `factory-worker` | `cmd/factory-worker` | Temporal worker on task queue `knowledge-curator`. Reconciles and applies retention once a minute. |
| `kfactory` | `cmd/kfactory` | Operator CLI, including `serve`, `curate`, `audit verify`, and `rollback`. |

The curator can open a pull request for human review and merge in GitHub. On the review-signal path, `Publish` checks that the claimed merge SHA matches the configured default-branch head. A default-branch push or the worker's periodic reconciliation can stage a changed head independently. Both paths stage the candidate before promotion. A critical regression in the configured evaluation cases keeps the previous revision available. The review-signal path requires targeted improvement for `verified`. Reconciliation can advance gaps with proposals to `verified` after promotion. See the [architecture guide](architecture.md#knowledge-maintenance-loop) for the full sequence.

MCP and `kfactory gaps report` store gaps and can start `TriageGapWorkflow` when `TEMPORAL_ADDRESS` is set. `kfactory curate` requires that variable to start a workflow and return its ID.

## 2. Prerequisites

- Go 1.27.4. The module language version is 1.26.0.
- PostgreSQL 18 for the factory database. `make test` and Compose use Postgres 18. Temporal's own database in Compose is Postgres 16.
- Docker, only for `make dev-up`.
- golangci-lint v2.15.0 for `make lint`.
- A separate GitHub repository for approved knowledge. The banking repository `dipjyotimetia/distributed-systems` is read-only evidence. The factory does not write to it.
- For a live curator: a GitHub App installed on the knowledge repository only, and Temporal reachable at `TEMPORAL_ADDRESS`.

Local binaries:

```bash
make build
# writes bin/factoryd, bin/factory-worker, and bin/kfactory
```

`factory-worker --check` prints the version and exits without dialing Temporal.

## 3. Knowledge repository setup

The publisher reads one repository: `workspace.knowledge_owner` and `workspace.knowledge_repo` in the YAML config (`examples/banking/config.yaml` is the sample). A `knowledge_repo` value of `owner/name` fills both fields when `knowledge_owner` is empty. `GITHUB_KNOWLEDGE_OWNER` and `GITHUB_KNOWLEDGE_REPO` override those fields at process start. They are not a second source the publisher consults on its own (`internal/publish/from_config.go`).

### Layout

Match `examples/banking/fixture-knowledge`:

- `index.md` with `okf_version: "0.3"` at the repository root.
- Concept files in directories such as `events/`. The banking fixture uses `events/movement-lifecycle.md`, not a `concepts/` directory.
- Agent Skills under `skills/<name>/SKILL.md`. The sample skill is `skills/movement-event-dedupe/SKILL.md`. `scripts/noop.sh` is inventoried and is never executed.

`kfactory validate` checks that tree:

```bash
go run ./cmd/kfactory validate examples/banking/fixture-knowledge
```

`workspace.knowledge_root` is the local checkout the worker materializes and indexes. `AKF_KNOWLEDGE_ROOT` overrides it. The banking config sets `knowledge_root: examples/banking/fixture-knowledge`.

### CODEOWNERS

Put humans in `CODEOWNERS` on the knowledge repository. Do not add the GitHub App.

```
* @your-org/knowledge-reviewers
```

### Ruleset

On `workspace.default_branch` (the field defaults to `main`; `AKF_DEFAULT_BRANCH` overrides it):

- Require a pull request before merging.
- Require a review from code owners.
- Dismiss stale reviews when new commits are pushed.
- Do not let the GitHub App bypass the ruleset.
- Do not grant the app a workflow that merges, and do not grant Administration.
- Leave auto-merge off for the app's pull requests if the repository enables auto-merge globally.

The factory opens the pull request with base set to that branch (`internal/publish/github.go`). A push webhook reconciles only `refs/heads/<that branch>`.

## 4. GitHub App

Create the app in the organization that owns the knowledge repository. Install it on that repository only.

Permissions:

- Contents: read and write.
- Pull requests: read and write.
- Metadata: read.

No Administration permission. The publisher refuses any URL ending in `/merge` or containing `/merges`.

Subscribe the app to:

- Pull requests. `closed` with `merged: true` signals a merge. `closed` without a merge signals rejection. `synchronize` asks the workflow to revalidate the head.
- Pushes. A push to `refs/heads/<default branch>` calls `Reconcile`.

Webhook:

- Payload URL: `https://<factory-host>/v1/github/webhook`
- Content type: JSON.
- Secret: the same value as `GITHUB_WEBHOOK_SECRET`.

The route checks `X-Hub-Signature-256` with HMAC-SHA256 (`internal/api/webhook.go`). An empty secret, a missing signature, or a mismatch returns 401. A missing `X-GitHub-Delivery` returns 400. A repeated delivery id returns `{"status":"duplicate"}` and does not signal again. The route is not behind the bearer-token middleware.

Private key: set `GITHUB_APP_ID`, `GITHUB_APP_INSTALLATION_ID`, and `GITHUB_APP_PRIVATE_KEY`. `GITHUB_APP_PRIVATE_KEY` is the PEM text, not a file path. `internal/publish/app_token.go` parses that string with `ParseRSAPrivateKeyFromPEM`. Keep it in the process environment or a secret manager. Do not commit it. If `GITHUB_APP_ID` is empty, the publisher uses `GITHUB_APP_TOKEN` instead, which is only for a local stand-in.

`GITHUB_API_URL` defaults to `https://api.github.com` when unset.

Evidence reads use `GITHUB_READONLY_TOKEN` when a source's `credential_ref` is `env:GITHUB_READONLY_TOKEN`. Public reads of a pinned SHA work without it. The banking source allowlist is `api.github.com` and `contracts/avro/` at pin `8e6680d683ac728f0f089bd60e4a96576ab4ed45`.

## 5. Postgres

The API role must not be a superuser and must not have `BYPASSRLS`. Compose creates it in `deployments/postgres-init/01-app-role.sql`:

```sql
CREATE ROLE akf_app LOGIN PASSWORD 'akf' NOSUPERUSER NOBYPASSRLS;
GRANT CONNECT ON DATABASE akf TO akf_app;
```

The password `akf` is a local default. Do not reuse it outside Compose. `make db-init` creates the same role against a local `akf` database.

Use two URLs:

- `AKF_INSTALLER_DATABASE_URL` runs migrations and `kfactory init`. `factoryd` also calls `GrantAppRole`, which grants `akf_app` DML on the operational tables and revokes `schema_migrations`.
- `AKF_DATABASE_URL` is the connection for HTTP, MCP search, the worker, and `kfactory` commands that read or write as the workspace. Point this at `akf_app`.

Migrations are the SQL files in `migrations/`, applied in order by `factoryd` when the installer URL is set, and by `kfactory init`. Tables in `migrations/001_init.sql` and `query_logs` in `migrations/003_operational.sql` use `ENABLE ROW LEVEL SECURITY` and `FORCE ROW LEVEL SECURITY`. Policies compare `workspace_id` (or `id` on `workspaces`) to `current_setting('akf.workspace_id', true)`. The request path sets that inside the transaction (`internal/storage/postgres/db.go`).

Check the role and the policies:

```sql
SELECT rolname, rolsuper, rolbypassrls
FROM pg_roles
WHERE rolname = 'akf_app';

SELECT c.relname, c.relrowsecurity, c.relforcerowsecurity
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'public' AND c.relkind = 'r'
ORDER BY c.relname;
```

`rolsuper` and `rolbypassrls` must be false. `relrowsecurity` and `relforcerowsecurity` must be true on the operational tables. A superuser still bypasses RLS, which is why the API must not use the installer role.

Create the workspace with the installer URL:

```bash
export AKF_INSTALLER_DATABASE_URL="postgres:///akf?host=/var/run/postgresql&sslmode=disable"
export AKF_CONFIG=examples/banking/config.yaml
go run ./cmd/kfactory init --workspace banking-demo
```

`init` writes `workspace_id`, `display_name`, `knowledge_repo`, `knowledge_root`, and `retention_days` from the config. It stores the repo name, not the owner.

## 6. Temporal

### Local

```bash
make dev-up
```

`deployments/compose.yaml` starts:

- Postgres 18 for the factory, published on port 5432.
- Postgres 16 for Temporal only (`temporal-postgres`).
- Temporal `temporalio/auto-setup:1.29.1` with `DB=postgres12`, `POSTGRES_SEEDS=temporal-postgres`, frontend on port 7233.
- Temporal UI on port 8088.
- `factoryd` on port 8080, with `TEMPORAL_ADDRESS=temporal:7233`.
- `factory-worker`, same Temporal address, entrypoint `/factory-worker`.

`factoryd-mcp` is not part of `make dev-up`. It is the attached MCP process. See section 8.

`make dev-down` stops the stack. `make dev-up-otel` starts the optional collector under the `otel` profile. The factory does not export traces in this tree.

From the host, outside Compose:

```bash
export TEMPORAL_ADDRESS=localhost:7233
export TEMPORAL_NAMESPACE=default
export TEMPORAL_TASK_QUEUE=knowledge-curator
```

An empty `TEMPORAL_NAMESPACE` becomes `default`. An empty task queue becomes `knowledge-curator`. Workflow id is `curate-<workspace_id>-<gap_id>` (`internal/workflows/start.go`). A second start of a running id is ignored.

If `TEMPORAL_ADDRESS` is unset, `factory-worker` logs that it is idle and waits for a signal. `factoryd` still serves HTTP and does not set the review signaler, so a push webhook is accepted and not reconciled.

### Production

Point `TEMPORAL_ADDRESS` at the frontend. Set `TEMPORAL_NAMESPACE` to the namespace you created.

Temporal Cloud API key:

```bash
export TEMPORAL_ADDRESS=<namespace>.tmprl.cloud:7233
export TEMPORAL_NAMESPACE=<namespace>
export TEMPORAL_API_KEY=<api-key>
```

`TEMPORAL_API_KEY` selects `NewAPIKeyStaticCredentials` (`internal/workflows/temporal.go`). The SDK turns on TLS for that credential.

mTLS, cert and key together:

```bash
export TEMPORAL_TLS_CERT=/path/to/client.pem
export TEMPORAL_TLS_KEY=/path/to/client.key
export TEMPORAL_TLS_CA=/path/to/ca.pem
export TEMPORAL_TLS_SERVER_NAME=<frontend-name>
```

`TEMPORAL_TLS_CERT` and `TEMPORAL_TLS_KEY` must both be set or both be empty. Those values are file paths. `TEMPORAL_TLS_CA` is a PEM file of CA certificates. `TEMPORAL_TLS_SERVER_NAME` is the TLS server name. `factoryd`, `kfactory`, and `factory-worker` share this dial.

Run one `factory-worker` (or more) against the same task queue, with `AKF_DATABASE_URL`, `AKF_CONFIG`, and the GitHub App environment set. The worker exits if `workspace.knowledge_owner` or `workspace.knowledge_repo` is empty.

## 7. Environment variables and config fields

Process environment overrides the YAML file passed with `-config`, `--config`, or `AKF_CONFIG`. `AKF_DEV` must be `1`, `true`, `yes`, or `on` before `dev_tokens` are loaded. Leave it unset outside a local demo.

### Process environment

| Variable | Meaning | Default | Example |
|---|---|---|---|
| `AKF_CONFIG` | YAML config path | empty, flags still work | `examples/banking/config.yaml` |
| `AKF_HTTP_ADDR` | HTTP listen address | `:8080`, or `http_addr` in YAML | `:8080` |
| `AKF_DEV` | Load `dev_tokens` from YAML | off | `1` only on a laptop |
| `AKF_DATABASE_URL` | API role DSN | `database_url` in YAML | `postgres://akf_app:akf@127.0.0.1:5432/akf?sslmode=disable` |
| `AKF_INSTALLER_DATABASE_URL` | Migration and `init` DSN | `installer_database_url` in YAML | `postgres:///akf?host=/var/run/postgresql&sslmode=disable` |
| `AKF_AGENT_TOKEN` | Bearer token when a token's `token_env` is this name | empty | a random secret |
| `AKF_CURATOR_TOKEN` | Same, for the curator sample entry | empty | a different random secret |
| `AKF_KNOWLEDGE_ROOT` | Overrides `workspace.knowledge_root` | the YAML value | `examples/banking/fixture-knowledge` |
| `AKF_DEFAULT_BRANCH` | Overrides `workspace.default_branch` | `main` | `main` |
| `GITHUB_KNOWLEDGE_OWNER` | Overrides `workspace.knowledge_owner` | parsed from `owner/repo` or the YAML owner | `dipjyotimetia` |
| `GITHUB_KNOWLEDGE_REPO` | Overrides `workspace.knowledge_repo` | the YAML repo name | `knowledge-factory-demo-knowledge` |
| `GITHUB_API_URL` | GitHub API base URL | `https://api.github.com` | `https://api.github.com` |
| `GITHUB_APP_ID` | GitHub App id. When set, the app mints installation tokens | empty | `123456` |
| `GITHUB_APP_INSTALLATION_ID` | Installation id for the knowledge repository | empty | `12345678` |
| `GITHUB_APP_PRIVATE_KEY` | PEM text of the app private key | empty | the PEM, including line breaks |
| `GITHUB_APP_TOKEN` | Used only when `GITHUB_APP_ID` is empty | empty | a local stand-in token |
| `GITHUB_WEBHOOK_SECRET` | HMAC key for `X-Hub-Signature-256` | empty, and then every webhook is rejected | a random secret |
| `GITHUB_READONLY_TOKEN` | Read token for a source with `credential_ref: env:GITHUB_READONLY_TOKEN` | empty | a fine-grained read token |
| `AKF_MODEL_BASE_URL` | OpenAI-compatible base URL, used only when the API key is set | `https://api.openai.com/v1` | `https://api.openai.com/v1` |
| `AKF_MODEL_API_KEY` | When set, the worker's advisor is `model.Gate` after the deterministic curator abstains | empty, deterministic curator only | a provider key |
| `AKF_MODEL_NAME` | Model name passed to that provider | empty | the model id your provider expects |
| `TEMPORAL_ADDRESS` | Frontend host:port | empty, and curation is not started | `localhost:7233` |
| `TEMPORAL_NAMESPACE` | Temporal namespace | `default` | `default` |
| `TEMPORAL_TASK_QUEUE` | Worker and start queue | `knowledge-curator` | `knowledge-curator` |
| `TEMPORAL_API_KEY` | Temporal Cloud API key | empty | a Cloud API key |
| `TEMPORAL_TLS_CERT` | Client certificate file. Requires `TEMPORAL_TLS_KEY` | empty | `/secrets/temporal-client.pem` |
| `TEMPORAL_TLS_KEY` | Client private key file | empty | `/secrets/temporal-client.key` |
| `TEMPORAL_TLS_CA` | CA bundle file | empty | `/secrets/temporal-ca.pem` |
| `TEMPORAL_TLS_SERVER_NAME` | TLS server name | empty | the frontend hostname |
| `AKF_TEST_ADMIN_DSN` | Installer DSN for `make test` only | local socket `postgres:///akf?host=/var/run/postgresql&sslmode=disable` | the CI Postgres 18 URL |
| `AKF_TEST_APP_DSN` | API-role DSN for tests | `postgres://akf_app:akf@127.0.0.1:5432/akf?sslmode=disable` | set only if the test role differs |

`RUN_GITHUB_SANDBOX` is a GitHub Actions repository variable, not a process environment variable. See section 10.

Leave `AKF_MODEL_API_KEY` empty to keep the deterministic curator. The banking demo does that. The binary does not choose a model name for you.

### YAML fields

File: path in `AKF_CONFIG`. Sample: `examples/banking/config.yaml`.

| Field | Meaning | Default |
|---|---|---|
| `http_addr` | Listen address if `AKF_HTTP_ADDR` is empty | `:8080` |
| `database_url` | API DSN if `AKF_DATABASE_URL` is empty | empty |
| `installer_database_url` | Installer DSN if `AKF_INSTALLER_DATABASE_URL` is empty | empty |
| `tokens[].token` | Inline bearer token. Prefer `token_env` outside tests | skipped when empty |
| `tokens[].token_env` | Environment variable that holds the token | empty |
| `tokens[].workspace_id` | Workspace the token can access | required to match the caller |
| `tokens[].role` | `agent`, `curator`, or `reviewer` | any other role fails config load |
| `dev_tokens[]` | Same shape. Loaded only when `AKF_DEV` is on | ignored otherwise |
| `workspace.workspace_id` | Workspace id | empty |
| `workspace.display_name` | Display name. `init` uses the id when this is empty | empty |
| `workspace.knowledge_owner` | GitHub owner | from `owner/repo` or `GITHUB_KNOWLEDGE_OWNER` |
| `workspace.knowledge_repo` | GitHub repo name, or `owner/repo` | `GITHUB_KNOWLEDGE_REPO` overrides it |
| `workspace.knowledge_root` | Local knowledge directory | `AKF_KNOWLEDGE_ROOT` overrides it |
| `workspace.default_branch` | Protected branch | `main` |
| `workspace.retention_days` | Passed to `kfactory init` and stored on the workspace | database column default is 30 |
| `sources[].id` | Source id | |
| `sources[].provider` | `github`, or empty. Anything else fails evidence load | |
| `sources[].owner`, `repo`, `pin_sha` | Pinned read | |
| `sources[].credential_ref` | `env:NAME` reads that variable | |
| `sources[].allow_hosts`, `allow_paths` | Fetch allowlist | |
| `sources[].paths` | Files fetched at the pin | |
| `sources[].classification` | Prefix to classification | |
| `curation.rules` | Rule names. A missing list uses `movement-dedupe-key` | that one rule |
| `curation.max_model_calls` | Advisor budget | 4 when unset or not positive |
| `curation.max_source_fetches` | Evidence fetch budget | 20 |
| `curation.max_tokens` | Token budget passed to the advisor | 8000 |
| `curation.cases` | Evaluation case file. `kfactory eval` uses this when `--cases` is empty | empty |

Banking sample budgets and cases: `examples/banking/config.yaml` sets `cases: examples/banking/tests/cases.json`.

## 8. Running locally, then a first real run

### Local loop with Compose

```bash
make dev-up
curl -s localhost:8080/healthz
curl -s localhost:8080/readyz
```

`/healthz` and `/readyz` are unauthenticated. `/readyz` fails until Postgres answers.

Compose sets `AKF_DEV=1` and mounts the repo at `/workspace`, with `AKF_CONFIG=/workspace/examples/banking/config.yaml`. The sample dev tokens in that file are `dev-agent-banking` and `dev-curator-banking`. They are refused when `AKF_DEV` is off.

Initialize once, from the host, against the published Postgres port:

```bash
export AKF_INSTALLER_DATABASE_URL="postgres://akf:akf@127.0.0.1:5432/akf?sslmode=disable"
export AKF_DATABASE_URL="postgres://akf_app:akf@127.0.0.1:5432/akf?sslmode=disable"
export AKF_CONFIG=examples/banking/config.yaml
go run ./cmd/kfactory init --workspace banking-demo
go run ./cmd/kfactory index --workspace banking-demo --sha fixture-sha
```

`index` defaults `--root` to `examples/banking/fixture-knowledge` when you omit it. Search then sees that SHA:

```bash
go run ./cmd/kfactory search --workspace banking-demo "dedupe movement"
# AKF_DEV=1 accepts the agent dev_tokens value from examples/banking/config.yaml.
# curl sends it as a bearer token. Do not use that value outside the local demo.
curl -s --oauth2-bearer "dev-agent-banking" \
  "http://127.0.0.1:8080/v1/search?q=dedupe%20movement"
```

Submit the sample gap. This stores the row. With Compose, `factoryd` has `TEMPORAL_ADDRESS` set, so an MCP report starts the workflow. `kfactory gaps report` starts it only when your shell has `TEMPORAL_ADDRESS` set.

```bash
export TEMPORAL_ADDRESS=localhost:7233
go run ./cmd/kfactory gaps report \
  --workspace banking-demo \
  --file schemas/examples/gap-report.json
go run ./cmd/kfactory curate --workspace banking-demo --gap <gap-id>
```

`curate` prints `started curate-banking-demo-<gap-id>` only after Temporal accepts the start. The Temporal UI is at `http://127.0.0.1:8088`.

A full learning cycle without a live GitHub repository is `make test`, which includes `go test ./internal/demo`. That test uses the in-memory forge and the Temporal test suite. `TestLiveTemporalLoop` runs only when `TEMPORAL_ADDRESS` is set. CI sets it. Locally:

```bash
export TEMPORAL_ADDRESS=localhost:7233
export AKF_TEST_ADMIN_DSN="postgres://akf:akf@127.0.0.1:5432/akf?sslmode=disable"
go test -race -count=1 -p 1 -run TestLiveTemporalLoop ./internal/demo/
```

### MCP client

MCP is stdio. The server name is `agent-knowledge-factory`. `factoryd -mcp` serves HTTP on `AKF_HTTP_ADDR` and MCP on stdin/stdout in one process. Do not run that next to Compose `factoryd` on the same port. Use Compose service `factoryd-mcp`, which listens on 8081:

```bash
docker compose -f deployments/compose.yaml run --rm -i --service-ports factoryd-mcp
```

Or from a built binary, with the HTTP port moved if 8080 is taken:

```bash
AKF_HTTP_ADDR=:8081 AKF_DEV=1 AKF_CONFIG=examples/banking/config.yaml \
  TEMPORAL_ADDRESS=localhost:7233 \
  ./bin/factoryd -mcp -config examples/banking/config.yaml
```

A client config that launches that process:

```json
{
  "mcpServers": {
    "agent-knowledge-factory": {
      "command": "./bin/factoryd",
      "args": ["-mcp", "-config", "examples/banking/config.yaml"],
      "env": {
        "AKF_DEV": "1",
        "AKF_DATABASE_URL": "postgres://akf_app:akf@127.0.0.1:5432/akf?sslmode=disable",
        "AKF_CONFIG": "examples/banking/config.yaml",
        "TEMPORAL_ADDRESS": "localhost:7233"
      }
    }
  }
}
```

Tools (`internal/mcpserver/server.go`):

| Tool | Arguments | What it does |
|---|---|---|
| `knowledge.search` | `query`, optional `type` (`concept` or `skill`), optional `limit`, optional `workspace_id` | Published search. `workspace_id` must match the process config when it is sent. |
| `knowledge.get` | `uri`, optional `revision`, optional `workspace_id` | One approved item. Empty `revision` is the published SHA. |
| `knowledge.report_gap` | a gap report object | Stores the gap. Does not change knowledge. Starts `TriageGapWorkflow` when Temporal is configured. |
| `knowledge.submit_feedback` | `knowledge_uri`, `revision`, `observation`, optional `agent_run_id`, optional `workspace_id` | Attaches an observation to a revision. |
| `knowledge.get_gap_status` | `gap_id`, optional `workspace_id` | Returns the gap record and `state`. |

Resource template: `kb://{authority}/{+path}`, name `knowledge`, MIME type `text/markdown`. The banking lifecycle URI is `kb://banking/events/movement-lifecycle`.

Gap report fields required by `ValidateReport`: `workspace_id`, `agent_run_id`, `task_kind`, `category`, `summary`, `observed`, `expected`, `client_event_id`. Categories: `missing-procedure`, `stale-instruction`, `bad-data-access`, `contradictory-sources`, `retrieval-failure`, `execution-failure`. A copyable body is `schemas/examples/gap-report.json`. The same `client_event_id` dedupes. A secret-shaped report is rejected.

There is no HTTP MCP listener. Streamable HTTP MCP is deferred.

### First real run

1. Create the knowledge repository with the layout in section 3, human `CODEOWNERS`, and the ruleset.
2. Create the GitHub App as in section 4. Set `GITHUB_APP_ID`, `GITHUB_APP_INSTALLATION_ID`, and `GITHUB_APP_PRIVATE_KEY` (the PEM text). Set `GITHUB_WEBHOOK_SECRET` to the webhook secret.
3. Set `workspace.knowledge_owner`, `workspace.knowledge_repo`, `workspace.default_branch`, and `workspace.knowledge_root` to that repository and a local clone.
4. Create `akf_app` as in section 5. Set `AKF_DATABASE_URL` to that role and `AKF_INSTALLER_DATABASE_URL` to a role that can migrate. Leave `AKF_DEV` unset. Set `AKF_AGENT_TOKEN` and `AKF_CURATOR_TOKEN`, and point `tokens[].token_env` at them.
5. Start Temporal (section 6) and `factory-worker` with the same `AKF_CONFIG`, `AKF_DATABASE_URL`, and GitHub variables.
6. Start `factoryd` without `-mcp` for the control API, or with `-mcp` when an agent client owns the process stdio.
7. `kfactory init`, then index the current default-branch SHA:

```bash
kfactory index --workspace <id> --sha <sha> --root <clone>
```

8. Point the GitHub App webhook at `https://<factory-host>/v1/github/webhook`.
9. Have an agent call `knowledge.report_gap`, or run `kfactory gaps report` and `kfactory curate --gap <id>`.
10. Review the pull request the worker opened. Merge it in GitHub. The webhook signals the workflow. `Publish` checks the default-branch head, stages and evaluates the index, and promotes it when the configured critical cases pass the regression gate. This path marks the gap `verified` when a targeted case improves.

`POST /v1/gaps/{id}/approval` with `{"action":"merged","merge_sha":"..."}` also signals the workflow. It does not merge. The activity still requires `DefaultHead` to equal that SHA.

## 9. Day-2 operations

### CLI

Config and workspace flags are `--config` and `--workspace` on every command that needs them. An empty `--config` uses `AKF_CONFIG`. An empty `--workspace` uses `workspace.workspace_id`.

| Command | Purpose |
|---|---|
| `kfactory version` | Print the version. |
| `kfactory init --workspace <id>` | Migrate with the installer DSN and insert the workspace. |
| `kfactory validate <path>` | Validate an OKF bundle and skills under it. |
| `kfactory search <query> --type concept\|skill --limit 10` | Search the published index. |
| `kfactory index --sha <sha> --root <dir>` | Rebuild the index from a directory and point the workspace at that SHA. |
| `kfactory gaps list` | List gaps. |
| `kfactory gaps show <id>` | One gap and linked submissions. |
| `kfactory gaps triage <id> --reason <text>` | Move a `new` gap to `triaged`. |
| `kfactory gaps report --file <json> --agent cli` | Store a report. Starts a workflow when `TEMPORAL_ADDRESS` is set. |
| `kfactory curate --gap <id>` | Start `TriageGapWorkflow`. Errors when `TEMPORAL_ADDRESS` is unset. |
| `kfactory eval --cases <file>` | Score cases against the published index. Defaults to `curation.cases`. |
| `kfactory serve --config <file>` | HTTP API. Wires webhook dedup and Temporal review when the database and `TEMPORAL_ADDRESS` are set. |
| `kfactory retain --workspace <id>` | Delete old query logs and source snapshots. Keeps audit rows. |
| `kfactory export --workspace <id>` | Write gap metadata JSON to stdout. Snapshot bodies are omitted. Refuses the payload if it looks like a secret. |
| `kfactory audit verify --workspace <id>` | Check the hash chain. Prints `audit chain ok`. |
| `kfactory rollback --sha <sha> --workspace <id>` | Point search at an already indexed SHA and append an audit event with action `rollback`. |

Gap states the service uses: `new`, `triaged`, `investigating`, `proposal-ready`, `awaiting-review`, `accepted`, `rejected`, `published`, `verified`, `duplicate`, `insufficient-evidence`, `cancelled`, `failed`.

### Control API

Bearer token: `Authorization: Bearer <token>`. The token's workspace is the only workspace the call can see.

| Route | Auth | Behavior |
|---|---|---|
| `GET /healthz` | no | Process is up. |
| `GET /readyz` | no | Database ping. |
| `POST /v1/github/webhook` | HMAC, not bearer | See section 4. |
| `GET /v1/workspace` | bearer | Workspace row, including `knowledge_repo` and `published_sha`. |
| `GET /v1/search?q=&type=&limit=` | bearer | Published search. `q` is required, at most 200 characters. A secret-shaped query is stored as `[redacted]` and returns no hits. |
| `GET /v1/knowledge?uri=&revision=` | bearer | One item. |
| `GET /v1/gaps` | bearer | `{"gaps":[...]}`. |
| `GET /v1/gaps/{id}` | bearer | One gap. |
| `POST /v1/gaps/{id}/approval` | bearer | Body `action` is `reject` or `merged`. `merged` also reads `merge_sha`. `reason` is optional. Signals the workflow. Does not merge. Returns 503 when Temporal was not configured. |
| `GET /v1/proposals` | bearer | Latest 50 proposals: id, decision, approval status, pull request URL. |
| `GET /v1/evaluations` | bearer | Latest 50 evaluation runs: id, phase, passed, failed, critical regressions. |

### Reconciliation and missed webhooks

Three paths notice a default branch that moved:

- The webhook, when `factoryd` or `kfactory serve` has `TEMPORAL_ADDRESS`, a database, and a configured knowledge repository. `Reconcile` reads `DefaultHead`, and if it differs from `published_sha`, fetches that tree and reindexes.
- `factory-worker`, every minute, the same `Reconcile`, then `EnforceRetention`.
- A `pull_request` `synchronize` event, which revalidates the open head instead of publishing it.

If owner or repo is missing, or the database was not passed in, `Reconcile` returns an error. The webhook then responds 502 with `reconcile failed` rather than pretending it ran. A delivery id that was already stored does not signal again. To force a known tree without GitHub, use `kfactory index --sha <sha> --root <clone>`.

### Audit chain

Each audit row stores `prev_hash`, `event_hash`, and `chain_seq` (`migrations/003_operational.sql`, `migrations/004_audit_seq.sql`). `kfactory audit verify` recomputes the hashes in chain order (`internal/audit/chain.go`). A deleted or rewritten row fails the command. Retention does not delete `audit_events`.

### Retention and export

`workspace.retention_days` is stored by `kfactory init`. The worker and `kfactory retain` delete `query_logs` and `source_snapshots` older than that many days, and the evidence links that pointed at those snapshots. They do not delete audit rows or knowledge items.

```bash
kfactory retain --workspace banking-demo
kfactory export --workspace banking-demo > gaps.json
```

`export` selects gap id, state, category, and summary. It does not write snapshot bodies.

### Rollback

`kfactory rollback --sha <sha>` only works for a SHA that already has `knowledge_items` rows. It clears `superseded_by` on that SHA, sets `workspaces.published_sha`, and appends an audit event. It does not delete the newer rows and it does not push to Git. Search joins on the published SHA, so the previous tree is what `knowledge.search` returns after a successful rollback. If the SHA was never indexed, the command returns `no indexed revision`.

Rolling back the index leaves the Git default-branch head in place. The next reconciliation can promote that head again. To make the change durable across reconciliation, a human reverts the change in the knowledge repository. The push webhook or worker then reconciles the new revert commit. Run `kfactory audit verify` after a rollback.

## 10. CI and release

Workflows: `.github/workflows/ci.yml`, `.github/workflows/security.yml`, `.github/workflows/release.yml`. Permissions on CI and security are `contents: read`.

| Job | Workflow | What it does |
|---|---|---|
| `test` | `ci.yml` | Postgres 18 service. golangci-lint v2.15.0, `go vet`, `go test -race -count=1 -p 1 ./...`. `AKF_TEST_ADMIN_DSN` is `postgres://akf:akf@127.0.0.1:5432/akf?sslmode=disable`. |
| `secrets` | `ci.yml` | gitleaks 8.30.0 `detect --redact --exit-code 1` on the full history. No allowlist file. |
| `sast` | `ci.yml` | gosec v2.30.0, `-fmt text -quiet ./...`. |
| `sbom` | `ci.yml` | `anchore/sbom-action` SPDX JSON, uploaded as an artifact. |
| `temporal` | `ci.yml` | Postgres 18 plus Temporal auto-setup 1.29.1 with `DB=postgres12`. Waits until port 7233 accepts a connection, then `go test -run TestLiveTemporalLoop ./internal/demo/`. |
| `github-sandbox` | `ci.yml` | Skipped unless the repository variable `RUN_GITHUB_SANDBOX` is `true`. |
| `govulncheck` | `security.yml` | `govulncheck ./...`. |

The sandbox job checks that these Actions secrets are non-empty: `GITHUB_APP_ID`, `GITHUB_APP_INSTALLATION_ID`, `GITHUB_APP_PRIVATE_KEY`, `GITHUB_WEBHOOK_SECRET`. It does not call GitHub. Turn it on with the repository variable `RUN_GITHUB_SANDBOX=true` and those four secrets.

Release runs on tags `v*`. It builds `dist/factoryd`, `dist/factory-worker`, and `dist/kfactory` with `CGO_ENABLED=0`, writes `go version -m` output beside each binary, and signs each blob:

```bash
cosign sign-blob --yes --bundle dist/factoryd.bundle dist/factoryd
```

The same form is used for `factory-worker` and `kfactory`. The job has `id-token: write` for keyless signing. Artifacts upload as `factory-binaries`, including the `.bundle` files. The CI SBOM is the SPDX artifact from the `sbom` job, not a file the release job attaches.

## 11. Security checklist

- `AKF_DEV` is unset in every shared environment. The dev tokens in `examples/banking/config.yaml` are not production credentials.
- `AKF_DATABASE_URL` uses `akf_app` or another role with `rolsuper = false` and `rolbypassrls = false`.
- `AKF_INSTALLER_DATABASE_URL` is not the DSN of the HTTP process.
- Bearer tokens are `agent`, `curator`, or `reviewer`, one workspace each, and are not in Git.
- The GitHub App is installed on the knowledge repository only, with Contents and Pull requests read/write and Metadata read. It is not in `CODEOWNERS` and cannot bypass the ruleset.
- `GITHUB_APP_PRIVATE_KEY` and `GITHUB_WEBHOOK_SECRET` are secret-manager values. The webhook URL is `/v1/github/webhook`.
- `GITHUB_READONLY_TOKEN` cannot write, and the source allowlist stays on `allow_hosts` and `allow_paths`.
- Skill scripts are not executed. The sample `scripts/noop.sh` stays an inventory entry.
- Search does not return draft, deprecated, or superseded documents, and it does not return a SHA other than `published_sha` unless `knowledge.get` is asked for a revision.
- A secret-shaped search query is redacted. `kfactory export` refuses a secret-shaped payload.
- `kfactory audit verify` succeeds after deploys and after rollback.
- Release tags are the cosign-signed blobs from the `v*` workflow. Verify the `.bundle` before you run a downloaded binary.

## 12. Troubleshooting

| Symptom | What it means |
|---|---|
| `kfactory curate` exits with `TEMPORAL_ADDRESS is unset` | The shell has no frontend address. The gap row, if you used `gaps report`, is still stored. Export the variable and run `curate` again. |
| Worker log `TEMPORAL_ADDRESS is unset; worker is idle` | The worker process will not poll. Set the variable and restart it. |
| Worker log `set workspace.knowledge_owner and workspace.knowledge_repo` | Config has no owner/repo and the override variables are empty. The worker exits. |
| Webhook 401 `signature rejected` | `GITHUB_WEBHOOK_SECRET` is empty or differs from the app secret, or `X-Hub-Signature-256` does not match the raw body. |
| Webhook 400 `delivery id is required` | `X-GitHub-Delivery` was missing. |
| Webhook `{"status":"duplicate"}` | That delivery id is already in `webhook_deliveries`. GitHub retries are ignored on purpose. |
| Webhook `{"status":"ignored"}` | The event was not a curator pull request (`curator/<gap-id>`), or it was not `closed` or `synchronize`. |
| Webhook 502 `reconcile failed` | Push reached the default branch, but owner/repo, the database, or `DefaultHead` failed. Read the process log. |
| Webhook 502 `signal failed` | Temporal did not accept the review signal. Check `TEMPORAL_ADDRESS` and that the workflow id `curate-<workspace>-<gap>` exists. |
| `POST /v1/gaps/{id}/approval` returns 503 | `TEMPORAL_ADDRESS` was unset when the process started, so no review signaler was wired. |
| Search is empty after a merge | `published_sha` did not move. Check that the merge commit is the default-branch head, then `kfactory index` or wait for the worker minute tick. |
| `kfactory rollback` says `no indexed revision` | That SHA has no `knowledge_items` rows. Index it first, or pick a SHA that was published before. |
| `kfactory audit verify` fails | A row was deleted or rewritten. Retention does not do this. Investigate `audit_events` before continuing. |
| `kfactory export` refuses the payload | A gap field matched the secret redactor. Fix the stored summary before exporting. |
| `readyz` fails | `AKF_DATABASE_URL` is down or pointed at the wrong database. |
| RLS looks unused | The connecting role is a superuser or has `BYPASSRLS`. Use `akf_app`. |
| Port 8080 already in use | Compose `factoryd` binds 8080. `factoryd -mcp` also binds `AKF_HTTP_ADDR`. Use `factoryd-mcp` on 8081 or change `AKF_HTTP_ADDR`. |
| Temporal container exits on startup | `DB` must be `postgres12` or `postgres12_pgx` for image `temporalio/auto-setup:1.29.1`. `sqlite` is not a valid driver. Compose already sets `postgres12`. |
| `github-sandbox` skipped | `RUN_GITHUB_SANDBOX` is not `true`. That skip is expected. |

## 13. Known limitations and deferred work

Shipped here: stdio MCP, the control API, Postgres full text, GitHub plus read-only Notion and Slack sources, a deterministic curator plus an optional OpenAI-compatible advisor, pull requests without merge, evaluation gates, hash-chained audit, retention, export, rollback of an indexed SHA, Temporal Cloud API-key or mTLS dialing, and KF-11 through KF-21 (`specs/`).

`deployments/otel-collector.yaml` is optional. `make dev-up-otel` only starts the collector. Trace intake reads a JSON file named by `trace_ref`. Search stays full text unless `SEARCH_MODE=hybrid`. `docs/sql/pgvector.sql` is not applied by the migrations.

Still deferred, from `docs/plan-status.md`:

- Streamable HTTP MCP.
- Kubernetes manifests.
- A graph database, and a required pgvector install.
- Model training.
- Billing.

## 14. Context versus knowledge

Knowledge is the reviewed Git tree: OKF concepts, Agent Skills, and data-access pages. Search returns that published revision only. An open pull request is not in the default index.

Evidence is a pinned, allowlisted snapshot. GitHub files, a Notion page, or a Slack message can be evidence. Notion and Slack bodies are wrapped as untrusted text and stored as `secondary`, so they cannot by themselves ground a claim. The page that tells an agent where to query movement events is `examples/banking/fixture-knowledge/events/movement-query.md`. It names the source and says not to copy rows. Fast-changing rows stay in the source system.

`trace_ref` may point at a JSON file. `internal/traces` reads that file, or a list of `gen_ai.prompt`, `gen_ai.completion`, and `gen_ai.human` attributes, and redacts secrets and simple emails and phone numbers before Prepare adds the human turns to the curator feedback.

`CURATION_INTERVAL` defaults to 24h. `SEARCH_MODE` defaults to full text; `hybrid` reranks those hits locally. `docs/sql/pgvector.sql` is not applied by `migrations/`.

Rollback does not revert Git. The sandbox CI job does not perform a live GitHub call. Live Notion, Slack, and pgvector are opt-in.
