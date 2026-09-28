#!/usr/bin/env bash
# PostgreSQL backup for the prod transcrib Postgres container.
# Default mode is off-box over SSH. Temporary local mode is supported for the
# shared VPS until an external backup host exists.
set -euo pipefail

backup_mode="${TRANSCRIB_BACKUP_MODE:-ssh}"
container="${TRANSCRIB_POSTGRES_CONTAINER:-transcrib-postgres-1}"
postgres_resource="${TRANSCRIB_POSTGRES_RESOURCE:-}"
db_name="${TRANSCRIB_DB_NAME:-transcrib}"
db_user="${TRANSCRIB_DB_USER:-transcrib}"
ssh_host="${TRANSCRIB_BACKUP_SSH_HOST:-root@203.0.113.10}"
backup_dir="${TRANSCRIB_BACKUP_DIR:-}"
retention_days="${TRANSCRIB_BACKUP_RETENTION_DAYS:-14}"
min_bytes="${TRANSCRIB_BACKUP_MIN_BYTES:-1024}"
min_free_kb="${TRANSCRIB_BACKUP_MIN_FREE_KB:-0}"

usage() {
    cat <<'USAGE'
Usage:
  TRANSCRIB_BACKUP_DIR=/secure/offbox/path deploy/backup.sh
  TRANSCRIB_BACKUP_MODE=local TRANSCRIB_BACKUP_DIR=/var/backups/transcrib/postgres deploy/backup.sh

Creates a gzip-compressed pg_dump from prod Postgres over SSH.
In local mode, pg_dump is run directly on the current host.

Environment:
  TRANSCRIB_BACKUP_MODE            ssh (default) or local.
  TRANSCRIB_BACKUP_DIR             Required off-box directory for dumps.
  TRANSCRIB_BACKUP_SSH_HOST        SSH target, default root@203.0.113.10.
  TRANSCRIB_BACKUP_RETENTION_DAYS  Delete local dumps older than N days, default 14.
  TRANSCRIB_BACKUP_MIN_BYTES       Minimum gzip file size, default 1024.
  TRANSCRIB_BACKUP_MIN_FREE_KB     Required free space in backup filesystem, default 0.
  TRANSCRIB_POSTGRES_CONTAINER     Default transcrib-postgres-1.
  TRANSCRIB_POSTGRES_RESOURCE      Optional Coolify resourceName, for example transcrib-prod.
                                  When set, resolves the postgres container by Docker labels.
  TRANSCRIB_DB_NAME                Default transcrib.
  TRANSCRIB_DB_USER                Default transcrib.

Exit codes:
  0  backup completed
  2  bad configuration or arguments
  3  local dependency missing
  4  pg_dump/ssh/gzip failed
  5  created dump is empty, too small, or fails gzip validation
  6  retention cleanup failed
USAGE
}

fail() {
    local code="$1"
    shift
    echo "backup: $*" >&2
    exit "$code"
}

need_cmd() {
    command -v "$1" >/dev/null 2>&1 || fail 3 "missing required command: $1"
}

is_positive_int() {
    [[ "$1" =~ ^[0-9]+$ ]] && (( "$1" > 0 ))
}

is_nonnegative_int() {
    [[ "$1" =~ ^[0-9]+$ ]]
}

file_size() {
    local file="$1"
    if stat -c '%s' "$file" >/dev/null 2>&1; then
        stat -c '%s' "$file"
        return
    fi
    if stat -f '%z' "$file" >/dev/null 2>&1; then
        stat -f '%z' "$file"
        return
    fi
    wc -c <"$file" | tr -d ' '
}

free_space_kb() {
    local dir="$1"
    df -Pk "$dir" | awk 'NR == 2 {print $4}'
}

run_dump() {
    local remote_cmd_quoted
    local actual_container="$container"
    local dump_cmd

    dump_cmd=(
        docker exec "$container"
        pg_dump
        --no-owner
        --no-privileges
        --format=plain
        -U "$db_user"
        -d "$db_name"
    )

    case "$backup_mode" in
        ssh)
            remote_cmd_quoted="$(remote_dump_command)"
            ssh -o BatchMode=yes -o ConnectTimeout=10 "$ssh_host" "$remote_cmd_quoted"
            ;;
        local)
            if [[ -n "$postgres_resource" ]]; then
                actual_container="$(docker ps -q \
                    --filter "label=coolify.resourceName=$postgres_resource" \
                    --filter "label=com.docker.compose.service=postgres" | head -1)"
                [[ -n "$actual_container" ]] || fail 4 "postgres container not found for Coolify resource: $postgres_resource"
            fi
            dump_cmd[2]="$actual_container"
            "${dump_cmd[@]}"
            ;;
        *)
            fail 2 "TRANSCRIB_BACKUP_MODE must be ssh or local"
            ;;
    esac
}

