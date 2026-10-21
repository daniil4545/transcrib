package app

import (
	"encoding/json"
	"time"
)

// User is a Telegram bot user.
//
// Legacy meeting counters are kept as a compatibility bridge after the billing
// runtime moved to minutes; the source of truth for quotas is described in docs/db.md.
// The google binding (google_accounts) is a separate GoogleAccount entity (google-slice).
// Full table schema and target type: docs/db.md §3.1/§4.
type User struct {
	TelegramID int64
	// TrialMeetingsUsed is a legacy counter, updated by the bridge together with
	// trial_minutes_used until the old schema is cleaned up separately.
	TrialMeetingsUsed int
	// TrialMeetingsDone is a legacy counter, not a source for F9/runtime logic
	// after the move to trial_minutes_done.
	TrialMeetingsDone     int
	ConsentAt             *time.Time // NULL until 152-FZ consent (F0); while nil, intake is blocked by the gate
	CreatedAt             time.Time
	NotifiedTrialLowAt    *time.Time // F9 push dedup (db.md §5.6)
	NotifiedSubExpiringAt *time.Time
	NotifiedSubExpiredAt  *time.Time
	PaymentEmail          *string // email for YooKassa payment link receipts; nil until the first payment
}

// HasConsent reports whether the user gave consent to personal data processing (F0).
func (u User) HasConsent() bool {
	return u.ConsentAt != nil
}

// JobStatus is the meeting processing status. The full set and the state machine are in
// docs/db.md §3.6 / architecture.md §7.
type JobStatus string

const (
	JobCreated JobStatus = "created"
	// JobJoining: the bot has been sent and is connecting to the meeting (phase before entering).
	JobJoining JobStatus = "joining"
	// JobRecording: the bot has joined the meeting, recording is in progress.
	JobRecording JobStatus = "recording"
	// JobProcessing: the bot has left the meeting, background transcription of the recording is in progress.
	JobProcessing JobStatus = "processing"
	// JobSummarizing: the transcript is delivered, summary generation is in progress (F3).
	// The transcript is already with the user: a summary failure does not lose it (transition to JobFailedSummary).
	JobSummarizing JobStatus = "summarizing"
	JobDone        JobStatus = "done"
	JobFailed      JobStatus = "failed"
	// JobFailedSummary is final: the transcript was delivered, but the summary failed.
	// The quota reservation is NOT rolled back (the meeting was used): db.md §5.2/§7.0.
	JobFailedSummary JobStatus = "failed_summary"
)

// Job is a meeting processing request. SubscriptionID/IsTrial bind it to a quota (payments).
// doc links and reauth_from will be added with Google Drive (google-slice). Full type: docs/db.md §4.
type Job struct {
	ID     int64
	UserID int64
	// SubscriptionID: the subscription the meeting was charged against (NULL for trial).
	// A DB CHECK guarantees (is_trial XOR subscription_id).
	SubscriptionID *int64
	MeetingURL     string
	Status         JobStatus
	IsTrial        bool
	// ExternalJobID is the task id at the transcription provider.
	// *string, not string: pgx scans SQL NULL into nil without COALESCE;
	// nil expresses "bot not sent yet" at the type level.
	ExternalJobID *string
	// Provider is the provider that created the job (mock|speech2text|mymeet). ExternalJobID
	// belongs to its namespace, so redelivery and cancellation resolve the
	// provider by this field, not by the active TRANSCRIPTION_PROVIDER (migration 00008).
	Provider  string
	CreatedAt time.Time
	// StatusMessageID is the id of the Telegram "call status" message that the bot edits
	// as processing progresses (accepted -> bot in the meeting -> done). NULL means no message.
	StatusMessageID *int64
	// SummaryMD is the generated summary, cached before delivery (migration 00007).
	// Not nil means the summary was already generated on a previous attempt: on retry after a failure of
	// Deliver we do NOT regenerate it with a paid call, we only deliver (orchestrator summarizeJob).
	SummaryMD *string
	// DurationSeconds is the stored billing duration from jobs.duration_seconds.
	// The worker needs it so as not to understate Speech2Text billing down to the ASR chunk duration.
	DurationSeconds *int
	// LastError is the last technical marker/error. For the stop flow without a migration
	// it stores manual_stop/watchdog_stop, to tell an s2t cancelled after stop from
	// an ordinary provider failure.
	LastError *string
}

