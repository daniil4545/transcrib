# YooKassa payments for transcrib

> **Canon for:** F6/F7, YooKassa direct payment links, default-on saving of the payment method for auto-renewal, 54-FZ receipt, confirmation polling, manual full refund, PCI/152-FZ.
> **Mirrors:** `architecture.md` §4/§8.5/§8.6, `db.md` §3.4/§5.4/§5.9, `../README.md`.
> **API contract:** the current direct flow is [`docs/api/yookassa/payment_links.md`](api/yookassa/payment_links.md); the former Telegram Payments integration is kept as a legacy reference in [`docs/api/yookassa/API.md`](api/yookassa/API.md).
> **Do not duplicate here:** transaction SQL (`db.md` §5.4/§5.9), Job state machine (`architecture.md` §7).
>
> **Status:** direct payment links are implemented on top of the minute runtime: the bot creates the YooKassa payment itself via REST, sends the `confirmation_url`, confirms payment by polling and credits 900 subscription minutes; trial limit = 180 minutes. In prod `RECURRING_PAYMENTS_ENABLED=true`: the first direct payment requests saving the payment method, after success it enables auto-renewal and reports the date of the next charge. The live bank-card smoke test of 2026-07-09 passed: a 1990 RUB payment was finalized with `recurring_active=true`, created a `900/900` subscription, and self-service disable moved the profile to `disabled` and cleared `payment_method_id`. In the MVP refunds are done by hand in the YooKassa dashboard; the REST refund was verified technically, but an admin flow is not a go-live gate. The legacy Telegram Payments code remains as the rollback baseline and test history.

## 1. User flow

1. The user presses "Оформить подписку" ("Subscribe") in Telegram and then the single "Оплатить" ("Pay") button.
2. If the user has no saved email, the bot asks for an email for the receipt.
3. The bot atomically creates a local direct intent: under `users FOR UPDATE` it checks for a fresh direct
   `payments.status='pending'` and inserts a `payments.status='pending'` row with
   `payment_intent_key = Idempotence-Key`, but without `provider_payment_charge_id` for now.
   A second parallel click waits for the lock, sees this row and does not call YooKassa.
4. The bot creates the payment via `POST /v3/payments` with `confirmation.type=redirect`, `capture=true`, `receipt` and `metadata.user_id`. When `RECURRING_PAYMENTS_ENABLED=true` it additionally sends `save_payment_method:"true"` and `metadata.auto_renew=true`; when `false` these fields are not sent.
5. After the YooKassa response the bot fills in the local intent: `provider_payment_charge_id = payment.id`,
   `raw_payload`, `metadata`, amount and currency. If the external `POST` failed before a payment id was received,
   the local intent is moved to `failed`.
6. The bot sends the "Перейти к оплате" ("Go to payment") button pointing to `confirmation.confirmation_url`.
7. The user pays on the YooKassa side.
8. `PaymentPoller` checks `GET /v3/payments/{id}` and, for a direct/recurring intent, strictly compares `metadata.user_id`.
9. On `succeeded && paid=true` `FinalizeYookassaPayment` moves the payment to `succeeded`, creates a new `subscriptions` row and the bot sends the period end date. If `metadata.auto_renew=true`, `payment_method.saved=true` and `payment_method.id` is non-empty, it saves `payments.payment_method_id`, activates `recurring_profiles` and reports the date of the next charge.
10. If `RECURRING_PAYMENTS_ENABLED=true`, `RecurringWorker` selects due `recurring_profiles`, atomically moves the profile to `charging` and creates a local pending intent before the external POST. Then it creates a merchant-initiated payment with `payment_method_id`, `capture=true`, `receipt`, `metadata.auto_renew=true` and fills in the provider id; after that the same `PaymentPoller` finalizes the payment into a new subscription row.
11. On `canceled && paid=false` the payment becomes `failed` and no subscription is created. A recurring failure is determined only by the local `payment_intent_key LIKE 'recurring:%'`: a canceled direct payment with `metadata.auto_renew=true` does not change `recurring_profiles`. For a recurring payment, a temporary reason moves the profile to `charge_failed` with a retry in about 24 hours; revoked/mandate reasons move the profile to `blocked` with no automatic retry. The current paid period is not deactivated.

