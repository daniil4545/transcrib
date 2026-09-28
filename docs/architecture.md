# Архитектура transcrib

> **Канон для:** модулей `src/internal/app/`, runtime-потока, state machine `Job`, интерфейсов `TranscriptionProvider`/`Summarizer`/`DocumentDelivery`, критических алгоритмов и архитектурных ограничений.
> **Не дублировать здесь:** продуктовые требования F0-F9 (`docs/product.md`), детальную схему БД и SQL-паттерны (`docs/db.md`), платежный runbook (`docs/payments.md`), инфраструктуру (`docs/deployment.md`).

Версия: 1.3, 2026-07-06. Документ описывает текущий кодовый срез: release 0.1 baseline с hourly billing #7, локальный #12 payment nudges / `analytics_events`, recurring hardening, billing expiry fix через migration `00014` и token erasure на disable через migration `00015`. Google Drive delivery, OAuth callback, `/reconnect` и `awaiting_reauth` описаны отдельно как post-0.1 целевой срез, потому что соответствующих Go-файлов в текущем коде нет.

## 1. Назначение сервиса

`transcrib` - Go-монолит Telegram-бота «transcrib». Пользователь присылает ссылку на Zoom, Google Meet или Яндекс Телемост, сервис отправляет внешнего бота поставщика транскрибации на встречу, получает транскрипт, формирует саммари и доставляет `.md`-файлы в Telegram.

Сервис является оркестратором интеграций, а не AI/ML-системой:

- запись встречи, ASR и диаризацию делает внешний `TranscriptionProvider`;
- саммари делает либо внешний provider, если он вернул готовый отчет, либо наш `Summarizer` через OpenRouter;
- UI, команды, платежи и выдача результата идут через Telegram;
- квоты, очередь и состояние хранятся в PostgreSQL.

Текущий код: Telegram-доставка, Speech2Text/mymeet/mock provider'ы, OpenRouter/mock/off summarizer, direct payment links ЮKassa, F9-пуши/payment nudges, `analytics_events`, панель «Мои звонки». Google Docs - post-0.1.

Billing go-live #7 поменял продуктовый контракт с количества встреч на часы: 3 часа trial, 15 часов/мес, runtime-учет в минутах. Подробный контракт зафиксирован в `docs/product.md`, `docs/payments.md` и `docs/db.md`; исторический слайс удалён из `docs/plans/`.

## 2. Runtime-схема

```
Telegram long polling
        |
        v
src/cmd/transcrib/main.go
        |
        +--> tg.go
        |       - команды и consent gate
        |       - прием meeting URL
        |       - сбор email и отправка ссылки оплаты
        |       - "Мои звонки", отмена, повторная выдача
        |
        +--> billing.go
        |       - POST /v3/payments и GET /v3/payments/{id}
        |       - REST refund через ЮKassa
        |
        +--> storage.go  <--------------------+
        |       - PostgreSQL                  |
        |       - quota reserve/refund        |
        |       - jobs queue                  |
        |       - payment/refund invariants   |
        |                                     |
        +--> payment_poller.go
        |       - polling pending-платежей ЮKassa
        |       - финализация подписки
        |
        +--> orchestrator.go -----------------+
        |       - worker tick 30 sec
        |       - Job state machine
        |       - provider submit/poll/fetch
        |       - delivery + summary
        |
        +--> notifier.go
        |       - F9 tick 1 hour
        |       - trial/subscription/payment nudges
        |       - send -> mark via users.notified_*_at and analytics_events
        |
        +--> recurring.go
                - default-on subscription renewal tick
                - due recurring_profiles -> charging intent -> YooKassa recurring payment

External integrations:
Telegram Bot API, Speech2Text API, mymeet API, OpenRouter API, ЮKassa REST API, PostgreSQL 16
```

В одном процессе сейчас работают пять активных веток выполнения:

1. Telegram long polling в `bot.Start()`.
2. `Orchestrator.Run(ctx)` - worker state machine по jobs.
3. `PaymentPoller.Run(ctx)` - polling pending-платежей ЮKassa.
4. `Notifier.Run(ctx)` - отдельный F9-тик.
5. `RecurringWorker.Run(ctx)` - worker автопродления; при `RECURRING_PAYMENTS_ENABLED=false` сразу выходит и не создаёт списаний.

HTTP-сервера в release 0.1 нет. Межпроцессной коммуникации нет. Состояние и очередь - через PostgreSQL.

