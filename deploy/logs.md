# transcrib logs

Logs runbook. Goal: after any deploy, an operator or an AI agent must quickly understand where the
log physically lives, which log is an archive, and which one is only a live tail.

## Current status

| Environment | Containers | Main log access | 14-day archive |
| --- | --- | --- | --- |
| Coolify prod | `app-<prod-resource-uuid>`, `postgres-<prod-resource-uuid>`; the migrate container may be absent after deploy | `deploy/safe-ssh.sh coolify-prod-*-logs`, Coolify UI | Yes, host journald |
| Coolify dev | `app-<dev-resource-uuid>`, `postgres-<dev-resource-uuid>`, `migrate-<dev-resource-uuid>` | `deploy/safe-ssh.sh coolify-dev-*-logs`, Coolify UI | No, currently Docker `json-file` |
| Legacy prod fallback | `transcrib-app-1`, `transcrib-postgres-1`, `transcrib-migrate-1` | `deploy/safe-ssh.sh prod-*-logs`, `prod-*-journal` | Yes, host journald, while the containers exist |
| Legacy dev fallback | `transcrib-dev-*` | `deploy/safe-ssh.sh test-*-logs`, `test-*-journal` | Yes, host journald, while the containers exist |
| OpenRouter proxy | `mihomo.service` | `deploy/safe-ssh.sh mihomo-status`, `deploy/safe-ssh.sh mihomo-logs` | Yes, host journald |
| Dozzle | `dozzle-dozzle-1` | SSH tunnel -> `http://127.0.0.1:48080` | No |

Important: as of 2026-07-01 the Coolify-generated compose for dev does not contain
`logging.driver=journald`, although `src/docker-compose.coolify.yml` sets `logging:`. The actual
`docker inspect` check shows `app_log_driver=json-file`. So Coolify dev logs exist now, but do not
count as a 14-day journald archive. Coolify prod passed the log gate: `deploy/safe-ssh.sh
coolify-prod-health` shows `app_log_driver=journald` and `postgres_log_driver=journald`.

## Commands

Coolify prod:

```sh
deploy/verify-deploy.sh prod <full-release-git-sha>
deploy/safe-ssh.sh coolify-prod-app-logs 300
deploy/safe-ssh.sh coolify-prod-postgres-logs 100
```

Before a prod deploy, run `coolify-prod-active-jobs` separately: it is a mandatory stop gate, the
command must show `active_jobs_count=0`. If there are active jobs or the app logs show a current
call, do not start the deploy. After the deploy this gate is not repeated.

The prod migrate container may be absent after deploy. `verify-deploy.sh prod` does not require it,
but checks the DB response and reads `goose_version` with a separate safe-ssh command.

Legacy prod fallback:

```sh
deploy/safe-ssh.sh prod-health
deploy/safe-ssh.sh prod-app-journal "2 hours ago"
deploy/safe-ssh.sh prod-postgres-journal "2 hours ago"
deploy/safe-ssh.sh prod-app-logs 200
```

Coolify dev:

```sh
deploy/verify-deploy.sh dev <full-dev-git-sha>
deploy/safe-ssh.sh coolify-dev-app-logs 200
deploy/safe-ssh.sh coolify-dev-postgres-logs 200
deploy/safe-ssh.sh coolify-dev-migrate-logs 200
```

If Coolify removed the one-shot migrate container, the dev check returns `INCONCLUSIVE` (`4`): the
presence of the goose version in the DB does not prove `Exited (0)` for the current deploy.

System layer:

```sh
deploy/safe-ssh.sh docker-events 24
deploy/safe-ssh.sh docker-daemon-logs 200
deploy/safe-ssh.sh kernel-errors 200
deploy/safe-ssh.sh oom-kills 200
deploy/safe-ssh.sh journal-usage
```

OpenRouter egress proxy:

```sh
deploy/safe-ssh.sh mihomo-status
deploy/safe-ssh.sh mihomo-logs 100
deploy/safe-ssh.sh openrouter-egress
```

Do not print the `mihomo` provider subscription/config; routing domains are acceptable in the logs,
but secrets are not.

## What to check after a deploy

1. `deploy/verify-deploy.sh <dev|prod> <full-sha>` returned `PASS`: app/Postgres healthy,
   OCI revision/image SHA exact, restart count `0`, OOM `false`, start time valid.
2. Before a prod deploy `active_jobs_count=0` was confirmed; this is not a post-deploy gate.
3. The logs of the current `StartedAt` contain `startup_complete`.
4. Over the whole current run there is no `panic`, `fatal`, `409 Conflict`, config/db errors.
5. The DB answers, `goose_version` is read by a separate allowlisted safe-ssh command.
6. For dev, an available migrate container has the same SHA and `Exited (0)`; a removed one-shot
   gives `INCONCLUSIVE`. For prod, the absence of the migrate container is not a fail.
7. For Telegram long polling, exactly one app runs per bot token.
8. If the OpenRouter summary is enabled, direct VPS egress may stay `403`, but a request through
   `mihomo` must give `HTTP 200` on `/api/v1/models`; public `:20170/:20171` must be closed.
9. For Coolify prod read the archive through `coolify-prod-*-logs`; for the legacy fallback by time
   use `*-journal`; use Dozzle only as a live tail.
10. For Coolify dev, until the log driver is fixed, remember: `coolify-dev-*-logs` reads Docker
   json-file, not the journald retention.

## When moving to a new server

Minimum requirements:

- the app writes structured JSON logs to stdout;
- every runtime environment has a documented command for app/postgres/migrate logs;
- prod logs must have an archive with retention of at least 14 days;
- Dozzle/Coolify UI can be a convenient live viewer, but do not replace the archive;
- logs must not contain tokens, passwords, DSNs, private links, or full secrets.
