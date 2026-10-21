# YooKassa API: legacy Telegram reference

Legacy reference for the former Telegram Payments + REST refund setup. The current working contract for direct payment links is in [`payment_links.md`](payment_links.md); where the two disagree on payment acceptance, that file wins.

**Source:** multi-agent research of the official docs `yookassa.ru/developers` + `yookassa.ru/docs` (2026-06-14, 15 agents, 7 facets + an independent check of 7 key facts). Every section links to its primary source (§19). Business/legal/fees/integration decisions are in `docs/payments.md` (canon).

> **STATUS (2026-06-14):** the contract is assembled from the official documentation; response shapes and the **main assumption** (`provider_payment_charge_id` == `payment_id`, §11) were NOT verified live - they get fixed on a test shop during the first implementation of `billing.go`. Confidence marks: [OK] confirmed by an independent check against the official docs; [!] needs a live check / not officially confirmed.

---

## 0. Base URL, environments, authentication

- **Base URL:** `https://api.yookassa.ru/v3/`. The `v3` version is part of the path - there is no separate version header. [OK]
- **Methods:** `POST`, `GET`, `DELETE`. Request and response bodies are always JSON (`Content-Type: application/json`).
- **Authentication - HTTP Basic Auth:** `Authorization: Basic base64(shopId:secretKey)`. [OK]
  - `username` = **shopId** (merchant dashboard: Settings -> Shop).
  - `password` = **secret key** (merchant dashboard: Integration -> API keys).
  - In curl: `-u <shopId>:<secretKey>`.
- **OAuth Bearer** (`Authorization: Bearer <token>`) is **for partner/marketplace integrations only**; we do NOT need it (we are a single shop). Do not confuse the `partners-api` branch of the docs with the main one.
- **Live and test shops have different `shopId` + key pairs.** `401 invalid_credentials` almost always means test/live credentials were mixed up.

### 0.1. Legacy: two credential pairs for Telegram Payments

The old Telegram Payments flow had two independent mechanisms with different credentials in each environment:

| Mechanism | What it does | Credential | Where to get it | test | live |
|---|---|---|---|---|---|
| **Payment acceptance** | `sendInvoice`, Telegram calls YooKassa | `provider_token` | BotFather -> bot -> Payments | `Connect ЮKassa: тест` ("Connect YooKassa: test") | `Connect ЮKassa: платежи` ("Connect YooKassa: payments") |
| **Refunds (+ reconciliation)** | `POST /v3/refunds` directly | `shopId:secretKey` | YooKassa merchant dashboard | test shop | live shop |

With direct payment links `provider_token` is not needed for payment acceptance: `shopId:secretKey` is used for both `POST /v3/payments` and `POST /v3/refunds`.

---

## 1. Legacy map of the Telegram integration

```
┌──────────┐ sendInvoice/receipt┌────────────┐  card/3-DS      ┌──────────┐
│ user in  │ <----------------> │  Telegram  │ <-------------> │ YooKassa │
│ Telegram │   successful_pay   │  (UI layer)│  processing     │processing│
└──────────┘                    └─────┬──────┘                 └────┬─────┘
                                       │ provider_payment_charge_id      │
                                       v                                 │
                               ┌──────────────┐  POST /v3/refunds        │
                               │  billing.go  │ ───(shopId:secretKey)───>│
                               │  (our bot)   │ <── refund (succeeded) ──┘
                               └──────────────┘
```

- **Legacy via Telegram:** payment acceptance, collecting email/phone for the receipt, passing the 54-FZ receipt (`provider_data`). Telegram itself calls YooKassa.
- **Direct payment links:** see [`payment_links.md`](payment_links.md); there we call `POST /v3/payments` directly and send `confirmation_url` to the user.
- **Refund:** in both approaches it goes directly through REST `POST /v3/refunds`.

---

## 2. API conventions

