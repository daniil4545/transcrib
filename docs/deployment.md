# Деплой и инфраструктура transcrib

Канон фактов и решений по серверу. Операционные команды держим в [`../deploy/README.md`](../deploy/README.md)
и профильных runbook в `deploy/`, чтобы не плодить конкурирующие инструкции.

## Что где читать

| Нужно | Документ |
| --- | --- |
| Как катить prod/test и проверять deploy | [`../deploy/README.md`](../deploy/README.md) |
| Backup/restore-test БД | [`../deploy/backup.md`](../deploy/backup.md) |
| Логи | [`../deploy/logs.md`](../deploy/logs.md) |
| Dozzle | [`../deploy/dozzle/README.md`](../deploy/dozzle/README.md) |
| DevOps-practices и backlog hardening | [`devops-practices.md`](devops-practices.md) |

## Текущий статус

На 2026-07-07 transcrib развёрнут на общем VPS `203.0.113.10` и работает 24/7:

- prod bot: Coolify service `transcrib-prod` (`<prod-resource-uuid>`), runtime containers
  `app-<prod-resource-uuid>` and `postgres-<prod-resource-uuid>`; migrate-container может
  отсутствовать после deploy;
- dev/test bot: Coolify service `transcrib-dev` (`<dev-resource-uuid>`), контейнеры
  `app-*`, `postgres-*`, `migrate-*`; legacy `transcrib-dev-app-1` остановлен, legacy Postgres
  сохранён как fallback/source;
- prod deploy: только ручной GitHub Actions `Deploy` по release tag;
- site: `https://transcrib.example.com`, host nginx, не Docker;
- dev/prod CD: GitHub Actions + Coolify (`https://coolify.example.com`) поверх GHCR image; prod
  deploy идёт только вручную по release tag с `target=prod`;
- logs: Coolify prod `app`/`postgres` - `systemd-journald` с retention 14 дней; Coolify dev сейчас
  использует Docker `json-file` logs; legacy prod/test имеют journald-архив, пока контейнеры
  существуют; Dozzle доступен только через SSH tunnel как live-viewer для
  контейнеров, попадающих под его фильтр;
- backup: временный on-server `pg_dump` prod-БД, retention 14 дней, первый restore-test PASS.

Prod/dev Coolify revision проверять через:

```sh
deploy/safe-ssh.sh coolify-prod-health
deploy/safe-ssh.sh coolify-dev-health
```

## Сервер

| Параметр | Значение |
| --- | --- |
| Назначение | Общий VPS команды; transcrib живёт рядом с host nginx, Zabbix, svyazka и отдельным `transcription-*` стеком; Vexa остановлена 2026-06-26 |
| IP | `203.0.113.10` |
| Hostname | `msk-1-vm-qkkc` |
| Нода провайдера | `kvmnvm-629` |
| ОС | Ubuntu 24.04.4 LTS |
| Архитектура | x86_64 |
| SSH | `root@203.0.113.10`, доступ через локальный `ssh-agent` |
| Провайдер VPS/тариф | TBD |

Фактический осмотр 2026-06-17 показал, что сервер не выделенный: на нём уже работали соседние
проекты и host nginx держит 80/443. 2026-06-26 Vexa остановлена, но на боксе остаются другие
соседние сервисы, поэтому transcrib развёрнут как изолированный Docker Compose гость:
свои project names, сети, volumes, Postgres без публикации порта. Глобальные `prune`,
`down -v`, правки nginx/ufw/certbot и операции с соседними проектами запрещены без отдельного
разбора.

AI/devops-доступ выполняется через [`../deploy/safe-ssh.sh`](../deploy/safe-ssh.sh). Прямой SSH,
правка `.env` и мутации сервера остаются операторскими действиями.

## Железо

Снято по SSH 2026-06-13:

| Ресурс | Значение |
| --- | --- |
| CPU | 4 vCPU, QEMU Virtual CPU 8.2.0 @ 2.0 GHz |
| RAM | 7.8 GiB |
| Swap | 4 GiB `/swapfile` |
| GPU | Нет |
| Диск | 80 GB `/dev/sda1` |

