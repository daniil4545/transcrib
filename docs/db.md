# transcrib DB architecture

> **Canon for:** the applied SQL schema of migrations `00001`-`00015`, indexes, Go types in `domain.go`, transactional patterns (quota reserve, payment idempotency, F9 ticks, refund).
> **Mirrors:** `architecture.md` §5 (short entity list), `payments.md` (links to §3.4/§5.4).
> **Do not duplicate here:** the state machine contract (`architecture.md` §7), the payment UX flow (`payments.md` §1), the product trial rules (`product.md` §5 F6).

Version: 1.16 (2026-07-06, migration `00015`: erase YooKassa recurring token on disable).
Accompanies `architecture.md` §5 and details the table schema, indexes, Go types and transactional patterns.

Changes v1.15 to v1.16: migration `00015_recurring_profile_disable_erases_token` makes `recurring_profiles.payment_method_id` nullable and erases the saved YooKassa token on `DisableRecurring` / auto-renewal disable.
Changes v1.14 to v1.15: migration `00014_align_recurring_next_charge` aligns `recurring_profiles.next_charge_at` for active profiles with the user's effective expiry (`MAX(active subscriptions.expires_at)`); new subscription rows are created for a fixed 30 days instead of a calendar month.
Changes v1.13 to v1.14: #9d added migration `00013_recurring_profile_status_hardening`: statuses `charging` and `blocked`, default-on recurring when `RECURRING_PAYMENTS_ENABLED` is on, atomic `StartRecurringPaymentIntent`, stale unknown-result -> `blocked`.
Changes v1.12 to v1.13: #9c added reading the `recurring_profiles` status for `/balance`, user-initiated disable via `DisableRecurring`, auto-renewal disable on successful refund and structured logs on critical payment/recurring/refund transitions; no new migration.
Changes v1.11 to v1.12: #9b added the recurring worker without a new migration: due-select over `recurring_profiles`, merchant-initiated payment with a deterministic `payment_intent_key`, shared polling/finalize and moving the profile to `charge_failed` on canceled/error.
Changes v1.10 to v1.11: migration `00012_recurring_profiles` added `recurring_profiles` and saving of `payments.payment_method_id` only if the payment is marked `metadata.auto_renew=true` and YooKassa returned `payment_method.saved=true`.
Changes v1.9 to v1.10: migration `00011_analytics_events` added an append-only `analytics_events` for UX facts: `/start`, consent decline, subscription view, payment click and `nudge_sent`; money, subscriptions and meetings remain sources of truth in `payments`/`subscriptions`/`jobs`.
Changes v1.8 to v1.9: migration `00010_yookassa_payment_links` added `users.payment_email`, `payments.payment_intent_key`, uniqueness of `provider_payment_charge_id`, the direct pending/finalize flow by YooKassa `payment.id`; a successful direct payment creates a subscription with `minutes_limit/minutes_balance`.
Changes v1.7 to v1.8: `recording_started_at`/`recording_finished_at` were added in `00009`; terminal billing rounds seconds up to a minute and for Speech2Text uses the observable recording window if it is already saved, taking the provider duration only as a fallback for a direct terminal result without a window.
Changes v1.6 to v1.7: the hourly runtime moved config, reserve/refund, terminal billing, `/balance` and F9 to minute fields. Legacy `meetings_*` remain in the schema and are partially supported by code as a compatibility bridge until a separate cleanup.
Changes v1.5 to v1.6: migration `00009` added minute counters next to the legacy `meetings_*`, backfill `1 meeting = 60 minutes`, minute-based indexes for reserve/F9 and a structural down migration.
Changes v1.4 to v1.5: removed duplicate Go snippets and the detailed ASCII ER; historical notes shortened to current invariants.
Changes v1.3 to v1.4: separated the actually applied schema `00001`-`00008` from the post-0.1 Google target; the `jobs` block was aligned with the real migration columns (`provider`, `status_message_id`, `summary_md`) without the not-yet-created `reauth_from`/`transcript_doc_url`/`summary_doc_url`/`retry_count`.
Early revisions: the subscription model `payment:subscription = 1:1`, two-phase refund, `trial_meetings_done`, COALESCE in F9, same-URL dedup and parallel calls.

Billing go-live #7 was accepted as the move to a 3-hour trial, 15 hours/month and internal accounting in minutes. PR #26 delivered the schema, config/domain, provider duration, reserve, finalize billing, UX/F9, docs sync and cleanup of the subscription renewal model.

## 1. Fixed decisions

| # | Decision | Choice | Why |
|---|---|---|---|
| 1 | Canon scope | Migration SQL schema + Go types in `domain.go` | One document holds the DB invariants, and the runtime algorithms below refer to the same fields |
| 2 | Enum status encoding | `TEXT NOT NULL` + `CHECK` | Postgres `ENUM` migrates badly (`ALTER TYPE` is not transactional), `SMALLINT` is unreadable |
| 3 | Money amounts | `BIGINT` in kopecks (199000 = 1990 ₽) | Telegram and YooKassa return kopecks: zero conversions at the boundary. `int64` is native in Go, no `decimal` library needed. Fintech standard |
| 4 | Job deduplication | Partial `UNIQUE (user_id, meeting_url) WHERE status NOT IN ('done','failed_join','failed','failed_summary')` | Protects against accidental double clicks, does not break regular calls in a Zoom personal meeting room |
| 5 | Google `refresh_token` | `TEXT` plaintext in the post-0.1 Google slice. **`BYTEA` + AES-GCM encryption is MANDATORY before the public Google mode** | In the closed Testing mode phase the backup leak risk is lower, but the Google API Security Requirements require encryption before Production |
| 6 | Trial and subscription balance | `users.trial_minutes_used` + `subscriptions.minutes_balance` with **a reserve at Job creation, refund of an early reserve and terminal billing by duration** | An atomic `UPDATE ... WHERE used < limit` physically closes the "two messages within 200ms" race. O(1) instead of `count()`. Symmetric for trial and subscription |
| 7 | Subscription state in F9 | **No** denormalized `users.last_paid_at`. The F9 tick computes `MAX(payments.paid_at WHERE status='succeeded')` on the fly | One source of truth. Correct on refund/chargeback automatically. For 30 users it takes microseconds |
| 8 | Telegram library | `gopkg.in/telebot.v4` | v3 has been frozen since June 2024. v4 is actively developed, the payments module is extended |
| 9 | `payments` structure | PK is `BIGSERIAL`. Direct flow: `payment_intent_key` and `provider_payment_charge_id` are unique; legacy Telegram flow: `telegram_payment_charge_id` `UNIQUE` nullable. Fields `status` (FSM), `subscription_id`, `metadata`/`provider_paid_at`, `invoice_payload`, `fiscal_receipt_url`, `raw_payload JSONB`. A separate `refunds` table (two-phase refund, `idempotence_key`) | A PK on the charge id is impossible in the `pending` stage. A direct payment is created before payment and finalization arrives via polling. The FSM is needed to tell "hanging" / "paid" / "refunded" apart. In the MVP refunds:payment is 1:1 (partial refunds are out of scope); `amount_refunded`/`partially_refunded` were removed as dead |
| 10 | Analytics UX events | Append-only `analytics_events`, no unique on `event_key` | We need the history of repeated views/clicks and dedup of new nudges. Business facts are not duplicated: `jobs`, `payments`, `subscriptions` remain sources of truth |
| 11 | Auto-renewal | A separate `recurring_profiles` 1:1 to `users`; `subscriptions` are not extended in place | Separates the saved payment method from the paid periods. Refund stays deterministic by payment:subscription 1:1, and an auto-charge creates a new payment and a new subscription through the shared polling/finalize |

## 2. ER summary

The applied migrations `00001`-`00015` create the business tables `users`, `jobs`, `subscriptions`, `payments`, `refunds`, `recurring_profiles`, and the analytics table `analytics_events`. `goose_db_version` is the migrator's service table. `google_accounts` is the post-0.1 Google-slice target.

