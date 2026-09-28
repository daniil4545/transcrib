# Analytics dashboard

Канон по Metabase dashboard для заказчика. SQL-runbook без BI живет в [`analytics.md`](analytics.md).

## 1. Контур

Текущий dashboard - demo v0, не production-grade reporting layer.

| Что | Значение |
|---|---|
| BI | Metabase |
| Runtime | VPS `203.0.113.10`, compose project `transcrib-analytics-dev` |
| URL | `https://analytics.example.com/dashboard/2`; fallback через SSH tunnel `localhost:3001` |
| Visible dashboard | `Аналитика transcrib PROD v0` |
| DEV dashboard | архивная/admin-only копия `Аналитика transcrib DEV archive v0` |
| Datasource | PROD: Coolify prod PostgreSQL; DEV archive: Coolify dev PostgreSQL |
| DB user | отдельные read-only роли: `metabase_dev_ro`, `metabase_prod_ro` |
| Product DB schema | не меняется: без миграций, views, materialized views |

С 2026-07-07 рабочий режим - один dashboard на существующем домене: видимая панель показывает
PROD-агрегаты, а DEV-копия остаётся только архивом/fallback для админа. Новый домен не заводим.
Настройка dashboard-слоя не является bot deploy: не запускать GitHub Actions, release tag, Coolify
deploy/restart prod app или миграции product DB.

## 2. Что выводим

Все карточки показывают агрегаты за последние 30 дней или текущий snapshot; исключение - `Встречи`,
это список отдельных встреч. Runtime-тексты в Metabase должны быть на русском: имя карточки,
описание и **заголовки колонок**. Заголовок колонки задается русским алиасом прямо в SQL
(`AS "Списано минут"`), а не настройкой визуализации - иначе он теряется при следующей заливке SQL.

| Карточка | Что показывает | Основные таблицы |
|---|---|---|
| `Главные показатели` | выручка, возвраты и чистая выручка, успешные оплаты, платящие пользователи, активные подписки, встречи с саммари / без саммари / сбои, списанные минуты | `payments`, `refunds`, `subscriptions`, `jobs` |
| `Воронка пользователей` | сколько пользователей дошло до этапов: первый контакт, согласие, первая ссылка, транскрипт, саммари, payment link, paid. Когорта - вся база, не 30 дней: при узком окне шаг `оплачено` показывал 0 рядом с непустыми оплатами в KPI | `users`, `jobs`, `payments` |
| `Текущий этап пользователя` | самый дальний достигнутый этап пользователя в воронке; когорта - вся база, как в воронке | `users`, `jobs`, `payments` |
| `Новые пользователи по дням` | новые пользователи и consent | `users` |
| `Платежные ссылки по дням` | созданные payment links и статусы оплат | `payments` |
| `Использование триала` | кто начал/использовал/исчерпал триал, зарезервированные и списанные минуты | `users` |
| `Состояние подписок` | активные подписки, истекающие, нулевой баланс, истекшие | `subscriptions` |
| `Встречи` | список встреч за 30 дней с точным временем: создание заявки, начало записи, provider, status, длительность, списанные минуты | `jobs` |
| `Длительность и списание минут` | avg/median/p90 duration, reserved/billed minutes по provider | `jobs` |
| `События бота` | агрегированные UX events: start, subscription viewed, payment requested, nudges | `analytics_events` |
| `Возвраты` | количество и сумма возвратов по status | `refunds` |
| `Операционный контроль` | активные и застрявшие jobs, старые pending payments | `jobs`, `payments` |

## 3. Источники данных

Business facts:
- `users` - первый контакт, consent, trial counters, F9 timestamps.
- `jobs` - meeting link, provider/status, terminal result, summary presence, duration, billing minutes.
- `payments` - payment links, status, фактическая оплата по `status='succeeded'` и `paid_at`.
- `subscriptions` - активная/историческая платная квота и остаток минут.
- `refunds` - возвраты.
- `analytics_events` - UX-события, которых нет в business tables.

Dashboard не должен выводить:
- `meeting_url`;
- email;
- provider payment id, external job id, payment intent key;
- raw payload/metadata;
- полный `last_error`.
- `summary_md` и другие body/raw поля.

`user_id` допустим только внутри SQL для join/count, включая `analytics_events.user_id` для
`COUNT(DISTINCT ...)`. В dashboard user-level строки не выводим.
Для признака "summary delivered" использовать `jobs.status = 'done'`, а не чтение `summary_md`.

## 4. Как менять dashboard

Правило: сначала меняется смысл карточки в документации, потом Metabase. Так шаблон можно
воспроизвести, если Metabase application DB потеряется.