- **`amount.value` is always a string with EXACTLY two decimal places:** `"1990.00"` (not `1990`, not `"1990.0"`, not a number). `currency` is `"RUB"`. [OK]
- **Two money units (the main trap of this setup):** in Telegram `sendInvoice.prices[].amount` is **kopecks as an integer** (`199000`); in `provider_data.receipt...amount.value` it is **rubles as a string** (`"1990.00"`). Conversion: `kopecks/100` formatted with `.2f`. Mixing them up means a receipt for 19.90 RUB when 1990 RUB was paid.
- **Dates** are ISO 8601 UTC (`2026-04-12T13:59:33.681Z`).
- **Lists** use cursor pagination: `limit` (1-100, default 10) + `cursor` (= `next_cursor` of the previous response). Envelope `{ "type":"list", "items":[...], "next_cursor":"..." }`, sorted by creation date descending. Lists contain only objects **from the last 3 years**.
- **`save_payment_method` is passed as the string `"true"`**, not a boolean. In the current direct REST flow it is used for default-on recurring with `RECURRING_PAYMENTS_ENABLED=true`; legacy Telegram Payments cannot do this.
- **Error `description` is English only** (§13): to show an error to the user we map `code` to Russian text, not the `description` itself.

---

## 3. Idempotence-Key (critical money node)

[OK] Confirmed by the official docs.

- **Required for `POST` and `DELETE`** (GET is idempotent by itself). For us: on every `POST /v3/refunds`.
- The value is a string unique per operation, **up to 64 characters**, **UUID v4** recommended.
- **TTL = 24 hours.** A repeat with the **same key and the same data** makes the API return the **original result** (the action is not executed again). A repeat with the same key but **different data** is an error. A different key is a new operation.
- **Refund strategy (mandatory):** the key is **deterministic per refund intent** (our `refunds.idempotence_key`, generated in phase 1 `BeginRefund` BEFORE `POST /v3/refunds`, when `provider_refund_id` does not exist yet, `db.md` §3.5/§5.9), and **NOT** a fresh uuid on every retry, otherwise the money is refunded twice.
- **Retry semantics:** on `HTTP 500`/timeout (result unknown), retry with **the same key and a byte-for-byte identical body**. On `400`/`404`, fix the request and use a **new** key.
- **TTL boundary:** a retry older than 24 hours creates a **new** refund; before such a retry a status check (`GET /v3/refunds` by `payment_id`) is mandatory, otherwise a double refund happens.

---

## 4. The `payment` object (for reconciliation and direct links)

In legacy Telegram Payments the object was created by Telegram/YooKassa, and we used it for reconciliation and refunds. In direct payment links we create it ourselves via `POST /v3/payments`; the current creation and polling contract is in [`payment_links.md`](payment_links.md).

### 4.1. Statuses (state machine)

| Status | Meaning | Transitions |
|---|---|---|
| `pending` | Created, waiting for the payer | -> `waiting_for_capture` \| `succeeded` \| `canceled` |
| `waiting_for_capture` | Hold, waiting for capture (two-stage only, not our case) | -> `succeeded` \| `canceled` |
| `succeeded` | Completed successfully. **Final, immutable.** Money goes back only via refund | - |
| `canceled` | Canceled/declined. Final | - |

With Telegram (single-stage) the payment arrives already as `succeeded`. The only way to cancel is a refund.

### 4.2. Key response fields

