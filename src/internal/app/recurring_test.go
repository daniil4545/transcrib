package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRecurringWorker_CreatesPendingPaymentOnce(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)
	nextChargeAt := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	insertRecurringProfile(t, s, userID, "pm-worker-1", "active", nextChargeAt)
	// Свежий цикл списания: обнуляем seed-счётчик попыток из хелпера, чтобы
	// проверить, что первое успешное списание даёт attempts_count = 1.
	if _, err := s.pool.Exec(ctx,
		`UPDATE recurring_profiles SET attempts_count = 0, last_error = NULL WHERE user_id = $1`,
		userID,
	); err != nil {
		t.Fatalf("reset attempts: %v", err)
	}
	if err := s.SetPaymentEmail(ctx, userID, "user@example.com"); err != nil {
		t.Fatalf("SetPaymentEmail: %v", err)
	}

	var calls int
	var body struct {
		PaymentMethodID string `json:"payment_method_id"`
		Metadata        struct {
			UserID    string `json:"user_id"`
			AutoRenew string `json:"auto_renew"`
			Recurring string `json:"recurring"`
		} `json:"metadata"`
		Receipt struct {
			Customer struct {
				Email string `json:"email"`
			} `json:"customer"`
		} `json:"receipt"`
	}
	b := testBilling(Config{
		SubscriptionPriceKopecks: 199000,
		SubscriptionDescription:  "15 часов на 1 месяц",
		YookassaShopID:           "shop-1",
		YookassaSecretKey:        "secret-1",
		YookassaBaseURL:          "http://yookassa.test",
	})
	b.storage = s
	b.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if got := r.Header.Get("Idempotence-Key"); got != recurringPaymentIntentKey(userID, nextChargeAt) {
			t.Fatalf("Idempotence-Key = %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode recurring request: %v", err)
		}
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"yk-recurring-worker-1",
			"status":"pending",
			"paid":false,
			"amount":{"value":"1990.00","currency":"RUB"},
			"metadata":{"user_id":"` + strconv.FormatInt(userID, 10) + `","auto_renew":"true","recurring":"true"}
		}`))
		return w.Result(), nil
	})}

	worker := NewRecurringWorker(s, b, time.Hour, true)
	if err := worker.tickOnce(ctx); err != nil {
		t.Fatalf("tickOnce: %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, ожидали 1", calls)
	}
	if body.PaymentMethodID != "pm-worker-1" {
		t.Fatalf("payment_method_id = %q", body.PaymentMethodID)
	}
	if body.Metadata.UserID != strconv.FormatInt(userID, 10) || body.Metadata.AutoRenew != "true" || body.Metadata.Recurring != "true" {
		t.Fatalf("metadata = %#v", body.Metadata)
	}
	if body.Receipt.Customer.Email != "user@example.com" {
		t.Fatalf("receipt email = %q", body.Receipt.Customer.Email)
	}
	assertRecurringPayment(t, s, userID, "yk-recurring-worker-1", recurringPaymentIntentKey(userID, nextChargeAt), "pm-worker-1", "pending")
	startedProfile := recurringProfile(t, s, userID)
	if startedProfile.status != "charging" {
		t.Fatalf("profile.status after StartRecurringPaymentIntent = %q, ожидали charging", startedProfile.status)
	}

	if err := worker.tickOnce(ctx); err != nil {
		t.Fatalf("tickOnce #2: %v", err)
	}
	if calls != 1 {
		t.Fatalf("second tick created duplicate payment, calls=%d", calls)
	}
	profile := recurringProfile(t, s, userID)
	if profile.attempts != 1 || profile.status != "charging" {
		t.Fatalf("profile = %#v", profile)
	}
}

func TestRecurringWorker_RetriesPendingIntentWithoutProviderID(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)
	nextChargeAt := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	insertRecurringProfile(t, s, userID, "pm-worker-retry", "active", nextChargeAt)
	if err := s.SetPaymentEmail(ctx, userID, "retry@example.com"); err != nil {
		t.Fatalf("SetPaymentEmail: %v", err)
	}
	intent := PaymentInput{
		UserID:          userID,
		AmountKopecks:   199000,
		Currency:        "RUB",
		IntentKey:       recurringPaymentIntentKey(userID, nextChargeAt),
		Metadata:        recurringPaymentMetadata(userID),
		PaymentMethodID: "pm-worker-retry",
	}
	created, err := s.StartRecurringPaymentIntent(ctx, intent, intent.IntentKey, nextChargeAt)
	if err != nil {
		t.Fatalf("StartRecurringPaymentIntent: %v", err)
	}
	if !created {
		t.Fatalf("expected initial local intent")
	}
	// claim выставил updated_at=now(); состариваем сверх 2-min lease, чтобы
	// следующий тик переподхватил зависший charging-профиль на retry POST.
	if _, err := s.pool.Exec(ctx,
		`UPDATE recurring_profiles SET updated_at = now() - INTERVAL '3 minutes' WHERE user_id = $1`,
		userID,
	); err != nil {
		t.Fatalf("age recurring profile: %v", err)
	}

	var calls int
	b := testBilling(Config{
		SubscriptionPriceKopecks: 199000,
		SubscriptionDescription:  "15 часов на 1 месяц",
		YookassaShopID:           "shop-1",
		YookassaSecretKey:        "secret-1",
		YookassaBaseURL:          "http://yookassa.test",
	})
	b.storage = s
	b.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if got := r.Header.Get("Idempotence-Key"); got != intent.IntentKey {
			t.Fatalf("Idempotence-Key = %q", got)
		}
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"yk-recurring-retry-provider",
			"status":"pending",
			"paid":false,
			"amount":{"value":"1990.00","currency":"RUB"},
			"metadata":{"user_id":"` + strconv.FormatInt(userID, 10) + `","auto_renew":"true","recurring":"true"}
		}`))
		return w.Result(), nil
	})}

	worker := NewRecurringWorker(s, b, time.Hour, true)
	if err := worker.tickOnce(ctx); err != nil {
		t.Fatalf("tickOnce: %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, ожидали retry внешнего POST", calls)
	}
	assertRecurringPayment(t, s, userID, "yk-recurring-retry-provider", intent.IntentKey, "pm-worker-retry", "pending")
}

