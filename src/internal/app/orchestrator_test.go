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

// Tests of the advance dispatch, a critical node: "state machine transitions" (docs/testing.md).
// Storage is real (Postgres from DATABASE_URL, see storage_test.go), the provider is
// MockProvider, delivery and notify are recording fakes.

// recordingDelivery records Deliver calls for assertions.
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

// testOrchestrator assembles an orchestrator on fakes; notify calls are written to *notified.
func testOrchestrator(s *Storage, p TranscriptionProvider, d DocumentDelivery, notified *[]int64) *Orchestrator {
	notify := func(chatID int64, _ string) error {
		*notified = append(*notified, chatID)
		return nil
	}
	// maxJobAge is large: freshly created test jobs must not hit the orphan timeout.
	// editStatus is a no-op: test jobs have status_message_id=nil, so updateStatus goes to notify.
	return NewOrchestrator(s, p, d, NewMockSummarizer(), notify, noopEdit, time.Second, time.Hour, 10*time.Minute, 3, 180)
}

// noopEdit is an editStatus stub for tests where there is no status message (fallback to notify).
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
			t.Fatalf("notify chatID = %d, want %d", chatID, userID)
		}
		sent = append(sent, text)
		return nil
	}
	o := NewOrchestrator(s, &MockProvider{}, &recordingDelivery{}, nil, notify, noopEdit, time.Second, time.Hour, 10*time.Minute, 3, 60)

	o.maybeNotifyBalanceDepleted(ctx, job)

	if len(sent) != 1 || sent[0] != "Баланс закончился" {
		t.Fatalf("sent = %v, want one message about the zero balance", sent)
	}
}

// editCall records an editStatus call (chat, message, text) for edit assertions.
type editCall struct {
	chatID, msgID int64
	text          string
}

// processingJob creates a job and brings it to processing (the bot has left, background
// transcription) with the given external id: a fixture for the pollJob branches. It goes through the phases
// joining -> recording -> processing honestly via setters, so that the DB row is really
// in processing (not only in memory): tests on "the job stays in processing"
// check the row state.
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

// TestAdvance_CreatedSubmits: created -> Submit at the provider, transition to joining
// (the bot is connecting; new semantics: processing means "the bot has already left"),
// external_job_id is saved; no deliveries or notifications.
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
		t.Errorf("status %q, want joining", status)
	}
	want := fmt.Sprintf("mock-job-%d", job.ID)
	if extID == nil || *extID != want {
		t.Errorf("external_job_id = %v, want %q", extID, want)
	}
	if len(delivery.calls) != 0 {
		t.Errorf("premature delivery: %v", delivery.calls)
	}
	// Progress statuses edit the saved status message. If the message id is not yet
	// recorded, we send no new fallback messages, to avoid spamming the user.
	if len(notified) != 0 {
		t.Errorf("progress push about submit without status_message_id: notified=%v", notified)
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
		t.Errorf("status = %q, want %q", got, JobFailed)
	}
	if got := trialUsed(t, s, userID); got != 0 {
		t.Errorf("trial_meetings_used = %d, want 0 (refund on submit failure)", got)
	}
	if provider.submitCalls != 1 {
		t.Errorf("submitCalls = %d, want 1", provider.submitCalls)
	}
	if got := o.submitAttempts[job.ID]; got != 0 {
		t.Errorf("submitAttempts[%d] = %d, want cleanup", job.ID, got)
	}
	if len(notified) != 1 || notified[0] != userID {
		t.Errorf("fail notification = %v, want one for %d", notified, userID)
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
		t.Fatalf("after first attempt status = %q, want %q", got, JobCreated)
	}
	o.advance(context.Background(), job)
	if got := jobStatus(t, s, job.ID); got != string(JobCreated) {
		t.Fatalf("after second attempt status = %q, want %q", got, JobCreated)
	}
	o.advance(context.Background(), job)
	if got := jobStatus(t, s, job.ID); got != string(JobFailed) {
		t.Errorf("after third attempt status = %q, want %q", got, JobFailed)
	}
	if got := trialUsed(t, s, userID); got != 0 {
		t.Errorf("trial_meetings_used = %d, want 0 (refund after attempts)", got)
	}
	if provider.submitCalls != 3 {
		t.Errorf("submitCalls = %d, want 3", provider.submitCalls)
	}
	if got := o.submitAttempts[job.ID]; got != 0 {
		t.Errorf("submitAttempts[%d] = %d, want cleanup", job.ID, got)
	}
	if len(notified) != 1 || notified[0] != userID {
		t.Errorf("fail notification = %v, want one for %d", notified, userID)
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
		t.Errorf("status = %q, want %q", got, JobFailed)
	}
	if provider.submitCalls != 0 {
		t.Errorf("submitCalls = %d, want 0", provider.submitCalls)
	}
	if got := trialUsed(t, s, userID); got != 0 {
		t.Errorf("trial_meetings_used = %d, want 0 (refund on created timeout)", got)
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
		t.Errorf("status = %q, want %q", got, JobFailed)
	}
	if len(provider.Cancelled) != 1 || provider.Cancelled[0] != extID {
		t.Errorf("Cancelled = %v, want [%s]", provider.Cancelled, extID)
	}
	if got := trialUsed(t, s, userID); got != 0 {
		t.Errorf("trial_meetings_used = %d, want 0 (refund on joining timeout)", got)
	}
}

