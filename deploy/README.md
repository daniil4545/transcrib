# Deployment and infrastructure of transcrib

Main infrastructure runbook. Start here to find out what is deployed where, how to roll out
prod/test, how to check the state, or where to look for a narrow operation.

## Quick map

| What | Where |
| --- | --- |
| Server | `203.0.113.10`, shared team VPS |
| Prod | Coolify service `transcrib-prod`, resource UUID `<prod-resource-uuid>`, image `ghcr.io/daniil4545/transcrib:sha-<git-sha>` |
| Dev bot | Coolify service `transcrib-dev`, resource UUID `<dev-resource-uuid>`, image `ghcr.io/daniil4545/transcrib:sha-<git-sha>` |
| Prod env | Coolify-managed environment variables |
| Dev env | Coolify-managed environment variables |
| Site | `https://transcrib.example.com`, host nginx, docroot `/var/www/transcrib.example.com` |
| Analytics | `https://analytics.example.com/dashboard/2`, Metabase on `127.0.0.1:3001`; visible dashboard is PROD, DEV kept as archive/admin fallback |
| Coolify CD | `https://coolify.example.com`, host nginx -> `127.0.0.1:8000` + loopback websocket `6001/6002`; Coolify `APP_URL` must match |
| Coolify MCP | `https://coolify.example.com/mcp`, read-only AI view via API token |
| Logs | [`logs.md`](logs.md); Dozzle through an SSH tunnel, live viewer only |
| Backup | prod `pg_dump`, temporarily on-server `/var/backups/transcrib/postgres` |

Secrets are not stored in git. The AI agent does not read `.env`, `docker inspect env`,
`docker exec printenv`, private keys, or dumps.

## Where to look

| Task | Document |
| --- | --- |
| Roll out prod/test | this file |
| Check or restore a DB backup | [`backup.md`](backup.md) |
| Read and check logs | [`logs.md`](logs.md) |
| Maintain Dozzle | [`dozzle/README.md`](dozzle/README.md) |
| Maintain the Metabase dashboard | [`../docs/analytics-dashboard.md`](../docs/analytics-dashboard.md) |
| Set up GitHub Actions + Coolify CD | [`coolify-cd.md`](coolify-cd.md) |
| Understand devops decisions and hardening priorities | [`../docs/devops-practices.md`](../docs/devops-practices.md) |
| History of server decisions and hardware | [`../docs/deployment.md`](../docs/deployment.md) |

## Analytics dashboard

Metabase lives separately from the bot deploy and does not change the product schema/runtime:

- compose project `transcrib-analytics-dev`;
- host bind `127.0.0.1:3001`, external access only through nginx `https://analytics.example.com`;
- visible dashboard is PROD aggregates on the existing `analytics.example.com` domain;
- DEV dashboard is kept only as an admin/archive fallback;
- PROD datasource uses Coolify prod DB through a column-limited read-only role;
- SQL/cards and the data boundary are described in [`../docs/analytics-dashboard.md`](../docs/analytics-dashboard.md).

Metabase was temporarily connected to the prod Docker network
`<prod-resource-uuid>` without restarting bot/Postgres. Durable follow-up: add that network as
an external analytics-compose network and recreate only Metabase. Also rotate the temporary
Metabase/admin and `metabase_prod_ro` passwords. Rolling back the dashboard layer does not require a bot rollback:
switch `/dashboard/2` back to the DEV archive or hide/remove the PROD datasource in Metabase.

## How deployment works

### Target CD through Coolify

The current dev CD is described in [`coolify-cd.md`](coolify-cd.md): the CI workflow and the Deploy
workflow run DB-backed Go checks with migrations; the Deploy workflow does not build the image if
these checks fail. After the checks it builds the image `ghcr.io/daniil4545/transcrib:sha-<sha>`,
then, strictly in sequence:

