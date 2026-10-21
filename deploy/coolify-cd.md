# Coolify CD runbook

Goal: GitHub Actions builds and publishes the Docker image, Coolify does the redeploy. Dev works
automatically from the `dev` branch; prod is rolled out only manually by release tag.

## Current setup

- Coolify UI: `https://coolify.example.com` (`302` to `/login`).
- MCP endpoint for AI agents: `https://coolify.example.com/mcp`.
- Coolify on the server listens only locally: `127.0.0.1:8000`.
- Coolify realtime listens only on loopback `127.0.0.1:6001-6002`.
- Public access is provided by host nginx through the host nginx vhost `coolify.example.com`: `/` -> `8000`,
  `/app/` -> `6001`, `/terminal/ws` -> `6002`.
- Runtime images in Coolify: `ghcr.io/daniil4545/transcrib:sha-<git-sha>`.
  Moving tags `dev` and `prod` are published but not used as the active `APP_IMAGE`.
- The Coolify Service stores Compose inside the resource and does not read it from the repository on
  every deploy.
- The repository file `src/docker-compose.coolify.yml` is the single source of truth for both
  environments: before deploy the workflow writes the exact content of this file into the Service.
- Editing Compose by hand in the Coolify UI is not allowed: the edit lives only until the next deploy
  and takes the environment down on the next restart (incident 2026-08-12).
- `exclude_from_hc` in migrate is a Coolify extension for a one-shot service; plain
  `docker compose config` rejects it unless this line is removed.
- Dev resource UUID: `<dev-resource-uuid>`.
- Prod resource UUID: `<prod-resource-uuid>`.

Why: GitHub Actions provides a test gate and an immutable image before deploy, Coolify is
responsible only for pull/redeploy. This is easier to control than a direct auto-deploy from Git.

## Coolify MCP for the AI agent

Coolify MCP is enabled as a read-only source of infrastructure truth for Codex: a list of projects,
servers, services, applications, databases, and the details of a specific resource. It is useful
before diagnostics and deploy analysis, because the agent can quickly see the Coolify model without
SSH and without reading server `.env` files.

Boundary: the current Coolify MCP is read-only. It does not replace the GitHub Actions deploy, the
Coolify webhook, `deploy/safe-ssh.sh`, and manual entry of secrets. For the actual deploy, rollback,
env changes, container logs, and Postgres health, keep using the runbook below.

Recommended order for the DevOps agent:

1. MCP `get_infrastructure_overview` - understand the current Coolify resources.
2. MCP `list_services`/`get_service` - check `transcrib-dev` or `transcrib-prod`.
3. GitHub Actions/Coolify webhook - only for deploy through the regular workflow.
4. `deploy/safe-ssh.sh coolify-*-health` and `coolify-*-logs` - confirm the actual runtime.

Codex MCP setup (do not commit the token). In a regular terminal:

```sh
codex mcp add coolify --url https://coolify.example.com/mcp --bearer-token-env-var COOLIFY_MCP_TOKEN
```

Equivalent TOML fragment:

```toml
[mcp_servers.coolify]
url = "https://coolify.example.com/mcp"
bearer_token_env_var = "COOLIFY_MCP_TOKEN"
enabled = true
```

Create the token in the Coolify UI: `Security` -> `API Tokens`, permission `read`; better to limit
the expiry. In the shell that launches Codex:

```sh
export COOLIFY_MCP_TOKEN='<token>'
codex mcp list
```

The project-local Codex config also needs a network allowlist entry for `coolify.example.com` if the
profile restricts domains.

## GitHub

Workflow file:

- `.github/workflows/deploy.yml` - checks, build/push GHCR image, dev Compose sync,
  PATCH `APP_IMAGE`, one Coolify webhook call.

Repository secrets:

```text
COOLIFY_TOKEN
COOLIFY_WEBHOOK_DEV
```

Status 2026-07-01: repo secrets are configured. `COOLIFY_TOKEN` is a Coolify API token with
`read`, `write`, and `deploy` abilities, because self-hosted Coolify keeps `/api/v1/deploy` behind
the global API gate. Coolify API is enabled and `GET /api/v1/version` returns `200`.

GitHub Environment:

```text
production
```

Environment secrets `production`:

