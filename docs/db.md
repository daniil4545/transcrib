# Архитектура БД transcrib

> **Канон для:** применённой SQL-схемы миграций `00001`-`00015`, индексов, Go-типов `domain.go`, транзакционных паттернов (резерв квоты, идемпотентность платежей, F9-тики, refund).
> **Зеркала:** `architecture.md` §5 (короткое перечисление сущностей), `payments.md` (ссылки на §3.4/§5.4).
> **Не дублировать здесь:** контракт state machine (`architecture.md` §7), платёжный UX-поток (`payments.md` §1), продуктовые правила триала (`product.md` §5 F6).

Версия: 1.16 (2026-07-06, migration `00015`: erase YooKassa recurring token on disable).
Сопровождает `architecture.md` §5 — детализирует схему таблиц, индексы, Go-типы, транзакционные паттерны.

Изменения v1.15 → v1.16: migration `00015_recurring_profile_disable_erases_token` делает `recurring_profiles.payment_method_id` nullable и очищает сохранённый YooKassa token при `DisableRecurring`/отключении автопродления.
Изменения v1.14 → v1.15: migration `00014_align_recurring_next_charge` выравнивает `recurring_profiles.next_charge_at` для active-профилей с effective expiry пользователя (`MAX(active subscriptions.expires_at)`); новые подписочные строки создаются на фиксированные 30 дней вместо календарного месяца.
Изменения v1.13 → v1.14: #9d добавил migration `00013_recurring_profile_status_hardening`: статусы `charging` и `blocked`, default-on recurring при включённом `RECURRING_PAYMENTS_ENABLED`, atomic `StartRecurringPaymentIntent`, stale unknown-result -> `blocked`.
Изменения v1.12 → v1.13: #9c добавил чтение статуса `recurring_profiles` для `/balance`, пользовательское отключение через `DisableRecurring`, отключение автопродления при successful refund и structured logs на критических payment/recurring/refund переходах; новой миграции нет.
Изменения v1.11 → v1.12: #9b добавил recurring worker без новой миграции: due-select по `recurring_profiles`, merchant-initiated payment с deterministic `payment_intent_key`, общий polling/finalize и перевод профиля в `charge_failed` при canceled/error.
Изменения v1.10 → v1.11: миграция `00012_recurring_profiles` добавила `recurring_profiles` и сохранение `payments.payment_method_id` только если payment помечен `metadata.auto_renew=true` и ЮKassa вернула `payment_method.saved=true`.
Изменения v1.9 → v1.10: миграция `00011_analytics_events` добавила append-only `analytics_events` для UX-фактов `/start`, отказа от consent, просмотра подписки, клика оплаты и `nudge_sent`; деньги, подписки и встречи остаются источниками правды в `payments`/`subscriptions`/`jobs`.
Изменения v1.8 → v1.9: миграция `00010_yookassa_payment_links` добавила `users.payment_email`, `payments.payment_intent_key`, уникальность `provider_payment_charge_id`, direct pending/finalize flow по `payment.id` ЮKassa; successful direct payment создаёт подписку с `minutes_limit/minutes_balance`.
Изменения v1.7 → v1.8: `recording_started_at`/`recording_finished_at` добавлены в `00009`; terminal billing округляет секунды вверх до минуты и для Speech2Text использует observable окно записи, если оно уже сохранено, а provider duration берёт как fallback для прямого terminal result без окна.
Изменения v1.6 → v1.7: hourly runtime перевёл config, reserve/refund, terminal billing, `/balance` и F9 на minute-поля. Legacy `meetings_*` остаются в схеме и частично поддерживаются кодом как compatibility bridge до отдельного cleanup.
Изменения v1.5 → v1.6: миграция `00009` добавила минутные счётчики рядом с legacy `meetings_*`, backfill `1 встреча = 60 минут`, minute-based индексы для резерва/F9 и structural down migration.
Изменения v1.4 → v1.5: убраны дублирующие Go-snippet и подробная ASCII-ER; исторические пояснения сокращены до текущих инвариантов.
Изменения v1.3 → v1.4: разделена фактически применённая схема `00001`-`00008` и post-0.1 Google target; блок `jobs` приведён к реальным колонкам миграций (`provider`, `status_message_id`, `summary_md`) без незаведённых `reauth_from`/`transcript_doc_url`/`summary_doc_url`/`retry_count`.
Ранние ревизии: модель подписки `payment:subscription = 1:1`, двухфазный refund, `trial_meetings_done`, COALESCE в F9, same-URL dedup и parallel calls.

Billing go-live #7 принят как переход на 3 часа trial, 15 часов/мес и внутренний учет в минутах. В PR #26 выполнены schema, config/domain, provider duration, reserve, finalize billing, UX/F9, docs sync и cleanup модели продления подписки.

## 1. Зафиксированные решения

| # | Решение | Выбор | Почему |
|---|---|---|---|
| 1 | Scope канона | SQL-схема миграций + Go-типы `domain.go` | Один документ держит DB-инварианты, а runtime-алгоритмы ниже ссылаются на те же поля |
| 2 | Кодирование enum-статусов | `TEXT NOT NULL` + `CHECK` | Postgres `ENUM` плохо мигрируется (`ALTER TYPE` не в транзакции), `SMALLINT` нечитаем |
| 3 | Денежные суммы | `BIGINT` в копейках (199000 = 1990 ₽) | Telegram и ЮKassa отдают копейками — нулевые пересчёты на границе. `int64` нативен в Go, не тащим `decimal`-библиотеку. Стандарт fintech |
| 4 | Дедупликация Job | Частичный `UNIQUE (user_id, meeting_url) WHERE status NOT IN ('done','failed_join','failed','failed_summary')` | Защищает от случайных двойных нажатий, не ломает регулярные созвоны в Zoom personal meeting room |
| 5 | Google `refresh_token` | `TEXT` plaintext на post-0.1 Google-слайсе. **Шифрование `BYTEA` + AES-GCM ОБЯЗАТЕЛЬНО до публичного Google-режима** | На фазе закрытого Testing mode риск утечки бэкапа ниже, но Google API Security Requirements обязывают зашифровать до Production |
| 6 | Триал и баланс подписки | `users.trial_minutes_used` + `subscriptions.minutes_balance` с **резервом при создании Job, возвратом раннего резерва и terminal billing по duration** | Атомарный `UPDATE … WHERE used < limit` физически закрывает гонку «два сообщения за 200мс». O(1) вместо `count()`. Симметрично для триала и подписки |
| 7 | Состояние подписки в F9 | **Нет** денормализации `users.last_paid_at`. F9-тик считает `MAX(payments.paid_at WHERE status='succeeded')` на лету | Один источник правды. Корректно при refund/chargeback автоматически. На 30 юзеров — микросекунды |
| 8 | Telegram-библиотека | `gopkg.in/telebot.v4` | v3 заморожена с июня 2024. v4 — активно развивается, payments-модуль расширен |
| 9 | Структура `payments` | PK — `BIGSERIAL`. Direct flow: `payment_intent_key` и `provider_payment_charge_id` уникальны; legacy Telegram flow: `telegram_payment_charge_id` `UNIQUE` nullable. Поля `status` (FSM), `subscription_id`, `metadata`/`provider_paid_at`, `invoice_payload`, `fiscal_receipt_url`, `raw_payload JSONB`. Отдельная таблица `refunds` (двухфазный возврат, `idempotence_key`) | PK по charge id невозможен в стадии `pending`. Direct payment создаётся до оплаты, а финализация приходит polling'ом. FSM нужен для различения «висит» / «оплачено» / «возвращено». На MVP refunds:payment 1:1 (частичные вне скоупа); `amount_refunded`/`partially_refunded` убраны как мёртвые |
| 10 | Аналитические UX-события | Append-only `analytics_events`, без unique на `event_key` | Нужна история повторных просмотров/кликов и дедуп новых дожимов. Бизнес-факты не дублируем: `jobs`, `payments`, `subscriptions` остаются источниками правды |
| 11 | Автопродление | Отдельная `recurring_profiles` 1:1 к `users`; `subscriptions` не продлеваем in-place | Разделяет сохранённый способ оплаты и оплаченные периоды. Refund остаётся детерминированным по payment:subscription 1:1, а автосписание создаёт новый payment и новую subscription через общий polling/finalize |

