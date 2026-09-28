package app

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Сентинелы CreateJob: tg.go различает их через errors.Is и шлёт юзеру разные тексты.
var (
	ErrNoQuota            = errors.New("storage: no trial or subscription quota")
	ErrDuplicateActiveJob = errors.New("storage: active job with same meeting url exists")
	ErrTooManyActiveJobs  = errors.New("storage: too many active jobs")
	// ErrJobNotCancelable — джоба не принадлежит юзеру либо уже не активна.
	ErrJobNotCancelable = errors.New("storage: job not cancelable")
	// ErrJobNotFound — джобы с таким id у этого юзера нет (ownership-проверка панели).
	ErrJobNotFound = errors.New("storage: job not found for user")
	// ErrJobNotCreated — SetJobSubmitted не нашёл джобу в статусе created (отменена
	// юзером, пока шёл Submit) — submitJob останавливает осиротевшего бота.
	ErrJobNotCreated = errors.New("storage: job no longer in created state")
)

const maxActiveJobsPerUser = 4

const (
	jobStopManual   = "manual_stop"
	jobStopWatchdog = "watchdog_stop"
)

const (
	analyticsBotStart           = "bot_start"
	analyticsConsentDeclined    = "consent_declined"
	analyticsSubscriptionViewed = "subscription_viewed"
	analyticsPaymentRequested   = "payment_requested"
	analyticsNudgeSent          = "nudge_sent"

	nudgePaymentFollowup = "payment_followup"
	nudgeSubLowBalance   = "sub_low_balance"
	nudgeSubEmptyBalance = "sub_empty_balance"
	nudgeTrialEnding     = "trial_ending"
	nudgeSubExpiring     = "sub_expiring"
	nudgeSubExpired      = "sub_expired"
)

func billedMinutes(durationSeconds int) int {
	switch {
	case durationSeconds <= 0:
		return 0
	default:
		return (durationSeconds + 59) / 60
	}
}

// ErrPaymentNotRefundable — платёж не найден / не в статусе succeeded и при этом
// нет уже существующего активного возврата по нему. billing.go различает через errors.Is.
var ErrPaymentNotRefundable = errors.New("storage: payment not found or not refundable")

// Balance — проекция остатка квоты для /balance и пушей F9. Локальный тип
// storage.go: это не сущность БД (нет таблицы), в domain.go ему не место.
// Для подписки агрегирует активные строки (модель 1:1); для триала — остаток лимита.
type Balance struct {
	IsSubscription bool
	Remaining      int
	Limit          int
	ExpiresAt      *time.Time // только для подписки; nil для триала
}

// PaymentInput — слепок платежа для RecordSuccessfulPayment и payment links (§5.4).
// Локальный тип storage.go: собирается billing.go из tele.Payment + наших полей,
// не сущность БД (для неё есть domain.Payment). RawPayload/Metadata — JSONB-колонки,
// nil-значения ложатся как SQL NULL (валидно для jsonb). ProviderPaidAt nil, пока
// telebot не отдаёт время ЮKassa из successful_payment.
type PaymentInput struct {
	UserID           int64
	AmountKopecks    int64
	Currency         string
	TelegramChargeID string
	ProviderChargeID string
	IntentKey        string
	InvoicePayload   string
	RawPayload       json.RawMessage
	Metadata         json.RawMessage
	ProviderPaidAt   *time.Time
	PaymentMethodID  string
}

// Storage — доступ к Postgres. Не содержит бизнес-правил (architecture.md §4).
type Storage struct {
	pool *pgxpool.Pool
}

func NewStorage(pool *pgxpool.Pool) *Storage {
	return &Storage{pool: pool}
}

func (s *Storage) TrackAnalyticsEvent(ctx context.Context, userID int64, eventType, eventKey string, metadata map[string]string) error {
	raw, err := json.Marshal(safeAnalyticsMetadata(eventType, metadata))
	if err != nil {
		return fmt.Errorf("marshal analytics event metadata: %w", err)
	}
	var key any
	if eventKey != "" {
		key = eventKey
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO analytics_events (user_id, event_type, event_key, metadata)
		 VALUES ($1, $2, $3, $4::jsonb)`,
		userID, eventType, key, raw,
	)
	if err != nil {
		return fmt.Errorf("track analytics event %s for user %d: %w", eventType, userID, err)
	}
	return nil
}

func safeAnalyticsMetadata(eventType string, metadata map[string]string) map[string]string {
	if eventType == analyticsNudgeSent && metadata["class"] != "" {
		return map[string]string{"class": metadata["class"]}
	}
	return map[string]string{}
}

// GetOrCreateUser регистрирует пользователя по telegram_id при первом контакте
// и возвращает его текущее состояние. Идемпотентно: для существующего юзера
// INSERT не пишет строку (ON CONFLICT DO NOTHING), поэтому горячий путь
// middleware не плодит мёртвые версии строк.
//
// Возвращает User только с identity+consent (telegram_id, consent_at, created_at);
// trial-счётчики и notified_* в нём НУЛЕВЫЕ, не читаются этим SELECT. Их актуальные
// значения берут точечно: квоту — Balance, резерв — CreateJob под транзакцией.
func (s *Storage) GetOrCreateUser(ctx context.Context, telegramID int64) (User, error) {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO users (telegram_id) VALUES ($1) ON CONFLICT (telegram_id) DO NOTHING`,
		telegramID)
	if err != nil {
		return User{}, fmt.Errorf("insert user %d: %w", telegramID, err)
	}

	// После upsert строка существует всегда (вставлена или уже была) — SELECT её находит.
	var u User
	err = s.pool.QueryRow(ctx,
		`SELECT telegram_id, consent_at, created_at FROM users WHERE telegram_id = $1`,
		telegramID,
	).Scan(&u.TelegramID, &u.ConsentAt, &u.CreatedAt)
	if err != nil {
		return User{}, fmt.Errorf("select user %d: %w", telegramID, err)
	}
	return u, nil
}

func (s *Storage) SetPaymentEmail(ctx context.Context, userID int64, email string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE users SET payment_email = $2 WHERE telegram_id = $1`,
		userID, email,
	)
	if err != nil {
		return fmt.Errorf("set payment email for user %d: %w", userID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("set payment email for user %d: %w", userID, pgx.ErrNoRows)
	}
	return nil
}

func (s *Storage) PaymentEmail(ctx context.Context, userID int64) (string, error) {
	var email *string
	err := s.pool.QueryRow(ctx,
		`SELECT payment_email FROM users WHERE telegram_id = $1`,
		userID,
	).Scan(&email)
	if err != nil {
		return "", fmt.Errorf("select payment email for user %d: %w", userID, err)
	}
	if email == nil {
		return "", nil
	}
	return *email, nil
}

// CreateJob создаёт заявку на обработку встречи с резервом квоты (db.md §5.1),
// всё в одной транзакции. Порядок: подписка, затем триал, затем insert Job.
// trialLimitMinutes — лимит триала в минутах.
// (storage без бизнес-правил: лимит приходит параметром, не хардкодом).
//
// Атомарный UPDATE ... WHERE физически закрывает гонку «два сообщения за 200мс»:
// два параллельных резерва не могут оба пройти WHERE minutes_balance > 0 /
// trial_minutes_used < limit на одной строке.
//
// Сентинелы: ErrDuplicateActiveJob (тот же URL уже активен), ErrTooManyActiveJobs
// (достигнут cap активных Job), ErrNoQuota (нет квоты).
func (s *Storage) CreateJob(ctx context.Context, userID int64, meetingURL, provider string, trialLimitMinutes int) (Job, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Job{}, fmt.Errorf("create job begin tx for user %d: %w", userID, err)
	}
	// Rollback идемпотентен после Commit (pgx возвращает ErrTxClosed, который мы
	// игнорируем тут): на успешном пути Commit уже закрыл tx, defer — no-op.
	defer func() { _ = tx.Rollback(ctx) }()

	var lockedUser int64
	err = tx.QueryRow(ctx,
		`SELECT telegram_id FROM users WHERE telegram_id = $1 FOR UPDATE`,
		userID,
	).Scan(&lockedUser)
	if err != nil {
		return Job{}, fmt.Errorf("create job lock user %d: %w", userID, err)
	}

	var duplicateID int64
	err = tx.QueryRow(ctx,
		`SELECT id FROM jobs
		 WHERE user_id = $1 AND meeting_url = $2
		   AND status NOT IN ('done','failed_join','failed','failed_summary')
		 LIMIT 1`,
		userID, meetingURL,
	).Scan(&duplicateID)
	switch {
	case err == nil:
		return Job{}, ErrDuplicateActiveJob
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return Job{}, fmt.Errorf("create job duplicate check for user %d: %w", userID, err)
	}

	var active int
	err = tx.QueryRow(ctx,
		`SELECT COUNT(*) FROM jobs
		 WHERE user_id = $1
		   AND status NOT IN ('done','failed_join','failed','failed_summary')`,
		userID,
	).Scan(&active)
	if err != nil {
		return Job{}, fmt.Errorf("create job active cap check for user %d: %w", userID, err)
	}
	if active >= maxActiveJobsPerUser {
		return Job{}, ErrTooManyActiveJobs
	}

	// Резерв §5.1: сначала подписка (платная квота расходуется раньше триала).
	// FOR UPDATE на выбранной строке сериализует параллельные списания одной подписки.
	var (
		subID           int64
		reservedMinutes int
	)
	isTrial := false
	err = tx.QueryRow(ctx,
		`WITH selected AS (
		     SELECT id FROM subscriptions
		     WHERE user_id = $1 AND expires_at > now() AND minutes_balance > 0
		     ORDER BY CASE WHEN minutes_balance >= 60 THEN 0 ELSE 1 END, expires_at ASC
		     LIMIT 1
		     FOR UPDATE
		 ),
		 reserved AS (
		     SELECT id, LEAST(60, minutes_balance) AS minutes
		     FROM subscriptions
		     WHERE id = (SELECT id FROM selected)
		 ),
		 updated AS (
		     UPDATE subscriptions s
		     SET minutes_balance = s.minutes_balance - reserved.minutes,
		         meetings_balance = GREATEST(0, s.meetings_balance - 1)
		     FROM reserved
		     WHERE s.id = reserved.id
		     RETURNING s.id, reserved.minutes
		 )
		 SELECT id, minutes FROM updated`,
		userID,
	).Scan(&subID, &reservedMinutes)
	switch {
	case err == nil:
		// списали с подписки
	case errors.Is(err, pgx.ErrNoRows):
		// Активной подписки нет — пробуем триал. ErrNoRows на UPDATE...RETURNING
		// здесь легитимный «не списалось», не сбой.
		err = tx.QueryRow(ctx,
			`WITH reserved AS (
			     SELECT LEAST(60, $2 - trial_minutes_used) AS minutes
			     FROM users
			     WHERE telegram_id = $1 AND trial_minutes_used < $2
			     FOR UPDATE
			 ),
			 updated AS (
			     UPDATE users
			     SET trial_minutes_used = trial_minutes_used + reserved.minutes,
			         trial_meetings_used = trial_meetings_used + 1
			     FROM reserved
			     WHERE telegram_id = $1
			     RETURNING reserved.minutes
			 )
			 SELECT minutes FROM updated`,
			userID, trialLimitMinutes,
		).Scan(&reservedMinutes)
		switch {
		case err == nil:
			isTrial = true
		case errors.Is(err, pgx.ErrNoRows):
			// Ни подписки, ни остатка триала.
			return Job{}, ErrNoQuota
		default:
			return Job{}, fmt.Errorf("create job reserve trial for user %d: %w", userID, err)
		}
	default:
		return Job{}, fmt.Errorf("create job reserve subscription for user %d: %w", userID, err)
	}

	// subscription_id: указатель, чтобы лечь в БД как NULL для триала.
	var subArg *int64
	if !isTrial {
		subArg = &subID
	}

	// Порядок Scan строго синхронен RETURNING: рассинхрон дал бы in-memory Job,
	// расходящийся с БД (напр. is_trial=false при реально true).
	var j Job
	err = tx.QueryRow(ctx,
		`INSERT INTO jobs (user_id, meeting_url, status, is_trial, subscription_id, provider, reserved_minutes)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING id, user_id, subscription_id, meeting_url, status, is_trial, external_job_id, provider, created_at`,
		userID, meetingURL, JobCreated, isTrial, subArg, provider, reservedMinutes,
	).Scan(&j.ID, &j.UserID, &j.SubscriptionID, &j.MeetingURL, &j.Status, &j.IsTrial, &j.ExternalJobID, &j.Provider, &j.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "jobs_active_dedup_idx" {
			return Job{}, ErrDuplicateActiveJob
		}
		return Job{}, fmt.Errorf("create job insert for user %d: %w", userID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Job{}, fmt.Errorf("create job commit for user %d: %w", userID, err)
	}
	return j, nil
}

// ClaimJobsForWork атомарно забирает джобы для обработки:
//   - все в статусе created (ещё не отправлены поставщику)
//   - joining|recording|processing|summarizing, у которых updated_at не обновлялся
//     дольше pollInterval (бот подключается/пишет / фоновая обработка / саммари: бэкофф
//     через bump updated_at в этом же UPDATE, иначе джоба поллилась бы каждым тиком —
//     ветка Pending не трогает статус)
//
// FOR UPDATE SKIP LOCKED защищает от двойного захвата только в пределах statement'а:
// после COMMIT created-джоба снова claimable (статус меняет лишь SetJobSubmitted).
// Сериализацию гарантирует единственный последовательный воркер; несколько
// инстансов требуют отдельного atomic-claim слайса.
// jobs_worker_idx (status, updated_at) делает вложенный SELECT index scan, не seq scan.
func (s *Storage) ClaimJobsForWork(ctx context.Context, pollInterval time.Duration, limit int) ([]Job, error) {
	// Интервал передаём строкой вида "30000 milliseconds" — стандартный PostgreSQL
	// interval literal: без расширений pgx, читается в explain, не теряет
	// суб-секундную точность короткого dev-тика.
	interval := fmt.Sprintf("%d milliseconds", pollInterval.Milliseconds())

	// $1 = JobCreated (без бэкоффа), $2 = слайс статусов с бэкоффом (ANY),
	// $3 = interval, $4 = limit. pgx кодирует []string как text[]; каст не нужен
	// (колонка status TEXT).
	rows, err := s.pool.Query(ctx,
		`UPDATE jobs SET updated_at = now()
		 WHERE id IN (
		     SELECT id FROM jobs
		     WHERE status = $1
		        OR (status = ANY($2) AND updated_at < now() - $3::interval)
		     ORDER BY created_at
		     LIMIT $4
		     FOR UPDATE SKIP LOCKED
		 )
		 RETURNING id, user_id, meeting_url, status, external_job_id, provider, created_at, status_message_id, summary_md, duration_seconds, last_error`,
		string(JobCreated),
		[]string{string(JobJoining), string(JobRecording), string(JobProcessing), string(JobSummarizing)},
		interval,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("claim jobs: %w", err)
	}
	defer rows.Close()

	var jobs []Job
	for rows.Next() {
		var j Job
		if err := rows.Scan(&j.ID, &j.UserID, &j.MeetingURL, &j.Status, &j.ExternalJobID, &j.Provider, &j.CreatedAt, &j.StatusMessageID, &j.SummaryMD, &j.DurationSeconds, &j.LastError); err != nil {
			return nil, fmt.Errorf("scan claimed job: %w", err)
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim jobs rows: %w", err)
	}
	return jobs, nil
}

// SetJobSubmitted переводит джобу из created в joining после успешного Submit к
// поставщику (бот отправлен, подключается). externalJobID сохраняется для
// последующего Poll/Fetch.
//
// Guard WHERE status='created' закрывает гонку с CancelJob: юзер мог отменить
// (created→failed), пока шёл сетевой Submit. Без guard последующий UPDATE воскресил бы
// отменённую джобу (failed→joining) с уже возвращённой квотой и запущенным ботом.
// RowsAffected=0 → ErrJobNotCreated: вызывающий (submitJob) останавливает осиротевшего
// бота у поставщика.
func (s *Storage) SetJobSubmitted(ctx context.Context, jobID int64, externalJobID string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE jobs SET status = $2, external_job_id = $3, updated_at = now()
		 WHERE id = $1 AND status = 'created'`,
		jobID, JobJoining, externalJobID,
	)
	if err != nil {
		return fmt.Errorf("set job %d submitted: %w", jobID, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrJobNotCreated
	}
	return nil
}

