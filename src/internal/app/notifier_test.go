package app

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"
)

// Critical invariant F9 (db.md §5.6, architecture.md §7.2): DEDUP means exactly one
// push per situation, not one per tick. We test the storage selections together with Mark*:
// (selected) -> Mark -> (NOT selected). For the subscription classes of §5.6 the dedup relies on
// COALESCE(MAX(paid_at)): after a NEW payment (paid_at later than notified) the user is
// selected again, otherwise a user who renewed would not get a new push.
// The Notifier Run loop and send itself (bot.SendText) are glue, covered by manual E2E (docs/testing.md).
// Integration tests: they hit a real Postgres via DATABASE_URL; without it they Skip.

// insertSubscriptionExpiring inserts an active subscription (minutes_balance>0) with the given
// expires_at = now()+expiresOffset. insertSubscription from storage_quota_test hardcodes
// +30 days, while F9 needs control over the end of coverage (3-4 day window / the past).
func insertSubscriptionExpiring(t *testing.T, s *Storage, userID int64, balance int, expiresOffset string) {
	t.Helper()
	// CHECK (expires_at > paid_at): paid_at = end of coverage minus 1 month, otherwise
	// for an expired subscription (expires in the past) paid_at=now() would violate the invariant.
	_, err := s.pool.Exec(context.Background(),
		`INSERT INTO subscriptions (
		     user_id, paid_at, expires_at, meetings_limit, meetings_balance, minutes_limit, minutes_balance
		 )
		 VALUES ($1, now() + $2::interval - interval '30 days', now() + $2::interval, 15, $3, 900, $4)`,
		userID, expiresOffset, balance, balance*60,
	)
	if err != nil {
		t.Fatalf("insert subscription (expires %s): %v", expiresOffset, err)
	}
	t.Cleanup(func() {
		if _, err := s.pool.Exec(context.Background(), `DELETE FROM subscriptions WHERE user_id = $1`, userID); err != nil {
			t.Logf("cleanup subscriptions: %v", err)
		}
	})
}

func insertSubscriptionMinutes(t *testing.T, s *Storage, userID int64, minutesBalance int, expiresOffset string) int64 {
	t.Helper()
	var id int64
	err := s.pool.QueryRow(context.Background(),
		`INSERT INTO subscriptions (
		     user_id, paid_at, expires_at, meetings_limit, meetings_balance, minutes_limit, minutes_balance
		 )
		 VALUES ($1, now() + $2::interval - interval '30 days', now() + $2::interval, 15, 1, 900, $3)
		 RETURNING id`,
		userID, expiresOffset, minutesBalance,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert subscription minutes (expires %s): %v", expiresOffset, err)
	}
	t.Cleanup(func() {
		if _, err := s.pool.Exec(context.Background(), `DELETE FROM subscriptions WHERE user_id = $1`, userID); err != nil {
			t.Logf("cleanup subscriptions: %v", err)
		}
	})
	return id
}

func insertActivePaidJob(t *testing.T, s *Storage, userID, subID int64, status string) {
	t.Helper()
	var id int64
	err := s.pool.QueryRow(context.Background(),
		`INSERT INTO jobs (
		     user_id, subscription_id, meeting_url, status, is_trial, provider, reserved_minutes
		 )
		 VALUES ($1, $2, $3, $4, false, 'mock', 60)
		 RETURNING id`,
		userID, subID, fmt.Sprintf("https://meet.google.com/active-paid-%d-%s", -userID, status), status,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert active paid job: %v", err)
	}
	t.Cleanup(func() {
		if _, err := s.pool.Exec(context.Background(), `DELETE FROM jobs WHERE id = $1`, id); err != nil {
			t.Logf("cleanup active paid job: %v", err)
		}
	})
}

// insertSucceededPayment inserts a succeeded payment with paid_at=now() (the call moment).
// The §5.6 dedup compares notified_at with MAX(paid_at): a payment inserted AFTER Mark
// carries a later paid_at and reopens the selection. charge_id is required
// (CHECK §5.4) and unique (UNIQUE), so we build it from userID+tag.
func insertSucceededPayment(t *testing.T, s *Storage, userID int64, tag string) {
	t.Helper()
	_, err := s.pool.Exec(context.Background(),
		`INSERT INTO payments (user_id, status, amount, currency, telegram_payment_charge_id, paid_at)
		 VALUES ($1, 'succeeded', 199000, 'RUB', $2, now())`,
		userID, fmtChargeID(userID, tag),
	)
	if err != nil {
		t.Fatalf("insert succeeded payment (%s): %v", tag, err)
	}
}