## 2. ER-сводка

Применённые миграции `00001`-`00015` создают бизнес-таблицы: `users`, `jobs`, `subscriptions`, `payments`, `refunds`, `recurring_profiles`, и аналитическую таблицу `analytics_events`. `goose_db_version` — служебная таблица мигратора. `google_accounts` — post-0.1 target Google-слайса.

| Таблица | Связи | Ответственность |
|---|---|---|
| `users` | PK `telegram_id`; 1:N к `jobs`, `subscriptions`, `payments` | consent, trial counters, F9 notification timestamps, payment email для чека |
| `jobs` | FK `user_id`; nullable FK `subscription_id` | state machine встречи, provider id, Telegram status message, summary cache, quota source |
| `subscriptions` | FK `user_id`; 1:1 с успешным платежом через `payments.subscription_id` | оплаченный период и остаток минут |
| `payments` | FK `user_id`, nullable FK `subscription_id` | ЮKassa payment intent; direct idempotency по `payment_intent_key`/`provider_payment_charge_id`, legacy Telegram idempotency по `telegram_payment_charge_id` |
| `recurring_profiles` | PK/FK `user_id`, nullable FK `last_payment_id` | сохранённый способ оплаты ЮKassa, статус `active|charging|charge_failed|blocked|disabled` и дата следующего автосписания |
| `refunds` | FK `payment_id` | двухфазный полный возврат, idempotency key и provider refund id |
| `analytics_events` | FK `user_id` | UX-события и `nudge_sent` для аналитики/дедупа payment nudges |
| `google_accounts` | FK `user_id` | post-0.1: refresh token и Drive folder |

## 3. SQL-схема

Разделы 3.1 и 3.3-3.7 описывают фактически применённую схему миграций `00001`-`00015`. Раздел 3.2 — целевой post-0.1 Google-слайс; его таблицы и колонки ещё не заведены в текущих миграциях.

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

- `telegram_id` — PK напрямую. Telegram гарантирует уникальность и неизменность за юзером.
- `trial_meetings_used` / `trial_meetings_done` — legacy compatibility counters. После включения minute runtime код обновляет их мостом для старых тестов/отчётов, но источник истины для тарификации — minute-поля.
- `trial_minutes_used` — зарезервированные и фактически занятые минуты триала. `CreateJob` резервирует до 60 минут, ранний `failed` возвращает `reserved_minutes`, terminal billing возвращает unused или добирает extra в пределах лимита.
- `trial_minutes_done` — finalized chargeable trial minutes. Увеличивается на `jobs.billed_minutes` для `done` и `failed_summary`; питает F9-пуш «триал заканчивается» по реальному расходу, а не по резерву.
- `consent_at` — момент согласия на обработку ПД (152-ФЗ). До `consent_at IS NOT NULL` приём ссылок заблокирован (enforce в `tg.go`, FSM онбординга).
- 3 notification timestamps — защита F9 от повторов; при новой подписке логика F9 сравнивает их с `COALESCE(MAX(payments.paid_at WHERE status='succeeded'), '-infinity'::timestamptz)`.
- `payment_email` — email для чека ЮKassa. Запрашивается перед первой direct payment link оплатой, затем переиспользуется при продлении.

### 3.2. `google_accounts` (post-0.1 target, не применено)

```sql
CREATE TABLE google_accounts (
    user_id       BIGINT      PRIMARY KEY REFERENCES users(telegram_id) ON DELETE CASCADE,
    refresh_token TEXT        NOT NULL,    -- TODO: BYTEA + AES-GCM в продакшен-доводке (Этап 3), до публичного запуска
    folder_id     TEXT        NOT NULL,    -- ID папки в Drive юзера
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

- 1:1 с `users`, поэтому `user_id` сам PK.
- `ON DELETE CASCADE` — при удалении юзера вспомогательные данные едут с ним.
- Таблица появится отдельной миграцией Google-слайса вместе с `jobs.reauth_from` и `jobs_awaiting_reauth_idx`.

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

-- Все периоды юзера (любой баланс) — для §5.1/§5.6 агрегации (SUM баланса, MAX срока).
CREATE INDEX subscriptions_user_expiry_idx
    ON subscriptions (user_id, expires_at DESC);
```

- **Модель «строка на платёж» (1:1, решение 2026-06-14).** Каждый успешный платёж создаёт ОТДЕЛЬНУЮ строку subscriptions (§5.4 НЕ продлевает существующую). Каждая новая строка получает независимый `expires_at = now() + 30 days` (решение 2026-07-03: публично говорим «месяц», внутри считаем фиксированные 30 дней). Следствия: у юзера может быть несколько активных строк одновременно — это норма; refund деактивирует ровно ту строку, что купил возвращаемый платёж (§5.9, детерминированно); `/balance` и F9 (§5.6) **агрегируют по юзеру** (SUM баланса, MAX срока, DISTINCT). Для active recurring `recurring_profiles.next_charge_at` должен совпадать с effective expiry пользователя: `MAX(subscriptions.expires_at)` по активным строкам.
- `meetings_limit` / `meetings_balance` — legacy compatibility поля. После #7 они больше не являются источником истины для UX/F9/резерва; код поддерживает их мостом до отдельного cleanup.
- `minutes_limit` и `minutes_balance` — рабочий лимит и остаток подписки. Successful payment пишет `SubscriptionMinutesPerPeriod` (900 минут при 15 часах), `CreateJob` резервирует до 60 минут, terminal billing корректирует остаток по фактической duration.
- `subscriptions_user_active_minutes_idx` — minute-аналог резерва: активные подписки с остатком минут. `CreateJob` сначала выбирает ближайшую строку с `minutes_balance >= 60`, а частичную строку берёт только если полной нет.
- `subscriptions_expiring_idx` и `subscriptions_expiring_minutes_idx` partial — для F9-тика «подписка истекает через 3 дня». `subscriptions_user_expiry_idx` — для агрегаций §5.1/§5.6 (MAX expires_at по юзеру, любой баланс).

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
    -- Идемпотентность приёма не зависит от дисциплины вызывающего:
    -- succeeded/refunded обязаны нести charge_id (на нём держится ON CONFLICT §5.4).
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

