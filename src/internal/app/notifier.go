package app

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// F9 push texts (architecture.md §7.2, db.md §5.6). Plain text without a button:
// notify is bot.SendText. The CTA refers to the reply button "💳 Оформить подписку"
// (there is no /subscribe slash command: all navigation is on buttons, see tg.go setup). The tone
// is neutral, the call to action is explicit.
const (
	msgTrialEnding = "Бесплатный баланс почти закончился — осталось меньше часа. " +
		"Чтобы продолжить пользоваться сервисом, нажмите кнопку «💳 Оформить подписку» в меню."
	msgSubExpiring = "Подписка истекает через 3 дня. Продлите её кнопкой " +
		"«💳 Оформить подписку» в меню, чтобы не потерять доступ к обработке встреч."
	msgSubExpired = "Срок подписки истёк. Оформите новую кнопкой " +
		"«💳 Оформить подписку» в меню, чтобы снова обрабатывать встречи."
	msgPaymentFollowup = "Вы смотрели подписку, но оплата не завершилась. Если хотите продолжить после бесплатного лимита, " +
		"нажмите «💳 Оформить подписку» в меню."
	msgSubLowBalance = "В подписке осталось меньше часа обработки встреч. Чтобы не прерываться, " +
		"оформите следующий месяц кнопкой «💳 Оформить подписку» в меню."
	msgSubEmptyBalance = "Платные часы закончились, хотя срок подписки ещё идёт. " +
		"Оформите следующий месяц кнопкой «💳 Оформить подписку» в меню, чтобы продолжить."
)

// Notifier is the F9 push worker (architecture.md §7.2). Split out of orchestrator.go for SRP:
// it works with users (not jobs). One goroutine, daily logic: a frequent tick is not needed.
type Notifier struct {
	storage *Storage
	// notify sends a push to the user. A closure over *Bot from main.go (bot.SendText),
	// as in Orchestrator: broadcasting and document delivery are separate responsibilities.
	notify     func(chatID int64, text string) error
	trialLimit int           // trial limit in minutes
	tick       time.Duration // 1h in prod; F9 is daily logic, a 30s tick is excessive
}

func NewNotifier(s *Storage, notify func(int64, string) error, trialLimit int, tick time.Duration) *Notifier {
	return &Notifier{storage: s, notify: notify, trialLimit: trialLimit, tick: tick}
}

// Run drives tickOnce until ctx is cancelled. The first tick is immediate on start (as in Orchestrator):
// after a restart we do not wait an hour before sending the accumulated notifications.
func (n *Notifier) Run(ctx context.Context) {
	n.tickOnce(ctx)

	ticker := time.NewTicker(n.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n.tickOnce(ctx)
		}
	}
}

// tickOnce is one iteration over the follow-up classes.
// Each class is independent: a failed query for one does not block the others (slog.Error + continue).
func (n *Notifier) tickOnce(ctx context.Context) {
	n.notifyGroup(ctx, "trial-ending", n.storage.TrialEndingUsers, msgTrialEnding, n.storage.MarkTrialLowAndNudgeSent)
	n.notifyGroup(ctx, "payment-followup", n.paymentFollowupFetch, msgPaymentFollowup, n.markNudge(nudgePaymentFollowup))
	n.notifyGroup(ctx, "sub-low-balance", n.subLowBalanceFetch, msgSubLowBalance, n.markNudge(nudgeSubLowBalance))
	n.notifyGroup(ctx, "sub-empty-balance", n.subEmptyBalanceFetch, msgSubEmptyBalance, n.markNudge(nudgeSubEmptyBalance))
	n.notifyGroup(ctx, "sub-expiring", n.subExpiringFetch, msgSubExpiring, n.storage.MarkSubExpiringAndNudgeSent)
	n.notifyGroup(ctx, "sub-expired", n.subExpiredFetch, msgSubExpired, n.storage.MarkSubExpiredAndNudgeSent)
}

// The fetch adapters align queries without trialLimit to the fetch signature
// in notifyGroup; otherwise the loop body would have to be duplicated for every class.
func (n *Notifier) subExpiringFetch(ctx context.Context, _ int) ([]int64, error) {
	return n.storage.SubExpiringUsers(ctx)
}

func (n *Notifier) subExpiredFetch(ctx context.Context, _ int) ([]int64, error) {
	return n.storage.SubExpiredUsers(ctx)
}

func (n *Notifier) paymentFollowupFetch(ctx context.Context, _ int) ([]int64, error) {
	return n.storage.PaymentViewedWithoutPaidUsers(ctx)
}

func (n *Notifier) subLowBalanceFetch(ctx context.Context, _ int) ([]int64, error) {
	return n.storage.SubLowBalanceUsers(ctx)
}

func (n *Notifier) subEmptyBalanceFetch(ctx context.Context, _ int) ([]int64, error) {
	return n.storage.SubEmptyBalanceUsers(ctx)
}

func (n *Notifier) markNudge(class string) func(context.Context, int64) error {
	return func(ctx context.Context, userID int64) error {
		return n.storage.MarkNudgeSent(ctx, userID, class)
	}
}

// notifyGroup handles one push class: select candidates and, for each,
// send -> mark. The order is strictly "send, then mark": if notify fails we do NOT mark,
// so the push is not lost and is retried on the next tick (dedup in the queries guards against spam,
// we only avoid losing the notification). A failure for one user does not break the rest (continue).
func (n *Notifier) notifyGroup(
	ctx context.Context,
	class string,
	// fetch: int is trialLimit (needed only by TrialEndingUsers; the sub classes ignore it
	// via the subExpiring/subExpiredFetch adapters, aligning to the common signature).
	fetch func(context.Context, int) ([]int64, error),
	text string,
	mark func(context.Context, int64) error,
) {
	fctx, cancel := context.WithTimeout(ctx, dbTimeout)
	userIDs, err := fetch(fctx, n.trialLimit)
	cancel()
	if err != nil {
		// context.Canceled on shutdown is normal, not an error.
		if !errors.Is(err, context.Canceled) {
			slog.Error("notifier fetch", "class", class, "err", err)
		}
		return
	}

	for _, uid := range userIDs {
		if err := n.notify(uid, text); err != nil {
			slog.Warn("notifier send", "class", class, "user_id", uid, "err", err)
			continue // not marked: retry on the next tick
		}
		mctx, cancel := context.WithTimeout(ctx, dbTimeout)
		err := mark(mctx, uid)
		cancel()
		if err != nil {
			// context.Canceled on shutdown is normal (the parent ctx is cancelled),
			// not an error: stop the class, the remaining uids are handled after restart.
			if errors.Is(err, context.Canceled) {
				return
			}
			// The push went out but the mark was not saved: the next tick repeats the push (the query
			// dedup does not guard against this, notified_at was not updated). Tolerable: an extra
			// push is better than a loss, but we log it as Error for visibility.
			slog.Error("notifier mark", "class", class, "user_id", uid, "err", err)
		}
	}
}
