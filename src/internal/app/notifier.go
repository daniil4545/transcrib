package app

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Тексты F9-пушей (architecture.md §7.2, db.md §5.6). Простой текст без кнопки:
// notify — это bot.SendText. CTA — отсылка к reply-кнопке «💳 Оформить подписку»
// (slash-команды /subscribe нет: вся навигация на кнопках, см. tg.go setup). Тон
// нейтральный, призыв к действию явный.
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

// Notifier — воркер F9-пушей (architecture.md §7.2). Вынесен из orchestrator.go ради SRP:
// работает с users (не с jobs). Одна горутина, суточная логика — частый тик не нужен.
type Notifier struct {
	storage *Storage
	// notify шлёт пуш юзеру. Замыкание поверх *Bot из main.go (bot.SendText),
	// как в Orchestrator: рассылка и доставка документов — разные ответственности.
	notify     func(chatID int64, text string) error
	trialLimit int           // лимит триала в минутах
	tick       time.Duration // 1h в проде; F9 — суточная логика, 30с-тик избыточен
}

func NewNotifier(s *Storage, notify func(int64, string) error, trialLimit int, tick time.Duration) *Notifier {
	return &Notifier{storage: s, notify: notify, trialLimit: trialLimit, tick: tick}
}

// Run гонит tickOnce до отмены ctx. Первый тик — сразу при старте (как Orchestrator):
// после рестарта не ждём час, чтобы разослать накопившиеся уведомления.
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

// tickOnce — одна итерация по классам дожимов.
// Каждый класс независим: сбой выборки одного не мешает остальным (slog.Error + continue).
func (n *Notifier) tickOnce(ctx context.Context) {
	n.notifyGroup(ctx, "trial-ending", n.storage.TrialEndingUsers, msgTrialEnding, n.storage.MarkTrialLowAndNudgeSent)
	n.notifyGroup(ctx, "payment-followup", n.paymentFollowupFetch, msgPaymentFollowup, n.markNudge(nudgePaymentFollowup))
	n.notifyGroup(ctx, "sub-low-balance", n.subLowBalanceFetch, msgSubLowBalance, n.markNudge(nudgeSubLowBalance))
	n.notifyGroup(ctx, "sub-empty-balance", n.subEmptyBalanceFetch, msgSubEmptyBalance, n.markNudge(nudgeSubEmptyBalance))
	n.notifyGroup(ctx, "sub-expiring", n.subExpiringFetch, msgSubExpiring, n.storage.MarkSubExpiringAndNudgeSent)
	n.notifyGroup(ctx, "sub-expired", n.subExpiredFetch, msgSubExpired, n.storage.MarkSubExpiredAndNudgeSent)
}

// fetch-адаптеры выравнивают выборки без trialLimit к сигнатуре fetch
// в notifyGroup — иначе пришлось бы дублировать тело цикла на каждый класс.
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

// notifyGroup обрабатывает один класс пушей: выбрать кандидатов и для каждого
// send -> mark. Порядок строго «send потом mark»: при сбое notify НЕ отмечаем —
// пуш потеряется, повторим на следующем тике (дедуп в выборках страхует от спама,
// мы лишь не теряем уведомление). Сбой одного юзера не рушит остальных (continue).
func (n *Notifier) notifyGroup(
	ctx context.Context,
	class string,
	// fetch: int — trialLimit (нужен только TrialEndingUsers; sub-классы его игнорируют
	// через адаптеры subExpiring/subExpiredFetch — выравнивание под общую сигнатуру).
	fetch func(context.Context, int) ([]int64, error),
	text string,
	mark func(context.Context, int64) error,
) {
	fctx, cancel := context.WithTimeout(ctx, dbTimeout)
	userIDs, err := fetch(fctx, n.trialLimit)
	cancel()
	if err != nil {
		// context.Canceled при shutdown — штатно, не ошибка.
		if !errors.Is(err, context.Canceled) {
			slog.Error("notifier fetch", "class", class, "err", err)
		}
		return
	}

	for _, uid := range userIDs {
		if err := n.notify(uid, text); err != nil {
			slog.Warn("notifier send", "class", class, "user_id", uid, "err", err)
			continue // не отмечаем — повторим на следующем тике
		}
		mctx, cancel := context.WithTimeout(ctx, dbTimeout)
		err := mark(mctx, uid)
		cancel()
		if err != nil {
			// context.Canceled при shutdown — штатно (родительский ctx отменён),
			// не ошибка: прекращаем класс, остальные uid обработаем после рестарта.
			if errors.Is(err, context.Canceled) {
				return
			}
			// Пуш ушёл, но отметка не легла: следующий тик повторит пуш (дедуп
			// выборки на это не страхует — notified_at не обновился). Терпимо: лишний
			// пуш лучше потери, но логируем как Error для видимости.
			slog.Error("notifier mark", "class", class, "user_id", uid, "err", err)
		}
	}
}