- PK = `id BIGSERIAL`, потому что запись возможна до получения charge id.
- `payment_intent_key` — наш `Idempotence-Key` для `POST /v3/payments`; уникальность защищает повтор создания payment intent.
- `provider_payment_charge_id` — настоящий `payment.id` ЮKassa в direct flow; по нему polling и REST-refund. Уникальность защищает от дубля provider payment.
- `telegram_payment_charge_id UNIQUE` — legacy-идемпотентность ретраев Telegram Payments; в direct flow может быть NULL.
- `metadata`, `raw_payload`, `provider_paid_at`, `payment_method_id`, `fiscal_receipt_url` нужны для сверок, fallback-lookup и рекуррента. `payment_method_id` заполняем только при `metadata.auto_renew=true`, `payment_method.saved=true` и непустом `payment_method.id`.
- Частичные возвраты вне скоупа MVP: `partially_refunded` и `amount_refunded` не заводим.
- `ON DELETE RESTRICT` — нельзя удалить юзера с историей платежей. Удаление по 152-ФЗ — отдельная процедура анонимизации (см. §6: ПДн есть и в `raw_payload`/`metadata`, не только в `users`).

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

- `payment_method_id` — reusable `payment_method.id` ЮKassa. Храним только после payment с `metadata.auto_renew=true` и только если ЮKassa вернула `payment_method.saved=true`; при `disabled` очищаем в `NULL`, чтобы отключение автопродления удаляло токен на нашей стороне.
- `status='active'` означает, что worker может пытаться списать следующий период. `charging` — in-flight попытка после локального intent и до результата poller. `charge_failed` оставляет профиль для retry; `blocked` останавливает автосписания до ручной проверки; `disabled` означает пользовательское отключение.
- `next_charge_at` для `status='active'` равен effective expiry пользователя: `MAX(expires_at)` по активным subscriptions, чтобы `/balance` и дата следующего списания совпадали. Для `charge_failed` это время следующего retry, обычно `now()+24h`; `blocked` не переносится на новый retry. Worker создаёт новый payment, а не меняет текущую subscription.
- `last_payment_id` указывает на payment, по которому профиль был создан или обновлён.

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

