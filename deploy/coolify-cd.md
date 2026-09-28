# Coolify CD runbook

Цель: GitHub Actions собирает и публикует Docker image, Coolify делает redeploy. Dev работает
автоматически от ветки `dev`; prod выкатывается только вручную по release tag.

## Текущая схема

- UI Coolify: `https://coolify.example.com` (`302` на `/login`).
- MCP endpoint для AI-агентов: `https://coolify.example.com/mcp`.
- Coolify на сервере слушает только локально: `127.0.0.1:8000`.
- Coolify realtime слушает только loopback `127.0.0.1:6001-6002`.
- Публичный доступ даёт host nginx через host nginx vhost `coolify.example.com`: `/` -> `8000`,
  `/app/` -> `6001`, `/terminal/ws` -> `6002`.
- Runtime-образы в Coolify: `ghcr.io/daniil4545/transcrib:sha-<git-sha>`.
  Moving tags `dev` и `prod` публикуются, но не используются как active `APP_IMAGE`.
- Coolify Service хранит Compose внутри ресурса, а не читает его из репозитория при каждом deploy.
- Репозиторный `src/docker-compose.coolify.yml` — единственный источник истины для обоих контуров:
  workflow перед deploy записывает в Service точное содержимое этого файла.
- Править Compose руками в Coolify UI нельзя: правка живёт до следующего deploy и роняет контур на
  ближайшем рестарте (инцидент 2026-08-12).
- `exclude_from_hc` в migrate - Coolify extension для one-shot service; обычный
  `docker compose config` без удаления этой строки его отклоняет.
- Dev resource UUID: `<dev-resource-uuid>`.
- Prod resource UUID: `<prod-resource-uuid>`.

Почему так: GitHub Actions даёт тест-гейт и immutable image до деплоя, Coolify отвечает только за
pull/redeploy. Это проще контролировать, чем прямой auto-deploy из Git.

## Coolify MCP для AI-агента

Coolify MCP включается как read-only источник инфраструктурной правды для Codex: список projects,
servers, services, applications, databases и детали конкретного resource. Он полезен перед
диагностикой и deploy-разбором, потому что агент может быстро увидеть Coolify-модель без SSH и без
чтения серверных `.env`.

Граница: текущий Coolify MCP read-only. Он не заменяет GitHub Actions deploy, Coolify webhook,
`deploy/safe-ssh.sh` и ручное внесение секретов. Для фактического deploy, rollback, env changes,
логов контейнера и Postgres health по-прежнему использовать runbook ниже.

Рекомендуемый порядок для DevOps-агента:

1. MCP `get_infrastructure_overview` - понять текущие Coolify resources.
2. MCP `list_services`/`get_service` - проверить `transcrib-dev` или `transcrib-prod`.
3. GitHub Actions/Coolify webhook - только для deploy по штатному workflow.
4. `deploy/safe-ssh.sh coolify-*-health` и `coolify-*-logs` - подтвердить фактический runtime.

Настройка Codex MCP (токен не коммитить). В обычном терминале:

```sh
codex mcp add coolify --url https://coolify.example.com/mcp --bearer-token-env-var COOLIFY_MCP_TOKEN
```

Эквивалентный TOML-фрагмент:

```toml
[mcp_servers.coolify]
url = "https://coolify.example.com/mcp"
bearer_token_env_var = "COOLIFY_MCP_TOKEN"
enabled = true
```

Токен создать в Coolify UI: `Security` -> `API Tokens`, permission `read`, срок действия лучше
ограничить. В shell, из которой запускается Codex:

```sh
export COOLIFY_MCP_TOKEN='<token>'
codex mcp list
```

Для project-local Codex config также нужен сетевой allowlist на `coolify.example.com`, если
профиль ограничивает домены.

## GitHub

Workflow file:

- `.github/workflows/deploy.yml` - checks, build/push GHCR image, синхронизация dev Compose,
  PATCH `APP_IMAGE`, один вызов Coolify webhook.

Repository secrets:

```text
COOLIFY_TOKEN
COOLIFY_WEBHOOK_DEV
```

Status 2026-07-01: repo secrets are configured. `COOLIFY_TOKEN` is a Coolify API token with
`read`, `write`, and `deploy` abilities, because self-hosted Coolify keeps `/api/v1/deploy` behind
the global API gate. Coolify API is enabled and `GET /api/v1/version` returns `200`.

GitHub Environment:

```text
production
```

