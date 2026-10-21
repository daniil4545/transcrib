package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestPaymentEmail_RoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	email, err := s.PaymentEmail(ctx, userID)
	if err != nil {
		t.Fatalf("PaymentEmail empty: %v", err)
	}
	if email != "" {
		t.Fatalf("PaymentEmail empty = %q, expected an empty string", email)
	}

	if err := s.SetPaymentEmail(ctx, userID, "user@example.com"); err != nil {
		t.Fatalf("SetPaymentEmail: %v", err)
	}
	email, err = s.PaymentEmail(ctx, userID)
	if err != nil {
		t.Fatalf("PaymentEmail saved: %v", err)
	}
	if email != "user@example.com" {
		t.Fatalf("PaymentEmail = %q, expected user@example.com", email)
	}
}

func TestCreatePendingPayment_IdempotentByIntentAndProvider(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)

	in := PaymentInput{
		UserID:           userID,
		AmountKopecks:    199000,
		Currency:         "RUB",
		ProviderChargeID: "yk-pending-1",
		IntentKey:        "intent-1",
		RawPayload:       json.RawMessage(`{"status":"pending"}`),
	}

	if err := s.CreatePendingPayment(ctx, in, in.IntentKey, "user@example.com"); err != nil {
		t.Fatalf("CreatePendingPayment #1: %v", err)
	}
	if err := s.CreatePendingPayment(ctx, in, in.IntentKey, "user@example.com"); err != nil {
		t.Fatalf("CreatePendingPayment same intent: %v", err)
	}
	var count int
	if err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM payments WHERE user_id = $1`,
		userID,
	).Scan(&count); err != nil {
		t.Fatalf("count payments: %v", err)
	}
	if count != 1 {
		t.Fatalf("payments count = %d, expected 1", count)
	}

	_, err := s.pool.Exec(ctx,
		`UPDATE payments SET created_at = now() - INTERVAL '10 seconds' WHERE user_id = $1`,
		userID,
	)
	if err != nil {
		t.Fatalf("age pending payment: %v", err)
	}
	pending, err := s.PendingPayments(ctx, 10)
	if err != nil {
		t.Fatalf("PendingPayments: %v", err)
	}
	found := false
	for _, p := range pending {
		if p.UserID == userID {
			found = true
			if p.ProviderPaymentChargeID == nil || *p.ProviderPaymentChargeID != "yk-pending-1" {
				t.Fatalf("provider_payment_charge_id = %v, expected yk-pending-1", p.ProviderPaymentChargeID)
			}
			if p.PaymentIntentKey == nil || *p.PaymentIntentKey != "intent-1" {
				t.Fatalf("payment_intent_key = %v, expected intent-1", p.PaymentIntentKey)
			}
		}
	}
	if !found {
		t.Fatal("PendingPayments did not return the created pending payment")
	}
}

func TestHasFreshPendingPayment_DirectOnly(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)

	since := time.Now().Add(-30 * time.Minute)
	exists, err := s.HasFreshPendingPayment(ctx, userID, since)
	if err != nil {
		t.Fatalf("HasFreshPendingPayment empty: %v", err)
	}
	if exists {
		t.Fatal("fresh pending exists with no payments")
	}

	recurring := PaymentInput{
		UserID:           userID,
		AmountKopecks:    199000,
		Currency:         "RUB",
		ProviderChargeID: "yk-recurring-pending",
		IntentKey:        "recurring:123:456",
	}
	if err := s.CreatePendingPayment(ctx, recurring, recurring.IntentKey, "user@example.com"); err != nil {
		t.Fatalf("CreatePendingPayment recurring: %v", err)
	}
	exists, err = s.HasFreshPendingPayment(ctx, userID, since)
	if err != nil {
		t.Fatalf("HasFreshPendingPayment recurring: %v", err)
	}
	if exists {
		t.Fatal("recurring pending must not block a direct payment link")
	}

	direct := PaymentInput{
		UserID:           userID,
		AmountKopecks:    199000,
		Currency:         "RUB",
		ProviderChargeID: "yk-direct-pending",
		IntentKey:        "intent-direct-pending",
	}
	if err := s.CreatePendingPayment(ctx, direct, direct.IntentKey, "user@example.com"); err != nil {
		t.Fatalf("CreatePendingPayment direct: %v", err)
	}
	exists, err = s.HasFreshPendingPayment(ctx, userID, since)
	if err != nil {
		t.Fatalf("HasFreshPendingPayment direct: %v", err)
	}
	if !exists {
		t.Fatal("fresh direct pending not found")
	}

	if _, err := s.pool.Exec(ctx,
		`UPDATE payments
		 SET created_at = now() - INTERVAL '2 hours'
		 WHERE provider_payment_charge_id = 'yk-direct-pending'`,
	); err != nil {
		t.Fatalf("age direct pending: %v", err)
	}
	exists, err = s.HasFreshPendingPayment(ctx, userID, since)
	if err != nil {
		t.Fatalf("HasFreshPendingPayment old direct: %v", err)
	}
	if exists {
		t.Fatal("old direct pending must not be considered fresh")
	}
}

func TestStartDirectPaymentIntent_ConcurrentSameUser(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)

	start := make(chan struct{})
	errs := make(chan error, 2)
	created := make(chan bool, 2)
	for i := 0; i < 2; i++ {
		i := i
		go func() {
			<-start
			idemKey := fmt.Sprintf("direct-intent-%d", i)
			in := PaymentInput{
				UserID:         userID,
				AmountKopecks:  199000,
				Currency:       "RUB",
				IntentKey:      idemKey,
				InvoicePayload: payloadPrefix + fmt.Sprint(userID),
				Metadata:       json.RawMessage(`{"payment_email":"user@example.com"}`),
			}
			ok, err := s.StartDirectPaymentIntent(ctx, in, idemKey, "user@example.com", time.Now().Add(-30*time.Minute))
			errs <- err
			created <- ok
		}()
	}
	close(start)

	var createdCount int
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("StartDirectPaymentIntent concurrent: %v", err)
		}
		if <-created {
			createdCount++
		}
	}
	if createdCount != 1 {
		t.Fatalf("created direct intents = %d, expected 1", createdCount)
	}

	var idemKey string
	var providerID *string
	if err := s.pool.QueryRow(ctx,
		`SELECT payment_intent_key, provider_payment_charge_id
		 FROM payments
		 WHERE user_id = $1 AND status = 'pending'`,
		userID,
	).Scan(&idemKey, &providerID); err != nil {
		t.Fatalf("select direct intent: %v", err)
	}
	if providerID != nil {
		t.Fatalf("provider_payment_charge_id = %q, expected NULL before attach", *providerID)
	}

	payment := YookassaPayment{
		ID:            "yk-direct-attached",
		AmountKopecks: 199000,
		Currency:      "RUB",
		Raw:           json.RawMessage(`{"id":"yk-direct-attached","status":"pending"}`),
	}
	if err := s.AttachDirectProviderPayment(ctx, idemKey, payment, json.RawMessage(`{"payment_email":"user@example.com"}`)); err != nil {
		t.Fatalf("AttachDirectProviderPayment: %v", err)
	}

	var count int
	if err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*)
		 FROM payments
		 WHERE user_id = $1
		   AND status = 'pending'
		   AND provider_payment_charge_id = 'yk-direct-attached'`,
		userID,
	).Scan(&count); err != nil {
		t.Fatalf("count attached direct intent: %v", err)
	}
	if count != 1 {
		t.Fatalf("attached direct payments = %d, expected 1", count)
	}
}