-- Идемпотентность финализации: provider_refund_id от ЮKassa уникален, но NULL на стадии pending.
CREATE UNIQUE INDEX refunds_provider_idx ON refunds (provider_refund_id) WHERE provider_refund_id IS NOT NULL;
-- Один активный возврат на платёж — закрывает гонку двойного BeginRefund ДО обращения к ЮKassa.
CREATE UNIQUE INDEX refunds_active_idx ON refunds (payment_id) WHERE status IN ('pending','succeeded');
CREATE INDEX refunds_payment_idx ON refunds (payment_id);
```

- Возврат двухфазный (§5.9): `pending` создаётся до вызова ЮKassa, `FinalizeRefund` заполняет `provider_refund_id` и статус после ответа.
- `idempotence_key UNIQUE` — наш ключ для заголовка ЮKassa `Idempotence-Key`, переживает краш/ретрай.
- `provider_refund_id` nullable и уникален только когда не NULL; `refunds_active_idx` держит MVP-инвариант один активный/успешный refund на платёж.
- `status` включает `canceled` — ЮKassa может завершить возврат в этом статусе (`api/yookassa/API.md` §5).

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

- `event_type` — базовый тип события: `bot_start`, `consent_declined`, `subscription_viewed`, `payment_requested`, `nudge_sent`.
- `event_key` — уточнение события. Для `nudge_sent` хранит класс дожима (`payment_followup`, `sub_low_balance`, `sub_empty_balance`, legacy `trial_ending`/`sub_expiring`/`sub_expired`). Не уникален: повторные действия и новый платёжный цикл должны оставаться в истории.
- `metadata` — allowlist на уровне storage: для `nudge_sent` сохраняется только `{"class":"..."}`, для остальных #12-событий `{}`. Meeting URL, email, payment/provider ids сюда не пишем.
- `ON DELETE RESTRICT` — аналитическая история блокирует физический DELETE user так же, как платежи/джобы; публичное удаление ПДн остаётся задачей анонимизации, не cascade-delete.

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
    provider               TEXT        NOT NULL DEFAULT '',  -- поставщик-создатель (mock|speech2text|mymeet): Fetch/Cancel резолвят его по этому полю, а не по активному TRANSCRIPTION_PROVIDER — иначе после смены провайдера чужой external_job_id даёт HTTP 400 (заведена миграцией 00008, job-provider-слайс)
    status_message_id      BIGINT,      -- id Telegram-сообщения статуса; бот редактирует его по ходу обработки; NULL — сообщения нет (заведена миграцией 00006, lifecycle-controls)
    summary_md             TEXT,        -- кэш сгенерированного конспекта F3 ДО доставки (миграция 00007): сбой Deliver не повлечёт повторный платный Summarize на ретрае (orchestrator summarizeJob); NULL — не сгенерировано / summary-off
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

- `subscription_id` указывает, против какой подписки списано; CHECK гарантирует `is_trial=true ⇔ subscription_id IS NULL`.
- `raw_provider_response JSONB` — сырой ответ поставщика для post-mortem.
- `jobs_active_dedup_idx` partial — защита от дублей только среди живых Job'ов, легитимные повторы (regular meeting room) разрешены. **Заведён миграцией `00005`** (payments-слайс). `failed_summary` входит в `WHERE status NOT IN (...)` — финальный статус освобождает URL для повторной отправки.
- `jobs_worker_idx` partial — выборка для `FOR UPDATE SKIP LOCKED` без сканирования финальных Job'ов. `awaiting_reauth` исключён заранее: worker его не должен подхватывать в будущем Google-слайсе. **Уже создан** в миграции `00003` и пересоздан в `00004` с финальным `WHERE`.
- `jobs_user_history_idx` — потребитель появился в demo-prep слайсе: `Storage.ListUserJobs` (`WHERE user_id=$1 AND created_at > $2 ORDER BY created_at DESC`) питает панель «Мои звонки» (история звонков юзера за последнюю неделю + статус каждого + повторная доставка). **Индекс пока НЕ заведён в миграциях** — на демо-данных (десятки строк) запрос идёт seq scan'ом, это не блокер. Завести отдельной forward-миграцией при росте данных или если панель истории станет горячим путём.
- **Колонка `provider` заведена миграцией `00008`** (job-provider-слайс, `ALTER TABLE jobs ADD COLUMN provider TEXT NOT NULL DEFAULT ''`). Фиксирует поставщика-создателя для резолва Fetch/Cancel в панели «Мои звонки».
- **Колонки квот `subscription_id`/`is_trial` заведены миграцией `00005`** (payments-слайс, `ALTER TABLE jobs` + CHECK `jobs_quota_check`). `reauth_from` ещё нет — её добавит отдельный post-0.1 Google-слайс.
- **Minute-колонки `reserved_minutes`/`billed_minutes`/`duration_seconds` заведены миграцией `00009`**. `reserved_minutes` хранит резерв при создании Job, `billed_minutes` — фактически списанные минуты после terminal billing, `duration_seconds` — billing-длительность в секундах или `NULL` для legacy rows.
- `recording_started_at` / `recording_finished_at` заведены миграцией `00009`: observable окно записи по state machine. Для Speech2Text это основной источник billing duration, потому что `result/json chunks[].time.to` отражает ASR-таймкоды речи, а не длительность присутствия бота на встрече.
- `jobs_awaiting_reauth_idx`, `reauth_from` и `google_accounts` — post-0.1 Google target; applied migrations их ещё не заводят.
- Рабочие фазы release 0.1: `joining` = бот подключается, `recording` = бот на встрече, `processing` = бот вышел и идёт расшифровка, `summarizing` = транскрипт доставлен и готовится саммари. `transcribed`/`failed_join`/`awaiting_reauth` остаются dormant/target values в CHECK.

### 3.8. Backfill и rollback `00009`

Миграция `00009_add_minute_quotas.sql` добавила minute-поля рядом с legacy meeting-полями и выполнила backfill:

- `users.trial_minutes_used = trial_meetings_used * 60`.
- `users.trial_minutes_done = max(trial_meetings_done * 60, chargeable trial jobs * 60)`, где chargeable trial jobs = `jobs.is_trial=true AND status IN ('done','failed_summary')`.
- `subscriptions.minutes_limit = meetings_limit * 60`.
- `subscriptions.minutes_balance = meetings_balance * 60`.
- `jobs.reserved_minutes = 60` для `created|joining|recording|processing|transcribed|summarizing|awaiting_reauth|done|failed_summary`; `0` для `failed|failed_join`.
- `jobs.billed_minutes = 60` для `done|failed_summary`; `0` для остальных статусов.
- `jobs.duration_seconds = NULL` для всех legacy rows.

Причина включить dormant `transcribed|awaiting_reauth` в reserve backfill: эти статусы разрешены CHECK и не являются terminal failure. Если такая строка появится до runtime switch, она должна продолжиться с сохранённым резервом.

Причина считать `failed_summary` в user-level done backfill: по hourly-контракту сбой саммари после доставки транскрипта является оказанной услугой. Legacy `trial_meetings_done` инкрементировался только на `done`, поэтому агрегат по jobs защищает F9 и будущий баланс от занижения.

Историческое ограничение schema-only deploy: `00009` не предназначалась для отдельного deploy на live writes без последующего catch-up, потому что старый runtime писал только `meetings_*`. После включения minute runtime новые rows пишут minute-поля сразу.

Down migration `00009` структурная: удаляет minute-индексы и minute-колонки, legacy `meetings_*` остаются. Это безопасно только до включения minute runtime; semantic rollback после реального minute-billing lossy и требует отдельного owner-gate.

## 4. Go-типы для `domain.go`

`domain.go` — источник правды по Go-структурам; этот раздел фиксирует только соответствие БД и nullable-границы, чтобы не дублировать код.

| Go-тип | Таблица | Важные соответствия |
|---|---|---|
| `User` | `users` | `ConsentAt` и `Notified*At` nullable; trial counters non-null |
| `Job` | `jobs` | `SubscriptionID`, `ExternalJobID`, `StatusMessageID`, `SummaryMD` nullable; `Provider` non-null default `''` |
| `Subscription` | `subscriptions` | balance/limit non-null, `expires_at > paid_at` |
| `Payment` | `payments` | nullable charge/provider/receipt/raw fields; `Amount` в копейках `int64` |
| `Refund` | `refunds` | `ProviderRefundID`, `Reason`, `CompletedAt` nullable |

Статусы в БД шире текущего Go-кода: `transcribed`, `failed_join`, `awaiting_reauth` разрешены CHECK как target/dormant values. В release 0.1 рабочие статусы Go: `created`, `joining`, `recording`, `processing`, `summarizing`, `done`, `failed`, `failed_summary`. `GoogleAccount`, `JobAwaitingReauth` и `reauth_from` относятся к post-0.1 Google-слайсу.

## 5. Транзакционные паттерны (критические узлы)

### 5.1. Создание Job с резервом минут

Атомарный `UPDATE … WHERE used < limit` ловит гонку «два сообщения за 200мс» физикой БД.

```sql
-- Подписка (модель 1:1: у юзера может быть несколько активных строк; берём
-- ближайшую строку с полным 60-минутным резервом, если такая есть; частичный
-- остаток используем только когда полной строки нет).
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

-- Если подписка не списалась — триал
UPDATE users
SET trial_minutes_used = trial_minutes_used + LEAST(60, $2 - trial_minutes_used)
WHERE telegram_id = $1 AND trial_minutes_used < $2  -- $2 = trialLimitMinutes
RETURNING LEAST(60, $2 - (trial_minutes_used - LEAST(60, $2 - trial_minutes_used))) AS reserved_minutes;
```

Если первый UPDATE вернул строку — `INSERT INTO jobs (is_trial=false, subscription_id=$id, reserved_minutes=$reserved)`. Если нет, и второй UPDATE вернул строку — `INSERT INTO jobs (is_trial=true, subscription_id=NULL, reserved_minutes=$reserved)`. Если оба пусто — отказ. Всё в одной транзакции pgx. **`FOR UPDATE` на выборе подписки обязательно** — иначе два параллельных запроса выберут одну и ту же подписку.

### 5.2. Возврат резерва при раннем `failed`

**Guard по статусу обязателен** — иначе два параллельных тика дадут lost-update (баланс отрастёт).

```sql
BEGIN;

