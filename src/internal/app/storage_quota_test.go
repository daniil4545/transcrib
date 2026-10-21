package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// Critical money nodes (db.md §5.1 reserve, §5.2 rollback, §5.7 done counter,
// §5.8 parallel calls; docs/testing.md). Targeted tests for money/quota/race/
// idempotency invariants; the glue happy path is not included here (manual E2E). Integration tests
// that hit a real Postgres via DATABASE_URL; without it they Skip (see newTestStorage).

// insertSubscription inserts an active subscription row via a direct INSERT (1:1 model,
// db.md §3.3). Creating a subscription through successful_payment (§5.4) is a separate node
// and is not tested here; we only need a ready paid quota to check the reserve.
// No separate cleanup needed: subscriptions has FK ON DELETE CASCADE to users, and
// newTestUser already deletes jobs (ON DELETE RESTRICT) before the user, so the cascade fires.
func insertSubscription(t *testing.T, s *Storage, userID int64, balance, limit int) int64 {
	t.Helper()
	var id int64
	err := s.pool.QueryRow(context.Background(),
		`INSERT INTO subscriptions (
		     user_id, paid_at, expires_at,
		     meetings_limit, meetings_balance, minutes_limit, minutes_balance
		 )
		 VALUES ($1, now(), now() + interval '30 days', $2, $3, $4, $5)
		 RETURNING id`,
		userID, limit, balance, limit*60, balance*60,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert subscription: %v", err)
	}
	return id
}

func TestHasActiveSubscription_IgnoresMeetingsBalance(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	active, err := s.HasActiveSubscription(ctx, userID)
	if err != nil {
		t.Fatalf("has active subscription without sub: %v", err)
	}
	if active {
		t.Fatal("active subscription = true without a subscription")
	}

	insertSubscription(t, s, userID, 0, 15)
	active, err = s.HasActiveSubscription(ctx, userID)
	if err != nil {
		t.Fatalf("has active subscription with empty balance: %v", err)
	}
	if !active {
		t.Fatal("active subscription = false for a paid period with a zero balance")
	}
}

// trialUsed reads users.trial_meetings_used directly.
func trialUsed(t *testing.T, s *Storage, userID int64) int {
	t.Helper()
	var used int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT trial_meetings_used FROM users WHERE telegram_id = $1`, userID,
	).Scan(&used); err != nil {
		t.Fatalf("read trial_meetings_used: %v", err)
	}
	return used
}

func trialMinutesUsed(t *testing.T, s *Storage, userID int64) int {
	t.Helper()
	var used int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT trial_minutes_used FROM users WHERE telegram_id = $1`, userID,
	).Scan(&used); err != nil {
		t.Fatalf("read trial_minutes_used: %v", err)
	}
	return used
}

func trialMinutesDone(t *testing.T, s *Storage, userID int64) int {
	t.Helper()
	var done int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT trial_minutes_done FROM users WHERE telegram_id = $1`, userID,
	).Scan(&done); err != nil {
		t.Fatalf("read trial_minutes_done: %v", err)
	}
	return done
}

// trialDone reads users.trial_meetings_done directly.
func trialDone(t *testing.T, s *Storage, userID int64) int {
	t.Helper()
	var done int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT trial_meetings_done FROM users WHERE telegram_id = $1`, userID,
	).Scan(&done); err != nil {
		t.Fatalf("read trial_meetings_done: %v", err)
	}
	return done
}

// subBalance reads the subscription meetings_balance directly.
func subBalance(t *testing.T, s *Storage, subID int64) int {
	t.Helper()
	var bal int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT meetings_balance FROM subscriptions WHERE id = $1`, subID,
	).Scan(&bal); err != nil {
		t.Fatalf("read meetings_balance: %v", err)
	}
	return bal
}

func subMinutesBalance(t *testing.T, s *Storage, subID int64) int {
	t.Helper()
	var bal int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT minutes_balance FROM subscriptions WHERE id = $1`, subID,
	).Scan(&bal); err != nil {
		t.Fatalf("read minutes_balance: %v", err)
	}
	return bal
}

// setTrialUsed sets the initial trial usage (as if the reserve was already made earlier),
// to put the user at the limit boundary without running extra CreateJob calls.
func setTrialUsed(t *testing.T, s *Storage, userID, used int64) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE users
		 SET trial_meetings_used = $2,
		     trial_minutes_used = $2 * 60
		 WHERE telegram_id = $1`,
		userID, used,
	); err != nil {
		t.Fatalf("set trial_meetings_used: %v", err)
	}
}

func jobReservedMinutes(t *testing.T, s *Storage, jobID int64) int {
	t.Helper()
	var reserved int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT reserved_minutes FROM jobs WHERE id = $1`, jobID,
	).Scan(&reserved); err != nil {
		t.Fatalf("read reserved_minutes: %v", err)
	}
	return reserved
}

func jobBilling(t *testing.T, s *Storage, jobID int64) (billed int, duration *int) {
	t.Helper()
	if err := s.pool.QueryRow(context.Background(),
		`SELECT billed_minutes, duration_seconds FROM jobs WHERE id = $1`, jobID,
	).Scan(&billed, &duration); err != nil {
		t.Fatalf("read job billing: %v", err)
	}
	return billed, duration
}

