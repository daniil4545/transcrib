# Документация transcrib

Канонический индекс. Все документы проекта живут здесь.

## Как читать этот индекс

У каждой темы есть **один канон** — место, где факт можно править. Остальные документы могут упоминать тот же факт, но обязаны ссылаться на канон, а не дублировать. Если меняется факт — правится только канон + список зеркал ниже, не больше.

## Каноны по темам

| Тема | Канон | Зеркала (ссылаются, не дублируют) |
|---|---|---|
| Продуктовые требования (F0–F9, антискоуп, метрики, дорожная карта, цена) | [`product.md`](product.md) | `architecture.md`, `../README.md` |
| Архитектура сервиса, модули, поток данных, state machine, критические узлы | [`architecture.md`](architecture.md) | `db.md`, `payments.md` |
| Схема БД, индексы, Go-типы для `domain.go`, транзакционные паттерны | [`db.md`](db.md) | `architecture.md` §5, `payments.md` |
| ЮKassa direct payment links: поток оплаты, чек 54-ФЗ, polling, refunds, ловушки | [`payments.md`](payments.md) | `architecture.md`, `db.md` §3.4/§5.4, `api/yookassa/payment_links.md` (актуальный REST-контракт), `api/yookassa/API.md` (legacy Telegram Payments) |
| Продуктовая аналитика воронки и UX-событий | [`analytics.md`](analytics.md) | GitHub Issues #39/#12, `analytics-dashboard.md` |
| Metabase dashboard для заказчика | [`analytics-dashboard.md`](analytics-dashboard.md) | `analytics.md` |
| Google OAuth + Drive: привязка аккаунта, доставка Google Doc, скоуп `drive.file`, refresh-токены, `/reconnect`, brand verification | [`google.md`](google.md) | `architecture.md` §14, `db.md` §3.2/§3.7, `api/google/API.md` |
| Процесс разработки (`возьми #NN` -> PR в `dev`, stop-gates) | [`workflow.md`](workflow.md) | - |
| Инфраструктура и deploy-решения (сервер, сеть, runtime, hardening backlog) | [`deployment.md`](deployment.md) | `../deploy/README.md`, `devops-practices.md` |
| API Speech2Text (экономически целевой provider) | [`api/speech2text/README.md`](api/speech2text/README.md) | `architecture.md` §9 |
| API mymeet.ai (рабочий fallback provider) | [`api/mymeet/README.md`](api/mymeet/README.md) | `architecture.md` §9 |
| API OpenRouter (целевой провайдер саммари F3, реализация `Summarizer`) | [`api/openrouter/README.md`](api/openrouter/README.md) | `architecture.md` §10, `summary.go` |

## Состав документации

### Каноны
- [`product.md`](product.md) — Паспорт продукта (F0–F9, антискоуп, метрики, дорожная карта).
- [`architecture.md`](architecture.md) — верхнеуровневая архитектура: модули `internal/app/`, runtime-поток, state machine Job, интерфейсы `TranscriptionProvider`/`Summarizer`/`DocumentDelivery`, критические алгоритмы.
- [`db.md`](db.md) — детальная схема БД, Go-типы, транзакционные паттерны (резерв квоты, идемпотентность платежей, recurring, refunds, F9-тики).
- [`payments.md`](payments.md) — интеграция ЮKassa direct payment links: поток оплаты, чек 54-ФЗ, polling, рекуррент, refunds, тестирование, ловушки. Актуальный REST-контракт — `api/yookassa/payment_links.md`; legacy Telegram Payments справочник — `api/yookassa/API.md`.
- [`analytics.md`](analytics.md) — runbook SQL-запросов для минимальной продуктовой аналитики воронки и UX-событий без внешнего BI.
- [`analytics-dashboard.md`](analytics-dashboard.md) — Metabase dashboard: PROD рабочий контур, DEV archive, карточки, data boundary и порядок изменений.
- [`google.md`](google.md) — интеграция Google OAuth + Drive: привязка аккаунта (`drive.file`), доставка Google Doc, refresh-токены (Testing mode), `/reconnect`/`awaiting_reauth`, конвертация markdown→Doc, путь в Production (brand verification), compliance.
- [`workflow.md`](workflow.md) - issue-driven workflow: команда `возьми #NN` означает готовность issue, автономное выполнение до PR в `dev`, ручные stop-gates для merge/prod/env/live DB/payment.

