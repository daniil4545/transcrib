# transcrib

A Telegram bot that sends a meeting bot to a call and returns a speaker-labeled transcript and an AI summary.

[![CI](https://github.com/daniil4545/transcrib/actions/workflows/ci.yml/badge.svg)](https://github.com/daniil4545/transcrib/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/daniil4545/transcrib)](https://github.com/daniil4545/transcrib/releases)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

You send a Zoom, Google Meet or Yandex Telemost link. The bot joins, records, and puts two files in the chat: the transcript and the summary with decisions and tasks. The service is an orchestrator: transcription comes from an external provider, and transcrib owns the job lifecycle, the minute quota, billing and delivery. Bot replies and the summary prompts are in Russian; code and documentation are in English.

## Demo

The dialog below is the output of one test, `TestDialog_MockHappyPath`. It drives a job through the whole state machine on a real PostgreSQL with the mock provider and the mock summarizer, so it needs no keys or accounts. The bot edits one status message as the job moves on.

```text
user: https://meet.google.com/abc-defg-hij
bot:  Ссылку получил. Отправляю бота на Google Meet.
bot:  (edit) Бот подключается к встрече.
bot:  (edit) Бот уже на встрече и записывает разговор.
bot:  (edit) Запись завершилась. Расшифровываю разговор.
bot:  (edit) Расшифровка готова. Готовлю краткий конспект.
bot:  (edit) Готово. Расшифровка и конспект ниже.
bot:  file summary_20261021_1219.md (# Конспект встречи)
bot:  file transcript_20261021_1219.md (# Транскрипт встречи)
```

File names carry the job creation time. The "My calls" panel lists past meetings, cancels an active recording, and re-delivers a transcript or a summary.

## Features

| Feature | In one phrase |
| --- | --- |
| Meeting recording | An external meeting bot joins Zoom, Google Meet or Yandex Telemost |
| Transcript and summary | Speaker-labeled transcript plus an AI summary, both delivered as files |
| Providers | Speech2Text, mymeet and a mock, switched by one environment variable |
| Summaries | OpenRouter, a mock, or off, behind the `Summarizer` interface |
| Minute quota | Reserved when the job is created, refunded on failure |
| Payments | YooKassa payment links with receipts, status polling, recurring payments with a switch-off button |
| Job control | Cancel an active recording, re-deliver files from "My calls" |

## Install

Requirements: Go 1.25, Docker (for PostgreSQL 16), a bot token from [@BotFather](https://t.me/BotFather) to run the live bot.

```bash
git clone https://github.com/daniil4545/transcrib.git
cd transcrib/src
go build ./...
```

The repository ships a `Dockerfile` for the container image and no prebuilt binaries.

## Quick start

All commands run from `src/`. This sequence needs no external keys.

```bash
docker compose up -d --wait
export DATABASE_URL='postgres://transcrib:transcrib@localhost:5432/transcrib?sslmode=disable'
make migrate-up
make test
```

Expected tail of the output:

```text
2026/10/21 16:20:37 OK   00015_recurring_profile_disable_erases_token.sql (1.27ms)
2026/10/21 16:20:37 goose: successfully migrated database to version: 15
go test ./...
ok  	github.com/daniil4545/transcrib/cmd/transcrib	(cached)
ok  	github.com/daniil4545/transcrib/internal/app	3.604s
```

Print the demo dialog again:

```bash
go test ./internal/app -run TestDialog -v
```

Start the bot with the mock provider and your own token:

```bash
BOT_TOKEN=<token from BotFather> TRANSCRIPTION_PROVIDER=mock make run
```

The first log line is `startup configured` with `"provider":"mock"`. Then the bot connects to Telegram; an invalid token stops it with `telegram: Unauthorized: invalid token specified (401)`.

## Configuration

Secrets are read from the environment only; `.env` files are git-ignored. The full list is in `src/.env.example`.

| Variable | Required | Meaning |
| --- | --- | --- |
| `DATABASE_URL` | yes | PostgreSQL connection string |
| `BOT_TOKEN` | yes | Telegram bot token |
| `TRANSCRIPTION_PROVIDER` | no | `mock` (default), `speech2text`, `mymeet` |
| `SUMMARIZER_PROVIDER` | no | `off` (default), `mock`, `openrouter` |
| `SPEECH2TEXT_API_KEY`, `MYMEET_API_KEY`, `OPENROUTER_API_KEY` | per provider | Keys for the chosen providers |
| `YOOKASSA_SHOP_ID`, `YOOKASSA_SECRET_KEY`, `PAYMENT_RETURN_URL` | only for payments | Must be set together |
| `RECURRING_PAYMENTS_ENABLED` | no | `false` by default |

## How it works

```mermaid
flowchart LR
    U[Telegram user] -->|meeting link| B[Bot, long polling]
    B -->|reserve minutes, insert job| DB[(PostgreSQL)]
    DB -->|claim with SKIP LOCKED| O[Orchestrator]
    O -->|submit, poll, fetch| P[Transcription provider]
    O -->|transcript| S[Summarizer]
    O -->|edit status, send files| U
    Y[YooKassa poller] -->|payment status| DB
```

PostgreSQL is both the store and the queue. The orchestrator claims rows, moves each job through `created, joining, recording, processing, summarizing, done` with guarded transitions, and edits one status message in the chat. Details: [`docs/architecture.md`](docs/architecture.md), [`docs/db.md`](docs/db.md).

## Engineering decisions

| Decision | Why | Rejected |
| --- | --- | --- |
| PostgreSQL as the queue and the job state machine, workers claim rows with `FOR UPDATE SKIP LOCKED` | One datastore, one transaction for "reserve quota and insert job"; no extra broker to run and back up for a single small service | Redis or a message broker: a second failure domain and no transactional link to billing |
| Quota is reserved when the job is created (up to 60 minutes) and refunded on failure, not charged at the end | Two messages sent in quick succession cannot both pass the balance check; the user row is locked inside the creation transaction | Charging in `done`: allows overspending under concurrent requests |
| Job status changes go through guarded transitions (`SetJobRecording` only from `joining`, and so on) | A late or repeated poll result cannot move a job backwards or refund it twice | Unconditional `UPDATE status = ...` |
| Payments are idempotent: unique `payment_intent_key` and provider payment id, `Idempotence-Key` on every YooKassa call, a pending row is written before the external call | A crash or a retry between "created at the provider" and "recorded locally" does not produce a second charge or a lost payment | Trusting a webhook only; there are no inbound ports, so status is polled |
| Long polling for Telegram, no inbound ports on the app | No public endpoint to expose or secure | Webhooks |
| Transcription and summary providers behind small interfaces (`TranscriptionProvider`, `Summarizer`, `DocumentDelivery`) | A provider swap is a config change and a restart, no rebuild | Hard-wiring one vendor |
| Deploys are manual and gated: a release tag, and `deploy/safe-ssh.sh coolify-prod-active-jobs` must report zero active jobs | A deploy restarts the worker; a recording in progress must not be cut | Auto-deploy of `main` |

## Tests

```bash
make migrate-up   # apply migrations to the database from DATABASE_URL
make test         # go test ./...
```

There are 228 tests. Without `DATABASE_URL` the integration tests skip (157 of 228), so a bare `go test ./...` passes without infrastructure but proves less. With a migrated database all 228 pass, and CI runs them against PostgreSQL 16. External services are replaced by `MockProvider`, a mock summarizer and recording fakes. What is tested and how: [`docs/testing.md`](docs/testing.md).

## Privacy and security

| Fact | Where it is checked |
| --- | --- |
| Secrets come from the environment; `.env` files are git-ignored | `.gitignore`, `LoadConfig` in `src/internal/app/config.go` |
| The saved payment method token is erased when auto-renewal is switched off | `src/migrations/00015_recurring_profile_disable_erases_token.sql`, `TestMigration00015_DisabledProfilesEraseTokenAndDownRestoresNotNull` |
| Card data never reaches the service: the card form is on the YooKassa side | [`docs/payments.md`](docs/payments.md) |
| The app opens no inbound ports | `src/docker-compose.prod.yml` (no `ports:`), long polling and status polling only |
| Meeting content leaves the system: it goes to the transcription provider and, with `SUMMARIZER_PROVIDER=openrouter`, to OpenRouter; the raw provider response and the summary are stored in the `jobs` table | `src/internal/app` providers, [`docs/db.md`](docs/db.md) |

## Limitations

- One worker process is assumed. Several instances are not supported: a `created` job can become claimable again before `SetJobSubmitted`.
- Delivery has a duplicate window: if the process crashes after a successful send but before the new status is written, the file can be sent twice.
- With the mymeet provider, `Cancel` does not stop the external bot.
- Graceful shutdown cancels the context and stops Telegram polling but does not drain in-flight operations.
- Google Drive delivery is designed but not implemented.

## Documentation

| Document | Contents |
| --- | --- |
| [`docs/README.md`](docs/README.md) | Index of all docs |
| [`docs/architecture.md`](docs/architecture.md) | Runtime flow, modules, state machine |
| [`docs/db.md`](docs/db.md) | Database schema and transactional patterns |
| [`docs/payments.md`](docs/payments.md) | YooKassa payment links |
| [`docs/testing.md`](docs/testing.md) | What is tested and how |
| [`docs/deployment.md`](docs/deployment.md) | Containers, Coolify, release deploys |
| [`deploy/README.md`](deploy/README.md) | Deploy runbooks, backup, verification |
| [`CHANGELOG.md`](CHANGELOG.md) | Release notes |

## License

[MIT](LICENSE)
