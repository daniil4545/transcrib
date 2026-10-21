package app

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrEmptyTranscript means a normal provider-done without meaningful speech.
// The meeting cannot be considered successfully processed: we refund the quota and do not start the summary.
var ErrEmptyTranscript = errors.New("empty transcript")

// ProviderHTTPError keeps the provider's HTTP code without parsing the error string.
// Body is already truncated in the provider client and contains no API keys: keys go in headers.
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

// TranscriptionProvider is the only interface isolating the orchestrator from the
// transcription provider (architecture.md §8). Implementations: mock.go (skeleton),
// speech2text.go (prod/live-risk), mymeet.go (fallback).
type TranscriptionProvider interface {
	// Submit sends the bot to a meeting and returns the provider's external task id.
	// It takes a Job: the implementation uses MeetingURL and derives the
	// platform from the host; template comes from config, title/local_date_time
	// are generated from the current time (mymeet requires them non-empty).
	Submit(ctx context.Context, job Job) (externalJobID string, err error)
	// Poll returns the normalized status (raw provider statuses
	// are mapped inside the implementation).
	Poll(ctx context.Context, externalJobID string) (ProviderStatus, error)
	// Fetch picks up the finished result. Called on StatusDone and on
	// StatusFailed after a stop marker, when the provider has already finalized the fragment.
	Fetch(ctx context.Context, externalJobID string) (Result, error)
	// Cancel stops recording at the provider early. Semantics are provider-dependent:
	// s2t /stop FINALIZES the recorded fragment with a transcript (support, 2026-06-17),
	// mymeet cannot interrupt (no-op). For an s2t recording already in progress, callers set a
	// stop marker, move the Job to processing and fetch the transcript; refund remains
	// only for the no-result path.
	// Best-effort: an error is non-critical, the source of truth for quota/job status is storage.
	Cancel(ctx context.Context, externalJobID string) error
}

// ProviderStatus is the normalized task status at the provider.
type ProviderStatus int

const (
	StatusPending         ProviderStatus = iota // s2t: queued/code=0 (task created) -> Job joining
	StatusQueuedRecording                       // s2t: queued/code=30 "В очереди на запись" (queued for recording) -> Job joining/recording
	StatusJoiningCall                           // mymeet: connecting/queued; s2t: bot joined -> Job joining
	StatusOnCall                                // mymeet: recording; s2t: bot is recording -> Job recording
	StatusTranscribing                          // mymeet: processing (bot left, background processing) -> Job processing
	StatusDone                                  // mymeet: new/processed (new = ready, LIVE); s2t: done (code 200/204)
	StatusFailed                                // mymeet: failed; s2t: error/cancelled
	StatusPaused                                // s2t: paused (not enough minutes on the account); mymeet never has it
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

// Result is the finished meeting processing result.
type Result struct {
	// TranscriptMarkdown is the finished transcript by speaker (s2t result/txt),
	// delivered to the user as is.
	TranscriptMarkdown string
	// SummaryMarkdown is the finished summary FROM the provider (mymeet computes it while
	// processing the meeting). An empty string = "the provider gave no summary: process
	// via Summarizer as before" (s2t path).
	SummaryMarkdown string
	// Raw is the provider's raw JSON response (s2t result/json), stored in
	// jobs.raw_provider_response. The column is JSONB: the value must be
	// valid JSON, plain text cannot be inserted.
	Raw []byte
	// DurationSeconds is the actual result duration from the provider.
	// 0 = unknown/unavailable; the billing fallback lives above, not in the provider.
	DurationSeconds int
}