### Внешние интеграции
- [`api/speech2text/README.md`](api/speech2text/README.md) — **экономически целевой** поставщик транскрибации: досье (бизнес, тарифы, риски, вердикт).
- [`api/speech2text/API.md`](api/speech2text/API.md) — единый справочник контракта S2T (файловый flow + Meeting API + Webhook).
- [`api/mymeet/README.md`](api/mymeet/README.md) — **рабочий fallback provider**: досье.
- [`api/mymeet/API.md`](api/mymeet/API.md) — выжимка контракта mymeet под `mymeet.go`.
- [`api/mymeet/openapi.json`](api/mymeet/openapi.json) — оригинал OpenAPI-спеки mymeet (v1.1.0).
- [`api/openrouter/README.md`](api/openrouter/README.md) — **целевой провайдер саммари F3** (реализация `Summarizer` через `openrouter.go`): досье (роль, модель/экономика, 152-ФЗ).
- [`api/openrouter/API.md`](api/openrouter/API.md) — выжимка контракта OpenRouter под `openrouter.go`: OpenAI-совместимый `chat/completions`, тело запроса/ответа, `finish_reason`, коды ошибок, эскиз с map-reduce.
- [`api/google/API.md`](api/google/API.md) — единый справочник контракта Google: OAuth 2.0 Web Server flow (endpoints, offline+consent, refresh, `invalid_grant`), Drive API v3 (multipart upload + конвертация markdown→Doc, ошибки, лимиты), классификация скоупа `drive.file`, верификация, Go-уровень `x/oauth2`.
- [`api/yookassa/payment_links.md`](api/yookassa/payment_links.md) — актуальный контракт direct payment links: `POST /v3/payments`, `confirmation_url`, polling `GET /v3/payments/{id}`, receipt и refund по настоящему `payment.id`.
- [`api/yookassa/API.md`](api/yookassa/API.md) — legacy-справочник по прежней связке Telegram Payments + REST-refund: Basic-auth, `Idempotence-Key`, объекты `payment`/`refund`, `POST /v3/refunds`, чек 54-ФЗ, webhooks, тест-режим.

### Аналитика, тесты, схемы
- [`analytics-dashboard-cards.md`](analytics-dashboard-cards.md) — SQL-шаблон Metabase cards v0; канон процесса и data boundary — [`analytics-dashboard.md`](analytics-dashboard.md).
- `plans/` удалён вместе с переходом проекта на поддержку: слайс-спеки больше не ведутся, спека живёт
  в GitHub Issue. История планов (шаблон слайса, Google-слайс, протокол замеров s2t) доступна в git.
- [`testing.md`](testing.md) — канон покрытия тестами: карта «критический узел → тесты», E2E-чек-листы ручной проверки.
- `diagrams/` — компактный набор редактируемых draw.io-схем: `context.drawio` (System Context),
  `components.drawio` (Go app + Postgres + внешние сервисы), `flow-transcription.drawio`
  (основной поток встречи). SVG-превью не коммитятся, пока в окружении нет draw.io CLI.

### Дополнительное
- [`deployment.md`](deployment.md) — канон фактов и решений по инфраструктуре: общий сервер, сеть,
  runtime stack, deploy model, наблюдаемость, backup, секреты, hardening backlog. Операции и команды —
  [`../deploy/README.md`](../deploy/README.md).

## Порядок чтения для нового участника

1. [`product.md`](product.md) — что строим и зачем.
2. [`architecture.md`](architecture.md) — устройство сервиса.
3. [`db.md`](db.md) и [`payments.md`](payments.md) — глубокие детали по слоям.

## Правила точечной правки

- Меняется продуктовое требование → правится `product.md`. В зеркалах остаются ссылки.
- Меняется схема БД → правится `db.md` + `architecture.md` §5 (короткое перечисление) + `domain.go` после реализации.
- Меняется платёжный поток → правится `payments.md` + `db.md` §3.4/§5.4 (если затронуты).
- Документ-зеркало упоминает факт → формат `см. <канон> §N`. Никогда не копировать.