// TestSubmit_LostToConcurrentCancel: a submit<->cancel race. The user canceled a created job
// while Submit was in flight. SetJobSubmitted (guard status='created') does not resurrect failed->processing;
// submitJob stops the orphaned bot at the provider. Without the guard the job would come back to life with
// the returned quota and an active bot (a money + state-machine bug).
func TestSubmit_LostToConcurrentCancel(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job, err := s.CreateJob(context.Background(), userID, "https://meet.google.com/submit-race", "mock", 180)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// The user cancels while the job is still created (the bot was not sent): trial refund.
	if _, _, _, err := s.CancelJob(context.Background(), userID, job.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	var notified []int64
	notify := func(chatID int64, _ string) error { notified = append(notified, chatID); return nil }
	mp := &MockProvider{}
	o := NewOrchestrator(s, mp, &recordingDelivery{}, NewMockSummarizer(), notify, noopEdit, time.Second, time.Hour, 10*time.Minute, 3, 180)

	// The orchestrator holds the in-memory created (claim happened before the cancel) and continues submit.
	o.advance(context.Background(), job)

	if st, _, _ := jobRow(t, s, job.ID); st != string(JobFailed) {
		t.Errorf("status=%s, want failed (do not resurrect a canceled one)", st)
	}
	want := fmt.Sprintf("mock-job-%d", job.ID)
	if len(mp.Cancelled) != 1 || mp.Cancelled[0] != want {
		t.Errorf("provider Cancel=%v, want [%s] (stop the orphaned bot)", mp.Cancelled, want)
	}
	if used := trialUsed(t, s, userID); used != 0 {
		t.Errorf("trial_used=%d, want 0 (the quota is not re-reserved on a lost submit)", used)
	}
	if len(notified) != 0 {
		t.Errorf("progress push on a lost submit: %v", notified)
	}
}

// TestAdvance_TerminalStatusIgnored: a state machine invariant. A terminal status
// (done/failed/failed_summary are excluded by the partial index, but if such a Job still
// passed claim) falls into the default branch: advance does nothing, the status
// does not move, no Deliver, no notify. Protects against an accidental repeated Fetch/Deliver
// of an already finished job. done represents the class of terminal statuses.
func TestAdvance_TerminalStatusIgnored(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	// A real job in the DB, brought to done. SetJobDone requires the pre-status
	// processing|summarizing (§5.7), so the job has to go through
	// submitted -> summarizing -> done, otherwise the test would check the wrong thing.
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
		t.Errorf("status %q, want done (a terminal one does not move)", status)
	}
	if len(delivery.calls) != 0 || len(notified) != 0 {
		t.Errorf("delivery/notify on a terminal status: %v / %v", delivery.calls, notified)
	}
}

// TestAdvance_PendingKeeps: Pending: the status does not move, nothing is sent.
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
		t.Errorf("status %q, want processing", status)
	}
	if len(delivery.calls) != 0 || len(notified) != 0 {
		t.Errorf("delivery/notify on Pending: %v / %v", delivery.calls, notified)
	}
}

// TestAdvance_FailedNotifies: Failed: a failed record + exactly one notification
// to the user, no delivery.
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
		t.Errorf("status %q, want failed", status)
	}
	if len(notified) != 1 || notified[0] != userID {
		t.Errorf("notify = %v, want one call for %d", notified, userID)
	}
	if len(delivery.calls) != 0 {
		t.Errorf("delivery on failed: %v", delivery.calls)
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
		t.Fatalf("Fetch called %d times without a stop marker", provider.fetchCalls)
	}
	if status, _, _ := jobRow(t, s, job.ID); status != string(JobFailed) {
		t.Fatalf("status = %q, want failed", status)
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
		t.Fatalf("Fetch called %d times, want 1", provider.fetchCalls)
	}
	if status, _, _ := jobRow(t, s, job.ID); status != string(JobSummarizing) {
		t.Fatalf("status = %q, want summarizing after the transcript", status)
	}
	if len(delivery.calls) != 0 {
		t.Fatalf("delivery before the summary was not expected: %v", delivery.calls)
	}
	if used := trialUsed(t, s, userID); used != 1 {
		t.Fatalf("trial_used = %d, want the reservation kept until terminal billing", used)
	}

	job = reloadJob(t, s, job.ID)
	o.advance(context.Background(), job)

	if status, _, _ := jobRow(t, s, job.ID); status != string(JobDone) {
		t.Fatalf("status = %q, want done after the summary", status)
	}
	billed, duration := jobBilling(t, s, job.ID)
	if billed != 1 {
		t.Fatalf("billed_minutes = %d, want 1 for stopped transcript duration=60s", billed)
	}
	if duration == nil || *duration != 60 {
		t.Fatalf("duration_seconds = %v, want 60", duration)
	}
	if used := trialUsed(t, s, userID); used != 1 {
		t.Fatalf("trial_used = %d, want a billable result without refund", used)
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
		t.Fatalf("Fetch called %d times before fixing processing/duration", provider.fetchCalls)
	}
	reloaded := reloadJob(t, s, job.ID)
	if reloaded.Status != JobProcessing {
		t.Fatalf("status = %q, want processing", reloaded.Status)
	}
	if reloaded.DurationSeconds == nil || *reloaded.DurationSeconds < 5*60 {
		t.Fatalf("duration_seconds = %v, want the fixed recording window", reloaded.DurationSeconds)
	}

	o.advance(context.Background(), reloaded)

	if provider.fetchCalls != 1 {
		t.Fatalf("Fetch called %d times, want 1 after the transition to processing", provider.fetchCalls)
	}
	if status, _, _ := jobRow(t, s, job.ID); status != string(JobSummarizing) {
		t.Fatalf("status = %q, want summarizing after fetch", status)
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
		t.Fatalf("Fetch called %d times, want 1", provider.fetchCalls)
	}
	if status, _, _ := jobRow(t, s, job.ID); status != string(JobFailed) {
		t.Fatalf("status = %q, want failed", status)
	}
	if used := trialUsed(t, s, userID); used != 0 {
		t.Fatalf("trial_used = %d, want refund", used)
	}
	if len(delivery.calls) != 0 || len(delivery.groups) != 0 {
		t.Fatalf("delivery on empty transcript: calls=%v groups=%v", delivery.calls, delivery.groups)
	}
}

