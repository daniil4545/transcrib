# transcrib documentation

Canonical index. All project documents live here.

## How to read this index

Each topic has **one canon**: the place where the fact may be edited. Other documents may mention the same fact, but must link to the canon instead of duplicating it. When a fact changes, edit only the canon and the list of mirrors below, nothing more.

## Canons by topic

| Topic | Canon | Mirrors (link, do not duplicate) |
|---|---|---|
| Product requirements (F0-F9, anti-scope, metrics, roadmap, price) | [`product.md`](product.md) | `architecture.md`, `../README.md` |
| Service architecture, modules, data flow, state machine, critical nodes | [`architecture.md`](architecture.md) | `db.md`, `payments.md` |
| DB schema, indexes, Go types for `domain.go`, transactional patterns | [`db.md`](db.md) | `architecture.md` §5, `payments.md` |
| YooKassa direct payment links: payment flow, 54-FZ receipt, polling, refunds, pitfalls | [`payments.md`](payments.md) | `architecture.md`, `db.md` §3.4/§5.4, `api/yookassa/payment_links.md` (current REST contract), `api/yookassa/API.md` (legacy Telegram Payments) |
| Product analytics of the funnel and UX events | [`analytics.md`](analytics.md) | GitHub Issues #39/#12, `analytics-dashboard.md` |
| Metabase dashboard for the customer | [`analytics-dashboard.md`](analytics-dashboard.md) | `analytics.md` |
| Google OAuth + Drive: account linking, Google Doc delivery, `drive.file` scope, refresh tokens, `/reconnect`, brand verification | [`google.md`](google.md) | `architecture.md` §14, `db.md` §3.2/§3.7, `api/google/API.md` |
| Development process (`take #NN` -> PR into `dev`, stop-gates) | [`workflow.md`](workflow.md) | - |
| Infrastructure and deploy decisions (server, network, runtime, hardening backlog) | [`deployment.md`](deployment.md) | `../deploy/README.md`, `devops-practices.md` |
| Speech2Text API (economically preferred provider) | [`api/speech2text/README.md`](api/speech2text/README.md) | `architecture.md` §9 |
| mymeet.ai API (working fallback provider) | [`api/mymeet/README.md`](api/mymeet/README.md) | `architecture.md` §9 |
| OpenRouter API (target F3 summary provider, `Summarizer` implementation) | [`api/openrouter/README.md`](api/openrouter/README.md) | `architecture.md` §10, `summary.go` |

## Contents

### Canons
- [`product.md`](product.md) - product passport (F0-F9, anti-scope, metrics, roadmap).
- [`architecture.md`](architecture.md) - top-level architecture: `internal/app/` modules, runtime flow, Job state machine, the `TranscriptionProvider`/`Summarizer`/`DocumentDelivery` interfaces, critical algorithms.
- [`db.md`](db.md) - detailed DB schema, Go types, transactional patterns (quota reserve, payment idempotency, recurring, refunds, F9 ticks).
- [`payments.md`](payments.md) - YooKassa direct payment links integration: payment flow, 54-FZ receipt, polling, recurring, refunds, testing, pitfalls. The current REST contract is `api/yookassa/payment_links.md`; the legacy Telegram Payments reference is `api/yookassa/API.md`.
- [`analytics.md`](analytics.md) - SQL query runbook for minimal product analytics of the funnel and UX events without an external BI tool.
- [`analytics-dashboard.md`](analytics-dashboard.md) - Metabase dashboard: PROD working setup, DEV archive, cards, data boundary and change procedure.
- [`google.md`](google.md) - Google OAuth + Drive integration: account linking (`drive.file`), Google Doc delivery, refresh tokens (Testing mode), `/reconnect`/`awaiting_reauth`, markdown to Doc conversion, the path to Production (brand verification), compliance.
- [`workflow.md`](workflow.md) - issue-driven workflow: the `take #NN` command means the issue is ready, autonomous execution up to a PR into `dev`, manual stop-gates for merge/prod/env/live DB/payment.

### External integrations
- [`api/speech2text/README.md`](api/speech2text/README.md) - **economically preferred** transcription provider: dossier (business, tariffs, risks, verdict).
- [`api/speech2text/API.md`](api/speech2text/API.md) - single S2T contract reference (file flow + Meeting API + Webhook).
- [`api/mymeet/README.md`](api/mymeet/README.md) - **working fallback provider**: dossier.
- [`api/mymeet/API.md`](api/mymeet/API.md) - digest of the mymeet contract for `mymeet.go`.
- [`api/mymeet/openapi.json`](api/mymeet/openapi.json) - original mymeet OpenAPI spec (v1.1.0).
- [`api/openrouter/README.md`](api/openrouter/README.md) - **target F3 summary provider** (`Summarizer` implementation via `openrouter.go`): dossier (role, model/economics, 152-FZ).
- [`api/openrouter/API.md`](api/openrouter/API.md) - digest of the OpenRouter contract for `openrouter.go`: OpenAI-compatible `chat/completions`, request/response body, `finish_reason`, error codes, map-reduce sketch.
- [`api/google/API.md`](api/google/API.md) - single Google contract reference: OAuth 2.0 Web Server flow (endpoints, offline+consent, refresh, `invalid_grant`), Drive API v3 (multipart upload + markdown to Doc conversion, errors, limits), `drive.file` scope classification, verification, Go-level `x/oauth2`.
- [`api/yookassa/payment_links.md`](api/yookassa/payment_links.md) - current direct payment links contract: `POST /v3/payments`, `confirmation_url`, polling `GET /v3/payments/{id}`, receipt and refund by the real `payment.id`.
- [`api/yookassa/API.md`](api/yookassa/API.md) - legacy reference for the former Telegram Payments + REST refund setup: Basic auth, `Idempotence-Key`, `payment`/`refund` objects, `POST /v3/refunds`, 54-FZ receipt, webhooks, test mode.

### Analytics, tests, diagrams
- [`analytics-dashboard-cards.md`](analytics-dashboard-cards.md) - SQL template for Metabase cards v0; the canon for the process and data boundary is [`analytics-dashboard.md`](analytics-dashboard.md).
- `plans/` was removed when the project moved to maintenance: slice specs are no longer kept, the spec lives
  in the GitHub Issue. The plan history (slice template, Google slice, s2t measurement protocol) is available in git.
- [`testing.md`](testing.md) - test coverage canon: "critical node -> tests" map, manual-check E2E checklists.
- `diagrams/` - a compact set of editable draw.io diagrams: `context.drawio` (System Context),
  `components.drawio` (Go app + Postgres + external services), `flow-transcription.drawio`
  (main meeting flow). SVG previews are not committed until the environment has the draw.io CLI.

### Additional
- [`deployment.md`](deployment.md) - canon of infrastructure facts and decisions: shared server, network,
  runtime stack, deploy model, observability, backup, secrets, hardening backlog. Operations and commands:
  [`../deploy/README.md`](../deploy/README.md).

## Reading order for a new contributor

1. [`product.md`](product.md) - what we build and why.
2. [`architecture.md`](architecture.md) - how the service is built.
3. [`db.md`](db.md) and [`payments.md`](payments.md) - deep details by layer.

## Rules for targeted edits

- A product requirement changes: edit `product.md`. Mirrors keep only links.
- The DB schema changes: edit `db.md` + `architecture.md` §5 (short listing) + `domain.go` after implementation.
- The payment flow changes: edit `payments.md` + `db.md` §3.4/§5.4 (if affected).
- A mirror document mentions a fact: use the format `see <canon> §N`. Never copy.
