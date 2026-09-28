# Speech2Text API — единый справочник контракта

Канонический справочник по API Speech2Text для `transcrib`. Объединяет файловый flow, Meeting API (запись встречи через бота) и Webhook API в один документ.

**Источник истины** — официальный контракт Speech2Text, сверенный 2026-06-10 и сжатый в этот справочник. Дополнительно: OpenAPI `https://api.speech2text.ru/openapi.json`, Python SDK `https://github.com/Speech2TextR/s2t-client-python`. Бизнес/юр/тарифы/риски — `docs/api/speech2text/README.md`.

> **СТАТУС (2026-06-10):** Meeting API и Webhook API теперь **задокументированы официально** (в версии PDF от 03.06 их не было — была только наша реконструкция). Sales подтвердили доработку Meeting API (релиз сегодня-завтра). Контракт ниже — по PDF; **живая проверка эндпоинтов — на тестовом доступе при первой реализации `speech2text.go`**. Прошлый файл `API_actual.md` (только файловый flow) и прежний архивный `API.md` (ручная реконструкция Meeting API) слиты сюда.

## 0. Базовый URL, хосты, аутентификация

- **Хост:** `https://api.speech2text.ru`.
- ✅ **Расхождение хостов закрыто (живая проба 2026-06-11):** `POST /api/meetings/join` отвечает идентично на обоих хостах (`api.speech2text.ru` и `speech2text.ru`). Стандартизуем `api.speech2text.ru`.
- ✅ **Ручки нашего flow существуют (живая проба 2026-06-11, без реальной встречи):** `meetings/join` (400 `meeting_url is required` на пустое тело), `recognitions/{id}`, `recognitions/{id}/result/txt`, `meetings/{id}/stop` — все отвечают валидацией, не 404-роутом. `GET /api/user/amounts` — 200 (на тестовом аккаунте 300 минут).
- ⚠ **Формат ошибок (живой факт):** JSON `{"name","message","code","status"}`. «Задача не найдена» приходит как **HTTP 400** (не 404!) с `message: "Not found recognition task"` — у `recognitions/{id}`, `result/*` и `meetings/{id}/stop`. Парсинг ошибок не должен полагаться на 404.
- ✅ **Ответы поддержки (2026-06-11):** штатное время «Очереди на запись» — **~10 минут** (дольше = инцидент); ограничений Meeting API на тестовом тарифе нет; **пароль встречи передавать отдельным полем `access_code`**, не в query `meeting_url` (Zoom `?pwd=` обязательно извлекать — реализовано в `speech2text.go`); `result/json` для meeting-задачи — тот же формат, что у файловой (сначала запись, потом расшифровка); **саммари в API не реализовано, только web-версия** (подтверждает решение отдельного `Summarizer`/OpenRouter для F3).
- ✅ **Ответы поддержки (2026-06-19, те же 5 вопросов, что mymeet):** **concurrent/тариф** — референс-тариф «24 Часа» 17 600 ₽/мес: 24 ч/сут, **3 потока одновременно из коробки**, диаризация в цене, сверх лимита 0,40 ₽/мин (~24 ₽/ч), посекундно с 15-й сек; доп. поток +100 ₽ (на тест-аккаунте было 1 поток — не потолок тарифа); **`bot_name`** — участникам на встрече видно наше имя, подтвердили официально (из коробки); **саммари** — только веб, в API нет (своё через OpenRouter); **договор** — оферта или отдельный B2B на выбор, конфликта не заявили; **зависание ботов** — заявили исправленным: бот уходит, если в комнате никого нет 10 мин. 🔴 Живой тест 2026-06-22 провален (зомби: бот не вышел из постоянной Телемост-комнаты, `code 30` >20 мин); позднее s2t снова выбран как экономически целевой prod-путь под live-risk.
- **Аутентификация** (на каждом запросе, два способа):
  - `Authorization: Bearer YOUR-API-KEY` — универсальный (meeting и webhook в примерах используют только его).
  - `?api-key=YOUR-API-KEY` в query — для `recognitions/*`.
  - У `transcrib` стандартизуем на **Bearer** во всех вызовах.
