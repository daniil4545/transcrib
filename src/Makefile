# DATABASE_URL и BOT_TOKEN берутся из окружения. Если рядом лежит .env —
# подхватываем автоматически (make читает файл, значения видны в run/migrate).
ifneq ($(NO_DOTENV),1)
    ENV_FILE := $(firstword $(wildcard .env ../.env))
    ifneq (,$(ENV_FILE))
        -include $(ENV_FILE)
        export
    endif
endif
# Опциональный оверрайд (напр. локальный тест): грузится ПОВЕРХ .env и перебивает
# значения. Только не-секретные переключатели; секреты остаются в .env.
# Использование: make run ENV_OVERRIDE=.env.test
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

# Быстрый ярус для петли внутри задачи: только то, что не ходит в БД.
test-fast:
	go test -short ./... $(if $(RUN),-run $(RUN) -count=1,)

vet:
	go vet ./...

# Форматирование не ловит никто другой: gofmt нет ни в дефолтном наборе
# golangci-lint, ни в хуке на правку файла.
fmt-check:
	@out=$$(gofmt -l ./cmd ./internal); \
	test -z "$$out" || { echo "gofmt needed:"; echo "$$out"; exit 1; }

lint:
	golangci-lint run --timeout 120s

# Кольцо коммита: секунды, без БД. Запускается хуком перед каждым git commit
# с .go в индексе. Полный набор закрывает задачу - ci-check.
#
# test-fast сюда не входит: тесты проекта не разделены на быстрые и БД-зависимые,
# `go test -short` всё равно идёт в Postgres и падает без поднятого compose.
# Разделить их - отдельная задача; до тех пор тесты закрывает ci-check.
commit-check: fmt-check vet lint

deploy-check:
	../deploy/check-compose.sh

ci-check: migrate-up deploy-check
	go test ./...
	go vet ./...
	go build ./...

# Живой вызов OpenRouter ключом из .env (build-тег live, обычный test его не трогает).
live-summary:
	go test -tags live -run TestOpenRouter_Live -v -count=1 ./internal/app

# Диагностика: какой бот за BOT_TOKEN (@username). Токен в вывод не попадает
# (@ глушит эхо рецепта, ответ getMe токена не содержит).
bot-whoami:
	@curl -s "https://api.telegram.org/bot$(BOT_TOKEN)/getMe" | grep -o '"username":"[^"]*"\|"ok":[a-z]*\|"description":"[^"]*"'

check-db:
	@test -n "$(DATABASE_URL)" || { echo "DATABASE_URL is not set"; exit 1; }

migrate-up: check-db
	$(GOOSE) -dir $(MIGRATIONS_DIR) postgres "$(DATABASE_URL)" up

migrate-down: check-db
	$(GOOSE) -dir $(MIGRATIONS_DIR) postgres "$(DATABASE_URL)" down
