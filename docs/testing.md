# Тестирование transcrib

Канон покрытия. Ведёт агент `tester`. Принцип проекта: **точечные тесты только на критические узлы**, остальное — ручной E2E. Меньше тестов, ловящих реальные баги, лучше, чем много ради coverage.

Стек тестов: stdlib `testing`, table-driven + `t.Run`. Интеграционные тесты критических узлов бьют в реальный Postgres через `DATABASE_URL`; без него — `t.Skip` (голый `go test` работает без инфры). Внешние сервисы — `MockProvider` и recording-фейки. Локальный DB-backed запуск: `docker compose up -d`, затем `NO_DOTENV=1 DATABASE_URL=... make ci-check` из `src/`; без `DATABASE_URL` интеграционные тесты будут пропущены. CI workflow и Deploy workflow поднимают PostgreSQL 16, выставляют `DATABASE_URL`, выполняют `make migrate-up`, затем `go test ./...`; Deploy workflow не собирает image, если DB-backed gate не прошел.

## Типы проверок и change gate

| Type | Что означает | Кто меняет | Change gate |
|---|---|---|---|
| `contract` | Тест защищает канонный инвариант из `docs/db.md`, `docs/architecture.md`, `docs/payments.md` или `docs/product.md`. | Не implementation-agent в одиночку. | Только вместе с изменением канона/issue-спеки или явным owner approval в PR. |
| `acceptance` | Тест или smoke прямо зафиксирован в issue как acceptance contract до реализации. | Независимый Tester/reviewer. | Нельзя удалять или ослаблять в implementation PR без owner approval. |
| `implementation` | Локальный helper/parser/format/config тест, полезный для разработки, но не самостоятельная приемка. | Автор кода. | Обычный code review; не подменяет acceptance. |
| `runtime` | Живой dev/test smoke для внешней границы: Telegram, provider, OpenRouter, ЮKassa, Coolify/env. | Tester/DevOps с артефактами. | В PR/release report: SHA, job/payment id, команды, логовое окно, результат. |
| `gap` | Контракт есть, автоматической или runtime-проверки пока нет. | Через follow-up issue. | Не закрывать как "покрыто"; добавить acceptance contract перед реализацией. |
| `noise candidate` | Проверка выглядит слабосвязанной с контрактом или дублирует более сильный тест. | Только отдельным cleanup issue. | Не удалять в feature PR "заодно". |

Для денег, квот, state machine, миграций, идемпотентности, refund/recurring и provider/deploy
контрактов acceptance пишет или проверяет независимый Tester/reviewer. Если тест оказался неверным,
меняется issue-спека или канон, а не тест молча подгоняется под implementation.

## Audit map

Источник истины по критическим узлам: `docs/db.md` §5 и `docs/architecture.md` §8. В таблице ниже
`Change gate = canon/owner` означает: тесты нельзя ослаблять без изменения канона/issue-спеки или
явного owner approval.

