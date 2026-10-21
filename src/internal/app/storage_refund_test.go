package app

import (
	"context"
	"errors"
	"testing"
	"time"
)

// F7 critical money node (db.md §5.9, plan-payments-slice §P2): two-phase
// refund in storage. Targeted tests ONLY for refund invariants:
// intent idempotency (BeginRefund), finalization idempotency (FinalizeRefund),
// deactivating exactly its own subscription (1:1 model), keeping access on failed/canceled,
// a sentinel for a non-refundable payment. The REST phase billing.Refund/admin trigger is behind a blocker
// (needs YooKassa credentials) and is not tested here. Integration tests that hit Postgres via
// DATABASE_URL; without it they Skip (newTestStorage).

// cleanupRefunds removes the FK refunds -> payments (plain FK, no ON DELETE CASCADE):
// cleanupPayments deletes payments, but refunds rows would block that DELETE.
// Registered AFTER cleanupPayments -> by LIFO t.Cleanup runs BEFORE it
// (cleanup order: refunds -> payments -> subscriptions -> user).
func cleanupRefunds(t *testing.T, s *Storage, userID int64) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := s.pool.Exec(context.Background(),
			`DELETE FROM refunds WHERE payment_id IN (SELECT id FROM payments WHERE user_id = $1)`,
			userID,
		); err != nil {
			t.Logf("cleanup refunds: %v", err)
		}
	})
}

// seedPayment inserts one successful payment + the linked subscription via §5.4
// (RecordSuccessfulPayment) and returns providerChargeID for BeginRefund plus
// the id of the linked subscription (to check deactivation of exactly its own 1:1 row).
func seedPayment(t *testing.T, s *Storage, userID int64, tgCharge, provCharge string) (providerChargeID string, subID int64) {
	t.Helper()
	in := PaymentInput{
		UserID:           userID,
		AmountKopecks:    199000,
		Currency:         "RUB",
		TelegramChargeID: tgCharge,
		ProviderChargeID: provCharge,
		InvoicePayload:   "sub:1",
	}
	if err := s.RecordSuccessfulPayment(context.Background(), in, 900); err != nil {
		t.Fatalf("seed payment: %v", err)
	}
	if err := s.pool.QueryRow(context.Background(),
		`SELECT subscription_id FROM payments WHERE user_id = $1 AND provider_payment_charge_id = $2`,
		userID, provCharge,
	).Scan(&subID); err != nil {
		t.Fatalf("seed payment lookup subscription_id: %v", err)
	}
	return provCharge, subID
}

// paymentStatusByCharge reads payments.status by provider_payment_charge_id.
func paymentStatusByCharge(t *testing.T, s *Storage, provCharge string) string {
	t.Helper()
	var status string
	if err := s.pool.QueryRow(context.Background(),
		`SELECT status FROM payments WHERE provider_payment_charge_id = $1`, provCharge,
	).Scan(&status); err != nil {
		t.Fatalf("read payment status: %v", err)
	}
	return status
}

// subActive reports whether the subscription is active (expires_at > now()).
func subActive(t *testing.T, s *Storage, subID int64) bool {
	t.Helper()
	var active bool
	if err := s.pool.QueryRow(context.Background(),
		`SELECT expires_at > now() FROM subscriptions WHERE id = $1`, subID,
	).Scan(&active); err != nil {
		t.Fatalf("read subscription active: %v", err)
	}
	return active
}