Вывод для локальной LLM: текущий VPS слабый CPU-only и делит ресурсы с ботом/Postgres/соседями.
Полноразмерный GPU-инференс невозможен; CPU-компромисс требует чанкинга и отдельного теста качества.

## Сеть

- 80/443 держит host nginx. Caddy не используется.
- Telegram bot работает через long polling, входящие порты app не нужны.
- Postgres не опубликован наружу и доступен только внутри compose-сети.
- Исходящие порты `25/465/587/2525/389/3389/53413` закрыты провайдером; для текущего продукта не
  блокер.
- Ловушка RU-сети: IPv4-маршрут к дефолтному IP `api.telegram.org` (`149.154.166.110`) с VPS
  таймаутится. В prod/dev `app` и в самом `coolify` задан `extra_hosts` на достижимый Telegram DC
  `149.154.167.220`; без pin Coolify Telegram notifications падают по timeout.
- Ловушка OpenRouter: direct egress с Timeweb VPS (`203.0.113.10`) получает Cloudflare `403
  Access denied by security policy`. Host `mihomo.service` поднят как outbound proxy sidecar:
  `10.0.0.1:20171` HTTP, `10.0.0.1:20170` SOCKS, controller `127.0.0.1:9090`; UFW разрешает доступ к
  proxy-портам только из Docker private networks. Целевой app-config — `OPENROUTER_PROXY_URL`, не
  глобальный `HTTPS_PROXY`.
- Для Coolify Telegram notifications `APP_URL` самого Coolify должен быть
  `https://coolify.example.com`: иначе inline button URL собирается как `http://localhost` и
  Telegram notification не отправляется.
- Для Coolify Monitoring/Sentinel дополнительно нужен внутренний bind `10.0.0.1:8000:8080`:
  `coolify-sentinel` пушит данные в `host.docker.internal:8000`. Публичный `0.0.0.0:8000` не
  открывать; внешний доступ остаётся через nginx и `https://coolify.example.com`.
- Для Coolify realtime не открывать `6001/6002` наружу: `coolify-realtime` слушает loopback, а host
  nginx прокидывает websocket paths `/app/` -> `127.0.0.1:6001` и `/terminal/ws` -> `127.0.0.1:6002`.

`transcrib.example.com` обслуживает host nginx из `/var/www/transcrib.example.com`. OAuth callback Google в будущем
должен идти через этот же nginx, proxy на локальный app-порт, когда появится Google-слайс.

## Runtime stack

| Компонент | Назначение |
| --- | --- |
| Docker + Compose | Coolify-managed prod/dev app/Postgres/migrate, legacy fallback projects |
| PostgreSQL 16 | БД и очередь через `FOR UPDATE SKIP LOCKED` |
| nginx | TLS и статический сайт `transcrib.example.com` |
| Dozzle | Веб live-viewer логов prod/test контейнеров |
| Coolify | Целевой CD UI и deploy orchestrator для GitHub Actions + GHCR |
| GitHub Actions + GHCR | build/test/publish image для Coolify CD |
| systemd-journald | Целевой архив Coolify prod и legacy fallback `app`/`postgres` на 14 дней |
| mihomo | Host outbound proxy sidecar for OpenRouter when direct RU egress is blocked |

Один Go-процесс содержит Telegram long polling, HTTP server под будущий OAuth callback и
worker/scheduler. Подробная архитектура сервиса — [`architecture.md`](architecture.md).

## Deploy model

Текущая dev CD модель: GitHub Actions гоняет проверки, собирает image
`ghcr.io/daniil4545/transcrib:sha-<git-sha>`, затем строго последовательно:

1. PATCH-ит Coolify Service точным `src/docker-compose.coolify.yml` как base64
   `docker_compose_raw` с `instant_deploy=false`.
2. PATCH-ит `APP_IMAGE` на полный SHA-tag.
3. Вызывает deploy webhook ровно один раз.

