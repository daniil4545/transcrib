package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPaymentPoller_FinalizesSucceededPayment(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)

	in := PaymentInput{
		UserID:           userID,
		AmountKopecks:    199000,
		Currency:         "RUB",
		ProviderChargeID: "yk-poll-succeeded",
		IntentKey:        "intent-poll-succeeded",
		RawPayload:       json.RawMessage(`{"status":"pending"}`),
		Metadata:         recurringPaymentMetadata(userID),
	}
	if err := s.CreatePendingPayment(ctx, in, in.IntentKey, "user@example.com"); err != nil {
		t.Fatalf("CreatePendingPayment: %v", err)
	}
	agePendingPayment(t, s, userID)

	b := testBilling(Config{
		SubscriptionMinutesPerPeriod: 900,
		PaymentReturnURL:             "https://transcrib.example.com/payments/return",
		YookassaShopID:               "shop-1",
		YookassaSecretKey:            "secret-1",
		YookassaBaseURL:              "http://yookassa.test",
	})
	b.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"yk-poll-succeeded",
				"status":"succeeded",
				"paid":true,
				"amount":{"value":"1990.00","currency":"RUB"},
				"captured_at":"2026-06-30T01:02:03.000Z",
				"payment_method":{"id":"pm-poll-succeeded","saved":true},
				"metadata":{"user_id":"` + strconv.FormatInt(userID, 10) + `","auto_renew":"true"}
			}`))
		return w.Result(), nil
	})}

	var notified []struct {
		chatID int64
		text   string
	}
	poller := NewPaymentPoller(s, b, func(chatID int64, text string) error {
		notified = append(notified, struct {
			chatID int64
			text   string
		}{chatID: chatID, text: text})
		return nil
	}, time.Hour)
	if err := poller.tickOnce(ctx); err != nil {
		t.Fatalf("tickOnce: %v", err)
	}

	pays := paymentRows(t, s, userID)
	if len(pays) != 1 {
		t.Fatalf("payments count = %d, want 1", len(pays))
	}
	if pays[0].status != "succeeded" {
		t.Fatalf("payment.status = %q, want succeeded", pays[0].status)
	}
	if pays[0].subID == nil {
		t.Fatal("payment.subscription_id = NULL, want a subscription")
	}
	subs := subRows(t, s, userID)
	if len(subs) != 1 {
		t.Fatalf("subscriptions count = %d, want 1", len(subs))
	}
	if subs[0].minutesLimit != 900 || subs[0].minutesBalance != 900 {
		t.Fatalf("subscription minutes = %d/%d, want 900/900", subs[0].minutesBalance, subs[0].minutesLimit)
	}
	if len(notified) != 1 || notified[0].chatID != userID {
		t.Fatalf("notified = %#v, want chatID %d", notified, userID)
	}
	if !strings.Contains(notified[0].text, "Следующая оплата") || !strings.Contains(notified[0].text, subs[0].expires.Format("02.01.2006")) {
		t.Fatalf("notify text = %q, want the next payment date", notified[0].text)
	}

	providerPaidAt := paymentProviderPaidAt(t, s, userID)
	if providerPaidAt == nil || providerPaidAt.UTC().Format(time.RFC3339) != "2026-06-30T01:02:03Z" {
		t.Fatalf("provider_paid_at = %v, want captured_at from YooKassa", providerPaidAt)
	}

	if err := poller.tickOnce(ctx); err != nil {
		t.Fatalf("tickOnce #2: %v", err)
	}
	if len(subRows(t, s, userID)) != 1 {
		t.Fatal("repeated polling created a second subscription")
	}
	if len(notified) != 1 {
		t.Fatalf("repeated polling sent an extra notification: %#v", notified)
	}
}

func TestPaymentPoller_PollsOldPendingPayment(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)

	in := PaymentInput{
		UserID:           userID,
		AmountKopecks:    199000,
		Currency:         "RUB",
		ProviderChargeID: "yk-poll-old",
		IntentKey:        "intent-poll-old",
	}
	if err := s.CreatePendingPayment(ctx, in, in.IntentKey, "user@example.com"); err != nil {
		t.Fatalf("CreatePendingPayment: %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE payments SET created_at = now() - INTERVAL '48 hours' WHERE user_id = $1`,
		userID,
	); err != nil {
		t.Fatalf("age old pending payment: %v", err)
	}

	b := testBilling(Config{
		SubscriptionMinutesPerPeriod: 900,
		PaymentReturnURL:             "https://transcrib.example.com/payments/return",
		YookassaShopID:               "shop-1",
		YookassaSecretKey:            "secret-1",
		YookassaBaseURL:              "http://yookassa.test",
	})
	var calls int
	b.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"yk-poll-old",
			"status":"pending",
			"paid":false,
			"amount":{"value":"1990.00","currency":"RUB"}
		}`))
		return w.Result(), nil
	})}

	poller := NewPaymentPoller(s, b, func(chatID int64, text string) error { return nil }, time.Hour)
	if err := poller.tickOnce(ctx); err != nil {
		t.Fatalf("tickOnce: %v", err)
	}
	if calls != 1 {
		t.Fatalf("poll calls = %d, want 1 for pending older than 24 hours", calls)
	}
}

func TestPaymentPoller_DoesNotFinalizeMismatchedPayment(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)

	in := PaymentInput{
		UserID:           userID,
		AmountKopecks:    199000,
		Currency:         "RUB",
		ProviderChargeID: "yk-poll-mismatch",
		IntentKey:        "intent-poll-mismatch",
	}
	if err := s.CreatePendingPayment(ctx, in, in.IntentKey, "user@example.com"); err != nil {
		t.Fatalf("CreatePendingPayment: %v", err)
	}
	agePendingPayment(t, s, userID)

	b := testBilling(Config{
		SubscriptionMinutesPerPeriod: 900,
		PaymentReturnURL:             "https://transcrib.example.com/payments/return",
		YookassaShopID:               "shop-1",
		YookassaSecretKey:            "secret-1",
		YookassaBaseURL:              "http://yookassa.test",
	})
	b.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"yk-poll-mismatch",
			"status":"succeeded",
			"paid":true,
			"amount":{"value":"10.00","currency":"RUB"},
			"metadata":{"user_id":"` + strconv.FormatInt(userID, 10) + `"}
		}`))
		return w.Result(), nil
	})}

	poller := NewPaymentPoller(s, b, func(chatID int64, text string) error {
		t.Fatalf("notify called for mismatched payment: %d %s", chatID, text)
		return nil
	}, time.Hour)
	if err := poller.tickOnce(ctx); err != nil {
		t.Fatalf("tickOnce: %v", err)
	}

	pays := paymentRows(t, s, userID)
	if len(pays) != 1 {
		t.Fatalf("payments count = %d, want 1", len(pays))
	}
	if pays[0].status != "pending" {
		t.Fatalf("payment.status = %q, want pending", pays[0].status)
	}
	if len(subRows(t, s, userID)) != 0 {
		t.Fatal("mismatched payment created a subscription")
	}
}

