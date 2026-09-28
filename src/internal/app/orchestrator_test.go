package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Тесты разводки advance — критический узел «переходы state machine» (docs/testing.md).
// Storage настоящий (Postgres из DATABASE_URL, см. storage_test.go), провайдер —
// MockProvider, доставка и notify — записывающие фейки.

// recordingDelivery записывает вызовы Deliver для проверок.
type recordingDelivery struct {
	calls  []deliveredDoc
	groups []deliveredGroup
}

type deliveredDoc struct {
	chatID   int64
	filename string
	caption  string
	content  string
}

type deliveredGroup struct {
	chatID  int64
	caption string
	docs    []deliveredDoc
}

func (d *recordingDelivery) Deliver(_ context.Context, chatID int64, filename, caption string, content []byte) error {
	d.calls = append(d.calls, deliveredDoc{chatID, filename, caption, string(content)})
	return nil
}

func (d *recordingDelivery) DeliverGroup(_ context.Context, chatID int64, caption string, docs []DeliveryDocument) error {
	group := deliveredGroup{chatID: chatID, caption: caption}
	for _, doc := range docs {
		group.docs = append(group.docs, deliveredDoc{
			chatID:   chatID,
			filename: doc.Filename,
			content:  string(doc.Content),
		})
	}
	d.groups = append(d.groups, group)
	return nil
}

type submitErrorProvider struct {
	MockProvider
	err         error
	submitCalls int
}

func (p *submitErrorProvider) Submit(_ context.Context, _ Job) (string, error) {
	p.submitCalls++
	return "", p.err
}

type failedPollProvider struct {
	MockProvider
	result     Result
	fetchErr   error
	fetchCalls int
}

func (p *failedPollProvider) Poll(context.Context, string) (ProviderStatus, error) {
	return StatusFailed, nil
}

func (p *failedPollProvider) Fetch(context.Context, string) (Result, error) {
	p.fetchCalls++
	return p.result, p.fetchErr
}

// testOrchestrator собирает оркестратор на фейках; вызовы notify пишутся в *notified.
func testOrchestrator(s *Storage, p TranscriptionProvider, d DocumentDelivery, notified *[]int64) *Orchestrator {
	notify := func(chatID int64, _ string) error {
		*notified = append(*notified, chatID)
		return nil
	}
	// maxJobAge большой: свежесозданные тестовые джобы не должны попадать в orphan-timeout.
	// editStatus — no-op: у тестовых джоб status_message_id=nil, updateStatus уходит в notify.
	return NewOrchestrator(s, p, d, NewMockSummarizer(), notify, noopEdit, time.Second, time.Hour, 10*time.Minute, 3, 180)
}

// noopEdit — заглушка editStatus для тестов, где статус-сообщения нет (fallback в notify).
func noopEdit(int64, int64, string) error { return nil }

func TestMaybeNotifyBalanceDepleted(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/balance-depleted", "mock", 60)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := s.SetJobSubmitted(ctx, job.ID, "ext-balance-depleted"); err != nil {
		t.Fatalf("submit job: %v", err)
	}
	if _, err := s.SetJobRecording(ctx, job.ID); err != nil {
		t.Fatalf("set recording: %v", err)
	}
	if err := s.SetJobDone(ctx, job.ID, []byte(`{"ok":true}`), 60*60, 60); err != nil {
		t.Fatalf("set done: %v", err)
	}

	var sent []string
	notify := func(chatID int64, text string) error {
		if chatID != userID {
			t.Fatalf("notify chatID = %d, ожидали %d", chatID, userID)
		}
		sent = append(sent, text)
		return nil
	}
	o := NewOrchestrator(s, &MockProvider{}, &recordingDelivery{}, nil, notify, noopEdit, time.Second, time.Hour, 10*time.Minute, 3, 60)

	o.maybeNotifyBalanceDepleted(ctx, job)

	if len(sent) != 1 || sent[0] != "Баланс закончился" {
		t.Fatalf("sent = %v, ожидали одно сообщение о нуле баланса", sent)
	}
}

// editCall фиксирует вызов editStatus (chat, message, текст) для проверок редактирования.
type editCall struct {
	chatID, msgID int64
	text          string
}

// processingJob создаёт джобу и доводит её до processing (бот вышел, фоновая
// расшифровка) с заданным external id — заготовка для веток pollJob. Проходит фазы
// joining -> recording -> processing честно через сеттеры, чтобы DB-строка реально
// была в processing (а не только in-memory): тесты на «джоба держится в processing»
// проверяют состояние строки.
func processingJob(t *testing.T, s *Storage, userID int64, extID string) Job {
	t.Helper()
	job, err := s.CreateJob(context.Background(), userID, "https://meet.google.com/adv-"+extID, "mock", 180)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := s.SetJobSubmitted(context.Background(), job.ID, extID); err != nil {
		t.Fatalf("submit job: %v", err)
	}
	if _, err := s.SetJobRecording(context.Background(), job.ID); err != nil {
		t.Fatalf("set recording: %v", err)
	}
	if _, err := s.SetJobProcessing(context.Background(), job.ID); err != nil {
		t.Fatalf("set processing: %v", err)
	}
	job.Status = JobProcessing
	job.ExternalJobID = &extID
	return job
}

// TestAdvance_CreatedSubmits — created: Submit у провайдера, переход в joining
// (бот подключается; новая семантика — processing означает «бот уже вышел»),
// external_job_id сохранён; доставки и уведомлений нет.
func TestAdvance_CreatedSubmits(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job, err := s.CreateJob(context.Background(), userID, "https://meet.google.com/adv-created", "mock", 180)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	var notified []int64
	delivery := &recordingDelivery{}
	o := testOrchestrator(s, &MockProvider{}, delivery, &notified)

	o.advance(context.Background(), job)

	status, extID, _ := jobRow(t, s, job.ID)
	if status != string(JobJoining) {
		t.Errorf("статус %q, ожидали joining", status)
	}
	want := fmt.Sprintf("mock-job-%d", job.ID)
	if extID == nil || *extID != want {
		t.Errorf("external_job_id = %v, ожидали %q", extID, want)
	}
	if len(delivery.calls) != 0 {
		t.Errorf("преждевременная доставка: %v", delivery.calls)
	}
	// Progress-статусы редактируют сохранённое status-сообщение. Если message id ещё
	// не записан, новых fallback-сообщений не шлём, чтобы не спамить пользователя.
	if len(notified) != 0 {
		t.Errorf("прогресс-пуш о submit без status_message_id: notified=%v", notified)
	}
}

func TestSubmit_PermanentHTTPErrorFailsAndRefunds(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job, err := s.CreateJob(context.Background(), userID, "https://meet.google.com/submit-403", "mock", 180)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	var notified []int64
	provider := &submitErrorProvider{err: ProviderHTTPError{
		Provider:   "mymeet",
		Op:         "submit",
		StatusCode: 403,
		Body:       `{"message":"API key is not available on the free plan"}`,
	}}
	o := testOrchestrator(s, provider, &recordingDelivery{}, &notified)

	o.advance(context.Background(), job)

	if got := jobStatus(t, s, job.ID); got != string(JobFailed) {
		t.Errorf("status = %q, ожидали %q", got, JobFailed)
	}
	if got := trialUsed(t, s, userID); got != 0 {
		t.Errorf("trial_meetings_used = %d, ожидали 0 (refund on submit failure)", got)
	}
	if provider.submitCalls != 1 {
		t.Errorf("submitCalls = %d, ожидали 1", provider.submitCalls)
	}
	if got := o.submitAttempts[job.ID]; got != 0 {
		t.Errorf("submitAttempts[%d] = %d, ожидали cleanup", job.ID, got)
	}
	if len(notified) != 1 || notified[0] != userID {
		t.Errorf("fail notification = %v, ожидали один для %d", notified, userID)
	}
}

