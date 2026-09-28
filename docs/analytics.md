# Продуктовая аналитика воронки

Runbook для issue #39 и runtime-событий issue #12. Базовая аналитика считается из PostgreSQL:
бизнес-факты берутся из существующих таблиц, а UX-события, которых в них нет, пишутся в
`analytics_events`.

Metabase dashboard для заказчика описан отдельно в [`analytics-dashboard.md`](analytics-dashboard.md).
SQL-шаблон cards v0 живёт в [`analytics-dashboard-cards.md`](analytics-dashboard-cards.md).

## 1. Контракт

Источник данных:
- `users` - первый контакт, consent, trial counters, payment email, F9 timestamps;
- `jobs` - отправка ссылки, подключение provider-бота, terminal result, summary cache, качество provider;
- `payments` - созданные direct payment links и successful payments;
- `subscriptions` - активная, истекающая и историческая платная квота;
- `refunds` - операционный контроль возвратов;
- `analytics_events` - `/start`, отказ от consent, просмотр подписки, клик оплаты и отправленные дожимы.

Стандартные отчеты не выводят:
- `meeting_url`;
- email;
- provider payment id, external job id, payment intent key;
- raw payload/metadata;
- полный `last_error`.

Если нужен разбор конкретного пользователя, использовать раздел 8. В агрегатных запросах достаточно `user_id` только как внутреннего ключа для join/count.

## 2. Как просить Codex/AI-агента

Шаблон запроса:

```text
Codex, подключись к dev/prod PostgreSQL проекта transcrib по актуальному runbook.
Не читай .env и не печатай секреты. Выполни запрос из docs/analytics.md:
<номер и название запроса>.
Период: с <YYYY-MM-DD> по <YYYY-MM-DD>.
Верни только агрегаты, без meeting URL, email, payment ids и raw payload.
```

Для `psql` удобно задавать границы периода переменными:

```sh
psql "$DATABASE_URL" -v from='2026-07-01 00:00:00+00' -v to='2026-07-08 00:00:00+00'
```

Все запросы ниже считают период как `[from, to)`.

## 3. Быстрый health-срез данных

Зачем: перед аналитикой понять, что БД на месте и какие объемы считаем.

```sql
SELECT 'users' AS table_name, COUNT(*) AS rows_count FROM users
UNION ALL
SELECT 'jobs', COUNT(*) FROM jobs
UNION ALL
SELECT 'payments', COUNT(*) FROM payments
UNION ALL
SELECT 'subscriptions', COUNT(*) FROM subscriptions
UNION ALL
SELECT 'refunds', COUNT(*) FROM refunds
UNION ALL
SELECT 'analytics_events', COUNT(*) FROM analytics_events;
```

## 4. Воронка пользователей

### 4.1. Cumulative reached counts

Зачем: ответить, сколько пользователей дошло до каждого этапа. Когорта - пользователи, впервые созданные в периоде.