// refundCountByPayment counts refunds rows for a provider payment.
func refundCountByPayment(t *testing.T, s *Storage, provCharge string) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM refunds r JOIN payments p ON p.id = r.payment_id
		 WHERE p.provider_payment_charge_id = $1`, provCharge,
	).Scan(&n); err != nil {
		t.Fatalf("count refunds: %v", err)
	}
	return n
}

// refundProviderID reads provider_refund_id (nil = NULL).
func refundProviderID(t *testing.T, s *Storage, refundID int64) *string {
	t.Helper()
	var pid *string
	if err := s.pool.QueryRow(context.Background(),
		`SELECT provider_refund_id FROM refunds WHERE id = $1`, refundID,
	).Scan(&pid); err != nil {
		t.Fatalf("read provider_refund_id: %v", err)
	}
	return pid
}

// TestBeginRefund_DoubleIntentIdempotent: §5.9 phase 1: a double BeginRefund for one
// payment yields ONE pending row (refunds_active_idx, partial UNIQUE on payment_id).
// The second call returns the SAME refundID and idemKey (intent idempotency: one and the
// same Idempotence-Key survives our retry, protecting against a double charge at YooKassa).
// The table holds exactly one refund per payment. db.md §5.9 / docs/testing.md.
func TestBeginRefund_DoubleIntentIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)
	cleanupRefunds(t, s, userID)

	prov, _ := seedPayment(t, s, userID, "tg-dbl", "yk-dbl")

	id1, key1, amt1, err := s.BeginRefund(ctx, prov)
	if err != nil {
		t.Fatalf("BeginRefund #1: %v", err)
	}
	if amt1 != 199000 {
		t.Errorf("amount = %d, expected 199000 (full-only from payments)", amt1)
	}

	id2, key2, amt2, err := s.BeginRefund(ctx, prov)
	if err != nil {
		t.Fatalf("BeginRefund #2 (intent repeat): %v", err)
	}
	if id2 != id1 {
		t.Errorf("refundID #2 = %d, expected the same %d (intent idempotency)", id2, id1)
	}
	if key2 != key1 {
		t.Errorf("idemKey #2 = %q, expected the same %q (Idempotence-Key survives a retry)", key2, key1)
	}
	if amt2 != amt1 {
		t.Errorf("amount #2 = %d, expected the same %d", amt2, amt1)
	}
	if n := refundCountByPayment(t, s, prov); n != 1 {
		t.Errorf("refunds count = %d, expected 1 (refunds_active_idx suppresses the duplicate)", n)
	}
}

// TestFinalizeRefund_SucceededDeactivatesOwnSubscription: §5.9 phase 2 (succeeded):
// payment -> 'refunded' AND EXACTLY the subscription row bought by the payment is deactivated
// (1:1 model, expires_at<=now()). 1:1 isolation: the user has a SECOND payment/subscription,
// and refunding the first does NOT touch the second active period. A repeated FinalizeRefund is a no-op
// (guard status='pending'): payment/subscription do not change again. db.md §5.9.
func TestFinalizeRefund_SucceededDeactivatesOwnSubscription(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)
	cleanupRefunds(t, s, userID)

	prov1, sub1 := seedPayment(t, s, userID, "tg-fin-1", "yk-fin-1")
	insertRecurringProfile(t, s, userID, "pm-refund-1", "active", time.Now().Add(24*time.Hour))
	// A second paid period of the same user: must stay untouched.
	prov2, sub2 := seedPayment(t, s, userID, "tg-fin-2", "yk-fin-2")
	if sub1 == sub2 {
		t.Fatal("seed produced one subscription for two payments: the 1:1 model is violated in setup")
	}

	refundID, _, _, err := s.BeginRefund(ctx, prov1)
	if err != nil {
		t.Fatalf("BeginRefund: %v", err)
	}

	if err := s.FinalizeRefund(ctx, refundID, "yk-refund-1", RefundSucceeded, []byte(`{"status":"succeeded"}`)); err != nil {
		t.Fatalf("FinalizeRefund #1: %v", err)
	}
	if got := paymentStatusByCharge(t, s, prov1); got != string(PaymentRefunded) {
		t.Errorf("payment #1 status = %q, expected refunded", got)
	}
	if subActive(t, s, sub1) {
		t.Error("subscription #1 is active, expected deactivation (expires_at<=now())")
	}
	profile, ok, err := s.RecurringProfile(ctx, userID)
	if err != nil {
		t.Fatalf("RecurringProfile: %v", err)
	}
	if !ok {
		t.Fatalf("recurring profile not found")
	}
	if profile.Status != "disabled" || profile.DisabledAt == nil {
		t.Fatalf("recurring profile = %+v, expected disabled after refund", profile)
	}
	if profile.PaymentMethodID != "" {
		t.Fatalf("recurring profile payment method = %q, expected an empty token after refund", profile.PaymentMethodID)
	}
	var tokenErased bool
	if err := s.pool.QueryRow(ctx,
		`SELECT payment_method_id IS NULL FROM recurring_profiles WHERE user_id = $1`,
		userID,
	).Scan(&tokenErased); err != nil {
		t.Fatalf("check recurring token erased: %v", err)
	}
	if !tokenErased {
		t.Fatal("recurring profile payment_method_id not cleared after refund")
	}
	// 1:1 isolation: the second payment/period is not affected by the refund of the first.
	if got := paymentStatusByCharge(t, s, prov2); got != string(PaymentSucceeded) {
		t.Errorf("payment #2 status = %q, expected succeeded (refund of the first touched the second)", got)
	}
	if !subActive(t, s, sub2) {
		t.Error("subscription #2 is deactivated, expected active (refund of the first touched the second period)")
	}

	// Repeated finalization (repeated webhook/trigger): a no-op by the status='pending' guard.
	if err := s.FinalizeRefund(ctx, refundID, "yk-refund-1", RefundSucceeded, []byte(`{"status":"succeeded"}`)); err != nil {
		t.Fatalf("FinalizeRefund #2 (repeat): %v", err)
	}
	if got := paymentStatusByCharge(t, s, prov1); got != string(PaymentRefunded) {
		t.Errorf("payment #1 status after repeat = %q, expected refunded (idempotency)", got)
	}
	if subActive(t, s, sub1) {
		t.Error("subscription #1 is active after repeat: repeated finalization must not touch anything")
	}
}

// TestFinalizeRefund_FailedAndCanceledKeepAccess: §5.9 phase 2 (failed/canceled):
// money was NOT returned -> payment stays 'succeeded', subscription is active (access
// is kept). We check the two terminal non-succeeded YooKassa statuses symmetrically.
func TestFinalizeRefund_FailedAndCanceledKeepAccess(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name   string
		status RefundStatus
		tg     string
		prov   string
		refTag string
	}{
		{"failed", RefundFailed, "tg-fail", "yk-fail", "yk-r-fail"},
		{"canceled", RefundCanceled, "tg-cancel", "yk-cancel", "yk-r-cancel"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newTestStorage(t)
			userID := newTestUser(t, s)
			cleanupPayments(t, s, userID)
			cleanupRefunds(t, s, userID)

			prov, subID := seedPayment(t, s, userID, c.tg, c.prov)
			refundID, _, _, err := s.BeginRefund(ctx, prov)
			if err != nil {
				t.Fatalf("BeginRefund: %v", err)
			}

			if err := s.FinalizeRefund(ctx, refundID, c.refTag, c.status, []byte(`{"status":"`+string(c.status)+`"}`)); err != nil {
				t.Fatalf("FinalizeRefund(%s): %v", c.status, err)
			}
			if got := paymentStatusByCharge(t, s, prov); got != string(PaymentSucceeded) {
				t.Errorf("payment status = %q, expected succeeded (money not returned on %s)", got, c.status)
			}
			if !subActive(t, s, subID) {
				t.Errorf("subscription deactivated on %s, expected active (access is kept)", c.status)
			}
		})
	}
}

// TestBeginRefund_NotRefundable: §5.9: BeginRefund for a non-existent payment and for a
// payment not in succeeded status returns ErrPaymentNotRefundable (nothing to refund).
// The "not succeeded" class is checked on a pending payment via a direct INSERT.
func TestBeginRefund_NotRefundable(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)
	cleanupRefunds(t, s, userID)

	if _, _, _, err := s.BeginRefund(ctx, "yk-does-not-exist"); !errors.Is(err, ErrPaymentNotRefundable) {
		t.Errorf("BeginRefund(non-existent) err = %v, expected ErrPaymentNotRefundable", err)
	}

	// pending payment: no succeeded predicate, refund is impossible.
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO payments (user_id, status, amount, currency, provider_payment_charge_id)
		 VALUES ($1, 'pending', 199000, 'RUB', $2)`,
		userID, "yk-pending",
	); err != nil {
		t.Fatalf("insert pending payment: %v", err)
	}
	if _, _, _, err := s.BeginRefund(ctx, "yk-pending"); !errors.Is(err, ErrPaymentNotRefundable) {
		t.Errorf("BeginRefund(pending) err = %v, expected ErrPaymentNotRefundable", err)
	}
}