func TestSubmit_RetryableErrorFailsOnThirdAttempt(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job, err := s.CreateJob(context.Background(), userID, "https://meet.google.com/submit-retry", "mock", 180)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	var notified []int64
	provider := &submitErrorProvider{err: errors.New("temporary network error")}
	o := testOrchestrator(s, provider, &recordingDelivery{}, &notified)

	o.advance(context.Background(), job)
	if got := jobStatus(t, s, job.ID); got != string(JobCreated) {
		t.Fatalf("after first attempt status = %q, ожидали %q", got, JobCreated)
	}
	o.advance(context.Background(), job)
	if got := jobStatus(t, s, job.ID); got != string(JobCreated) {
		t.Fatalf("after second attempt status = %q, ожидали %q", got, JobCreated)
	}
	o.advance(context.Background(), job)
	if got := jobStatus(t, s, job.ID); got != string(JobFailed) {
		t.Errorf("after third attempt status = %q, ожидали %q", got, JobFailed)
	}
	if got := trialUsed(t, s, userID); got != 0 {
		t.Errorf("trial_meetings_used = %d, ожидали 0 (refund after attempts)", got)
	}
	if provider.submitCalls != 3 {
		t.Errorf("submitCalls = %d, ожидали 3", provider.submitCalls)
	}
	if got := o.submitAttempts[job.ID]; got != 0 {
		t.Errorf("submitAttempts[%d] = %d, ожидали cleanup", job.ID, got)
	}
	if len(notified) != 1 || notified[0] != userID {
		t.Errorf("fail notification = %v, ожидали один для %d", notified, userID)
	}
}

func TestSubmit_StartTimeoutFailsWithoutProviderCall(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job, err := s.CreateJob(context.Background(), userID, "https://meet.google.com/submit-timeout", "mock", 180)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	job.CreatedAt = time.Now().Add(-11 * time.Minute)

	var notified []int64
	provider := &submitErrorProvider{err: errors.New("should not be called")}
	o := testOrchestrator(s, provider, &recordingDelivery{}, &notified)

	o.advance(context.Background(), job)

	if got := jobStatus(t, s, job.ID); got != string(JobFailed) {
		t.Errorf("status = %q, ожидали %q", got, JobFailed)
	}
	if provider.submitCalls != 0 {
		t.Errorf("submitCalls = %d, ожидали 0", provider.submitCalls)
	}
	if got := trialUsed(t, s, userID); got != 0 {
		t.Errorf("trial_meetings_used = %d, ожидали 0 (refund on created timeout)", got)
	}
}

func TestPoll_JoiningStartTimeoutCancelsAndRefunds(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job, err := s.CreateJob(context.Background(), userID, "https://meet.google.com/join-timeout", "mock", 180)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	extID := "ext-join-timeout"
	if err := s.SetJobSubmitted(context.Background(), job.ID, extID); err != nil {
		t.Fatalf("SetJobSubmitted: %v", err)
	}
	job.Status = JobJoining
	job.ExternalJobID = &extID
	job.CreatedAt = time.Now().Add(-11 * time.Minute)

	var notified []int64
	provider := &MockProvider{}
	o := testOrchestrator(s, provider, &recordingDelivery{}, &notified)

	o.advance(context.Background(), job)

	if got := jobStatus(t, s, job.ID); got != string(JobFailed) {
		t.Errorf("status = %q, ожидали %q", got, JobFailed)
	}
	if len(provider.Cancelled) != 1 || provider.Cancelled[0] != extID {
		t.Errorf("Cancelled = %v, ожидали [%s]", provider.Cancelled, extID)
	}
	if got := trialUsed(t, s, userID); got != 0 {
		t.Errorf("trial_meetings_used = %d, ожидали 0 (refund on joining timeout)", got)
	}
}

// TestSubmit_LostToConcurrentCancel — гонка submit↔cancel: юзер отменил created-джобу,
// пока шёл Submit. SetJobSubmitted (guard status='created') не воскрешает failed→processing;
// submitJob останавливает осиротевшего бота у поставщика. Без guard джоба ожила бы с
// возвращённой квотой и активным ботом (денежный + state-machine баг).
func TestSubmit_LostToConcurrentCancel(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job, err := s.CreateJob(context.Background(), userID, "https://meet.google.com/submit-race", "mock", 180)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Юзер отменяет, пока джоба ещё created (бот не отправлен) — рефанд триала.
	if _, _, _, err := s.CancelJob(context.Background(), userID, job.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	var notified []int64
	notify := func(chatID int64, _ string) error { notified = append(notified, chatID); return nil }
	mp := &MockProvider{}
	o := NewOrchestrator(s, mp, &recordingDelivery{}, NewMockSummarizer(), notify, noopEdit, time.Second, time.Hour, 10*time.Minute, 3, 180)

	// Оркестратор держит in-memory created (claim случился до отмены) и продолжает submit.
	o.advance(context.Background(), job)

	if st, _, _ := jobRow(t, s, job.ID); st != string(JobFailed) {
		t.Errorf("статус=%s, ожидали failed (отменённую не воскрешать)", st)
	}
	want := fmt.Sprintf("mock-job-%d", job.ID)
	if len(mp.Cancelled) != 1 || mp.Cancelled[0] != want {
		t.Errorf("Cancel у поставщика=%v, ожидали [%s] (остановить осиротевшего бота)", mp.Cancelled, want)
	}
	if used := trialUsed(t, s, userID); used != 0 {
		t.Errorf("trial_used=%d, ожидали 0 (квота не пере-резервируется на проигранном submit)", used)
	}
	if len(notified) != 0 {
		t.Errorf("прогресс-пуш на проигранном submit: %v", notified)
	}
}

// TestAdvance_TerminalStatusIgnored — инвариант state machine: терминальный статус
// (done/failed/failed_summary исключены частичным индексом, но если такой Job всё же
// прошёл claim) попадает в ветку default — advance ничего не делает: статус не
// двигается, нет Deliver, нет notify. Защищает от случайного повторного Fetch/Deliver
// уже завершённой джобы. done — представитель класса терминальных статусов.
func TestAdvance_TerminalStatusIgnored(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	// Реальная джоба в БД, доведённая до done. SetJobDone требует pre-статус
	// processing|summarizing (§5.7), поэтому джобу нужно провести
	// submitted -> summarizing -> done, иначе тест проверял бы не то.
	job, err := s.CreateJob(context.Background(), userID, "https://meet.google.com/adv-terminal", "mock", 180)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := s.SetJobSubmitted(context.Background(), job.ID, "ext-adv-terminal"); err != nil {
		t.Fatalf("submit job: %v", err)
	}
	if err := s.SetJobSummarizing(context.Background(), job.ID, []byte(`{"transcript":"x"}`), 3600); err != nil {
		t.Fatalf("set summarizing: %v", err)
	}
	if err := s.SetJobDone(context.Background(), job.ID, []byte(`{"status":"done"}`), 3600, 180); err != nil {
		t.Fatalf("set done: %v", err)
	}
	job.Status = JobDone

	var notified []int64
	delivery := &recordingDelivery{}
	o := testOrchestrator(s, &MockProvider{}, delivery, &notified)

	o.advance(context.Background(), job)

	status, _, _ := jobRow(t, s, job.ID)
	if status != string(JobDone) {
		t.Errorf("статус %q, ожидали done (терминальный не двигается)", status)
	}
	if len(delivery.calls) != 0 || len(notified) != 0 {
		t.Errorf("delivery/notify на терминальном статусе: %v / %v", delivery.calls, notified)
	}
}

// TestAdvance_PendingKeeps — Pending: статус не двигается, ничего не отправляется.
func TestAdvance_PendingKeeps(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job := processingJob(t, s, userID, "ext-pending")

	var notified []int64
	delivery := &recordingDelivery{}
	o := testOrchestrator(s, &MockProvider{DoneAfterPolls: 5}, delivery, &notified)

	o.advance(context.Background(), job)

	status, _, _ := jobRow(t, s, job.ID)
	if status != string(JobProcessing) {
		t.Errorf("статус %q, ожидали processing", status)
	}
	if len(delivery.calls) != 0 || len(notified) != 0 {
		t.Errorf("delivery/notify на Pending: %v / %v", delivery.calls, notified)
	}
}

// TestAdvance_FailedNotifies — Failed: запись failed + ровно одно уведомление
// юзеру, доставки нет.
func TestAdvance_FailedNotifies(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job := processingJob(t, s, userID, "ext-failed")

	var notified []int64
	delivery := &recordingDelivery{}
	o := testOrchestrator(s, &MockProvider{FailOnPoll: true}, delivery, &notified)

	o.advance(context.Background(), job)

	status, _, _ := jobRow(t, s, job.ID)
	if status != string(JobFailed) {
		t.Errorf("статус %q, ожидали failed", status)
	}
	if len(notified) != 1 || notified[0] != userID {
		t.Errorf("notify = %v, ожидали один вызов для %d", notified, userID)
	}
	if len(delivery.calls) != 0 {
		t.Errorf("доставка на failed: %v", delivery.calls)
	}
}

func TestAdvance_FailedWithoutStopMarkerDoesNotFetch(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job := processingJob(t, s, userID, "ext-failed-no-marker")
	provider := &failedPollProvider{}

	var notified []int64
	o := testOrchestrator(s, provider, &recordingDelivery{}, &notified)

	o.advance(context.Background(), job)

	if provider.fetchCalls != 0 {
		t.Fatalf("Fetch вызван %d раз без stop marker", provider.fetchCalls)
	}
	if status, _, _ := jobRow(t, s, job.ID); status != string(JobFailed) {
		t.Fatalf("status = %q, ожидали failed", status)
	}
}

func TestAdvance_FailedWithStopMarkerFetchesTranscriptAndBills(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job := processingJob(t, s, userID, "ext-failed-stop-marker")
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE jobs SET last_error = $2 WHERE id = $1`, job.ID, jobStopManual); err != nil {
		t.Fatalf("set stop marker: %v", err)
	}
	job = reloadJob(t, s, job.ID)
	provider := &failedPollProvider{result: Result{
		TranscriptMarkdown: "# Транскрипт встречи\n\n**A:** stop result",
		Raw:                []byte(`{"ok":true}`),
		DurationSeconds:    60,
	}}

	var notified []int64
	delivery := &recordingDelivery{}
	o := testOrchestrator(s, provider, delivery, &notified)

	o.advance(context.Background(), job)

	if provider.fetchCalls != 1 {
		t.Fatalf("Fetch вызван %d раз, ожидали 1", provider.fetchCalls)
	}
	if status, _, _ := jobRow(t, s, job.ID); status != string(JobSummarizing) {
		t.Fatalf("status = %q, ожидали summarizing после transcript", status)
	}
	if len(delivery.calls) != 0 {
		t.Fatalf("delivery до summary не ожидалась: %v", delivery.calls)
	}
	if used := trialUsed(t, s, userID); used != 1 {
		t.Fatalf("trial_used = %d, ожидали сохранённый резерв до terminal billing", used)
	}

	job = reloadJob(t, s, job.ID)
	o.advance(context.Background(), job)

	if status, _, _ := jobRow(t, s, job.ID); status != string(JobDone) {
		t.Fatalf("status = %q, ожидали done после summary", status)
	}
	billed, duration := jobBilling(t, s, job.ID)
	if billed != 1 {
		t.Fatalf("billed_minutes = %d, ожидали 1 для stopped transcript duration=60s", billed)
	}
	if duration == nil || *duration != 60 {
		t.Fatalf("duration_seconds = %v, ожидали 60", duration)
	}
	if used := trialUsed(t, s, userID); used != 1 {
		t.Fatalf("trial_used = %d, ожидали billable result без refund", used)
	}
}

func TestAdvance_FailedWithStopMarkerRecordingClosesWindowBeforeFetch(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job, err := s.CreateJob(context.Background(), userID, "https://meet.google.com/stop-race", "mock", 180)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.SetJobSubmitted(context.Background(), job.ID, "ext-stop-race"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := s.SetJobRecording(context.Background(), job.ID); err != nil {
		t.Fatalf("recording: %v", err)
	}
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE jobs
		 SET last_error = $2, recording_started_at = now() - interval '5 minutes'
		 WHERE id = $1`,
		job.ID, jobStopManual); err != nil {
		t.Fatalf("set stop marker: %v", err)
	}
	job = reloadJob(t, s, job.ID)
	provider := &failedPollProvider{result: Result{
		TranscriptMarkdown: "# Транскрипт встречи\n\n**A:** stop result",
		Raw:                []byte(`{"ok":true}`),
		DurationSeconds:    60,
	}}

	var notified []int64
	o := testOrchestrator(s, provider, &recordingDelivery{}, &notified)

	o.advance(context.Background(), job)

	if provider.fetchCalls != 0 {
		t.Fatalf("Fetch вызван %d раз до фикса processing/duration", provider.fetchCalls)
	}
	reloaded := reloadJob(t, s, job.ID)
	if reloaded.Status != JobProcessing {
		t.Fatalf("status = %q, ожидали processing", reloaded.Status)
	}
	if reloaded.DurationSeconds == nil || *reloaded.DurationSeconds < 5*60 {
		t.Fatalf("duration_seconds = %v, ожидали зафиксированное recording window", reloaded.DurationSeconds)
	}

	o.advance(context.Background(), reloaded)

	if provider.fetchCalls != 1 {
		t.Fatalf("Fetch вызван %d раз, ожидали 1 после перехода в processing", provider.fetchCalls)
	}
	if status, _, _ := jobRow(t, s, job.ID); status != string(JobSummarizing) {
		t.Fatalf("status = %q, ожидали summarizing после fetch", status)
	}
}

