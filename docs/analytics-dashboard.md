# Analytics dashboard

Canon for the Metabase dashboard for the customer. The SQL runbook without BI lives in [`analytics.md`](analytics.md).

## 1. Scope

The current dashboard is demo v0, not a production-grade reporting layer.

| What | Value |
|---|---|
| BI | Metabase |
| Runtime | VPS `203.0.113.10`, compose project `transcrib-analytics-dev` |
| URL | `https://analytics.example.com/dashboard/2`; fallback via SSH tunnel `localhost:3001` |
| Visible dashboard | `Аналитика transcrib PROD v0` (Russian name, shown to the customer) |
| DEV dashboard | archived/admin-only copy `Аналитика transcrib DEV archive v0` |
| Datasource | PROD: Coolify prod PostgreSQL; DEV archive: Coolify dev PostgreSQL |
| DB user | separate read-only roles: `metabase_dev_ro`, `metabase_prod_ro` |
| Product DB schema | unchanged: no migrations, views or materialized views |

Since 2026-07-07 the working mode is one dashboard on the existing domain: the visible panel shows
PROD aggregates, and the DEV copy stays only as an archive/fallback for the admin. We do not add a new domain.
Dashboard layer setup is not a bot deploy: do not run GitHub Actions, a release tag, a Coolify
deploy/restart of the prod app or product DB migrations.

## 2. What is shown

All cards show aggregates for the last 30 days or the current snapshot; the exception is `Встречи` (Meetings),
which is a list of individual meetings. Runtime texts in Metabase must be in Russian: card name,
description and **column headers**. A column header is set by a Russian alias directly in SQL
(`AS "Списано минут"`), not by visualization settings, otherwise it is lost on the next SQL upload.

| Card | What it shows | Main tables |
|---|---|---|
| `Главные показатели` (Key metrics) | revenue, refunds and net revenue, successful payments, paying users, active subscriptions, meetings with summary / without summary / failures, billed minutes | `payments`, `refunds`, `subscriptions`, `jobs` |
| `Воронка пользователей` (User funnel) | how many users reached each stage: first contact, consent, first link, transcript, summary, payment link, paid. The cohort is the whole base, not 30 days: with a narrow window the `paid` step showed 0 next to non-empty payments in the KPI | `users`, `jobs`, `payments` |
| `Текущий этап пользователя` (Current user stage) | the furthest funnel stage reached by the user; the cohort is the whole base, as in the funnel | `users`, `jobs`, `payments` |
| `Новые пользователи по дням` (New users per day) | new users and consent | `users` |
| `Платежные ссылки по дням` (Payment links per day) | created payment links and payment statuses | `payments` |
| `Использование триала` (Trial usage) | who started/used/exhausted the trial, reserved and billed minutes | `users` |
| `Состояние подписок` (Subscription health) | active, expiring, zero-balance and expired subscriptions | `subscriptions` |
| `Встречи` (Meetings) | list of meetings for 30 days with exact times: request creation, recording start, provider, status, duration, billed minutes | `jobs` |
| `Длительность и списание минут` (Duration and minute billing) | avg/median/p90 duration, reserved/billed minutes per provider | `jobs` |
| `События бота` (Bot events) | aggregated UX events: start, subscription viewed, payment requested, nudges | `analytics_events` |
| `Возвраты` (Refunds) | count and amount of refunds per status | `refunds` |
| `Операционный контроль` (Operational control) | active and stuck jobs, old pending payments | `jobs`, `payments` |

## 3. Data sources

Business facts:
- `users` - first contact, consent, trial counters, F9 timestamps.
- `jobs` - meeting link, provider/status, terminal result, summary presence, duration, billing minutes.
- `payments` - payment links, status, actual payment by `status='succeeded'` and `paid_at`.
- `subscriptions` - active/historical paid quota and remaining minutes.
- `refunds` - refunds.
- `analytics_events` - UX events that are absent from the business tables.

The dashboard must not output:
- `meeting_url`;
- email;
- provider payment id, external job id, payment intent key;
- raw payload/metadata;
- full `last_error`;
- `summary_md` and other body/raw fields.

`user_id` is allowed only inside SQL for join/count, including `analytics_events.user_id` for
`COUNT(DISTINCT ...)`. We do not output user-level rows in the dashboard.
For the "summary delivered" flag use `jobs.status = 'done'`, not a read of `summary_md`.

## 4. How to change the dashboard

Rule: first the meaning of the card changes in the documentation, then Metabase. This way the template can be
reproduced if the Metabase application DB is lost.

