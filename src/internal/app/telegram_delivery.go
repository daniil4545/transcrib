package app

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"

	tele "gopkg.in/telebot.v4"
)

// TelegramDelivery реализует DocumentDelivery: отправляет файл документом
// в Telegram-чат через telebot.
type TelegramDelivery struct {
	bot *tele.Bot
}

// NewTelegramDelivery создаёт TelegramDelivery поверх уже инициализированного
// бота. Принимает *Bot, а не *tele.Bot: main.go держит именно *Bot, и внутренний
// telebot-клиент не приходится экспортировать наружу пакета.
func NewTelegramDelivery(b *Bot) *TelegramDelivery {
	return &TelegramDelivery{bot: b.bot}
}

// Deliver отправляет content как Document с именем filename в чат chatID.
// caption — подпись под файлом (видна в чате); пустая строка — без подписи.
// ctx не передаётся в telebot: библиотека сама управляет таймаутами
// HTTP-клиента (context cancellation не поддерживается в v4 beta).
func (d *TelegramDelivery) Deliver(_ context.Context, chatID int64, filename, caption string, content []byte) error {
	// Индикатор «загружаю документ» — ощущение живости, пока файл готовится. Best-effort.
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