func TestSamePaymentIntent_StrictMetadataForIntent(t *testing.T) {
	local := Payment{
		UserID:                  42,
		Amount:                  199000,
		Currency:                "RUB",
		ProviderPaymentChargeID: ptrString("yk-strict"),
		PaymentIntentKey:        ptrString("intent-strict"),
	}
	cases := []struct {
		name     string
		metadata json.RawMessage
		want     bool
	}{
		{"missing", nil, false},
		{"null", json.RawMessage(`null`), false},
		{"empty_user", json.RawMessage(`{"user_id":""}`), false},
		{"wrong_user", json.RawMessage(`{"user_id":"43"}`), false},
		{"bad_user", json.RawMessage(`{"user_id":"abc"}`), false},
		{"match", json.RawMessage(`{"user_id":"42"}`), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := samePaymentIntent(local, YookassaPayment{
				ID:            "yk-strict",
				AmountKopecks: 199000,
				Currency:      "RUB",
				Metadata:      tc.metadata,
			})
			if got != tc.want {
				t.Fatalf("samePaymentIntent = %v, want %v", got, tc.want)
			}
		})
	}
}

func ptrString(v string) *string { return &v }

func TestPaymentPoller_FailsCanceledPayment(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)

	in := PaymentInput{
		UserID:           userID,
		AmountKopecks:    199000,
		Currency:         "RUB",
		ProviderChargeID: "yk-poll-canceled",
		IntentKey:        "intent-poll-canceled",
		RawPayload:       json.RawMessage(`{"status":"pending"}`),
	}
	if err := s.CreatePendingPayment(ctx, in, in.IntentKey, "user@example.com"); err != nil {
		t.Fatalf("CreatePendingPayment: %v", err)
	}
	agePendingPayment(t, s, userID)

	b := testBilling(Config{
		SubscriptionMinutesPerPeriod: 900,
		PaymentReturnURL:             "https://transcrib.example.com/payments/return",
		YookassaShopID:               "shop-1",
		YookassaSecretKey:            "secret-1",
		YookassaBaseURL:              "http://yookassa.test",
	})
	b.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"yk-poll-canceled",
			"status":"canceled",
			"paid":false,
			"amount":{"value":"1990.00","currency":"RUB"},
			"metadata":{"user_id":"` + strconv.FormatInt(userID, 10) + `"}
		}`))
		return w.Result(), nil
	})}

	poller := NewPaymentPoller(s, b, func(chatID int64, text string) error {
		t.Fatalf("notify called for canceled payment: %d %s", chatID, text)
		return nil
	}, time.Hour)
	if err := poller.tickOnce(ctx); err != nil {
		t.Fatalf("tickOnce: %v", err)
	}

	pays := paymentRows(t, s, userID)
	if len(pays) != 1 {
		t.Fatalf("payments count = %d, want 1", len(pays))
	}
	if pays[0].status != "failed" {
		t.Fatalf("payment.status = %q, want failed", pays[0].status)
	}
	if len(subRows(t, s, userID)) != 0 {
		t.Fatal("canceled payment created a subscription")
	}
}

func TestPaymentPoller_CanceledDefaultOnDirectDoesNotTouchRecurringProfile(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)
	nextCharge := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	insertRecurringProfile(t, s, userID, "pm-direct-canceled", "active", nextCharge)

	in := PaymentInput{
		UserID:           userID,
		AmountKopecks:    199000,
		Currency:         "RUB",
		ProviderChargeID: "yk-direct-canceled-auto",
		IntentKey:        "intent-direct-canceled-auto",
		RawPayload:       json.RawMessage(`{"status":"pending"}`),
		Metadata:         recurringPaymentMetadata(userID),
	}
	if err := s.CreatePendingPayment(ctx, in, in.IntentKey, "user@example.com"); err != nil {
		t.Fatalf("CreatePendingPayment: %v", err)
	}
	agePendingPayment(t, s, userID)

	b := testBilling(Config{
		SubscriptionMinutesPerPeriod: 900,
		PaymentReturnURL:             "https://transcrib.example.com/payments/return",
		YookassaShopID:               "shop-1",
		YookassaSecretKey:            "secret-1",
		YookassaBaseURL:              "http://yookassa.test",
	})
	b.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"yk-direct-canceled-auto",
			"status":"canceled",
			"paid":false,
			"amount":{"value":"1990.00","currency":"RUB"},
			"cancellation_details":{"reason":"insufficient_funds"},
			"metadata":{"user_id":"` + strconv.FormatInt(userID, 10) + `","auto_renew":"true"}
		}`))
		return w.Result(), nil
	})}

	poller := NewPaymentPoller(s, b, func(chatID int64, text string) error {
		t.Fatalf("notify called for direct canceled payment: %d %s", chatID, text)
		return nil
	}, time.Hour)
	if err := poller.tickOnce(ctx); err != nil {
		t.Fatalf("tickOnce: %v", err)
	}

	pays := paymentRows(t, s, userID)
	if len(pays) != 1 || pays[0].status != string(PaymentFailed) {
		t.Fatalf("payments = %#v, want one failed payment", pays)
	}
	profile := recurringProfile(t, s, userID)
	if profile.status != "active" || !profile.nextChargeAt.Equal(nextCharge) {
		t.Fatalf("profile = %#v, want active without a next_charge_at change", profile)
	}
}

