#!/usr/bin/env bash
# Read-only SSH helper for transcrib operations.
# It intentionally exposes named checks instead of arbitrary remote commands.
set -euo pipefail

host="${TRANSCRIB_SSH_HOST:-root@203.0.113.10}"
action="${1:-}"
arg="${2:-}"

usage() {
    cat <<'USAGE'
Usage: deploy/safe-ssh.sh <action> [args...]

Read-only actions:
  prod-ps             docker container status for prod
  prod-health         prod container status + deployed SHA + app image/status
  prod-deployed-sha   prod deployed SHA + app image/status
  prod-app-logs       app logs, optional tail lines (default 200, max 1000)
  prod-postgres-logs  postgres logs, optional tail lines (default 200, max 1000)
  prod-migrate-logs   migrate logs, optional tail lines (default 200, max 1000)
  test-ps             docker container status for test bot
  test-health         test container status + deployed SHA + app image/status
  test-deployed-sha   test deployed SHA + app image/status
  test-app-logs       test app logs, optional tail lines (default 200, max 1000)
  test-postgres-logs  test postgres logs, optional tail lines (default 200, max 1000)
  test-migrate-logs   test migrate logs, optional tail lines (default 200, max 1000)
  dozzle-ps           dozzle compose status
  dozzle-logs         dozzle logs, optional tail lines (default 100, max 500)
  dozzle-firewall     dozzle listener and firewall guard for public :48080
  analytics-ps        Metabase analytics compose status
  analytics-logs      Metabase logs, optional tail lines (default 200, max 1000)
  analytics-net       Metabase and prod postgres docker networks
  analytics-cards     Metabase dashboards, cards and their datasource
  analytics-queries   Metabase recent query executions and errors
  analytics-users     Metabase accounts, last logins and caching settings
  coolify-health      Coolify container status + local UI HEAD
  coolify-public      public HEAD check for coolify.example.com
  coolify-dev-ps      Coolify transcrib-dev container status
  coolify-dev-health  Coolify transcrib-dev status + image revision + log driver
  coolify-dev-deploy-status   Coolify dev deploy state without env/secrets
  coolify-dev-current-run-evidence current app-run startup/error evidence
  coolify-dev-db-status       Coolify dev DB readiness + goose version
  coolify-dev-app-logs       Coolify dev app logs, optional tail lines (default 200, max 1000)
  coolify-dev-postgres-logs  Coolify dev postgres logs, optional tail lines (default 200, max 1000)
  coolify-dev-migrate-logs   Coolify dev migrate logs, optional tail lines (default 200, max 1000)
  coolify-prod-ps      Coolify transcrib-prod container status
  coolify-prod-health  Coolify transcrib-prod status + image revision + log driver
  coolify-prod-deploy-status  Coolify prod deploy state without env/secrets
  coolify-prod-current-run-evidence current app-run startup/error evidence
  coolify-prod-db-status      Coolify prod DB readiness + goose version
  coolify-prod-active-jobs   Coolify prod active jobs gate; exits 1 when active calls exist
  coolify-prod-stats         Coolify prod read-only aggregates over last N days (default 7, max 90)
  coolify-prod-failures      Coolify prod failed jobs over last N days with truncated last_error (default 7, max 90)
  coolify-prod-job           Coolify prod single job row + its subscription quota, args: job id
  coolify-prod-freshness     Coolify prod newest row per analytics table
  coolify-prod-shutdown-notice  WRITE: shutdown notice to users active in 30 days, args: dry-run|send
  coolify-prod-app-logs       Coolify prod app logs, optional tail lines (default 200, max 1000)
  coolify-prod-postgres-logs  Coolify prod postgres logs, optional tail lines (default 200, max 1000)
  coolify-prod-migrate-logs   Coolify prod migrate logs, optional tail lines (default 200, max 1000)
  docker-inventory    running container names/images/status/ports
  mihomo-status       mihomo service/listeners/firewall-safe status
  mihomo-logs         mihomo journal, optional tail lines (default 100, max 500)
  openrouter-egress   direct vs mihomo-proxied OpenRouter reachability
  nginx-inventory     nginx enabled sites and Coolify vhost references
  system-failed       failed systemd units
  docker-events       transcrib container events by container name, optional hours back (default 24, max 168)
  docker-daemon-logs  Docker daemon journal, optional tail lines (default 200, max 1000)
  kernel-errors       kernel errors from journal, optional tail lines (default 200, max 1000)
  oom-kills           OOM-kill evidence from kernel journal, optional tail lines (default 200, max 1000)
  cron-logs           cron journal, optional tail lines (default 200, max 1000)
  backup-status       on-server backup cron/log/dump status
  backup-cron         on-server backup cron file content
  backup-log          on-server backup log, optional tail lines (default 100, max 500)
  nginx-logs          nginx journal, optional tail lines (default 200, max 1000)
  prod-app-journal      prod app journald logs, args: since [until]
  prod-postgres-journal prod postgres journald logs, args: since [until]
  test-app-journal      test app journald logs, args: since [until]
  test-postgres-journal test postgres journald logs, args: since [until]
  journal-usage         journalctl --disk-usage + retention config
  disk                disk and inode usage
  memory              memory and uptime
  nginx-status        nginx systemd status
  nginx-test          nginx config test
  site-head           HEAD checks for transcrib.example.com pages
  git-status          bare repo refs + deployed markers

Set TRANSCRIB_SSH_HOST to override root@203.0.113.10.
USAGE
}

tail_lines() {
    local value="${1:-200}"
    local max="${2:-1000}"
    if [[ ! "$value" =~ ^[0-9]+$ ]]; then
        echo "tail-lines must be a number" >&2
        exit 2
    fi
    if (( value > max )); then
        value="$max"
    fi
    echo "$value"
}

hours_back() {
    local value="${1:-24}"
    local max="${2:-168}"
    if [[ ! "$value" =~ ^[0-9]+$ ]]; then
        echo "hours must be a number" >&2
        exit 2
    fi
    if (( value == 0 )); then
        value="1"
    fi
    if (( value > max )); then
        value="$max"
    fi
    echo "$value"
}