| Field | Type | Description |
|---|---|---|
| `id` | string (UUID) | Payment ID. Used in refund (`payment_id`) and reconciliation |
| `status` | string | `pending`/`waiting_for_capture`/`succeeded`/`canceled` |
| `amount` | object | `{ value: "1990.00", currency: "RUB" }` |
| `paid` | bool | Whether the money was charged |
| `refundable` | bool | Whether it can be refunded now |
| `refunded_amount` | object | How much was already refunded (optional) |
| `payment_method` | object | `type` (`bank_card`, `sbp`, `yoo_money`, `sberbank`...), `id`, `saved` |
| `receipt_registration` | string | 54-FZ receipt registration status: `pending`/`succeeded`/`canceled` |
| `created_at` / `captured_at` / `expires_at` | string (ISO 8601) | Created / captured / capture deadline |
| `confirmation` | object | Confirmation scenario (`redirect`/`embedded`/`qr`/`external`/`mobile_application`) |
| `cancellation_details` | object | On `canceled`: `{ party, reason }` ([!] full enum, §18) |
| `test` | bool | Test payment (the only programmatic test vs prod marker) |
| `metadata` | object | Arbitrary key-value pairs (up to 16 keys) |

---

## 5. The `refund` object (main object of our direct integration)

| Field | Type | In response | Description / what to store |
|---|---|---|---|
| `id` | string (UUID) | yes | Refund ID. **We store** it as `refunds.provider_refund_id` (nullable, partial-UNIQUE as protection against repeated finalization; primary idempotence is our `idempotence_key`, `db.md` §3.5) |
| `payment_id` | string (UUID) | yes | ID of the original payment (= `provider_payment_charge_id`, [!] §11) |
| `status` | string | yes | `pending` \| `succeeded` \| `canceled` |
| `amount` | object | yes | `{ value: "1990.00", currency: "RUB" }` |
| `created_at` | string (ISO 8601) | yes | Creation time |
| `description` | string up to 250 | no | Reason for the refund |
| `cancellation_details` | object | on `canceled` | `{ party: yoo_money\|payment_network\|merchant, reason }` ([!] enum §18) |
| `receipt_registration` | string | no | Refund receipt status (`pending`/`succeeded`/`canceled`) |
| `refund_method` / `refund_authorization_details` | object | no | Method details / refund RRN (cards) |
| `sources` / `deal` | array/object | no | Split refunds / safe deal - **not our case** |

---

## 6. `POST /v3/refunds`: creating a refund

```
POST https://api.yookassa.ru/v3/refunds
Authorization: Basic base64(shopId:secretKey)
Idempotence-Key: <deterministic uuid per refund intent>
Content-Type: application/json
```

### 6.1. Request body

| Parameter | Type | Req. | Ours | Description |
|---|---|---|---|---|
| `payment_id` | string (UUID) | **yes** | yes | The payment (status=`succeeded`) being refunded |
| `amount` | object | **yes** | yes | `{ value, currency }`. Full refund = the whole amount |
| `description` | string up to 250 | no | opt. | Reason (for example "Subscription canceled on request") |
| `receipt` | object | conditional | **no** | 54-FZ receipt: needed **only for a partial** refund; for a full one YooKassa builds it itself |
| `sources` | array | no | no | Split refunds |
| `deal` | object | no | no | Safe deal |

Minimal body and response:

```json
// request
{ "payment_id": "21740069-000f-50be-b000-0486ffbf45b0",
  "amount": { "value": "1990.00", "currency": "RUB" } }

// response (usually succeeded right away)
{ "id": "216749f7-0016-50be-b000-078d43a63ae4",
  "status": "succeeded",
  "amount": { "value": "1990.00", "currency": "RUB" },
  "payment_id": "21740069-000f-50be-b000-0486ffbf45b0",
  "created_at": "2026-06-14T19:27:51.407Z" }
```

### 6.2. Rules and limits [OK]

- The original payment must have status `succeeded`.
- Full refund: `amount` = the whole amount; partial: at least 1 RUB, with the remainder at least 1 RUB or exactly 0. **The automated MVP support flow handles only full refunds** (`product.md` §6); a manual lawful refund on the user's written request is handled separately under the offer terms.
- **Deadline:** formally up to 3 years; for cards/Mir Pay the acquirer may reject refunds older than **~15 months** (not a blocker for a monthly subscription).
- The status in the POST response is usually already final (`succeeded`/`canceled`), but the `canceled` branch (`cancellation_details`) must be handled.
- **54-FZ receipt:** for a full refund we do NOT pass `receipt`: YooKassa builds the refund receipt from the original payment itself.