Environment secrets `production`:

```text
COOLIFY_WEBHOOK_PROD
COOLIFY_SERVICE_UUID_PROD
```

Status 2026-07-03: `production` exists; `COOLIFY_WEBHOOK_PROD` and
`COOLIFY_SERVICE_UUID_PROD=<prod-resource-uuid>` are configured. Current deploy branch policies:
branch `main`, tag pattern `0.*` and legacy explicit tag `0.0.1`.

Dev deploy запускается автоматически с `dev`. Prod deploy не запускается по push в `main`: его
запускает владелец вручную через `workflow_dispatch` с `target=prod`, выбрав release tag как ref.
GitHub required reviewers недоступны на текущем billing plan; текущий рабочий gate - ручной запуск
workflow на tag. Prod webhook лежит только в environment `production`; общий `COOLIFY_TOKEN` лежит
в repo secrets.

После локальной проверки dev workflow выполняет операции строго в таком порядке:

1. Кодирует точное содержимое `src/docker-compose.coolify.yml` в base64 и делает PATCH Service с
   полями `docker_compose_raw` и `instant_deploy=false`.
2. Обновляет `APP_IMAGE` через `PATCH /api/v1/services/{uuid}/envs` на
   `ghcr.io/daniil4545/transcrib:sha-${GITHUB_SHA}`.
3. Вызывает deploy webhook ровно один раз.

`instant_deploy=false` обязателен: Compose PATCH только обновляет конфигурацию, а единственный
webhook запускает deploy уже с согласованными Compose и образом. Плавающий tag `dev` может остаться
в локальном Docker cache Coolify, поэтому active `APP_IMAGE` всегда использует полный SHA-tag.

Этот compose-sync действует только для dev. Prod workflow пока обновляет только `APP_IMAGE` и
вызывает свой webhook; `transcrib-prod` не синхронизировать с репозиторным Compose до успешной
проверки работающего dev-окружения.

Если `COOLIFY_*` secrets пустые или `APP_IMAGE` не обновился, deploy job должен падать. Успешная
публикация image без Coolify redeploy не считается успешным CD.

## Coolify

Структура Coolify:

```text
Project: transcrib
  Environment: dev
    Service: transcrib-dev
  Environment: prod
    Service: transcrib-prod
```

Не класть `transcrib-dev` в environment `production`: env в Coolify - это граница настроек,
секретов, volumes и webhook/resource lifecycle.

Создать два Coolify Service:

| Service | Deploy ref | Compose внутри Service | Image |
| --- | --- | --- | --- |
| `transcrib-dev` | `dev` | Синхронизируется из точного `src/docker-compose.coolify.yml` перед deploy | `ghcr.io/daniil4545/transcrib:sha-<git-sha>` |
| `transcrib-prod` | release tag из `main` | Синхронизируется из точного `src/docker-compose.coolify.yml` перед deploy | `ghcr.io/daniil4545/transcrib:sha-<git-sha>` |

В каждом Service вручную задать стартовый `APP_IMAGE`, дальше workflow обновляет его перед deploy:

```text
dev:  ghcr.io/daniil4545/transcrib:sha-<current-dev-sha>
prod: ghcr.io/daniil4545/transcrib:sha-<current-main-sha>
```

Остальные env-переменные брать из `src/.env.example` и вводить вручную в Coolify UI. Секреты не
коммитить и не читать через SSH.

OpenRouter proxy policy:

- Durable config: `OPENROUTER_PROXY_URL=http://10.0.0.1:20171` in Coolify-managed env; code support
  exists in Git `main` since `0.0.3.5`, but live prod `0.0.3.4` has not deployed it yet.
- Emergency-only config before the next deploy: global `HTTP_PROXY` / `HTTPS_PROXY` can be
  set in service `.env` together with broad `NO_PROXY`, but Coolify may regenerate this file on the
  next deploy. Replace the hotfix through managed env as soon as possible.
- Do not route Telegram, Speech2Text or YooKassa through the proxy intentionally unless a separate
  incident requires it.

Status 2026-07-01: Coolify project `transcrib` has environments `dev` and `prod`.
`transcrib-dev` exists in `dev` as Coolify Service `<dev-resource-uuid>`; env was imported
into Coolify-managed `environment_variables`, not only copied to
`/data/coolify/services/.../.env`, because Coolify regenerates that file on deploy.
`DATABASE_URL` is set to the internal service DNS `postgres:5432`. Dev DB was migrated from legacy
`transcrib-dev-postgres-1` into Coolify Postgres on 2026-07-01; source and before-restore dumps are
in `/var/backups/transcrib/coolify-migration/`.

