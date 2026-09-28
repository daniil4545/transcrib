# Google OAuth 2.0 + Drive API — единый справочник контракта

Канонический справочник по REST/HTTP-контракту Google для `transcrib`. Покрывает ровно то, что нужно нашей интеграции: привязка Google-аккаунта юзера через **OAuth 2.0 Web Server flow** (offline access, refresh-токен), **создание папки** в Drive юзера и **загрузка markdown как нативного Google Doc** через Drive API v3 (scope `drive.file`), обработка `invalid_grant` для `/reconnect`, классификация скоупа и процесс верификации.

**Источник** — мультиагентный ресёрч официальных доков `developers.google.com` + `support.google.com` (2026-06-14, 5 фасетов: OAuth web flow, Drive v3, scopes/verification, Go `x/oauth2`, compliance/ловушки). Каждый раздел снабжён ссылками на первоисточник (§20). Бизнес/интеграционные решения, поток, compliance и ловушки — `docs/google.md` (канон).

> **СТАТУС (2026-06-14):** контракт собран из официальной документации. Ключевые факты подтверждены независимой проверкой по офиц. доке (✅). **Единственный реальный ⚠ — конвертация именно `text/markdown` → Google Doc** (§10): фича анонсирована Google 07.2024 и доступна через Drive API, но устная сводка таблицы импорт-форматов markdown не перечислила → проверить live-чеком `about.importFormats` при первой реализации `google.go`, держать `text/html` fallback. Метки: ✅ подтверждено офиц. докой; ⚠ требует живой проверки / официально неясно.

---

## 0. Базовые URL, среды, модель аутентификации

| Назначение | URL | Достоверность |
|---|---|---|
| OAuth — authorization | `https://accounts.google.com/o/oauth2/v2/auth` | ✅ |
| OAuth — token (code exchange + refresh) | `https://oauth2.googleapis.com/token` | ✅ |
| OAuth — revocation | `https://oauth2.googleapis.com/revoke` | ✅ |
| Drive — метаданные (создать папку) | `https://www.googleapis.com/drive/v3/files` | ✅ |
| Drive — upload (создать файл с контентом) | `https://www.googleapis.com/upload/drive/v3/files?uploadType=multipart` | ✅ |
| Drive — about (live-чек импорт-форматов) | `https://www.googleapis.com/drive/v3/about?fields=importFormats` | ✅ |

- **Модель аутентификации — OAuth 2.0 Bearer.** Все Drive-вызовы идут с заголовком `Authorization: Bearer <access_token>`; access-токен короткоживущий (~1ч), обновляется из refresh-токена через token endpoint.
- **Никаких API-ключей / service account.** Мы действуем **от имени юзера** (его Drive, его файлы), поэтому только пользовательский OAuth с refresh-токеном. Service account создавал бы файлы в своём Drive, а не в Drive юзера — не наш кейс.
- **Один OAuth-клиент (Web application)** с парой `client_id` + `client_secret` из Google Cloud Console. Тестовая и боевая среды — **один и тот же клиент**, разница только в publishing status (Testing vs Production, §15) — в отличие от ЮKassa, где test/live это разные креды.

### 0.1. Что мы НЕ используем (граница контракта)

- **Google Docs API** (`docs.googleapis.com`, `batchUpdate`) — не нужен: Doc создаём загрузкой markdown с конвертацией через Drive (§10), а не построением документа по операциям. Docs API потребовал бы отдельный скоуп `documents`.
- **Service account / domain-wide delegation** — не наш сценарий (см. выше).
- **Resumable / simple media upload** — наши доки мелкие (десятки–сотни КБ), хватает `uploadType=multipart` (§10, лимит 5 МБ).
- **Restricted-скоупы** (`drive`, `drive.readonly`, `drive.metadata`) — намеренно не трогаем (§14), иначе тяжёлый security assessment (CASA).

---

## 1. Карта интеграции transcrib

