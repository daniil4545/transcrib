-- +goose Up
-- status_message_id — id Telegram-сообщения «статус звонка», которое бот шлёт при приёме
-- ссылки и затем РЕДАКТИРУЕТ по ходу обработки (принял → бот на встрече → готово/ошибка),
-- вместо потока новых сообщений (UX: «живо, но без спама»). NULL — статус-сообщения нет
-- (старые джобы, сбой отправки). Чат для редактирования = user_id (личный чат бота).
ALTER TABLE jobs ADD COLUMN status_message_id BIGINT;

-- +goose Down
ALTER TABLE jobs DROP COLUMN status_message_id;