- **API-ключ** — в личном кабинете `https://api.speech2text.ru/profile-api`. Тарифы API — `https://speech2text.ru/rates-api`.

## 1. Flow для transcrib (Meeting → Poll → Fetch)

Целевой сценарий — запись онлайн-встречи. Файловый/link API (раздел 3) — для отладки и единичных случаев.

```
Submit  -> POST /api/meetings/join                 -> external_id (id), status queued
Poll    -> GET  /api/recognitions/{id}             -> status {code, value, description}
Fetch   -> GET  /api/recognitions/{id}/result/{fmt}-> транскрипт (json / txt / docx)
Stop    -> POST /api/meetings/{id}/stop            -> status cancelled (досрочно)
Webhook -> push на наш URL при смене статуса        -> страховка/замена поллинга
```

Текущий runtime получает статус polling-first через `GET /api/recognitions/{id}` в `orchestrator.go`.
Webhook `transcription.status_changed` остаётся future hardening для `web.go` (см. §6 и §11).

## 2. Meeting API — запись встречи через бота

Бот заходит на конференцию по ссылке, записывает и автоматически транскрибирует.

### 2.1 Поддерживаемые платформы (8)

| Платформа | Хост |
|---|---|
| Zoom | `zoom.us` |
| Google Meet | `meet.google.com` |
| Яндекс Телемост | `telemost.yandex.ru` |
| Сферум | `sferum.ru` |
| MTS Link | `my.mts-link.ru` |
| Bitrix24 | `bitrix24.ru/video/` |
| BigBlueButton | `bigbluebutton.ru/` |
| KTalk | `kontur.ru/talk` |

Телемост закрыт — ключевое требование ТЗ выполнено. Нам критичны Zoom / Google Meet / Телемост.

### 2.2 Запустить бота

```
POST /api/meetings/join
Authorization: Bearer YOUR_API_KEY
Content-Type: application/json
```

| Параметр | Тип | Обяз. | Описание |
|---|---|---|---|
| `meeting_url` | string | да | Ссылка на встречу |
| `title` | string | нет | Название встречи |
| `lang` | string | нет | `ru`, `en`, ... По умолчанию — из настроек аккаунта |
| `speakers` | int | нет | Количество спикеров (если известно) |
| `bot_name` | string | нет | Имя бота в конференции |
| `access_code` | string | нет | Пароль к встрече |

Ответ `201 Created`:
```json
{
  "id": "abc123def456",
  "meeting_url": "https://zoom.us/j/123456789",
  "title": "Созвон с командой",
  "lang": "ru",
  "bot_name": "Транскрибатор",
  "status": "queued",
  "created_at": "2026-05-18 10:00:00"
}
```

Ошибки: `400` (`meeting_url` не указан / невалидный URL), `400` (платформа не поддерживается), `401` (неверный/отсутствует ключ).

### 2.3 Остановить бота досрочно

```
POST /api/meetings/{id}/stop
Authorization: Bearer YOUR_API_KEY
```

Останавливает бота досрочно. Ответ `200 OK` — объект задачи (живая проба отдавала статус `cancelled`).

Ошибки: `400` (задача не найдена / уже завершена / уже отменена), `401`, `403` (нет доступа к задаче).

> Это закрывает прежний пробел «нет эндпоинта остановки бота».

> **🔴 РАЗВОРОТ КОНТРАКТА `/stop` (поддержка, 2026-06-17).** Поддержка уточнила семантику — это **досрочная финализация с транскриптом**, а НЕ чистая отмена, как считалось раньше:
> 1. **Автовыход по таймауту НЕ отменяет задачу.** Даже на пустой встрече формируется результат: задача → `done`, транскрипт строится из записанного фрагмента. (Закрывает прежний открытый вопрос: автовыход даёт транскрипт.)
> 2. **`/stop` на боте, который УЖЕ ведёт запись, финализирует фрагмент:** бот выходит из конференции, записанное уходит на транскрибацию. То есть `/stop` = досрочное завершение **с** транскриптом, не «отмена без результата».
> 3. **Зависание `code 30` (бот не заходит, минуты не списываются) — подтверждённый баг s2t, в работе.** После фикса бот выходит автоматически ~за 10 мин, как заявлено.
>
> **Следствие для кода (обновлено 2026-07-03):** manual stop и watchdog stop для уже пишущей s2t-задачи больше не считаются refund-сценарием. Код ставит marker `manual_stop|watchdog_stop`, вызывает `/stop`, переводит Job в `processing` и ждёт результат. Если итоговый provider-статус после `/stop` приходит как `cancelled`, worker всё равно пробует `Fetch` только при stop-marker. Непустой transcript считается оказанной услугой и идёт в обычный terminal billing; пустой transcript или невозможность получить результат ведут в `failed` с возвратом резерва.