```
┌──────────┐  AuthURL/consent   ┌────────────┐   code на callback   ┌──────────┐
│  юзер в  │ ◄────────────────► │  Google    │ ───────────────────► │  web.go  │
│ Telegram │   жмёт ссылку      │  accounts  │  /oauth/google/...   │ (callback)│
└──────────┘                    └────────────┘                      └────┬─────┘
                                                                          │ code→token
                                          ┌───────────────────────────────┤ (google.go)
                                          ▼                                ▼
                                  ┌──────────────┐  Bearer access  ┌──────────────┐
                                  │ oauth2.Config│ ◄── refresh ───►│ token endpoint│
                                  │  .Client()   │  (авто-обновл.) └──────────────┘
                                  └──────┬───────┘
                                         │ POST /drive/v3/files (папка)
                                         │ POST /upload/drive/v3/files (Doc)
                                         ▼
                                  ┌──────────────┐
                                  │  Drive юзера │  webViewLink → ссылка юзеру в Telegram
                                  └──────────────┘
```

- **OAuth-флоу (`google.go` + `web.go`):** мы строим `AuthURL`, юзер авторизуется на стороне Google, Google шлёт `code` на наш публичный callback, мы меняем `code` на токены, сохраняем **только refresh-токен** + `folder_id`.
- **Drive-вызовы (`google.go`):** HTTP-клиент от `oauth2.Config.Client(ctx, token)` сам подставляет и обновляет access-токен. Две операции — создать папку, загрузить Doc.
- **`provider_payment_charge_id`-аналога нет:** Google не двигает деньги, всё взаимодействие — про доступ к Drive юзера.

---

## 2. OAuth 2.0 — authorization request (Web Server flow)

`GET https://accounts.google.com/o/oauth2/v2/auth?<params>`

| Параметр | Обяз. | Что делает |
|---|---|---|
| `client_id` | **да** | ID OAuth-клиента из Cloud Console |
| `redirect_uri` | **да** | **Точное** (exact match) совпадение с зарегистрированным Authorized redirect URI |
| `response_type` | **да** | Для web server flow = `code` |
| `scope` | **да** | Space-delimited; у нас один: `https://www.googleapis.com/auth/drive.file` |
| `access_type` | **критично** | `offline` — **обязателен, чтобы вернулся refresh-токен** (по умолч. `online`) |
| `prompt` | **критично** | `consent` — форсит экран согласия, чтобы refresh-токен пришёл **на каждой** авторизации, а не только на первой |
| `state` | **да (для нас)** | Произвольная строка: CSRF-защита + привязка OAuth-сессии к Telegram `userID`. Вернётся в callback |
| `include_granted_scopes` | нет | Incremental authorization; у нас один скоуп — **не нужен** |
| `login_hint` | нет | Подсказка email/ID |

### 2.1. ✅ Критичная комбинация для гарантированного refresh-токена

**`access_type=offline` + `prompt=consent`** — оба обязательны.

- `access_type=offline` инструктирует сервер вернуть refresh-токен. Но сам по себе refresh выдаётся **только при первом согласии юзера**.
- `prompt=consent` принудительно показывает consent повторно → refresh-токен приходит **каждый раз**.
- Без `prompt=consent` повторная авторизация уже согласившегося юзера вернёт `code`, обмен которого **не даст** `refresh_token` (только `access_token`). Это ловушка №1 (`docs/google.md`).

Цитата офиц. доки: *«This value [offline] instructs the Google authorization server to return a refresh token … the first time …»* / *«The refresh_token is only returned on the first authorization.»*

---

## 3. Code exchange (token endpoint)

`POST https://oauth2.googleapis.com/token` — `Content-Type: application/x-www-form-urlencoded`

**Поля запроса (все обязательны):** ✅
- `code` — authorization code из callback
- `client_id`, `client_secret`
- `redirect_uri` — совпадает с исходным
- `grant_type=authorization_code`

**Поля ответа (JSON):** ✅
| Поле | Описание |
|---|---|
| `access_token` | Bearer-токен для Drive-вызовов |
| `expires_in` | TTL access-токена в секундах (в примере доки `3920`; фактически ~1ч). ⚠ «1 час» словами не зафиксирован — **читать из `expires_in`, не хардкодить** |
| `refresh_token` | **Только** при `access_type=offline` (+ `prompt=consent` для повторных). **Это всё, что сохраняем** |
| `scope` | Выданные скоупы (space-delimited, case-sensitive) |
| `token_type` | Всегда `Bearer` |
| `id_token` | JWT, **только** если запрошены OpenID-скоупы (`openid`/`email`/`profile`). Для одного `drive.file` **не придёт** ⚠ |

- **Authorization code одноразовый.** Повторный обмен тем же `code` → `invalid_grant`, причём Google может отозвать уже выданные по нему токены (ловушка). На сбой — гнать юзера на новый OAuth-флоу, не ретраить обмен.

