-- +goose Up
-- failed_summary is the fourth final status (architecture.md §7, db.md §3.6):
-- the F3 summary is a separate step, it fails independently of the transcript; the transcript by this
-- moment is already delivered. A CHECK in Postgres cannot be changed in place - we recreate it.
ALTER TABLE jobs DROP CONSTRAINT jobs_status_check;
ALTER TABLE jobs ADD CONSTRAINT jobs_status_check CHECK (status IN (
    'created','joining','recording','processing',
    'transcribed','summarizing','awaiting_reauth',
    'done','failed_join','failed','failed_summary'
));

-- failed_summary is final - exclude it from the worker selection, otherwise claim would pull it forever.
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