### 2.4 Отслеживание статуса meeting-задачи

Тот же эндпоинт, что и для файлов:
```
GET /api/recognitions/{id}
Authorization: Bearer YOUR_API_KEY
```

Объект задачи (поля — см. §4). У встреч `resource.type = "link"`, `resource.url` = ссылка на встречу. `status` — объект `{code, value, description}`.

### 2.5 Статусы встречи (`status.value` / `status.code`)

| value | code | Описание |
|---|---|---|
| `queued` | 0 | Задание создано |
| `queued` | 30 | Бот в очереди на запись (`description: "В очереди на запись"`, живой факт 2026-06-15) |
| `queued` | 30 | Бот присоединился к встрече |
| `queued` | 30 | Бот ведёт запись встречи |
| `paused` | 102 | Приостановлено: лимит доступных минут исчерпан |
| `done` | 200 | Распознавание прошло успешно |
| `done` | 204 | Завершено: речь не обнаружена |
| `error` | 404 | Бот не смог присоединиться к встрече |
| `error` | 406 | Некорректный формат файла |
| `error` | 407 | Превышен лимит длительности |
| `error` | 501 | Ошибка при распознавании |
| `error` | 502 | Ошибка загрузки из внешнего источника |
| `cancelled` | — | Остановлено через `/stop` (§2.3) |

`code` 30 повторяется для ТРЁХ под-состояний (в очереди на запись / присоединился / ведёт запись) — различать по `description`. Для нашего state machine: `queued/code=0` и `queued/code=30 "В очереди на запись"` → `Pending`/`joining`; `queued/code=30 "Бот присоединился..."` или `"Бот ведёт запись..."` → `OnCall`/`recording`; `done` → Fetch; `error`/`cancelled` → `failed_*`, кроме `cancelled` после нашего `/stop` с marker `manual_stop|watchdog_stop`: тогда сначала пробуем `Fetch`; `paused` → нехватка минут (см. §7). Незнакомый `description` при `code=30` считаем дрейфом контракта и оставляем как ошибку Poll, чтобы не показать ложную запись.

> **Инцидент «зависшие воркеры записи» (повтор 2026-06-15, как 11 июня).** Задачи Телемоста застревали в `code 30 / "В очереди на запись"` и не двигались часами; `/api/user/amounts` показывал `used: 0` из 300 (бот не записал ни секунды) — бот ни разу не зашёл на встречу. Это НЕ наш баг и НЕ нехватка минут: s2t принимает задачу, но не диспатчит бота из очереди записи. 11 июня лечилось перезапуском воркеров на стороне s2t. Симптом для диагностики: задача висит `queued/30 "В очереди на запись"` + `amounts.used` не растёт. Live-примеры: `l80ndH2qLDNELXRNxDLwuSnYb-OmBDja`, ранее `_IN2JjDhnzJZqfDSIzYmjoBY3jQzwaT5`.