func insertAnalyticsEventAt(t *testing.T, s *Storage, userID int64, eventType, eventKey, offset string) {
	t.Helper()
	var key any
	if eventKey != "" {
		key = eventKey
	}
	_, err := s.pool.Exec(context.Background(),
		`INSERT INTO analytics_events (user_id, event_type, event_key, occurred_at)
		 VALUES ($1, $2, $3, now() + $4::interval)`,
		userID, eventType, key, offset,
	)
	if err != nil {
		t.Fatalf("insert analytics event %s/%s: %v", eventType, eventKey, err)
	}
}

func fmtChargeID(userID int64, tag string) string {
	return "f9-" + tag + "-" + strconv.FormatInt(userID, 10)
}

// hasUser checks that userID is in the result of a global F9 selection. Selections
// are global (whole DB), and a dev DB may hold foreign rows, so we filter by our own id,
// like mustClaim in storage_test.go.
func hasUser(ids []int64, userID int64) bool {
	for _, id := range ids {
		if id == userID {
			return true
		}
	}
	return false
}

func nudgeEventCount(t *testing.T, s *Storage, userID int64, class string) int {
	t.Helper()
	var count int
	err := s.pool.QueryRow(context.Background(),
		`SELECT COUNT(*)
		 FROM analytics_events
		 WHERE user_id = $1 AND event_type = $2 AND event_key = $3`,
		userID, analyticsNudgeSent, class,
	).Scan(&count)
	if err != nil {
		t.Fatalf("count nudge events %s: %v", class, err)
	}
	return count
}

// =========================== UNIT: send->mark (no DB) ===========================

// TestNotifyGroup_SendThenMark covers the §5.6 "send, then mark" order in notifyGroup. This is
// the decisive fork for dedup: when notify fails we do NOT mark (the push is lost, we
// retry on the next tick); on success we mark (otherwise spam on every tick). Selections
// are tested by the integration tests above; here is pure worker logic on fake fetch/notify/
// mark, without a DB (n.storage is not touched in notifyGroup). architecture.md §7.2 / db.md §5.6.
func TestNotifyGroup_SendThenMark(t *testing.T) {
	ctx := context.Background()

	t.Run("send_ok_then_mark", func(t *testing.T) {
		var marked []int64
		n := NewNotifier(nil, func(int64, string) error { return nil }, 3, time.Hour)
		fetch := func(context.Context, int) ([]int64, error) { return []int64{10, 20}, nil }
		mark := func(_ context.Context, uid int64) error { marked = append(marked, uid); return nil }

		n.notifyGroup(ctx, "test", fetch, "txt", mark)

		if len(marked) != 2 || marked[0] != 10 || marked[1] != 20 {
			t.Errorf("marked = %v, want [10 20] (successful send -> mark of each)", marked)
		}
	})

	t.Run("send_fails_no_mark", func(t *testing.T) {
		var marked []int64
		// notify fails for all -> mark must never be called (continue before mark).
		n := NewNotifier(nil, func(int64, string) error { return errors.New("tg down") }, 3, time.Hour)
		fetch := func(context.Context, int) ([]int64, error) { return []int64{10, 20}, nil }
		mark := func(_ context.Context, uid int64) error { marked = append(marked, uid); return nil }

		n.notifyGroup(ctx, "test", fetch, "txt", mark)

		if len(marked) != 0 {
			t.Errorf("marked = %v, want empty (a notify failure must not mark: the push is lost, we retry)", marked)
		}
	})

	t.Run("partial_send_failure_marks_only_delivered", func(t *testing.T) {
		// notify fails only for uid=10; uid=20 is delivered. Invariant: exactly 20 is marked
		// (the lost push for 10 repeats on the next tick, the selection dedup does not mask it).
		var mu sync.Mutex
		var marked []int64
		notify := func(uid int64, _ string) error {
			if uid == 10 {
				return errors.New("blocked")
			}
			return nil
		}
		n := NewNotifier(nil, notify, 3, time.Hour)
		fetch := func(context.Context, int) ([]int64, error) { return []int64{10, 20}, nil }
		mark := func(_ context.Context, uid int64) error {
			mu.Lock()
			marked = append(marked, uid)
			mu.Unlock()
			return nil
		}

		n.notifyGroup(ctx, "test", fetch, "txt", mark)

		if len(marked) != 1 || marked[0] != 20 {
			t.Errorf("marked = %v, want [20] (mark only the delivered one)", marked)
		}
	})
}

