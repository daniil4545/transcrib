# YooKassa payment links

Status: current working contract for the direct payment links slice, verified against the official YooKassa documentation on 2026-06-30.

This article describes the toolkit we use instead of a Telegram Payments provider token: a direct YooKassa REST payment, a redirect link for the user, status polling, default-on recurring payments behind a runtime flag, and a REST refund. The old Telegram contract remains as historical background in [`API.md`](API.md).

## Goals and boundaries

In scope:
- create a payment via `POST /v3/payments`;
- send `confirmation.confirmation_url` to the user as a Telegram button;
- confirm the payment by polling `GET /v3/payments/{payment_id}`;
- on `succeeded && paid=true`, grant the subscription;
- with `RECURRING_PAYMENTS_ENABLED=true`, the first payment requests saving the payment method and enables recurring only if YooKassa returned `payment_method.saved=true`;
- on `canceled`, close the pending payment without a subscription;
- for refunds, use `POST /v3/refunds` with the same `payment.id`.

Out of scope for the first slice:
- the `payment.succeeded` webhook;
- two-stage payment (`waiting_for_capture`);
- payment without a fiscal receipt while the "Receipts from YooKassa" mode is on.

## Why REST link + polling

A REST link removes the dependency of payment acceptance on the BotFather binding and on Telegram `successful_payment`. Our payment key is immediately the real YooKassa `payment.id`, so refunds no longer rely on the Telegram assumption `provider_payment_charge_id == payment_id`.

Polling is the first step because it needs no incoming HTTPS endpoint, no webhook setup in the merchant dashboard, and no nginx/deploy work. We use `return_url` only as a UX return: the user may pay and close the page, so the payment status in YooKassa remains the source of truth.

## API, auth and environments

- Base URL: `https://api.yookassa.ru/v3`.
- Format: REST/JSON.
- Auth: HTTP Basic Auth, username = `shopId`, password = `secretKey`.
- OAuth is not needed: it applies to partner scenarios.
- Test and live shops have different `shopId` and `secretKey`; in test objects YooKassa returns `test=true`.

## Creating a payment

```http
POST /v3/payments
Authorization: Basic base64(shopId:secretKey)
Idempotence-Key: <uuid-or-other-unique-key>
Content-Type: application/json
```

Minimal working form for our service with the recurring flag on. With `RECURRING_PAYMENTS_ENABLED=false`, the fields `save_payment_method` and `metadata.auto_renew` are not sent. The `description` values are Russian on purpose: they are shown to the payer and printed on the receipt ("15 часов на 1 месяц" means "15 hours for 1 month"):

```json
{
  "amount": { "value": "1990.00", "currency": "RUB" },
  "capture": true,
  "confirmation": {
    "type": "redirect",
    "return_url": "https://transcrib.example.com/payments/return"
  },
  "description": "15 часов на 1 месяц",
  "metadata": { "user_id": "123456", "auto_renew": "true" },
  "save_payment_method": "true",
  "receipt": {
    "customer": { "email": "user@example.com" },
    "items": [{
      "description": "15 часов на 1 месяц",
      "quantity": 1,
      "amount": { "value": "1990.00", "currency": "RUB" },
      "vat_code": 1,
      "payment_subject": "service",
      "payment_mode": "full_payment",
      "measure": "piece"
    }]
  }
}
```

The expected response contains:
- `id`: provider payment id, stored in `payments.provider_payment_charge_id`;
- `status`: usually `pending` until the user acts;
- `paid`: bool;
- `confirmation.confirmation_url`: the link we send to the user;
- `amount`, `metadata`, raw JSON for audit;
- `payment_method.id` and `payment_method.saved`, if saving the payment method was requested.

## Idempotence-Key

`Idempotence-Key` is required for `POST` and `DELETE`. The value must be unique per intent to create a payment or refund, up to 64 characters. YooKassa stores the idempotence result for 24 hours: a repeat with the same key and the same body returns the result of the original request.

In code the key is generated once when the payment intent is created and stored as `payments.payment_intent_key`. For a refund the key is stored in `refunds.idempotence_key` before calling `POST /v3/refunds`.

## Redirect flow

1. The user taps pay in Telegram.
2. The bot creates a payment in YooKassa.
3. The bot stores `payments.status='pending'`.
4. The bot sends a button with `confirmation_url`.
5. The user pays on the YooKassa side.
6. The bot learns the result through polling, not through `return_url`.

## Status polling

```http
GET /v3/payments/{payment_id}
Authorization: Basic base64(shopId:secretKey)
Accept: application/json
```

Payment statuses:

| Status | Action |
|---|---|
| `pending` | Nothing, retry on the next tick |
| `waiting_for_capture` | Not used with `capture=true`; keep pending and log as a not-ready status |
| `succeeded` + `paid=true` | Idempotently move the payment to `succeeded`, create a subscription, activate recurring on `auto_renew + saved`, notify the user |
| `canceled` | Move our pending payment to `failed`, create no subscription; for recurring, a temporary cancel gives a retry, revoked/mandate blocks the profile |