days_back() {
    local value="${1:-7}"
    local max="${2:-90}"
    if [[ ! "$value" =~ ^[0-9]+$ ]]; then
        echo "days must be a number" >&2
        exit 2
    fi
    if (( value == 0 )); then
        value="1"
    fi
    if (( value > max )); then
        value="$max"
    fi
    echo "$value"
}

job_id() {
    local value="${1:-}"
    if [[ ! "$value" =~ ^[0-9]+$ ]]; then
        echo "job id must be a number" >&2
        exit 2
    fi
    echo "$value"
}

journal_range() {
    local tag="$1"
    local since="${2:-}"
    local until="${3:-}"
    local extra="${4:-}"
    local since_q until_q

    if [[ -z "$since" ]]; then
        echo "since is required" >&2
        exit 2
    fi
    if [[ -n "$extra" ]]; then
        echo "too many arguments" >&2
        exit 2
    fi

    printf -v since_q '%q' "$since"
    remote="journalctl -t $tag --since $since_q"
    if [[ -n "$until" ]]; then
        printf -v until_q '%q' "$until"
        remote+=" --until $until_q"
    fi
    remote+=" --no-pager -o short-iso"
}

coolify_deploy_status() {
    local resource="$1"

    remote="resource='$resource'; printf 'resource=%s\n' \"\$resource\"; "
    remote+="if ! app_ids=\$(docker ps -aq --filter \"label=coolify.resourceName=\$resource\" --filter 'label=com.docker.compose.service=app'); then echo 'docker ps app failed' >&2; exit 10; fi; "
    remote+="set -- \$app_ids; printf 'app_container_count=%s\n' \"\$#\"; if test \"\$#\" -eq 1; then app=\"\$1\"; "
    remote+="docker inspect -f 'app_id={{.Id}}{{println}}app_name={{.Name}}{{println}}app_image={{.Config.Image}}{{println}}app_revision={{index .Config.Labels \"org.opencontainers.image.revision\"}}{{println}}app_running={{.State.Running}}{{println}}app_status={{.State.Status}}{{println}}app_restart_count={{.RestartCount}}{{println}}app_oom_killed={{.State.OOMKilled}}{{println}}app_started_at={{.State.StartedAt}}' \"\$app\" || exit 11; "
    remote+="if app_health=\$(docker inspect -f '{{.State.Health.Status}}' \"\$app\" 2>/dev/null); then printf 'app_health=%s\n' \"\$app_health\"; elif docker inspect -f '{{.Id}}' \"\$app\" >/dev/null 2>&1; then echo 'app_health=missing'; else exit 11; fi; "
    remote+="started=\$(docker inspect -f '{{.State.StartedAt}}' \"\$app\") || exit 11; started_epoch=\$(date -d \"\$started\" +%s 2>/dev/null || true); now_epoch=\$(date +%s); if test -n \"\$started_epoch\"; then printf 'app_started_age_seconds=%s\n' \"\$((now_epoch - started_epoch))\"; else echo 'app_started_age_seconds=invalid'; fi; fi; "
    remote+="if ! postgres_ids=\$(docker ps -aq --filter \"label=coolify.resourceName=\$resource\" --filter 'label=com.docker.compose.service=postgres'); then echo 'docker ps postgres failed' >&2; exit 10; fi; "
    remote+="set -- \$postgres_ids; printf 'postgres_container_count=%s\n' \"\$#\"; if test \"\$#\" -eq 1; then postgres=\"\$1\"; "
    remote+="docker inspect -f 'postgres_id={{.Id}}{{println}}postgres_name={{.Name}}{{println}}postgres_running={{.State.Running}}{{println}}postgres_status={{.State.Status}}{{println}}postgres_restart_count={{.RestartCount}}{{println}}postgres_oom_killed={{.State.OOMKilled}}{{println}}postgres_started_at={{.State.StartedAt}}' \"\$postgres\" || exit 11; "
    remote+="if postgres_health=\$(docker inspect -f '{{.State.Health.Status}}' \"\$postgres\" 2>/dev/null); then printf 'postgres_health=%s\n' \"\$postgres_health\"; elif docker inspect -f '{{.Id}}' \"\$postgres\" >/dev/null 2>&1; then echo 'postgres_health=missing'; else exit 11; fi; "
    remote+="started=\$(docker inspect -f '{{.State.StartedAt}}' \"\$postgres\") || exit 11; started_epoch=\$(date -d \"\$started\" +%s 2>/dev/null || true); now_epoch=\$(date +%s); if test -n \"\$started_epoch\"; then printf 'postgres_started_age_seconds=%s\n' \"\$((now_epoch - started_epoch))\"; else echo 'postgres_started_age_seconds=invalid'; fi; fi; "
    remote+="if ! migrate_ids=\$(docker ps -aq --filter \"label=coolify.resourceName=\$resource\" --filter 'label=com.docker.compose.service=migrate'); then echo 'docker ps migrate failed' >&2; exit 10; fi; "
    remote+="set -- \$migrate_ids; printf 'migrate_container_count=%s\n' \"\$#\"; if test \"\$#\" -eq 1; then migrate=\"\$1\"; docker inspect -f 'migrate_id={{.Id}}{{println}}migrate_name={{.Name}}{{println}}migrate_image={{.Config.Image}}{{println}}migrate_revision={{index .Config.Labels \"org.opencontainers.image.revision\"}}{{println}}migrate_running={{.State.Running}}{{println}}migrate_status={{.State.Status}}{{println}}migrate_exit_code={{.State.ExitCode}}{{println}}migrate_oom_killed={{.State.OOMKilled}}{{println}}migrate_started_at={{.State.StartedAt}}' \"\$migrate\" || exit 11; fi"
}

