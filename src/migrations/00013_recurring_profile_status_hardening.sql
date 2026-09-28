-- +goose Up
-- Hardening recurring worker states: charging = in-flight attempt, blocked = manual reconciliation required.

ALTER TABLE recurring_profiles
    DROP CONSTRAINT IF EXISTS recurring_profiles_status_check;

ALTER TABLE recurring_profiles
    ADD CONSTRAINT recurring_profiles_status_check
    CHECK (status IN ('active','charging','charge_failed','blocked','disabled'));

-- +goose Down
UPDATE recurring_profiles
SET status = 'charge_failed'
WHERE status IN ('charging','blocked');

ALTER TABLE recurring_profiles
    DROP CONSTRAINT IF EXISTS recurring_profiles_status_check;

ALTER TABLE recurring_profiles
    ADD CONSTRAINT recurring_profiles_status_check
    CHECK (status IN ('active','disabled','charge_failed'));
