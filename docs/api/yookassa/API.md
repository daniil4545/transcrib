# ЮKassa API — legacy Telegram справочник

Legacy-справочник по прежней связке Telegram Payments + REST-refund. Новый рабочий контракт direct payment links описан в [`payment_links.md`](payment_links.md); при расхождении по приёму оплаты актуален он.

**Источник** — мультиагентный ресёрч официальных доков `yookassa.ru/developers` + `yookassa.ru/docs` (2026-06-14, 15 агентов, 7 фасетов + независимая проверка 7 ключевых фактов). Каждый раздел снабжён ссылками на первоисточник (§19). Бизнес/юр/комиссии/решения интеграции — `docs/payments.md` (канон).

> **СТАТУС (2026-06-14):** контракт собран из официальной документации; формы ответов и **главное допущение** (`provider_payment_charge_id` == `payment_id`, §11) живьём НЕ проверены — фиксируются на тестовом магазине при первой реализации `billing.go`. Отметки достоверности: ✅ подтверждено независимой проверкой по офиц. доке; ⚠ требует живой проверки / официально не подтверждено.

---

## 0. Базовый URL, среды, аутентификация

- **Базовый URL:** `https://api.yookassa.ru/v3/`. Версия `v3` зашита в путь — отдельного заголовка версии нет. ✅
- **Методы:** `POST`, `GET`, `DELETE`. Тело запросов и ответов — всегда JSON (`Content-Type: application/json`).
- **Аутентификация — HTTP Basic Auth:** `Authorization: Basic base64(shopId:secretKey)`. ✅
  - `username` = **shopId** (ЛК → Настройки → Магазин).
  - `password` = **секретный ключ** (ЛК → Интеграция → Ключи API).
  - В curl: `-u <shopId>:<secretKey>`.
- **OAuth Bearer** (`Authorization: Bearer <token>`) — **только партнёрские/маркетплейс-интеграции**, нам НЕ нужен (мы — один магазин). Не путать ветку `partners-api` доков с основной.
- **Боевой и тестовый магазины — разные пары `shopId`+ключ.** `401 invalid_credentials` почти всегда = перепутаны test/live креды.

### 0.1. Legacy: две пары кредов для Telegram Payments

В старом Telegram Payments потоке было два независимых механизма с разными кредами в каждой среде:

| Механизм | Что делает | Кред | Где взять | test | live |
|---|---|---|---|---|---|
| **Приём оплаты** | `sendInvoice` → Telegram вызывает ЮKassa | `provider_token` | BotFather → бот → Payments | `Connect ЮKassa: тест` | `Connect ЮKassa: платежи` |
| **Возвраты (+сверки)** | `POST /v3/refunds` напрямую | `shopId:secretKey` | ЛК ЮKassa | тестовый магазин | боевой магазин |

В direct payment links `provider_token` не нужен для приёма оплаты: `shopId:secretKey` используются и для `POST /v3/payments`, и для `POST /v3/refunds`.

---

## 1. Legacy-карта Telegram интеграции

```
┌──────────┐  sendInvoice/чек   ┌────────────┐   карта/3-DS    ┌──────────┐
│  юзер в  │ ◄────────────────► │  Telegram  │ ◄────────────► │ ЮKassa   │
│ Telegram │   successful_pay   │  (UI-слой) │   процессинг    │процессинг│
└──────────┘                    └─────┬──────┘                 └────┬─────┘
                                       │ provider_payment_charge_id      │
                                       ▼                                 │
                               ┌──────────────┐  POST /v3/refunds        │
                               │  billing.go  │ ───(shopId:secretKey)───►│
                               │  (наш бот)   │ ◄── refund (succeeded) ──┘
                               └──────────────┘
```

- **Legacy через Telegram:** приём оплаты, сбор email/phone для чека, передача чека 54-ФЗ (`provider_data`). Telegram сам вызывает ЮKassa.
- **Direct payment links:** см. [`payment_links.md`](payment_links.md); там мы вызываем `POST /v3/payments` напрямую и отправляем пользователю `confirmation_url`.
- **Refund:** в обоих подходах идёт напрямую через REST `POST /v3/refunds`.

---

## 2. Конвенции API