coolify_current_run_evidence() {
    local resource="$1"

    remote="set -o pipefail || exit 10; resource='$resource'; "
    remote+="if ! app_ids=\$(docker ps -aq --filter \"label=coolify.resourceName=\$resource\" --filter 'label=com.docker.compose.service=app'); then echo 'docker ps app failed' >&2; exit 10; fi; "
    remote+="set -- \$app_ids; printf 'app_container_count=%s\n' \"\$#\"; if test \"\$#\" -ne 1; then echo 'evidence_app_id=unavailable'; echo 'evidence_started_at=unavailable'; echo 'evidence_finished_app_id=unavailable'; echo 'evidence_finished_started_at=unavailable'; echo 'startup_complete_count=unavailable'; echo 'run_error_count=unavailable'; exit 0; fi; app=\"\$1\"; "
    remote+="app_id=\$(docker inspect -f '{{.Id}}' \"\$app\") || exit 11; started=\$(docker inspect -f '{{.State.StartedAt}}' \"\$app\") || exit 11; printf 'evidence_app_id=%s\n' \"\$app_id\"; printf 'evidence_started_at=%s\n' \"\$started\"; "
    remote+="startup_count=\$(docker logs --since \"\$started\" \"\$app\" 2>&1 | awk '/startup_complete/ { count++ } END { print count + 0 }') || exit 12; "
    remote+="error_pattern='panic|fatal|409 conflict|terminated by other getupdates request|load config|config([^[:alnum:]]|uration[^[:alnum:]])*(error|failed|invalid)|((database|postgres|db)[^[:cntrl:]]*(error|failed|refused|unavailable))|((error|failed|refused)[^[:cntrl:]]*(database|postgres|db))'; "
    remote+="error_count=\$(docker logs --since \"\$started\" \"\$app\" 2>&1 | awk -v pattern=\"\$error_pattern\" 'tolower(\$0) ~ pattern { count++ } END { print count + 0 }') || exit 12; "
    remote+="finished_id=\$(docker inspect -f '{{.Id}}' \"\$app\") || exit 11; finished=\$(docker inspect -f '{{.State.StartedAt}}' \"\$app\") || exit 11; printf 'evidence_finished_app_id=%s\n' \"\$finished_id\"; printf 'evidence_finished_started_at=%s\n' \"\$finished\"; printf 'startup_complete_count=%s\n' \"\$startup_count\"; printf 'run_error_count=%s\n' \"\$error_count\"; "
    remote+="if test \"\$error_count\" -gt 0; then echo 'run_error_lines:'; docker logs --since \"\$started\" \"\$app\" 2>&1 | grep -Ei \"\$error_pattern\" | tail -20 || exit 12; fi"
}

coolify_db_status() {
    local resource="$1"

    remote="set -o pipefail || exit 10; resource='$resource'; "
    remote+="if ! postgres_ids=\$(docker ps -q --no-trunc --filter \"label=coolify.resourceName=\$resource\" --filter 'label=com.docker.compose.service=postgres'); then echo 'docker ps postgres failed' >&2; exit 10; fi; "
    remote+="set -- \$postgres_ids; printf 'postgres_container_count=%s\n' \"\$#\"; if test \"\$#\" -ne 1; then echo 'db_postgres_id=unavailable'; echo 'db_ready=0'; echo 'goose_version=unavailable'; exit 0; fi; postgres=\"\$1\"; printf 'db_postgres_id=%s\n' \"\$postgres\"; "
    remote+="if ! db_ready=\$(docker exec \"\$postgres\" psql -X -A -t -U transcrib -d transcrib -c 'SELECT 1;' 2>/dev/null | tr -d '[:space:]'); then echo 'docker exec DB readiness failed' >&2; exit 13; fi; "
    remote+="if test \"\$db_ready\" = 1; then echo 'db_ready=1'; else echo 'db_ready=0'; echo 'goose_version=unavailable'; exit 0; fi; "
    remote+="if ! goose_version=\$(docker exec \"\$postgres\" psql -X -A -t -U transcrib -d transcrib -c 'SELECT COALESCE(MAX(version_id) FILTER (WHERE is_applied), 0) FROM goose_db_version;' 2>/dev/null | tr -d '[:space:]'); then echo 'docker exec goose version failed' >&2; exit 13; fi; "
    remote+="if test -n \"\$goose_version\"; then printf 'goose_version=%s\n' \"\$goose_version\"; else echo 'goose_version=unavailable'; fi"
}

