package app

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// Tests of manual cancellation (the "My calls" panel) are a critical node: quota refund §5.2
// + ownership guard + idempotency. Storage is real (Postgres from DATABASE_URL).

// TestCancelJob_JoiningTrialRefund: cancelling a joining trial job: status failed,
// the trial reservation is returned, external_job_id is handed back to stop the bot at the provider.
func TestCancelJob_JoiningTrialRefund(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job, err := s.CreateJob(context.Background(), userID, "https://meet.google.com/cancel-join", "mock", 180)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.SetJobSubmitted(context.Background(), job.ID, "ext-cancel-join"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	usedBefore := trialUsed(t, s, userID) // 1 (reservation at CreateJob)

	ext, _, refunded, err := s.CancelJob(context.Background(), userID, job.ID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if !refunded {
		t.Error("refunded=false, expected true")
	}
	if ext == nil || *ext != "ext-cancel-join" {
		t.Errorf("extID=%v, expected ext-cancel-join", ext)
	}
	if used := trialUsed(t, s, userID); used != usedBefore-1 {
		t.Errorf("trial_used=%d, expected %d (refund §5.2)", used, usedBefore-1)
	}
	if st, _, _ := jobRow(t, s, job.ID); st != string(JobFailed) {
		t.Errorf("status=%s, expected failed", st)
	}
}

// TestCancelJob_CreatedNoExtID: cancelling a created job (bot not sent): refund happens,
// extID == nil (nothing to stop at the provider).
func TestCancelJob_CreatedNoExtID(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job, err := s.CreateJob(context.Background(), userID, "https://meet.google.com/cancel-created", "mock", 180)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	usedBefore := trialUsed(t, s, userID)

	ext, _, refunded, err := s.CancelJob(context.Background(), userID, job.ID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if !refunded {
		t.Error("refunded=false, expected true")
	}
	if ext != nil {
		t.Errorf("extID=%v, expected nil (created)", *ext)
	}
	if used := trialUsed(t, s, userID); used != usedBefore-1 {
		t.Errorf("trial_used=%d, expected %d", used, usedBefore-1)
	}
}

func TestMarkJobManualStop_RecordingKeepsReserve(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job, err := s.CreateJob(context.Background(), userID, "https://meet.google.com/manual-stop", "mock", 180)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.SetJobSubmitted(context.Background(), job.ID, "ext-manual-stop"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := s.SetJobRecording(context.Background(), job.ID); err != nil {
		t.Fatalf("recording: %v", err)
	}
	usedBefore := trialUsed(t, s, userID)

	extID, provider, err := s.MarkJobManualStop(context.Background(), userID, job.ID)
	if err != nil {
		t.Fatalf("mark manual stop: %v", err)
	}
	if extID != "ext-manual-stop" {
		t.Fatalf("extID = %q, expected ext-manual-stop", extID)
	}
	if provider != "mock" {
		t.Fatalf("provider = %q, expected mock", provider)
	}
	if used := trialUsed(t, s, userID); used != usedBefore {
		t.Fatalf("trial_used = %d, expected %d (reservation is kept until the result)", used, usedBefore)
	}
	reloaded := reloadJob(t, s, job.ID)
	if reloaded.Status != JobRecording {
		t.Fatalf("status = %q, expected recording before provider stop", reloaded.Status)
	}
	if reloaded.LastError == nil || *reloaded.LastError != jobStopManual {
		t.Fatalf("last_error = %v, expected %q", reloaded.LastError, jobStopManual)
	}
}

// TestCancelJob_DoneNotCancelable: a finished job cannot be cancelled: ErrJobNotCancelable,
// no refund (the meeting took place, the quota was spent legitimately).
func TestCancelJob_DoneNotCancelable(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job, err := s.CreateJob(context.Background(), userID, "https://meet.google.com/cancel-done", "mock", 180)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.SetJobSubmitted(context.Background(), job.ID, "ext-done"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if err := s.SetJobSummarizing(context.Background(), job.ID, []byte(`{"x":1}`), 3600); err != nil {
		t.Fatalf("summarizing: %v", err)
	}
	if err := s.SetJobDone(context.Background(), job.ID, []byte(`{"x":1}`), 3600, 180); err != nil {
		t.Fatalf("done: %v", err)
	}
	usedBefore := trialUsed(t, s, userID)

	_, _, refunded, err := s.CancelJob(context.Background(), userID, job.ID)
	if !errors.Is(err, ErrJobNotCancelable) {
		t.Errorf("err=%v, expected ErrJobNotCancelable", err)
	}
	if refunded {
		t.Error("refunded=true on done")
	}
	if used := trialUsed(t, s, userID); used != usedBefore {
		t.Errorf("trial_used changed: %d != %d (refund on a terminal job)", used, usedBefore)
	}
}

// TestCancelJob_OtherUserDenied: another user's job cannot be cancelled: ErrJobNotCancelable,
// the owner's reservation is untouched.
func TestCancelJob_OtherUserDenied(t *testing.T) {
	s := newTestStorage(t)
	owner := newTestUser(t, s)
	other := newTestUser(t, s)
	job, err := s.CreateJob(context.Background(), owner, "https://meet.google.com/cancel-owner", "mock", 180)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	usedBefore := trialUsed(t, s, owner)

	_, _, refunded, err := s.CancelJob(context.Background(), other, job.ID)
	if !errors.Is(err, ErrJobNotCancelable) {
		t.Errorf("err=%v, expected ErrJobNotCancelable", err)
	}
	if refunded {
		t.Error("refunded=true on another user's job")
	}
	if used := trialUsed(t, s, owner); used != usedBefore {
		t.Errorf("owner's reservation touched: %d != %d", used, usedBefore)
	}
}

// TestCancelJob_Idempotent: repeated cancel: the second time ErrJobNotCancelable,
// the reservation is returned exactly once (not twice).
func TestCancelJob_Idempotent(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job, err := s.CreateJob(context.Background(), userID, "https://meet.google.com/cancel-idem", "mock", 180)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, _, err := s.CancelJob(context.Background(), userID, job.ID); err != nil {
		t.Fatalf("first cancel: %v", err)
	}
	usedAfterFirst := trialUsed(t, s, userID)

	_, _, refunded, err := s.CancelJob(context.Background(), userID, job.ID)
	if !errors.Is(err, ErrJobNotCancelable) {
		t.Errorf("second cancel err=%v, expected ErrJobNotCancelable", err)
	}
	if refunded {
		t.Error("second refund on repeated cancel")
	}
	if used := trialUsed(t, s, userID); used != usedAfterFirst {
		t.Errorf("trial_used after second cancel=%d, expected %d (double refund)", used, usedAfterFirst)
	}
}

// TestCancelJob_SubscriptionRefund: cancelling a subscription job returns the slot to the subscription
// balance (symmetry of §5.2 for a subscription, not a trial).
func TestCancelJob_SubscriptionRefund(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	subID := insertSubscription(t, s, userID, 5, 15)
	job, err := s.CreateJob(context.Background(), userID, "https://meet.google.com/cancel-sub", "mock", 180)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := subBalance(t, s, subID); got != 4 {
		t.Fatalf("balance after reservation=%d, expected 4", got)
	}
	if got := subMinutesBalance(t, s, subID); got != 240 {
		t.Fatalf("minutes_balance after reservation=%d, expected 240 (300-60)", got)
	}
	if _, _, _, err := s.CancelJob(context.Background(), userID, job.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got := subBalance(t, s, subID); got != 5 {
		t.Errorf("balance after cancel=%d, expected 5 (returned to subscription)", got)
	}
	if got := subMinutesBalance(t, s, subID); got != 300 {
		t.Errorf("minutes_balance after cancel=%d, expected 300 (minutes returned)", got)
	}
}

// TestListUserJobs_RecentFirstOwnerScoped: a user's call list: newest first,
// other users' jobs are not visible. We cancel the first job to check history with a terminal Job.
func TestListUserJobs_RecentFirstOwnerScoped(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	other := newTestUser(t, s)

	j1, err := s.CreateJob(context.Background(), userID, "https://meet.google.com/list-1", "mock", 180)
	if err != nil {
		t.Fatalf("create j1: %v", err)
	}
	if _, _, _, err := s.CancelJob(context.Background(), userID, j1.ID); err != nil {
		t.Fatalf("cancel j1: %v", err)
	}
	j2, err := s.CreateJob(context.Background(), userID, "https://meet.google.com/list-2", "mock", 180)
	if err != nil {
		t.Fatalf("create j2: %v", err)
	}
	if _, err := s.CreateJob(context.Background(), other, "https://meet.google.com/list-other", "mock", 180); err != nil {
		t.Fatalf("create other: %v", err)
	}

	jobs, err := s.ListUserJobs(context.Background(), userID, time.Now().Add(-24*time.Hour), 10, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("got %d jobs, expected 2 (owner's only)", len(jobs))
	}
	if jobs[0].ID != j2.ID || jobs[1].ID != j1.ID {
		t.Errorf("order [%d,%d], expected [%d,%d] (newest first)", jobs[0].ID, jobs[1].ID, j2.ID, j1.ID)
	}
	for _, j := range jobs {
		if j.UserID != userID {
			t.Errorf("another user's job in the result user_id=%d", j.UserID)
		}
	}
}

// TestListUserJobs_Pagination: limit/offset page through history, newest first.
// We cancel between creations so that the pages include a terminal Job.
func TestListUserJobs_Pagination(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	since := time.Now().Add(-24 * time.Hour)

	var ids []int64
	for i := 0; i < 3; i++ {
		j, err := s.CreateJob(context.Background(), userID, fmt.Sprintf("https://meet.google.com/page-%d", i), "mock", 180)
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		if _, _, _, err := s.CancelJob(context.Background(), userID, j.ID); err != nil {
			t.Fatalf("cancel %d: %v", i, err)
		}
		ids = append(ids, j.ID) // ids[0] is the oldest, ids[2] the newest
	}

	// Page 0 (limit 2): the two newest.
	page0, err := s.ListUserJobs(context.Background(), userID, since, 2, 0)
	if err != nil {
		t.Fatalf("page0: %v", err)
	}
	if len(page0) != 2 || page0[0].ID != ids[2] || page0[1].ID != ids[1] {
		t.Fatalf("page0=%v, expected [%d,%d]", jobIDs(page0), ids[2], ids[1])
	}
	// Page 1 (offset 2): the oldest.
	page1, err := s.ListUserJobs(context.Background(), userID, since, 2, 2)
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1) != 1 || page1[0].ID != ids[0] {
		t.Fatalf("page1=%v, expected [%d]", jobIDs(page1), ids[0])
	}
}

func jobIDs(jobs []Job) []int64 {
	ids := make([]int64, len(jobs))
	for i, j := range jobs {
		ids[i] = j.ID
	}
	return ids
}

// TestJobProvider_RoundTrip: a job remembers its creating provider (migration 00008):
// CreateJob writes provider, UserJob and ListUserJobs read it. This is the resolve contract
// in the "My calls" panel: redelivery/cancel go to the job.Provider provider,
// so after TRANSCRIPTION_PROVIDER changes a foreign external_job_id is not sent to the active one.
func TestJobProvider_RoundTrip(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job, err := s.CreateJob(context.Background(), userID, "https://meet.google.com/prov", "speech2text", 180)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if job.Provider != "speech2text" {
		t.Errorf("CreateJob provider=%q, expected speech2text", job.Provider)
	}
	got, err := s.UserJob(context.Background(), userID, job.ID)
	if err != nil {
		t.Fatalf("user job: %v", err)
	}
	if got.Provider != "speech2text" {
		t.Errorf("UserJob provider=%q, expected speech2text", got.Provider)
	}
	list, err := s.ListUserJobs(context.Background(), userID, time.Now().Add(-time.Hour), 10, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) == 0 || list[0].Provider != "speech2text" {
		t.Errorf("ListUserJobs provider mismatch: %+v", list)
	}
}