-- Смена статуса с защитой от двойного перехода
UPDATE jobs
SET status = 'failed', updated_at = now()
WHERE id = $1 AND status IN ('created','joining','recording','processing')
RETURNING is_trial, subscription_id, user_id;

-- Откат делаем ТОЛЬКО если RowsAffected = 1
-- Для триала (is_trial=true)
UPDATE users SET trial_minutes_used = trial_minutes_used - $reserved_minutes
WHERE telegram_id = $user_id;

-- ИЛИ для подписки (is_trial=false)
UPDATE subscriptions
SET minutes_balance = LEAST(minutes_limit, minutes_balance + $reserved_minutes)
WHERE id = $subscription_id;

COMMIT;
```

Если первый UPDATE вернул `RowsAffected = 0` — это второй тик, который пытается «откатить» уже закрытый Job. Не делаем ничего.

**Из каких статусов какой исход (полная спецификация возврата/списания):**
- ручная отмена `created|joining` → `failed` **с откатом резерва** через `CancelJob`; `recording` вручную не отменяется, а останавливается как stop-result path ниже.
- `created`/`joining`/`recording`/`processing` → `failed` **с откатом резерва**, если transcript не получен (provider не принял задачу, бот не дошёл, stop не дал результата или транскрипция не получилась). Guard выше перечисляет именно эти no-result статусы.
- `recording` manual/watchdog stop → `processing` с marker `jobs.last_error='manual_stop'|'watchdog_stop'`; если после stop provider отдаёт непустой transcript, путь становится billable `done`/`failed_summary`, а не refund.
- `summarizing` → `failed_summary` **без раннего refund**: transcript-only файл доставлен, но summary не сформировалась. Отдельный переход — guard `WHERE id=$1 AND status='summarizing'`; terminal billing финализирует `billed_minutes` по `duration_seconds`.
- `failed_summary` → `done` возможен только через `SetTerminalJobSummary` после пользовательского восстановления конспекта из «Мои звонки»: обновляется `summary_md`, очищается `last_error`, billing/квоты не пересчитываются.
- `awaiting_reauth` — не финальный, не фейлится по таймауту (ждёт `/reconnect`), резерв держится.

То есть полный возврат `reserved_minutes` делается ТОЛЬКО на переходах из ранних статусов до оказания услуги; поздний сбой саммари идет через terminal billing. Это критично для money-логики — не реализовать возврат единым guard'ом на все статусы.

> **Реализовано (слайс provider-start-timeout + stop-result):** `FailJobWithQuotaRefund` несёт guard `WHERE id=$1 AND status IN ('created','joining','recording','processing')` — активные фазы до billable terminal result. `created` добавлен для submit-fail/start-timeout: provider ещё не принял задачу, но квота уже зарезервирована в `CreateJob`, поэтому её нужно вернуть. `CancelJob` намеренно уже и работает только для ручной отмены `created|joining`. `failJob` зовётся из `submitJob` (permanent HTTP submit error, retry-limit, start-timeout) и `pollJob` (StatusFailed, joining-timeout, no-result stop). Для s2t stop-result `last_error` временно хранит marker `manual_stop|watchdog_stop`: `StatusFailed` после такого stop сначала пробует `Fetch`; непустой transcript ведёт в `done`/`failed_summary` и terminal billing, пустой transcript или fetch-fail — в `failed` с refund. Статус `summarizing` в guard **намеренно исключён**: транскрипт уже доставлен, встреча использована, сбой саммари идёт через `SetJobFailedSummary` без отката (§5.2, `architecture.md` §8.4).

### 5.3. Перевод в `done`

Резерв уже сделан, но terminal billing корректирует итог по фактической длительности.

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

`SetJobDone` допускает `joining|recording|processing|summarizing`, чтобы покрыть s2t-fallback и перескок фаз polling'ом. Полная версия terminal billing — §5.7.

### 5.4. Идемпотентность платежа (модель 1:1: платёж = строка подписки)

Direct payment links создают строку `payments.status='pending'` до оплаты. Финализация идёт только после `GET /v3/payments/{id}` со статусом `succeeded` и `paid=true`.

**Создание direct pending payment:**

```sql
INSERT INTO payments (
    user_id, status, amount, currency,
    payment_intent_key, invoice_payload, metadata, raw_payload
)
VALUES ($1, 'pending', $2, $3, $4, $5, $6, $7)
ON CONFLICT DO NOTHING; -- provider charge или intent key
```

Direct flow двухфазный: сначала `StartDirectPaymentIntent` под `users FOR UPDATE` создаёт локальный
intent без `provider_payment_charge_id`, чтобы второй параллельный callback не сделал второй внешний
`POST /v3/payments`. После успешного ответа ЮKassa `AttachDirectProviderPayment` дозаполняет
`provider_payment_charge_id = payment.id`, `raw_payload`, `metadata`, сумму и currency. Если внешний
POST упал до provider id, `FailDirectPaymentIntent` переводит локальный intent в `failed`.

Если конфликт уже есть, legacy `CreatePendingPayment` читает существующую строку и сверяет `user_id`, сумму, currency, provider id и intent key. Конфликт с другой семантикой — ошибка, а не молчаливое переиспользование.

**Финализация polling'ом:**

```sql
BEGIN;

SELECT id, user_id, subscription_id, amount, currency, metadata
FROM payments
WHERE provider_payment_charge_id = $1
FOR UPDATE;

-- если статус уже succeeded/refunded -> changed=false
-- если статус не pending или subscription_id уже есть -> ошибка инварианта

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

`PaymentPoller` до транзакции сверяет ответ ЮKassa с локальным intent: provider id, amount, currency и `metadata.user_id`. Для direct/recurring payment с `payment_intent_key` metadata user id обязательна; пустая metadata допустима только для legacy Telegram payment. Если payment создан с `metadata.auto_renew=true` и ЮKassa вернула `payment_method.saved=true`, финализация сохраняет `payments.payment_method_id` и upsert'ит `recurring_profiles` с `next_charge_at = MAX(active subscriptions.expires_at)`, обновляя `consent_at`. Для recurring payment без нового `saved=true` финализация обновляет существующий профиль: `status='active'`, новый `next_charge_at`, `last_payment_id`, `attempts_count=0`, `last_error=NULL`, но только если профиль не `disabled` и не `blocked`. Повторный polling того же `payment.id` возвращает `changed=false` и не создаёт вторую подписку.


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

Для каждой due-строки worker строит `payment_intent_key = recurring:<user_id>:<next_charge_at_unix>` по исходному `next_charge_at`. `StartRecurringPaymentIntent` в одной транзакции проверяет текущий профиль, переводит `active|charge_failed -> charging` и создаёт локальный `payments.status='pending'` intent без provider id. Если пользователь уже отключил профиль, профиль заблокирован или `next_charge_at` изменился, метод возвращает `created=false`, внешний `POST /payments` не выполняется.

