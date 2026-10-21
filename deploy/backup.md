# Prod Postgres backup

Goal: a daily `pg_dump` of the prod DB without reading `.env` and without printing passwords/DSN. The
active prod after the cutover is the Coolify service `transcrib-prod`; for it `deploy/backup.sh` finds
the Postgres container by the label `coolify.resourceName=transcrib-prod`. The legacy prod compose
project `transcrib` (`transcrib-postgres-1`) is kept only as a rollback source. The target mode is an
off-box backup from a separate backup host or from the owner's working computer. The temporary mode
until external storage appears is an on-server backup on the same VPS.

The on-server backup is a deliberate degraded mode: it protects against a bad migration or logical
corruption of the DB, but not against loss of the VPS, the disk, root compromise, or a provider error.

## Safety rules

- Do not run `docker compose down -v`, `docker volume rm transcrib_*`, global `docker system prune`
  or `docker image prune` for this task.
- Do not read `/opt/transcrib/src/.env`, do not run `docker inspect`, `docker exec printenv`, do not
  print the password or `DATABASE_URL`.
- Do not treat a single on-server copy as a full backup: as soon as external storage appears, the
  dumps must be moved off-box.
- Keep the backup directory on an encrypted disk or in the operator's protected storage. Do not store
  `age`/S3/rsync keys in the repo.

## Temporary on-server run

Important: for the active Coolify prod use `TRANSCRIB_POSTGRES_RESOURCE=transcrib-prod`, so that the
container is looked up by label rather than by generated name. Commands without this variable go to
the legacy `transcrib-postgres-1`.

On the VPS:

```sh
install -d -m 700 -o root -g root /var/backups/transcrib/postgres
cd /opt/transcrib
TRANSCRIB_BACKUP_MODE=local \
  TRANSCRIB_POSTGRES_RESOURCE=transcrib-prod \
  TRANSCRIB_BACKUP_DIR=/var/backups/transcrib/postgres \
  TRANSCRIB_BACKUP_RETENTION_DAYS=14 \
  TRANSCRIB_BACKUP_MIN_FREE_KB=10485760 \
  ./deploy/backup.sh
```

Before a restore test or a manual check of the active Coolify prod, make a fresh dump from the
Coolify prod DB:

```sh
cd /opt/transcrib
TRANSCRIB_BACKUP_MODE=local \
  TRANSCRIB_POSTGRES_RESOURCE=transcrib-prod \
  TRANSCRIB_BACKUP_DIR=/var/backups/transcrib/postgres \
  TRANSCRIB_BACKUP_RETENTION_DAYS=14 \
  TRANSCRIB_BACKUP_MIN_FREE_KB=10485760 \
  ./deploy/backup.sh
```

Gate criterion: the command ends with `backup: created ...`, `gzip -t` passes inside the script, then
the fresh dump passes the restore test from the section below.

`/etc/cron.d/transcrib-backup` on the VPS, daily at 03:20 MSK (server timezone):

```cron
20 3 * * * root cd /opt/transcrib && TRANSCRIB_BACKUP_MODE=local TRANSCRIB_POSTGRES_RESOURCE=transcrib-prod TRANSCRIB_BACKUP_DIR=/var/backups/transcrib/postgres TRANSCRIB_BACKUP_RETENTION_DAYS=14 TRANSCRIB_BACKUP_MIN_FREE_KB=10485760 ./deploy/backup.sh >>/var/backups/transcrib/postgres/backup.log 2>&1
```

`TRANSCRIB_BACKUP_MIN_FREE_KB=10485760` is a guard for 10 GiB of free space, so that the backup does
not fill up the shared VPS.

`backup.log` is not covered by the dump retention. While the backup stays on-server, add a system
`logrotate` or clean the log periodically as an operator task; when moving to an off-box cron, it is
better to send the result to syslog/monitoring.

Actual state on 2026-06-25:
- the directory `/var/backups/transcrib/postgres` was created with permissions `700`;
- first manual dump: `transcrib_20260625T010019Z.sql.gz`, `136556` bytes, `gzip -t` PASS;
- check run of the repo script: `transcrib_20260625T010541Z.sql.gz`, `136556` bytes;
- cron installed in `/etc/cron.d/transcrib-backup`, mode `0644`;
- restore test PASS in a temporary Postgres 16 volume: `goose_version=8`, `users_count=6`,
  `jobs_count=14`; the prod volume was not attached.

Actual state after the Coolify prod cutover on 2026-07-01:
- cron `/etc/cron.d/transcrib-backup` switched to
  `TRANSCRIB_POSTGRES_RESOURCE=transcrib-prod`;