Why polling and not `return_url`: the user may pay and close the page. `return_url` is needed only for the UX return, the source of truth remains the payment object in YooKassa.

## 2. Connection and env

Minimum working set for direct payments:

| Variable | Purpose |
|---|---|
| `YOOKASSA_SHOP_ID` | YooKassa shop ID |
| `YOOKASSA_SECRET_KEY` | secret key for Basic Auth |
| `PAYMENT_RETURN_URL` | return URL after the payment form, not a payment confirmation |
| `PAYMENT_POLL_INTERVAL_SECONDS` | polling frequency for pending payments and recurring worker tick, default 30 |
| `RECURRING_PAYMENTS_ENABLED` | enables merchant-initiated auto-charges; default `false` |
| `RECEIPT_VAT_CODE` | VAT rate in the receipt, default 1 |
| `RECEIPT_PAYMENT_SUBJECT` | payment subject, for us `service` |
| `RECEIPT_PAYMENT_MODE` | payment mode, for us `full_payment` |
| `RECEIPT_MEASURE` | unit of measure, default `piece` |

`PROVIDER_TOKEN` from BotFather is no longer needed for the target payment flow. It is kept only for legacy `sendInvoice` tests and historical context.

`YOOKASSA_SHOP_ID`, `YOOKASSA_SECRET_KEY` and `PAYMENT_RETURN_URL` are set together: if only part of the direct config is set, the application must fail at startup rather than run in a half-configured payment mode.

## 3. 54-FZ receipt and email

The YooKassa API technically allows creating a payment without `receipt` and without an email. But in our chosen "Receipts from YooKassa" mode YooKassa issues the receipt, so we pass `receipt` when creating the payment.

For the current shop the bot collects `users.payment_email` before the first payment and reuses it on renewal. This is not a limitation of the payment itself but our fiscal decision: without an email we cannot pass `receipt.customer.email` for the receipt in the chosen mode.
User offer #7: 1 990 RUB/month for 15 hours, trial 3 hours. Internal accounting is in minutes: a successful payment creates a subscription with a limit of 900 minutes, trial limit = 180 minutes. The legacy `meetings_*` fields in the DB remain only for compatibility and a future cleanup.

Receipt shape in `POST /v3/payments`:

```json
{
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
```

(The `description` is the Russian receipt line shown to the customer: "15 hours for 1 month".)

The currency unit trap remains: in the YooKassa REST API the amount is in rubles as a string with two decimals (`"1990.00"`), inside the DB the amount is in kopecks as `BIGINT` (`199000`).

## 4. Idempotency and DB

Keys:

- `payment_intent_key` - our `Idempotence-Key` for `POST /v3/payments`;
- `provider_payment_charge_id` - the real YooKassa `payment.id`, used for polling; for a manual refund the operator finds this payment in the YooKassa dashboard;
- `telegram_payment_charge_id` - legacy field for the old Telegram Payments flow, nullable.

Invariants:

- creation of a pending payment is protected by the uniqueness of `payment_intent_key` and `provider_payment_charge_id`;
- finalization is protected by a transactional `pending -> succeeded` guard under `FOR UPDATE`;
- repeated polling of the same `payment.id` does not create a second subscription;
- on a manual refund the operator deactivates exactly the subscription tied to the refunded payment and disables the user's auto-renewal if it was enabled.

Detailed SQL patterns: `db.md` §5.4/§5.9.

## 5. Refunds

Standard MVP process: the operator makes the refund by hand in the YooKassa dashboard by `payments.provider_payment_charge_id`
and then manually deactivates the related subscription. This is simpler and more reliable for a closed beta, where the refund
volume is small and a separate admin flow is not needed for launch.

Operator checklist for a manual refund:

1. In the DB find `payments.provider_payment_charge_id`, `payments.subscription_id` and `payments.user_id` for the payment being refunded.
2. In the YooKassa dashboard find the payment by `provider_payment_charge_id` and make a full refund of 1990 RUB.
3. In the DB deactivate only the related row `subscriptions.id = payments.subscription_id`: set `expires_at = now()`.
4. For this user disable auto-renewal if a `recurring_profiles` row exists: set `status='disabled'`, `payment_method_id=NULL`, `disabled_at=now()`.
5. Check `/balance`: the refunded subscription must not grant access, other paid periods must not be affected.