func TestRecurringWorker_StalePendingIntentWithoutProviderIDDoesNotCharge(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)
	nextChargeAt := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	insertRecurringProfile(t, s, userID, "pm-worker-stale", "active", nextChargeAt)
	if err := s.SetPaymentEmail(ctx, userID, "stale@example.com"); err != nil {
		t.Fatalf("SetPaymentEmail: %v", err)
	}
	intent := PaymentInput{
		UserID:          userID,
		AmountKopecks:   199000,
		Currency:        "RUB",
		IntentKey:       recurringPaymentIntentKey(userID, nextChargeAt),
		Metadata:        recurringPaymentMetadata(userID),
		PaymentMethodID: "pm-worker-stale",
	}
	created, err := s.StartRecurringPaymentIntent(ctx, intent, intent.IntentKey, nextChargeAt)
	if err != nil {
		t.Fatalf("StartRecurringPaymentIntent: %v", err)
	}
	if !created {
		t.Fatalf("expected initial local intent")
	}
	_, err = s.pool.Exec(ctx,
		`UPDATE payments SET created_at = now() - INTERVAL '25 hours'
		 WHERE user_id = $1 AND payment_intent_key = $2`,
		userID, intent.IntentKey,
	)
	if err != nil {
		t.Fatalf("age recurring intent: %v", err)
	}
	_, err = s.pool.Exec(ctx,
		`UPDATE recurring_profiles SET updated_at = now() - INTERVAL '1 hour' WHERE user_id = $1`,
		userID,
	)
	if err != nil {
		t.Fatalf("release recurring lease: %v", err)
	}

	var calls int
	b := testBilling(Config{
		SubscriptionPriceKopecks: 199000,
		SubscriptionDescription:  "15 часов на 1 месяц",
		YookassaShopID:           "shop-1",
		YookassaSecretKey:        "secret-1",
		YookassaBaseURL:          "http://yookassa.test",
	})
	b.storage = s
	b.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"should-not-happen","status":"pending","paid":false,"amount":{"value":"1990.00","currency":"RUB"}}`))
		return w.Result(), nil
	})}

	worker := NewRecurringWorker(s, b, time.Hour, true)
	if err := worker.tickOnce(ctx); err != nil {
		t.Fatalf("tickOnce: %v", err)
	}
	if calls != 0 {
		t.Fatalf("external POST calls = %d, ожидали 0 после retry window", calls)
	}
	var status string
	var failedAt *time.Time
	if err := s.pool.QueryRow(ctx,
		`SELECT status, failed_at FROM payments WHERE user_id = $1 AND payment_intent_key = $2`,
		userID, intent.IntentKey,
	).Scan(&status, &failedAt); err != nil {
		t.Fatalf("query payment: %v", err)
	}
	if status != string(PaymentFailed) || failedAt == nil {
		t.Fatalf("payment status/failed_at = %q/%v, ожидали failed", status, failedAt)
	}
	profile := recurringProfile(t, s, userID)
	if profile.status != "blocked" || profile.lastError == nil {
		t.Fatalf("profile after stale intent = %#v", profile)
	}
}

func TestRecurringWorker_DisabledAfterClaimDoesNotCharge(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)
	nextChargeAt := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	insertRecurringProfile(t, s, userID, "pm-worker-disabled", "active", nextChargeAt)
	if err := s.SetPaymentEmail(ctx, userID, "disable@example.com"); err != nil {
		t.Fatalf("SetPaymentEmail: %v", err)
	}
	profiles, err := s.DueRecurringProfiles(ctx, 1)
	if err != nil {
		t.Fatalf("DueRecurringProfiles: %v", err)
	}
	if len(profiles) != 1 {
		t.Fatalf("profiles len = %d, ожидали 1", len(profiles))
	}
	changed, err := s.DisableRecurring(ctx, userID)
	if err != nil {
		t.Fatalf("DisableRecurring: %v", err)
	}
	if !changed {
		t.Fatal("DisableRecurring changed=false, ожидали true")
	}
	if methodID := recurringProfilePaymentMethodID(t, s, userID); methodID != nil {
		t.Fatalf("payment_method_id = %q, ожидали NULL после disable", *methodID)
	}

	var calls int
	b := testBilling(Config{
		SubscriptionPriceKopecks: 199000,
		SubscriptionDescription:  "15 часов на 1 месяц",
		YookassaShopID:           "shop-1",
		YookassaSecretKey:        "secret-1",
		YookassaBaseURL:          "http://yookassa.test",
	})
	b.storage = s
	b.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"should-not-happen","status":"pending","paid":false,"amount":{"value":"1990.00","currency":"RUB"}}`))
		return w.Result(), nil
	})}

	worker := NewRecurringWorker(s, b, time.Hour, true)
	worker.chargeProfile(ctx, profiles[0])
	if calls != 0 {
		t.Fatalf("external POST calls = %d, ожидали 0 после disable", calls)
	}
	var payments int
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM payments WHERE user_id = $1 AND payment_intent_key = $2`, userID, recurringPaymentIntentKey(userID, nextChargeAt)).Scan(&payments); err != nil {
		t.Fatalf("count payments: %v", err)
	}
	if payments != 0 {
		t.Fatalf("payments = %d, ожидали 0", payments)
	}
}

func TestRecurringWorker_NullPaymentMethodDoesNotCharge(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)
	nextChargeAt := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	insertRecurringProfile(t, s, userID, "pm-worker-null", "active", nextChargeAt)
	if _, err := s.pool.Exec(ctx,
		`UPDATE recurring_profiles
		 SET payment_method_id = NULL, updated_at = now() - INTERVAL '1 hour'
		 WHERE user_id = $1`,
		userID,
	); err != nil {
		t.Fatalf("null payment_method_id: %v", err)
	}
	if err := s.SetPaymentEmail(ctx, userID, "null-method@example.com"); err != nil {
		t.Fatalf("SetPaymentEmail: %v", err)
	}

	var calls int
	b := testBilling(Config{
		SubscriptionPriceKopecks: 199000,
		SubscriptionDescription:  "15 часов на 1 месяц",
		YookassaShopID:           "shop-1",
		YookassaSecretKey:        "secret-1",
		YookassaBaseURL:          "http://yookassa.test",
	})
	b.storage = s
	b.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"should-not-happen","status":"pending","paid":false,"amount":{"value":"1990.00","currency":"RUB"}}`))
		return w.Result(), nil
	})}

	worker := NewRecurringWorker(s, b, time.Hour, true)
	if err := worker.tickOnce(ctx); err != nil {
		t.Fatalf("tickOnce: %v", err)
	}
	if calls != 0 {
		t.Fatalf("external POST calls = %d, ожидали 0 при NULL payment_method_id", calls)
	}
	var payments int
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM payments WHERE user_id = $1`, userID).Scan(&payments); err != nil {
		t.Fatalf("count payments: %v", err)
	}
	if payments != 0 {
		t.Fatalf("payments = %d, ожидали 0", payments)
	}
	profile := recurringProfile(t, s, userID)
	if profile.status != "active" || profile.lastError == nil {
		t.Fatalf("profile after NULL payment method = %#v", profile)
	}
}

func TestDueRecurringProfilesConcurrentClaimOnce(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)
	nextChargeAt := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	insertRecurringProfile(t, s, userID, "pm-concurrent-claim", "active", nextChargeAt)

	const workers = 8
	start := make(chan struct{})
	results := make(chan int64, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			profiles, err := s.DueRecurringProfiles(ctx, 1)
			if err != nil {
				errs <- err
				return
			}
			for _, p := range profiles {
				results <- p.UserID
			}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Fatalf("DueRecurringProfiles: %v", err)
	}
	var claimed []int64
	for got := range results {
		claimed = append(claimed, got)
	}
	if len(claimed) != 1 || claimed[0] != userID {
		t.Fatalf("claimed = %#v, ожидали один claim userID=%d", claimed, userID)
	}
}

func TestFinalizeYookassaPayment_RenewsRecurringProfile(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)
	oldNextChargeAt := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	insertRecurringProfile(t, s, userID, "pm-renew-1", "charge_failed", oldNextChargeAt)

	in := PaymentInput{
		UserID:           userID,
		AmountKopecks:    199000,
		Currency:         "RUB",
		ProviderChargeID: "yk-recurring-renew-1",
		IntentKey:        recurringPaymentIntentKey(userID, oldNextChargeAt),
		Metadata:         recurringPaymentMetadata(userID),
		PaymentMethodID:  "pm-renew-1",
	}
	if err := s.CreatePendingPayment(ctx, in, in.IntentKey, ""); err != nil {
		t.Fatalf("CreatePendingPayment: %v", err)
	}

	activation, err := s.FinalizeYookassaPayment(ctx, "yk-recurring-renew-1", json.RawMessage(`{"metadata":{"auto_renew":"true"}}`), nil, 900, "", false)
	if err != nil {
		t.Fatalf("FinalizeYookassaPayment: %v", err)
	}
	if !activation.Changed || activation.UserID != userID {
		t.Fatalf("changed/user = %v/%d", activation.Changed, activation.UserID)
	}
	profile := recurringProfile(t, s, userID)
	if profile.status != "active" || profile.attempts != 0 || profile.lastError != nil || profile.lastPaymentID == nil {
		t.Fatalf("profile after renew = %#v", profile)
	}
	subs := subRows(t, s, userID)
	if len(subs) != 1 {
		t.Fatalf("subscriptions count = %d, ожидали 1", len(subs))
	}
	if !profile.nextChargeAt.Equal(subs[0].expires) {
		t.Fatalf("next_charge_at = %v, subscription.expires_at = %v", profile.nextChargeAt, subs[0].expires)
	}
}

func TestPaymentPoller_MarksRecurringCanceled(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)
	nextChargeAt := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	if err := s.RecordSuccessfulPayment(ctx, PaymentInput{
		UserID:           userID,
		AmountKopecks:    199000,
		Currency:         "RUB",
		TelegramChargeID: "tg-active-before-recurring-cancel",
		ProviderChargeID: "yk-active-before-recurring-cancel",
	}, 900); err != nil {
		t.Fatalf("RecordSuccessfulPayment: %v", err)
	}
	activeBefore := subRows(t, s, userID)
	if len(activeBefore) != 1 || !activeBefore[0].expires.After(time.Now()) {
		t.Fatalf("active subscription before cancel = %#v", activeBefore)
	}
	insertRecurringProfile(t, s, userID, "pm-canceled-1", "active", nextChargeAt)

	in := PaymentInput{
		UserID:           userID,
		AmountKopecks:    199000,
		Currency:         "RUB",
		ProviderChargeID: "yk-recurring-canceled-1",
		IntentKey:        recurringPaymentIntentKey(userID, nextChargeAt),
		Metadata:         recurringPaymentMetadata(userID),
		PaymentMethodID:  "pm-canceled-1",
	}
	if err := s.CreatePendingPayment(ctx, in, in.IntentKey, ""); err != nil {
		t.Fatalf("CreatePendingPayment: %v", err)
	}
	agePendingPayment(t, s, userID)

	b := testBilling(Config{
		SubscriptionMinutesPerPeriod: 900,
		YookassaShopID:               "shop-1",
		YookassaSecretKey:            "secret-1",
		YookassaBaseURL:              "http://yookassa.test",
	})
	b.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"yk-recurring-canceled-1",
			"status":"canceled",
			"paid":false,
			"amount":{"value":"1990.00","currency":"RUB"},
			"metadata":{"user_id":"` + strconv.FormatInt(userID, 10) + `","auto_renew":"true","recurring":"true"}
		}`))
		return w.Result(), nil
	})}

	var notified []int64
	poller := NewPaymentPoller(s, b, func(chatID int64, text string) error {
		notified = append(notified, chatID)
		return nil
	}, time.Hour)
	if err := poller.tickOnce(ctx); err != nil {
		t.Fatalf("tickOnce: %v", err)
	}

	assertRecurringPayment(t, s, userID, "yk-recurring-canceled-1", in.IntentKey, "pm-canceled-1", "failed")
	profile := recurringProfile(t, s, userID)
	if profile.status != "charge_failed" || profile.lastError == nil {
		t.Fatalf("profile after cancel = %#v", profile)
	}
	if !profile.nextChargeAt.After(time.Now()) {
		t.Fatalf("next_charge_at = %v, ожидали retry позже", profile.nextChargeAt)
	}
	activeAfter := subRows(t, s, userID)
	if len(activeAfter) != 1 || !activeAfter[0].expires.Equal(activeBefore[0].expires) {
		t.Fatalf("active subscription changed after recurring cancel: before=%#v after=%#v", activeBefore, activeAfter)
	}
	if len(notified) != 1 || notified[0] != userID {
		t.Fatalf("notified = %#v, ожидали [%d]", notified, userID)
	}
}