| Contract source | Инвариант | Tests/smoke | Type | Change gate |
|---|---|---|---|---|
| `architecture.md` §8.1, `db.md` §5.1 | `CreateJob` атомарно резервирует минуты, сериализует пользователя, не допускает двойной расход квоты. | `storage_quota_test.go`: `TestCreateJob_TrialRace`, `TestCreateJob_SubscriptionRace`, `TestCreateJob_SubscriptionPriorityOverTrial`, `TestCreateJob_SelectsFullSubscriptionBeforeOlderPartial`, partial-minute tests. | `contract` | `canon/owner` |
| `db.md` §5.8, `architecture.md` §8.1 | Разные активные URL разрешены в пределах квоты и cap=4; тот же URL режется до расхода квоты. | `storage_quota_test.go`: `TestCreateJob_RateLimit*`, `TestCreateJob_ActiveCap*`; `storage_phases_test.go`: `TestCreateJob_RateLimit_Phases`. | `contract` | `canon/owner` |
| `architecture.md` §8.2 | Worker claim не выдаёт один Job двум воркерам; backoff держится через `updated_at`. | `storage_test.go`: `TestClaimJobsForWork_*`, `TestClaimJobsForWork_NoDoubleClaim`. | `contract` | `canon/owner` |
| `architecture.md` §8.2 | Submit/poll failure paths переводят Job в правильные статусы и возвращают квоту только до оказания услуги. | `orchestrator_test.go`: `TestSubmit_*`, `TestPoll_JoiningStartTimeoutCancelsAndRefunds`, `TestPollJob_StaleTimeout`, `TestPollJob_FreshJobNotStale`. | `contract` | `canon/owner` |
| `architecture.md` §7/§8.2, `db.md` §5.3 | State machine не двигает terminal Job повторно; фазовые переходы `joining/recording/processing/summarizing/done` идемпотентны. | `orchestrator_test.go`: `TestAdvance_*`, `TestPhaseFlow_*`; `storage_phases_test.go`: `TestSetJobRecording_Phase`, `TestSetJobProcessing_Phase`, `TestSetJobDone_From*`. | `contract` | `canon/owner` |
| `architecture.md` §8.3, `db.md` §5.2 | Empty transcript не списывает квоту, не запускает summary и закрывает Job как failed. | `speech2text_test.go`: `TestSpeech2TextFetch_EmptyTranscript`; `orchestrator_test.go`: `TestFinishTranscription_EmptyTranscriptRefunds`. Dev provider empty-meeting smoke остаётся runtime. | `contract`, `runtime` | `canon/owner` для unit, smoke evidence для runtime |
| `architecture.md` §8.4, `db.md` §5.7 | Summary path кэширует `summary_md`, не повторяет платный LLM при retry, `failed_summary` не делает ранний refund, ручное восстановление summary не ребиллит Job. | `orchestrator_test.go`: `TestSummarizeJob_*`, `TestSummarizeJob_DeliverRetryDoesNotResummarize`, duration tests; `tg_test.go`: redelivery/card tests; `storage_test.go`: `TestSetTerminalJobSummary_*`. | `contract` | `canon/owner` |
| `db.md` §5.2 | `FailJobWithQuotaRefund` откатывает резерв ровно один раз и только из ранних статусов. | `storage_quota_test.go`: `TestFailJobWithQuotaRefund_*`, `TestSetJobFailedSummary_NoRefund`; `storage_phases_test.go`: `TestFailJobWithQuotaRefund_FromRecording`. | `contract` | `canon/owner` |
| `db.md` §5.3/§5.7 | Terminal billing округляет duration, возвращает unused, добирает extra только из исходного source, не создаёт долг. | `storage_quota_test.go`: `TestBilledMinutes`, `TestSetJobDone_*`, `TestSetJobFailedSummary_*`, `TestSetJobDone_ConcurrentDoubleDone`. | `contract` | `canon/owner` |
| `architecture.md` §8.5, `db.md` §5.4 | Direct payment intent создаётся двухфазно, polling финализирует ровно одну подписку и сверяет provider response. | `billing_test.go`: `TestCreatePayment_*`; `storage_yookassa_test.go`: pending/finalize tests; `payment_poller_test.go`: finalize/mismatch/cancel tests. | `contract` | `canon/owner` |
| `db.md` §5.4 | Успешный платёж создаёт отдельную subscription строку на 30 дней; concurrent payments не chain-ят срок. | `billing_test.go`: `TestRecordSuccessfulPayment_NoActive`, `TestRecordSuccessfulPayment_IndependentOfActive`, `TestRecordSuccessfulPayment_ConcurrentSameUser`; `storage_yookassa_test.go`: independent/concurrent tests. | `contract` | `canon/owner` |
| `db.md` §5.4, `architecture.md` §8.5 | Recurring profile проходит `active/charging/charge_failed/blocked/disabled` без двойного external POST и без реактивации disabled/blocked. | `recurring_test.go`, `payment_poller_test.go`, `storage_yookassa_test.go`, `tg_test.go`: recurring status, due claim, stale intent, disable, canceled reasons. | `contract` | `canon/owner` |
| `db.md` §5.5 | Legacy `pre_checkout_query` валидирует только payload быстро и без JOIN. | `billing_test.go`: `TestValidatePreCheckout`. | `contract` | `canon/owner` |
| `architecture.md` §8.6, `db.md` §5.9 | Refund двухфазный; succeeded refund деактивирует ровно купленную subscription и отключает recurring. | `storage_refund_test.go`: `TestBeginRefund_*`, `TestFinalizeRefund_*`; `billing_test.go`: `TestCreateRefund_PostsYookassaRequest`. Full YooKassa refund остаётся runtime/manual. | `contract`, `runtime` | `canon/owner` для unit, smoke evidence для runtime |
| `architecture.md` §8.7, `db.md` §5.2 | Cancel делает owner check, идемпотентно фейлит активную Job, возвращает резерв и не трогает чужие/done Job. | `storage_cancel_test.go`: `TestCancelJob_*`; provider Cancel через Telegram panel остаётся runtime. | `contract`, `runtime` | `canon/owner` для storage, smoke evidence для runtime |
| `architecture.md` §8.8, `db.md` §5.6 | F9/nudges сначала отправляют, потом mark; дедупят классы пушей и не теряют retry при send failure. | `notifier_test.go`: `TestNotifyGroup_SendThenMark`, `Test*Dedup`, payment/sub low/empty tests. Реальная доставка TG остаётся runtime. | `contract`, `runtime` | `canon/owner` для storage, smoke evidence для runtime |
| `architecture.md` §8.5, `payments.md` | ЮKassa REST request shape: amount, receipt, metadata, idempotence key, save payment method. | `billing_test.go`: `TestBuildInvoice_*`, `TestCreatePayment_*`, `TestCreateRecurringPayment_PostsYookassaRequest`, `TestGetPayment_ParsesYookassaResponse`. | `contract` | `canon/owner` |
| `architecture.md` §9/§10, provider API docs | Provider status mapping treats unknown status as contract drift; access code/link parsing keeps meeting credentials. | `speech2text_test.go`: `TestMapS2TStatus`, `TestMeetingAccessCode*`; `mymeet_test.go`: status/source/poll/fetch tests; `tg_test.go`: `TestFindMeetingLink`. | `contract`, `implementation` | `canon/owner` для status mapping; code review для parsers |
| `architecture.md` §8.4, OpenRouter API docs | OpenRouter summarizer handles non-2xx, body-level errors, choice errors, transient retry, empty input and map-reduce calls. | `openrouter_test.go`: `TestOpenRouterSummarize_*`, `TestSplitTranscript`; `openrouter_live_test.go` is opt-in live. | `contract`, `runtime` | `canon/owner` для parser contract; live-key smoke для runtime |
| `architecture.md` §5/§8.5, migration `00014` | Active recurring profiles align `next_charge_at` with effective subscription expiry; retry profiles are not touched. | `migration_test.go`: `TestMigration00014_AlignsOnlyActiveRecurringProfiles`. | `contract` | `canon/owner` |
| `architecture.md` post-0.1 Google target | `awaiting_reauth` and `/reconnect` resume delivery without duplicate document. | No Go implementation in release 0.1; covered only as future E2E checklist. | `gap` | Follow-up Google issue before implementation |
| `architecture.md` §8.2 provider unknown/paused status | `StatusPaused` and unknown provider statuses do not deliver, notify, or mutate Job incorrectly. | No dedicated unit by design; live provider behavior and logs are runtime evidence. | `runtime` | Smoke evidence; add test only if provider fake is needed by a future issue |

