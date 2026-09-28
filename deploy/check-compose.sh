#!/bin/sh

set -eu

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
repo_dir=$(dirname -- "$script_dir")
compose_file=${1:-"$repo_dir/src/docker-compose.coolify.yml"}
env_file="$repo_dir/src/.env.example"

if [ ! -f "$compose_file" ]; then
	echo "Compose file not found: $compose_file" >&2
	exit 1
fi

exclude_count=$(
	awk '$1 == "exclude_from_hc:" && $2 == "true" { count++ } END { print count + 0 }' \
		"$compose_file"
)
if [ "$exclude_count" -ne 1 ]; then
	echo "Expected exactly one 'exclude_from_hc: true', found $exclude_count" >&2
	exit 1
fi

env_file_count=$(
	awk '$1 == "env_file:" && $2 == ".env" && NF == 2 { count++ } END { print count + 0 }' \
		"$compose_file"
)
if [ "$env_file_count" -ne 1 ]; then
	echo "Expected exactly one 'env_file: .env', found $env_file_count" >&2
	exit 1
fi

compose_dir=$(CDPATH='' cd -- "$(dirname -- "$compose_file")" && pwd)
compose_name=$(basename -- "$compose_file")
temp_file=$(mktemp "$compose_dir/.${compose_name}.check.XXXXXX")
trap 'rm -f "$temp_file"' EXIT HUP INT TERM

awk '
	$1 == "exclude_from_hc:" && $2 == "true" { next }
	$1 == "env_file:" && $2 == ".env" && NF == 2 {
		sub(/env_file:[[:space:]]*\.env[[:space:]]*$/, "env_file: .env.example")
	}
	{ print }
' "$compose_file" >"$temp_file"

APP_IMAGE=ghcr.io/example/transcrib:compose-check \
POSTGRES_PASSWORD=compose-check \
DATABASE_URL='postgres://transcrib:compose-check@postgres:5432/transcrib?sslmode=disable' \
	docker compose \
		--env-file "$env_file" \
		-f "$temp_file" \
		config --quiet --no-env-resolution