// SetJobRecording переводит джобу joining→recording (бот зашёл на встречу, идёт
// запись). Питается провайдер-статусом OnCall. moved=RowsAffected>0 сообщает
// вызывающему (pollJob), что фаза реально сменилась — статус-сообщение редактируется
// ровно раз на переход. Guard WHERE status='joining': повторный тик (уже recording)
// даёт moved=false и не повторяет Edit.
func (s *Storage) SetJobRecording(ctx context.Context, jobID int64) (moved bool, err error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE jobs
		 SET status = 'recording',
		     recording_started_at = COALESCE(recording_started_at, now()),
		     updated_at = now()
		 WHERE id = $1 AND status = 'joining'`,
		jobID,
	)
	if err != nil {
		return false, fmt.Errorf("set job %d recording: %w", jobID, err)
	}
	return tag.RowsAffected() > 0, nil
}

// SetJobProcessing переводит джобу в processing (бот вышел, идёт фоновая расшифровка).
// Питается провайдер-статусом Transcribing (mymeet). moved=RowsAffected>0 для
// разового Edit.
//
// Guard WHERE status IN ('joining','recording'): обычный вход — из recording, но
// поллинг дискретен и может перескочить recording (тик увидел joining, следующий уже
// processing), поэтому joining в guard. Повторный тик (уже processing) → moved=false.
func (s *Storage) SetJobProcessing(ctx context.Context, jobID int64) (moved bool, err error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE jobs
		 SET status = 'processing',
		     recording_started_at = CASE
		         WHEN status = 'recording' THEN COALESCE(recording_started_at, now())
		         ELSE recording_started_at
		     END,
		     recording_finished_at = CASE
		         WHEN status = 'recording' THEN COALESCE(recording_finished_at, now())
		         ELSE recording_finished_at
		     END,
		     duration_seconds = CASE
		         WHEN status = 'recording' AND duration_seconds IS NULL THEN
		             GREATEST(1, CEIL(EXTRACT(EPOCH FROM (now() - COALESCE(recording_started_at, now())))))::INTEGER
		         ELSE duration_seconds
		     END,
		     updated_at = now()
		 WHERE id = $1 AND status IN ('joining','recording')`,
		jobID,
	)
	if err != nil {
		return false, fmt.Errorf("set job %d processing: %w", jobID, err)
	}
	return tag.RowsAffected() > 0, nil
}

// MarkJobManualStop помечает recording-джобу как остановленную пользователем.
// Сам stop у provider'а вызывается снаружи: storage только атомарно проверяет owner/status
// и сохраняет marker, по которому orchestrator отличит ожидаемый s2t cancelled от
// настоящего provider failure. created/joining отменяются старым CancelJob с refund.
func (s *Storage) MarkJobManualStop(ctx context.Context, userID, jobID int64) (extID string, provider string, err error) {
	err = s.pool.QueryRow(ctx,
		`UPDATE jobs
		 SET last_error = $3, updated_at = now()
		 WHERE id = $1 AND user_id = $2 AND status = 'recording' AND external_job_id IS NOT NULL
		 RETURNING external_job_id, provider`,
		jobID, userID, jobStopManual,
	).Scan(&extID, &provider)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrJobNotCancelable
	}
	if err != nil {
		return "", "", fmt.Errorf("mark job %d manual stop: %w", jobID, err)
	}
	return extID, provider, nil
}

// MarkJobWatchdogStop помечает recording-джобу, которую watchdog останавливает по
// max age. Marker сохраняется до provider.Cancel: если s2t после /stop вернёт
// cancelled, worker всё равно имеет право попробовать Fetch.
func (s *Storage) MarkJobWatchdogStop(ctx context.Context, jobID int64) (marked bool, err error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE jobs
		 SET last_error = $2, updated_at = now()
		 WHERE id = $1 AND status = 'recording' AND external_job_id IS NOT NULL`,
		jobID, jobStopWatchdog,
	)
	if err != nil {
		return false, fmt.Errorf("mark job %d watchdog stop: %w", jobID, err)
	}
	return tag.RowsAffected() > 0, nil
}

// SetJobStatusMessage запоминает id Telegram-сообщения «статус звонка», чтобы
// оркестратор редактировал его по ходу обработки (а не слал новые). Вызывается из
// onText сразу после отправки первого статус-сообщения.
func (s *Storage) SetJobStatusMessage(ctx context.Context, jobID, statusMessageID int64) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE jobs SET status_message_id = $2 WHERE id = $1`,
		jobID, statusMessageID,
	)
	if err != nil {
		return fmt.Errorf("set job %d status message: %w", jobID, err)
	}
	return nil
}

// SetJobDone переводит джобу в done, сохраняет сырой JSON-ответ поставщика и
// финализирует списание минут по фактической duration.
func (s *Storage) SetJobDone(ctx context.Context, jobID int64, raw []byte, durationSeconds int, trialLimitMinutes int) error {
	return s.finishJob(ctx, jobID, JobDone, raw, durationSeconds, "", trialLimitMinutes)
}

// SetJobSummarizing переводит джобу в summarizing и сохраняет сырой JSON-ответ
// поставщика (транскрипт) вместе с фактической duration, если provider её отдал.
func (s *Storage) SetJobSummarizing(ctx context.Context, jobID int64, raw []byte, durationSeconds int) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE jobs
		 SET status = $2,
		     raw_provider_response = $3,
		     duration_seconds = CASE WHEN $4 > 0 THEN $4 ELSE duration_seconds END,
		     updated_at = now()
		 WHERE id = $1 AND status IN ('joining','recording','processing','summarizing') AND billed_minutes = 0`,
		jobID, JobSummarizing, raw, durationSeconds,
	)
	if err != nil {
		return fmt.Errorf("set job %d summarizing: %w", jobID, err)
	}
	return nil
}

// SetJobSummary кэширует конспект ДО доставки, чтобы транзиентный сбой Deliver не
// повлёк повторный платный Summarize на ретрае. Для provider-native summary вызывается
// до SetJobDone, поэтому guard допускает активные фазы до terminal done.
func (s *Storage) SetJobSummary(ctx context.Context, jobID int64, summary string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE jobs SET summary_md = $2, updated_at = now()
		 WHERE id = $1 AND status IN ('joining','recording','processing','summarizing')`,
		jobID, summary,
	)
	if err != nil {
		return fmt.Errorf("set job %d summary: %w", jobID, err)
	}
	return nil
}

