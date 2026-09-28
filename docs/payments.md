# Платежи ЮKassa для transcrib

> **Канон для:** F6/F7, direct payment links ЮKassa, default-on сохранение payment method для автопродления, чек 54-ФЗ, polling подтверждения, ручной full refund, PCI/152-ФЗ.
> **Зеркала:** `architecture.md` §4/§8.5/§8.6, `db.md` §3.4/§5.4/§5.9, `../README.md`.
> **API-контракт:** актуальный direct flow — [`docs/api/yookassa/payment_links.md`](api/yookassa/payment_links.md); прежняя Telegram Payments связка — [`docs/api/yookassa/API.md`](api/yookassa/API.md) как legacy-справочник.
> **Не дублировать здесь:** SQL транзакций (`db.md` §5.4/§5.9), state machine Job (`architecture.md` §7).
>
> **Статус:** direct payment links реализованы поверх minute runtime: бот сам создаёт платёж в ЮKassa через REST, отправляет `confirmation_url`, подтверждает оплату polling'ом и начисляет 900 минут подписки; trial limit = 180 минут. В prod `RECURRING_PAYMENTS_ENABLED=true`: первый direct payment запрашивает сохранение способа оплаты, после success включает автопродление и сообщает дату следующего списания. Live bank-card smoke 2026-07-09 прошёл: платёж на 1990 ₽ финализировался с `recurring_active=true`, создал `900/900` subscription, а self-service disable перевёл профиль в `disabled` и очистил `payment_method_id`. Возвраты в MVP выполняются руками через ЛК ЮKassa; REST refund проверен технически, но admin-flow не является go-live gate. Legacy Telegram Payments код остаётся rollback baseline и тестовой историей.

## 1. Пользовательский поток

1. Пользователь нажимает «Оформить подписку» в Telegram и одну кнопку «Оплатить».
2. Если у пользователя нет сохранённого email, бот просит email для чека.
3. Бот атомарно создаёт локальный direct intent: под `users FOR UPDATE` проверяет свежий direct
   `payments.status='pending'` и вставляет строку `payments.status='pending'` с
   `payment_intent_key = Idempotence-Key`, но пока без `provider_payment_charge_id`.
   Второй параллельный клик ждёт lock, видит эту строку и не вызывает ЮKassa.
4. Бот создаёт платёж через `POST /v3/payments` с `confirmation.type=redirect`, `capture=true`, `receipt` и `metadata.user_id`. При `RECURRING_PAYMENTS_ENABLED=true` дополнительно передаёт `save_payment_method:"true"` и `metadata.auto_renew=true`; при `false` эти поля не отправляются.
5. После ответа ЮKassa бот дозаполняет локальный intent: `provider_payment_charge_id = payment.id`,
   `raw_payload`, `metadata`, сумму и currency. Если внешний `POST` упал до получения payment id,
   локальный intent переводится в `failed`.
6. Бот отправляет кнопку «Перейти к оплате» на `confirmation.confirmation_url`.
7. Пользователь оплачивает на стороне ЮKassa.
8. `PaymentPoller` проверяет `GET /v3/payments/{id}` и для direct/recurring intent строго сверяет `metadata.user_id`.
9. При `succeeded && paid=true` `FinalizeYookassaPayment` переводит payment в `succeeded`, создаёт новую `subscriptions` строку и бот пишет дату окончания периода. Если `metadata.auto_renew=true`, `payment_method.saved=true` и `payment_method.id` непустой, сохраняет `payments.payment_method_id`, активирует `recurring_profiles` и сообщает дату следующего списания.
10. Если `RECURRING_PAYMENTS_ENABLED=true`, `RecurringWorker` выбирает due `recurring_profiles`, атомарно переводит профиль в `charging` и создаёт локальный pending intent до внешнего POST. Затем создаёт merchant-initiated payment с `payment_method_id`, `capture=true`, `receipt`, `metadata.auto_renew=true` и дозаполняет provider id; дальше тот же `PaymentPoller` финализирует payment в новую subscription-строку.
11. При `canceled && paid=false` payment становится `failed`, подписка не создаётся. Recurring failure определяется только локальным `payment_intent_key LIKE 'recurring:%'`: canceled direct payment с `metadata.auto_renew=true` не меняет `recurring_profiles`. Для recurring payment временная причина переводит профиль в `charge_failed` с retry примерно через 24 часа; revoked/mandate причины переводят профиль в `blocked` без автоматического retry. Текущий оплаченный период не деактивируется.