После успешного внешнего POST `AttachRecurringProviderPayment` дозаполняет `provider_payment_charge_id`, а `PaymentPoller` финализирует payment как обычный direct payment. Успех recurring обновляет профиль в `active`, сбрасывает ошибки и ставит новый `next_charge_at`; если во время `charging` профиль был отключён пользователем (`disabled`) или уведён в `blocked`, успешный in-flight payment создаёт subscription, но профиль не реактивируется (`blocked` ждёт ручной проверки).

При локальной ошибке POST/attach до provider id `MarkRecurringRetry` возвращает профиль из `charging` в `charge_failed` без переноса `next_charge_at`; если процесс упал и профиль остался `charging`, due-select подберёт его только при наличии pending intent без provider id: следующий tick в пределах 23 часов повторит тот же `POST` с тем же `Idempotence-Key`. После 23 часов `StartRecurringPaymentIntent` переводит локальный intent в `failed`, профиль в `blocked`, пишет `recurring_intent_blocked` и больше не ретраит автоматически. При подтверждённом временном `canceled` recurring intent переводит payment в `failed` и профиль в `charge_failed`/`blocked` в одной транзакции; revoked/mandate причины уходят в `blocked`. `DisableRecurring` применим к любому статусу кроме уже `disabled`, очищает `payment_method_id` и возвращает `changed bool`.

Legacy `successful_payment` остаётся для старого Telegram Payments пути:

Successful payment идемпотентно пишет `payments` по `telegram_payment_charge_id` и создаёт новую строку `subscriptions` с независимым `expires_at = now()+30 days`. Это **два statement в одной транзакции**: финальный `UPDATE payments` внутри того же CTE не видит строку, вставленную `new_payment`, поэтому `subscription_id` проставляется отдельным statement.

`SELECT telegram_id FROM users WHERE telegram_id = $1 FOR UPDATE` идёт первым statement транзакции пользователя: каждая новая строка подписки независима (`expires_at = now()+30 days`, без chain-чтения `MAX(expires_at)` как базы нового периода), поэтому лок для этой конкретной гонки не обязателен, но оставлен как общая сериализация конкурентных успешных платежей одного юзера в одной транзакции (защита от будущих зависимостей от состояния `users`/подписок в этом же коде). Проверено тестом `TestRecordSuccessfulPayment_ConcurrentSameUser` (billing_test.go) — независимые периоды создаются корректно без гонок. Платежи разных юзеров друг друга не блокируют — лочится конкретная строка `users`, не таблица.

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

Ретрай Telegram с тем же charge id даёт пустой `new_payment`, значит `new_sub` не создаётся и statement 2 пропускается. Конкурентный ретрай того же charge id сериализуется UNIQUE-индексом. Два разных successful_payment одного юзера — две независимые легитимные строки подписки, каждая со своим `expires_at = now()+30 days`; `FOR UPDATE` на строке `users` (выше) остаётся общей защитой транзакции платежа, не завязан на общий срок между строками.

### 5.5. Legacy `pre_checkout_query` — только in-memory валидация

Этот раздел относится к старому Telegram Payments пути. В direct payment links Telegram `pre_checkout_query` не используется: подтверждение оплаты идёт через polling ЮKassa (§5.4).

Telegram даёт **10 секунд** на ответ (жёсткий лимит, после — `Bad Request: query is too old`). Минус сетевой roundtrip → целевое время ответа **≤500 мс**, превышение логируется как WARN, систематические превышения (>1% по rolling-окну) — алерт. Подробности legacy-пути — `docs/api/yookassa/API.md`.

Для legacy-пути: никаких походов в БД с JOIN. Валидация — формат `invoice_payload`, существование юзера в in-memory кеше (или один `SELECT` по PK). Проверки «нет активной подписки» здесь НЕТ: при модели 1:1 (§3.3) несколько активных периодов — норма, а повторная оплата создаёт независимый период (`expires_at = now()+30 days`, §5.4) легитимно; дедуп ретрая обеспечивает `UNIQUE(telegram_payment_charge_id)` в `successful_payment`, не pre_checkout. Бизнес-логика создания подписки — только в `successful_payment`.

### 5.6. F9-тики и payment nudges

```sql
SELECT u.telegram_id,
       COALESCE(MAX(p.paid_at), '-infinity'::timestamptz) AS last_paid_at
FROM users u
JOIN subscriptions s ON s.user_id = u.telegram_id AND s.expires_at > now() AND s.minutes_balance > 0
LEFT JOIN payments p ON p.user_id = u.telegram_id AND p.status = 'succeeded'
GROUP BY u.telegram_id, u.notified_sub_expiring_at
-- Модель 1:1: у юзера может быть несколько активных строк с независимыми
-- expires_at — берём MAX(expires_at), ровно один на юзера (GROUP BY снимает дубли).
HAVING MAX(s.expires_at) BETWEEN now() + INTERVAL '3 days' AND now() + INTERVAL '4 days'
   AND (u.notified_sub_expiring_at IS NULL
        OR u.notified_sub_expiring_at < COALESCE(MAX(p.paid_at), '-infinity'::timestamptz));
```

`COALESCE(..., '-infinity')` нужен, чтобы `MAX(paid_at)=NULL` не ломал сравнение. При модели 1:1 у юзера может быть несколько активных подписок, поэтому F9 агрегирует по `u.telegram_id` и пушит по `MAX(s.expires_at)`, ровно одну строку на юзера.

Новые дожимы #12 дедупятся через `analytics_events`:

- `payment_followup`: последний `subscription_viewed`/`payment_requested` старше 24 часов; нет активной подписки, нет `payments.status='succeeded'` после просмотра, нет `nudge_sent/payment_followup` после просмотра.
- `sub_low_balance`: активные подписки пользователя дают `SUM(minutes_balance) > 0 AND < 60`; нет `nudge_sent/sub_low_balance` после последнего successful payment. Имеет приоритет над `sub_expiring`, чтобы один тик не слал два подписочных пуша.
- `trial_ending`: отсеивает любую календарно активную подписку, даже если `minutes_balance = 0`; платящим пользователям нужен подписочный дожим, а не trial-дожим.
- `sub_empty_balance`: есть календарно активная подписка, но суммарный `minutes_balance = 0`; нет `nudge_sent/sub_empty_balance` после последнего successful payment. Календарно истёкшие строки обрабатывает legacy `sub_expired`.
- `sub_low_balance` и `sub_empty_balance` не выбирают пользователя, пока есть активная не-trial Job с `reserved_minutes > 0`: `CreateJob` резервирует до 60 минут в начале созвона, и до terminal billing баланс может выглядеть искусственно низким.

Инвариант для всех классов сохраняется: сначала отправить сообщение, потом поставить mark. Для новых классов mark = `analytics_events(event_type='nudge_sent', event_key=<class>)`; для старых F9-классов `users.notified_*_at` и `nudge_sent` пишутся одной транзакцией.

