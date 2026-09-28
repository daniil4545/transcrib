package app

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Критический денежный узел F7 (db.md §5.9, plan-payments-slice §P2): двухфазный
// refund в storage. Точечные тесты ТОЛЬКО на инварианты возврата:
// идемпотентность намерения (BeginRefund), идемпотентность финализации (FinalizeRefund),
// деактивация ровно своей подписки (модель 1:1), сохранение доступа при failed/canceled,
// сентинел на нерефандабельный платёж. REST-фаза billing.Refund/admin-триггер — за блокером
// (нужны креды ЮKassa), здесь не тестируются. Интеграционные, бьют в Postgres через
// DATABASE_URL; без него — Skip (newTestStorage).

// cleanupRefunds снимает FK refunds -> payments (плейн FK, без ON DELETE CASCADE):
// cleanupPayments удаляет платежи, но строки refunds заблокировали бы этот DELETE.
// Регистрируется ПОСЛЕ cleanupPayments -> по LIFO t.Cleanup отрабатывает РАНЬШЕ него
// (порядок уборки: refunds -> payments -> subscriptions -> user).
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

// seedPayment кладёт один успешный платёж + связанную подписку через §5.4
// (RecordSuccessfulPayment) и возвращает providerChargeID для BeginRefund плюс
// id связанной подписки (для проверки деактивации именно своей строки 1:1).
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

// paymentStatusByCharge читает payments.status по provider_payment_charge_id.
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

// subActive сообщает, активна ли подписка (expires_at > now()).
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

// refundCountByPayment считает строки refunds для платежа провайдера.
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

// refundProviderID читает provider_refund_id (nil = NULL).
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

// TestBeginRefund_DoubleIntentIdempotent — §5.9 фаза 1: двойной BeginRefund на один
// платёж даёт ОДНУ pending-строку (refunds_active_idx, partial UNIQUE на payment_id).
// Второй вызов отдаёт ТОТ ЖЕ refundID и idemKey (идемпотентность намерения: один и тот
// же Idempotence-Key переживает наш ретрай, защищая от двойного списания у ЮKassa).
// В таблице ровно один refund по платежу. db.md §5.9 / docs/testing.md.
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
		t.Errorf("amount = %d, ожидали 199000 (full-only из payments)", amt1)
	}

	id2, key2, amt2, err := s.BeginRefund(ctx, prov)
	if err != nil {
		t.Fatalf("BeginRefund #2 (повтор намерения): %v", err)
	}
	if id2 != id1 {
		t.Errorf("refundID #2 = %d, ожидали тот же %d (идемпотентность намерения)", id2, id1)
	}
	if key2 != key1 {
		t.Errorf("idemKey #2 = %q, ожидали тот же %q (Idempotence-Key переживает ретрай)", key2, key1)
	}
	if amt2 != amt1 {
		t.Errorf("amount #2 = %d, ожидали тот же %d", amt2, amt1)
	}
	if n := refundCountByPayment(t, s, prov); n != 1 {
		t.Errorf("refunds count = %d, ожидали 1 (refunds_active_idx гасит дубль)", n)
	}
}

// TestFinalizeRefund_SucceededDeactivatesOwnSubscription — §5.9 фаза 2 (succeeded):
// payment -> 'refunded' И деактивируется РОВНО та строка подписки, что купил платёж
// (модель 1:1, expires_at<=now()). Изоляция 1:1: у юзера есть ВТОРОЙ платёж/подписка —
// refund первого НЕ задевает второй активный период. Повторный FinalizeRefund — no-op
// (guard status='pending'): payment/subscription не меняются повторно. db.md §5.9.
func TestFinalizeRefund_SucceededDeactivatesOwnSubscription(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)
	cleanupRefunds(t, s, userID)

	prov1, sub1 := seedPayment(t, s, userID, "tg-fin-1", "yk-fin-1")
	insertRecurringProfile(t, s, userID, "pm-refund-1", "active", time.Now().Add(24*time.Hour))
	// Второй оплаченный период того же юзера: должен остаться нетронутым.
	prov2, sub2 := seedPayment(t, s, userID, "tg-fin-2", "yk-fin-2")
	if sub1 == sub2 {
		t.Fatal("seed дал одну подписку на два платежа — модель 1:1 нарушена в подготовке")
	}

	refundID, _, _, err := s.BeginRefund(ctx, prov1)
	if err != nil {
		t.Fatalf("BeginRefund: %v", err)
	}

	if err := s.FinalizeRefund(ctx, refundID, "yk-refund-1", RefundSucceeded, []byte(`{"status":"succeeded"}`)); err != nil {
		t.Fatalf("FinalizeRefund #1: %v", err)
	}
	if got := paymentStatusByCharge(t, s, prov1); got != string(PaymentRefunded) {
		t.Errorf("payment #1 status = %q, ожидали refunded", got)
	}
	if subActive(t, s, sub1) {
		t.Error("подписка #1 активна, ожидали деактивацию (expires_at<=now())")
	}
	profile, ok, err := s.RecurringProfile(ctx, userID)
	if err != nil {
		t.Fatalf("RecurringProfile: %v", err)
	}
	if !ok {
		t.Fatalf("recurring profile not found")
	}
	if profile.Status != "disabled" || profile.DisabledAt == nil {
		t.Fatalf("recurring profile = %+v, ожидали disabled после refund", profile)
	}
	if profile.PaymentMethodID != "" {
		t.Fatalf("recurring profile payment method = %q, ожидали пустой token после refund", profile.PaymentMethodID)
	}
	var tokenErased bool
	if err := s.pool.QueryRow(ctx,
		`SELECT payment_method_id IS NULL FROM recurring_profiles WHERE user_id = $1`,
		userID,
	).Scan(&tokenErased); err != nil {
		t.Fatalf("check recurring token erased: %v", err)
	}
	if !tokenErased {
		t.Fatal("recurring profile payment_method_id не очищен после refund")
	}
	// Изоляция 1:1: второй платёж/период не затронут возвратом первого.
	if got := paymentStatusByCharge(t, s, prov2); got != string(PaymentSucceeded) {
		t.Errorf("payment #2 status = %q, ожидали succeeded (refund первого задел второй)", got)
	}
	if !subActive(t, s, sub2) {
		t.Error("подписка #2 деактивирована, ожидали активной (refund первого задел второй период)")
	}

	// Повторная финализация (повторный webhook/триггер) — no-op по guard status='pending'.
	if err := s.FinalizeRefund(ctx, refundID, "yk-refund-1", RefundSucceeded, []byte(`{"status":"succeeded"}`)); err != nil {
		t.Fatalf("FinalizeRefund #2 (повтор): %v", err)
	}
	if got := paymentStatusByCharge(t, s, prov1); got != string(PaymentRefunded) {
		t.Errorf("payment #1 status после повтора = %q, ожидали refunded (идемпотентность)", got)
	}
	if subActive(t, s, sub1) {
		t.Error("подписка #1 после повтора активна — повторная финализация не должна ничего трогать")
	}
}

