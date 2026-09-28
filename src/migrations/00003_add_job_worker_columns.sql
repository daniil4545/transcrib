-- +goose Up
-- Колонки воркера (план-слайс S0, канон db.md §3.6): id задачи у поставщика,
-- сырой JSON-ответ для post-mortem парсера, текст последней ошибки,
-- updated_at — штамп для бэкоффа поллинга (claim сдвигает его при заборе).
ALTER TABLE jobs
    ADD COLUMN external_job_id       TEXT,
    ADD COLUMN raw_provider_response JSONB,
    ADD COLUMN last_error            TEXT,
    ADD COLUMN updated_at            TIMESTAMPTZ NOT NULL DEFAULT now();

-- Выборка живых Job под FOR UPDATE SKIP LOCKED без скана финальных строк.
-- awaiting_reauth исключён: воркер его не подхватывает (будит web.go, Этап 5).
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