```sql
WITH
params AS (
    SELECT :'from'::timestamptz AS date_from, :'to'::timestamptz AS date_to
),
cohort AS (
    SELECT u.telegram_id, u.created_at, u.consent_at, u.payment_email
    FROM users u, params p
    WHERE u.created_at >= p.date_from
      AND u.created_at < p.date_to
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
        MIN(j.updated_at) FILTER (
            WHERE j.status = 'done'
              AND j.summary_md IS NOT NULL
              AND j.summary_md <> ''
        ) AS summary_at,
        MIN(j.updated_at) FILTER (WHERE j.status = 'failed_summary') AS summary_failed_at
    FROM jobs j
    GROUP BY j.user_id
),
payment_results AS (
    SELECT
        p.user_id,
        MIN(p.created_at) AS payment_link_at,
        MIN(p.paid_at) FILTER (WHERE p.status = 'succeeded') AS paid_at
    FROM payments p
    WHERE p.status IN ('pending', 'succeeded', 'failed', 'refunded')
    GROUP BY p.user_id
),
funnel AS (
    SELECT
        c.telegram_id,
        c.created_at AS first_contact_at,
        c.consent_at,
        fj.first_job_at,
        fr.first_recording_at,
        jr.transcript_at,
        jr.summary_at,
        jr.summary_failed_at,
        c.payment_email IS NOT NULL AS has_payment_email,
        pr.payment_link_at,
        pr.paid_at
    FROM cohort c
    LEFT JOIN first_jobs fj ON fj.user_id = c.telegram_id
    LEFT JOIN first_recordings fr ON fr.user_id = c.telegram_id
    LEFT JOIN job_results jr ON jr.user_id = c.telegram_id
    LEFT JOIN payment_results pr ON pr.user_id = c.telegram_id
)
SELECT
    COUNT(*) AS first_contact_users,
    COUNT(*) FILTER (WHERE consent_at IS NOT NULL) AS consent_users,
    COUNT(*) FILTER (WHERE first_job_at IS NOT NULL) AS first_link_users,
    COUNT(*) FILTER (WHERE first_recording_at IS NOT NULL) AS provider_joined_users,
    COUNT(*) FILTER (WHERE transcript_at IS NOT NULL) AS transcript_users,
    COUNT(*) FILTER (WHERE summary_at IS NOT NULL) AS summary_users,
    COUNT(*) FILTER (WHERE summary_failed_at IS NOT NULL) AS summary_failed_users,
    COUNT(*) FILTER (WHERE has_payment_email) AS payment_email_users,
    COUNT(*) FILTER (WHERE payment_link_at IS NOT NULL) AS payment_link_users,
    COUNT(*) FILTER (WHERE paid_at IS NOT NULL) AS paid_users
FROM funnel;
```

Интерпретация:
- `first_contact_users` - все созданные пользователи; точные `/start` после #12 смотрим в
  `analytics_events` как `bot_start`;
- `transcript_users` - terminal `done` или `failed_summary`;
- `summary_users` - `done` с непустым `summary_md`;
- `payment_link_users` - payment intent уже создан в БД, не клик по внешней ЮKassa ссылке.

### 4.2. Current stage

Зачем: увидеть, где сейчас лежит последний достижимый этап пользователя. Этапы взаимоисключающие.

```sql
WITH
params AS (
    SELECT :'from'::timestamptz AS date_from, :'to'::timestamptz AS date_to
),
cohort AS (
    SELECT u.telegram_id, u.created_at, u.consent_at, u.payment_email
    FROM users u, params p
    WHERE u.created_at >= p.date_from
      AND u.created_at < p.date_to
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
        MIN(updated_at) FILTER (
            WHERE status = 'done'
              AND summary_md IS NOT NULL
              AND summary_md <> ''
        ) AS summary_at
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
        c.payment_email IS NOT NULL AS has_payment_email,
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
            WHEN paid_at IS NOT NULL THEN '10_paid'
            WHEN payment_link_at IS NOT NULL THEN '09_payment_link'
            WHEN has_payment_email THEN '08_payment_email'
            WHEN summary_at IS NOT NULL THEN '07_summary'
            WHEN transcript_at IS NOT NULL THEN '06_transcript'
            WHEN first_recording_at IS NOT NULL THEN '05_provider_joined'
            WHEN first_job_at IS NOT NULL THEN '04_first_link'
            WHEN consent_at IS NOT NULL THEN '03_consent'
            ELSE '02_first_contact'
        END AS current_stage
    FROM funnel
)
SELECT current_stage, COUNT(*) AS users_count
FROM stages
GROUP BY current_stage
ORDER BY current_stage;
```

## 5. Дневные срезы

### 5.1. Новые пользователи и consent

```sql
WITH params AS (
    SELECT :'from'::timestamptz AS date_from, :'to'::timestamptz AS date_to
)
SELECT
    date_trunc('day', u.created_at) AS day,
    COUNT(*) AS new_users,
    COUNT(*) FILTER (WHERE u.consent_at IS NOT NULL) AS consent_users,
    COUNT(*) FILTER (WHERE u.payment_email IS NOT NULL) AS payment_email_users
FROM users u, params p
WHERE u.created_at >= p.date_from
  AND u.created_at < p.date_to
GROUP BY day
ORDER BY day;
```

### 5.2. Первые ссылки и provider join

