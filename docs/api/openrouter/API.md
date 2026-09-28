# OpenRouter API — выжимка контракта под transcrib

Рабочий справочник для реализации `internal/app/openrouter.go` (реализация интерфейса `Summarizer`, `summary.go`). Только то, что нужно нашему сценарию: один синхронный вызов chat-completion, который превращает готовый markdown-транскрипт в markdown-конспект (F3). Бизнес/юр/экономика — `docs/api/openrouter/README.md`.

**Источник истины** — официальная документация [openrouter.ai/docs](https://openrouter.ai/docs/api/reference/overview). Контракт-критичные и числовые факты прошли состязательную верификацию, часть подтверждена живым `curl` к API (рой агентов 2026-06-22). Маркеры: «(LIVE)» — подтверждено вызовом API; «(DOCS)» — из документации; «(⚠ непроверено)» — в доках шаблонная переменная/не отрендерено.

OpenRouter **OpenAI-совместим**: схемы запроса/ответа повторяют OpenAI Chat API с небольшими отличиями (помечены ниже).

## 0. База и аутентификация

- **Base URL:** `https://openrouter.ai/api/v1` (LIVE)
- **Auth:** заголовок `Authorization: Bearer <OPENROUTER_API_KEY>` на каждом запросе (LIVE). Ключ — только в заголовке, в логи и тексты ошибок не попадает (как в `speech2text.go`).
- **Content-Type:** `application/json`.
- **Опциональная атрибуция (косметика, на саммари не влияет):** `HTTP-Referer: <url>` и `X-Title: <app>` (канонично `X-OpenRouter-Title`, `X-Title` — алиас) — только ранжирование приложения на openrouter.ai/rankings. Не маршрутизация, не приватность. **Можно не слать.**
- **Ключ:** самообслуживание на openrouter.ai, предоплаченные кредиты (prepaid). B2B/модерация не нужны.

## 1. Наш flow: один вызов `chat/completions`

Один эндпоинт закрывает сценарий саммари. Для длинного транскрипта — map-reduce поверх того же вызова (§4).

### 1.1 Request

```
POST /api/v1/chat/completions
Authorization: Bearer <key>
Content-Type: application/json
```

Тело (минимум под наш сценарий):

| Поле | Тип | Обяз. | Назначение |
|---|---|---|---|
| `model` | string | да | Slug модели, напр. `google/gemini-2.5-flash` (точный slug критичен; см. §3) |
| `messages` | array | да | `[{role, content}]`; для нас `system` (промпт `summary_prompt.txt`) + `user` (транскрипт) |
| `max_tokens` | int | нет | Потолок длины ответа (алиас `max_completion_tokens`) |
| `temperature` | float | нет | 0.0–2.0, дефолт 1.0. Для конспекта ставим низкую (0.2–0.3) — детерминизм |
| `stream` | bool | нет | Дефолт `false` — наш случай (синхронный ответ) |

Сообщение: `{"role": "system"|"user"|"assistant", "content": "<text>"}`. Нам хватает строкового `content`.

Пример:
```json
{
  "model": "google/gemini-2.5-flash",
  "messages": [
    {"role": "system", "content": "<содержимое summary_prompt.txt>"},
    {"role": "user", "content": "<markdown-транскрипт>"}
  ],
  "temperature": 0.3,
  "max_tokens": 4096
}
```

> ⚠ **Ловушка Go `omitempty` + `temperature: 0`.** Если поле `Temperature float64` помечено `json:",omitempty"`, значение `0.0` НЕ сериализуется → API применит дефолт `1.0` (недетерминированно). Если нужен `temperature: 0`, поле слать без `omitempty` (или отдельным указателем). Мы используем 0.3 — для нас не стреляет, но в коде помечаем.

### 1.2 Response (синхронный успех)

```json
{
  "id": "gen-...",
  "model": "google/gemini-2.5-flash",
  "choices": [
    {
      "index": 0,
      "finish_reason": "stop",
      "message": {"role": "assistant", "content": "# Конспект встречи..."}
    }
  ],
  "usage": {"prompt_tokens": 312, "completion_tokens": 187, "total_tokens": 499}
}
```

- 🎯 **Наш результат — `choices[0].message.content`** (markdown конспекта).
- `model` в ответе — РЕАЛЬНО использованная модель (может отличаться от запрошенной при фолбэке маршрутизации). (DOCS)
- `usage` — для лога стоимости; в Go держать nil-safe (отдельные провайдеры могут не вернуть). (DOCS)

`finish_reason` (LIVE/DOCS):

| Значение | Смысл для нас |
|---|---|
| `stop` | Норма — модель завершила сама. Ожидаемое. |
| `length` | **Обрезано** по `max_tokens` — конспект неполный. Поднять `max_tokens` или флаг ошибки. |
| `content_filter` | Заблокировано модерацией. |
| `error` | Ошибка обработки (см. ниже про 200+error). |
| `tool_calls` | Не наш сценарий (инструментов не шлём). |

Для саммари: любой `finish_reason != "stop"` — трактуем как сбой (наш контракт `Summarize` → ошибка → `failed_summary`).

> ⚠ **(DOCS) `200 OK` может нести ошибку в теле.** В синхронном ответе ошибка апстрима приходит как `choices[0].finish_reason == "error"` + `choices[0].error` при HTTP 200. Поэтому проверять не только HTTP-код, но и `finish_reason`/наличие `content`. Пустой `content` при `finish_reason != "stop"` → ошибка.

## 2. Коды ошибок и тело

**Тело ошибки (DOCS):** `{"error": {"code": <int>, "message": <string>, "metadata": {...}}}` — `code` дублирует HTTP-статус числом; `metadata` опциональна (для 403-модерации: `reasons[]`, `flagged_input`; для 502: `provider_name`). Тело — для диагноза в тексте нашей ошибки (как `s2tHTTPError`); ключ в нём не светится (он в заголовке).

| Код | Значение | Ретрай |
|---|---|---|
| 400 | Невалидный запрос; **в т.ч. превышен контекст** | Нет — чинить payload (для контекста → чанкинг, §4) |
| 401 | Неверный/просроченный ключ | Нет |
| 402 | Кончились кредиты | Нет — пополнить |
| 403 | Модерация / нет прав | Нет |
| 408 | Таймаут запроса | Да (backoff) |
| 429 | Rate limit | Да (backoff; `Retry-After` best-effort) |
| 500 / 502 / 503 / 504 | Внутр. ошибка / модель недоступна / нет провайдера / таймаут шлюза | Да (backoff) |

> Также возможны 404 / 413 / 422 (DOCS) — редки для нашего payload, обрабатываются общей веткой «не-2xx → ошибка».
>
> ⚠ `Retry-After` заявлен на 429 и 503 (errors-doc), но в limits-doc не задокументирован → **best-effort**: парсим если есть, иначе фиксированный backoff.
>
> ⚠ **(непроверено)** Числа лимитов free-моделей и платёжная комиссия в доках — JS-шаблоны, конкретных значений нет; в код/расчёты не закладываем. Платные модели жёстких публичных RPM не имеют (лимит глобальный на аккаунт, лишние ключи не помогают).

**Контракт `Summarize`:** оркестратор уводит ошибку `Summarize` в `failed_summary`, но сначала доставляет transcript-only файл и объясняет, что не сформировался именно конспект. OpenRouter-клиент перед этим делает bounded retry внутри каждого `chat`-шага: transient HTTP `408/429/5xx`, transient `error.metadata.error_type`, `Retry-After` best-effort, request/read/decode сбои и известная сигнатура `JSON error injected into SSE stream`. Permanent `400/401/402/403` и permanent `error_type` не ретраятся.

## 3. Модели

Числа — LIVE `GET /api/v1/models`, 2026-06-22 (полная таблица и экономика — `README.md`):

| Slug | Контекст | Max output | $in/1M | $out/1M |
|---|---|---|---|---|
| `google/gemini-2.5-flash` (дефолт) | 1 048 576 | 65 535 | $0.30 | $2.50 |
| `openai/gpt-4o-mini` | 128 000 | 16 384 | $0.15 | $0.60 |
| `deepseek/deepseek-chat` | 131 072 | 16 000 | $0.20 | $0.80 |

> Slug должен быть точным — `anthropic/claude-sonnet-4.5` (через точку), не `-4-5`; preview-варианты gemini на OpenRouter недоступны, брать GA-slug `google/gemini-2.5-flash`.
>
> **Thinking у gemini-2.5-flash:** включён по умолчанию, токены рассуждения тарифицируются как output ($2.50/1M). Управление — вложенным объектом `reasoning` (НЕ `max_tokens_for_reasoning`): бюджет `{"reasoning": {"max_tokens": N}}`, выключить `{"reasoning": {"effort": "none"}}`. Для саммари — настроить на живом транскрипте (цена/латентность vs качество); v1 оставляем дефолт.
>
> Обнаружение моделей программно — `GET /api/v1/models` (поля `id`, `context_length`, `top_provider.max_completion_tokens`, `pricing.prompt`/`pricing.completion` — per-token строкой, ×1 000 000 для цены за 1M). Routing-суффиксы `:floor` (дешёвый провайдер) / `:nitro` (макс. throughput).

## 4. Эскиз интеграции (`openrouter.go`)

Реализует `Summarizer` (`summary.go`). `orchestrator.go` про OpenRouter не знает — видит интерфейс. Зеркалит стиль `speech2text.go` (raw `net/http`, свой `*http.Client`, `io.LimitReader` на тело, ключ в заголовке). Без SDK и новых зависимостей.

```
NewOpenRouterSummarizer(apiKey, baseURL, model, proxyURL) -> промпты из summary_prompt.txt + summary_map_prompt.txt (go:embed)

Summarize(ctx, transcript) (markdown, error):
  if len(transcript) <= maxChunkChars:                 // короткая встреча — один вызов
      return chat(ctx, defaultSummaryPrompt, transcript)
  // map-reduce (длинная встреча):
  chunks := splitByLines(transcript, maxChunkChars)    // режем по строкам, не рвём реплики
  partials := for each chunk: chat(ctx, mapPrompt, chunk)   // сжать фрагмент
  return chat(ctx, defaultSummaryPrompt, join(partials))    // свести в финальный конспект

chat(ctx, system, user):
  до 3 попыток внутри одного map/reduce шага, ctx ограничивает весь цикл
  POST /chat/completions {model, messages:[system,user], temperature:0.3, max_tokens}
  non-2xx          -> transient HTTP ретраится, permanent HTTP сразу ошибка
  choices[].error / top-level error -> transient error_type/code/message ретраится
  finish_reason!=stop / пустой content -> ошибка
  return choices[0].message.content
```

Решения слайса (под выбор владельца 2026-06-22):
- **Модель** — `google/gemini-2.5-flash` дефолтом, env `OPENROUTER_MODEL` (свап строкой).
- **Proxy** — опциональный env `OPENROUTER_PROXY_URL`; применяется только к `OpenRouterSummarizer`
  через отдельный `http.Transport.Proxy`. Глобальный `HTTPS_PROXY` не является целевым решением:
  он зацепит Telegram, Speech2Text и ЮKassa.
- **Чанкинг** — map-reduce **по порогу размера** (`maxChunkChars`), не по лимиту контекста: при 1M-контексте gemini лимитный триггер не сработал бы никогда (мёртвый код). Короткие встречи → один вызов; длинные дробятся. Map-промпт — отдельный embed `summary_map_prompt.txt` (сжать фрагмент); reduce — существующий `summary_prompt.txt`.
- **Таймаут.** Текущий `summaryTimeout = 180s` (`orchestrator.go`) оборачивает весь `Summarize`. Map-reduce делает N+1 последовательных вызовов, поэтому таймаут должен покрывать весь цикл, а не один HTTP-вызов.
- **Retry.** Ретрай находится внутри `chat`, а не вокруг всего `Summarize`: при map-reduce уже успешные chunks не пересчитываются, повторяется только упавший HTTP-вызов. После исчерпания retry поведение прежнее: ошибка возвращается в orchestrator, тот доставляет transcript-only и ставит `failed_summary`.

Конфиг (`config.go`, по образцу s2t): `SUMMARIZER_PROVIDER` += `openrouter`; `OPENROUTER_API_KEY` (обязателен при provider=openrouter), `OPENROUTER_MODEL` (дефолт `google/gemini-2.5-flash`), `OPENROUTER_BASE_URL` (дефолт `https://openrouter.ai/api/v1`), `OPENROUTER_PROXY_URL` (опционально). Проводка — `main.go` `case "openrouter"`; `.env.example` — новые строки.

## 5. Открытые / непроверенные вопросы

- **Юр-основание трансграничной передачи ПДн (152-ФЗ)** — prod/dev уже используют OpenRouter как `Summarizer`; до решения заказчика это live-risk перед широкой выдачей пользователям, а не технический блокер runtime.
- **Пустой/пробельный транскрипт** — поведение API (400 или 200 с тривиальным ответом) не зафиксировано; guard на пустой вход — на нашей стороне до вызова.
- **Детект «превышен контекст»** среди прочих 400 — отдельного `error.code` в доках нет, отличать по `error.message`; на дефолтной 1M-модели практически недостижимо.
- **`Retry-After`** — наличие не гарантировано (best-effort, §2).
- **Thinking gemini** — выключать ли ради цены (§3) — на живом тесте.

## 6. Источники

[chat completions](https://openrouter.ai/docs/api/api-reference/chat/send-chat-completion-request), [overview/auth](https://openrouter.ai/docs/api/reference/overview), [errors-and-debugging](https://openrouter.ai/docs/api/reference/errors-and-debugging), [limits](https://openrouter.ai/docs/api/reference/limits), [provider-selection](https://openrouter.ai/docs/guides/routing/provider-selection), [structured-outputs](https://openrouter.ai/docs/guides/features/structured-outputs), [reasoning-tokens](https://openrouter.ai/docs/guides/best-practices/reasoning-tokens), [models API](https://openrouter.ai/docs/api/api-reference/models/get-models), модели [gemini-2.5-flash](https://openrouter.ai/google/gemini-2.5-flash) / [gpt-4o-mini](https://openrouter.ai/openai/gpt-4o-mini) / [deepseek-chat](https://openrouter.ai/deepseek/deepseek-chat).