---

## 4. Refresh-токен и обновление access-токена

**Обновление access-токена:** ✅
`POST https://oauth2.googleapis.com/token`, `application/x-www-form-urlencoded`:
- `client_id`, `client_secret`
- `grant_type=refresh_token`
- `refresh_token`

Ответ: новый `access_token`, `expires_in`, `scope`, `token_type`. **Новый `refresh_token` НЕ приходит** — переиспользуется старый.

- **На стороне Go это делает `x/oauth2` сам** (§16): `oauth2.Config.Client(ctx, token)` отдаёт `*http.Client`, который кеширует access в памяти и рефрешит по истечении. Ручной refresh не пишем.
- **Хранение:** в БД лежит только `refresh_token` (`google_accounts.refresh_token`, `db.md` §3.2). Access-токен нигде не персистится — живёт в памяти процесса.

---

## 5. Testing mode: 7-дневное протухание refresh-токенов (критический узел)

✅ **ПРАВИЛО ДЕЙСТВУЕТ в 2026** — подтверждено двумя офиц. источниками.

Условия протухания refresh-токена за **7 дней**:
- OAuth consent screen — user type **External**;
- publishing status — **Testing**;
- клиент запросил `access_type=offline` и получил refresh-токен.

Цитата: *«Authorizations by a test user will expire seven days from the time of consent. If your OAuth client requests an offline access type and receives a refresh token, that token will also expire.»*

**Единственное исключение (нас НЕ спасает):** правило не применяется, если приложение запрашивает **только** basic-скоупы (`userinfo.email`, `userinfo.profile`, `openid` / Sign in with Google). Наш `drive.file` — **sensitive, не basic** → 7-дневное протухание **применяется**.

**Следствие для MVP-демо:** в Testing mode refresh-токен умирает каждые 7 дней → `/reconnect` (§6) обязателен в скоупе. Снимается только переводом приложения **In production** (Publish app, см. `docs/google.md` про brand verification).

### 5.1. Протухание/отзыв refresh-токенов вне Testing (Production) ✅

Refresh-токен инвалидируется, если:
- юзер **отозвал доступ** приложению;
- refresh-токен **не использовался 6 месяцев**;
- юзер сменил пароль, **а токен содержит Gmail-скоупы** (нас не касается — `drive.file`, не Gmail);
- превышен лимит **100 live refresh-токенов на (Google-аккаунт × OAuth client)** — старейший молча инвалидируется;
- admin Workspace ограничил скоуп (`admin_policy_enforced`).

---

## 6. `invalid_grant` — триггер `/reconnect`

✅ Главный сигнал, что refresh-токен мёртв и нужен повторный consent. Прилетает при обновлении (§4), когда:
- юзер **отозвал доступ** приложению;
- **7-дневное протухание в Testing mode** (наш основной кейс на демо);
- 6 месяцев неактивности; превышение лимита 100; смена пароля при Gmail-скоупах (не наш случай); admin policy.

⚠ Дополнительно (вторичные источники): `invalid_grant` бывает на **clock skew** (рассинхрон часов сервера), на **повторный обмен уже использованного `code`**, и на недокументированные security-эвристики. При `invalid_grant` повтор того же запроса всегда падает — лечится только новым consent-флоу.

**Обработка:** при `invalid_grant` на refresh → доменный `ErrReauth` → Job в `awaiting_reauth` (не `failed`), пуш юзеру «переподключите /reconnect». Архитектурная разводка — `architecture.md` §7.0, §9; SQL — `db.md` §3.6/§5. Go-детекция (`errors.As` на `*oauth2.RetrieveError`, `ErrorCode == "invalid_grant"`) — §16.

> **Важно (§16):** ошибка всплывает **лениво** — не из `Config.Client(...)`, а из **первого реального Drive-запроса** (или явного `TokenSource.Token()`). Маппинг в `ErrReauth` вешать на место Drive-вызова, не на конструктор клиента.

---

## 7. redirect_uri, localhost, OOB, PKCE


