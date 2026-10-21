<div align="center">

# transcrib

**A Telegram bot that turns an online meeting into a speaker-labeled transcript and an AI summary with decisions and action items.**

You send a meeting link, the bot joins, records, and returns the transcript and the summary to the chat.

![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white)
![PostgreSQL](https://img.shields.io/badge/PostgreSQL-16-4169E1?logo=postgresql&logoColor=white)
![Telegram](https://img.shields.io/badge/telebot-v4-26A5E4?logo=telegram&logoColor=white)
![Docker](https://img.shields.io/badge/Docker-compose-2496ED?logo=docker&logoColor=white)
![License](https://img.shields.io/badge/license-MIT-blue)

</div>

## What it does

transcrib joins meetings in Zoom, Google Meet, and Yandex Telemost, records the conversation, and sends two files to Telegram: a transcript split by speaker and an AI summary with decisions and tasks.

The service is an integration orchestrator. It has no ASR of its own and no meeting bot of its own: an external transcription provider does the heavy work, and transcrib owns the job lifecycle, billing, and delivery. It is a production service with a paid subscription (1990 RUB per month for 15 hours, 3 hours of trial). Internal accounting is in minutes.

User-facing texts (bot replies, buttons, the LLM summary prompts) are in Russian, because the product serves Russian-speaking users. Code, comments, and documentation are in English.

## Usage

```text
user:  https://zoom.us/j/...            (a meeting link sent to the bot)
bot:   status message, updated as the job moves through
       created, joining, recording, processing, summarizing
bot:   transcript file (by speaker) + summary file (decisions, tasks)
```

The "My calls" panel lists past meetings, lets the user cancel an active recording, and re-deliver a transcript or a summary.

## Features

- Meeting recording through an external meeting bot.
- Speaker-labeled transcript and an AI summary.
- File delivery to Telegram.
- Speech2Text and mymeet as swappable transcription providers, switched by one environment variable.
- OpenRouter as the summary provider for the Speech2Text path.
- YooKassa direct payment links, 54-FZ fiscal receipts, payment status polling, recurring payments with a self-service switch-off button.
- "My calls" panel: cancel an active recording, re-deliver a transcript or summary.

## Engineering decisions

| Decision | Why | Rejected |
| --- | --- | --- |
| PostgreSQL as the queue and the job state machine, workers claim rows with `FOR UPDATE SKIP LOCKED` | One datastore, one transaction for "reserve quota and insert job"; no extra broker to run and back up for a single small service | Redis or a message broker: a second failure domain and no transactional link to billing |
| Quota is reserved when the job is created (up to 60 minutes) and refunded on failure, not charged at the end | Two messages sent in quick succession cannot both pass the balance check; the user row is locked inside the creation transaction | Charging in `done`: allows overspending under concurrent requests |
| Job status changes go through guarded transitions (`SetJobRecording` only from `joining`, and so on) | A late or repeated poll result cannot move a job backwards or refund it twice | Unconditional `UPDATE status = ...` |
| Payments are idempotent: unique `payment_intent_key` and provider payment id, `Idempotence-Key` on every YooKassa call, a pending row is written before the external call | A crash or a retry between "created at the provider" and "recorded locally" does not produce a second charge or a lost payment | Trusting a webhook only; there are no inbound ports, so status is polled |
| Long polling for Telegram, no inbound ports on the app | No public endpoint to expose or secure; simpler deploy behind Coolify | Webhooks |
| Transcription and summary providers behind small interfaces (`TranscriptionProvider`, `Summarizer`, `DocumentDelivery`) | A provider swap is a config change and a restart, no rebuild | Hard-wiring one vendor |
| Production deploys are manual and gated: a release tag, and `deploy/safe-ssh.sh coolify-prod-active-jobs` must report zero active jobs | A deploy restarts the worker; a recording in progress must not be cut | Auto-deploy of `main` |

Details and the reasoning behind each choice are in [`docs/architecture.md`](docs/architecture.md) and [`docs/db.md`](docs/db.md).

## Tech stack

| Layer | Choice |
| --- | --- |
| Language | Go 1.25 |
| Database / queue | PostgreSQL 16, `FOR UPDATE SKIP LOCKED` |
| Driver / migrations | `pgx/v5`, `goose` |
| Telegram | `telebot.v4`, long polling |
| Containers | Docker + docker-compose |
| Dev deploy | GitHub Actions to GHCR to Coolify |
| Prod deploy | manual deploy of a release tag only |

## Quick start

You need Docker and a bot token from [@BotFather](https://t.me/BotFather). All commands run from `src/`.

```bash
git clone git@github.com:daniil4545/transcrib.git
cd transcrib/src

cp .env.example .env
docker compose up -d
make migrate-up
make run
```

`mock` mode needs no external keys. The main switches:

| Variable | Values | Default |
| --- | --- | --- |
| `TRANSCRIPTION_PROVIDER` | `mock`, `speech2text`, `mymeet` | `mock` |
| `SUMMARIZER_PROVIDER` | `off`, `mock`, `openrouter` | `off` |

The full list of variables is in [`src/.env.example`](src/.env.example); development commands are in `src/Makefile`.

## Tests

```bash
cd src
make migrate-up      # apply migrations to the database from DATABASE_URL
make test            # go test ./...
```

Integration tests need PostgreSQL: without `DATABASE_URL` they are skipped (about two thirds of the tests), so a bare `go test ./...` passes without infrastructure but proves less. With a migrated database all tests run. What is tested and how: [`docs/testing.md`](docs/testing.md).

## Repository layout

```text
transcrib/
├── docs/        # canonical docs: product, architecture, database, payments, tests
├── deploy/      # deploy runbooks, Coolify CD, backup, deploy verification
└── src/         # Go module: cmd/transcrib, internal/app, migrations
```

## Deployment

`dev` passes GitHub Actions automatically, publishes an image to GHCR, and rolls out to the Coolify service `transcrib-dev`. Production is deployed rarely: a release PR from `dev` to `main`, a Git tag, and a manual run of the `Deploy` workflow with `target=prod`. Before a prod deploy the stop gate `deploy/safe-ssh.sh coolify-prod-active-jobs` is mandatory: if `active_jobs_count` is not `0`, the deploy is postponed. The app has no inbound ports: the bot uses long polling and PostgreSQL is internal.

Operations entry point: [`deploy/README.md`](deploy/README.md). Infrastructure canon: [`docs/deployment.md`](docs/deployment.md).

## Documentation

| Document | Contents |
| --- | --- |
| [`docs/README.md`](docs/README.md) | Index of the canonical docs |
| [`docs/product.md`](docs/product.md) | Product passport and anti-scope |
| [`docs/architecture.md`](docs/architecture.md) | Runtime flow, modules, state machine |
| [`docs/db.md`](docs/db.md) | Database schema and transactional patterns |
| [`docs/payments.md`](docs/payments.md) | YooKassa direct payment links |
| [`docs/testing.md`](docs/testing.md) | What is tested and how |
| [`docs/workflow.md`](docs/workflow.md) | Development process |

## Status and limitations

Production runs release tag `v0.1.0`. Direct payment links, `RECURRING_PAYMENTS_ENABLED=true`, and the self-service button that turns off auto-renewal passed a live smoke test on 2026-07-09. Product analytics in Metabase read production through a read-only datasource.

Known limits, also listed in [`docs/architecture.md`](docs/architecture.md):

- One worker process is assumed. Running several instances is not supported as a guarantee: a `created` job can become claimable again before `SetJobSubmitted`.
- Delivery has a duplicate window: if the process crashes after a successful send but before the new status is written, the file can be sent twice.
- With the mymeet provider, `Cancel` does not stop the external bot.
- Graceful shutdown cancels the context and stops Telegram polling but does not drain in-flight operations.
- Google Drive delivery is designed but not implemented.

## License

[MIT](LICENSE)