func TestPaymentPoller_BlocksRecurringPermissionRevoked(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)
	nextChargeAt := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	insertRecurringProfile(t, s, userID, "pm-revoked-1", "charging", nextChargeAt)

	in := PaymentInput{
		UserID:           userID,
		AmountKopecks:    199000,
		Currency:         "RUB",
		ProviderChargeID: "yk-recurring-revoked-1",
		IntentKey:        recurringPaymentIntentKey(userID, nextChargeAt),
		Metadata:         recurringPaymentMetadata(userID),
		PaymentMethodID:  "pm-revoked-1",
	}
	if err := s.CreatePendingPayment(ctx, in, in.IntentKey, ""); err != nil {
		t.Fatalf("CreatePendingPayment: %v", err)
	}
	agePendingPayment(t, s, userID)

	b := testBilling(Config{
		SubscriptionMinutesPerPeriod: 900,
		YookassaShopID:               "shop-1",
		YookassaSecretKey:            "secret-1",
		YookassaBaseURL:              "http://yookassa.test",
	})
	b.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"yk-recurring-revoked-1",
			"status":"canceled",
			"paid":false,
			"amount":{"value":"1990.00","currency":"RUB"},
			"metadata":{"user_id":"` + strconv.FormatInt(userID, 10) + `","auto_renew":"true","recurring":"true"},
			"cancellation_details":{"reason":"permission_revoked"}
		}`))
		return w.Result(), nil
	})}

	var notified []string
	poller := NewPaymentPoller(s, b, func(chatID int64, text string) error {
		if chatID != userID {
			t.Fatalf("notify chatID = %d, ожидали %d", chatID, userID)
		}
		notified = append(notified, text)
		return nil
	}, time.Hour)
	if err := poller.tickOnce(ctx); err != nil {
		t.Fatalf("tickOnce: %v", err)
	}

	assertRecurringPayment(t, s, userID, "yk-recurring-revoked-1", in.IntentKey, "pm-revoked-1", "failed")
	profile := recurringProfile(t, s, userID)
	if profile.status != "blocked" || profile.lastError == nil {
		t.Fatalf("profile after permission_revoked = %#v", profile)
	}
	if len(notified) != 1 || !strings.Contains(notified[0], "не будем повторять списание") {
		t.Fatalf("notified = %#v, ожидали blocked message", notified)
	}
}

func insertRecurringProfile(t *testing.T, s *Storage, userID int64, methodID string, status string, nextChargeAt time.Time) {
	t.Helper()
	_, err := s.pool.Exec(context.Background(),
		`INSERT INTO recurring_profiles (
		     user_id, payment_method_id, status, next_charge_at,
		     attempts_count, last_error, consent_at, updated_at
		 )
		 VALUES ($1, $2, $3, $4, 2, 'seed error', now(), now() - INTERVAL '1 hour')
		 ON CONFLICT (user_id) DO UPDATE
		 SET payment_method_id = EXCLUDED.payment_method_id,
		     status = EXCLUDED.status,
		     next_charge_at = EXCLUDED.next_charge_at,
		     attempts_count = EXCLUDED.attempts_count,
		     last_error = EXCLUDED.last_error,
		     updated_at = now() - INTERVAL '1 hour'`,
		userID, methodID, status, nextChargeAt,
	)
	if err != nil {
		t.Fatalf("insert recurring profile: %v", err)
	}
}

func assertRecurringPayment(t *testing.T, s *Storage, userID int64, providerID string, intentKey string, methodID string, status string) {
	t.Helper()
	var gotProviderID, gotIntentKey, gotMethodID, gotStatus string
	err := s.pool.QueryRow(context.Background(),
		`SELECT provider_payment_charge_id, payment_intent_key, payment_method_id, status
		 FROM payments WHERE user_id = $1 AND provider_payment_charge_id = $2`,
		userID, providerID,
	).Scan(&gotProviderID, &gotIntentKey, &gotMethodID, &gotStatus)
	if err != nil {
		t.Fatalf("query recurring payment: %v", err)
	}
	if gotProviderID != providerID || gotIntentKey != intentKey || gotMethodID != methodID || gotStatus != status {
		t.Fatalf("payment = %q/%q/%q/%q", gotProviderID, gotIntentKey, gotMethodID, gotStatus)
	}
}

func recurringProfile(t *testing.T, s *Storage, userID int64) struct {
	status        string
	nextChargeAt  time.Time
	attempts      int
	lastError     *string
	lastPaymentID *int64
} {
	t.Helper()
	var p struct {
		status        string
		nextChargeAt  time.Time
		attempts      int
		lastError     *string
		lastPaymentID *int64
	}
	err := s.pool.QueryRow(context.Background(),
		`SELECT status, next_charge_at, attempts_count, last_error, last_payment_id
		 FROM recurring_profiles WHERE user_id = $1`,
		userID,
	).Scan(&p.status, &p.nextChargeAt, &p.attempts, &p.lastError, &p.lastPaymentID)
	if err != nil {
		t.Fatalf("query recurring profile: %v", err)
	}
	return p
}