remote=""
case "$action" in
    prod-ps)
        remote="docker ps --filter 'label=com.docker.compose.project=transcrib' --format 'name={{.Names}} image={{.Image}} status={{.Status}}'"
        ;;
    prod-health)
        remote="cd /opt/transcrib && printf 'deployed_sha=' && { test -f .deployed-sha && cat .deployed-sha || git --git-dir=/srv/git/transcrib.git rev-parse --short=12 refs/heads/master; } && docker ps --filter 'label=com.docker.compose.project=transcrib' --format 'name={{.Names}} image={{.Image}} status={{.Status}}' && docker ps --filter 'name=^/transcrib-app-1$' --format 'app_image={{.Image}} app_status={{.Status}}'"
        ;;
    prod-deployed-sha)
        remote="cd /opt/transcrib && printf 'deployed_sha=' && { test -f .deployed-sha && cat .deployed-sha || git --git-dir=/srv/git/transcrib.git rev-parse --short=12 refs/heads/master; } && docker ps --filter 'name=^/transcrib-app-1$' --format 'app_image={{.Image}} app_status={{.Status}}'"
        ;;
    prod-app-logs)
        n="$(tail_lines "$arg" 1000)"
        remote="docker logs --tail $n transcrib-app-1"
        ;;
    prod-postgres-logs)
        n="$(tail_lines "$arg" 1000)"
        remote="docker logs --tail $n transcrib-postgres-1"
        ;;
    prod-migrate-logs)
        n="$(tail_lines "$arg" 1000)"
        remote="docker logs --tail $n transcrib-migrate-1"
        ;;
    test-ps)
        remote="docker ps --filter 'label=com.docker.compose.project=transcrib-dev' --format 'name={{.Names}} image={{.Image}} status={{.Status}}'"
        ;;
    test-health)
        remote="cd /opt/transcrib-dev && printf 'deployed_sha=' && { test -f .deployed-sha && cat .deployed-sha || git --git-dir=/srv/git/transcrib.git rev-parse --short=12 refs/heads/dev; } && docker ps --filter 'label=com.docker.compose.project=transcrib-dev' --format 'name={{.Names}} image={{.Image}} status={{.Status}}' && docker ps --filter 'name=^/transcrib-dev-app-1$' --format 'app_image={{.Image}} app_status={{.Status}}'"
        ;;
    test-deployed-sha)
        remote="cd /opt/transcrib-dev && printf 'deployed_sha=' && { test -f .deployed-sha && cat .deployed-sha || git --git-dir=/srv/git/transcrib.git rev-parse --short=12 refs/heads/dev; } && docker ps --filter 'name=^/transcrib-dev-app-1$' --format 'app_image={{.Image}} app_status={{.Status}}'"
        ;;
    test-app-logs)
        n="$(tail_lines "$arg" 1000)"
        remote="docker logs --tail $n transcrib-dev-app-1"
        ;;
    test-postgres-logs)
        n="$(tail_lines "$arg" 1000)"
        remote="docker logs --tail $n transcrib-dev-postgres-1"
        ;;
    test-migrate-logs)
        n="$(tail_lines "$arg" 1000)"
        remote="docker logs --tail $n transcrib-dev-migrate-1"
        ;;
    dozzle-ps)
        remote="cd /opt/transcrib/deploy/dozzle && docker compose -p dozzle ps"
        ;;
    dozzle-logs)
        n="$(tail_lines "${arg:-100}" 500)"
        remote="docker logs --tail $n dozzle-dozzle-1"
        ;;
    dozzle-firewall)
        remote="printf 'listeners\n'; ss -tlnp | grep -w 48080 || true; echo; printf 'iptables_input\n'; iptables -S INPUT | grep -- '--dport 48080' || true; echo; printf 'iptables_docker_user\n'; iptables -S DOCKER-USER | grep -- '--ctorigdstport 48080' || true"
        ;;
    analytics-ps)
        remote="cd /opt/transcrib-analytics-dev && docker compose -p transcrib-analytics-dev ps"
        ;;
    analytics-logs)
        n="$(tail_lines "$arg" 1000)"
        remote="docker logs --tail $n transcrib-analytics-dev-metabase-1"
        ;;
    analytics-net)
        remote="printf 'metabase_networks='; docker inspect -f '{{range \$k,\$v := .NetworkSettings.Networks}}{{\$k}} {{end}}' transcrib-analytics-dev-metabase-1; printf 'prod_postgres_networks='; p=\$(docker ps -aq --filter 'label=coolify.resourceName=transcrib-prod' --filter 'label=com.docker.compose.service=postgres' | head -1); test -n \"\$p\" && docker inspect -f '{{range \$k,\$v := .NetworkSettings.Networks}}{{\$k}} {{end}}' \"\$p\" || echo missing"
        ;;
    analytics-cards)
        remote="docker exec transcrib-analytics-dev-metabase-db-1 sh -c 'psql -U \$POSTGRES_USER -d \$POSTGRES_DB -c \"SELECT id, name, engine, is_audit FROM metabase_database ORDER BY id\" -c \"SELECT id, name, archived FROM report_dashboard ORDER BY id\" -c \"SELECT dc.dashboard_id, c.id, c.database_id, c.archived, left(c.name, 40) AS card FROM report_dashboardcard dc JOIN report_card c ON c.id = dc.card_id ORDER BY dc.dashboard_id, c.id\"'"
        ;;
    analytics-queries)
        remote="docker exec transcrib-analytics-dev-metabase-db-1 sh -c 'psql -U \$POSTGRES_USER -d \$POSTGRES_DB -c \"SELECT database_id, error IS NULL AS ok, count(*), max(started_at) AS last_run FROM query_execution WHERE started_at > current_date - 7 GROUP BY 1,2 ORDER BY 1,2\" -c \"SELECT started_at, card_id, running_time, left(error, 200) AS error FROM query_execution WHERE error IS NOT NULL ORDER BY started_at DESC LIMIT 10\"'"
        ;;
    analytics-users)
        remote="docker exec transcrib-analytics-dev-metabase-db-1 sh -c 'psql -U \$POSTGRES_USER -d \$POSTGRES_DB -c \"SELECT id, email, is_superuser, is_active, date_joined, last_login FROM core_user ORDER BY id\" -c \"SELECT key, left(value, 40) AS value FROM setting ORDER BY key\" -c \"SELECT started_at, executor_id, card_id, dashboard_id, cache_hit FROM query_execution ORDER BY started_at DESC LIMIT 15\"'"
        ;;
    coolify-health)
        remote="printf 'containers\n'; docker ps --format 'name={{.Names}} image={{.Image}} status={{.Status}} ports={{.Ports}}' | grep -Ei '^(name=)?coolify' || true; echo; printf 'local_ui\n'; curl -I --max-time 10 -H 'Host: coolify.example.com' -H 'X-Forwarded-Proto: https' http://127.0.0.1:8000/"
        ;;
    coolify-public)
        remote="curl -I --max-time 10 https://coolify.example.com/"
        ;;
    coolify-dev-ps)
        remote="docker ps -a --filter 'label=coolify.resourceName=transcrib-dev' --format 'service={{.Label \"com.docker.compose.service\"}} name={{.Names}} image={{.Image}} status={{.Status}}'"
        ;;
    coolify-dev-health)
        remote="docker ps -a --filter 'label=coolify.resourceName=transcrib-dev' --format 'service={{.Label \"com.docker.compose.service\"}} name={{.Names}} image={{.Image}} status={{.Status}}'; app=\$(docker ps -aq --filter 'label=coolify.resourceName=transcrib-dev' --filter 'label=com.docker.compose.service=app' | head -1); if test -n \"\$app\"; then docker inspect -f 'app_revision={{index .Config.Labels \"org.opencontainers.image.revision\"}} app_log_driver={{.HostConfig.LogConfig.Type}} app_restart={{.HostConfig.RestartPolicy.Name}}' \"\$app\"; else echo 'app_container=missing'; fi"
        ;;
    coolify-dev-deploy-status)
        coolify_deploy_status "transcrib-dev"
        ;;
    coolify-dev-current-run-evidence)
        coolify_current_run_evidence "transcrib-dev"
        ;;
    coolify-dev-db-status)
        coolify_db_status "transcrib-dev"
        ;;
    coolify-dev-app-logs)
        n="$(tail_lines "$arg" 1000)"
        remote="c=\$(docker ps -aq --filter 'label=coolify.resourceName=transcrib-dev' --filter 'label=com.docker.compose.service=app' | head -1); test -n \"\$c\" || { echo 'Coolify dev app container not found' >&2; exit 1; }; docker logs --tail $n \"\$c\""
        ;;
    coolify-dev-postgres-logs)
        n="$(tail_lines "$arg" 1000)"
        remote="c=\$(docker ps -aq --filter 'label=coolify.resourceName=transcrib-dev' --filter 'label=com.docker.compose.service=postgres' | head -1); test -n \"\$c\" || { echo 'Coolify dev postgres container not found' >&2; exit 1; }; docker logs --tail $n \"\$c\""
        ;;
    coolify-dev-migrate-logs)
        n="$(tail_lines "$arg" 1000)"
        remote="c=\$(docker ps -aq --filter 'label=coolify.resourceName=transcrib-dev' --filter 'label=com.docker.compose.service=migrate' | head -1); test -n \"\$c\" || { echo 'Coolify dev migrate container not found' >&2; exit 1; }; docker logs --tail $n \"\$c\""
        ;;
    coolify-prod-ps)
        remote="docker ps -a --filter 'label=coolify.resourceName=transcrib-prod' --format 'service={{.Label \"com.docker.compose.service\"}} name={{.Names}} image={{.Image}} status={{.Status}}'"
        ;;
    coolify-prod-health)
        remote="containers=\$(docker ps -aq --filter 'label=coolify.resourceName=transcrib-prod'); if test -z \"\$containers\"; then echo 'resource=transcrib-prod status=missing'; exit 0; fi; docker ps -a --filter 'label=coolify.resourceName=transcrib-prod' --format 'service={{.Label \"com.docker.compose.service\"}} name={{.Names}} image={{.Image}} status={{.Status}}'; app=\$(docker ps -aq --filter 'label=coolify.resourceName=transcrib-prod' --filter 'label=com.docker.compose.service=app' | head -1); postgres=\$(docker ps -aq --filter 'label=coolify.resourceName=transcrib-prod' --filter 'label=com.docker.compose.service=postgres' | head -1); if test -n \"\$app\"; then docker inspect -f 'app_revision={{index .Config.Labels \"org.opencontainers.image.revision\"}} app_log_driver={{.HostConfig.LogConfig.Type}} app_restart={{.HostConfig.RestartPolicy.Name}}' \"\$app\"; else echo 'app_container=missing'; fi; if test -n \"\$postgres\"; then docker inspect -f 'postgres_log_driver={{.HostConfig.LogConfig.Type}} postgres_restart={{.HostConfig.RestartPolicy.Name}}' \"\$postgres\"; else echo 'postgres_container=missing'; fi"
        ;;
    coolify-prod-deploy-status)
        coolify_deploy_status "transcrib-prod"
        ;;
    coolify-prod-current-run-evidence)
        coolify_current_run_evidence "transcrib-prod"
        ;;
    coolify-prod-db-status)
        coolify_db_status "transcrib-prod"
        ;;
    coolify-prod-active-jobs)
        remote="postgres=\$(docker ps -q --filter 'label=coolify.resourceName=transcrib-prod' --filter 'label=com.docker.compose.service=postgres' | head -1); test -n \"\$postgres\" || { echo 'Coolify prod postgres container not found' >&2; exit 1; }; statuses=\"'created','joining','recording','processing','transcribed','summarizing','awaiting_reauth'\"; active_count=\$(docker exec \"\$postgres\" psql -X -A -t -U transcrib -d transcrib -c \"SELECT count(*) FROM jobs WHERE status IN (\$statuses);\"); echo \"active_jobs_count=\$active_count\"; docker exec \"\$postgres\" psql -X -U transcrib -d transcrib -c \"SELECT id, status, provider, created_at, updated_at, round(EXTRACT(EPOCH FROM (now() - updated_at)) / 60)::int AS age_min FROM jobs WHERE status IN (\$statuses) ORDER BY updated_at DESC LIMIT 20;\"; test \"\$active_count\" = 0"
        ;;
    coolify-prod-stats)
        d="$(days_back "${arg:-7}" 90)"
        remote="postgres=\$(docker ps -q --filter 'label=coolify.resourceName=transcrib-prod' --filter 'label=com.docker.compose.service=postgres' | head -1); test -n \"\$postgres\" || { echo 'Coolify prod postgres container not found' >&2; exit 1; }; echo \"period_days=$d\"; echo; echo '== JOBS by status =='; docker exec \"\$postgres\" psql -X -U transcrib -d transcrib -c \"SELECT status, count(*) AS jobs, coalesce(round(avg(duration_seconds)/60.0,1),0) AS avg_min, coalesce(sum(billed_minutes),0) AS billed_min FROM jobs WHERE created_at >= now() - INTERVAL '$d days' GROUP BY status ORDER BY jobs DESC;\"; echo '== JOBS trial split =='; docker exec \"\$postgres\" psql -X -U transcrib -d transcrib -c \"SELECT is_trial, count(*) AS jobs, coalesce(sum(billed_minutes),0) AS billed_min FROM jobs WHERE created_at >= now() - INTERVAL '$d days' GROUP BY is_trial ORDER BY is_trial;\"; echo '== PAYMENTS by status (rub) =='; docker exec \"\$postgres\" psql -X -U transcrib -d transcrib -c \"SELECT status, count(*) AS cnt, coalesce(round(sum(amount)/100.0,2),0) AS rub FROM payments WHERE created_at >= now() - INTERVAL '$d days' GROUP BY status ORDER BY cnt DESC;\"; echo '== REFUNDS by status (rub) =='; docker exec \"\$postgres\" psql -X -U transcrib -d transcrib -c \"SELECT status, count(*) AS cnt, coalesce(round(sum(amount)/100.0,2),0) AS rub FROM refunds WHERE created_at >= now() - INTERVAL '$d days' GROUP BY status ORDER BY cnt DESC;\"; echo '== SUBSCRIPTIONS =='; docker exec \"\$postgres\" psql -X -U transcrib -d transcrib -c \"SELECT (SELECT count(*) FROM subscriptions WHERE expires_at > now() AND minutes_balance > 0) AS active_now, (SELECT count(*) FROM subscriptions WHERE paid_at >= now() - INTERVAL '$d days') AS new_in_period;\"; echo '== USERS =='; docker exec \"\$postgres\" psql -X -U transcrib -d transcrib -c \"SELECT (SELECT count(*) FROM users WHERE created_at >= now() - INTERVAL '$d days') AS new_users, (SELECT count(DISTINCT user_id) FROM jobs WHERE created_at >= now() - INTERVAL '$d days') AS active_users;\""
        ;;
    coolify-prod-failures)
        d="$(days_back "${arg:-7}" 90)"
        remote="postgres=\$(docker ps -q --filter 'label=coolify.resourceName=transcrib-prod' --filter 'label=com.docker.compose.service=postgres' | head -1); test -n \"\$postgres\" || { echo 'Coolify prod postgres container not found' >&2; exit 1; }; echo \"period_days=$d\"; docker exec \"\$postgres\" psql -X -U transcrib -d transcrib -c \"SELECT id, user_id, status, provider, created_at, updated_at, duration_seconds, billed_minutes, left(last_error, 400) AS last_error FROM jobs WHERE status IN ('failed','failed_join','failed_summary') AND created_at >= now() - INTERVAL '$d days' ORDER BY created_at DESC LIMIT 50;\""
        ;;
    coolify-prod-job)
        j="$(job_id "$arg")"
        remote="postgres=\$(docker ps -q --filter 'label=coolify.resourceName=transcrib-prod' --filter 'label=com.docker.compose.service=postgres' | head -1); test -n \"\$postgres\" || { echo 'Coolify prod postgres container not found' >&2; exit 1; }; docker exec \"\$postgres\" psql -X -U transcrib -d transcrib -c \"SELECT id, user_id, status, provider, external_job_id, is_trial, subscription_id, reserved_minutes, billed_minutes, duration_seconds, created_at, updated_at, left(last_error, 200) AS last_error FROM jobs WHERE id = $j;\"; docker exec \"\$postgres\" psql -X -U transcrib -d transcrib -c \"SELECT s.id, s.minutes_balance, s.minutes_limit, s.meetings_balance, s.meetings_limit, s.expires_at FROM subscriptions s JOIN jobs j ON j.subscription_id = s.id WHERE j.id = $j;\""
        ;;
    coolify-prod-freshness)
        remote="postgres=\$(docker ps -q --filter 'label=coolify.resourceName=transcrib-prod' --filter 'label=com.docker.compose.service=postgres' | head -1); test -n \"\$postgres\" || { echo 'Coolify prod postgres container not found' >&2; exit 1; }; docker exec \"\$postgres\" psql -X -U transcrib -d transcrib -c \"SELECT 'users' AS t, count(*) AS rows, max(created_at) AS newest FROM users UNION ALL SELECT 'jobs', count(*), max(created_at) FROM jobs UNION ALL SELECT 'payments', count(*), max(created_at) FROM payments UNION ALL SELECT 'subscriptions', count(*), max(paid_at) FROM subscriptions UNION ALL SELECT 'refunds', count(*), max(created_at) FROM refunds UNION ALL SELECT 'analytics_events', count(*), max(occurred_at) FROM analytics_events ORDER BY 1;\""
        ;;
    coolify-prod-shutdown-notice)
        # Разовая рассылка о закрытии бота активным за 30 дней и держателям подписки.
        # Токен берётся из env контейнера app и не печатается; chat_id тоже.
        # Адрес Telegram прибит к IP так же, как extra_hosts в Compose.
        [[ "$arg" == dry-run || "$arg" == send ]] || { echo "mode must be dry-run or send" >&2; exit 2; }
        notice="Здравствуйте! Бот «transcrib» прекращает работу: новые встречи и оплаты больше не принимаются, автопродление отключено.

