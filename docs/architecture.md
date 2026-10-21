# transcrib architecture

> **Canon for:** the `src/internal/app/` modules, the runtime flow, the `Job` state machine, the `TranscriptionProvider`/`Summarizer`/`DocumentDelivery` interfaces, critical algorithms and architectural constraints.
> **Do not duplicate here:** product requirements F0-F9 (`docs/product.md`), the detailed DB schema and SQL patterns (`docs/db.md`), the payment runbook (`docs/payments.md`), infrastructure (`docs/deployment.md`).

Version: 1.3, 2026-07-06. The document describes the current code slice: the release 0.1 baseline with hourly billing #7, local #12 payment nudges / `analytics_events`, recurring hardening, the billing expiry fix via migration `00014` and token erasure on disable via migration `00015`. Google Drive delivery, the OAuth callback, `/reconnect` and `awaiting_reauth` are described separately as the post-0.1 target slice, because the corresponding Go files do not exist in the current code.

## 1. Service purpose

`transcrib` is a Go monolith, the Telegram bot "transcrib". The user sends a Zoom, Google Meet or Yandex Telemost link, the service sends the transcription provider's external bot to the meeting, receives the transcript, builds a summary and delivers `.md` files to Telegram.

The service is an integration orchestrator, not an AI/ML system:

- meeting recording, ASR and diarization are done by the external `TranscriptionProvider`;
- the summary is made either by the external provider, if it returned a ready report, or by our `Summarizer` through OpenRouter;
- UI, commands, payments and result delivery go through Telegram;
- quotas, queue and state are stored in PostgreSQL.

Current code: Telegram delivery, Speech2Text/mymeet/mock providers, OpenRouter/mock/off summarizer, YooKassa direct payment links, F9 pushes/payment nudges, `analytics_events`, the "My calls" panel. Google Docs are post-0.1.

Billing go-live #7 changed the product contract from a number of meetings to hours: 3 hours of trial, 15 hours/month, minutes in runtime accounting. The detailed contract is recorded in `docs/product.md`, `docs/payments.md` and `docs/db.md`; the historical slice was removed from `docs/plans/`.

## 2. Runtime scheme

```
Telegram long polling
        |
        v
src/cmd/transcrib/main.go
        |
        +--> tg.go
        |       - commands and consent gate
        |       - meeting URL intake
        |       - email collection and payment link sending
        |       - "My calls", cancel, re-delivery
        |
        +--> billing.go
        |       - POST /v3/payments and GET /v3/payments/{id}
        |       - REST refund via YooKassa
        |
        +--> storage.go  <--------------------+
        |       - PostgreSQL                  |
        |       - quota reserve/refund        |
        |       - jobs queue                  |
        |       - payment/refund invariants   |
        |                                     |
        +--> payment_poller.go
        |       - polling pending YooKassa payments
        |       - subscription finalization
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
Telegram Bot API, Speech2Text API, mymeet API, OpenRouter API, YooKassa REST API, PostgreSQL 16
```

Five active execution branches currently run in one process:

1. Telegram long polling in `bot.Start()`.
2. `Orchestrator.Run(ctx)` - the worker state machine over jobs.
3. `PaymentPoller.Run(ctx)` - polling of pending YooKassa payments.
4. `Notifier.Run(ctx)` - a separate F9 tick.
5. `RecurringWorker.Run(ctx)` - the auto-renewal worker; with `RECURRING_PAYMENTS_ENABLED=false` it exits immediately and creates no charges.

There is no HTTP server in release 0.1. There is no inter-process communication. State and queue go through PostgreSQL.

## 3. Modules

All working Go files live in one package, `src/internal/app/`. The flat structure is intentional: the file name matches the domain of responsibility.

