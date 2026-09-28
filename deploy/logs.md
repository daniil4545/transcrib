# Логи transcrib

Runbook по логам. Цель: после любого deploy оператор или AI-агент должен быстро понять, где
физически лежит лог, какой лог является архивом, а какой только live-tail.

## Текущий статус

| Контур | Контейнеры | Основной доступ к логам | Архив 14 дней |
| --- | --- | --- | --- |
| Coolify prod | `app-<prod-resource-uuid>`, `postgres-<prod-resource-uuid>`; migrate-container может отсутствовать после deploy | `deploy/safe-ssh.sh coolify-prod-*-logs`, Coolify UI | Да, host journald |
| Coolify dev | `app-<dev-resource-uuid>`, `postgres-<dev-resource-uuid>`, `migrate-<dev-resource-uuid>` | `deploy/safe-ssh.sh coolify-dev-*-logs`, Coolify UI | Нет, сейчас Docker `json-file` |
| Legacy prod fallback | `transcrib-app-1`, `transcrib-postgres-1`, `transcrib-migrate-1` | `deploy/safe-ssh.sh prod-*-logs`, `prod-*-journal` | Да, host journald, пока контейнеры существуют |
| Legacy dev fallback | `transcrib-dev-*` | `deploy/safe-ssh.sh test-*-logs`, `test-*-journal` | Да, host journald, пока контейнеры существуют |
| OpenRouter proxy | `mihomo.service` | `deploy/safe-ssh.sh mihomo-status`, `deploy/safe-ssh.sh mihomo-logs` | Да, host journald |
| Dozzle | `dozzle-dozzle-1` | SSH tunnel -> `http://127.0.0.1:48080` | Нет |

Важно: Coolify-generated compose на 2026-07-01 для dev не содержит `logging.driver=journald`, хотя
`src/docker-compose.coolify.yml` задаёт `logging:`. Фактическая проверка `docker inspect` показывает
`app_log_driver=json-file`. Поэтому Coolify dev logs сейчас существуют, но не считаются
14-дневным journald-архивом. Coolify prod прошёл log gate: `deploy/safe-ssh.sh coolify-prod-health`
показывает `app_log_driver=journald` и `postgres_log_driver=journald`.

## Команды

Coolify prod:

```sh
deploy/verify-deploy.sh prod <full-release-git-sha>
deploy/safe-ssh.sh coolify-prod-app-logs 300
deploy/safe-ssh.sh coolify-prod-postgres-logs 100
```

Перед prod deploy отдельно выполнить `coolify-prod-active-jobs`: это обязательный stop-gate, команда
должна показать `active_jobs_count=0`. Если есть активные Job'ы или app logs показывают текущий
звонок, deploy не запускать. После deploy этот gate не повторяется.

Prod migrate-container может отсутствовать после deploy. `verify-deploy.sh prod` не требует его,
но отдельной safe-ssh командой проверяет ответ БД и читает `goose_version`.

Legacy prod fallback:

```sh
deploy/safe-ssh.sh prod-health
deploy/safe-ssh.sh prod-app-journal "2 hours ago"
deploy/safe-ssh.sh prod-postgres-journal "2 hours ago"
deploy/safe-ssh.sh prod-app-logs 200
```

Coolify dev:

```sh
deploy/verify-deploy.sh dev <full-dev-git-sha>
deploy/safe-ssh.sh coolify-dev-app-logs 200
deploy/safe-ssh.sh coolify-dev-postgres-logs 200
deploy/safe-ssh.sh coolify-dev-migrate-logs 200
```

Если Coolify удалил one-shot migrate-container, dev-проверка возвращает `INCONCLUSIVE` (`4`):
наличие goose version в БД не доказывает `Exited (0)` именно для текущего deploy.

Системный слой:

```sh
deploy/safe-ssh.sh docker-events 24
deploy/safe-ssh.sh docker-daemon-logs 200
deploy/safe-ssh.sh kernel-errors 200
deploy/safe-ssh.sh oom-kills 200
deploy/safe-ssh.sh journal-usage
```

OpenRouter egress proxy:

```sh
deploy/safe-ssh.sh mihomo-status
deploy/safe-ssh.sh mihomo-logs 100
deploy/safe-ssh.sh openrouter-egress
```

Не печатать subscription/config provider'а `mihomo`; в логах допускаются домены маршрутизации, но
не секреты.

## Что проверять после deploy

1. `deploy/verify-deploy.sh <dev|prod> <full-sha>` вернул `PASS`: app/Postgres healthy,
   OCI revision/image SHA точный, restart count `0`, OOM `false`, start time валиден.
2. Перед prod deploy подтверждён `active_jobs_count=0`; это не post-deploy gate.
3. В логах текущего `StartedAt` есть `startup_complete`.
4. За весь текущий запуск нет `panic`, `fatal`, `409 Conflict`, config/db errors.
5. DB отвечает, `goose_version` читается отдельной allowlisted safe-ssh командой.
6. Для dev доступный migrate-container имеет тот же SHA и `Exited (0)`; удалённый one-shot даёт
   `INCONCLUSIVE`. Для prod отсутствие migrate-container не является fail.
7. Для Telegram long polling запущен ровно один app на один bot token.
8. Если OpenRouter summary включён, direct VPS egress может оставаться `403`, но запрос через
   `mihomo` должен давать `HTTP 200` на `/api/v1/models`; public `:20170/:20171` должен быть закрыт.
9. Для Coolify prod читать архив через `coolify-prod-*-logs`; для legacy fallback по времени
   использовать `*-journal`; Dozzle использовать только как live-tail.
10. Для Coolify dev до исправления log driver помнить: `coolify-dev-*-logs` читает Docker json-file,
   а не journald-retention.

## При переносе на новый сервер

Минимальные требования:

- app пишет структурные JSON-логи в stdout;
- у каждого runtime-контура есть documented command для app/postgres/migrate logs;
- prod logs должны иметь архив с retention не меньше 14 дней;
- Dozzle/Coolify UI могут быть удобным live-viewer, но не заменяют архив;
- логи не должны содержать токены, пароли, DSN, приватные ссылки и полные секреты.