Change procedure:
1. Phrase the customer's question in plain language: what they want to understand about the business.
2. Check whether the fact exists in the current tables from section 3.
3. If the fact already exists, add or change the SQL in `docs/analytics-dashboard-cards.md`.
4. Check the SQL on the dev DB via the read-only user or via the Metabase card query API, if
   the card logic changes.
5. For PROD, check the same SQL through the prod read-only datasource.
6. Update the card in Metabase: name, description, SQL, visualization type.
7. Check that the card does not output sensitive fields from section 3.
8. If the fact does not exist in the DB, do not invent a proxy silently: add a blind spot and a separate
   follow-up issue for a new event, cost telemetry or a reporting view.

Defaults for new cards:
- text in Russian;
- a short description in Russian: what the card means and how to read it;
- period `last 30 days`, unless a snapshot is needed;
- a table for complex groupings, a chart only when it is actually readable;
- count revenue only by `payments.status='succeeded'` and `paid_at`.

## 5. How to verify

Minimal check after changing cards:

```sh
ssh -N -L 3001:127.0.0.1:3001 root@203.0.113.10
```

Open:

```text
http://localhost:3001/dashboard/2
```

Runtime checks:
- the working PROD dashboard opens at `https://analytics.example.com/dashboard/2`;
- the DEV archive, if needed, is available to the administrator only;
- all cards run without errors;
- cards are readable on a laptop screen;
- `Встречи по статусам и провайдерам` (Meetings by status and provider) and `События бота` (Bot events) are not empty if prod has events;
- sensitive fields from section 3 are not visible.

Infra checks:

```sh
deploy/safe-ssh.sh analytics-ps
curl -I https://analytics.example.com/dashboard/2
```

Diagnosing "analytics is not updating": three read-only checks that answer different questions:

```sh
deploy/safe-ssh.sh analytics-net            # Metabase and prod postgres in the same docker network?
deploy/safe-ssh.sh analytics-queries        # do cards run or fail with an error?
deploy/safe-ssh.sh coolify-prod-freshness   # are there any fresh rows in the prod tables at all?
```

Additionally: `analytics-logs` (Metabase logs), `analytics-cards` (cards and their datasource),
`analytics-users` (accounts, last_login, instance settings). An empty dashboard for a specific
person more often means they are not logged in, not a datasource failure: check by `last_login`.

PROD datasource acceptance:
- The Metabase role can only run `SELECT` on the fields needed for cards.
- `CREATE TABLE`, `INSERT`, `UPDATE`, `DELETE` and reading `meeting_url`, email, provider/payment ids,
  raw payload/metadata, `last_error` and `summary_md` are forbidden.
- The customer's viewer user sees only the ready dashboard/collection, without the SQL editor and raw table
  browsing.

Operator-only setup notes:
- Metabase is temporarily connected to the prod Docker network `<prod-resource-uuid>`; the durable option is to
  add this external network to the analytics compose and recreate only Metabase.
- Prod datasource host inside Docker: `postgres-<prod-resource-uuid>`, port `5432`, db
  `transcrib`, user `metabase_prod_ro`.
- The prod datasource in Metabase is called `Transcrib PROD Readonly`; `/dashboard/2` is switched to it,
  the DEV copy is archived as `Аналитика transcrib DEV archive v0`.
- `metabase_prod_ro` is already created: only column-level SELECT for aggregates is allowed, including
  `analytics_events.user_id` for distinct-count only; DDL/DML and sensitive columns are verified as
  forbidden.
- Temporary passwords of the Metabase admin and `metabase_prod_ro` must be replaced with strong unique ones. Rotation:
  change the role password in prod Postgres (`ALTER ROLE metabase_prod_ro LOGIN PASSWORD '<NEW_PASSWORD>'`),
  then in the Metabase UI open Admin settings -> Databases -> `Transcrib PROD Readonly`, update the
  password and save. Change the Metabase admin password via Account settings -> Password. The AI agent does not
  read `.env`, Docker env, Metabase credentials, DB passwords or DSN.

## 6. Blind spots v0

Not counted honestly for now:
- profit: there is no cost telemetry for Speech2Text/OpenRouter and no agreed model of fixed costs;
- click on the external YooKassa link: there is no redirect/callback tracking;
- exact text/context of nudges: `analytics_events` stores the event class, not the full text;
- future card changes: the DEV archive is not equal to PROD, so new cards should first be checked
  deliberately on the dev datasource or the SQL runbook, then moved to the working PROD dashboard.

After customer feedback the next step is to draft the list of needed metrics. Only after that
decide whether new events, reporting views/materialized views or new cards are needed.