- **`amount.value` — всегда строка с РОВНО двумя знаками** после точки: `"1990.00"` (не `1990`, не `"1990.0"`, не число). `currency` — `"RUB"`. ✅
- **Две денежные единицы (главная ловушка связки):** в Telegram `sendInvoice.prices[].amount` — **копейки целым** (`199000`); в `provider_data.receipt...amount.value` — **рубли строкой** (`"1990.00"`). Конвертация: `kopecks/100` с форматом `.2f`. Перепутать = чек на 19,90 ₽ при оплате 1990 ₽.
- **Даты** — ISO 8601 UTC (`2026-04-12T13:59:33.681Z`).
- **Списки** — cursor-пагинация: `limit` (1–100, default 10) + `cursor` (= `next_cursor` предыдущего ответа). Конверт `{ "type":"list", "items":[...], "next_cursor":"..." }`, сортировка по дате создания убыванием. В списках только объекты **за последние 3 года**.
- **`save_payment_method` передаётся строкой `"true"`**, не boolean. В текущем direct REST flow используется для default-on recurring при `RECURRING_PAYMENTS_ENABLED=true`; legacy Telegram Payments этого не умеет.
- **`description` ошибок — только на английском** (§13): для показа юзеру маппим `code` → русский текст, не сам `description`.

---

## 3. Idempotence-Key (критический денежный узел)

✅ Подтверждено офиц. докой.

- **Обязателен для `POST` и `DELETE`** (GET идемпотентен сам). Для нас — на каждый `POST /v3/refunds`.
- Значение — уникальная для операции строка **≤64 символов**, рекомендуется **UUID v4**.
- **TTL = 24 часа.** Повтор с **тем же ключом и теми же данными** → API вернёт **исходный результат** (повторное действие не выполнится). Повтор с тем же ключом, но **другими данными** → ошибка. Другой ключ → новая операция.
- **Стратегия для refund (обязательна):** ключ **детерминирован по одному refund-намерению** (наш `refunds.idempotence_key`, генерируется в фазе 1 `BeginRefund` ДО `POST /v3/refunds` — `provider_refund_id` тогда ещё не существует, `db.md` §3.5/§5.9), а **НЕ** свежий uuid на каждый ретрай — иначе двойной возврат денег.
- **Ретрай-семантика:** на `HTTP 500`/таймаут (результат неизвестен) — ретраить **тем же ключом, байт-в-байт тем же телом**. На `400`/`404` — чинить запрос, **новый** ключ.
- **Граница TTL:** ретрай старше 24 часов создаст **новый** возврат — перед таким повтором обязательна проверка статуса (`GET /v3/refunds` по `payment_id`), иначе двойной возврат.

---

## 4. Объект `payment` (для сверок и direct links)

В legacy Telegram Payments объект создавал Telegram/ЮKassa, а мы использовали его для сверок и refund. В direct payment links мы создаём его сами через `POST /v3/payments`; актуальный контракт создания и polling описан в [`payment_links.md`](payment_links.md).

### 4.1. Статусы (state machine)

| Статус | Смысл | Переходы |
|---|---|---|
| `pending` | Создан, ждёт действия плательщика | → `waiting_for_capture` \| `succeeded` \| `canceled` |
| `waiting_for_capture` | Холд, ждёт capture (только двухстадийная — не наш кейс) | → `succeeded` \| `canceled` |
| `succeeded` | Успешно завершён. **Финальный, неизменяемый.** Деньги назад — только refund | — |
| `canceled` | Отменён/отклонён. Финальный | — |

В связке с Telegram (одностадийная) платёж приходит сразу `succeeded`. Единственный путь отмены — возврат.

### 4.2. Ключевые поля ответа