## Test file audit

| Домен | Файлы | Текущая роль | Примечания |
|---|---|---|---|
| Billing and ЮKassa HTTP boundary | `billing_test.go`, `payment_poller_test.go`, `storage_yookassa_test.go` | `contract` | Protected: money, receipts, polling, pending intent, independent periods. |
| Quota and storage transactions | `storage_quota_test.go`, `storage_test.go`, `storage_phases_test.go`, `storage_cancel_test.go`, `storage_refund_test.go` | `contract` | Protected: DB physics, guards, races, refund/cancel/terminal billing. |
| Worker state machine | `orchestrator_test.go` | `contract` | Protected when assertions cover status, delivery, notify, quota and summary retry semantics. |
| Recurring | `recurring_test.go` plus payment/storage tests | `contract` | Protected until recurring runtime is deliberately redesigned. |
| Provider adapters | `speech2text_test.go`, `mymeet_test.go` | `contract`, `implementation` | Status mapping is protected; URL/access-code/render helpers are implementation unless tied to provider API docs. |
| Summary | `openrouter_test.go`, `summary_test.go`, `openrouter_live_test.go` | `contract`, `implementation`, `runtime` | OpenRouter parser/map-reduce is protected; mock prompt wiring is implementation; live test is opt-in runtime. |
| Telegram UX glue | `tg_test.go` | `contract`, `implementation`, `runtime` | Payment/card/balance behavior is protected; formatting/support-copy tests are implementation or noise candidates. |
| Notifications | `notifier_test.go` | `contract` | Protected for dedup/send-then-mark; analytics metadata helper is implementation. |
| Config/bootstrap/mocks | `main_test.go`, `mock_test.go` | `implementation`, `noise candidate` | Useful guardrails, but not release acceptance by themselves. |