// jobQuota reads is_trial/subscription_id of a job to check the direction of the reserve.
func jobQuota(t *testing.T, s *Storage, jobID int64) (isTrial bool, subID *int64) {
	t.Helper()
	if err := s.pool.QueryRow(context.Background(),
		`SELECT is_trial, subscription_id FROM jobs WHERE id = $1`, jobID,
	).Scan(&isTrial, &subID); err != nil {
		t.Fatalf("read job quota: %v", err)
	}
	return isTrial, subID
}

func TestBilledMinutes(t *testing.T) {
	cases := []struct {
		seconds int
		want    int
	}{
		{-1, 0},
		{0, 0},
		{1, 1},
		{59, 1},
		{60, 1},
		{61, 2},
		{119, 2},
		{120, 2},
	}
	for _, tc := range cases {
		if got := billedMinutes(tc.seconds); got != tc.want {
			t.Errorf("billedMinutes(%d) = %d, expected %d", tc.seconds, got, tc.want)
		}
	}
}

// TestCreateJob_TrialRace: the trial reserve §5.1 under the "two messages within 200ms" race.
// The user is at the boundary: trialLimit=2, used=1, exactly ONE slot is left. K goroutines hit
// CreateJob with a synchronous start. Money invariant: exactly one Job is created, all
// others get ErrNoQuota, and trial_meetings_used == limit (not more: the atomic
// UPDATE ... WHERE trial_meetings_used < limit physically closes the race, otherwise the
// counter would exceed the limit = a free meeting granted beyond the limit).
// Under -race. docs/testing.md / db.md §5.1.
func TestCreateJob_TrialRace(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	const (
		trialLimit = 2
		goroutines = 8
	)
	setTrialUsed(t, s, userID, trialLimit-1) // one slot left

	var (
		mu        sync.Mutex
		okCount   int
		noQuota   int
		otherErrs []error
		start     = make(chan struct{})
		wg        sync.WaitGroup
	)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			<-start
			// Different meeting_url: the dedup index must not mask the reserve race.
			_, err := s.CreateJob(ctx, userID, fmt.Sprintf("https://meet.google.com/trial-race-%d", g), "mock", trialLimit*60)
			mu.Lock()
			switch {
			case err == nil:
				okCount++
			case errors.Is(err, ErrNoQuota):
				noQuota++
			default:
				otherErrs = append(otherErrs, err)
			}
			mu.Unlock()
		}(g)
	}
	close(start)
	wg.Wait()

	for _, e := range otherErrs {
		t.Errorf("unexpected CreateJob error: %v", e)
	}
	if okCount != 1 {
		t.Errorf("Jobs created = %d, expected exactly 1 (trial reserve race)", okCount)
	}
	if noQuota != goroutines-1 {
		t.Errorf("ErrNoQuota = %d, expected %d", noQuota, goroutines-1)
	}
	if got := trialUsed(t, s, userID); got != trialLimit {
		t.Errorf("trial_meetings_used = %d, expected %d (a free meeting was granted beyond the limit)", got, trialLimit)
	}
}

// TestCreateJob_SubscriptionRace: the subscription reserve §5.1 under a race. A subscription with
// meetings_balance=1; two parallel CreateJob calls. The point of the test is FOR UPDATE on the
// SELECT ... FOR UPDATE subquery: without it both queries would pick the same row and
// debit the balance twice (down to -1, which CHECK >= 0 would also catch, but the invariant is exactly
// "exactly one debit"). Invariant: exactly one Job against the subscription, balance==0.
// Under -race. db.md §5.1.
func TestCreateJob_SubscriptionRace(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	setTrialUsed(t, s, userID, 3)
	subID := insertSubscription(t, s, userID, 1, 15) // exactly one paid slot

	const goroutines = 2
	var (
		mu        sync.Mutex
		okCount   int
		noQuota   int
		otherErrs []error
		start     = make(chan struct{})
		wg        sync.WaitGroup
	)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			<-start
			_, err := s.CreateJob(ctx, userID, fmt.Sprintf("https://meet.google.com/sub-race-%d", g), "mock", 180)
			mu.Lock()
			switch {
			case err == nil:
				okCount++
			case errors.Is(err, ErrNoQuota):
				noQuota++
			default:
				otherErrs = append(otherErrs, err)
			}
			mu.Unlock()
		}(g)
	}
	close(start)
	wg.Wait()

	for _, e := range otherErrs {
		t.Errorf("unexpected CreateJob error: %v", e)
	}
	if okCount != 1 {
		t.Errorf("Jobs created = %d, expected exactly 1 (subscription reserve race)", okCount)
	}
	if noQuota != goroutines-1 {
		t.Errorf("ErrNoQuota = %d, expected %d", noQuota, goroutines-1)
	}
	if got := subBalance(t, s, subID); got != 0 {
		t.Errorf("meetings_balance = %d, expected 0 (double debit of one subscription)", got)
	}
}