```text
COOLIFY_WEBHOOK_PROD
COOLIFY_SERVICE_UUID_PROD
```

Status 2026-07-03: `production` exists; `COOLIFY_WEBHOOK_PROD` and
`COOLIFY_SERVICE_UUID_PROD=<prod-resource-uuid>` are configured. Current deploy branch policies:
branch `main`, tag pattern `0.*` and legacy explicit tag `0.0.1`.

Dev deploy starts automatically from `dev`. Prod deploy does not start on a push to `main`: the owner
starts it manually through `workflow_dispatch` with `target=prod`, choosing a release tag as the ref.
GitHub required reviewers are unavailable on the current billing plan; the current working gate is a
manual workflow run on a tag. The prod webhook lives only in the `production` environment; the shared
`COOLIFY_TOKEN` lives in repo secrets.

After the local check, the dev workflow performs the operations strictly in this order:

1. Encodes the exact content of `src/docker-compose.coolify.yml` into base64 and PATCHes the Service
   with the fields `docker_compose_raw` and `instant_deploy=false`.
2. Updates `APP_IMAGE` through `PATCH /api/v1/services/{uuid}/envs` to
   `ghcr.io/daniil4545/transcrib:sha-${GITHUB_SHA}`.
3. Calls the deploy webhook exactly once.

`instant_deploy=false` is mandatory: the Compose PATCH only updates the configuration, and the single
webhook starts the deploy with a consistent Compose and image. The floating `dev` tag may stay in the
local Docker cache of Coolify, so the active `APP_IMAGE` always uses the full SHA tag.

This compose sync applies to dev only. The prod workflow so far updates only `APP_IMAGE` and calls
its own webhook; do not sync `transcrib-prod` with the repository Compose until the running dev
environment is successfully verified.

If the `COOLIFY_*` secrets are empty or `APP_IMAGE` was not updated, the deploy job must fail. A
successful image publish without a Coolify redeploy does not count as a successful CD.

## Coolify

Coolify structure:

```text
Project: transcrib
  Environment: dev
    Service: transcrib-dev
  Environment: prod
    Service: transcrib-prod
```

Do not put `transcrib-dev` into the `production` environment: an environment in Coolify is the
boundary of settings, secrets, volumes, and webhook/resource lifecycle.

Create two Coolify Services:

| Service | Deploy ref | Compose inside the Service | Image |
| --- | --- | --- | --- |
| `transcrib-dev` | `dev` | Synced from the exact `src/docker-compose.coolify.yml` before deploy | `ghcr.io/daniil4545/transcrib:sha-<git-sha>` |
| `transcrib-prod` | release tag from `main` | Synced from the exact `src/docker-compose.coolify.yml` before deploy | `ghcr.io/daniil4545/transcrib:sha-<git-sha>` |

In each Service set the initial `APP_IMAGE` manually; after that the workflow updates it before deploy:

```text
dev:  ghcr.io/daniil4545/transcrib:sha-<current-dev-sha>
prod: ghcr.io/daniil4545/transcrib:sha-<current-main-sha>
```

Take the remaining env variables from `src/.env.example` and enter them manually in the Coolify UI.
Do not commit secrets or read them through SSH.

OpenRouter proxy policy:

- Durable config: `OPENROUTER_PROXY_URL=http://10.0.0.1:20171` in Coolify-managed env; code support
  exists in Git `main` since `0.0.3.5`, but live prod `0.0.3.4` has not deployed it yet.
- Emergency-only config before the next deploy: global `HTTP_PROXY` / `HTTPS_PROXY` can be
  set in service `.env` together with broad `NO_PROXY`, but Coolify may regenerate this file on the
  next deploy. Replace the hotfix through managed env as soon as possible.
- Do not route Telegram, Speech2Text or YooKassa through the proxy intentionally unless a separate
  incident requires it.

Status 2026-07-01: Coolify project `transcrib` has environments `dev` and `prod`.
`transcrib-dev` exists in `dev` as Coolify Service `<dev-resource-uuid>`; env was imported
into Coolify-managed `environment_variables`, not only copied to
`/data/coolify/services/.../.env`, because Coolify regenerates that file on deploy.
`DATABASE_URL` is set to the internal service DNS `postgres:5432`. Dev DB was migrated from legacy
`transcrib-dev-postgres-1` into Coolify Postgres on 2026-07-01; source and before-restore dumps are
in `/var/backups/transcrib/coolify-migration/`.