---

## 7. Refund status polling (our main mechanism)

```
GET https://api.yookassa.ru/v3/refunds/{refund_id}    # one refund
GET https://api.yookassa.ru/v3/payments/{payment_id}  # one payment (reconciliation)
```

- GET is idempotent, `Idempotence-Key` is not needed.
- A refund usually completes in **seconds**; occasionally the status updates after **up to ~20 minutes**, so build that into the polling interval/timeout.
- For us polling is simpler than a webhook: it needs no public HTTPS endpoint for YooKassa notifications.

---

## 8. List endpoints (reconciliation)

```
GET /v3/refunds?payment_id=...&status=...&limit=...&cursor=...
GET /v3/payments?status=...&limit=...&cursor=...
```

- Envelope `{ type:"list", items:[...], next_cursor }`, cursor pagination, 3-year window.
- [!] The exact names of the date filters (`created_at.gte/gt/lte/lt`, `captured_at.*`) did not export fully from the official page; confirm with live access before implementing reconciliation. They are also needed for the fallback payment lookup (§11).

---

## 9. 54-FZ receipt (the `receipt` object)

In the legacy Telegram scheme the receipt went through Telegram `provider_data` (a JSON string in `sendInvoice`). In direct payment links the same `receipt` object goes in the body of `POST /v3/payments`; for the current contract see [`payment_links.md`](payment_links.md).

```jsonc
// provider_data (as a JSON string) for sendInvoice
{
  "receipt": {
    "customer": { "email": "user@example.com" },          // or phone
    "items": [{
      "description": "Подписка transcrib, 15 часов/мес",  // receipt line shown to the payer (Russian on purpose: "transcrib subscription, 15 hours/month"), up to 128 characters
      "quantity": "1.00",
      "amount": { "value": "1990.00", "currency": "RUB" }, // RUBLES as a string
      "vat_code": 1,                                        // see §10
      "payment_subject": "service",
      "payment_mode": "full_payment",
      "measure": "piece"
    }],
    "tax_system_code": 2                                    // [!] depends on the customer's legal form
  }
}
```

### 9.1. `receipt` fields

| Field | Type | Req. | Description |
|---|---|---|---|
| `customer` | object | yes* | The general API allows `{ full_name?, inn?, email?, phone? }` and usually needs **email OR phone**; our direct flow is email-only and passes `receipt.customer.email` |
| `items` | array | yes | Up to 100 items |
| `tax_system_code` | int 1..6 | opt. | Not needed for "Receipts from YooKassa"; if the shop is configured so that the field is required, it is set explicitly in env |

### 9.2. `items[]` fields

| Field | Type | Req. | Description |
|---|---|---|---|
| `description` | string | yes | Item name, 1-128 characters |
| `quantity` | string/number | yes | Quantity (subscription: `"1.00"`) |
| `amount` | object | yes | `{ value (rubles as a string), currency: "RUB" }` per unit |
| `vat_code` | int 1..12 | yes | VAT rate code (§10) |
| `payment_subject` | string | yes (FFD 1.05+) | Payment subject attribute (subscription: `service`) |
| `payment_mode` | string | yes (FFD 1.05+) | Payment mode attribute (subscription: `full_payment`) |
| `measure` | string | no | Unit (`piece`) |

Validation: the payment amount = sum of (`quantity` x `amount.value`) over all items.

### 9.3. The "Receipts from YooKassa" service

YooKassa fiscalizes and registers the receipt with the fiscal data operator (OFD) itself, so **our own online cash register is not needed**. Requirement: the "Receipts from YooKassa" service enabled in the merchant dashboard (shop setting, customer track). We still pass `receipt` with every payment.

---

## 10. Tax codes

### 10.1. `vat_code` (VAT rate, 1..12) [OK] corrected by the check (not 1..10!)

