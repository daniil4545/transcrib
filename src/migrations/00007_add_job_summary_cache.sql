-- +goose Up
-- summary_md — сгенерированный конспект F3, закэшированный ДО доставки. Защита от
-- повторного платного LLM-вызова: при сбое Deliver джоба остаётся summarizing и
-- ретраится; без кэша каждый ретрай пересоздавал бы саммари заново (orchestrator
-- summarizeJob). На ретрае берём этот текст и только доставляем. NULL — саммари ещё
-- не сгенерировано (или demo summary-off, путь summarizeJob не достигается).
ALTER TABLE jobs ADD COLUMN summary_md TEXT;

-- +goose Down
ALTER TABLE jobs DROP COLUMN summary_md;