| File | Responsibility |
|---|---|
| `src/cmd/transcrib/main.go` | Bootstrap: `slog`, `LoadConfig`, `pgxpool`, building the provider map, choosing the active provider, creating `Bot`, `TelegramDelivery`, `Orchestrator`, `PaymentPoller`, `Notifier`, signal shutdown. |
| `config.go` | Env contract: DB/bot, active transcription provider, summarizer, quotas, watchdog, direct YooKassa payments/refund and receipt. |
| `domain.go` | Pure types `User`, `Job`, `Subscription`, `Payment`, `Refund` and status enums. `Job` holds `Provider`, `StatusMessageID`, `SummaryMD`. |
| `storage.go` | Owner of the transactional invariants: quota reserve/refund, active cap, same-URL dedup, worker claim, status guards, payment idempotency, refund phases, F9/nudge selections, `analytics_events`. |
| `tg.go` | Telegram UI: consent gate, commands, link intake, Job creation, email collection for the receipt, sending the payment link, the "My calls" panel, cancel, transcript/summary re-delivery. |
| `billing.go` | YooKassa REST client: payment creation, payment polling, full refund; legacy Telegram invoice helpers are kept for the old path. |
| `payment_poller.go` | Polling of pending YooKassa payments, reconciliation of amount/currency/user metadata, subscription finalization, recurring failure path and user notification. |
| `orchestrator.go` | Worker state machine: submit, poll, fetch, transcript delivery, summary path, failure/refund transitions, status-message edits. |
| `notifier.go` | F9/payment nudges: the trial is ending, payment viewed but not paid, paid balance below an hour, paid hours ran out, the subscription is expiring, the subscription expired. Invariant: send first, then mark. |
| `transcription.go` | The `TranscriptionProvider` interface, `ProviderStatus`, `Result`, `ErrEmptyTranscript`. |
| `speech2text.go` | Speech2Text provider: join, poll recognition, fetch `result/txt` and `result/json`, status mapping, empty transcript. |
| `mymeet.go` | mymeet provider: record meeting, poll status, fetch report, render transcript, extract provider-summary from `templates[]`, degraded `Cancel`. |
| `mock.go` | Mock provider for development and tests. |
| `summary.go` | The `Summarizer` interface, `MockSummarizer`, the built-in prompt. |
| `openrouter.go` | OpenRouter summarizer: chat completions, chunking, map-reduce for a long transcript. |
| `delivery.go` | The `DocumentDelivery` interface and helpers for file names/Moscow time. |
| `telegram_delivery.go` | Implementation of `DocumentDelivery`: sending an `.md` file through the Telegram Bot API. |

`main.go` builds a map of all configured providers, not only the active one. The reason: old jobs carry `job.provider`; cancel and re-delivery must go to the provider that created the `external_job_id`, even after `TRANSCRIPTION_PROVIDER` is changed. A new Job and the `Orchestrator` get only the active provider.

## 4. External integrations and config switches

| Integration | Role | Code |
|---|---|---|
| Telegram Bot API | UI, long polling, documents, payment updates | `tg.go`, `telegram_delivery.go` |
| YooKassa REST API | Payment link creation, payment polling, full refund | `tg.go`, `billing.go`, `payment_poller.go`, `storage.go` |
| Speech2Text API | Economically preferred transcription provider | `speech2text.go` |
| mymeet API | Alternative/fallback provider, may return a provider-summary | `mymeet.go` |
| OpenRouter API | F3 summary, when the provider did not return a ready summary | `openrouter.go` |
| PostgreSQL 16 | Jobs queue, quotas, payments, state | `storage.go` |

Main switches:

- `TRANSCRIPTION_PROVIDER=mock|speech2text|mymeet` - the active provider for new jobs and the orchestrator.
- `SUMMARIZER_PROVIDER=off|mock|openrouter` - the summary path.
- `TRIAL_HOURS_LIMIT` - the trial limit in hours; runtime stores minutes.
- `SUBSCRIPTION_HOURS_PER_PERIOD` - the subscription limit in hours; a successful payment creates a minute balance.
- `JOB_MAX_AGE_HOURS` - watchdog for stuck jobs.
- `JOB_START_TIMEOUT_MINUTES` - common timeout for `created|joining` before recording starts.
- `JOB_SUBMIT_MAX_ATTEMPTS` - best-effort limit of Submit attempts in app process memory.
- `YOOKASSA_SHOP_ID`, `YOOKASSA_SECRET_KEY`, `PAYMENT_RETURN_URL` - direct payments and refund.
- `PAYMENT_POLL_INTERVAL_SECONDS` - polling frequency of pending payments and the recurring worker tick.
- `RECURRING_PAYMENTS_ENABLED` - a separate kill switch for merchant-initiated auto-charges, default false.
- `PROVIDER_TOKEN` - the legacy Telegram Payments provider token, not needed for the target direct flow.