The technical REST path has been verified by a smoke test and may become a future admin flow:

```http
POST /v3/refunds
Authorization: Basic <shopId:secretKey>
Idempotence-Key: <refund-intent-key>
Content-Type: application/json
```

```json
{
  "payment_id": "2f8f...",
  "amount": { "value": "1990.00", "currency": "RUB" }
}
```

In the direct flow `payment_id` comes from `payments.provider_payment_charge_id`, because it is the real YooKassa `payment.id`. The old risky Telegram assumption `provider_payment_charge_id == yookassa payment.id` is no longer needed.

- **The MVP supports full refunds only.** Partial refunds are out of scope (`product.md` §6). On a full refund we do NOT put `receipt` in the request: YooKassa builds the refund receipt from the original payment itself.
- **`Idempotence-Key` is deterministic** per refund intent: it is stored in the column `refunds.idempotence_key` (our UUID v4, generated in phase 1 `BeginRefund` BEFORE the `POST`, see `db.md` §3.5/§5.9), it is **not** derived from the PK `refunds.id` (which appears only after INSERT) and is not fresh on every retry, otherwise the money is refunded twice. The key TTL is 24h; for a retry older than a day, check the status first (`api/yookassa/API.md` §3/§14).
- **Refund status** is usually final already in the `POST` response (`succeeded`/`canceled`), but the `canceled` branch (`cancellation_details`) must be handled. The current code has no separate `GET /v3/refunds/{id}` polling; if YooKassa returns an intermediate status, the operator must check the refund in the dashboard before `FinalizeRefund`.
- Refund window: formally up to 3 years; for cards/Mir Pay the acquirer may reject refunds older than ~15 months (not a blocker for a monthly subscription).
- The user gets **no automatic notification** in Telegram: the bot sends the message itself.
- **The refund is two-phase** (`db.md` §5.9): phase 1 `BeginRefund` creates a `refunds` row with our deterministic `idempotence_key` (primary idempotency, UNIQUE) BEFORE calling YooKassa; phase 2 `FinalizeRefund` stores `provider_refund_id` from the response (nullable, partial UNIQUE: protection against repeated finalization). `idempotence_key` is primary, `provider_refund_id` is known only from the response.
- **Deactivating the subscription on refund** is mandatory: otherwise the user got the money back and keeps using the service. The atomic REST transaction is in `db.md` §5.9: `payments.status='refunded'` + `subscriptions.expires_at=now()` on **exactly the row that this payment bought** + `recurring_profiles.status='disabled'`, `payment_method_id=NULL` for the user. On a manual refund through the YooKassa dashboard the operator performs the same business steps by hand: deactivate the related subscription and disable auto-renewal. Active Jobs are not canceled (the bot is already connected to the meeting), new ones will not be accepted against the refunded row.
- In the hourly model #7, a refund of the original subscription mid-job does not charge the next subscription and creates no debt: an active Job may finish with `billed_minutes` less than the actual duration if the original source is already deactivated or lacks extra minutes.

The MVP supports only a full manual refund. Partial refunds, chargeback automation and a separate
admin command `/refund` are out of scope.

## 6. Recurring payments

Slice #9d implements default-on recurring on top of the direct REST flow. With `RECURRING_PAYMENTS_ENABLED=true` the main payment requests saving the payment method (`save_payment_method:"true"`) and writes `metadata.auto_renew=true`; with `false` it is a regular one-off payment. `metadata.auto_renew=true` on a direct payment only means the intent to enable recurring after a successful payment; a canceled direct payment is not counted as a recurring failure. A reusable method is saved only if the local metadata contains `auto_renew=true`, YooKassa returned `payment_method.saved=true` and `payment_method.id` is non-empty.

