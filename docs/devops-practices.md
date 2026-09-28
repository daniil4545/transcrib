# DevOps-практики transcrib (эталон под наш масштаб)

> **Назначение:** прагматичный канон деплоя и эксплуатации — «как делают правильно» в индустрии, отфильтрованное под нашу реальность (один Go-сервис, один VPS, Docker Compose, CI/CD, две среды).
> **Происхождение:** мультиагентный ресёрч (8 категорий, 87 практик, сверка с первоисточниками) + инфра-ревью слайса CI/CD (2026-06-22). Помечено явно: что берём сейчас / откладываем / сознательно пропускаем.

## Наш масштаб (под него фильтруем)

Один Go-монолит (Telegram-бот, long polling — один потребитель на токен), Postgres как БД и очередь, деньги (ЮKassa) и Google OAuth. Один общий VPS (~4 vCPU/8 ГБ), Docker Compose/Coolify, без кластера; Vexa остановлена 2026-06-26, но сервер остаётся общим. Dev CD и prod CD идут через GitHub Actions -> GHCR -> Coolify; prod выкатывается только вручную по release tag, legacy bare-repo hook остаётся fallback. Канон проекта: **минимум инфры, без k8s**. Девопс ведёт AI-агент.

Главное следствие масштаба: **цель — дешёвый откат и чистая остановка, а не zero-downtime**; эталонные «энтерпрайз»-меры (k8s, Vault, GitOps, PITR, OIDC-федерация) для нас — over-engineering, пока не упрёмся.

## Эталон по категориям → что берём сейчас

| Категория | Эталон (как правильно) | Берём сейчас |
|---|---|---|
| **Сборка/артефакты** | Multi-stage, non-root, минимальный base (distroless), тег по git-SHA, реестр | distroless/nonroot (есть), GitHub Actions build, GHCR image `sha-<sha>` + moving tags `dev/prod`; legacy server build только fallback |
| **Конфиг/секреты** | 12-factor, секреты вне git/логов/образа, шифрование чувствительного at-rest, ротация | `.env` chmod 600 (есть), **off-box зашифрованное хранилище секретов**, `gitleaks` pre-commit, шифрование `refresh_token` |
| **Выкатка/откат** | Health-gated, быстрый откат на прошлый артефакт, graceful shutdown | **recreate** (blue-green для бота невозможен), **откат на прежний git-SHA-тег**, пост-деплой health-сигнал |
| **Миграции БД** | Обратносовместимые (expand-contract), `lock_timeout`, индексы `CONCURRENTLY`, forward-fix | expand-contract, `pg_dump` ПЕРЕД разрушающей, `lock_timeout`, forward-fix вместо `goose down` на проде |
| **CI/CD** | Тест-гейт, продвижение по средам, ручной approval на прод, контроль конкурентности | GitHub Actions: тест-гейт, **dev авто через Coolify / prod ручной workflow_dispatch**, legacy `flock` только fallback |
| **Supply-chain CI** | Pin actions по commit-SHA, least-privilege токен, секреты вне PR-воркфлоу | **pin по SHA**, `permissions: contents: read`, секреты в protected `production`-environment, Dependabot |
| **Наблюдаемость/бэкап** | Структурные логи, ротация, health, метрики/алерты, бэкап + тест восстановления | slog JSON + ротация (есть), **ежедневный off-box `pg_dump` + ежемесячный restore-тест**, cron-алерты (диск/бэкап/зомби) |
| **Хардненинг хоста** | Non-root деплой, forced-command SSH-ключ, firewall, non-root в контейнере, изоляция | **`deploy`-юзер вместо root** + forced-command, `cap_drop: all`+`no-new-privileges`, **Dozzle на localhost** (Docker публикует порт в обход ufw!) |

## Что уже внедрено

- Git-SHA image tag на prod/test, без общего `:latest`.
- GitHub Actions: CI workflow и deploy workflow гоняют DB-backed tests с миграциями; deploy workflow
  не собирает и не выкатывает GHCR image, если DB-backed проверки не прошли.
- Coolify dev CD: service `transcrib-dev` в environment `dev`, image `ghcr.io/daniil4545/transcrib:sha-<sha>`.
- Coolify prod CD: service `transcrib-prod` в environment `prod`, текущий rollout `0.0.3.4` выполнен через GitHub Actions -> GHCR -> Coolify.
- Coolify MCP: read-only endpoint `https://coolify.example.com/mcp` для AI-агента; использовать как
  быстрый обзор Coolify resources, не как механизм deploy или env changes.
- Coolify Telegram notifications: для самого Coolify задан `APP_URL=https://coolify.example.com`
  и pin `api.telegram.org:149.154.167.220`; без этого infra alerts падают по timeout или
  `http://localhost` inline button URL.
- Exact-SHA checkout в deploy hook, отдельный deploy-index на среду, `flock` против гонки сборок.
- Dev/test bot как отдельная Coolify-среда с отдельным Postgres volume и Telegram token; legacy
  `transcrib-dev` Postgres сохранён, app остановлен.
