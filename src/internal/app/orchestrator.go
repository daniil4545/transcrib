package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Tick operation timeouts: one hung operation (TCP to Postgres, a slow
// provider at S6) must not silently freeze the single worker forever.
// Style as in tg.go: every external call is explicitly bounded.
const (
	dbTimeout       = 5 * time.Second
	providerTimeout = 30 * time.Second
	// summaryTimeout differs from providerTimeout: an LLM summary is not an HTTP Fetch,
	// it is a generative call with a different latency profile. It wraps the whole Summarize,
	// including the N+1 sequential map-reduce calls on a long transcript (openrouter.go).
	summaryTimeout = 180 * time.Second
)

// Orchestrator is the worker for the transcription job state machine (architecture.md §7).
// One goroutine, no internal parallelism: enough for the MVP,
// and the external provider requires polling anyway.
type Orchestrator struct {
	storage    *Storage
	provider   TranscriptionProvider
	delivery   DocumentDelivery
	summarizer Summarizer // F3 summary provider; wired in the next wave
	// notify sends a NEW message to the user (nudge, fallback). editStatus EDITS
	// the call status message by id (accepted -> bot in the meeting -> done/error):
	// one evolving message instead of a stream of new ones. Both are closures over *Bot.
	notify     func(chatID int64, text string) error
	editStatus func(chatID, messageID int64, text string) error
	tick       time.Duration // 30s in prod; shorter for local development
	// maxJobAge is the orphan/zombie-recovery threshold: a recording older than this
	// is stopped at the provider and reaches the stop-result path.
	maxJobAge time.Duration
	// startTimeout is the overall limit for the created/joining phases before recording starts.
	startTimeout time.Duration
	// submitMaxAttempts is a best-effort Submit limit within the memory of one process.
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

// Run starts the ticker and drives tickOnce until ctx is cancelled.
// The first call is immediate on start, we do not wait for the first tick: it reduces processing delay
// when the service restarts with already existing jobs.
func (o *Orchestrator) Run(ctx context.Context) {
	// First tick without waiting. context.Canceled is not logged as an error:
	// it is a normal shutdown, not a failure; ERROR in the logs would be a false signal.
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

// tickOnce is one iteration: claim jobs from the DB and advance them through the state machine.
// It returns an error only if the claim itself failed; errors of individual jobs are
// slog.Error + continue, so a failure of one does not break processing of the rest.
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

// advance moves one job to the next state machine step.
// Errors are logged, the function does not return them: the retry happens on the next tick.
func (o *Orchestrator) advance(ctx context.Context, job Job) {
	switch job.Status {
	case JobCreated:
		o.submitJob(ctx, job)
	case JobJoining, JobRecording, JobProcessing:
		// All three "bot is at the provider" phases are fed by polling: claim returns
		// joining/recording alongside processing, and without them the job would fall into
		// default-warn (Poll not called), being claimed every tick (BLOCKER B1).
		o.pollJob(ctx, job)
	case JobSummarizing:
		o.summarizeJob(ctx, job)
	default:
		// done/failed should no longer pass through claim, but if they somehow did, ignore them.
		slog.Warn("orchestrator: unexpected job status in claim",
			"event", "job_claim_unexpected_status", "phase", "state_machine",
			"job_id", job.ID, "user_id", job.UserID, "provider", job.Provider,
			"current_status", job.Status)
	}
}

// submitJob sends the bot to the meeting and moves the job to joining (the bot is connecting).
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
		return // retry on the next tick
	}
	delete(o.submitAttempts, job.ID)

	// Submit already happened (the bot went to the meeting): we do not cancel the status write
	// on shutdown (WithoutCancel), otherwise the job stays created and after a restart
	// a second bot would go to the same meeting. Symmetric to SetJobDone in finishJob.
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbTimeout)
	defer cancel()
	if err := o.storage.SetJobSubmitted(dctx, job.ID, extID); err != nil {
		if errors.Is(err, ErrJobNotCreated) {
			// The user cancelled the job while Submit was running (created->failed): the transition was not applied.
			// The bot has already been sent to the meeting: stop it, otherwise the orphaned recording
			// burns minutes. WithoutCancel: Submit is irreversible, we do not abandon the stop on shutdown.
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

	// Progress: edit the status message "accepted" -> "the bot is going to the meeting,
	// connecting" (joining phase). Honestly "connecting", NOT "recording":
	// the join signal comes from the next poll (OnCall -> recording). Best-effort, without
	// spam (we edit the existing message).
	o.updateProgressStatus(job, "Бот подключается к встрече.")
}

// updateStatus updates the call status message: edits the existing one (editStatus),
// otherwise sends a new one (notify fallback if the id was not saved). Best-effort, UX is not critical.
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

// maybeNotifyBalanceDepleted, after a billable job is finalized, sends a separate message
// if the user's minute balance has run out.
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

// pollJob requests the status from the provider and reacts to Done/Failed.
func (o *Orchestrator) pollJob(ctx context.Context, job Job) {
	// A joining/recording/processing job without ExternalJobID is impossible by construction
	// (SetJobSubmitted always writes it), but we check the invariant explicitly: a panic would hide
	// the cause in the logs.
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

	// The watchdog stops only an active recording. processing already means the bot
	// has left and the provider is preparing the result; a repeated cancel by created_at could fail
	// a long meeting right after a manual stop, without waiting for the transcript.
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
		return // retry on the next tick
	}

	switch status {
	case StatusPending:
		o.updateProgressStatus(job, "Готовлю подключение к встрече.")

	case StatusQueuedRecording:
		o.updateProgressStatus(job, "Бот в очереди на подключение. Обычно это занимает пару минут.")

	case StatusJoiningCall:
		o.updateProgressStatus(job, "Бот подключается к встрече.")

	case StatusOnCall:
		// The bot joined the meeting, recording is in progress: joining -> recording. The status message
		// is edited ONLY when moved=true (the phase really changed); a repeated tick on an
		// already-recording job gives moved=false and does not spam Edit (idempotency).
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
		// The bot left, background transcription is in progress: -> processing. This transition frees
		// the user (§5.8 stops counting the job as busy). Edit only when moved=true.
		o.moveToProcessing(ctx, job, extID, status)

	case StatusPaused:
		// Not enough minutes on the provider account (s2t only).
		// In the skeleton we wait as for Pending; timeout/alert is S6 technical debt.
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
		// Contract drift (a new status at S6) must not silently look
		// like Pending: the job would be polled forever with no trace in the logs.
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

// finishTranscription fetches the result from the provider, delivers the transcript
// to the user and moves the job to summarizing (not done: the summary is still ahead).
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
		return Result{}, false // the ordinary done flow is retried on the next tick
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
		// For s2t chunks, duration reflects the timecodes of recognized speech, not the full
		// presence of the bot. If the state machine already recorded the recording window
		// on the transition to processing, billing will use the stored duration_seconds.
		durationSeconds = 0
	}

	// A persistently invalid Raw (the s2t result/json schema is pinned only by a live
	// test) must not loop delivery: SetJobSummarizing would fail on JSONB every
	// tick, and Deliver would send to the user again. We lose the debug raw, not correctness.
	// nil is a legitimate path (s2t 204 "no speech detected" or a result/json failure),
	// not an error: json.Valid(nil) == false would give a false ERROR on a normal scenario.
	if result.Raw != nil && !json.Valid(result.Raw) {
		slog.Error("provider raw is not valid JSON, storing NULL",
			"event", "provider_raw_invalid", "phase", "fetch",
			"job_id", job.ID, "user_id", job.UserID, "provider", job.Provider,
			"current_status", job.Status, "external_job_id", extID)
		result.Raw = nil
	}

	// We save the phase after the irreversible Fetch under WithoutCancel: if shutdown
	// interrupts this section, the next tick goes to the external provider again.
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbTimeout)
	defer cancel()

	// Provider summary: mymeet computed the summary while processing the meeting. We deliver it
	// as one file group and move to done, bypassing summarizing/Summarizer. It wins
	// over the configured Summarizer: with the mymeet provider SUMMARIZER_PROVIDER=off.
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

	// Summary out of scope (summarizer not configured, SUMMARIZER_PROVIDER=off):
	// the flow ends with the transcript, the job goes straight to done (SetJobDone allows entry
	// from processing). Otherwise hand off to summarizing, where summarizeJob finishes the summary.
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

// summarizeJob generates the summary from the transcript and moves the job to done.
//
// The summary is cached in the DB (SetJobSummary) IMMEDIATELY after generation, before Deliver. If
// delivery fails the job stays summarizing and is retried, but Summarize (a paid LLM call) is
// NOT repeated on retry: we take job.SummaryMD from the cache and only deliver. Without the cache
// every transient Deliver failure would regenerate the summary for money.
func (o *Orchestrator) summarizeJob(ctx context.Context, job Job) {
	// A summarizing job without ExternalJobID is impossible by construction (the path goes through
	// finishTranscription, which writes SetJobSummarizing only after a successful Fetch),
	// but we check the invariant explicitly: a panic would hide the cause in the logs.
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
		return // retry summarizing on the next tick; the transcript is still alive at the provider
	}

	slog.Info("summary transcript fetched",
		"event", "provider_fetch_completed", "phase", "summary",
		"job_id", job.ID, "user_id", job.UserID, "provider", job.Provider,
		"current_status", job.Status, "external_job_id", *job.ExternalJobID,
		"provider_duration_seconds", result.DurationSeconds,
		"transcript_chars", len(result.TranscriptMarkdown))

	// Cached path: the summary was already generated on a previous attempt (the failure was at Deliver).
	// We do not pay for Summarize again: we take the cache, but still Fetch the transcript,
	// because the final document now contains both the summary and the transcript.
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

		// Cache BEFORE Deliver: a transient delivery failure will not trigger a repeated paid
		// Summarize. A caching failure is not fatal: it degrades to the old behavior
		// (regenerate on retry), so we only log and continue to delivery.
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
		return // retry ONLY delivery: the summary is cached, Summarize will not repeat
	}
	slog.Info("result documents delivered",
		"event", "delivery_completed", "phase", "delivery",
		"job_id", job.ID, "user_id", job.UserID, "provider", job.Provider,
		"current_status", job.Status, "external_job_id", *job.ExternalJobID,
		"document", "result_documents", "has_summary", true,
		"transcript_chars", len(result.TranscriptMarkdown), "summary_chars", len(summary))

	// Summary delivery already happened: we do not cancel the final status write on shutdown
	// (WithoutCancel). Symmetric to finishTranscription. Raw=nil: on the cached path there is no fresh
	// Fetch, and on the first pass Raw was already saved in SetJobSummarizing -> COALESCE.
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

