<div align="center">

# transcrib

**Telegram-бот, который превращает онлайн-встречу в транскрипт по спикерам и конспект с задачами.**

Прислали ссылку на созвон -> бот зашёл, записал, вернул расшифровку и конспект прямо в чат.

![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white)
![PostgreSQL](https://img.shields.io/badge/PostgreSQL-16-4169E1?logo=postgresql&logoColor=white)
![Telegram](https://img.shields.io/badge/telebot-v4-26A5E4?logo=telegram&logoColor=white)
![Docker](https://img.shields.io/badge/Docker-compose-2496ED?logo=docker&logoColor=white)
![Status](https://img.shields.io/badge/prod-v0.1.0-green)

</div>

## О проекте

transcrib подключается к встречам в Zoom, Google Meet и Яндекс Телемост, записывает разговор и присылает в Telegram два файла: расшифровку по спикерам и AI-конспект с решениями и задачами.

Сервис - оркестратор интеграций: своего ASR и ботов-участников нет, тяжёлую работу делает внешний поставщик транскрибации. Монетизация - подписка 1990 руб/мес за 15 часов через прямую ссылку ЮKassa; триал перед оплатой - 3 часа. Внутренний учёт идёт в минутах.

## Возможности

- Запись встречи через внешнего meeting-бота.
- Транскрипт по спикерам и AI-конспект.
- Доставка результата файлами в Telegram.
- Speech2Text/mymeet как сменные провайдеры транскрибации.
- OpenRouter как провайдер саммари для Speech2Text-пути.
- ЮKassa direct payment links, чек 54-ФЗ, polling статуса оплаты; автопродление включено в prod после live-gate ЮKassa.
- Панель "Мои звонки", отмена активной записи, повторная доставка.

## Технологии

| Слой | Выбор |
| --- | --- |
| Язык | Go 1.25 |
| БД / очередь | PostgreSQL 16, `FOR UPDATE SKIP LOCKED` |
| Драйвер / миграции | `pgx/v5`, `goose` |
| Telegram | `telebot.v4`, long polling |
| Контейнеры | Docker + docker-compose |
| Dev deploy | GitHub Actions -> GHCR -> Coolify |
| Prod deploy | только ручной deploy release tag |

## Быстрый старт

Нужны Docker и токен бота от [@BotFather](https://t.me/BotFather). Все команды запускаются из `src/`.

```bash
git clone git@github.com:daniil4545/transcrib.git
cd transcrib/src

cp .env.example .env
docker compose up -d
make migrate-up
make run
```

`mock`-режим не требует внешних ключей. Основные переключатели:

| Переменная | Значения | По умолчанию |
| --- | --- | --- |
| `TRANSCRIPTION_PROVIDER` | `mock`, `speech2text`, `mymeet` | `mock` |
| `SUMMARIZER_PROVIDER` | `off`, `mock`, `openrouter` | `off` |

Полный список переменных - [`src/.env.example`](src/.env.example), команды для разработки - `src/Makefile`.

## Структура

```text
transcrib/
├── docs/        # каноны: продукт, архитектура, БД, платежи, тесты
├── deploy/      # runbooks деплоя, Coolify CD, backup, проверка выката
└── src/         # Go-модуль: cmd/transcrib, internal/app, migrations
```

## Деплой

`dev` автоматически проходит GitHub Actions, публикует image в GHCR и выкатывается в Coolify service `transcrib-dev`. Prod выкатывается редко: release PR `dev -> main`, Git tag и ручной запуск `Deploy` workflow с `target=prod`. Перед prod deploy обязателен stop-gate `deploy/safe-ssh.sh coolify-prod-active-jobs`: если `active_jobs_count` не равен `0`, deploy переносится. Входящих портов у app нет: бот работает через long polling, PostgreSQL внутренний.

Операционный вход: [`deploy/README.md`](deploy/README.md). Канон инфраструктуры: [`docs/deployment.md`](docs/deployment.md).

## Документация

| Документ | О чём |
| --- | --- |
| [`docs/README.md`](docs/README.md) | Индекс канонов |
| [`docs/product.md`](docs/product.md) | Паспорт продукта и антискоуп |
| [`docs/architecture.md`](docs/architecture.md) | Runtime-поток, модули, state machine |
| [`docs/db.md`](docs/db.md) | Схема БД и транзакционные паттерны |
| [`docs/payments.md`](docs/payments.md) | ЮKassa direct payment links |
| [`docs/testing.md`](docs/testing.md) | Что и как тестируем |
| [`docs/workflow.md`](docs/workflow.md) | Процесс разработки |

## Статус

Prod работает на release tag `v0.1.0`: direct payment links, `RECURRING_PAYMENTS_ENABLED=true` и self-service кнопка отключения автопродления проверены live-smoke 2026-07-09; продуктовая аналитика в Metabase смотрит в prod через read-only datasource. Успешные live-платежи пока были owner smoke: остаются мониторинг первой оплаты не от владельца и первого реального автосписания.