| Поле | Тип | Описание |
|---|---|---|
| `id` | string (UUID) | ID платежа. Используется в refund (`payment_id`) и сверках |
| `status` | string | `pending`/`waiting_for_capture`/`succeeded`/`canceled` |
| `amount` | object | `{ value: "1990.00", currency: "RUB" }` |
| `paid` | bool | Списаны ли деньги |
| `refundable` | bool | Можно ли вернуть сейчас |
| `refunded_amount` | object | Сколько уже возвращено (опц.) |
| `payment_method` | object | `type` (`bank_card`, `sbp`, `yoo_money`, `sberbank`...), `id`, `saved` |
| `receipt_registration` | string | Статус регистрации чека 54-ФЗ: `pending`/`succeeded`/`canceled` |
| `created_at` / `captured_at` / `expires_at` | string (ISO 8601) | Создан / подтверждён / дедлайн capture |
| `confirmation` | object | Сценарий подтверждения (`redirect`/`embedded`/`qr`/`external`/`mobile_application`) |
| `cancellation_details` | object | При `canceled`: `{ party, reason }` (⚠ полный enum §18) |
| `test` | bool | Тестовый платёж (единственный программный маркер test vs prod) |
| `metadata` | object | Произвольные key-value (≤16 ключей) |

---

## 5. Объект `refund` (главный объект нашей прямой интеграции)

| Поле | Тип | В ответе | Описание / что сохранять |
|---|---|---|---|
| `id` | string (UUID) | да | ID возврата. **Сохраняем** как `refunds.provider_refund_id` (nullable, partial-UNIQUE — защита от повторной финализации; первичная идемпотентность — наш `idempotence_key`, `db.md` §3.5) |
| `payment_id` | string (UUID) | да | ID исходного платежа (= `provider_payment_charge_id`, ⚠ §11) |
| `status` | string | да | `pending` \| `succeeded` \| `canceled` |
| `amount` | object | да | `{ value: "1990.00", currency: "RUB" }` |
| `created_at` | string (ISO 8601) | да | Момент создания |
| `description` | string ≤250 | нет | Основание возврата |
| `cancellation_details` | object | при `canceled` | `{ party: yoo_money\|payment_network\|merchant, reason }` (⚠ enum §18) |
| `receipt_registration` | string | нет | Статус чека возврата (`pending`/`succeeded`/`canceled`) |
| `refund_method` / `refund_authorization_details` | object | нет | Детали метода / RRN возврата (карты) |
| `sources` / `deal` | array/object | нет | Сплит-возвраты / безопасная сделка — **не наш кейс** |

---

## 6. `POST /v3/refunds` — создание возврата

```
POST https://api.yookassa.ru/v3/refunds
Authorization: Basic base64(shopId:secretKey)
Idempotence-Key: <детерминированный uuid на refund-намерение>
Content-Type: application/json
```

### 6.1. Тело запроса

| Параметр | Тип | Обяз. | Нам | Описание |
|---|---|---|---|---|
| `payment_id` | string (UUID) | **да** | да | Платёж (status=`succeeded`), который возвращаем |
| `amount` | object | **да** | да | `{ value, currency }`. Полный = вся сумма |
| `description` | string ≤250 | нет | опц. | Основание (например «Отмена подписки по запросу») |
| `receipt` | object | условно | **нет** | Чек 54-ФЗ — нужен **только при частичном** возврате; при полном ЮKassa формирует сам |
| `sources` | array | нет | нет | Сплит-возвраты |
| `deal` | object | нет | нет | Безопасная сделка |

Минимальное тело и ответ:

```json
// запрос
{ "payment_id": "21740069-000f-50be-b000-0486ffbf45b0",
  "amount": { "value": "1990.00", "currency": "RUB" } }

// ответ (обычно сразу succeeded)
{ "id": "216749f7-0016-50be-b000-078d43a63ae4",
  "status": "succeeded",
  "amount": { "value": "1990.00", "currency": "RUB" },
  "payment_id": "21740069-000f-50be-b000-0486ffbf45b0",
  "created_at": "2026-06-14T19:27:51.407Z" }
```

### 6.2. Правила и ограничения ✅

- Исходный платёж обязан быть в статусе `succeeded`.
- Полный возврат — `amount` = вся сумма; частичный — ≥ 1 RUB, остаток ≥ 1 RUB или ровно 0. **В автоматизированном MVP support-flow только полные** (`product.md` §6); ручной законный возврат по заявлению пользователя обрабатывается отдельно по оферте.
- **Срок:** формально до 3 лет; по картам/Mir Pay эквайер может отклонить старше **~15 месяцев** (для месячной подписки не блокер).
- Статус в ответе POST обычно уже финальный (`succeeded`/`canceled`), но `canceled`-ветку (`cancellation_details`) обработать обязательно.
- **Чек 54-ФЗ:** при полном возврате `receipt` НЕ передаём — ЮKassa формирует чек возврата из исходного платежа сама.

