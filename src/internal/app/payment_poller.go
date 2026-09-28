package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

const (
	paymentPollTimeout = 15 * time.Second
	paymentPollLimit   = 50
)

type PaymentPoller struct {
	storage *Storage
	billing *Billing
	notify  func(int64, string) error
	tick    time.Duration
}

func NewPaymentPoller(s *Storage, b *Billing, notify func(int64, string) error, tick time.Duration) *PaymentPoller {
	return &PaymentPoller{storage: s, billing: b, notify: notify, tick: tick}
}

func (p *PaymentPoller) Run(ctx context.Context) {
	if !p.billing.CanPollPayments() {
		return
	}
	if p.tick <= 0 {
		slog.Error("payment poller disabled: non-positive tick",
			"event", "payment_poll_invalid_tick", "phase", "payment_poll", "tick", p.tick)
		return
	}
	if err := p.tickOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("payment poller tick", "event", "payment_poll_tick_failed", "phase", "payment_poll", "err", err)
	}

	ticker := time.NewTicker(p.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := p.tickOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("payment poller tick", "event", "payment_poll_tick_failed", "phase", "payment_poll", "err", err)
			}
		}
	}
}

func (p *PaymentPoller) tickOnce(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, dbTimeout)
	payments, err := p.storage.PendingPayments(cctx, paymentPollLimit)
	cancel()
	if err != nil {
		return fmt.Errorf("pending payments: %w", err)
	}

	for _, payment := range payments {
		if err := ctx.Err(); err != nil {
			return err
		}
		p.pollPayment(ctx, payment)
	}
	return nil
}

func (p *PaymentPoller) pollPayment(ctx context.Context, payment Payment) {
	if payment.ProviderPaymentChargeID == nil || *payment.ProviderPaymentChargeID == "" {
		return
	}
	paymentID := *payment.ProviderPaymentChargeID

	pctx, cancel := context.WithTimeout(ctx, paymentPollTimeout)
	got, err := p.billing.GetPayment(pctx, paymentID)
	cancel()
	if err != nil {
		slog.Error("get yookassa payment",
			"event", "payment_poll_get_failed", "phase", "payment_poll",
			"err", err, "payment_id", payment.ID, "provider_payment_id", paymentID, "user_id", payment.UserID)
		return
	}

	switch {
	case got.Status == "succeeded" && got.Paid:
		if !samePaymentIntent(payment, got) {
			logPaymentMismatch(payment, got)
			return
		}
		p.finalizePayment(ctx, got)
	case got.Status == "canceled" && !got.Paid:
		if !samePaymentIntent(payment, got) {
			logPaymentMismatch(payment, got)
			return
		}
		p.failPayment(ctx, payment, got)
	case got.Status == "canceled":
		slog.Error("yookassa payment canceled but paid",
			"event", "payment_poll_canceled_paid", "phase", "payment_poll",
			"provider_payment_id", got.ID, "payment_status", got.Status, "paid", got.Paid)
	default:
		slog.Debug("yookassa payment still pending",
			"event", "payment_poll_pending", "phase", "payment_poll",
			"provider_payment_id", got.ID, "payment_status", got.Status, "paid", got.Paid)
	}
}

func logPaymentMismatch(local Payment, got YookassaPayment) {
	providerPaymentID := ""
	if local.ProviderPaymentChargeID != nil {
		providerPaymentID = *local.ProviderPaymentChargeID
	}
	slog.Error("yookassa payment mismatch",
		"event", "payment_poll_mismatch", "phase", "payment_poll",
		"payment_id", local.ID, "provider_payment_id", providerPaymentID,
		"got_provider_payment_id", got.ID, "user_id", local.UserID,
		"local_amount", local.Amount, "got_amount", got.AmountKopecks,
		"local_currency", local.Currency, "got_currency", got.Currency)
}

func samePaymentIntent(local Payment, got YookassaPayment) bool {
	if local.ProviderPaymentChargeID == nil || *local.ProviderPaymentChargeID != got.ID {
		return false
	}
	if local.Amount != got.AmountKopecks || local.Currency != got.Currency {
		return false
	}

	strictMetadata := local.PaymentIntentKey != nil && *local.PaymentIntentKey != ""
	var metadata struct {
		UserID string `json:"user_id"`
	}
	if len(got.Metadata) == 0 || string(got.Metadata) == "null" {
		return !strictMetadata
	}
	if err := json.Unmarshal(got.Metadata, &metadata); err != nil {
		return false
	}
	if metadata.UserID == "" {
		return !strictMetadata
	}
	userID, err := strconv.ParseInt(metadata.UserID, 10, 64)
	return err == nil && userID == local.UserID
}

func isRecurringIntent(local Payment) bool {
	return local.PaymentIntentKey != nil && strings.HasPrefix(*local.PaymentIntentKey, "recurring:")
}

