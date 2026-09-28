package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Таймауты операций тика: одна зависшая операция (TCP к Postgres, медленный
// поставщик на S6) не должна молча заморозить единственный воркер навсегда.
// Стиль как в tg.go: каждый внешний вызов ограничен явно.
const (
	dbTimeout       = 5 * time.Second
	providerTimeout = 30 * time.Second
	// summaryTimeout отличается от providerTimeout: LLM-саммари — не HTTP-Fetch,
	// а генеративный вызов с иным профилем latency. Оборачивает весь Summarize целиком,
	// включая N+1 последовательных вызовов map-reduce на длинном транскрипте (openrouter.go).
	summaryTimeout = 180 * time.Second
)

// Orchestrator — воркер state machine джобов транскрибации (architecture.md §7).
// Одна горутина, без внутреннего параллелизма: достаточно для MVP,
// а внешний поставщик всё равно требует поллинга.
type Orchestrator struct {
	storage    *Storage
	provider   TranscriptionProvider
	delivery   DocumentDelivery
	summarizer Summarizer // провайдер саммари F3; подключается в следующей волне
	// notify шлёт НОВОЕ сообщение юзеру (нудж, фолбэк). editStatus РЕДАКТИРУЕТ
	// статус-сообщение звонка по id (принял → бот на встрече → готово/ошибка) —
	// один эволюционирующий месседж вместо потока новых. Оба — замыкания поверх *Bot.
	notify     func(chatID int64, text string) error
	editStatus func(chatID, messageID int64, text string) error
	tick       time.Duration // 30s в проде; короче для локальной разработки
	// maxJobAge — порог orphan/zombie-recovery: recording старше него
	// останавливается у provider и доходит через stop-result path.
	maxJobAge time.Duration
	// startTimeout — общий лимит фаз created/joining до начала записи.
	startTimeout time.Duration
	// submitMaxAttempts — best-effort лимит Submit в памяти одного процесса.
	submitMaxAttempts int
	submitAttempts    map[int64]int
	trialLimitMinutes int
	statusTexts       map[int64]string
}

func NewOrchestrator(
	s *Storage,
	p TranscriptionProvider,
	d DocumentDelivery,
	summarizer Summarizer,
	notify func(int64, string) error,
	editStatus func(int64, int64, string) error,
	tick time.Duration,
	maxJobAge time.Duration,
	startTimeout time.Duration,
	submitMaxAttempts int,
	trialLimitMinutes int,
) *Orchestrator {
	return &Orchestrator{
		storage:           s,
		provider:          p,
		delivery:          d,
		summarizer:        summarizer,
		notify:            notify,
		editStatus:        editStatus,
		tick:              tick,
		maxJobAge:         maxJobAge,
		startTimeout:      startTimeout,
		submitMaxAttempts: submitMaxAttempts,
		submitAttempts:    make(map[int64]int),
		trialLimitMinutes: trialLimitMinutes,
		statusTexts:       make(map[int64]string),
	}
}

// Run запускает тикер и гонит tickOnce до отмены ctx.
// Первый вызов — сразу при старте, не ждём первый тик: уменьшает задержку обработки
// при рестарте сервиса с уже существующими джобами.
func (o *Orchestrator) Run(ctx context.Context) {
	// Первый тик без ожидания. context.Canceled не логируем как ошибку:
	// это штатный shutdown, а не сбой — ERROR в логах был бы ложным сигналом.
	if err := o.tickOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("orchestrator tick", "err", err)
	}

	ticker := time.NewTicker(o.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := o.tickOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("orchestrator tick", "err", err)
			}
		}
	}
}

// tickOnce — одна итерация: claim джобов из БД и advance по state machine.
// Возвращает ошибку только если сам claim упал; ошибки отдельных джобов —
// slog.Error + continue, чтобы сбой одной не рушил обработку остальных.
func (o *Orchestrator) tickOnce(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, dbTimeout)
	jobs, err := o.storage.ClaimJobsForWork(cctx, o.tick, 10)
	cancel()
	if err != nil {
		return fmt.Errorf("claim jobs: %w", err)
	}

	for _, job := range jobs {
		o.advance(ctx, job)
	}
	return nil
}

// advance продвигает одну джобу на следующий шаг state machine.
// Ошибки логируются, функция не возвращает их: ретрай произойдёт на следующем тике.
func (o *Orchestrator) advance(ctx context.Context, job Job) {
	switch job.Status {
	case JobCreated:
		o.submitJob(ctx, job)
	case JobJoining, JobRecording, JobProcessing:
		// Все три фазы «бот у провайдера» питаются поллингом: claim возвращает
		// joining/recording наравне с processing, и без них джоба падала бы в
		// default-warn (Poll не вызвался), клеймясь каждый тик (BLOCKER B1).
		o.pollJob(ctx, job)
	case JobSummarizing:
		o.summarizeJob(ctx, job)
	default:
		// done/failed уже не должны проходить через claim, но если вдруг прошли — игнорируем.
		slog.Warn("orchestrator: unexpected job status in claim",
			"event", "job_claim_unexpected_status", "phase", "state_machine",
			"job_id", job.ID, "user_id", job.UserID, "provider", job.Provider,
			"current_status", job.Status)
	}
}

