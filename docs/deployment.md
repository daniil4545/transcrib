# transcrib deployment and infrastructure

Canon of facts and decisions about the server. Operational commands are kept in [`../deploy/README.md`](../deploy/README.md)
and the dedicated runbooks in `deploy/`, so that competing instructions do not multiply.

## Where to read what

| Need | Document |
| --- | --- |
| How to roll out prod/test and verify a deploy | [`../deploy/README.md`](../deploy/README.md) |
| DB backup/restore-test | [`../deploy/backup.md`](../deploy/backup.md) |
| Logs | [`../deploy/logs.md`](../deploy/logs.md) |
| Dozzle | [`../deploy/dozzle/README.md`](../deploy/dozzle/README.md) |
| DevOps practices and hardening backlog | [`devops-practices.md`](devops-practices.md) |

## Current status

As of 2026-07-07 transcrib is deployed on the shared VPS `203.0.113.10` and runs 24/7:

- prod bot: Coolify service `transcrib-prod` (`<prod-resource-uuid>`), runtime containers
  `app-<prod-resource-uuid>` and `postgres-<prod-resource-uuid>`; the migrate container may
  be absent after a deploy;
- dev/test bot: Coolify service `transcrib-dev` (`<dev-resource-uuid>`), containers
  `app-*`, `postgres-*`, `migrate-*`; the legacy `transcrib-dev-app-1` is stopped, the legacy Postgres
  is kept as a fallback/source;
- prod deploy: only the manual GitHub Actions `Deploy` by release tag;
- site: `https://transcrib.example.com`, host nginx, not Docker;
- dev/prod CD: GitHub Actions + Coolify (`https://coolify.example.com`) on top of a GHCR image; prod
  deploy goes only manually by release tag with `target=prod`;
- logs: Coolify prod `app`/`postgres` use `systemd-journald` with 14-day retention; Coolify dev currently
  uses Docker `json-file` logs; legacy prod/test have a journald archive while the containers
  exist; Dozzle is available only via an SSH tunnel as a live viewer for the
  containers matching its filter;
- backup: temporary on-server `pg_dump` of the prod DB, 14-day retention, first restore-test PASS.

Check the prod/dev Coolify revision with:

```sh
deploy/safe-ssh.sh coolify-prod-health
deploy/safe-ssh.sh coolify-dev-health
```

## Server

| Parameter | Value |
| --- | --- |
| Purpose | Shared team VPS; transcrib lives next to host nginx, Zabbix, svyazka and a separate `transcription-*` stack; Vexa was stopped on 2026-06-26 |
| IP | `203.0.113.10` |
| Hostname | `msk-1-vm-qkkc` |
| Provider node | `kvmnvm-629` |
| OS | Ubuntu 24.04.4 LTS |
| Architecture | x86_64 |
| SSH | `root@203.0.113.10`, access via the local `ssh-agent` |
| VPS provider/plan | TBD |

The actual inspection on 2026-06-17 showed that the server is not dedicated: neighboring projects were already
running on it and host nginx holds 80/443. Vexa was stopped on 2026-06-26, but other neighboring services remain
on the box, so transcrib is deployed as an isolated Docker Compose guest:
its own project names, networks, volumes, Postgres without a published port. Global `prune`,
`down -v`, edits of nginx/ufw/certbot and operations on neighboring projects are forbidden without a separate
review.

AI/devops access goes through [`../deploy/safe-ssh.sh`](../deploy/safe-ssh.sh). Direct SSH,
editing `.env` and server mutations remain operator actions.

## Hardware

Taken over SSH on 2026-06-13:

| Resource | Value |
| --- | --- |
| CPU | 4 vCPU, QEMU Virtual CPU 8.2.0 @ 2.0 GHz |
| RAM | 7.8 GiB |
| Swap | 4 GiB `/swapfile` |
| GPU | None |
| Disk | 80 GB `/dev/sda1` |