## 3. Модули

Все рабочие Go-файлы лежат в одном пакете `src/internal/app/`. Плоская структура намеренная: имя файла соответствует домену ответственности.

| Файл | Ответственность |
|---|---|
| `src/cmd/transcrib/main.go` | Bootstrap: `slog`, `LoadConfig`, `pgxpool`, сбор provider map, выбор активного provider, создание `Bot`, `TelegramDelivery`, `Orchestrator`, `PaymentPoller`, `Notifier`, signal shutdown. |
| `config.go` | Env-контракт: БД/бот, active transcription provider, summarizer, квоты, watchdog, direct ЮKassa payments/refund и receipt. |
| `domain.go` | Чистые типы `User`, `Job`, `Subscription`, `Payment`, `Refund` и enum'ы статусов. `Job` хранит `Provider`, `StatusMessageID`, `SummaryMD`. |
| `storage.go` | Владелец транзакционных инвариантов: quota reserve/refund, active cap, same-URL dedup, worker claim, status guards, payment idempotency, refund phases, F9/nudge-выборки, `analytics_events`. |
| `tg.go` | Telegram UI: consent gate, команды, прием ссылок, создание Job, сбор email для чека, отправка payment link, панель «Мои звонки», отмена, повторная доставка transcript/summary. |
| `billing.go` | ЮKassa REST client: создание платежа, polling payment, full refund; legacy Telegram invoice helpers оставлены для старого пути. |
| `payment_poller.go` | Polling pending-платежей ЮKassa, сверка суммы/currency/user metadata, финализация подписки, recurring failure path и уведомление пользователя. |
| `orchestrator.go` | Worker state machine: submit, poll, fetch, delivery transcript, summary path, failure/refund transitions, status-message edits. |
| `notifier.go` | F9/payment nudges: trial заканчивается, просмотр оплаты без оплаты, платный баланс ниже часа, платные часы закончились, подписка истекает, подписка истекла. Инвариант: сначала send, потом mark. |
| `transcription.go` | Интерфейс `TranscriptionProvider`, `ProviderStatus`, `Result`, `ErrEmptyTranscript`. |
| `speech2text.go` | Provider Speech2Text: join, poll recognition, fetch `result/txt` и `result/json`, mapping статусов, empty transcript. |
| `mymeet.go` | Provider mymeet: record meeting, poll status, fetch report, render transcript, extract provider-summary from `templates[]`, degraded `Cancel`. |
| `mock.go` | Mock provider для разработки и тестов. |
| `summary.go` | Интерфейс `Summarizer`, `MockSummarizer`, встроенный prompt. |
| `openrouter.go` | OpenRouter summarizer: chat completions, chunking, map-reduce для длинного транскрипта. |
| `delivery.go` | Интерфейс `DocumentDelivery` и helpers имен файлов/времени МСК. |
| `telegram_delivery.go` | Реализация `DocumentDelivery`: отправка `.md`-файла через Telegram Bot API. |

В `main.go` строится map всех сконфигурированных provider'ов, а не только активного. Причина: старые jobs несут `job.provider`; отмена и повторная выдача должны обращаться к тому provider'у, который создал `external_job_id`, даже после смены `TRANSCRIPTION_PROVIDER`. Новый Job и `Orchestrator` получают только активный provider.

## 4. Внешние интеграции и конфиг-переключатели

| Интеграция | Роль | Код |
|---|---|---|
| Telegram Bot API | UI, long polling, документы, платежные update'ы | `tg.go`, `telegram_delivery.go` |
| ЮKassa REST API | Создание payment link, polling оплаты, full refund | `tg.go`, `billing.go`, `payment_poller.go`, `storage.go` |
| Speech2Text API | Экономически целевой provider транскрибации | `speech2text.go` |
| mymeet API | Альтернативный/fallback provider, может вернуть provider-summary | `mymeet.go` |
| OpenRouter API | Саммари F3, когда provider не вернул готовое summary | `openrouter.go` |
| PostgreSQL 16 | Очередь jobs, квоты, платежи, состояние | `storage.go` |

Основные переключатели:

- `TRANSCRIPTION_PROVIDER=mock|speech2text|mymeet` - активный provider для новых jobs и orchestrator.
- `SUMMARIZER_PROVIDER=off|mock|openrouter` - путь саммари.
- `TRIAL_HOURS_LIMIT` - лимит триала в часах; runtime хранит минуты.
- `SUBSCRIPTION_HOURS_PER_PERIOD` - лимит подписки в часах; successful payment создает минутный баланс.
- `JOB_MAX_AGE_HOURS` - watchdog для зависших jobs.
- `JOB_START_TIMEOUT_MINUTES` - общий timeout для `created|joining` до начала записи.
- `JOB_SUBMIT_MAX_ATTEMPTS` - best-effort лимит Submit-попыток в памяти app-процесса.
- `YOOKASSA_SHOP_ID`, `YOOKASSA_SECRET_KEY`, `PAYMENT_RETURN_URL` - direct payments и refund.
- `PAYMENT_POLL_INTERVAL_SECONDS` - частота polling pending-платежей и recurring worker tick.
- `RECURRING_PAYMENTS_ENABLED` - отдельный рубильник merchant-initiated автосписаний, default false.
- `PROVIDER_TOKEN` - legacy Telegram Payments provider token, не нужен для целевого direct flow.

## 5. Данные

Детальная схема БД, индексы, SQL и Go-типы - `docs/db.md`. Здесь только архитектурная карта текущих примененных миграций.

Текущие таблицы:

- `users` - Telegram user, consent, trial counters, F9 notification marks.
- `jobs` - встреча и state machine: `provider`, `external_job_id`, `status_message_id`, `summary_md`, `raw_provider_response`, quota binding.
- `subscriptions` - один оплаченный период на один successful payment.
- `payments` - ЮKassa payment intent: direct flow идемпотентен по `payment_intent_key` и `provider_payment_charge_id`; legacy Telegram flow - по `telegram_payment_charge_id`.
- `recurring_profiles` - сохранённый payment method ЮKassa, статус, отключение и дата следующего автосписания.
- `refunds` - двухфазный full refund.
- `analytics_events` - append-only UX-события и `nudge_sent` для аналитики/дедупа дожимов.

Ключевые индексы/ограничения:

- same-URL dedup для активных jobs;
- worker index для `FOR UPDATE SKIP LOCKED`;
- уникальность `payment_intent_key` и `provider_payment_charge_id` для direct payments;
- уникальность Telegram payment charge id для legacy Telegram Payments;
- уникальность refund idempotence key.
- индексы `analytics_events` по `(user_id,event_type,occurred_at)` и `(event_type,occurred_at)` для воронки и F9-дедупа.

Post-0.1 Google-схема (`google_accounts`, `reauth_from`, `awaiting_reauth` helpers) должна быть описана отдельно от текущих applied migrations, чтобы не выглядеть реализованной частью release 0.1.

## 6. Основной поток встречи

Фактический happy path release 0.1:

1. `Bot.gate` через `Storage.GetOrCreateUser` создает или читает пользователя.
2. До `consent_at != NULL` gate пропускает только `/start`, `/help`, consent callbacks и payment update'ы.
3. `Bot.onText` ищет meeting URL, валидирует supported host и вызывает `Storage.CreateJob`.
4. `CreateJob` в одной транзакции блокирует user row, проверяет same-URL dedup, active cap 4, резервирует до 60 минут из платной квоты или триала и вставляет `jobs(status=created, provider=activeProvider, reserved_minutes=...)`.
5. `tg.go` отправляет статус-сообщение и сохраняет `status_message_id`.
6. `Orchestrator.tickOnce` claim'ит jobs через `Storage.ClaimJobsForWork`.
7. `advance(created)` вызывает `submitJob`: `provider.Submit`, затем `SetJobSubmitted(created -> joining)`.
8. `advance(joining|recording|processing)` вызывает `pollJob`: `provider.Poll` и переводит фазу в `recording`, `processing`, `done` или `failed`.
9. При `StatusDone` `finishTranscription` вызывает `provider.Fetch`.
10. Если `SUMMARIZER_PROVIDER=off`, транскрипт доставляется через `TelegramDelivery.Deliver`, затем `SetJobDone`.
11. Если provider вернул `Result.SummaryMarkdown`, summary кэшируется и вместе с транскриптом доставляется одной Telegram media group из двух файлов, затем `SetJobDone`.
12. Если summarizer включен, `SetJobSummarizing`, затем `summarizeJob`: fetch transcript, `Summarizer.Summarize`, cache `summary_md`, deliver one grouped Telegram media set with `summary_*.md` and `transcript_*.md`, `SetJobDone`.
13. Если `Summarizer.Summarize` упал, `summarizeJob` доставляет transcript-only файл и переводит Job в `failed_summary`.
14. `SetJobDone` атомарно закрывает Job, финализирует `billed_minutes` по duration и для trial увеличивает `trial_minutes_done`.