// submitJob отправляет бота на встречу и переводит джобу в joining (бот подключается).
func (o *Orchestrator) submitJob(ctx context.Context, job Job) {
	if startTimedOut(job, o.startTimeout) {
		slog.Warn("job start timed out before submit",
			"event", "job_start_timeout", "phase", "submit",
			"job_id", job.ID, "user_id", job.UserID, "provider", job.Provider,
			"current_status", job.Status, "start_timeout", o.startTimeout)
		o.failJob(ctx, job, fmt.Sprintf("timeout: start exceeded %s", o.startTimeout))
		delete(o.submitAttempts, job.ID)
		return
	}
	if o.submitMaxAttempts > 0 && o.submitAttempts[job.ID] >= o.submitMaxAttempts {
		slog.Warn("submit attempts exceeded before submit",
			"event", "provider_submit_attempts_exceeded", "phase", "submit",
			"job_id", job.ID, "user_id", job.UserID, "provider", job.Provider,
			"current_status", job.Status, "attempts", o.submitAttempts[job.ID],
			"max_attempts", o.submitMaxAttempts)
		o.failJob(ctx, job, fmt.Sprintf("submit attempts exceeded: %d", o.submitMaxAttempts))
		delete(o.submitAttempts, job.ID)
		return
	}
	slog.Info("provider submit started",
		"event", "provider_submit_started", "phase", "submit",
		"job_id", job.ID, "user_id", job.UserID, "provider", job.Provider,
		"current_status", job.Status)
	pctx, cancel := context.WithTimeout(ctx, providerTimeout)
	extID, err := o.provider.Submit(pctx, job)
	cancel()
	if err != nil {
		if o.submitMaxAttempts > 0 {
			o.submitAttempts[job.ID]++
		}
		slog.Error("submit job",
			"event", "provider_submit_failed", "phase", "submit",
			"err", err, "job_id", job.ID, "user_id", job.UserID,
			"provider", job.Provider, "current_status", job.Status,
			"attempts", o.submitAttempts[job.ID], "max_attempts", o.submitMaxAttempts)
		if permanentSubmitError(err) {
			o.failJob(ctx, job, fmt.Sprintf("permanent submit error: %v", err))
			delete(o.submitAttempts, job.ID)
			return
		}
		if o.submitMaxAttempts > 0 && o.submitAttempts[job.ID] >= o.submitMaxAttempts {
			o.failJob(ctx, job, fmt.Sprintf("submit attempts exceeded: %d", o.submitMaxAttempts))
			delete(o.submitAttempts, job.ID)
			return
		}
		return // ретрай на следующем тике
	}
	delete(o.submitAttempts, job.ID)

	// Submit уже случился (бот пошёл на встречу) — запись статуса не отменяем
	// по shutdown (WithoutCancel), иначе джоба остаётся created и после рестарта
	// на ту же встречу уйдёт второй бот. Симметрично SetJobDone в finishJob.
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbTimeout)
	defer cancel()
	if err := o.storage.SetJobSubmitted(dctx, job.ID, extID); err != nil {
		if errors.Is(err, ErrJobNotCreated) {
			// Юзер отменил джобу, пока шёл Submit (created→failed) — переход не применён.
			// Бот уже отправлен на встречу: останавливаем его, иначе осиротевшая запись
			// жжёт минуты. WithoutCancel: Submit необратим, stop не бросаем по shutdown.
			slog.Info("submit lost to concurrent cancel, stopping provider bot",
				"event", "provider_submit_cancel_race", "phase", "submit",
				"job_id", job.ID, "user_id", job.UserID, "provider", job.Provider,
				"current_status", job.Status, "external_job_id", extID)
			cctx, ccancel := context.WithTimeout(context.WithoutCancel(ctx), providerTimeout)
			if cerr := o.provider.Cancel(cctx, extID); cerr != nil {
				slog.Warn("cancel orphaned bot after lost submit",
					"event", "provider_cancel_orphan_failed", "phase", "cancel",
					"err", cerr, "job_id", job.ID, "user_id", job.UserID,
					"provider", job.Provider, "external_job_id", extID)
			}
			ccancel()
			return
		}
		slog.Error("set job submitted",
			"event", "job_transition_failed", "phase", "state_machine",
			"err", err, "job_id", job.ID, "user_id", job.UserID,
			"provider", job.Provider, "current_status", job.Status, "new_status", JobJoining,
			"external_job_id", extID)
		return
	}
	slog.Info("job submitted",
		"event", "job_transition", "phase", "state_machine",
		"job_id", job.ID, "user_id", job.UserID, "provider", job.Provider,
		"current_status", job.Status, "new_status", JobJoining, "external_job_id", extID)

	// Прогресс: редактируем статус-сообщение «принял» → «бот идёт на встречу,
	// подключается» (фаза joining). Честно «подключается», а НЕ «идёт запись»:
	// сигнал захода даёт следующий поллинг (OnCall → recording). Best-effort, без
	// спама (редактируем существующее сообщение).
	o.updateProgressStatus(job, "Бот подключается к встрече.")
}