// TestCreateJob_SubscriptionPriorityOverTrial: reserve §5.1: with an active subscription
// that has a remainder the paid quota is spent FIRST (the trial is untouched). Invariant:
// the Job is created with is_trial=false and a filled subscription_id, balance-1,
// trial_meetings_used does NOT grow (otherwise the user would "lose" a paid meeting for nothing).
func TestCreateJob_SubscriptionPriorityOverTrial(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	subID := insertSubscription(t, s, userID, 5, 15)

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/sub-priority", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	if job.IsTrial {
		t.Error("Job.IsTrial = true with an active subscription, expected a subscription reserve")
	}
	gotTrial, gotSub := jobQuota(t, s, job.ID)
	if gotTrial {
		t.Error("is_trial = true in the DB with an active subscription")
	}
	if gotSub == nil || *gotSub != subID {
		t.Errorf("subscription_id = %v, expected %d", gotSub, subID)
	}
	if got := subBalance(t, s, subID); got != 4 {
		t.Errorf("meetings_balance = %d, expected 4 (debited from the subscription)", got)
	}
	if got := trialUsed(t, s, userID); got != 0 {
		t.Errorf("trial_meetings_used = %d, expected 0 (the trial must not be touched)", got)
	}
}

func TestCreateJob_TrialPartialMinuteReserve(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	setTrialUsed(t, s, userID, 2) // 120 minutes out of 150 are already taken

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/trial-partial-minutes", "mock", 150)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if !job.IsTrial {
		t.Fatal("expected a trial Job")
	}
	if got := jobReservedMinutes(t, s, job.ID); got != 30 {
		t.Errorf("reserved_minutes = %d, expected 30", got)
	}
	if got := trialMinutesUsed(t, s, userID); got != 150 {
		t.Errorf("trial_minutes_used = %d, expected 150", got)
	}
}

func TestCreateJob_SubscriptionPartialMinuteReserveAndRefund(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	subID := insertSubscription(t, s, userID, 1, 15)
	if _, err := s.pool.Exec(ctx,
		`UPDATE subscriptions SET minutes_balance = 30 WHERE id = $1`,
		subID,
	); err != nil {
		t.Fatalf("set subscription minutes_balance: %v", err)
	}

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/sub-partial-minutes", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if job.IsTrial {
		t.Fatal("expected a subscription Job")
	}
	if got := jobReservedMinutes(t, s, job.ID); got != 30 {
		t.Fatalf("reserved_minutes = %d, expected 30", got)
	}
	if got := subMinutesBalance(t, s, subID); got != 0 {
		t.Fatalf("minutes_balance after reserve = %d, expected 0", got)
	}

	refunded, err := s.FailJobWithQuotaRefund(ctx, job.ID, "provider failed")
	if err != nil {
		t.Fatalf("FailJobWithQuotaRefund: %v", err)
	}
	if !refunded {
		t.Fatal("refunded=false, expected true")
	}
	if got := subMinutesBalance(t, s, subID); got != 30 {
		t.Errorf("minutes_balance after rollback = %d, expected 30", got)
	}
}

func TestCreateJob_SelectsFullSubscriptionBeforeOlderPartial(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	oldSubID := insertSubscription(t, s, userID, 1, 15)
	if _, err := s.pool.Exec(ctx,
		`UPDATE subscriptions
		 SET expires_at = now() + INTERVAL '10 days',
		     minutes_balance = 10
		 WHERE id = $1`,
		oldSubID,
	); err != nil {
		t.Fatalf("set old subscription partial minutes: %v", err)
	}
	newSubID := insertSubscription(t, s, userID, 15, 15)

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/sub-full-before-partial", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if job.IsTrial {
		t.Fatal("expected a subscription Job")
	}
	_, gotSubID := jobQuota(t, s, job.ID)
	if gotSubID == nil || *gotSubID != newSubID {
		t.Fatalf("subscription_id = %v, expected the new full subscription %d", gotSubID, newSubID)
	}
	if got := jobReservedMinutes(t, s, job.ID); got != 60 {
		t.Fatalf("reserved_minutes = %d, expected 60", got)
	}
	if got := subMinutesBalance(t, s, oldSubID); got != 10 {
		t.Fatalf("old minutes_balance = %d, expected 10", got)
	}
	if got := subMinutesBalance(t, s, newSubID); got != 840 {
		t.Fatalf("new minutes_balance after reserve = %d, expected 840", got)
	}

	if err := s.SetJobSubmitted(ctx, job.ID, "ext-sub-full-before-partial"); err != nil {
		t.Fatalf("SetJobSubmitted: %v", err)
	}
	if err := s.SetJobDone(ctx, job.ID, []byte(`{"transcript":"ok"}`), 50*60, 180); err != nil {
		t.Fatalf("SetJobDone: %v", err)
	}
	if got := subMinutesBalance(t, s, oldSubID); got != 10 {
		t.Fatalf("old minutes_balance after done = %d, expected 10", got)
	}
	if got := subMinutesBalance(t, s, newSubID); got != 850 {
		t.Fatalf("new minutes_balance after done = %d, expected 850", got)
	}
	billed, _ := jobBilling(t, s, job.ID)
	if billed != 50 {
		t.Fatalf("billed_minutes = %d, expected 50", billed)
	}
}