1. PATCHes the Coolify Service with the exact `src/docker-compose.coolify.yml` as base64
   `docker_compose_raw` with `instant_deploy=false`.
2. PATCHes `APP_IMAGE` to the full SHA tag.
3. Calls the Coolify deploy webhook exactly once.

The Coolify Service stores the Compose file inside. The repository file `src/docker-compose.coolify.yml`
is the single source of truth for both environments: the workflow syncs it before deploy into both
`transcrib-dev` and `transcrib-prod`. Editing Compose by hand in the Coolify UI is not allowed: the
edit lives only until the next deploy and breaks the environment on the next restart (incident
2026-08-12). `dev` is rolled out automatically from the `dev` branch; prod is rolled out only
manually by release tag through `workflow_dispatch` with `target=prod`. A push to `main` does not
roll out prod. The `production` environment stores the prod secrets; required reviewers can be
enabled only if the GitHub plan allows it.

Mandatory check of the running environment after the workflow finishes:

```sh
deploy/verify-deploy.sh <dev|prod> <full-40-char-git-sha>
```

It calls only the allowed commands from `safe-ssh.sh`, does not read environment variables or
secrets, and checks two Docker snapshots linked by container ID: the exact image SHA and OCI
revision, health state, restarts, OOM, start time, `startup_complete`, forbidden errors over the
whole current run, and a separate DB/goose check. A Docker/inspect/logs/exec error counts as an
operational error, not as a runtime check result. Codes: `0` PASS, `1` runtime FAIL, `2` invalid
input, `3` operational error, `4` INCONCLUSIVE. GitHub Actions does not call this script
automatically yet: an operator or an agent runs it right after the workflow. Without `PASS` the
deploy is not accepted.

Coolify MCP (`https://coolify.example.com/mcp`) can be connected to Codex as a read-only source of
information about Coolify resources. It helps the agent quickly understand projects/services/databases
before diagnostics, but deploy, rollback, env changes, and runtime checks stay with GitHub Actions,
the Coolify webhook, and `deploy/safe-ssh.sh`. Setup and the token are described in
[`coolify-cd.md`](coolify-cd.md).

Coolify Telegram notifications need two infra settings on Coolify itself: an `extra_hosts` pin
`api.telegram.org:149.154.167.220` and `APP_URL=https://coolify.example.com`. Otherwise Telegram may
time out or get an inline button URL like `http://localhost`. Coolify Monitoring/Sentinel needs an
internal bind `10.0.0.1:8000:8080`: `coolify-sentinel` reaches Coolify through
`host.docker.internal:8000`, while the public UI stays behind host nginx.
Telegram notification settings are enabled on the team/project `transcrib` and cover both Coolify
services: `transcrib-dev` and `transcrib-prod`.
Coolify realtime stays closed on loopback `127.0.0.1:6001-6002`; the host nginx vhost
`coolify.example.com` proxies the websocket routes `/app/` and `/terminal/ws`.

Current prod rollout: tag `0.0.3.4` -> `4804e9e6162238f663bf0789eacff9bcc087ce95`,
active resource `transcrib-prod` (`<prod-resource-uuid>`).

The legacy deploy through the bare repo `/srv/git/transcrib.git` and `post-receive` is removed: the
artifacts are deleted from the repository, the git remote `prod` is gone. The only rollout path is
Coolify CD; the order and gates are in this file and [`coolify-cd.md`](coolify-cd.md).
The hook history is available in git up to the tag `0.0.3.7`.

One compose file [`../src/docker-compose.prod.yml`](../src/docker-compose.prod.yml) serves both
environments. Different `-p` values give different networks, containers, and volumes.

## Prod deploy

Working order: a feature branch is merged into `dev`, GitHub Actions automatically rolls out the
Coolify dev bot, the owner checks `@transcrib_dev_bot`, then a release PR `dev -> main` is prepared.
Merge into `main` does not roll out prod. For prod the owner gives a separate command, a release tag
is created, and the `Deploy` workflow is started manually on that tag with `target=prod`. Only a
stable version must run on prod.