// TestAdvance_FailedDBErrorSkipsNotify: if the failed write did not go through, notify
// is not sent: otherwise the user would get a duplicate "did not work" on every tick
// until the UPDATE succeeds.
func TestAdvance_FailedDBErrorSkipsNotify(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job := processingJob(t, s, userID, "ext-dberr")

	var notified []int64
	o := testOrchestrator(s, &MockProvider{FailOnPoll: true}, &recordingDelivery{}, &notified)

	// A canceled ctx fails FailJobWithQuotaRefund (the mock ignores ctx and honestly returns Failed).
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	o.advance(ctx, job)

	status, _, _ := jobRow(t, s, job.ID)
	if status != string(JobProcessing) {
		t.Errorf("status %q, want processing (the failed write did not go through)", status)
	}
	if len(notified) != 0 {
		t.Errorf("notify on a failed write of failed: %v", notified)
	}
}

// TestPollJob_StaleProcessingDoesNotTimeout: processing already means the bot has left
// and the provider is preparing the result. created_at must no longer fail such a job:
// otherwise a manual stop of a long meeting near the limit would lose the transcript at once.
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
	usedReserved := readUsed() // the trial reservation is deducted after CreateJob

	// Age the job past the maxJobAge threshold.
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
		t.Errorf("status %q, want processing (stale processing is not a timeout)", status)
	}
	if used := readUsed(); used != usedReserved {
		t.Errorf("trial_meetings_used = %d, want %d (no refund for processing)", used, usedReserved)
	}
	if len(mp.Cancelled) != 0 {
		t.Errorf("provider Cancel = %v, want no-op for processing", mp.Cancelled)
	}
	if len(notified) != 0 {
		t.Errorf("notify = %v, want no-op", notified)
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
		t.Fatalf("status %q, want processing after the watchdog stop", status)
	}
	reloaded := reloadJob(t, s, job.ID)
	if reloaded.LastError == nil || *reloaded.LastError != jobStopWatchdog {
		t.Fatalf("last_error = %v, want %q", reloaded.LastError, jobStopWatchdog)
	}
	if len(mp.Cancelled) != 1 || mp.Cancelled[0] != "ext-stale-recording" {
		t.Fatalf("provider Cancel = %v, want [ext-stale-recording]", mp.Cancelled)
	}
	if len(notified) != 0 {
		t.Fatalf("notify = %v, want no-op progress without status_message_id", notified)
	}
}

// TestPollJob_FreshJobNotStale: the negative boundary of the orphan timeout (B2): a fresh
// processing job (younger than maxJobAge) is NOT failed: it passes the guard and goes to Poll.
// Twin of TestPollJob_StaleTimeout: together they close both sides of the time.Since>maxJobAge threshold
// (a mirror against an inverted comparison sign). Poll returns Pending (DoneAfterPolls is large),
// so with a working guard the job stays processing, Cancel is NOT called, no refund/notify.
func TestPollJob_FreshJobNotStale(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job := processingJob(t, s, userID, "ext-fresh") // created_at = now(): fresh

	readUsed := func() int {
		var u int
		if err := s.pool.QueryRow(context.Background(),
			`SELECT trial_meetings_used FROM users WHERE telegram_id = $1`, userID).Scan(&u); err != nil {
			t.Fatalf("read trial_meetings_used: %v", err)
		}
		return u
	}
	usedReserved := readUsed() // trial reservation deducted at CreateJob

	var notified []int64
	notify := func(chatID int64, _ string) error { notified = append(notified, chatID); return nil }
	mp := &MockProvider{DoneAfterPolls: 100} // Poll returns Pending: the job must not move
	o := NewOrchestrator(s, mp, &recordingDelivery{}, NewMockSummarizer(), notify, noopEdit, time.Second, time.Hour, 10*time.Minute, 3, 180)

	o.advance(context.Background(), job)

	status, _, _ := jobRow(t, s, job.ID)
	if status != string(JobProcessing) {
		t.Errorf("status %q, want processing (a fresh job is not an orphan)", status)
	}
	if len(mp.Cancelled) != 0 {
		t.Errorf("provider Cancel on a fresh job: %v", mp.Cancelled)
	}
	if used := readUsed(); used != usedReserved {
		t.Errorf("trial_meetings_used = %d, want %d (refund of a fresh job)", used, usedReserved)
	}
	if len(notified) != 0 {
		t.Errorf("notify on a fresh processing job: %v", notified)
	}
}