## 7. State machine `Job`

Текущий реализованный граф:

```text
created -> joining -> recording -> processing -> summarizing -> done
   |         |          |            |              |
   +---------+----------+------------+--------------+-> failed
                                      |
                                      +-> failed_summary (только из summarizing)
```

Финальные статусы в Go-коде: `done`, `failed`, `failed_summary`.

Допустимые сокращения:

- Speech2Text может идти `joining -> recording -> processing -> done`, если после `on_call` снова приходит `pending`/очередь: это observable сигнал, что бот вышел и запись ушла в обработку. Если `processing` уже зафиксировал observable window, terminal billing использует сохранённый `duration_seconds`; `Result.DurationSeconds` от provider используется только как fallback для прямого terminal result без записанного окна, затем fallback 3600s.
- mymeet/provider-summary может идти из активной фазы сразу в `done`, минуя `summarizing`.
- `SUMMARIZER_PROVIDER=off` закрывает Job сразу после доставки transcript.

Критические guards:

- `SetJobSubmitted` - только `created`; если запись уже не `created`, orchestrator делает best-effort `Cancel`.
- `SetJobRecording` - только `joining`.
- `SetJobProcessing` - `joining|recording`; при входе из `recording` фиксирует `recording_finished_at` и `duration_seconds`.
- `SetJobDone` - `joining|recording|processing|summarizing`, чтобы покрыть дискретный polling и provider-summary.
- `FailJobWithQuotaRefund` - только `created|joining|recording|processing`; `summarizing` не рефандится, потому что transcript уже доставлен.
- `SetJobFailedSummary` - отдельный финал без quota refund.

`transcribed`, `failed_join`, `awaiting_reauth` могут встречаться в исторических SQL CHECK/доках как target/dormant values, но не являются рабочим путем release 0.1.

## 8. Критические алгоритмы

### 8.1. Создание Job и резерв квоты

Функция: `Storage.CreateJob(ctx, userID, meetingURL, provider, trialLimitMinutes)`.

Алгоритм:

1. Начать транзакцию.
2. Заблокировать строку пользователя `SELECT ... FOR UPDATE`.
3. Проверить активный Job с тем же `meeting_url`. Если есть - `ErrDuplicateActiveJob`.
4. Посчитать активные jobs пользователя. Если `>= 4` - `ErrTooManyActiveJobs`.
5. Попробовать зарезервировать `min(60, minutes_balance)` из ближайшей активной подписки с `minutes_balance > 0`.
6. Если подписки нет, атомарно зарезервировать `min(60, trialLimitMinutes - trial_minutes_used)` из trial.
7. Если квоты нет - `ErrNoQuota`.
8. Вставить `jobs(status=created, provider=...)`.
9. Если insert упал на `jobs_active_dedup_idx`, вернуть `ErrDuplicateActiveJob`; транзакция откатит резерв.
10. Commit.

Почему резерв на создании, а не только в `done`: два сообщения за короткое время не могут оба пройти `WHERE minutes_balance > 0` или `trial_minutes_used < limit`.

### 8.2. Worker claim и polling

Функции: `Storage.ClaimJobsForWork`, `Orchestrator.tickOnce`, `advance`, `submitJob`, `pollJob`.

Алгоритм:

1. Claim выбирает `created` и активные jobs, которые давно не обновлялись.
2. `FOR UPDATE SKIP LOCKED` защищает от двойного claim внутри SQL statement.
3. `updated_at` bump в claim служит простым backoff между poll-тика.
4. В текущем runtime предполагается один worker в одном процессе; multi-instance потребует отдельного atomic-claim слайса.
5. Для `created` сначала проверяется `JOB_START_TIMEOUT_MINUTES`; timeout переводит Job в `failed` с возвратом квоты без вызова provider.
6. `created` отправляется поставщику через `Submit`.
7. Ошибка `Submit` увеличивает in-memory `submitAttempts`; permanent HTTP-коды `400|401|402|403|404|409` фейлят Job сразу, прочие ошибки ретраятся до `JOB_SUBMIT_MAX_ATTEMPTS`.
8. `joining|recording|processing` поллятся через `Poll`.
9. Для `joining` тот же `JOB_START_TIMEOUT_MINUTES` проверяется до `Poll`; timeout делает best-effort `Cancel`, затем `failed` с возвратом квоты.
10. `maxJobAge` останавливает только зависшую `recording`-встречу: ставит marker `last_error='watchdog_stop'`, вызывает `Cancel`, переводит Job в `processing` и ждёт transcript. `processing` по `created_at` больше не фейлится: бот уже вышел, provider готовит результат.
11. Если после stop provider возвращает `StatusFailed`, worker пробует `Fetch` только при marker `manual_stop|watchdog_stop`; непустой transcript идёт в обычный billable result path, `ErrEmptyTranscript`/fetch-fail — в `failed` с возвратом квоты.