---

## 7. Polling статуса возврата (наш основной механизм)

```
GET https://api.yookassa.ru/v3/refunds/{refund_id}    # один возврат
GET https://api.yookassa.ru/v3/payments/{payment_id}  # один платёж (сверка)
```

- GET идемпотентны, `Idempotence-Key` не нужен.
- Возврат обычно проходит за **секунды**, изредка статус обновляется до **~20 минут** — закладывать в интервал/таймаут polling.
- Для нас polling проще webhook'а: не требует публичного HTTPS-эндпоинта под уведомления ЮKassa.

---

## 8. Списочные эндпоинты (сверка/реконсиляция)

```
GET /v3/refunds?payment_id=...&status=...&limit=...&cursor=...
GET /v3/payments?status=...&limit=...&cursor=...
```

- Конверт `{ type:"list", items:[...], next_cursor }`, cursor-пагинация, окно 3 года.
- ⚠ Точные имена date-фильтров (`created_at.gte/gt/lte/lt`, `captured_at.*`) официальной страницей целиком не выгрузились — подтвердить на живом доступе перед реализацией сверок. Нужны в т.ч. для fallback-lookup платежа (§11).

---

## 9. Чек 54-ФЗ (объект `receipt`)

В legacy Telegram-схеме чек уходил через Telegram `provider_data` (JSON-строка в `sendInvoice`). В direct payment links этот же объект `receipt` уходит в теле `POST /v3/payments`; актуальный contract см. [`payment_links.md`](payment_links.md).

```jsonc
// provider_data (JSON-строкой) для sendInvoice
{
  "receipt": {
    "customer": { "email": "user@example.com" },          // или phone
    "items": [{
      "description": "Подписка transcrib, 15 часов/мес",  // ≤128 символов
      "quantity": "1.00",
      "amount": { "value": "1990.00", "currency": "RUB" }, // РУБЛИ строкой
      "vat_code": 1,                                        // см. §10
      "payment_subject": "service",
      "payment_mode": "full_payment",
      "measure": "piece"
    }],
    "tax_system_code": 2                                    // ⚠ зависит от юр.формы заказчика
  }
}
```

### 9.1. Поля `receipt`

| Поле | Тип | Обяз. | Описание |
|---|---|---|---|
| `customer` | object | да* | Общий API допускает `{ full_name?, inn?, email?, phone? }` и обычно нужен **email ИЛИ phone**; наш direct flow выбран как email-only и передаёт `receipt.customer.email` |
| `items` | array | да | До 100 позиций |
| `tax_system_code` | int 1..6 | опц. | Для "Чеков от ЮKassa" не нужен; если магазин настроен так, что поле требуется, задаётся явно в env |

### 9.2. Поля `items[]`

| Поле | Тип | Обяз. | Описание |
|---|---|---|---|
| `description` | string | да | Наименование, 1–128 символов |
| `quantity` | string/number | да | Количество (подписка — `"1.00"`) |
| `amount` | object | да | `{ value (рубли строкой), currency: "RUB" }` за единицу |
| `vat_code` | int 1..12 | да | Код ставки НДС (§10) |
| `payment_subject` | string | да (FFD 1.05+) | Признак предмета расчёта (подписка — `service`) |
| `payment_mode` | string | да (FFD 1.05+) | Признак способа расчёта (подписка — `full_payment`) |
| `measure` | string | нет | Единица (`piece`) |

Валидация: сумма платежа = Σ(`quantity` × `amount.value`) по всем позициям.

### 9.3. Сервис «Чеки от ЮKassa»

ЮKassa сама фискализирует и регистрирует чек у ОФД — **своя онлайн-касса не нужна**. Требование: подключённый сервис «Чеки от ЮKassa» в ЛК (настройка магазина, трек заказчика). При этом `receipt` всё равно передаём при каждом платеже.

---

## 10. Налоговые коды

### 10.1. `vat_code` (ставка НДС, 1..12) ✅ исправлено проверкой (не 1..10!)

