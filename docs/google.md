# Google OAuth + Drive для transcrib

> **Канон для:** интеграции Google (привязка аккаунта по OAuth 2.0, доставка результата как Google Doc в Drive юзера), выбор скоупа `drive.file`, жизненный цикл refresh-токенов (Testing mode 7 дней), `/reconnect`/`awaiting_reauth`, конвертация markdown→Doc, путь Testing→Production (brand verification), compliance (152-ФЗ, Google User Data Policy).
> **Зеркала:** `architecture.md` §14 (post-0.1 Google delivery), `db.md` §3.2/§3.6/§5 (`google_accounts`, `reauth_from`, индексы).
> **Контракт API Google** (OAuth-endpoints, параметры, token exchange/refresh, `invalid_grant`, Drive v3 multipart+convert, ошибки, rate limits, Go-уровень `x/oauth2`, классификация скоупа, верификация) — [`docs/api/google/API.md`](api/google/API.md). Здесь — интеграционные решения и поток, не контракт.
> **Реализация.** План слайса удалён вместе с переходом проекта на поддержку; трек живёт в issue #13 (P2, post-0.1). История плана — в git до тега `0.0.3.7`.
> **Не дублировать здесь:** SQL и схему (`db.md` §3.2/§3.6/§5), state machine Job и runtime limits (`architecture.md` §7/§13), wire-контракт (`api/google/API.md`).
>
> **Статус:** канон post-0.1 Google-интеграции (F5 Google Doc + F8.5 `/reconnect`). Release 0.1 доставляет результат `.md`-файлами в Telegram; Google-режим реализуется отдельным слайсом `plan-google-slice.md` через **Testing mode без brand verification**. Brand verification (Testing→Production) — следующий шаг после живого Google-теста.

Версия: 1.0 (2026-06-14). Источник: мультиагентный ресёрч официальных доков `developers.google.com` + `support.google.com` (5 фасетов, контракт вынесен в [`api/google/API.md`](api/google/API.md)). Документ описывает доставку результата в Google Drive и привязку аккаунта. Сопровождает `architecture.md` (модули `google.go`/`web.go`/`google_delivery.go`) и `db.md` (таблица `google_accounts`).

## 1. Связка

```
┌──────────┐   OAuth-ссылка     ┌────────────┐   code на callback   ┌──────────┐
│  юзер в  │ ◄────────────────► │   Google   │ ───────────────────► │  web.go  │
│ Telegram │   жмёт, даёт доступ │  accounts  │                      │ (callback)│
└────┬─────┘                    └────────────┘                      └────┬─────┘
     │ результат (ссылка на Doc)                                          │ code→refresh_token
     ▼                                                            ┌───────▼────────┐
┌──────────────┐                                                  │   google.go    │
│google_delivery│  Deliver(ctx, chatID, filename, caption, content)│ OAuth + Drive  │
│     .go      │ ───────────────────────────────────────────────►│  (drive.file)  │
└──────────────┘                                                  └───────┬────────┘
                                                                          │ папка + Doc
                                                                          ▼
                                                                  ┌──────────────┐
                                                                  │  Drive юзера │
                                                                  └──────────────┘
```

**Роли:**
- **Google accounts** — OAuth-сервер: показывает consent, выдаёт `code`, обменивает на токены.
- **Google Drive** — хранилище результата. Документы лежат **у юзера**, мы их **не храним** (оператором ПД по содержимому не становимся, `architecture.md` §14/§15).
- **Наш сервис** — `google.go` (OAuth-флоу + создание папки/Doc), `web.go` (callback + бужение `awaiting_reauth`), `google_delivery.go` (реализация `DocumentDelivery`: создать Doc + отправить ссылку юзеру), `tg.go` (OAuth-ссылка в онбординге, `/reconnect`, пуши).

**Что хранится у нас:** только `refresh_token` + `folder_id` в `google_accounts` (`db.md` §3.2). Access-токен — в памяти процесса, обновляется `x/oauth2` сам. Готовые документы и аудио — **не храним**.

## 2. Подключение (Google Cloud, Testing mode)

Делается в Google Cloud Console, **без brand verification** (она — для Production, §7):

1. console.cloud.google.com → создать проект.
2. Включить **Google Drive API**.
3. OAuth consent screen: **External**, app name, support email; scope `drive.file`; добавить демо-юзеров в **Test users** (лимит 100).
4. Credentials → OAuth client ID (**Web application**), Authorized redirect URI = `GOOGLE_REDIRECT_URL`. Скопировать **Client ID + Secret** в `.env`.