// summarisingJob creates a job and moves it to summarizing with the given external id:
// a fixture for the summarizeJob branches.
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

// TestFinishTranscription_WaitsForSummaryDelivery: processing + Done with the Summarizer
// enabled: Fetch saves raw/duration and moves to summarizing, but does NOT deliver the
// transcript in advance. summarizeJob sends the file group after the summary.
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
		t.Errorf("status %q, want summarizing", status)
	}
	if !json.Valid(raw) {
		t.Errorf("raw_provider_response is not valid JSON: %s", raw)
	}
	if len(delivery.calls) != 0 {
		t.Fatalf("premature delivery before the summary: %v", delivery.calls)
	}
	if len(notified) != 0 {
		t.Errorf("notify on finishTranscription: %v", notified)
	}
}

// providerSummaryText is a fixed summary FROM the provider fake. The test checks that
// exactly it goes to the chat (not the Summarizer output), which proves the provider-summary priority.
const providerSummaryText = "# Саммари встречи\n\nКраткое содержание."

// providerWithSummary is a minimal provider fake whose Fetch returns a non-empty
// SummaryMarkdown (a test of the provider-summary branch in finishTranscription).
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

// TestFinishTranscription_EmptyTranscriptRefunds: s2t done/code=204 or an empty txt:
// the meeting does not count as successful, no files are sent, summary is not started,
// the trial reservation is rolled back, trial_done does not grow.
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
		t.Fatalf("trial_meetings_used after the reservation = %d, want 1", used)
	}

	var notified []int64
	delivery := &recordingDelivery{}
	o := testOrchestrator(s, &emptyFetchProvider{}, delivery, &notified)

	o.advance(context.Background(), job)

	status, _, _ := jobRow(t, s, job.ID)
	if status != string(JobFailed) {
		t.Errorf("status %q, want failed", status)
	}
	if used := trialUsed(t, s, userID); used != 0 {
		t.Errorf("trial_meetings_used = %d, want 0 (refund of an empty meeting)", used)
	}
	if done := trialDone(t, s, userID); done != 0 {
		t.Errorf("trial_meetings_done = %d, want 0 (the empty meeting is not counted)", done)
	}
	if len(delivery.calls) != 0 {
		t.Errorf("delivery on an empty transcript: %v", delivery.calls)
	}
	if len(notified) != 1 || notified[0] != userID {
		t.Errorf("notify = %v, want only the terminal failed fallback for %d", notified, userID)
	}
}

// TestFinishTranscription_ProviderSummary_Done: the provider-summary branch:
// processing + Done with a non-empty SummaryMarkdown -> one group of summary+transcript,
// status done (bypassing summarizing), raw is valid.
func TestFinishTranscription_ProviderSummary_Done(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job := processingJob(t, s, userID, "ext-provider-summary")

	var notified []int64
	delivery := &recordingDelivery{}
	notify := func(chatID int64, _ string) error { notified = append(notified, chatID); return nil }
	// The Summarizer is CONFIGURED (not nil): we prove that provider-summary wins:
	// the branch sits before summarizer==nil, the summary comes from the provider, not from the Summarizer.
	o := NewOrchestrator(s, &providerWithSummary{}, delivery, NewMockSummarizer(), notify, noopEdit, time.Second, time.Hour, 10*time.Minute, 3, 180)

	o.advance(context.Background(), job)

	status, _, raw := jobRow(t, s, job.ID)
	if status != string(JobDone) {
		t.Errorf("status %q, want done (provider-summary bypasses summarizing)", status)
	}
	if !json.Valid(raw) {
		t.Errorf("raw_provider_response is not valid JSON: %s", raw)
	}
	if len(delivery.calls) != 0 {
		t.Fatalf("want group delivery without single files, got %d", len(delivery.calls))
	}
	if len(delivery.groups) != 1 {
		t.Fatalf("want 1 file group, got %d", len(delivery.groups))
	}
	group := delivery.groups[0]
	if group.caption != "✅ Конспект и расшифровка встречи" {
		t.Errorf("caption %q", group.caption)
	}
	if len(group.docs) != 2 {
		t.Fatalf("want 2 files in the group, got %d", len(group.docs))
	}
	if want := summaryFilename(job.CreatedAt); group.docs[0].filename != want {
		t.Errorf("summary file %q, want %q", group.docs[0].filename, want)
	}
	if want := transcriptFilename(job.CreatedAt); group.docs[1].filename != want {
		t.Errorf("transcript file %q, want %q", group.docs[1].filename, want)
	}
	if !strings.Contains(group.docs[0].content, providerSummaryText) {
		t.Errorf("summary file does not contain provider-summary: %q", group.docs[0].content)
	}
	if !strings.Contains(group.docs[1].content, "# Транскрипт встречи") {
		t.Errorf("transcript file does not contain the transcript: %q", group.docs[1].content)
	}
	reloaded := reloadJob(t, s, job.ID)
	if reloaded.SummaryMD == nil || *reloaded.SummaryMD != providerSummaryText {
		t.Errorf("summary_md = %v, want the cached provider-summary", reloaded.SummaryMD)
	}
	// the job has no status_message_id -> the "done" status goes through the notify fallback.
	if len(notified) != 1 || notified[0] != userID {
		t.Errorf("notify (done status) = %v, want one for %d", notified, userID)
	}
}