| Code | Meaning | | Code | Meaning |
|---|---|---|---|---|
| 1 | No VAT | | 7 | VAT 5% (from 28.01.2025) |
| 2 | VAT 0% | | 8 | VAT 7% (from 28.01.2025) |
| 3 | VAT 10% | | 9 | VAT calculated 5/105 |
| 4 | VAT 20% | | 10 | VAT calculated 7/107 |
| 5 | VAT calculated 10/110 | | 11 | **VAT 22% (from 01.01.2026)** |
| 6 | VAT calculated 20/120 | | 12 | **VAT calculated 22/122 (from 01.01.2026)** |

For our case: `1` (No VAT: self-employed/simplified tax regime without VAT) or the rate that matches the actual legal form. [!] Hardcode only after the customer's legal form is confirmed.

### 10.2. `tax_system_code` (tax system, 1..6)

`1` general tax system (OSN); `2` simplified, income (USN); `3` simplified, income minus expenses; `4` imputed income tax (ENVD); `5` unified agricultural tax (ESHN); `6` patent tax system (PSN). Required only when the shop has several tax systems; otherwise it can be omitted.

### 10.3. `payment_subject` / `payment_mode` / `measure`

- `payment_subject` (subscription -> `service`): `commodity`, `service`, `job`, `payment`, `agent_commission`, `property_right`, `another`, ... (full enum: official docs, §19).
- `payment_mode` (subscription -> `full_payment`; `full_prepayment` if access has not been granted yet): `full_prepayment`, `partial_prepayment`, `advance`, `full_payment`, `partial_payment`, `credit`, `credit_payment`.
- `measure` (subscription -> `piece`): `piece`, `hour`, `day`, `gigabyte`, ...

---

## 11. Telegram and YooKassa integration

### 11.1. `provider_token`

- Issued by **BotFather** after the bot shop is connected to YooKassa. Format ([!] secondary source, not published officially): `ShopID:MODE:ArticleID`, `MODE` = `TEST`/`LIVE`.
- Passed in `sendInvoice` as the `provider_token` parameter. Telegram itself calls YooKassa and **takes no fee** (YooKassa charges per its tariff).
- Payment methods through this setup: only **Russian cards, YooMoney wallets, SberPay**.

### 11.2. `SuccessfulPayment` (what the bot receives)

| Field | Type | Description |
|---|---|---|
| `currency` | string | `RUB` (or `XTR` for Stars, not our case) |
| `total_amount` | int | Amount in **kopecks** (`199000`) |
| `invoice_payload` | string | Our payload from `sendInvoice` (links pre to post) |
| `telegram_payment_charge_id` | string | Payment ID in Telegram. **We store it** (UNIQUE: idempotence against Telegram retries) |
| `provider_payment_charge_id` | string | "Transaction number in YooKassa". **We store it** for the refund |
| `order_info` | object | email/phone (if requested) |

`is_recurring` / `is_first_recurring` / `subscription_expiration_date` arrive **only for Stars subscriptions**; they are absent with YooKassa.

### 11.3. Legacy assumption for Telegram refunds

**The equality `provider_payment_charge_id` (Telegram) == `payment_id` (for `POST /v3/refunds`) is officially NOT confirmed.** YooKassa calls this field the "transaction number in YooKassa", but in its Telegram payments guide it **refers to the merchant dashboard for refunds**, not to the API, and does not state the UUID equality directly. The whole direct refund scenario hangs on this assumption.

**Legacy check:** a live test payment through a test `provider_token`, take `provider_payment_charge_id`, then a trial `POST /v3/refunds` with it as `payment_id`. For direct payment links this check is not needed: the refund uses `payment.id`, which we got ourselves from `POST /v3/payments`.

### 11.4. Telegram-native recurring is impossible [OK]