## 5. Data

The detailed DB schema, indexes, SQL and Go types are in `docs/db.md`. Here is only the architectural map of the currently applied migrations.

Current tables:

- `users` - Telegram user, consent, trial counters, F9 notification marks.
- `jobs` - a meeting and its state machine: `provider`, `external_job_id`, `status_message_id`, `summary_md`, `raw_provider_response`, quota binding.
- `subscriptions` - one paid period per one successful payment.
- `payments` - a YooKassa payment intent: the direct flow is idempotent by `payment_intent_key` and `provider_payment_charge_id`; the legacy Telegram flow by `telegram_payment_charge_id`.
- `recurring_profiles` - the saved YooKassa payment method, status, disable and the date of the next auto-charge.
- `refunds` - a two-phase full refund.
- `analytics_events` - append-only UX events and `nudge_sent` for analytics/nudge dedup.

Key indexes/constraints:

- same-URL dedup for active jobs;
- the worker index for `FOR UPDATE SKIP LOCKED`;
- uniqueness of `payment_intent_key` and `provider_payment_charge_id` for direct payments;
- uniqueness of the Telegram payment charge id for legacy Telegram Payments;
- uniqueness of the refund idempotence key;
- `analytics_events` indexes on `(user_id,event_type,occurred_at)` and `(event_type,occurred_at)` for the funnel and F9 dedup.

The post-0.1 Google schema (`google_accounts`, `reauth_from`, `awaiting_reauth` helpers) must be described separately from the currently applied migrations, so it does not look like an implemented part of release 0.1.

## 6. Main meeting flow

The actual happy path of release 0.1:

1. `Bot.gate` creates or reads the user through `Storage.GetOrCreateUser`.
2. Until `consent_at != NULL` the gate lets through only `/start`, `/help`, consent callbacks and payment updates.
3. `Bot.onText` looks for a meeting URL, validates the supported host and calls `Storage.CreateJob`.
4. In one transaction `CreateJob` locks the user row, checks same-URL dedup and the active cap of 4, reserves up to 60 minutes from the paid quota or the trial and inserts `jobs(status=created, provider=activeProvider, reserved_minutes=...)`.
5. `tg.go` sends a status message and saves `status_message_id`.
6. `Orchestrator.tickOnce` claims jobs through `Storage.ClaimJobsForWork`.
7. `advance(created)` calls `submitJob`: `provider.Submit`, then `SetJobSubmitted(created -> joining)`.
8. `advance(joining|recording|processing)` calls `pollJob`: `provider.Poll` and moves the phase to `recording`, `processing`, `done` or `failed`.
9. On `StatusDone` `finishTranscription` calls `provider.Fetch`.
10. If `SUMMARIZER_PROVIDER=off`, the transcript is delivered through `TelegramDelivery.Deliver`, then `SetJobDone`.
11. If the provider returned `Result.SummaryMarkdown`, the summary is cached and delivered together with the transcript as one Telegram media group of two files, then `SetJobDone`.
12. If the summarizer is enabled, `SetJobSummarizing`, then `summarizeJob`: fetch transcript, `Summarizer.Summarize`, cache `summary_md`, deliver one grouped Telegram media set with `summary_*.md` and `transcript_*.md`, `SetJobDone`.
13. If `Summarizer.Summarize` failed, `summarizeJob` delivers a transcript-only file and moves the Job to `failed_summary`.
14. `SetJobDone` atomically closes the Job, finalizes `billed_minutes` by duration and, for the trial, increases `trial_minutes_done`.

