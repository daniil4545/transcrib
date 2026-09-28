# ЮKassa payment links

Статус: актуальный рабочий контракт для слайса direct payment links, проверен по официальной документации ЮKassa 2026-06-30.

Эта статья описывает инструментарий, который используем вместо Telegram Payments provider token: прямой REST-платёж ЮKassa, redirect-ссылка пользователю, polling статуса, default-on recurring при runtime-флаге и REST-refund. Старый Telegram-контракт остаётся историческим фоном в [`API.md`](API.md).

## Цель и границы

Делаем:
- создаём платёж через `POST /v3/payments`;
- отправляем пользователю `confirmation.confirmation_url` кнопкой в Telegram;
- подтверждаем оплату polling-запросом `GET /v3/payments/{payment_id}`;
- при `succeeded && paid=true` начисляем подписку;
- при `RECURRING_PAYMENTS_ENABLED=true` первый платёж запрашивает сохранение payment method и включает recurring только если ЮKassa вернула `payment_method.saved=true`;
- при `canceled` закрываем pending payment без подписки;
- для возврата используем `POST /v3/refunds` по тому же `payment.id`.

Не делаем в первом слайсе:
- webhook `payment.succeeded`;
- двухстадийную оплату (`waiting_for_capture`);
- оплату без фискального чека, пока включён режим "Чеки от ЮKassa".

## Почему REST link + polling

REST link убирает зависимость приёма оплаты от BotFather binding и Telegram `successful_payment`. Наш ключ платежа сразу становится настоящим `payment.id` ЮKassa, поэтому refund больше не держится на допущении Telegram `provider_payment_charge_id == payment_id`.

Polling выбран первым шагом, потому что не требует входящего HTTPS endpoint, настройки webhook в личном кабинете и nginx/deploy-работы. `return_url` используем только как UX-возврат: пользователь может оплатить и закрыть страницу, поэтому источником истины остаётся статус платежа в ЮKassa.

## API, auth и среды

- Base URL: `https://api.yookassa.ru/v3`.
- Формат: REST/JSON.
- Auth: HTTP Basic Auth, username = `shopId`, password = `secretKey`.
- OAuth не нужен: он относится к партнёрским сценариям.
- Test/live магазины имеют разные `shopId` и `secretKey`; в тестовых объектах ЮKassa возвращает `test=true`.

## Создание платежа

```http
POST /v3/payments
Authorization: Basic base64(shopId:secretKey)
Idempotence-Key: <uuid-or-other-unique-key>
Content-Type: application/json
```

Минимальная рабочая форма для нашего сервиса при включённом recurring-флаге. При `RECURRING_PAYMENTS_ENABLED=false` поля `save_payment_method` и `metadata.auto_renew` не отправляются:

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

Ожидаемый ответ содержит:
- `id`: provider payment id, сохраняем в `payments.provider_payment_charge_id`;
- `status`: обычно `pending` до действия пользователя;
- `paid`: bool;
- `confirmation.confirmation_url`: ссылку, которую отправляем пользователю;
- `amount`, `metadata`, raw JSON для аудита;
- `payment_method.id` и `payment_method.saved`, если запрашивали сохранение способа оплаты.

## Idempotence-Key

`Idempotence-Key` обязателен для `POST` и `DELETE`. Значение должно быть уникальным для одного намерения создать платёж или возврат, длиной до 64 символов. ЮKassa хранит результат идемпотентности 24 часа: повтор с тем же ключом и тем же телом возвращает результат исходного запроса.

В коде ключ генерируется один раз при создании payment intent и сохраняется как `payments.payment_intent_key`. Для refund ключ сохраняется в `refunds.idempotence_key` до вызова `POST /v3/refunds`.

## Redirect flow

1. Пользователь нажимает оплату в Telegram.
2. Бот создаёт payment в ЮKassa.
3. Бот сохраняет `payments.status='pending'`.
4. Бот отправляет кнопку с `confirmation_url`.
5. Пользователь платит на стороне ЮKassa.
6. Бот узнаёт результат через polling, а не через `return_url`.

## Polling статуса

```http
GET /v3/payments/{payment_id}
Authorization: Basic base64(shopId:secretKey)
Accept: application/json
```

Статусы платежа:

| Статус | Что делаем |
|---|---|
| `pending` | Ничего, повторяем на следующем тике |
| `waiting_for_capture` | Не используется при `capture=true`; оставляем pending и логируем как неготовый статус |
| `succeeded` + `paid=true` | Идемпотентно переводим payment в `succeeded`, создаём subscription, при `auto_renew + saved` активируем recurring, уведомляем пользователя |
| `canceled` | Переводим наш pending payment в `failed`, подписку не создаём; для recurring temporary cancel даёт retry, revoked/mandate блокирует профиль |

Финализация защищена нашей БД: `pending -> succeeded` под `FOR UPDATE`, guard по статусу и `subscription_id IS NULL`; для direct/recurring intent обязательно сверяем `metadata.user_id`. Повторный polling не создаёт вторую подписку.

