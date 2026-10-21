# transcrib DevOps practices (reference sized for our scale)

> **Purpose:** a pragmatic deploy and operations canon: "how it is done right" in the industry, filtered for our reality (one Go service, one VPS, Docker Compose, CI/CD, two environments).
> **Origin:** multi-agent research (8 categories, 87 practices, checked against primary sources) + an infra review of the CI/CD slice (2026-06-22). Marked explicitly: what we take now / defer / deliberately skip.

## Our scale (we filter for it)

One Go monolith (Telegram bot, long polling: one consumer per token), Postgres as DB and queue, money (YooKassa) and Google OAuth. One shared VPS (~4 vCPU/8 GB), Docker Compose/Coolify, no cluster; Vexa was stopped on 2026-06-26, but the server remains shared. Dev CD and prod CD go through GitHub Actions -> GHCR -> Coolify; prod is rolled out only manually by release tag, the legacy bare-repo hook remains a fallback. Project canon: **minimum infrastructure, no k8s**. DevOps is run by an AI agent.

The main consequence of the scale: **the goal is a cheap rollback and a clean stop, not zero-downtime**; the reference "enterprise" measures (k8s, Vault, GitOps, PITR, OIDC federation) are over-engineering for us until we hit a limit.

## Reference by category: what we take now

| Category | Reference (how it is done right) | Taken now |
|---|---|---|
| **Build/artifacts** | Multi-stage, non-root, minimal base (distroless), git-SHA tag, registry | distroless/nonroot (in place), GitHub Actions build, GHCR image `sha-<sha>` + moving tags `dev/prod`; legacy server build only as a fallback |
| **Config/secrets** | 12-factor, secrets outside git/logs/image, encryption of sensitive data at rest, rotation | `.env` chmod 600 (in place), **off-box encrypted secret storage**, `gitleaks` pre-commit, `refresh_token` encryption |
| **Rollout/rollback** | Health-gated, fast rollback to the previous artifact, graceful shutdown | **recreate** (blue-green is impossible for the bot), **rollback to the previous git-SHA tag**, post-deploy health signal |
| **DB migrations** | Backward compatible (expand-contract), `lock_timeout`, `CONCURRENTLY` indexes, forward-fix | expand-contract, `pg_dump` BEFORE a destructive one, `lock_timeout`, forward-fix instead of `goose down` on prod |
| **CI/CD** | Test gate, promotion across environments, manual approval for prod, concurrency control | GitHub Actions: test gate, **dev auto via Coolify / prod manual workflow_dispatch**, legacy `flock` only as a fallback |
| **CI supply chain** | Pin actions by commit SHA, least-privilege token, secrets outside PR workflows | **pin by SHA**, `permissions: contents: read`, secrets in the protected `production` environment, Dependabot |
| **Observability/backup** | Structured logs, rotation, health, metrics/alerts, backup + restore test | slog JSON + rotation (in place), **daily off-box `pg_dump` + monthly restore test**, cron alerts (disk/backup/zombies) |
| **Host hardening** | Non-root deploy, forced-command SSH key, firewall, non-root in the container, isolation | **`deploy` user instead of root** + forced-command, `cap_drop: all` + `no-new-privileges`, **Dozzle on localhost** (Docker publishes the port bypassing ufw!) |

## Already implemented

- Git-SHA image tag on prod/test, no shared `:latest`.
- GitHub Actions: the CI workflow and the deploy workflow run DB-backed tests with migrations; the deploy workflow
  does not build or roll out a GHCR image if the DB-backed checks failed.
- Coolify dev CD: service `transcrib-dev` in environment `dev`, image `ghcr.io/daniil4545/transcrib:sha-<sha>`.
- Coolify prod CD: service `transcrib-prod` in environment `prod`, the current rollout `0.0.3.4` was done through GitHub Actions -> GHCR -> Coolify.
- Coolify MCP: read-only endpoint `https://coolify.example.com/mcp` for the AI agent; use it as a
  quick overview of Coolify resources, not as a deploy or env change mechanism.
- Coolify Telegram notifications: Coolify itself has `APP_URL=https://coolify.example.com`
  and the pin `api.telegram.org:149.154.167.220`; without this, infra alerts fail with a timeout or
  an `http://localhost` inline button URL.
- Exact-SHA checkout in the deploy hook, a separate deploy index per environment, `flock` against build races.
- The dev/test bot as a separate Coolify environment with a separate Postgres volume and Telegram token; the legacy
  `transcrib-dev` Postgres is kept, the app is stopped.
