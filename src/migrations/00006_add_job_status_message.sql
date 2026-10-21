-- +goose Up
-- status_message_id is the id of the Telegram "call status" message that the bot sends when it accepts
-- the link and then EDITS during processing (accepted -> bot in the meeting -> done/error),
-- instead of a stream of new messages (UX: "alive, but no spam"). NULL - there is no status message
-- (old jobs, send failure). The chat for editing = user_id (the bot's private chat).
ALTER TABLE jobs ADD COLUMN status_message_id BIGINT;

-- +goose Down
ALTER TABLE jobs DROP COLUMN status_message_id;