Conclusion for a local LLM: the current VPS is a weak CPU-only machine and shares resources with the bot/Postgres/neighbors.
Full-size GPU inference is impossible; a CPU compromise requires chunking and a separate quality test.

## Network

- Host nginx holds 80/443. Caddy is not used.
- The Telegram bot works via long polling, no incoming app ports are needed.
- Postgres is not published outside and is reachable only inside the compose network.
- Outgoing ports `25/465/587/2525/389/3389/53413` are closed by the provider; this is not a blocker
  for the current product.
- RU network trap: the IPv4 route from the VPS to the default IP of `api.telegram.org` (`149.154.166.110`)
  times out. In prod/dev `app` and in `coolify` itself `extra_hosts` is set to a reachable Telegram DC
  `149.154.167.220`; without the pin, Coolify Telegram notifications fail with a timeout.
- OpenRouter trap: direct egress from the Timeweb VPS (`203.0.113.10`) gets a Cloudflare `403
  Access denied by security policy`. Host `mihomo.service` runs as an outbound proxy sidecar:
  `10.0.0.1:20171` HTTP, `10.0.0.1:20170` SOCKS, controller `127.0.0.1:9090`; UFW allows access to the
  proxy ports only from Docker private networks. The target app config is `OPENROUTER_PROXY_URL`, not
  a global `HTTPS_PROXY`.
- For Coolify Telegram notifications, Coolify's own `APP_URL` must be
  `https://coolify.example.com`: otherwise the inline button URL is built as `http://localhost` and
  the Telegram notification is not sent.
- Coolify Monitoring/Sentinel additionally needs an internal bind `10.0.0.1:8000:8080`:
  `coolify-sentinel` pushes data to `host.docker.internal:8000`. Do not open the public `0.0.0.0:8000`;
  external access stays through nginx and `https://coolify.example.com`.
- For Coolify realtime do not open `6001/6002` externally: `coolify-realtime` listens on loopback, and host
  nginx proxies the websocket paths `/app/` -> `127.0.0.1:6001` and `/terminal/ws` -> `127.0.0.1:6002`.

`transcrib.example.com` is served by host nginx from `/var/www/transcrib.example.com`. The future Google OAuth callback
must go through the same nginx, proxied to a local app port, when the Google slice appears.

## Runtime stack

| Component | Purpose |
| --- | --- |
| Docker + Compose | Coolify-managed prod/dev app/Postgres/migrate, legacy fallback projects |
| PostgreSQL 16 | DB and queue via `FOR UPDATE SKIP LOCKED` |
| nginx | TLS and the static site `transcrib.example.com` |
| Dozzle | Web live viewer of prod/test container logs |
| Coolify | Target CD UI and deploy orchestrator for GitHub Actions + GHCR |
| GitHub Actions + GHCR | build/test/publish image for Coolify CD |
| systemd-journald | Target 14-day archive of Coolify prod and legacy fallback `app`/`postgres` |
| mihomo | Host outbound proxy sidecar for OpenRouter when direct RU egress is blocked |

A single Go process contains Telegram long polling, an HTTP server for the future OAuth callback and
the worker/scheduler. Detailed service architecture: [`architecture.md`](architecture.md).

## Deploy model

The current dev CD model: GitHub Actions runs checks, builds the image
`ghcr.io/daniil4545/transcrib:sha-<git-sha>`, then strictly sequentially:

1. PATCHes the Coolify Service with the exact `src/docker-compose.coolify.yml` as base64
   `docker_compose_raw` with `instant_deploy=false`.
2. PATCHes `APP_IMAGE` to the full SHA tag.
3. Calls the deploy webhook exactly once.

