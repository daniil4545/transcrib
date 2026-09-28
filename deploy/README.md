# Деплой и инфраструктура transcrib

Главный runbook по инфраструктуре. Если нужно понять, что где развернуто, как катить prod/test,
как проверить состояние или где искать узкую операцию, начинать отсюда.

## Быстрая карта

| Что | Где |
| --- | --- |
| Сервер | `203.0.113.10`, общий VPS команды |
| Prod | Coolify service `transcrib-prod`, resource UUID `<prod-resource-uuid>`, image `ghcr.io/daniil4545/transcrib:sha-<git-sha>` |
| Dev bot | Coolify service `transcrib-dev`, resource UUID `<dev-resource-uuid>`, image `ghcr.io/daniil4545/transcrib:sha-<git-sha>` |
| Prod env | Coolify-managed environment variables |
| Dev env | Coolify-managed environment variables |
| Site | `https://transcrib.example.com`, host nginx, docroot `/var/www/transcrib.example.com` |
| Analytics | `https://analytics.example.com/dashboard/2`, Metabase on `127.0.0.1:3001`; visible dashboard is PROD, DEV kept as archive/admin fallback |
| Coolify CD | `https://coolify.example.com`, host nginx -> `127.0.0.1:8000` + loopback websocket `6001/6002`; Coolify `APP_URL` должен совпадать |
| Coolify MCP | `https://coolify.example.com/mcp`, read-only AI view через API token |
| Logs | [`logs.md`](logs.md); Dozzle через SSH tunnel, только live-viewer |
| Backup | prod `pg_dump`, временно on-server `/var/backups/transcrib/postgres` |

Секреты не хранятся в git. AI-агент не читает `.env`, `docker inspect env`,
`docker exec printenv`, приватные ключи и дампы.

## Что где искать

| Задача | Документ |
| --- | --- |
| Катить prod/test | этот файл |
| Проверить или восстановить backup БД | [`backup.md`](backup.md) |
| Читать и проверять логи | [`logs.md`](logs.md) |
| Обслужить Dozzle | [`dozzle/README.md`](dozzle/README.md) |
| Обслужить Metabase dashboard | [`../docs/analytics-dashboard.md`](../docs/analytics-dashboard.md) |
| Настроить GitHub Actions + Coolify CD | [`coolify-cd.md`](coolify-cd.md) |
| Понять devops-решения и приоритеты hardening | [`../docs/devops-practices.md`](../docs/devops-practices.md) |
| История серверных решений и железо | [`../docs/deployment.md`](../docs/deployment.md) |

## Analytics dashboard

Metabase живёт отдельно от bot deploy и не меняет product schema/runtime:

- compose project `transcrib-analytics-dev`;
- host bind `127.0.0.1:3001`, внешний доступ только через nginx `https://analytics.example.com`;
- visible dashboard is PROD aggregates on the existing `analytics.example.com` domain;
- DEV dashboard is kept only as an admin/archive fallback;
- PROD datasource uses Coolify prod DB through a column-limited read-only role;
- SQL/cards и data boundary описаны в [`../docs/analytics-dashboard.md`](../docs/analytics-dashboard.md).

Metabase was temporarily connected to the prod Docker network
`<prod-resource-uuid>` without restarting bot/Postgres. Durable follow-up: add that network as
an external analytics-compose network and recreate only Metabase. Also rotate the temporary
Metabase/admin and `metabase_prod_ro` passwords. Rollback dashboard-слоя не требует rollback бота:
switch `/dashboard/2` back to the DEV archive or hide/remove the PROD datasource in Metabase.

## Как работает деплой

### Целевой CD через Coolify

Текущий dev CD описан в [`coolify-cd.md`](coolify-cd.md): CI workflow и Deploy workflow гоняют
DB-backed Go-проверки с миграциями; Deploy workflow не собирает image, если эти проверки не прошли.
После проверок он собирает image `ghcr.io/daniil4545/transcrib:sha-<sha>`, затем строго
последовательно:

1. PATCH-ит Coolify Service точным `src/docker-compose.coolify.yml` как base64
   `docker_compose_raw` с `instant_deploy=false`.