### 5.7. Terminal billing в `done` / `failed_summary`

Резерв уже сделан при создании Job (§5.1). На terminal `done` или `failed_summary` считаем фактическое списание по duration:

- приоритет источников duration: явное значение caller'а, сохранённое `jobs.duration_seconds`, observable окно `recording_started_at -> now()`, fallback 3600s;
- для Speech2Text caller передаёт `Result.DurationSeconds` только если Job ещё не дошёл до `processing` с сохранённым observable window; если окно сохранено, caller передаёт `0`, чтобы terminal billing взял `jobs.duration_seconds`;
- `duration_seconds <= 0` после всех источников превращается в fallback 3600s;
- `1..59` секунд = 1 минута;
- `>=60` секунд округляется вверх до полной минуты;
- если wanted меньше резерва, unused возвращается в исходный source;
- если wanted больше резерва, extra добирается только из исходного source и только при достаточном активном остатке;
- долг, negative balance и списание следующей подписки не создаются.

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

Если terminal UPDATE вернул `RowsAffected=0` (повторный поллинг провайдера) — второй тоже ничего не делает, всё атомарно в одной транзакции. `summary_md` пишется до доставки саммари, чтобы retry доставки не вызывал LLM повторно.

Позднее восстановление конспекта из «Мои звонки» не вызывает terminal billing повторно: `SetTerminalJobSummary` работает только с `done|failed_summary`, сохраняет `summary_md`, а для `failed_summary` переводит статус в `done` и очищает `last_error`.

### 5.8. Parallel calls, active cap и same-URL dedup при создании Job

`CreateJob` разрешает несколько активных разных `meeting_url`, если хватает квоты и `active < 4`. Денежный ограничитель — атомарный резерв §5.1; cap=4 защищает provider capacity. Проверка cap атомарна: в начале транзакции лочим `users` строку, затем проверяем same-URL duplicate и считаем активные Job. При `active >= 4` reserve ещё не выполнен.

Same-URL dedup остаётся физическим ограничением БД:

```sql
CREATE UNIQUE INDEX jobs_active_dedup_idx
ON jobs (user_id, meeting_url)
WHERE status NOT IN ('done','failed_join','failed','failed_summary');
```

Тот же URL возвращает `ErrDuplicateActiveJob` до reserve квоты; unique violation на `jobs_active_dedup_idx` мапится туда же и откатывает транзакцию. Разные URL ограничены квотой и cap=4; account-wide provider concurrency остаётся hardening-треком.

### 5.9. Полный refund — деактивация подписки (двухфазный)

На MVP частичные refund'ы запрещены — все возвраты полные, refunds:payment строго 1:1 (§3.5, ручной процесс support → ЮKassa API). Возврат **двухфазный**: детерминированный `Idempotence-Key` нужен ДО `POST /v3/refunds`, а `provider_refund_id` приходит только из ответа (`api/yookassa/API.md` §3/§11.3) — одностадийный INSERT с `provider_refund_id` в прямом REST-флоу физически невозможен.

**Фаза 1 — `BeginRefund` (до вызова ЮKassa):** создаём pending-возврат с нашим детерминированным `idempotence_key`. `refunds_active_idx` (§3.5) физически отсекает второй возврат по тому же платежу.

```sql
INSERT INTO refunds (payment_id, idempotence_key, amount, status)
SELECT id, $2, amount, 'pending'
FROM payments
WHERE provider_payment_charge_id = $1 AND status = 'succeeded'
ON CONFLICT DO NOTHING            -- гасится refunds_active_idx (активный pending/succeeded по платежу) ИЛИ idempotence_key UNIQUE (ретрай того же намерения тем же ключом, в т.ч. когда прошлый возврат уже failed/canceled и выпал из refunds_active_idx); обе ветки -> RowsAffected=0 -> берём существующий SELECT
RETURNING id, idempotence_key;
-- RowsAffected=0 → платёж не succeeded ИЛИ уже есть активный возврат: берём существующий отдельным SELECT.
```

Затем `billing.CreateRefund(provider_payment_charge_id, amount, idempotence_key)` → `POST /v3/refunds` (ретрай тем же ключом, `api/yookassa/API.md` §14) → `provider_refund_id` + статус.

**Фаза 2 — `FinalizeRefund` (после ответа ЮKassa), идемпотентна по `refunds.id`:**

```sql
BEGIN;

-- 1. Финализируем возврат (provider_refund_id от ЮKassa; idempotence_key уже стоит с фазы 1)
UPDATE refunds
SET status = $status, provider_refund_id = $provider_refund_id,
    raw_payload = $raw, completed_at = now()
WHERE id = $refund_id AND status = 'pending'
RETURNING payment_id;
-- RowsAffected=0 → возврат уже финализирован (повторный webhook/триггер) → выходим без COMMIT.

-- 2. Только при УСПЕШНОМ возврате (status='succeeded') помечаем платёж
UPDATE payments
SET status = 'refunded'
WHERE id = $payment_id AND status = 'succeeded'
RETURNING subscription_id, user_id;

-- 3. Деактивируем РОВНО ту строку подписки, что купил этот платёж (модель 1:1).
--    Одно поле: expires_at = now() (баланс не трогаем — он нерелевантен при истёкшем сроке,
--    §5.1 фильтрует по expires_at > now; refunded-строка корректно остаётся видимой для F9).
UPDATE subscriptions
SET expires_at = now()
WHERE id = $subscription_id AND expires_at > now();

-- 4. Отключаем автопродление пользователя после успешного возврата, чтобы refund не
--    оставил будущие merchant-initiated списания активными.
UPDATE recurring_profiles
SET status = 'disabled',
    payment_method_id = NULL,
    disabled_at = COALESCE(disabled_at, now()),
    updated_at = now()
WHERE user_id = $user_id AND status <> 'disabled';

COMMIT;
```

Гарантия идемпотентности двойная: `refunds_active_idx` (фаза 1) физически не даёт создать второй возврат по платежу; guard `WHERE id=$refund_id AND status='pending'` (фаза 2) отсекает повторную финализацию. `Idempotence-Key` (фаза 1) защищает от двойного списания на стороне ЮKassa при ретрае REST. При `failed`/`canceled` возврате (фаза 2 шаг 1 проставит статус) платёж и подписка НЕ трогаются — деньги не вернулись, доступ сохраняется.

**Модель 1:1 делает деактивацию детерминированной:** платёж купил ровно одну строку subscription (§5.4), refund деактивирует ровно её; другие оплаченные периоды юзера не затрагиваются (нет rollover-в-одну-строку — закрывает rank 1 ревью). Активные Job'ы юзера не отменяются (поставщик уже подключил бота). Новые Job'ы против этой строки не примутся (`expires_at` в прошлом); если других активных подписок и остатка триала нет → отказ через `billing.go`. **Триал не трогается рефандом** — остаток триала снова даёт доступ, это намеренно (юзер триал не оплачивал).

