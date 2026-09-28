package app

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestMigration00014_AlignsOnlyActiveRecurringProfiles(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)

	activeUserID := newTestUser(t, s)
	activeSubID := insertSubscription(t, s, activeUserID, 5, 15)
	var effectiveExpires time.Time
	if err := s.pool.QueryRow(ctx,
		`UPDATE subscriptions
		 SET expires_at = now() + INTERVAL '180 days'
		 WHERE id = $1
		 RETURNING expires_at`,
		activeSubID,
	).Scan(&effectiveExpires); err != nil {
		t.Fatalf("extend active subscription: %v", err)
	}
	oldActiveNext := time.Now().Add(30 * 24 * time.Hour).UTC().Truncate(time.Second)
	insertRecurringProfile(t, s, activeUserID, "pm-migration-active", "active", oldActiveNext)

	failedUserID := newTestUser(t, s)
	failedSubID := insertSubscription(t, s, failedUserID, 5, 15)
	if _, err := s.pool.Exec(ctx,
		`UPDATE subscriptions
		 SET expires_at = now() + INTERVAL '180 days'
		 WHERE id = $1`,
		failedSubID,
	); err != nil {
		t.Fatalf("extend failed subscription: %v", err)
	}
	failedNext := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	insertRecurringProfile(t, s, failedUserID, "pm-migration-failed", "charge_failed", failedNext)

	upSQL := migrationUpSQL(t, "../../migrations/00014_align_recurring_next_charge.sql")
	if _, err := s.pool.Exec(ctx, upSQL); err != nil {
		t.Fatalf("run migration 00014 up: %v", err)
	}

	activeProfile := recurringProfile(t, s, activeUserID)
	if activeProfile.status != "active" || !activeProfile.nextChargeAt.Equal(effectiveExpires) {
		t.Fatalf("active profile = %#v, ожидали next_charge_at=%v", activeProfile, effectiveExpires)
	}
	failedProfile := recurringProfile(t, s, failedUserID)
	if failedProfile.status != "charge_failed" || !failedProfile.nextChargeAt.Equal(failedNext) {
		t.Fatalf("charge_failed profile changed: %#v, ожидали next_charge_at=%v", failedProfile, failedNext)
	}
}

func TestMigration00015_DisabledProfilesEraseTokenAndDownRestoresNotNull(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)

	disabledUserID := newTestUser(t, s)
	insertRecurringProfile(t, s, disabledUserID, "pm-migration-disabled", "disabled", time.Now().UTC())

	activeUserID := newTestUser(t, s)
	insertRecurringProfile(t, s, activeUserID, "pm-migration-active", "active", time.Now().UTC())

	failedUserID := newTestUser(t, s)
	insertRecurringProfile(t, s, failedUserID, "pm-migration-failed", "charge_failed", time.Now().UTC())

	upSQL := migrationUpSQL(t, "../../migrations/00015_recurring_profile_disable_erases_token.sql")
	if _, err := s.pool.Exec(ctx, upSQL); err != nil {
		t.Fatalf("run migration 00015 up: %v", err)
	}
	t.Cleanup(func() {
		if _, err := s.pool.Exec(context.Background(), upSQL); err != nil {
			t.Fatalf("restore migration 00015 up: %v", err)
		}
	})

	if methodID := recurringProfilePaymentMethodID(t, s, disabledUserID); methodID != nil {
		t.Fatalf("disabled payment_method_id = %q, ожидали NULL", *methodID)
	}
	if methodID := recurringProfilePaymentMethodID(t, s, activeUserID); methodID == nil || *methodID != "pm-migration-active" {
		t.Fatalf("active payment_method_id = %v, ожидали pm-migration-active", methodID)
	}
	if methodID := recurringProfilePaymentMethodID(t, s, failedUserID); methodID == nil || *methodID != "pm-migration-failed" {
		t.Fatalf("charge_failed payment_method_id = %v, ожидали pm-migration-failed", methodID)
	}

	downSQL := migrationDownSQL(t, "../../migrations/00015_recurring_profile_disable_erases_token.sql")
	if _, err := s.pool.Exec(ctx, downSQL); err != nil {
		t.Fatalf("run migration 00015 down: %v", err)
	}

	if methodID := recurringProfilePaymentMethodID(t, s, disabledUserID); methodID == nil || *methodID != "" {
		t.Fatalf("disabled payment_method_id after down = %v, ожидали empty placeholder", methodID)
	}
	if methodID := recurringProfilePaymentMethodID(t, s, activeUserID); methodID == nil || *methodID != "pm-migration-active" {
		t.Fatalf("active payment_method_id after down = %v, ожидали pm-migration-active", methodID)
	}
	if methodID := recurringProfilePaymentMethodID(t, s, failedUserID); methodID == nil || *methodID != "pm-migration-failed" {
		t.Fatalf("charge_failed payment_method_id after down = %v, ожидали pm-migration-failed", methodID)
	}

	var nullable string
	if err := s.pool.QueryRow(ctx,
		`SELECT is_nullable
		 FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND table_name = 'recurring_profiles'
		   AND column_name = 'payment_method_id'`,
	).Scan(&nullable); err != nil {
		t.Fatalf("query payment_method_id nullability: %v", err)
	}
	if nullable != "NO" {
		t.Fatalf("payment_method_id is_nullable = %q, ожидали NO", nullable)
	}
}

func migrationUpSQL(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration %s: %v", path, err)
	}
	sql := string(raw)
	down := strings.Index(sql, "-- +goose Down")
	if down < 0 {
		t.Fatalf("migration %s has no goose Down marker", path)
	}
	return strings.TrimSpace(sql[:down])
}

func migrationDownSQL(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration %s: %v", path, err)
	}
	sql := string(raw)
	down := strings.Index(sql, "-- +goose Down")
	if down < 0 {
		t.Fatalf("migration %s has no goose Down marker", path)
	}
	return strings.TrimSpace(sql[down:])
}