- On-server `pg_dump` as a temporary degraded backup, 14-day retention, restore-test passed.
- A journald archive of Coolify prod and legacy prod/test `app` and `postgres` with 14-day retention (`MaxRetentionSec=14day`,
  `SystemMaxUse=2G`, `SystemKeepFree=10G`).
- Coolify dev logs are currently read through Docker `json-file`/Coolify UI, not through the 14-day
  journald archive.
- Dozzle is started with the filter `name=transcrib`: it suits the legacy `transcrib-*` containers, but is not
  a viewer of the active Coolify prod/dev `app-*`/`postgres-*`; the port is closed to `127.0.0.1:48080`,
  access through an SSH tunnel.
- `safe-ssh.sh` covers manual read-only diagnostics not only of app/Postgres, but also of the system layer:
  failed systemd units, Docker events/daemon journal, kernel/OOM, cron, backup log, nginx journal,
  disk/memory.

## Next P0

**P0:**
1. **Off-box `pg_dump`** of the prod DB (runbook `../deploy/backup.md`) + a monthly restore test.
2. **Off-box encrypted secret storage** + runbook (losing the local copy stops being an emergency).
3. **Automatic prod health/log alerts** for Coolify prod: restart-loop, OOM, backup freshness,
   disk and zombie jobs; Coolify Telegram notifications already require a correct `APP_URL` and the
   `api.telegram.org` pin.
4. **Unprivileged `deploy` user** (docker group) instead of root + forced-command in `authorized_keys`.
5. **Dozzle on `127.0.0.1`**: closed on 2026-07-01; verify in the pre-beta smoke.
6. **Rotation/control of `backup.log`** for the on-server cron, until the off-box backup is in place.

**P1:**
6. Post-deploy health signal (startup log / `getMe`): tell "came up" from a restart-loop.
7. 4 cron alerts or a metrics stack after launch: disk, backup failure, zombie jobs,
   Telegram DC unavailability (`extra_hosts` pin).
8. Migration discipline in `db.md` (expand-contract, `lock_timeout`, `CONCURRENTLY`).
9. `cap_drop: all` + `no-new-privileges` in compose.
10. `gitleaks` pre-commit (secrets do not leak into git).
11. Supply chain: pin actions by SHA, least-privilege token, secrets in a protected environment.
12. Metrics/monitoring slice: after the public launch choose Grafana/Prometheus or a simpler
    lightweight healthcheck agent.

## Deferred (with a trigger to return)

- **`refresh_token` AES-GCM encryption**: together with Google moving to Production (google slice).
- **Trivy/SBOM image scan**: after we set up Actions.
- **HTTP graceful `Shutdown`**: when `web.go` appears (OAuth callback).
- **Postgres replica (HA)**: when uptime becomes critical (this is about hardware failure, not data loss).

## Deliberately NOT doing (at our scale)

- **Blue-green for the bot**: breaks the single consumer (two pollers = `409`). Only recreate.
- **Vault/secret manager**: extra infrastructure; `sops`+`age` / a password manager is enough.
- **WAL/PITR**: a daily dump is enough (losing up to a day is acceptable; revisit if the RPO tightens).
- **Prometheus/Grafana**: cron alerts are enough on one box.
- **OIDC federation for deploy from the cloud, Cosign signing, k8s, rootless Docker**: not our scale/risk.

## Separating the protection measures (important not to confuse)

| What it protects against | Measure |
|---|---|
| Loss/corruption of **data**, AI/human error | Off-box `pg_dump` backup + restore test |
| Loss of **access/secret** | Secrets management (an encrypted, backed-up storage) |
| Failure of **hardware/host** | Replica (HA): deferred |

A replica replicates a `DROP TABLE` verbatim too: it does not save from a mistake. Only a backup saves from a mistake.

## Principles of the devops agent (ALWAYS / NEVER)

**ALWAYS:** `pg_dump` before a destructive migration · `lock_timeout` in a risky migration · `CONCURRENTLY` indexes on populated tables · a migration is backward compatible · a git-SHA tag for rollback · check "the bot started" after a deploy · secrets outside git/logs · remember that Docker publishes a port bypassing ufw.

**NEVER:** deploy without a green DB-backed `make ci-check` with `DATABASE_URL` · blue-green for the bot · `goose down` as a way to restore prod · a backup without an off-box restore test · `docker.sock` in the app container · interrupt a running deploy · treat a `Read(.env)` deny as a barrier on the box (root SSH bypasses it via `docker exec/inspect`) · read secrets over SSH (`.env`, `docker inspect env`, `docker exec printenv`).
