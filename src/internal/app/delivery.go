package app

import (
	"context"
	"time"
)

var moscowLocation = loadMoscowLocation()

func loadMoscowLocation() *time.Location {
	loc, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		return time.FixedZone("MSK", 3*60*60)
	}
	return loc
}

func moscowTime(t time.Time) time.Time {
	return t.In(moscowLocation)
}

func formatCallTime(t time.Time) string {
	return moscowTime(t).Format("02.01.2006 15:04")
}

func formatCallShortTime(t time.Time) string {
	return moscowTime(t).Format("02.01 15:04")
}

// transcriptFilename — имя файла транскрипта по времени звонка (понятнее и сортируется
// лучше, чем по внутреннему id). Без двоеточий — безопасно для файловых систем.
func transcriptFilename(createdAt time.Time) string {
	return "transcript_" + moscowTime(createdAt).Format("20060102_1504") + ".md"
}

func summaryFilename(createdAt time.Time) string {
	return "summary_" + moscowTime(createdAt).Format("20060102_1504") + ".md"
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

type DeliveryDocument struct {
	Filename string
	Content  []byte
}

// DocumentDelivery отправляет готовый документ пользователю.
// Обоснование интерфейса: вторая реализация (google_delivery.go) уже в
// дорожной карте Этапа 5 (architecture.md §11 п.11) — не преждевременная
// абстракция.
type DocumentDelivery interface {
	// Deliver отправляет контент как файл с именем filename в чат chatID
	// (chatID = users.telegram_id). caption — подпись файла для каналов, где она
	// применима (Telegram); реализации, которым caption не нужен, игнорируют.
	Deliver(ctx context.Context, chatID int64, filename, caption string, content []byte) error
	// DeliverGroup отправляет несколько файлов одной пользовательской группой.
	// В Telegram это sendMediaGroup: отдельные документы с общим media_group_id.
	DeliverGroup(ctx context.Context, chatID int64, caption string, docs []DeliveryDocument) error
}