- **Exact match обязателен** ✅ — `redirect_uri` точно совпадает с зарегистрированным (схема, хост, порт, путь, без расхождений по trailing-slash). Несовпадение → `redirect_uri_mismatch`.
- **localhost / loopback разрешён** ✅ — *«Localhost URIs (including localhost IP address URIs) are exempt from [the HTTPS] rule.»* Для локального прогона G2 допустим `http://localhost:PORT/...` / `http://127.0.0.1:PORT/...`. Боевой callback — **HTTPS** (`https://transcrib.example.com/oauth/google/callback`).
- **OOB (`urn:ietf:wg:oauth:2.0:oob`) — полностью заблокирован с 31.01.2023** ✅. Нерелевантно (у нас полноценный web callback), но использовать нельзя.
- **PKCE** ⚠ — для confidential web-app с `client_secret` **не обязателен** (Google требует `client_secret` при обмене, PKCE его не заменяет). Рекомендуется как hardening; для MVP можно не внедрять. Тренд OAuth 2.1 — PKCE для всех, но Google для web+secret не требует.

---

## 8. Drive API v3 — две операции, две базы URL ✅

| Операция | Endpoint | Тело |
|---|---|---|
| Создать папку (метаданные-только) | `POST https://www.googleapis.com/drive/v3/files` | JSON File resource |
| Создать Doc из markdown (upload+convert) | `POST https://www.googleapis.com/upload/drive/v3/files?uploadType=multipart` | multipart/related: метаданные + контент |

`uploadType`: `media` (только контент), `multipart` (метаданные+контент — наш случай), `resumable` (крупные файлы). Для `/upload`-URL параметр обязателен.

---

## 9. Создание папки ✅

```http
POST https://www.googleapis.com/drive/v3/files?fields=id,name,webViewLink
Authorization: Bearer <access_token>
Content-Type: application/json; charset=UTF-8

{ "name": "Транскрибации созвонов",
  "mimeType": "application/vnd.google-apps.folder" }
```

- `parents` **не обязателен**: без него папка кладётся в корень My Drive юзера. Для одноуровневой папки приложения `parents` опускаем.
- Ответ — File resource. Без `?fields=` приходит урезанный набор — для гарантии нужного запрашиваем `?fields=id,name,webViewLink`.
- **Сохраняем `id` папки** → `google_accounts.folder_id`. Все Doc'ы создаём с `parents:[folder_id]`.

```json
{ "id": "1AbC...", "name": "Транскрибации созвонов",
  "webViewLink": "https://drive.google.com/drive/folders/1AbC..." }
```

---

## 10. Multipart upload + конвертация markdown → нативный Google Doc (ГЛАВНОЕ)

```http
POST https://www.googleapis.com/upload/drive/v3/files?uploadType=multipart&fields=id,webViewLink
Authorization: Bearer <access_token>
Content-Type: multipart/related; boundary=foo_bar_baz

--foo_bar_baz
Content-Type: application/json; charset=UTF-8

{ "name": "Транскрипт — Имя встречи",
  "parents": ["<folder_id>"],
  "mimeType": "application/vnd.google-apps.document" }
--foo_bar_baz
Content-Type: text/markdown

# Заголовок
**жирный** текст, списки и т.д.
--foo_bar_baz--
```

### 10.1. Жёсткие правила формата (RFC 2387 / Google) ✅

- Заголовок запроса — `Content-Type: multipart/related; boundary=<строка>` (**не** `form-data`!).
- **Часть 1 — метаданные, ПЕРВОЙ**, `Content-Type: application/json; charset=UTF-8`.
- **Часть 2 — контент, ВТОРОЙ**, `Content-Type: text/markdown` (исходный формат контента).
- Каждый boundary в теле предваряется `--`; финальный закрывается `--` с обеих сторон (`--foo_bar_baz--`).

### 10.2. Механизм конвертации ✅

Целевой формат задаётся полем `mimeType` в **метаданных** (`application/vnd.google-apps.document`), а `Content-Type` второй части — **исходный** формат (`text/markdown`). Drive конвертирует исходник в нативный Google Doc на сервере.

### 10.3. ⚠ Статус поддержки `text/markdown` — ЕДИНСТВЕННЫЙ реальный блокер достоверности