// TestFailJobWithQuotaRefund_TrialDoubleTick: rollback §5.2 under a double tick (status
// guard). A trial Job in processing. First FailJobWithQuotaRefund: refunded=
// true, trial_meetings_used rolled back by 1. Second (the Job is already failed): refunded=false,
// the counter does NOT change. Without the guard the second tick would return the quota twice (lost update,
// db.md §5.2). Money invariant: the quota is returned exactly once.
func TestFailJobWithQuotaRefund_TrialDoubleTick(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/trial-refund", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if got := trialUsed(t, s, userID); got != 1 {
		t.Fatalf("trial_meetings_used after reserve = %d, expected 1", got)
	}
	if err := s.SetJobSubmitted(ctx, job.ID, "ext-trial-refund"); err != nil {
		t.Fatalf("SetJobSubmitted: %v", err)
	}

	refunded, err := s.FailJobWithQuotaRefund(ctx, job.ID, "provider failed")
	if err != nil {
		t.Fatalf("FailJobWithQuotaRefund #1: %v", err)
	}
	if !refunded {
		t.Error("first rollback: refunded = false, expected true")
	}
	if got := trialUsed(t, s, userID); got != 0 {
		t.Errorf("trial_meetings_used after rollback = %d, expected 0", got)
	}

	refunded, err = s.FailJobWithQuotaRefund(ctx, job.ID, "provider failed again")
	if err != nil {
		t.Fatalf("FailJobWithQuotaRefund #2: %v", err)
	}
	if refunded {
		t.Error("second rollback: refunded = true, expected false (status guard)")
	}
	if got := trialUsed(t, s, userID); got != 0 {
		t.Errorf("trial_meetings_used after double rollback = %d, expected 0 (quota returned twice)", got)
	}
}

func TestFailJobWithQuotaRefund_FromCreated(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/created-refund", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if got := trialUsed(t, s, userID); got != 1 {
		t.Fatalf("trial_meetings_used after reserve = %d, expected 1", got)
	}

	refunded, err := s.FailJobWithQuotaRefund(ctx, job.ID, "submit failed")
	if err != nil {
		t.Fatalf("FailJobWithQuotaRefund: %v", err)
	}
	if !refunded {
		t.Error("refunded = false, expected true (rollback from created)")
	}
	if got := trialUsed(t, s, userID); got != 0 {
		t.Errorf("trial_meetings_used after rollback = %d, expected 0", got)
	}
	if got := jobStatus(t, s, job.ID); got != string(JobFailed) {
		t.Errorf("status = %q, expected %q", got, JobFailed)
	}
}

// TestFailJobWithQuotaRefund_SubscriptionSymmetry: rollback §5.2 for a subscription:
// a subscription Job (is_trial=false) in processing -> rollback -> meetings_balance +1.
// Symmetry with the trial: same behavior, different refund direction.
func TestFailJobWithQuotaRefund_SubscriptionSymmetry(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	subID := insertSubscription(t, s, userID, 5, 15)

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/sub-refund", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if job.IsTrial {
		t.Fatal("expected a subscription Job (is_trial=false)")
	}
	if got := subBalance(t, s, subID); got != 4 {
		t.Fatalf("meetings_balance after reserve = %d, expected 4", got)
	}
	if err := s.SetJobSubmitted(ctx, job.ID, "ext-sub-refund"); err != nil {
		t.Fatalf("SetJobSubmitted: %v", err)
	}

	refunded, err := s.FailJobWithQuotaRefund(ctx, job.ID, "provider failed")
	if err != nil {
		t.Fatalf("FailJobWithQuotaRefund: %v", err)
	}
	if !refunded {
		t.Error("refunded = false, expected true")
	}
	if got := subBalance(t, s, subID); got != 5 {
		t.Errorf("meetings_balance after rollback = %d, expected 5 (returned to the subscription)", got)
	}
}

// TestSetJobFailedSummary_NoRefund: §5.2: a summary failure (summarizing -> failed_summary)
// does NOT roll back the reserve for either a trial or a subscription: the transcript is already delivered,
// the meeting is used (symmetry with awaiting_reauth, db.md §5.2/§7.0). We check
// both quota branches: the rollback direction must not fire here at all.
func TestSetJobFailedSummary_NoRefund(t *testing.T) {
	ctx := context.Background()

	t.Run("trial", func(t *testing.T) {
		s := newTestStorage(t)
		userID := newTestUser(t, s)
		job, err := s.CreateJob(ctx, userID, "https://meet.google.com/fs-trial", "mock", 180)
		if err != nil {
			t.Fatalf("CreateJob: %v", err)
		}
		if err := s.SetJobSubmitted(ctx, job.ID, "ext-fs-trial"); err != nil {
			t.Fatalf("SetJobSubmitted: %v", err)
		}
		if err := s.SetJobSummarizing(ctx, job.ID, []byte(`{"transcript":"x"}`), 3600); err != nil {
			t.Fatalf("SetJobSummarizing: %v", err)
		}
		if err := s.SetJobFailedSummary(ctx, job.ID, "summarizer timeout", 180); err != nil {
			t.Fatalf("SetJobFailedSummary: %v", err)
		}
		if got := trialUsed(t, s, userID); got != 1 {
			t.Errorf("trial_meetings_used = %d, expected 1 (the reserve is NOT rolled back on failed_summary)", got)
		}
	})

	t.Run("subscription", func(t *testing.T) {
		s := newTestStorage(t)
		userID := newTestUser(t, s)
		subID := insertSubscription(t, s, userID, 5, 15)
		job, err := s.CreateJob(ctx, userID, "https://meet.google.com/fs-sub", "mock", 180)
		if err != nil {
			t.Fatalf("CreateJob: %v", err)
		}
		if err := s.SetJobSubmitted(ctx, job.ID, "ext-fs-sub"); err != nil {
			t.Fatalf("SetJobSubmitted: %v", err)
		}
		if err := s.SetJobSummarizing(ctx, job.ID, []byte(`{"transcript":"x"}`), 3600); err != nil {
			t.Fatalf("SetJobSummarizing: %v", err)
		}
		if err := s.SetJobFailedSummary(ctx, job.ID, "summarizer timeout", 180); err != nil {
			t.Fatalf("SetJobFailedSummary: %v", err)
		}
		if got := subBalance(t, s, subID); got != 4 {
			t.Errorf("meetings_balance = %d, expected 4 (the reserve is NOT rolled back on failed_summary)", got)
		}
	})
}

