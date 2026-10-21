package app

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"

	tele "gopkg.in/telebot.v4"
)

// TelegramDelivery implements DocumentDelivery: it sends a file as a document
// to a Telegram chat via telebot.
type TelegramDelivery struct {
	bot *tele.Bot
}

// NewTelegramDelivery creates a TelegramDelivery on top of an already initialized
// bot. It takes *Bot, not *tele.Bot: main.go holds exactly *Bot, so the internal
// telebot client does not have to be exported outside the package.
func NewTelegramDelivery(b *Bot) *TelegramDelivery {
	return &TelegramDelivery{bot: b.bot}
}

// Deliver sends content as a Document named filename to chat chatID.
// caption is the caption under the file (visible in the chat); an empty string means no caption.
// ctx is not passed to telebot: the library manages the HTTP client
// timeouts itself (context cancellation is not supported in v4 beta).
func (d *TelegramDelivery) Deliver(_ context.Context, chatID int64, filename, caption string, content []byte) error {
	// "Uploading document" indicator: a sense of liveness while the file is being prepared. Best-effort.
	if err := d.bot.Notify(tele.ChatID(chatID), tele.UploadingDocument); err != nil {
		slog.Warn("chat action uploading", "err", err, "chat_id", chatID)
	}
	doc := &tele.Document{
		File:     tele.FromReader(bytes.NewReader(content)),
		FileName: filename,
		Caption:  caption,
	}
	_, err := d.bot.Send(tele.ChatID(chatID), doc)
	if err != nil {
		return fmt.Errorf("telegram deliver to %d: %w", chatID, err)
	}
	return nil
}

func (d *TelegramDelivery) DeliverGroup(_ context.Context, chatID int64, caption string, docs []DeliveryDocument) error {
	if len(docs) == 0 {
		return fmt.Errorf("telegram deliver group to %d: empty documents", chatID)
	}
	if err := d.bot.Notify(tele.ChatID(chatID), tele.UploadingDocument); err != nil {
		slog.Warn("chat action uploading", "err", err, "chat_id", chatID)
	}
	album := make(tele.Album, 0, len(docs))
	for i, item := range docs {
		doc := &tele.Document{
			File:     tele.FromReader(bytes.NewReader(item.Content)),
			FileName: item.Filename,
		}
		if i == 0 {
			doc.Caption = caption
		}
		album = append(album, doc)
	}
	if _, err := d.bot.SendAlbum(tele.ChatID(chatID), album); err != nil {
		return fmt.Errorf("telegram deliver group to %d: %w", chatID, err)
	}
	return nil
}