2. PATCH-ит `APP_IMAGE` на полный SHA-tag.
3. Вызывает Coolify deploy webhook ровно один раз.

Coolify Service хранит Compose внутри. Репозиторный `src/docker-compose.coolify.yml` —
единственный источник истины для обоих контуров: workflow синхронизирует его перед deploy и в
`transcrib-dev`, и в `transcrib-prod`. Правка Compose руками в Coolify UI недопустима: она живёт
до следующего deploy и ломает контур на ближайшем рестарте (инцидент 2026-08-12). `dev` выкатывается
автоматически с ветки `dev`; prod выкатывается только вручную по release tag через
`workflow_dispatch` с `target=prod`. Push в `main` prod не выкатывает. Environment `production`
хранит prod secrets, required reviewers можно включить только если тариф GitHub это позволяет.

Обязательная проверка работающего окружения после завершения workflow:

```sh
deploy/verify-deploy.sh <dev|prod> <full-40-char-git-sha>
```

Она вызывает только разрешённые команды из `safe-ssh.sh`, не читает переменные окружения и секреты
и проверяет два связанных по ID контейнера снимка Docker: точный SHA образа и OCI revision,
health-состояние, перезапуски, OOM, время старта, `startup_complete`, запрещённые ошибки за весь
текущий запуск и отдельную проверку БД/goose. Ошибка Docker/inspect/logs/exec считается
операционной ошибкой, а не результатом проверки runtime. Коды: `0` PASS, `1` runtime FAIL,
`2` неверный ввод, `3` операционная ошибка, `4` INCONCLUSIVE. GitHub Actions пока не вызывает
этот скрипт автоматически: сразу после workflow его запускает оператор или агент. Без `PASS`
deploy не принят.

Coolify MCP (`https://coolify.example.com/mcp`) можно подключить к Codex как read-only источник
сведений о Coolify resources. Он помогает агенту быстро понять projects/services/databases перед
диагностикой, но deploy, rollback, env changes и runtime-проверки остаются через GitHub Actions,
Coolify webhook и `deploy/safe-ssh.sh`. Настройка и токен описаны в [`coolify-cd.md`](coolify-cd.md).

Для Coolify Telegram notifications нужны две infra-настройки на самом Coolify: `extra_hosts` pin
`api.telegram.org:149.154.167.220` и `APP_URL=https://coolify.example.com`. Иначе Telegram может
таймаутиться или получить inline button URL вида `http://localhost`. Для Coolify Monitoring/Sentinel
нужен внутренний bind `10.0.0.1:8000:8080`: `coolify-sentinel` ходит в Coolify через
`host.docker.internal:8000`, а публичный UI остаётся за host nginx.
Telegram notification settings включены на team/project `transcrib` и покрывают оба Coolify service:
`transcrib-dev` и `transcrib-prod`.
Coolify realtime остаётся закрыт на loopback `127.0.0.1:6001-6002`; host nginx в
host nginx vhost `coolify.example.com` прокидывает websocket routes `/app/` и `/terminal/ws`.

Текущий prod rollout: tag `0.0.3.4` -> `4804e9e6162238f663bf0789eacff9bcc087ce95`,
active resource `transcrib-prod` (`<prod-resource-uuid>`).

Legacy deploy через bare repo `/srv/git/transcrib.git` и `post-receive` снят: артефакты удалены из
репозитория, git remote `prod` убран. Единственный путь выкатки — Coolify CD; порядок и гейты — этот
файл и [`coolify-cd.md`](coolify-cd.md).
История hook'а доступна в git до тега `0.0.3.7`.

Один compose-файл [`../src/docker-compose.prod.yml`](../src/docker-compose.prod.yml) обслуживает обе
среды. Разные `-p` дают разные сети, контейнеры и volumes.

## Деплой prod

Рабочий порядок: feature branch мержится в `dev`, GitHub Actions автоматически выкатывает Coolify
dev bot, владелец проверяет `@transcrib_dev_bot`, затем готовится release PR `dev -> main`. Merge в
`main` не выкатывает prod. Для prod владелец даёт отдельную команду, ставится release tag и вручную
запускается `Deploy` workflow на этом tag с `target=prod`. На prod должна лежать только стабильная
версия.