func TestPaymentPoller_CanceledRecurringMarksFailure(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)
	nextCharge := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	insertRecurringProfile(t, s, userID, "pm-recurring-failed", "charging", nextCharge)
	intentKey := recurringPaymentIntentKey(userID, nextCharge)

	in := PaymentInput{
		UserID:           userID,
		AmountKopecks:    199000,
		Currency:         "RUB",
		ProviderChargeID: "yk-recurring-canceled",
		IntentKey:        intentKey,
		RawPayload:       json.RawMessage(`{"status":"pending"}`),
		Metadata:         recurringPaymentMetadata(userID),
	}
	if err := s.CreatePendingPayment(ctx, in, in.IntentKey, "user@example.com"); err != nil {
		t.Fatalf("CreatePendingPayment: %v", err)
	}
	agePendingPayment(t, s, userID)

	b := testBilling(Config{
		SubscriptionMinutesPerPeriod: 900,
		PaymentReturnURL:             "https://transcrib.example.com/payments/return",
		YookassaShopID:               "shop-1",
		YookassaSecretKey:            "secret-1",
		YookassaBaseURL:              "http://yookassa.test",
	})
	b.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"yk-recurring-canceled",
			"status":"canceled",
			"paid":false,
			"amount":{"value":"1990.00","currency":"RUB"},
			"cancellation_details":{"reason":"insufficient_funds"},
			"metadata":{"user_id":"` + strconv.FormatInt(userID, 10) + `","recurring":"true"}
		}`))
		return w.Result(), nil
	})}

	var notified []string
	poller := NewPaymentPoller(s, b, func(chatID int64, text string) error {
		notified = append(notified, text)
		return nil
	}, time.Hour)
	if err := poller.tickOnce(ctx); err != nil {
		t.Fatalf("tickOnce: %v", err)
	}

	profile := recurringProfile(t, s, userID)
	if profile.status != "charge_failed" || profile.lastError == nil {
		t.Fatalf("profile = %#v, want charge_failed with last_error", profile)
	}
	if len(notified) != 1 || !strings.Contains(notified[0], "Не удалось автопродлить") {
		t.Fatalf("notified = %#v, want recurring failure notification", notified)
	}
}

func TestPaymentPoller_CanceledRecurringPermissionRevokedBlocksProfile(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)
	nextCharge := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	insertRecurringProfile(t, s, userID, "pm-recurring-revoked", "charging", nextCharge)
	intentKey := recurringPaymentIntentKey(userID, nextCharge)

	in := PaymentInput{
		UserID:           userID,
		AmountKopecks:    199000,
		Currency:         "RUB",
		ProviderChargeID: "yk-recurring-revoked",
		IntentKey:        intentKey,
		RawPayload:       json.RawMessage(`{"status":"pending"}`),
		Metadata:         recurringPaymentMetadata(userID),
	}
	if err := s.CreatePendingPayment(ctx, in, in.IntentKey, "user@example.com"); err != nil {
		t.Fatalf("CreatePendingPayment: %v", err)
	}
	agePendingPayment(t, s, userID)

	b := testBilling(Config{
		SubscriptionMinutesPerPeriod: 900,
		PaymentReturnURL:             "https://transcrib.example.com/payments/return",
		YookassaShopID:               "shop-1",
		YookassaSecretKey:            "secret-1",
		YookassaBaseURL:              "http://yookassa.test",
	})
	b.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"yk-recurring-revoked",
			"status":"canceled",
			"paid":false,
			"amount":{"value":"1990.00","currency":"RUB"},
			"cancellation_details":{"reason":"permission_revoked"},
			"metadata":{"user_id":"` + strconv.FormatInt(userID, 10) + `","recurring":"true"}
		}`))
		return w.Result(), nil
	})}

	poller := NewPaymentPoller(s, b, func(chatID int64, text string) error { return nil }, time.Hour)
	if err := poller.tickOnce(ctx); err != nil {
		t.Fatalf("tickOnce: %v", err)
	}

	profile := recurringProfile(t, s, userID)
	if profile.status != "blocked" || profile.lastError == nil {
		t.Fatalf("profile = %#v, want blocked with last_error", profile)
	}
}