> **🔴 Инцидент «бот не выходит из постоянной комнаты» (LIVE 2026-06-17, признан s2t).** Отличен от «не зашёл»: здесь бот зашёл и пишет (`code 30 "Бот ведёт запись встречи"`, минуты идут), но после выхода юзера из Телемост-комнаты `/j/<id>` **не выходит сам** — комната постоянная, не закрывается, признака «встреча завершена» нет, и автовыход (§2.3 п.1) не срабатывает. Задача висит в `code 30` неограниченно; теперь её добивает наш watchdog (`JOB_MAX_AGE_HOURS`, от `created_at`): вызывает `POST /stop`, ждёт transcript и списывает фактическую duration при непустом результате; refund остаётся только для no-result. **s2t признали баг и обещали починить** (тикет 2026-06-17). Live-пример: задача `i57jv02UkZ3RnUo_t9rAP4UHxsgpdW6g` (Телемост, висела `code 30` после выхода юзера). Обходной путь до фикса — `POST /stop` финализирует с транскриптом (§2.3 п.2). Следствие: §2.3 п.1 «автовыход даёт `done`» **не покрывает постоянные комнаты**, которые не сигналят «завершено».

> **🔴 Исторический инцидент — фикс не работал (LIVE 2026-06-22).** После заявленного s2t фикса автовыхода (2026-06-19: «бот уходит, если в комнате никого 10 мин») проведён живой ретест на проде: постоянная Телемост-комната, юзер завершил звонок -> задача зависла в `code 30 "Бот ведет запись встречи"`, `result: null` >20 мин (автовыход не сработал). Прямой `GET /api/recognitions/{id}` подтвердил. На тот момент prod был временно откатан на mymeet. zombie-risk закрывается нашим watchdog/stop-flow, а не доверием к автовыходу provider'а.

### 2.6 Результат встречи

После `done` (code 200) в объекте задачи появляется `result` со ссылками на форматы:
```json
"result": {
  "raw":  ".../api/recognitions/{id}/result/raw",
  "txt":  ".../api/recognitions/{id}/result/txt",
  "srt":  ".../result/srt", "vtt": ".../result/vtt",
  "json": ".../result/json", "xml": ".../result/xml"
}
```
Можно ходить по этим URL или напрямую `GET /api/recognitions/{id}/result/{format}` (§5).

**Срок доступности результата:** точный TTL в документации API не указан. В prod-инциденте
2026-07-14 запрос результата примерно через 7 суток вернул HTTP 410 «время жизни результата
истекло». Поэтому продуктовый контракт считает ссылки `result/*` доступными не дольше недели.
Указанный в privacy policy срок до 90 календарных дней относится к обработке аудиоданных и не
гарантирует доступность результата через API.

Встреча с паролем: передать `access_code` при создании; иначе задача завершится `error`.

## 3. Файловый и link API (отладка / fallback)

В архитектуре transcrib почти не нужны (звонки приходят через Meeting API), но полезны для отладки и единичных загрузок.

### 3.1 Загрузка файла

```
POST /api/recognitions/task/file
Content-Type: multipart/form-data
```

Поля: `file` (обяз.), `lang`, `speakers`, `min_speakers`/`max_speakers`, `multi_channel` (`1` — стерео, по каналу на спикера; обычно записи АТС).

Допустимые расширения (расширенный список из нового PDF): `3gp, 3gpp, 7z, aac, adts, amr, avi, awb, caf, dav, f4v, flv, m4a, m4p, m4v, mkv, mov, mp3, mp4, mpeg, mpeg4, mpg, mpga, mts, mxf, oga, ogg, opus, rar, ts, wav, webm, wma, wmv, zip`.

### 3.2 Загрузка по ссылке

```
POST /api/recognitions/task/link
Content-Type: application/json
{"lang": "ru", "url": "https://example.com/audio.mp3"}
```
Параметры те же. Поддерживаются и внешние источники (в примере PDF — YouTube Shorts).

### 3.3 Объект задачи (ответ file/link)

```json
{
  "id": "y8mNppLfE9p9IWZISs-ZymkSoD48Gyxx",
  "created": null,
  "file_source_id": "19606360",
  "options": {"lang": "ru", "speakers": null, "multi_channel": null},
  "file_meta": {"mime": "audio/mpeg", "format": "MPEG Audio", "audio_format": "MPEG Audio", "channels": 1, "duration": "00:00:59"},
  "resource": {"type": "file", "name": "mp3test_dialog.mp3"},
  "status": {"code": 100, "description": "В очереди на распознание"},
  "payment": {"source": 1, "price": 0},
  "result": null
}
```
У link-задачи `resource.type = "link"`, `resource.url`, начальный `status.code` = 0 («Задание создано»). `file_meta` может быть `[]`, пока файл не обработан.