// TestFinishTranscription_SummaryOff_Done: summary is off (summarizer == nil,
// SUMMARIZER_PROVIDER=off): processing + Done -> the transcript is delivered, the status goes straight to
// done (bypassing summarizing), no notify. Covers the demo flow without F3.
func TestFinishTranscription_SummaryOff_Done(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job := processingJob(t, s, userID, "ext-summary-off")

	var notified []int64
	delivery := &recordingDelivery{}
	notify := func(chatID int64, _ string) error { notified = append(notified, chatID); return nil }
	// summarizer == nil: summary is off.
	o := NewOrchestrator(s, &MockProvider{}, delivery, nil, notify, noopEdit, time.Second, time.Hour, 10*time.Minute, 3, 180)

	o.advance(context.Background(), job)

	status, _, raw := jobRow(t, s, job.ID)
	if status != string(JobDone) {
		t.Errorf("status %q, want done (summary is off)", status)
	}
	if !json.Valid(raw) {
		t.Errorf("raw_provider_response is not valid JSON: %s", raw)
	}
	if len(delivery.calls) != 1 {
		t.Fatalf("want 1 delivery (transcript), got %d", len(delivery.calls))
	}
	if want := transcriptFilename(job.CreatedAt); delivery.calls[0].filename != want {
		t.Errorf("file name %q, want %q", delivery.calls[0].filename, want)
	}
	// the job has no status_message_id -> the "done" status goes through the notify fallback (1 message).
	if len(notified) != 1 || notified[0] != userID {
		t.Errorf("notify (done status fallback) = %v, want one for %d", notified, userID)
	}
}

// TestUpdateStatus_EditsExistingMessage: when status_message_id exists the orchestrator
// EDITS the status message (editStatus) instead of sending a new one (notify). UX "no spam".
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
	// summarizer nil -> summary-off: pollJob Done -> finishTranscription -> done + the "done" status.
	o := NewOrchestrator(s, &MockProvider{}, &recordingDelivery{}, nil, notify, edit, time.Second, time.Hour, 10*time.Minute, 3, 180)

	o.advance(context.Background(), job)

	if len(edits) == 0 {
		t.Fatal("editStatus was not called although status_message_id exists")
	}
	last := edits[len(edits)-1]
	if last.chatID != userID || last.msgID != 555 {
		t.Errorf("edit at (%d,%d), want (%d,555)", last.chatID, last.msgID, userID)
	}
	if len(notified) != 0 {
		t.Errorf("notify although a status message exists (must be edit): %v", notified)
	}
}

// TestUpdateStatus_EditFailureFallsBackToNotify: if Telegram did not allow editing the
// old message, the final status must still appear as a new message.
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
		t.Errorf("notify fallback = %v, want one call for %d", notified, userID)
	}
}

// TestSummarizeJob_EditsStatusOnDone: summarizing -> done must edit the same
// status message, otherwise the user is left with "the bot is at the meeting" after the files.
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
		t.Fatalf("status %q, want done", status)
	}
	if len(edits) != 1 {
		t.Fatalf("editStatus called %d times, want 1", len(edits))
	}
	if edits[0].chatID != userID || edits[0].msgID != 557 {
		t.Errorf("edit at (%d,%d), want (%d,557)", edits[0].chatID, edits[0].msgID, userID)
	}
	if !strings.Contains(edits[0].text, "Готово") {
		t.Errorf("final status %q does not look like a done message", edits[0].text)
	}
	if len(notified) != 0 {
		t.Errorf("notify on a successful edit: %v", notified)
	}
}