func TestSafeAnalyticsMetadata(t *testing.T) {
	got := safeAnalyticsMetadata(analyticsSubscriptionViewed, map[string]string{
		"email":       "user@example.com",
		"meeting_url": "https://meet.google.com/test",
	})
	if len(got) != 0 {
		t.Fatalf("subscription metadata = %v, want empty", got)
	}

	got = safeAnalyticsMetadata(analyticsNudgeSent, map[string]string{
		"class":       nudgePaymentFollowup,
		"payment_id":  "provider-secret",
		"meeting_url": "https://meet.google.com/test",
	})
	if len(got) != 1 || got["class"] != nudgePaymentFollowup {
		t.Fatalf("nudge metadata = %v, want class only", got)
	}
}

func TestPaymentViewedWithoutPaidUsers_Dedup(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)

	followupUser := newTestUser(t, s)
	insertAnalyticsEventAt(t, s, followupUser, analyticsSubscriptionViewed, "", "-25 hours")

	ids, err := s.PaymentViewedWithoutPaidUsers(ctx)
	if err != nil {
		t.Fatalf("PaymentViewedWithoutPaidUsers #1: %v", err)
	}
	if !hasUser(ids, followupUser) {
		t.Fatal("user with a payment view older than 24 hours was not selected for payment followup")
	}

	if err := s.MarkNudgeSent(ctx, followupUser, nudgePaymentFollowup); err != nil {
		t.Fatalf("MarkNudgeSent payment followup: %v", err)
	}
	ids, err = s.PaymentViewedWithoutPaidUsers(ctx)
	if err != nil {
		t.Fatalf("PaymentViewedWithoutPaidUsers #2: %v", err)
	}
	if hasUser(ids, followupUser) {
		t.Error("user is selected again after nudge_sent payment_followup")
	}

	paidUser := newTestUser(t, s)
	cleanupPayments(t, s, paidUser)
	insertAnalyticsEventAt(t, s, paidUser, analyticsPaymentRequested, "", "-25 hours")
	insertSucceededPayment(t, s, paidUser, "payment-followup-paid")

	ids, err = s.PaymentViewedWithoutPaidUsers(ctx)
	if err != nil {
		t.Fatalf("PaymentViewedWithoutPaidUsers #3: %v", err)
	}
	if hasUser(ids, paidUser) {
		t.Error("user with a succeeded payment after the payment view was selected for payment followup")
	}

	activeUser := newTestUser(t, s)
	insertAnalyticsEventAt(t, s, activeUser, analyticsSubscriptionViewed, "", "-25 hours")
	insertSubscriptionMinutes(t, s, activeUser, 900, "30 days")

	ids, err = s.PaymentViewedWithoutPaidUsers(ctx)
	if err != nil {
		t.Fatalf("PaymentViewedWithoutPaidUsers #4: %v", err)
	}
	if hasUser(ids, activeUser) {
		t.Error("user with an active subscription was selected for payment followup")
	}
}