// TestFinalizeRefund_FailedAndCanceledKeepAccess — §5.9 фаза 2 (failed/canceled):
// деньги НЕ вернулись -> payment остаётся 'succeeded', подписка активна (доступ
// сохраняется). Два терминальных не-succeeded статуса ЮKassa проверяем симметрично.
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
				t.Errorf("payment status = %q, ожидали succeeded (деньги не вернулись при %s)", got, c.status)
			}
			if !subActive(t, s, subID) {
				t.Errorf("подписка деактивирована при %s, ожидали активной (доступ сохраняется)", c.status)
			}
		})
	}
}

// TestBeginRefund_NotRefundable — §5.9: BeginRefund по несуществующему платежу и по
// платежу не в статусе succeeded возвращает ErrPaymentNotRefundable (возвращать нечего).
// Класс «не succeeded» проверяем на pending-платеже прямым INSERT.
func TestBeginRefund_NotRefundable(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)
	cleanupRefunds(t, s, userID)

	if _, _, _, err := s.BeginRefund(ctx, "yk-does-not-exist"); !errors.Is(err, ErrPaymentNotRefundable) {
		t.Errorf("BeginRefund(несуществующий) err = %v, ожидали ErrPaymentNotRefundable", err)
	}

	// pending-платёж: succeeded-предиката нет, refund невозможен.
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO payments (user_id, status, amount, currency, provider_payment_charge_id)
		 VALUES ($1, 'pending', 199000, 'RUB', $2)`,
		userID, "yk-pending",
	); err != nil {
		t.Fatalf("insert pending payment: %v", err)
	}
	if _, _, _, err := s.BeginRefund(ctx, "yk-pending"); !errors.Is(err, ErrPaymentNotRefundable) {
		t.Errorf("BeginRefund(pending) err = %v, ожидали ErrPaymentNotRefundable", err)
	}
}

// TestFinalizeRefund_EmptyProviderIDStoredAsNull — §5.9: пустой providerRefundID
// (failed/canceled не несут id у ЮKassa) хранится как NULL, а не пустая строка —
// иначе несколько таких строк столкнулись бы в refunds_provider_idx (partial UNIQUE,
// который NULL коллизией не считает). Проверяем два refund'а с пустым id у разных
// платежей: оба сохраняются NULL, индекс не падает.
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

	// Пустой provider_refund_id у обоих — partial UNIQUE не должен конфликтовать на NULL.
	if err := s.FinalizeRefund(ctx, idA, "", RefundFailed, nil); err != nil {
		t.Fatalf("FinalizeRefund A (пустой provider id): %v", err)
	}
	if err := s.FinalizeRefund(ctx, idB, "", RefundFailed, nil); err != nil {
		t.Fatalf("FinalizeRefund B (пустой provider id): %v", err)
	}
	if pid := refundProviderID(t, s, idA); pid != nil {
		t.Errorf("provider_refund_id A = %q, ожидали NULL", *pid)
	}
	if pid := refundProviderID(t, s, idB); pid != nil {
		t.Errorf("provider_refund_id B = %q, ожидали NULL", *pid)
	}
}