Почему polling, а не `return_url`: пользователь может оплатить и закрыть страницу. `return_url` нужен только для UX-возврата, источником истины остаётся объект payment в ЮKassa.

## 2. Подключение и env

Минимальный рабочий набор для direct payments:

| Переменная | Назначение |
|---|---|
| `YOOKASSA_SHOP_ID` | ShopID магазина ЮKassa |
| `YOOKASSA_SECRET_KEY` | secret key для Basic Auth |
| `PAYMENT_RETURN_URL` | URL возврата после формы оплаты, не подтверждение оплаты |
| `PAYMENT_POLL_INTERVAL_SECONDS` | частота polling pending-платежей и recurring worker tick, default 30 |
| `RECURRING_PAYMENTS_ENABLED` | включает merchant-initiated автосписания; default `false` |
| `RECEIPT_VAT_CODE` | ставка НДС в чеке, default 1 |
| `RECEIPT_PAYMENT_SUBJECT` | предмет расчёта, для нас `service` |
| `RECEIPT_PAYMENT_MODE` | способ расчёта, для нас `full_payment` |
| `RECEIPT_MEASURE` | единица измерения, default `piece` |

`PROVIDER_TOKEN` из BotFather больше не нужен для целевого приёма оплаты. Он оставлен только для legacy `sendInvoice`-тестов и исторического контекста.

`YOOKASSA_SHOP_ID`, `YOOKASSA_SECRET_KEY` и `PAYMENT_RETURN_URL` задаются вместе: если задана часть direct-конфига, приложение должно падать на старте, а не работать в полунастроенном платёжном режиме.

## 3. Чек 54-ФЗ и email

ЮKassa API технически позволяет создать payment без `receipt` и без email. Но в нашем выбранном режиме «Чеки от ЮKassa» чек формирует ЮKassa, поэтому при создании платежа передаём `receipt`.

Для текущего магазина бот собирает `users.payment_email` перед первой оплатой и переиспользует его при продлении. Это не ограничение самого платежа, а наше фискальное решение: без email мы не сможем передать `receipt.customer.email` для чека в выбранном режиме.
Пользовательский оффер #7: 1 990 ₽/мес за 15 часов, trial 3 часа. Внутренний учет идет в минутах: successful payment создает подписку с лимитом 900 минут, trial limit = 180 минут. Legacy `meetings_*` поля в БД остаются только для совместимости и будущего cleanup.

Форма receipt в `POST /v3/payments`:

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

Ловушка валютных единиц остаётся: в ЮKassa REST сумма — рубли строкой с двумя знаками (`"1990.00"`), внутри БД сумма — копейки `BIGINT` (`199000`).

## 4. Идемпотентность и БД

Ключи:

- `payment_intent_key` — наш `Idempotence-Key` для `POST /v3/payments`;
- `provider_payment_charge_id` — настоящий `payment.id` ЮKassa, по нему делаем polling; для ручного возврата оператор ищет этот payment в ЛК ЮKassa;
- `telegram_payment_charge_id` — legacy-поле для старого Telegram Payments потока, nullable.

Инварианты:

- создание pending payment защищено уникальностью `payment_intent_key` и `provider_payment_charge_id`;
- финализация защищена транзакционным guard `pending -> succeeded` под `FOR UPDATE`;
- повторный polling того же `payment.id` не создаёт вторую подписку;
- при ручном refund оператор деактивирует ровно подписку, связанную с возвращаемым payment, и отключает автопродление пользователя, если оно было включено.

Подробные SQL-паттерны — `db.md` §5.4/§5.9.

## 5. Возвраты

Штатный MVP-процесс: возврат делается оператором руками в ЛК ЮKassa по `payments.provider_payment_charge_id`
и затем вручную деактивируется связанная подписка. Это проще и надёжнее для закрытой beta, где объём
возвратов мал, а отдельный admin-flow не нужен для запуска.

Операторский checklist ручного refund:

1. В БД найти `payments.provider_payment_charge_id`, `payments.subscription_id` и `payments.user_id` для возвращаемого платежа.
2. В ЛК ЮKassa найти payment по `provider_payment_charge_id` и сделать полный refund на 1990 ₽.
3. В БД деактивировать только связанную строку `subscriptions.id = payments.subscription_id`: поставить `expires_at = now()`.
4. Для этого пользователя отключить автопродление, если есть `recurring_profiles`: поставить `status='disabled'`, `payment_method_id=NULL`, `disabled_at=now()`.
5. Проверить `/balance`: возвращённая подписка не должна давать доступ, другие оплаченные периоды не должны быть затронуты.

Технический REST-контур проверен smoke-тестом и может стать будущим admin-flow:

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

В direct flow `payment_id` берём из `payments.provider_payment_charge_id`, потому что это настоящий `payment.id` ЮKassa. Старое рискованное допущение Telegram `provider_payment_charge_id == yookassa payment.id` больше не нужно.

- **На MVP — только полные refund'ы.** Частичные возвраты — out of scope (`product.md` §6). При полном возврате `receipt` в запрос НЕ кладём — ЮKassa формирует чек возврата из исходного платежа сама.
- **`Idempotence-Key` детерминирован** по одному refund-намерению: хранится в колонке `refunds.idempotence_key` (наш UUID v4, генерируется в фазе 1 `BeginRefund` ДО `POST` — `db.md` §3.5/§5.9), **не** выводится из PK `refunds.id` (тот появляется только после INSERT) и не свежий на каждый ретрай — иначе двойной возврат денег. TTL ключа 24ч; ретрай старше суток — сначала проверить статус (`api/yookassa/API.md` §3/§14).
- **Статус возврата** обычно финальный уже в ответе `POST` (`succeeded`/`canceled`), но `canceled`-ветку (`cancellation_details`) обработать обязательно. В текущем коде отдельный polling `GET /v3/refunds/{id}` не реализован; если ЮKassa вернёт промежуточный статус, оператор должен проверить refund в ЛК перед `FinalizeRefund`.
- Срок возврата — формально до 3 лет; по картам/Mir Pay эквайер может отклонить старше ~15 месяцев (для месячной подписки не блокер).
- В Telegram юзеру **автоматическое уведомление НЕ приходит** — бот шлёт сообщение сам.
- **Возврат двухфазный** (`db.md` §5.9): фаза 1 `BeginRefund` создаёт `refunds`-строку с нашим детерминированным `idempotence_key` (первичная идемпотентность, UNIQUE) ДО вызова ЮKassa; фаза 2 `FinalizeRefund` сохраняет `provider_refund_id` из ответа (nullable, partial-UNIQUE — защита от повторной финализации). `idempotence_key` первичен, `provider_refund_id` известен только из ответа.
- **Деактивация подписки при refund** обязательна: иначе юзер получил деньги обратно и продолжает пользоваться сервисом. Атомарная REST-транзакция — `db.md` §5.9: `payments.status='refunded'` + `subscriptions.expires_at=now()` на **ровно той строке, что купил этот платёж** + `recurring_profiles.status='disabled'`, `payment_method_id=NULL` для пользователя. При ручном refund через ЛК ЮKassa оператор делает те же бизнес-шаги вручную: деактивировать связанную subscription и отключить автопродление. Активные Job не отменяются (бот уже подключён к встрече), новые не примутся против refunded-строки.
- В почасовой модели #7 refund исходной подписки mid-job не списывает следующую подписку и не создает долг: active Job может завершиться с `billed_minutes` меньше фактической длительности, если original source уже деактивирован или в нем не хватает extra.

На MVP поддерживаем только полный ручной refund. Частичные возвраты, chargeback automation и отдельная
admin-команда `/refund` не входят в scope.

## 6. Рекуррент

Срез #9d реализует default-on recurring поверх direct REST flow. При `RECURRING_PAYMENTS_ENABLED=true` основной payment запрашивает сохранение способа оплаты (`save_payment_method:"true"`) и пишет `metadata.auto_renew=true`; при `false` это обычный разовый payment. `metadata.auto_renew=true` у direct payment означает только намерение включить recurring после успешной оплаты; canceled direct payment не считается recurring failure. Reusable method сохраняется только если локальная metadata содержит `auto_renew=true`, ЮKassa вернула `payment_method.saved=true`, а `payment_method.id` непустой.