// updateStatus обновляет статус-сообщение звонка: редактирует существующее (editStatus),
// иначе шлёт новое (notify-фолбэк, если id не сохранился). Best-effort, UX не критичен.
func (o *Orchestrator) updateStatus(job Job, text string) {
	if o.statusTexts[job.ID] == text {
		return
	}
	if job.StatusMessageID != nil {
		if err := o.editStatus(job.UserID, *job.StatusMessageID, text); err != nil {
			slog.Warn("edit status message", "err", err, "job_id", job.ID, "user_id", job.UserID)
			if err := o.notify(job.UserID, text); err != nil {
				slog.Warn("notify status after edit failure", "err", err, "job_id", job.ID, "user_id", job.UserID)
				return
			}
			o.statusTexts[job.ID] = text
			return
		}
		o.statusTexts[job.ID] = text
		return
	}
	if err := o.notify(job.UserID, text); err != nil {
		slog.Warn("notify status", "err", err, "job_id", job.ID, "user_id", job.UserID)
		return
	}
	o.statusTexts[job.ID] = text
}

func (o *Orchestrator) updateProgressStatus(job Job, text string) {
	if job.StatusMessageID == nil {
		return
	}
	o.updateStatus(job, text)
}

// maybeNotifyBalanceDepleted после финализации billable job шлёт отдельное сообщение,
// если у пользователя закончился минутный баланс.
func (o *Orchestrator) maybeNotifyBalanceDepleted(ctx context.Context, job Job) {
	bctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	bal, err := o.storage.Balance(bctx, job.UserID, o.trialLimitMinutes)
	if err != nil {
		slog.Warn("balance depleted check", "err", err, "job_id", job.ID)
		return
	}
	if bal.Remaining > 0 {
		return
	}
	if err := o.notify(job.UserID, "Баланс закончился"); err != nil {
		slog.Warn("balance depleted notify", "err", err, "job_id", job.ID, "user_id", job.UserID)
	}
}