func TestAdvance_FailedWithStopMarkerEmptyTranscriptRefunds(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job := processingJob(t, s, userID, "ext-failed-stop-empty")
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE jobs SET last_error = $2 WHERE id = $1`, job.ID, jobStopManual); err != nil {
		t.Fatalf("set stop marker: %v", err)
	}
	job = reloadJob(t, s, job.ID)
	provider := &failedPollProvider{fetchErr: ErrEmptyTranscript}

	var notified []int64
	delivery := &recordingDelivery{}
	o := testOrchestrator(s, provider, delivery, &notified)

	o.advance(context.Background(), job)

	if provider.fetchCalls != 1 {
		t.Fatalf("Fetch вызван %d раз, ожидали 1", provider.fetchCalls)
	}
	if status, _, _ := jobRow(t, s, job.ID); status != string(JobFailed) {
		t.Fatalf("status = %q, ожидали failed", status)
	}
	if used := trialUsed(t, s, userID); used != 0 {
		t.Fatalf("trial_used = %d, ожидали refund", used)
	}
	if len(delivery.calls) != 0 || len(delivery.groups) != 0 {
		t.Fatalf("delivery при empty transcript: calls=%v groups=%v", delivery.calls, delivery.groups)
	}
}

// TestAdvance_FailedDBErrorSkipsNotify — если запись failed не прошла, notify
// не уходит: иначе юзер получал бы дубль «не получилось» на каждом тике,
// пока UPDATE не успеет.
func TestAdvance_FailedDBErrorSkipsNotify(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job := processingJob(t, s, userID, "ext-dberr")

	var notified []int64
	o := testOrchestrator(s, &MockProvider{FailOnPoll: true}, &recordingDelivery{}, &notified)

	// Отменённый ctx валит FailJobWithQuotaRefund (mock на ctx не смотрит и честно отдаёт Failed).
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	o.advance(ctx, job)

	status, _, _ := jobRow(t, s, job.ID)
	if status != string(JobProcessing) {
		t.Errorf("статус %q, ожидали processing (запись failed не прошла)", status)
	}
	if len(notified) != 0 {
		t.Errorf("notify при упавшей записи failed: %v", notified)
	}
}

// TestPollJob_StaleProcessingDoesNotTimeout — processing уже значит, что бот вышел
// и provider готовит результат. created_at больше не должен фейлить такую джобу:
// иначе ручной stop длинной встречи около лимита сразу терял бы transcript.
func TestPollJob_StaleProcessingDoesNotTimeout(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job := processingJob(t, s, userID, "ext-stale")

	readUsed := func() int {
		var u int
		if err := s.pool.QueryRow(context.Background(),
			`SELECT trial_meetings_used FROM users WHERE telegram_id = $1`, userID).Scan(&u); err != nil {
			t.Fatalf("read trial_meetings_used: %v", err)
		}
		return u
	}
	usedReserved := readUsed() // после CreateJob резерв триала списан

	// Состариваем джобу дальше порога maxJobAge.
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE jobs SET created_at = now() - interval '10 hours' WHERE id = $1`, job.ID); err != nil {
		t.Fatalf("backdate job: %v", err)
	}
	job.CreatedAt = time.Now().Add(-10 * time.Hour)

	var notified []int64
	notify := func(chatID int64, _ string) error { notified = append(notified, chatID); return nil }
	mp := &MockProvider{DoneAfterPolls: 100}
	o := NewOrchestrator(s, mp, &recordingDelivery{}, NewMockSummarizer(), notify, noopEdit, time.Second, time.Hour, 10*time.Minute, 3, 180)

	o.advance(context.Background(), job)

	status, _, _ := jobRow(t, s, job.ID)
	if status != string(JobProcessing) {
		t.Errorf("статус %q, ожидали processing (stale processing не timeout)", status)
	}
	if used := readUsed(); used != usedReserved {
		t.Errorf("trial_meetings_used = %d, ожидали %d (без рефанда processing)", used, usedReserved)
	}
	if len(mp.Cancelled) != 0 {
		t.Errorf("Cancel у поставщика = %v, ожидали no-op для processing", mp.Cancelled)
	}
	if len(notified) != 0 {
		t.Errorf("notify = %v, ожидали no-op", notified)
	}
}

