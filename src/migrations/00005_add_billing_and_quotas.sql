-- +goose Up
-- Слайс payments (F6 триал + F7 оплата/возврат ЮKassa). Канон схемы — db.md §3.3-3.6,
-- модель «строка на платёж» 1:1 (ревизия 1.2). users НЕ трогаем: trial/notified-колонки
-- уже созданы в 00001 (повторный ADD COLUMN упал бы с column already exists).

-- subscriptions (db.md §3.3): каждый успешный платёж = отдельная строка (1:1),
-- продление = новая строка с chain expires_at; у юзера может быть несколько активных.
CREATE TABLE subscriptions (
    id               BIGSERIAL   PRIMARY KEY,
    user_id          BIGINT      NOT NULL REFERENCES users(telegram_id) ON DELETE CASCADE,
    paid_at          TIMESTAMPTZ NOT NULL,
    expires_at       TIMESTAMPTZ NOT NULL,
    meetings_limit   INTEGER     NOT NULL,
    meetings_balance INTEGER     NOT NULL,
    CHECK (meetings_balance >= 0 AND meetings_balance <= meetings_limit),
    CHECK (expires_at > paid_at)
);

-- partial: активные строки с остатком — для резерва §5.1.
CREATE INDEX subscriptions_user_active_idx
    ON subscriptions (user_id, expires_at DESC)
    WHERE meetings_balance > 0;
-- partial: F9-тик «подписка истекает через 3 дня».
CREATE INDEX subscriptions_expiring_idx
    ON subscriptions (expires_at)
    WHERE meetings_balance > 0;
-- non-partial: chain MAX(expires_at) §5.4 и агрегации по юзеру §5.1/§5.6 (любой баланс).
CREATE INDEX subscriptions_user_expiry_idx
    ON subscriptions (user_id, expires_at DESC);

-- payments (db.md §3.4): PK=id (запись нужна до charge_id в стадии pending).
-- telegram_payment_charge_id UNIQUE nullable — единственный механизм идемпотентности
-- от ретраев Telegram (§5.4 ON CONFLICT). Частичные возвраты вне скоупа: amount_refunded
-- и partially_refunded убраны (full-only 1:1).
CREATE TABLE payments (
    id                          BIGSERIAL   PRIMARY KEY,
    user_id                     BIGINT      NOT NULL REFERENCES users(telegram_id) ON DELETE RESTRICT,
    subscription_id             BIGINT      REFERENCES subscriptions(id) ON DELETE RESTRICT,
    status                      TEXT        NOT NULL CHECK (status IN (
                                                'pending','succeeded','failed','refunded'
                                            )),
    amount                      BIGINT      NOT NULL CHECK (amount > 0),
    currency                    TEXT        NOT NULL DEFAULT 'RUB' CHECK (currency = 'RUB'),
    telegram_payment_charge_id  TEXT        UNIQUE,
    provider_payment_charge_id  TEXT,
    payment_method_id           TEXT,
    invoice_payload             TEXT,
    fiscal_receipt_url          TEXT,
    metadata                    JSONB,
    raw_payload                 JSONB,
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    paid_at                     TIMESTAMPTZ,
    provider_paid_at            TIMESTAMPTZ,
    failed_at                   TIMESTAMPTZ,
    -- succeeded/refunded обязаны нести charge_id (на нём держится ON CONFLICT §5.4).
    CHECK (telegram_payment_charge_id IS NOT NULL OR status IN ('pending','failed'))
);

CREATE INDEX payments_user_paid_idx
    ON payments (user_id, paid_at DESC)
    WHERE status = 'succeeded';
CREATE INDEX payments_pending_idx
    ON payments (created_at)
    WHERE status = 'pending';

-- refunds (db.md §3.5): двухфазный возврат. Строка создаётся в pending ДО POST /v3/refunds
-- с детерминированным idempotence_key (нужен до вызова), provider_refund_id приходит из ответа.
CREATE TABLE refunds (
    id                  BIGSERIAL   PRIMARY KEY,
    payment_id          BIGINT      NOT NULL REFERENCES payments(id),
    idempotence_key     TEXT        NOT NULL UNIQUE,
    provider_refund_id  TEXT,
    amount              BIGINT      NOT NULL CHECK (amount > 0),
    status              TEXT        NOT NULL CHECK (status IN ('pending','succeeded','failed','canceled')),
    reason              TEXT,
    raw_payload         JSONB,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at        TIMESTAMPTZ
);

-- partial UNIQUE: provider_refund_id уникален у ЮKassa, но NULL на стадии pending —
-- защита от повторной финализации/webhook.
CREATE UNIQUE INDEX refunds_provider_idx
    ON refunds (provider_refund_id)
    WHERE provider_refund_id IS NOT NULL;
-- partial UNIQUE: один активный возврат на платёж — закрывает гонку двойного BeginRefund.
CREATE UNIQUE INDEX refunds_active_idx
    ON refunds (payment_id)
    WHERE status IN ('pending','succeeded');
CREATE INDEX refunds_payment_idx
    ON refunds (payment_id);

-- jobs: привязка к квоте (db.md §3.6). subscription_id NULL для триала, заполнен для подписки.
-- is_trial DEFAULT true бэкфилит существующие строки и сохраняет pre-P0 CreateJob рабочим
-- (он не знает про is_trial до Commit 2). CHECK гарантирует консистентность с subscription_id.
ALTER TABLE jobs
    ADD COLUMN subscription_id BIGINT REFERENCES subscriptions(id) ON DELETE RESTRICT,
    ADD COLUMN is_trial        BOOLEAN NOT NULL DEFAULT true;
ALTER TABLE jobs ADD CONSTRAINT jobs_quota_check CHECK (
    (is_trial = true  AND subscription_id IS NULL) OR
    (is_trial = false AND subscription_id IS NOT NULL)
);

-- dedup живых джоб (db.md §3.6, объявлен в каноне — заводится здесь). failed_summary в
-- исключениях: финальный статус освобождает URL для повторной отправки.
CREATE UNIQUE INDEX jobs_active_dedup_idx
    ON jobs (user_id, meeting_url)
    WHERE status NOT IN ('done','failed_join','failed','failed_summary');

-- +goose Down
DROP INDEX jobs_active_dedup_idx;
ALTER TABLE jobs DROP CONSTRAINT jobs_quota_check;
ALTER TABLE jobs DROP COLUMN is_trial;
ALTER TABLE jobs DROP COLUMN subscription_id;
DROP TABLE refunds;
DROP TABLE payments;
DROP TABLE subscriptions;