func TestCreatePendingPayment_ConflictMismatch(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)

	in := PaymentInput{
		UserID:           userID,
		AmountKopecks:    199000,
		Currency:         "RUB",
		ProviderChargeID: "yk-conflict-1",
		IntentKey:        "intent-conflict-1",
	}
	if err := s.CreatePendingPayment(ctx, in, in.IntentKey, "user@example.com"); err != nil {
		t.Fatalf("CreatePendingPayment #1: %v", err)
	}

	sameProvider := in
	sameProvider.IntentKey = "intent-conflict-2"
	if err := s.CreatePendingPayment(ctx, sameProvider, sameProvider.IntentKey, "user@example.com"); err == nil {
		t.Fatal("CreatePendingPayment same provider with different intent: err=nil, expected an error")
	}

	sameIntent := in
	sameIntent.ProviderChargeID = "yk-conflict-2"
	if err := s.CreatePendingPayment(ctx, sameIntent, sameIntent.IntentKey, "user@example.com"); err == nil {
		t.Fatal("CreatePendingPayment same intent with different provider: err=nil, expected an error")
	}

	mismatchedInput := in
	mismatchedInput.IntentKey = "intent-from-input"
	if err := s.CreatePendingPayment(ctx, mismatchedInput, "intent-from-arg", "user@example.com"); err == nil {
		t.Fatal("CreatePendingPayment mismatched input/arg intent: err=nil, expected an error")
	}
}

