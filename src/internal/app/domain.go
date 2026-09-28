package app

import (
	"encoding/json"
	"time"
)

// User — пользователь Telegram-бота.
//
// Legacy meeting-счётчики оставлены как compatibility bridge после перехода
// billing runtime на минуты; source of truth для квот описан в docs/db.md.
// google-привязка (google_accounts) — отдельная сущность GoogleAccount (google-slice).
// Полная схема таблицы и целевой тип — docs/db.md §3.1/§4.
type User struct {
	TelegramID int64
	// TrialMeetingsUsed — legacy-счётчик, обновляется мостом вместе с
	// trial_minutes_used до отдельного cleanup старой схемы.
	TrialMeetingsUsed int
	// TrialMeetingsDone — legacy-счётчик, не является источником F9/runtime-логики
	// после перехода на trial_minutes_done.
	TrialMeetingsDone     int
	ConsentAt             *time.Time // NULL до согласия 152-ФЗ (F0); пока nil — приём заблокирован gate
	CreatedAt             time.Time
	NotifiedTrialLowAt    *time.Time // дедуп F9-пушей (db.md §5.6)
	NotifiedSubExpiringAt *time.Time
	NotifiedSubExpiredAt  *time.Time
	PaymentEmail          *string // email для чеков ЮKassa payment links; nil до первого платежа
}

// HasConsent сообщает, дал ли пользователь согласие на обработку ПД (F0).
func (u User) HasConsent() bool {
	return u.ConsentAt != nil
}

// JobStatus — статус обработки встречи. Полный набор и state machine —
// docs/db.md §3.6 / architecture.md §7.
type JobStatus string

const (
	JobCreated JobStatus = "created"
	// JobJoining — бот отправлен, подключается к встрече (фаза до входа).
	JobJoining JobStatus = "joining"
	// JobRecording — бот зашёл на встречу, идёт запись.
	JobRecording JobStatus = "recording"
	// JobProcessing — бот вышел со встречи, идёт фоновая расшифровка записи.
	JobProcessing JobStatus = "processing"
	// JobSummarizing — транскрипт доставлен, идёт генерация саммари (F3).
	// Транскрипт уже у юзера: сбой саммари не теряет его (переход в JobFailedSummary).
	JobSummarizing JobStatus = "summarizing"
	JobDone        JobStatus = "done"
	JobFailed      JobStatus = "failed"
	// JobFailedSummary — финальный: транскрипт доставлен, но саммари не удалось.
	// Резерв квоты НЕ откатывается (встреча использована) — db.md §5.2/§7.0.
	JobFailedSummary JobStatus = "failed_summary"
)

// Job — заявка на обработку встречи. SubscriptionID/IsTrial — привязка к квоте (payments).
// doc-ссылки и reauth_from добавятся с Google Drive (google-slice). Полный тип — docs/db.md §4.
type Job struct {
	ID     int64
	UserID int64
	// SubscriptionID — против какой подписки списана встреча (NULL для триала).
	// CHECK в БД гарантирует (is_trial XOR subscription_id).
	SubscriptionID *int64
	MeetingURL     string
	Status         JobStatus
	IsTrial        bool
	// ExternalJobID — id задачи у поставщика транскрибации.
	// *string, а не string: pgx сканирует SQL NULL в nil без COALESCE;
	// nil явно выражает «бот ещё не отправлен» на уровне типа.
	ExternalJobID *string
	// Provider — поставщик, создавший джобу (mock|speech2text|mymeet). ExternalJobID
	// принадлежит его пространству, поэтому повторная доставка и отмена резолвят
	// провайдера по этому полю, а не по активному TRANSCRIPTION_PROVIDER (миграция 00008).
	Provider  string
	CreatedAt time.Time
	// StatusMessageID — id Telegram-сообщения «статус звонка», которое бот редактирует
	// по ходу обработки (принял → бот на встрече → готово). NULL — сообщения нет.
	StatusMessageID *int64
	// SummaryMD — сгенерированный конспект, закэшированный до доставки (миграция 00007).
	// Не nil → саммари уже сгенерировано на прошлой попытке: при ретрае после сбоя
	// Deliver НЕ пересоздаём платным вызовом, только доставляем (orchestrator summarizeJob).
	SummaryMD *string
	// DurationSeconds — сохранённая billing-длительность из jobs.duration_seconds.
	// Нужна worker'у, чтобы не занижать Speech2Text billing до ASR chunk duration.
	DurationSeconds *int
	// LastError — последний технический маркер/ошибка. Для stop-flow без миграции
	// хранит manual_stop/watchdog_stop, чтобы отличить s2t cancelled после stop от
	// обычного provider failure.
	LastError *string
}