- **Один OAuth-клиент на обе среды.** Test/prod различаются publishing status (Testing vs Production), а не парой кредов — в отличие от ЮKassa (там test/live = разные `provider_token`). Свапа кредов «test→prod» нет; «боевой режим» = Publish app + brand verification.
- **Локальный прогон G2** — `localhost`/loopback redirect разрешён в Testing (`api/google/API.md` §7), либо туннель на поднятый `web.go`.

## 3. Скоуп `drive.file` — выбор и почему

**Единственный скоуп — `https://www.googleapis.com/auth/drive.file`** (non-sensitive, Recommended). Даёт пофайловый доступ: создавать файлы и читать/менять **только** созданное приложением. Папку и Doc'ы создаёт само приложение → они в зоне доступа; к остальному Drive юзера доступа нет.

**Почему именно он (главная развилка):** restricted-скоупы (`drive`, `drive.readonly`, `drive.metadata`) дают доступ ко всему Drive и требуют **CASA security assessment** (тяжёлая проверка, недели). `drive.file` их **обходит** — security assessment не нужен, остаётся только лёгкая brand verification (~2-3 дня, §7). Контракт классификации — `api/google/API.md` §14.

**Следствие для архитектуры:** документы создаём, не читаем чужое; «не храним готовые документы у себя» (`architecture.md` §15) согласуется со скоупом естественно.

## 4. Поток интеграции

### 4.1. Привязка (онбординг Google-режима)

```
/start → согласие 152-ФЗ → tg.go отдаёт AuthURL(state) → юзер авторизуется на Google
  → Google шлёт code на /oauth/google/callback → web.go валидирует state
  → google.go: Exchange(code) → refresh_token → создать папку «Транскрибации созвонов»
  → storage.UpsertGoogleAccount(refresh_token, folder_id) → tg.go: «Google привязан»
```

- **`state` обязателен** — CSRF-защита + привязка OAuth-сессии к Telegram `userID`. Хранение `state` на MVP — in-memory (map userID↔state с TTL); DB-backed (`oauth_states`) — к продакшену (`plan-google-slice.md` техдолг).
- **`access_type=offline` + `prompt=consent`** в AuthURL — обязательны, иначе refresh-токен не придёт (`api/google/API.md` §2.1).
- **Enforcement привязки включается только в Google-режиме.** Release 0.1 с Telegram-доставкой не блокирует приём meeting-ссылок из-за отсутствия Google-привязки. В будущем Google-режиме приём ссылок блокируется (`IsGoogleLinked`), пока аккаунт не привязан.

### 4.2. Доставка результата

`google_delivery.go` реализует `DocumentDelivery` (`Deliver(ctx, chatID, filename, caption string, content []byte) error`): внутри `Deliver` создаёт Doc через `google.go` и **сам шлёт юзеру ссылку** (держит `notify`-замыкание поверх `*tele.Bot`, как `telegram_delivery.go` шлёт сам файл). Интерфейс **не меняем** — обоснование в `plan-google-slice.md` (журнал, п.4).

Текущий `orchestrator.go` для Telegram-доставки отдаёт transcript и summary в финальной grouped delivery. Целевая Google-семантика post-0.1 уточняется в `plan-google-slice.md`: базовый вариант — отдельные Doc-файлы в папке пользователя без изменения интерфейса `DocumentDelivery`.

### 4.3. `/reconnect` и `awaiting_reauth` (закрывает дыру с refresh)

При доставке `CreateDoc` может получить `invalid_grant` (юзер отозвал доступ или токен протух в Testing за 7 дней) → `ErrReauth`. Тогда:
- `SetJobAwaitingReauth`: статус `awaiting_reauth` (**не `failed`**), текущий статус (`processing`|`summarizing`) запоминается в `reauth_from`. **Резерв квоты НЕ откатывается** (транскрипт уже у поставщика, юзер заплатил).
- `tg.go` шлёт пуш «переподключите Google: /reconnect».
- После повторного OAuth-callback `web.go` → `WakeAwaitingReauthJobs(userID)`: восстанавливает `reauth_from` (`UPDATE jobs SET status = reauth_from, reauth_from = NULL ...`). Orchestrator подхватывает **ровно с той точки**, где упал — без дубля уже доставленного документа.

Полная разводка — `architecture.md` §14; SQL и индексы — `db.md` §3.6/§5. Wire-уровень `invalid_grant` и Go-детекция — `api/google/API.md` §6/§16.5.

