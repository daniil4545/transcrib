-- +goose Up
-- failed_summary — четвёртый финальный статус (architecture.md §7, db.md §3.6):
-- саммари F3 — отдельный шаг, падает независимо от транскрипта; транскрипт к этому
-- моменту уже доставлен. CHECK в Postgres не меняется на месте — пересоздаём.
ALTER TABLE jobs DROP CONSTRAINT jobs_status_check;
ALTER TABLE jobs ADD CONSTRAINT jobs_status_check CHECK (status IN (
    'created','joining','recording','processing',
    'transcribed','summarizing','awaiting_reauth',
    'done','failed_join','failed','failed_summary'
));

-- failed_summary финальный — исключаем из выборки воркера, иначе claim тянул бы его вечно.
DROP INDEX jobs_worker_idx;
CREATE INDEX jobs_worker_idx
    ON jobs (status, updated_at)
    WHERE status NOT IN ('done','failed_join','failed','failed_summary','awaiting_reauth');

-- +goose Down
DROP INDEX jobs_worker_idx;
CREATE INDEX jobs_worker_idx
    ON jobs (status, updated_at)
    WHERE status NOT IN ('done','failed_join','failed','awaiting_reauth');

ALTER TABLE jobs DROP CONSTRAINT jobs_status_check;
ALTER TABLE jobs ADD CONSTRAINT jobs_status_check CHECK (status IN (
    'created','joining','recording','processing',
    'transcribed','summarizing','awaiting_reauth',
    'done','failed_join','failed'
));