func TestPollJob_StaleRecordingStopsAndWaitsForResult(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job, err := s.CreateJob(context.Background(), userID, "https://meet.google.com/stale-recording", "mock", 180)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.SetJobSubmitted(context.Background(), job.ID, "ext-stale-recording"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := s.SetJobRecording(context.Background(), job.ID); err != nil {
		t.Fatalf("recording: %v", err)
	}
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE jobs SET created_at = now() - interval '10 hours' WHERE id = $1`, job.ID); err != nil {
		t.Fatalf("backdate job: %v", err)
	}
	job = reloadJob(t, s, job.ID)

	var notified []int64
	mp := &MockProvider{DoneAfterPolls: 100}
	o := NewOrchestrator(s, mp, &recordingDelivery{}, NewMockSummarizer(), func(chatID int64, _ string) error {
		notified = append(notified, chatID)
		return nil
	}, noopEdit, time.Second, time.Hour, 10*time.Minute, 3, 180)

	o.advance(context.Background(), job)

	status, _, _ := jobRow(t, s, job.ID)
	if status != string(JobProcessing) {
		t.Fatalf("статус %q, ожидали processing после watchdog stop", status)
	}
	reloaded := reloadJob(t, s, job.ID)
	if reloaded.LastError == nil || *reloaded.LastError != jobStopWatchdog {
		t.Fatalf("last_error = %v, ожидали %q", reloaded.LastError, jobStopWatchdog)
	}
	if len(mp.Cancelled) != 1 || mp.Cancelled[0] != "ext-stale-recording" {
		t.Fatalf("Cancel у поставщика = %v, ожидали [ext-stale-recording]", mp.Cancelled)
	}
	if len(notified) != 0 {
		t.Fatalf("notify = %v, ожидали no-op progress без status_message_id", notified)
	}
}

// TestPollJob_FreshJobNotStale — негативная граница orphan-timeout (B2): свежая
// processing-джоба (моложе maxJobAge) НЕ фейлится — проходит guard и идёт в Poll.
// Близнец TestPollJob_StaleTimeout: вместе закрывают обе стороны порога time.Since>maxJobAge
// (зеркало против инверсии знака сравнения). Poll вернёт Pending (DoneAfterPolls велик),
// поэтому при исправном guard джоба остаётся processing, Cancel НЕ вызван, рефанда/notify нет.
func TestPollJob_FreshJobNotStale(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job := processingJob(t, s, userID, "ext-fresh") // created_at = now() — свежая

	readUsed := func() int {
		var u int
		if err := s.pool.QueryRow(context.Background(),
			`SELECT trial_meetings_used FROM users WHERE telegram_id = $1`, userID).Scan(&u); err != nil {
			t.Fatalf("read trial_meetings_used: %v", err)
		}
		return u
	}
	usedReserved := readUsed() // резерв триала списан при CreateJob

	var notified []int64
	notify := func(chatID int64, _ string) error { notified = append(notified, chatID); return nil }
	mp := &MockProvider{DoneAfterPolls: 100} // Poll вернёт Pending — джоба не должна сдвинуться
	o := NewOrchestrator(s, mp, &recordingDelivery{}, NewMockSummarizer(), notify, noopEdit, time.Second, time.Hour, 10*time.Minute, 3, 180)

	o.advance(context.Background(), job)

	status, _, _ := jobRow(t, s, job.ID)
	if status != string(JobProcessing) {
		t.Errorf("статус %q, ожидали processing (свежая джоба не orphan)", status)
	}
	if len(mp.Cancelled) != 0 {
		t.Errorf("Cancel у поставщика на свежей джобе: %v", mp.Cancelled)
	}
	if used := readUsed(); used != usedReserved {
		t.Errorf("trial_meetings_used = %d, ожидали %d (рефанд свежей джобы)", used, usedReserved)
	}
	if len(notified) != 0 {
		t.Errorf("notify на свежей processing-джобе: %v", notified)
	}
}

// summarisingJob создаёт джобу и переводит её в summarizing с заданным external id —
// заготовка для веток summarizeJob.
func summarisingJob(t *testing.T, s *Storage, userID int64, extID string) Job {
	t.Helper()
	job, err := s.CreateJob(context.Background(), userID, "https://meet.google.com/adv-"+extID, "mock", 180)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := s.SetJobSubmitted(context.Background(), job.ID, extID); err != nil {
		t.Fatalf("submit job: %v", err)
	}
	if err := s.SetJobSummarizing(context.Background(), job.ID, []byte(`{"ok":true}`), 3600); err != nil {
		t.Fatalf("set summarizing: %v", err)
	}
	job.Status = JobSummarizing
	job.ExternalJobID = &extID
	return job
}

// TestFinishTranscription_WaitsForSummaryDelivery — processing + Done при включенном
// Summarizer: Fetch сохраняет raw/duration и переводит в summarizing, но НЕ доставляет
// транскрипт заранее. Группу файлов отправит summarizeJob после summary.
func TestFinishTranscription_WaitsForSummaryDelivery(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job := processingJob(t, s, userID, "ext-finish-transcription")

	var notified []int64
	delivery := &recordingDelivery{}
	o := testOrchestrator(s, &MockProvider{}, delivery, &notified)

	o.advance(context.Background(), job)

	status, _, raw := jobRow(t, s, job.ID)
	if status != string(JobSummarizing) {
		t.Errorf("статус %q, ожидали summarizing", status)
	}
	if !json.Valid(raw) {
		t.Errorf("raw_provider_response не валидный JSON: %s", raw)
	}
	if len(delivery.calls) != 0 {
		t.Fatalf("преждевременная доставка до summary: %v", delivery.calls)
	}
	if len(notified) != 0 {
		t.Errorf("notify на finishTranscription: %v", notified)
	}
}

// providerSummaryText — фиксированное саммари ОТ фейка провайдера. Тест сверяет, что
// в чат уходит именно оно (а не вывод Summarizer) — доказывает приоритет provider-summary.
const providerSummaryText = "# Саммари встречи\n\nКраткое содержание."

// providerWithSummary — минимальный фейк провайдера, чей Fetch возвращает непустой
// SummaryMarkdown (тест provider-summary ветки в finishTranscription).
type providerWithSummary struct{ MockProvider }

func (p *providerWithSummary) Fetch(ctx context.Context, externalJobID string) (Result, error) {
	r, err := p.MockProvider.Fetch(ctx, externalJobID)
	if err != nil {
		return r, err
	}
	r.SummaryMarkdown = providerSummaryText
	return r, nil
}

type emptyFetchProvider struct{ MockProvider }

func (p *emptyFetchProvider) Fetch(_ context.Context, _ string) (Result, error) {
	return Result{}, ErrEmptyTranscript
}

type durationFetchProvider struct {
	MockProvider
	durationSeconds int
}

func (p *durationFetchProvider) Fetch(ctx context.Context, externalJobID string) (Result, error) {
	r, err := p.MockProvider.Fetch(ctx, externalJobID)
	if err != nil {
		return r, err
	}
	r.DurationSeconds = p.durationSeconds
	return r, nil
}

// TestFinishTranscription_EmptyTranscriptRefunds — s2t done/code=204 или пустой txt:
// встреча не считается успешной, файлы не отправляются, summary не запускается,
// резерв триала откатывается, trial_done не растёт.
func TestFinishTranscription_EmptyTranscriptRefunds(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job, err := s.CreateJob(context.Background(), userID, "https://meet.google.com/empty-result", "speech2text", 180)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := s.SetJobSubmitted(context.Background(), job.ID, "ext-empty-result"); err != nil {
		t.Fatalf("submit job: %v", err)
	}
	if _, err := s.SetJobRecording(context.Background(), job.ID); err != nil {
		t.Fatalf("set recording: %v", err)
	}
	job.Status = JobRecording
	extID := "ext-empty-result"
	job.ExternalJobID = &extID

	if used := trialUsed(t, s, userID); used != 1 {
		t.Fatalf("trial_meetings_used после резерва = %d, ожидали 1", used)
	}

	var notified []int64
	delivery := &recordingDelivery{}
	o := testOrchestrator(s, &emptyFetchProvider{}, delivery, &notified)

	o.advance(context.Background(), job)

	status, _, _ := jobRow(t, s, job.ID)
	if status != string(JobFailed) {
		t.Errorf("статус %q, ожидали failed", status)
	}
	if used := trialUsed(t, s, userID); used != 0 {
		t.Errorf("trial_meetings_used = %d, ожидали 0 (рефанд пустой встречи)", used)
	}
	if done := trialDone(t, s, userID); done != 0 {
		t.Errorf("trial_meetings_done = %d, ожидали 0 (пустая встреча не засчитана)", done)
	}
	if len(delivery.calls) != 0 {
		t.Errorf("доставка при пустом транскрипте: %v", delivery.calls)
	}
	if len(notified) != 1 || notified[0] != userID {
		t.Errorf("notify = %v, ожидали только terminal failed fallback для %d", notified, userID)
	}
}

// TestFinishTranscription_ProviderSummary_Done — provider-summary ветка:
// processing + Done c непустым SummaryMarkdown -> одна группа из summary+transcript,
// статус done (минуя summarizing), raw валиден.
func TestFinishTranscription_ProviderSummary_Done(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job := processingJob(t, s, userID, "ext-provider-summary")

	var notified []int64
	delivery := &recordingDelivery{}
	notify := func(chatID int64, _ string) error { notified = append(notified, chatID); return nil }
	// Summarizer СКОНФИГУРИРОВАН (не nil): доказываем, что provider-summary выигрывает —
	// ветка стоит перед summarizer==nil, конспект берётся от провайдера, а не от Summarizer.
	o := NewOrchestrator(s, &providerWithSummary{}, delivery, NewMockSummarizer(), notify, noopEdit, time.Second, time.Hour, 10*time.Minute, 3, 180)

	o.advance(context.Background(), job)

	status, _, raw := jobRow(t, s, job.ID)
	if status != string(JobDone) {
		t.Errorf("статус %q, ожидали done (provider-summary минует summarizing)", status)
	}
	if !json.Valid(raw) {
		t.Errorf("raw_provider_response не валидный JSON: %s", raw)
	}
	if len(delivery.calls) != 0 {
		t.Fatalf("ожидали group-доставку без одиночных файлов, получили %d", len(delivery.calls))
	}
	if len(delivery.groups) != 1 {
		t.Fatalf("ожидали 1 группу файлов, получили %d", len(delivery.groups))
	}
	group := delivery.groups[0]
	if group.caption != "✅ Конспект и расшифровка встречи" {
		t.Errorf("caption %q", group.caption)
	}
	if len(group.docs) != 2 {
		t.Fatalf("ожидали 2 файла в группе, получили %d", len(group.docs))
	}
	if want := summaryFilename(job.CreatedAt); group.docs[0].filename != want {
		t.Errorf("summary файл %q, ожидали %q", group.docs[0].filename, want)
	}
	if want := transcriptFilename(job.CreatedAt); group.docs[1].filename != want {
		t.Errorf("transcript файл %q, ожидали %q", group.docs[1].filename, want)
	}
	if !strings.Contains(group.docs[0].content, providerSummaryText) {
		t.Errorf("summary файл не содержит provider-summary: %q", group.docs[0].content)
	}
	if !strings.Contains(group.docs[1].content, "# Транскрипт встречи") {
		t.Errorf("transcript файл не содержит транскрипт: %q", group.docs[1].content)
	}
	reloaded := reloadJob(t, s, job.ID)
	if reloaded.SummaryMD == nil || *reloaded.SummaryMD != providerSummaryText {
		t.Errorf("summary_md = %v, ожидали кэш provider-summary", reloaded.SummaryMD)
	}
	// status_message_id у джобы нет → статус «готово» уходит через notify-фолбэк.
	if len(notified) != 1 || notified[0] != userID {
		t.Errorf("notify (статус «готово») = %v, ожидали один для %d", notified, userID)
	}
}

// TestFinishTranscription_SummaryOff_Done — саммари выключено (summarizer == nil,
// SUMMARIZER_PROVIDER=off): processing + Done -> транскрипт доставлен, статус сразу
// done (минуя summarizing), notify нет. Покрывает демо-поток без F3.
func TestFinishTranscription_SummaryOff_Done(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job := processingJob(t, s, userID, "ext-summary-off")

	var notified []int64
	delivery := &recordingDelivery{}
	notify := func(chatID int64, _ string) error { notified = append(notified, chatID); return nil }
	// summarizer == nil — саммари выключено.
	o := NewOrchestrator(s, &MockProvider{}, delivery, nil, notify, noopEdit, time.Second, time.Hour, 10*time.Minute, 3, 180)

	o.advance(context.Background(), job)

	status, _, raw := jobRow(t, s, job.ID)
	if status != string(JobDone) {
		t.Errorf("статус %q, ожидали done (саммари выключено)", status)
	}
	if !json.Valid(raw) {
		t.Errorf("raw_provider_response не валидный JSON: %s", raw)
	}
	if len(delivery.calls) != 1 {
		t.Fatalf("ожидали 1 доставку (транскрипт), получили %d", len(delivery.calls))
	}
	if want := transcriptFilename(job.CreatedAt); delivery.calls[0].filename != want {
		t.Errorf("имя файла %q, ожидали %q", delivery.calls[0].filename, want)
	}
	// status_message_id у джобы нет → статус «готово» уходит через notify-фолбэк (1 сообщение).
	if len(notified) != 1 || notified[0] != userID {
		t.Errorf("notify (статус «готово» fallback) = %v, ожидали один для %d", notified, userID)
	}
}

// TestUpdateStatus_EditsExistingMessage — при наличии status_message_id оркестратор
// РЕДАКТИРУЕТ статус-сообщение (editStatus), а не шлёт новое (notify). UX «без спама».
func TestUpdateStatus_EditsExistingMessage(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job := processingJob(t, s, userID, "ext-edit")
	if err := s.SetJobStatusMessage(context.Background(), job.ID, 555); err != nil {
		t.Fatalf("set status message: %v", err)
	}
	mid := int64(555)
	job.StatusMessageID = &mid

	var notified []int64
	var edits []editCall
	notify := func(chatID int64, _ string) error { notified = append(notified, chatID); return nil }
	edit := func(chatID, msgID int64, text string) error {
		edits = append(edits, editCall{chatID, msgID, text})
		return nil
	}
	// summarizer nil → summary-off: pollJob Done → finishTranscription → done + статус «готово».
	o := NewOrchestrator(s, &MockProvider{}, &recordingDelivery{}, nil, notify, edit, time.Second, time.Hour, 10*time.Minute, 3, 180)

	o.advance(context.Background(), job)

	if len(edits) == 0 {
		t.Fatal("editStatus не вызван при наличии status_message_id")
	}
	last := edits[len(edits)-1]
	if last.chatID != userID || last.msgID != 555 {
		t.Errorf("edit в (%d,%d), ожидали (%d,555)", last.chatID, last.msgID, userID)
	}
	if len(notified) != 0 {
		t.Errorf("notify при наличии статус-сообщения (должен быть edit): %v", notified)
	}
}

// TestUpdateStatus_EditFailureFallsBackToNotify — если Telegram не дал отредактировать
// старое сообщение, финальный статус всё равно должен появиться новым сообщением.
func TestUpdateStatus_EditFailureFallsBackToNotify(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job := processingJob(t, s, userID, "ext-edit-fail")
	if err := s.SetJobStatusMessage(context.Background(), job.ID, 556); err != nil {
		t.Fatalf("set status message: %v", err)
	}
	mid := int64(556)
	job.StatusMessageID = &mid

	var notified []int64
	notify := func(chatID int64, _ string) error { notified = append(notified, chatID); return nil }
	edit := func(int64, int64, string) error { return errors.New("telegram edit failed") }
	o := NewOrchestrator(s, &MockProvider{}, &recordingDelivery{}, nil, notify, edit, time.Second, time.Hour, 10*time.Minute, 3, 180)

	o.advance(context.Background(), job)

	if len(notified) != 1 || notified[0] != userID {
		t.Errorf("notify fallback = %v, ожидали один вызов для %d", notified, userID)
	}
}

// TestSummarizeJob_EditsStatusOnDone — summarizing -> done должен править то же
// статус-сообщение, иначе у пользователя остаётся «бот на встрече» после файлов.
func TestSummarizeJob_EditsStatusOnDone(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job := summarisingJob(t, s, userID, "ext-summarize-edit")
	if err := s.SetJobStatusMessage(context.Background(), job.ID, 557); err != nil {
		t.Fatalf("set status message: %v", err)
	}
	mid := int64(557)
	job.StatusMessageID = &mid

	var notified []int64
	var edits []editCall
	notify := func(chatID int64, _ string) error { notified = append(notified, chatID); return nil }
	edit := func(chatID, msgID int64, text string) error {
		edits = append(edits, editCall{chatID, msgID, text})
		return nil
	}
	o := NewOrchestrator(s, &MockProvider{}, &recordingDelivery{}, NewMockSummarizer(), notify, edit, time.Second, time.Hour, 10*time.Minute, 3, 180)

	o.advance(context.Background(), job)

	if status, _, _ := jobRow(t, s, job.ID); status != string(JobDone) {
		t.Fatalf("статус %q, ожидали done", status)
	}
	if len(edits) != 1 {
		t.Fatalf("editStatus вызван %d раз, ожидали 1", len(edits))
	}
	if edits[0].chatID != userID || edits[0].msgID != 557 {
		t.Errorf("edit в (%d,%d), ожидали (%d,557)", edits[0].chatID, edits[0].msgID, userID)
	}
	if !strings.Contains(edits[0].text, "Готово") {
		t.Errorf("финальный статус %q не похож на done-сообщение", edits[0].text)
	}
	if len(notified) != 0 {
		t.Errorf("notify при успешном edit: %v", notified)
	}
}

// TestSummarizeJob_Happy — summarizing -> done:
// доставлена одна группа файлов: summary_*.md и transcript_*.md.
func TestSummarizeJob_Happy(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job := summarisingJob(t, s, userID, "ext-summarize-happy")

	var notified []int64
	delivery := &recordingDelivery{}
	o := testOrchestrator(s, &MockProvider{}, delivery, &notified)

	o.advance(context.Background(), job)

	status, _, _ := jobRow(t, s, job.ID)
	if status != string(JobDone) {
		t.Errorf("статус %q, ожидали done", status)
	}
	if len(delivery.calls) != 0 {
		t.Fatalf("ожидали group-доставку без одиночных файлов, получили %d", len(delivery.calls))
	}
	if len(delivery.groups) != 1 {
		t.Fatalf("ожидали 1 группу файлов, получили %d", len(delivery.groups))
	}
	group := delivery.groups[0]
	if group.chatID != userID {
		t.Errorf("доставка в чат %d, ожидали %d", group.chatID, userID)
	}
	if group.caption != "✅ Конспект и расшифровка встречи" {
		t.Errorf("caption %q", group.caption)
	}
	if len(group.docs) != 2 {
		t.Fatalf("ожидали 2 файла в группе, получили %d", len(group.docs))
	}
	if want := summaryFilename(job.CreatedAt); group.docs[0].filename != want {
		t.Errorf("summary файл %q, ожидали %q", group.docs[0].filename, want)
	}
	if want := transcriptFilename(job.CreatedAt); group.docs[1].filename != want {
		t.Errorf("transcript файл %q, ожидали %q", group.docs[1].filename, want)
	}
	if !strings.Contains(group.docs[0].content, "# Конспект встречи") {
		t.Errorf("summary файл не содержит конспект: %q", group.docs[0].content)
	}
	if !strings.Contains(group.docs[1].content, "# Транскрипт встречи") {
		t.Errorf("transcript файл не содержит транскрипт: %q", group.docs[1].content)
	}
	if len(notified) != 1 || notified[0] != userID {
		t.Errorf("notify fallback на happy summarize = %v, ожидали один для %d", notified, userID)
	}
}

// TestSummarizeJob_FailSummarize — сбой Summarizer:
// transcript-only файл доставлен, статус failed_summary, notify вызван ровно раз.
func TestSummarizeJob_FailSummarize(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job := summarisingJob(t, s, userID, "ext-summarize-fail")

	var notified []int64
	delivery := &recordingDelivery{}
	// FailOnSummarize=true: Summarize вернёт ошибку.
	failSummarizer := &MockSummarizer{FailOnSummarize: true}
	notify := func(chatID int64, _ string) error {
		notified = append(notified, chatID)
		return nil
	}
	o := NewOrchestrator(s, &MockProvider{}, delivery, failSummarizer, notify, noopEdit, time.Second, time.Hour, 10*time.Minute, 3, 180)

	o.advance(context.Background(), job)

	status, _, _ := jobRow(t, s, job.ID)
	if status != string(JobFailedSummary) {
		t.Errorf("статус %q, ожидали failed_summary", status)
	}
	if len(notified) != 1 || notified[0] != userID {
		t.Errorf("notify = %v, ожидали один вызов для %d", notified, userID)
	}
	if len(delivery.calls) != 1 {
		t.Fatalf("ожидали 1 доставку transcript-only, получили %d", len(delivery.calls))
	}
	if want := transcriptFilename(job.CreatedAt); delivery.calls[0].filename != want {
		t.Errorf("имя файла %q, ожидали %q", delivery.calls[0].filename, want)
	}
	if !strings.Contains(delivery.calls[0].caption, "Конспект временно недоступен") {
		t.Errorf("caption %q не объясняет сбой конспекта", delivery.calls[0].caption)
	}
	if strings.Contains(delivery.calls[0].content, "# Конспект встречи") {
		t.Errorf("transcript-only документ содержит конспект: %q", delivery.calls[0].content)
	}
	if !strings.Contains(delivery.calls[0].content, "# Транскрипт встречи") {
		t.Errorf("transcript-only документ не содержит транскрипт: %q", delivery.calls[0].content)
	}
}

// TestSpeech2TextFailedSummary_UsesProviderDuration — регресс prod-инцидента #38:
// s2t может перескочить joining -> done без recording window, но result/json уже
// содержит duration. При сбое summary billing должен взять provider duration, а не
// fallback 3600s.
func TestSpeech2TextFailedSummary_UsesProviderDuration(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job, err := s.CreateJob(context.Background(), userID, "https://telemost.yandex.ru/j/duration-regression", "speech2text", 180)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := s.SetJobSubmitted(context.Background(), job.ID, "ext-s2t-short"); err != nil {
		t.Fatalf("submit job: %v", err)
	}
	job.Status = JobJoining
	extID := "ext-s2t-short"
	job.ExternalJobID = &extID

	var notified []int64
	delivery := &recordingDelivery{}
	provider := &durationFetchProvider{durationSeconds: 6}
	summarizer := &MockSummarizer{FailOnSummarize: true}
	notify := func(chatID int64, _ string) error {
		notified = append(notified, chatID)
		return nil
	}
	o := NewOrchestrator(s, provider, delivery, summarizer, notify, noopEdit, time.Second, time.Hour, 10*time.Minute, 3, 180)

	o.advance(context.Background(), job)
	if status, _, _ := jobRow(t, s, job.ID); status != string(JobSummarizing) {
		t.Fatalf("после fetch статус %q, ожидали summarizing", status)
	}

	reloaded := reloadJob(t, s, job.ID)
	o.advance(context.Background(), reloaded)

	if status, _, _ := jobRow(t, s, job.ID); status != string(JobFailedSummary) {
		t.Fatalf("после summary failure статус %q, ожидали failed_summary", status)
	}
	billed, duration := jobBilling(t, s, job.ID)
	if billed != 1 {
		t.Fatalf("billed_minutes = %d, ожидали 1 для duration=6s", billed)
	}
	if duration == nil || *duration != 6 {
		t.Fatalf("duration_seconds = %v, ожидали 6", duration)
	}
	if used := trialMinutesUsed(t, s, userID); used != 1 {
		t.Fatalf("trial_minutes_used = %d, ожидали 1", used)
	}
	if done := trialMinutesDone(t, s, userID); done != 1 {
		t.Fatalf("trial_minutes_done = %d, ожидали 1", done)
	}
	if len(delivery.calls) != 1 {
		t.Fatalf("ожидали transcript-only доставку, получили %d", len(delivery.calls))
	}
	if len(notified) != 1 || notified[0] != userID {
		t.Fatalf("notify = %v, ожидали один для %d", notified, userID)
	}
}

func TestSpeech2TextFailedSummary_KeepsRecordingWindowDuration(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job := processingJob(t, s, userID, "ext-s2t-window")
	job.Provider = "speech2text"
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE jobs SET provider = 'speech2text', duration_seconds = 61 WHERE id = $1`,
		job.ID,
	); err != nil {
		t.Fatalf("set stored duration: %v", err)
	}
	storedDuration := 61
	job.DurationSeconds = &storedDuration

	var notified []int64
	delivery := &recordingDelivery{}
	provider := &durationFetchProvider{durationSeconds: 53}
	summarizer := &MockSummarizer{FailOnSummarize: true}
	notify := func(chatID int64, _ string) error {
		notified = append(notified, chatID)
		return nil
	}
	o := NewOrchestrator(s, provider, delivery, summarizer, notify, noopEdit, time.Second, time.Hour, 10*time.Minute, 3, 180)

	o.advance(context.Background(), job)
	if status, _, _ := jobRow(t, s, job.ID); status != string(JobSummarizing) {
		t.Fatalf("после fetch статус %q, ожидали summarizing", status)
	}

	reloaded := reloadJob(t, s, job.ID)
	o.advance(context.Background(), reloaded)

	billed, duration := jobBilling(t, s, job.ID)
	if billed != 2 {
		t.Fatalf("billed_minutes = %d, ожидали 2 для stored duration=61s", billed)
	}
	if duration == nil || *duration != 61 {
		t.Fatalf("duration_seconds = %v, ожидали сохранённые 61s, а не provider chunks 53s", duration)
	}
	if len(delivery.calls) != 1 {
		t.Fatalf("ожидали transcript-only доставку, получили %d", len(delivery.calls))
	}
	if len(notified) != 1 || notified[0] != userID {
		t.Fatalf("notify = %v, ожидали один для %d", notified, userID)
	}
}