### 8.3. Empty transcript

Если provider возвращает `ErrEmptyTranscript`, orchestrator переводит Job в `failed` через `FailJobWithQuotaRefund`. Summary не запускается, quota возвращается. Это нужно для пустых или технически бесполезных встреч: пользователь не должен терять встречу из лимита.

### 8.4. Summary path

Функции: `finishTranscription`, `summarizeJob`, `SetJobSummary`, `SetTerminalJobSummary`, `SetJobDone`, `SetJobFailedSummary`.

Алгоритм:

1. Transcript доставляется сразу только при `SUMMARIZER_PROVIDER=off`.
2. Provider-summary, если есть, кэшируется в `summary_md`, доставляется одним файлом вместе с transcript и закрывает Job.
3. При нашем summarizer Job переводится в `summarizing`.
4. `summarizeJob` сначала проверяет `job.SummaryMD`.
5. Если summary нет, делает `Fetch`, вызывает `Summarizer.Summarize`, кэширует summary до доставки.
6. Если delivery итогового файла временно упал, следующий тик берет summary из кэша и не повторяет платный LLM-вызов.
7. Ошибка генерации summary доставляет transcript-only файл и ведет в `failed_summary` без раннего quota refund; terminal billing считает встречу оказанной услугой по сохранённой duration, а при отсутствии сохранённой duration берёт provider duration/fallback.
8. Кнопка «Получить конспект» в «Мои звонки» для terminal Job без `summary_md` пробует заново получить transcript у provider, восстановить summary и сохранить его через `SetTerminalJobSummary`; для `failed_summary` успешное восстановление переводит статус в `done` без повторного billing.

### 8.5. Payment path

Функции: `Billing.CreatePayment`, `Billing.CreateRecurringPayment`, `Billing.GetPayment`, `Storage.CreatePendingPayment`, `Storage.FinalizeYookassaPayment`, `Storage.FailYookassaPayment`, `PaymentPoller.Run`, `RecurringWorker.Run`.

Алгоритм:

1. Пользователь нажимает оплату; `tg.go` берёт сохранённый `payment_email` или просит email для чека.
2. `CreatePayment` вызывает `POST /v3/payments` с `Idempotence-Key`, `confirmation.type=redirect`, `capture=true`, `receipt` и `metadata.user_id`; при `RECURRING_PAYMENTS_ENABLED=true` добавляет `save_payment_method:"true"` и `metadata.auto_renew=true`.
3. `CreatePendingPayment` сохраняет `payments.status='pending'`, `provider_payment_charge_id=payment.id`, `payment_intent_key`.
4. Бот отправляет пользователю кнопку на `confirmation.confirmation_url`.
5. `PaymentPoller` проверяет `GET /v3/payments/{id}`.
6. При `succeeded && paid=true` сверяет provider id, сумму, currency и metadata user id, затем `FinalizeYookassaPayment` создаёт отдельную строку `subscriptions`; для `metadata.auto_renew=true` + `payment_method.saved=true` сохраняет `payment_method_id` и upsert'ит `recurring_profiles`.
7. `expires_at` каждой новой строки независим: `now()+30 days`; старая строка и её остаток не меняются. Для recurring дата следующего списания равна effective expiry пользователя (`MAX(expires_at)` активных строк), поэтому совпадает с `/balance`.
8. При `RECURRING_PAYMENTS_ENABLED=true` `RecurringWorker` выбирает due `active|charge_failed`, а `StartRecurringPaymentIntent` атомарно переводит профиль в `charging` и создаёт локальный pending intent до внешнего POST.
9. При `canceled && paid=false` `FailYookassaPayment` закрывает pending без подписки; для recurring payment временная причина становится `charge_failed` с retry через 24 часа, revoked/mandate причина становится `blocked`. `/balance` показывает статус автопродления; «Отключить автопродление» переводит профиль в `disabled` и очищает `payment_method_id`, а «Обновить подписку» запускает обычную оплату нового периода.