// PaymentStatus — статус платежа (db.md §3.4/§4). FSM: pending до charge_id,
// succeeded после successful_payment, refunded после возврата.
type PaymentStatus string

const (
	PaymentPending   PaymentStatus = "pending"
	PaymentSucceeded PaymentStatus = "succeeded"
	PaymentFailed    PaymentStatus = "failed"
	PaymentRefunded  PaymentStatus = "refunded"
	// partially_refunded убран — частичные возвраты вне скоупа MVP (full-only 1:1).
)

// RefundStatus — статус возврата (db.md §3.5/§4).
type RefundStatus string

const (
	RefundPending   RefundStatus = "pending"
	RefundSucceeded RefundStatus = "succeeded"
	RefundFailed    RefundStatus = "failed"
	RefundCanceled  RefundStatus = "canceled" // ЮKassa может вернуть возврат в этом статусе
)

// Subscription — один оплаченный период (модель «строка на платёж» 1:1, db.md §3.3).
// Продление = новая строка с независимым expires_at = now()+30 дней; у юзера может
// быть несколько активных. Meetings* — legacy bridge; minute-поля являются source
// of truth и читаются SQL-методами.
type Subscription struct {
	ID              int64
	UserID          int64
	PaidAt          time.Time
	ExpiresAt       time.Time
	MeetingsLimit   int
	MeetingsBalance int
}

// Payment — платёж (db.md §3.4). Указатели — nullable-колонки.
type Payment struct {
	ID                      int64
	UserID                  int64
	SubscriptionID          *int64
	Status                  PaymentStatus
	Amount                  int64 // копейки
	Currency                string
	TelegramPaymentChargeID *string
	ProviderPaymentChargeID *string
	PaymentIntentKey        *string
	PaymentMethodID         *string // сохранённый способ оплаты для recurring/direct flow; nil без auto_renew/saved
	InvoicePayload          *string
	FiscalReceiptURL        *string
	Metadata                json.RawMessage // наши ключи для fallback-lookup (api/yookassa/API.md §11.3)
	RawPayload              json.RawMessage
	CreatedAt               time.Time
	PaidAt                  *time.Time // наш now()
	ProviderPaidAt          *time.Time // время succeeded со стороны ЮKassa
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

// RecurringProfile — сохранённый способ оплаты для автопродления.
// Подписочные периоды по-прежнему создаются отдельными payments/subscriptions.
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

// Refund — возврат (двухфазный, db.md §3.5/§5.9).

// PaymentActivation описывает результат финализации direct/recurring payment для
// уведомления пользователя без повторного чтения денежных таблиц.
type PaymentActivation struct {
	Changed          bool
	UserID           int64
	ExpiresAt        time.Time // effective expiry для UX: MAX(active subscriptions.expires_at)
	RecurringActive  bool
	NextChargeAt     time.Time
	RecurringPayment bool
}

type Refund struct {
	ID               int64
	PaymentID        int64
	IdempotenceKey   string  // наш детерминированный ключ для Idempotence-Key ЮKassa
	ProviderRefundID *string // id у ЮKassa; nil до финализации (приходит из ответа)
	Amount           int64
	Status           RefundStatus
	Reason           *string
	RawPayload       json.RawMessage
	CreatedAt        time.Time
	CompletedAt      *time.Time
}
