-- +goose Up
CREATE TABLE users (
    telegram_id              BIGINT      PRIMARY KEY,
    trial_meetings_used      INTEGER     NOT NULL DEFAULT 0,
    trial_meetings_done      INTEGER     NOT NULL DEFAULT 0,
    consent_at               TIMESTAMPTZ,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    notified_trial_low_at    TIMESTAMPTZ,
    notified_sub_expiring_at TIMESTAMPTZ,
    notified_sub_expired_at  TIMESTAMPTZ,
    CHECK (trial_meetings_used  >= 0),
    CHECK (trial_meetings_done  >= 0),
    CHECK (trial_meetings_done <= trial_meetings_used)
);

-- +goose Down
DROP TABLE users;