// SetTerminalJobSummary сохраняет поздно восстановленный конспект для уже финальной
// job. Для failed_summary переводит статус в done без повторного биллинга.
func (s *Storage) SetTerminalJobSummary(ctx context.Context, jobID int64, summary string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE jobs
		 SET summary_md = $2,
		     status = CASE WHEN status = 'failed_summary' THEN 'done' ELSE status END,
		     last_error = CASE WHEN status = 'failed_summary' THEN NULL ELSE last_error END,
		     updated_at = now()
		 WHERE id = $1 AND status IN ('done','failed_summary')`,
		jobID, summary,
	)
	if err != nil {
		return fmt.Errorf("set terminal job %d summary: %w", jobID, err)
	}
	return nil
}

// SetJobFailedSummary переводит джобу в failed_summary и фиксирует причину.
// Используется когда транскрипт уже доставлен, но генерация саммари не удалась.
// Транскрипт уже у пользователя, поэтому списание финализируется как billable Job.
func (s *Storage) SetJobFailedSummary(ctx context.Context, jobID int64, reason string, trialLimitMinutes int) error {
	return s.finishJob(ctx, jobID, JobFailedSummary, nil, 0, reason, trialLimitMinutes)
}

func (s *Storage) finishJob(ctx context.Context, jobID int64, finalStatus JobStatus, raw []byte, durationSeconds int, reason string, trialLimitMinutes int) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("finish job %d begin tx: %w", jobID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	statusGuard := `status IN ('joining','recording','processing','summarizing')`
	if finalStatus == JobFailedSummary {
		statusGuard = `status = 'summarizing'`
	}

	var (
		userID        int64
		isTrial       bool
		subID         *int64
		reserved      int
		storedSeconds pgtype.Int4
		recordingFrom pgtype.Timestamptz
	)
	err = tx.QueryRow(ctx,
		fmt.Sprintf(
			`SELECT user_id, is_trial, subscription_id, reserved_minutes, duration_seconds, recording_started_at
			 FROM jobs
			 WHERE id = $1 AND %s AND billed_minutes = 0
			 FOR UPDATE`,
			statusGuard,
		),
		jobID,
	).Scan(&userID, &isTrial, &subID, &reserved, &storedSeconds, &recordingFrom)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("finish job %d load billing row: %w", jobID, err)
	}

	effectiveSeconds := durationSeconds
	durationSource := "argument"
	if effectiveSeconds <= 0 && storedSeconds.Valid {
		effectiveSeconds = int(storedSeconds.Int32)
		durationSource = "stored"
	}
	if effectiveSeconds <= 0 && recordingFrom.Valid {
		effectiveSeconds = int(time.Since(recordingFrom.Time).Seconds())
		durationSource = "recording_window"
	}
	if effectiveSeconds <= 0 {
		effectiveSeconds = 3600
		durationSource = "fallback"
		slog.Warn("job duration missing, using billing fallback",
			"job_id", jobID, "status", finalStatus, "fallback_seconds", effectiveSeconds)
	}
	finalDurationSeconds := durationSeconds
	if finalDurationSeconds <= 0 {
		finalDurationSeconds = effectiveSeconds
	}
	wanted := billedMinutes(effectiveSeconds)
	billed := reserved
	if wanted <= reserved {
		refund := reserved - wanted
		billed = wanted
		if refund > 0 {
			if isTrial {
				_, err = tx.Exec(ctx,
					`UPDATE users SET trial_minutes_used = trial_minutes_used - $2
					 WHERE telegram_id = $1`,
					userID, refund,
				)
			} else {
				if subID == nil {
					return fmt.Errorf("finish job %d: non-trial job has nil subscription_id", jobID)
				}
				_, err = tx.Exec(ctx,
					`UPDATE subscriptions
					 SET minutes_balance = LEAST(minutes_limit, minutes_balance + $2)
					 WHERE id = $1`,
					*subID, refund,
				)
			}
			if err != nil {
				return fmt.Errorf("finish job %d refund unused minutes: %w", jobID, err)
			}
		}
	} else {
		extra := wanted - reserved
		if isTrial {
			tag, err := tx.Exec(ctx,
				`UPDATE users
				 SET trial_minutes_used = trial_minutes_used + $3
				 WHERE telegram_id = $1 AND $2 - trial_minutes_used >= $3`,
				userID, trialLimitMinutes, extra,
			)
			if err != nil {
				return fmt.Errorf("finish job %d charge trial extra: %w", jobID, err)
			}
			if tag.RowsAffected() > 0 {
				billed += extra
			}
		} else {
			if subID == nil {
				return fmt.Errorf("finish job %d: non-trial job has nil subscription_id", jobID)
			}
			tag, err := tx.Exec(ctx,
				`UPDATE subscriptions
				 SET minutes_balance = minutes_balance - $2
				 WHERE id = $1 AND expires_at > now() AND minutes_balance >= $2`,
				*subID, extra,
			)
			if err != nil {
				return fmt.Errorf("finish job %d charge subscription extra: %w", jobID, err)
			}
			if tag.RowsAffected() > 0 {
				billed += extra
			}
		}
	}

	if isTrial && billed > 0 {
		_, err = tx.Exec(ctx,
			`UPDATE users
			 SET trial_minutes_done = trial_minutes_done + $2,
			     trial_meetings_done = trial_meetings_done + 1
			 WHERE telegram_id = $1`,
			userID, billed,
		)
		if err != nil {
			return fmt.Errorf("finish job %d mark trial done: %w", jobID, err)
		}
	}

	_, err = tx.Exec(ctx,
		`UPDATE jobs
		 SET status = $2,
		     raw_provider_response = COALESCE($3, raw_provider_response),
		     duration_seconds = CASE WHEN $4 > 0 THEN $4 ELSE duration_seconds END,
		     recording_finished_at = CASE
		         WHEN recording_started_at IS NOT NULL AND recording_finished_at IS NULL THEN now()
		         ELSE recording_finished_at
		     END,
		     billed_minutes = $5,
		     last_error = CASE WHEN $2 = 'failed_summary' THEN $6 ELSE last_error END,
		     updated_at = now()
		 WHERE id = $1`,
		jobID, finalStatus, raw, finalDurationSeconds, billed, reason,
	)
	if err != nil {
		return fmt.Errorf("finish job %d update terminal status: %w", jobID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("finish job %d commit: %w", jobID, err)
	}
	var loggedSubID any
	if subID != nil {
		loggedSubID = *subID
	}
	slog.Info("job billing finalized",
		"event", "job_billing_finalized", "phase", "billing",
		"job_id", jobID, "user_id", userID, "status", finalStatus,
		"is_trial", isTrial, "subscription_id", loggedSubID,
		"reserved_minutes", reserved, "billed_minutes", billed,
		"duration_seconds", finalDurationSeconds, "duration_source", durationSource)
	return nil
}

// CancelJob — ранняя отмена джобы юзером из панели «Мои звонки». Фейлит только
// created/joining джобу ЭТОГО юзера с рефандом резерва квоты (§5.2), в одной
// транзакции. recording останавливается через MarkJobManualStop и ждёт transcript.
// Возвращает external_job_id (nil для created — бот ещё не отправлен), provider-создатель
// для best-effort остановки бота у нужного поставщика, и refunded.
//
// Ownership+status guard в одном UPDATE: чужую или уже неактивную джобу не трогает →
// ErrJobNotCancelable. Идемпотентна: повторная отмена тоже вернёт ErrJobNotCancelable
// (резерв вернётся ровно один раз). is_trial решает направление рефанда, как в §5.2.
func (s *Storage) CancelJob(ctx context.Context, userID, jobID int64) (extID *string, provider string, refunded bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, "", false, fmt.Errorf("cancel job %d begin tx: %w", jobID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		isTrial         bool
		subID           *int64
		ext             *string
		prov            string
		reservedMinutes int
	)
	err = tx.QueryRow(ctx,
		`UPDATE jobs SET status = 'failed', last_error = 'cancelled by user', updated_at = now()
		 WHERE id = $1 AND user_id = $2 AND status IN ('created','joining')
		 RETURNING is_trial, subscription_id, external_job_id, provider, reserved_minutes`,
		jobID, userID,
	).Scan(&isTrial, &subID, &ext, &prov, &reservedMinutes)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", false, ErrJobNotCancelable
	}
	if err != nil {
		return nil, "", false, fmt.Errorf("cancel job %d: %w", jobID, err)
	}

	if isTrial {
		_, err = tx.Exec(ctx,
			`UPDATE users
			 SET trial_minutes_used = trial_minutes_used - $2,
			     trial_meetings_used = trial_meetings_used - 1
			 WHERE telegram_id = $1`,
			userID, reservedMinutes)
	} else {
		// CHECK (is_trial XOR subscription_id) гарантирует subID!=nil у не-триала, но
		// storage-метод не полагается на схему: WHERE id=NULL молча вернул бы 0 строк
		// и потерял квоту. Денежный путь не теряет возврат молча.
		if subID == nil {
			return nil, "", false, fmt.Errorf("cancel job %d: non-trial job has nil subscription_id", jobID)
		}
		_, err = tx.Exec(ctx,
			`UPDATE subscriptions
			 SET minutes_balance = LEAST(minutes_limit, minutes_balance + $2),
			     meetings_balance = LEAST(meetings_limit, meetings_balance + 1)
			 WHERE id = $1`,
			*subID, reservedMinutes)
	}
	if err != nil {
		return nil, "", false, fmt.Errorf("cancel job %d refund quota: %w", jobID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, "", false, fmt.Errorf("cancel job %d commit: %w", jobID, err)
	}
	return ext, prov, true, nil
}

// ListUserJobs возвращает джобы юзера, созданные после since, новые первыми —
// история звонков для панели. limit/offset — пагинация (панель листает по 5).
// Чтобы узнать, есть ли следующая страница, вызывающий запрашивает limit+1.
func (s *Storage) ListUserJobs(ctx context.Context, userID int64, since time.Time, limit, offset int) ([]Job, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, user_id, meeting_url, status, external_job_id, provider, created_at
		 FROM jobs
		 WHERE user_id = $1 AND created_at > $2
		 ORDER BY created_at DESC
		 LIMIT $3 OFFSET $4`,
		userID, since, limit, offset,
	)
	if err != nil {
		return nil, fmt.Errorf("list user jobs for %d: %w", userID, err)
	}
	defer rows.Close()

	var jobs []Job
	for rows.Next() {
		var j Job
		if err := rows.Scan(&j.ID, &j.UserID, &j.MeetingURL, &j.Status, &j.ExternalJobID, &j.Provider, &j.CreatedAt); err != nil {
			return nil, fmt.Errorf("list user jobs scan for %d: %w", userID, err)
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list user jobs rows for %d: %w", userID, err)
	}
	return jobs, nil
}

// UserJob возвращает джобу юзера по id с ownership-проверкой (панель: повторная
// доставка). ErrJobNotFound, если джобы нет или она принадлежит другому юзеру.
func (s *Storage) UserJob(ctx context.Context, userID, jobID int64) (Job, error) {
	var j Job
	err := s.pool.QueryRow(ctx,
		`SELECT id, user_id, subscription_id, meeting_url, status, is_trial, external_job_id, provider, created_at, summary_md, last_error
		 FROM jobs WHERE id = $1 AND user_id = $2`,
		jobID, userID,
	).Scan(&j.ID, &j.UserID, &j.SubscriptionID, &j.MeetingURL, &j.Status, &j.IsTrial, &j.ExternalJobID, &j.Provider, &j.CreatedAt, &j.SummaryMD, &j.LastError)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrJobNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("user job %d for user %d: %w", jobID, userID, err)
	}
	return j, nil
}

// FailJobWithQuotaRefund переводит джобу в failed и откатывает резерв квоты (§5.2),
// всё в одной транзакции. refunded сообщает, был ли откат фактически сделан.
//
// Guard WHERE status IN ('created','joining','recording','processing') — фазы до
// доставки транскрипта: сбой/watchdog тут откатывает резерв (§5.2).
// failJob зовётся из pollJob/StatusFailed и watchdog (orchestrator.go), которые клеймят
// любую из этих фаз. summarizing исключён намеренно: транскрипт уже доставлен, встреча
// использована (сбой саммари идёт через SetJobFailedSummary без отката, §5.2/§7.0).
//
// RowsAffected=0 (второй тик / уже закрыта) → refunded=false, err=nil: ничего не
// откатываем, повторный откат вернул бы квоту дважды. is_trial решает направление:
// триал — возврат reserved_minutes в users.trial_minutes_used, подписка —
// возврат reserved_minutes в subscriptions.minutes_balance.
func (s *Storage) FailJobWithQuotaRefund(ctx context.Context, jobID int64, reason string) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("fail job %d begin tx: %w", jobID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		isTrial         bool
		subID           *int64
		userID          int64
		reservedMinutes int
	)
	err = tx.QueryRow(ctx,
		`UPDATE jobs SET status = 'failed', last_error = $2, updated_at = now()
		 WHERE id = $1 AND status IN ('created','joining','recording','processing')
		 RETURNING is_trial, subscription_id, user_id, reserved_minutes`,
		jobID, reason,
	).Scan(&isTrial, &subID, &userID, &reservedMinutes)
	if errors.Is(err, pgx.ErrNoRows) {
		// Уже не в активной фазе (второй тик / закрыта другим путём) — откат не нужен.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("fail job %d: %w", jobID, err)
	}

	if isTrial {
		_, err = tx.Exec(ctx,
			`UPDATE users
			 SET trial_minutes_used = trial_minutes_used - $2,
			     trial_meetings_used = trial_meetings_used - 1
			 WHERE telegram_id = $1`,
			userID, reservedMinutes,
		)
	} else {
		if subID == nil {
			return false, fmt.Errorf("fail job %d: non-trial job has nil subscription_id", jobID)
		}
		_, err = tx.Exec(ctx,
			`UPDATE subscriptions
			 SET minutes_balance = LEAST(minutes_limit, minutes_balance + $2),
			     meetings_balance = LEAST(meetings_limit, meetings_balance + 1)
			 WHERE id = $1`,
			*subID, reservedMinutes,
		)
	}
	if err != nil {
		return false, fmt.Errorf("fail job %d refund quota: %w", jobID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("fail job %d commit: %w", jobID, err)
	}
	return true, nil
}