func TestFinalizeYookassaPayment_Idempotent(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)

	in := PaymentInput{
		UserID:           userID,
		AmountKopecks:    199000,
		Currency:         "RUB",
		ProviderChargeID: "yk-finalize-1",
		IntentKey:        "intent-finalize-1",
		RawPayload:       json.RawMessage(`{"status":"pending"}`),
	}
	if err := s.CreatePendingPayment(ctx, in, in.IntentKey, "user@example.com"); err != nil {
		t.Fatalf("CreatePendingPayment: %v", err)
	}

	providerPaidAt := time.Now().UTC()
	activation, err := s.FinalizeYookassaPayment(ctx, "yk-finalize-1", json.RawMessage(`{"status":"succeeded"}`), &providerPaidAt, 900, "", false)
	if err != nil {
		t.Fatalf("FinalizeYookassaPayment #1: %v", err)
	}
	if !activation.Changed {
		t.Fatal("FinalizeYookassaPayment #1 changed=false, expected true")
	}
	if activation.UserID != userID {
		t.Fatalf("FinalizeYookassaPayment userID = %d, expected %d", activation.UserID, userID)
	}

	activation, err = s.FinalizeYookassaPayment(ctx, "yk-finalize-1", json.RawMessage(`{"status":"succeeded"}`), &providerPaidAt, 900, "", false)
	if err != nil {
		t.Fatalf("FinalizeYookassaPayment #2: %v", err)
	}
	if activation.Changed {
		t.Fatal("FinalizeYookassaPayment #2 changed=true, expected false")
	}
	if activation.UserID != userID {
		t.Fatalf("FinalizeYookassaPayment #2 userID = %d, expected %d", activation.UserID, userID)
	}

	pays := paymentRows(t, s, userID)
	if len(pays) != 1 {
		t.Fatalf("payments count = %d, expected 1", len(pays))
	}
	if pays[0].status != "succeeded" {
		t.Fatalf("payment.status = %q, expected succeeded", pays[0].status)
	}
	if pays[0].subID == nil {
		t.Fatal("payment.subscription_id = NULL, expected a link")
	}

	subs := subRows(t, s, userID)
	if len(subs) != 1 {
		t.Fatalf("subscriptions count = %d, expected 1", len(subs))
	}
	if *pays[0].subID != subs[0].id {
		t.Fatalf("payment.subscription_id = %d, expected %d", *pays[0].subID, subs[0].id)
	}
	if subs[0].minutesLimit != 900 || subs[0].minutesBalance != 900 {
		t.Fatalf("subscription minutes = %d/%d, expected 900/900", subs[0].minutesBalance, subs[0].minutesLimit)
	}
	if subs[0].meetingsLimit != 15 || subs[0].meetingsBalance != 15 {
		t.Fatalf("legacy meetings = %d/%d, expected 15/15", subs[0].meetingsBalance, subs[0].meetingsLimit)
	}
}