## 4. Проверка статуса и схема объекта задачи

```
GET /api/recognitions/{TASK-ID}
```

Возвращает объект задачи. Описание полей:

| Поле | Тип | Описание |
|---|---|---|
| `id` | string | Идентификатор задачи (хэш) |
| `created` | string | Дата/время создания (UTC) |
| `file_source_id` | int | Внутренний ID источника |
| `options.lang` | string | Язык распознавания |
| `options.speakers` | int\|null | Кол-во спикеров (null — авто) |
| `options.multi_channel` | int | Многоканальный режим (0 — выкл) |
| `file_meta` | object\|array | Метаданные файла (mime, format, channels, duration); `[]` пока не обработан |
| `resource.type` | string | `file` или `link` (встречи — `link`) |
| `resource.url` / `.name` | string | URL встречи / имя файла |
| `status` | object | Текущий статус (см. ниже) |
| `payment.source` | int | Источник оплаты: 1 — тарифный пакет, 2 — списание с лицевого счёта |
| `payment.price` | float | Стоимость задачи |
| `result` | object\|null | Ссылки на форматы результата (после `done`) |

**Числовые статусы файлового/link API** (поле `status.code`, без `value`):

| code | Описание |
|---|---|
| 0 | Задание создано |
| 80 | Контент получен |
| 100 | Процесс распознавания / в очереди |
| 200 | Распознавание прошло успешно |
| 501 | Ошибка при распознавании |

У Meeting API `status` дополнительно содержит текстовый `value` (§2.5). Сводка соответствия — §9.

✅ **Живая форма объекта задачи (зафиксирована 2026-06-11, meeting-задача на Телемост):**
```json
{"id":"J_aWF...","created":"2026-06-11 13:21:31","file_source_id":22627675,
 "options":{"lang":null,"speakers":null,"multi_channel":0},"file_meta":[],
 "resource":{"type":"link","url":"https://telemost.yandex.ru/j/..."},
 "status":{"code":30,"value":"queued","description":"В очереди на запись"},
 "payment":{"source":1,"price":0},"result":null}
```
Совпадает с таблицей выше. ⚠ Нюанс против PDF: `code 30` у живого API может означать «В очереди на запись» (бот ещё НЕ на встрече), а не «бот присоединился» из §2.5. Поэтому `speech2text.go` различает `code=30` по `description`: очередь остаётся `Pending`, присоединение/запись переходят в `OnCall`.

## 5. Получение результата

```
GET /api/recognitions/{TASK-ID}/result/{FORMAT}
```

Форматы: `raw`, `txt`, `srt`, `vtt`, `json`, `xml`, `plain-txt`, `plain-txt-no-speaker`, `docx`.

Для transcrib основной — `json` (сегменты со спикерами и таймкодами) или `txt` (готовая строка «Спикер N: hh:mm:ss — текст»). Выбор формата для склейки в Google Doc / `.md` решаем при разработке `google.go`.

✅ **Структура `result/json` зафиксирована живым тестом 2026-06-11** (meeting-задача, Телемост; сырой ответ сохраняется в `jobs.raw_provider_response JSONB`):
```json
{
  "chunks":   [ {"text": "...", "time": {"from": 301.3, "to": 302.7}, "speaker": 4} ],
  "speakers": [ {"id": 4, "name": "Спикер 3"} ],
  "languages": ["en"]
}
```
- `chunks[]` — сегменты: `text` (строка), `time.from`/`time.to` (секунды, float), `speaker` (int id → справочник `speakers`).
- `speakers[]` — справочник: `id` + генерируемое имя «Спикер N» (реальных имён нет — подтверждает антискоуп `product.md` §6).
- `languages[]` — определённые языки.
- ⚠ Поля `confidence` нет. ⚠ На записи тишины ASR галлюцинирует короткие «You.»-сегменты с `languages: ["en"]` и плодит фантомных спикеров (8 «спикеров» на пустой встрече) — оценивать диаризацию можно только на реальной речи.

## 6. Webhook API

