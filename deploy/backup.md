# Бэкап Postgres prod

Цель: ежедневный `pg_dump` prod-БД без чтения `.env` и без печати паролей/DSN. Активный prod после
cutover - Coolify service `transcrib-prod`; для него `deploy/backup.sh` ищет Postgres container по
label `coolify.resourceName=transcrib-prod`. Legacy prod compose project `transcrib`
(`transcrib-postgres-1`) сохранён только как rollback source. Целевой режим — off-box backup с отдельного backup-хоста или
рабочего компьютера владельца. Временный режим до появления внешнего хранилища — on-server backup
на том же VPS.

On-server backup — осознанный degraded mode: он защищает от ошибочной миграции или логической порчи
БД, но не защищает от потери VPS, диска, root compromise или ошибки провайдера.

## Правила безопасности

- Не запускать `docker compose down -v`, `docker volume rm transcrib_*`, глобальный `docker system prune`
  или `docker image prune` для этой задачи.
- Не читать `/opt/transcrib/src/.env`, не делать `docker inspect`, `docker exec printenv`, не печатать
  пароль или `DATABASE_URL`.
- Не считать единственную on-server копию полноценным backup: как только появится внешний носитель,
  дампы нужно уносить off-box.
- Каталог бэкапов держать на зашифрованном диске или в операторском защищённом хранилище. Ключи
  `age`/S3/rsync не хранить в репо.

## Временный on-server запуск

Важно: для active Coolify prod использовать `TRANSCRIB_POSTGRES_RESOURCE=transcrib-prod`, чтобы
контейнер искался по label, а не по generated-name. Команды без этой переменной обращаются к legacy
`transcrib-postgres-1`.

На VPS:

```sh
install -d -m 700 -o root -g root /var/backups/transcrib/postgres
cd /opt/transcrib
TRANSCRIB_BACKUP_MODE=local \
  TRANSCRIB_POSTGRES_RESOURCE=transcrib-prod \
  TRANSCRIB_BACKUP_DIR=/var/backups/transcrib/postgres \
  TRANSCRIB_BACKUP_RETENTION_DAYS=14 \
  TRANSCRIB_BACKUP_MIN_FREE_KB=10485760 \
  ./deploy/backup.sh
```

Перед restore-test или ручной проверкой active Coolify prod сделать свежий dump из Coolify prod БД:

```sh
cd /opt/transcrib
TRANSCRIB_BACKUP_MODE=local \
  TRANSCRIB_POSTGRES_RESOURCE=transcrib-prod \
  TRANSCRIB_BACKUP_DIR=/var/backups/transcrib/postgres \
  TRANSCRIB_BACKUP_RETENTION_DAYS=14 \
  TRANSCRIB_BACKUP_MIN_FREE_KB=10485760 \
  ./deploy/backup.sh
```

Критерий gate: команда завершается `backup: created ...`, `gzip -t` проходит внутри скрипта, затем
свежий dump проходит restore-test из раздела ниже.

`/etc/cron.d/transcrib-backup` на VPS, ежедневно в 03:20 MSK (timezone сервера):

```cron
20 3 * * * root cd /opt/transcrib && TRANSCRIB_BACKUP_MODE=local TRANSCRIB_POSTGRES_RESOURCE=transcrib-prod TRANSCRIB_BACKUP_DIR=/var/backups/transcrib/postgres TRANSCRIB_BACKUP_RETENTION_DAYS=14 TRANSCRIB_BACKUP_MIN_FREE_KB=10485760 ./deploy/backup.sh >>/var/backups/transcrib/postgres/backup.log 2>&1
```

`TRANSCRIB_BACKUP_MIN_FREE_KB=10485760` — guard на 10 GiB свободного места, чтобы backup не забил
общий VPS.

`backup.log` не попадает под retention дампов. Пока backup остаётся on-server, добавить системный
`logrotate` или периодически чистить лог операторской задачей; при переходе на off-box cron лучше
слать результат в syslog/мониторинг.

Фактическое состояние на 2026-06-25:
- каталог `/var/backups/transcrib/postgres` создан с правами `700`;
- первый ручной dump: `transcrib_20260625T010019Z.sql.gz`, `136556` bytes, `gzip -t` PASS;
- проверочный запуск repo-скрипта: `transcrib_20260625T010541Z.sql.gz`, `136556` bytes;
- cron установлен в `/etc/cron.d/transcrib-backup`, mode `0644`;
- restore-test PASS во временном Postgres 16 volume: `goose_version=8`, `users_count=6`,
  `jobs_count=14`; prod volume не подключался.

Фактическое состояние после Coolify prod cutover 2026-07-01:
- cron `/etc/cron.d/transcrib-backup` переведён на
  `TRANSCRIB_POSTGRES_RESOURCE=transcrib-prod`;