The official restriction applies to the classic Telegram Payments + YooKassa setup: autopayments and holds cannot be configured there. The current working recurring contract is implemented not through Telegram-native payments but through direct REST payment links with `save_payment_method:"true"` and a merchant-initiated payment by `payment_method_id` (`payment_links.md`, `payments.md` §6).

---

## 12. Webhooks (optional for us)

YooKassa sends a `POST` to our HTTPS URL: `{ type:"notification", event, object }`.

- **Events:** `payment.succeeded`, `payment.canceled`, `payment.waiting_for_capture`, `refund.succeeded`, `payment_method.active`.
- **Setup for a Basic Auth shop is in the merchant dashboard** (Integration -> HTTP notifications). Through the API (`POST /v3/webhooks`) it is OAuth only (partners), not our case.
- **Endpoint requirements:** `https://` port 443/8443, TLS 1.2+, response **HTTP 200** (otherwise retries for 24 h).
- **There is NO signature.** Protection: (1) whitelist YooKassa IPs, (2) re-check the object via GET (do not trust the status in the body).
- **YooKassa IPs** [!] (re-check before prod): `185.71.76.0/27`, `185.71.77.0/27`, `77.75.153.0/25`, `77.75.156.11`, `77.75.156.35`, `77.75.154.128/25`, `2a02:5180::/32`.

**For the current slice:** we confirm payment links by polling `GET /v3/payments/{id}`; a webhook can be added later. If needed, we learn about refund completion by polling `GET /v3/refunds/{id}` (§7), which is simpler than standing up a public webhook.

---

## 13. Errors

Error body (HTTP 400/401/403/404/405/415/429/500):

```json
{ "type": "error", "id": "<id for support>", "code": "invalid_request",
  "description": "<text in English>", "parameter": "Idempotence-Key" }
```

| HTTP | `code` | Meaning | Shown to the user (the Russian mapping is ours) |
|---|---|---|---|
| 400 | `invalid_request` | request syntax/logic | «Не удалось обработать запрос» ("Could not process the request") |
| 401 | `invalid_credentials` | wrong shopId/key | internal error (alert) |
| 403 | `forbidden` | no permission for the operation | internal error (alert) |
| 404 | `not_found` | resource not found | «Платёж не найден» ("Payment not found") |
| 429 | `too_many_requests` | limit exceeded | retry with backoff |
| 500 | `internal_server_error` | result unknown | retry with the same key |

`description` is **English only**: we map by `code`, not by text.

---

## 14. Refund retry policy (critical money node)

| HTTP | Retry? | Idempotence-Key | Action |
|---|---|---|---|
| 200 | - | - | Done (or polling on `pending`) |
| 400 / 404 / 415 | no | **new** | Fix the request; a repeat with the same key is useless |
| 401 / 403 | no | - | Alert: wrong credentials / no permission |
| 429 | yes | same | Backoff (Fibonacci/exponential), increase the interval |
| 500 / timeout | **yes** | **same**, body byte-for-byte | Result unknown: a retry is mandatory, otherwise there is a risk of a double refund |

[!] YooKassa does not publish numeric rate limits; the `Retry-After` header is not confirmed, so handle `429` reactively. [!] Key TTL is 24 h: for a retry older than a day, check the status first (§3).

---

## 15. Test mode

