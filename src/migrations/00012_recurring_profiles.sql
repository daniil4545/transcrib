-- +goose Up
-- Saved payment method for future YooKassa recurring charges.
-- The subscription itself stays in the model "one successful payment = one subscriptions row".

CREATE TABLE recurring_profiles (
    user_id BIGINT PRIMARY KEY REFERENCES users(telegram_id) ON DELETE RESTRICT,
    payment_method_id TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('active','disabled','charge_failed')),
    next_charge_at TIMESTAMPTZ NOT NULL,
    last_payment_id BIGINT REFERENCES payments(id) ON DELETE RESTRICT,
    attempts_count INTEGER NOT NULL DEFAULT 0,
    last_error TEXT,
    consent_at TIMESTAMPTZ NOT NULL,
    disabled_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX recurring_profiles_due_idx
    ON recurring_profiles (next_charge_at)
    WHERE status IN ('active','charge_failed');

-- +goose Down
DROP INDEX recurring_profiles_due_idx;
DROP TABLE recurring_profiles;