## 7. `Job` state machine

The currently implemented graph:

```text
created -> joining -> recording -> processing -> summarizing -> done
   |         |          |            |              |
   +---------+----------+------------+--------------+-> failed
                                      |
                                      +-> failed_summary (only from summarizing)
```

Final statuses in the Go code: `done`, `failed`, `failed_summary`.

Allowed shortcuts:

- Speech2Text may go `joining -> recording -> processing -> done` if after `on_call` `pending`/queue arrives again: this is an observable signal that the bot left and the recording went to processing. If `processing` has already fixed an observable window, terminal billing uses the saved `duration_seconds`; `Result.DurationSeconds` from the provider is used only as a fallback for a direct terminal result without a recorded window, then the 3600s fallback.
- mymeet/provider-summary may go from an active phase straight to `done`, skipping `summarizing`.
- `SUMMARIZER_PROVIDER=off` closes the Job right after the transcript is delivered.

Critical guards:

- `SetJobSubmitted` - only `created`; if the record is no longer `created`, the orchestrator does a best-effort `Cancel`.
- `SetJobRecording` - only `joining`.
- `SetJobProcessing` - `joining|recording`; on entry from `recording` it records `recording_finished_at` and `duration_seconds`.
- `SetJobDone` - `joining|recording|processing|summarizing`, to cover discrete polling and provider-summary.
- `FailJobWithQuotaRefund` - only `created|joining|recording|processing`; `summarizing` is not refunded, because the transcript has already been delivered.
- `SetJobFailedSummary` - a separate final state without a quota refund.

`transcribed`, `failed_join`, `awaiting_reauth` may appear in historical SQL CHECKs/docs as target/dormant values, but are not a working path of release 0.1.

## 8. Critical algorithms

### 8.1. Job creation and quota reserve

Function: `Storage.CreateJob(ctx, userID, meetingURL, provider, trialLimitMinutes)`.

Algorithm:

1. Begin a transaction.
2. Lock the user row with `SELECT ... FOR UPDATE`.
3. Check for an active Job with the same `meeting_url`. If there is one: `ErrDuplicateActiveJob`.
4. Count the user's active jobs. If `>= 4`: `ErrTooManyActiveJobs`.
5. Try to reserve `min(60, minutes_balance)` from the nearest active subscription with `minutes_balance > 0`.
6. If there is no subscription, atomically reserve `min(60, trialLimitMinutes - trial_minutes_used)` from the trial.
7. If there is no quota: `ErrNoQuota`.
8. Insert `jobs(status=created, provider=...)`.
9. If the insert fails on `jobs_active_dedup_idx`, return `ErrDuplicateActiveJob`; the transaction rolls back the reserve.
10. Commit.

Why reserve at creation and not only at `done`: two messages in a short time cannot both pass `WHERE minutes_balance > 0` or `trial_minutes_used < limit`.

### 8.2. Worker claim and polling

Functions: `Storage.ClaimJobsForWork`, `Orchestrator.tickOnce`, `advance`, `submitJob`, `pollJob`.

Algorithm:

1. Claim selects `created` and active jobs that have not been updated for a while.
2. `FOR UPDATE SKIP LOCKED` protects against a double claim within an SQL statement.
3. The `updated_at` bump in claim serves as a simple backoff between poll ticks.
4. The current runtime assumes a single worker in a single process; multi-instance will need a separate atomic-claim slice.
5. For `created`, `JOB_START_TIMEOUT_MINUTES` is checked first; on timeout the Job goes to `failed` with a quota refund without calling the provider.
6. `created` is sent to the provider through `Submit`.
7. A `Submit` error increments the in-memory `submitAttempts`; permanent HTTP codes `400|401|402|403|404|409` fail the Job immediately, other errors are retried up to `JOB_SUBMIT_MAX_ATTEMPTS`.
8. `joining|recording|processing` are polled through `Poll`.
9. For `joining` the same `JOB_START_TIMEOUT_MINUTES` is checked before `Poll`; on timeout it does a best-effort `Cancel`, then `failed` with a quota refund.
10. `maxJobAge` stops only a stuck `recording` meeting: it sets the marker `last_error='watchdog_stop'`, calls `Cancel`, moves the Job to `processing` and waits for the transcript. `processing` is no longer failed by `created_at`: the bot has already left, the provider is preparing the result.
11. If after stop the provider returns `StatusFailed`, the worker tries `Fetch` only with the marker `manual_stop|watchdog_stop`; a non-empty transcript goes to the normal billable result path, `ErrEmptyTranscript`/fetch failure go to `failed` with a quota refund.