func TestFinalizeYookassaPayment_RecurringProfileOptInGuard(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)

	cases := []struct {
		name        string
		autoRenew   bool
		methodSaved bool
		wantProfile bool
	}{
		{"opt_in_and_saved", true, true, true},
		{"opt_in_not_saved", true, false, false},
		{"saved_without_opt_in", false, true, false},
	}

	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			userID := newTestUser(t, s)
			cleanupPayments(t, s, userID)

			providerID := fmt.Sprintf("yk-recurring-%d-%d", i, -userID)
			methodID := fmt.Sprintf("pm-recurring-%d-%d", i, -userID)
			metadata := `{"user_id":"` + fmt.Sprint(userID) + `"}`
			if c.autoRenew {
				metadata = `{"user_id":"` + fmt.Sprint(userID) + `","auto_renew":"true"}`
			}

			in := PaymentInput{
				UserID:           userID,
				AmountKopecks:    199000,
				Currency:         "RUB",
				ProviderChargeID: providerID,
				IntentKey:        "intent-" + providerID,
				// auto_renew is stored in local metadata when the payment is created
				// (like paymentLinkMetadata in production); finalize reads the opt-in from here.
				Metadata:   json.RawMessage(metadata),
				RawPayload: json.RawMessage(`{"status":"pending"}`),
			}
			if err := s.CreatePendingPayment(ctx, in, in.IntentKey, "user@example.com"); err != nil {
				t.Fatalf("CreatePendingPayment: %v", err)
			}

			raw := json.RawMessage(fmt.Sprintf(`{
				"id":%q,
				"status":"succeeded",
				"paid":true,
				"amount":{"value":"1990.00","currency":"RUB"},
				"payment_method":{"id":%q,"saved":%t},
				"metadata":%s
			}`, providerID, methodID, c.methodSaved, metadata))

			activation, err := s.FinalizeYookassaPayment(ctx, providerID, raw, nil, 900, methodID, c.methodSaved)
			if err != nil {
				t.Fatalf("FinalizeYookassaPayment: %v", err)
			}
			if !activation.Changed {
				t.Fatal("FinalizeYookassaPayment changed=false, expected true")
			}
			if activation.UserID != userID {
				t.Fatalf("FinalizeYookassaPayment userID = %d, expected %d", activation.UserID, userID)
			}

			gotMethodID := paymentMethodID(t, s, userID)
			if c.wantProfile {
				if gotMethodID == nil || *gotMethodID != methodID {
					t.Fatalf("payments.payment_method_id = %v, expected %q", gotMethodID, methodID)
				}
			} else if gotMethodID != nil {
				t.Fatalf("payments.payment_method_id = %v, expected NULL without auto_renew/saved", *gotMethodID)
			}

			profiles := recurringProfileCount(t, s, userID, methodID)
			if c.wantProfile && profiles != 1 {
				t.Fatalf("recurring_profiles count = %d, expected 1", profiles)
			}
			if !c.wantProfile && profiles != 0 {
				t.Fatalf("recurring_profiles count = %d, expected 0", profiles)
			}
		})
	}
}

func TestFinalizeYookassaPayment_RecurringUsesEffectiveExpiry(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)

	oldSubID := insertSubscription(t, s, userID, 5, 15)
	var effectiveExpires time.Time
	if err := s.pool.QueryRow(ctx,
		`UPDATE subscriptions
		 SET expires_at = now() + INTERVAL '180 days'
		 WHERE id = $1
		 RETURNING expires_at`,
		oldSubID,
	).Scan(&effectiveExpires); err != nil {
		t.Fatalf("extend old subscription: %v", err)
	}

	providerID := "yk-recurring-effective"
	methodID := "pm-recurring-effective"
	metadata := `{"user_id":"` + fmt.Sprint(userID) + `","auto_renew":"true"}`
	in := PaymentInput{
		UserID:           userID,
		AmountKopecks:    199000,
		Currency:         "RUB",
		ProviderChargeID: providerID,
		IntentKey:        "intent-" + providerID,
		Metadata:         json.RawMessage(metadata),
		RawPayload:       json.RawMessage(`{"status":"pending"}`),
	}
	if err := s.CreatePendingPayment(ctx, in, in.IntentKey, "user@example.com"); err != nil {
		t.Fatalf("CreatePendingPayment: %v", err)
	}

	activation, err := s.FinalizeYookassaPayment(ctx, providerID, json.RawMessage(`{"status":"succeeded"}`), nil, 900, methodID, true)
	if err != nil {
		t.Fatalf("FinalizeYookassaPayment: %v", err)
	}
	if !activation.Changed || !activation.RecurringActive {
		t.Fatalf("activation = %#v, expected changed recurring active", activation)
	}
	if !activation.ExpiresAt.Equal(effectiveExpires) || !activation.NextChargeAt.Equal(effectiveExpires) {
		t.Fatalf("activation dates = expires %v next %v, expected effective %v",
			activation.ExpiresAt, activation.NextChargeAt, effectiveExpires)
	}

	profile := recurringProfile(t, s, userID)
	if profile.status != "active" || !profile.nextChargeAt.Equal(effectiveExpires) {
		t.Fatalf("profile = %#v, expected active next_charge_at=%v", profile, effectiveExpires)
	}
	bal, err := s.Balance(ctx, userID, 180)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.ExpiresAt == nil || !bal.ExpiresAt.Equal(profile.nextChargeAt) {
		t.Fatalf("balance expires = %v, profile next = %v", bal.ExpiresAt, profile.nextChargeAt)
	}
}

