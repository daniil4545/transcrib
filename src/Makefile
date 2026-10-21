# DATABASE_URL and BOT_TOKEN are taken from the environment. If a .env file is nearby,
# it is picked up automatically (make reads the file, the values are visible in run/migrate).
ifneq ($(NO_DOTENV),1)
    ENV_FILE := $(firstword $(wildcard .env ../.env))
    ifneq (,$(ENV_FILE))
        -include $(ENV_FILE)
        export
    endif
endif
# Optional override (e.g. a local test): loaded ON TOP of .env and overrides its
# values. Non-secret switches only; secrets stay in .env.
# Usage: make run ENV_OVERRIDE=.env.test
ifneq ($(NO_DOTENV),1)
    ifneq (,$(ENV_OVERRIDE))
        -include $(ENV_OVERRIDE)
        export
    endif
endif

GOOSE := go run github.com/pressly/goose/v3/cmd/goose@v3.27.1
MIGRATIONS_DIR := ./migrations

.PHONY: run build test test-fast fmt-check vet lint commit-check deploy-check ci-check live-summary migrate-up migrate-down check-db

run:
	go run ./cmd/transcrib

build:
	go build ./...

test:
	go test ./...

# Fast tier for the loop inside a task: only what does not touch the DB.
test-fast:
	go test -short ./... $(if $(RUN),-run $(RUN) -count=1,)

vet:
	go vet ./...

# Nobody else catches formatting: gofmt is neither in the default golangci-lint
# set nor in the file-edit hook.
fmt-check:
	@out=$$(gofmt -l ./cmd ./internal); \
	test -z "$$out" || { echo "gofmt needed:"; echo "$$out"; exit 1; }

lint:
	golangci-lint run --timeout 120s

# Commit ring: seconds, no DB. Run by a hook before every git commit
# with .go files in the index. The full set closes a task: ci-check.
#
# test-fast is not included here: the project tests are not split into fast and DB-dependent,
# `go test -short` still goes to Postgres and fails without a running compose.
# Splitting them is a separate task; until then ci-check covers the tests.
commit-check: fmt-check vet lint

deploy-check:
	../deploy/check-compose.sh

ci-check: migrate-up deploy-check
	go test ./...
	go vet ./...
	go build ./...

# Live OpenRouter call with the key from .env (build tag live, a regular test does not touch it).
live-summary:
	go test -tags live -run TestOpenRouter_Live -v -count=1 ./internal/app

# Diagnostics: which bot is behind BOT_TOKEN (@username). The token does not reach the output
# (@ suppresses the recipe echo, the getMe response does not contain the token).
bot-whoami:
	@curl -s "https://api.telegram.org/bot$(BOT_TOKEN)/getMe" | grep -o '"username":"[^"]*"\|"ok":[a-z]*\|"description":"[^"]*"'

check-db:
	@test -n "$(DATABASE_URL)" || { echo "DATABASE_URL is not set"; exit 1; }

migrate-up: check-db
	$(GOOSE) -dir $(MIGRATIONS_DIR) postgres "$(DATABASE_URL)" up

migrate-down: check-db
	$(GOOSE) -dir $(MIGRATIONS_DIR) postgres "$(DATABASE_URL)" down