func TestSubLowBalanceUsers_Dedup(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)
	insertSubscriptionMinutes(t, s, userID, 30, "30 days")
	insertSucceededPayment(t, s, userID, "low-balance-1")

	ids, err := s.SubLowBalanceUsers(ctx)
	if err != nil {
		t.Fatalf("SubLowBalanceUsers #1: %v", err)
	}
	if !hasUser(ids, userID) {
		t.Fatal("paying user with less than an hour left was not selected in SubLowBalanceUsers")
	}

	if err := s.MarkNudgeSent(ctx, userID, nudgeSubLowBalance); err != nil {
		t.Fatalf("MarkNudgeSent low balance: %v", err)
	}
	ids, err = s.SubLowBalanceUsers(ctx)
	if err != nil {
		t.Fatalf("SubLowBalanceUsers #2: %v", err)
	}
	if hasUser(ids, userID) {
		t.Error("user is selected again after nudge_sent sub_low_balance")
	}

	activeJobUser := newTestUser(t, s)
	cleanupPayments(t, s, activeJobUser)
	subID := insertSubscriptionMinutes(t, s, activeJobUser, 30, "30 days")
	insertSucceededPayment(t, s, activeJobUser, "low-balance-active-job")
	insertActivePaidJob(t, s, activeJobUser, subID, string(JobRecording))

	ids, err = s.SubLowBalanceUsers(ctx)
	if err != nil {
		t.Fatalf("SubLowBalanceUsers #3: %v", err)
	}
	if hasUser(ids, activeJobUser) {
		t.Error("user with low balance due to an active paid reserve was selected in SubLowBalanceUsers")
	}
}

func TestSubEmptyBalanceUsers_Dedup(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)
	insertSubscriptionMinutes(t, s, userID, 0, "30 days")
	insertSucceededPayment(t, s, userID, "empty-balance-1")

	ids, err := s.SubEmptyBalanceUsers(ctx)
	if err != nil {
		t.Fatalf("SubEmptyBalanceUsers #1: %v", err)
	}
	if !hasUser(ids, userID) {
		t.Fatal("paying user with zero hours before expiry was not selected in SubEmptyBalanceUsers")
	}

	if err := s.MarkNudgeSent(ctx, userID, nudgeSubEmptyBalance); err != nil {
		t.Fatalf("MarkNudgeSent empty balance: %v", err)
	}
	ids, err = s.SubEmptyBalanceUsers(ctx)
	if err != nil {
		t.Fatalf("SubEmptyBalanceUsers #2: %v", err)
	}
	if hasUser(ids, userID) {
		t.Error("user is selected again after nudge_sent sub_empty_balance")
	}

	expiredUser := newTestUser(t, s)
	cleanupPayments(t, s, expiredUser)
	insertSubscriptionMinutes(t, s, expiredUser, 0, "-2 days")
	insertSucceededPayment(t, s, expiredUser, "empty-balance-expired")

	ids, err = s.SubEmptyBalanceUsers(ctx)
	if err != nil {
		t.Fatalf("SubEmptyBalanceUsers #3: %v", err)
	}
	if hasUser(ids, expiredUser) {
		t.Error("user with a calendar-expired subscription was selected in SubEmptyBalanceUsers instead of sub-expired")
	}

	activeJobUser := newTestUser(t, s)
	cleanupPayments(t, s, activeJobUser)
	subID := insertSubscriptionMinutes(t, s, activeJobUser, 0, "30 days")
	insertSucceededPayment(t, s, activeJobUser, "empty-balance-active-job")
	insertActivePaidJob(t, s, activeJobUser, subID, string(JobProcessing))

	ids, err = s.SubEmptyBalanceUsers(ctx)
	if err != nil {
		t.Fatalf("SubEmptyBalanceUsers #4: %v", err)
	}
	if hasUser(ids, activeJobUser) {
		t.Error("user with empty balance due to an active paid reserve was selected in SubEmptyBalanceUsers")
	}
}