### 8.3. Empty transcript

If the provider returns `ErrEmptyTranscript`, the orchestrator moves the Job to `failed` through `FailJobWithQuotaRefund`. The summary does not start, the quota is returned. This is for empty or technically useless meetings: the user must not lose the meeting from the limit.

### 8.4. Summary path

Functions: `finishTranscription`, `summarizeJob`, `SetJobSummary`, `SetTerminalJobSummary`, `SetJobDone`, `SetJobFailedSummary`.

Algorithm:

1. The transcript is delivered immediately only with `SUMMARIZER_PROVIDER=off`.
2. A provider-summary, if present, is cached in `summary_md`, delivered as one file together with the transcript and closes the Job.
3. With our summarizer the Job moves to `summarizing`.
4. `summarizeJob` first checks `job.SummaryMD`.
5. If there is no summary, it does `Fetch`, calls `Summarizer.Summarize` and caches the summary before delivery.
6. If delivery of the final file temporarily failed, the next tick takes the summary from the cache and does not repeat the paid LLM call.
7. A summary generation error delivers a transcript-only file and leads to `failed_summary` without an early quota refund; terminal billing counts the meeting as a delivered service by the saved duration, and without a saved duration it takes the provider duration/fallback.
8. The "Получить конспект" ("Get summary") button in "My calls" for a terminal Job without `summary_md` tries to fetch the transcript from the provider again, restore the summary and save it through `SetTerminalJobSummary`; for `failed_summary` a successful restore moves the status to `done` without repeated billing.

### 8.5. Payment path

Functions: `Billing.CreatePayment`, `Billing.CreateRecurringPayment`, `Billing.GetPayment`, `Storage.CreatePendingPayment`, `Storage.FinalizeYookassaPayment`, `Storage.FailYookassaPayment`, `PaymentPoller.Run`, `RecurringWorker.Run`.

Algorithm:

1. The user taps pay; `tg.go` takes the saved `payment_email` or asks for an email for the receipt.
2. `CreatePayment` calls `POST /v3/payments` with `Idempotence-Key`, `confirmation.type=redirect`, `capture=true`, `receipt` and `metadata.user_id`; with `RECURRING_PAYMENTS_ENABLED=true` it adds `save_payment_method:"true"` and `metadata.auto_renew=true`.
3. `CreatePendingPayment` saves `payments.status='pending'`, `provider_payment_charge_id=payment.id`, `payment_intent_key`.
4. The bot sends the user a button to `confirmation.confirmation_url`.
5. `PaymentPoller` checks `GET /v3/payments/{id}`.
6. On `succeeded && paid=true` it reconciles the provider id, amount, currency and metadata user id, then `FinalizeYookassaPayment` creates a separate `subscriptions` row; for `metadata.auto_renew=true` + `payment_method.saved=true` it saves `payment_method_id` and upserts `recurring_profiles`.
7. The `expires_at` of each new row is independent: `now()+30 days`; the old row and its remainder do not change. For recurring, the next charge date equals the user's effective expiry (`MAX(expires_at)` of active rows), so it matches `/balance`.
8. With `RECURRING_PAYMENTS_ENABLED=true`, `RecurringWorker` selects due `active|charge_failed`, and `StartRecurringPaymentIntent` atomically moves the profile to `charging` and creates a local pending intent before the external POST.
9. On `canceled && paid=false`, `FailYookassaPayment` closes the pending payment without a subscription; for a recurring payment a temporary reason becomes `charge_failed` with a retry in 24 hours, a revoked/mandate reason becomes `blocked`. `/balance` shows the auto-renewal status; "Отключить автопродление" ("Disable auto-renewal") moves the profile to `disabled` and clears `payment_method_id`, and "Обновить подписку" ("Renew subscription") starts a normal payment for a new period.