`transcrib-prod` exists in `prod` as Coolify Service `<prod-resource-uuid>`. Current prod rollout
uses `APP_IMAGE=ghcr.io/daniil4545/transcrib:sha-8940569680b43af397b59f3e9df51276f3989b02`.
Prod env was imported into Coolify-managed `environment_variables`; `DATABASE_URL` points to the
internal `postgres:5432`. Live payment links and bank-card recurring were accepted on 2026-07-09:
`RECURRING_PAYMENTS_ENABLED=true`, prod redeploy started `recurring_worker_started`, payment-link
smoke activated a `900/900` subscription, and self-service disable cleared the saved method.
Monitoring of the first real recurring charge is still open.

GHCR pull access:

- GitHub Actions publishes with `GITHUB_TOKEN`.
- Server Docker must be able to pull the private package:
  `docker pull ghcr.io/daniil4545/transcrib:sha-<git-sha>`.
- Current working setup: root Docker login to `ghcr.io` as `daniil4545` with a classic PAT that has
  package read access. Do not commit or print this token.
- Minimal token scope: `read:packages`. If GHCR returns `403` for a private repo-linked package,
  recreate the classic PAT with `read:packages` + `repo`, then run:

```sh
ssh -t root@203.0.113.10 'docker login ghcr.io -u daniil4545'
```

Validation:

```sh
gh workflow run deploy.yml --ref dev -f target=dev
gh run watch <run-id> --exit-status
deploy/verify-deploy.sh dev <full-dev-git-sha>
```

Зелёный workflow подтверждает только выполнение CI/CD. Сразу после него оператор или агент обязан
запустить `verify-deploy.sh` с полным SHA; без результата `PASS` деплой dev не принят.

Ожидаемое состояние dev:

- `verify-deploy.sh` возвращает `PASS`;
- app и Postgres находятся в состоянии `running/healthy`, без перезапусков и OOM;
- образ app и OCI revision точно совпадают с полным ожидаемым GitHub SHA;
- в текущем запуске app есть `startup_complete` и нет fatal/panic, `409 Conflict`,
  ошибок конфигурации и БД;
- отдельная проверка БД проходит и читает `goose_version`;
- доступный контейнер migrate имеет тот же revision и состояние `Exited (0)`;
- legacy-контейнер `transcrib-dev-app-1` остановлен, поэтому dev-бот с long polling только один.

Coolify может удалить одноразовый контейнер migrate. В этом случае команда возвращает
`INCONCLUSIVE` (`4`), потому что состояние БД/goose не доказывает exit code именно этого деплоя;
результат задачи деплоя в Coolify нужно проверить отдельно. Ложный `PASS` не возвращается.

Проверка prod:

```sh
# Только перед деплоем:
deploy/safe-ssh.sh coolify-prod-active-jobs
deploy/safe-ssh.sh coolify-prod-app-logs 300
gh workflow run deploy.yml --ref <release-tag> -f target=prod
gh run watch <run-id> --exit-status
deploy/verify-deploy.sh prod <full-release-git-sha>
```

`coolify-prod-active-jobs` is a hard pre-deploy stop-gate: it must print `active_jobs_count=0`
and exit successfully before `Deploy target=prod` is started. Any active
`created|joining|recording|processing|transcribed|summarizing|awaiting_reauth` job or current-call
evidence in app logs postpones prod deploy.

Ожидаемое состояние prod:

- `verify-deploy.sh` возвращает `PASS`;
- app и Postgres находятся в состоянии `running/healthy`, без перезапусков и OOM;
- образ app и OCI revision точно совпадают с полным commit release-тега;
- в текущем запуске app есть `startup_complete` и нет fatal/panic, `409 Conflict`,
  ошибок конфигурации и БД;
- отдельная проверка БД проходит и читает `goose_version`;
- перед деплоем подтверждено `active_jobs_count=0`;

Текущий Coolify prod может не сохранять видимый контейнер migrate после деплоя. Проверка prod
не требует этот контейнер и отдельно проверяет состояние БД/goose. `coolify-prod-active-jobs`
остаётся только stop-gate перед деплоем; после деплоя эта команда не запускается как gate.

