#!/usr/bin/env bash
# Read-only post-deploy verification for Coolify dev/prod.
set -uo pipefail

readonly EXIT_FAILED=1
readonly EXIT_USAGE=2
readonly EXIT_OPERATIONAL=3
readonly EXIT_INCONCLUSIVE=4

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
safe_ssh="${VERIFY_DEPLOY_SAFE_SSH:-$script_dir/safe-ssh.sh}"
environment="${1:-}"
expected_input="${2:-}"

usage() {
    cat <<'USAGE'
Usage: deploy/verify-deploy.sh <dev|prod> <expected-sha>

expected-sha must be the full 40-character Git SHA, with optional sha- prefix.

Exit codes:
  0  PASS
  1  FAIL: runtime does not satisfy the deploy contract
  2  invalid arguments
  3  safe-ssh/local operational error
  4  INCONCLUSIVE: required evidence is unavailable
USAGE
}

if [[ "$environment" != "dev" && "$environment" != "prod" ]]; then
    usage >&2
    exit "$EXIT_USAGE"
fi
if [[ -z "$expected_input" || -n "${3:-}" ]]; then
    usage >&2
    exit "$EXIT_USAGE"
fi

expected_sha="${expected_input#sha-}"
if [[ ! "$expected_sha" =~ ^[0-9a-f]{40}$ ]]; then
    echo "FAIL input: expected-sha must be an unambiguous full 40-character lowercase Git SHA" >&2
    exit "$EXIT_USAGE"
fi
if [[ ! -x "$safe_ssh" ]]; then
    echo "FAIL operational: safe-ssh helper is not executable: $safe_ssh" >&2
    exit "$EXIT_OPERATIONAL"
fi

tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/verify-deploy.XXXXXX")" || {
    echo "FAIL operational: cannot create temporary directory" >&2
    exit "$EXIT_OPERATIONAL"
}
trap 'rm -rf -- "$tmp_dir"' EXIT

initial_status_file="$tmp_dir/status-initial"
run_file="$tmp_dir/current-run"
db_file="$tmp_dir/db"
final_status_file="$tmp_dir/status-final"
status_action="coolify-${environment}-deploy-status"
run_action="coolify-${environment}-current-run-evidence"
db_action="coolify-${environment}-db-status"

run_safe() {
    local action="$1"
    local output_file="$2"

    if ! "$safe_ssh" "$action" >"$output_file" 2>&1; then
        echo "FAIL operational: safe-ssh action failed: $action" >&2
        sed -n '1,80p' "$output_file" >&2
        exit "$EXIT_OPERATIONAL"
    fi
}

run_safe "$status_action" "$initial_status_file"
run_safe "$run_action" "$run_file"
run_safe "$db_action" "$db_file"
run_safe "$status_action" "$final_status_file"

echo "verify_environment=$environment"
echo "expected_sha=$expected_sha"
echo "initial_status_evidence:"
sed 's/^/  /' "$initial_status_file"
echo "current_run_evidence:"
sed 's/^/  /' "$run_file"
echo "db_evidence:"
sed 's/^/  /' "$db_file"
echo "final_status_evidence:"
sed 's/^/  /' "$final_status_file"

failures=0
inconclusive=0

fail() {
    echo "FAIL $*" >&2
    failures=$((failures + 1))
}

get_field() {
    local file="$1"
    local key="$2"
    local count

    count="$(grep -c "^${key}=" "$file" || true)"
    if [[ "$count" != "1" ]]; then
        return 1
    fi
    sed -n "s/^${key}=//p" "$file"
}

check_eq() {
    local file="$1"
    local key="$2"
    local expected="$3"
    local actual

    if ! actual="$(get_field "$file" "$key")"; then
        fail "$key: expected one evidence field"
    elif [[ "$actual" != "$expected" ]]; then
        fail "$key=$actual, expected $expected"
    fi
}

check_uint() {
    local file="$1"
    local key="$2"
    local actual

    if ! actual="$(get_field "$file" "$key")"; then
        fail "$key: expected one evidence field"
    elif [[ ! "$actual" =~ ^[0-9]+$ ]]; then
        fail "$key=$actual, expected a non-negative integer"
    fi
}

check_started_at() {
    local file="$1"
    local key="$2"
    local actual

    if ! actual="$(get_field "$file" "$key")"; then
        fail "$key: expected one evidence field"
    elif [[ ! "$actual" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?Z$ ]]; then
        fail "$key=$actual, expected Docker UTC RFC3339 timestamp"
    fi
}