### 8.6. Refund path

Functions: `Storage.BeginRefund`, `Billing.CreateRefund`, `Storage.FinalizeRefund`.

Algorithm:

1. `BeginRefund` finds a refundable payment, creates or returns a pending refund intent and the idempotence key.
2. `CreateRefund` makes a REST request to YooKassa with `Idempotence-Key`.
3. `FinalizeRefund` records the provider's status.
4. On a succeeded refund the payment becomes `refunded`, the related subscription is deactivated, the user's `recurring_profiles` moves to `disabled`.

The MVP supports only a full refund. Critical payment/refund/recurring events are written to structured logs: `payment_poll_finalized`, `payment_poll_recurring_failure_marked`, `recurring_worker_started`, `recurring_provider_payment_created`, `recurring_provider_payment_attached`, `recurring_intent_blocked`, `refund_finalized`.

### 8.7. Cancel and re-delivery

Through the "My calls" panel `tg.go` provides:

- open a Job card;
- cancel a `created|joining` Job with a reserve refund;
- stop the recording of a `recording` Job and wait for the transcript;
- get the transcript again;
- get the summary again from `summary_md`, and if it is missing for `done|failed_summary`, try to restore the summary from the provider transcript.

`created|joining` are cancelled by the old no-result path: owner check, `failed`, quota refund, best-effort `provider.Cancel` if an external id already exists. For `recording` the button means "stop recording": the owner check sets the marker `last_error='manual_stop'`, `provider.Cancel` stops the bot, then the Job moves to `processing`; if the provider later returns a transcript, the meeting counts as a delivered service and terminal billing charges the actual duration. If the transcript is empty or the result cannot be obtained, the Job goes to `failed` with a quota refund. `processing|summarizing` have no stop/cancel button in the UI: the result is already being prepared. For mymeet `Cancel` is degraded: the external bot may not stop, so on a stop error the local job is closed by the no-result path with a quota refund.

### 8.8. F9 notifier

Once an hour `Notifier` looks for:

- the trial is almost used up;
- the user viewed/requested payment more than 24 hours ago but did not pay;
- less than 60 minutes of paid hours left;
- paid hours ran out before the calendar expiry of the subscription;
- the subscription expires in 3 days;
- the subscription expired.

Invariant: send the message first, then set the mark. Legacy trial/sub-expiring/sub-expired are deduplicated through `users.notified_*_at`, and `notified_*_at` + `analytics_events.nudge_sent` are written in one transaction; new classes are deduplicated through `analytics_events.nudge_sent`. If sending failed, the mark is not written and the push repeats. If the mark failed after a successful send, a duplicate is possible, but no notification is lost.

Subscription nudges are mutually exclusive: `sub_low_balance` takes priority over `sub_expiring`, so one tick does not send two messages to one user. `sub_low_balance` and `sub_empty_balance` do not select a user while there is an active paid Job with `reserved_minutes > 0`: until terminal billing the balance may be temporarily reduced by the hourly reserve.

## 9. Provider boundary

`TranscriptionProvider`:

```go
Submit(ctx context.Context, job Job) (string, error)
Poll(ctx context.Context, externalJobID string) (ProviderStatus, error)
Fetch(ctx context.Context, externalJobID string) (Result, error)
Cancel(ctx context.Context, externalJobID string) error
```

The meaning of the interface is practical: providers did change in reliability and economics, and the state machine must stay the same.

`ProviderStatus` maps to domain phases:

- `StatusPending` - stay in `joining`;
- `StatusOnCall` - `recording`;
- `StatusTranscribing` - `processing`;
- `StatusDone` - `finishTranscription`;
- `StatusFailed` - `failed` with a quota refund; with a stop marker, `Fetch` first, and a non-empty transcript goes to the billable result path;
- `StatusPaused` - wait/log, no status change.