// Balance отдаёт остаток квоты для /balance и пушей F9. Активная подписка имеет
// приоритет над триалом до expires_at, даже если минуты уже израсходованы.
// trialLimitMinutes — лимит триала в минутах.
func (s *Storage) Balance(ctx context.Context, userID int64, trialLimitMinutes int) (Balance, error) {
	// Агрегация по активным строкам подписки (модель 1:1: у юзера их может быть
	// несколько). SUM/MAX по пустому набору дают NULL — COALESCE сводит к нулю,
	// MAX(expires_at) сканируется в *time.Time (NULL при отсутствии строк).
	var (
		sumBalance int
		sumLimit   int
		maxExpires *time.Time
	)
	err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(minutes_balance), 0), COALESCE(SUM(minutes_limit), 0), MAX(expires_at)
		 FROM subscriptions
		 WHERE user_id = $1 AND expires_at > now()`,
		userID,
	).Scan(&sumBalance, &sumLimit, &maxExpires)
	if err != nil {
		return Balance{}, fmt.Errorf("balance subscription for user %d: %w", userID, err)
	}
	if maxExpires != nil {
		return Balance{
			IsSubscription: true,
			Remaining:      sumBalance,
			Limit:          sumLimit,
			ExpiresAt:      maxExpires,
		}, nil
	}

	var used int
	err = s.pool.QueryRow(ctx,
		`SELECT trial_minutes_used FROM users WHERE telegram_id = $1`,
		userID,
	).Scan(&used)
	if err != nil {
		return Balance{}, fmt.Errorf("balance trial for user %d: %w", userID, err)
	}
	return Balance{
		IsSubscription: false,
		// max(0, ...): если оператор понизит trial limit ниже уже
		// израсходованного used, /balance не должен показать отрицательный остаток
		// (приём всё равно блокирует CreateJob по WHERE used < limit).
		Remaining: max(0, trialLimitMinutes-used),
		Limit:     trialLimitMinutes,
		ExpiresAt: nil,
	}, nil
}

// HasActiveSubscription сообщает, есть ли у пользователя оплаченный период,
// который ещё не истёк. Баланс встреч не учитываем: поддержка остаётся платной
// привилегией до конца оплаченного периода.
func (s *Storage) HasActiveSubscription(ctx context.Context, userID int64) (bool, error) {
	var active bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (
		     SELECT 1 FROM subscriptions
		     WHERE user_id = $1 AND expires_at > now()
		 )`,
		userID,
	).Scan(&active)
	if err != nil {
		return false, fmt.Errorf("active subscription for user %d: %w", userID, err)
	}
	return active, nil
}

func (s *Storage) RecurringProfile(ctx context.Context, userID int64) (RecurringProfile, bool, error) {
	var p RecurringProfile
	err := s.pool.QueryRow(ctx,
		`SELECT user_id, COALESCE(payment_method_id, ''), status, next_charge_at,
		        last_payment_id, attempts_count, last_error, consent_at, disabled_at, updated_at
		 FROM recurring_profiles
		 WHERE user_id = $1`,
		userID,
	).Scan(
		&p.UserID, &p.PaymentMethodID, &p.Status, &p.NextChargeAt,
		&p.LastPaymentID, &p.AttemptsCount, &p.LastError, &p.ConsentAt, &p.DisabledAt, &p.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RecurringProfile{}, false, nil
		}
		return RecurringProfile{}, false, fmt.Errorf("recurring profile for user %d: %w", userID, err)
	}
	return p, true, nil
}