The Coolify Service stores the Compose inside; the repository file is the single source of truth
for both environments.
The moving tags `dev`/`prod` are published, but the active runtime must be a SHA tag. `dev` is rolled out
automatically from the `dev` branch. The prod workflow syncs Compose in the same step as dev; editing
Compose by hand in the Coolify UI is forbidden: a divergence of the UI version from the repository took prod down
on 2026-08-12 for 29 hours. Prod deploy is started only
manually via `workflow_dispatch` on a Git tag with `target=prod`; a push to `main` does not roll out prod.
The GitHub Environment `production` holds the prod webhook; required reviewers depend on the GitHub plan.
Runbook: [`../deploy/coolify-cd.md`](../deploy/coolify-cd.md).

The current prod rollout through the target CD: tag `0.0.3.7` ->
`8940569680b43af397b59f3e9df51276f3989b02`, active image
`ghcr.io/daniil4545/transcrib:sha-8940569680b43af397b59f3e9df51276f3989b02`. The legacy prod app
is stopped, the legacy Postgres volume is kept as a rollback source.

Legacy fallback: bare repo + server-side hook. This is not the regular release path.

- prod fallback: `git push prod <ref>:master` -> `/opt/transcrib` -> compose project `transcrib`;
- test fallback: `git push prod <ref>:dev` -> `/opt/transcrib-dev` -> compose project
  `transcrib-dev`.

The hook deploys the exact accepted SHA, uses `flock`, sets a git-SHA image tag and brings up
compose. The shared `transcrib:latest` is not used. This protects prod from an accidental test image and
gives a clear rollback to the previous SHA. `.deployed-sha` means a completed `compose up`, not an
app-level health gate; after a deploy, always check health/logs.

Release order since 2026-06-25: a feature is first rolled out to the test/dev bot, checked by hand, then
the same SHA is promoted to prod. The prod environment is considered stable; the dev environment allows diagnostics and
experiments. Since 2026-07-07 prod deploy has a mandatory stop-gate: before manually starting the
`Deploy` workflow, confirm `deploy/safe-ssh.sh coolify-prod-active-jobs` with
`active_jobs_count=0` and a clean `coolify-prod-app-logs 300`. If there are active
`created|joining|recording|processing|transcribed|summarizing|awaiting_reauth` Jobs or the logs
show a current call, the deploy is postponed.

Operational commands and success criteria: [`../deploy/README.md`](../deploy/README.md).

## Observability

The app writes `slog` JSON to stdout. For Coolify prod, the Docker logging driver `journald` sends
the stdout/stderr of `app` and `postgres` to the host `systemd-journald`; this is the target archive with 14-day retention.
As of 2026-07-01 Coolify dev actually uses Docker `json-file`, so its logs are read through the
Coolify UI or `deploy/safe-ssh.sh coolify-dev-*-logs` and are not yet a 14-day archive.
Legacy prod/test also have a journald archive while the corresponding containers exist.
Dozzle remains a live viewer of the current containers on top of the Docker API and is not a history archive.

Time-based investigation goes through `deploy/safe-ssh.sh`:

- `coolify-prod-app-logs 300`
- `coolify-prod-postgres-logs 100`
- `prod-app-journal "2026-06-24 00:00" "2026-06-25 00:00"` for the legacy fallback
- `prod-postgres-journal "2026-06-24 00:00" "2026-06-25 00:00"` for the legacy fallback
- `test-app-journal "2 hours ago"`
- `test-postgres-journal "2 hours ago"`
- `coolify-dev-app-logs 200`
- `journal-usage`

Filter by `job_id`, `user_id`, `external_job_id`, `event` locally: `deploy/safe-ssh.sh
coolify-prod-app-logs 300 | rg '"job_id":123'`.

Weekly aggregates and analysis of failed jobs from the prod DB (read-only `SELECT`, without Metabase):

- `coolify-prod-stats [days]` - jobs by status, payments, refunds, subscriptions, users for N days (default 7, max 90);
- `coolify-prod-failures [days]` - failed jobs (`failed`/`failed_join`/`failed_summary`) for N days with a truncated `last_error`.

