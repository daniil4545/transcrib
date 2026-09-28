package app

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// Тесты ручной отмены (панель «Мои звонки») — критический узел: рефанд квоты §5.2
// + ownership-guard + идемпотентность. Storage настоящий (Postgres из DATABASE_URL).

// TestCancelJob_JoiningTrialRefund — отмена joining-триал-джобы: статус failed,
// триал-резерв возвращён, отдан external_job_id для остановки бота у поставщика.
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
	usedBefore := trialUsed(t, s, userID) // 1 (резерв при CreateJob)

	ext, _, refunded, err := s.CancelJob(context.Background(), userID, job.ID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if !refunded {
		t.Error("refunded=false, ожидали true")
	}
	if ext == nil || *ext != "ext-cancel-join" {
		t.Errorf("extID=%v, ожидали ext-cancel-join", ext)
	}
	if used := trialUsed(t, s, userID); used != usedBefore-1 {
		t.Errorf("trial_used=%d, ожидали %d (рефанд §5.2)", used, usedBefore-1)
	}
	if st, _, _ := jobRow(t, s, job.ID); st != string(JobFailed) {
		t.Errorf("статус=%s, ожидали failed", st)
	}
}

// TestCancelJob_CreatedNoExtID — отмена created-джобы (бот не отправлен): рефанд есть,
// extID == nil (нечего останавливать у поставщика).
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
		t.Error("refunded=false, ожидали true")
	}
	if ext != nil {
		t.Errorf("extID=%v, ожидали nil (created)", *ext)
	}
	if used := trialUsed(t, s, userID); used != usedBefore-1 {
		t.Errorf("trial_used=%d, ожидали %d", used, usedBefore-1)
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
		t.Fatalf("extID = %q, ожидали ext-manual-stop", extID)
	}
	if provider != "mock" {
		t.Fatalf("provider = %q, ожидали mock", provider)
	}
	if used := trialUsed(t, s, userID); used != usedBefore {
		t.Fatalf("trial_used = %d, ожидали %d (резерв сохраняется до результата)", used, usedBefore)
	}
	reloaded := reloadJob(t, s, job.ID)
	if reloaded.Status != JobRecording {
		t.Fatalf("status = %q, ожидали recording до provider stop", reloaded.Status)
	}
	if reloaded.LastError == nil || *reloaded.LastError != jobStopManual {
		t.Fatalf("last_error = %v, ожидали %q", reloaded.LastError, jobStopManual)
	}
}

// TestCancelJob_DoneNotCancelable — готовую джобу отменить нельзя: ErrJobNotCancelable,
// рефанда нет (встреча состоялась, квота списана законно).
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
		t.Errorf("err=%v, ожидали ErrJobNotCancelable", err)
	}
	if refunded {
		t.Error("refunded=true на done")
	}
	if used := trialUsed(t, s, userID); used != usedBefore {
		t.Errorf("trial_used изменился: %d != %d (рефанд на терминальной джобе)", used, usedBefore)
	}
}

// TestCancelJob_OtherUserDenied — чужую джобу отменить нельзя: ErrJobNotCancelable,
// резерв владельца не тронут.
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
		t.Errorf("err=%v, ожидали ErrJobNotCancelable", err)
	}
	if refunded {
		t.Error("refunded=true на чужой джобе")
	}
	if used := trialUsed(t, s, owner); used != usedBefore {
		t.Errorf("резерв владельца тронут: %d != %d", used, usedBefore)
	}
}

// TestCancelJob_Idempotent — повторная отмена: второй раз ErrJobNotCancelable,
// резерв возвращается ровно один раз (не дважды).
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
		t.Errorf("second cancel err=%v, ожидали ErrJobNotCancelable", err)
	}
	if refunded {
		t.Error("второй рефанд на повторной отмене")
	}
	if used := trialUsed(t, s, userID); used != usedAfterFirst {
		t.Errorf("trial_used после второй отмены=%d, ожидали %d (двойной возврат)", used, usedAfterFirst)
	}
}