Coolify Service хранит Compose внутри; репозиторный файл является единственным источником истины
для обоих контуров.
Moving tags `dev`/`prod` публикуются, но active runtime должен быть SHA-tag. `dev` выкатывается
автоматически с ветки `dev`. Prod workflow синхронизирует Compose тем же шагом, что и dev; править
Compose руками в Coolify UI нельзя — расхождение UI-версии с репозиторием уронило прод
2026-08-12 на 29 часов. Prod deploy запускается только
вручную через `workflow_dispatch` на Git tag с `target=prod`; push в `main` не выкатывает prod.
GitHub Environment `production` хранит prod webhook, required reviewers зависят от тарифа GitHub.
Runbook — [`../deploy/coolify-cd.md`](../deploy/coolify-cd.md).

Текущий prod rollout через целевой CD: tag `0.0.3.7` ->
`8940569680b43af397b59f3e9df51276f3989b02`, active image
`ghcr.io/daniil4545/transcrib:sha-8940569680b43af397b59f3e9df51276f3989b02`. Legacy prod app
остановлен, legacy Postgres volume сохранён как rollback source.

Legacy fallback: bare repo + server-side hook. Это не штатный release path.

- prod fallback: `git push prod <ref>:master` -> `/opt/transcrib` -> compose project `transcrib`;
- test fallback: `git push prod <ref>:dev` -> `/opt/transcrib-dev` -> compose project
  `transcrib-dev`.

Hook выкладывает exact accepted SHA, использует `flock`, задаёт git-SHA image tag и поднимает
compose. Общий `transcrib:latest` не используется. Это защищает prod от случайного test-образа и
даёт понятный rollback на предыдущий SHA. `.deployed-sha` означает завершённый `compose up`, а не
app-level health gate; после deploy обязательно проверять health/logs.

Релизный порядок с 2026-06-25: фича сначала выкатывается в test/dev bot, проверяется руками, затем
тот же SHA продвигается в prod. Prod-контур считается стабильным; dev-контур допускает диагностику и
эксперименты. С 2026-07-07 prod deploy имеет обязательный stop-gate: перед ручным запуском
`Deploy` workflow нужно подтвердить `deploy/safe-ssh.sh coolify-prod-active-jobs` с
`active_jobs_count=0` и чистый `coolify-prod-app-logs 300`. Если есть активные
`created|joining|recording|processing|transcribed|summarizing|awaiting_reauth` Job'ы или логи
показывают текущий звонок, deploy переносится.

Операционные команды и критерии успеха — [`../deploy/README.md`](../deploy/README.md).

## Наблюдаемость

App пишет `slog` JSON в stdout. Для Coolify prod Docker logging driver `journald` отправляет
stdout/stderr `app` и `postgres` в host `systemd-journald`; это целевой архив с retention 14 дней.
Coolify dev на 2026-07-01 фактически использует Docker `json-file`, поэтому его логи читаются через
Coolify UI или `deploy/safe-ssh.sh coolify-dev-*-logs` и пока не являются 14-дневным архивом.
Legacy prod/test тоже имеют journald-архив, пока соответствующие контейнеры существуют.
Dozzle остаётся live-viewer'ом текущих контейнеров поверх Docker API и не является архивом истории.

Расследование по времени делаем через `deploy/safe-ssh.sh`:

- `coolify-prod-app-logs 300`
- `coolify-prod-postgres-logs 100`
- `prod-app-journal "2026-06-24 00:00" "2026-06-25 00:00"` для legacy fallback
- `prod-postgres-journal "2026-06-24 00:00" "2026-06-25 00:00"` для legacy fallback
- `test-app-journal "2 hours ago"`
- `test-postgres-journal "2 hours ago"`
- `coolify-dev-app-logs 200`
- `journal-usage`

Фильтрацию по `job_id`, `user_id`, `external_job_id`, `event` делать локально: `deploy/safe-ssh.sh
coolify-prod-app-logs 300 | rg '"job_id":123'`.

Недельные агрегаты и разбор упавших джоб по прод-БД (read-only `SELECT`, без Metabase):

- `coolify-prod-stats [days]` — джобы по статусам, платежи, refund'ы, подписки, пользователи за N дней (default 7, max 90);
- `coolify-prod-failures [days]` — упавшие джобы (`failed`/`failed_join`/`failed_summary`) за N дней с обрезанным `last_error`.