`/etc/systemd/journald.conf.d/transcrib-retention.conf` was installed on 2026-06-26:
`MaxRetentionSec=14day`, `SystemMaxUse=2G`, `SystemKeepFree=10G`, `Compress=yes`.

System read-only checks through safe-ssh remain a separate set: `system-failed`,
`docker-events`, `docker-daemon-logs`, `kernel-errors`, `oom-kills`, `cron-logs`,
`backup-status`, `backup-log`, `nginx-logs`, `disk`, `memory`.

Dozzle is closed from public access: Docker bind `127.0.0.1:48080`, external `tcp/48080` is closed by an INPUT firewall rule, access only through an SSH tunnel.

`LOG_LEVEL` is set in env without a rebuild: prod keeps `info`, test/dev keeps `debug` for manual
diagnostics. Even at `debug` it is forbidden to log secrets, full tokens, DSNs and private links.

The current mode is manual diagnostics via the runbook. The next observability step: automatic
alerts on containers/disk/backup/OOM/restart-loop and, if the volume justifies it, a light metrics stack
such as Grafana/Prometheus or a simpler healthcheck agent.

## Backup

Temporary mode: a daily on-server `pg_dump` of the prod DB to `/var/backups/transcrib/postgres`,
14-day retention, 10 GiB guard, cron `03:20 MSK`. The first restore-test passed on 2026-06-25.

This is a degraded mode: it protects against logical DB corruption and a faulty migration, but not against loss of the VPS.
The target next step is an off-box backup and a regular restore-test. Runbook: [`../deploy/backup.md`](../deploy/backup.md).

## Secrets and env

Variable format: [`../src/.env.example`](../src/.env.example). On the server:

- prod env: Coolify-managed environment variables in service `transcrib-prod`;
- dev env: Coolify-managed environment variables in service `transcrib-dev`;
- legacy prod fallback env: `/opt/transcrib/src/.env`;
- legacy test fallback env: `/opt/transcrib-dev/src/.env`.

The owner creates and edits the files manually, permissions `600`. The AI agent does not read `.env`, does not print the DSN,
does not run `docker inspect env` and `docker exec printenv`.

Prod and test must never use the same Telegram `BOT_TOKEN`: two long polling processes on
one token knock each other out with `409 Conflict`.

## Open infra/hardening tasks

Policy/backlog: [`devops-practices.md`](devops-practices.md).
Nearest infra tails:

- move the backup off-box;
- keep Dozzle on `127.0.0.1:48080` and check that the public `:48080` is closed;
- move from root deploy to a separate `deploy` user/forced command;
- later add automatic control of containers, `backup.log`, disk/OOM/restart-loop and zombie jobs
  or a full metrics stack;
- keep prod migrations in the expand-contract style and forward-fix.

## Historical decisions

- 2026-06-17: a bare repo + `post-receive` was chosen over a registry/CI as the minimal working deploy
  for a single Go service on a shared VPS.
- 2026-06-22: the site `transcrib.example.com` was deployed on host nginx, separately from the Docker/app deploy.
- 2026-06-24: the test/reserve bot was moved to a separate compose project `transcrib-dev`.
- 2026-06-25: prod/test were separated by git-SHA image tags; on-server backup was enabled; Dozzle shows
  prod/test containers with the filter `name=transcrib`.
- 2026-07-01: the CD cutover to GitHub Actions + Coolify started; DNS `coolify.example.com` points
  to the VPS, the nginx/TLS vhost is enabled, the Coolify UI is available through host nginx, the repo got workflows and
  the Coolify compose/runbook.
- 2026-07-01: the dev bot was moved to the Coolify service `transcrib-dev`, GHCR pull access was fixed,
  the legacy dev DB was moved to Coolify Postgres, the legacy `transcrib-dev-app-1` was stopped.

Current operations live only in `deploy/*`.