// TestSubExpiringUsers_Dedup (§5.6): subscription expires in 3-4 days + succeeded payment
// -> user is selected; Mark -> dropped (one-push-per-situation dedup); a NEW payment
// (paid_at later than notified) -> selected again (COALESCE dedup lets it through after renewal).
func TestSubExpiringUsers_Dedup(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)

	// End of coverage in the now()+[3d,4d] window: 3 days 12 hours is the middle of the window.
	insertSubscriptionExpiring(t, s, userID, 5, "3 days 12 hours")
	insertSucceededPayment(t, s, userID, "expiring-1")

	ids, err := s.SubExpiringUsers(ctx)
	if err != nil {
		t.Fatalf("SubExpiringUsers #1: %v", err)
	}
	if !hasUser(ids, userID) {
		t.Fatal("user with a subscription expiring in 3.5 days was not selected in SubExpiringUsers")
	}

	if err := s.MarkSubExpiringAndNudgeSent(ctx, userID); err != nil {
		t.Fatalf("MarkSubExpiringAndNudgeSent: %v", err)
	}
	if got := nudgeEventCount(t, s, userID, nudgeSubExpiring); got != 1 {
		t.Fatalf("sub expiring nudge events = %d, want 1", got)
	}

	ids, err = s.SubExpiringUsers(ctx)
	if err != nil {
		t.Fatalf("SubExpiringUsers #2: %v", err)
	}
	if hasUser(ids, userID) {
		t.Error("user is selected again after Mark: dedup failed (spam on every tick)")
	}

	// New payment: paid_at=now() strictly later than notified_sub_expiring_at (Mark was earlier).
	insertSucceededPayment(t, s, userID, "expiring-2")

	ids, err = s.SubExpiringUsers(ctx)
	if err != nil {
		t.Fatalf("SubExpiringUsers #3: %v", err)
	}
	if !hasUser(ids, userID) {
		t.Error("user did not return to the selection after a new payment: COALESCE dedup blocks the renewed user")
	}

	lowBalanceUser := newTestUser(t, s)
	cleanupPayments(t, s, lowBalanceUser)
	insertSubscriptionMinutes(t, s, lowBalanceUser, 30, "3 days 12 hours")
	insertSucceededPayment(t, s, lowBalanceUser, "expiring-low-balance")
	insertSucceededPayment(t, s, lowBalanceUser, "expiring-low-balance-2")

	ids, err = s.SubExpiringUsers(ctx)
	if err != nil {
		t.Fatalf("SubExpiringUsers #4: %v", err)
	}
	if hasUser(ids, lowBalanceUser) {
		t.Error("user with less than an hour left was selected in SubExpiringUsers: must get sub_low_balance")
	}
}

func TestMarkLegacyAndNudgeSent_Rollback(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	err := s.markLegacyAndNudgeSent(ctx, userID, "",
		`UPDATE users SET notified_trial_low_at = now() WHERE telegram_id = $1`)
	if err == nil {
		t.Fatal("markLegacyAndNudgeSent succeeded with empty class, want CHECK error")
	}

	var notified *time.Time
	err = s.pool.QueryRow(ctx,
		`SELECT notified_trial_low_at FROM users WHERE telegram_id = $1`,
		userID,
	).Scan(&notified)
	if err != nil {
		t.Fatalf("read notified_trial_low_at: %v", err)
	}
	if notified != nil {
		t.Fatal("notified_trial_low_at was committed after analytics insert failed")
	}
}

// TestTrialEndingUsers_Dedup (§5.6): trial without a subscription, less than an hour left -> selected;
// Mark -> dropped (notify-once via IS NULL); a user with an active subscription and the same done
// is NOT selected (they pay, NOT EXISTS cuts them off).
func TestTrialEndingUsers_Dedup(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	const trialLimit = 180

	// --- user at the edge of the trial (59 minutes left), no subscription ---
	trialUser := newTestUser(t, s)
	setTrialDone(t, s, trialUser, 121)

	ids, err := s.TrialEndingUsers(ctx, trialLimit)
	if err != nil {
		t.Fatalf("TrialEndingUsers #1: %v", err)
	}
	if !hasUser(ids, trialUser) {
		t.Fatal("user with less than an hour of trial left was not selected in TrialEndingUsers")
	}

	if err := s.MarkTrialLowAndNudgeSent(ctx, trialUser); err != nil {
		t.Fatalf("MarkTrialLowAndNudgeSent: %v", err)
	}
	if got := nudgeEventCount(t, s, trialUser, nudgeTrialEnding); got != 1 {
		t.Fatalf("trial ending nudge events = %d, want 1", got)
	}
	ids, err = s.TrialEndingUsers(ctx, trialLimit)
	if err != nil {
		t.Fatalf("TrialEndingUsers #2: %v", err)
	}
	if hasUser(ids, trialUser) {
		t.Error("trial user is selected again after Mark: notify-once dedup failed")
	}

	// --- user with the same done but an active subscription: a paying user needs no push ---
	subUser := newTestUser(t, s)
	cleanupPayments(t, s, subUser)
	setTrialDone(t, s, subUser, 121)
	insertSubscriptionExpiring(t, s, subUser, 5, "30 days")

	ids, err = s.TrialEndingUsers(ctx, trialLimit)
	if err != nil {
		t.Fatalf("TrialEndingUsers #3: %v", err)
	}
	if hasUser(ids, subUser) {
		t.Error("user with an active subscription was selected in TrialEndingUsers: NOT EXISTS did not cut off the paying user")
	}

	zeroBalanceSubUser := newTestUser(t, s)
	setTrialDone(t, s, zeroBalanceSubUser, 121)
	insertSubscriptionMinutes(t, s, zeroBalanceSubUser, 0, "30 days")

	ids, err = s.TrialEndingUsers(ctx, trialLimit)
	if err != nil {
		t.Fatalf("TrialEndingUsers #4: %v", err)
	}
	if hasUser(ids, zeroBalanceSubUser) {
		t.Error("user with a calendar-active subscription and zero minutes was selected in TrialEndingUsers: must go to sub_empty_balance")
	}
}

