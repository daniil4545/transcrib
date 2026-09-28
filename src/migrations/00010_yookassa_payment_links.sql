-- +goose Up
-- Прямые payment links ЮKassa: платеж может быть successful без Telegram charge_id.
-- Источник идемпотентности создания ссылки — наш Idempotence-Key, payment.id ЮKassa
-- хранится в provider_payment_charge_id.

ALTER TABLE users
    ADD COLUMN payment_email TEXT;

ALTER TABLE payments
    ADD COLUMN payment_intent_key TEXT;

ALTER TABLE payments
    DROP CONSTRAINT payments_check;

ALTER TABLE payments
    ADD CONSTRAINT payments_charge_id_check CHECK (
        telegram_payment_charge_id IS NOT NULL
        OR provider_payment_charge_id IS NOT NULL
        OR status IN ('pending','failed')
    );

CREATE UNIQUE INDEX payments_provider_charge_idx
    ON payments (provider_payment_charge_id)
    WHERE provider_payment_charge_id IS NOT NULL AND provider_payment_charge_id <> '';

CREATE UNIQUE INDEX payments_intent_key_idx
    ON payments (payment_intent_key)
    WHERE payment_intent_key IS NOT NULL;

-- +goose Down
DROP INDEX payments_intent_key_idx;
DROP INDEX payments_provider_charge_idx;

ALTER TABLE payments
    DROP CONSTRAINT payments_charge_id_check;

ALTER TABLE payments
    ADD CONSTRAINT payments_check CHECK (
        telegram_payment_charge_id IS NOT NULL OR status IN ('pending','failed')
    );

ALTER TABLE payments
    DROP COLUMN payment_intent_key;

ALTER TABLE users
    DROP COLUMN payment_email;