## 6. Открытые вопросы (для следующих сессий)

Решены ревизией 1.2 (2026-06-14, мультиагентное DB-ревью перед payments-слайсом):
- ~~rollover-vs-refund (продление в одну строку → refund обнуляет оба периода)~~ → **модель «строка на платёж» 1:1** (§3.3/§5.4): каждый платёж = своя строка subscription, refund деактивирует ровно её.
- ~~Гонка СОЗДАНИЯ первой подписки (FOR UPDATE не лочит несуществующую строку → две подписки с двух карт)~~ → снята моделью 1:1: общей мутируемой строки нет, два платежа = два легитимных периода (§5.4).
- ~~`payments.subscription_id` отдельным UPDATE → риск FK=NULL → refund не деактивирует~~ → линковка втянута в ту же транзакцию §5.4.

Решено в hourly-subscription слайсе (issue #7):
- ~~Гонка ПРОДЛЕНИЯ (chain): два конкурентных successful_payment ОДНОГО юзера читают один и тот же `MAX(expires_at)` и оба продлевают от одной базы → пользователь теряет оплаченный период~~ → снято вместе с самой chain-моделью: каждая строка теперь получает независимый `expires_at = now()+30 days`, общего состояния для гонки срока новой строки больше нет. `FOR UPDATE` на строке `users` (§5.4) сохранён как общая сериализация транзакции. Подтверждено тестом `TestRecordSuccessfulPayment_ConcurrentSameUser`.
- ~~Одностадийный refund несовместим с детерминированным `Idempotence-Key`~~ → двухфазный `BeginRefund`/`FinalizeRefund` (§3.5 `idempotence_key`, nullable `provider_refund_id` + partial-unique, §5.9).
- ~~`meetings_balance <= meetings_limit` CHECK ломался при продлении~~ → при модели 1:1 инвариант снова истинен (баланс ≤ лимита строки), CHECK сохранён.
- ~~Мёртвые `partially_refunded`/`amount_refunded` против антискоупа~~ → убраны (§3.4).
- ~~Идемпотентность приёма хрупка к NULL charge_id~~ → CHECK «succeeded ⇒ charge_id NOT NULL» (§3.4).

Решены ранее (1.1, 2026-06-04, ревью синьора):
- ~~F9 NULL в MAX(paid_at)~~ → §5.6 COALESCE с `-infinity`.
- ~~Parallel calls / same-URL dedup~~ → §5.8.
- ~~Триал-счётчик «успешно завершённых»~~ → `users.trial_meetings_done` + §5.7.
- ~~OAuth refresh fail из `summarizing`~~ → новый статус `awaiting_reauth`, не финальный.

Остаются открытыми:
- **Многоинстансовость — двойной Submit:** `ClaimJobsForWork` для `created`-джоб без backoff-фильтра + claim-bump коммитит до `SetJobSubmitted` (в другой tx после 30с Submit). На ОДНОМ последовательном воркере гонки нет; при 2-м инстансе B повторно склеймит ту же `created`-джобу → второй бот на встрече. **Обязательный пункт ПЕРЕД запуском 2-го инстанса** (не «позже»): клеймить `created` атомарно со сменой статуса (created→joining через RETURNING) либо backoff на created. Не убирать guard-по-статусу на денежных UPDATE даже на одном воркере (стоимость нулевая, отсутствие — мина при масштабировании). **`jobs_active_dedup_idx` этот случай НЕ закрывает:** он дедуплицирует РАЗНЫЕ строки одного юзера по `(user_id, meeting_url)`, а двойной Submit — повторный клейм ОДНОЙ И ТОЙ ЖЕ `created`-строки (тот же `job.id`), второй строки для нарушения UNIQUE нет. Фикс — атомарный клейм, не индекс (комментарий `storage.go` о dedup-индексе как multi-instance-защите вводит в заблуждение).
- **Контракт `storage.go`:** какие функции экспортируем, как `pgx.Tx` прокидывается между слоями. Решается при детализации модуля — должен быть зафиксирован ДО скаффолда, иначе `billing.go` не сможет делиться транзакцией.
- ~~**F9-тики вынести из `orchestrator.go` в `notifier.go`**~~ — **Сделано** (слайс payments, `notifier.go`, commit `c3e8779`).
- **152-ФЗ «право на забвение»** — `ON DELETE RESTRICT` на `payments`/`jobs` означает, что `DELETE FROM users` не сработает. Нужна анонимизация, сохраняя финансовую историю. Две тонкости из DB-ревью 2026-06-14: (1) `users.telegram_id` — натуральный PK и FK во всех таблицах; анонимизация `telegram_id := -id` = UPDATE PK, но `ON UPDATE CASCADE` в схеме нет → упадёт. Выбор ДО публичного запуска: либо `ON UPDATE CASCADE` на FK к users, либо суррогатный `BIGSERIAL` PK + `telegram_id UNIQUE` (каноничнее, но дорогая смена PK — а код уже построен на натуральном telegram_id, поэтому на закрытой бете оставляем натуральный). (2) ПДн (email/phone) лежат и в `payments.raw_payload`/`metadata`, `jobs.raw_provider_response`, `jobs.meeting_url` — анонимизация `telegram_id` их НЕ затронет, нужна чистка/маскировка JSONB. Задача продакшен-доводки — compliance-блокер публичного запуска, на закрытой бете отложено осознанно.
- **Аудит платежей / event-log** — история переходов payment/refund сейчас только в `raw_payload` (последнее состояние) + эфемерные `slog`. Для денежных споров/сверки за период append-only лог переходов был бы надёжнее. На MVP избыточно (30 юзеров); заложено `provider_paid_at` + `refunds.completed_at` как минимум. Полный `payment_events` — техдолг продакшена.
- **Orphan-Job recovery после рестарта** — Job в `joining`/`recording`/`processing` при крэше worker'а остаются «висящими». Процедура на старте `main.go`. Задача продакшен-доводки (Этап 3).
- **Архивация старых `jobs.raw_provider_response JSONB`** — на 30 юзерах × 15 часов × 50 KB условного provider raw за встречу объём пока мал. Через год — процедура удаления старше N дней. Задача после публичного запуска.
- **Текст согласия 152-ФЗ** для `consent_at` — требуется держать синхронным с сайтом и политикой обработки ПД. Базовый consent gate реализован.
- ~~**Точное `TRIAL_MEETINGS_LIMIT` и цена**~~ — superseded by #7: продуктовый канон теперь 3 часа trial и 1990 руб/мес за 15 часов; runtime переведён на minute accounting в PR #26.