```sql
WITH
params AS (
    SELECT :'from'::timestamptz AS date_from, :'to'::timestamptz AS date_to
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
)
SELECT
    date_trunc('day', fj.first_job_at) AS day,
    COUNT(*) AS first_link_users,
    COUNT(*) FILTER (WHERE fr.first_recording_at IS NOT NULL) AS provider_joined_users
FROM first_jobs fj
LEFT JOIN first_recordings fr ON fr.user_id = fj.user_id
CROSS JOIN params p
WHERE fj.first_job_at >= p.date_from
  AND fj.first_job_at < p.date_to
GROUP BY day
ORDER BY day;
```

### 5.3. Transcript и summary

```sql
WITH params AS (
    SELECT :'from'::timestamptz AS date_from, :'to'::timestamptz AS date_to
)
SELECT
    date_trunc('day', j.updated_at) AS day,
    COUNT(*) FILTER (WHERE j.status IN ('done', 'failed_summary')) AS transcript_jobs,
    COUNT(*) FILTER (
        WHERE j.status = 'done'
          AND j.summary_md IS NOT NULL
          AND j.summary_md <> ''
    ) AS summary_jobs,
    COUNT(*) FILTER (WHERE j.status = 'failed_summary') AS summary_failed_jobs
FROM jobs j, params p
WHERE j.updated_at >= p.date_from
  AND j.updated_at < p.date_to
  AND j.status IN ('done', 'failed_summary')
GROUP BY day
ORDER BY day;
```

### 5.4. Payment links и successful payments

```sql
WITH params AS (
    SELECT :'from'::timestamptz AS date_from, :'to'::timestamptz AS date_to
)
SELECT
    date_trunc('day', p.created_at) AS day,
    COUNT(*) AS payment_links,
    COUNT(*) FILTER (WHERE p.status = 'succeeded') AS succeeded_payments,
    COUNT(*) FILTER (WHERE p.status = 'failed') AS failed_payments,
    COUNT(*) FILTER (WHERE p.status = 'refunded') AS refunded_payments,
    COALESCE(SUM(p.amount) FILTER (WHERE p.status = 'succeeded'), 0) AS succeeded_amount_kopecks
FROM payments p, params q
WHERE p.created_at >= q.date_from
  AND p.created_at < q.date_to
GROUP BY day
ORDER BY day;
```

### 5.5. Successful payments by paid_at

Зачем: отдельно смотреть дату фактической оплаты, а не дату создания payment link.

```sql
WITH params AS (
    SELECT :'from'::timestamptz AS date_from, :'to'::timestamptz AS date_to
)
SELECT
    date_trunc('day', p.paid_at) AS day,
    COUNT(*) AS succeeded_payments,
    COALESCE(SUM(p.amount), 0) AS succeeded_amount_kopecks
FROM payments p, params q
WHERE p.status = 'succeeded'
  AND p.paid_at >= q.date_from
  AND p.paid_at < q.date_to
GROUP BY day
ORDER BY day;
```

### 5.6. UX events and nudges

Зачем: видеть события, которых нет в бизнес-таблицах: `/start`, отказ от consent, открытие оплаты,
клик оплаты и отправленные дожимы.

```sql
WITH params AS (
    SELECT :'from'::timestamptz AS date_from, :'to'::timestamptz AS date_to
)
SELECT
    date_trunc('day', e.occurred_at) AS day,
    e.event_type,
    COALESCE(e.event_key, '') AS event_key,
    COUNT(*) AS events_count,
    COUNT(DISTINCT e.user_id) AS users_count
FROM analytics_events e, params p
WHERE e.occurred_at >= p.date_from
  AND e.occurred_at < p.date_to
GROUP BY day, e.event_type, e.event_key
ORDER BY day, e.event_type, e.event_key;
```

## 6. Trial, paid и subscription health

### 6.1. Trial usage

```sql
SELECT
    COUNT(*) AS users_total,
    COUNT(*) FILTER (WHERE trial_minutes_used > 0) AS trial_reserved_users,
    COUNT(*) FILTER (WHERE trial_minutes_done > 0) AS trial_done_users,
    COUNT(*) FILTER (WHERE trial_minutes_done >= 180) AS trial_depleted_users,
    SUM(trial_minutes_used) AS trial_minutes_reserved,
    SUM(trial_minutes_done) AS trial_minutes_done
FROM users;
```