// RecordSuccessfulPayment записывает успешный платёж и создаёт новый период
// подписки (db.md §5.4, модель «строка на платёж» 1:1) — два statement в одной
// транзакции.
//
// ПОЧЕМУ ДВА STATEMENT, А НЕ ОДИН CTE: линковку payments.subscription_id нельзя
// сделать финальным UPDATE внутри того же CTE — Postgres исполняет весь запрос на
// одном снимке, и строка, вставленная CTE new_payment, НЕ видна сканированию
// таблицы payments в том же statement (UPDATE подобрал бы 0 строк, FK остался бы
// NULL). Поэтому: statement 1 (CTE) создаёт платёж+подписку и ВОЗВРАЩАЕТ их id;
// statement 2 в той же tx проставляет subscription_id (он уже видит свою строку).
//
// Идемпотентность: ретрай Telegram с тем же telegram_payment_charge_id →
// ON CONFLICT DO NOTHING → new_payment пуст → new_sub (WHERE EXISTS) не создаётся →
// SELECT отдаёт (NULL, NULL) → statement 2 пропускается. Дубля платежа/подписки нет.
// Конкурентный повтор того же charge_id сериализуется UNIQUE-индексом charge_id
// (второй INSERT...ON CONFLICT ждёт коммита первого), поэтому подписка не дублируется
// и orphan-строк не возникает.
//
// Линковка в той же транзакции: FK не остаётся NULL, поэтому refund (§5.9) всегда
// находит строку подписки для деактивации.
//
// expires_at = now() + 30 дней: новый период не зависит от остальных активных
// строк юзера (без chain). minutesPerPeriod — рабочий лимит подписки; legacy
// meetings_* поля заполняются эквивалентом в часах до удаления старого UI/F9-кода.
func (s *Storage) RecordSuccessfulPayment(ctx context.Context, p PaymentInput, minutesPerPeriod int) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("record payment begin tx for user %d: %w", p.UserID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var lockedUser int64
	if err := tx.QueryRow(ctx,
		`SELECT telegram_id FROM users WHERE telegram_id = $1 FOR UPDATE`,
		p.UserID,
	).Scan(&lockedUser); err != nil {
		return fmt.Errorf("record payment lock user %d: %w", p.UserID, err)
	}

	// Параметры $1..$10 идут линейно по порядку Go-аргументов (рассинхрон SQL↔args
	// в платёжной записи особенно опасен). new_sub читает new_payment напрямую (CTE,
	// не таблицу — это видно), поэтому гейт WHERE EXISTS работает корректно.
	var paymentID, subID *int64
	err = tx.QueryRow(ctx,
		`WITH new_payment AS (
		     INSERT INTO payments (user_id, status, amount, currency,
		         telegram_payment_charge_id, provider_payment_charge_id,
		         invoice_payload, metadata, raw_payload, provider_paid_at, paid_at)
		     VALUES ($1, 'succeeded', $2, $3, $4, NULLIF($5, ''), $6, $7, $8, $9, now())
		     ON CONFLICT (telegram_payment_charge_id) DO NOTHING
		     RETURNING id
		 ),
		 new_sub AS (
		     INSERT INTO subscriptions (user_id, paid_at, expires_at,
		         meetings_limit, meetings_balance, minutes_limit, minutes_balance)
		     SELECT $1, now(), now() + INTERVAL '30 days',
		            $10 / 60, $10 / 60, $10, $10
		     WHERE EXISTS (SELECT 1 FROM new_payment)
		     RETURNING id
		 )
		 SELECT (SELECT id FROM new_payment), (SELECT id FROM new_sub)`,
		p.UserID, p.AmountKopecks, p.Currency, p.TelegramChargeID, p.ProviderChargeID,
		p.InvoicePayload, p.Metadata, p.RawPayload, p.ProviderPaidAt, minutesPerPeriod,
	).Scan(&paymentID, &subID)
	if err != nil {
		return fmt.Errorf("record payment insert for user %d: %w", p.UserID, err)
	}

	// paymentID==nil → ретрай (ON CONFLICT погасил вставку): линковать нечего.
	// Новый платёж всегда создаёт new_sub, поэтому при paymentID!=nil subID!=nil.
	// Guard subscription_id IS NULL делает повторную линковку невозможной (денежный
	// UPDATE всегда под guard, db.md §6) — fail-safe против будущего второго пути.
	if paymentID != nil {
		_, err = tx.Exec(ctx,
			`UPDATE payments SET subscription_id = $2 WHERE id = $1 AND subscription_id IS NULL`,
			*paymentID, subID,
		)
		if err != nil {
			return fmt.Errorf("record payment link subscription for user %d: %w", p.UserID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("record payment commit for user %d: %w", p.UserID, err)
	}
	return nil
}

func (s *Storage) StartRecurringPaymentIntent(ctx context.Context, p PaymentInput, idemKey string, nextChargeAt time.Time) (bool, error) {
	if idemKey == "" {
		return false, fmt.Errorf("start recurring payment intent for user %d: empty idempotence key", p.UserID)
	}
	if p.IntentKey != "" && p.IntentKey != idemKey {
		return false, fmt.Errorf("start recurring payment intent for user %d: mismatched idempotence key", p.UserID)
	}
	if p.PaymentMethodID == "" {
		return false, fmt.Errorf("start recurring payment intent for user %d: empty payment method id", p.UserID)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("start recurring payment intent begin for user %d: %w", p.UserID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var existingID int64
	var existingUserID int64
	var existingAmount int64
	var existingCurrency string
	var existingStatus PaymentStatus
	var existingMethodID *string
	var existingProviderID *string
	var stale bool
	err = tx.QueryRow(ctx,
		`SELECT id, user_id, amount, currency, status, payment_method_id, provider_payment_charge_id,
		        now() - created_at >= INTERVAL '23 hours'
		 FROM payments
		 WHERE payment_intent_key = $1
		 FOR UPDATE`,
		idemKey,
	).Scan(&existingID, &existingUserID, &existingAmount, &existingCurrency, &existingStatus, &existingMethodID, &existingProviderID, &stale)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("start recurring payment intent lookup for user %d: %w", p.UserID, err)
	}
	if err == nil {
		if existingUserID != p.UserID || existingAmount != p.AmountKopecks || existingCurrency != p.Currency || existingMethodID == nil || *existingMethodID != p.PaymentMethodID {
			return false, fmt.Errorf("start recurring payment intent conflict for user %d", p.UserID)
		}
		if existingStatus == PaymentPending && existingProviderID == nil {
			if stale {
				blocked, err := s.failStaleRecurringPaymentIntentTx(ctx, tx, p.UserID, idemKey, nextChargeAt)
				if err != nil {
					return false, err
				}
				if err := tx.Commit(ctx); err != nil {
					return false, fmt.Errorf("start recurring stale intent commit for user %d: %w", p.UserID, err)
				}
				if blocked {
					slog.Warn("recurring intent blocked",
						"event", "recurring_intent_blocked", "phase", "recurring",
						"user_id", p.UserID, "next_charge_at", nextChargeAt)
				}
				return false, nil
			}
			changed, err := s.resumeRecurringIntentTx(ctx, tx, p.UserID, nextChargeAt)
			if err != nil {
				return false, err
			}
			if !changed {
				if err := tx.Commit(ctx); err != nil {
					return false, fmt.Errorf("start recurring existing intent commit for user %d: %w", p.UserID, err)
				}
				return false, nil
			}
			if err := tx.Commit(ctx); err != nil {
				return false, fmt.Errorf("start recurring existing intent commit for user %d: %w", p.UserID, err)
			}
			return true, nil
		}
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("start recurring existing intent commit for user %d: %w", p.UserID, err)
		}
		return false, nil
	}

	changed, err := s.claimRecurringProfileTx(ctx, tx, p.UserID, nextChargeAt)
	if err != nil {
		return false, err
	}
	if !changed {
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("start recurring no profile commit for user %d: %w", p.UserID, err)
		}
		return false, nil
	}

	tag, err := tx.Exec(ctx,
		`INSERT INTO payments (user_id, status, amount, currency,
		     payment_intent_key, payment_method_id, metadata, raw_payload)
		 VALUES ($1, 'pending', $2, $3, $4, $5, $6, $7)
		 ON CONFLICT DO NOTHING`,
		p.UserID, p.AmountKopecks, p.Currency, idemKey, p.PaymentMethodID, p.Metadata, p.RawPayload,
	)
	if err != nil {
		return false, fmt.Errorf("start recurring payment intent insert for user %d: %w", p.UserID, err)
	}
	if tag.RowsAffected() != 1 {
		return false, fmt.Errorf("start recurring payment intent insert for user %d: inserted %d rows", p.UserID, tag.RowsAffected())
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("start recurring payment intent commit for user %d: %w", p.UserID, err)
	}
	return true, nil
}

func (s *Storage) claimRecurringProfileTx(ctx context.Context, tx pgx.Tx, userID int64, nextChargeAt time.Time) (bool, error) {
	tag, err := tx.Exec(ctx,
		`UPDATE recurring_profiles
		 SET status = 'charging', updated_at = now()
		 WHERE user_id = $1
		   AND status IN ('active', 'charge_failed')
		   AND next_charge_at = $2`,
		userID, nextChargeAt,
	)
	if err != nil {
		return false, fmt.Errorf("claim recurring profile for user %d: %w", userID, err)
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Storage) resumeRecurringIntentTx(ctx context.Context, tx pgx.Tx, userID int64, nextChargeAt time.Time) (bool, error) {
	tag, err := tx.Exec(ctx,
		`UPDATE recurring_profiles
		 SET status = 'charging', updated_at = now()
		 WHERE user_id = $1
		   AND status IN ('charging', 'active', 'charge_failed')
		   AND next_charge_at = $2`,
		userID, nextChargeAt,
	)
	if err != nil {
		return false, fmt.Errorf("resume recurring intent for user %d: %w", userID, err)
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Storage) failStaleRecurringPaymentIntentTx(ctx context.Context, tx pgx.Tx, userID int64, idemKey string, nextChargeAt time.Time) (bool, error) {
	reason := "recurring payment provider id was not attached before idempotence key retry window"
	tag, err := tx.Exec(ctx,
		`UPDATE payments
		 SET status = 'failed', failed_at = now()
		 WHERE user_id = $1
		   AND payment_intent_key = $2
		   AND status = 'pending'
		   AND provider_payment_charge_id IS NULL
		   AND now() - created_at >= INTERVAL '23 hours'`,
		userID, idemKey,
	)
	if err != nil {
		return false, fmt.Errorf("fail stale recurring payment intent %s for user %d: %w", idemKey, userID, err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if tag.RowsAffected() != 1 {
		return false, fmt.Errorf("fail stale recurring payment intent %s for user %d: updated %d payments", idemKey, userID, tag.RowsAffected())
	}
	tag, err = tx.Exec(ctx,
		`UPDATE recurring_profiles
		 SET status = 'blocked',
		     last_error = left($3, 500),
		     updated_at = now()
		 WHERE user_id = $1
		   AND next_charge_at = $2
		   AND status IN ('charging', 'active', 'charge_failed')`,
		userID, nextChargeAt, reason,
	)
	if err != nil {
		return false, fmt.Errorf("block stale recurring profile for user %d: %w", userID, err)
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Storage) AttachRecurringProviderPayment(ctx context.Context, idemKey string, payment YookassaPayment) error {
	if idemKey == "" {
		return fmt.Errorf("attach recurring provider payment: empty idempotence key")
	}
	if payment.ID == "" {
		return fmt.Errorf("attach recurring provider payment %s: empty provider payment id", idemKey)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE payments
		 SET provider_payment_charge_id = $2,
		     raw_payload = $3,
		     provider_paid_at = $4
		 WHERE payment_intent_key = $1
		   AND status = 'pending'
		   AND (provider_payment_charge_id IS NULL OR provider_payment_charge_id = $2)`,
		idemKey, payment.ID, payment.Raw, payment.ProviderPaidAt,
	)
	if err != nil {
		return fmt.Errorf("attach recurring provider payment %s/%s: %w", idemKey, payment.ID, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("attach recurring provider payment %s/%s: updated %d rows", idemKey, payment.ID, tag.RowsAffected())
	}
	return nil
}

func (s *Storage) StartDirectPaymentIntent(ctx context.Context, p PaymentInput, idemKey string, email string, since time.Time) (bool, error) {
	if idemKey == "" {
		return false, fmt.Errorf("start direct payment intent for user %d: empty idempotence key", p.UserID)
	}
	if p.IntentKey != "" && p.IntentKey != idemKey {
		return false, fmt.Errorf("start direct payment intent for user %d: mismatched idempotence key", p.UserID)
	}

	metadata := p.Metadata
	if len(metadata) == 0 && email != "" {
		var err error
		metadata, err = json.Marshal(map[string]string{"payment_email": email})
		if err != nil {
			return false, fmt.Errorf("marshal direct payment metadata for user %d: %w", p.UserID, err)
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("start direct payment intent begin for user %d: %w", p.UserID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var lockedUser int64
	if err := tx.QueryRow(ctx,
		`SELECT telegram_id FROM users WHERE telegram_id = $1 FOR UPDATE`,
		p.UserID,
	).Scan(&lockedUser); err != nil {
		return false, fmt.Errorf("start direct payment intent lock user %d: %w", p.UserID, err)
	}

	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (
		     SELECT 1 FROM payments
		     WHERE user_id = $1
		       AND status = 'pending'
		       AND created_at >= $2
		       AND (payment_intent_key IS NULL OR payment_intent_key NOT LIKE 'recurring:%')
		 )`,
		p.UserID, since,
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("start direct payment intent lookup for user %d: %w", p.UserID, err)
	}
	if exists {
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("start direct payment intent existing commit for user %d: %w", p.UserID, err)
		}
		return false, nil
	}

	tag, err := tx.Exec(ctx,
		`INSERT INTO payments (user_id, status, amount, currency,
		     payment_intent_key, invoice_payload, metadata, raw_payload)
		 VALUES ($1, 'pending', $2, $3, $4, $5, $6, $7)
		 ON CONFLICT DO NOTHING`,
		p.UserID, p.AmountKopecks, p.Currency, idemKey, p.InvoicePayload, metadata, p.RawPayload,
	)
	if err != nil {
		return false, fmt.Errorf("start direct payment intent insert for user %d: %w", p.UserID, err)
	}
	if tag.RowsAffected() != 1 {
		return false, fmt.Errorf("start direct payment intent insert for user %d: inserted %d rows", p.UserID, tag.RowsAffected())
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("start direct payment intent commit for user %d: %w", p.UserID, err)
	}
	return true, nil
}

func (s *Storage) AttachDirectProviderPayment(ctx context.Context, idemKey string, payment YookassaPayment, metadata json.RawMessage) error {
	if idemKey == "" {
		return fmt.Errorf("attach direct provider payment: empty idempotence key")
	}
	if payment.ID == "" {
		return fmt.Errorf("attach direct provider payment %s: empty provider payment id", idemKey)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE payments
		 SET provider_payment_charge_id = $2,
		     amount = $3,
		     currency = $4,
		     metadata = $5,
		     raw_payload = $6,
		     provider_paid_at = $7
		 WHERE payment_intent_key = $1
		   AND status = 'pending'
		   AND (provider_payment_charge_id IS NULL OR provider_payment_charge_id = $2)
		   AND (payment_intent_key IS NULL OR payment_intent_key NOT LIKE 'recurring:%')`,
		idemKey, payment.ID, payment.AmountKopecks, payment.Currency, metadata, payment.Raw, payment.ProviderPaidAt,
	)
	if err != nil {
		return fmt.Errorf("attach direct provider payment %s/%s: %w", idemKey, payment.ID, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("attach direct provider payment %s/%s: updated %d rows", idemKey, payment.ID, tag.RowsAffected())
	}
	return nil
}

func (s *Storage) FailDirectPaymentIntent(ctx context.Context, idemKey string, reason string) error {
	if idemKey == "" {
		return fmt.Errorf("fail direct payment intent: empty idempotence key")
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE payments
		 SET status = 'failed',
		     failed_at = now(),
		     raw_payload = jsonb_build_object('error', left($2, 500))
		 WHERE payment_intent_key = $1
		   AND status = 'pending'
		   AND provider_payment_charge_id IS NULL
		   AND (payment_intent_key IS NULL OR payment_intent_key NOT LIKE 'recurring:%')`,
		idemKey, reason,
	)
	if err != nil {
		return fmt.Errorf("fail direct payment intent %s: %w", idemKey, err)
	}
	if tag.RowsAffected() > 1 {
		return fmt.Errorf("fail direct payment intent %s: updated %d rows", idemKey, tag.RowsAffected())
	}
	return nil
}

func (s *Storage) CreatePendingPayment(ctx context.Context, p PaymentInput, idemKey string, email string) error {
	if idemKey == "" {
		return fmt.Errorf("create pending payment for user %d: empty idempotence key", p.UserID)
	}
	if p.IntentKey != "" && p.IntentKey != idemKey {
		return fmt.Errorf("create pending payment for user %d: mismatched idempotence key", p.UserID)
	}
	if p.ProviderChargeID == "" {
		return fmt.Errorf("create pending payment for user %d: empty provider payment id", p.UserID)
	}

	metadata := p.Metadata
	if len(metadata) == 0 && email != "" {
		var err error
		metadata, err = json.Marshal(map[string]string{"payment_email": email})
		if err != nil {
			return fmt.Errorf("marshal payment metadata for user %d: %w", p.UserID, err)
		}
	}

	tag, err := s.pool.Exec(ctx,
		`INSERT INTO payments (user_id, status, amount, currency,
		     provider_payment_charge_id, payment_intent_key, payment_method_id, invoice_payload,
		     metadata, raw_payload, provider_paid_at)
		 VALUES ($1, 'pending', $2, $3, $4, $5, NULLIF($6, ''), $7, $8, $9, $10)
		 ON CONFLICT DO NOTHING`,
		p.UserID, p.AmountKopecks, p.Currency, p.ProviderChargeID, idemKey, p.PaymentMethodID,
		p.InvoicePayload, metadata, p.RawPayload, p.ProviderPaidAt,
	)
	if err != nil {
		return fmt.Errorf("create pending payment for user %d: %w", p.UserID, err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}

	rows, err := s.pool.Query(ctx,
		`SELECT user_id, amount, currency, provider_payment_charge_id, payment_intent_key
		 FROM payments
		 WHERE provider_payment_charge_id = $1 OR payment_intent_key = $2`,
		p.ProviderChargeID, idemKey,
	)
	if err != nil {
		return fmt.Errorf("create pending payment lookup conflict for user %d: %w", p.UserID, err)
	}
	defer rows.Close()

	var matches int
	for rows.Next() {
		var userID int64
		var amount int64
		var currency string
		var providerID, intentKey *string
		if err := rows.Scan(&userID, &amount, &currency, &providerID, &intentKey); err != nil {
			return fmt.Errorf("create pending payment scan conflict for user %d: %w", p.UserID, err)
		}
		matches++
		if userID != p.UserID || amount != p.AmountKopecks || currency != p.Currency ||
			providerID == nil || *providerID != p.ProviderChargeID ||
			intentKey == nil || *intentKey != idemKey {
			return fmt.Errorf("create pending payment conflict for user %d", p.UserID)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("create pending payment iterate conflict for user %d: %w", p.UserID, err)
	}
	if matches != 1 {
		return fmt.Errorf("create pending payment conflict for user %d: matched %d rows", p.UserID, matches)
	}
	return nil
}

func (s *Storage) HasFreshPendingPayment(ctx context.Context, userID int64, since time.Time) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (
		     SELECT 1 FROM payments
		     WHERE user_id = $1
		       AND status = 'pending'
			       AND created_at >= $2
			       AND (payment_intent_key IS NULL OR payment_intent_key NOT LIKE 'recurring:%')
			 )`,
		userID, since,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check fresh pending payment for user %d: %w", userID, err)
	}
	return exists, nil
}

func (s *Storage) PendingPayments(ctx context.Context, limit int) ([]Payment, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, user_id, subscription_id, status, amount, currency,
		        telegram_payment_charge_id, provider_payment_charge_id, payment_intent_key,
		        payment_method_id, invoice_payload, fiscal_receipt_url, metadata, raw_payload,
		        created_at, paid_at, provider_paid_at, failed_at
		 FROM payments
		 WHERE status = 'pending'
		   AND provider_payment_charge_id IS NOT NULL
		   AND created_at < now() - INTERVAL '5 seconds'
		 ORDER BY created_at
		 LIMIT $1`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("select pending payments: %w", err)
	}
	defer rows.Close()

	var payments []Payment
	for rows.Next() {
		var p Payment
		if err := rows.Scan(
			&p.ID, &p.UserID, &p.SubscriptionID, &p.Status, &p.Amount, &p.Currency,
			&p.TelegramPaymentChargeID, &p.ProviderPaymentChargeID, &p.PaymentIntentKey,
			&p.PaymentMethodID, &p.InvoicePayload, &p.FiscalReceiptURL, &p.Metadata, &p.RawPayload,
			&p.CreatedAt, &p.PaidAt, &p.ProviderPaidAt, &p.FailedAt,
		); err != nil {
			return nil, fmt.Errorf("scan pending payment: %w", err)
		}
		payments = append(payments, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending payments: %w", err)
	}
	return payments, nil
}

func (s *Storage) FinalizeYookassaPayment(ctx context.Context, paymentID string, raw json.RawMessage, providerPaidAt *time.Time, minutesPerPeriod int, paymentMethodID string, paymentMethodSaved bool) (PaymentActivation, error) {
	result := PaymentActivation{}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return result, fmt.Errorf("finalize yookassa payment begin tx %s: %w", paymentID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var paymentRowID int64
	var subscriptionID *int64
	var status PaymentStatus
	var metadata json.RawMessage
	var intentKey *string
	err = tx.QueryRow(ctx,
		`SELECT id, user_id, subscription_id, status, metadata, payment_intent_key
		 FROM payments
		 WHERE provider_payment_charge_id = $1
		 FOR UPDATE`,
		paymentID,
	).Scan(&paymentRowID, &result.UserID, &subscriptionID, &status, &metadata, &intentKey)
	if err != nil {
		return result, fmt.Errorf("finalize yookassa payment lock %s: %w", paymentID, err)
	}
	result.RecurringPayment = intentKey != nil && strings.HasPrefix(*intentKey, "recurring:")
	if status == PaymentSucceeded || status == PaymentRefunded {
		return result, nil
	}
	if status != PaymentPending {
		return result, fmt.Errorf("finalize yookassa payment %s: unexpected status %s", paymentID, status)
	}
	if subscriptionID != nil {
		return result, fmt.Errorf("finalize yookassa payment %s: pending payment already has subscription %d", paymentID, *subscriptionID)
	}

	var lockedUserID int64
	err = tx.QueryRow(ctx,
		`SELECT telegram_id FROM users WHERE telegram_id = $1 FOR UPDATE`,
		result.UserID,
	).Scan(&lockedUserID)
	if err != nil {
		return result, fmt.Errorf("finalize yookassa payment lock user %d: %w", result.UserID, err)
	}

	var subID int64
	var newSubExpires time.Time
	err = tx.QueryRow(ctx,
		`INSERT INTO subscriptions (user_id, paid_at, expires_at,
		     meetings_limit, meetings_balance, minutes_limit, minutes_balance)
		 VALUES ($1, now(), now() + INTERVAL '30 days',
		         $2 / 60, $2 / 60, $2, $2)
		 RETURNING id, expires_at`,
		result.UserID, minutesPerPeriod,
	).Scan(&subID, &newSubExpires)
	if err != nil {
		return result, fmt.Errorf("finalize yookassa payment create subscription %s: %w", paymentID, err)
	}
	result.ExpiresAt = newSubExpires

	autoRenew := paymentAutoRenew(metadata)
	storeMethod := autoRenew && !result.RecurringPayment && paymentMethodSaved && paymentMethodID != ""
	tag, err := tx.Exec(ctx,
		`UPDATE payments
		 SET status = 'succeeded',
		     subscription_id = $2,
		     payment_method_id = CASE WHEN $5 THEN $6 ELSE payment_method_id END,
		     paid_at = now(),
		     provider_paid_at = $3,
		     raw_payload = $4
		 WHERE id = $1 AND status = 'pending' AND subscription_id IS NULL`,
		paymentRowID, subID, providerPaidAt, raw, storeMethod, paymentMethodID,
	)
	if err != nil {
		return result, fmt.Errorf("finalize yookassa payment update %s: %w", paymentID, err)
	}
	if tag.RowsAffected() != 1 {
		return result, fmt.Errorf("finalize yookassa payment update %s: updated %d rows", paymentID, tag.RowsAffected())
	}

	err = tx.QueryRow(ctx,
		`SELECT MAX(expires_at)
		 FROM subscriptions
		 WHERE user_id = $1 AND expires_at > now()`,
		result.UserID,
	).Scan(&result.ExpiresAt)
	if err != nil {
		return result, fmt.Errorf("finalize yookassa payment effective expiry %s: %w", paymentID, err)
	}

	if storeMethod {
		_, err = tx.Exec(ctx,
			`INSERT INTO recurring_profiles (
			     user_id, payment_method_id, status, next_charge_at,
			     last_payment_id, attempts_count, last_error, consent_at, disabled_at, updated_at
			 )
			 VALUES ($1, $2, 'active', $3, $4, 0, NULL, now(), NULL, now())
			 ON CONFLICT (user_id) DO UPDATE
			 SET payment_method_id = EXCLUDED.payment_method_id,
			     status = 'active',
			     next_charge_at = EXCLUDED.next_charge_at,
			     last_payment_id = EXCLUDED.last_payment_id,
			     attempts_count = 0,
			     last_error = NULL,
			     consent_at = now(),
			     disabled_at = NULL,
			     updated_at = now()`,
			result.UserID, paymentMethodID, result.ExpiresAt, paymentRowID,
		)
		if err != nil {
			return result, fmt.Errorf("finalize yookassa payment enable recurring %s: %w", paymentID, err)
		}
		result.RecurringActive = true
		result.NextChargeAt = result.ExpiresAt
	} else if autoRenew && result.RecurringPayment {
		// blocked профиль оставляем как есть: он требует ручной проверки, и запоздалый
		// success in-flight платежа не должен молча его переактивировать. Подписка выше
		// уже продлена; автопродление возобновит только оператор.
		tag, err = tx.Exec(ctx,
			`UPDATE recurring_profiles
			 SET status = 'active',
			     next_charge_at = $2,
			     last_payment_id = $3,
			     attempts_count = 0,
			     last_error = NULL,
			     updated_at = now()
			 WHERE user_id = $1 AND status NOT IN ('disabled', 'blocked')`,
			result.UserID, result.ExpiresAt, paymentRowID,
		)
		if err != nil {
			return result, fmt.Errorf("finalize yookassa payment renew recurring %s: %w", paymentID, err)
		}
		if tag.RowsAffected() == 1 {
			result.RecurringActive = true
			result.NextChargeAt = result.ExpiresAt
		}
	} else if autoRenew {
		slog.Warn("recurring method was not saved",
			"event", "recurring_method_not_saved", "phase", "payment_poll",
			"provider_payment_id", paymentID, "user_id", result.UserID, "method_saved", paymentMethodSaved)
	}

	if err := tx.Commit(ctx); err != nil {
		return result, fmt.Errorf("finalize yookassa payment commit %s: %w", paymentID, err)
	}
	result.Changed = true
	return result, nil
}

func paymentAutoRenew(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var payload struct {
		AutoRenew any             `json:"auto_renew"`
		Metadata  json.RawMessage `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return false
	}
	switch v := payload.AutoRenew.(type) {
	case bool:
		return v
	case string:
		return v == "true"
	}
	if len(payload.Metadata) > 0 && string(payload.Metadata) != "null" {
		return paymentAutoRenew(payload.Metadata)
	}
	return false
}

func recurringPaymentIntentKey(userID int64, nextChargeAt time.Time) string {
	return fmt.Sprintf("recurring:%d:%d", userID, nextChargeAt.UTC().Unix())
}

func (s *Storage) DueRecurringProfiles(ctx context.Context, limit int) ([]RecurringProfile, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx,
		`WITH due AS (
		     SELECT user_id, COALESCE(payment_method_id, '') AS payment_method_id, status, next_charge_at,
		            last_payment_id, attempts_count, last_error, consent_at, disabled_at
		     FROM recurring_profiles rp
		     WHERE (
		         status IN ('active', 'charge_failed')
		         OR (
		             status = 'charging'
		             AND EXISTS (
		                 SELECT 1 FROM payments p
			                 WHERE p.payment_intent_key = format('recurring:%s:%s', rp.user_id, floor(EXTRACT(EPOCH FROM rp.next_charge_at))::bigint)
			                   AND p.status = 'pending'
			                   AND p.provider_payment_charge_id IS NULL
			             )
			         )
			       )
			       AND next_charge_at <= now()
			       AND payment_method_id IS NOT NULL
			       AND payment_method_id <> ''
			       AND updated_at < now() - INTERVAL '2 minutes'
			       AND NOT EXISTS (
			           SELECT 1 FROM payments p
			           WHERE p.payment_intent_key = format('recurring:%s:%s', rp.user_id, floor(EXTRACT(EPOCH FROM rp.next_charge_at))::bigint)
			             AND (p.status = 'succeeded' OR (p.status = 'pending' AND p.provider_payment_charge_id IS NOT NULL))
			       )
		     ORDER BY next_charge_at, user_id
		     LIMIT $1
		     FOR UPDATE SKIP LOCKED
		 ), claimed AS (
		     UPDATE recurring_profiles rp
		     SET updated_at = now()
		     FROM due
		     WHERE rp.user_id = due.user_id
		     RETURNING due.user_id, due.payment_method_id, due.status, due.next_charge_at,
		               due.last_payment_id, due.attempts_count, due.last_error, due.consent_at, due.disabled_at, rp.updated_at
		 )
		 SELECT user_id, payment_method_id, status, next_charge_at,
		        last_payment_id, attempts_count, last_error, consent_at, disabled_at, updated_at
		 FROM claimed`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("select due recurring profiles: %w", err)
	}
	defer rows.Close()

	var profiles []RecurringProfile
	for rows.Next() {
		var p RecurringProfile
		if err := rows.Scan(
			&p.UserID, &p.PaymentMethodID, &p.Status, &p.NextChargeAt,
			&p.LastPaymentID, &p.AttemptsCount, &p.LastError, &p.ConsentAt, &p.DisabledAt, &p.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan due recurring profile: %w", err)
		}
		profiles = append(profiles, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate due recurring profiles: %w", err)
	}
	return profiles, nil
}

func (s *Storage) MarkRecurringAttempt(ctx context.Context, userID int64, providerPaymentID string, idemKey string) error {
	if idemKey == "" {
		return fmt.Errorf("mark recurring attempt for user %d: empty idempotence key", userID)
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE recurring_profiles
		 SET attempts_count = attempts_count + 1,
		     last_error = NULL,
		     updated_at = now()
		 WHERE user_id = $1 AND status = 'charging'`,
		userID,
	)
	if err != nil {
		return fmt.Errorf("mark recurring attempt for user %d payment %s key %s: %w", userID, providerPaymentID, idemKey, err)
	}
	return nil
}

func (s *Storage) MarkRecurringRetry(ctx context.Context, userID int64, reason string) error {
	if strings.TrimSpace(reason) == "" {
		reason = "recurring payment retry"
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE recurring_profiles
		 SET status = 'charge_failed',
		     last_error = left($2, 500),
		     updated_at = now()
		 WHERE user_id = $1 AND status = 'charging'`,
		userID, reason,
	)
	if err != nil {
		return fmt.Errorf("mark recurring retry for user %d: %w", userID, err)
	}
	return nil
}

func (s *Storage) MarkRecurringFailure(ctx context.Context, userID int64, reason string) (bool, error) {
	if strings.TrimSpace(reason) == "" {
		reason = "recurring payment failed"
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE recurring_profiles
		 SET status = 'charge_failed',
		     next_charge_at = now() + INTERVAL '24 hours',
		     last_error = left($2, 500),
		     updated_at = now()
		 WHERE user_id = $1 AND status IN ('charging', 'active', 'charge_failed')`,
		userID, reason,
	)
	if err != nil {
		return false, fmt.Errorf("mark recurring failure for user %d: %w", userID, err)
	}
	return tag.RowsAffected() > 0, nil
}

func (s *Storage) DisableRecurring(ctx context.Context, userID int64) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE recurring_profiles
		 SET status = 'disabled',
		     payment_method_id = NULL,
		     disabled_at = now(),
		     updated_at = now()
		 WHERE user_id = $1 AND status <> 'disabled'`,
		userID,
	)
	if err != nil {
		return false, fmt.Errorf("disable recurring for user %d: %w", userID, err)
	}
	return tag.RowsAffected() > 0, nil
}

func (s *Storage) BlockRecurring(ctx context.Context, userID int64, reason string) (bool, error) {
	if strings.TrimSpace(reason) == "" {
		reason = "recurring payment requires manual review"
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE recurring_profiles
		 SET status = 'blocked',
		     last_error = left($2, 500),
		     updated_at = now()
		 WHERE user_id = $1 AND status <> 'disabled'`,
		userID, reason,
	)
	if err != nil {
		return false, fmt.Errorf("block recurring for user %d: %w", userID, err)
	}
	return tag.RowsAffected() > 0, nil
}

func (s *Storage) FailYookassaPayment(ctx context.Context, paymentID string, raw json.RawMessage) (bool, int64, error) {
	var userID int64
	err := s.pool.QueryRow(ctx,
		`UPDATE payments
		 SET status = 'failed',
		     failed_at = now(),
		     raw_payload = $2
		 WHERE provider_payment_charge_id = $1 AND status = 'pending'
		 RETURNING user_id`,
		paymentID, raw,
	).Scan(&userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = s.pool.QueryRow(ctx,
				`SELECT user_id FROM payments WHERE provider_payment_charge_id = $1`,
				paymentID,
			).Scan(&userID)
			if err != nil {
				return false, 0, fmt.Errorf("fail yookassa payment lookup %s: %w", paymentID, err)
			}
			return false, userID, nil
		}
		return false, 0, fmt.Errorf("fail yookassa payment lookup %s: %w", paymentID, err)
	}
	return true, userID, nil
}

func (s *Storage) FailRecurringYookassaPayment(ctx context.Context, paymentID string, raw json.RawMessage, retryable bool, reason string) (bool, int64, bool, error) {
	if strings.TrimSpace(reason) == "" {
		reason = "recurring payment failed"
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, 0, false, fmt.Errorf("fail recurring yookassa payment begin tx %s: %w", paymentID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var paymentRowID int64
	var userID int64
	var status PaymentStatus
	err = tx.QueryRow(ctx,
		`SELECT id, user_id, status
		 FROM payments
		 WHERE provider_payment_charge_id = $1
		 FOR UPDATE`,
		paymentID,
	).Scan(&paymentRowID, &userID, &status)
	if err != nil {
		return false, 0, false, fmt.Errorf("fail recurring yookassa payment lock %s: %w", paymentID, err)
	}
	if status != PaymentPending {
		return false, userID, false, nil
	}

	tag, err := tx.Exec(ctx,
		`UPDATE payments
		 SET status = 'failed',
		     failed_at = now(),
		     raw_payload = $2
		 WHERE id = $1 AND status = 'pending'`,
		paymentRowID, raw,
	)
	if err != nil {
		return false, 0, false, fmt.Errorf("fail recurring yookassa payment update %s: %w", paymentID, err)
	}
	if tag.RowsAffected() != 1 {
		return false, 0, false, fmt.Errorf("fail recurring yookassa payment update %s: updated %d rows", paymentID, tag.RowsAffected())
	}

	if retryable {
		tag, err = tx.Exec(ctx,
			`UPDATE recurring_profiles
			 SET status = 'charge_failed',
			     next_charge_at = now() + INTERVAL '24 hours',
			     last_error = left($2, 500),
			     updated_at = now()
			 WHERE user_id = $1 AND status IN ('charging', 'active', 'charge_failed')`,
			userID, reason,
		)
	} else {
		tag, err = tx.Exec(ctx,
			`UPDATE recurring_profiles
			 SET status = 'blocked',
			     last_error = left($2, 500),
			     updated_at = now()
			 WHERE user_id = $1 AND status <> 'disabled'`,
			userID, reason,
		)
	}
	if err != nil {
		return false, 0, false, fmt.Errorf("fail recurring yookassa payment profile %s: %w", paymentID, err)
	}
	profileChanged := tag.RowsAffected() > 0

	if err := tx.Commit(ctx); err != nil {
		return false, 0, false, fmt.Errorf("fail recurring yookassa payment commit %s: %w", paymentID, err)
	}
	return true, userID, profileChanged, nil
}

// newIdempotenceKey генерирует UUID v4 на crypto/rand без внешних зависимостей
// (принцип «минимум кода»: не тащим google/uuid ради 16 байт). Ключ генерируется
// однократно в BeginRefund и переиспользуется на ретраях через сохранённую refunds-строку
// (fallback-SELECT): один и тот же idempotence_key уходит в заголовок Idempotence-Key
// ЮKassa, защищая от двойного списания (db.md §5.9, api/yookassa §3).
func newIdempotenceKey() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate idempotence key: %w", err)
	}
	b[6] = b[6]&0x0f | 0x40 // версия 4
	b[8] = b[8]&0x3f | 0x80 // вариант RFC 4122
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// BeginRefund — фаза 1 двухфазного refund (db.md §5.9): создаёт pending-возврат с
// idempotence_key ДО вызова ЮKassa. amount берём из payments (full-only, 1:1) и
// возвращаем — будущий billing.Refund положит его в REST-body POST /v3/refunds.
//
// refunds_active_idx (partial UNIQUE на payment_id WHERE status IN pending/succeeded) и
// UNIQUE(idempotence_key) физически закрывают гонку двойного BeginRefund: ON CONFLICT
// DO NOTHING гасит вставку, RowsAffected=0, и мы отдаём существующий активный возврат —
// идемпотентный повтор того же намерения (тот же idemKey переживает ретрай, api §3).
//
// Оба statement — в одной транзакции (как прочие денежные методы storage): INSERT и
// fallback-SELECT видят согласованный снимок относительно параллельной финализации.
//
// Сентинел ErrPaymentNotRefundable: платёж не succeeded/отсутствует И активного возврата нет.
func (s *Storage) BeginRefund(ctx context.Context, providerChargeID string) (refundID int64, idemKey string, amountKopecks int64, err error) {
	key, err := newIdempotenceKey()
	if err != nil {
		return 0, "", 0, fmt.Errorf("begin refund for charge %s: %w", providerChargeID, err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, "", 0, fmt.Errorf("begin refund tx for charge %s: %w", providerChargeID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// INSERT...SELECT берёт amount из платежа (full-only refund). ON CONFLICT DO NOTHING
	// гасится либо refunds_active_idx (уже есть активный возврат), либо UNIQUE idempotence_key.
	err = tx.QueryRow(ctx,
		`INSERT INTO refunds (payment_id, idempotence_key, amount, status)
		 SELECT id, $2, amount, 'pending' FROM payments
		 WHERE provider_payment_charge_id = $1 AND status = 'succeeded'
		 ON CONFLICT DO NOTHING
		 RETURNING id, idempotence_key, amount`,
		providerChargeID, key,
	).Scan(&refundID, &idemKey, &amountKopecks)
	switch {
	case err == nil:
		// вставили новый pending-возврат
	case errors.Is(err, pgx.ErrNoRows):
		// RowsAffected=0: либо платёж не succeeded/нет, либо уже есть активный возврат.
		// Достаём существующий активный возврат — идемпотентный повтор отдаёт его idemKey.
		err = tx.QueryRow(ctx,
			`SELECT r.id, r.idempotence_key, r.amount FROM refunds r
			 JOIN payments p ON p.id = r.payment_id
			 WHERE p.provider_payment_charge_id = $1 AND r.status IN ('pending','succeeded')
			 LIMIT 1`,
			providerChargeID,
		).Scan(&refundID, &idemKey, &amountKopecks)
		switch {
		case err == nil:
			// нашли существующий активный возврат
		case errors.Is(err, pgx.ErrNoRows):
			// Ни succeeded-платежа, ни активного возврата — возвращать нечего.
			return 0, "", 0, ErrPaymentNotRefundable
		default:
			return 0, "", 0, fmt.Errorf("begin refund lookup for charge %s: %w", providerChargeID, err)
		}
	default:
		return 0, "", 0, fmt.Errorf("begin refund insert for charge %s: %w", providerChargeID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, "", 0, fmt.Errorf("begin refund commit for charge %s: %w", providerChargeID, err)
	}
	return refundID, idemKey, amountKopecks, nil
}

// FinalizeRefund — фаза 2 двухфазного refund (db.md §5.9): после ответа ЮKassa фиксирует
// результат в одной транзакции. Идемпотентна по refundID через guard WHERE status='pending'.
//
// При status='succeeded': помечаем платёж refunded, деактивируем ровно ту строку подписки
// что купил платёж (модель 1:1, expires_at=now()), и отключаем recurring profile. При failed/canceled платёж и подписку
// НЕ трогаем — деньги не вернулись, доступ сохраняется. Повторная финализация (повторный
// webhook/триггер) даёт RowsAffected=0 на шаге 1 → no-op commit, err=nil.
//
// providerRefundID пустой пишем как NULL (partial-unique refunds_provider_idx не считает
// NULL коллизией); raw nil → JSONB NULL.
func (s *Storage) FinalizeRefund(ctx context.Context, refundID int64, providerRefundID string, status RefundStatus, raw []byte) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("finalize refund %d begin tx: %w", refundID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Пустой provider_refund_id → NULL: иначе пустые строки от failed/canceled столкнулись бы
	// в refunds_provider_idx (provider_refund_id известен только для succeeded).
	var provArg *string
	if providerRefundID != "" {
		provArg = &providerRefundID
	}

	var paymentID int64
	err = tx.QueryRow(ctx,
		`UPDATE refunds SET status = $2, provider_refund_id = $3, raw_payload = $4, completed_at = now()
		 WHERE id = $1 AND status = 'pending'
		 RETURNING payment_id`,
		refundID, status, provArg, raw,
	).Scan(&paymentID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Уже финализирован (повторный триггер/webhook) — no-op идемпотентно.
		// Ничего не записали: defer Rollback закроет пустую транзакцию, Commit не нужен.
		return nil
	}
	if err != nil {
		return fmt.Errorf("finalize refund %d: %w", refundID, err)
	}

	var paymentUserID int64
	var recurringDisabled bool

	// failed/canceled: деньги не вернулись — платёж и подписку оставляем как есть.
	if status == RefundSucceeded {
		var subID *int64
		err = tx.QueryRow(ctx,
			`UPDATE payments SET status = 'refunded'
			 WHERE id = $1 AND status = 'succeeded'
			 RETURNING subscription_id, user_id`,
			paymentID,
		).Scan(&subID, &paymentUserID)
		switch {
		case err == nil:
			// платёж помечен refunded; subID может быть NULL у edge-платежа без линковки
		case errors.Is(err, pgx.ErrNoRows):
			// Инвариант: refund заводится только для succeeded-платежа (BeginRefund), значит
			// сюда платёж приходит succeeded. 0 строк = платёж уже не succeeded (рассинхрон/
			// ручная правка). Откатываем всю транзакцию (defer Rollback) вместо commit: иначе
			// зафиксировали бы refunds.status='succeeded' при неотменённом платеже — fail-silent
			// на денежном пути. Возврат остаётся pending = видимое «не завершено» для оператора.
			return fmt.Errorf("finalize refund %d: payment %d not in succeeded state, rolling back", refundID, paymentID)
		default:
			return fmt.Errorf("finalize refund %d mark payment refunded: %w", refundID, err)
		}

		// Деактивируем ровно ту строку подписки (модель 1:1). Guard expires_at > now()
		// делает повтор безвредным. subID может быть NULL у edge-платежа без линковки.
		if subID != nil {
			_, err = tx.Exec(ctx,
				`UPDATE subscriptions SET expires_at = now()
				 WHERE id = $1 AND expires_at > now()`,
				*subID,
			)
			if err != nil {
				return fmt.Errorf("finalize refund %d deactivate subscription: %w", refundID, err)
			}
		}

		tag, err := tx.Exec(ctx,
			`UPDATE recurring_profiles
			 SET status = 'disabled',
			     payment_method_id = NULL,
			     disabled_at = COALESCE(disabled_at, now()),
			     updated_at = now()
			 WHERE user_id = $1 AND status <> 'disabled'`,
			paymentUserID,
		)
		if err != nil {
			return fmt.Errorf("finalize refund %d disable recurring profile: %w", refundID, err)
		}
		recurringDisabled = tag.RowsAffected() > 0
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("finalize refund %d commit: %w", refundID, err)
	}
	if status == RefundSucceeded {
		slog.Info("refund finalized",
			"event", "refund_finalized", "phase", "refund",
			"refund_id", refundID, "payment_id", paymentID, "refund_status", status,
			"user_id", paymentUserID, "recurring_disabled", recurringDisabled)
	} else {
		slog.Info("refund finalized",
			"event", "refund_finalized", "phase", "refund",
			"refund_id", refundID, "payment_id", paymentID, "refund_status", status)
	}
	return nil
}

// SubExpiringUsers — кандидаты F9-пуша «подписка истекает через 3 дня» (db.md §5.6).
// Агрегация по юзеру (модель 1:1 — несколько активных строк), пуш по КОНЦУ покрытия
// MAX(expires_at), ровно один на юзера. Если осталось меньше часа, приоритет у
// sub_low_balance: так один тик не шлёт два подписочных пуша подряд. Дедуп — notified < COALESCE(MAX(paid_at),
// -infinity): COALESCE спасает от MAX=NULL (сравнение с NULL дало бы NULL и строка прошла бы),
// первый пуш ловит ветка notified IS NULL, повторный после нового платежа — сдвиг MAX(paid_at).
func (s *Storage) SubExpiringUsers(ctx context.Context) ([]int64, error) {
	rows, err := s.pool.Query(ctx,
		`WITH coverage AS (
		     SELECT user_id, MAX(expires_at) AS expires_at, SUM(minutes_balance) AS balance
		     FROM subscriptions
		     WHERE expires_at > now() AND minutes_balance > 0
		     GROUP BY user_id
		     HAVING MAX(expires_at) BETWEEN now() + INTERVAL '3 days' AND now() + INTERVAL '4 days'
		        AND SUM(minutes_balance) >= 60
		 ),
		 paid AS (
		     SELECT user_id, MAX(paid_at) AS paid_at
		     FROM payments
		     WHERE status = 'succeeded'
		     GROUP BY user_id
		 )
		 SELECT u.telegram_id
		 FROM users u
		 JOIN coverage c ON c.user_id = u.telegram_id
		 LEFT JOIN paid p ON p.user_id = u.telegram_id
		 WHERE u.notified_sub_expiring_at IS NULL
		    OR u.notified_sub_expiring_at < COALESCE(p.paid_at, '-infinity'::timestamptz)`,
	)
	if err != nil {
		return nil, fmt.Errorf("sub expiring users: %w", err)
	}
	return scanUserIDs(rows, "sub expiring users")
}

// SubExpiredUsers — кандидаты F9-пуша «подписка истекла» (db.md §5.6, по образцу).
// Отличие от SubExpiring: у юзера НЕТ активного покрытия (нет строки expires_at>now AND
// minutes_balance>0), но есть succeeded-платёж и MAX(expires_at) уже в прошлом. JOIN на ВСЕ строки
// подписок (без фильтра expires_at) — иначе истёкшие строки выпали бы и MAX не посчитался;
// отсутствие активного покрытия проверяем через NOT EXISTS. INNER JOIN на payments (а не LEFT):
// «истекла» имеет смысл только для платившего юзера. Дедуп — тот же COALESCE-паттерн §5.6.
// Refund-юзер (единственный платёж стал refunded) сюда НЕ попадает — INNER JOIN на 'succeeded':
// возврат = добровольный выход с возвратом денег, не естественное истечение (намеренно).
func (s *Storage) SubExpiredUsers(ctx context.Context) ([]int64, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT u.telegram_id
		 FROM users u
		 JOIN subscriptions s ON s.user_id = u.telegram_id
		 JOIN payments p ON p.user_id = u.telegram_id AND p.status = 'succeeded'
		 WHERE NOT EXISTS (
		     SELECT 1 FROM subscriptions a
		     WHERE a.user_id = u.telegram_id AND a.expires_at > now() AND a.minutes_balance > 0
		 )
		 GROUP BY u.telegram_id, u.notified_sub_expired_at
		 HAVING MAX(s.expires_at) < now()
		    AND (u.notified_sub_expired_at IS NULL
		         OR u.notified_sub_expired_at < COALESCE(MAX(p.paid_at), '-infinity'::timestamptz))`,
	)
	if err != nil {
		return nil, fmt.Errorf("sub expired users: %w", err)
	}
	return scanUserIDs(rows, "sub expired users")
}

// PaymentViewedWithoutPaidUsers — кандидаты дожима после просмотра оплаты без успешной оплаты.
// Событие должно быть старше 24 часов; активную подписку и оплату после просмотра исключаем.
func (s *Storage) PaymentViewedWithoutPaidUsers(ctx context.Context) ([]int64, error) {
	rows, err := s.pool.Query(ctx,
		`WITH viewed AS (
		     SELECT user_id, MAX(occurred_at) AS viewed_at
		     FROM analytics_events
		     WHERE event_type IN ($1, $2)
		     GROUP BY user_id
		     HAVING MAX(occurred_at) < now() - INTERVAL '24 hours'
		 )
		 SELECT v.user_id
		 FROM viewed v
		 WHERE NOT EXISTS (
		     SELECT 1 FROM subscriptions s
		     WHERE s.user_id = v.user_id AND s.expires_at > now()
		 )
		   AND NOT EXISTS (
		     SELECT 1 FROM payments p
		     WHERE p.user_id = v.user_id
		       AND p.status = 'succeeded'
		       AND p.paid_at >= v.viewed_at
		 )
		   AND NOT EXISTS (
		     SELECT 1 FROM analytics_events e
		     WHERE e.user_id = v.user_id
		       AND e.event_type = $3
		       AND e.event_key = $4
		       AND e.occurred_at >= v.viewed_at
		 )`,
		analyticsSubscriptionViewed, analyticsPaymentRequested, analyticsNudgeSent, nudgePaymentFollowup,
	)
	if err != nil {
		return nil, fmt.Errorf("payment viewed without paid users: %w", err)
	}
	return scanUserIDs(rows, "payment viewed without paid users")
}

// SubLowBalanceUsers — платящие пользователи с активным календарным периодом и остатком < 60 минут.
func (s *Storage) SubLowBalanceUsers(ctx context.Context) ([]int64, error) {
	rows, err := s.pool.Query(ctx,
		`WITH balances AS (
		     SELECT s.user_id, SUM(s.minutes_balance) AS balance
		     FROM subscriptions s
		     WHERE s.expires_at > now()
		     GROUP BY s.user_id
		     HAVING SUM(s.minutes_balance) > 0 AND SUM(s.minutes_balance) < 60
		 ),
		 paid AS (
		     SELECT user_id, MAX(paid_at) AS paid_at
		     FROM payments
		     WHERE status = 'succeeded'
		     GROUP BY user_id
		 )
		 SELECT b.user_id
		 FROM balances b
		 JOIN paid p ON p.user_id = b.user_id
		 WHERE NOT EXISTS (
		     SELECT 1 FROM jobs j
		     WHERE j.user_id = b.user_id
		       AND j.is_trial = false
		       AND j.reserved_minutes > 0
		       AND j.status NOT IN ('done','failed_join','failed','failed_summary')
		 )
		   AND NOT EXISTS (
		     SELECT 1 FROM analytics_events e
		     WHERE e.user_id = b.user_id
		       AND e.event_type = $1
		       AND e.event_key = $2
		       AND e.occurred_at >= COALESCE(p.paid_at, '-infinity'::timestamptz)
		 )`,
		analyticsNudgeSent, nudgeSubLowBalance,
	)
	if err != nil {
		return nil, fmt.Errorf("sub low balance users: %w", err)
	}
	return scanUserIDs(rows, "sub low balance users")
}

// SubEmptyBalanceUsers — платящие пользователи, у которых часы закончились до календарного expiry.
func (s *Storage) SubEmptyBalanceUsers(ctx context.Context) ([]int64, error) {
	rows, err := s.pool.Query(ctx,
		`WITH balances AS (
		     SELECT s.user_id, SUM(s.minutes_balance) AS balance
		     FROM subscriptions s
		     WHERE s.expires_at > now()
		     GROUP BY s.user_id
		     HAVING SUM(s.minutes_balance) = 0
		 ),
		 paid AS (
		     SELECT user_id, MAX(paid_at) AS paid_at
		     FROM payments
		     WHERE status = 'succeeded'
		     GROUP BY user_id
		 )
		 SELECT b.user_id
		 FROM balances b
		 JOIN paid p ON p.user_id = b.user_id
		 WHERE NOT EXISTS (
		     SELECT 1 FROM jobs j
		     WHERE j.user_id = b.user_id
		       AND j.is_trial = false
		       AND j.reserved_minutes > 0
		       AND j.status NOT IN ('done','failed_join','failed','failed_summary')
		 )
		   AND NOT EXISTS (
		     SELECT 1 FROM analytics_events e
		     WHERE e.user_id = b.user_id
		       AND e.event_type = $1
		       AND e.event_key = $2
		       AND e.occurred_at >= COALESCE(p.paid_at, '-infinity'::timestamptz)
		 )`,
		analyticsNudgeSent, nudgeSubEmptyBalance,
	)
	if err != nil {
		return nil, fmt.Errorf("sub empty balance users: %w", err)
	}
	return scanUserIDs(rows, "sub empty balance users")
}

// TrialEndingUsers — кандидаты F9-пуша «триал заканчивается»: осталось меньше часа
// (trialLimitMinutes - trial_minutes_done < 60) и пуша ещё не было.
// Считаем по trial_minutes_done (реально завершённые), не по used (резерв) — иначе пуш
// сработал бы на сфейленной встрече, которая откатила резерв (architecture.md §7.2).
// Дедуп простой «notify once» через IS NULL: у триала нет paid_at, в MVP он не сбрасывается.
// NOT EXISTS на календарно активную подписку — платящим юзерам этот пуш не нужен.
func (s *Storage) TrialEndingUsers(ctx context.Context, trialLimitMinutes int) ([]int64, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT u.telegram_id
		 FROM users u
		 WHERE GREATEST($1 - u.trial_minutes_done, 0) < 60
		   AND u.notified_trial_low_at IS NULL
		   AND NOT EXISTS (
		       SELECT 1 FROM subscriptions s
		       WHERE s.user_id = u.telegram_id AND s.expires_at > now()
		   )`,
		trialLimitMinutes,
	)
	if err != nil {
		return nil, fmt.Errorf("trial ending users: %w", err)
	}
	return scanUserIDs(rows, "trial ending users")
}

// scanUserIDs собирает telegram_id из строк трёх F9-выборок (один формат результата).
func scanUserIDs(rows pgx.Rows, op string) ([]int64, error) {
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("%s scan: %w", op, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s rows: %w", op, err)
	}
	return ids, nil
}

// MarkTrialLowNotified ставит notified_trial_low_at=now() после успешного пуша (идемпотентно
// перезаписывает — дедуп выборки страхует от повторной отправки между тиками).
func (s *Storage) MarkTrialLowNotified(ctx context.Context, userID int64) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE users SET notified_trial_low_at = now() WHERE telegram_id = $1`, userID)
	if err != nil {
		return fmt.Errorf("mark trial low notified %d: %w", userID, err)
	}
	return nil
}

func (s *Storage) MarkTrialLowAndNudgeSent(ctx context.Context, userID int64) error {
	return s.markLegacyAndNudgeSent(ctx, userID, nudgeTrialEnding,
		`UPDATE users SET notified_trial_low_at = now() WHERE telegram_id = $1`)
}

// MarkSubExpiringNotified ставит notified_sub_expiring_at=now() после успешного пуша.
func (s *Storage) MarkSubExpiringNotified(ctx context.Context, userID int64) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE users SET notified_sub_expiring_at = now() WHERE telegram_id = $1`, userID)
	if err != nil {
		return fmt.Errorf("mark sub expiring notified %d: %w", userID, err)
	}
	return nil
}

func (s *Storage) MarkSubExpiringAndNudgeSent(ctx context.Context, userID int64) error {
	return s.markLegacyAndNudgeSent(ctx, userID, nudgeSubExpiring,
		`UPDATE users SET notified_sub_expiring_at = now() WHERE telegram_id = $1`)
}

// MarkSubExpiredNotified ставит notified_sub_expired_at=now() после успешного пуша.
func (s *Storage) MarkSubExpiredNotified(ctx context.Context, userID int64) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE users SET notified_sub_expired_at = now() WHERE telegram_id = $1`, userID)
	if err != nil {
		return fmt.Errorf("mark sub expired notified %d: %w", userID, err)
	}
	return nil
}

func (s *Storage) MarkSubExpiredAndNudgeSent(ctx context.Context, userID int64) error {
	return s.markLegacyAndNudgeSent(ctx, userID, nudgeSubExpired,
		`UPDATE users SET notified_sub_expired_at = now() WHERE telegram_id = $1`)
}

func (s *Storage) MarkNudgeSent(ctx context.Context, userID int64, class string) error {
	return s.TrackAnalyticsEvent(ctx, userID, analyticsNudgeSent, class, map[string]string{"class": class})
}

func (s *Storage) markLegacyAndNudgeSent(ctx context.Context, userID int64, class string, updateSQL string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin legacy nudge mark %s for user %d: %w", class, userID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, updateSQL, userID); err != nil {
		return fmt.Errorf("mark legacy nudge %s for user %d: %w", class, userID, err)
	}
	raw, err := json.Marshal(map[string]string{"class": class})
	if err != nil {
		return fmt.Errorf("marshal legacy nudge metadata %s for user %d: %w", class, userID, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO analytics_events (user_id, event_type, event_key, metadata)
		 VALUES ($1, $2, $3, $4::jsonb)`,
		userID, analyticsNudgeSent, class, raw,
	); err != nil {
		return fmt.Errorf("mark legacy nudge analytics %s for user %d: %w", class, userID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit legacy nudge mark %s for user %d: %w", class, userID, err)
	}
	return nil
}

// SetConsent фиксирует согласие 152-ФЗ (F0). Guard WHERE consent_at IS NULL
// делает операцию идемпотентной: повторное согласие не перетирает момент
// первого (юридически значим именно он). RowsAffected=0 здесь не ошибка —
// согласие уже было дано ранее.
func (s *Storage) SetConsent(ctx context.Context, telegramID int64) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE users SET consent_at = now() WHERE telegram_id = $1 AND consent_at IS NULL`,
		telegramID)
	if err != nil {
		return fmt.Errorf("set consent %d: %w", telegramID, err)
	}
	return nil
}