| Код | Значение | | Код | Значение |
|---|---|---|---|---|
| 1 | Без НДС | | 7 | НДС 5% (с 28.01.2025) |
| 2 | НДС 0% | | 8 | НДС 7% (с 28.01.2025) |
| 3 | НДС 10% | | 9 | НДС расч. 5/105 |
| 4 | НДС 20% | | 10 | НДС расч. 7/107 |
| 5 | НДС расч. 10/110 | | 11 | **НДС 22% (с 01.01.2026)** |
| 6 | НДС расч. 20/120 | | 12 | **НДС расч. 22/122 (с 01.01.2026)** |

Для нашего кейса: `1` (Без НДС — самозанятый/УСН без НДС) либо ставка по факту юр.формы. ⚠ Хардкодить только после подтверждения юр.формы заказчика.

### 10.2. `tax_system_code` (система налогообложения, 1..6)

`1` ОСН · `2` УСН доходы · `3` УСН доходы−расходы · `4` ЕНВД · `5` ЕСХН · `6` ПСН (патент). Обязателен только при нескольких системах у магазина; иначе можно не слать.

### 10.3. `payment_subject` / `payment_mode` / `measure`

- `payment_subject` (подписка → `service`): `commodity`, `service`, `job`, `payment`, `agent_commission`, `property_right`, `another`, ... (полный enum — офиц. дока §19).
- `payment_mode` (подписка → `full_payment`; `full_prepayment` если доступ ещё не предоставлен): `full_prepayment`, `partial_prepayment`, `advance`, `full_payment`, `partial_payment`, `credit`, `credit_payment`.
- `measure` (подписка → `piece`): `piece`, `hour`, `day`, `gigabyte`, ...

---

## 11. Связка Telegram ↔ ЮKassa

### 11.1. `provider_token`

- Выдаёт **BotFather** после подключения бот-магазина к ЮKassa. Формат (⚠ вторичный источник, официально не публикуется): `ShopID:MODE:ArticleID`, `MODE` = `TEST`/`LIVE`.
- В `sendInvoice` передаётся параметром `provider_token`. Telegram сам вызывает ЮKassa, **комиссию не берёт** (берёт ЮKassa по тарифу).
- Способы оплаты через связку: только **российские карты, кошельки ЮMoney, SberPay**.

### 11.2. `SuccessfulPayment` (что приходит боту)

| Поле | Тип | Описание |
|---|---|---|
| `currency` | string | `RUB` (или `XTR` для Stars — не наш кейс) |
| `total_amount` | int | Сумма в **копейках** (`199000`) |
| `invoice_payload` | string | Наш payload из `sendInvoice` (связка pre→post) |
| `telegram_payment_charge_id` | string | ID платежа в Telegram. **Сохраняем** (UNIQUE — идемпотентность от ретраев Telegram) |
| `provider_payment_charge_id` | string | «Номер транзакции в ЮKassa». **Сохраняем** для возврата |
| `order_info` | object | email/phone (если запрошены) |

`is_recurring` / `is_first_recurring` / `subscription_expiration_date` приходят **только для Stars-подписок**, в связке с ЮKassa их нет.

### 11.3. Legacy-допущение Telegram refund

**Равенство `provider_payment_charge_id` (Telegram) == `payment_id` (для `POST /v3/refunds`) официально НЕ подтверждено.** ЮKassa называет это поле «номером транзакции в ЮKassa», но в гайде по Telegram-платежам **отсылает за возвратами в личный кабинет**, а не к API, и прямо равенство UUID не заявляет. Весь прямой refund-сценарий висит на этом допущении.

**Legacy-проверка:** живой тестовый платёж через тестовый `provider_token` → взять `provider_payment_charge_id` → пробный `POST /v3/refunds` с ним как `payment_id`. Для direct payment links эта проверка не нужна: refund идёт по `payment.id`, который мы сами получили из `POST /v3/payments`.

### 11.4. Telegram-native рекуррент невозможен ✅

Официальное ограничение относится к классической связке Telegram Payments + ЮKassa: настроить автоплатежи и холдирование там нельзя. Текущий рабочий recurring-контракт реализован не через Telegram-native payments, а через direct REST payment links с `save_payment_method:"true"` и merchant-initiated payment по `payment_method_id` (`payment_links.md`, `payments.md` §6).