// TestSetJobDone_TrialCounterIdempotent: done §5.7: a trial Job summarizing->done
// finalizes billed_minutes once and keeps raw_provider_response; a repeated
// SetJobDone does not move the counters a second time.
func TestSetJobDone_TrialCounterIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/done-trial", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.SetJobSubmitted(ctx, job.ID, "ext-done-trial"); err != nil {
		t.Fatalf("SetJobSubmitted: %v", err)
	}
	if err := s.SetJobSummarizing(ctx, job.ID, []byte(`{"transcript":"draft"}`), 3600); err != nil {
		t.Fatalf("SetJobSummarizing: %v", err)
	}

	// One key: JSONB reorders keys, and jsonEqual only Compacts
	// (does not sort), so a multi-key literal would give a false failure on order.
	raw := []byte(`{"transcript":"final"}`)
	if err := s.SetJobDone(ctx, job.ID, raw, 3600, 180); err != nil {
		t.Fatalf("SetJobDone #1: %v", err)
	}
	status, _, gotRaw := jobRow(t, s, job.ID)
	if status != string(JobDone) {
		t.Errorf("status = %q, expected %q", status, JobDone)
	}
	if !jsonEqual(gotRaw, raw) {
		t.Errorf("raw_provider_response = %q, expected equivalent of %q", gotRaw, raw)
	}
	if got := trialDone(t, s, userID); got != 1 {
		t.Errorf("trial_meetings_done after done = %d, expected 1", got)
	}
	// With duration 3600 the final usage equals the reserve: no refund/extra needed.
	if got := trialUsed(t, s, userID); got != 1 {
		t.Errorf("trial_meetings_used after done = %d, expected 1", got)
	}

	// Repeated done (second provider poll): the guard does not let the counter move.
	if err := s.SetJobDone(ctx, job.ID, []byte(`{"transcript":"dup"}`), 3600, 180); err != nil {
		t.Fatalf("SetJobDone #2: %v", err)
	}
	if got := trialDone(t, s, userID); got != 1 {
		t.Errorf("trial_meetings_done after repeated done = %d, expected 1 (status guard)", got)
	}
}

func TestSetJobDone_TrialShortDurationRefundsMinutes(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/done-trial-short", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.SetJobSubmitted(ctx, job.ID, "ext-done-trial-short"); err != nil {
		t.Fatalf("SetJobSubmitted: %v", err)
	}
	if err := s.SetJobDone(ctx, job.ID, []byte(`{"transcript":"short"}`), 10, 180); err != nil {
		t.Fatalf("SetJobDone: %v", err)
	}

	if got := trialMinutesUsed(t, s, userID); got != 1 {
		t.Errorf("trial_minutes_used = %d, expected 1", got)
	}
	if got := trialMinutesDone(t, s, userID); got != 1 {
		t.Errorf("trial_minutes_done = %d, expected 1", got)
	}
	billed, duration := jobBilling(t, s, job.ID)
	if billed != 1 {
		t.Errorf("billed_minutes = %d, expected 1", billed)
	}
	if duration == nil || *duration != 10 {
		t.Errorf("duration_seconds = %v, expected 10", duration)
	}
}

func TestSetJobDone_SubscriptionExtraFromOriginalSource(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	subID := insertSubscription(t, s, userID, 2, 15)

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/done-sub-extra", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.SetJobSubmitted(ctx, job.ID, "ext-done-sub-extra"); err != nil {
		t.Fatalf("SetJobSubmitted: %v", err)
	}
	if err := s.SetJobDone(ctx, job.ID, []byte(`{"transcript":"long"}`), 90*60, 180); err != nil {
		t.Fatalf("SetJobDone: %v", err)
	}

	if got := subMinutesBalance(t, s, subID); got != 30 {
		t.Errorf("minutes_balance = %d, expected 30", got)
	}
	billed, _ := jobBilling(t, s, job.ID)
	if billed != 90 {
		t.Errorf("billed_minutes = %d, expected 90", billed)
	}
}

func TestSetJobDone_SubscriptionExtraNotEnoughNoDebt(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	subID := insertSubscription(t, s, userID, 1, 15)

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/done-sub-no-extra", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.SetJobSubmitted(ctx, job.ID, "ext-done-sub-no-extra"); err != nil {
		t.Fatalf("SetJobSubmitted: %v", err)
	}
	if err := s.SetJobDone(ctx, job.ID, []byte(`{"transcript":"long"}`), 90*60, 180); err != nil {
		t.Fatalf("SetJobDone: %v", err)
	}

	if got := subMinutesBalance(t, s, subID); got != 0 {
		t.Errorf("minutes_balance = %d, expected 0", got)
	}
	billed, _ := jobBilling(t, s, job.ID)
	if billed != 60 {
		t.Errorf("billed_minutes = %d, expected 60 with no debt and no partial extra", billed)
	}
}