### 8.6. Refund path

Функции: `Storage.BeginRefund`, `Billing.CreateRefund`, `Storage.FinalizeRefund`.

Алгоритм:

1. `BeginRefund` находит refundable payment, создает или возвращает pending refund intent и idempotence key.
2. `CreateRefund` делает REST-запрос в ЮKassa с `Idempotence-Key`.
3. `FinalizeRefund` фиксирует статус provider'а.
4. При succeeded refund платеж становится `refunded`, связанная подписка деактивируется, `recurring_profiles` пользователя переводится в `disabled`.

MVP поддерживает только полный refund. Критические события payment/refund/recurring пишутся в structured logs: `payment_poll_finalized`, `payment_poll_recurring_failure_marked`, `recurring_worker_started`, `recurring_provider_payment_created`, `recurring_provider_payment_attached`, `recurring_intent_blocked`, `refund_finalized`.

### 8.7. Cancel и повторная выдача

`tg.go` через панель «Мои звонки» дает:

- открыть карточку Job;
- отменить `created|joining` Job с возвратом резерва;
- остановить запись `recording` Job и дождаться transcript;
- повторно получить transcript;
- повторно получить summary из `summary_md`, а если его нет у `done|failed_summary` — попробовать восстановить summary из provider transcript.

`created|joining` отменяются старым no-result path: owner check, `failed`, quota refund, best-effort `provider.Cancel`, если внешний id уже есть. Для `recording` кнопка означает «остановить запись»: owner check ставит marker `last_error='manual_stop'`, `provider.Cancel` останавливает бота, затем Job переводится в `processing`; если provider потом отдаёт transcript, встреча считается оказанной услугой и terminal billing списывает фактическую duration. Если transcript пустой или результат получить нельзя, Job идёт в `failed` с quota refund. `processing|summarizing` в UI не имеют stop/cancel-кнопки: результат уже готовится. У mymeet `Cancel` деградирован: внешний бот может не остановиться, поэтому при ошибке stop локальная джоба закрывается no-result path с возвратом квоты.

### 8.8. F9 notifier

`Notifier` раз в час ищет:

- trial почти исчерпан;
- пользователь смотрел/запрашивал оплату больше 24 часов назад, но не оплатил;
- платных часов осталось меньше 60 минут;
- платные часы закончились до календарного истечения подписки;
- подписка истекает через 3 дня;
- подписка истекла.

Инвариант: сначала отправить сообщение, потом поставить mark. Legacy trial/sub-expiring/sub-expired дедупятся через `users.notified_*_at`, а `notified_*_at` + `analytics_events.nudge_sent` пишутся одной транзакцией; новые классы дедупятся через `analytics_events.nudge_sent`. Если отправка упала, mark не пишется и пуш повторится. Если mark упал после успешной отправки, возможен дубль, но не потеря уведомления.

Подписочные дожимы взаимоисключаются: `sub_low_balance` имеет приоритет над `sub_expiring`, чтобы один тик не отправлял два сообщения одному пользователю. `sub_low_balance` и `sub_empty_balance` не выбирают пользователя, пока есть активная платная Job с `reserved_minutes > 0`: до terminal billing баланс может быть временно уменьшен часовым резервом.

## 9. Provider boundary

`TranscriptionProvider`:

```go
Submit(ctx context.Context, job Job) (string, error)
Poll(ctx context.Context, externalJobID string) (ProviderStatus, error)
Fetch(ctx context.Context, externalJobID string) (Result, error)
Cancel(ctx context.Context, externalJobID string) error
```

Смысл интерфейса практический: provider'ы реально менялись по надежности и экономике, а state machine должна оставаться той же.

`ProviderStatus` маппится в доменные фазы:

- `StatusPending` - остаемся в `joining`;
- `StatusOnCall` - `recording`;
- `StatusTranscribing` - `processing`;
- `StatusDone` - `finishTranscription`;
- `StatusFailed` - `failed` с quota refund; при stop-marker сначала `Fetch`, и непустой transcript идёт в billable result path;
- `StatusPaused` - ожидание/лог, без смены статуса.

Особенности provider'ов:

