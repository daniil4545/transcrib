-- +goose Up
CREATE TABLE jobs (
    id          BIGSERIAL   PRIMARY KEY,
    user_id     BIGINT      NOT NULL REFERENCES users(telegram_id) ON DELETE RESTRICT,
    meeting_url TEXT        NOT NULL,
    -- Полный набор статусов из db.md §3.6: БД готова к будущей state machine,
    -- опечатка в статусе ловится сразу. Пишем пока только 'created'.
    status      TEXT        NOT NULL CHECK (status IN (
                               'created','joining','recording','processing',
                               'transcribed','summarizing','awaiting_reauth',
                               'done','failed_join','failed'
                            )),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE jobs;