`180` соответствует текущему продуктовому лимиту 3 часа. Если лимит поменяется, заменить число на актуальный `TRIAL_HOURS_LIMIT * 60`.

### 6.2. Trial to paid

```sql
WITH
trial_users AS (
    SELECT DISTINCT user_id
    FROM jobs
    WHERE is_trial = true
      AND status IN ('done', 'failed_summary')
),
paid_users AS (
    SELECT DISTINCT user_id
    FROM payments
    WHERE status = 'succeeded'
)
SELECT
    COUNT(*) AS trial_users,
    COUNT(*) FILTER (WHERE pu.user_id IS NOT NULL) AS trial_to_paid_users
FROM trial_users tu
LEFT JOIN paid_users pu ON pu.user_id = tu.user_id;
```

### 6.3. Subscription health

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
    ) AS active_ok_users,
    COUNT(*) FILTER (
        WHERE active_minutes_balance > 0
          AND active_expires_at < now() + interval '3 days'
    ) AS active_expiring_3d_users,
    COUNT(*) FILTER (
        WHERE COALESCE(active_minutes_balance, 0) = 0
          AND last_expires_at > now()
    ) AS zero_balance_not_expired_users,
    COUNT(*) FILTER (
        WHERE last_expires_at <= now()
    ) AS expired_users
FROM subscriptions_by_user;
```

### 6.4. F9 nudges

```sql
SELECT
    COUNT(*) FILTER (WHERE notified_trial_low_at IS NOT NULL) AS trial_low_notified_users,
    COUNT(*) FILTER (WHERE notified_sub_expiring_at IS NOT NULL) AS sub_expiring_notified_users,
    COUNT(*) FILTER (WHERE notified_sub_expired_at IS NOT NULL) AS sub_expired_notified_users
FROM users;
```

## 7. Операционное качество

### 7.1. Job status by provider

```sql
WITH params AS (
    SELECT :'from'::timestamptz AS date_from, :'to'::timestamptz AS date_to
)
SELECT
    date_trunc('day', j.created_at) AS day,
    j.provider,
    j.status,
    COUNT(*) AS jobs_count,
    COUNT(*) FILTER (WHERE j.recording_started_at IS NOT NULL) AS recording_started_jobs,
    COUNT(*) FILTER (WHERE j.status IN ('done', 'failed_summary')) AS transcript_jobs,
    COUNT(*) FILTER (WHERE j.status = 'failed') AS failed_jobs
FROM jobs j, params p
WHERE j.created_at >= p.date_from
  AND j.created_at < p.date_to
GROUP BY day, j.provider, j.status
ORDER BY day, j.provider, j.status;
```

### 7.2. Terminal duration and billing

```sql
WITH params AS (
    SELECT :'from'::timestamptz AS date_from, :'to'::timestamptz AS date_to
)
SELECT
    j.provider,
    COUNT(*) AS terminal_jobs,
    ROUND(AVG(j.duration_seconds)) AS avg_duration_seconds,
    percentile_cont(0.5) WITHIN GROUP (ORDER BY j.duration_seconds) AS p50_duration_seconds,
    percentile_cont(0.9) WITHIN GROUP (ORDER BY j.duration_seconds) AS p90_duration_seconds,
    SUM(j.reserved_minutes) AS reserved_minutes,
    SUM(j.billed_minutes) AS billed_minutes
FROM jobs j, params p
WHERE j.updated_at >= p.date_from
  AND j.updated_at < p.date_to
  AND j.status IN ('done', 'failed_summary')
GROUP BY j.provider
ORDER BY j.provider;
```

### 7.3. Refund status

```sql
WITH params AS (
    SELECT :'from'::timestamptz AS date_from, :'to'::timestamptz AS date_to
)
SELECT
    r.status,
    COUNT(*) AS refunds_count,
    COALESCE(SUM(r.amount), 0) AS amount_kopecks
FROM refunds r, params p
WHERE r.created_at >= p.date_from
  AND r.created_at < p.date_to
