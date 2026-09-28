-- +goose Up
-- Аналитические события для продуктовой воронки и дедупа новых payment nudges.
-- Бизнес-таблицы payments/subscriptions/jobs остаются источником истины по деньгам и квоте.

CREATE TABLE analytics_events (
    id          BIGSERIAL   PRIMARY KEY,
    user_id     BIGINT      NOT NULL REFERENCES users(telegram_id) ON DELETE RESTRICT,
    event_type  TEXT        NOT NULL,
    event_key   TEXT,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    metadata    JSONB       NOT NULL DEFAULT '{}'::jsonb,
    CHECK (event_type <> ''),
    CHECK (event_key IS NULL OR event_key <> '')
);

CREATE INDEX analytics_events_user_type_time_idx
    ON analytics_events (user_id, event_type, occurred_at DESC);

CREATE INDEX analytics_events_type_time_idx
    ON analytics_events (event_type, occurred_at DESC);

-- +goose Down
DROP TABLE analytics_events;