remote_dump_command() {
    local db_user_q db_name_q container_q resource_filter_q service_filter_q

    printf -v db_user_q '%q' "$db_user"
    printf -v db_name_q '%q' "$db_name"

    if [[ -n "$postgres_resource" ]]; then
        printf -v resource_filter_q '%q' "label=coolify.resourceName=$postgres_resource"
        printf -v service_filter_q '%q' "label=com.docker.compose.service=postgres"
        # shellcheck disable=SC2016 # $container must expand on the remote host.
        printf 'container=$(docker ps -q --filter %s --filter %s | head -1); test -n "$container" || { echo "postgres container not found for Coolify resource" >&2; exit 1; }; exec docker exec "$container" pg_dump --no-owner --no-privileges --format=plain -U %s -d %s' \
            "$resource_filter_q" "$service_filter_q" "$db_user_q" "$db_name_q"
        return
    fi

    printf -v container_q '%q' "$container"
    printf 'exec docker exec %s pg_dump --no-owner --no-privileges --format=plain -U %s -d %s' \
        "$container_q" "$db_user_q" "$db_name_q"
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
    usage
    exit 0
fi

if (( "$#" > 0 )); then
    usage >&2
    exit 2
fi

[[ -n "$backup_dir" ]] || fail 2 "TRANSCRIB_BACKUP_DIR is required"
is_positive_int "$retention_days" || fail 2 "TRANSCRIB_BACKUP_RETENTION_DAYS must be a positive integer"
is_positive_int "$min_bytes" || fail 2 "TRANSCRIB_BACKUP_MIN_BYTES must be a positive integer"
is_nonnegative_int "$min_free_kb" || fail 2 "TRANSCRIB_BACKUP_MIN_FREE_KB must be a non-negative integer"

need_cmd gzip
need_cmd find
need_cmd date
need_cmd mkdir
need_cmd mktemp
need_cmd df
need_cmd awk
case "$backup_mode" in
    ssh)
        need_cmd ssh
        ;;
    local)
        need_cmd docker
        ;;
    *)
        fail 2 "TRANSCRIB_BACKUP_MODE must be ssh or local"
        ;;
esac

umask 077
mkdir -p "$backup_dir"
if (( min_free_kb > 0 )); then
    available_kb="$(free_space_kb "$backup_dir")"
    if (( available_kb < min_free_kb )); then
        fail 5 "not enough free space in backup filesystem: ${available_kb} KB, minimum ${min_free_kb} KB"
    fi
fi

timestamp="$(date -u '+%Y%m%dT%H%M%SZ')"
backup_file="$backup_dir/transcrib_${timestamp}.sql.gz"
tmp_file="$(mktemp "$backup_dir/.transcrib_${timestamp}.sql.gz.tmp.XXXXXX")"
source_label="$container"
if [[ -n "$postgres_resource" ]]; then
    source_label="resource:$postgres_resource"
fi

cleanup_tmp() {
    rm -f "$tmp_file"
}
trap cleanup_tmp EXIT

echo "backup: starting pg_dump from $source_label/$db_name to $backup_file (mode=$backup_mode)"
if ! run_dump | gzip -9 >"$tmp_file"; then
    fail 4 "pg_dump or gzip failed"
fi

if [[ ! -s "$tmp_file" ]]; then
    fail 5 "dump file is empty"
fi

size="$(file_size "$tmp_file")"
if (( size < min_bytes )); then
    fail 5 "dump file is too small: ${size} bytes, minimum ${min_bytes}"
fi

if ! gzip -t "$tmp_file"; then
    fail 5 "gzip validation failed"
fi

mv "$tmp_file" "$backup_file"
trap - EXIT

echo "backup: created $backup_file (${size} bytes)"

if ! find "$backup_dir" -type f -name 'transcrib_*.sql.gz' -mtime +"$retention_days" -print -exec rm -f {} +; then
    fail 6 "retention cleanup failed"
fi

echo "backup: retention cleanup completed, kept files newer than ${retention_days} days"