Жёсткий stop-gate перед запуском prod deploy: на prod не должно быть активных звонков. Перед
`gh workflow run ... -f target=prod` обязательно выполнить:

```sh
deploy/safe-ssh.sh coolify-prod-active-jobs
deploy/safe-ssh.sh coolify-prod-app-logs 300
```

`coolify-prod-active-jobs` должен показать `active_jobs_count=0` и завершиться с кодом `0`.
Активными считаются `created`, `joining`, `recording`, `processing`, `transcribed`, `summarizing`,
`awaiting_reauth`. Если команда вернула ненулевой код или app logs показывают текущий звонок,
provider polling/processing текущей встречи, restart-loop, `409 Conflict`, config/db ошибки или
сомнительную активность по `job_id`, prod deploy переносится до завершения звонков и повторной
чистой проверки. Никаких исключений "по ощущениям".

Перед prod deploy локально должны быть зелёные проверки по смыслу изменения. Базовый набор для Go:

```sh
cd src
NO_DOTENV=1 DATABASE_URL=postgres://transcrib:transcrib@localhost:5432/transcrib?sslmode=disable make ci-check
```

`go test ./...` без `DATABASE_URL` не считается полным gate: DB-backed тесты пропускаются.
`NO_DOTENV=1` не даёт AI-агенту читать локальные `.env` с секретами; обычный локальный запуск может
использовать `.env`.

Деплой:

```sh
gh workflow run deploy.yml --ref <release-tag> -f target=prod
gh run watch <run-id> --exit-status
```

Проверка:

```sh
deploy/verify-deploy.sh prod <full-release-git-sha>
```

Ожидаемо:

- результат `PASS`, app и Postgres `running/healthy`;
- app image и `app_revision` точно совпадают с полным release tag SHA;
- restart count равен `0`, OOM flag равен `false`, start time корректен;
- в логах текущего запуска есть `startup_complete` и за весь этот запуск нет fatal/panic,
  `409 Conflict`, config/db errors;
- отдельный DB probe отвечает и читает положительный `goose_version`;
- `active_jobs_count=0` был подтверждён перед запуском prod deploy;

На текущем Coolify prod после успешного deploy migrate-container может не оставаться доступным в
Docker. Prod post-deploy контракт проверяет DB/goose, но не требует сохранённый one-shot container.
`coolify-prod-active-jobs` остаётся только pre-deploy stop-gate и после deploy не запускается.

## Деплой test/dev

Dev-среда нужна для живых проверок фиксов и как резервный бот. Текущий default: GitHub Actions
автоматически деплоит `dev` в Coolify service `transcrib-dev`. У неё отдельный Telegram token,
отдельные Coolify-managed env variables и отдельный Postgres volume. Один `BOT_TOKEN` в prod и dev
запрещён: Telegram long polling допускает только один активный процесс на токен.

Log-level policy: test/dev env держит `LOG_LEVEL=debug`, prod env держит `LOG_LEVEL=info`. Секреты не
печатать даже на debug; уровень нужен для диагностики состояний и внешних интеграций.

Деплой:

```sh
git push origin dev
# или ручной rerun:
gh workflow run deploy.yml --ref dev -f target=dev
```

Сначала дождаться успешного workflow, затем обязательно проверить работающее окружение:

```sh
gh run watch <run-id> --exit-status
deploy/verify-deploy.sh dev <full-dev-git-sha>
deploy/safe-ssh.sh coolify-prod-health
```

Ожидаемо:

- `verify-deploy.sh` возвращает `PASS`;
- app image и `app_revision` точно совпадают с полным ожидаемым SHA;
- app и Postgres находятся в состоянии `running/healthy`, без restart/OOM;
- dev migrate-container, если доступен, имеет тот же image/revision и `Exited (0)`;
- если Coolify уже удалил one-shot migrate-container, скрипт возвращает `INCONCLUSIVE` (`4`):
  DB/goose не подменяет доказательство exit code, deployment task проверяется отдельно;
