# Product funnel analytics

Runbook for issue #39 and the runtime events of issue #12. Base analytics is computed from PostgreSQL:
business facts come from existing tables, and UX events that are not stored there are written to
`analytics_events`.

The Metabase dashboard for the customer is described separately in [`analytics-dashboard.md`](analytics-dashboard.md).
The SQL template for cards v0 lives in [`analytics-dashboard-cards.md`](analytics-dashboard-cards.md).

## 1. Contract

Data sources:
- `users` - first contact, consent, trial counters, payment email, F9 timestamps;
- `jobs` - link submission, provider bot join, terminal result, summary cache, provider quality;
- `payments` - created direct payment links and successful payments;
- `subscriptions` - active, expiring and historical paid quota;
- `refunds` - operational refund control;
- `analytics_events` - `/start`, consent decline, subscription view, payment click and sent nudges.

Standard reports do not output:
- `meeting_url`;
- email;
- provider payment id, external job id, payment intent key;
- raw payload/metadata;
- full `last_error`.

For a specific user investigation, use section 8. In aggregate queries `user_id` is enough, as an internal join/count key only.

## 2. How to ask Codex/an AI agent

Request template:

```text
Codex, connect to the dev/prod PostgreSQL of the transcrib project per the current runbook.
Do not read .env and do not print secrets. Run the query from docs/analytics.md:
<number and name of the query>.
Period: from <YYYY-MM-DD> to <YYYY-MM-DD>.
Return aggregates only, without meeting URL, email, payment ids and raw payload.
```

For `psql` it is convenient to pass the period bounds as variables:

```sh
psql "$DATABASE_URL" -v from='2026-07-01 00:00:00+00' -v to='2026-07-08 00:00:00+00'
```

All queries below treat the period as `[from, to)`.

## 3. Quick data health check

Why: before analytics, make sure the DB is in place and see the volumes being counted.

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

## 4. User funnel

### 4.1. Cumulative reached counts

Why: answer how many users reached each stage. Cohort: users first created in the period.

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

Interpretation:
- `first_contact_users` - all created users; exact `/start` events after #12 are in
  `analytics_events` as `bot_start`;
- `transcript_users` - terminal `done` or `failed_summary`;
- `summary_users` - `done` with a non-empty `summary_md`;
- `payment_link_users` - the payment intent is already created in the DB, not a click on the external YooKassa link.

### 4.2. Current stage

Why: see where the last reachable stage of each user currently lies. Stages are mutually exclusive.

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

## 5. Daily slices

### 5.1. New users and consent

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

### 5.2. First links and provider join

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

### 5.3. Transcript and summary

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

### 5.4. Payment links and successful payments

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

Why: look at the date of the actual payment separately, not the payment link creation date.

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

Why: see events that are absent from the business tables: `/start`, consent decline, payment view,
payment click and sent nudges.

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

## 6. Trial, paid and subscription health

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

`180` matches the current product limit of 3 hours. If the limit changes, replace the number with the current `TRIAL_HOURS_LIMIT * 60`.

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

## 7. Operational quality

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

These queries may be used only to support a specific user. They intentionally do not output `meeting_url`, email, provider payment ids and raw payload.

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

## 9. Blind spots

After issue #12 some UX events are written to `analytics_events`: `bot_start`, `consent_declined`,
`subscription_viewed`, `payment_requested`, `nudge_sent`.

Not counted for now without new data:
- click on the external YooKassa link: there is no redirect/callback tracking;
- separate post-call notify "balance ran out": we use the computed zero balance;
- exact content of a sent message: only the `nudge_sent` class is stored, not the text.

`analytics_events` does not duplicate `jobs`, `payments` and `subscriptions`: payments, subscriptions and meetings
remain the source of truth in their business tables.

## 10. CTE template without extra objects

If the same report is needed often, use this read-only CTE. We do not create a DB view, to avoid adding new objects even manually.

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