- the server copy `/opt/transcrib/deploy/backup.sh` was updated to the version with the Coolify
  resource lookup;
- control dump of the active prod: `transcrib_20260701T141023Z.sql.gz`, `199131` bytes,
  the source in the output of the manual run is `resource:transcrib-prod/transcrib`.

## Target off-box run

Prerequisites on the off-box machine:
- SSH access to `root@203.0.113.10` through `ssh-agent` or a local `ssh_config`.
- `ssh`, `gzip`, `find`, `date`, `mkdir`, `mktemp`, `df`, `awk`.
- A directory for dumps outside the repository, permissions `700`.

Example of a manual run:

```sh
mkdir -p /secure/backups/transcrib
chmod 700 /secure/backups/transcrib

TRANSCRIB_BACKUP_DIR=/secure/backups/transcrib \
  TRANSCRIB_BACKUP_MODE=ssh \
  TRANSCRIB_POSTGRES_RESOURCE=transcrib-prod \
  TRANSCRIB_BACKUP_RETENTION_DAYS=14 \
  deploy/backup.sh
```

The script creates a file like `transcrib_YYYYMMDDTHHMMSSZ.sql.gz` and checks:
- the `pg_dump` command finished successfully;
- the file is not empty and is larger than `TRANSCRIB_BACKUP_MIN_BYTES` (default `1024`);
- `gzip -t` passes;
- old `transcrib_*.sql.gz` files are removed by retention in the local directory.

Exit codes:
- `0` - backup created;
- `2` - configuration error;
- `3` - missing local dependency;
- `4` - `pg_dump`/SSH/gzip failure (SSH is not used in local mode);
- `5` - empty, too small, or corrupted dump;
- `6` - retention cleanup failure.

Cron on the off-box machine, if the same `03:20 MSK` window is needed, daily at `00:20 UTC`:

```cron
20 0 * * * cd /path/to/transcrib && TRANSCRIB_BACKUP_DIR=/secure/backups/transcrib TRANSCRIB_BACKUP_MODE=ssh TRANSCRIB_POSTGRES_RESOURCE=transcrib-prod ./deploy/backup.sh >>/secure/backups/transcrib/backup.log 2>&1
```

If S3/rsync/`age` is needed, make an operator wrapper outside the repo. Example of the shape without
secrets:

```sh
#!/usr/bin/env bash
set -euo pipefail

cd /path/to/transcrib
TRANSCRIB_BACKUP_DIR=/secure/backups/transcrib \
  TRANSCRIB_BACKUP_MODE=ssh \
  TRANSCRIB_POSTGRES_RESOURCE=transcrib-prod \
  ./deploy/backup.sh

# Next the operator adds their own sync/encrypt step.
# Do not store secrets, access keys, the age identity, or the remote URL in the repository.
```

## Monthly restore test

The check runs on the off-box machine or a separate test host. The prod volume is not used and is not
attached. The names of the temporary container/volume are unique; only they are removed.

1. Pick a fresh dump:

```sh
backup_file="$(ls -1t /secure/backups/transcrib/transcrib_*.sql.gz | head -1)"
test_id="transcrib-restore-$(date -u +%Y%m%dT%H%M%SZ)"
```

2. Start a clean temporary Postgres 16:

```sh
docker volume create "${test_id}-pgdata"
docker run -d --name "${test_id}" \
  -e POSTGRES_USER=transcrib \
  -e POSTGRES_PASSWORD=restore-test-only \
  -e POSTGRES_DB=transcrib \
  -v "${test_id}-pgdata:/var/lib/postgresql/data" \
  postgres:16
```

3. Wait for readiness and restore the dump:

```sh
until docker exec "${test_id}" pg_isready -U transcrib -d transcrib; do sleep 1; done
gzip -dc "$backup_file" | docker exec -i "${test_id}" psql -v ON_ERROR_STOP=1 -U transcrib -d transcrib
```

4. Check the basic signs of a live schema and data:

```sh
docker exec "${test_id}" psql -U transcrib -d transcrib -c "select max(version_id) from goose_db_version;"
docker exec "${test_id}" psql -U transcrib -d transcrib -c "select count(*) as users from users;"
docker exec "${test_id}" psql -U transcrib -d transcrib -c "select count(*) as jobs from jobs;"
```

Acceptance criterion: the restore finishes without errors, `goose_db_version` is readable, the key
tables `users`/`jobs` exist, and queries against them pass.

5. Remove only the temporary objects:

```sh
docker rm -f "${test_id}"
docker volume rm "${test_id}-pgdata"
```

Record the restore test date, the dump name, and the result in the operator log. Do not commit dumps,
logs with personal data, or secrets to the repo.