// failingDelivery падает на первых failTimes вызовах, дальше успех; считает все вызовы.
type failingDelivery struct {
	failTimes int
	calls     int
}

func (d *failingDelivery) Deliver(_ context.Context, _ int64, _, _ string, _ []byte) error {
	d.calls++
	if d.calls <= d.failTimes {
		return fmt.Errorf("deliver boom")
	}
	return nil
}

func (d *failingDelivery) DeliverGroup(_ context.Context, _ int64, _ string, _ []DeliveryDocument) error {
	d.calls++
	if d.calls <= d.failTimes {
		return fmt.Errorf("deliver boom")
	}
	return nil
}

// countingSummarizer считает вызовы Summarize — для проверки, что платная генерация
// не повторяется на ретрае доставки.
type countingSummarizer struct{ calls int }

func (c *countingSummarizer) Summarize(_ context.Context, _ string) (string, error) {
	c.calls++
	return "конспект встречи", nil
}

// reloadJob перечитывает джобу из БД со всеми полями (включая SummaryMD) — оркестратор
// на следующем тике получает её из ClaimJobsForWork именно так.
func reloadJob(t *testing.T, s *Storage, id int64) Job {
	t.Helper()
	var j Job
	err := s.pool.QueryRow(context.Background(),
		`SELECT id, user_id, meeting_url, status, external_job_id, provider, created_at, status_message_id, summary_md, duration_seconds, last_error
		 FROM jobs WHERE id = $1`, id,
	).Scan(&j.ID, &j.UserID, &j.MeetingURL, &j.Status, &j.ExternalJobID, &j.Provider, &j.CreatedAt, &j.StatusMessageID, &j.SummaryMD, &j.DurationSeconds, &j.LastError)
	if err != nil {
		t.Fatalf("reload job %d: %v", id, err)
	}
	return j
}