| Table | Relations | Responsibility |
|---|---|---|
| `users` | PK `telegram_id`; 1:N to `jobs`, `subscriptions`, `payments` | consent, trial counters, F9 notification timestamps, payment email for the receipt |
| `jobs` | FK `user_id`; nullable FK `subscription_id` | meeting state machine, provider id, Telegram status message, summary cache, quota source |
| `subscriptions` | FK `user_id`; 1:1 with a successful payment via `payments.subscription_id` | paid period and remaining minutes |
| `payments` | FK `user_id`, nullable FK `subscription_id` | YooKassa payment intent; direct idempotency by `payment_intent_key`/`provider_payment_charge_id`, legacy Telegram idempotency by `telegram_payment_charge_id` |
| `recurring_profiles` | PK/FK `user_id`, nullable FK `last_payment_id` | saved YooKassa payment method, status `active|charging|charge_failed|blocked|disabled` and the date of the next auto-charge |
| `refunds` | FK `payment_id` | two-phase full refund, idempotency key and provider refund id |
| `analytics_events` | FK `user_id` | UX events and `nudge_sent` for analytics/dedup of payment nudges |
| `google_accounts` | FK `user_id` | post-0.1: refresh token and Drive folder |

## 3. SQL schema

Sections 3.1 and 3.3-3.7 describe the schema actually applied by migrations `00001`-`00015`. Section 3.2 is the target post-0.1 Google slice; its tables and columns are not yet created in the current migrations.

### 3.1. `users`

```sql
CREATE TABLE users (
    telegram_id              BIGINT      PRIMARY KEY,
    trial_meetings_used      INTEGER     NOT NULL DEFAULT 0,
    trial_meetings_done      INTEGER     NOT NULL DEFAULT 0,
    trial_minutes_used       INTEGER     NOT NULL DEFAULT 0,
    trial_minutes_done       INTEGER     NOT NULL DEFAULT 0,
    consent_at               TIMESTAMPTZ,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    notified_trial_low_at    TIMESTAMPTZ,
    notified_sub_expiring_at TIMESTAMPTZ,
    notified_sub_expired_at  TIMESTAMPTZ,
    payment_email            TEXT,
    CHECK (trial_meetings_used  >= 0),
    CHECK (trial_meetings_done  >= 0),
    CHECK (trial_meetings_done <= trial_meetings_used),
    CHECK (trial_minutes_used  >= 0),
    CHECK (trial_minutes_done  >= 0),
    CHECK (trial_minutes_done <= trial_minutes_used)
);

CREATE INDEX users_trial_minutes_done_idx
    ON users (trial_minutes_done)
    WHERE notified_trial_low_at IS NULL;
```

- `telegram_id` is the PK directly. Telegram guarantees uniqueness and immutability per user.
- `trial_meetings_used` / `trial_meetings_done` are legacy compatibility counters. After the minute runtime was enabled, the code updates them through a bridge for old tests/reports, but the source of truth for billing is the minute fields.
- `trial_minutes_used` is the reserved and actually consumed trial minutes. `CreateJob` reserves up to 60 minutes, an early `failed` returns `reserved_minutes`, terminal billing returns the unused part or collects the extra within the limit.
- `trial_minutes_done` is the finalized chargeable trial minutes. It grows by `jobs.billed_minutes` for `done` and `failed_summary`; it feeds the F9 push "trial is ending" by real consumption, not by the reserve.
- `consent_at` is the moment of consent to personal data processing (152-FZ). Until `consent_at IS NOT NULL`, accepting links is blocked (enforced in `tg.go`, the onboarding FSM).
- The 3 notification timestamps protect F9 from repeats; on a new subscription the F9 logic compares them with `COALESCE(MAX(payments.paid_at WHERE status='succeeded'), '-infinity'::timestamptz)`.
- `payment_email` is the email for the YooKassa receipt. It is requested before the first direct payment link payment and then reused on renewal.

### 3.2. `google_accounts` (post-0.1 target, not applied)

```sql
CREATE TABLE google_accounts (
    user_id       BIGINT      PRIMARY KEY REFERENCES users(telegram_id) ON DELETE CASCADE,
    refresh_token TEXT        NOT NULL,    -- TODO: BYTEA + AES-GCM in production hardening (Stage 3), before the public launch
    folder_id     TEXT        NOT NULL,    -- ID of the user's Drive folder
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

- 1:1 with `users`, so `user_id` is the PK itself.
- `ON DELETE CASCADE`: when a user is deleted, the auxiliary data goes with them.
- The table will appear in a separate Google-slice migration together with `jobs.reauth_from` and `jobs_awaiting_reauth_idx`.

### 3.3. `subscriptions`

```sql
CREATE TABLE subscriptions (
    id               BIGSERIAL   PRIMARY KEY,
    user_id          BIGINT      NOT NULL REFERENCES users(telegram_id) ON DELETE CASCADE,
    paid_at          TIMESTAMPTZ NOT NULL,
    expires_at       TIMESTAMPTZ NOT NULL,
    meetings_limit   INTEGER     NOT NULL,
    meetings_balance INTEGER     NOT NULL,
    minutes_limit    INTEGER     NOT NULL DEFAULT 0,
    minutes_balance  INTEGER     NOT NULL DEFAULT 0,
    CHECK (meetings_balance >= 0 AND meetings_balance <= meetings_limit),
    CHECK (minutes_limit >= 0),
    CHECK (minutes_balance >= 0 AND minutes_balance <= minutes_limit),
    CHECK (expires_at > paid_at)
);

CREATE INDEX subscriptions_user_active_idx
    ON subscriptions (user_id, expires_at DESC)
    WHERE meetings_balance > 0;

CREATE INDEX subscriptions_expiring_idx
    ON subscriptions (expires_at)
    WHERE meetings_balance > 0;

CREATE INDEX subscriptions_user_active_minutes_idx
    ON subscriptions (user_id, expires_at ASC)
    WHERE minutes_balance > 0;

CREATE INDEX subscriptions_expiring_minutes_idx
    ON subscriptions (expires_at)
    WHERE minutes_balance > 0;

-- All of a user's periods (any balance): for the §5.1/§5.6 aggregation (SUM of balance, MAX of expiry).
CREATE INDEX subscriptions_user_expiry_idx
    ON subscriptions (user_id, expires_at DESC);
```

- **"Row per payment" model (1:1, decision of 2026-06-14).** Each successful payment creates a SEPARATE subscriptions row (§5.4 does NOT extend an existing one). Each new row gets an independent `expires_at = now() + 30 days` (decision of 2026-07-03: publicly we say "a month", internally we count a fixed 30 days). Consequences: a user may have several active rows at once, this is normal; a refund deactivates exactly the row that the refunded payment bought (§5.9, deterministic); `/balance` and F9 (§5.6) **aggregate per user** (SUM of balance, MAX of expiry, DISTINCT). For active recurring, `recurring_profiles.next_charge_at` must match the user's effective expiry: `MAX(subscriptions.expires_at)` over active rows.
- `meetings_limit` / `meetings_balance` are legacy compatibility fields. After #7 they are no longer the source of truth for UX/F9/reserve; the code maintains them through a bridge until a separate cleanup.
- `minutes_limit` and `minutes_balance` are the working limit and remaining balance of the subscription. A successful payment writes `SubscriptionMinutesPerPeriod` (900 minutes at 15 hours), `CreateJob` reserves up to 60 minutes, terminal billing adjusts the balance to the actual duration.
- `subscriptions_user_active_minutes_idx` is the minute analog for the reserve: active subscriptions with remaining minutes. `CreateJob` first picks the nearest row with `minutes_balance >= 60` and takes a partial row only if there is no full one.
- `subscriptions_expiring_idx` and `subscriptions_expiring_minutes_idx` are partial, for the F9 tick "subscription expires in 3 days". `subscriptions_user_expiry_idx` is for the §5.1/§5.6 aggregations (MAX expires_at per user, any balance).

### 3.4. `payments`

```sql
CREATE TABLE payments (
    id                          BIGSERIAL   PRIMARY KEY,
    user_id                     BIGINT      NOT NULL REFERENCES users(telegram_id) ON DELETE RESTRICT,
    subscription_id             BIGINT      REFERENCES subscriptions(id) ON DELETE RESTRICT,
    status                      TEXT        NOT NULL CHECK (status IN (
                                                'pending','succeeded','failed','refunded'
                                            )),
    amount                      BIGINT      NOT NULL CHECK (amount > 0),
    currency                    TEXT        NOT NULL DEFAULT 'RUB' CHECK (currency = 'RUB'),
    telegram_payment_charge_id  TEXT        UNIQUE,
    provider_payment_charge_id  TEXT,
    payment_intent_key          TEXT,
    payment_method_id           TEXT,
    invoice_payload             TEXT,
    fiscal_receipt_url          TEXT,
    metadata                    JSONB,
    raw_payload                 JSONB,
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    paid_at                     TIMESTAMPTZ,
    provider_paid_at            TIMESTAMPTZ,
    failed_at                   TIMESTAMPTZ,
    -- Intake idempotency does not depend on the caller's discipline:
    -- succeeded/refunded must carry a charge_id (ON CONFLICT §5.4 relies on it).
    CHECK (
        telegram_payment_charge_id IS NOT NULL
        OR provider_payment_charge_id IS NOT NULL
        OR status IN ('pending','failed')
    )
);