Базовый URL: `/api/webhook`. Авторизация: `Authorization: Bearer {api_key}`. Один клиент = один webhook.

| Метод | Назначение |
|---|---|
| `POST /api/webhook/create` | Создать. Тело: `url` (обяз.), `is_enabled` (по умолч. true), `events` (непустой массив) |
| `GET /api/webhook/get` | Получить webhook текущего клиента |
| `POST /api/webhook/update` | Обновить (любое подмножество полей) |
| `POST /api/webhook/delete` | Удалить (тело не нужно) |

Единственное событие: `transcription.status_changed` (при любой смене статуса).

Ответ create/get/update — `{webhook_uid, url, is_enabled, events}`. Ошибки `400`: `URL is required`, `Invalid URL format`, `Events array is required and must not be empty`, `Webhook already exists`, `Client not found`, `Webhook not found`, `At least one field must be provided for update`, `URL cannot be empty`. Удалённый webhook можно создать заново, но нельзя обновить.

### 6.1 Входящий payload (Speech2Text → наш URL)

```json
{
  "event": "transcription.status_changed",
  "timestamp": "2026-03-23T10:15:23Z",
  "transcription": {
    "id": "a1b2c3d4-...",
    "status": "done",
    "previous_status": "processing",
    "error_message": null
  },
  "secret_token": "your-secret-token",
  "webhook_uid": "550e8400-e29b-41d4-a716-446655440000"
}
```

`transcription.status` — текстовые статусы: `created`, `processing`, `paused`, `done`, `error`, `deleted`.

> **Важно (закрыт старый вопрос):** в PDF явно — `secret_token` «будет подставлен **ваш ключ доступа к API**». То есть отдельного секрета нет: сравниваем пришедший `secret_token` со своим API-ключом (constant-time, `subtle.ConstantTimeCompare`), не совпал — `403`. TLS обязателен через текущий nginx edge. Это слабее HMAC: токен в payload — кто его узнал, может слать поддельные события. Хранить рядом с секретами.

### 6.2 Ретраи и обработка

Наш endpoint должен вернуть HTTP 2xx. Иначе — ретраи: через 5 / 10 / 15 / 20 минут, 5-я попытка финальная → статус доставки `failed`.

Следствие для архитектуры: handler идемпотентен (одно событие до 5 раз); quick-ack (парсим → валидируем `secret_token` → пишем в БД-очередь → 200); поллинг-страховка в `orchestrator.go` добивает статус через `GET /api/recognitions/{id}`, если webhook не дошёл.

## 7. Лимиты минут и оплата с лицевого счёта

```
GET /api/user/amounts
```
Ответ: `{"minutes": {"available": 360, "used": 0}, "amount": 0}` — остаток минут по тарифу и баланс лицевого счёта.

При нехватке минут задача переходит в `paused` (code 102). Тогда — оплата с лицевого счёта:
```
GET   /api/recognitions/{TASK-ID}/price   # {"duration":1.9,"pricePerMinute":0.4,"price":0.76}
PATCH /api/recognitions/{TASK-ID}/pay     # списать и возобновить
```

`pricePerMinute` в примере PDF — `0.4 ₽/мин` (вероятно ставка тестового доступа; боевая — по тарифу `rates-api`). Следствие: `billing.go` прогнозирует исчерпание квоты через `/user/amounts`; на `paused` — либо подложить деньги на лицевой счёт, либо остановить Job с понятной ошибкой.

## 8. Удаление и список задач

```
DELETE /api/recognitions/{TASK-ID}
```
⚠ Только пока задача **в очереди**, не в работе. После `processing`/`done` программно стереть аудио/транскрипт нельзя.

Следствие для 152-ФЗ: «удаляем аудио после успеха» автоматически не выполняется. Либо просим у поддержки отдельный эндпоинт удаления после `done`, либо учитываем, что privacy policy допускает обработку аудиоданных до 90 календарных дней. Этот срок не гарантирует доступность результата через API; продуктовый TTL результата описан в §2.6.

```
GET /api/recognitions?page=PAGE&per-page=LIMIT   # page≥1, per-page 10..1000 (умолч. 50)
GET /api/recognitions/processing | /complete | /errors
```
Нужны в основном для админ-скриптов; production-state хранится у нас в Postgres.

