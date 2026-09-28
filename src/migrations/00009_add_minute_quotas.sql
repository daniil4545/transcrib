-- +goose Up
-- Schema-only step for issue #18: add minute counters next to legacy meeting counters.
-- Runtime code continues to use meetings_* until the later hourly sub-issues switch it.

ALTER TABLE users
    ADD COLUMN trial_minutes_used INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN trial_minutes_done INTEGER NOT NULL DEFAULT 0,
    ADD CONSTRAINT users_trial_minutes_used_check CHECK (trial_minutes_used >= 0),
    ADD CONSTRAINT users_trial_minutes_done_check CHECK (trial_minutes_done >= 0),
    ADD CONSTRAINT users_trial_minutes_done_lte_used_check CHECK (trial_minutes_done <= trial_minutes_used);

ALTER TABLE subscriptions
    ADD COLUMN minutes_limit INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN minutes_balance INTEGER NOT NULL DEFAULT 0,
    ADD CONSTRAINT subscriptions_minutes_limit_check CHECK (minutes_limit >= 0),
    ADD CONSTRAINT subscriptions_minutes_balance_check CHECK (
        minutes_balance >= 0 AND minutes_balance <= minutes_limit
    );

ALTER TABLE jobs
    ADD COLUMN reserved_minutes INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN billed_minutes INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN duration_seconds INTEGER,
    ADD COLUMN recording_started_at TIMESTAMPTZ,
    ADD COLUMN recording_finished_at TIMESTAMPTZ,
    ADD CONSTRAINT jobs_reserved_minutes_check CHECK (
        reserved_minutes >= 0 AND reserved_minutes <= 60
    ),
    ADD CONSTRAINT jobs_billed_minutes_check CHECK (billed_minutes >= 0),
    ADD CONSTRAINT jobs_duration_seconds_check CHECK (
        duration_seconds IS NULL OR duration_seconds >= 0
    ),
    ADD CONSTRAINT jobs_recording_window_check CHECK (
        recording_finished_at IS NULL
        OR recording_started_at IS NULL
        OR recording_finished_at >= recording_started_at
    );

UPDATE users
SET trial_minutes_used = trial_meetings_used * 60,
    trial_minutes_done = trial_meetings_done * 60;

UPDATE subscriptions
SET minutes_limit = meetings_limit * 60,
    minutes_balance = meetings_balance * 60;

-- Legacy billable jobs had exactly one meeting reserved. Dormant non-terminal
-- statuses keep a reserve too, so they can safely continue after the runtime switch.
UPDATE jobs
SET reserved_minutes = CASE
        WHEN status IN (
            'created', 'joining', 'recording', 'processing',
            'transcribed', 'summarizing', 'awaiting_reauth',
            'done', 'failed_summary'
        ) THEN 60
        ELSE 0
    END,
    billed_minutes = CASE
        WHEN status IN ('done', 'failed_summary') THEN 60
        ELSE 0
    END,
    duration_seconds = NULL;

UPDATE users u
SET trial_minutes_used = GREATEST(u.trial_minutes_used, j.chargeable_minutes),
    trial_minutes_done = GREATEST(u.trial_minutes_done, j.chargeable_minutes)
FROM (
    SELECT user_id, COUNT(*)::INTEGER * 60 AS chargeable_minutes
    FROM jobs
    WHERE is_trial = true
      AND status IN ('done', 'failed_summary')
    GROUP BY user_id
) j
WHERE u.telegram_id = j.user_id;

CREATE INDEX subscriptions_user_active_minutes_idx
    ON subscriptions (user_id, expires_at ASC)
    WHERE minutes_balance > 0;

CREATE INDEX subscriptions_expiring_minutes_idx
    ON subscriptions (expires_at)
    WHERE minutes_balance > 0;

-- Used by F9 "trial is running out" after #23 switches from meetings to minutes.
CREATE INDEX users_trial_minutes_done_idx
    ON users (trial_minutes_done)
    WHERE notified_trial_low_at IS NULL;

-- +goose Down
DROP INDEX users_trial_minutes_done_idx;
DROP INDEX subscriptions_expiring_minutes_idx;
DROP INDEX subscriptions_user_active_minutes_idx;

ALTER TABLE jobs
    DROP COLUMN recording_finished_at,
    DROP COLUMN recording_started_at,
    DROP COLUMN duration_seconds,
    DROP COLUMN billed_minutes,
    DROP COLUMN reserved_minutes;

ALTER TABLE subscriptions
    DROP COLUMN minutes_balance,
    DROP COLUMN minutes_limit;

ALTER TABLE users
    DROP COLUMN trial_minutes_done,
    DROP COLUMN trial_minutes_used;