// pollJob запрашивает статус у поставщика и реагирует на Done/Failed.
func (o *Orchestrator) pollJob(ctx context.Context, job Job) {
	// joining/recording/processing-джоба без ExternalJobID невозможна по построению
	// (SetJobSubmitted всегда пишет его), но проверяем инвариант явно: паника скроет
	// причину в логах.
	if job.ExternalJobID == nil {
		slog.Error("in-progress job has nil ExternalJobID",
			"event", "job_missing_external_id", "phase", "poll",
			"job_id", job.ID, "user_id", job.UserID, "provider", job.Provider,
			"current_status", job.Status)
		return
	}
	extID := *job.ExternalJobID

	if job.Status == JobJoining && startTimedOut(job, o.startTimeout) {
		slog.Warn("job start timed out while joining, cancelling provider bot",
			"event", "job_start_timeout", "phase", "cancel",
			"job_id", job.ID, "user_id", job.UserID, "provider", job.Provider,
			"current_status", job.Status, "external_job_id", extID,
			"start_timeout", o.startTimeout)
		cctx, ccancel := context.WithTimeout(ctx, providerTimeout)
		if err := o.provider.Cancel(cctx, extID); err != nil {
			slog.Warn("cancel start-timeout job at provider",
				"event", "provider_cancel_failed", "phase", "cancel",
				"err", err, "job_id", job.ID, "user_id", job.UserID,
				"provider", job.Provider, "external_job_id", extID)
		}
		ccancel()
		o.failJob(ctx, job, fmt.Sprintf("timeout: start exceeded %s", o.startTimeout))
		return
	}

	// Watchdog останавливает только активную запись. processing уже означает, что бот
	// вышел и provider готовит результат; повторный cancel по created_at мог бы зафейлить
	// длинную встречу сразу после ручного stop, не дождавшись transcript.
	if job.Status == JobRecording && o.maxJobAge > 0 && time.Since(job.CreatedAt) > o.maxJobAge {
		mctx, mcancel := context.WithTimeout(ctx, dbTimeout)
		marked, err := o.storage.MarkJobWatchdogStop(mctx, job.ID)
		mcancel()
		if err != nil {
			slog.Error("mark watchdog stop",
				"event", "job_transition_failed", "phase", "state_machine",
				"err", err, "job_id", job.ID, "user_id", job.UserID,
				"provider", job.Provider, "current_status", job.Status,
				"external_job_id", extID)
			return
		}
		if !marked {
			return
		}
		job.LastError = stringPtr(jobStopWatchdog)
		slog.Warn("job exceeded max age, stopping provider bot",
			"event", "job_orphan_timeout", "phase", "cancel",
			"job_id", job.ID, "user_id", job.UserID, "provider", job.Provider,
			"current_status", job.Status, "external_job_id", extID)
		cctx, ccancel := context.WithTimeout(ctx, providerTimeout)
		if err := o.provider.Cancel(cctx, extID); err != nil {
			slog.Warn("cancel stale job at provider",
				"event", "provider_cancel_failed", "phase", "cancel",
				"err", err, "job_id", job.ID, "user_id", job.UserID,
				"provider", job.Provider, "external_job_id", extID)
			ccancel()
			o.failJob(ctx, job, fmt.Sprintf("timeout: stop failed after %s: %v", o.maxJobAge, err))
			return
		}
		ccancel()
		o.moveToProcessing(ctx, job, extID, StatusTranscribing)
		return
	}

	pctx, cancel := context.WithTimeout(ctx, providerTimeout)
	status, err := o.provider.Poll(pctx, extID)
	cancel()
	if err != nil {
		slog.Error("poll job",
			"event", "provider_poll_failed", "phase", "poll",
			"err", err, "job_id", job.ID, "user_id", job.UserID,
			"provider", job.Provider, "current_status", job.Status, "external_job_id", extID)
		return // ретрай на следующем тике
	}

	switch status {
	case StatusPending:
		o.updateProgressStatus(job, "Готовлю подключение к встрече.")

	case StatusQueuedRecording:
		o.updateProgressStatus(job, "Бот в очереди на подключение. Обычно это занимает пару минут.")

	case StatusJoiningCall:
		o.updateProgressStatus(job, "Бот подключается к встрече.")

	case StatusOnCall:
		// Бот зашёл на встречу, идёт запись: joining -> recording. Статус-сообщение
		// редактируем ТОЛЬКО при moved=true (фаза реально сменилась); повторный тик на
		// уже-recording даёт moved=false и не спамит Edit (идемпотентность).
		rctx, rcancel := context.WithTimeout(ctx, dbTimeout)
		moved, err := o.storage.SetJobRecording(rctx, job.ID)
		rcancel()
		if err != nil {
			slog.Error("set job recording",
				"event", "job_transition_failed", "phase", "state_machine",
				"err", err, "job_id", job.ID, "user_id", job.UserID,
				"provider", job.Provider, "current_status", job.Status, "new_status", JobRecording,
				"external_job_id", extID)
			return
		}
		if moved {
			slog.Info("job recording",
				"event", "job_transition", "phase", "state_machine",
				"job_id", job.ID, "user_id", job.UserID, "provider", job.Provider,
				"current_status", job.Status, "new_status", JobRecording,
				"external_job_id", extID, "provider_status", status.String())
		}
		o.updateProgressStatus(job, "Бот уже на встрече и записывает разговор.")

	case StatusTranscribing:
		// Бот вышел, идёт фоновая расшифровка: -> processing. Этот переход освобождает
		// юзера (§5.8 перестаёт считать джобу занятой). Edit только при moved=true.
		o.moveToProcessing(ctx, job, extID, status)

	case StatusPaused:
		// Нехватка минут на счёте поставщика (только s2t).
		// На скелете ждём как Pending; таймаут/алерт — технический долг S6.
		slog.Warn("job paused at provider (insufficient balance?)",
			"event", "provider_poll_paused", "phase", "poll",
			"job_id", job.ID, "user_id", job.UserID, "provider", job.Provider,
			"current_status", job.Status, "external_job_id", extID,
			"provider_status", status.String())
		o.updateProgressStatus(job, "Обработка временно на паузе у сервиса расшифровки. Жду продолжения.")

	case StatusDone:
		if job.Provider == "speech2text" && job.Status == JobRecording {
			o.moveToProcessing(ctx, job, extID, status)
			job.Status = JobProcessing
		}
		o.finishTranscription(ctx, job, extID)

	case StatusFailed:
		if jobStopRequested(job) {
			if job.Status == JobRecording {
				o.moveToProcessing(ctx, job, extID, status)
				return
			}
			o.finishStoppedTranscription(ctx, job, extID)
			return
		}
		o.failJob(ctx, job, "provider reported failure")

	default:
		// Дрейф контракта (новый статус при S6) не должен молча выглядеть
		// как Pending — джоба поллилась бы вечно без следа в логах.
		slog.Error("unknown provider status",
			"event", "provider_status_unknown", "phase", "poll",
			"job_id", job.ID, "user_id", job.UserID, "provider", job.Provider,
			"current_status", job.Status, "external_job_id", extID,
			"provider_status", status.String())
	}
}

func (o *Orchestrator) moveToProcessing(ctx context.Context, job Job, extID string, status ProviderStatus) {
	tctx, tcancel := context.WithTimeout(ctx, dbTimeout)
	moved, err := o.storage.SetJobProcessing(tctx, job.ID)
	tcancel()
	if err != nil {
		slog.Error("set job processing",
			"event", "job_transition_failed", "phase", "state_machine",
			"err", err, "job_id", job.ID, "user_id", job.UserID,
			"provider", job.Provider, "current_status", job.Status, "new_status", JobProcessing,
			"external_job_id", extID)
		return
	}
	if moved {
		slog.Info("job processing",
			"event", "job_transition", "phase", "state_machine",
			"job_id", job.ID, "user_id", job.UserID, "provider", job.Provider,
			"current_status", job.Status, "new_status", JobProcessing,
			"external_job_id", extID, "provider_status", status.String())
	}
	o.updateProgressStatus(job, "Запись завершилась. Расшифровываю разговор.")
}

func jobStopRequested(job Job) bool {
	if job.LastError == nil {
		return false
	}
	return *job.LastError == jobStopManual || *job.LastError == jobStopWatchdog
}