---

## 12. Webhooks (для нас опционально)

ЮKassa шлёт `POST` на наш HTTPS-URL: `{ type:"notification", event, object }`.

- **События:** `payment.succeeded`, `payment.canceled`, `payment.waiting_for_capture`, `refund.succeeded`, `payment_method.active`.
- **Настройка для Basic-Auth-магазина — через ЛК** (Интеграция → HTTP-уведомления). Через API (`POST /v3/webhooks`) — только OAuth (партнёры), не наш случай.
- **Требования к эндпоинту:** `https://` порт 443/8443, TLS 1.2+, ответ **HTTP 200** (иначе ретраи в течение 24 ч).
- **Безопасности-подписи НЕТ.** Защита: (1) whitelist IP ЮKassa, (2) перепроверка объекта через GET (не доверять статусу из тела).
- **IP ЮKassa** ⚠ (перепроверить перед продом): `185.71.76.0/27`, `185.71.77.0/27`, `77.75.153.0/25`, `77.75.156.11`, `77.75.156.35`, `77.75.154.128/25`, `2a02:5180::/32`.

**Для текущего слайса:** payment links подтверждаем polling'ом `GET /v3/payments/{id}`; webhook можно добавить позже. Завершение refund при необходимости узнаём polling'ом `GET /v3/refunds/{id}` (§7) — проще, чем поднимать публичный webhook.

---

## 13. Ошибки

Тело ошибки (HTTP 400/401/403/404/405/415/429/500):

```json
{ "type": "error", "id": "<id для поддержки>", "code": "invalid_request",
  "description": "<текст на английском>", "parameter": "Idempotence-Key" }
```

| HTTP | `code` | Смысл | Юзеру (русский маппинг — наш) |
|---|---|---|---|
| 400 | `invalid_request` | синтаксис/логика запроса | «Не удалось обработать запрос» |
| 401 | `invalid_credentials` | неверные shopId/ключ | внутренняя ошибка (алерт) |
| 403 | `forbidden` | нет прав на операцию | внутренняя ошибка (алерт) |
| 404 | `not_found` | ресурс не найден | «Платёж не найден» |
| 429 | `too_many_requests` | превышен лимит | ретрай с бэкоффом |
| 500 | `internal_server_error` | результат неизвестен | ретрай тем же ключом |

`description` — **только на английском**: маппим на `code`, не на текст.

---

## 14. Retry-политика возврата (критический денежный узел)

| HTTP | Ретраить? | Idempotence-Key | Действие |
|---|---|---|---|
| 200 | — | — | Готово (или polling при `pending`) |
| 400 / 404 / 415 | нет | **новый** | Чинить запрос; повтор тем же ключом бесполезен |
| 401 / 403 | нет | — | Алерт: неверные креды / нет прав |
| 429 | да | тот же | Бэкофф (Фибоначчи/экспонента), увеличивать интервал |
| 500 / таймаут | **да** | **тот же**, тело байт-в-байт | Результат неизвестен — ретрай обязателен, иначе риск двойного возврата |

⚠ Числовых rate-limit'ов ЮKassa публично не раскрывает; заголовок `Retry-After` не подтверждён — обрабатывать `429` реактивно. ⚠ TTL ключа 24 ч: ретрай старше суток — сначала проверить статус (§3).

---

## 15. Тестовый режим