Если у вас оплачена подписка или встреча не обработалась, напишите в Telegram @dasukhar или на support@example.com, и мы вернём деньги.

Спасибо, что пользовались transcrib."
        notice_b64="$(printf '%s' "$notice" | base64 | tr -d '\n')"
        remote="set -o pipefail; postgres=\$(docker ps -q --filter 'label=coolify.resourceName=transcrib-prod' --filter 'label=com.docker.compose.service=postgres' | head -1); app=\$(docker ps -q --filter 'label=coolify.resourceName=transcrib-prod' --filter 'label=com.docker.compose.service=app' | head -1); test -n \"\$postgres\" -a -n \"\$app\" || { echo 'Coolify prod containers not found' >&2; exit 1; }; "
        remote+="ids=\$(docker exec \"\$postgres\" psql -X -A -t -U transcrib -d transcrib -c \"SELECT telegram_id FROM users WHERE created_at >= now() - INTERVAL '30 days' UNION SELECT user_id FROM jobs WHERE created_at >= now() - INTERVAL '30 days' UNION SELECT user_id FROM subscriptions WHERE expires_at > now();\") || exit 13; "
        remote+="set -- \$ids; echo \"recipients=\$#\"; test '$arg' = send || exit 0; "
        remote+="token=\$(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' \"\$app\" | sed -n 's/^BOT_TOKEN=//p'); test -n \"\$token\" || { echo 'BOT_TOKEN not found' >&2; exit 1; }; text=\$(printf '%s' '$notice_b64' | base64 -d); ok=0; failed=0; "
        remote+="for id; do code=\$(curl -s -o /dev/null -w '%{http_code}' --max-time 15 --resolve api.telegram.org:443:149.154.167.220 --data-urlencode \"chat_id=\$id\" --data-urlencode \"text=\$text\" \"https://api.telegram.org/bot\$token/sendMessage\"); if test \"\$code\" = 200; then ok=\$((ok + 1)); else failed=\$((failed + 1)); echo \"send_failed http=\$code\"; fi; sleep 0.2; done; echo \"sent_ok=\$ok sent_failed=\$failed\""
        ;;
    coolify-prod-app-logs)
        n="$(tail_lines "$arg" 1000)"
        remote="c=\$(docker ps -aq --filter 'label=coolify.resourceName=transcrib-prod' --filter 'label=com.docker.compose.service=app' | head -1); test -n \"\$c\" || { echo 'Coolify prod app container not found' >&2; exit 1; }; docker logs --tail $n \"\$c\""
        ;;
    coolify-prod-postgres-logs)
        n="$(tail_lines "$arg" 1000)"
        remote="c=\$(docker ps -aq --filter 'label=coolify.resourceName=transcrib-prod' --filter 'label=com.docker.compose.service=postgres' | head -1); test -n \"\$c\" || { echo 'Coolify prod postgres container not found' >&2; exit 1; }; docker logs --tail $n \"\$c\""
        ;;
    coolify-prod-migrate-logs)
        n="$(tail_lines "$arg" 1000)"
        remote="c=\$(docker ps -aq --filter 'label=coolify.resourceName=transcrib-prod' --filter 'label=com.docker.compose.service=migrate' | head -1); test -n \"\$c\" || { echo 'Coolify prod migrate container not found' >&2; exit 1; }; docker logs --tail $n \"\$c\""
        ;;
    docker-inventory)
        remote="docker ps --format 'name={{.Names}} image={{.Image}} status={{.Status}} ports={{.Ports}}' | sort"
        ;;
    mihomo-status)
        remote="printf 'unit\n'; systemctl is-enabled mihomo.service 2>/dev/null || true; systemctl is-active mihomo.service 2>/dev/null || true; systemctl show mihomo.service -p LoadState -p ActiveState -p SubState -p UnitFileState -p ExecMainPID --no-pager 2>/dev/null || true; echo; printf 'listeners\n'; ss -tlnp | grep -E ':20170|:20171|:9090' || true; echo; printf 'ufw\n'; ufw status verbose 2>/dev/null | grep -E '20170|20171|Status|Default' || true; echo; printf 'public_20171\n'; curl --noproxy '*' -I --max-time 5 http://203.0.113.10:20171/ 2>&1 | sed -n '1,8p'"
        ;;
    mihomo-nodes)
        # Агрегат по нодам подписки с контроллера: сколько живых, что выбрано в
        # ключевых группах. Имена нод не печатаем (deploy/logs.md).
        remote="curl -s -m 10 http://127.0.0.1:9090/proxies | python3 -c \"import sys,json