func stringPtr(value string) *string {
	return &value
}

// finishTranscription забирает результат у поставщика, доставляет транскрипт
// юзеру и переводит джобу в summarizing (не в done: саммари ещё предстоит).
func (o *Orchestrator) finishTranscription(ctx context.Context, job Job, extID string) {
	result, ok := o.fetchJobResult(ctx, job, extID, false)
	if !ok {
		return
	}
	o.handleFetchedResult(ctx, job, extID, result)
}

func (o *Orchestrator) finishStoppedTranscription(ctx context.Context, job Job, extID string) {
	result, ok := o.fetchJobResult(ctx, job, extID, true)
	if !ok {
		return
	}
	o.handleFetchedResult(ctx, job, extID, result)
}

func (o *Orchestrator) fetchJobResult(ctx context.Context, job Job, extID string, failOnFetch bool) (Result, bool) {
	slog.Info("provider fetch started",
		"event", "provider_fetch_started", "phase", "fetch",
		"job_id", job.ID, "user_id", job.UserID, "provider", job.Provider,
		"current_status", job.Status, "external_job_id", extID)
	pctx, cancel := context.WithTimeout(ctx, providerTimeout)
	result, err := o.provider.Fetch(pctx, extID)
	cancel()
	if err != nil {
		if errors.Is(err, ErrEmptyTranscript) {
			slog.Warn("provider returned empty transcript",
				"event", "provider_empty_transcript", "phase", "fetch",
				"job_id", job.ID, "user_id", job.UserID,
				"provider", job.Provider, "current_status", job.Status, "external_job_id", extID)
			o.failJob(ctx, job, "empty transcript")
			return Result{}, false
		}
		slog.Error("fetch job result",
			"event", "provider_fetch_failed", "phase", "fetch",
			"err", err, "job_id", job.ID, "user_id", job.UserID,
			"provider", job.Provider, "current_status", job.Status, "external_job_id", extID)
		if failOnFetch {
			o.failJob(ctx, job, fmt.Sprintf("fetch after stop: %v", err))
		}
		return Result{}, false // обычный done-flow ретраится на следующем тике
	}
	slog.Info("provider fetch completed",
		"event", "provider_fetch_completed", "phase", "fetch",
		"job_id", job.ID, "user_id", job.UserID, "provider", job.Provider,
		"current_status", job.Status, "external_job_id", extID,
		"has_provider_summary", result.SummaryMarkdown != "",
		"provider_duration_seconds", result.DurationSeconds)
	return result, true
}