Worker включается только при `RECURRING_PAYMENTS_ENABLED=true`. Due-выборка берёт `active|charge_failed` профили с непустым `payment_method_id`, а также `charging` с local pending intent без provider id для crash recovery, с `next_charge_at <= now()` и пропускает период, если уже есть succeeded payment или pending payment с заполненным `provider_payment_charge_id` для того же deterministic key `recurring:<user_id>:<next_charge_at_unix>`. Перед внешним POST `StartRecurringPaymentIntent` атомарно переводит профиль в `charging` и создаёт локальный pending intent; если пользователь успел отключить профиль или `next_charge_at` изменился, внешний POST не выполняется. Успешный recurring payment проходит через общий `PaymentPoller` и создаёт новую `subscriptions` строку на 900 минут с независимым `expires_at = now()+30 days` (без chain от предыдущего периода, `db.md` §3.3/§5.4). После финализации `next_charge_at` ставится в effective expiry пользователя: `MAX(expires_at)` по активным подпискам, чтобы `/balance` и дата следующего списания совпадали.

При подтверждённом временном `canceled` recurring intent атомарно переводит local payment в `failed`, а профиль в `status='charge_failed'` с `last_error` и `next_charge_at = now()+24h`; текущий оплаченный период не трогаем. Если результат внешнего `POST /payments` неизвестен до получения `payment.id` или attach provider id в БД временно не прошёл, профиль возвращается в `charge_failed`, и следующий tick в пределах 23 часов повторит тот же `POST` с тем же `Idempotence-Key`. После 23 часов worker не повторяет внешний POST с тем же key: локальный intent переводится в `failed`, профиль в `blocked`, нужен ручной разбор. `/balance` показывает статус автопродления (`active`, `charging`, `charge_failed`, `blocked`, `disabled` или не включено); при выключенном `RECURRING_PAYMENTS_ENABLED` не показывает дату следующего списания и оставляет только нейтральный cleanup-статус для existing profile. Кнопка «Отключить автопродление» переводит профиль в `disabled` и очищает сохранённый `payment_method_id`; кнопка «Обновить подписку» запускает обычную оплату нового периода.

ЮKassa live-рекуррент требует включения автоплатежей на стороне магазина через менеджера ЮKassa; test store поддерживает сценарий по умолчанию. История подключения: 2026-07-06 поддержка сообщила, что для магазина автоплатежи ещё не подключены; live attempt 2026-07-07 с `RECURRING_PAYMENTS_ENABLED=true` вернул 403 `This store can't make recurring payments`, поэтому prod временно вернули к разовой оплате. После подтверждения ЮKassa по банковским картам 2026-07-09 prod redeploy на `0.0.3.4` стартовал worker (`recurring_worker_started`), live payment финализировался с `recurring_active=true`, `payments.payment_method_id IS NOT NULL`, `recurring_profiles.status='active'`, `next_charge_at=2026-08-08 16:10:57 UTC`. Затем self-service disable проверен: `recurring_profiles.status='disabled'`, `payment_method_id IS NULL`, `disabled_at IS NOT NULL`, due-выборка worker не находит профиль. Требование ЮKassa к self-service отвязке способа оплаты без обращения в поддержку закрыто: `/disable_autorenew` и кнопка «Отключить автопродление» (`tg.go`) очищают локальный `payment_method_id`; для screenshots кнопка видима на `/balance` даже до появления сохранённого способа оплаты. Следить за первым реальным автосписанием.

Для runtime-диагностики без чтения секретов используются structured logs: `recurring_worker_started` подтверждает запуск worker, `recurring_profiles_claimed` показывает найденные due-профили, `recurring_provider_payment_created`/`recurring_provider_payment_attached` закрывают внешний POST и attach provider id, `payment_poll_finalized` подтверждает активацию подписки, `payment_poll_recurring_failure_marked` фиксирует retryable canceled recurring payment, `payment_poll_recurring_blocked` фиксирует non-retryable canceled recurring payment, `recurring_intent_blocked` показывает TTL cutoff pending intent без provider id, `refund_finalized` фиксирует successful/failed/canceled refund и флаг отключения автопродления.

## 7. Smoke перед запуском

Test smoke на `@transcrib_dev_bot`:

1. В test env задать test `YOOKASSA_SHOP_ID`, `YOOKASSA_SECRET_KEY`, `PAYMENT_RETURN_URL`; для проверки автосписания отдельно включить `RECURRING_PAYMENTS_ENABLED=true`.
2. Пересоздать app / выкатить ветку на test.
3. Нажать «Оформить подписку» и проверить единственную кнопку «Оплатить»; при включённом флаге payment должен запросить сохранение способа оплаты.
4. Если email не сохранён, отправить валидный email.
5. Открыть кнопку `confirmation_url`.
6. Оплатить тестовой картой ЮKassa.
7. Дождаться сообщения с датой окончания подписки; при включённом автопродлении сообщение должно содержать дату следующей оплаты.
8. Проверить `/balance` и БД: `payments.status='succeeded'`, `subscription_id IS NOT NULL`, `provider_payment_charge_id` заполнен, `minutes_balance=900`. Для recurring дополнительно: `/balance` показывает включённое автопродление и кнопку отключения, `payments.payment_method_id IS NOT NULL`, `recurring_profiles.status='active'`, `next_charge_at` совпадает с датой `Действует до` в `/balance`.
9. Для recurring smoke: временно сделать профиль due в test DB или дождаться due, убедиться что worker создал новый pending payment с `payment_intent_key LIKE 'recurring:%'`, polling перевёл его в `succeeded` и добавил новую `subscriptions` строку. Отдельно проверить canceled/insufficient funds: профиль `charge_failed`, оплаченный период не деактивирован, `/balance` показывает проблему и оставляет кнопку отключения.
10. Нажать «Отключить автопродление» или выполнить `/disable_autorenew`: `recurring_profiles.status='disabled'`, `payment_method_id IS NULL`, `disabled_at IS NOT NULL`, новые автосписания не выбираются worker'ом; текущая subscription остаётся активной.
11. Для ручного refund smoke: найти payment по `provider_payment_charge_id` в ЛК ЮKassa, сделать полный возврат, деактивировать связанную подписку и убедиться, что автопродление отключено.

Live smoke на `@transcrib_bot`:

1. Перед публичным запуском проверить `TRIAL_HOURS_LIMIT=3` и `SUBSCRIPTION_HOURS_PER_PERIOD=15`.
2. В Coolify prod задать live `YOOKASSA_SHOP_ID`, `YOOKASSA_SECRET_KEY`, `PAYMENT_RETURN_URL` вместе
   и проверить receipt config.
3. Redeploy prod и проверить, что app стартует без config errors.
4. Создать live payment link без оплаты и убедиться, что pending payment создаётся без ошибок.
5. Для полного go-live smoke провести минимальный live-платёж по штатной цене или согласованной
   live-smoke цене.
6. Убедиться, что polling активировал подписку; при включённом recurring дополнительно проверить
   `payments.payment_method_id IS NOT NULL`, `recurring_profiles.status='active'` и совпадение
   `next_charge_at` с effective expiry.
7. Проверить self-service disable: `recurring_profiles.status='disabled'`,
   `payment_method_id IS NULL`, `disabled_at IS NOT NULL`.
8. Если live smoke требует отката денег, сделать ручной refund в ЛК ЮKassa, деактивировать связанную подписку и отключить автопродление пользователя со стиранием `payment_method_id`.

## 8. Compliance

| Тема | Требование |
|---|---|
| PCI DSS | Мы остаёмся в SAQ A: карточная форма на стороне ЮKassa, PAN/CVV не видим и не храним. |
| 152-ФЗ | В платёжном контексте храним email для чека (`users.payment_email`, raw provider payload) и reusable `payment_method_id` только после default-on платежа с включённым флагом и `payment_method.saved=true`; при отключении автопродления токен стирается. Не логировать email без необходимости. |
| 54-ФЗ | При режиме «Чеки от ЮKassa» передаём `receipt` в `POST /v3/payments`; свою онлайн-кассу не поднимаем. |

## 9. Источники

- Direct payment links contract — [`api/yookassa/payment_links.md`](api/yookassa/payment_links.md)
- Legacy Telegram Payments справочник — [`api/yookassa/API.md`](api/yookassa/API.md)
- ЮKassa official quick start — https://yookassa.ru/developers/payment-acceptance/getting-started/quick-start
- Payment process and statuses — https://yookassa.ru/developers/payment-acceptance/getting-started/payment-process
- Refunds — https://yookassa.ru/developers/payment-acceptance/after-the-payment/refunds
- Test mode — https://yookassa.ru/developers/payment-acceptance/testing-and-going-live/testing
- Receipt values — https://yookassa.ru/developers/payment-acceptance/receipts/54fz/yoomoney/parameters-values