- Тестовый магазин со своим `shopId` + тестовым секретным ключом; деньги не списываются, нотификации приходят как в бою; объекты помечены **`test: true`** (единственный программный маркер).
- Тестовый магазин создаётся в ЛК ЮKassa отдельно от боевого. Официальная дока: тестовый режим доступен сразу после регистрации, тестовый магазин появляется в списке магазинов примерно в течение минуты; максимум — 20 тестовых магазинов.
- Настройка перед smoke: получить `shopId` и `secret_key`; при необходимости включить HTTP-уведомления; если проверяем чеки 54-ФЗ через ЮKassa, включить режим проверки чеков в настройках онлайн-кассы тестового магазина.
- Для direct payment links приём в тесте идёт через тестовые `shopId:secretKey`: бот вызывает `POST /v3/payments`, отправляет `confirmation_url` и polling'ом проверяет `GET /v3/payments/{id}`. Тестовый `provider_token` нужен только для legacy Telegram Payments smoke.
- Настоящие карты нельзя использовать в тестовом магазине.
- **Карты без 3-DS:** `4111 1111 1111 1111` (Visa), `5555 5555 5555 4444` (MC), `2202 4743 0132 2987` (Mir). **С 3-DS:** `4793 1281 6164 4804` (Visa), `5555 5555 5555 4477` (MC), `2200 0000 0000 0004` (Mir). Срок — любой будущий, CVC/код 3-DS — любые.
- Для первого direct smoke использовать `4111 1111 1111 1111`: меньше внешних факторов, чем в 3-DS-сценарии.
- **Эмуляция отказов:** отдельные карты дают конкретный `cancellation_details.reason` (`insufficient_funds` — Visa `4562 2655 8771 2390`; `card_expired`, `fraud_suspected`, `3d_secure_failed`, `general_decline` и др. — полная таблица в офиц. доке §19).
- ⚠ Нюансы статусов **тестового refund** (бывает ли `pending`) официальной инструкцией не покрыты — проверить пробным REST-возвратом по тестовому платежу.

---

## 16. Боевая активация и свап test→prod

**Регистрация магазина (исторический трек заказчика):** регистрация → данные договора (реквизиты, цветной паспорт директора, вид деятельности/ОКВЭД) → проверка → подписание договора → активны боевые `shopId` + секретный ключ. Live direct payment-link и bank-card recurring smoke пройдены 2026-07-09; текущий runtime-статус и следующие gate фиксируются в [`payments.md`](../../payments.md), а не в legacy-справочнике.

**Legacy: боевой `provider_token` для Telegram:**
- ✅ Официальный флоу — **BotFather** → бот → Payments → **`Connect ЮKassa: платежи`** → авторизация бота в ЛК ЮKassa. ShopID/ключ в BotFather **не вводятся** — связь через авторизацию в ЛК.
- ⚠ Условие: магазин должен работать на протоколе API. Вторичные/исторические гайды описывают перевод протокола **письмом в `ecommerce@yoomoney.ru` с ShopID** — официальный текущий док этого шага НЕ содержит (заменён in-chat переводом через бота ЮKassa). Считать письмо обходным/устаревшим путём; актуальный порядок зеркалит `payments.md` §2.

**Свап в коде для direct links:** test/live `YOOKASSA_SHOP_ID` + `YOOKASSA_SECRET_KEY` + `PAYMENT_RETURN_URL` + receipt vars. Порядок rollout был пройден: dev/test-бот с тестовыми REST-кредами, payment-link и refund smoke, затем prod с live REST-кредами и live smoke. `PROVIDER_TOKEN` не участвует в целевом приёме оплаты.

---

## 17. cancel / capture — НЕ наш кейс

`POST /v3/payments/{id}/capture` и `POST /v3/payments/{id}/cancel` — для двухстадийной оплаты (`waiting_for_capture`). В связке с Telegram платёж сразу `succeeded`, поэтому capture/cancel мы **не используем**; единственная отмена `succeeded` — возврат (§6). Если в сверке всплыл `waiting_for_capture` — что-то не так с настройкой магазина (алерт).

---

## 18. Что НЕ покрыто / открытые вопросы (follow-up)

Закрывается живым тестом на тестовом магазине, браузером по JS-страницам доки и уточнением у заказчика/менеджера ЮKassa.