## Чеки 54-ФЗ и email

Важное уточнение: ЮKassa API технически позволяет создать payment без `receipt` и без email. Но если для магазина включены "Чеки от ЮKassa" и чек формирует ЮKassa, данные чека надо передавать в `receipt` при создании платежа.

Для нашего выбранного режима:
- `receipt.items` обязателен;
- `items[].description`, `quantity`, `amount`, `vat_code` обязательны;
- `payment_subject=service`, `payment_mode=full_payment`, `measure=piece` передаём явно;
- для "Чеков от ЮKassa" нужен `receipt.customer.email`.
- `tax_system_code` не отправляем, если у магазина одна СНО и ЮKassa не требует поле; если у ИП несколько СНО, задаём его явно через env и проверяем на test receipt.

Поэтому бот собирает и сохраняет `users.payment_email` перед первой оплатой. Это не ограничение самого платежа, а наше фискальное решение для текущего магазина.

## Recurring payment method

Default-on recurring работает только в direct REST flow. При включённом `RECURRING_PAYMENTS_ENABLED` первый payment отправляет `save_payment_method:"true"` и `metadata.auto_renew=true`. `payment_method.id` сохраняем только если локальная metadata содержит `auto_renew=true`, ЮKassa вернула `payment_method.saved=true`, а id непустой.

Повторные списания создаются отдельным `POST /v3/payments` с `payment_method_id`, тем же receipt и deterministic `Idempotence-Key=recurring:<user_id>:<next_charge_at_unix>`. До внешнего POST БД переводит профиль в `charging`; stale intent без provider id старше 23 часов блокирует профиль, чтобы не повторять списание после TTL идемпотентности ЮKassa.

## Refunds

```http
POST /v3/refunds
Authorization: Basic base64(shopId:secretKey)
Idempotence-Key: <refund-intent-key>
Content-Type: application/json
```

Тело полного возврата:

```json
{
  "payment_id": "2f8f...",
  "amount": { "value": "1990.00", "currency": "RUB" }
}
```

В MVP делаем только полный refund. `payment_id` берём из `payments.provider_payment_charge_id`, потому что при direct payment link это настоящий `payment.id` ЮKassa.

## Webhooks vs polling

ЮKassa поддерживает события `payment.succeeded`, `payment.canceled`, `refund.succeeded`. Для Basic Auth уведомления настраиваются в личном кабинете. API `/v3/webhooks` относится к OAuth/партнёрскому сценарию, не к нашему обычному магазину.

Webhook можно добавить позже, если задержка polling окажется плохой для UX. В первом слайсе polling проще: не нужен публичный endpoint и новая инфраструктура.

## Test mode и smoke

Минимальный smoke:
1. test `YOOKASSA_SHOP_ID`, `YOOKASSA_SECRET_KEY`, `PAYMENT_RETURN_URL` и receipt vars в env;
2. нажать оплату в `@transcrib_dev_bot`;
3. ввести email и проверить `users.payment_email`;
4. открыть `confirmation_url`;
5. оплатить тестовой картой;
6. дождаться polling-сообщения об активации и, при включённом recurring, даты следующего списания;
7. проверить `/balance`: статус автопродления, «Отключить автопродление», «Обновить подписку»;
8. проверить БД: `pending -> succeeded`, `provider_payment_charge_id=payment.id`, `payment_intent_key` заполнен, `subscription_id IS NOT NULL`;
9. повторный polling не создаёт вторую subscription;
10. сделать full refund по `provider_payment_charge_id`;
11. отдельным негативным прогоном проверить, что `canceled` unpaid payment становится `failed` без подписки; для recurring temporary cancel даёт `charge_failed`, stale unknown-result старше 23 часов даёт `blocked`.

## Опасные допущения

- Не считать `return_url` подтверждением оплаты.
- Не ждать Telegram `successful_payment` в новом потоке.
- Не использовать `/v3/webhooks` для Basic Auth магазина.
- Не генерировать новый `Idempotence-Key` при retry той же операции.
- Не путать рубли строкой в ЮKassa (`"1990.00"`) и копейки целым в старом Telegram invoice (`199000`).

## Официальные источники

- Interaction format: https://yookassa.ru/developers/using-api/interaction-format
- Quick start payments: https://yookassa.ru/developers/payment-acceptance/getting-started/quick-start
- Payment process and statuses: https://yookassa.ru/developers/payment-acceptance/getting-started/payment-process
- Webhooks: https://yookassa.ru/developers/using-api/webhooks
- Refunds: https://yookassa.ru/developers/payment-acceptance/after-the-payment/refunds
- Test mode: https://yookassa.ru/developers/payment-acceptance/testing-and-going-live/testing
- Receipt values: https://yookassa.ru/developers/payment-acceptance/receipts/54fz/yoomoney/parameters-values
- OpenAPI YAML: https://yookassa.ru/developers/api/yookassa-openapi-specification.yaml