func paymentMethodID(t *testing.T, s *Storage, userID int64) *string {
	t.Helper()
	var methodID *string
	err := s.pool.QueryRow(context.Background(),
		`SELECT payment_method_id FROM payments WHERE user_id = $1`,
		userID,
	).Scan(&methodID)
	if err != nil {
		t.Fatalf("select payment_method_id: %v", err)
	}
	return methodID
}

func recurringProfileCount(t *testing.T, s *Storage, userID int64, methodID string) int {
	t.Helper()
	var count int
	err := s.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM recurring_profiles WHERE user_id = $1 AND payment_method_id = $2`,
		userID, methodID,
	).Scan(&count)
	if err != nil {
		t.Fatalf("count recurring_profiles: %v", err)
	}
	return count
}

func recurringProfilePaymentMethodID(t *testing.T, s *Storage, userID int64) *string {
	t.Helper()
	var methodID *string
	err := s.pool.QueryRow(context.Background(),
		`SELECT payment_method_id FROM recurring_profiles WHERE user_id = $1`,
		userID,
	).Scan(&methodID)
	if err != nil {
		t.Fatalf("select recurring profile payment_method_id: %v", err)
	}
	return methodID
}

func cleanupRecurringProfiles(t *testing.T, s *Storage, userID int64) {
	t.Helper()
	var exists bool
	if err := s.pool.QueryRow(context.Background(),
		`SELECT to_regclass('public.recurring_profiles') IS NOT NULL`,
	).Scan(&exists); err != nil {
		t.Logf("check recurring_profiles: %v", err)
		return
	}
	if !exists {
		return
	}
	if _, err := s.pool.Exec(context.Background(), `DELETE FROM recurring_profiles WHERE user_id = $1`, userID); err != nil {
		t.Logf("cleanup recurring_profiles: %v", err)
	}
}

func TestDisableRecurring_ChangedBool(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)
	insertRecurringProfile(t, s, userID, "pm-disable-bool", "active", time.Now().Add(24*time.Hour))

	changed, err := s.DisableRecurring(ctx, userID)
	if err != nil {
		t.Fatalf("DisableRecurring #1: %v", err)
	}
	if !changed {
		t.Fatal("DisableRecurring #1 changed=false, expected true")
	}
	profile := recurringProfile(t, s, userID)
	if profile.status != "disabled" {
		t.Fatalf("profile.status = %q, expected disabled", profile.status)
	}
	if methodID := recurringProfilePaymentMethodID(t, s, userID); methodID != nil {
		t.Fatalf("payment_method_id = %q, expected NULL after disable", *methodID)
	}

	changed, err = s.DisableRecurring(ctx, userID)
	if err != nil {
		t.Fatalf("DisableRecurring #2: %v", err)
	}
	if changed {
		t.Fatal("DisableRecurring #2 changed=true, expected false")
	}

	missingUserID := userID - 1000000
	changed, err = s.DisableRecurring(ctx, missingUserID)
	if err != nil {
		t.Fatalf("DisableRecurring missing: %v", err)
	}
	if changed {
		t.Fatal("DisableRecurring missing changed=true, expected false")
	}
}

func TestFinalizeYookassaPayment_DisabledRecurringNotReactivated(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)
	nextChargeAt := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	insertRecurringProfile(t, s, userID, "pm-disabled-inflight", "disabled", nextChargeAt)

	in := PaymentInput{
		UserID:           userID,
		AmountKopecks:    199000,
		Currency:         "RUB",
		ProviderChargeID: "yk-disabled-inflight",
		IntentKey:        recurringPaymentIntentKey(userID, nextChargeAt),
		Metadata:         recurringPaymentMetadata(userID),
		PaymentMethodID:  "pm-disabled-inflight",
	}
	if err := s.CreatePendingPayment(ctx, in, in.IntentKey, "user@example.com"); err != nil {
		t.Fatalf("CreatePendingPayment: %v", err)
	}

	activation, err := s.FinalizeYookassaPayment(ctx, in.ProviderChargeID, json.RawMessage(`{"payment_method":{"id":"pm-disabled-inflight-new","saved":true},"metadata":{"auto_renew":"true","recurring":"true"}}`), nil, 900, "pm-disabled-inflight-new", true)
	if err != nil {
		t.Fatalf("FinalizeYookassaPayment: %v", err)
	}
	if !activation.Changed || activation.RecurringActive {
		t.Fatalf("activation = %+v, expected changed without recurring reactivation", activation)
	}
	profile := recurringProfile(t, s, userID)
	if profile.status != "disabled" {
		t.Fatalf("profile.status = %q, expected disabled", profile.status)
	}
	if len(subRows(t, s, userID)) != 1 {
		t.Fatalf("subscriptions count = %d, expected 1", len(subRows(t, s, userID)))
	}
}

// blocked requires a manual check: a late success of an in-flight recurring payment
// extends the subscription, but must not silently reactivate the profile.
func TestFinalizeYookassaPayment_BlockedRecurringNotReactivated(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)
	nextChargeAt := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	insertRecurringProfile(t, s, userID, "pm-blocked-inflight", "blocked", nextChargeAt)

	in := PaymentInput{
		UserID:           userID,
		AmountKopecks:    199000,
		Currency:         "RUB",
		ProviderChargeID: "yk-blocked-inflight",
		IntentKey:        recurringPaymentIntentKey(userID, nextChargeAt),
		Metadata:         recurringPaymentMetadata(userID),
		PaymentMethodID:  "pm-blocked-inflight",
	}
	if err := s.CreatePendingPayment(ctx, in, in.IntentKey, "user@example.com"); err != nil {
		t.Fatalf("CreatePendingPayment: %v", err)
	}

	activation, err := s.FinalizeYookassaPayment(ctx, in.ProviderChargeID, json.RawMessage(`{"metadata":{"auto_renew":"true","recurring":"true"}}`), nil, 900, "", false)
	if err != nil {
		t.Fatalf("FinalizeYookassaPayment: %v", err)
	}
	if !activation.Changed || activation.RecurringActive {
		t.Fatalf("activation = %+v, expected changed without recurring reactivation", activation)
	}
	profile := recurringProfile(t, s, userID)
	if profile.status != "blocked" {
		t.Fatalf("profile.status = %q, expected blocked", profile.status)
	}
	if len(subRows(t, s, userID)) != 1 {
		t.Fatalf("subscriptions count = %d, expected 1", len(subRows(t, s, userID)))
	}
}

// TestFinalizeYookassaPayment_ConcurrentSameUser: two parallel final
// payments of one user create two independent subscription rows, each with its own
// expires_at ~ now()+30 days, without races/lost updates.
func TestFinalizeYookassaPayment_ConcurrentSameUser(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)

	inputs := []PaymentInput{
		{
			UserID:           userID,
			AmountKopecks:    199000,
			Currency:         "RUB",
			ProviderChargeID: "yk-concurrent-1",
			IntentKey:        "intent-concurrent-1",
		},
		{
			UserID:           userID,
			AmountKopecks:    199000,
			Currency:         "RUB",
			ProviderChargeID: "yk-concurrent-2",
			IntentKey:        "intent-concurrent-2",
		},
	}
	for _, in := range inputs {
		if err := s.CreatePendingPayment(ctx, in, in.IntentKey, "user@example.com"); err != nil {
			t.Fatalf("CreatePendingPayment %s: %v", in.ProviderChargeID, err)
		}
	}

	before := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, len(inputs))
	for _, in := range inputs {
		in := in
		wg.Add(1)
		go func() {
			defer wg.Done()
			activation, err := s.FinalizeYookassaPayment(ctx, in.ProviderChargeID, json.RawMessage(`{"status":"succeeded"}`), nil, 900, "", false)
			if err != nil {
				errs <- err
				return
			}
			if !activation.Changed {
				errs <- fmt.Errorf("payment %s changed=false", in.ProviderChargeID)
				return
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("FinalizeYookassaPayment concurrent: %v", err)
		}
	}

	subs := subRows(t, s, userID)
	if len(subs) != 2 {
		t.Fatalf("subscriptions count = %d, expected 2", len(subs))
	}
	wantLo := before.Add(30 * 24 * time.Hour).Add(-1 * time.Hour)
	wantHi := time.Now().Add(30 * 24 * time.Hour).Add(1 * time.Hour)
	for i, sub := range subs {
		if sub.expires.Before(wantLo) || sub.expires.After(wantHi) {
			t.Errorf("subscriptions[%d].expires = %v, expected ~ now()+30 days (window %v..%v)", i, sub.expires, wantLo, wantHi)
		}
	}
}

func TestFinalizeYookassaPayment_IndependentOfActive(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)

	oldSubID := insertSubscription(t, s, userID, 5, 15)
	subs := subRows(t, s, userID)
	if len(subs) != 1 {
		t.Fatalf("initial subscriptions = %d, expected 1", len(subs))
	}
	oldExpires := subs[0].expires

	before := time.Now()
	in := PaymentInput{
		UserID:           userID,
		AmountKopecks:    199000,
		Currency:         "RUB",
		ProviderChargeID: "yk-finalize-independent",
		IntentKey:        "intent-finalize-independent",
		RawPayload:       json.RawMessage(`{"status":"pending"}`),
	}
	if err := s.CreatePendingPayment(ctx, in, in.IntentKey, "user@example.com"); err != nil {
		t.Fatalf("CreatePendingPayment: %v", err)
	}
	activation, err := s.FinalizeYookassaPayment(ctx, "yk-finalize-independent", json.RawMessage(`{"status":"succeeded"}`), nil, 900, "", false)
	if err != nil {
		t.Fatalf("FinalizeYookassaPayment: %v", err)
	}
	if !activation.Changed {
		t.Fatal("FinalizeYookassaPayment changed=false, expected true")
	}

	subs = subRows(t, s, userID)
	if len(subs) != 2 {
		t.Fatalf("subscriptions count = %d, expected 2", len(subs))
	}
	newSub := subs[0]
	foundNewSub := false
	for _, sub := range subs {
		if sub.id != oldSubID {
			newSub = sub
			foundNewSub = true
		} else if !sub.expires.Equal(oldExpires) {
			t.Errorf("old subscription expires_at = %v, expected unchanged %v", sub.expires, oldExpires)
		}
	}
	if !foundNewSub {
		t.Fatal("new subscription row not found")
	}
	if newSub.minutesLimit != 900 || newSub.minutesBalance != 900 {
		t.Fatalf("new subscription minutes = %d/%d, expected 900/900", newSub.minutesBalance, newSub.minutesLimit)
	}

	// The new period is independent of the old one: expires ~ now()+30 days, NOT old+30 days.
	wantLo := before.Add(30 * 24 * time.Hour).Add(-1 * time.Hour)
	wantHi := time.Now().Add(30 * 24 * time.Hour).Add(1 * time.Hour)
	if newSub.expires.Before(wantLo) || newSub.expires.After(wantHi) {
		t.Fatalf("new expires_at = %v, expected ~ now()+30 days in window %v..%v, without a chain from the old subscription",
			newSub.expires, wantLo, wantHi)
	}
}