Порядок изменения:
1. Сформулировать вопрос заказчика обычным языком: что он хочет понять по бизнесу.
2. Проверить, есть ли факт в текущих таблицах из раздела 3.
3. Если факт уже есть, добавить или изменить SQL в `docs/analytics-dashboard-cards.md`.
4. Проверить SQL на dev DB через read-only пользователя или через Metabase card query API, если
   меняется логика карточки.
5. Для PROD проверить тот же SQL через prod read-only datasource.
6. Обновить карточку в Metabase: название, описание, SQL, тип визуализации.
7. Проверить, что карточка не выводит sensitive поля из раздела 3.
8. Если факт не существует в БД, не придумывать proxy silently: добавить blind spot и отдельный
   follow-up issue на новое событие, cost telemetry или reporting view.

Для новых карточек default:
- текст на русском;
- короткое описание на русском: что означает карточка и как ее читать;
- период `last 30 days`, если не нужен snapshot;
- таблица для сложных группировок, график только когда он действительно читается;
- revenue считать только по `payments.status='succeeded'` и `paid_at`.

## 5. Как проверять

Минимальная проверка после изменения карточек:

```sh
ssh -N -L 3001:127.0.0.1:3001 root@203.0.113.10
```

Открыть:

```text
http://localhost:3001/dashboard/2
```

Runtime checks:
- рабочий PROD dashboard открывается на `https://analytics.example.com/dashboard/2`;
- DEV archive, если нужен, доступен только администратору;
- все карточки выполняются без ошибок;
- карточки читаемые на ноутбучном экране;
- `Встречи по статусам и провайдерам` и `События бота` не пустые, если в prod есть события;
- sensitive поля из раздела 3 не видны.

Infra checks:

```sh
deploy/safe-ssh.sh analytics-ps
curl -I https://analytics.example.com/dashboard/2
```

Диагностика "аналитика не обновляется" - три read-only проверки, отвечающие на разные вопросы:

```sh
deploy/safe-ssh.sh analytics-net            # Metabase и prod postgres в одной docker-сети?
deploy/safe-ssh.sh analytics-queries        # карточки выполняются или падают с ошибкой?
deploy/safe-ssh.sh coolify-prod-freshness   # есть ли вообще свежие строки в prod-таблицах?
```

Дополнительно: `analytics-logs` (логи Metabase), `analytics-cards` (карточки и их datasource),
`analytics-users` (аккаунты, last_login, настройки инстанса). Пустой dashboard у конкретного
человека чаще означает, что он не залогинен, а не отказ датасорса - сверять по `last_login`.

PROD datasource acceptance:
- Metabase role может выполнять только `SELECT` по нужным полям для карточек.
- `CREATE TABLE`, `INSERT`, `UPDATE`, `DELETE` и чтение `meeting_url`, email, provider/payment ids,
  raw payload/metadata, `last_error` и `summary_md` запрещены.
- Viewer-пользователь заказчика видит только готовую dashboard/collection, без SQL editor и raw table
  browsing.

Operator-only setup notes:
- Metabase временно подключён к prod Docker network `<prod-resource-uuid>`; durable-вариант -
  добавить эту external network в analytics compose и пересоздать только Metabase.
- Prod datasource host inside Docker: `postgres-<prod-resource-uuid>`, port `5432`, db
  `transcrib`, user `metabase_prod_ro`.
- Prod datasource в Metabase называется `Transcrib PROD Readonly`; `/dashboard/2` переключен на него,
  DEV-копия заархивирована как `Аналитика transcrib DEV archive v0`.
- `metabase_prod_ro` уже создан: разрешены только column-level SELECT для агрегатов, включая
  `analytics_events.user_id` только для distinct-count; DDL/DML и sensitive columns проверенно
  запрещены.
- Временные пароли Metabase admin и `metabase_prod_ro` надо заменить на сильные уникальные. Ротация:
  сменить пароль роли в prod Postgres (`ALTER ROLE metabase_prod_ro LOGIN PASSWORD '<NEW_PASSWORD>'`),
  затем в Metabase UI открыть Admin settings -> Databases -> `Transcrib PROD Readonly`, обновить
  password и сохранить. Пароль Metabase admin менять через Account settings -> Password. AI-агент не
  читает `.env`, Docker env, Metabase credentials, DB passwords или DSN.

## 6. Blind spots v0

Пока не считаем честно:
- прибыль: нет cost telemetry по Speech2Text/OpenRouter и нет согласованной модели fixed costs;
- клик по внешней ссылке ЮKassa: нет redirect/callback tracking;
- точный текст/контекст дожимов: в `analytics_events` хранится класс события, не полный текст;
- future card changes: DEV archive не равен PROD, поэтому новые карточки сначала проверять
  осознанно на dev datasource или SQL runbook, затем переносить в рабочий PROD dashboard.

После обратной связи заказчика следующий шаг - оформить список нужных метрик. Только после этого
решать, нужны ли новые события, reporting views/materialized views или новые карточки.
