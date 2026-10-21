# Dozzle - live viewer of transcrib prod logs

Viewing the current logs of transcrib containers from a browser through an SSH tunnel. A separate
compose project `dozzle` (the transcrib deploy through `git push prod` does not touch it). Dozzle is
not an archive: historical logs of Coolify prod and of legacy prod/test `app` and `postgres` are read
through `systemd-journald` and `safe-ssh.sh`.
Spec and rationale:
[`../../docs/deployment.md`](../../docs/deployment.md).

**Status:** running on prod since 2026-06-22 (`amir20/dozzle:v10.6.6`, container `dozzle-dozzle-1`,
port `127.0.0.1:48080`, simple-auth). Since 2026-07-01 the port is closed to public access; view it
through an SSH tunnel. The "Server setup" section below is a reproducible procedure (reinstall, new
server), not a TODO.

**Access:** only through an SSH tunnel, login/password (Dozzle simple-auth):

```sh
ssh -N -L 48080:127.0.0.1:48080 root@203.0.113.10
open http://127.0.0.1:48080
```

This is a live viewer, not a history archive. Direct `http://203.0.113.10:48080` must be unreachable.
On the current server the protection has two layers: the Docker bind `127.0.0.1:48080:8080` and an
INPUT drop for external `tcp/48080`.

## Server setup (recreate/new server)

The `data/` directory (in `.gitignore`) is created on the server by hand - it is untracked, so it
survives deploys (like `.env`). For an already running Dozzle this section is needed only on
reinstall or move to a new server.

```sh
ssh root@203.0.113.10
cd /opt/transcrib/deploy/dozzle
ss -tlnp | grep -w 48080 && echo "BUSY - pick another port" || echo "48080 is free"
mkdir -p data

# create a user. The owner runs the command; do not keep the password in history or print it in chat.
# The variant below is acceptable only in an operator-only shell with history disabled during input.
docker run --rm amir20/dozzle:v10.6.6 generate admin --password '<PASSWORD_FROM_OPERATOR>' --name admin > data/users.yml
chmod 600 data/users.yml

docker compose -p dozzle up -d
iptables -C INPUT ! -i lo -p tcp --dport 48080 -j DROP 2>/dev/null || \
  iptables -I INPUT ! -i lo -p tcp --dport 48080 -j DROP
```

Check: bring up the SSH tunnel, open `http://127.0.0.1:48080` -> login form -> `admin` + password ->
the legacy prod/test containers `transcrib-*`/`transcrib-dev-*` are visible, if they are running. The
Vexa/svyazka containers must be absent (filter by container name `name=transcrib`).

After the Coolify cutover the containers got names like `app-<resource_uuid>` and do not match
`DOZZLE_FILTER=name=transcrib`. For Coolify prod/dev use the Coolify UI or:

```sh
deploy/safe-ssh.sh coolify-prod-app-logs 300
deploy/safe-ssh.sh coolify-prod-postgres-logs 100
deploy/safe-ssh.sh coolify-prod-migrate-logs 100
deploy/safe-ssh.sh coolify-dev-app-logs 200
deploy/safe-ssh.sh coolify-dev-postgres-logs 200
deploy/safe-ssh.sh coolify-dev-migrate-logs 200
```

Firewall/readiness check:

```sh
deploy/safe-ssh.sh dozzle-ps
deploy/safe-ssh.sh dozzle-firewall
curl --noproxy '*' -I --max-time 8 http://203.0.113.10:48080/  # must time out/fail
```

Do not treat Dozzle as a full viewer of the Coolify environment until the filter is explicitly updated.

## Operation

```sh
docker compose -p dozzle ps                                  # status (from this directory)
docker compose -p dozzle pull && docker compose -p dozzle up -d   # update the image
docker compose -p dozzle logs --tail 30                      # logs of the viewer itself (from the directory)
docker logs --tail 30 dozzle-dozzle-1                        # the same by container name (from any cwd)
docker compose -p dozzle down                                # stop
```

The real container name is `dozzle-dozzle-1` (compose adds a suffix to the service name).
`docker logs dozzle` fails with "No such container"; `docker compose -p dozzle ...` works only
from this directory (the compose file is needed).

Change the password: regenerate `data/users.yml` with the same `generate` command, then
`docker compose -p dozzle restart`.

## Important notes

- **ufw does not manage this port.** Docker publishes the port by editing iptables, bypassing ufw. So
  the Dozzle bind must stay `127.0.0.1:48080:8080`; `ufw deny 48080` is not sufficient protection.
- **Filter.** `DOZZLE_FILTER=name=transcrib` shows the legacy prod/test containers with names
  `transcrib-*` and `transcrib-dev-*`. Coolify prod/dev containers do not match this filter, because
  their names start with `app-`/`postgres-`/`migrate-` and the resource UUID. Neighboring services
  like `transcription-service`/`recording-processor` remain on the shared box; Vexa was stopped on
  2026-06-26. They are hidden by the filter.
- **The `:ro` socket** protects against replacing the socket file, but does NOT make the Docker API
  read-only. The real boundary: Dozzle does not proxy an arbitrary Docker API, and control
  (Actions/Shell) is off by default (`DOZZLE_ENABLE_ACTIONS`/`DOZZLE_ENABLE_SHELL` are not set). The
  surface boils down to "web authorization -> viewing filtered logs".