func TestSetJobDone_TrialExtraFromOriginalSource(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/done-trial-extra", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.SetJobSubmitted(ctx, job.ID, "ext-done-trial-extra"); err != nil {
		t.Fatalf("SetJobSubmitted: %v", err)
	}
	if err := s.SetJobDone(ctx, job.ID, []byte(`{"transcript":"long"}`), 90*60, 180); err != nil {
		t.Fatalf("SetJobDone: %v", err)
	}

	if got := trialMinutesUsed(t, s, userID); got != 90 {
		t.Errorf("trial_minutes_used = %d, expected 90", got)
	}
	if got := trialMinutesDone(t, s, userID); got != 90 {
		t.Errorf("trial_minutes_done = %d, expected 90", got)
	}
	billed, _ := jobBilling(t, s, job.ID)
	if billed != 90 {
		t.Errorf("billed_minutes = %d, expected 90", billed)
	}
}

func TestSetJobDone_TrialExtraNotEnoughNoDebt(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/done-trial-no-extra", "mock", 60)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.SetJobSubmitted(ctx, job.ID, "ext-done-trial-no-extra"); err != nil {
		t.Fatalf("SetJobSubmitted: %v", err)
	}
	if err := s.SetJobDone(ctx, job.ID, []byte(`{"transcript":"long"}`), 90*60, 60); err != nil {
		t.Fatalf("SetJobDone: %v", err)
	}

	if got := trialMinutesUsed(t, s, userID); got != 60 {
		t.Errorf("trial_minutes_used = %d, expected 60 with no debt", got)
	}
	if got := trialMinutesDone(t, s, userID); got != 60 {
		t.Errorf("trial_minutes_done = %d, expected 60 with no debt", got)
	}
	billed, _ := jobBilling(t, s, job.ID)
	if billed != 60 {
		t.Errorf("billed_minutes = %d, expected 60 with no debt and no partial extra", billed)
	}
}

func TestSetJobFailedSummary_UsesStoredDuration(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/failed-summary-duration", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.SetJobSubmitted(ctx, job.ID, "ext-failed-summary-duration"); err != nil {
		t.Fatalf("SetJobSubmitted: %v", err)
	}
	if err := s.SetJobSummarizing(ctx, job.ID, []byte(`{"transcript":"x"}`), 15*60); err != nil {
		t.Fatalf("SetJobSummarizing: %v", err)
	}
	if err := s.SetJobFailedSummary(ctx, job.ID, "summary failed", 180); err != nil {
		t.Fatalf("SetJobFailedSummary: %v", err)
	}

	if got := trialMinutesUsed(t, s, userID); got != 15 {
		t.Errorf("trial_minutes_used = %d, expected 15", got)
	}
	if got := trialMinutesDone(t, s, userID); got != 15 {
		t.Errorf("trial_minutes_done = %d, expected 15", got)
	}
	billed, duration := jobBilling(t, s, job.ID)
	if billed != 15 {
		t.Errorf("billed_minutes = %d, expected 15", billed)
	}
	if duration == nil || *duration != 15*60 {
		t.Errorf("duration_seconds = %v, expected %d", duration, 15*60)
	}
}

func TestSetJobFailedSummary_Idempotent(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/failed-summary-idem", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.SetJobSubmitted(ctx, job.ID, "ext-failed-summary-idem"); err != nil {
		t.Fatalf("SetJobSubmitted: %v", err)
	}
	if err := s.SetJobSummarizing(ctx, job.ID, []byte(`{"transcript":"x"}`), 15*60); err != nil {
		t.Fatalf("SetJobSummarizing: %v", err)
	}
	if err := s.SetJobFailedSummary(ctx, job.ID, "summary failed", 180); err != nil {
		t.Fatalf("SetJobFailedSummary #1: %v", err)
	}
	if err := s.SetJobFailedSummary(ctx, job.ID, "summary failed again", 180); err != nil {
		t.Fatalf("SetJobFailedSummary #2: %v", err)
	}

	if got := trialMinutesUsed(t, s, userID); got != 15 {
		t.Errorf("trial_minutes_used = %d, expected 15", got)
	}
	if got := trialMinutesDone(t, s, userID); got != 15 {
		t.Errorf("trial_minutes_done = %d, expected 15", got)
	}
	billed, _ := jobBilling(t, s, job.ID)
	if billed != 15 {
		t.Errorf("billed_minutes = %d, expected 15", billed)
	}
}

func TestSetJobDone_SubscriptionDeactivatedNoExtra(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	subID := insertSubscription(t, s, userID, 2, 15)

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/done-sub-deactivated", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.SetJobSubmitted(ctx, job.ID, "ext-done-sub-deactivated"); err != nil {
		t.Fatalf("SetJobSubmitted: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE subscriptions SET expires_at = now() WHERE id = $1`, subID); err != nil {
		t.Fatalf("deactivate subscription: %v", err)
	}
	if err := s.SetJobDone(ctx, job.ID, []byte(`{"transcript":"long"}`), 90*60, 180); err != nil {
		t.Fatalf("SetJobDone: %v", err)
	}

	if got := subMinutesBalance(t, s, subID); got != 60 {
		t.Errorf("minutes_balance = %d, expected 60 (extra is not debited from a deactivated subscription)", got)
	}
	billed, _ := jobBilling(t, s, job.ID)
	if billed != 60 {
		t.Errorf("billed_minutes = %d, expected 60 without extra", billed)
	}
}