Provider specifics:

- Speech2Text: `join`, `recognitions/{id}`, `result/txt`, `result/json`; an empty transcript is `ErrEmptyTranscript`.
- mymeet: `record-meeting`, `meeting/status`, `video/report`; may return a provider-summary; `Cancel` does not stop the external bot.
- mock: a deterministic provider for dev/test.

## 10. Summarizer boundary

`Summarizer`:

```go
Summarize(ctx context.Context, transcript string) (string, error)
```

OpenRouter path:

1. An empty transcript is rejected.
2. A short transcript goes in a single chat completion.
3. A long transcript is split by lines into chunks up to `maxChunkChars`.
4. A map stage runs over the chunks.
5. Then a reduce stage assembles the final summary.
6. Each chat step has a bounded retry for transient OpenRouter/provider errors; in map-reduce only the failed step is repeated, already successful chunks are not recomputed.
7. The response is validated: empty content counts as an error.

A provider-summary has priority over `Summarizer`: if `Fetch` brought `SummaryMarkdown`, OpenRouter is not called.

## 11. Delivery boundary

`DocumentDelivery`:

```go
Deliver(ctx context.Context, chatID int64, filename, caption string, content []byte) error
```

Release 0.1 has a single implementation, `TelegramDelivery`. It sends `.md` as a Telegram document.

Known duplicate window: if the process crashes after a successful `Deliver` but before the new status is written to the DB, the next tick may send the file again. This is partially reduced by `context.WithoutCancel` after irreversible actions, but the crash window remains a deliberate runtime limit.

## 12. Logs and diagnostics

Logs go through `slog` JSON to stdout and are collected by journald on the server. The canon for infrastructure observability is `docs/deployment.md`.

Key fields actually used in the code:

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

To investigate a Job, search by `job_id`; for provider problems, by `provider`, `external_job_id`, `event`.

## 13. Current runtime limits

These items are not implemented functionality, even if there are fields or historical notes nearby:

- a separate startup orphan recovery procedure is not implemented;
- `retry_count` and exponential pause are not used as a retry algorithm, claim/backoff through `updated_at` works instead;
- graceful shutdown cancels the context and stops Telegram long polling, but does not explicitly drain all current operations;
- multi-instance worker is not supported as a guarantee, because after commit a `created` job may become claimable again before `SetJobSubmitted`;
- mymeet `Cancel` does not stop the external bot;
- delivery duplicate windows remain;
- Google delivery/OAuth/`awaiting_reauth` are absent from the current Go code.

## 14. Post-0.1 Google delivery

Google Drive delivery remains a separate future slice. The target composition:

- `google.go` - OAuth 2.0, refresh token, Drive upload/convert;
- `web.go` - `/oauth/google/callback`;
- `google_delivery.go` - the second implementation of `DocumentDelivery`;
- `/reconnect` - access recovery;
- `awaiting_reauth` + `reauth_from` - pausing a Job on 401/invalid refresh and resuming after the OAuth callback.

Why keep this separate: release 0.1 already works through Telegram delivery, and Google changes the DB schema, the runtime composition, the delivery semantics and the failure model.

## 15. Out of scope release 0.1

- Web interface, personal account, mobile app.
- Google Docs delivery.
- Auto-join from a calendar.
- Editing the transcript inside the service.
- AI chat about a meeting.
- Multiple tariffs.
- Automatic partial refunds; a manual lawful refund at the user's request is handled separately under the offer.
- Dynamic runtime routing between providers.
- Multi-instance worker.
- Own storage of audio or finished documents.

## 16. Where to read details

- Product requirements and roadmap - `docs/product.md`.
- DB, indexes, SQL patterns - `docs/db.md`.
- Payments and YooKassa - `docs/payments.md`.
- Infrastructure and deploy - `docs/deployment.md`, `deploy/README.md`.
- Provider contracts - `docs/api/speech2text/`, `docs/api/mymeet/`, `docs/api/openrouter/`.