// TestSummarizeJob_DeliverRetryDoesNotResummarize — критический денежный узел: сбой
// Deliver оставляет джобу summarizing с закэшированным summary; на ретрае Summarize
// (платный LLM-вызов) НЕ повторяется — берётся кэш, идёт только доставка, джоба -> done.
// Регресс этого = линейный рост стоимости саммари по числу транзиентных сбоев доставки.
func TestSummarizeJob_DeliverRetryDoesNotResummarize(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job := summarisingJob(t, s, userID, "ext-summary-cache")

	notify := func(int64, string) error { return nil }
	delivery := &failingDelivery{failTimes: 1}
	summarizer := &countingSummarizer{}
	o := NewOrchestrator(s, &MockProvider{}, delivery, summarizer, notify, noopEdit, time.Second, time.Hour, 10*time.Minute, 3, 180)

	// Тик 1: Fetch -> Summarize -> кэш summary -> Deliver ПАДАЕТ -> джоба остаётся summarizing.
	o.summarizeJob(context.Background(), job)
	if summarizer.calls != 1 {
		t.Fatalf("после тика 1 Summarize вызван %d раз, ожидали 1", summarizer.calls)
	}
	if status, _, _ := jobRow(t, s, job.ID); status != string(JobSummarizing) {
		t.Fatalf("после сбоя Deliver статус %q, ожидали summarizing", status)
	}
	reloaded := reloadJob(t, s, job.ID)
	if reloaded.SummaryMD == nil || *reloaded.SummaryMD == "" {
		t.Fatal("summary_md не закэширован после генерации — ретрай пересоздаст платно")
	}

	// Тик 2 (cached-путь): Summarize НЕ повторяется, доставка проходит, джоба -> done.
	o.summarizeJob(context.Background(), reloaded)
	if summarizer.calls != 1 {
		t.Errorf("после тика 2 Summarize вызван %d раз — повторная ПЛАТНАЯ генерация! Ожидали 1", summarizer.calls)
	}
	if delivery.calls != 2 {
		t.Errorf("Deliver вызван %d раз, ожидали 2 (сбой + успех)", delivery.calls)
	}
	if status, _, _ := jobRow(t, s, job.ID); status != string(JobDone) {
		t.Errorf("после успешной доставки статус %q, ожидали done", status)
	}
}