check_status_snapshot() {
    local file="$1"

    check_eq "$file" resource "transcrib-$environment"
    check_eq "$file" app_container_count "1"
    check_eq "$file" app_image "ghcr.io/daniil4545/transcrib:sha-$expected_sha"
    check_eq "$file" app_revision "$expected_sha"
    check_eq "$file" app_running "true"
    check_eq "$file" app_status "running"
    check_eq "$file" app_health "healthy"
    check_eq "$file" app_restart_count "0"
    check_eq "$file" app_oom_killed "false"
    check_started_at "$file" app_started_at
    check_uint "$file" app_started_age_seconds

    check_eq "$file" postgres_container_count "1"
    check_eq "$file" postgres_running "true"
    check_eq "$file" postgres_status "running"
    check_eq "$file" postgres_health "healthy"
    check_eq "$file" postgres_restart_count "0"
    check_eq "$file" postgres_oom_killed "false"
    check_started_at "$file" postgres_started_at
    check_uint "$file" postgres_started_age_seconds
}

compare_snapshot_field() {
    local key="$1"
    local initial final

    initial="$(get_field "$initial_status_file" "$key" || true)"
    final="$(get_field "$final_status_file" "$key" || true)"
    if [[ -z "$initial" || -z "$final" ]]; then
        fail "$key: cannot compare initial and final snapshots"
    elif [[ "$initial" != "$final" ]]; then
        fail "$key changed during verification: initial=$initial final=$final"
    fi
}

check_status_snapshot "$initial_status_file"
check_status_snapshot "$final_status_file"

compare_snapshot_field app_id
compare_snapshot_field app_started_at
compare_snapshot_field app_image
compare_snapshot_field app_revision
compare_snapshot_field app_health
compare_snapshot_field postgres_id
compare_snapshot_field postgres_started_at
compare_snapshot_field postgres_health

initial_app_id="$(get_field "$initial_status_file" app_id || true)"
initial_app_started_at="$(get_field "$initial_status_file" app_started_at || true)"
initial_postgres_id="$(get_field "$initial_status_file" postgres_id || true)"
check_eq "$run_file" app_container_count "1"
check_eq "$run_file" evidence_app_id "$initial_app_id"
check_eq "$run_file" evidence_started_at "$initial_app_started_at"
check_eq "$run_file" evidence_finished_app_id "$initial_app_id"
check_eq "$run_file" evidence_finished_started_at "$initial_app_started_at"

startup_count="$(get_field "$run_file" startup_complete_count || true)"
if [[ ! "$startup_count" =~ ^[0-9]+$ ]]; then
    fail "startup_complete_count=$startup_count, expected a non-negative integer"
elif [[ "$startup_count" == "0" ]]; then
    fail "startup_complete was not found in logs of the current app run"
fi
check_eq "$run_file" run_error_count "0"

check_eq "$db_file" postgres_container_count "1"
check_eq "$db_file" db_postgres_id "$initial_postgres_id"
check_eq "$db_file" db_ready "1"
goose_version="$(get_field "$db_file" goose_version || true)"
if [[ ! "$goose_version" =~ ^[1-9][0-9]*$ ]]; then
    fail "goose_version=$goose_version, expected a positive integer"
fi

if [[ "$environment" == "dev" ]]; then
    migrate_count="$(get_field "$final_status_file" migrate_container_count || true)"
    if [[ "$migrate_count" == "0" ]]; then
        echo "INCONCLUSIVE migrate: Coolify removed the dev one-shot container; DB version alone does not prove this deploy exited 0" >&2
        inconclusive=1
    elif [[ "$migrate_count" != "1" ]]; then
        fail "migrate_container_count=$migrate_count, expected exactly one dev migrate container"
    else
        check_eq "$final_status_file" migrate_image "ghcr.io/daniil4545/transcrib:sha-$expected_sha"
        check_eq "$final_status_file" migrate_revision "$expected_sha"
        check_eq "$final_status_file" migrate_running "false"
        check_eq "$final_status_file" migrate_status "exited"
        check_eq "$final_status_file" migrate_exit_code "0"
        check_eq "$final_status_file" migrate_oom_killed "false"
        check_started_at "$final_status_file" migrate_started_at
    fi
fi

if (( failures > 0 )); then
    echo "RESULT=FAIL failures=$failures" >&2
    exit "$EXIT_FAILED"
fi
if (( inconclusive > 0 )); then
    echo "RESULT=INCONCLUSIVE" >&2
    exit "$EXIT_INCONCLUSIVE"
fi

echo "RESULT=PASS"