- prod контейнеры не пересоздавались test-деплоем.

## Диагностика без произвольного SSH

Для AI/devops диагностики использовать [`safe-ssh.sh`](safe-ssh.sh). Скрипт read-only и не читает
секреты. До запуска должен быть загружен SSH key в локальный `ssh-agent`:

```sh
ssh-add ~/.ssh/id_ed25519
```

Основные команды:

```sh
deploy/safe-ssh.sh prod-health
deploy/safe-ssh.sh prod-deployed-sha
deploy/safe-ssh.sh test-health
deploy/safe-ssh.sh test-deployed-sha
deploy/safe-ssh.sh prod-app-journal "2026-06-24 00:00" "2026-06-25 00:00"
deploy/safe-ssh.sh prod-postgres-journal "2026-06-24 00:00" "2026-06-25 00:00"
deploy/safe-ssh.sh test-app-journal "2 hours ago"
deploy/safe-ssh.sh test-postgres-journal "2 hours ago"
deploy/safe-ssh.sh journal-usage
deploy/safe-ssh.sh dozzle-ps
deploy/safe-ssh.sh dozzle-firewall
deploy/safe-ssh.sh coolify-health
deploy/safe-ssh.sh coolify-public
deploy/safe-ssh.sh coolify-dev-health
deploy/verify-deploy.sh dev <full-dev-git-sha>
deploy/safe-ssh.sh coolify-dev-app-logs 200
deploy/safe-ssh.sh coolify-prod-health
deploy/verify-deploy.sh prod <full-release-git-sha>
deploy/safe-ssh.sh coolify-prod-app-logs 300
deploy/safe-ssh.sh coolify-prod-postgres-logs 100
deploy/safe-ssh.sh coolify-prod-migrate-logs 100
deploy/safe-ssh.sh docker-inventory
deploy/safe-ssh.sh nginx-inventory
deploy/safe-ssh.sh system-failed
deploy/safe-ssh.sh docker-events 24
deploy/safe-ssh.sh docker-daemon-logs 200
deploy/safe-ssh.sh kernel-errors 200
deploy/safe-ssh.sh oom-kills 200
deploy/safe-ssh.sh cron-logs 200
deploy/safe-ssh.sh backup-status
deploy/safe-ssh.sh backup-log 100
deploy/safe-ssh.sh nginx-logs 200
deploy/safe-ssh.sh disk
deploy/safe-ssh.sh memory
deploy/safe-ssh.sh nginx-test
deploy/safe-ssh.sh site-head
deploy/safe-ssh.sh git-status
```

OpenRouter direct access from the RU VPS can be blocked by Cloudflare. Current host-side workaround
is `mihomo.service` on `10.0.0.1:20171` (HTTP) / `10.0.0.1:20170` (SOCKS), available only to Docker
private networks. Validate it without printing secrets:

```sh
deploy/safe-ssh.sh mihomo-status
deploy/safe-ssh.sh openrouter-egress
```

Durable app wiring is `OPENROUTER_PROXY_URL=http://10.0.0.1:20171` in Coolify-managed env. Global
`HTTPS_PROXY` is emergency-only because it can affect Telegram, Speech2Text and YooKassa.

Логи описаны в [`logs.md`](logs.md). Коротко: active Coolify prod пишет app/Postgres в host journald
с retention 14 дней; Coolify dev на 2026-07-01 фактически использует Docker `json-file`, поэтому его
логи читаются через `coolify-dev-*-logs` или Coolify UI и пока не являются 14-дневным архивом.
Dozzle логи не хранит и не является архивом: это live-viewer поверх Docker API для текущих
контейнеров.

Архивные расследования legacy prod и legacy test fallback по времени делаются через journald на
хосте и читаются только через `safe-ssh.sh`. Активный Coolify dev сейчас не имеет journald-архива:
для него доступны текущие Docker/Coolify логи через `coolify-dev-*-logs`. Активный Coolify prod
проверяется через `coolify-prod-*-logs` и `coolify-prod-health`. Фильтрацию по `job_id`,
`user_id`, `external_job_id`, `event` делать локально через `rg` поверх вывода, не через
произвольный remote shell:

