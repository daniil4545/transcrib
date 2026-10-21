-- +goose Up
-- Worker columns (plan slice S0, canon db.md §3.6): the job id at the provider,
-- the raw JSON response for parser post-mortem, the text of the last error,
-- updated_at - a stamp for polling backoff (claim moves it forward on take).
ALTER TABLE jobs
    ADD COLUMN external_job_id       TEXT,
    ADD COLUMN raw_provider_response JSONB,
    ADD COLUMN last_error            TEXT,
    ADD COLUMN updated_at            TIMESTAMPTZ NOT NULL DEFAULT now();

-- Selection of live jobs under FOR UPDATE SKIP LOCKED without scanning final rows.
-- awaiting_reauth is excluded: the worker does not pick it up (it is woken by web.go, Stage 5).
CREATE INDEX jobs_worker_idx
    ON jobs (status, updated_at)
    WHERE status NOT IN ('done','failed_join','failed','awaiting_reauth');

-- +goose Down
DROP INDEX jobs_worker_idx;

ALTER TABLE jobs
    DROP COLUMN external_job_id,
    DROP COLUMN raw_provider_response,
    DROP COLUMN last_error,
    DROP COLUMN updated_at;