func TestPaymentPoller_CanceledIntentMismatchDoesNotMutatePaymentOrProfile(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)
	nextCharge := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	insertRecurringProfile(t, s, userID, "pm-mismatch", "active", nextCharge)

	in := PaymentInput{
		UserID:           userID,
		AmountKopecks:    199000,
		Currency:         "RUB",
		ProviderChargeID: "yk-canceled-mismatch",
		IntentKey:        "intent-canceled-mismatch",
		RawPayload:       json.RawMessage(`{"status":"pending"}`),
		Metadata:         recurringPaymentMetadata(userID),
	}
	if err := s.CreatePendingPayment(ctx, in, in.IntentKey, "user@example.com"); err != nil {
		t.Fatalf("CreatePendingPayment: %v", err)
	}
	agePendingPayment(t, s, userID)

	b := testBilling(Config{
		SubscriptionMinutesPerPeriod: 900,
		PaymentReturnURL:             "https://transcrib.example.com/payments/return",
		YookassaShopID:               "shop-1",
		YookassaSecretKey:            "secret-1",
		YookassaBaseURL:              "http://yookassa.test",
	})
	b.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"yk-canceled-mismatch",
			"status":"canceled",
			"paid":false,
			"amount":{"value":"1990.00","currency":"RUB"}
		}`))
		return w.Result(), nil
	})}

	poller := NewPaymentPoller(s, b, func(chatID int64, text string) error {
		t.Fatalf("notify called for mismatched canceled payment: %d %s", chatID, text)
		return nil
	}, time.Hour)
	if err := poller.tickOnce(ctx); err != nil {
		t.Fatalf("tickOnce: %v", err)
	}

	pays := paymentRows(t, s, userID)
	if len(pays) != 1 || pays[0].status != string(PaymentPending) {
		t.Fatalf("payments = %#v, want pending payment without mutation", pays)
	}
	profile := recurringProfile(t, s, userID)
	if profile.status != "active" || !profile.nextChargeAt.Equal(nextCharge) {
		t.Fatalf("profile = %#v, want active without mutation", profile)
	}
}

func TestRecurringCancellationRetryable(t *testing.T) {
	cases := []struct {
		reason string
		want   bool
	}{
		{"", true},
		{"insufficient_funds", true},
		{"general_decline", true},
		{"permission_revoked", false},
		{"mandate_revoked", false},
	}
	for _, tc := range cases {
		if got := recurringCancellationRetryable(tc.reason); got != tc.want {
			t.Fatalf("recurringCancellationRetryable(%q) = %v, want %v", tc.reason, got, tc.want)
		}
	}
}

func agePendingPayment(t *testing.T, s *Storage, userID int64) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE payments SET created_at = now() - INTERVAL '10 seconds' WHERE user_id = $1`,
		userID,
	); err != nil {
		t.Fatalf("age pending payment: %v", err)
	}
}

func paymentProviderPaidAt(t *testing.T, s *Storage, userID int64) *time.Time {
	t.Helper()
	var paidAt *time.Time
	err := s.pool.QueryRow(context.Background(),
		`SELECT provider_paid_at FROM payments WHERE user_id = $1`,
		userID,
	).Scan(&paidAt)
	if err != nil {
		t.Fatalf("select provider_paid_at: %v", err)
	}
	return paidAt
}
