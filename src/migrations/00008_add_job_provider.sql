-- +goose Up
-- provider — поставщик транскрибации, создавший джобу (mock|speech2text|mymeet).
-- external_job_id принадлежит пространству этого поставщика, поэтому повторная
-- доставка (Fetch) и отмена (Cancel) у поставщика должны идти через него же, а не
-- через текущий активный TRANSCRIPTION_PROVIDER. Без этого после смены провайдера
-- готовая джоба становится неповторнодоставляемой (чужой id → HTTP 400). DEFAULT ''
-- для исторических строк: резолв трактует пустого как «поставщик не настроен».
ALTER TABLE jobs ADD COLUMN provider TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE jobs DROP COLUMN provider;
