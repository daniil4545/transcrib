# mymeet.ai API — выжимка контракта под transcrib

Рабочий справочник для реализации `internal/app/mymeet.go`. Только то, что нужно нашему сценарию: запустить бота на встречу по ссылке, дождаться обработки, забрать транскрипт по спикерам и саммари.

**Источник истины** — `docs/api/mymeet/openapi.json` (официальный OpenAPI 3.0, v1.1.0). Дополнено фактами из репозитория [github.com/MyMeetAI/API-Docs](https://github.com/MyMeetAI/API-Docs) (README + `test_api.py`) и официального MCP-сервера [github.com/MyMeetAI/mymeet-mcp-server](https://github.com/MyMeetAI/mymeet-mcp-server). Бизнес/юр/риски — `docs/api/mymeet/README.md`.

> **Вскрытие исходников MCP-сервера 2026-06-16** (`src/client.ts`, `src/types.ts`, `src/tools/*.ts`). Это рабочий TS-клиент, который реально дёргает `backend.mymeet.ai` — по фактическим формам запроса/ответа он **приоритетнее декларативного `openapi.json`**, который местами устарел. `test_api.py` из API-Docs — болванка (печатает `response.text`, не парсит, `meeting_id` не извлекает), форм ответов там нет. Существенное из MCP ниже по разделам помечено «(MCP)».
>
> Где источники расходятся: ~~формы запроса/ответа — приоритет у кода MCP-клиента~~ — **ОПРОВЕРГНУТО живым тестом (см. ниже): для `source` прав был `openapi.json` (`yandextelemost`), не MCP (`telemost`). Приоритет везде у LIVE-данных, при их отсутствии — у `openapi.json`.** Auth: `X-API-KEY` header (подтверждён кодом MCP и тестом) против deprecated `api_key` в теле/query.
>
> **🔴 Живой тест боевым ключом 2026-06-16** (curl против `backend.mymeet.ai`, ключ получен). Прощупаны все наши ручки: Submit, Poll, Fetch (report + download), Delete, List. Формы ответов теперь **подтверждены живыми ответами** (помечено «(LIVE)»), часть прежних предположений из MCP/openapi **опровергнута** (помечено «(LIVE-исправление)»). Самое важное: схема `report` другая (транскрипт в `followup_v2.chapters[].transcript[]`, не top-level), `status` и тела ошибок — **plain-text, не JSON**, свежесозданная встреча какое-то время даёт `404` на `status`. Детали — по разделам.

## 0. База и аутентификация

- **Base URL:** `https://backend.mymeet.ai/`
- **Auth:** заголовок `X-API-KEY: <key>` на каждом запросе (рекомендованный способ, работает на всех эндпоинтах). **(LIVE)** подтверждён живым тестом 2026-06-16 на всех дёрнутых ручках.
  - `api_key` в теле запроса (`record-meeting`, `video`) или в query — **deprecated**, оставлен для обратной совместимости, будет удалён. Мы сразу пишем на `X-API-KEY`.
- **Получение ключа:** только B2B, по запросу через [sales](https://mymeet.ai/contact) (`hello@mymeet.ai`). Самообслуживания нет. См. `README.md` (раздел «Статус»).
- **ToS (из спеки):** [Google Doc](https://docs.google.com/document/d/1drhWmEGdfIzwBiW_MPE2IAq6H0Nauo_51AMVb5vc89Q/edit) — отличается от публичной оферты `mymeet.ai/documents/offer-for-access`. Какой документ регулирует API-доступ — уточнить (см. `README.md` §3, ключевой риск п.7.4).

## 1. Наш flow: Submit → Poll → Fetch

Три эндпоинта закрывают сценарий transcrib. Остальное (раздел 3) — справочно.

### 1.1 Submit — запустить бота на встречу

```
POST /api/record-meeting
X-API-KEY: <key>
Content-Type: application/json
```

Тело (`Recording`):

| Поле | Тип | Обяз. | Назначение |
|---|---|---|---|
| `link` | string | да | URL встречи |
| `source` | enum | да | Платформа (см. ниже) |
| `title` | string | да | Название отчёта |
| `local_date_time` | ISO 8601 + tz | да | Локальное время, напр. `2026-04-25T15:30:00+03:00` |
| `template_name` | enum `Templates` | да | Шаблон саммари |
| `cron` | string | нет | UTC cron отложенного запуска. **Пусто = записать прямо сейчас** |
| `meeting_password` | string | нет | Пароль встречи (Zoom) |
| `participants` | string | нет | Список email через запятую для followup-рассылки |

`source` enum. **🔴 РАЗРЕШЕНО ЖИВЫМ ТЕСТОМ 2026-06-16 — для Телемоста брать `yandextelemost`, НЕ `telemost`:**
- `openapi.json` (8): `gmeet`, `zoom`, **`yandextelemost`**, `sberjazz`, `trueconf`, `konturtalk`, `msteams`, `jitsi`.
- MCP-клиент `types.ts`/`detectSource()`: `gmeet`, `zoom`, `telemost`, `sberjazz`, `trueconf`, `konturtalk`, `teams`, `jitsi`.

> 🔴 **(LIVE-исправление — ВАЖНО) Прежний вывод «брать значения MCP» ОПРОВЕРГНУТ. Правильно — `openapi.json`-значение `yandextelemost`.** Прямой тест на реальном личном Телемосте: `source:"yandextelemost"` -> бот **зашёл и пишет** (`status:recording` в `meeting/status` и `all-meetings`); `source:"telemost"` -> `record-meeting` отвечает `200 {"meeting_id"}`, **но бота НЕ назначает** -> встреча навсегда висит в `404`, в `all-meetings` не появляется. **`200` от `record-meeting` НЕ доказывает заход бота** — backend проглатывает невалидный `source` молча. Признак реального захода — встреча со `status:recording` в `all-meetings` (а не код ответа Submit). По аналогии и для Teams вероятно нужен `msteams` (openapi), не `teams` (MCP) — отдельным тестом не проверяли, но логика та же. **Вывод: для `source` источник истины — `openapi.json`, не MCP-клиент.**

Маппинг хост -> `source`. Логику определения хоста берём из MCP `detectSource()`, но **значения `source` — из `openapi.json`** (LIVE-урок: для Телемоста это критично). Нам нужны первые три:
| Хост содержит | source (openapi) |
|---|---|
| `meet.google.com` | `gmeet` |
| `zoom.us` / `zoom.com` | `zoom` |
| `telemost.yandex` | **`yandextelemost`** ← LIVE-подтверждено (НЕ `telemost`) |
| `teams.microsoft.com` / `teams.live.com` | `msteams` (openapi; MCP даёт `teams` — не тестировали) |
| `jazz.sber` / `salute.online` | `sberjazz` |
| `trueconf.` | `trueconf` |
| `konturtalk.` | `konturtalk` |
| `jitsi` / `meet.jit.si` | `jitsi` |

MCP всегда шлёт `source` (если не задан — `detectSource(link)`, fallback `gmeet`). У нас неизвестный хост -> ошибка (бот в никуда жжёт минуты), fallback `gmeet` не зеркалим.

`template_name` enum `Templates` (11): `default-meeting`, `sales-meeting`, `sales-coaching`, `hr-interview`, `research-interview`, `team-sync`, `article`, `lecture-notes`, `one-to-one`, `protocol`, `medicine`. Стартовый дефолт MVP — `default-meeting` (фиксируется конфигом, не хардкодом).

**Для MVP:** `cron` пустой (бот заходит на идущий звонок), `source` определяется из `link`, `template_name` из конфига. **`meeting_password` сознательно НЕ шлём** (откат 2026-06-17): причина незахода бота в Zoom — внешняя, не форма пароля. Эксперименты на проде: бот не входит в Zoom ни без пароля (`connecting → failed`), ни с `?pwd=`-токеном (`404`-лимбо, строго хуже), ни с **верным плоским passcode** (`connecting → failed`). Поддержка mymeet подтвердила: обновление Zoom (~13.06) блокирует ботов, чинится на их стороне (ETA ~22.06). Поле вернём, когда mymeet подтвердит правильный формат и почистит свою Zoom-логику.

Ответы: `200 OK`, `401`, `402` (лимит минут исчерпан), `409` (встреча уже запланирована).

> ✅ **(LIVE 2026-06-16) Схема ответа `200` подтверждена:** тело — плоский JSON `{"meeting_id":"<uuid>"}` (`content-type: application/json`), ровно как закладывали. `meeting_id` -> `jobs.external_job_id`. Декодим под `{"meeting_id": ...}`, пустой -> ошибка + лог сырого тела.
>
> ✅ **(LIVE) Идемпотентность `409` (закрыт вопрос §5 #2):** повторный `record-meeting` с тем же `link` -> `409`, тело — **plain-text** (`text/html`, НЕ JSON): `Meeting already scheduled <link> , meeting_id=<uuid>`. Ключ дедупа — по **`link`** (в сообщении эхо ссылки). **Существующий `meeting_id` возвращается** в тексте (`meeting_id=...`) — при `409` парсим его регуляркой из тела и переиспользуем как `external_job_id` (не плодим дубль Job). Связка `link + local_date_time` не различалась (время в обоих запросах было одинаковым), но текст ошибки апеллирует только к `link`.

### 1.2 Poll — статус обработки

```
GET /api/meeting/status?meeting_id=<uuid>
X-API-KEY: <key>
```

**🔴 (LIVE 2026-06-16) Реальный жизненный цикл — НЕ как в MCP-списке.** Наблюдённая последовательность на живой записи: `404` (scheduled, бот не зашёл) → **`connecting`** (бот подключается, LIVE 2026-06-17) → **`recording`** (бот пишет звонок) → **`processing`** (расшифровка/саммари) → **`new`** (ГОТОВО, отчёт полный, не просмотрен) → [после Fetch/просмотра] **`processed`**. Ключевые поправки к MCP-списку (`new/queued/processing/processed/failed`):
- **`connecting`** — недокументированный статус (LIVE 2026-06-17); фаза «бот подключается», ДО `recording`. Маппится в Pending; без него `mapMymeetStatus` падал на каждом тике, пока бот заходил (инцидент на первом боевом тесте: ~2 мин poll-ошибок, джоба не двигалась).
- **`recording`** — недокументированный статус, в MCP-списке его нет; это «бот вошёл и пишет».
- **🔴 `new` = ГОТОВО к Fetch, а НЕ «в очереди».** Опровергнут маппинг `new → в работе`. В `new` отчёт уже полный (`is_transcript_empty:false`, главы+транскрипт на месте — проверено). Встреча стабильно висит в `new`, сама в `processed` не уходит; `processed` = «готово И просмотрено» (демо-`upload` стало `processed` только после нашего скачивания).
- Свежесозданная встреча в `new` НЕ появляется: до записи это `404` (§1.2 выше), `new` приходит только ПОСЛЕ `processing`.

**Маппинг для `mapMymeetStatus`:** `connecting`, `recording`, `processing`, `queued` → **Pending** (в работе); **`new`, `processed` → Done (готов к Fetch)**; `failed` → Failed; **неизвестный статус → ошибка Poll** (сигнал дрейфа контракта; `recording` теперь явная ветка Pending, поэтому unknown оставляем ошибкой — не молча Pending, иначе пропустим неопознанную форму `failed`).

> ✅ **(LIVE 2026-06-16) Форма ответа — plain-text, НЕ JSON.** Опровергнуто предположение «MCP парсит как JSON». Валидная встреча -> голый текст статуса `new` (`content-type: text/html`, без кавычек, без обёртки). `Poll` читаем робастно: тело -> `TrimSpace` -> `ToLower` -> маппинг. JSON-декод НЕ применять (упадёт).
>
> ✅ **(LIVE) Ошибка — `404`, не `400` (исправление §4).** Неизвестный И невалидный (`not-a-uuid`) `meeting_id` -> **`404` plain-text** `There is no meeting with this id` (`text/html`). Backend формат UUID не валидирует, просто «не найдено». `400` для status не наблюдался.
>
> 🔴 **(LIVE — КРИТИЧНО для `orchestrator.go`) Свежесозданная встреча даёт `404` на `status`, пока бот не начал запись.** Сразу после `record-meeting` (вернул валидный `meeting_id`, `409` это подтверждает) `meeting/status` отдаёт `404 "There is no meeting with this id"` — встреча в состоянии «scheduled, бот ещё не зашёл», эндпоинты `status` И `active/all-meetings` её пока не видят. **`404` на `status` нельзя трактовать как `failed`**: это «ещё не появилась» (в работе).
>
> 🔴 **Бот, который НЕ смог зайти, статус `failed` НЕ выдаёт — встреча навсегда висит в `404`.** Подтверждено dead-link тестом: при недоступном линке `status` оставался `404` **8+ минут подряд**, перехода в `failed` не было (бот mymeet сам уходит из встречи/зала ожидания ≤5 мин, но запись о встрече остаётся в limbo, невидимой для `status`). **Вывод:** ждать `failed` для случая «бот не зашёл / не пустили» бесполезно — нужен **наш собственный watchdog-таймаут** в `orchestrator.go` (напр. N минут в `404` после Submit -> наш `failed_*` + откат резерва квоты). В `failed_*` по статусу провайдера уходим только при явном `failed`; всё остальное закрывает watchdog.
>
Ответы (LIVE): `200` (plain-text статус) | `404` (неизвестная / ещё-не-появившаяся встреча) | `401`.

Webhook'ов нет — только поллинг. Scheduler-тик `orchestrator.go` раз в 30 сек дёргает статус активных Job. `processed` → Fetch; `failed` → Job в `failed_*`.

### 1.3 Fetch — забрать результат

Два варианта. `download?format=md` — готовый markdown для Telegram-доставки, без своего рендера. `report` (структурированный JSON) — для рендера в свой Google Doc (F5, в MVP через Testing mode).

**JSON-отчёт:**
```
GET /api/video/report?meeting_id=<uuid>
X-API-KEY: <key>
```
Ответ — JSON-объект. Ответы (LIVE): `200`. `401`/`403`/`404` — не перепроверяли на report (для status/download: неизвестный id -> `404`).

> 🔴 **(LIVE 2026-06-16) Реальная схема `report` — НЕ как предполагал MCP.** Транскрипта на верхнем уровне НЕТ (опровергнуто `result.transcript`). Top-level: `feedback` (bool), `media_type` (str), **`followup_v2`** (object) — вся полезная нагрузка внутри `followup_v2`.
>
> **`followup_v2` (23 поля, существенные):**
> - **`chapters`** `[]` — транскрипт, разбитый по смысловым главам. Каждая глава: `{ "name": "<заголовок главы>", "transcript": [<сегмент>, ...] }`.
>   - **Сегмент** (LIVE): `{ "color": "#7000E0", "speaker": {"color","id","speaker":"<имя>"}, "text": "<реплика>", "timestamp": "H:MM:SS" }`.
>   - 🎯 **Наш `Result.TranscriptMarkdown`:** обойти `chapters[]` -> внутри каждой `transcript[]` -> рендер `timestamp` + `speaker.speaker` + `text`. Имя спикера — `segment.speaker.speaker`; диаризация = `speaker.id`.
> - **`speakers`** `[]` — реестр спикеров: `{color, id, speaker:"<имя>"}`. **(LIVE) Имя берётся из отображаемого имени участника звонка** (в live-тесте спикер вышел как `Daniil S.` — имя из Телемоста), а НЕ анонимное «Спикер 1». Product/privacy канон теперь явно не гарантирует обезличивание: реальные имена могут прийти от платформы встречи или поставщика.
> - **`templates`** `[]` — массив шаблонов-саммари, посчитанных mymeet при обработке (бесплатно для нас). Каждый шаблон: `{name, title, id, is_default, entities:[<entity>]}`. **Entity** = `{name, is_json (bool), text, html_text}`.
>   - 🔴 **(LIVE 2026-06-18) Имена секций И структура ПЛАВАЮТ между версиями mymeet — на имена НЕ завязываться.** Свежая встреча (`default-meeting`): entities `Overview` + `Key Topics`, а `tasks` приходит ОТДЕЛЬНЫМ шаблоном `{name:"tasks"}`. Старая демка: `summary_agenda` + `summary` + `tasks` — все три entity ВНУТРИ одного `default-meeting`. Вывод: парсить **имя-агностично** (ветвление по `is_json`), а не по именам секций/шаблонов.
>   - 🔴 **`text` имеет РАЗНЫЙ JSON-тип в зависимости от `is_json`:** при `is_json=false` — строка готового markdown (`##`-заголовки, буллеты, ссылки-таймкоды `[0:38](#00:00:38)`); при `is_json=true` (секция `tasks`) — ОБЪЕКТ `{"actions":[{"id":N,"done":bool,"name":"<задача>"}]}` (пустой `{"actions":[]}`, если задач нет). `html_text` тоже разнотипна (строка vs `[]`). В Go поле `text` -> `json.RawMessage`, НЕ `string` (иначе `Unmarshal` упадёт на tasks).
>   - 🎯 **(слайс mymeet-summary 2026-06-18) ИСПОЛЬЗУЕМ как готовое саммари поставщика** через опциональное `Result.SummaryMarkdown` (минуя наш `Summarizer`): не-json entities -> `text` как markdown; `is_json` tasks -> `actions[].name` в чек-лист. Заголовки рендерим свои (рус.). Берём `text` (markdown), `html_text` игнор.
> - `word_timings` `[]` — пословный тайминг. **(LIVE) На свежей живой записи заполнен** (40 слов на коротком звонке); элемент = `{start_time, end_time, word}` (время в секундах, float). На старом `upload`-демо был пуст. Для биллинга provider отдаёт фактический `DurationSeconds = max(end_time)`; если `word_timings` пуст, duration остаётся `0` и storage применяет общий fallback.
> - Прочее (не нужно нам): `chat` `{history,suggestions}` (AI-чат по встрече), `keywords` `[]`, `chapters`-метаданные, `amo_*` (интеграция AmoCRM), `authors/participants/date/duration/source/status/name/...`.
> - ⚠ В `followup_v2` есть своё поле `status` — у демо-встречи `new` при готовом отчёте; на семантику нашего Poll не опираемся, см. §1.2.
>
> Сырой ответ -> `jobs.raw_provider_response JSONB`.

**Готовый файл:**
```
GET /api/storage/download?meeting_id=<uuid>&format=<pdf|md|json|docx>&template_name=<...>&timezone=<...>
X-API-KEY: <key>
```
`format` по умолчанию `pdf`. `template_name` и `timezone` опциональны. Ответы (LIVE): `200`; неизвестный id -> `404`.

> ✅ **(LIVE 2026-06-16) `download?format=md` — готовый полный документ под Telegram-доставку.** `200`, `content-type: text/markdown`, ~16 КБ на 17-мин встрече. Структура: `**<Название>**` / `**Brief Summary**:` / `**Summary by topics**:` (`### <глава>` на каждую `chapter`) / `**Action items:**` (`- <задача> (**Assignee:** <имя>)`) / `**Transcript:**` (блоки `H:MM:SS **<Speaker>:**` + текст). То есть `md` уже содержит и саммари, и транскрипт по спикерам — для MVP-доставки в чат (`telegram_delivery.go`) рендер свой не нужен. Для своего Google Doc (F5) — наоборот, берём JSON `report` (см. выше) и игнорим саммари.

**Удаление встречи (152-ФЗ):**

> 🔴 **(LIVE-исправление 2026-06-16) Рабочий роут удаления — `DELETE /api/video/report?meeting_id=<uuid>`, а НЕ `DELETE /api/delete-meeting`.** `DELETE /api/delete-meeting` отдал дефолтный Flask-404 «The requested URL was not found on the server» -> **роута не существует** (MCP `client.ts` ошибается / эндпоинт не задеплоен). `DELETE /api/video/report` — реальный роут (app-level `404 "Meeting not found"` на неизвестный id, как в `openapi.json`).
>
> Это удаление **обработанной** встречи (отчёта), НЕ остановка активной записи. **(LIVE) подтверждено: `DELETE /api/video/report` на «scheduled»/активной встрече -> `404 "Meeting not found"`** (отчёта ещё нет), встреча остаётся запланированной -> **программной отмены активной записи нет** (см. §5 #4, `Cancel`).

## 2. Эскиз интеграции (внутри mymeet.go)

Реализует интерфейс `TranscriptionProvider` (`internal/app/transcription.go`). `orchestrator.go` про mymeet не знает.

```
Submit(ctx, job)        -> POST /api/record-meeting (cron="")  -> {"meeting_id"} (external_id); 409 -> парсим meeting_id из текста
Poll(ctx, externalID)   -> GET  /api/meeting/status            -> plain-text статус; 404 = ещё-не-появилась/неизвестна (НЕ failed)
Fetch(ctx, externalID)  -> GET  /api/video/report             -> followup_v2.chapters[].transcript[] -> TranscriptMarkdown; followup_v2.templates[] -> SummaryMarkdown (слайс mymeet-summary)
                       (либо GET /api/storage/download?format=md -> готовый .md для доставки в чат)
```

Маппинг статусов mymeet → наш state machine: `queued/processing/`**`recording`** → в работе; **`new`/`processed` → готов к Fetch** (LIVE: `new` = готово, не «в очереди» — §1.2); `failed` → `failed_*`. **(LIVE)** транзиентный `404` на `status` сразу после Submit → тоже «в работе», не `failed`. Неизвестная статус-строка → ошибка Poll (сигнал дрейфа), не молчаливый Pending.

## 3. Прочие эндпоинты (не в MVP-скоупе)

Справочно, на будущее. Полные сигнатуры и примеры кода — в `openapi.json`.

| Метод и путь | Назначение |
|---|---|
| `POST /api/video` | Загрузка готового аудио/видео чанками по 20 МБ (`Uploading`). Fallback для пересылки записей и отладки. Здесь есть `speakers_number` (0 = авто) |
| `POST /api/generate-new-template` | Перегенерировать саммари другим шаблоном для готовой встречи |
| `GET /api/workspaces/active/all-meetings` | Пагинированный список встреч (`page`, `perPage`). Админ-нужды |
| `PUT /api/meeting` | Переименовать встречу (`meetingId`, `newName`) |
| `PUT /api/meeting/{meetingId}/summary` | Править подраздел саммари (`templateName`, `entityName`, `newSummaryText`) |
| `POST /api/clear-transcript` / `POST /api/undo-clear-transcript` | Очистить/восстановить транскрипт |

> **(LIVE 2026-06-16) Форма `all-meetings`:** `200` JSON `{"followups":[<meeting>], "total":N}` (поле зовётся `followups`, не `meetings`). `page` **0-индексирован** (дефолт `0`; запрос `page=1` дал пустую страницу при `total:1`). `<meeting>` = `{authors:[{avatar,login}], date (RFC1123 GMT), duration (мин), id (uuid), is_transcript_empty (bool), link, locked, media_type, name, preview_image_url, source, speakers:[<имена>], status}`. Полезно для отладки (найти `meeting_id` готовой встречи без знания id).

`Entities` enum (подразделы саммари, на которые ссылается правка summary; косвенно — секции отчёта): `summary`, `summary_agenda`, `sales_general`, `sales_coach`, `hr_summary`, `questions_and_answers`, `research_insights`, `team_sync_agenda`, `summary_by_speaker`, `workshop-double`, `seo-article-double`, `one_to_one`, `med-anamnesis`, `protocol`, `template-recommendation`.

## 4. Каталог кодов ошибок

Сводно по эндпоинтам (`record-meeting` + `video` + чтение):

| Код | Значение |
|---|---|
| 400 | Невалидный запрос / `meeting_id` |
| 401 | Unauthorized (нет/неверный ключ) |
| 402 | Лимит минут исчерпан |
| 403 | Нет прав на встречу |
| 409 | Уже запланирована / уже загружена |
| 414 | Файл > 3 ГБ |
| 415 | Неподдерживаемый формат файла |
| 426 | Не хватает минут на обработку файла |
| 432 | Нужен апгрейд тарифа для файлов > 1 ГБ |
| 433 | Длительность < 30 сек |

**(LIVE 2026-06-16) Уточнения по реально наблюдённым кодам:**
- Чтение (`status`, `video/report`, `download`) на неизвестный/невалидный `meeting_id` -> **`404`** (не `400`), тело **plain-text** (`There is no meeting with this id` / `Meeting not found`), не JSON.
- `record-meeting` повтор того же `link` -> **`409`**, тело plain-text с `meeting_id` существующей задачи (§1.1).
- Несуществующий роут (напр. `DELETE /api/delete-meeting`) -> дефолтный Flask-404 (HTML «The requested URL was not found»). Отличать от app-level `404` («Meeting not found»): первый = роута нет, второй = встречи нет.
- Тела ошибок mymeet **не JSON** — наш клиент не должен пытаться их парсить как JSON.

## 5. Открытые технические вопросы

**Ключ получен, живой E2E пройден 2026-06-16 — большинство вопросов закрыто** (см. блоки «✅ Закрыто» ниже). Остаток — форма `failed`, многоголосая диаризация, эндпоинт остановки бота (письмо mymeet).

**Ответы поддержки mymeet (2026-06-16, переписка по докам, ключа ещё нет):**
- **Concurrent (#5 закрыт):** 2 параллельные встречи на аккаунт; логика «1 аккаунт = 1 тариф = 1 пользователь». **РЕШЕНО 2026-06-19:** лимит 2 — только на тест-тарифе; на API-тарифе ограничения параллельных встреч снимаются, доплаты нет (биллинг за обработанные минуты, не за пользователя). См. блок 2026-06-19 ниже.
- **Таймауты бота:** в одиночестве ≤5 мин -> выходит; в зале ожидания ≤5 мин -> выходит. (Плюс vs s2t: бот сам уходит, не виснет в постоянной комнате.)
- **Отмена активной записи (#4):** программного API нет. Способы — выгнать бота из встречи вручную (kick) / кнопка в ЛК (появится ~июль) / удалить встречу полностью. `DELETE /api/video/report` запись **НЕ** прерывает (LIVE-подтверждено). Роута `DELETE /api/delete-meeting` **не существует** (LIVE: дефолтный Flask-404) — прежний под-вопрос «прерывает ли delete-meeting» снят, такого эндпоинта нет.
- **failure_reason (#3) и схема `report` (форма):** mymeet просит скриншоты; ответ на живом тесте/в переписке.

**Ответы поддержки mymeet (2026-06-19) — главный блокер снят:**
- **Concurrent — РЕШЕНО.** Лимит 2 — только на тест-тарифе. На **API-тарифе ограничения параллельных встреч отключаются**; доплачивать не нужно — биллинг за обработанные минуты, не за пользователя/сотрудника. Снимает главное стратегическое ограничение mymeet (масштаб): провайдер годен как основной, не только пилот.
- **Брендинг бота — частично.** Полная кастомизация (лого, имя, приветствие) — на тарифе **Бизнес**. На **API-тарифе** обсуждается индивидуально, в общем доступе функции пока нет. → имя «transcrib» на боте-в-звонке через API не из коробки, нужна отдельная договорённость (на s2t — просто `bot_name`).
- **Кастомный промпт саммари — есть.** 11 готовых шаблонов + с 18.05 функция «Кастомный отчёт»: можно задавать свои промпты. Альтернатива своему OpenRouter-модулю для F3 (минус наш код, плюс привязка саммари к mymeet).
- **Zoom — ETA конец месяца.** Полное исправление обещано до конца июня (бот не входит из-за обновления Zoom ~13.06).
- **Договор — счёт-оферта.** Работают по счёту-оферте; отдельный B2B-договор — от суммы оплаты. Озабоченности «конкурирующим продуктом» (п.7.4) снова не выразили.

**✅ Закрыто живым тестом боевым ключом (2026-06-16):**
- ~~Форма ответа `200` у `record-meeting`~~ -> плоский JSON `{"meeting_id":"<uuid>"}` (§1.1, был #1).
- ~~Идемпотентность `409`~~ -> по `link`, тело plain-text с `meeting_id=<uuid>` существующей задачи (§1.1, был #2).
- ~~Схема `/api/video/report`~~ -> транскрипт в `followup_v2.chapters[].transcript[]`, сегмент `{color, speaker:{...,speaker}, text, timestamp}` (§1.3). **Опровергнут** top-level `result.transcript` из MCP.
- ~~`source` для Телемоста~~ -> **`yandextelemost`** (openapi), НЕ `telemost` (MCP). `telemost` -> `200`, но бот не заходит; `yandextelemost` -> бот зашёл и пишет (§1.1). **Источник истины для `source` — openapi, не MCP.**
- ~~Статус-список~~ -> реально приходит недокументированный **`recording`** (бот пишет звонок), §1.2.
- ~~Идемпотентность вечная~~ -> `409` действует только пока встреча активно «scheduled»; после ухода бота (~5 мин для незашедшего) тот же `link` снова даёт `200` с новым `meeting_id` (§1.1).
- ~~Poll = JSON или plain-text~~ -> **plain-text** (§1.2); тела ошибок тоже plain-text.
- ~~Cancel через `DELETE`~~ (был #4) -> **программной отмены нет**: `DELETE /api/video/report` на активной встрече -> `404`, запись не прерывается (§1.3). `DELETE /api/delete-meeting` — роута нет.
- ~~`download?format=md`~~ -> готовый полный .md (саммари + транскрипт по спикерам) для доставки в чат (§1.3).

**✅ Дополнительно закрыто live-записью реального звонка (2026-06-16, бот в личном Телемосте):**
- ~~Жизненный цикл статуса~~ -> `404 → connecting → recording → processing → new(готово) → [processed после просмотра]`; **`new` = готов к Fetch**, `connecting`/`recording` — недокументированные in-progress статусы (§1.2).
- ~~`word_timings` на свежей записи~~ -> заполняется (40 слов; `{start_time,end_time,word}`, §1.3).
- ~~Живой заход бота / диаризация (один спикер)~~ -> бот зашёл с `yandextelemost`, записал реальную русскую речь, спикер `Daniil S.` (имя из звонка). Транскрипт читаемый.

**Остаётся открытым:**
1. ~~Поле `failure_reason` в `failed`~~ -> **форма `failed` подтверждена инцидентом 2026-06-17**: голая plain-text строка `failed` за ~3 мин, БЕЗ `failure_reason` и деталей. Диагностики причины mymeet не отдаёт — почему именно `failed`, по API не узнать. **Причина именно этого инцидента — внешняя (НЕ форма пароля):** поддержка mymeet подтвердила, что обновление Zoom (~13.06) блокирует ботов/ассистентов; бот mymeet не входит в Zoom ни без пароля, ни с `?pwd=`-токеном (→ `404`-лимбо), ни с верным плоским passcode (всё → `connecting → failed`). Чинится на стороне mymeet, ETA ~22.06. Поэтому `meeting_password` сейчас не шлём (§1.1).
2. Диаризация на **многоголосом** звонке (в live-тесте говорил один человек -> один спикер). Качество разделения 2+ голосов не проверено.
3. Поведение при пустом звонке (бот зашёл, никто не говорил) — `failed`, пустой транскрипт или `new` с `is_transcript_empty:true`?

**Ранее закрытое (поддержка / MCP, не live):**
- Concurrent лимит — 2 только на тест-тарифе; **на API-тарифе снимается без доплаты** (поддержка 2026-06-19, биллинг за минуты). Прежнее «узкое место масштаба» закрыто.
- Язык транскрипции — параметра нет, только автоопределение (`RecordMeetingSchema` MCP без поля языка).
- Поминутная цена API-тарифа — 1,5 ₽/мин (12 мес) / 2 ₽/мин (6 мес), `README.md` §2.1; фиксация ставки договором — открыта.