## 5. Refresh-токены: Testing mode — главный нюанс (аналог «рекуррента» у платежей)

**В Testing mode refresh-токены протухают за 7 дней.** ✅ Подтверждено офиц. докой (`api/google/API.md` §5). Условие: External + publishing status Testing + offline-токен. Наш `drive.file` — **не basic** (для правила протухания важна дихотомия basic / не-basic, а не non-sensitive; по классификации §3 `drive.file` — non-sensitive), поэтому правило **применяется**.

**Что это значит для демо:** refresh-токен умирает каждые 7 дней → доступ к Drive рвётся → нужен путь восстановления. Закрыто двумя механизмами:
1. **`/reconnect`** (§4.3) — юзер переподключается, Job'ы возобновляются с точки падения.
2. **Refresh-token monitor** — post-0.1 крон-тик раз в сутки шлёт пуш «через 2 дня доступ к Google истечёт, обнови через /reconnect». Относится к классу F9-пушей.

**Снимается только переводом приложения In production** (Publish app + brand verification, §7) — тогда токены живут неограниченно, монитор и частые `/reconnect` убираются.

> **Открытый вопрос (см. §11):** монитор тикает по «давно обновляли», но в схеме `google_accounts` (`db.md` §3.2) **нет колонки `last_refresh_at`** — нужна колонка либо иной критерий. Разрешить при реализации монитора.

## 6. Конвертация markdown → нативный Google Doc

Doc создаётся **загрузкой markdown с конвертацией**: `POST /upload/drive/v3/files?uploadType=multipart`, метадата с `mimeType:"application/vnd.google-apps.document"` + контент с `Content-Type: text/markdown`. Google конвертирует на сервере. Docs API и `batchUpdate` **не нужны** — остаёмся в `drive.file`. Полный wire-контракт — `api/google/API.md` §10.

**⚠ Единственный реальный риск достоверности:** конвертация именно `text/markdown` анонсирована Google (07.2024), но не была в устной сводке таблицы импорт-форматов. **Решение:**
- основной путь — `text/markdown`;
- **fallback `text/html`** (стабильный импорт-формат) готов за флагом — markdown→HTML конвертим на своей стороне;
- на G2 проверить оба live-чеком `GET /drive/v3/about?fields=importFormats`.

Картинки в markdown конвертируются плохо — у наших транскриптов/саммари их нет, неактуально.

## 7. Путь Testing → Production (brand verification)

**Это продакшен-трек, НЕ блокер демо.** Демо работает в Testing mode (§2). Переход в Production снимает 7-дневное протухание токенов (§5) и экран «приложение не проверено». Детальные инфра-шаги (nginx server-блок + Certbot, homepage, privacy на `transcrib.example.com`, Search Console, submit) — `plan-google-slice.md` (раздел «Путь в Production»).

**Ключевая находка:** наш единственный скоуп `drive.file` — non-sensitive, поэтому нужна **только brand verification (~2-3 рабочих дня)**, а НЕ тяжёлая sensitive/restricted scope-верификация с CASA. Оценка «4-6 недель» в ранних канонах относилась к restricted-скоупам, которых у нас нет.

**Триггер brand verification:** External user type **+** показ logo/app name на consent screen. Что проверяет Google (`api/google/API.md` §15.3):

| Элемент | Требование |
|---|---|
| App name + logo | На consent screen |
| Support email | Есть (допустим gmail) |
| Homepage | Публично доступна, релевантна, не ссылка на Telegram/стор |
| Privacy policy | Видна юзеру, **на том же домене, что homepage**, описывает работу с Google-данными |
| Domain ownership | Все top-level домены из URI подтверждены в Google Search Console |

**Реальный длинный шаг — не ревью Google, а наша инфраструктура:** публичная homepage + privacy policy на домене `transcrib.example.com` + nginx server-блок/Certbot TLS + подтверждение домена в Search Console. Совпадает с инфра-частью продакшен-доводки (Этап 3). Публичная версия должна быть доступна на `https://transcrib.example.com/privacy/`.

## 8. Библиотека — `golang.org/x/oauth2`

**OAuth — через `golang.org/x/oauth2` + `golang.org/x/oauth2/google`.** Даёт `oauth2.Config` (`AuthCodeURL`/`Exchange`) и `Client(ctx, token)`, который **сам обновляет** access-токен из refresh и **сам сигналит** `invalid_grant` (`*oauth2.RetrieveError`) — ровно триггер `awaiting_reauth`. Ручной refresh не пишем. Имена, эскизы — `api/google/API.md` §16.