GROUP BY r.status
ORDER BY r.status;
```

## 8. Debug by user_id

Эти запросы можно использовать только для поддержки конкретного пользователя. Они намеренно не выводят `meeting_url`, email, provider payment ids и raw payload.

### 8.1. User timeline

```sql
SELECT
    u.telegram_id AS user_id,
    u.created_at,
    u.consent_at,
    u.trial_minutes_used,
    u.trial_minutes_done,
    u.notified_trial_low_at,
    u.notified_sub_expiring_at,
    u.notified_sub_expired_at,
    u.payment_email IS NOT NULL AS has_payment_email
FROM users u
WHERE u.telegram_id = :user_id;
```

### 8.2. User jobs

```sql
SELECT
    j.id,
    j.provider,
    j.status,
    j.created_at,
    j.updated_at,
    j.recording_started_at,
    j.recording_finished_at,
    j.duration_seconds,
    j.reserved_minutes,
    j.billed_minutes,
    j.is_trial,
    j.summary_md IS NOT NULL AND j.summary_md <> '' AS has_summary,
    j.last_error IS NOT NULL AS has_last_error
FROM jobs j
WHERE j.user_id = :user_id
ORDER BY j.created_at DESC;
```

### 8.3. User payments and subscriptions

```sql
SELECT
    p.id,
    p.status,
    p.amount,
    p.currency,
    p.created_at,
    p.paid_at,
    p.failed_at,
    p.subscription_id
FROM payments p
WHERE p.user_id = :user_id
ORDER BY p.created_at DESC;

SELECT
    s.id,
    s.paid_at,
    s.expires_at,
    s.minutes_limit,
    s.minutes_balance
FROM subscriptions s
WHERE s.user_id = :user_id
ORDER BY s.paid_at DESC;
```

## 9. Слепые зоны

После issue #12 часть UX-событий пишется в `analytics_events`: `bot_start`, `consent_declined`,
`subscription_viewed`, `payment_requested`, `nudge_sent`.

Без новых данных сейчас не считаются:
- клик по внешней ссылке ЮKassa: нет redirect/callback tracking;
- отдельный post-call notify "баланс закончился": используем расчетный нулевой баланс;
- точное содержание отправленного сообщения: хранится только класс `nudge_sent`, не текст.

`analytics_events` не дублирует `jobs`, `payments` и `subscriptions`: платежи, подписки и встречи
остаются источниками правды в своих бизнес-таблицах.

## 10. CTE-шаблон без дополнительных объектов

Если один и тот же отчет нужен часто, использовать этот read-only CTE. View в БД не создаем, чтобы не добавлять новые объекты даже вручную.

```sql
WITH
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
        MIN(updated_at) FILTER (
            WHERE status = 'done'
              AND summary_md IS NOT NULL
              AND summary_md <> ''
        ) AS summary_at,
        MIN(updated_at) FILTER (WHERE status = 'failed_summary') AS summary_failed_at
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
        u.telegram_id AS user_id,
        u.created_at AS first_contact_at,
        u.consent_at,
        fj.first_job_at,
        fr.first_recording_at,
        jr.transcript_at,
        jr.summary_at,
        jr.summary_failed_at,
        u.payment_email IS NOT NULL AS has_payment_email,
        pr.payment_link_at,
        pr.paid_at,
        CASE
            WHEN pr.paid_at IS NOT NULL THEN '10_paid'
            WHEN pr.payment_link_at IS NOT NULL THEN '09_payment_link'
            WHEN u.payment_email IS NOT NULL THEN '08_payment_email'
            WHEN jr.summary_at IS NOT NULL THEN '07_summary'
            WHEN jr.transcript_at IS NOT NULL THEN '06_transcript'
            WHEN fr.first_recording_at IS NOT NULL THEN '05_provider_joined'
            WHEN fj.first_job_at IS NOT NULL THEN '04_first_link'
            WHEN u.consent_at IS NOT NULL THEN '03_consent'
            ELSE '02_first_contact'
        END AS current_stage
    FROM users u
    LEFT JOIN first_jobs fj ON fj.user_id = u.telegram_id
    LEFT JOIN first_recordings fr ON fr.user_id = u.telegram_id
    LEFT JOIN job_results jr ON jr.user_id = u.telegram_id
    LEFT JOIN payment_results pr ON pr.user_id = u.telegram_id
)
SELECT current_stage, COUNT(*) AS users_count
FROM funnel
GROUP BY current_stage
ORDER BY current_stage;
```
