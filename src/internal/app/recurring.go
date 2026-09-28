package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"
)

const (
	recurringPaymentTimeout = 15 * time.Second
	recurringPaymentLimit   = 20
)

type RecurringWorker struct {
	storage *Storage
	billing *Billing
	tick    time.Duration
	enabled bool
}

func NewRecurringWorker(s *Storage, b *Billing, tick time.Duration, enabled bool) *RecurringWorker {
	return &RecurringWorker{storage: s, billing: b, tick: tick, enabled: enabled}
}

func (w *RecurringWorker) Run(ctx context.Context) {
	if !w.enabled || !w.billing.CanPollPayments() {
		slog.Info("recurring worker disabled",
			"event", "recurring_worker_disabled", "phase", "recurring",
			"enabled", w.enabled, "payment_polling_configured", w.billing.CanPollPayments())
		return
	}
	if w.tick <= 0 {
		slog.Error("recurring worker disabled: non-positive tick",
			"event", "recurring_invalid_tick", "phase", "recurring", "tick", w.tick)
		return
	}
	slog.Info("recurring worker started",
		"event", "recurring_worker_started", "phase", "recurring", "tick", w.tick)
	if err := w.tickOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("recurring worker tick", "event", "recurring_tick_failed", "phase", "recurring", "err", err)
	}

	ticker := time.NewTicker(w.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.tickOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("recurring worker tick", "event", "recurring_tick_failed", "phase", "recurring", "err", err)
			}
		}
	}
}

func (w *RecurringWorker) tickOnce(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, dbTimeout)
	profiles, err := w.storage.DueRecurringProfiles(cctx, recurringPaymentLimit)
	cancel()
	if err != nil {
		return fmt.Errorf("due recurring profiles: %w", err)
	}
	if len(profiles) > 0 {
		slog.Info("recurring profiles claimed",
			"event", "recurring_profiles_claimed", "phase", "recurring", "count", len(profiles))
	}

	for _, profile := range profiles {
		if err := ctx.Err(); err != nil {
			return err
		}
		w.chargeProfile(ctx, profile)
	}
	return nil
}

func (w *RecurringWorker) chargeProfile(ctx context.Context, profile RecurringProfile) {
	idemKey := recurringPaymentIntentKey(profile.UserID, profile.NextChargeAt)
	metadata := recurringPaymentMetadata(profile.UserID)
	intent := PaymentInput{
		UserID:          profile.UserID,
		AmountKopecks:   w.billing.priceKopecks,
		Currency:        "RUB",
		IntentKey:       idemKey,
		Metadata:        metadata,
		PaymentMethodID: profile.PaymentMethodID,
	}

	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbTimeout)
	created, err := w.storage.StartRecurringPaymentIntent(cctx, intent, idemKey, profile.NextChargeAt)
	cancel()
	if err != nil {
		w.markRecurringRetry(ctx, profile.UserID, fmt.Sprintf("store recurring intent: %v", err))
		return
	}
	if !created {
		slog.Debug("recurring payment intent skipped",
			"event", "recurring_intent_skipped", "phase", "recurring",
			"user_id", profile.UserID, "next_charge_at", profile.NextChargeAt)
		return
	}
	slog.Info("recurring payment intent ready",
		"event", "recurring_intent_ready", "phase", "recurring",
		"user_id", profile.UserID, "next_charge_at", profile.NextChargeAt)

	pctx, cancel := context.WithTimeout(ctx, recurringPaymentTimeout)
	payment, err := w.billing.CreateRecurringPayment(pctx, profile.UserID, profile.PaymentMethodID, idemKey)
	cancel()
	if err != nil {
		w.markRecurringRetry(ctx, profile.UserID, fmt.Sprintf("create recurring payment: %v", err))
		return
	}
	slog.Info("recurring provider payment created",
		"event", "recurring_provider_payment_created", "phase", "recurring",
		"user_id", profile.UserID, "provider_payment_id", payment.ID)

	cctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), dbTimeout)
	err = w.storage.AttachRecurringProviderPayment(cctx, idemKey, payment)
	cancel()
	if err != nil {
		w.markRecurringRetry(ctx, profile.UserID, fmt.Sprintf("attach recurring payment: %v", err))
		return
	}
	slog.Info("recurring provider payment attached",
		"event", "recurring_provider_payment_attached", "phase", "recurring",
		"user_id", profile.UserID, "provider_payment_id", payment.ID)

	cctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), dbTimeout)
	err = w.storage.MarkRecurringAttempt(cctx, profile.UserID, payment.ID, idemKey)
	cancel()
	if err != nil {
		slog.Error("mark recurring attempt",
			"event", "recurring_mark_attempt_failed", "phase", "recurring",
			"err", err, "provider_payment_id", payment.ID, "user_id", profile.UserID)
	}
}

func (w *RecurringWorker) markRecurringRetry(ctx context.Context, userID int64, reason string) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbTimeout)
	err := w.storage.MarkRecurringRetry(cctx, userID, reason)
	cancel()
	if err != nil {
		slog.Error("mark recurring retry", "event", "recurring_mark_retry_failed", "phase", "recurring", "err", err, "user_id", userID)
		return
	}
	slog.Info("recurring retry marked",
		"event", "recurring_retry_marked", "phase", "recurring", "user_id", userID)
}

func recurringPaymentMetadata(userID int64) json.RawMessage {
	raw, err := json.Marshal(map[string]string{
		"user_id":    strconv.FormatInt(userID, 10),
		"auto_renew": "true",
		"recurring":  "true",
	})
	if err != nil {
		return nil
	}
	return raw
}