Finalization is protected by our DB: `pending -> succeeded` under `FOR UPDATE`, with a guard on status and `subscription_id IS NULL`; for a direct/recurring intent we must also check `metadata.user_id`. Repeated polling does not create a second subscription.

## 54-FZ receipts and email

Important clarification: the YooKassa API technically allows creating a payment without `receipt` and without an email. But if the shop has "Receipts from YooKassa" enabled and YooKassa issues the receipt, the receipt data must be passed in `receipt` when the payment is created.

For our chosen mode:
- `receipt.items` is required;
- `items[].description`, `quantity`, `amount`, `vat_code` are required;
- `payment_subject=service`, `payment_mode=full_payment`, `measure=piece` are passed explicitly;
- "Receipts from YooKassa" needs `receipt.customer.email`;
- we do not send `tax_system_code` if the shop has a single tax system and YooKassa does not require the field; if a sole proprietor has several tax systems, set it explicitly via env and check it on a test receipt.

That is why the bot collects and stores `users.payment_email` before the first payment. This is not a limitation of the payment itself, but our fiscal decision for the current shop.

## Recurring payment method

Default-on recurring works only in the direct REST flow. With `RECURRING_PAYMENTS_ENABLED` on, the first payment sends `save_payment_method:"true"` and `metadata.auto_renew=true`. We store `payment_method.id` only if the local metadata contains `auto_renew=true`, YooKassa returned `payment_method.saved=true`, and the id is non-empty.

Recurring charges are created as a separate `POST /v3/payments` with `payment_method_id`, the same receipt and a deterministic `Idempotence-Key=recurring:<user_id>:<next_charge_at_unix>`. Before the external POST the DB moves the profile to `charging`; a stale intent without a provider id older than 23 hours blocks the profile, so the charge is not repeated after the YooKassa idempotence TTL.

## Refunds

```http
POST /v3/refunds
Authorization: Basic base64(shopId:secretKey)
Idempotence-Key: <refund-intent-key>
Content-Type: application/json
```

Full refund body:

```json
{
  "payment_id": "2f8f...",
  "amount": { "value": "1990.00", "currency": "RUB" }
}
```

The MVP supports only a full refund. We take `payment_id` from `payments.provider_payment_charge_id`, because for a direct payment link it is the real YooKassa `payment.id`.

## Webhooks vs polling

YooKassa supports the events `payment.succeeded`, `payment.canceled`, `refund.succeeded`. With Basic Auth, notifications are configured in the merchant dashboard. The `/v3/webhooks` API belongs to the OAuth/partner scenario, not to our regular shop.

A webhook can be added later if polling delay turns out to be bad for UX. In the first slice polling is simpler: no public endpoint and no new infrastructure.

## Test mode and smoke

Minimal smoke test:
1. put the test `YOOKASSA_SHOP_ID`, `YOOKASSA_SECRET_KEY`, `PAYMENT_RETURN_URL` and the receipt vars in env;
2. tap pay in `@transcrib_dev_bot`;
3. enter an email and check `users.payment_email`;
4. open `confirmation_url`;
5. pay with a test card;
6. wait for the polling message about activation and, with recurring on, the next charge date;
7. check `/balance`: auto-renewal status and the buttons «Отключить автопродление» ("Disable auto-renewal") and «Обновить подписку» ("Renew subscription");
8. check the DB: `pending -> succeeded`, `provider_payment_charge_id=payment.id`, `payment_intent_key` filled, `subscription_id IS NOT NULL`;
9. repeated polling does not create a second subscription;
10. make a full refund by `provider_payment_charge_id`;
11. in a separate negative run check that a `canceled` unpaid payment becomes `failed` without a subscription; for recurring, a temporary cancel gives `charge_failed`, and a stale unknown result older than 23 hours gives `blocked`.

## Dangerous assumptions

- Do not treat `return_url` as payment confirmation.
- Do not wait for Telegram `successful_payment` in the new flow.
- Do not use `/v3/webhooks` for a Basic Auth shop.
- Do not generate a new `Idempotence-Key` when retrying the same operation.
- Do not confuse rubles as a string in YooKassa (`"1990.00"`) with kopecks as an integer in the old Telegram invoice (`199000`).

## Official sources

- Interaction format: https://yookassa.ru/developers/using-api/interaction-format
- Quick start payments: https://yookassa.ru/developers/payment-acceptance/getting-started/quick-start
- Payment process and statuses: https://yookassa.ru/developers/payment-acceptance/getting-started/payment-process
- Webhooks: https://yookassa.ru/developers/using-api/webhooks
- Refunds: https://yookassa.ru/developers/payment-acceptance/after-the-payment/refunds
- Test mode: https://yookassa.ru/developers/payment-acceptance/testing-and-going-live/testing
- Receipt values: https://yookassa.ru/developers/payment-acceptance/receipts/54fz/yoomoney/parameters-values
- OpenAPI YAML: https://yookassa.ru/developers/api/yookassa-openapi-specification.yaml