func (p *PaymentPoller) finalizePayment(ctx context.Context, payment YookassaPayment) {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbTimeout)
	activation, err := p.storage.FinalizeYookassaPayment(dctx, payment.ID, payment.Raw, payment.ProviderPaidAt, p.billing.subscriptionMinutesPerPeriod, payment.PaymentMethodID, payment.PaymentMethodSaved)
	cancel()
	if err != nil {
		slog.Error("finalize yookassa payment",
			"event", "payment_poll_finalize_failed", "phase", "payment_poll",
			"err", err, "provider_payment_id", payment.ID)
		return
	}
	if !activation.Changed {
		return
	}
	slog.Info("yookassa payment finalized",
		"event", "payment_poll_finalized", "phase", "payment_poll",
		"provider_payment_id", payment.ID, "user_id", activation.UserID,
		"recurring", paymentAutoRenew(payment.Raw), "recurring_active", activation.RecurringActive,
		"recurring_payment", activation.RecurringPayment)
	if err := p.notify(activation.UserID, paymentActivationMessage(activation)); err != nil {
		slog.Warn("notify payment activated",
			"event", "payment_poll_notify_failed", "phase", "payment_poll",
			"err", err, "provider_payment_id", payment.ID, "user_id", activation.UserID)
	}
}

func paymentActivationMessage(a PaymentActivation) string {
	expires := a.ExpiresAt.Format("02.01.2006")
	if a.RecurringPayment {
		return fmt.Sprintf("Автопродление прошло. Подписка продлена до %s.", expires)
	}
	if a.RecurringActive {
		return fmt.Sprintf("Оплата прошла. Подписка активна до %s. Следующая оплата %s.", expires, a.NextChargeAt.Format("02.01.2006"))
	}
	return fmt.Sprintf("Оплата прошла. Подписка активна до %s.", expires)
}

func (p *PaymentPoller) failPayment(ctx context.Context, local Payment, payment YookassaPayment) {
	if isRecurringIntent(local) {
		p.failRecurringPayment(ctx, payment)
		return
	}
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbTimeout)
	changed, userID, err := p.storage.FailYookassaPayment(dctx, payment.ID, payment.Raw)
	cancel()
	if err != nil {
		slog.Error("fail yookassa payment",
			"event", "payment_poll_fail_failed", "phase", "payment_poll",
			"err", err, "provider_payment_id", payment.ID)
		return
	}
	if !changed {
		return
	}
	slog.Info("yookassa payment canceled",
		"event", "payment_poll_canceled", "phase", "payment_poll",
		"provider_payment_id", payment.ID, "user_id", userID)
}

func (p *PaymentPoller) failRecurringPayment(ctx context.Context, payment YookassaPayment) {
	reason := strings.TrimSpace(payment.CancellationReason)
	if reason == "" {
		reason = "recurring payment canceled"
	}
	retryable := recurringCancellationRetryable(payment.CancellationReason)
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbTimeout)
	changed, userID, profileChanged, err := p.storage.FailRecurringYookassaPayment(dctx, payment.ID, payment.Raw, retryable, reason)
	cancel()
	if err != nil {
		slog.Error("mark recurring payment canceled",
			"event", "payment_poll_recurring_failure_failed", "phase", "payment_poll",
			"err", err, "provider_payment_id", payment.ID, "user_id", userID, "reason", payment.CancellationReason)
		return
	}
	if !changed {
		return
	}
	slog.Info("yookassa payment canceled",
		"event", "payment_poll_canceled", "phase", "payment_poll",
		"provider_payment_id", payment.ID, "user_id", userID)
	if !profileChanged {
		return
	}
	if retryable {
		slog.Info("recurring payment failure marked",
			"event", "payment_poll_recurring_failure_marked", "phase", "payment_poll",
			"provider_payment_id", payment.ID, "user_id", userID, "reason", payment.CancellationReason)
		if err := p.notify(userID, "Не удалось автопродлить подписку. Текущий оплаченный период остаётся активным, мы повторим попытку позже. Если нужна помощь, напишите в поддержку."); err != nil {
			slog.Warn("notify recurring payment failed",
				"event", "payment_poll_recurring_notify_failed", "phase", "payment_poll",
				"err", err, "provider_payment_id", payment.ID, "user_id", userID)
		}
		return
	}
	slog.Warn("recurring payment blocked after non-retryable cancel",
		"event", "payment_poll_recurring_blocked", "phase", "payment_poll",
		"provider_payment_id", payment.ID, "user_id", userID, "reason", payment.CancellationReason)
	if err := p.notify(userID, "Автопродление требует проверки. Мы не будем повторять списание автоматически; напишите в поддержку."); err != nil {
		slog.Warn("notify recurring payment blocked",
			"event", "payment_poll_recurring_blocked_notify_failed", "phase", "payment_poll",
			"err", err, "provider_payment_id", payment.ID, "user_id", userID)
	}
}

func recurringCancellationRetryable(reason string) bool {
	reason = strings.ToLower(strings.TrimSpace(reason))
	if reason == "permission_revoked" || strings.Contains(reason, "mandate") || strings.Contains(reason, "revoked") {
		return false
	}
	return true
}