- Импорт/экспорт Markdown в Google Docs официально добавлен Google **31.07.2024**, доступен через Drive API (офиц. блог Google Workspace Updates) ✅.
- **НО:** устная сводка официальной таблицы импорт-форматов в момент проверки перечислила как конвертируемые в Docs: `.docx`, OpenDocument Text, **`text/html`**, RTF, **`text/plain`** — а `text/markdown` в этой выжимке **не фигурировал**. Расхождение анонса и сводки таблицы → метка ⚠.
- **Канонический live-чек (обязателен на G2):** `GET /drive/v3/about?fields=importFormats` — убедиться, что в карте есть `"text/markdown": ["application/vnd.google-apps.document"]`. Карта импорт-форматов аккаунт-специфична → это единственный 100%-достоверный рантайм-чек.

### 10.4. Fallback `text/html` ✅

Если `text/markdown` не отрабатывает на проде: конвертировать markdown → HTML на нашей стороне, грузить часть 2 с `Content-Type: text/html`. `text/html` — стабильно поддерживаемый импорт-формат (есть во всех версиях таблицы), сохраняет заголовки/списки/жирный. Последний резерв — `text/plain` (без форматирования). Подвох markdown→Doc: картинки (base64) конвертируются плохо — у наших транскриптов/саммари картинок нет, неактуально.

**Решение для дока:** `text/markdown` — основной путь; `text/html` — готовый fallback за флагом; оба проверить через `about.importFormats` на интеграции.

---

## 11. `webViewLink` — нужен `?fields=` ✅

- Multipart-upload и metadata-create **по умолчанию возвращают только `id`** (`webViewLink` — output-only, в дефолт не входит).
- Чтобы получить ссылку на Doc — **обязательно** `?fields=id,webViewLink` (для папки `?fields=id,name,webViewLink`).
- `webViewLink` = ссылка на открытие файла в браузере → её и отдаём юзеру в Telegram.

---

## 12. Ошибки Drive ✅

Тело ошибки — единая JSON-структура:

```json
{ "error": { "code": 403, "message": "<текст>",
    "errors": [ { "domain": "usageLimits", "reason": "userRateLimitExceeded", "message": "..." } ] } }
```

| HTTP | `reason` | Смысл / реакция |
|---|---|---|
| **401** | `authError` | Токен протух/невалиден/не тот скоуп → refresh access; при провале (`invalid_grant`) — `ErrReauth` → `awaiting_reauth`, **не `failed`** (§6) |
| 403 | `userRateLimitExceeded` | Пер-юзер лимит → exponential backoff |
| 403 | `rateLimitExceeded` / `dailyLimitExceeded` | Проектный/дневной лимит → backoff |
| 403 | `storageQuotaExceeded` | У юзера кончилось место в Drive → **не ретраить**, сообщить юзеру |
| 403 | `insufficientFilePermissions` / `appNotAuthorizedToFile` | Нет прав на файл/ACL |
| **404** | `notFound` | Parent-папка удалена юзером → **это НЕ `invalid_grant`** (токен валиден). Пересоздать папку, обновить `folder_id`; на MVP допустимо уйти в `failed` (`docs/google.md` техдолг) |

Ретраить **только** rate-limit (429 / 403-rate) через backoff. `storageQuotaExceeded`, `404`, `401` — не слепой ретрай.

---

## 13. Rate limits / квоты ✅

- **Per project:** 1 000 000 quota units/мин.
- **Per user per project:** 325 000 quota units/мин.
- **Per day per project:** 400 000 000 quota units.
- Стоимость: read ~5, list ~100, edit ~50 units.
- **Backoff:** truncated exponential — `min((2^n)+random_ms, maximum_backoff)`, `maximum_backoff` ~32–64с.
- Наш профиль (единицы запросов на встречу) на порядки ниже лимитов — реальный риск только всплески, закрываются backoff на 403/429.

### 13.1. Лимиты размера upload ✅

- **multipart upload — до 5 МБ** контента. Свыше — `uploadType=resumable`.
- Наши доки (транскрипт/саммари) — десятки–сотни КБ → **multipart достаточно**, resumable не нужен.

---

## 14. Скоупы: классификация `drive.file` ✅

| Параметр | Значение |
|---|---|
| Скоуп | `https://www.googleapis.com/auth/drive.file` |
| Класс | **Non-sensitive** (и **Recommended**) |
| Что даёт | Per-file доступ: создавать новые файлы и читать/менять **только** файлы и папки, созданные/открытые приложением. К остальному Drive юзера доступа нет |
| App (scope) verification | **НЕ требуется** для самого скоупа |
| Security assessment (CASA) | **НЕ требуется** |

