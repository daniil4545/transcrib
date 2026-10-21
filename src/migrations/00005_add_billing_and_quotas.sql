-- +goose Up
-- Payments slice (F6 trial + F7 YooKassa payment/refund). Schema canon: db.md §3.3-3.6,
-- the "row per payment" 1:1 model (revision 1.2). users is NOT touched: the trial/notified columns
-- were already created in 00001 (a repeated ADD COLUMN would fail with column already exists).

-- subscriptions (db.md §3.3): each successful payment = a separate row (1:1),
-- renewal = a new row with a chained expires_at; a user may have several active ones.
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

-- partial: active rows with a remainder - for the reservation §5.1.
CREATE INDEX subscriptions_user_active_idx
    ON subscriptions (user_id, expires_at DESC)
    WHERE meetings_balance > 0;
-- partial: F9 tick "subscription expires in 3 days".
CREATE INDEX subscriptions_expiring_idx
    ON subscriptions (expires_at)
    WHERE meetings_balance > 0;
-- non-partial: chain MAX(expires_at) §5.4 and per-user aggregations §5.1/§5.6 (any balance).
CREATE INDEX subscriptions_user_expiry_idx
    ON subscriptions (user_id, expires_at DESC);

-- payments (db.md §3.4): PK=id (the record is needed before charge_id at the pending stage).
-- telegram_payment_charge_id UNIQUE nullable is the only idempotence mechanism
-- against Telegram retries (§5.4 ON CONFLICT). Partial refunds are out of scope: amount_refunded
-- and partially_refunded were removed (full-only 1:1).
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
    -- succeeded/refunded must carry charge_id (ON CONFLICT §5.4 relies on it).
    CHECK (telegram_payment_charge_id IS NOT NULL OR status IN ('pending','failed'))
);

CREATE INDEX payments_user_paid_idx
    ON payments (user_id, paid_at DESC)
    WHERE status = 'succeeded';
CREATE INDEX payments_pending_idx
    ON payments (created_at)
    WHERE status = 'pending';

-- refunds (db.md §3.5): two-phase refund. The row is created in pending BEFORE POST /v3/refunds
-- with a deterministic idempotence_key (needed before the call), provider_refund_id comes from the response.
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

-- partial UNIQUE: provider_refund_id is unique at YooKassa, but NULL at the pending stage -
-- protection against repeated finalization/webhook.
CREATE UNIQUE INDEX refunds_provider_idx
    ON refunds (provider_refund_id)
    WHERE provider_refund_id IS NOT NULL;
-- partial UNIQUE: one active refund per payment - closes the race of a double BeginRefund.
CREATE UNIQUE INDEX refunds_active_idx
    ON refunds (payment_id)
    WHERE status IN ('pending','succeeded');
CREATE INDEX refunds_payment_idx
    ON refunds (payment_id);

-- jobs: binding to the quota (db.md §3.6). subscription_id is NULL for a trial, filled for a subscription.
-- is_trial DEFAULT true backfills existing rows and keeps pre-P0 CreateJob working
-- (it does not know about is_trial until Commit 2). CHECK guarantees consistency with subscription_id.
ALTER TABLE jobs
    ADD COLUMN subscription_id BIGINT REFERENCES subscriptions(id) ON DELETE RESTRICT,
    ADD COLUMN is_trial        BOOLEAN NOT NULL DEFAULT true;
ALTER TABLE jobs ADD CONSTRAINT jobs_quota_check CHECK (
    (is_trial = true  AND subscription_id IS NULL) OR
    (is_trial = false AND subscription_id IS NOT NULL)
);

-- dedup of live jobs (db.md §3.6, declared in the canon - created here). failed_summary is in
-- the exclusions: a final status frees the URL for resubmission.
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