// joiningJob создаёт джобу и оставляет её в joining (естественное состояние после
// SetJobSubmitted) — стартовая точка для фазового потока поллинга.
func joiningJob(t *testing.T, s *Storage, userID int64, extID string) Job {
	t.Helper()
	job, err := s.CreateJob(context.Background(), userID, "https://meet.google.com/phase-"+extID, "mock", 180)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := s.SetJobSubmitted(context.Background(), job.ID, extID); err != nil {
		t.Fatalf("submit job: %v", err)
	}
	job.Status = JobJoining
	job.ExternalJobID = &extID
	return job
}

func setJobProvider(t *testing.T, s *Storage, jobID int64, provider string) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE jobs SET provider = $2 WHERE id = $1`,
		jobID, provider,
	); err != nil {
		t.Fatalf("set job provider: %v", err)
	}
}

// scriptedProvider отдаёт заранее заданную последовательность статусов по вызовам Poll
// (по одному на тик; после конца сценария держит последний). Позволяет смоделировать
// и mymeet (OnCall->Transcribing->Done), и s2t (OnCall->...->Done без Transcribing).
type scriptedProvider struct {
	MockProvider
	statuses []ProviderStatus
	polls    int
}

func (p *scriptedProvider) Poll(_ context.Context, _ string) (ProviderStatus, error) {
	i := p.polls
	if i >= len(p.statuses) {
		i = len(p.statuses) - 1
	}
	p.polls++
	return p.statuses[i], nil
}

// recordingEdits собирает editStatus-вызовы; chatID берётся из status_message_id джобы.
func newPhaseOrchestrator(s *Storage, p TranscriptionProvider, edits *[]editCall) *Orchestrator {
	notify := func(int64, string) error { return nil }
	edit := func(chatID, msgID int64, text string) error {
		*edits = append(*edits, editCall{chatID, msgID, text})
		return nil
	}
	// summarizer nil → summary-off: Done завершает джобу транскриптом, без summarizing.
	return NewOrchestrator(s, p, &recordingDelivery{}, nil, notify, edit, time.Second, time.Hour, 10*time.Minute, 3, 180)
}

// TestPhaseFlow_Mymeet — путь mymeet: joining -> recording -> processing -> done.
// На каждом из трёх переходов статус-сообщение редактируется РОВНО раз (moved=true),
// тексты различаются по фазе. Done-тик правит статус на «готово» (4-я правка).
func TestPhaseFlow_Mymeet(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job := joiningJob(t, s, userID, "ext-phase-mymeet")
	if err := s.SetJobStatusMessage(context.Background(), job.ID, 700); err != nil {
		t.Fatalf("set status message: %v", err)
	}

	var edits []editCall
	p := &scriptedProvider{statuses: []ProviderStatus{StatusOnCall, StatusTranscribing, StatusDone}}
	o := newPhaseOrchestrator(s, p, &edits)

	// Тик 1: OnCall -> recording.
	o.advance(context.Background(), reloadJob(t, s, job.ID))
	if st, _, _ := jobRow(t, s, job.ID); st != string(JobRecording) {
		t.Fatalf("после тика 1 статус %q, ожидали recording", st)
	}
	if len(edits) != 1 {
		t.Fatalf("после тика 1 правок %d, ожидали 1 (recording)", len(edits))
	}

	// Тик 2: Transcribing -> processing.
	o.advance(context.Background(), reloadJob(t, s, job.ID))
	if st, _, _ := jobRow(t, s, job.ID); st != string(JobProcessing) {
		t.Fatalf("после тика 2 статус %q, ожидали processing", st)
	}
	if len(edits) != 2 {
		t.Fatalf("после тика 2 правок %d, ожидали 2 (recording, processing)", len(edits))
	}

	// Тик 3: Done -> done (транскрипт доставлен, статус «готово»).
	o.advance(context.Background(), reloadJob(t, s, job.ID))
	if st, _, _ := jobRow(t, s, job.ID); st != string(JobDone) {
		t.Fatalf("после тика 3 статус %q, ожидали done", st)
	}
	if len(edits) != 3 {
		t.Fatalf("после тика 3 правок %d, ожидали 3 (recording, processing, готово)", len(edits))
	}

	// Каждая правка — в нужное сообщение и с различимым по фазе текстом.
	for i, e := range edits {
		if e.chatID != userID || e.msgID != 700 {
			t.Errorf("правка %d в (%d,%d), ожидали (%d,700)", i, e.chatID, e.msgID, userID)
		}
	}
	if edits[0].text == edits[1].text || edits[1].text == edits[2].text || edits[0].text == edits[2].text {
		t.Errorf("тексты правок не различимы по фазе: %q / %q / %q", edits[0].text, edits[1].text, edits[2].text)
	}
}

// TestPhaseFlow_S2T — путь s2t: joining -> recording -> processing -> done.
// s2t не отдаёт отдельный Transcribing, поэтому processing фиксируется прямо перед Done,
// чтобы закрыть billing window.
func TestPhaseFlow_S2T(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job := joiningJob(t, s, userID, "ext-phase-s2t")
	setJobProvider(t, s, job.ID, "speech2text")
	if err := s.SetJobStatusMessage(context.Background(), job.ID, 701); err != nil {
		t.Fatalf("set status message: %v", err)
	}

	var edits []editCall
	// OnCall дважды (бот ещё пишет), затем Done — Transcribing не приходит.
	p := &scriptedProvider{statuses: []ProviderStatus{StatusOnCall, StatusOnCall, StatusDone}}
	o := newPhaseOrchestrator(s, p, &edits)

	// Тик 1: OnCall -> recording (1 правка).
	o.advance(context.Background(), reloadJob(t, s, job.ID))
	if st, _, _ := jobRow(t, s, job.ID); st != string(JobRecording) {
		t.Fatalf("после тика 1 статус %q, ожидали recording", st)
	}
	if len(edits) != 1 {
		t.Fatalf("после тика 1 правок %d, ожидали 1 (recording)", len(edits))
	}

	// Тик 2: OnCall на уже-recording — moved=false, второй правки НЕТ (идемпотентность).
	o.advance(context.Background(), reloadJob(t, s, job.ID))
	if st, _, _ := jobRow(t, s, job.ID); st != string(JobRecording) {
		t.Fatalf("после тика 2 статус %q, ожидали recording (держится)", st)
	}
	if len(edits) != 1 {
		t.Fatalf("после тика 2 правок %d, ожидали 1 (повторный OnCall не правит)", len(edits))
	}

	// Тик 3: Done сначала фиксирует processing/duration, затем закрывает job.
	o.advance(context.Background(), reloadJob(t, s, job.ID))
	if st, _, _ := jobRow(t, s, job.ID); st != string(JobDone) {
		t.Fatalf("после тика 3 статус %q, ожидали done", st)
	}
	if len(edits) != 3 {
		t.Fatalf("после тика 3 правок %d, ожидали 3 (recording, processing, готово)", len(edits))
	}
}

func TestPhaseFlow_S2T_QueuedRecordingDoesNotMeanProcessing(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job := joiningJob(t, s, userID, "ext-phase-s2t-queued")
	setJobProvider(t, s, job.ID, "speech2text")
	if err := s.SetJobStatusMessage(context.Background(), job.ID, 702); err != nil {
		t.Fatalf("set status message: %v", err)
	}

	var edits []editCall
	p := &scriptedProvider{statuses: []ProviderStatus{StatusOnCall, StatusQueuedRecording, StatusQueuedRecording, StatusDone}}
	o := newPhaseOrchestrator(s, p, &edits)

	o.advance(context.Background(), reloadJob(t, s, job.ID))
	if st, _, _ := jobRow(t, s, job.ID); st != string(JobRecording) {
		t.Fatalf("после тика 1 статус %q, ожидали recording", st)
	}

	o.advance(context.Background(), reloadJob(t, s, job.ID))
	if st, _, _ := jobRow(t, s, job.ID); st != string(JobRecording) {
		t.Fatalf("после тика 2 статус %q, ожидали recording: очередь s2t не означает processing", st)
	}
	if len(edits) != 2 {
		t.Fatalf("после тика 2 правок %d, ожидали 2 (recording, queued)", len(edits))
	}
	if strings.Contains(edits[1].text, "Запись завершилась") || strings.Contains(edits[1].text, "Расшифровываю") {
		t.Fatalf("тик 2 показал ложное завершение записи: %q", edits[1].text)
	}

	o.advance(context.Background(), reloadJob(t, s, job.ID))
	if st, _, _ := jobRow(t, s, job.ID); st != string(JobRecording) {
		t.Fatalf("после тика 3 статус %q, ожидали recording: повтор очереди не двигает job", st)
	}
	if len(edits) != 2 {
		t.Fatalf("после повтора очереди правок %d, ожидали 2 (dedupe одинакового текста)", len(edits))
	}

	o.advance(context.Background(), reloadJob(t, s, job.ID))
	if st, _, _ := jobRow(t, s, job.ID); st != string(JobDone) {
		t.Fatalf("после тика 4 статус %q, ожидали done", st)
	}
	if len(edits) != 4 {
		t.Fatalf("правок статуса %d, ожидали 4 (recording, queued, processing, done)", len(edits))
	}
}

// TestPhaseFlow_OnCallIdempotent — повторный OnCall на уже-recording джобе НЕ шлёт
// второй Edit. Гарантия «правка ровно раз на фактический переход»: moved=false при
// неизменной фазе. Изолированная проверка идемпотентности отдельно от полного потока.
func TestPhaseFlow_OnCallIdempotent(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job := joiningJob(t, s, userID, "ext-phase-idem")
	if err := s.SetJobStatusMessage(context.Background(), job.ID, 702); err != nil {
		t.Fatalf("set status message: %v", err)
	}

	var edits []editCall
	p := &scriptedProvider{statuses: []ProviderStatus{StatusOnCall, StatusOnCall, StatusOnCall}}
	o := newPhaseOrchestrator(s, p, &edits)

	for tick := 1; tick <= 3; tick++ {
		o.advance(context.Background(), reloadJob(t, s, job.ID))
	}

	if st, _, _ := jobRow(t, s, job.ID); st != string(JobRecording) {
		t.Errorf("статус %q, ожидали recording", st)
	}
	if len(edits) != 1 {
		t.Errorf("правок %d, ожидали ровно 1 (только первый OnCall правит статус)", len(edits))
	}
}