`/etc/systemd/journald.conf.d/transcrib-retention.conf` установлен 2026-06-26:
`MaxRetentionSec=14day`, `SystemMaxUse=2G`, `SystemKeepFree=10G`, `Compress=yes`.

Системные read-only проверки через safe-ssh остаются отдельным набором: `system-failed`,
`docker-events`, `docker-daemon-logs`, `kernel-errors`, `oom-kills`, `cron-logs`,
`backup-status`, `backup-log`, `nginx-logs`, `disk`, `memory`.

Dozzle закрыт от публичного доступа: Docker bind `127.0.0.1:48080`, внешний `tcp/48080` закрыт INPUT firewall rule, доступ только через SSH tunnel.

`LOG_LEVEL` задаётся в env без пересборки: prod держит `info`, test/dev держит `debug` для ручной
диагностики. Даже на `debug` запрещено логировать секреты, полные токены, DSN и приватные ссылки.

Текущий режим — ручная диагностика через runbook. Следующий observability-шаг: автоматические
алерты по контейнерам/диску/backup/OOM/restart-loop и, если объём оправдает, лёгкий стек метрик
вроде Grafana/Prometheus или более простой healthcheck-агент.

## Бэкап

Временный режим: ежедневный on-server `pg_dump` prod-БД в `/var/backups/transcrib/postgres`,
retention 14 дней, guard 10 GiB, cron `03:20 MSK`. Первый restore-test прошёл 2026-06-25.

Это degraded mode: защищает от логической порчи БД и ошибочной миграции, но не от потери VPS.
Целевой следующий шаг — off-box backup и регулярный restore-test. Runbook — [`../deploy/backup.md`](../deploy/backup.md).

## Секреты и env

Формат переменных — [`../src/.env.example`](../src/.env.example). На сервере:

- prod env: Coolify-managed environment variables in service `transcrib-prod`;
- dev env: Coolify-managed environment variables in service `transcrib-dev`;
- legacy prod fallback env: `/opt/transcrib/src/.env`;
- legacy test fallback env: `/opt/transcrib-dev/src/.env`.

Файлы создаёт и правит владелец вручную, права `600`. AI-агент не читает `.env`, не печатает DSN,
не делает `docker inspect env` и `docker exec printenv`.

Prod и test никогда не должны использовать один Telegram `BOT_TOKEN`: два long polling процесса на
один токен выбивают друг друга с `409 Conflict`.

## Открытые infra/hardening задачи

Policy/backlog — [`devops-practices.md`](devops-practices.md).
Ближайшие infra-хвосты:

- вынести backup off-box;
- держать Dozzle на `127.0.0.1:48080` и проверять, что публичный `:48080` закрыт;
- перейти с root deploy на отдельного `deploy`-пользователя/forced command;
- позже добавить автоматический контроль контейнеров, `backup.log`, disk/OOM/restart-loop и zombie jobs
  или полноценный metrics stack;
- держать миграции prod в стиле expand-contract и forward-fix.

## Исторические решения

- 2026-06-17: выбран bare repo + `post-receive` вместо registry/CI как минимальный working deploy
  для одного Go-сервиса на общем VPS.
- 2026-06-22: сайт `transcrib.example.com` развёрнут на host nginx, отдельно от Docker/app deploy.
- 2026-06-24: test/reserve bot выделен в отдельный compose project `transcrib-dev`.
- 2026-06-25: prod/test разведены по git-SHA image tags; включён on-server backup; Dozzle показывает
  prod/test контейнеры фильтром `name=transcrib`.
- 2026-07-01: начат cutover CD на GitHub Actions + Coolify; DNS `coolify.example.com` указывает
  на VPS, nginx/TLS vhost включён, Coolify UI доступен через host nginx, repo получил workflows и
  Coolify compose/runbook.
- 2026-07-01: dev bot переведён на Coolify service `transcrib-dev`, GHCR pull access починен,
  legacy dev DB перенесена в Coolify Postgres, legacy `transcrib-dev-app-1` остановлен.

Текущие операции — только в `deploy/*`.