// setTrialDone sets trial_minutes_done. CHECK (done <= used), so we raise used
// in step, otherwise the row violates the invariant of migration 00009.
func setTrialDone(t *testing.T, s *Storage, userID int64, done int) {
	t.Helper()
	_, err := s.pool.Exec(context.Background(),
		`UPDATE users
		 SET trial_minutes_done = $2,
		     trial_minutes_used = $2,
		     trial_meetings_done = $2 / 60,
		     trial_meetings_used = $2 / 60
		 WHERE telegram_id = $1`,
		userID, done,
	)
	if err != nil {
		t.Fatalf("set trial_minutes_done=%d: %v", done, err)
	}
}

// TestSubExpiredUsers_Dedup (§5.6): subscription expired (expires_at in the past, no active
// coverage) + succeeded payment -> selected; Mark -> dropped; an active subscription is NOT
// considered "expired". The dedup is the same COALESCE pattern as in expiring.
func TestSubExpiredUsers_Dedup(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)

	// --- user with an expired subscription ---
	expiredUser := newTestUser(t, s)
	cleanupPayments(t, s, expiredUser)
	// expires_at in the past: balance does not matter, there is no active coverage (NOT EXISTS passes).
	insertSubscriptionExpiring(t, s, expiredUser, 5, "-2 days")
	insertSucceededPayment(t, s, expiredUser, "expired-1")

	ids, err := s.SubExpiredUsers(ctx)
	if err != nil {
		t.Fatalf("SubExpiredUsers #1: %v", err)
	}
	if !hasUser(ids, expiredUser) {
		t.Fatal("user with an expired subscription and a succeeded payment was not selected in SubExpiredUsers")
	}

	if err := s.MarkSubExpiredAndNudgeSent(ctx, expiredUser); err != nil {
		t.Fatalf("MarkSubExpiredAndNudgeSent: %v", err)
	}
	if got := nudgeEventCount(t, s, expiredUser, nudgeSubExpired); got != 1 {
		t.Fatalf("sub expired nudge events = %d, want 1", got)
	}
	ids, err = s.SubExpiredUsers(ctx)
	if err != nil {
		t.Fatalf("SubExpiredUsers #2: %v", err)
	}
	if hasUser(ids, expiredUser) {
		t.Error("user is in SubExpiredUsers again after Mark: dedup failed")
	}

	// New payment: paid_at=now() later than notified; COALESCE dedup reopens the selection
	// (the user paid again; the new period will expire too -> a repeated "expired" push is legitimate).
	insertSucceededPayment(t, s, expiredUser, "expired-2")
	ids, err = s.SubExpiredUsers(ctx)
	if err != nil {
		t.Fatalf("SubExpiredUsers #2b: %v", err)
	}
	if !hasUser(ids, expiredUser) {
		t.Error("user did not return to SubExpiredUsers after a new payment: COALESCE dedup blocks")
	}

	// --- user with an active subscription: NOT "expired" ---
	activeUser := newTestUser(t, s)
	cleanupPayments(t, s, activeUser)
	insertSubscriptionExpiring(t, s, activeUser, 5, "10 days")
	insertSucceededPayment(t, s, activeUser, "active-1")

	ids, err = s.SubExpiredUsers(ctx)
	if err != nil {
		t.Fatalf("SubExpiredUsers #3: %v", err)
	}
	if hasUser(ids, activeUser) {
		t.Error("user with an active subscription was selected in SubExpiredUsers: NOT EXISTS did not cut off the active coverage")
	}
}