## Runtime acceptance matrix

Runtime checks validate the real integration boundary. They must be reported with SHA, command/log
window, ids and result. They are not replaced by `go test`.

| Граница | Сценарий | Обязательные артефакты |
|---|---|---|
| Telegram + provider happy path | `/start`, meeting URL, provider bot joins, transcript and summary delivered as `.md`. | dev SHA, Job id, Telegram bot, app log window, delivered filenames. |
| Speech2Text readiness | success, empty meeting, provider/start-timeout, parallel different URLs, duplicate same URL, active cap. | Job ids, provider status/log events, quota before/after for refund cases. |
| OpenRouter live | real summary with configured model; provider/API failure gives transcript-only and `failed_summary`. | Job id, model/provider env from logs, summary status, no duplicate LLM call on retry if observed. |
| ЮKassa direct payment links | email capture, payment link, test payment, polling subscription `900/900`, canceled/unpaid path. | payment id, `payments` row status, subscription id, `/balance`, app log window. |
| Recurring before enabling flag | saved payment method, recurring success, retryable failure, revoked/blocked, disable flow. | payment/profile ids, profile statuses, `RECURRING_PAYMENTS_ENABLED` value, log events. |
| Refund MVP | manual ЛК refund plus linked subscription deactivation, no unrelated subscription touched. | provider payment/refund id, subscription id, DB before/after, operator action note. |
| Coolify/dev deploy | `origin/dev` image is actually running, migrate ok, no config/db/restart-loop errors. | `deploy/safe-ssh.sh coolify-dev-health`, logs window, expected SHA. |
| Release/main gate | release PR `dev -> main` has green CI, prebeta check, smoke evidence and rollback note. | release acceptance report with SHA, issues/PRs, checks, risks, rollback. |

## Noise candidates and follow-up

Noise candidates are not deleted in feature PRs. They need a separate cleanup issue with rationale and
owner approval.

| Candidate | Почему может быть слабым | До какого решения держим |
|---|---|---|
| `main_test.go::TestParseLogLevel` | Small config parser guard, not a product/architecture acceptance test. | Config bootstrap changes or cleanup issue. |
| `mock_test.go` happy/counter details | Protects dev fake behavior, not user-facing contract. | MockProvider shape changes. |
| `summary_test.go::TestMockSummarizer_PromptWiredIntoOutput` | Checks mock wiring and embedded prompt coupling, not summary quality. | Summary test strategy cleanup. |
| `tg_test.go` pure formatting/support-copy tests | Useful UX guardrails, but can become copy-locking noise if product text changes. | Product copy issue or UX contract update. |
| URL/email parser edge cases not linked to provider/API docs | Some are good regression tests, but change gate should be code review, not canon. | Parser refactor issue. |
| Config default tests in `billing_test.go` | Valuable when env contract changes, but not independent acceptance. | Env/config contract update. |

## Known gaps

- Google delivery `awaiting_reauth` / `/reconnect` has no Go implementation and no automated coverage
  in release 0.1. Add acceptance contract in the Google issue before coding.
- Full external `BeginRefund -> YooKassa -> FinalizeRefund` is a manual MVP process. Unit tests cover DB
  phases and request shape; runtime evidence must come from a test/live refund smoke.
- Provider `StatusPaused` and unknown status behavior is intentionally runtime/log checked until a future
  issue needs a richer provider fake.
- Telegram panel glue for provider-specific redelivery/cancel after provider switch remains runtime E2E:
  storage round-trip is covered, real button flow is not.

## Required commands

Use a temp Go cache in Codex/sandbox sessions:

```sh
cd src
GOCACHE=/private/tmp/transcrib-gocache go test -list . ./...
GOCACHE=/private/tmp/transcrib-gocache go test ./...
```

For DB-affecting slices, CI already runs Postgres-backed tests with migrations. If a local schema
roundtrip is part of the issue, run it on a throwaway database: `goose up`, `goose down`, `goose up`.
