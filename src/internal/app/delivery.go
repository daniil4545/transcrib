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

// transcriptFilename is the transcript file name based on the call time (clearer and sorts
// better than the internal id). No colons, so it is safe for file systems.
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

// DocumentDelivery sends a finished document to the user.
// Rationale for the interface: a second implementation (google_delivery.go) is already on
// the Stage 5 roadmap (architecture.md §11 item 11), so this is not a premature
// abstraction.
type DocumentDelivery interface {
	// Deliver sends content as a file named filename to chat chatID
	// (chatID = users.telegram_id). caption is the file caption for channels where it is
	// applicable (Telegram); implementations that do not need a caption ignore it.
	Deliver(ctx context.Context, chatID int64, filename, caption string, content []byte) error
	// DeliverGroup sends several files as one user-visible group.
	// In Telegram this is sendMediaGroup: separate documents sharing a media_group_id.
	DeliverGroup(ctx context.Context, chatID int64, caption string, docs []DeliveryDocument) error
}