- On-server `pg_dump` как временный degraded backup, retention 14 дней, restore-test пройден.
- Journald-архив Coolify prod и legacy prod/test `app` и `postgres` с retention 14 дней (`MaxRetentionSec=14day`,
  `SystemMaxUse=2G`, `SystemKeepFree=10G`).
- Coolify dev logs сейчас читаются через Docker `json-file`/Coolify UI, не через 14-дневный
  journald-архив.
- Dozzle запущен с фильтром `name=transcrib`: подходит для legacy `transcrib-*` контейнеров, но не
  является viewer'ом active Coolify prod/dev `app-*`/`postgres-*`; порт закрыт на `127.0.0.1:48080`,
  доступ через SSH tunnel.
- `safe-ssh.sh` покрывает ручную read-only диагностику не только app/Postgres, но и системный слой:
  failed systemd units, Docker events/daemon journal, kernel/OOM, cron, backup log, nginx journal,
  disk/memory.

## Следующее P0

**P0:**
1. **Off-box `pg_dump`** prod-БД (runbook `../deploy/backup.md`) + ежемесячный restore-test.
2. **Off-box зашифрованное хранилище секретов** + runbook (потеря локальной копии перестаёт быть ЧП).
3. **Автоматические prod health/log alerts** для Coolify prod: restart-loop, OOM, backup freshness,
   disk и zombie jobs; Coolify Telegram notifications уже требуют корректные `APP_URL` и
   `api.telegram.org` pin.
4. **Непривилегированный `deploy`-юзер** (группа docker) вместо root + forced-command в `authorized_keys`.
5. **Dozzle на `127.0.0.1`** — закрыто 2026-07-01; проверять pre-beta smoke.
6. **Ротация/контроль `backup.log`** для on-server cron, пока off-box backup не внедрён.

**P1:**
6. Пост-деплой health-сигнал (лог старта / `getMe`) — отличать «поднялось» от restart-loop.
7. 4 cron-алерта или metrics stack после запуска: диск, провал бэкапа, зомби-джобы,
   недоступность Telegram-DC (`extra_hosts`-пин).
8. Дисциплина миграций в `db.md` (expand-contract, `lock_timeout`, `CONCURRENTLY`).
9. `cap_drop: all` + `no-new-privileges` в compose.
10. `gitleaks` pre-commit (секреты не утекают в git).
11. Supply-chain: pin actions по SHA, least-privilege токен, секреты в protected environment.
12. Metrics/monitoring slice: после публичного запуска выбрать Grafana/Prometheus или более простой
    lightweight healthcheck-агент.

## Откладываем (с триггером возврата)

- **Шифрование `refresh_token` AES-GCM** — вместе с переходом Google в Production (google-слайс).
- **Trivy/SBOM-скан образа** — после того как заведём Actions.
- **HTTP `Shutdown` graceful** — когда появится `web.go` (OAuth callback).
- **Реплика Postgres (HA)** — когда аптайм станет критичен (это про отказ железа, не про потерю данных).

## Сознательно НЕ делаем (на нашем масштабе)

- **Blue-green для бота** — ломает single-consumer (два поллера = `409`). Только recreate.
- **Vault/секрет-менеджер** — лишняя инфра; хватает `sops`+`age` / менеджера паролей.
- **WAL/PITR** — ежедневного дампа достаточно (потеря до суток приемлема; пересмотреть, если RPO ужесточится).
- **Prometheus/Grafana** — cron-алертов хватает на одном боксе.
- **OIDC-федерация для деплоя из облака, Cosign-подпись, k8s, rootless Docker** — не наш масштаб/риск.

## Развод мер защиты (важно не путать)

| От чего защищает | Мера |
|---|---|
| Потеря/порча **данных**, ошибка ИИ/человека | Бэкап `pg_dump` off-box + restore-тест |
| Потеря **доступа/секрета** | Secrets-management (зашифрованное забэкапленное хранилище) |
| Отказ **железа/хоста** | Реплика (HA) — отложено |

Реплика дословно реплицирует и `DROP TABLE` — от ошибки не спасает. От ошибки спасает только бэкап.

## Принципы devops-агента (ALWAYS / NEVER)

**ВСЕГДА:** `pg_dump` перед разрушающей миграцией · `lock_timeout` в рискованной миграции · индексы `CONCURRENTLY` на заполненных таблицах · миграция обратносовместима · git-SHA-тег для отката · проверить «бот стартовал» после деплоя · секреты вне git/логов · помнить, что Docker публикует порт в обход ufw.

**НИКОГДА:** деплой без зелёного DB-backed `make ci-check` с `DATABASE_URL` · blue-green для бота · `goose down` как способ восстановления прода · бэкап без off-box restore-теста · `docker.sock` в app-контейнер · прерывать идущий деплой · считать deny `Read(.env)` барьером на боксе (root-SSH его обходит через `docker exec/inspect`) · читать секреты через SSH (`.env`, `docker inspect env`, `docker exec printenv`).