- A test shop has its own `shopId` + test secret key; no money is charged, notifications arrive as in production; objects are marked **`test: true`** (the only programmatic marker).
- The test shop is created in the YooKassa merchant dashboard separately from the live one. Official docs: test mode is available right after registration, the test shop appears in the shop list within about a minute; at most 20 test shops.
- Setup before smoke: get `shopId` and `secret_key`; enable HTTP notifications if needed; if we check 54-FZ receipts through YooKassa, enable the receipt check mode in the online cash register settings of the test shop.
- For direct payment links, acceptance in test goes through test `shopId:secretKey`: the bot calls `POST /v3/payments`, sends `confirmation_url` and checks `GET /v3/payments/{id}` by polling. A test `provider_token` is needed only for the legacy Telegram Payments smoke test.
- Real cards cannot be used in a test shop.
- **Cards without 3-DS:** `4111 1111 1111 1111` (Visa), `5555 5555 5555 4444` (MC), `2202 4743 0132 2987` (Mir). **With 3-DS:** `4793 1281 6164 4804` (Visa), `5555 5555 5555 4477` (MC), `2200 0000 0000 0004` (Mir). Expiry: any future date; CVC/3-DS code: anything.
- For the first direct smoke test use `4111 1111 1111 1111`: fewer external factors than in the 3-DS scenario.
- **Failure emulation:** separate cards produce a specific `cancellation_details.reason` (`insufficient_funds`: Visa `4562 2655 8771 2390`; `card_expired`, `fraud_suspected`, `3d_secure_failed`, `general_decline` and others: full table in the official docs, §19).
- [!] The official instructions do not cover the nuances of **test refund** statuses (whether it can be `pending`); check with a trial REST refund on a test payment.

---

## 16. Live activation and the test to prod swap

**Shop registration (historical customer track):** registration, contract data (company details, color passport scan of the director, business type/OKVED code), review, contract signing, then the live `shopId` + secret key become active. The live direct payment-link and bank-card recurring smoke tests passed on 2026-07-09; the current runtime status and next gates are recorded in [`payments.md`](../../payments.md), not in this legacy reference.

**Legacy: live `provider_token` for Telegram:**
- [OK] Official flow: **BotFather** -> bot -> Payments -> **`Connect ЮKassa: платежи`** ("Connect YooKassa: payments") -> authorize the bot in the YooKassa merchant dashboard. ShopID/key are **not entered** in BotFather; the link goes through authorization in the dashboard.
- [!] Condition: the shop must work on the API protocol. Secondary/historical guides describe switching the protocol by **an email to `ecommerce@yoomoney.ru` with the ShopID**; the current official doc does NOT contain this step (replaced by an in-chat switch through the YooKassa bot). Treat the email as a workaround/outdated path; the current procedure mirrors `payments.md` §2.

**Swap in code for direct links:** test/live `YOOKASSA_SHOP_ID` + `YOOKASSA_SECRET_KEY` + `PAYMENT_RETURN_URL` + receipt vars. The rollout order was completed: dev/test bot with test REST credentials, payment-link and refund smoke tests, then prod with live REST credentials and a live smoke test. `PROVIDER_TOKEN` does not take part in the target payment acceptance.

---

## 17. cancel / capture: NOT our case

`POST /v3/payments/{id}/capture` and `POST /v3/payments/{id}/cancel` are for two-stage payment (`waiting_for_capture`). With Telegram the payment is `succeeded` right away, so we **do not use** capture/cancel; the only way to cancel a `succeeded` payment is a refund (§6). If `waiting_for_capture` shows up in reconciliation, something is wrong with the shop setup (alert).

---

## 18. What is NOT covered / open questions (follow-up)

To be closed by a live test on a test shop, by a browser pass over the JS pages of the docs, and by asking the customer/YooKassa manager.