// PaymentStatus is the payment status (db.md §3.4/§4). FSM: pending until charge_id,
// succeeded after successful_payment, refunded after a refund.
type PaymentStatus string

const (
	PaymentPending   PaymentStatus = "pending"
	PaymentSucceeded PaymentStatus = "succeeded"
	PaymentFailed    PaymentStatus = "failed"
	PaymentRefunded  PaymentStatus = "refunded"
	// partially_refunded removed: partial refunds are out of MVP scope (full-only 1:1).
)

// RefundStatus is the refund status (db.md §3.5/§4).
type RefundStatus string

const (
	RefundPending   RefundStatus = "pending"
	RefundSucceeded RefundStatus = "succeeded"
	RefundFailed    RefundStatus = "failed"
	RefundCanceled  RefundStatus = "canceled" // YooKassa may return a refund in this status
)

// Subscription is one paid period (the "row per payment" 1:1 model, db.md §3.3).
// A renewal is a new row with an independent expires_at = now()+30 days; a user may have
// several active ones. Meetings* is a legacy bridge; the minute fields are the source
// of truth and are read by SQL methods.
type Subscription struct {
	ID              int64
	UserID          int64
	PaidAt          time.Time
	ExpiresAt       time.Time
	MeetingsLimit   int
	MeetingsBalance int
}

// Payment is a payment (db.md §3.4). Pointers are nullable columns.
type Payment struct {
	ID                      int64
	UserID                  int64
	SubscriptionID          *int64
	Status                  PaymentStatus
	Amount                  int64 // kopecks
	Currency                string
	TelegramPaymentChargeID *string
	ProviderPaymentChargeID *string
	PaymentIntentKey        *string
	PaymentMethodID         *string // saved payment method for the recurring/direct flow; nil without auto_renew/saved
	InvoicePayload          *string
	FiscalReceiptURL        *string
	Metadata                json.RawMessage // our keys for fallback lookup (api/yookassa/API.md §11.3)
	RawPayload              json.RawMessage
	CreatedAt               time.Time
	PaidAt                  *time.Time // our now()
	ProviderPaidAt          *time.Time // succeeded time on the YooKassa side
	FailedAt                *time.Time
}

type RecurringStatus string

const (
	RecurringActive       RecurringStatus = "active"
	RecurringCharging     RecurringStatus = "charging"
	RecurringChargeFailed RecurringStatus = "charge_failed"
	RecurringBlocked      RecurringStatus = "blocked"
	RecurringDisabled     RecurringStatus = "disabled"
)

// RecurringProfile is a saved payment method for auto-renewal.
// Subscription periods are still created as separate payments/subscriptions.
type RecurringProfile struct {
	UserID          int64
	PaymentMethodID string
	Status          RecurringStatus
	NextChargeAt    time.Time
	LastPaymentID   *int64
	AttemptsCount   int
	LastError       *string
	ConsentAt       time.Time
	DisabledAt      *time.Time
	UpdatedAt       time.Time
}

// Refund is a refund (two-phase, db.md §3.5/§5.9).

// PaymentActivation describes the result of finalizing a direct/recurring payment for
// the user notification without re-reading the money tables.
type PaymentActivation struct {
	Changed          bool
	UserID           int64
	ExpiresAt        time.Time // effective expiry for UX: MAX(active subscriptions.expires_at)
	RecurringActive  bool
	NextChargeAt     time.Time
	RecurringPayment bool
}

type Refund struct {
	ID               int64
	PaymentID        int64
	IdempotenceKey   string  // our deterministic key for YooKassa Idempotence-Key
	ProviderRefundID *string // id at YooKassa; nil until finalization (comes from the response)
	Amount           int64
	Status           RefundStatus
	Reason           *string
	RawPayload       json.RawMessage
	CreatedAt        time.Time
	CompletedAt      *time.Time
}