```sh
deploy/safe-ssh.sh test-app-journal "15 minutes ago"
deploy/safe-ssh.sh test-app-journal "15 minutes ago" | rg '"event":"startup_complete"'
deploy/safe-ssh.sh coolify-dev-app-logs 200 | rg '"event":"startup_complete"'
deploy/safe-ssh.sh coolify-prod-app-logs 300 | rg '"event":"startup_complete"'
deploy/safe-ssh.sh prod-app-journal "2026-06-24 00:00" "2026-06-25 00:00" | rg '"job_id":123'
deploy/safe-ssh.sh prod-app-journal "yesterday" "today" | rg '"user_id":456'
deploy/safe-ssh.sh prod-app-journal "2 hours ago" | rg 'external_job_id|provider_status'
deploy/safe-ssh.sh prod-postgres-journal "1 hour ago"
deploy/safe-ssh.sh journal-usage
```

Retention drop-in установлен 2026-06-26:
`/etc/systemd/journald.conf.d/transcrib-retention.conf` задаёт `MaxRetentionSec=14day`,
`SystemMaxUse=2G`, `SystemKeepFree=10G`, `Compress=yes`. При переносе/recovery создание drop-in и
restart `systemd-journald` остаются DevOps/manual mutation после подтверждения владельца.

Инцидентный минимум: сначала `coolify-prod-health`/`coolify-dev-health` (`prod-health` и
`test-health` только для legacy fallback), затем app/postgres logs для нужной среды, затем
`docker-events`, `system-failed`,
`docker-daemon-logs`, `kernel-errors`, `oom-kills`, `disk`, `memory`, `cron-logs`, `backup-status` и
`backup-log`. Это ручная диагностика; автоматические алерты и метрики остаются отдельным
observability-слайсом.

## Rollback

Rollback делается переводом Coolify-ресурса на предыдущий image tag:

```sh
# dev rollback, только после подтверждения владельца:
# поставить APP_IMAGE=ghcr.io/daniil4545/transcrib:sha-<previous_sha> и redeploy

# prod rollback, только после отдельного подтверждения владельца:
# поставить APP_IMAGE=ghcr.io/daniil4545/transcrib:sha-<previous_sha> и redeploy
```

Откат кода не откатывает данные. Если релиз включал миграцию БД, сначала проверить совместимость
схемы. На prod предпочтительнее forward-fix, а не `goose down`.

## Миграции и частичный deploy

В Coolify compose `app` зависит от `migrate: service_completed_successfully`; при failed migrate
нужно проверять `verify-deploy.sh`, Coolify deployment task и `coolify-dev-migrate-logs`. Если
one-shot container уже удалён, `verify-deploy.sh dev` честно возвращает `INCONCLUSIVE`: успешный
DB/goose probe доказывает доступность схемы, но не exit code миграции именно этого deploy. Rollback
данных через `goose down` не делать; использовать forward-fix или restore из backup по отдельному
решению.

Перед рискованными миграциями prod-БД нужен свежий backup. Runbook — [`backup.md`](backup.md).

## Что нельзя делать без отдельного подтверждения

- `docker compose down -v`, `docker volume rm`, глобальный `docker system prune`;
- prod/test deploy, prod/test rollback, restart/recreate app;
- правка Coolify prod/dev env;
- чтение или печать секретов;
- изменение nginx/certbot/ufw;
- операции, которые могут задеть соседние проекты на общем VPS.

## Первичная установка или восстановление

Эти шаги нужны только при переносе/recovery, не при обычном deploy:

Prod и dev поднимаются как Coolify services из репозиторного `src/docker-compose.prod.yml`; env
задаётся в Coolify UI. Порядок первичной установки сервиса описан выше,
специфика Coolify для transcrib — [`coolify-cd.md`](coolify-cd.md).

Минимум Coolify prod env:

```sh
BOT_TOKEN=...
POSTGRES_PASSWORD=<strong password>
DATABASE_URL=postgres://transcrib:<same password>@postgres:5432/transcrib?sslmode=disable
```

Полный список переменных и дефолтов — [`../src/.env.example`](../src/.env.example).