`transcrib-prod` exists in `prod` as Coolify Service `<prod-resource-uuid>`. Current prod rollout
uses `APP_IMAGE=ghcr.io/daniil4545/transcrib:sha-8940569680b43af397b59f3e9df51276f3989b02`.
Prod env was imported into Coolify-managed `environment_variables`; `DATABASE_URL` points to the
internal `postgres:5432`. Live payment links and bank-card recurring were accepted on 2026-07-09:
`RECURRING_PAYMENTS_ENABLED=true`, prod redeploy started `recurring_worker_started`, payment-link
smoke activated a `900/900` subscription, and self-service disable cleared the saved method.
Monitoring of the first real recurring charge is still open.

GHCR pull access:

- GitHub Actions publishes with `GITHUB_TOKEN`.
- Server Docker must be able to pull the private package:
  `docker pull ghcr.io/daniil4545/transcrib:sha-<git-sha>`.
- Current working setup: root Docker login to `ghcr.io` as `daniil4545` with a classic PAT that has
  package read access. Do not commit or print this token.
- Minimal token scope: `read:packages`. If GHCR returns `403` for a private repo-linked package,
  recreate the classic PAT with `read:packages` + `repo`, then run:

```sh
ssh -t root@203.0.113.10 'docker login ghcr.io -u daniil4545'
```

Validation:

```sh
gh workflow run deploy.yml --ref dev -f target=dev
gh run watch <run-id> --exit-status
deploy/verify-deploy.sh dev <full-dev-git-sha>
```

A green workflow confirms only that CI/CD ran. Right after it, the operator or agent must run
`verify-deploy.sh` with the full SHA; without a `PASS` result the dev deploy is not accepted.

Expected dev state:

- `verify-deploy.sh` returns `PASS`;
- app and Postgres are in the `running/healthy` state, without restarts and OOM;
- the app image and OCI revision exactly match the full expected GitHub SHA;
- the current app run has `startup_complete` and no fatal/panic, `409 Conflict`,
  config or DB errors;
- a separate DB check passes and reads `goose_version`;
- the available migrate container has the same revision and the `Exited (0)` state;
- the legacy container `transcrib-dev-app-1` is stopped, so there is only one dev bot with long
  polling.

Coolify may remove the one-shot migrate container. In that case the command returns
`INCONCLUSIVE` (`4`), because the DB/goose state does not prove the exit code of this particular
deploy; the result of the deployment task in Coolify must be checked separately. A false `PASS` is
never returned.

Prod check:

```sh
# Only before the deploy:
deploy/safe-ssh.sh coolify-prod-active-jobs
deploy/safe-ssh.sh coolify-prod-app-logs 300
gh workflow run deploy.yml --ref <release-tag> -f target=prod
gh run watch <run-id> --exit-status
deploy/verify-deploy.sh prod <full-release-git-sha>
```

`coolify-prod-active-jobs` is a hard pre-deploy stop-gate: it must print `active_jobs_count=0`
and exit successfully before `Deploy target=prod` is started. Any active
`created|joining|recording|processing|transcribed|summarizing|awaiting_reauth` job or current-call
evidence in app logs postpones prod deploy.

Expected prod state:

- `verify-deploy.sh` returns `PASS`;
- app and Postgres are in the `running/healthy` state, without restarts and OOM;
- the app image and OCI revision exactly match the full commit of the release tag;
- the current app run has `startup_complete` and no fatal/panic, `409 Conflict`,
  config or DB errors;
- a separate DB check passes and reads `goose_version`;
- `active_jobs_count=0` was confirmed before the deploy;

The current Coolify prod may not keep a visible migrate container after the deploy. The prod check
does not require this container and checks the DB/goose state separately. `coolify-prod-active-jobs`
stays only a stop gate before the deploy; after the deploy this command is not run as a gate.

