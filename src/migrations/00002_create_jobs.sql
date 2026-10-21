-- +goose Up
CREATE TABLE jobs (
    id          BIGSERIAL   PRIMARY KEY,
    user_id     BIGINT      NOT NULL REFERENCES users(telegram_id) ON DELETE RESTRICT,
    meeting_url TEXT        NOT NULL,
    -- Full set of statuses from db.md §3.6: the DB is ready for the future state machine,
    -- a typo in a status is caught immediately. For now we write only 'created'.
    status      TEXT        NOT NULL CHECK (status IN (
                               'created','joining','recording','processing',
                               'transcribed','summarizing','awaiting_reauth',
                               'done','failed_join','failed'
                            )),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE jobs;
