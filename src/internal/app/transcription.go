package app

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrEmptyTranscript означает штатный provider-done без содержательной речи.
// Встречу нельзя считать успешно обработанной: квоту возвращаем, summary не запускаем.
var ErrEmptyTranscript = errors.New("empty transcript")

// ProviderHTTPError сохраняет HTTP-код provider'а без парсинга строки ошибки.
// Body уже усечён в provider-клиенте и не содержит API-ключей: ключи уходят в headers.
type ProviderHTTPError struct {
	Provider   string
	Op         string
	StatusCode int
	Body       string
}

func (e ProviderHTTPError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("%s %s: HTTP %d", e.Provider, e.Op, e.StatusCode)
	}
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Provider, e.Op, e.StatusCode, e.Body)
}

func permanentSubmitError(err error) bool {
	var httpErr ProviderHTTPError
	if !errors.As(err, &httpErr) || httpErr.Op != "submit" {
		return false
	}
	switch httpErr.StatusCode {
	case 400, 401, 402, 403, 404, 409:
		return true
	default:
		return false
	}
}

func startTimedOut(job Job, timeout time.Duration) bool {
	return timeout > 0 && time.Since(job.CreatedAt) > timeout
}

// TranscriptionProvider — единственный интерфейс, изолирующий orchestrator от
// поставщика транскрибации (architecture.md §8). Реализации: mock.go (скелет),
// speech2text.go (prod/live-risk), mymeet.go (fallback).
type TranscriptionProvider interface {
	// Submit отправляет бота на встречу, возвращает внешний id задачи у
	// поставщика. Принимает Job: реализация берёт MeetingURL, платформу
	// выводит из хоста; template — из конфига, title/local_date_time
	// генерируются из текущего времени (mymeet требует их непустыми).
	Submit(ctx context.Context, job Job) (externalJobID string, err error)
	// Poll возвращает нормализованный статус (сырые статусы поставщика
	// маппятся внутри реализации).
	Poll(ctx context.Context, externalJobID string) (ProviderStatus, error)
	// Fetch забирает готовый результат. Вызывается при StatusDone и при
	// StatusFailed после stop-marker, когда provider уже финализировал фрагмент.
	Fetch(ctx context.Context, externalJobID string) (Result, error)
	// Cancel досрочно останавливает запись у поставщика. Семантика провайдеро-зависима:
	// s2t /stop ФИНАЛИЗИРУЕТ записанный фрагмент с транскриптом (поддержка 2026-06-17),
	// mymeet не умеет прерывать (no-op). Для уже пишущей s2t-записи вызывающие ставят
	// stop marker, переводят Job в processing и забирают transcript; refund остаётся
	// только для no-result path.
	// Best-effort: ошибка некритична — источник истины по квоте/статусу джобы в storage.
	Cancel(ctx context.Context, externalJobID string) error
}

// ProviderStatus — нормализованный статус задачи у поставщика.
type ProviderStatus int

const (
	StatusPending         ProviderStatus = iota // s2t: queued/code=0 (задача создана) -> Job joining
	StatusQueuedRecording                       // s2t: queued/code=30 "В очереди на запись" -> Job joining/recording
	StatusJoiningCall                           // mymeet: connecting/queued; s2t: бот присоединился -> Job joining
	StatusOnCall                                // mymeet: recording; s2t: бот ведёт запись -> Job recording
	StatusTranscribing                          // mymeet: processing (бот вышел, фоновая обработка) -> Job processing
	StatusDone                                  // mymeet: new/processed (new=готово, LIVE); s2t: done (code 200/204)
	StatusFailed                                // mymeet: failed; s2t: error/cancelled
	StatusPaused                                // s2t: paused (нехватка минут на счёте); у mymeet не бывает
)

func (s ProviderStatus) String() string {
	switch s {
	case StatusPending:
		return "pending"
	case StatusQueuedRecording:
		return "queued_recording"
	case StatusJoiningCall:
		return "joining_call"
	case StatusOnCall:
		return "on_call"
	case StatusTranscribing:
		return "transcribing"
	case StatusDone:
		return "done"
	case StatusFailed:
		return "failed"
	case StatusPaused:
		return "paused"
	default:
		return "unknown"
	}
}

// Result — готовый результат обработки встречи.
type Result struct {
	// TranscriptMarkdown — готовый транскрипт по спикерам (s2t result/txt),
	// доставляется юзеру как есть.
	TranscriptMarkdown string
	// SummaryMarkdown — готовое саммари ОТ поставщика (mymeet считает его при
	// обработке встречи). Пустая строка = «провайдер саммари не дал — обрабатывай
	// через Summarizer как раньше» (s2t-путь).
	SummaryMarkdown string
	// Raw — сырой JSON-ответ поставщика (s2t result/json), сохраняется в
	// jobs.raw_provider_response. Колонка JSONB: значение обязано быть
	// валидным JSON, plain text туда не вставится.
	Raw []byte
	// DurationSeconds — фактическая длительность результата от provider.
	// 0 = unknown/unavailable; billing fallback живёт выше, не в provider.
	DurationSeconds int
}