d=json.load(sys.stdin)['proxies']
nodes=[p for p in d.values() if p.get('type') in ('Vless','Vmess','Trojan','Shadowsocks','Hysteria2','Ss','Tuic')]
alive=[p for p in nodes if p.get('history') and p['history'][-1].get('delay',0)>0]
print('nodes_total=%d' % len(nodes))
print('nodes_alive=%d' % len(alive))
print('last_delays=%s' % sorted({p['history'][-1]['delay'] for p in alive})[:5])
for g in ('AI','Auto-Best','Fallback-Stable','Manual','Telegram'):
    p=d.get(g)
    if not p: continue
    now=d.get(p.get('now'),{})
    h=now.get('history') or []
    print('group=%s type=%s now_type=%s last_checks=%s' % (g, p.get('type'), now.get('type'), [x.get('delay') for x in h[-3:]]))
\""
        ;;
    mihomo-routing)
        # Конфиг печатается с маскировкой credential-полей: маршрутизацию видно,
        # subscription-ссылка и ключи нод наружу не идут (deploy/logs.md).
        remote="cfg=''; for p in /etc/mihomo/config.yaml /etc/mihomo/config.yml /root/.config/mihomo/config.yaml /opt/mihomo/config.yaml; do test -f \"\$p\" && { cfg=\"\$p\"; break; }; done; test -n \"\$cfg\" || { echo 'mihomo config not found' >&2; exit 1; }; printf 'config_path=%s\n' \"\$cfg\"; printf 'config_lines=%s\n' \"\$(wc -l < \"\$cfg\")\"; echo; sed -E 's/((password|uuid|token|secret|auth-str|api-key|username|url|path)[[:space:]]*:[[:space:]]*).*/\1***/I' \"\$cfg\" | head -300"
        ;;
    mihomo-logs)
        n="$(tail_lines "${arg:-100}" 500)"
        remote="journalctl -u mihomo.service -n $n --no-pager -o short-iso | sed -E 's#vless://[^[:space:]]+#vless://<redacted>#g; s#vmess://[^[:space:]]+#vmess://<redacted>#g; s#trojan://[^[:space:]]+#trojan://<redacted>#g; s#ss://[^[:space:]]+#ss://<redacted>#g; s#hysteria2://[^[:space:]]+#hysteria2://<redacted>#g'"
        ;;
    openrouter-egress)
        remote="printf 'direct\n'; curl --noproxy '*' -sS -o /dev/null -D - --max-time 15 https://openrouter.ai/api/v1/models | sed -n '1,10p'; echo; printf 'via_mihomo\n'; HTTPS_PROXY=http://10.0.0.1:20171 curl -sS -o /dev/null -D - --max-time 30 https://openrouter.ai/api/v1/models | sed -n '1,12p'"
        ;;
    nginx-inventory)
        remote="printf 'enabled_sites\n'; find /etc/nginx/sites-enabled -maxdepth 1 -type l -printf '%f -> %l\n' 2>/dev/null | sort; echo; printf 'coolify_refs\n'; grep -InE 'coolify.example.com|127\.0\.0\.1:8000|localhost:8000' /etc/nginx/sites-enabled/* /etc/nginx/sites-available/* /etc/nginx/conf.d/* 2>/dev/null || true"
        ;;
    system-failed)
        remote="systemctl --failed --no-pager --plain"
        ;;
    docker-events)
        hours="$(hours_back "${arg:-24}" 168)"
        remote="since=\$(date -u -d '$hours hours ago' '+%Y-%m-%dT%H:%M:%SZ'); until=\$(date -u '+%Y-%m-%dT%H:%M:%SZ'); docker events --since \"\$since\" --until \"\$until\" --filter type=container --format '{{.Time}} {{.Type}} {{.Action}} name={{.Actor.Attributes.name}} image={{.Actor.Attributes.image}} project={{index .Actor.Attributes \"com.docker.compose.project\"}} resource={{index .Actor.Attributes \"coolify.resourceName\"}} service={{index .Actor.Attributes \"com.docker.compose.service\"}}' | grep -E 'name=transcrib(-dev)?-|project=transcrib(-dev)?( |$)|resource=transcrib-(dev|prod)' | grep -Ev ' container exec_(create|start|die):? ' || true"
        ;;
    docker-daemon-logs)
        n="$(tail_lines "$arg" 1000)"
        remote="journalctl -u docker --since '24 hours ago' -n $n --no-pager -o short-iso"
        ;;
    kernel-errors)
        n="$(tail_lines "$arg" 1000)"
        remote="journalctl -k -p err..alert --since '24 hours ago' -n $n --no-pager -o short-iso"
        ;;
    oom-kills)
        n="$(tail_lines "$arg" 1000)"
        remote="journalctl -k --since '7 days ago' --no-pager -o short-iso | grep -Ei 'out of memory|oom-kill|killed process' | tail -n $n || true"
        ;;
    cron-logs)
        n="$(tail_lines "$arg" 1000)"
        remote="journalctl -u cron --since '24 hours ago' -n $n --no-pager -o short-iso"
        ;;
    backup-status)
        remote="backup_dir=/var/backups/transcrib/postgres; printf 'cron_file='; test -f /etc/cron.d/transcrib-backup && echo present || echo missing; printf 'backup_dir='; test -d \"\$backup_dir\" && echo present || echo missing; printf 'backup_log='; test -f \"\$backup_dir/backup.log\" && ls -lh \"\$backup_dir/backup.log\" | awk '{print \$5, \$6, \$7, \$8}' || echo missing; latest_epoch=\$(find \"\$backup_dir\" -maxdepth 1 -type f -name 'transcrib_*.sql.gz' -printf '%T@\n' 2>/dev/null | sort -n | tail -1); printf 'latest_backup_age_seconds='; if test -n \"\$latest_epoch\"; then now_epoch=\$(date +%s); awk -v now=\"\$now_epoch\" -v latest=\"\$latest_epoch\" 'BEGIN { printf \"%d\n\", now - latest }'; else echo missing; fi; echo 'recent_backups:'; find \"\$backup_dir\" -maxdepth 1 -type f -name 'transcrib_*.sql.gz' -printf '%TY-%Tm-%Td %TH:%TM %s bytes %f\n' 2>/dev/null | sort | tail -10"
        ;;
    backup-cron)
        remote="test -f /etc/cron.d/transcrib-backup && sed -n '/^[^#].*backup.sh/p' /etc/cron.d/transcrib-backup || echo 'cron file missing: /etc/cron.d/transcrib-backup'"
        ;;
    backup-log)
        n="$(tail_lines "${arg:-100}" 500)"
        remote="test -f /var/backups/transcrib/postgres/backup.log && tail -n $n /var/backups/transcrib/postgres/backup.log || echo 'backup.log missing: /var/backups/transcrib/postgres/backup.log'"
        ;;
    nginx-logs)
        n="$(tail_lines "$arg" 1000)"
        remote="journalctl -u nginx --since '24 hours ago' -n $n --no-pager -o short-iso"
        ;;
    prod-app-journal)
        journal_range "transcrib-transcrib-app-1" "${2:-}" "${3:-}" "${4:-}"
        ;;
    prod-postgres-journal)
        journal_range "transcrib-transcrib-postgres-1" "${2:-}" "${3:-}" "${4:-}"
        ;;
    test-app-journal)
        journal_range "transcrib-transcrib-dev-app-1" "${2:-}" "${3:-}" "${4:-}"
        ;;
    test-postgres-journal)
        journal_range "transcrib-transcrib-dev-postgres-1" "${2:-}" "${3:-}" "${4:-}"
        ;;
    journal-usage)
        remote="journalctl --disk-usage --no-pager; echo; printf 'retention_conf='; if test -r /etc/systemd/journald.conf.d/transcrib-retention.conf; then echo present; cat /etc/systemd/journald.conf.d/transcrib-retention.conf; else echo missing; fi"
        ;;
    disk)
        remote="df -h; echo; df -ih"
        ;;
    memory)
        remote="free -h; echo; uptime"
        ;;
    nginx-status)
        remote="systemctl status nginx --no-pager"
        ;;
    nginx-test)
        remote="nginx -t"
        ;;
    site-head)
        remote="curl -I --max-time 10 https://transcrib.example.com/; echo; curl -I --max-time 10 https://transcrib.example.com/privacy/; echo; curl -I --max-time 10 https://transcrib.example.com/offer/; echo; curl -I --max-time 10 https://transcrib.example.com/contacts/"
        ;;
    git-status)
        remote="git --git-dir=/srv/git/transcrib.git show-ref --heads master dev; echo; printf 'prod_deployed_sha='; { test -f /opt/transcrib/.deployed-sha && cat /opt/transcrib/.deployed-sha || echo missing; }; printf 'test_deployed_sha='; { test -f /opt/transcrib-dev/.deployed-sha && cat /opt/transcrib-dev/.deployed-sha || echo missing; }"
        ;;
    ""|-h|--help|help)
        usage
        exit 0
        ;;
    *)
        echo "unknown action: $action" >&2
        usage >&2
        exit 2
        ;;
esac

# Проверяем именно отпечаток: любая загруженная личность агента раньше проходила
# эту проверку, и после снятия старых ключей это давало Permission denied вместо
# понятной ошибки.
deploy_key_fp="SHA256:PlUcGZ4y2NsdM97PXCvYH1Uz006KxeD2xULAl1rI4IE"

if ! ssh-add -l 2>/dev/null | grep -qF "$deploy_key_fp"; then
    cat >&2 <<'ERROR'
The deploy key is not loaded in ssh-agent.
Load it before starting Codex:
  ssh-add ~/.ssh/id_ed25519

safe-ssh.sh intentionally does not read private key files directly.
ERROR
    exit 3
fi

ssh -o BatchMode=yes -o ConnectTimeout=10 "$host" "$remote"