// finishWithoutSummary delivers a transcript-only result, then moves the job
// to failed_summary: the meeting was processed, but the summary was not produced.
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

// failSummary moves the job to failed_summary and notifies the user.
// Following failJob: an early return on a write error prevents a duplicate notify on every tick.
func (o *Orchestrator) failSummary(ctx context.Context, job Job, reason string) {
	dctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	if err := o.storage.SetJobFailedSummary(dctx, job.ID, reason, o.trialLimitMinutes); err != nil {
		slog.Error("set job failed_summary",
			"event", "job_transition_failed", "phase", "summary",
			"err", err, "job_id", job.ID, "user_id", job.UserID,
			"provider", job.Provider, "current_status", job.Status, "new_status", JobFailedSummary)
		// Without the early return, notify would go to the user again on every tick
		// until failed_summary is written successfully.
		return
	}
	slog.Warn("summary failed",
		"event", "summary_failed", "phase", "summary",
		"job_id", job.ID, "user_id", job.UserID, "provider", job.Provider,
		"current_status", job.Status, "new_status", JobFailedSummary, "reason", reason)
	// The transcript is already delivered: we report a separate summary failure, not a meeting failure.
	o.updateStatus(job, "Расшифровка готова. Конспект сейчас не получился.")
	o.maybeNotifyBalanceDepleted(ctx, job)
}

// failJob moves the job to failed with a quota reservation rollback (§5.2) and notifies the user.
func (o *Orchestrator) failJob(ctx context.Context, job Job, reason string) {
	dctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	refunded, err := o.storage.FailJobWithQuotaRefund(dctx, job.ID, reason)
	if err != nil {
		slog.Error("fail job with quota refund",
			"event", "quota_refund_failed", "phase", "quota",
			"err", err, "job_id", job.ID, "user_id", job.UserID,
			"provider", job.Provider, "current_status", job.Status, "new_status", JobFailed)
		// Without the early return the job would stay processing, and notify would go
		// to the user again on every tick until failed is written successfully.
		return
	}
	slog.Info("job failed",
		"event", "job_transition", "phase", "state_machine",
		"job_id", job.ID, "user_id", job.UserID, "provider", job.Provider,
		"current_status", job.Status, "new_status", JobFailed,
		"refunded", refunded, "reason", reason)
	// Silence is worse than a refusal: the user waits for the result forever. We edit the status message
	// (fallback to a new one if there is no id). A delivery error is not critical.
	o.updateStatus(job, "Не получилось обработать встречу. Минуты вернулись на баланс.")
}
