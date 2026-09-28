# Dozzle — live-viewer прод-логов transcrib

Просмотр текущих логов контейнеров transcrib из браузера через SSH tunnel. Отдельный compose-проект
`dozzle` (деплой transcrib через `git push prod` его не трогает). Dozzle не является архивом:
исторические логи Coolify prod и legacy prod/test `app` и `postgres` читаются через
`systemd-journald` и `safe-ssh.sh`.
Спека и обоснования —
[`../../docs/deployment.md`](../../docs/deployment.md).

**Статус:** запущен на проде 2026-06-22 (`amir20/dozzle:v10.6.6`, контейнер `dozzle-dozzle-1`,
порт `127.0.0.1:48080`, simple-auth). С 2026-07-01 порт закрыт от публичного доступа; смотреть
через SSH tunnel. Раздел «Установка» ниже — воспроизводимая процедура (переустановка, новый
сервер), не TODO.

**Доступ:** только через SSH tunnel, логин/пароль (Dozzle simple-auth):

```sh
ssh -N -L 48080:127.0.0.1:48080 root@203.0.113.10
open http://127.0.0.1:48080
```

Это live-viewer, не архив истории. Прямой `http://203.0.113.10:48080` должен быть недоступен.
На текущем сервере защита состоит из двух слоёв: Docker bind `127.0.0.1:48080:8080` и INPUT drop
для внешнего `tcp/48080`.

## Установка на сервере (recreate/new server)

Каталог `data/` (в `.gitignore`) создаётся на сервере руками — он untracked, поэтому переживает
деплои (как `.env`). Для уже работающего Dozzle этот раздел нужен только при переустановке или
переносе на новый сервер.

```sh
ssh root@203.0.113.10
cd /opt/transcrib/deploy/dozzle
ss -tlnp | grep -w 48080 && echo "ЗАНЯТ — выбрать другой порт" || echo "48080 свободен"
mkdir -p data

# создать пользователя. Команду выполняет владелец; пароль не хранить в history и не печатать в чат.
# Вариант ниже допустим только в operator-only shell с отключённой history на время ввода.
docker run --rm amir20/dozzle:v10.6.6 generate admin --password '<PASSWORD_FROM_OPERATOR>' --name admin > data/users.yml
chmod 600 data/users.yml

docker compose -p dozzle up -d
iptables -C INPUT ! -i lo -p tcp --dport 48080 -j DROP 2>/dev/null || \
  iptables -I INPUT ! -i lo -p tcp --dport 48080 -j DROP
```

Проверка: поднять SSH tunnel, открыть `http://127.0.0.1:48080` → форма логина → `admin` + пароль →
видны legacy prod/test контейнеры `transcrib-*`/`transcrib-dev-*`, если они запущены. Контейнеры Vexa/svyazka
отсутствовать должны (фильтр по имени контейнера `name=transcrib`).

После Coolify cutover контейнеры получили имена вида `app-<resource_uuid>` и не попадают под
`DOZZLE_FILTER=name=transcrib`. Для Coolify prod/dev использовать Coolify UI или:

```sh
deploy/safe-ssh.sh coolify-prod-app-logs 300
deploy/safe-ssh.sh coolify-prod-postgres-logs 100
deploy/safe-ssh.sh coolify-prod-migrate-logs 100
deploy/safe-ssh.sh coolify-dev-app-logs 200
deploy/safe-ssh.sh coolify-dev-postgres-logs 200
deploy/safe-ssh.sh coolify-dev-migrate-logs 200
```

Firewall/readiness check:

```sh
deploy/safe-ssh.sh dozzle-ps
deploy/safe-ssh.sh dozzle-firewall
curl --noproxy '*' -I --max-time 8 http://203.0.113.10:48080/  # должен timeout/fail
```

Не считать Dozzle полным viewer-ом Coolify-контура, пока фильтр явно не обновлён.

## Эксплуатация

```sh
docker compose -p dozzle ps                                  # статус (из этого каталога)
docker compose -p dozzle pull && docker compose -p dozzle up -d   # обновить образ
docker compose -p dozzle logs --tail 30                      # логи самого вьюера (из каталога)
docker logs --tail 30 dozzle-dozzle-1                        # то же по имени контейнера (из любого cwd)
docker compose -p dozzle down                                # остановить
```

Реальное имя контейнера — `dozzle-dozzle-1` (compose добавляет суффикс к имени сервиса).
`docker logs dozzle` упадёт «No such container»; `docker compose -p dozzle ...` работает только
из этого каталога (нужен compose-файл).

Сменить пароль: перегенерить `data/users.yml` той же командой `generate`, затем
`docker compose -p dozzle restart`.

## Важные нюансы

- **ufw не управляет этим портом.** Docker публикует порт правкой iptables в обход ufw. Поэтому
  Dozzle bind должен оставаться `127.0.0.1:48080:8080`; `ufw deny 48080` не является достаточной
  защитой.
- **Фильтр.** `DOZZLE_FILTER=name=transcrib` показывает legacy prod/test контейнеры с именами
  `transcrib-*` и `transcrib-dev-*`. Coolify prod/dev containers не попадают под этот фильтр, потому что
  их имена начинаются с `app-`/`postgres-`/`migrate-` и resource UUID. На общем боксе остаются
  соседние сервисы вроде `transcription-service`/`recording-processor`; Vexa остановлена
  2026-06-26. Они скрыты фильтром.
- **Сокет `:ro`** защищает от подмены файла сокета, но НЕ делает Docker API read-only. Реальная
  граница — Dozzle не проксирует произвольный Docker API, а управление (Actions/Shell) выключено
  по умолчанию (`DOZZLE_ENABLE_ACTIONS`/`DOZZLE_ENABLE_SHELL` не заданы). Поверхность сводится к
  «веб-авторизация → просмотр отфильтрованных логов».