func (o *Orchestrator) handleFetchedResult(ctx context.Context, job Job, extID string, result Result) {
	durationSeconds := result.DurationSeconds
	if job.Provider == "speech2text" && job.Status == JobProcessing && job.DurationSeconds != nil {
		// Для s2t chunks duration отражает таймкоды распознанной речи, а не полное
		// присутствие бота. Если state machine уже зафиксировала recording window
		// при переходе в processing, billing возьмет сохранённое duration_seconds.
		durationSeconds = 0
	}

	// Стойко невалидный Raw (схема result/json у s2t фиксируется только живым
	// тестом) не должен зациклить доставку: SetJobSummarizing падал бы на JSONB каждый
	// тик, а Deliver уходил бы юзеру заново. Теряем отладочный raw, не корректность.
	// nil — легитимный путь (s2t 204 «речь не обнаружена» или сбой result/json),
	// не ошибка: json.Valid(nil) == false дал бы ложный ERROR на штатном сценарии.
	if result.Raw != nil && !json.Valid(result.Raw) {
		slog.Error("provider raw is not valid JSON, storing NULL",
			"event", "provider_raw_invalid", "phase", "fetch",
			"job_id", job.ID, "user_id", job.UserID, "provider", job.Provider,
			"current_status", job.Status, "external_job_id", extID)
		result.Raw = nil
	}

	// Сохраняем фазу после необратимого Fetch под WithoutCancel: если shutdown
	// прервёт этот участок, следующий тик заново пойдёт во внешний provider.
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbTimeout)
	defer cancel()

	// Provider-summary: mymeet посчитал саммари при обработке встречи — доставляем его
	// одной группой файлов и переходим в done, минуя summarizing/Summarizer. Выигрывает
	// над сконфигурированным Summarizer: при mymeet-провайдере SUMMARIZER_PROVIDER=off.
	if result.SummaryMarkdown != "" {
		if err := o.storage.SetJobSummary(dctx, job.ID, result.SummaryMarkdown); err != nil {
			slog.Error("cache provider summary before deliver",
				"event", "summary_cache_failed", "phase", "summary",
				"err", err, "job_id", job.ID, "user_id", job.UserID,
				"provider", job.Provider, "current_status", job.Status,
				"external_job_id", extID)
		}
		docs := resultDocuments(job.CreatedAt, result.SummaryMarkdown, result.TranscriptMarkdown)
		sdctx, sc := context.WithTimeout(context.WithoutCancel(ctx), providerTimeout)
		err := o.delivery.DeliverGroup(sdctx, job.UserID, "✅ Конспект и расшифровка встречи", docs)
		sc()
		if err != nil {
			slog.Error("deliver provider summary result",
				"event", "delivery_failed", "phase", "delivery",
				"err", err, "job_id", job.ID, "user_id", job.UserID,
				"provider", job.Provider, "current_status", job.Status,
				"external_job_id", extID, "document", "result_documents",
				"has_summary", true, "transcript_chars", len(result.TranscriptMarkdown),
				"summary_chars", len(result.SummaryMarkdown))
			return
		}
		slog.Info("provider summary result delivered",
			"event", "delivery_completed", "phase", "delivery",
			"job_id", job.ID, "user_id", job.UserID, "provider", job.Provider,
			"current_status", job.Status, "external_job_id", extID,
			"document", "result_documents", "has_summary", true,
			"transcript_chars", len(result.TranscriptMarkdown), "summary_chars", len(result.SummaryMarkdown))
		if err := o.storage.SetJobDone(dctx, job.ID, result.Raw, durationSeconds, o.trialLimitMinutes); err != nil {
			slog.Error("set job done (provider summary)",
				"event", "job_transition_failed", "phase", "state_machine",
				"err", err, "job_id", job.ID, "user_id", job.UserID,
				"provider", job.Provider, "current_status", job.Status, "new_status", JobDone,
				"external_job_id", extID)
			return
		}
		slog.Info("job done",
			"event", "job_transition", "phase", "state_machine",
			"job_id", job.ID, "user_id", job.UserID, "provider", job.Provider,
			"current_status", job.Status, "new_status", JobDone, "external_job_id", extID)
		o.updateStatus(job, "Готово. Расшифровка и конспект ниже.")
		o.maybeNotifyBalanceDepleted(ctx, job)
		return
	}

	// Саммари вне скоупа (summarizer не сконфигурирован, SUMMARIZER_PROVIDER=off):
	// поток заканчивается транскриптом, джоба сразу done (SetJobDone допускает вход
	// из processing). Иначе передаём в summarizing — саммари доделает summarizeJob.
	if o.summarizer == nil {
		filename := transcriptFilename(job.CreatedAt)
		dlctx, dlcancel := context.WithTimeout(ctx, providerTimeout)
		err := o.delivery.Deliver(dlctx, job.UserID, filename, "📄 Расшифровка встречи", []byte(result.TranscriptMarkdown))
		dlcancel()
		if err != nil {
			slog.Error("deliver transcript",
				"event", "delivery_failed", "phase", "delivery",
				"err", err, "job_id", job.ID, "user_id", job.UserID,
				"provider", job.Provider, "current_status", job.Status,
				"external_job_id", extID, "document", "transcript",
				"transcript_chars", len(result.TranscriptMarkdown))
			return
		}
		slog.Info("transcript delivered",
			"event", "delivery_completed", "phase", "delivery",
			"job_id", job.ID, "user_id", job.UserID, "provider", job.Provider,
			"current_status", job.Status, "external_job_id", extID,
			"document", "transcript", "transcript_chars", len(result.TranscriptMarkdown))
		if err := o.storage.SetJobDone(dctx, job.ID, result.Raw, durationSeconds, o.trialLimitMinutes); err != nil {
			slog.Error("set job done (summary off)",
				"event", "job_transition_failed", "phase", "state_machine",
				"err", err, "job_id", job.ID, "user_id", job.UserID,
				"provider", job.Provider, "current_status", job.Status, "new_status", JobDone,
				"external_job_id", extID)
			return
		}
		slog.Info("job done",
			"event", "job_transition", "phase", "state_machine",
			"job_id", job.ID, "user_id", job.UserID, "provider", job.Provider,
			"current_status", job.Status, "new_status", JobDone, "external_job_id", extID)
		o.updateStatus(job, "Готово. Расшифровка ниже.")
		o.maybeNotifyBalanceDepleted(ctx, job)
		return
	}
	if err := o.storage.SetJobSummarizing(dctx, job.ID, result.Raw, durationSeconds); err != nil {
		slog.Error("set job summarizing",
			"event", "job_transition_failed", "phase", "state_machine",
			"err", err, "job_id", job.ID, "user_id", job.UserID,
			"provider", job.Provider, "current_status", job.Status, "new_status", JobSummarizing,
			"external_job_id", extID)
		return
	}
	slog.Info("job summarizing",
		"event", "job_transition", "phase", "state_machine",
		"job_id", job.ID, "user_id", job.UserID, "provider", job.Provider,
		"current_status", job.Status, "new_status", JobSummarizing, "external_job_id", extID)
	o.updateProgressStatus(job, "Расшифровка готова. Готовлю краткий конспект.")
}