func TestSetJobSummarizing_AfterCancelNoop(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/late-summarizing-cancel", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.SetJobSubmitted(ctx, job.ID, "ext-late-summarizing-cancel"); err != nil {
		t.Fatalf("SetJobSubmitted: %v", err)
	}
	if _, _, refunded, err := s.CancelJob(ctx, userID, job.ID); err != nil || !refunded {
		t.Fatalf("CancelJob refunded=%v err=%v", refunded, err)
	}
	if err := s.SetJobSummarizing(ctx, job.ID, []byte(`{"transcript":"late"}`), 15*60); err != nil {
		t.Fatalf("SetJobSummarizing late: %v", err)
	}

	if got := jobStatus(t, s, job.ID); got != string(JobFailed) {
		t.Errorf("status = %q, expected failed", got)
	}
	billed, duration := jobBilling(t, s, job.ID)
	if billed != 0 {
		t.Errorf("billed_minutes = %d, expected 0", billed)
	}
	if duration != nil {
		t.Errorf("duration_seconds = %v, expected nil", duration)
	}
	if got := trialMinutesUsed(t, s, userID); got != 0 {
		t.Errorf("trial_minutes_used = %d, expected 0 after cancel", got)
	}
}

// TestCreateJob_RateLimit: R2a: an active Job no longer blocks another URL.
// The limiters are now the quota and same-URL dedup. The same URL must return
// ErrDuplicateActiveJob, and the trial reserve must be rolled back by a transactional rollback.
func TestCreateJob_RateLimit(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	const firstURL = "https://meet.google.com/rl-first"
	first, err := s.CreateJob(ctx, userID, firstURL, "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob #1: %v", err)
	}

	second, err := s.CreateJob(ctx, userID, "https://meet.google.com/rl-second", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob #2 with a different URL: %v", err)
	}
	if second.ID == 0 {
		t.Fatal("second Job with a different URL not created")
	}
	if got := trialUsed(t, s, userID); got != 2 {
		t.Fatalf("trial_meetings_used = %d, expected 2 after two different URLs", got)
	}

	_, err = s.CreateJob(ctx, userID, firstURL, "mock", 180)
	if !errors.Is(err, ErrDuplicateActiveJob) {
		t.Fatalf("CreateJob duplicate err = %v, expected ErrDuplicateActiveJob", err)
	}
	if got := trialUsed(t, s, userID); got != 2 {
		t.Errorf("trial_meetings_used = %d, expected 2 (duplicate must roll back the reserve)", got)
	}
	if first.ID == second.ID {
		t.Error("two different meetings got the same job ID")
	}
}

// TestCreateJob_DuplicateActiveJobRollsBackSubscriptionReserve: a same-URL duplicate
// must not spend a subscription meeting. The main path rejects before reserve;
// the unique-violation fallback in CreateJob is kept as a safety rollback path.
func TestCreateJob_DuplicateActiveJobRollsBackSubscriptionReserve(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	subID := insertSubscription(t, s, userID, 2, 15)

	const url = "https://meet.google.com/sub-dedup-rollback"
	if _, err := s.CreateJob(ctx, userID, url, "mock", 180); err != nil {
		t.Fatalf("CreateJob #1: %v", err)
	}
	if got := subBalance(t, s, subID); got != 1 {
		t.Fatalf("meetings_balance after the first reserve = %d, expected 1", got)
	}

	_, err := s.CreateJob(ctx, userID, url, "mock", 180)
	if !errors.Is(err, ErrDuplicateActiveJob) {
		t.Fatalf("CreateJob duplicate err = %v, expected ErrDuplicateActiveJob", err)
	}
	if got := subBalance(t, s, subID); got != 1 {
		t.Errorf("meetings_balance = %d, expected 1 (duplicate must roll back the reserve)", got)
	}
}

// TestCreateJob_DuplicateActiveJobWinsOverNoQuota: R2a UX invariant: repeating the same
// link must get ErrDuplicateActiveJob even if no quota is left.
func TestCreateJob_DuplicateActiveJobWinsOverNoQuota(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	const url = "https://meet.google.com/trial-dedup-no-quota"
	if _, err := s.CreateJob(ctx, userID, url, "mock", 60); err != nil {
		t.Fatalf("CreateJob #1: %v", err)
	}
	if got := trialUsed(t, s, userID); got != 1 {
		t.Fatalf("trial_meetings_used = %d, expected 1", got)
	}

	_, err := s.CreateJob(ctx, userID, url, "mock", 60)
	if !errors.Is(err, ErrDuplicateActiveJob) {
		t.Fatalf("CreateJob duplicate exhausted err = %v, expected ErrDuplicateActiveJob", err)
	}
	if got := trialUsed(t, s, userID); got != 1 {
		t.Errorf("trial_meetings_used = %d, expected 1", got)
	}
}