// TestSummarizeJob_Happy: summarizing -> done:
// one file group delivered: summary_*.md and transcript_*.md.
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
		t.Errorf("status %q, want done", status)
	}
	if len(delivery.calls) != 0 {
		t.Fatalf("want group delivery without single files, got %d", len(delivery.calls))
	}
	if len(delivery.groups) != 1 {
		t.Fatalf("want 1 file group, got %d", len(delivery.groups))
	}
	group := delivery.groups[0]
	if group.chatID != userID {
		t.Errorf("delivery to chat %d, want %d", group.chatID, userID)
	}
	if group.caption != "✅ Конспект и расшифровка встречи" {
		t.Errorf("caption %q", group.caption)
	}
	if len(group.docs) != 2 {
		t.Fatalf("want 2 files in the group, got %d", len(group.docs))
	}
	if want := summaryFilename(job.CreatedAt); group.docs[0].filename != want {
		t.Errorf("summary file %q, want %q", group.docs[0].filename, want)
	}
	if want := transcriptFilename(job.CreatedAt); group.docs[1].filename != want {
		t.Errorf("transcript file %q, want %q", group.docs[1].filename, want)
	}
	if !strings.Contains(group.docs[0].content, "# Конспект встречи") {
		t.Errorf("summary file does not contain the summary: %q", group.docs[0].content)
	}
	if !strings.Contains(group.docs[1].content, "# Транскрипт встречи") {
		t.Errorf("transcript file does not contain the transcript: %q", group.docs[1].content)
	}
	if len(notified) != 1 || notified[0] != userID {
		t.Errorf("notify fallback on happy summarize = %v, want one for %d", notified, userID)
	}
}

// TestSummarizeJob_FailSummarize: a Summarizer failure:
// the transcript-only file is delivered, status failed_summary, notify called exactly once.
func TestSummarizeJob_FailSummarize(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job := summarisingJob(t, s, userID, "ext-summarize-fail")

	var notified []int64
	delivery := &recordingDelivery{}
	// FailOnSummarize=true: Summarize returns an error.
	failSummarizer := &MockSummarizer{FailOnSummarize: true}
	notify := func(chatID int64, _ string) error {
		notified = append(notified, chatID)
		return nil
	}
	o := NewOrchestrator(s, &MockProvider{}, delivery, failSummarizer, notify, noopEdit, time.Second, time.Hour, 10*time.Minute, 3, 180)

	o.advance(context.Background(), job)

	status, _, _ := jobRow(t, s, job.ID)
	if status != string(JobFailedSummary) {
		t.Errorf("status %q, want failed_summary", status)
	}
	if len(notified) != 1 || notified[0] != userID {
		t.Errorf("notify = %v, want one call for %d", notified, userID)
	}
	if len(delivery.calls) != 1 {
		t.Fatalf("want 1 transcript-only delivery, got %d", len(delivery.calls))
	}
	if want := transcriptFilename(job.CreatedAt); delivery.calls[0].filename != want {
		t.Errorf("file name %q, want %q", delivery.calls[0].filename, want)
	}
	if !strings.Contains(delivery.calls[0].caption, "Конспект временно недоступен") {
		t.Errorf("caption %q does not explain the summary failure", delivery.calls[0].caption)
	}
	if strings.Contains(delivery.calls[0].content, "# Конспект встречи") {
		t.Errorf("transcript-only document contains the summary: %q", delivery.calls[0].content)
	}
	if !strings.Contains(delivery.calls[0].content, "# Транскрипт встречи") {
		t.Errorf("transcript-only document does not contain the transcript: %q", delivery.calls[0].content)
	}
}

// TestSpeech2TextFailedSummary_UsesProviderDuration: a regression of prod incident #38:
// s2t can jump joining -> done without a recording window, but result/json already
// contains duration. On a summary failure billing must take the provider duration, not the
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
		t.Fatalf("after fetch status %q, want summarizing", status)
	}

	reloaded := reloadJob(t, s, job.ID)
	o.advance(context.Background(), reloaded)

	if status, _, _ := jobRow(t, s, job.ID); status != string(JobFailedSummary) {
		t.Fatalf("after summary failure status %q, want failed_summary", status)
	}
	billed, duration := jobBilling(t, s, job.ID)
	if billed != 1 {
		t.Fatalf("billed_minutes = %d, want 1 for duration=6s", billed)
	}
	if duration == nil || *duration != 6 {
		t.Fatalf("duration_seconds = %v, want 6", duration)
	}
	if used := trialMinutesUsed(t, s, userID); used != 1 {
		t.Fatalf("trial_minutes_used = %d, want 1", used)
	}
	if done := trialMinutesDone(t, s, userID); done != 1 {
		t.Fatalf("trial_minutes_done = %d, want 1", done)
	}
	if len(delivery.calls) != 1 {
		t.Fatalf("want transcript-only delivery, got %d", len(delivery.calls))
	}
	if len(notified) != 1 || notified[0] != userID {
		t.Fatalf("notify = %v, want one for %d", notified, userID)
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
		t.Fatalf("after fetch status %q, want summarizing", status)
	}

	reloaded := reloadJob(t, s, job.ID)
	o.advance(context.Background(), reloaded)

	billed, duration := jobBilling(t, s, job.ID)
	if billed != 2 {
		t.Fatalf("billed_minutes = %d, want 2 for stored duration=61s", billed)
	}
	if duration == nil || *duration != 61 {
		t.Fatalf("duration_seconds = %v, want the stored 61s, not provider chunks 53s", duration)
	}
	if len(delivery.calls) != 1 {
		t.Fatalf("want transcript-only delivery, got %d", len(delivery.calls))
	}
	if len(notified) != 1 || notified[0] != userID {
		t.Fatalf("notify = %v, want one for %d", notified, userID)
	}
}