Hard stop gate before starting a prod deploy: there must be no active calls on prod. Before
`gh workflow run ... -f target=prod` you must run:

```sh
deploy/safe-ssh.sh coolify-prod-active-jobs
deploy/safe-ssh.sh coolify-prod-app-logs 300
```

`coolify-prod-active-jobs` must show `active_jobs_count=0` and exit with code `0`.
The active states are `created`, `joining`, `recording`, `processing`, `transcribed`, `summarizing`,
`awaiting_reauth`. If the command returned a non-zero code, or the app logs show a current call,
provider polling/processing of a current meeting, a restart loop, `409 Conflict`, config/db errors,
or suspicious activity by `job_id`, the prod deploy is postponed until the calls finish and a repeated
clean check. No exceptions "by feel".

Before a prod deploy, the local checks relevant to the change must be green. The base set for Go:

```sh
cd src
NO_DOTENV=1 DATABASE_URL=postgres://transcrib:transcrib@localhost:5432/transcrib?sslmode=disable make ci-check
```

`go test ./...` without `DATABASE_URL` is not a full gate: DB-backed tests are skipped.
`NO_DOTENV=1` keeps the AI agent from reading local `.env` files with secrets; a regular local run
may use `.env`.

Deploy:

```sh
gh workflow run deploy.yml --ref <release-tag> -f target=prod
gh run watch <run-id> --exit-status
```

Check:

```sh
deploy/verify-deploy.sh prod <full-release-git-sha>
```

Expected:

- result `PASS`, app and Postgres `running/healthy`;
- app image and `app_revision` exactly match the full release tag SHA;
- restart count is `0`, OOM flag is `false`, start time is correct;
- the logs of the current run contain `startup_complete` and, over the whole run, no fatal/panic,
  `409 Conflict`, config/db errors;
- a separate DB probe answers and reads a positive `goose_version`;
- `active_jobs_count=0` was confirmed before the prod deploy started;

On the current Coolify prod, the migrate container may no longer be available in Docker after a
successful deploy. The prod post-deploy contract checks DB/goose but does not require a preserved
one-shot container. `coolify-prod-active-jobs` stays only a pre-deploy stop gate and is not run after
deploy.

## Test/dev deploy

The dev environment is for live checks of fixes and as a spare bot. Current default: GitHub Actions
automatically deploys `dev` to the Coolify service `transcrib-dev`. It has a separate Telegram token,
separate Coolify-managed env variables, and a separate Postgres volume. Using one `BOT_TOKEN` in prod
and dev is forbidden: Telegram long polling allows only one active process per token.

Log-level policy: the test/dev env keeps `LOG_LEVEL=debug`, the prod env keeps `LOG_LEVEL=info`. Do
not print secrets even at debug; the level is for diagnosing states and external integrations.

Deploy:

```sh
git push origin dev
# or a manual rerun:
gh workflow run deploy.yml --ref dev -f target=dev
```

First wait for the workflow to succeed, then be sure to check the running environment:

```sh
gh run watch <run-id> --exit-status
deploy/verify-deploy.sh dev <full-dev-git-sha>
deploy/safe-ssh.sh coolify-prod-health
```

Expected:

- `verify-deploy.sh` returns `PASS`;
- app image and `app_revision` exactly match the full expected SHA;
- app and Postgres are in the `running/healthy` state, without restart/OOM;
- the dev migrate container, if available, has the same image/revision and `Exited (0)`;
- if Coolify has already removed the one-shot migrate container, the script returns `INCONCLUSIVE`
  (`4`): DB/goose does not replace the exit code proof, the deployment task is checked separately;
- prod containers were not recreated by the test deploy.

## Diagnostics without arbitrary SSH

