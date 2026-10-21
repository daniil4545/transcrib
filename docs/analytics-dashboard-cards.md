# Metabase dashboard v0 cards

Issue: [#44](https://github.com/daniil4545/transcrib/issues/44)
Source: `docs/analytics.md`
Schema changes: none
Default period: last 30 days

These SQL questions are for the first Metabase dashboard. Runtime Metabase card names, descriptions
and visible columns are in Russian. Cards return aggregates only and must not expose `meeting_url`,
email, provider/payment ids, raw payload/metadata or full `last_error`.

PROD datasource uses a column-limited read-only role. Cards must not require sensitive columns such
as `summary_md`; `jobs.status = 'done'` is the aggregate summary-delivered marker.

Day buckets: always cast to `::date` after `date_trunc`. Without the cast the column stays
`timestamptz` and Metabase renders every row as `12:00 AM`. Day boundaries follow the Metabase
`report-timezone` setting (`Europe/Moscow`), so the cast must not be replaced by client-side
formatting.

Runtime dashboard card names:
- `Главные показатели` (Key metrics)
- `Воронка пользователей` (User funnel)
- `Текущий этап пользователя` (Current user stage)
- `Новые пользователи по дням` (New users per day)
- `Платежные ссылки по дням` (Payment links per day)
- `Использование триала` (Trial usage)
- `Состояние подписок` (Subscription health)
- `Встречи` (Meetings)
- `Длительность и списание минут` (Duration and minute billing)
- `События бота` (Bot events)
- `Возвраты` (Refunds)
- `Операционный контроль` (Operational control)

## Executive KPI

Refunds are counted by `created_at`, not `completed_at`: `metabase_prod_ro` has column-level grants
on `refunds` for `status`, `amount` and `created_at` only, and `completed_at` is not readable.
A refund of a payment made before the window still lowers net revenue of the current window - that
is intended, it is the cash that left in this period.

```sql
WITH
params AS (
    SELECT now() - interval '30 days' AS date_from, now() AS date_to
),
paid AS (
    SELECT
        COALESCE(SUM(amount), 0) AS revenue_kopecks,
        COUNT(*) AS payments_succeeded,
        COUNT(DISTINCT user_id) AS paid_users
    FROM payments p, params q
    WHERE p.status = 'succeeded'
      AND p.paid_at >= q.date_from
      AND p.paid_at < q.date_to
),
refunded AS (
    SELECT
        COALESCE(SUM(amount), 0) AS refunded_kopecks,
        COUNT(*) AS refunds_succeeded
    FROM refunds r, params q
    WHERE r.status = 'succeeded'
      AND r.created_at >= q.date_from
      AND r.created_at < q.date_to
),
jobs_done AS (
    SELECT
        COUNT(*) FILTER (WHERE j.status = 'done') AS meetings_done,
        COUNT(*) FILTER (WHERE j.status = 'failed_summary') AS meetings_no_summary,
        COUNT(*) FILTER (WHERE j.status = 'failed') AS meetings_failed,
        COALESCE(SUM(j.billed_minutes), 0) AS minutes_billed
    FROM jobs j, params q
    WHERE j.status IN ('done', 'failed_summary', 'failed')
      AND j.updated_at >= q.date_from
      AND j.updated_at < q.date_to
),
active_subs AS (
    SELECT
        COUNT(*) AS active_subscriptions,
        COUNT(DISTINCT user_id) AS active_paid_users
    FROM subscriptions
    WHERE expires_at > now()
      AND minutes_balance > 0
)
SELECT
    p.revenue_kopecks / 100.0 AS "Выручка, ₽",
    r.refunded_kopecks / 100.0 AS "Возвращено, ₽",
    (p.revenue_kopecks - r.refunded_kopecks) / 100.0 AS "Чистая выручка, ₽",
    p.payments_succeeded AS "Успешных оплат",
    r.refunds_succeeded AS "Возвратов",
    p.paid_users AS "Платящих пользователей",
    a.active_paid_users AS "С активной подпиской",
    a.active_subscriptions AS "Активных подписок",
    j.meetings_done AS "Встреч с саммари",
    j.meetings_no_summary AS "Без саммари",
    j.meetings_failed AS "Сбоев",
    j.minutes_billed AS "Списано минут"
FROM paid p
CROSS JOIN refunded r
CROSS JOIN jobs_done j
CROSS JOIN active_subs a;
```

## Funnel

```sql
WITH
cohort AS (
    SELECT u.telegram_id, u.created_at, u.consent_at
    FROM users u
),
first_jobs AS (
    SELECT j.user_id, MIN(j.created_at) AS first_job_at
    FROM jobs j
    GROUP BY j.user_id
),
first_recordings AS (
    SELECT j.user_id, MIN(j.recording_started_at) AS first_recording_at
    FROM jobs j
    WHERE j.recording_started_at IS NOT NULL
    GROUP BY j.user_id
),
job_results AS (
    SELECT
        j.user_id,
        MIN(j.updated_at) FILTER (WHERE j.status IN ('done', 'failed_summary')) AS transcript_at,
        MIN(j.updated_at) FILTER (WHERE j.status = 'done') AS summary_at
    FROM jobs j
    GROUP BY j.user_id
),
payment_results AS (
    SELECT
        p.user_id,
        MIN(p.created_at) AS payment_link_at,
        MIN(p.paid_at) FILTER (WHERE p.status = 'succeeded') AS paid_at
    FROM payments p
    GROUP BY p.user_id
),
funnel AS (
    SELECT
        c.telegram_id,
        c.consent_at,
        fj.first_job_at,
        fr.first_recording_at,
        jr.transcript_at,
        jr.summary_at,
        pr.payment_link_at,
        pr.paid_at
    FROM cohort c
    LEFT JOIN first_jobs fj ON fj.user_id = c.telegram_id
    LEFT JOIN first_recordings fr ON fr.user_id = c.telegram_id
    LEFT JOIN job_results jr ON jr.user_id = c.telegram_id
    LEFT JOIN payment_results pr ON pr.user_id = c.telegram_id
)
SELECT step AS "Этап", users_count AS "Пользователей"
FROM (
    SELECT 1 AS step_order, '01 первый контакт' AS step, COUNT(*) AS users_count FROM funnel
    UNION ALL SELECT 2, '02 согласие', COUNT(*) FILTER (WHERE consent_at IS NOT NULL) FROM funnel
    UNION ALL SELECT 3, '03 первая ссылка на встречу', COUNT(*) FILTER (WHERE first_job_at IS NOT NULL) FROM funnel
    UNION ALL SELECT 4, '04 бот подключился', COUNT(*) FILTER (WHERE first_recording_at IS NOT NULL) FROM funnel
    UNION ALL SELECT 5, '05 транскрипт доставлен', COUNT(*) FILTER (WHERE transcript_at IS NOT NULL) FROM funnel
    UNION ALL SELECT 6, '06 саммари доставлено', COUNT(*) FILTER (WHERE summary_at IS NOT NULL) FROM funnel
    UNION ALL SELECT 7, '07 ссылка на оплату создана', COUNT(*) FILTER (WHERE payment_link_at IS NOT NULL) FROM funnel
    UNION ALL SELECT 8, '08 оплачено', COUNT(*) FILTER (WHERE paid_at IS NOT NULL) FROM funnel
) s
ORDER BY step_order;
```

## Current Stage

```sql
WITH
cohort AS (
    SELECT u.telegram_id, u.consent_at
    FROM users u
),
first_jobs AS (
    SELECT user_id, MIN(created_at) AS first_job_at
    FROM jobs
    GROUP BY user_id
),
first_recordings AS (
    SELECT user_id, MIN(recording_started_at) AS first_recording_at
    FROM jobs
    WHERE recording_started_at IS NOT NULL
    GROUP BY user_id
),
job_results AS (
    SELECT
        user_id,
        MIN(updated_at) FILTER (WHERE status IN ('done', 'failed_summary')) AS transcript_at,
        MIN(updated_at) FILTER (WHERE status = 'done') AS summary_at
    FROM jobs
    GROUP BY user_id
),
payment_results AS (
    SELECT
        user_id,
        MIN(created_at) AS payment_link_at,
        MIN(paid_at) FILTER (WHERE status = 'succeeded') AS paid_at
    FROM payments
    GROUP BY user_id
),
funnel AS (
    SELECT
        c.telegram_id,
        c.consent_at,
        fj.first_job_at,
        fr.first_recording_at,
        jr.transcript_at,
        jr.summary_at,
        pr.payment_link_at,
        pr.paid_at
    FROM cohort c
    LEFT JOIN first_jobs fj ON fj.user_id = c.telegram_id
    LEFT JOIN first_recordings fr ON fr.user_id = c.telegram_id
    LEFT JOIN job_results jr ON jr.user_id = c.telegram_id
    LEFT JOIN payment_results pr ON pr.user_id = c.telegram_id
),
stages AS (
    SELECT
        CASE
            WHEN paid_at IS NOT NULL THEN '08 оплачено'
            WHEN payment_link_at IS NOT NULL THEN '07 ссылка на оплату'
            WHEN summary_at IS NOT NULL THEN '05 саммари'
            WHEN transcript_at IS NOT NULL THEN '04 транскрипт'
            WHEN first_recording_at IS NOT NULL THEN '03 бот подключился'
            WHEN first_job_at IS NOT NULL THEN '02 первая ссылка'
            WHEN consent_at IS NOT NULL THEN '01 согласие'
            ELSE '00 первый контакт'
        END AS current_stage
    FROM funnel
)
SELECT current_stage AS "Этап", COUNT(*) AS "Пользователей"
FROM stages
GROUP BY current_stage
ORDER BY current_stage;
```

## Daily Users

```sql
WITH params AS (
    SELECT now() - interval '30 days' AS date_from, now() AS date_to
)
SELECT
    date_trunc('day', u.created_at)::date AS "День",
    COUNT(*) AS "Новых пользователей",
    COUNT(*) FILTER (WHERE u.consent_at IS NOT NULL) AS "Дали согласие"
FROM users u, params p
WHERE u.created_at >= p.date_from
  AND u.created_at < p.date_to
GROUP BY "День"
ORDER BY "День";
```

## Payment Links

```sql
WITH params AS (
    SELECT now() - interval '30 days' AS date_from, now() AS date_to
)
SELECT
    date_trunc('day', p.created_at)::date AS "День",
    COUNT(*) AS "Создано ссылок",
    COUNT(*) FILTER (WHERE p.status = 'succeeded') AS "Оплачено",
    COUNT(*) FILTER (WHERE p.status = 'failed') AS "Не оплачено",
    COUNT(*) FILTER (WHERE p.status = 'refunded') AS "Возвращено",
    COALESCE(SUM(p.amount) FILTER (WHERE p.status = 'succeeded'), 0) / 100.0 AS "Оплачено по ссылкам этого дня, ₽"
FROM payments p, params q
WHERE p.created_at >= q.date_from
  AND p.created_at < q.date_to
GROUP BY "День"
ORDER BY "День";
```

## Trial Usage

`180` is the trial limit in minutes and duplicates `TRIAL_HOURS_LIMIT` (`config.go`, default 3 hours).
The limit lives in bot env, not in the DB, so it cannot be read from SQL: when `TRIAL_HOURS_LIMIT`
changes, `trial_depleted_users` must be updated here and in the Metabase card.

```sql
SELECT
    COUNT(*) AS "Всего пользователей",
    COUNT(*) FILTER (WHERE trial_minutes_used > 0) AS "Начали триал",
    COUNT(*) FILTER (WHERE trial_minutes_done > 0) AS "Использовали триал",
    COUNT(*) FILTER (WHERE trial_minutes_done >= 180) AS "Исчерпали триал",
    COALESCE(SUM(trial_minutes_used), 0) AS "Зарезервировано минут",
    COALESCE(SUM(trial_minutes_done), 0) AS "Списано минут"
FROM users;
```

## Subscription Health

```sql
WITH subscriptions_by_user AS (
    SELECT
        user_id,
        SUM(minutes_balance) FILTER (WHERE expires_at > now()) AS active_minutes_balance,
        MAX(expires_at) AS last_expires_at,
        MAX(expires_at) FILTER (
            WHERE expires_at > now()
              AND minutes_balance > 0
        ) AS active_expires_at
    FROM subscriptions
    GROUP BY user_id
)
SELECT
    COUNT(*) FILTER (
        WHERE active_minutes_balance > 0
          AND active_expires_at >= now() + interval '3 days'
    ) AS "Активные",
    COUNT(*) FILTER (
        WHERE active_minutes_balance > 0
          AND active_expires_at < now() + interval '3 days'
    ) AS "Истекают за 3 дня",
    COUNT(*) FILTER (
        WHERE COALESCE(active_minutes_balance, 0) = 0
          AND last_expires_at > now()
    ) AS "Нулевой баланс",
    COUNT(*) FILTER (
        WHERE last_expires_at <= now()
    ) AS "Истекшие"
FROM subscriptions_by_user;
```

## Meetings

Per-meeting list, not an aggregate: the owner needs the exact time each meeting ran. Status and
provider stay raw runtime values so they match logs and `docs/db.md`. No `meeting_url`, no ids.

```sql
WITH params AS (
    SELECT now() - interval '30 days' AS date_from, now() AS date_to
)
SELECT
    j.created_at AS "Заявка создана",
    j.recording_started_at AS "Начало записи",
    j.provider AS "Провайдер",
    j.status AS "Статус",
    ROUND(j.duration_seconds / 60.0, 1) AS "Длительность, мин",
    j.billed_minutes AS "Списано минут"
FROM jobs j, params p
WHERE j.created_at >= p.date_from
  AND j.created_at < p.date_to
ORDER BY j.created_at DESC
LIMIT 200;
```

## Terminal Duration And Billing

```sql
WITH params AS (
    SELECT now() - interval '30 days' AS date_from, now() AS date_to
)
SELECT
    j.provider AS "Провайдер",
    COUNT(*) AS "Завершенных встреч",
    ROUND(AVG(j.duration_seconds) / 60.0, 1) AS "Средняя, мин",
    ROUND((percentile_cont(0.5) WITHIN GROUP (ORDER BY j.duration_seconds))::numeric / 60.0, 1) AS "Медиана, мин",
    ROUND((percentile_cont(0.9) WITHIN GROUP (ORDER BY j.duration_seconds))::numeric / 60.0, 1) AS "p90, мин",
    COALESCE(SUM(j.reserved_minutes), 0) AS "Зарезервировано минут",
    COALESCE(SUM(j.billed_minutes), 0) AS "Списано минут"
FROM jobs j, params p
WHERE j.updated_at >= p.date_from
  AND j.updated_at < p.date_to
  AND j.status IN ('done', 'failed_summary')
GROUP BY j.provider
ORDER BY j.provider;
```

## UX Events

```sql
WITH params AS (
    SELECT now() - interval '30 days' AS date_from, now() AS date_to
)
SELECT
    date_trunc('day', e.occurred_at)::date AS "День",
    e.event_type AS "Тип события",
    COALESCE(e.event_key, '') AS "Ключ",
    COUNT(e.occurred_at) AS "Событий",
    COUNT(DISTINCT e.user_id) AS "Пользователей"
FROM analytics_events e, params p
WHERE e.occurred_at >= p.date_from
  AND e.occurred_at < p.date_to
GROUP BY "День", e.event_type, e.event_key
ORDER BY "День", e.event_type, e.event_key;
```

## Refunds

```sql
WITH params AS (
    SELECT now() - interval '30 days' AS date_from, now() AS date_to
),
statuses AS (
    SELECT *
    FROM (VALUES
        (1, 'pending'),
        (2, 'succeeded'),
        (3, 'failed'),
        (4, 'canceled')
    ) AS s(sort_order, status)
),
refunds_30d AS (
    SELECT r.status, r.amount
    FROM refunds r, params p
    WHERE r.created_at >= p.date_from
      AND r.created_at < p.date_to
)
SELECT
    s.status AS "Статус",
    COUNT(r.status) AS "Возвратов",
    (COALESCE(SUM(r.amount), 0) / 100.0)::numeric(12, 2) AS "Сумма, ₽"
FROM statuses s
LEFT JOIN refunds_30d r ON r.status = s.status
GROUP BY s.sort_order, s.status
ORDER BY s.sort_order;
```

## Operations

```sql
SELECT
    COUNT(*) FILTER (
        WHERE status IN ('created', 'joining', 'recording', 'processing', 'transcribed', 'summarizing', 'awaiting_reauth')
    ) AS "Активных встреч",
    COUNT(*) FILTER (
        WHERE status IN ('created', 'joining', 'recording', 'processing', 'transcribed', 'summarizing', 'awaiting_reauth')
          AND updated_at < now() - interval '30 minutes'
    ) AS "Активных дольше 30 мин",
    COUNT(*) FILTER (
        WHERE status IN ('created', 'joining', 'recording', 'processing', 'transcribed', 'summarizing', 'awaiting_reauth')
          AND updated_at < now() - interval '6 hours'
    ) AS "Активных дольше 6 ч",
    (
        SELECT COUNT(*)
        FROM payments
        WHERE status = 'pending'
          AND created_at < now() - interval '2 hours'
    ) AS "Ожидающих оплат дольше 2 ч"
FROM jobs;
```