**Drive — сырой `net/http`, без `google.golang.org/api/drive/v3`.** Две ручки (папка + Doc) против тяжёлого дерева зависимостей (`grpc`/`protobuf`/`gax`) — нерационально. HTTP-клиент берём у `oauth2.Config.Client` (авто-токен). Соответствует стеку проекта (`net/http`) и симметрично `billing.go` (ЮKassa тоже сырым net/http).

**Подводные камни Go (`api/google/API.md` §16):**
- `&oauth2.Token{RefreshToken: rt}` форсит refresh из-за **пустого AccessToken**, не из-за zero Expiry — не подставлять фейковый AccessToken.
- `invalid_grant` прилетает **лениво** — на первом Drive-запросе, не из `Client()`. Маппинг в `ErrReauth` вешать на место Drive-вызова.
- `mime/multipart` пишет `form-data` — для Drive нужен `multipart/related` (ручной заголовок + `CreatePart` без `Content-Disposition`).

## 9. Compliance

| Тема | Требование |
|---|---|
| **Google API Services User Data Policy** | Базовые обязательства для всех: точный запрос скоупа, минимум разрешений, опубликованная privacy policy с описанием работы с Google-данными, использование данных строго по policy. Строгий **Limited Use** (security assessment, запрет на передачу) — часть **restricted**-процесса; к non-sensitive `drive.file` в полном объёме **не применяется**. |
| **Privacy policy** | Для brand verification — на том же домене, что homepage; раскрывает access/use/store/share Google user data; залинкована на consent. к Production проверить публикацию на `https://transcrib.example.com/privacy/`. |
| **152-ФЗ** (угол, не юрконсультация) | ПД в Google-контексте: refresh-токен (косвенный идентификатор), email (если запрошен — в `drive.file`-сценарии не обязателен), содержимое доков (транскрипты = ПД участников). Минимизация: ровно `drive.file`, refresh-токен шифровать в БД (AES-GCM, продакшен-доводка — `db.md` §3.2 TODO), содержимое доков у себя не персистить. Трансграничная передача (Drive) и список субпроцессоров — в privacy policy + уведомление РКН (вопрос юристу/заказчику, не код). |

## 10. Топ-ловушек (полный список — `api/google/API.md` §6/§10/§16)

1. **refresh_token приходит ТОЛЬКО при первом согласии** — повторный OAuth без `prompt=consent` вернёт пустой refresh. Всегда слать `access_type=offline` + `prompt=consent`; если refresh пуст — не затирать им хранимый.
2. **7-дневное протухание в Testing mode** — заложить `/reconnect` + монитор; снимается только Production.
3. **`invalid_grant` ≠ `failed`** — это `awaiting_reauth` без отката резерва; прилетает лениво на первом Drive-запросе.
4. **redirect_uri exact match** — схема/хост/порт/путь/слеш; единая константа в коде и Console.
5. **multipart-upload без `?fields=` возвращает только `id`** — нет `webViewLink`. Добавлять `?fields=id,webViewLink`.
6. **`multipart/related`, не `form-data`** — ручной заголовок запроса (Go `mime/multipart`).
7. **Удалённая юзером папка → 404 на `parents`** — это НЕ `invalid_grant`. На MVP допустимо `failed`; later — пересоздавать папку.
8. **Конвертация markdown** — целевой `mimeType: application/vnd.google-apps.document` + исходный `Content-Type: text/markdown`; `text/html` fallback.

## 11. Открытые вопросы

- **(монитор)** `google_accounts` без `last_refresh_at` — добавить колонку либо выбрать иной критерий тика. Решить при реализации монитора (F9-класс).
- **(markdown)** Live-подтверждение `text/markdown`→Doc на реальном аккаунте (`about.importFormats`); fallback `text/html` (§6).
- **(дефолт доставки)** В release 0.1 дефолт остается `telegram`. Вопрос смены дефолта на `google` возвращается только внутри отдельного post-0.1 Google-слайса после live Google-теста.
- **(хранение `state`)** in-memory на MVP против мини-таблицы `oauth_states` (переживание рестарта в момент онбординга) — `plan-google-slice.md`.

## 12. Источники

Контракт и первоисточники Google (официальные `developers.google.com`/`support.google.com`, метки ✅/⚠) — [`docs/api/google/API.md`](api/google/API.md) §20. Здесь — только интеграционные решения; факты не дублируются.