// failingDelivery fails on the first failTimes calls, then succeeds; counts all calls.
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

// countingSummarizer counts Summarize calls, to check that paid generation
// is not repeated on a delivery retry.
type countingSummarizer struct{ calls int }

func (c *countingSummarizer) Summarize(_ context.Context, _ string) (string, error) {
	c.calls++
	return "конспект встречи", nil
}

// reloadJob rereads a job from the DB with all fields (including SummaryMD): the orchestrator
// gets it from ClaimJobsForWork exactly this way on the next tick.
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

// TestSummarizeJob_DeliverRetryDoesNotResummarize: a critical money node. A
// Deliver failure leaves the job in summarizing with a cached summary; on retry Summarize
// (a paid LLM call) is NOT repeated: the cache is used, only delivery runs, the job -> done.
// A regression here = the summary cost grows linearly with the number of transient delivery failures.
func TestSummarizeJob_DeliverRetryDoesNotResummarize(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job := summarisingJob(t, s, userID, "ext-summary-cache")

	notify := func(int64, string) error { return nil }
	delivery := &failingDelivery{failTimes: 1}
	summarizer := &countingSummarizer{}
	o := NewOrchestrator(s, &MockProvider{}, delivery, summarizer, notify, noopEdit, time.Second, time.Hour, 10*time.Minute, 3, 180)

	// Tick 1: Fetch -> Summarize -> cache the summary -> Deliver FAILS -> the job stays in summarizing.
	o.summarizeJob(context.Background(), job)
	if summarizer.calls != 1 {
		t.Fatalf("after tick 1 Summarize called %d times, want 1", summarizer.calls)
	}
	if status, _, _ := jobRow(t, s, job.ID); status != string(JobSummarizing) {
		t.Fatalf("after Deliver failure status %q, want summarizing", status)
	}
	reloaded := reloadJob(t, s, job.ID)
	if reloaded.SummaryMD == nil || *reloaded.SummaryMD == "" {
		t.Fatal("summary_md is not cached after generation: a retry would regenerate it for money")
	}

	// Tick 2 (cached path): Summarize is NOT repeated, delivery succeeds, the job -> done.
	o.summarizeJob(context.Background(), reloaded)
	if summarizer.calls != 1 {
		t.Errorf("after tick 2 Summarize called %d times: repeated PAID generation! Want 1", summarizer.calls)
	}
	if delivery.calls != 2 {
		t.Errorf("Deliver called %d times, want 2 (failure + success)", delivery.calls)
	}
	if status, _, _ := jobRow(t, s, job.ID); status != string(JobDone) {
		t.Errorf("after successful delivery status %q, want done", status)
	}
}

// joiningJob creates a job and leaves it in joining (the natural state after
// SetJobSubmitted): the starting point for the phased polling flow.
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

// scriptedProvider returns a preset sequence of statuses per Poll call
// (one per tick; after the end of the script it holds the last one). It can model
// both mymeet (OnCall->Transcribing->Done) and s2t (OnCall->...->Done without Transcribing).
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

// recordingEdits collects editStatus calls; chatID is taken from the job's status_message_id.
func newPhaseOrchestrator(s *Storage, p TranscriptionProvider, edits *[]editCall) *Orchestrator {
	notify := func(int64, string) error { return nil }
	edit := func(chatID, msgID int64, text string) error {
		*edits = append(*edits, editCall{chatID, msgID, text})
		return nil
	}
	// summarizer nil -> summary-off: Done finishes the job with the transcript, without summarizing.
	return NewOrchestrator(s, p, &recordingDelivery{}, nil, notify, edit, time.Second, time.Hour, 10*time.Minute, 3, 180)
}