For AI/devops diagnostics use [`safe-ssh.sh`](safe-ssh.sh). The script is read-only and does not read
secrets. Before running it, an SSH key must be loaded into the local `ssh-agent`:

```sh
ssh-add ~/.ssh/id_ed25519
```

Main commands:

```sh
deploy/safe-ssh.sh prod-health
deploy/safe-ssh.sh prod-deployed-sha
deploy/safe-ssh.sh test-health
deploy/safe-ssh.sh test-deployed-sha
deploy/safe-ssh.sh prod-app-journal "2026-06-24 00:00" "2026-06-25 00:00"
deploy/safe-ssh.sh prod-postgres-journal "2026-06-24 00:00" "2026-06-25 00:00"
deploy/safe-ssh.sh test-app-journal "2 hours ago"
deploy/safe-ssh.sh test-postgres-journal "2 hours ago"
deploy/safe-ssh.sh journal-usage
deploy/safe-ssh.sh dozzle-ps
deploy/safe-ssh.sh dozzle-firewall
deploy/safe-ssh.sh coolify-health
deploy/safe-ssh.sh coolify-public
deploy/safe-ssh.sh coolify-dev-health
deploy/verify-deploy.sh dev <full-dev-git-sha>
deploy/safe-ssh.sh coolify-dev-app-logs 200
deploy/safe-ssh.sh coolify-prod-health
deploy/verify-deploy.sh prod <full-release-git-sha>
deploy/safe-ssh.sh coolify-prod-app-logs 300
deploy/safe-ssh.sh coolify-prod-postgres-logs 100
deploy/safe-ssh.sh coolify-prod-migrate-logs 100
deploy/safe-ssh.sh docker-inventory
deploy/safe-ssh.sh nginx-inventory
deploy/safe-ssh.sh system-failed
deploy/safe-ssh.sh docker-events 24
deploy/safe-ssh.sh docker-daemon-logs 200
deploy/safe-ssh.sh kernel-errors 200
deploy/safe-ssh.sh oom-kills 200
deploy/safe-ssh.sh cron-logs 200
deploy/safe-ssh.sh backup-status
deploy/safe-ssh.sh backup-log 100
deploy/safe-ssh.sh nginx-logs 200
deploy/safe-ssh.sh disk
deploy/safe-ssh.sh memory
deploy/safe-ssh.sh nginx-test
deploy/safe-ssh.sh site-head
deploy/safe-ssh.sh git-status
```

OpenRouter direct access from the RU VPS can be blocked by Cloudflare. Current host-side workaround
is `mihomo.service` on `10.0.0.1:20171` (HTTP) / `10.0.0.1:20170` (SOCKS), available only to Docker
private networks. Validate it without printing secrets:

```sh
deploy/safe-ssh.sh mihomo-status
deploy/safe-ssh.sh openrouter-egress
```

Durable app wiring is `OPENROUTER_PROXY_URL=http://10.0.0.1:20171` in Coolify-managed env. Global
`HTTPS_PROXY` is emergency-only because it can affect Telegram, Speech2Text and YooKassa.

Logs are described in [`logs.md`](logs.md). In short: the active Coolify prod writes app/Postgres to
the host journald with 14-day retention; Coolify dev as of 2026-07-01 actually uses Docker
`json-file`, so its logs are read through `coolify-dev-*-logs` or the Coolify UI and are not yet a
14-day archive. Dozzle does not store logs and is not an archive: it is a live viewer on top of the
Docker API for current containers.

Archive investigations of legacy prod and the legacy test fallback by time are done through journald
on the host and read only through `safe-ssh.sh`. The active Coolify dev currently has no journald
archive: current Docker/Coolify logs are available for it through `coolify-dev-*-logs`. The active
Coolify prod is checked through `coolify-prod-*-logs` and `coolify-prod-health`. Filter by `job_id`,
`user_id`, `external_job_id`, `event` locally with `rg` over the output, not through an arbitrary
remote shell:

