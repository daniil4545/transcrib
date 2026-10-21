-- +goose Up
-- provider is the transcription provider that created the job (mock|speech2text|mymeet).
-- external_job_id belongs to this provider's namespace, so repeated
-- delivery (Fetch) and cancellation (Cancel) at the provider must go through it, not
-- through the currently active TRANSCRIPTION_PROVIDER. Without this, after a provider switch
-- a ready job cannot be redelivered (a foreign id -> HTTP 400). DEFAULT ''
-- for historical rows: the resolver treats an empty value as "provider not configured".
ALTER TABLE jobs ADD COLUMN provider TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE jobs DROP COLUMN provider;