- серверная копия `/opt/transcrib/deploy/backup.sh` обновлена до версии с Coolify resource lookup;
- контрольный active prod dump: `transcrib_20260701T141023Z.sql.gz`, `199131` bytes,
  источник в выводе ручного запуска `resource:transcrib-prod/transcrib`.

## Целевой off-box запуск

Предусловия на off-box машине:
- SSH-доступ к `root@203.0.113.10` через `ssh-agent` или локальный `ssh_config`.
- `ssh`, `gzip`, `find`, `date`, `mkdir`, `mktemp`, `df`, `awk`.
- Каталог для дампов вне репозитория, права `700`.

Пример ручного прогона:

```sh
mkdir -p /secure/backups/transcrib
chmod 700 /secure/backups/transcrib

TRANSCRIB_BACKUP_DIR=/secure/backups/transcrib \
  TRANSCRIB_BACKUP_MODE=ssh \
  TRANSCRIB_POSTGRES_RESOURCE=transcrib-prod \
  TRANSCRIB_BACKUP_RETENTION_DAYS=14 \
  deploy/backup.sh
```

Скрипт создаёт файл вида `transcrib_YYYYMMDDTHHMMSSZ.sql.gz`, проверяет:
- команда `pg_dump` завершилась успешно;
- файл не пустой и больше `TRANSCRIB_BACKUP_MIN_BYTES` (по умолчанию `1024`);
- `gzip -t` проходит;
- старые `transcrib_*.sql.gz` удалены по retention в локальном каталоге.

Exit-коды:
- `0` — бэкап создан;
- `2` — ошибка конфигурации;
- `3` — нет локальной зависимости;
- `4` — сбой `pg_dump`/SSH/gzip (в local mode SSH не используется);
- `5` — пустой, слишком маленький или битый дамп;
- `6` — сбой retention cleanup.

Cron на off-box машине, если нужно то же окно `03:20 MSK`, ежедневно в `00:20 UTC`:

```cron
20 0 * * * cd /path/to/transcrib && TRANSCRIB_BACKUP_DIR=/secure/backups/transcrib TRANSCRIB_BACKUP_MODE=ssh TRANSCRIB_POSTGRES_RESOURCE=transcrib-prod ./deploy/backup.sh >>/secure/backups/transcrib/backup.log 2>&1
```

Если нужен S3/rsync/`age`, сделать операторский wrapper вне репо. Пример формы без секретов:

```sh
#!/usr/bin/env bash
set -euo pipefail

cd /path/to/transcrib
TRANSCRIB_BACKUP_DIR=/secure/backups/transcrib \
  TRANSCRIB_BACKUP_MODE=ssh \
  TRANSCRIB_POSTGRES_RESOURCE=transcrib-prod \
  ./deploy/backup.sh

# Дальше оператор добавляет свой sync/encrypt шаг.
# Секреты, access key, age identity и remote URL не хранить в репозитории.
```

## Ежемесячный restore-test

Проверка выполняется на off-box машине или отдельном тестовом хосте. Prod volume не используется и не
подключается. Имена временных контейнера/volume уникальны; удаляются только они.

1. Выбрать свежий дамп:

```sh
backup_file="$(ls -1t /secure/backups/transcrib/transcrib_*.sql.gz | head -1)"
test_id="transcrib-restore-$(date -u +%Y%m%dT%H%M%SZ)"
```

2. Поднять чистый временный Postgres 16:

```sh
docker volume create "${test_id}-pgdata"
docker run -d --name "${test_id}" \
  -e POSTGRES_USER=transcrib \
  -e POSTGRES_PASSWORD=restore-test-only \
  -e POSTGRES_DB=transcrib \
  -v "${test_id}-pgdata:/var/lib/postgresql/data" \
  postgres:16
```

3. Дождаться готовности и восстановить дамп:

```sh
until docker exec "${test_id}" pg_isready -U transcrib -d transcrib; do sleep 1; done
gzip -dc "$backup_file" | docker exec -i "${test_id}" psql -v ON_ERROR_STOP=1 -U transcrib -d transcrib
```

4. Проверить базовые признаки живой схемы и данных:

```sh
docker exec "${test_id}" psql -U transcrib -d transcrib -c "select max(version_id) from goose_db_version;"
docker exec "${test_id}" psql -U transcrib -d transcrib -c "select count(*) as users from users;"
docker exec "${test_id}" psql -U transcrib -d transcrib -c "select count(*) as jobs from jobs;"
```

Критерий приёмки: restore завершается без ошибок, `goose_db_version` читается, ключевые таблицы
`users`/`jobs` существуют и запросы к ним проходят.

5. Удалить только временные объекты:

```sh
docker rm -f "${test_id}"
docker volume rm "${test_id}-pgdata"
```

Записать дату restore-test, имя дампа и результат в операторский журнал. В репо не коммитить дампы,
логи с персональными данными и секреты.