`verify-deploy.sh` обязательно запускает оператор или агент после `gh run watch`; GitHub Actions
не вызывает его автоматически. Скрипт использует только фиксированные команды
`coolify-*-deploy-status`, `coolify-*-current-run-evidence` и `coolify-*-db-status` из
`safe-ssh.sh`, не читает переменные Coolify или контейнеров. Начальный и финальный снимки должны
содержать одинаковые ID контейнеров app/Postgres и `StartedAt`; финальный снимок повторяет все
проверки health, revision и runtime.

## Database model

Each Coolify environment owns its service and Postgres volume. Dev and prod must never share a
database, volume or `BOT_TOKEN`.

Dev current state:

- old source DB: legacy `transcrib-dev-postgres-1`, still preserved;
- active DB: Coolify `postgres-<dev-resource-uuid>`;
- active app: Coolify `app-<dev-resource-uuid>`;
- legacy `transcrib-dev-app-1`: stopped.

Prod current state:

- old source DB: legacy `transcrib-postgres-1`, still preserved as rollback source;
- active DB: Coolify `postgres-<prod-resource-uuid>`;
- active app: Coolify `app-<prod-resource-uuid>`;
- legacy `transcrib-app-1`: stopped.

Prod DB migration evidence 2026-07-01:

- fresh legacy dump: `/var/backups/transcrib/postgres/transcrib_20260701T135323Z.sql.gz`;
- before app deploy, source and target counts matched: `goose_version=8`, `users=6`, `jobs=22`,
  `payments=0`, `subscriptions=9`;
- after Coolify deploy, migrate reached `goose_version=10`;
- legacy DB volume was not modified and remains rollback source.

Repeat prod cutover checklist:

1. Create or verify `transcrib-prod` inside Coolify environment `prod`.
2. Set `APP_IMAGE=ghcr.io/daniil4545/transcrib:sha-<verified_sha>`.
3. Import prod env manually into Coolify-managed variables.
4. Make a fresh prod backup and restore it into Coolify Postgres.
5. Compare source/target counts before app cutover.
6. Create release tag from the verified `main` SHA.
7. Deploy prod manually through GitHub `Deploy` workflow on that tag with `target=prod`.
8. Check `coolify-prod-health`, logs and bot manually before considering legacy prod fallback removable.

## Logs

See [`logs.md`](logs.md). Important current fact: Coolify prod app/Postgres use `journald` logging
and satisfy the 14-day archive gate through host journald retention. Coolify dev containers use
Docker `json-file` logging on 2026-07-01, not host journald. `coolify-dev-*-logs` and Coolify UI work
for live/current logs, but this is not the 14-day journald archive.

## Server setup

Статус: выполнено 2026-07-01, сертификат Let's Encrypt до `2026-09-29`. Воспроизводимая процедура:

```sh
mkdir -p /var/www/coolify.example.com/.well-known/acme-challenge
# установить nginx vhost coolify.example.com в sites-available/sites-enabled
nginx -t
certbot certonly --webroot -w /var/www/coolify.example.com -d coolify.example.com
nginx -t && nginx -s reload
```

Проверки:

```sh
curl -I https://coolify.example.com/
curl --http1.1 -i --max-time 10 \
  -H 'Connection: Upgrade' \
  -H 'Upgrade: websocket' \
  -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' \
  -H 'Sec-WebSocket-Version: 13' \
  https://coolify.example.com/terminal/ws
deploy/safe-ssh.sh coolify-health
deploy/safe-ssh.sh coolify-public
deploy/safe-ssh.sh nginx-test
```

## Cutover

1. Сначала настроить и проверить `transcrib-dev` в Coolify.
2. Убедиться, что push в `dev` проходит CI, публикует image и запускает Coolify deploy.
3. Проверить test bot: `startup_complete`, нет `409 Conflict`, Postgres healthy.
4. Для prod перед первым Coolify deploy сделать свежий backup и restore into Coolify Postgres.
5. Prod переключать только через release tag и ручной `Deploy` workflow с `target=prod`.
6. Старый `git push prod <tag-or-sha>:master` держать emergency fallback до 2-3 успешных prod deploy через Coolify.

## Rollback

Быстрый rollback в Coolify:

1. Поставить `APP_IMAGE=ghcr.io/daniil4545/transcrib:sha-<previous_sha>`.
2. Redeploy app.
3. Проверить `startup_complete` и health.

Rollback данных не делается через `goose down`. Если релиз включал миграцию, использовать
forward-fix или восстановление из backup по отдельному решению.
