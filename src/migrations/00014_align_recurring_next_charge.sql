-- +goose Up
-- Active auto-renewal must not charge before the currently paid effective expiry.

UPDATE recurring_profiles rp
SET next_charge_at = active_subs.expires_at,
    updated_at = now()
FROM (
    SELECT user_id, MAX(expires_at) AS expires_at
    FROM subscriptions
    WHERE expires_at > now()
    GROUP BY user_id
) active_subs
WHERE rp.user_id = active_subs.user_id
  AND rp.status = 'active'
  AND rp.next_charge_at <> active_subs.expires_at;

-- +goose Down
-- Data repair only. The previous mismatched next_charge_at value is not recoverable.