// TestCreateJob_ActiveCap: R2a provider-safety cap: 4 active different URLs
// are allowed, the 5th is rejected before the quota reserve. Duplicate takes priority over cap for UX.
func TestCreateJob_ActiveCap(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	var firstURL string
	for i := 0; i < maxActiveJobsPerUser; i++ {
		url := fmt.Sprintf("https://meet.google.com/cap-%d", i)
		if i == 0 {
			firstURL = url
		}
		if _, err := s.CreateJob(ctx, userID, url, "mock", 600); err != nil {
			t.Fatalf("CreateJob #%d: %v", i+1, err)
		}
	}
	if got := trialUsed(t, s, userID); got != maxActiveJobsPerUser {
		t.Fatalf("trial_meetings_used = %d, expected %d", got, maxActiveJobsPerUser)
	}

	_, err := s.CreateJob(ctx, userID, "https://meet.google.com/cap-5", "mock", 600)
	if !errors.Is(err, ErrTooManyActiveJobs) {
		t.Fatalf("CreateJob #5 err = %v, expected ErrTooManyActiveJobs", err)
	}
	if got := trialUsed(t, s, userID); got != maxActiveJobsPerUser {
		t.Errorf("trial_meetings_used = %d, expected %d (cap must not debit the quota)", got, maxActiveJobsPerUser)
	}

	_, err = s.CreateJob(ctx, userID, firstURL, "mock", 600)
	if !errors.Is(err, ErrDuplicateActiveJob) {
		t.Fatalf("CreateJob duplicate at cap err = %v, expected ErrDuplicateActiveJob", err)
	}
}

// TestCreateJob_ActiveCapRace: the cap must hold under parallel different URLs.
// The users row lock serializes CreateJob of one user; without it a plain COUNT
// could let through more than maxActiveJobsPerUser.
func TestCreateJob_ActiveCapRace(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	const goroutines = 8
	var (
		mu        sync.Mutex
		okCount   int
		capCount  int
		otherErrs []error
		start     = make(chan struct{})
		wg        sync.WaitGroup
	)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			<-start
			_, err := s.CreateJob(ctx, userID, fmt.Sprintf("https://meet.google.com/cap-race-%d", g), "mock", 600)
			mu.Lock()
			switch {
			case err == nil:
				okCount++
			case errors.Is(err, ErrTooManyActiveJobs):
				capCount++
			default:
				otherErrs = append(otherErrs, err)
			}
			mu.Unlock()
		}(g)
	}
	close(start)
	wg.Wait()

	for _, e := range otherErrs {
		t.Errorf("unexpected CreateJob error: %v", e)
	}
	if okCount != maxActiveJobsPerUser {
		t.Errorf("Jobs created = %d, expected %d", okCount, maxActiveJobsPerUser)
	}
	if capCount != goroutines-maxActiveJobsPerUser {
		t.Errorf("ErrTooManyActiveJobs = %d, expected %d", capCount, goroutines-maxActiveJobsPerUser)
	}
	if got := trialUsed(t, s, userID); got != maxActiveJobsPerUser {
		t.Errorf("trial_meetings_used = %d, expected %d", got, maxActiveJobsPerUser)
	}
}

// TestSetJobDone_ConcurrentDoubleDone: done §5.7 under true concurrency: K ticks
// call SetJobDone simultaneously on one summarizing Job (webhook+polling / 2 instances).
// The physics differ from a sequential re-poll: the row lock on the jobs row serializes
// the UPDATEs, and the losers re-read the guard against the committed 'done'. Invariant:
// trial_meetings_done is incremented exactly once (no lost update). Under -race.
func TestSetJobDone_ConcurrentDoubleDone(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/done-concurrent", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.SetJobSubmitted(ctx, job.ID, "ext-done-concurrent"); err != nil {
		t.Fatalf("SetJobSubmitted: %v", err)
	}
	if err := s.SetJobSummarizing(ctx, job.ID, []byte(`{"transcript":"x"}`), 3600); err != nil {
		t.Fatalf("SetJobSummarizing: %v", err)
	}

	const goroutines = 8
	var (
		start = make(chan struct{})
		wg    sync.WaitGroup
	)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // synchronous start: maximize transaction overlap
			if err := s.SetJobDone(ctx, job.ID, []byte(`{"transcript":"final"}`), 3600, 180); err != nil {
				t.Errorf("SetJobDone: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := trialDone(t, s, userID); got != 1 {
		t.Errorf("trial_meetings_done = %d, expected 1 (concurrent double done -> one increment)", got)
	}
}

// TestSetJobDone_SubscriptionNoTrialCounter: done §5.7: a subscription Job (is_trial=false)
// moves to done, but trial_meetings_done does NOT grow (filter closed.is_trial=true). Otherwise
// a paid user would get a false F9 push "trial is ending".
func TestSetJobDone_SubscriptionNoTrialCounter(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	insertSubscription(t, s, userID, 5, 15)

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/done-sub", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if job.IsTrial {
		t.Fatal("expected a subscription Job (is_trial=false)")
	}
	if err := s.SetJobSubmitted(ctx, job.ID, "ext-done-sub"); err != nil {
		t.Fatalf("SetJobSubmitted: %v", err)
	}
	if err := s.SetJobSummarizing(ctx, job.ID, []byte(`{"transcript":"x"}`), 3600); err != nil {
		t.Fatalf("SetJobSummarizing: %v", err)
	}
	if err := s.SetJobDone(ctx, job.ID, []byte(`{"transcript":"final"}`), 3600, 180); err != nil {
		t.Fatalf("SetJobDone: %v", err)
	}

	status, _, _ := jobRow(t, s, job.ID)
	if status != string(JobDone) {
		t.Errorf("status = %q, expected %q", status, JobDone)
	}
	if got := trialDone(t, s, userID); got != 0 {
		t.Errorf("trial_meetings_done = %d, expected 0 (a subscription Job does not feed the trial counter)", got)
	}
}