// TestPhaseFlow_Mymeet: the mymeet path: joining -> recording -> processing -> done.
// On each of the three transitions the status message is edited EXACTLY once (moved=true),
// the texts differ by phase. The Done tick edits the status to "done" (the 4th edit).
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

	// Tick 1: OnCall -> recording.
	o.advance(context.Background(), reloadJob(t, s, job.ID))
	if st, _, _ := jobRow(t, s, job.ID); st != string(JobRecording) {
		t.Fatalf("after tick 1 status %q, want recording", st)
	}
	if len(edits) != 1 {
		t.Fatalf("after tick 1 edits %d, want 1 (recording)", len(edits))
	}

	// Tick 2: Transcribing -> processing.
	o.advance(context.Background(), reloadJob(t, s, job.ID))
	if st, _, _ := jobRow(t, s, job.ID); st != string(JobProcessing) {
		t.Fatalf("after tick 2 status %q, want processing", st)
	}
	if len(edits) != 2 {
		t.Fatalf("after tick 2 edits %d, want 2 (recording, processing)", len(edits))
	}

	// Tick 3: Done -> done (the transcript is delivered, status "done").
	o.advance(context.Background(), reloadJob(t, s, job.ID))
	if st, _, _ := jobRow(t, s, job.ID); st != string(JobDone) {
		t.Fatalf("after tick 3 status %q, want done", st)
	}
	if len(edits) != 3 {
		t.Fatalf("after tick 3 edits %d, want 3 (recording, processing, done)", len(edits))
	}

	// Each edit goes to the right message and with phase-distinguishable text.
	for i, e := range edits {
		if e.chatID != userID || e.msgID != 700 {
			t.Errorf("edit %d at (%d,%d), want (%d,700)", i, e.chatID, e.msgID, userID)
		}
	}
	if edits[0].text == edits[1].text || edits[1].text == edits[2].text || edits[0].text == edits[2].text {
		t.Errorf("edit texts are not distinguishable by phase: %q / %q / %q", edits[0].text, edits[1].text, edits[2].text)
	}
}

// TestPhaseFlow_S2T: the s2t path: joining -> recording -> processing -> done.
// s2t does not return a separate Transcribing, so processing is recorded right before Done,
// to close the billing window.
func TestPhaseFlow_S2T(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job := joiningJob(t, s, userID, "ext-phase-s2t")
	setJobProvider(t, s, job.ID, "speech2text")
	if err := s.SetJobStatusMessage(context.Background(), job.ID, 701); err != nil {
		t.Fatalf("set status message: %v", err)
	}

	var edits []editCall
	// OnCall twice (the bot is still recording), then Done: Transcribing never arrives.
	p := &scriptedProvider{statuses: []ProviderStatus{StatusOnCall, StatusOnCall, StatusDone}}
	o := newPhaseOrchestrator(s, p, &edits)

	// Tick 1: OnCall -> recording (1 edit).
	o.advance(context.Background(), reloadJob(t, s, job.ID))
	if st, _, _ := jobRow(t, s, job.ID); st != string(JobRecording) {
		t.Fatalf("after tick 1 status %q, want recording", st)
	}
	if len(edits) != 1 {
		t.Fatalf("after tick 1 edits %d, want 1 (recording)", len(edits))
	}

	// Tick 2: OnCall on an already-recording job: moved=false, NO second edit (idempotency).
	o.advance(context.Background(), reloadJob(t, s, job.ID))
	if st, _, _ := jobRow(t, s, job.ID); st != string(JobRecording) {
		t.Fatalf("after tick 2 status %q, want recording (holds)", st)
	}
	if len(edits) != 1 {
		t.Fatalf("after tick 2 edits %d, want 1 (a repeated OnCall does not edit)", len(edits))
	}

	// Tick 3: Done first records processing/duration, then closes the job.
	o.advance(context.Background(), reloadJob(t, s, job.ID))
	if st, _, _ := jobRow(t, s, job.ID); st != string(JobDone) {
		t.Fatalf("after tick 3 status %q, want done", st)
	}
	if len(edits) != 3 {
		t.Fatalf("after tick 3 edits %d, want 3 (recording, processing, done)", len(edits))
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
		t.Fatalf("after tick 1 status %q, want recording", st)
	}

	o.advance(context.Background(), reloadJob(t, s, job.ID))
	if st, _, _ := jobRow(t, s, job.ID); st != string(JobRecording) {
		t.Fatalf("after tick 2 status %q, want recording: the s2t queue does not mean processing", st)
	}
	if len(edits) != 2 {
		t.Fatalf("after tick 2 edits %d, want 2 (recording, queued)", len(edits))
	}
	if strings.Contains(edits[1].text, "Запись завершилась") || strings.Contains(edits[1].text, "Расшифровываю") {
		t.Fatalf("tick 2 showed a false recording end: %q", edits[1].text)
	}

	o.advance(context.Background(), reloadJob(t, s, job.ID))
	if st, _, _ := jobRow(t, s, job.ID); st != string(JobRecording) {
		t.Fatalf("after tick 3 status %q, want recording: a queue repeat does not move the job", st)
	}
	if len(edits) != 2 {
		t.Fatalf("after the queue repeat edits %d, want 2 (dedupe of identical text)", len(edits))
	}

	o.advance(context.Background(), reloadJob(t, s, job.ID))
	if st, _, _ := jobRow(t, s, job.ID); st != string(JobDone) {
		t.Fatalf("after tick 4 status %q, want done", st)
	}
	if len(edits) != 4 {
		t.Fatalf("status edits %d, want 4 (recording, queued, processing, done)", len(edits))
	}
}

// TestPhaseFlow_OnCallIdempotent: a repeated OnCall on an already-recording job does NOT send
// a second Edit. Guarantee "one edit per actual transition": moved=false when the
// phase is unchanged. An isolated idempotency check, separate from the full flow.
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
		t.Errorf("status %q, want recording", st)
	}
	if len(edits) != 1 {
		t.Errorf("edits %d, want exactly 1 (only the first OnCall edits the status)", len(edits))
	}
}
