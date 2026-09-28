-- +goose Up
-- YooKassa compliance: disabling autorenew must erase the saved payment token, not just flip status.
ALTER TABLE recurring_profiles
    ALTER COLUMN payment_method_id DROP NOT NULL;

UPDATE recurring_profiles
SET payment_method_id = NULL
WHERE status = 'disabled';

-- +goose Down
-- Down cannot restore erased tokens; use a non-token placeholder so the old NOT NULL schema can load.
UPDATE recurring_profiles
SET payment_method_id = ''
WHERE payment_method_id IS NULL;

ALTER TABLE recurring_profiles
    ALTER COLUMN payment_method_id SET NOT NULL;