// summarizeJob генерирует саммари из транскрипта и переводит джобу в done.
//
// Саммари кэшируется в БД (SetJobSummary) СРАЗУ после генерации, до Deliver. При сбое
// доставки джоба остаётся summarizing и ретраится — но Summarize (платный LLM-вызов) на
// ретрае НЕ повторяется: берём job.SummaryMD из кэша и только доставляем. Без кэша
// каждый транзиентный сбой Deliver пересоздавал бы саммари за деньги.
func (o *Orchestrator) summarizeJob(ctx context.Context, job Job) {
	// summarizing-джоба без ExternalJobID невозможна по построению (путь через
	// finishTranscription, которая пишет SetJobSummarizing только после успешного Fetch),
	// но проверяем инвариант явно: паника скроет причину в логах.
	if job.ExternalJobID == nil {
		slog.Error("summarizing job has nil ExternalJobID",
			"event", "job_missing_external_id", "phase", "summary",
			"job_id", job.ID, "user_id", job.UserID, "provider", job.Provider,
			"current_status", job.Status)
		return
	}

	pctx, cancel := context.WithTimeout(ctx, providerTimeout)
	result, err := o.provider.Fetch(pctx, *job.ExternalJobID)
	cancel()
	if err != nil {
		slog.Error("fetch for summarize",
			"event", "provider_fetch_failed", "phase", "summary",
			"err", err, "job_id", job.ID, "user_id", job.UserID,
			"provider", job.Provider, "current_status", job.Status,
			"external_job_id", *job.ExternalJobID)
		return // ретрай summarizing на следующем тике; транскрипт у поставщика ещё жив
	}

	slog.Info("summary transcript fetched",
		"event", "provider_fetch_completed", "phase", "summary",
		"job_id", job.ID, "user_id", job.UserID, "provider", job.Provider,
		"current_status", job.Status, "external_job_id", *job.ExternalJobID,
		"provider_duration_seconds", result.DurationSeconds,
		"transcript_chars", len(result.TranscriptMarkdown))

	// Cached-путь: саммари уже сгенерировано на прошлой попытке (сбой был на Deliver).
	// Не платим за Summarize повторно — берём кэш, но transcript всё равно Fetch-им,
	// потому что итоговый документ теперь содержит и конспект, и расшифровку.
	summary := ""
	if job.SummaryMD != nil {
		summary = *job.SummaryMD
	}

	if summary == "" {
		slog.Info("summary started",
			"event", "summary_started", "phase", "summary",
			"job_id", job.ID, "user_id", job.UserID, "provider", job.Provider,
			"current_status", job.Status, "external_job_id", *job.ExternalJobID)
		sctx, cancel := context.WithTimeout(ctx, summaryTimeout)
		summary, err = o.summarizer.Summarize(sctx, result.TranscriptMarkdown)
		cancel()
		if err != nil {
			o.finishWithoutSummary(ctx, job, fmt.Sprintf("summarize: %v", err), result.TranscriptMarkdown)
			return
		}
		slog.Info("summary completed",
			"event", "summary_completed", "phase", "summary",
			"job_id", job.ID, "user_id", job.UserID, "provider", job.Provider,
			"current_status", job.Status, "external_job_id", *job.ExternalJobID)

		// Кэшируем ДО Deliver: транзиентный сбой доставки не повлечёт повторный платный
		// Summarize. Неудача кэширования не фатальна — деградирует к старому поведению
		// (на ретрае пересоздадим), поэтому только логируем и продолжаем к доставке.
		scctx, cancel := context.WithTimeout(ctx, dbTimeout)
		if err := o.storage.SetJobSummary(scctx, job.ID, summary); err != nil {
			slog.Error("cache summary before deliver",
				"event", "summary_cache_failed", "phase", "summary",
				"err", err, "job_id", job.ID, "user_id", job.UserID,
				"provider", job.Provider, "current_status", job.Status,
				"external_job_id", *job.ExternalJobID)
		}
		cancel()
	} else {
		slog.Info("summary cache hit",
			"event", "summary_cache_hit", "phase", "summary",
			"job_id", job.ID, "user_id", job.UserID, "provider", job.Provider,
			"current_status", job.Status, "external_job_id", *job.ExternalJobID)
	}

	docs := resultDocuments(job.CreatedAt, summary, result.TranscriptMarkdown)
	dlctx, cancel := context.WithTimeout(ctx, providerTimeout)
	err = o.delivery.DeliverGroup(dlctx, job.UserID, "✅ Конспект и расшифровка встречи", docs)
	cancel()
	if err != nil {
		slog.Error("deliver result documents",
			"event", "delivery_failed", "phase", "delivery",
			"err", err, "job_id", job.ID, "user_id", job.UserID,
			"provider", job.Provider, "current_status", job.Status,
			"external_job_id", *job.ExternalJobID, "document", "result_documents",
			"has_summary", true, "transcript_chars", len(result.TranscriptMarkdown),
			"summary_chars", len(summary))
		return // ретрай ТОЛЬКО доставки: summary в кэше, Summarize не повторится
	}
	slog.Info("result documents delivered",
		"event", "delivery_completed", "phase", "delivery",
		"job_id", job.ID, "user_id", job.UserID, "provider", job.Provider,
		"current_status", job.Status, "external_job_id", *job.ExternalJobID,
		"document", "result_documents", "has_summary", true,
		"transcript_chars", len(result.TranscriptMarkdown), "summary_chars", len(summary))

	// Доставка саммари уже случилась — финальную запись статуса не отменяем по shutdown
	// (WithoutCancel). Симметрично finishTranscription. Raw=nil: на cached-пути свежего
	// Fetch нет, на первом проходе Raw тоже уже сохранён в SetJobSummarizing -> COALESCE.
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbTimeout)
	defer cancel()
	if err := o.storage.SetJobDone(dctx, job.ID, nil, 0, o.trialLimitMinutes); err != nil {
		slog.Error("set job done",
			"event", "job_transition_failed", "phase", "state_machine",
			"err", err, "job_id", job.ID, "user_id", job.UserID,
			"provider", job.Provider, "current_status", job.Status, "new_status", JobDone,
			"external_job_id", *job.ExternalJobID)
		return
	}
	slog.Info("job done",
		"event", "job_transition", "phase", "state_machine",
		"job_id", job.ID, "user_id", job.UserID, "provider", job.Provider,
		"current_status", job.Status, "new_status", JobDone, "external_job_id", *job.ExternalJobID)
	o.updateStatus(job, "Готово. Расшифровка и конспект ниже.")
	o.maybeNotifyBalanceDepleted(ctx, job)
}