CREATE INDEX payments_user_paid_idx
    ON payments (user_id, paid_at DESC)
    WHERE status = 'succeeded';

CREATE INDEX payments_pending_idx
    ON payments (created_at)
    WHERE status = 'pending';

CREATE UNIQUE INDEX payments_provider_charge_idx
    ON payments (provider_payment_charge_id)
    WHERE provider_payment_charge_id IS NOT NULL AND provider_payment_charge_id <> '';

CREATE UNIQUE INDEX payments_intent_key_idx
    ON payments (payment_intent_key)
    WHERE payment_intent_key IS NOT NULL;
```

- PK = `id BIGSERIAL`, because a row can be written before the charge id is known.
- `payment_intent_key` is our `Idempotence-Key` for `POST /v3/payments`; uniqueness protects against a repeated creation of the payment intent.
- `provider_payment_charge_id` is the real YooKassa `payment.id` in the direct flow; used for polling and REST refund. Uniqueness protects against a duplicate provider payment.
- `telegram_payment_charge_id UNIQUE` is the legacy idempotency of Telegram Payments retries; in the direct flow it may be NULL.
- `metadata`, `raw_payload`, `provider_paid_at`, `payment_method_id`, `fiscal_receipt_url` are needed for reconciliation, fallback lookup and recurring. We fill `payment_method_id` only with `metadata.auto_renew=true`, `payment_method.saved=true` and a non-empty `payment_method.id`.
- Partial refunds are out of MVP scope: we do not create `partially_refunded` and `amount_refunded`.
- `ON DELETE RESTRICT`: a user with payment history cannot be deleted. Deletion under 152-FZ is a separate anonymization procedure (see §6: personal data is also in `raw_payload`/`metadata`, not only in `users`).

### 3.4a. `recurring_profiles`

```sql
CREATE TABLE recurring_profiles (
    user_id           BIGINT      PRIMARY KEY REFERENCES users(telegram_id) ON DELETE RESTRICT,
    payment_method_id TEXT,
    status            TEXT        NOT NULL CHECK (status IN ('active','charging','charge_failed','blocked','disabled')),
    next_charge_at    TIMESTAMPTZ NOT NULL,
    last_payment_id   BIGINT      REFERENCES payments(id) ON DELETE RESTRICT,
    attempts_count    INTEGER     NOT NULL DEFAULT 0,
    last_error        TEXT,
    consent_at        TIMESTAMPTZ NOT NULL,
    disabled_at       TIMESTAMPTZ,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX recurring_profiles_due_idx
    ON recurring_profiles (next_charge_at)
    WHERE status IN ('active','charge_failed');
```

- `payment_method_id` is the reusable YooKassa `payment_method.id`. We store it only after a payment with `metadata.auto_renew=true` and only if YooKassa returned `payment_method.saved=true`; on `disabled` we clear it to `NULL`, so that disabling auto-renewal erases the token on our side.
- `status='active'` means the worker may try to charge the next period. `charging` is an in-flight attempt after the local intent and before the poller result. `charge_failed` keeps the profile for retry; `blocked` stops auto-charges until a manual check; `disabled` means a user-initiated disable.
- `next_charge_at` for `status='active'` equals the user's effective expiry: `MAX(expires_at)` over active subscriptions, so that `/balance` and the next charge date match. For `charge_failed` it is the next retry time, usually `now()+24h`; `blocked` is not moved to a new retry. The worker creates a new payment and does not change the current subscription.
- `last_payment_id` points to the payment by which the profile was created or updated.

### 3.5. `refunds`

```sql
CREATE TABLE refunds (
    id                  BIGSERIAL   PRIMARY KEY,
    payment_id          BIGINT      NOT NULL REFERENCES payments(id),
    idempotence_key     TEXT        NOT NULL UNIQUE,
    provider_refund_id  TEXT,
    amount              BIGINT      NOT NULL CHECK (amount > 0),
    status              TEXT        NOT NULL CHECK (status IN ('pending','succeeded','failed','canceled')),
    reason              TEXT,
    raw_payload         JSONB,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at        TIMESTAMPTZ
);

-- Finalization idempotency: the YooKassa provider_refund_id is unique, but NULL at the pending stage.
CREATE UNIQUE INDEX refunds_provider_idx ON refunds (provider_refund_id) WHERE provider_refund_id IS NOT NULL;
-- One active refund per payment: closes the double BeginRefund race BEFORE calling YooKassa.
CREATE UNIQUE INDEX refunds_active_idx ON refunds (payment_id) WHERE status IN ('pending','succeeded');
CREATE INDEX refunds_payment_idx ON refunds (payment_id);
```

- The refund is two-phase (§5.9): `pending` is created before calling YooKassa, `FinalizeRefund` fills `provider_refund_id` and the status after the response.
- `idempotence_key UNIQUE` is our key for the YooKassa `Idempotence-Key` header, it survives a crash/retry.
- `provider_refund_id` is nullable and unique only when not NULL; `refunds_active_idx` holds the MVP invariant of one active/successful refund per payment.
- `status` includes `canceled`: YooKassa may complete a refund in this status (`api/yookassa/API.md` §5).

### 3.6. `analytics_events`

```sql
CREATE TABLE analytics_events (
    id          BIGSERIAL   PRIMARY KEY,
    user_id     BIGINT      NOT NULL REFERENCES users(telegram_id) ON DELETE RESTRICT,
    event_type  TEXT        NOT NULL,
    event_key   TEXT,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    metadata    JSONB       NOT NULL DEFAULT '{}'::jsonb,
    CHECK (event_type <> ''),
    CHECK (event_key IS NULL OR event_key <> '')
);

CREATE INDEX analytics_events_user_type_time_idx
    ON analytics_events (user_id, event_type, occurred_at DESC);

CREATE INDEX analytics_events_type_time_idx
    ON analytics_events (event_type, occurred_at DESC);
```

- `event_type` is the base event type: `bot_start`, `consent_declined`, `subscription_viewed`, `payment_requested`, `nudge_sent`.
- `event_key` refines the event. For `nudge_sent` it stores the nudge class (`payment_followup`, `sub_low_balance`, `sub_empty_balance`, legacy `trial_ending`/`sub_expiring`/`sub_expired`). Not unique: repeated actions and a new payment cycle must stay in the history.
- `metadata` is an allowlist at the storage level: for `nudge_sent` only `{"class":"..."}` is saved, for the other #12 events `{}`. We do not write the meeting URL, email or payment/provider ids here.
- `ON DELETE RESTRICT`: the analytics history blocks a physical DELETE of a user just like payments/jobs do; public deletion of personal data remains an anonymization task, not a cascade delete.

### 3.7. `jobs`

```sql
CREATE TABLE jobs (
    id                     BIGSERIAL   PRIMARY KEY,
    user_id                BIGINT      NOT NULL REFERENCES users(telegram_id) ON DELETE RESTRICT,
    subscription_id        BIGINT      REFERENCES subscriptions(id) ON DELETE RESTRICT,
    meeting_url            TEXT        NOT NULL,
    status                 TEXT        NOT NULL CHECK (status IN (
                                          'created','joining','recording','processing',
                                          'transcribed','summarizing','awaiting_reauth',
                                          'done','failed_join','failed','failed_summary'
                                       )),
    is_trial               BOOLEAN     NOT NULL,
    external_job_id        TEXT,
    provider               TEXT        NOT NULL DEFAULT '',  -- creating provider (mock|speech2text|mymeet): Fetch/Cancel resolve it by this field, not by the active TRANSCRIPTION_PROVIDER, otherwise after a provider switch a foreign external_job_id gives HTTP 400 (added by migration 00008, job-provider slice)
    status_message_id      BIGINT,      -- id of the Telegram status message; the bot edits it while processing; NULL: no message (added by migration 00006, lifecycle-controls)
    summary_md             TEXT,        -- cache of the generated F3 summary BEFORE delivery (migration 00007): a Deliver failure will not cause a repeated paid Summarize on retry (orchestrator summarizeJob); NULL: not generated / summary-off
    reserved_minutes       INTEGER     NOT NULL DEFAULT 0,
    billed_minutes         INTEGER     NOT NULL DEFAULT 0,
    duration_seconds       INTEGER,
    recording_started_at   TIMESTAMPTZ,
    recording_finished_at  TIMESTAMPTZ,

    last_error             TEXT,
    raw_provider_response  JSONB,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (
        (is_trial = true  AND subscription_id IS NULL) OR
        (is_trial = false AND subscription_id IS NOT NULL)
    ),
    CHECK (reserved_minutes >= 0 AND reserved_minutes <= 60),
    CHECK (billed_minutes >= 0),
    CHECK (duration_seconds IS NULL OR duration_seconds >= 0),
    CHECK (
        recording_finished_at IS NULL
        OR recording_started_at IS NULL
        OR recording_finished_at >= recording_started_at
    )
);

CREATE UNIQUE INDEX jobs_active_dedup_idx
    ON jobs (user_id, meeting_url)
    WHERE status NOT IN ('done','failed_join','failed','failed_summary');

CREATE INDEX jobs_worker_idx
    ON jobs (status, updated_at)
    WHERE status NOT IN ('done','failed_join','failed','failed_summary','awaiting_reauth');
```

- `subscription_id` shows which subscription the job was charged against; the CHECK guarantees `is_trial=true` if and only if `subscription_id IS NULL`.
- `raw_provider_response JSONB` is the raw provider response for post-mortem.
- `jobs_active_dedup_idx` is partial: protection against duplicates only among live Jobs, legitimate repeats (a regular meeting room) are allowed. **Created by migration `00005`** (payments slice). `failed_summary` is part of `WHERE status NOT IN (...)`: the final status frees the URL for resubmission.
- `jobs_worker_idx` is partial: the selection for `FOR UPDATE SKIP LOCKED` without scanning final Jobs. `awaiting_reauth` is excluded in advance: the worker must not pick it up in the future Google slice. **Already created** in migration `00003` and recreated in `00004` with the final `WHERE`.
- `jobs_user_history_idx`: its consumer appeared in the demo-prep slice: `Storage.ListUserJobs` (`WHERE user_id=$1 AND created_at > $2 ORDER BY created_at DESC`) feeds the "My calls" panel (the user's call history for the last week + the status of each + re-delivery). **The index is NOT yet created in migrations**: on demo data (tens of rows) the query does a seq scan, this is not a blocker. Create it with a separate forward migration when the data grows or if the history panel becomes a hot path.
- **The `provider` column was added by migration `00008`** (job-provider slice, `ALTER TABLE jobs ADD COLUMN provider TEXT NOT NULL DEFAULT ''`). It records the creating provider for resolving Fetch/Cancel in the "My calls" panel.
- **The quota columns `subscription_id`/`is_trial` were added by migration `00005`** (payments slice, `ALTER TABLE jobs` + CHECK `jobs_quota_check`). `reauth_from` does not exist yet: it will be added by a separate post-0.1 Google slice.
- **The minute columns `reserved_minutes`/`billed_minutes`/`duration_seconds` were added by migration `00009`**. `reserved_minutes` holds the reserve at Job creation, `billed_minutes` the minutes actually charged after terminal billing, `duration_seconds` the billing duration in seconds or `NULL` for legacy rows.
- `recording_started_at` / `recording_finished_at` were added by migration `00009`: the observable recording window per the state machine. For Speech2Text this is the main source of billing duration, because `result/json chunks[].time.to` reflects the ASR timecodes of speech, not the duration of the bot's presence in the meeting.
- `jobs_awaiting_reauth_idx`, `reauth_from` and `google_accounts` are the post-0.1 Google target; the applied migrations do not create them yet.
- Working phases of release 0.1: `joining` = the bot is connecting, `recording` = the bot is in the meeting, `processing` = the bot left and transcription is running, `summarizing` = the transcript is delivered and the summary is being prepared. `transcribed`/`failed_join`/`awaiting_reauth` remain dormant/target values in the CHECK.

### 3.8. Backfill and rollback of `00009`

Migration `00009_add_minute_quotas.sql` added the minute fields next to the legacy meeting fields and ran a backfill:

- `users.trial_minutes_used = trial_meetings_used * 60`.
- `users.trial_minutes_done = max(trial_meetings_done * 60, chargeable trial jobs * 60)`, where chargeable trial jobs = `jobs.is_trial=true AND status IN ('done','failed_summary')`.
- `subscriptions.minutes_limit = meetings_limit * 60`.
- `subscriptions.minutes_balance = meetings_balance * 60`.
- `jobs.reserved_minutes = 60` for `created|joining|recording|processing|transcribed|summarizing|awaiting_reauth|done|failed_summary`; `0` for `failed|failed_join`.
- `jobs.billed_minutes = 60` for `done|failed_summary`; `0` for the other statuses.
- `jobs.duration_seconds = NULL` for all legacy rows.

Reason to include the dormant `transcribed|awaiting_reauth` in the reserve backfill: these statuses are allowed by the CHECK and are not a terminal failure. If such a row appears before the runtime switch, it must continue with its saved reserve.

Reason to count `failed_summary` in the user-level done backfill: under the hourly contract a summary failure after the transcript was delivered is a rendered service. The legacy `trial_meetings_done` was incremented only on `done`, so the aggregate over jobs protects F9 and the future balance from understatement.

Historical limitation of a schema-only deploy: `00009` was not meant for a separate deploy under live writes without a subsequent catch-up, because the old runtime wrote only `meetings_*`. After the minute runtime was enabled, new rows write the minute fields right away.

The down migration of `00009` is structural: it drops the minute indexes and minute columns, the legacy `meetings_*` remain. This is safe only before the minute runtime is enabled; a semantic rollback after real minute billing is lossy and requires a separate owner gate.

## 4. Go types for `domain.go`

`domain.go` is the source of truth for the Go structs; this section records only the mapping to the DB and the nullable boundaries, to avoid duplicating code.

| Go type | Table | Important mappings |
|---|---|---|
| `User` | `users` | `ConsentAt` and `Notified*At` are nullable; trial counters are non-null |
| `Job` | `jobs` | `SubscriptionID`, `ExternalJobID`, `StatusMessageID`, `SummaryMD` are nullable; `Provider` is non-null default `''` |
| `Subscription` | `subscriptions` | balance/limit are non-null, `expires_at > paid_at` |
| `Payment` | `payments` | nullable charge/provider/receipt/raw fields; `Amount` in kopecks as `int64` |
| `Refund` | `refunds` | `ProviderRefundID`, `Reason`, `CompletedAt` are nullable |

The statuses in the DB are wider than the current Go code: `transcribed`, `failed_join`, `awaiting_reauth` are allowed by the CHECK as target/dormant values. In release 0.1 the working Go statuses are: `created`, `joining`, `recording`, `processing`, `summarizing`, `done`, `failed`, `failed_summary`. `GoogleAccount`, `JobAwaitingReauth` and `reauth_from` belong to the post-0.1 Google slice.

## 5. Transactional patterns (critical nodes)

### 5.1. Creating a Job with a minute reserve

An atomic `UPDATE ... WHERE used < limit` catches the "two messages within 200ms" race by DB physics.

```sql
-- Subscription (1:1 model: a user may have several active rows; we take the
-- nearest row with a full 60-minute reserve if there is one; we use a partial
-- remainder only when there is no full row).
WITH selected AS (
    SELECT id, LEAST(60, minutes_balance) AS reserved_minutes
    FROM subscriptions
    WHERE user_id = $1 AND expires_at > now() AND minutes_balance > 0
    ORDER BY CASE WHEN minutes_balance >= 60 THEN 0 ELSE 1 END, expires_at ASC
    LIMIT 1
    FOR UPDATE
)
UPDATE subscriptions s
SET minutes_balance = s.minutes_balance - selected.reserved_minutes
FROM selected
WHERE s.id = selected.id
RETURNING s.id, selected.reserved_minutes;

-- If nothing was charged to a subscription: trial
UPDATE users
SET trial_minutes_used = trial_minutes_used + LEAST(60, $2 - trial_minutes_used)
WHERE telegram_id = $1 AND trial_minutes_used < $2  -- $2 = trialLimitMinutes
RETURNING LEAST(60, $2 - (trial_minutes_used - LEAST(60, $2 - trial_minutes_used))) AS reserved_minutes;
```

If the first UPDATE returned a row: `INSERT INTO jobs (is_trial=false, subscription_id=$id, reserved_minutes=$reserved)`. If not, and the second UPDATE returned a row: `INSERT INTO jobs (is_trial=true, subscription_id=NULL, reserved_minutes=$reserved)`. If both are empty: reject. All in one pgx transaction. **`FOR UPDATE` on the subscription selection is mandatory**, otherwise two parallel requests will pick the same subscription.

### 5.2. Reserve refund on an early `failed`

**A status guard is mandatory**, otherwise two parallel ticks cause a lost update (the balance grows).

```sql
BEGIN;

-- Status change with a double-transition guard
UPDATE jobs
SET status = 'failed', updated_at = now()
WHERE id = $1 AND status IN ('created','joining','recording','processing')
RETURNING is_trial, subscription_id, user_id;

-- Roll back ONLY if RowsAffected = 1
-- For a trial (is_trial=true)
UPDATE users SET trial_minutes_used = trial_minutes_used - $reserved_minutes
WHERE telegram_id = $user_id;

-- OR for a subscription (is_trial=false)
UPDATE subscriptions
SET minutes_balance = LEAST(minutes_limit, minutes_balance + $reserved_minutes)
WHERE id = $subscription_id;

COMMIT;
```

If the first UPDATE returned `RowsAffected = 0`, this is a second tick trying to "roll back" an already closed Job. Do nothing.

**Which statuses lead to which outcome (full refund/charge specification):**
- manual cancel `created|joining` to `failed` **with a reserve rollback** via `CancelJob`; `recording` is not canceled manually but stopped through the stop-result path below.
- `created`/`joining`/`recording`/`processing` to `failed` **with a reserve rollback** if no transcript was received (the provider did not accept the task, the bot did not join, stop gave no result or transcription failed). The guard above lists exactly these no-result statuses.
- `recording` manual/watchdog stop to `processing` with the marker `jobs.last_error='manual_stop'|'watchdog_stop'`; if after the stop the provider returns a non-empty transcript, the path becomes a billable `done`/`failed_summary`, not a refund.
- `summarizing` to `failed_summary` **without an early refund**: the transcript-only file was delivered but the summary was not produced. A separate transition with the guard `WHERE id=$1 AND status='summarizing'`; terminal billing finalizes `billed_minutes` by `duration_seconds`.
- `failed_summary` to `done` is possible only via `SetTerminalJobSummary` after the user restores the summary from the "My calls" panel: `summary_md` is updated, `last_error` is cleared, billing/quotas are not recalculated.
- `awaiting_reauth` is not final, does not fail by timeout (it waits for `/reconnect`), the reserve is held.

So a full refund of `reserved_minutes` is done ONLY on transitions from early statuses before the service is rendered; a late summary failure goes through terminal billing. This is critical for the money logic: do not implement the refund as a single guard for all statuses.

> **Implemented (slice provider-start-timeout + stop-result):** `FailJobWithQuotaRefund` carries the guard `WHERE id=$1 AND status IN ('created','joining','recording','processing')`: the active phases before a billable terminal result. `created` was added for submit-fail/start-timeout: the provider has not yet accepted the task, but the quota was already reserved in `CreateJob`, so it must be returned. `CancelJob` is intentionally narrower and works only for manual cancel of `created|joining`. `failJob` is called from `submitJob` (permanent HTTP submit error, retry-limit, start-timeout) and `pollJob` (StatusFailed, joining-timeout, no-result stop). For an s2t stop-result, `last_error` temporarily holds the marker `manual_stop|watchdog_stop`: `StatusFailed` after such a stop first tries `Fetch`; a non-empty transcript leads to `done`/`failed_summary` and terminal billing, an empty transcript or a fetch failure leads to `failed` with a refund. The `summarizing` status is **intentionally excluded** from the guard: the transcript is already delivered, the meeting is used, and a summary failure goes through `SetJobFailedSummary` without a rollback (§5.2, `architecture.md` §8.4).

### 5.3. Transition to `done`

The reserve is already made, but terminal billing adjusts the result to the actual duration.

```sql
UPDATE jobs
SET status = 'done',
    billed_minutes = $final_billed_minutes,
    duration_seconds = COALESCE($duration_seconds, duration_seconds),
    updated_at = now()
WHERE id = $1
  AND status IN ('joining','recording','processing','summarizing')
  AND billed_minutes = 0
```

`SetJobDone` allows `joining|recording|processing|summarizing` to cover the s2t fallback and polling skipping phases. The full terminal billing version is in §5.7.

### 5.4. Payment idempotency (1:1 model: payment = subscription row)

Direct payment links create a `payments.status='pending'` row before payment. Finalization happens only after `GET /v3/payments/{id}` returns status `succeeded` and `paid=true`.

**Creating a direct pending payment:**

```sql
INSERT INTO payments (
    user_id, status, amount, currency,
    payment_intent_key, invoice_payload, metadata, raw_payload
)
VALUES ($1, 'pending', $2, $3, $4, $5, $6, $7)
ON CONFLICT DO NOTHING; -- provider charge or intent key
```

The direct flow is two-phase: first `StartDirectPaymentIntent` under `users FOR UPDATE` creates a local
intent without `provider_payment_charge_id`, so that a second parallel callback does not make a second external
`POST /v3/payments`. After a successful YooKassa response `AttachDirectProviderPayment` fills in
`provider_payment_charge_id = payment.id`, `raw_payload`, `metadata`, amount and currency. If the external
POST failed before the provider id, `FailDirectPaymentIntent` moves the local intent to `failed`.

If a conflict already exists, the legacy `CreatePendingPayment` reads the existing row and compares `user_id`, amount, currency, provider id and intent key. A conflict with different semantics is an error, not a silent reuse.

**Finalization by polling:**

```sql
BEGIN;

SELECT id, user_id, subscription_id, amount, currency, metadata
FROM payments
WHERE provider_payment_charge_id = $1
FOR UPDATE;

-- if the status is already succeeded/refunded -> changed=false
-- if the status is not pending or subscription_id already exists -> invariant error

INSERT INTO subscriptions (user_id, paid_at, expires_at,
    meetings_limit, meetings_balance, minutes_limit, minutes_balance)
VALUES (
    $user_id, now(), now() + INTERVAL '30 days',
    $minutes_per_period / 60, $minutes_per_period / 60, $minutes_per_period, $minutes_per_period
)
RETURNING id, expires_at;

UPDATE payments
SET status = 'succeeded',
    paid_at = now(),
    provider_paid_at = $provider_paid_at,
    subscription_id = $subscription_id,
    payment_method_id = CASE WHEN $auto_renew AND $payment_method_saved THEN $payment_method_id ELSE payment_method_id END,
    raw_payload = $raw_payload
WHERE id = $payment_id AND status = 'pending' AND subscription_id IS NULL;

COMMIT;
```

Before the transaction, `PaymentPoller` compares the YooKassa response with the local intent: provider id, amount, currency and `metadata.user_id`. For a direct/recurring payment with a `payment_intent_key` the metadata user id is mandatory; empty metadata is allowed only for a legacy Telegram payment. If the payment was created with `metadata.auto_renew=true` and YooKassa returned `payment_method.saved=true`, finalization saves `payments.payment_method_id` and upserts `recurring_profiles` with `next_charge_at = MAX(active subscriptions.expires_at)`, updating `consent_at`. For a recurring payment without a new `saved=true`, finalization updates the existing profile: `status='active'`, new `next_charge_at`, `last_payment_id`, `attempts_count=0`, `last_error=NULL`, but only if the profile is not `disabled` and not `blocked`. Repeated polling of the same `payment.id` returns `changed=false` and does not create a second subscription.


**Recurring worker (#9d):**

```sql
WITH due AS (
    SELECT user_id, payment_method_id, status, next_charge_at
    FROM recurring_profiles rp
    WHERE (
        status IN ('active', 'charge_failed')
        OR (
            status = 'charging'
            AND EXISTS (
                SELECT 1 FROM payments p
                WHERE p.payment_intent_key = format('recurring:%s:%s', rp.user_id, floor(EXTRACT(EPOCH FROM rp.next_charge_at))::bigint)
                  AND p.status = 'pending'
                  AND p.provider_payment_charge_id IS NULL
            )
        )
      )
      AND next_charge_at <= now()
      AND payment_method_id IS NOT NULL
      AND payment_method_id <> ''
      AND updated_at < now() - INTERVAL '2 minutes'
      AND NOT EXISTS (
          SELECT 1 FROM payments p
          WHERE p.payment_intent_key = format('recurring:%s:%s', rp.user_id, floor(EXTRACT(EPOCH FROM rp.next_charge_at))::bigint)
            AND (p.status = 'succeeded' OR (p.status = 'pending' AND p.provider_payment_charge_id IS NOT NULL))
      )
    ORDER BY next_charge_at, user_id
    LIMIT $limit
    FOR UPDATE SKIP LOCKED
)
UPDATE recurring_profiles rp
SET updated_at = now()
FROM due
WHERE rp.user_id = due.user_id
RETURNING due.user_id, due.payment_method_id, due.status, due.next_charge_at;
```

For each due row the worker builds `payment_intent_key = recurring:<user_id>:<next_charge_at_unix>` from the original `next_charge_at`. `StartRecurringPaymentIntent` in one transaction checks the current profile, moves `active|charge_failed -> charging` and creates a local `payments.status='pending'` intent without a provider id. If the user has already disabled the profile, the profile is blocked or `next_charge_at` changed, the method returns `created=false` and the external `POST /payments` is not made.

After a successful external POST `AttachRecurringProviderPayment` fills in `provider_payment_charge_id`, and `PaymentPoller` finalizes the payment like a regular direct payment. A successful recurring payment sets the profile to `active`, resets errors and sets a new `next_charge_at`; if during `charging` the profile was disabled by the user (`disabled`) or moved to `blocked`, the successful in-flight payment creates a subscription, but the profile is not reactivated (`blocked` waits for a manual check).

On a local POST/attach error before the provider id, `MarkRecurringRetry` returns the profile from `charging` to `charge_failed` without moving `next_charge_at`; if the process crashed and the profile stayed `charging`, the due-select will pick it up only when there is a pending intent without a provider id: the next tick within 23 hours repeats the same `POST` with the same `Idempotence-Key`. After 23 hours `StartRecurringPaymentIntent` moves the local intent to `failed` and the profile to `blocked`, writes `recurring_intent_blocked` and no longer retries automatically. On a confirmed temporary `canceled`, the recurring intent moves the payment to `failed` and the profile to `charge_failed`/`blocked` in one transaction; revoked/mandate reasons go to `blocked`. `DisableRecurring` is applicable to any status except an already `disabled` one, clears `payment_method_id` and returns `changed bool`.

Legacy `successful_payment` remains for the old Telegram Payments path:

A successful payment idempotently writes `payments` by `telegram_payment_charge_id` and creates a new `subscriptions` row with an independent `expires_at = now()+30 days`. These are **two statements in one transaction**: the final `UPDATE payments` inside the same CTE does not see the row inserted by `new_payment`, so `subscription_id` is set by a separate statement.

`SELECT telegram_id FROM users WHERE telegram_id = $1 FOR UPDATE` is the first statement of the user's transaction: each new subscription row is independent (`expires_at = now()+30 days`, no chain-reading of `MAX(expires_at)` as the base of the new period), so the lock is not required for this particular race, but it is kept as a general serialization of concurrent successful payments of one user in one transaction (protection against future dependencies on the state of `users`/subscriptions in the same code). Verified by the test `TestRecordSuccessfulPayment_ConcurrentSameUser` (billing_test.go): independent periods are created correctly without races. Payments of different users do not block each other: a specific `users` row is locked, not the table.

```sql
SELECT telegram_id FROM users WHERE telegram_id = $1 FOR UPDATE;

WITH new_payment AS (
    INSERT INTO payments (
        user_id, status, amount, currency,
        telegram_payment_charge_id, provider_payment_charge_id,
        invoice_payload, metadata, raw_payload, provider_paid_at, paid_at
    )
    VALUES ($1, 'succeeded', $2, $3, $4, $5, $6, $7, $8, $9, now())
    ON CONFLICT (telegram_payment_charge_id) DO NOTHING
    RETURNING id
),
new_sub AS (
    INSERT INTO subscriptions (
        user_id, paid_at, expires_at,
        meetings_limit, meetings_balance,
        minutes_limit, minutes_balance
    )
    SELECT $1, now(), now() + INTERVAL '30 days',
           $legacy_meetings, $legacy_meetings,
           $minutes_limit, $minutes_limit
    WHERE EXISTS (SELECT 1 FROM new_payment)
    RETURNING id
)
SELECT (SELECT id FROM new_payment), (SELECT id FROM new_sub);

UPDATE payments SET subscription_id = $2 WHERE id = $1;  -- $1=new_payment.id, $2=new_sub.id
```

A Telegram retry with the same charge id yields an empty `new_payment`, so `new_sub` is not created and statement 2 is skipped. A concurrent retry of the same charge id is serialized by the UNIQUE index. Two different successful_payment events of one user are two independent legitimate subscription rows, each with its own `expires_at = now()+30 days`; `FOR UPDATE` on the `users` row (above) remains a general protection of the payment transaction and does not depend on a shared expiry between rows.

### 5.5. Legacy `pre_checkout_query`: in-memory validation only

This section applies to the old Telegram Payments path. In direct payment links Telegram `pre_checkout_query` is not used: payment confirmation goes through YooKassa polling (§5.4).

Telegram gives **10 seconds** to answer (a hard limit, after that `Bad Request: query is too old`). Minus the network roundtrip, the target response time is **<=500 ms**, exceeding it is logged as WARN, systematic overruns (>1% over a rolling window) trigger an alert. Legacy path details: `docs/api/yookassa/API.md`.

For the legacy path: no DB trips with JOINs. Validation is the `invoice_payload` format and the existence of the user in an in-memory cache (or a single `SELECT` by PK). There is NO "no active subscription" check here: under the 1:1 model (§3.3) several active periods are normal, and a repeated payment legitimately creates an independent period (`expires_at = now()+30 days`, §5.4); retry dedup is provided by `UNIQUE(telegram_payment_charge_id)` in `successful_payment`, not by pre_checkout. The business logic of subscription creation lives only in `successful_payment`.

### 5.6. F9 ticks and payment nudges

```sql
SELECT u.telegram_id,
       COALESCE(MAX(p.paid_at), '-infinity'::timestamptz) AS last_paid_at
FROM users u
JOIN subscriptions s ON s.user_id = u.telegram_id AND s.expires_at > now() AND s.minutes_balance > 0
LEFT JOIN payments p ON p.user_id = u.telegram_id AND p.status = 'succeeded'
GROUP BY u.telegram_id, u.notified_sub_expiring_at
-- 1:1 model: a user may have several active rows with independent
-- expires_at; we take MAX(expires_at), exactly one per user (GROUP BY removes duplicates).
HAVING MAX(s.expires_at) BETWEEN now() + INTERVAL '3 days' AND now() + INTERVAL '4 days'
   AND (u.notified_sub_expiring_at IS NULL
        OR u.notified_sub_expiring_at < COALESCE(MAX(p.paid_at), '-infinity'::timestamptz));
```

`COALESCE(..., '-infinity')` is needed so that `MAX(paid_at)=NULL` does not break the comparison. Under the 1:1 model a user may have several active subscriptions, so F9 aggregates by `u.telegram_id` and pushes by `MAX(s.expires_at)`, exactly one row per user.

The new #12 nudges are deduplicated through `analytics_events`:

- `payment_followup`: the last `subscription_viewed`/`payment_requested` is older than 24 hours; no active subscription, no `payments.status='succeeded'` after the view, no `nudge_sent/payment_followup` after the view.
- `sub_low_balance`: the user's active subscriptions give `SUM(minutes_balance) > 0 AND < 60`; no `nudge_sent/sub_low_balance` after the last successful payment. It has priority over `sub_expiring`, so that one tick does not send two subscription pushes.
- `trial_ending`: filters out any calendar-active subscription, even if `minutes_balance = 0`; paying users need a subscription nudge, not a trial nudge.
- `sub_empty_balance`: there is a calendar-active subscription, but the total `minutes_balance = 0`; no `nudge_sent/sub_empty_balance` after the last successful payment. Calendar-expired rows are handled by the legacy `sub_expired`.
- `sub_low_balance` and `sub_empty_balance` do not select a user while there is an active non-trial Job with `reserved_minutes > 0`: `CreateJob` reserves up to 60 minutes at the start of a call, and until terminal billing the balance may look artificially low.

The invariant holds for all classes: first send the message, then set the mark. For the new classes the mark = `analytics_events(event_type='nudge_sent', event_key=<class>)`; for the old F9 classes, `users.notified_*_at` and `nudge_sent` are written in one transaction.

### 5.7. Terminal billing into `done` / `failed_summary`

The reserve was already made at Job creation (§5.1). On terminal `done` or `failed_summary` we compute the actual charge by duration:

- duration source priority: the caller's explicit value, the saved `jobs.duration_seconds`, the observable window `recording_started_at -> now()`, fallback 3600s;
- for Speech2Text the caller passes `Result.DurationSeconds` only if the Job has not yet reached `processing` with a saved observable window; if the window is saved, the caller passes `0`, so that terminal billing takes `jobs.duration_seconds`;
- `duration_seconds <= 0` after all sources turns into the fallback 3600s;
- `1..59` seconds = 1 minute;
- `>=60` seconds is rounded up to a full minute;
- if wanted is less than the reserve, the unused part is returned to the original source;
- if wanted is greater than the reserve, the extra is collected only from the original source and only with a sufficient active remainder;
- no debt, negative balance or charge of the next subscription is created.

```sql
WITH closed AS (
    UPDATE jobs
    SET status = $terminal_status,
        billed_minutes = $final_billed_minutes,
        duration_seconds = $duration_seconds,
        updated_at = now()
    WHERE id = $1
      AND status IN ('joining','recording','processing','summarizing')
      AND billed_minutes = 0
    RETURNING is_trial, user_id, billed_minutes
)
UPDATE users
SET trial_minutes_done = trial_minutes_done + closed.billed_minutes
FROM closed
WHERE users.telegram_id = closed.user_id
  AND closed.is_trial = true;
```

If the terminal UPDATE returned `RowsAffected=0` (repeated provider polling), the second one also does nothing, all atomic in one transaction. `summary_md` is written before the summary is delivered, so that a delivery retry does not call the LLM again.

A later summary restore from the "My calls" panel does not call terminal billing again: `SetTerminalJobSummary` works only with `done|failed_summary`, saves `summary_md`, and for `failed_summary` moves the status to `done` and clears `last_error`.

### 5.8. Parallel calls, active cap and same-URL dedup at Job creation

`CreateJob` allows several active different `meeting_url` values if there is enough quota and `active < 4`. The money limiter is the atomic reserve §5.1; cap=4 protects provider capacity. The cap check is atomic: at the start of the transaction we lock the `users` row, then check for a same-URL duplicate and count active Jobs. With `active >= 4` the reserve has not yet been made.

Same-URL dedup remains a physical DB constraint:

```sql
CREATE UNIQUE INDEX jobs_active_dedup_idx
ON jobs (user_id, meeting_url)
WHERE status NOT IN ('done','failed_join','failed','failed_summary');
```

The same URL returns `ErrDuplicateActiveJob` before the quota reserve; a unique violation on `jobs_active_dedup_idx` is mapped to the same error and rolls back the transaction. Different URLs are limited by the quota and cap=4; account-wide provider concurrency remains a hardening track.

### 5.9. Full refund: subscription deactivation (two-phase)

In the MVP partial refunds are forbidden: all refunds are full, refunds:payment is strictly 1:1 (§3.5, manual process support to YooKassa API). The refund is **two-phase**: a deterministic `Idempotence-Key` is needed BEFORE `POST /v3/refunds`, while `provider_refund_id` comes only from the response (`api/yookassa/API.md` §3/§11.3); a single-stage INSERT with `provider_refund_id` in the direct REST flow is physically impossible.

**Phase 1, `BeginRefund` (before calling YooKassa):** we create a pending refund with our deterministic `idempotence_key`. `refunds_active_idx` (§3.5) physically rejects a second refund for the same payment.

```sql
INSERT INTO refunds (payment_id, idempotence_key, amount, status)
SELECT id, $2, amount, 'pending'
FROM payments
WHERE provider_payment_charge_id = $1 AND status = 'succeeded'
ON CONFLICT DO NOTHING            -- suppressed by refunds_active_idx (an active pending/succeeded refund for the payment) OR by idempotence_key UNIQUE (a retry of the same intent with the same key, including when the previous refund is already failed/canceled and dropped out of refunds_active_idx); both branches -> RowsAffected=0 -> take the existing one with a SELECT
RETURNING id, idempotence_key;
-- RowsAffected=0: the payment is not succeeded OR an active refund already exists: take the existing one with a separate SELECT.
```

Then `billing.CreateRefund(provider_payment_charge_id, amount, idempotence_key)` -> `POST /v3/refunds` (retry with the same key, `api/yookassa/API.md` §14) -> `provider_refund_id` + status.

**Phase 2, `FinalizeRefund` (after the YooKassa response), idempotent by `refunds.id`:**

```sql
BEGIN;

-- 1. Finalize the refund (provider_refund_id from YooKassa; idempotence_key is already set from phase 1)
UPDATE refunds
SET status = $status, provider_refund_id = $provider_refund_id,
    raw_payload = $raw, completed_at = now()
WHERE id = $refund_id AND status = 'pending'
RETURNING payment_id;
-- RowsAffected=0: the refund is already finalized (repeated webhook/trigger), exit without COMMIT.

-- 2. Only on a SUCCESSFUL refund (status='succeeded') mark the payment
UPDATE payments
SET status = 'refunded'
WHERE id = $payment_id AND status = 'succeeded'
RETURNING subscription_id, user_id;

-- 3. Deactivate EXACTLY the subscription row that this payment bought (1:1 model).
--    One field: expires_at = now() (the balance is not touched: it is irrelevant with an expired term,
--    §5.1 filters by expires_at > now; the refunded row correctly stays visible to F9).
UPDATE subscriptions
SET expires_at = now()
WHERE id = $subscription_id AND expires_at > now();

-- 4. Disable the user's auto-renewal after a successful refund, so that the refund does not
--    leave future merchant-initiated charges active.
UPDATE recurring_profiles
SET status = 'disabled',
    payment_method_id = NULL,
    disabled_at = COALESCE(disabled_at, now()),
    updated_at = now()
WHERE user_id = $user_id AND status <> 'disabled';

COMMIT;
```

The idempotency guarantee is double: `refunds_active_idx` (phase 1) physically prevents creating a second refund for the payment; the guard `WHERE id=$refund_id AND status='pending'` (phase 2) rejects repeated finalization. `Idempotence-Key` (phase 1) protects against a double charge on the YooKassa side on a REST retry. On a `failed`/`canceled` refund (phase 2 step 1 sets the status) the payment and subscription are NOT touched: the money was not returned, access is kept.

**The 1:1 model makes deactivation deterministic:** a payment bought exactly one subscription row (§5.4), a refund deactivates exactly that row; the user's other paid periods are not affected (no rollover into a single row, which closes rank 1 of the review). The user's active Jobs are not canceled (the provider has already connected the bot). New Jobs will not be accepted against this row (`expires_at` is in the past); if there are no other active subscriptions and no trial remainder, the request is rejected via `billing.go`. **A refund does not touch the trial:** the trial remainder grants access again, this is intentional (the user did not pay for the trial).

## 6. Open questions (for future sessions)

Resolved by revision 1.2 (2026-06-14, multi-agent DB review before the payments slice):
- ~~rollover-vs-refund (renewal into one row, a refund zeroes both periods)~~ -> **"row per payment" 1:1 model** (§3.3/§5.4): each payment = its own subscription row, a refund deactivates exactly that one.
- ~~The race on CREATING the first subscription (FOR UPDATE does not lock a nonexistent row, two subscriptions from two cards)~~ -> removed by the 1:1 model: there is no shared mutable row, two payments = two legitimate periods (§5.4).
- ~~`payments.subscription_id` as a separate UPDATE, risk of FK=NULL so a refund does not deactivate~~ -> the linking was pulled into the same transaction in §5.4.

Resolved in the hourly-subscription slice (issue #7):
- ~~The RENEWAL race (chain): two concurrent successful_payment events of ONE user read the same `MAX(expires_at)` and both extend from one base, so the user loses a paid period~~ -> removed together with the chain model itself: each row now gets an independent `expires_at = now()+30 days`, there is no shared state for a race on the new row's expiry. `FOR UPDATE` on the `users` row (§5.4) is kept as a general serialization of the transaction. Confirmed by the test `TestRecordSuccessfulPayment_ConcurrentSameUser`.
- ~~A single-stage refund is incompatible with a deterministic `Idempotence-Key`~~ -> two-phase `BeginRefund`/`FinalizeRefund` (§3.5 `idempotence_key`, nullable `provider_refund_id` + partial-unique, §5.9).
- ~~The `meetings_balance <= meetings_limit` CHECK broke on renewal~~ -> under the 1:1 model the invariant is true again (balance <= row limit), the CHECK is kept.
- ~~Dead `partially_refunded`/`amount_refunded` against the anti-scope~~ -> removed (§3.4).
- ~~Intake idempotency is fragile to a NULL charge_id~~ -> CHECK "succeeded implies charge_id NOT NULL" (§3.4).

Resolved earlier (1.1, 2026-06-04, senior review):
- ~~F9 NULL in MAX(paid_at)~~ -> §5.6 COALESCE with `-infinity`.
- ~~Parallel calls / same-URL dedup~~ -> §5.8.
- ~~Counter of "successfully completed" trial meetings~~ -> `users.trial_meetings_done` + §5.7.
- ~~OAuth refresh fail from `summarizing`~~ -> new status `awaiting_reauth`, not final.

Still open:
- **Multi-instance: double Submit.** `ClaimJobsForWork` for `created` jobs has no backoff filter + the claim-bump commits before `SetJobSubmitted` (in another tx after the 30s Submit). With ONE sequential worker there is no race; with a 2nd instance B re-claims the same `created` job, giving a second bot in the meeting. **A mandatory item BEFORE launching a 2nd instance** (not "later"): claim `created` atomically with a status change (created to joining via RETURNING) or add a backoff on created. Do not remove the status guard on money UPDATEs even on one worker (the cost is zero, its absence is a mine when scaling). **`jobs_active_dedup_idx` does NOT cover this case:** it deduplicates DIFFERENT rows of one user by `(user_id, meeting_url)`, while a double Submit is a repeated claim of the SAME `created` row (the same `job.id`), there is no second row to violate the UNIQUE. The fix is an atomic claim, not an index (the `storage.go` comment about the dedup index as multi-instance protection is misleading).
- **`storage.go` contract:** which functions we export, how `pgx.Tx` is passed between layers. To be decided when the module is detailed; it must be fixed BEFORE the scaffold, otherwise `billing.go` cannot share a transaction.
- ~~**Move the F9 ticks from `orchestrator.go` to `notifier.go`**~~ - **Done** (payments slice, `notifier.go`, commit `c3e8779`).
- **152-FZ "right to be forgotten":** `ON DELETE RESTRICT` on `payments`/`jobs` means `DELETE FROM users` will not work. Anonymization is needed while keeping the financial history. Two subtleties from the DB review of 2026-06-14: (1) `users.telegram_id` is a natural PK and an FK in all tables; anonymization `telegram_id := -id` is an UPDATE of the PK, but the schema has no `ON UPDATE CASCADE`, so it will fail. The choice BEFORE the public launch: either `ON UPDATE CASCADE` on the FKs to users, or a surrogate `BIGSERIAL` PK + `telegram_id UNIQUE` (more canonical, but changing the PK is expensive, and the code is already built on the natural telegram_id, so in the closed beta we keep the natural one). (2) Personal data (email/phone) also lives in `payments.raw_payload`/`metadata`, `jobs.raw_provider_response`, `jobs.meeting_url`: anonymizing `telegram_id` will NOT touch it, JSONB cleanup/masking is needed. A production hardening task, a compliance blocker for the public launch, deliberately deferred in the closed beta.
- **Payment audit / event log:** the history of payment/refund transitions is currently only in `raw_payload` (the last state) + ephemeral `slog`. For money disputes/reconciliation over a period an append-only transition log would be more reliable. Excessive for the MVP (30 users); `provider_paid_at` + `refunds.completed_at` are in place as a minimum. A full `payment_events` is production tech debt.
- **Orphan-Job recovery after restart:** a Job in `joining`/`recording`/`processing` stays "hanging" when the worker crashes. A procedure at `main.go` startup. A production hardening task (Stage 3).
- **Archiving old `jobs.raw_provider_response JSONB`:** with 30 users x 15 hours x 50 KB of notional provider raw per meeting the volume is still small. In a year, a procedure to delete data older than N days. A task after the public launch.
- **152-FZ consent text** for `consent_at` must be kept in sync with the website and the personal data processing policy. The basic consent gate is implemented.
- ~~**Exact `TRIAL_MEETINGS_LIMIT` and price**~~ - superseded by #7: the product canon is now a 3-hour trial and 1990 RUB/month for 15 hours; the runtime was moved to minute accounting in PR #26.