**Подтверждено: `drive.file` — НЕ restricted.** Restricted (требуют CASA, доступ ко всему Drive): `drive`, `drive.readonly`, `drive.metadata`. Их **намеренно не используем** — это ключевая развилка, обойдённая выбором скоупа.

Архитектурное следствие: и папку, и Doc'ы создаёт само приложение → они в зоне доступа `drive.file`; чужие файлы юзера — нет. Совпадает с напоминалкой «не храним готовые документы у себя».

---

## 15. Consent screen, статусы публикации, brand verification ✅

### 15.1. OAuth consent screen (Google Auth Platform → Branding/Audience)

User type **External**; app name (показывается на consent **после** верификации); user support email; developer contact; authorized domains (все домены из homepage/privacy/redirect URI); scopes (`drive.file`); test users.

### 15.2. Testing vs In production

| | Testing | In production |
|---|---|---|
| Кто авторизуется | только **test users** (лимит **100**) | любой Google-аккаунт |
| Экран | «unverified app» (тестер: **Advanced → Go to … (unsafe)**) | чисто, без предупреждения |
| Refresh-токен | **протухает за 7 дней** (§5) | живёт неограниченно |

### 15.3. App verification vs Brand verification

| Тип | Когда требуется | Наш кейс |
|---|---|---|
| **Brand verification** | External **И** показ logo/display name на consent | **нужна** (только она) |
| **App (scope) verification** | sensitive/restricted скоупы (restricted +CASA) | **не нужна** (нет sensitive/restricted) |

**Срок brand verification: типично 2–3 рабочих дня** ✅ (а не 4–6 недель — та оценка про sensitive/restricted scope-верификацию).

**Требования brand verification:**
- **Homepage** — публично доступна (не за логином), релевантна приложению; не Google Play / не Facebook-ссылка.
- **Privacy policy** — **на ТОМ ЖЕ домене**, что homepage; раскрывает работу с Google user data; залинкована на consent screen.
- **Domain ownership** — все top-level домены из URI подтверждены в **Google Search Console** (аккаунт Cloud-проекта — owner/editor в Search Console).
- При смене logo/name/homepage/privacy/доменов — **пересабмит** и повторное одобрение.

### 15.4. Cap «100 новых юзеров» — только sensitive/restricted ✅

Production-cap «100 new users total» применяется к приложениям с **sensitive/restricted** скоупами в unverified-состоянии. Для **non-sensitive** (`drive.file`) этот cap **НЕ применяется**. Лимит **100 test users** — отдельный, это про **Testing publishing status** (любые скоупы), не путать.

---

## 16. Go-контракт (`golang.org/x/oauth2` + сырой `net/http`) ✅

### 16.1. Config + Endpoint

```go
import ("golang.org/x/oauth2"; "golang.org/x/oauth2/google")

cfg := &oauth2.Config{
    ClientID:     cid,
    ClientSecret: secret,
    Endpoint:     google.Endpoint, // готовая переменная, URL руками не зашивать
    RedirectURL:  "https://transcrib.example.com/oauth/google/callback",
    Scopes:       []string{"https://www.googleapis.com/auth/drive.file"},
}
```

`drive.DriveFileScope` из `google.golang.org/api/drive/v3` == строка `https://www.googleapis.com/auth/drive.file` — **зашить строкой**, пакет ради одной строки не тащить.

### 16.2. AuthURL — offline + consent

```go
url := cfg.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce)
```

- `oauth2.AccessTypeOffline` == `access_type=offline`.
- `oauth2.ApprovalForce` == `prompt=consent` (в 2026 это буквально алиас `SetAuthURLParam("prompt","consent")`; раньше слал устаревший `approval_prompt=force`). Идиоматично — `ApprovalForce`.

### 16.3. Exchange → сохраняем только refresh

```go
tok, err := cfg.Exchange(ctx, code) // *oauth2.Token{AccessToken, RefreshToken, Expiry, ...}
// сохранить только tok.RefreshToken
```

### 16.4. Самообновляющийся клиент из сохранённого refresh

```go
tok := &oauth2.Token{RefreshToken: rt} // AccessToken пуст, Expiry zero
client := cfg.Client(ctx, tok)         // *http.Client сам рефрешит access из refresh
```