`verify-deploy.sh` must be run by the operator or agent after `gh run watch`; GitHub Actions does not
call it automatically. The script uses only the fixed commands `coolify-*-deploy-status`,
`coolify-*-current-run-evidence`, and `coolify-*-db-status` from `safe-ssh.sh`, and does not read
Coolify or container variables. The initial and final snapshots must contain the same app/Postgres
container IDs and `StartedAt`; the final snapshot repeats all health, revision, and runtime checks.

## Database model

Each Coolify environment owns its service and Postgres volume. Dev and prod must never share a
database, volume or `BOT_TOKEN`.

Dev current state:

- old source DB: legacy `transcrib-dev-postgres-1`, still preserved;
- active DB: Coolify `postgres-<dev-resource-uuid>`;
- active app: Coolify `app-<dev-resource-uuid>`;
- legacy `transcrib-dev-app-1`: stopped.

Prod current state:

- old source DB: legacy `transcrib-postgres-1`, still preserved as rollback source;
- active DB: Coolify `postgres-<prod-resource-uuid>`;
- active app: Coolify `app-<prod-resource-uuid>`;
- legacy `transcrib-app-1`: stopped.

Prod DB migration evidence 2026-07-01:

- fresh legacy dump: `/var/backups/transcrib/postgres/transcrib_20260701T135323Z.sql.gz`;
- before app deploy, source and target counts matched: `goose_version=8`, `users=6`, `jobs=22`,
  `payments=0`, `subscriptions=9`;
- after Coolify deploy, migrate reached `goose_version=10`;
- legacy DB volume was not modified and remains rollback source.

Repeat prod cutover checklist:

1. Create or verify `transcrib-prod` inside Coolify environment `prod`.
2. Set `APP_IMAGE=ghcr.io/daniil4545/transcrib:sha-<verified_sha>`.
3. Import prod env manually into Coolify-managed variables.
4. Make a fresh prod backup and restore it into Coolify Postgres.
5. Compare source/target counts before app cutover.
6. Create release tag from the verified `main` SHA.
7. Deploy prod manually through GitHub `Deploy` workflow on that tag with `target=prod`.
8. Check `coolify-prod-health`, logs and bot manually before considering legacy prod fallback removable.

## Logs

See [`logs.md`](logs.md). Important current fact: Coolify prod app/Postgres use `journald` logging
and satisfy the 14-day archive gate through host journald retention. Coolify dev containers use
Docker `json-file` logging on 2026-07-01, not host journald. `coolify-dev-*-logs` and Coolify UI work
for live/current logs, but this is not the 14-day journald archive.

## Server setup

Status: done on 2026-07-01, Let's Encrypt certificate valid until `2026-09-29`. Reproducible procedure:

```sh
mkdir -p /var/www/coolify.example.com/.well-known/acme-challenge
# install the nginx vhost coolify.example.com into sites-available/sites-enabled
nginx -t
certbot certonly --webroot -w /var/www/coolify.example.com -d coolify.example.com
nginx -t && nginx -s reload
```

Checks:

```sh
curl -I https://coolify.example.com/
curl --http1.1 -i --max-time 10 \
  -H 'Connection: Upgrade' \
  -H 'Upgrade: websocket' \
  -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' \
  -H 'Sec-WebSocket-Version: 13' \
  https://coolify.example.com/terminal/ws
deploy/safe-ssh.sh coolify-health
deploy/safe-ssh.sh coolify-public
deploy/safe-ssh.sh nginx-test
```

## Cutover

1. First set up and verify `transcrib-dev` in Coolify.
2. Make sure a push to `dev` passes CI, publishes the image, and starts the Coolify deploy.
3. Check the test bot: `startup_complete`, no `409 Conflict`, Postgres healthy.
4. For prod, before the first Coolify deploy, make a fresh backup and restore it into Coolify Postgres.
5. Switch prod only through a release tag and the manual `Deploy` workflow with `target=prod`.
6. Keep the old `git push prod <tag-or-sha>:master` as an emergency fallback until 2-3 successful prod deploys through Coolify.

## Rollback

Quick rollback in Coolify:

1. Set `APP_IMAGE=ghcr.io/daniil4545/transcrib:sha-<previous_sha>`.
2. Redeploy the app.
3. Check `startup_complete` and health.

Do not roll back data with `goose down`. If the release included a migration, use a forward fix or
a restore from backup by a separate decision.