// TestCancelJob_SubscriptionRefund — отмена подписочной джобы возвращает слот на баланс
// подписки (симметрия §5.2 для подписки, не триала).
func TestCancelJob_SubscriptionRefund(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	subID := insertSubscription(t, s, userID, 5, 15)
	job, err := s.CreateJob(context.Background(), userID, "https://meet.google.com/cancel-sub", "mock", 180)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := subBalance(t, s, subID); got != 4 {
		t.Fatalf("баланс после резерва=%d, ожидали 4", got)
	}
	if got := subMinutesBalance(t, s, subID); got != 240 {
		t.Fatalf("minutes_balance после резерва=%d, ожидали 240 (300-60)", got)
	}
	if _, _, _, err := s.CancelJob(context.Background(), userID, job.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got := subBalance(t, s, subID); got != 5 {
		t.Errorf("баланс после отмены=%d, ожидали 5 (возврат на подписку)", got)
	}
	if got := subMinutesBalance(t, s, subID); got != 300 {
		t.Errorf("minutes_balance после отмены=%d, ожидали 300 (возврат минут)", got)
	}
}

// TestListUserJobs_RecentFirstOwnerScoped — список звонков юзера: новые первыми,
// чужие не видны. Первую джобу отменяем, чтобы проверить историю с terminal Job.
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
		t.Fatalf("получили %d джоб, ожидали 2 (только владельца)", len(jobs))
	}
	if jobs[0].ID != j2.ID || jobs[1].ID != j1.ID {
		t.Errorf("порядок [%d,%d], ожидали [%d,%d] (новые первыми)", jobs[0].ID, jobs[1].ID, j2.ID, j1.ID)
	}
	for _, j := range jobs {
		if j.UserID != userID {
			t.Errorf("в выдаче чужая джоба user_id=%d", j.UserID)
		}
	}
}

// TestListUserJobs_Pagination — limit/offset листают историю страницами, новые первыми.
// Между созданиями отменяем, чтобы страницы включали terminal Job.
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
		ids = append(ids, j.ID) // ids[0] старейшая, ids[2] новейшая
	}

	// Страница 0 (limit 2): две новейшие.
	page0, err := s.ListUserJobs(context.Background(), userID, since, 2, 0)
	if err != nil {
		t.Fatalf("page0: %v", err)
	}
	if len(page0) != 2 || page0[0].ID != ids[2] || page0[1].ID != ids[1] {
		t.Fatalf("page0=%v, ожидали [%d,%d]", jobIDs(page0), ids[2], ids[1])
	}
	// Страница 1 (offset 2): старейшая.
	page1, err := s.ListUserJobs(context.Background(), userID, since, 2, 2)
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1) != 1 || page1[0].ID != ids[0] {
		t.Fatalf("page1=%v, ожидали [%d]", jobIDs(page1), ids[0])
	}
}

func jobIDs(jobs []Job) []int64 {
	ids := make([]int64, len(jobs))
	for i, j := range jobs {
		ids[i] = j.ID
	}
	return ids
}

// TestJobProvider_RoundTrip — джоба помнит поставщика-создателя (миграция 00008):
// CreateJob пишет provider, UserJob и ListUserJobs его читают. Это контракт резолва
// в панели «Мои звонки»: повторная доставка/отмена идут к провайдеру job.Provider,
// поэтому после смены TRANSCRIPTION_PROVIDER чужой external_job_id не уходит активному.
func TestJobProvider_RoundTrip(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job, err := s.CreateJob(context.Background(), userID, "https://meet.google.com/prov", "speech2text", 180)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if job.Provider != "speech2text" {
		t.Errorf("CreateJob provider=%q, ожидали speech2text", job.Provider)
	}
	got, err := s.UserJob(context.Background(), userID, job.ID)
	if err != nil {
		t.Fatalf("user job: %v", err)
	}
	if got.Provider != "speech2text" {
		t.Errorf("UserJob provider=%q, ожидали speech2text", got.Provider)
	}
	list, err := s.ListUserJobs(context.Background(), userID, time.Now().Add(-time.Hour), 10, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) == 0 || list[0].Provider != "speech2text" {
		t.Errorf("ListUserJobs provider mismatch: %+v", list)
	}
}