// TestFinalizeRefund_EmptyProviderIDStoredAsNull: §5.9: an empty providerRefundID
// (failed/canceled carry no id at YooKassa) is stored as NULL, not an empty string:
// otherwise several such rows would collide in refunds_provider_idx (partial UNIQUE,
// which does not count NULL as a collision). We check two refunds with an empty id for different
// payments: both are stored as NULL, the index does not fail.
func TestFinalizeRefund_EmptyProviderIDStoredAsNull(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)
	cleanupRefunds(t, s, userID)

	provA, _ := seedPayment(t, s, userID, "tg-null-a", "yk-null-a")
	provB, _ := seedPayment(t, s, userID, "tg-null-b", "yk-null-b")

	idA, _, _, err := s.BeginRefund(ctx, provA)
	if err != nil {
		t.Fatalf("BeginRefund A: %v", err)
	}
	idB, _, _, err := s.BeginRefund(ctx, provB)
	if err != nil {
		t.Fatalf("BeginRefund B: %v", err)
	}

	// Empty provider_refund_id on both: partial UNIQUE must not conflict on NULL.
	if err := s.FinalizeRefund(ctx, idA, "", RefundFailed, nil); err != nil {
		t.Fatalf("FinalizeRefund A (empty provider id): %v", err)
	}
	if err := s.FinalizeRefund(ctx, idB, "", RefundFailed, nil); err != nil {
		t.Fatalf("FinalizeRefund B (empty provider id): %v", err)
	}
	if pid := refundProviderID(t, s, idA); pid != nil {
		t.Errorf("provider_refund_id A = %q, expected NULL", *pid)
	}
	if pid := refundProviderID(t, s, idB); pid != nil {
		t.Errorf("provider_refund_id B = %q, expected NULL", *pid)
	}
}