```sh
deploy/safe-ssh.sh test-app-journal "15 minutes ago"
deploy/safe-ssh.sh test-app-journal "15 minutes ago" | rg '"event":"startup_complete"'
deploy/safe-ssh.sh coolify-dev-app-logs 200 | rg '"event":"startup_complete"'
deploy/safe-ssh.sh coolify-prod-app-logs 300 | rg '"event":"startup_complete"'
deploy/safe-ssh.sh prod-app-journal "2026-06-24 00:00" "2026-06-25 00:00" | rg '"job_id":123'
deploy/safe-ssh.sh prod-app-journal "yesterday" "today" | rg '"user_id":456'
deploy/safe-ssh.sh prod-app-journal "2 hours ago" | rg 'external_job_id|provider_status'
deploy/safe-ssh.sh prod-postgres-journal "1 hour ago"
deploy/safe-ssh.sh journal-usage
```

The retention drop-in was installed on 2026-06-26:
`/etc/systemd/journald.conf.d/transcrib-retention.conf` sets `MaxRetentionSec=14day`,
`SystemMaxUse=2G`, `SystemKeepFree=10G`, `Compress=yes`. On migration/recovery, creating the drop-in
and restarting `systemd-journald` remain a DevOps/manual mutation after owner confirmation.

Incident minimum: first `coolify-prod-health`/`coolify-dev-health` (`prod-health` and `test-health`
only for the legacy fallback), then app/postgres logs for the needed environment, then
`docker-events`, `system-failed`, `docker-daemon-logs`, `kernel-errors`, `oom-kills`, `disk`,
`memory`, `cron-logs`, `backup-status`, and `backup-log`. This is manual diagnostics; automatic alerts
and metrics remain a separate observability slice.

## Rollback

Rollback is done by switching the Coolify resource to the previous image tag:

```sh
# dev rollback, only after owner confirmation:
# set APP_IMAGE=ghcr.io/daniil4545/transcrib:sha-<previous_sha> and redeploy

# prod rollback, only after separate owner confirmation:
# set APP_IMAGE=ghcr.io/daniil4545/transcrib:sha-<previous_sha> and redeploy
```

Rolling back code does not roll back data. If the release included a DB migration, first check
schema compatibility. On prod, a forward fix is preferable to `goose down`.

## Migrations and partial deploy

In the Coolify compose, `app` depends on `migrate: service_completed_successfully`; on a failed
migrate, check `verify-deploy.sh`, the Coolify deployment task, and `coolify-dev-migrate-logs`. If
the one-shot container is already removed, `verify-deploy.sh dev` honestly returns `INCONCLUSIVE`: a
successful DB/goose probe proves the schema is available, but not the exit code of the migration of
this particular deploy. Do not roll back data with `goose down`; use a forward fix or a restore from
backup by a separate decision.

Before risky prod DB migrations a fresh backup is required. Runbook: [`backup.md`](backup.md).

## What not to do without separate confirmation

- `docker compose down -v`, `docker volume rm`, global `docker system prune`;
- prod/test deploy, prod/test rollback, restart/recreate app;
- editing Coolify prod/dev env;
- reading or printing secrets;
- changing nginx/certbot/ufw;
- operations that may affect neighboring projects on the shared VPS.

## Initial setup or recovery

These steps are needed only on migration/recovery, not on a regular deploy:

Prod and dev are brought up as Coolify services from the repository `src/docker-compose.prod.yml`;
env is set in the Coolify UI. The initial service setup order is described above, the Coolify
specifics for transcrib are in [`coolify-cd.md`](coolify-cd.md).

Minimum Coolify prod env:

```sh
BOT_TOKEN=...
POSTGRES_PASSWORD=<strong password>
DATABASE_URL=postgres://transcrib:<same password>@postgres:5432/transcrib?sslmode=disable
```

Full list of variables and defaults: [`../src/.env.example`](../src/.env.example).