func resultDocuments(createdAt time.Time, summary, transcript string) []DeliveryDocument {
	return []DeliveryDocument{
		{Filename: summaryFilename(createdAt), Content: []byte(summary)},
		{Filename: transcriptFilename(createdAt), Content: []byte(transcript)},
	}
}

// finishWithoutSummary доставляет transcript-only результат, затем переводит джобу
// в failed_summary: встреча обработана, но конспект не сформировался.
func (o *Orchestrator) finishWithoutSummary(ctx context.Context, job Job, reason, transcript string) {
	filename := transcriptFilename(job.CreatedAt)
	dlctx, cancel := context.WithTimeout(ctx, providerTimeout)
	err := o.delivery.Deliver(dlctx, job.UserID, filename, "📄 Расшифровка встречи. Конспект временно недоступен.", []byte(transcript))
	cancel()
	if err != nil {
		slog.Error("deliver transcript after summary failure",
			"event", "delivery_failed", "phase", "delivery",
			"err", err, "job_id", job.ID, "user_id", job.UserID,
			"provider", job.Provider, "current_status", job.Status,
			"external_job_id", valueOrEmpty(job.ExternalJobID), "document", "transcript",
			"has_summary", false, "transcript_chars", len(transcript))
		return
	}
	slog.Info("transcript delivered after summary failure",
		"event", "delivery_completed", "phase", "delivery",
		"job_id", job.ID, "user_id", job.UserID, "provider", job.Provider,
		"current_status", job.Status, "external_job_id", valueOrEmpty(job.ExternalJobID),
		"document", "transcript", "has_summary", false, "transcript_chars", len(transcript))

	o.failSummary(ctx, job, reason)
}

// failSummary переводит джобу в failed_summary и уведомляет юзера.
// По образцу failJob: ранний return при ошибке записи предотвращает дубль notify на каждом тике.
func (o *Orchestrator) failSummary(ctx context.Context, job Job, reason string) {
	dctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	if err := o.storage.SetJobFailedSummary(dctx, job.ID, reason, o.trialLimitMinutes); err != nil {
		slog.Error("set job failed_summary",
			"event", "job_transition_failed", "phase", "summary",
			"err", err, "job_id", job.ID, "user_id", job.UserID,
			"provider", job.Provider, "current_status", job.Status, "new_status", JobFailedSummary)
		// Без раннего return notify уходил бы юзеру заново на каждом тике
		// до успешной записи failed_summary.
		return
	}
	slog.Warn("summary failed",
		"event", "summary_failed", "phase", "summary",
		"job_id", job.ID, "user_id", job.UserID, "provider", job.Provider,
		"current_status", job.Status, "new_status", JobFailedSummary, "reason", reason)
	// Транскрипт уже доставлен — сообщаем об отдельном сбое конспекта, не встречи.
	o.updateStatus(job, "Расшифровка готова. Конспект сейчас не получился.")
	o.maybeNotifyBalanceDepleted(ctx, job)
}

// failJob переводит джобу в failed с откатом резерва квоты (§5.2) и уведомляет юзера.
func (o *Orchestrator) failJob(ctx context.Context, job Job, reason string) {
	dctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	refunded, err := o.storage.FailJobWithQuotaRefund(dctx, job.ID, reason)
	if err != nil {
		slog.Error("fail job with quota refund",
			"event", "quota_refund_failed", "phase", "quota",
			"err", err, "job_id", job.ID, "user_id", job.UserID,
			"provider", job.Provider, "current_status", job.Status, "new_status", JobFailed)
		// Без раннего return джоба осталась бы processing, и notify уходил бы
		// юзеру заново на каждом тике до успешной записи failed.
		return
	}
	slog.Info("job failed",
		"event", "job_transition", "phase", "state_machine",
		"job_id", job.ID, "user_id", job.UserID, "provider", job.Provider,
		"current_status", job.Status, "new_status", JobFailed,
		"refunded", refunded, "reason", reason)
	// Молчание хуже отказа: юзер ждёт результат вечно. Редактируем статус-сообщение
	// (фолбэк на новое, если id нет). Ошибка доставки не критична.
	o.updateStatus(job, "Не получилось обработать встречу. Минуты вернулись на баланс.")
}