- **Legacy-блокер: `provider_payment_charge_id` == `payment_id`** (§11.3) — относится только к старому Telegram Payments пути. В direct payment links риск снят: `provider_payment_charge_id` хранит настоящий `payment.id` из `POST /v3/payments`.
- **enum `cancellation_details.reason`** (платёж и возврат) — страница рендерится client-side, через fetch не отдаётся. Подтверждены только `expired_on_confirmation`/`expired_on_capture`. Остальные (`general_decline`, `insufficient_funds`, `fraud_suspected`, `3d_secure_failed`, `canceled_by_merchant`, `payment_method_restricted`, ...) — открыть в браузере `yookassa.ru/developers/api#refund_object` и `.../declined-payments/cancellation-details`. Нужно для русского маппинга причины юзеру.
- **Имена date-фильтров `GET /v3/payments|refunds`** (`created_at.gte/lte`...) — подтвердить на живом доступе (нужны для fallback-lookup §11.3).
- **Формат `provider_token`** (`x:TEST:y`/`x:LIVE:y`) — только вторичный источник, confidence low.
- **vat_code / tax_system_code / payment_subject / payment_mode** под нашу подписку — бухгалтерско-юридический вопрос, решается при регистрации магазина (заказчик).
- **Подписка `refund.succeeded` через ЛК для Basic-Auth-магазина** — проверить на живом магазине (для нас не критично, основной путь — polling).
- **Тестовые нюансы refund** (статусы в тесте), **тариф/комиссия ЮKassa** за Telegram-приём, **актуальность письма `ecommerce@yoomoney.ru`** — уточнить при регистрации.
- **Дословная формулировка Telegram Bot API про «наименьшие единицы валюты»** — страница `core.telegram.org/bots/api` велика для fetch; факт подтверждён косвенно (`labeledPrice amount:long` + вторичный источник), перепроверить при написании кода.

---

## 19. Legacy-эскиз `billing.go` для Telegram Payments

Старый эскиз описывает только возвраты + сверки, когда приём оплаты шёл через `tg.go` (`sendInvoice`) и `successful_payment`. Новый direct-payment код расширяет этот же файл ручками `CreatePayment` и `GetPayment`; см. [`payment_links.md`](payment_links.md).

```
CreateRefund(ctx, paymentID, amountKopecks, idemKey) -> POST /v3/refunds     -> refund{id,status}
PollRefund(ctx, refundID)                            -> GET  /v3/refunds/{id} -> status
(fallback) FindPayment(ctx, ...)                     -> GET  /v3/payments?... -> payment{id}
```

- Auth — `Basic base64(shopId:secretKey)`; `Idempotence-Key` = `refunds.idempotence_key` (наш UUID v4, генерируется в `BeginRefund` ДО `POST`; **не** PK `refunds.id` — §3, `db.md` §5.9).
- `amount.value` — `kopecks/100` в формате `.2f` строкой (§2).
- SQL возврата (двухфазный: `idempotence_key` первичен, `provider_refund_id` nullable; деактивация ровно той строки подписки) — `db.md` §5.9; direct финализация payment и legacy приём `successful_payment` — `db.md` §5.4.

---

## 20. Источники

Приоритет — официальные `yookassa.ru/developers` и `yookassa.ru/docs`; вторичные (Habr) помечены явно.

- Формат взаимодействия (Basic auth, Idempotence-Key, TTL 24ч) — https://yookassa.ru/developers/using-api/interaction-format
- Обработка ответов (HTTP-коды, формат ошибки, ретраи) — https://yookassa.ru/developers/using-api/response-handling
- Процесс платежа (статусы, confirmation) — https://yookassa.ru/developers/payment-acceptance/getting-started/payment-process
- Возвраты — https://yookassa.ru/developers/payment-acceptance/after-the-payment/refunds
- Чеки 54-ФЗ (receipt, vat_code, payment_subject/mode) — https://yookassa.ru/developers/payment-acceptance/receipts/54fz/yoomoney/parameters-values
- Входящие уведомления (webhooks, IP, события) — https://yookassa.ru/developers/using-api/webhooks
- Работа со списками (cursor, limit) — https://yookassa.ru/developers/using-api/lists
- Автоплатежи / save_payment_method — https://yookassa.ru/developers/payment-acceptance/scenario-extensions/recurring-payments
- Тестирование (test shop, карты, test:true) — https://yookassa.ru/developers/payment-acceptance/testing-and-going-live/testing#test-bank-card-data
- Настройка платежей через Telegram-бота — https://yookassa.ru/docs/support/payments/onboarding/integration/cms-module/telegram
- Подключение приёма платежей (регистрация, договор) — https://yookassa.ru/connection/
- Справочник API (объекты payment/refund — JS-рендеринг, открывать в браузере) — https://yookassa.ru/developers/api
- Telegram Bot Payments API — https://core.telegram.org/bots/payments
- Вторичный: Habr «Интеграция Telegram-бота с ЮKassa» — https://habr.com/ru/articles/855824/