- Speech2Text: `join`, `recognitions/{id}`, `result/txt`, `result/json`; пустой transcript - `ErrEmptyTranscript`.
- mymeet: `record-meeting`, `meeting/status`, `video/report`; может вернуть provider-summary; `Cancel` не останавливает внешнего бота.
- mock: deterministic provider для dev/test.

## 10. Summarizer boundary

`Summarizer`:

```go
Summarize(ctx context.Context, transcript string) (string, error)
```

OpenRouter path:

1. Пустой transcript отклоняется.
2. Короткий transcript идет одним chat completion.
3. Длинный transcript режется по строкам на chunks до `maxChunkChars`.
4. Для chunks выполняется map stage.
5. Затем reduce stage собирает итоговое summary.
6. Каждый chat-шаг имеет bounded retry для transient OpenRouter/provider ошибок; при map-reduce повторяется только упавший шаг, уже успешные chunks не пересчитываются.
7. Ответ валидируется: пустой content считается ошибкой.

Provider-summary имеет приоритет над `Summarizer`: если `Fetch` принес `SummaryMarkdown`, OpenRouter не вызывается.

## 11. Delivery boundary

`DocumentDelivery`:

```go
Deliver(ctx context.Context, chatID int64, filename, caption string, content []byte) error
```

Release 0.1 реализация одна - `TelegramDelivery`. Она отправляет `.md` как Telegram document.

Известное окно дубля: если процесс падает после успешного `Deliver`, но до записи нового статуса в БД, следующий тик может отправить файл повторно. Частично это снижено `context.WithoutCancel` после необратимых действий, но crash-window остается осознанным runtime limit.

## 12. Логи и диагностика

Логи идут через `slog` JSON в stdout, на сервере собираются journald. Канон инфраструктурной наблюдаемости - `docs/deployment.md`.

Ключевые поля, которые реально используются в коде:

- `event`;
- `phase`;
- `job_id`;
- `user_id`;
- `provider`;
- `current_status`;
- `new_status`;
- `external_job_id`;
- `document`;
- `err`.

Для разбора Job нужен поиск по `job_id`; для provider-проблем - по `provider`, `external_job_id`, `event`.

## 13. Текущие runtime limits

Эти пункты не являются реализованной функциональностью, даже если рядом есть поля или исторические заметки:

- startup orphan recovery отдельной процедурой не реализован;
- `retry_count` и экспоненциальная пауза не используются как алгоритм retry, вместо этого работает claim/backoff через `updated_at`;
- graceful shutdown отменяет context и останавливает Telegram long polling, но не делает явный drain всех текущих операций;
- multi-instance worker не поддержан как гарантия, потому что после commit `created` job может снова стать claimable до `SetJobSubmitted`;
- mymeet `Cancel` не останавливает внешнего бота;
- delivery duplicate windows остаются;
- Google delivery/OAuth/`awaiting_reauth` отсутствуют в текущем Go-коде.

## 14. Post-0.1 Google delivery

Google Drive delivery остается отдельным будущим срезом. Целевой состав:

- `google.go` - OAuth 2.0, refresh token, Drive upload/convert;
- `web.go` - `/oauth/google/callback`;
- `google_delivery.go` - вторая реализация `DocumentDelivery`;
- `/reconnect` - восстановление доступа;
- `awaiting_reauth` + `reauth_from` - пауза Job при 401/invalid refresh и возобновление после OAuth callback.

Причина держать это отдельно: release 0.1 уже работает через Telegram delivery, а Google меняет схему БД, runtime-состав, delivery semantics и failure model.

## 15. Out of scope release 0.1

- Веб-интерфейс, личный кабинет, мобильное приложение.
- Google Docs delivery.
- Автоподключение из календаря.
- Редактирование транскрипта внутри сервиса.
- AI-чат по встрече.
- Несколько тарифов.
- Автоматические частичные refund'ы; ручной законный возврат по заявлению пользователя обрабатывается отдельно по оферте.
- Динамический runtime routing между provider'ами.
- Multi-instance worker.
- Собственное хранение аудио или готовых документов.

## 16. Где читать детали

- Продуктовые требования и roadmap - `docs/product.md`.
- БД, индексы, SQL-паттерны - `docs/db.md`.
- Платежи и ЮKassa - `docs/payments.md`.
- Инфраструктура и deploy - `docs/deployment.md`, `deploy/README.md`.
- Provider-контракты - `docs/api/speech2text/`, `docs/api/mymeet/`, `docs/api/openrouter/`.