- **Legacy blocker: `provider_payment_charge_id` == `payment_id`** (§11.3) applies only to the old Telegram Payments path. In direct payment links the risk is gone: `provider_payment_charge_id` stores the real `payment.id` from `POST /v3/payments`.
- **`cancellation_details.reason` enum** (payment and refund): the page renders client-side and is not returned by fetch. Only `expired_on_confirmation`/`expired_on_capture` are confirmed. The others (`general_decline`, `insufficient_funds`, `fraud_suspected`, `3d_secure_failed`, `canceled_by_merchant`, `payment_method_restricted`, ...) must be opened in a browser at `yookassa.ru/developers/api#refund_object` and `.../declined-payments/cancellation-details`. Needed for the Russian mapping of the reason for the user.
- **Date filter names of `GET /v3/payments|refunds`** (`created_at.gte/lte`...): confirm with live access (needed for the fallback lookup, §11.3).
- **`provider_token` format** (`x:TEST:y`/`x:LIVE:y`): only a secondary source, low confidence.
- **vat_code / tax_system_code / payment_subject / payment_mode** for our subscription: an accounting/legal question, resolved at shop registration (customer).
- **`refund.succeeded` subscription via the merchant dashboard for a Basic Auth shop:** check on a live shop (not critical for us, the main path is polling).
- **Test refund nuances** (statuses in test), **YooKassa tariff/fee** for Telegram acceptance, **relevance of the `ecommerce@yoomoney.ru` email:** clarify at registration.
- **Exact wording of the Telegram Bot API about "smallest units of the currency":** the `core.telegram.org/bots/api` page is too large to fetch; the fact is confirmed indirectly (`labeledPrice amount:long` + a secondary source), re-check when writing the code.

---

## 19. Legacy `billing.go` sketch for Telegram Payments

The old sketch covers only refunds + reconciliation, when payment acceptance went through `tg.go` (`sendInvoice`) and `successful_payment`. The new direct-payment code extends the same file with the handlers `CreatePayment` and `GetPayment`; see [`payment_links.md`](payment_links.md).

```
CreateRefund(ctx, paymentID, amountKopecks, idemKey) -> POST /v3/refunds     -> refund{id,status}
PollRefund(ctx, refundID)                            -> GET  /v3/refunds/{id} -> status
(fallback) FindPayment(ctx, ...)                     -> GET  /v3/payments?... -> payment{id}
```

- Auth is `Basic base64(shopId:secretKey)`; `Idempotence-Key` = `refunds.idempotence_key` (our UUID v4, generated in `BeginRefund` BEFORE the `POST`; **not** the PK `refunds.id`, see §3 and `db.md` §5.9).
- `amount.value` is `kopecks/100` formatted `.2f` as a string (§2).
- Refund SQL (two-phase: `idempotence_key` is primary, `provider_refund_id` is nullable; deactivates exactly that subscription row) is in `db.md` §5.9; direct payment finalization and legacy `successful_payment` acceptance are in `db.md` §5.4.

---

## 20. Sources

Priority goes to the official `yookassa.ru/developers` and `yookassa.ru/docs`; secondary sources (Habr) are marked explicitly.

- Interaction format (Basic auth, Idempotence-Key, 24h TTL): https://yookassa.ru/developers/using-api/interaction-format
- Response handling (HTTP codes, error format, retries): https://yookassa.ru/developers/using-api/response-handling
- Payment process (statuses, confirmation): https://yookassa.ru/developers/payment-acceptance/getting-started/payment-process
- Refunds: https://yookassa.ru/developers/payment-acceptance/after-the-payment/refunds
- 54-FZ receipts (receipt, vat_code, payment_subject/mode): https://yookassa.ru/developers/payment-acceptance/receipts/54fz/yoomoney/parameters-values
- Incoming notifications (webhooks, IPs, events): https://yookassa.ru/developers/using-api/webhooks
- Working with lists (cursor, limit): https://yookassa.ru/developers/using-api/lists
- Autopayments / save_payment_method: https://yookassa.ru/developers/payment-acceptance/scenario-extensions/recurring-payments
- Testing (test shop, cards, test:true): https://yookassa.ru/developers/payment-acceptance/testing-and-going-live/testing#test-bank-card-data
- Setting up payments through a Telegram bot: https://yookassa.ru/docs/support/payments/onboarding/integration/cms-module/telegram
- Connecting payment acceptance (registration, contract): https://yookassa.ru/connection/
- API reference (payment/refund objects, JS-rendered, open in a browser): https://yookassa.ru/developers/api
- Telegram Bot Payments API: https://core.telegram.org/bots/payments
- Secondary: Habr, "Integrating a Telegram bot with YooKassa" (Russian-language article): https://habr.com/ru/articles/855824/