The worker is enabled only when `RECURRING_PAYMENTS_ENABLED=true`. The due selection takes `active|charge_failed` profiles with a non-empty `payment_method_id`, and also `charging` profiles with a local pending intent without a provider id (for crash recovery), with `next_charge_at <= now()`, and skips the period if there is already a succeeded payment or a pending payment with a filled `provider_payment_charge_id` for the same deterministic key `recurring:<user_id>:<next_charge_at_unix>`. Before the external POST `StartRecurringPaymentIntent` atomically moves the profile to `charging` and creates a local pending intent; if the user managed to disable the profile or `next_charge_at` changed, the external POST is not made. A successful recurring payment goes through the shared `PaymentPoller` and creates a new `subscriptions` row for 900 minutes with an independent `expires_at = now()+30 days` (no chain from the previous period, `db.md` §3.3/§5.4). After finalization `next_charge_at` is set to the user's effective expiry: `MAX(expires_at)` over active subscriptions, so that `/balance` and the next charge date match.

On a confirmed temporary `canceled`, the recurring intent atomically moves the local payment to `failed` and the profile to `status='charge_failed'` with `last_error` and `next_charge_at = now()+24h`; we do not touch the current paid period. If the result of the external `POST /payments` is unknown before `payment.id` is received, or attaching the provider id in the DB temporarily failed, the profile returns to `charge_failed`, and the next tick within 23 hours repeats the same `POST` with the same `Idempotence-Key`. After 23 hours the worker does not repeat the external POST with the same key: the local intent is moved to `failed`, the profile to `blocked`, manual review is needed. `/balance` shows the auto-renewal status (`active`, `charging`, `charge_failed`, `blocked`, `disabled` or not enabled); when `RECURRING_PAYMENTS_ENABLED` is off it does not show the next charge date and keeps only a neutral cleanup status for an existing profile. The "Отключить автопродление" ("Disable auto-renewal") button moves the profile to `disabled` and clears the saved `payment_method_id`; the "Обновить подписку" ("Renew subscription") button starts a regular payment for a new period.

YooKassa live recurring requires auto-payments to be enabled on the shop side through a YooKassa manager; the test store supports the scenario by default. Connection history: on 2026-07-06 support said that auto-payments were not yet connected for the shop; the live attempt of 2026-07-07 with `RECURRING_PAYMENTS_ENABLED=true` returned 403 `This store can't make recurring payments`, so prod was temporarily returned to one-off payment. After YooKassa confirmed bank cards on 2026-07-09, the prod redeploy on `0.0.3.4` started the worker (`recurring_worker_started`), a live payment was finalized with `recurring_active=true`, `payments.payment_method_id IS NOT NULL`, `recurring_profiles.status='active'`, `next_charge_at=2026-08-08 16:10:57 UTC`. Then self-service disable was verified: `recurring_profiles.status='disabled'`, `payment_method_id IS NULL`, `disabled_at IS NOT NULL`, the worker's due selection does not find the profile. The YooKassa requirement for self-service unbinding of the payment method without contacting support is closed: `/disable_autorenew` and the "Отключить автопродление" button (`tg.go`) clear the local `payment_method_id`; for screenshots the button is visible on `/balance` even before a saved payment method appears. Watch the first real auto-charge.

For runtime diagnostics without reading secrets, structured logs are used: `recurring_worker_started` confirms the worker started, `recurring_profiles_claimed` shows the due profiles found, `recurring_provider_payment_created`/`recurring_provider_payment_attached` close the external POST and the provider id attach, `payment_poll_finalized` confirms subscription activation, `payment_poll_recurring_failure_marked` records a retryable canceled recurring payment, `payment_poll_recurring_blocked` records a non-retryable canceled recurring payment, `recurring_intent_blocked` shows the TTL cutoff of a pending intent without a provider id, `refund_finalized` records a successful/failed/canceled refund and the auto-renewal disable flag.

## 7. Smoke tests before launch

Test smoke on `@transcrib_dev_bot`:

1. In the test env set the test `YOOKASSA_SHOP_ID`, `YOOKASSA_SECRET_KEY`, `PAYMENT_RETURN_URL`; to check auto-charging, separately enable `RECURRING_PAYMENTS_ENABLED=true`.
2. Recreate the app / roll out the branch to test.
3. Press "Оформить подписку" and check the single "Оплатить" button; with the flag enabled the payment must request saving the payment method.
4. If no email is saved, send a valid email.
5. Open the `confirmation_url` button.
6. Pay with a YooKassa test card.
7. Wait for the message with the subscription end date; with auto-renewal enabled the message must contain the next payment date.
8. Check `/balance` and the DB: `payments.status='succeeded'`, `subscription_id IS NOT NULL`, `provider_payment_charge_id` is filled, `minutes_balance=900`. For recurring additionally: `/balance` shows auto-renewal enabled and the disable button, `payments.payment_method_id IS NOT NULL`, `recurring_profiles.status='active'`, `next_charge_at` matches the "Действует до" ("Valid until") date in `/balance`.
9. For the recurring smoke test: temporarily make the profile due in the test DB or wait until it is due, make sure the worker created a new pending payment with `payment_intent_key LIKE 'recurring:%'`, polling moved it to `succeeded` and added a new `subscriptions` row. Separately check canceled/insufficient funds: the profile is `charge_failed`, the paid period is not deactivated, `/balance` shows the problem and keeps the disable button.
10. Press "Отключить автопродление" or run `/disable_autorenew`: `recurring_profiles.status='disabled'`, `payment_method_id IS NULL`, `disabled_at IS NOT NULL`, new auto-charges are not selected by the worker; the current subscription stays active.
11. For the manual refund smoke test: find the payment by `provider_payment_charge_id` in the YooKassa dashboard, make a full refund, deactivate the related subscription and make sure auto-renewal is disabled.

Live smoke on `@transcrib_bot`:

1. Before the public launch check `TRIAL_HOURS_LIMIT=3` and `SUBSCRIPTION_HOURS_PER_PERIOD=15`.
2. In Coolify prod set the live `YOOKASSA_SHOP_ID`, `YOOKASSA_SECRET_KEY`, `PAYMENT_RETURN_URL` together
   and check the receipt config.
3. Redeploy prod and check that the app starts without config errors.
4. Create a live payment link without paying and make sure the pending payment is created without errors.
5. For the full go-live smoke test, make a minimal live payment at the regular price or at an agreed
   live-smoke price.
6. Make sure polling activated the subscription; with recurring enabled additionally check
   `payments.payment_method_id IS NOT NULL`, `recurring_profiles.status='active'` and that
   `next_charge_at` matches the effective expiry.
7. Check self-service disable: `recurring_profiles.status='disabled'`,
   `payment_method_id IS NULL`, `disabled_at IS NOT NULL`.
8. If the live smoke test requires rolling the money back, make a manual refund in the YooKassa dashboard, deactivate the related subscription and disable the user's auto-renewal with erasure of `payment_method_id`.

## 8. Compliance

| Topic | Requirement |
|---|---|
| PCI DSS | We stay in SAQ A: the card form is on the YooKassa side, we neither see nor store PAN/CVV. |
| 152-FZ | In the payment context we store the email for the receipt (`users.payment_email`, raw provider payload) and a reusable `payment_method_id` only after a default-on payment with the flag enabled and `payment_method.saved=true`; when auto-renewal is disabled the token is erased. Do not log the email unless necessary. |
| 54-FZ | In the "Receipts from YooKassa" mode we pass `receipt` in `POST /v3/payments`; we do not run our own online cash register. |

## 9. Sources

- Direct payment links contract: [`api/yookassa/payment_links.md`](api/yookassa/payment_links.md)
- Legacy Telegram Payments reference: [`api/yookassa/API.md`](api/yookassa/API.md)
- YooKassa official quick start: https://yookassa.ru/developers/payment-acceptance/getting-started/quick-start
- Payment process and statuses: https://yookassa.ru/developers/payment-acceptance/getting-started/payment-process
- Refunds: https://yookassa.ru/developers/payment-acceptance/after-the-payment/refunds
- Test mode: https://yookassa.ru/developers/payment-acceptance/testing-and-going-live/testing
- Receipt values: https://yookassa.ru/developers/payment-acceptance/receipts/54fz/yoomoney/parameters-values
