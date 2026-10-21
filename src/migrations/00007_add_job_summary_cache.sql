-- +goose Up
-- summary_md is the generated F3 summary, cached BEFORE delivery. Protection against
-- a repeated paid LLM call: when Deliver fails the job stays summarizing and
-- is retried; without the cache every retry would regenerate the summary (orchestrator
-- summarizeJob). On retry we take this text and only deliver. NULL - the summary is not yet
-- generated (or demo summary-off, the summarizeJob path is not reached).
ALTER TABLE jobs ADD COLUMN summary_md TEXT;

-- +goose Down
ALTER TABLE jobs DROP COLUMN summary_md;