⚠ **Поправка к интуиции:** `&oauth2.Token{RefreshToken: rt}` форсит refresh **из-за пустого `AccessToken`**, а НЕ из-за zero `Expiry` (zero Expiry в `x/oauth2` трактуется как «не истёк»). Не подставлять непустой фейковый AccessToken с zero Expiry — тогда refresh не сработает.

`Config.TokenSource`/`Config.Client` уже оборачивают рефрешер в `ReuseTokenSource` (кеш access в памяти). `StaticTokenSource` — НЕ использовать (не обновляет). Руками `ReuseTokenSource` звать не нужно.

### 16.5. Детекция reauth

```go
var ErrReauth = errors.New("google: refresh token invalid, reconnect required")

func classifyReauth(err error) error {
    var re *oauth2.RetrieveError
    if errors.As(err, &re) && re.ErrorCode == "invalid_grant" {
        return ErrReauth
    }
    return err
}
```

- `*oauth2.RetrieveError.ErrorCode` присутствует (golang/go#58125). `errors.As`, не голый type-assert (ошибка может быть обёрнута).
- Ошибка прилетает **лениво** — на первом Drive-запросе / явном `Token()`. Оборачивать `classifyReauth` вокруг места Drive-вызова.

### 16.6. multipart/related сырым net/http

`mime/multipart.Writer` пишет байты тела идентично для `related` и `form-data`; отличается заголовок запроса и отсутствие `Content-Disposition` у частей. Используем `CreatePart` с явным `MIMEHeader` (только `Content-Type`) и вручную ставим тип запроса `multipart/related`:

```go
func buildRelated(metadataJSON, content []byte, contentMime string) (*bytes.Buffer, string, error) {
    body := &bytes.Buffer{}
    w := multipart.NewWriter(body)
    mh := textproto.MIMEHeader{}; mh.Set("Content-Type", "application/json; charset=UTF-8")
    p1, _ := w.CreatePart(mh); p1.Write(metadataJSON)       // часть 1 — метаданные (имя, parents, target mimeType)
    mh2 := textproto.MIMEHeader{}; mh2.Set("Content-Type", contentMime) // text/markdown | text/html
    p2, _ := w.CreatePart(mh2); p2.Write(content)           // часть 2 — медиа
    w.Close()
    return body, "multipart/related; boundary=" + w.Boundary(), nil // тип запроса — related, НЕ form-data
}
```

`Content-Length` для `*bytes.Buffer` net/http проставит сам. `Authorization` подставит `cfg.Client(...)`.

### 16.7. SDK `drive/v3` vs сырой net/http

`google.golang.org/api/drive/v3` тянет тяжёлое дерево (`gax-go`, `grpc`, `protobuf`, `option` — десятки пакетов) ради двух вызовов. Для проекта (принцип «минимум зависимостей, сырой net/http») — **сырой net/http**: `oauth2`-клиент + 2 ручных POST. `oauth2`/`oauth2/google` тащим обязательно (лёгкие, чистый stdlib + net/http), `drive/v3` — нет. Симметрия с `billing.go` (ЮKassa тоже сырым net/http).

---

## 17. Тестовый режим / как проверять

- **Sandbox нет** — тестируем реальным OAuth-клиентом в **Testing mode**: добавить демо-юзеров в Test users (лимит 100), проходить «Advanced → Go to … (unsafe)».
- **Локальный прогон G2** — `localhost`/loopback redirect разрешён в Testing (§7); либо туннель на поднятый callback.
- **Live-чеки на интеграции (G2):** (1) `about.importFormats` — поддержка `text/markdown` (§10.3); (2) формат `webViewLink` в ответе с `?fields=id,webViewLink` (§11); (3) реальный путь `invalid_grant` — отозвать доступ в Google-аккаунте, убедиться, что прилетает `*oauth2.RetrieveError{ErrorCode:"invalid_grant"}` на Drive-запросе (§16.5); (4) что `id_token` при одном `drive.file` не приходит (§3).
- Unit: OAuth-флоу мокается на уровне `*http.Client` / подменённого транспорта; Drive-вызовы — против записанных ответов.

---

## 18. Что НЕ покрыто / открытые вопросы (follow-up)

Закрывается живым прогоном G2 на реальном Google Cloud client и тест-юзере.

- **⚠ Конвертация `text/markdown` → Doc** (§10.3) — главный live-чек. `about.importFormats`; `text/html` fallback наготове.
- **Точный `expires_in`** access-токена (формально не «1 час») — читать из ответа, не хардкодить.
- **`invalid_grant` на clock skew / reused code** — наблюдать на тест-флоу.
- **Необязательность PKCE** для web+secret — подтвердить пробным exchange без `code_verifier`.
- **Отсутствие `id_token`** при одном `drive.file` — проверить в реальном ответе.
- **Refresh-token monitor** требует знать «когда обновляли» — в текущей схеме `google_accounts` (`db.md` §3.2) **нет колонки `last_refresh_at`**, на которую ссылается `architecture.md` §9. Расхождение зафиксировать в `docs/google.md` (открытые вопросы) — нужна колонка либо иной критерий тика.

---

## 19. Эскиз `google.go` (OAuth + Drive сырым net/http)

```
AuthURL(state) string                          -> cfg.AuthCodeURL(state, offline, consent)
Exchange(ctx, userID, code) error              -> token endpoint -> создать папку -> upsert google_accounts
CreateDoc(ctx, userID, title, markdown) (url, error)
                                                -> POST /upload/drive/v3/files?uploadType=multipart&fields=id,webViewLink
                                                -> ErrReauth при invalid_grant
```

- HTTP-клиент — `cfg.Client(ctx, &oauth2.Token{RefreshToken: rt})` (авто-refresh).
- Метадата папки — `mimeType:"application/vnd.google-apps.folder"`; Doc — `mimeType:"application/vnd.google-apps.document"` + `parents:[folder_id]`.
- Интеграционные решения, поток, compliance, ловушки — `docs/google.md`. Статус трека — issue #13 (P2, post-0.1).

---

## 20. Источники

Приоритет — официальные `developers.google.com` / `support.google.com`; вторичные помечены.

**OAuth 2.0:**
- Web Server flow (endpoints, параметры, offline+consent, refresh, localhost/HTTPS) — https://developers.google.com/identity/protocols/oauth2/web-server
- Token expiration (причины протухания, 6 мес, лимит 100) — https://developers.google.com/identity/protocols/oauth2#expiration
- OOB migration (deprecation) — https://developers.google.com/identity/protocols/oauth2/resources/oob-migration

**Scopes / verification / publishing:**
- Requesting Minimum Scopes (классификация) — https://support.google.com/cloud/answer/13807380
- Choose Google Drive API scopes — https://developers.google.com/workspace/drive/api/guides/api-specific-auth
- Manage App Audience (Testing/test users, 7-дн токен, 100-cap) — https://support.google.com/cloud/answer/15549945
- Submit for brand verification (срок, требования) — https://developers.google.com/identity/protocols/oauth2/production-readiness/brand-verification
- Configure OAuth consent screen — https://developers.google.com/workspace/guides/configure-oauth-consent
- Manage OAuth App Branding — https://support.google.com/cloud/answer/15549049

**Drive API v3:**
- files.create reference — https://developers.google.com/drive/api/reference/rest/v3/files/create
- Manage uploads (multipart, импорт-форматы, лимит 5 МБ) — https://developers.google.com/workspace/drive/api/guides/manage-uploads
- Create folders — https://developers.google.com/workspace/drive/api/guides/folder
- Partial response (fields, webViewLink) — https://developers.google.com/workspace/drive/api/guides/performance
- Resolve errors — https://developers.google.com/workspace/drive/api/guides/handle-errors
- Usage limits — https://developers.google.com/workspace/drive/api/guides/limits
- Markdown import/export (офиц. блог Google) — https://workspaceupdates.googleblog.com/2024/07/import-and-export-markdown-in-google-docs.html

**Go:**
- golang.org/x/oauth2 — https://pkg.go.dev/golang.org/x/oauth2
- golang.org/x/oauth2/google — https://pkg.go.dev/golang.org/x/oauth2/google
- RetrieveError.ErrorCode — https://github.com/golang/go/issues/58125

**Compliance:**
- API Services User Data Policy / Limited Use — https://developers.google.com/terms/api-services-user-data-policy

**Вторичные (⚠, перепроверять):**
- invalid_grant edge cases — https://nango.dev/blog/google-oauth-invalid-grant-token-has-been-expired-or-revoked/
- multipart upload to Drive in Go — https://freshman.tech/snippets/go/multipart-upload-google-drive/
- 152-ФЗ (юр.блоги) — https://1ps.ru/blog/dirs/2025/, https://www.klerk.ru/blogs/roskom24/674017/