## 9. Сводка статусов (file vs meeting)

| Числовой (file, `status.code`) | Текстовый (meeting, `status.value`) | Смысл |
|---|---|---|
| 0 | `queued` (0) | Задание создано |
| 80 | — | Контент получен |
| 100 | `queued` (30) | В обработке / бот присоединился / ведёт запись |
| 200 | `done` (200) | Успешно |
| — | `done` (204) | Завершено, речь не обнаружена |
| 501 | `error` (501) | Ошибка распознавания |
| — | `error` (404/406/407/502) | Не присоединился / формат / лимит длительности / загрузка из источника |
| — | `paused` (102) | Нехватка минут |
| — | `cancelled` | Остановлено через `/stop` |

Webhook отдаёт текстовые статусы (`created`/`processing`/`paused`/`done`/`error`/`deleted`). Точное соответствие числовых и текстовых кодов — подтвердить эмпирически при первой реализации.

## 10. Что НЕ покрыто документацией (follow-up к поддержке)

Большинство прежних пробелов закрыто новым PDF (stop-эндпоинт, статусы, secret_token, схемы ответов). Осталось:

- **Внутренняя структура `result/json`** (поля сегментов: спикер, текст, таймкоды, confidence) — узнаём живым тестом.
- **Удаление задачи после `done`** (для 152-ФЗ) — `DELETE` работает только в очереди.
- **Таймаут одиночества бота** на встрече, поведение в waiting room.
- **Эндпоинт «бот сейчас на встрече или нет»** — отдельного нет, но статус `queued`/30 «ведёт запись» это покрывает.
- **Лимит одновременных потоков** сверх тарифа — на уровне тарифа, не API.
- **SLA, DPA, изоляция данных между клиентами** (после инцидента в отзывах — `README.md`, раздел «Юридическая часть»).
- **Расхождение хоста** `api.speech2text.ru` vs `speech2text.ru` в meeting/webhook (§0).

## 11. Эскиз speech2text.go (резервная реализация)

Реализует `TranscriptionProvider` (`internal/app/transcription.go`). Speech2Text — экономически целевой поставщик и текущий prod/dev provider; live-risk постоянных комнат закрывается нашим watchdog/stop-flow. `mymeet.go` остаётся рабочим fallback за тем же интерфейсом. На walking-skeleton реальный провайдер заменяется `mock.go` (`TRANSCRIPTION_PROVIDER=mock`).

```
Submit(ctx, job)        -> POST /api/meetings/join                 -> external_id (id)
Poll(ctx, externalID)   -> GET  /api/recognitions/{id}             -> status.value -> наш Status
Fetch(ctx, externalID)  -> GET  /api/recognitions/{id}/result/json -> транскрипт + спикеры
Cancel(ctx, externalID) -> POST /api/meetings/{id}/stop            -> финализация фрагмента с транскриптом (§2.3; billable при непустом transcript)
EnsureWebhook(ctx, url) -> get -> create/update (идемпотентно)
Amounts(ctx)            -> GET  /api/user/amounts                  -> прогноз квоты
```
`domain.Job`: `ExternalJobID` (id S2T), `Status` (маппинг их статусов в наш state machine); пароль встречи (`access_code`) извлекается из query-параметра `pwd` ссылки через `meetingAccessCode` — отдельного поля `MeetingPassword` в `Job` нет.

> **Статус реализации:** в `speech2text.go` реализованы только `Submit`/`Poll`/`Fetch`/`Cancel` (текущий интерфейс `TranscriptionProvider`). `EnsureWebhook`/`Amounts` из эскиза выше — **НЕ реализованы** (webhook-приём §6 и прогноз квоты §7 = план Этап 3, `web.go` пока нет).

## 12. Примеры кода

В PDF — готовые примеры на PHP / Python / JavaScript для файлового flow (стр. 22-24). Для Go нужны как референс контракта, не как код. Официальный Python SDK: `https://github.com/Speech2TextR/s2t-client-python`. Коды языков для `lang` — Приложение 1 в PDF.
