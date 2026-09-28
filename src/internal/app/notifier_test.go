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

// Критический инвариант F9 (db.md §5.6, architecture.md §7.2): ДЕДУП — ровно один
// пуш на ситуацию, не на каждый тик. Тестируем storage-выборки в связке с Mark*:
// (выбран) -> Mark -> (НЕ выбран). Для подписочных классов §5.6 дедуп держится на
// COALESCE(MAX(paid_at)): после НОВОГО платежа (paid_at позже notified) юзер снова
// попадает в выборку — иначе продливший подписку юзер не получил бы новый пуш.
// Run-цикл Notifier и сам send (bot.SendText) — клей, ручной E2E (docs/testing.md).
// Интеграционные: бьют в настоящий Postgres через DATABASE_URL; без него — Skip.

// insertSubscriptionExpiring вставляет активную подписку (minutes_balance>0) с заданным
// expires_at = now()+expiresOffset. insertSubscription из storage_quota_test жёстко
// задаёт +30 дней, а F9 нужен контроль конца покрытия (окно 3-4 дня / прошлое).
func insertSubscriptionExpiring(t *testing.T, s *Storage, userID int64, balance int, expiresOffset string) {
	t.Helper()
	// CHECK (expires_at > paid_at): paid_at = end of coverage минус 1 месяц, иначе
	// для истёкшей подписки (expires в прошлом) paid_at=now() нарушил бы инвариант.
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

// insertSucceededPayment вставляет succeeded-платёж с paid_at=now() (момент вызова).
// Дедуп §5.6 сравнивает notified_at с MAX(paid_at): платёж, вставленный ПОСЛЕ Mark,
// несёт более поздний paid_at и снова открывает выборку. charge_id обязателен
// (CHECK §5.4) и уникален (UNIQUE) — формируем из userID+tag.
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

// hasUser проверяет вхождение userID в результат глобальной F9-выборки. Выборки
// глобальны (вся БД), в dev могут лежать чужие строки — фильтруем по своему id,
// как mustClaim в storage_test.go.
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

// =========================== UNIT: send->mark (без БД) ===========================

// TestNotifyGroup_SendThenMark — §5.6 порядок «send потом mark» в notifyGroup. Это
// решающая для дедупа развилка: при сбое notify отметку НЕ ставим (пуш потеряется —
// повторим на следующем тике), при успехе — ставим (иначе спам каждый тик). Выборки
// тестируются интеграционно выше; здесь — чистая логика воркера на фейках fetch/notify/
// mark, без БД (n.storage не трогается в notifyGroup). architecture.md §7.2 / db.md §5.6.
func TestNotifyGroup_SendThenMark(t *testing.T) {
	ctx := context.Background()

	t.Run("send_ok_then_mark", func(t *testing.T) {
		var marked []int64
		n := NewNotifier(nil, func(int64, string) error { return nil }, 3, time.Hour)
		fetch := func(context.Context, int) ([]int64, error) { return []int64{10, 20}, nil }
		mark := func(_ context.Context, uid int64) error { marked = append(marked, uid); return nil }

		n.notifyGroup(ctx, "test", fetch, "txt", mark)

		if len(marked) != 2 || marked[0] != 10 || marked[1] != 20 {
			t.Errorf("marked = %v, ожидали [10 20] (успешный send -> mark каждого)", marked)
		}
	})

	t.Run("send_fails_no_mark", func(t *testing.T) {
		var marked []int64
		// notify падает на всех -> mark не должен вызваться ни разу (continue до mark).
		n := NewNotifier(nil, func(int64, string) error { return errors.New("tg down") }, 3, time.Hour)
		fetch := func(context.Context, int) ([]int64, error) { return []int64{10, 20}, nil }
		mark := func(_ context.Context, uid int64) error { marked = append(marked, uid); return nil }

		n.notifyGroup(ctx, "test", fetch, "txt", mark)

		if len(marked) != 0 {
			t.Errorf("marked = %v, ожидали пусто (сбой notify не должен отмечать — пуш потеряется, повторим)", marked)
		}
	})

	t.Run("partial_send_failure_marks_only_delivered", func(t *testing.T) {
		// notify падает только для uid=10; uid=20 доставлен. Инвариант: помечен ровно 20
		// (потерянный пуш 10 повторится на следующем тике, дедуп выборки его не маскирует).
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
			t.Errorf("marked = %v, ожидали [20] (помечаем только доставленного)", marked)
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
		t.Fatal("юзер с просмотром оплаты старше 24 часов не попал в payment followup")
	}

	if err := s.MarkNudgeSent(ctx, followupUser, nudgePaymentFollowup); err != nil {
		t.Fatalf("MarkNudgeSent payment followup: %v", err)
	}
	ids, err = s.PaymentViewedWithoutPaidUsers(ctx)
	if err != nil {
		t.Fatalf("PaymentViewedWithoutPaidUsers #2: %v", err)
	}
	if hasUser(ids, followupUser) {
		t.Error("после nudge_sent payment_followup юзер снова в выборке")
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
		t.Error("юзер с succeeded-платежом после просмотра оплаты попал в payment followup")
	}

	activeUser := newTestUser(t, s)
	insertAnalyticsEventAt(t, s, activeUser, analyticsSubscriptionViewed, "", "-25 hours")
	insertSubscriptionMinutes(t, s, activeUser, 900, "30 days")

	ids, err = s.PaymentViewedWithoutPaidUsers(ctx)
	if err != nil {
		t.Fatalf("PaymentViewedWithoutPaidUsers #4: %v", err)
	}
	if hasUser(ids, activeUser) {
		t.Error("юзер с активной подпиской попал в payment followup")
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
		t.Fatal("платящий юзер с остатком меньше часа не попал в SubLowBalanceUsers")
	}

	if err := s.MarkNudgeSent(ctx, userID, nudgeSubLowBalance); err != nil {
		t.Fatalf("MarkNudgeSent low balance: %v", err)
	}
	ids, err = s.SubLowBalanceUsers(ctx)
	if err != nil {
		t.Fatalf("SubLowBalanceUsers #2: %v", err)
	}
	if hasUser(ids, userID) {
		t.Error("после nudge_sent sub_low_balance юзер снова в выборке")
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
		t.Error("юзер с low balance из-за активного платного резерва попал в SubLowBalanceUsers")
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
		t.Fatal("платящий юзер с нулём часов до expiry не попал в SubEmptyBalanceUsers")
	}

	if err := s.MarkNudgeSent(ctx, userID, nudgeSubEmptyBalance); err != nil {
		t.Fatalf("MarkNudgeSent empty balance: %v", err)
	}
	ids, err = s.SubEmptyBalanceUsers(ctx)
	if err != nil {
		t.Fatalf("SubEmptyBalanceUsers #2: %v", err)
	}
	if hasUser(ids, userID) {
		t.Error("после nudge_sent sub_empty_balance юзер снова в выборке")
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
		t.Error("юзер с календарно истёкшей подпиской попал в SubEmptyBalanceUsers вместо sub-expired")
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
		t.Error("юзер с empty balance из-за активного платного резерва попал в SubEmptyBalanceUsers")
	}
}

// TestSubExpiringUsers_Dedup — §5.6: подписка истекает через 3-4 дня + succeeded-платёж
// -> юзер в выборке; Mark -> выпадает (дедуп один-пуш-на-ситуацию); НОВЫЙ платёж
// (paid_at позже notified) -> снова в выборке (COALESCE-дедуп пускает после продления).
func TestSubExpiringUsers_Dedup(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)

	// Конец покрытия в окне now()+[3d,4d]: 3 дня 12 часов — середина окна.
	insertSubscriptionExpiring(t, s, userID, 5, "3 days 12 hours")
	insertSucceededPayment(t, s, userID, "expiring-1")

	ids, err := s.SubExpiringUsers(ctx)
	if err != nil {
		t.Fatalf("SubExpiringUsers #1: %v", err)
	}
	if !hasUser(ids, userID) {
		t.Fatal("юзер с подпиской, истекающей через 3.5 дня, не попал в SubExpiringUsers")
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
		t.Error("после Mark юзер снова в выборке — дедуп не сработал (спам каждый тик)")
	}

	// Новый платёж: paid_at=now() строго позже notified_sub_expiring_at (Mark был раньше).
	insertSucceededPayment(t, s, userID, "expiring-2")

	ids, err = s.SubExpiringUsers(ctx)
	if err != nil {
		t.Fatalf("SubExpiringUsers #3: %v", err)
	}
	if !hasUser(ids, userID) {
		t.Error("после нового платежа юзер не вернулся в выборку — COALESCE-дедуп не пускает продлившего")
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
		t.Error("юзер с остатком меньше часа попал в SubExpiringUsers — должен получить sub_low_balance")
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

// TestTrialEndingUsers_Dedup — §5.6: триал без подписки, осталось меньше часа -> в выборке;
// Mark -> выпадает (notify-once через IS NULL); юзер с активной подпиской и тем же done
// НЕ попадает (он платит, NOT EXISTS отсекает).
func TestTrialEndingUsers_Dedup(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	const trialLimit = 180

	// --- юзер на грани триала (осталось 59 минут), без подписки ---
	trialUser := newTestUser(t, s)
	setTrialDone(t, s, trialUser, 121)

	ids, err := s.TrialEndingUsers(ctx, trialLimit)
	if err != nil {
		t.Fatalf("TrialEndingUsers #1: %v", err)
	}
	if !hasUser(ids, trialUser) {
		t.Fatal("юзер с остатком триала меньше часа не попал в TrialEndingUsers")
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
		t.Error("после Mark триал-юзер снова в выборке — notify-once дедуп не сработал")
	}

	// --- юзер с тем же done, но активной подпиской: платящему пуш не нужен ---
	subUser := newTestUser(t, s)
	cleanupPayments(t, s, subUser)
	setTrialDone(t, s, subUser, 121)
	insertSubscriptionExpiring(t, s, subUser, 5, "30 days")

	ids, err = s.TrialEndingUsers(ctx, trialLimit)
	if err != nil {
		t.Fatalf("TrialEndingUsers #3: %v", err)
	}
	if hasUser(ids, subUser) {
		t.Error("юзер с активной подпиской попал в TrialEndingUsers — NOT EXISTS не отсёк платящего")
	}

	zeroBalanceSubUser := newTestUser(t, s)
	setTrialDone(t, s, zeroBalanceSubUser, 121)
	insertSubscriptionMinutes(t, s, zeroBalanceSubUser, 0, "30 days")

	ids, err = s.TrialEndingUsers(ctx, trialLimit)
	if err != nil {
		t.Fatalf("TrialEndingUsers #4: %v", err)
	}
	if hasUser(ids, zeroBalanceSubUser) {
		t.Error("юзер с календарно активной подпиской и нулём минут попал в TrialEndingUsers — должен идти в sub_empty_balance")
	}
}

// setTrialDone выставляет trial_minutes_done. CHECK (done <= used) — поднимаем used
// синхронно, иначе строка нарушит инвариант миграции 00009.
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

// TestSubExpiredUsers_Dedup — §5.6: подписка истекла (expires_at в прошлом, нет активного
// покрытия) + succeeded-платёж -> в выборке; Mark -> выпадает; активная подписка НЕ
// считается «истёкшей». Дедуп — тот же COALESCE-паттерн, что в expiring.
func TestSubExpiredUsers_Dedup(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)

	// --- юзер с истёкшей подпиской ---
	expiredUser := newTestUser(t, s)
	cleanupPayments(t, s, expiredUser)
	// expires_at в прошлом: balance не важен, активного покрытия нет (NOT EXISTS пройдёт).
	insertSubscriptionExpiring(t, s, expiredUser, 5, "-2 days")
	insertSucceededPayment(t, s, expiredUser, "expired-1")

	ids, err := s.SubExpiredUsers(ctx)
	if err != nil {
		t.Fatalf("SubExpiredUsers #1: %v", err)
	}
	if !hasUser(ids, expiredUser) {
		t.Fatal("юзер с истёкшей подпиской и succeeded-платежом не попал в SubExpiredUsers")
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
		t.Error("после Mark юзер снова в SubExpiredUsers — дедуп не сработал")
	}

	// Новый платёж: paid_at=now() позже notified — COALESCE-дедуп снова открывает выборку
	// (юзер заново оплатил; новый период тоже истечёт -> повторный «истекла»-пуш правомерен).
	insertSucceededPayment(t, s, expiredUser, "expired-2")
	ids, err = s.SubExpiredUsers(ctx)
	if err != nil {
		t.Fatalf("SubExpiredUsers #2b: %v", err)
	}
	if !hasUser(ids, expiredUser) {
		t.Error("после нового платежа юзер не вернулся в SubExpiredUsers — COALESCE-дедуп не пускает")
	}

	// --- юзер с активной подпиской: НЕ «истекла» ---
	activeUser := newTestUser(t, s)
	cleanupPayments(t, s, activeUser)
	insertSubscriptionExpiring(t, s, activeUser, 5, "10 days")
	insertSucceededPayment(t, s, activeUser, "active-1")

	ids, err = s.SubExpiredUsers(ctx)
	if err != nil {
		t.Fatalf("SubExpiredUsers #3: %v", err)
	}
	if hasUser(ids, activeUser) {
		t.Error("юзер с активной подпиской попал в SubExpiredUsers — NOT EXISTS не отсёк активное покрытие")
	}
}
