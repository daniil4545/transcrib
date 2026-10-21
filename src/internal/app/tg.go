package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	tele "gopkg.in/telebot.v4"
)

// Unique identifiers of the consent inline buttons. The gate uses them to decide
// which callbacks to let through before consent (whitelist, architecture.md §5 F0).
const (
	consentAccept  = "consent_accept"
	consentDecline = "consent_decline"
)

// paySubscription - unique identifier of the payment inline button.
const (
	paySubscription   = "pay_subscription"
	payAutoRenew      = "pay_auto_renew"
	disableAutoRenew  = "disable_auto_renew"
	renewSubscription = "renew_subscription"
)

// Reply-menu labels. The values match the button text: telebot routes
// incoming text to the handler registered under the same value
// (update.go "1:1 satisfaction" - fires before OnText).
const (
	menuSubscribe = "💳 Оформить подписку"
	menuMyCalls   = "🎙 Мои звонки"
	menuBalance   = "💰 Баланс"
	menuAbout     = "ℹ️ О сервисе"
	menuSupport   = "🛟 Служба поддержки"
	// The old text is not shown in the new menu, but is accepted from
	// persistent Telegram keyboards that were already issued.
	menuSupportOld = "💬 Поддержка"
)

// Unique identifiers of the inline buttons of the "My calls" panel. The button payload carries
// jobID; the handler is registered once per unique and reads the id from c.Data().
const (
	jobCancel           = "job_cancel"            // payload "id:page"; cancel from the card
	jobRedeliver        = "job_redeliver"         // payload "id:page"; redeliver the transcript
	jobRedeliverSummary = "job_redeliver_summary" // payload "id:page"; redeliver the summary
	jobOpen             = "job_open"              // payload "id:page"; open the call card
	jobBack             = "job_back"              // payload "page"; go back from the card to the list
	jobsPage            = "jobs_page"             // payload "page"; list paging (left/right)
)

const (
	callsPageSize     = 5
	callsHistoryLimit = 7 * 24 * time.Hour
)

const freshPendingPaymentWindow = 30 * time.Minute

// texts - the bot's user-facing messages (brand "transcrib").
// The final legal text of the 152-FZ consent is owned by the customer.
type texts struct {
	consent              string
	acceptButton         string
	declineButton        string
	payButton            string
	payOpenButton        string
	welcome              string
	subscription         string
	about                string
	support              string
	consentDeclined      string
	help                 string
	needConsent          string
	sendLink             string
	payFacade            string
	payEmailRequest      string
	payEmailInvalid      string
	payPendingExists     string
	payLinkReady         string
	internalError        string
	noQuota              string
	duplicateActiveJob   string
	tooManyActiveJobs    string
	zoomWebApp           string
	paymentSuccess       string
	paymentReject        string
	paymentError         string
	autoRenewDisabled    string
	autoRenewAlreadyOff  string
	autoRenewUnavailable string
	myCallsEmpty         string
	myCallsHeader        string
	jobCancelled         string
	jobStopStarted       string
	jobCancelFailed      string
	jobNotFound          string
	redeliverNotReady    string
	redeliverFail        string
	summaryNotAvailable  string
}

func defaultTexts() texts {
	return texts{
		// Temporary wording for 152-FZ. The final legal text and the link to the full
		// policy are owned by the customer.
		consent: "Перед началом — согласие на обработку данных (152-ФЗ).\n\n" +
			"Чтобы сделать расшифровку, сервис записывает аудио ваших онлайн-встреч и " +
			"передаёт его подрядчику для распознавания речи. В расшифровке реплики " +
			"размечены по говорящим; аудио и результат используются только " +
			"для оказания услуги.\n\n" +
			"Нажимая «Согласен», вы соглашаетесь на обработку персональных данных на этих условиях.",
		acceptButton:  "Согласен",
		declineButton: "Не согласен",
		payButton:     "Оплатить",
		payOpenButton: "Перейти к оплате",
		welcome: "👋 transcrib — расшифровки и конспекты ваших онлайн-встреч\n\n" +
			"Пришлите ссылку на встречу (Zoom, Google Meet, Яндекс Телемост) — " +
			"бот подключится как участник, запишет разговор и вернёт:\n" +
			"• полную расшифровку по спикерам\n" +
			"• конспект встречи с задачами (скоро)\n\n" +
			"📦 Подписка: 1990 ₽/мес — 15 часов обработки встреч\n" +
			"🎁 Первые 3 часа бесплатно, чтобы попробовать",
		subscription: "💳 Подписка transcrib\n\n" +
			"Входит:\n" +
			"✓ 15 часов обработки встреч в месяц\n" +
			"✓ Расшифровка разговора по спикерам\n" +
			"✓ Конспект встречи с решениями и задачами (скоро)\n" +
			"✓ Zoom, Google Meet, Яндекс Телемост\n" +
			"✓ Доставка результата прямо в Telegram\n\n" +
			"Стоимость — 1990 ₽ в месяц.",
		about: "ℹ️ О сервисе transcrib\n\n" +
			"transcrib подключается к вашим онлайн-встречам, записывает разговор и " +
			"присылает расшифровку по спикерам прямо в Telegram (конспект с задачами — скоро).\n\n" +
			"Приватность: бот записывает только аудио встречи; в расшифровке реплики " +
			"размечены по говорящим.\n\n" +
			"Сервис работает по вашему согласию на обработку персональных данных (152-ФЗ).",
		support: "🛟 Служба поддержки\n\n" +
			"Для вопросов по подписке и обработке встреч пишите в Telegram: @dasukhar\n" +
			"Почта: support@example.com",
		consentDeclined:  "Без согласия на обработку данных я не смогу работать. Нажмите /start, когда будете готовы.",
		help:             "transcrib: пришлите ссылку на онлайн-встречу — верну расшифровку (конспект — скоро). Начните с /start.",
		needConsent:      "Сначала нужно согласие на обработку данных. Нажмите /start.",
		sendLink:         "Пришлите ссылку на встречу (Zoom, Google Meet, Телемост).",
		payFacade:        "Оплата скоро будет доступна — мы подключаем платёжную систему. Спасибо за интерес!",
		payEmailRequest:  "Пришлите email для чека. Он сохранится для следующих оплат.",
		payEmailInvalid:  "Не похоже на email. Пришлите адрес вида name@example.com.",
		payPendingExists: "Платёж уже создан. Завершите оплату по открытой ссылке или попробуйте ещё раз позже.",
		payLinkReady:     "Ссылка на оплату готова. После оплаты я активирую подписку автоматически.",
		internalError:    "Что-то пошло не так. Попробуйте ещё раз.",
		noQuota:          "Бесплатный баланс закончился. Оформите подписку, чтобы продолжить.",
		duplicateActiveJob: "Эта ссылка уже в работе. Для другой встречи пришлите " +
			"другую ссылку.",
		tooManyActiveJobs: "У вас уже 4 встречи в работе. Дождитесь завершения одной из них или отмените лишнюю в «Мои звонки».",
		zoomWebApp: "Это ссылка на веб-клиент Zoom — бот по ней зайти не сможет. " +
			"Пришлите ссылку-приглашение вида https://...zoom.us/j/... (она есть в приглашении на встречу).",
		paymentSuccess: "Оплата прошла, подписка активна. Проверить остаток — /balance.",
		paymentReject:  "Не удалось обработать заказ. Попробуйте ещё раз.",
		paymentError: "Оплата прошла, но активация подписки задержалась. Напишите в поддержку:\n" +
			"Telegram: @dasukhar\n" +
			"Почта: support@example.com\n" +
			"Мы всё поправим.",
		autoRenewDisabled:    "Автопродление отключено. Текущая подписка останется активной до конца оплаченного периода.",
		autoRenewAlreadyOff:  "Автопродление уже отключено или не было включено.",
		autoRenewUnavailable: "Автопродление сейчас недоступно, откройте актуальный экран подписки.",
		myCallsEmpty:         "За последнюю неделю звонков нет. Пришлите ссылку на встречу, чтобы начать.",
		myCallsHeader:        "Ваши звонки за последнюю неделю:",
		jobCancelled:         "Запись отменена, встреча возвращена в баланс.",
		jobStopStarted:       "Запись остановлена. Готовлю расшифровку и пришлю результат после обработки.",
		jobCancelFailed:      "Не удалось отменить — возможно, звонок уже завершился. Обновите список «Мои звонки».",
		jobNotFound:          "Звонок не найден.",
		redeliverNotReady:    "Транскрипт по этому звонку ещё не готов.",
		redeliverFail:        "Не получилось достать транскрипт. Попробуйте позже.",
		summaryNotAvailable:  "Саммари по этому звонку недоступно.",
	}
}

// Bot - Telegram layer: commands, onboarding FSM, consent gate (architecture.md §4).
type Bot struct {
	bot           *tele.Bot
	storage       *Storage
	billing       *Billing
	summarizer    Summarizer
	texts         texts
	consentMarkup *tele.ReplyMarkup
	menuMarkup    *tele.ReplyMarkup // persistent reply keyboard of the main menu
	payMarkup     *tele.ReplyMarkup // inline payment buttons on the subscription screen

	trialLimitMinutes int

	paymentMu             sync.Mutex
	summaryMu             sync.Mutex
	summaryRestores       map[int64]*summaryRestore
	awaitPaymentEmail     map[int64]struct{}
	awaitPaymentAutoRenew map[int64]bool

	// providers - implementations of all configured providers. The "My calls" panel
	// resolves the right one by job.Provider: Cancel (stop the bot) and Fetch (re-fetch
	// the transcript) go to the provider that created the job, not the active one -
	// otherwise after a provider switch a foreign external_job_id gives HTTP 400.
	providers map[string]TranscriptionProvider
	// activeProvider - provider name for NEW jobs (= TRANSCRIPTION_PROVIDER),
	// written to job.Provider on CreateJob.
	activeProvider string

	recurringPaymentsEnabled bool
}

type summaryRestore struct {
	done    chan struct{}
	summary string
}

func NewBot(cfg Config, storage *Storage, billing *Billing, providers map[string]TranscriptionProvider, summarizer Summarizer) (*Bot, error) {
	pref := tele.Settings{
		Token:  cfg.BotToken,
		Poller: &tele.LongPoller{Timeout: 10 * time.Second},
	}
	tb, err := tele.NewBot(pref)
	if err != nil {
		return nil, fmt.Errorf("create telebot: %w", err)
	}

	b := &Bot{
		bot:                      tb,
		storage:                  storage,
		billing:                  billing,
		summarizer:               summarizer,
		texts:                    defaultTexts(),
		trialLimitMinutes:        cfg.TrialLimitMinutes,
		summaryRestores:          map[int64]*summaryRestore{},
		awaitPaymentEmail:        map[int64]struct{}{},
		awaitPaymentAutoRenew:    map[int64]bool{},
		providers:                providers,
		activeProvider:           cfg.TranscriptionProvider,
		recurringPaymentsEnabled: cfg.RecurringPaymentsEnabled,
	}
	b.setup()
	return b, nil
}

func (b *Bot) setup() {
	b.bot.Use(b.gate)

	b.bot.Handle("/start", b.onStart)
	b.bot.Handle("/help", b.onHelp)
	b.bot.Handle("/balance", b.onBalance)
	b.bot.Handle("/disable_autorenew", b.onDisableAutoRenew)

	m := &tele.ReplyMarkup{}
	accept := m.Data(b.texts.acceptButton, consentAccept)
	decline := m.Data(b.texts.declineButton, consentDecline)
	m.Inline(m.Row(accept, decline))
	b.consentMarkup = m
	b.bot.Handle(&accept, b.onConsentAccept)
	b.bot.Handle(&decline, b.onConsentDecline)

	// Main menu - a persistent reply keyboard at the bottom (shown after consent).
	// Buttons = all the main navigation (always at hand); commands (setCommands) -
	// utilities /start, /help only. This avoids duplicating one action as "command AND button".
	menu := &tele.ReplyMarkup{ResizeKeyboard: true}
	myCalls := menu.Text(menuMyCalls)
	subscribe := menu.Text(menuSubscribe)
	balance := menu.Text(menuBalance)
	about := menu.Text(menuAbout)
	support := menu.Text(menuSupport)
	supportOld := menu.Text(menuSupportOld)
	menu.Reply(menu.Row(myCalls), menu.Row(subscribe, balance), menu.Row(about, support))
	b.menuMarkup = menu
	b.bot.Handle(&myCalls, b.onMyCalls)
	b.bot.Handle(&subscribe, b.onSubscription)
	b.bot.Handle(&balance, b.onBalance)
	b.bot.Handle(&about, b.onAbout)
	b.bot.Handle(&support, b.onSupport)
	b.bot.Handle(&supportOld, b.onSupport)

	// Inline navigation and actions of the "My calls" panel. Registered per unique;
	// payload (id/page) goes in Data. List -> card -> actions, everything via c.Edit.
	openBtn := tele.Btn{Unique: jobOpen}
	backBtn := tele.Btn{Unique: jobBack}
	pageBtn := tele.Btn{Unique: jobsPage}
	cancelBtn := tele.Btn{Unique: jobCancel}
	redeliverBtn := tele.Btn{Unique: jobRedeliver}
	summaryBtn := tele.Btn{Unique: jobRedeliverSummary}
	b.bot.Handle(&openBtn, b.onJobOpen)
	b.bot.Handle(&backBtn, b.onJobBack)
	b.bot.Handle(&pageBtn, b.onJobsPage)
	b.bot.Handle(&cancelBtn, b.onCancelJob)
	b.bot.Handle(&redeliverBtn, b.onRedeliver)
	b.bot.Handle(&summaryBtn, b.onRedeliverSummary)

	// Payment button on the subscription screen. Legacy pay_auto_renew is kept as a hidden alias for old callbacks.
	pay := &tele.ReplyMarkup{}
	payBtn := pay.Data(b.texts.payButton, paySubscription)
	pay.Inline(pay.Row(payBtn))
	b.payMarkup = pay
	b.bot.Handle(&payBtn, b.onPaySubscription)
	legacyAutoPayBtn := tele.Btn{Unique: payAutoRenew}
	b.bot.Handle(&legacyAutoPayBtn, b.onPayAutoRenew)
	disableAutoPayBtn := tele.Btn{Unique: disableAutoRenew}
	renewPayBtn := tele.Btn{Unique: renewSubscription}
	b.bot.Handle(&disableAutoPayBtn, b.onDisableAutoRenew)
	b.bot.Handle(&renewPayBtn, b.onPaySubscription)

	// Legacy Telegram Payments are kept as a rollback baseline until a separate removal.
	// The target payment flow below goes through YooKassa direct payment links.
	b.bot.Handle(tele.OnCheckout, b.onPreCheckout)
	b.bot.Handle(tele.OnPayment, b.onSuccessfulPayment)

	b.bot.Handle(tele.OnText, b.onText)

	b.setCommands()
}

// setCommands sets the bot command list - the "Menu" button at the bottom left in the client.
// Best-effort: on a network error we log it but do not fail the bot startup.
func (b *Bot) setCommands() {
	// Utilities only: navigation is on reply buttons (see setup). /balance stays a
	// working handler, but is not duplicated in the command menu (there is a "Balance" button).
	cmds := []tele.Command{
		{Text: "start", Description: "Начать"},
		{Text: "help", Description: "Помощь"},
	}
	if err := b.bot.SetCommands(cmds); err != nil {
		slog.Warn("set bot commands", "err", err)
	}
}

// Start starts long polling (blocks until Stop).
func (b *Bot) Start() { b.bot.Start() }

// Stop stops long polling.
func (b *Bot) Stop() { b.bot.Stop() }

// gate - the single point: registers the user on user updates and blocks
// everything except the whitelist until 152-FZ consent exists (architecture.md §5/§9).
func (b *Bot) gate(next tele.HandlerFunc) tele.HandlerFunc {
	return func(c tele.Context) error {
		if isPaymentUpdate(c) {
			return next(c)
		}
		sender := c.Sender()
		if sender == nil {
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		user, err := b.storage.GetOrCreateUser(ctx, sender.ID)
		if err != nil {
			slog.Error("get or create user", "err", err, "user_id", sender.ID)
			// For a callback we clear the loading indicator, otherwise the user sees a "clock".
			// For a regular message we reply explicitly, otherwise silence reads as a hang.
			if c.Callback() != nil {
				if rerr := c.Respond(); rerr != nil {
					slog.Warn("respond callback", "err", rerr)
				}
			} else if serr := c.Send(b.texts.internalError); serr != nil {
				slog.Warn("send internal error", "err", serr)
			}
			return nil // do not continue the update: without user/consent the regular handlers are unsafe
		}

		if user.HasConsent() || allowedBeforeConsent(c) {
			c.Set("user", user)
			return next(c)
		}
		return c.Send(b.texts.needConsent)
	}
}

// isPaymentUpdate lets Telegram Payments callbacks past the consent gate.
// pre_checkout must answer quickly and without the DB; successful_payment creates
// the user itself before recording the payment, so as not to lose charged money because of the gate.
func isPaymentUpdate(c tele.Context) bool {
	return c.PreCheckoutQuery() != nil || c.Payment() != nil
}

// allowedBeforeConsent - whitelist by exact values (architecture.md §5, SF-3):
// before consent_at only /start, /help and the consent callbacks pass.
func allowedBeforeConsent(c tele.Context) bool {
	if cb := c.Callback(); cb != nil {
		return cb.Unique == consentAccept || cb.Unique == consentDecline
	}
	if m := c.Message(); m != nil {
		// Take the command itself without arguments and @botname (/start@bot, /start arg).
		cmd := m.Text
		if i := strings.IndexAny(cmd, " @"); i >= 0 {
			cmd = cmd[:i]
		}
		return cmd == "/start" || cmd == "/help"
	}
	return false
}

func (b *Bot) onStart(c tele.Context) error {
	user, _ := c.Get("user").(User)
	if user.HasConsent() {
		err := c.Send(b.texts.welcome, b.menuMarkup)
		b.trackAnalytics(user.TelegramID, analyticsBotStart, "", nil)
		return err
	}
	err := c.Send(b.texts.consent, b.consentMarkup)
	b.trackAnalytics(user.TelegramID, analyticsBotStart, "", nil)
	return err
}

func (b *Bot) onHelp(c tele.Context) error {
	return c.Send(b.texts.help)
}

// onSubscription - subscription screen with an inline payment button (facade).
func (b *Bot) onSubscription(c tele.Context) error {
	user, _ := c.Get("user").(User)
	err := c.Send(b.texts.subscription, b.payMarkup)
	b.trackAnalytics(user.TelegramID, analyticsSubscriptionViewed, "", nil)
	return err
}

func (b *Bot) onAbout(c tele.Context) error {
	return c.Send(b.texts.about)
}

func (b *Bot) onSupport(c tele.Context) error {
	return c.Send(b.texts.support)
}

// onBalance shows the remaining quota: an active subscription or the trial remainder (F6).
func (b *Bot) onBalance(c tele.Context) error {
	// gate puts the user in the context after consent; the guard enforces the invariant explicitly.
	user, ok := c.Get("user").(User)
	if !ok || user.TelegramID == 0 {
		slog.Error("onBalance: user missing in context")
		return c.Send(b.texts.internalError)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	bal, err := b.storage.Balance(ctx, user.TelegramID, b.trialLimitMinutes)
	if err != nil {
		slog.Error("balance", "err", err, "user_id", user.TelegramID)
		return c.Send(b.texts.internalError)
	}
	recurring, hasRecurring, err := b.storage.RecurringProfile(ctx, user.TelegramID)
	if err != nil {
		slog.Error("recurring profile", "err", err, "user_id", user.TelegramID)
		return c.Send(b.texts.internalError)
	}
	var text string
	if bal.IsSubscription {
		text = fmt.Sprintf("Подписка активна. Осталось: %s.", formatMinutesBalance(bal.Remaining))
		if bal.ExpiresAt != nil {
			text = fmt.Sprintf("Подписка активна. Осталось: %s. Действует до %s.",
				formatMinutesBalance(bal.Remaining), bal.ExpiresAt.Format("02.01.2006"))
		}
	} else {
		text = fmt.Sprintf("Бесплатный баланс: осталось %s из %s.",
			formatMinutesBalance(bal.Remaining), formatMinutesBalance(bal.Limit))
	}
	recurringText, _ := formatRecurringStatus(recurring, hasRecurring, b.recurringPaymentsEnabled)
	// YooKassa activation requires a visible self-service detach control even before a saved method exists.
	canDisable := true
	if hasRecurring && recurring.Status != RecurringDisabled {
		text += "\n" + recurringText
	} else if bal.IsSubscription {
		text += "\n" + recurringText
	}
	return c.Send(text, b.balanceMarkup(canDisable, bal.IsSubscription))
}

func formatRecurringStatus(p RecurringProfile, ok bool, recurringEnabled bool) (string, bool) {
	if !ok {
		return "Автопродление: не включено.", false
	}
	if !recurringEnabled && p.Status != RecurringDisabled {
		return "Автопродление сейчас выключено.", true
	}
	switch p.Status {
	case RecurringActive:
		return fmt.Sprintf("Автопродление: включено. Следующее списание %s.", p.NextChargeAt.Format("02.01.2006")), true
	case RecurringCharging:
		return "Автопродление: идёт списание.", true
	case RecurringChargeFailed:
		return fmt.Sprintf("Автопродление: не удалось списать оплату. Следующая попытка %s.", p.NextChargeAt.Format("02.01.2006")), true
	case RecurringBlocked:
		return "Автопродление: требует проверки поддержки.", true
	case RecurringDisabled:
		return "Автопродление: отключено.", false
	default:
		return "Автопродление: статус требует проверки поддержки.", true
	}
}

func (b *Bot) balanceMarkup(canDisable bool, isSubscription bool) *tele.ReplyMarkup {
	if !canDisable && (b.billing == nil || !b.billing.Enabled()) {
		return nil
	}
	markup := &tele.ReplyMarkup{}
	var rows []tele.Row
	if canDisable {
		btn := markup.Data("Отключить автопродление", disableAutoRenew)
		rows = append(rows, markup.Row(btn))
	}
	if b.billing != nil && b.billing.Enabled() {
		label := "Оформить подписку"
		if isSubscription {
			label = "Обновить подписку"
		}
		pay := markup.Data(label, renewSubscription)
		rows = append(rows, markup.Row(pay))
	}
	markup.Inline(rows...)
	return markup
}

func (b *Bot) onDisableAutoRenew(c tele.Context) error {
	chatID := int64(0)
	if chat := c.Chat(); chat != nil {
		chatID = chat.ID
	}
	source := "command"
	if c.Callback() != nil {
		source = "callback"
		if err := c.Respond(); err != nil {
			slog.Warn("respond callback", "err", err)
		}
	}
	user, ok := c.Get("user").(User)
	if !ok || user.TelegramID == 0 {
		slog.Error("onDisableAutoRenew: user missing in context")
		return c.Send(b.texts.internalError)
	}
	slog.Debug("recurring disable requested",
		"event", "recurring_disable_requested", "phase", "recurring",
		"user_id", user.TelegramID, "chat_id", chatID, "source", source)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	changed, err := b.storage.DisableRecurring(ctx, user.TelegramID)
	if err != nil {
		slog.Error("disable recurring",
			"event", "recurring_disable_failed", "phase", "recurring",
			"err", err, "user_id", user.TelegramID, "chat_id", chatID, "source", source)
		return c.Send(b.texts.internalError)
	}
	if !changed {
		slog.Debug("recurring disable noop",
			"event", "recurring_disable_noop", "phase", "recurring",
			"user_id", user.TelegramID, "chat_id", chatID, "source", source)
		return c.Send(b.texts.autoRenewAlreadyOff)
	}
	slog.Debug("recurring disabled",
		"event", "recurring_disable_completed", "phase", "recurring",
		"user_id", user.TelegramID, "chat_id", chatID, "source", source)
	return c.Send(b.texts.autoRenewDisabled)
}

func formatMinutesBalance(minutes int) string {
	if minutes < 0 {
		minutes = 0
	}
	hours := minutes / 60
	rest := minutes % 60
	if hours == 0 {
		return fmt.Sprintf("%d %s", rest, minuteWord(rest))
	}
	if rest == 0 {
		return fmt.Sprintf("%d %s", hours, hourWord(hours))
	}
	return fmt.Sprintf("%d %s %d %s", hours, hourWord(hours), rest, minuteWord(rest))
}

func hourWord(hours int) string {
	n := hours % 100
	if n >= 11 && n <= 14 {
		return "часов"
	}
	switch hours % 10 {
	case 1:
		return "час"
	case 2, 3, 4:
		return "часа"
	default:
		return "часов"
	}
}

func minuteWord(minutes int) string {
	n := minutes % 100
	if n >= 11 && n <= 14 {
		return "минут"
	}
	switch minutes % 10 {
	case 1:
		return "минута"
	case 2, 3, 4:
		return "минуты"
	default:
		return "минут"
	}
}

// jobStatusLabel - human-readable call status for the "My calls" panel.
func jobStatusLabel(s JobStatus) string {
	switch s {
	case JobCreated:
		return "📤 ожидает отправки"
	case JobJoining:
		return "🤖 бот подключается"
	case JobRecording:
		return "🔴 бот на встрече"
	case JobProcessing:
		return "⏳ обрабатываю запись"
	case JobSummarizing:
		return "⏳ готовлю саммари"
	case JobDone:
		return "✅ готово"
	case JobFailedSummary:
		return "✅ расшифровка готова, конспект не готов"
	case JobFailed:
		return "⚠️ не выполнено"
	default:
		return string(s)
	}
}

// jobPayload encodes jobID and the list page into the inline button payload ("id:page").
// The page is needed to return from the card to the same list page.
func jobPayload(jobID int64, page int) string {
	return strconv.FormatInt(jobID, 10) + ":" + strconv.Itoa(page)
}

// parseJobPayload parses "id:page". ok=false on garbage (callback is not in our format).
func parseJobPayload(data string) (jobID int64, page int, ok bool) {
	id, rest, found := strings.Cut(data, ":")
	jobID, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	if found {
		// Garbage page -> 0: the payload is ours, the page is not critical for correctness.
		page, _ = strconv.Atoi(rest)
	}
	return jobID, page, true
}

// renderCallsList builds the text+markup of a calls list page (by callsPageSize).
// empty=true - there are no calls on the page (show myCallsEmpty without buttons).
func (b *Bot) renderCallsList(ctx context.Context, userID int64, page int) (string, *tele.ReplyMarkup, bool, error) {
	if page < 0 {
		page = 0
	}
	// +1 row beyond the page - to find out whether there is a next one (without COUNT).
	jobs, err := b.storage.ListUserJobs(ctx, userID, time.Now().Add(-callsHistoryLimit), callsPageSize+1, page*callsPageSize)
	if err != nil {
		return "", nil, false, err
	}
	hasNext := len(jobs) > callsPageSize
	if hasNext {
		jobs = jobs[:callsPageSize]
	}
	if len(jobs) == 0 {
		return b.texts.myCallsEmpty, nil, true, nil
	}

	var sb strings.Builder
	sb.WriteString(b.texts.myCallsHeader)
	markup := &tele.ReplyMarkup{}
	var rows []tele.Row
	for _, j := range jobs {
		ts := formatCallShortTime(j.CreatedAt)
		fmt.Fprintf(&sb, "\n• %s — %s", ts, jobStatusLabel(j.Status))
		rows = append(rows, markup.Row(markup.Data(ts, jobOpen, jobPayload(j.ID, page))))
	}
	var nav []tele.Btn
	if page > 0 {
		nav = append(nav, markup.Data("◀ Назад", jobsPage, strconv.Itoa(page-1)))
	}
	if hasNext {
		nav = append(nav, markup.Data("Вперёд ▶", jobsPage, strconv.Itoa(page+1)))
	}
	if len(nav) > 0 {
		rows = append(rows, markup.Row(nav...))
	}
	markup.Inline(rows...)
	return sb.String(), markup, false, nil
}

// renderCallCard builds a call card: status + actions by status + "to list".
func (b *Bot) renderCallCard(ctx context.Context, userID, jobID int64, page int) (string, *tele.ReplyMarkup, error) {
	job, err := b.storage.UserJob(ctx, userID, jobID)
	if err != nil {
		return "", nil, err
	}
	text := fmt.Sprintf("Звонок %s\nСтатус: %s",
		formatCallTime(job.CreatedAt), jobStatusLabel(job.Status))
	markup := &tele.ReplyMarkup{}
	var rows []tele.Row
	switch job.Status {
	case JobCreated, JobJoining:
		rows = append(rows, markup.Row(markup.Data("✖ Отменить", jobCancel, jobPayload(jobID, page))))
	case JobRecording:
		rows = append(rows, markup.Row(markup.Data("⏹ Остановить запись", jobCancel, jobPayload(jobID, page))))
	case JobDone, JobFailedSummary:
		rows = append(rows, markup.Row(markup.Data("📄 Получить транскрипт", jobRedeliver, jobPayload(jobID, page))))
		rows = append(rows, markup.Row(markup.Data("📝 Получить конспект", jobRedeliverSummary, jobPayload(jobID, page))))
	}
	rows = append(rows, markup.Row(markup.Data("◀ к списку", jobBack, strconv.Itoa(page))))
	markup.Inline(rows...)
	return text, markup, nil
}

// editCard redraws the call card in place (after a cancel, etc.).
func (b *Bot) editCard(c tele.Context, userID, jobID int64, page int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	text, markup, err := b.renderCallCard(ctx, userID, jobID, page)
	if err != nil {
		if errors.Is(err, ErrJobNotFound) {
			return c.Edit(b.texts.jobNotFound)
		}
		slog.Error("render call card", "err", err, "job_id", jobID)
		return nil
	}
	return c.Edit(text, markup)
}

// onMyCalls opens the "My calls" panel (the first message is new, then navigation
// edits it in place).
func (b *Bot) onMyCalls(c tele.Context) error {
	user, ok := c.Get("user").(User)
	if !ok || user.TelegramID == 0 {
		slog.Error("onMyCalls: user missing in context")
		return c.Send(b.texts.internalError)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	text, markup, empty, err := b.renderCallsList(ctx, user.TelegramID, 0)
	if err != nil {
		slog.Error("render calls list", "err", err, "user_id", user.TelegramID)
		return c.Send(b.texts.internalError)
	}
	if empty {
		return c.Send(text)
	}
	return c.Send(text, markup)
}

// onJobsPage pages the list (left/right) - edits the same message. Payload - page index.
func (b *Bot) onJobsPage(c tele.Context) error {
	if err := c.Respond(); err != nil {
		slog.Warn("respond callback", "err", err)
	}
	user, ok := c.Get("user").(User)
	if !ok || user.TelegramID == 0 {
		return c.Edit(b.texts.internalError)
	}
	page, err := strconv.Atoi(c.Data())
	if err != nil {
		slog.Error("onJobsPage: bad page", "data", c.Data())
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	text, markup, empty, err := b.renderCallsList(ctx, user.TelegramID, page)
	if err != nil {
		slog.Error("render calls list", "err", err, "user_id", user.TelegramID)
		return nil
	}
	if empty {
		return c.Edit(text, &tele.ReplyMarkup{})
	}
	return c.Edit(text, markup)
}

// onJobOpen opens the call card - edits the list into the card.
func (b *Bot) onJobOpen(c tele.Context) error {
	if err := c.Respond(); err != nil {
		slog.Warn("respond callback", "err", err)
	}
	user, ok := c.Get("user").(User)
	if !ok || user.TelegramID == 0 {
		return c.Edit(b.texts.internalError)
	}
	jobID, page, ok := parseJobPayload(c.Data())
	if !ok {
		slog.Error("onJobOpen: bad payload", "data", c.Data())
		return nil
	}
	return b.editCard(c, user.TelegramID, jobID, page)
}

// onJobBack returns from the card to the list at the same page (edits the message).
func (b *Bot) onJobBack(c tele.Context) error {
	if err := c.Respond(); err != nil {
		slog.Warn("respond callback", "err", err)
	}
	user, ok := c.Get("user").(User)
	if !ok || user.TelegramID == 0 {
		return c.Edit(b.texts.internalError)
	}
	page, err := strconv.Atoi(c.Data())
	if err != nil {
		page = 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	text, markup, empty, err := b.renderCallsList(ctx, user.TelegramID, page)
	if err != nil {
		slog.Error("render calls list", "err", err, "user_id", user.TelegramID)
		return nil
	}
	if empty {
		return c.Edit(text, &tele.ReplyMarkup{})
	}
	return c.Edit(text, markup)
}

// onCancelJob handles the card action: created/joining are cancelled with refund,
// recording stops the recording and waits for the transcript. Payload - "id:page".
func (b *Bot) onCancelJob(c tele.Context) error {
	user, ok := c.Get("user").(User)
	if !ok || user.TelegramID == 0 {
		slog.Error("onCancelJob: user missing in context")
		return c.Respond(&tele.CallbackResponse{Text: b.texts.internalError})
	}
	jobID, page, ok := parseJobPayload(c.Data())
	if !ok {
		slog.Error("onCancelJob: bad payload", "data", c.Data())
		return c.Respond()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	slog.Info("telegram job cancel requested",
		"event", "telegram_callback", "phase", "telegram",
		"callback", jobCancel, "user_id", user.TelegramID, "chat_id", user.TelegramID, "job_id", jobID)

	job, err := b.storage.UserJob(ctx, user.TelegramID, jobID)
	if err != nil {
		if errors.Is(err, ErrJobNotFound) {
			return c.Respond(&tele.CallbackResponse{Text: b.texts.jobNotFound})
		}
		slog.Error("read job before cancel", "err", err, "user_id", user.TelegramID, "job_id", jobID)
		return c.Respond(&tele.CallbackResponse{Text: b.texts.internalError})
	}

	switch job.Status {
	case JobCreated, JobJoining:
		extID, prov, _, err := b.storage.CancelJob(ctx, user.TelegramID, jobID)
		if err != nil {
			if errors.Is(err, ErrJobNotCancelable) {
				if rerr := c.Respond(&tele.CallbackResponse{Text: b.texts.jobCancelFailed}); rerr != nil {
					slog.Warn("respond callback", "err", rerr)
				}
				return b.editCard(c, user.TelegramID, jobID, page)
			}
			slog.Error("cancel job", "err", err, "user_id", user.TelegramID, "job_id", jobID)
			return c.Respond(&tele.CallbackResponse{Text: b.texts.internalError})
		}
		if extID != nil {
			b.cancelProviderBestEffort(jobID, user.TelegramID, prov, *extID)
		}
		slog.Info("telegram job cancelled",
			"event", "job_cancelled_by_user", "phase", "telegram",
			"job_id", jobID, "user_id", user.TelegramID, "provider", prov, "external_job_id", extID)
		if rerr := c.Respond(&tele.CallbackResponse{Text: b.texts.jobCancelled}); rerr != nil {
			slog.Warn("respond callback", "err", rerr)
		}
		return b.editCard(c, user.TelegramID, jobID, page)

	case JobRecording:
		extID, prov, err := b.storage.MarkJobManualStop(ctx, user.TelegramID, jobID)
		if err != nil {
			if errors.Is(err, ErrJobNotCancelable) {
				if rerr := c.Respond(&tele.CallbackResponse{Text: b.texts.jobCancelFailed}); rerr != nil {
					slog.Warn("respond callback", "err", rerr)
				}
				return b.editCard(c, user.TelegramID, jobID, page)
			}
			slog.Error("mark manual stop", "err", err, "user_id", user.TelegramID, "job_id", jobID)
			return c.Respond(&tele.CallbackResponse{Text: b.texts.internalError})
		}
		p, ok := b.providers[prov]
		if !ok {
			slog.Warn("stop job: provider not configured",
				"event", "provider_missing", "phase", "cancel",
				"job_id", jobID, "user_id", user.TelegramID, "provider", prov)
			b.refundStoppedJob(context.Background(), jobID, "manual stop provider missing")
			if rerr := c.Respond(&tele.CallbackResponse{Text: b.texts.jobCancelled}); rerr != nil {
				slog.Warn("respond callback", "err", rerr)
			}
			return b.editCard(c, user.TelegramID, jobID, page)
		}
		pctx, pcancel := context.WithTimeout(context.Background(), 30*time.Second)
		cerr := p.Cancel(pctx, extID)
		pcancel()
		if cerr != nil {
			slog.Warn("stop job at provider",
				"event", "provider_cancel_failed", "phase", "cancel",
				"err", cerr, "job_id", jobID, "user_id", user.TelegramID,
				"provider", prov, "external_job_id", extID)
			b.refundStoppedJob(context.Background(), jobID, fmt.Sprintf("manual stop provider failed: %v", cerr))
			if rerr := c.Respond(&tele.CallbackResponse{Text: b.texts.jobCancelled}); rerr != nil {
				slog.Warn("respond callback", "err", rerr)
			}
			return b.editCard(c, user.TelegramID, jobID, page)
		}
		pctx, pcancel = context.WithTimeout(context.Background(), 5*time.Second)
		moved, err := b.storage.SetJobProcessing(pctx, jobID)
		if err != nil {
			pcancel()
			slog.Error("set stopped job processing", "err", err, "user_id", user.TelegramID, "job_id", jobID)
			return c.Respond(&tele.CallbackResponse{Text: b.texts.internalError})
		}
		pcancel()
		if !moved {
			rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
			latest, err := b.storage.UserJob(rctx, user.TelegramID, jobID)
			rcancel()
			if err != nil {
				slog.Error("read stopped job after processing race", "err", err, "user_id", user.TelegramID, "job_id", jobID)
				return c.Respond(&tele.CallbackResponse{Text: b.texts.internalError})
			}
			if latest.Status != JobProcessing && latest.Status != JobSummarizing && latest.Status != JobDone && latest.Status != JobFailedSummary {
				slog.Warn("manual stop did not move job to processing",
					"event", "job_transition_race", "phase", "telegram",
					"job_id", jobID, "user_id", user.TelegramID,
					"provider", prov, "external_job_id", extID, "current_status", latest.Status)
				if rerr := c.Respond(&tele.CallbackResponse{Text: b.texts.jobCancelFailed}); rerr != nil {
					slog.Warn("respond callback", "err", rerr)
				}
				return b.editCard(c, user.TelegramID, jobID, page)
			}
		}
		slog.Info("telegram job stop requested",
			"event", "job_stop_requested_by_user", "phase", "telegram",
			"job_id", jobID, "user_id", user.TelegramID, "provider", prov, "external_job_id", extID)
		if rerr := c.Respond(&tele.CallbackResponse{Text: b.texts.jobStopStarted}); rerr != nil {
			slog.Warn("respond callback", "err", rerr)
		}
		return b.editCard(c, user.TelegramID, jobID, page)

	default:
		if rerr := c.Respond(&tele.CallbackResponse{Text: b.texts.jobCancelFailed}); rerr != nil {
			slog.Warn("respond callback", "err", rerr)
		}
		return b.editCard(c, user.TelegramID, jobID, page)
	}
}

func (b *Bot) cancelProviderBestEffort(jobID, userID int64, provider, extID string) {
	p, ok := b.providers[provider]
	if !ok {
		slog.Warn("cancel job: provider not configured",
			"event", "provider_missing", "phase", "cancel",
			"job_id", jobID, "user_id", userID, "provider", provider)
		return
	}
	pctx, pcancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := p.Cancel(pctx, extID); err != nil {
		slog.Warn("cancel job at provider",
			"event", "provider_cancel_failed", "phase", "cancel",
			"err", err, "job_id", jobID, "user_id", userID,
			"provider", provider, "external_job_id", extID)
	}
	pcancel()
}

func (b *Bot) refundStoppedJob(ctx context.Context, jobID int64, reason string) {
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := b.storage.FailJobWithQuotaRefund(rctx, jobID, reason); err != nil {
		slog.Error("refund stopped job", "err", err, "job_id", jobID)
	}
}

// onRedeliver re-fetches the transcript of a finished call from the provider (s2t keeps it for
// 90 days) and sends it as a file (the card stays). Payload - "id:page".
func (b *Bot) onRedeliver(c tele.Context) error {
	user, ok := c.Get("user").(User)
	if !ok || user.TelegramID == 0 {
		slog.Error("onRedeliver: user missing in context")
		return c.Respond(&tele.CallbackResponse{Text: b.texts.internalError})
	}
	jobID, _, ok := parseJobPayload(c.Data())
	if !ok {
		slog.Error("onRedeliver: bad payload", "data", c.Data())
		return c.Respond()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	job, err := b.storage.UserJob(ctx, user.TelegramID, jobID)
	if err != nil {
		if errors.Is(err, ErrJobNotFound) {
			return c.Respond(&tele.CallbackResponse{Text: b.texts.jobNotFound})
		}
		slog.Error("user job", "err", err, "user_id", user.TelegramID, "job_id", jobID)
		return c.Respond(&tele.CallbackResponse{Text: b.texts.internalError})
	}
	if (job.Status != JobDone && job.Status != JobFailedSummary) || job.ExternalJobID == nil {
		return c.Respond(&tele.CallbackResponse{Text: b.texts.redeliverNotReady})
	}
	slog.Info("telegram redeliver requested",
		"event", "telegram_callback", "phase", "telegram",
		"callback", jobRedeliver, "job_id", jobID, "user_id", user.TelegramID,
		"provider", job.Provider, "external_job_id", *job.ExternalJobID)
	// Resolve the creating provider by job.Provider: external_job_id belongs to its
	// namespace. The active one may have changed (TRANSCRIPTION_PROVIDER) - its Fetch would return
	// HTTP 400 on a foreign id. Absent from the map (provider disabled / old job with provider='') -
	// honestly report that the recording is unavailable, instead of a vague error.
	prov, ok := b.providers[job.Provider]
	if !ok {
		slog.Warn("redeliver: provider not configured", "job_id", jobID, "provider", job.Provider)
		return c.Respond(&tele.CallbackResponse{Text: b.texts.redeliverFail})
	}
	pctx, pcancel := context.WithTimeout(context.Background(), 30*time.Second)
	result, ferr := prov.Fetch(pctx, *job.ExternalJobID)
	pcancel()
	if ferr != nil {
		slog.Error("redeliver fetch",
			"event", "provider_fetch_failed", "phase", "redeliver",
			"err", ferr, "job_id", jobID, "user_id", user.TelegramID,
			"provider", job.Provider, "external_job_id", *job.ExternalJobID)
		return c.Respond(&tele.CallbackResponse{Text: b.texts.redeliverFail})
	}
	if rerr := c.Respond(); rerr != nil {
		slog.Warn("respond callback", "err", rerr)
	}
	doc := &tele.Document{
		File:     tele.FromReader(strings.NewReader(result.TranscriptMarkdown)),
		FileName: transcriptFilename(job.CreatedAt),
		Caption:  "📄 Расшифровка встречи от " + formatCallTime(job.CreatedAt),
	}
	return c.Send(doc)
}

// onRedeliverSummary re-issues or restores the summary of a finished call.
// The main path is the summary_md cache; for a terminal job without a cache we try to fetch the
// transcript from the provider again and generate the summary.
func (b *Bot) onRedeliverSummary(c tele.Context) error {
	user, ok := c.Get("user").(User)
	if !ok || user.TelegramID == 0 {
		slog.Error("onRedeliverSummary: user missing in context")
		return c.Respond(&tele.CallbackResponse{Text: b.texts.internalError})
	}
	jobID, _, ok := parseJobPayload(c.Data())
	if !ok {
		slog.Error("onRedeliverSummary: bad payload", "data", c.Data())
		return c.Respond()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	job, err := b.storage.UserJob(ctx, user.TelegramID, jobID)
	if err != nil {
		if errors.Is(err, ErrJobNotFound) {
			return c.Respond(&tele.CallbackResponse{Text: b.texts.jobNotFound})
		}
		slog.Error("user job", "err", err, "user_id", user.TelegramID, "job_id", jobID)
		return c.Respond(&tele.CallbackResponse{Text: b.texts.internalError})
	}
	if (job.Status != JobDone && job.Status != JobFailedSummary) || job.ExternalJobID == nil {
		return c.Respond(&tele.CallbackResponse{Text: b.texts.redeliverNotReady})
	}
	slog.Info("telegram summary redeliver requested",
		"event", "telegram_callback", "phase", "telegram",
		"callback", jobRedeliverSummary, "job_id", jobID, "user_id", user.TelegramID,
		"provider", job.Provider, "external_job_id", *job.ExternalJobID)

	summary := ""
	if job.SummaryMD != nil {
		summary = strings.TrimSpace(*job.SummaryMD)
	}
	if summary == "" {
		if rerr := c.Respond(&tele.CallbackResponse{Text: "Готовлю конспект. Пришлю сюда, когда будет готово."}); rerr != nil {
			slog.Warn("respond callback", "err", rerr)
		}
		summary = b.restoreJobSummary(c, job)
		if summary == "" {
			return nil
		}
	} else if rerr := c.Respond(); rerr != nil {
		slog.Warn("respond callback", "err", rerr)
	}

	return c.Send(summaryDocument(job, summary))
}

func (b *Bot) restoreJobSummary(c tele.Context, job Job) (summary string) {
	b.summaryMu.Lock()
	if b.summaryRestores == nil {
		b.summaryRestores = make(map[int64]*summaryRestore)
	}
	if restore, ok := b.summaryRestores[job.ID]; ok {
		b.summaryMu.Unlock()
		<-restore.done
		if restore.summary == "" {
			_ = c.Send(b.texts.summaryNotAvailable)
		}
		return restore.summary
	}
	restore := &summaryRestore{done: make(chan struct{})}
	b.summaryRestores[job.ID] = restore
	b.summaryMu.Unlock()
	defer func() {
		b.summaryMu.Lock()
		restore.summary = summary
		delete(b.summaryRestores, job.ID)
		close(restore.done)
		b.summaryMu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	latest, err := b.storage.UserJob(ctx, job.UserID, job.ID)
	cancel()
	if err != nil {
		slog.Error("reload job before summary restore", "err", err, "job_id", job.ID, "user_id", job.UserID)
		_ = c.Send(b.texts.internalError)
		return ""
	}
	if latest.SummaryMD != nil && strings.TrimSpace(*latest.SummaryMD) != "" {
		return strings.TrimSpace(*latest.SummaryMD)
	}
	if latest.ExternalJobID == nil {
		_ = c.Send(b.texts.redeliverNotReady)
		return ""
	}

	prov, ok := b.providers[latest.Provider]
	if !ok {
		slog.Warn("restore summary: provider not configured", "job_id", latest.ID, "provider", latest.Provider)
		_ = c.Send(b.texts.redeliverFail)
		return ""
	}
	pctx, pcancel := context.WithTimeout(context.Background(), 30*time.Second)
	result, ferr := prov.Fetch(pctx, *latest.ExternalJobID)
	pcancel()
	if ferr != nil {
		slog.Error("restore summary fetch",
			"event", "provider_fetch_failed", "phase", "redeliver",
			"err", ferr, "job_id", latest.ID, "user_id", latest.UserID,
			"provider", latest.Provider, "external_job_id", *latest.ExternalJobID)
		_ = c.Send(b.texts.redeliverFail)
		return ""
	}

	summary = strings.TrimSpace(result.SummaryMarkdown)
	if summary == "" {
		if strings.TrimSpace(result.TranscriptMarkdown) == "" {
			_ = c.Send(b.texts.summaryNotAvailable)
			return ""
		}
		if b.summarizer == nil {
			_ = c.Send(b.texts.summaryNotAvailable)
			return ""
		}
		sctx, scancel := context.WithTimeout(context.Background(), summaryTimeout)
		generated, serr := b.summarizer.Summarize(sctx, result.TranscriptMarkdown)
		scancel()
		if serr != nil {
			slog.Error("restore summary generate",
				"event", "summary_failed", "phase", "redeliver",
				"err", serr, "job_id", latest.ID, "user_id", latest.UserID,
				"provider", latest.Provider, "external_job_id", *latest.ExternalJobID)
			_ = c.Send(b.texts.summaryNotAvailable)
			return ""
		}
		summary = strings.TrimSpace(generated)
	}
	if summary == "" {
		_ = c.Send(b.texts.summaryNotAvailable)
		return ""
	}

	dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
	err = b.storage.SetTerminalJobSummary(dctx, latest.ID, summary)
	dcancel()
	if err != nil {
		slog.Error("store restored summary",
			"event", "summary_cache_failed", "phase", "redeliver",
			"err", err, "job_id", latest.ID, "user_id", latest.UserID)
		_ = c.Send(b.texts.internalError)
		return ""
	}
	slog.Info("summary restored",
		"event", "summary_restored", "phase", "redeliver",
		"job_id", latest.ID, "user_id", latest.UserID,
		"provider", latest.Provider, "external_job_id", *latest.ExternalJobID,
		"summary_chars", len(summary))
	return summary
}

func summaryDocument(job Job, summary string) *tele.Document {
	return &tele.Document{
		File:     tele.FromReader(strings.NewReader(summary)),
		FileName: summaryFilename(job.CreatedAt),
		Caption:  "📝 Саммари встречи от " + formatCallTime(job.CreatedAt),
	}
}

// onPaySubscription starts the YooKassa payment link flow.
func (b *Bot) onPaySubscription(c tele.Context) error {
	return b.onPay(c, b.recurringPaymentsEnabled)
}

func (b *Bot) onPayAutoRenew(c tele.Context) error {
	if b.recurringPaymentsEnabled {
		return b.onPay(c, true)
	}
	if err := c.Respond(); err != nil {
		slog.Warn("respond callback", "err", err)
	}
	return c.Send(b.texts.autoRenewUnavailable)
}

func (b *Bot) onPay(c tele.Context, autoRenew bool) error {
	// First clear the "clock" on the callback, then send the invoice/message.
	if err := c.Respond(); err != nil {
		slog.Warn("respond callback", "err", err)
	}
	if c.Sender() == nil {
		slog.Error("onPaySubscription: nil sender")
		return c.Send(b.texts.internalError)
	}
	userID := c.Sender().ID
	defer b.trackAnalytics(userID, analyticsPaymentRequested, "", nil)
	if !b.billing.Enabled() {
		slog.Info("payment invoice requested while disabled",
			"event", "payment_invoice_disabled", "phase", "payment",
			"user_id", userID, "chat_id", userID)
		return c.Send(b.texts.payFacade)
	}
	slog.Info("payment link requested",
		"event", "payment_link_requested", "phase", "payment",
		"user_id", userID, "chat_id", userID, "auto_renew", autoRenew)
	return b.startPayment(c, userID, autoRenew)
}

func (b *Bot) startPayment(c tele.Context, userID int64, autoRenew bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	email, err := b.storage.PaymentEmail(ctx, userID)
	if err != nil {
		slog.Error("payment email lookup", "err", err, "user_id", userID)
		return c.Send(b.texts.internalError)
	}
	if email == "" {
		b.waitPaymentEmail(userID, autoRenew)
		return c.Send(b.texts.payEmailRequest)
	}
	return b.sendPaymentLink(c, userID, email, autoRenew)
}

func (b *Bot) sendPaymentLink(c tele.Context, userID int64, email string, autoRenew bool) error {
	idemKey, err := newIdempotenceKey()
	if err != nil {
		slog.Error("payment idempotence key", "err", err, "user_id", userID)
		return c.Send(b.texts.internalError)
	}

	checkCtx, checkCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer checkCancel()
	intent := PaymentInput{
		UserID:         userID,
		AmountKopecks:  b.billing.priceKopecks,
		Currency:       "RUB",
		IntentKey:      idemKey,
		InvoicePayload: payloadPrefix + strconv.FormatInt(userID, 10),
		Metadata:       paymentLinkMetadata(nil, email, autoRenew),
	}
	created, err := b.storage.StartDirectPaymentIntent(checkCtx, intent, idemKey, email, time.Now().Add(-freshPendingPaymentWindow))
	if err != nil {
		slog.Error("start direct payment intent", "err", err, "user_id", userID)
		return c.Send(b.texts.internalError)
	}
	if !created {
		slog.Info("payment link skipped: fresh pending payment exists",
			"event", "payment_link_pending_exists", "phase", "payment",
			"user_id", userID, "chat_id", userID)
		return c.Send(b.texts.payPendingExists)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	payment, err := b.billing.CreatePayment(ctx, userID, email, idemKey, autoRenew)
	if err != nil {
		slog.Error("create yookassa payment", "err", err, "user_id", userID)
		failCtx, failCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if failErr := b.storage.FailDirectPaymentIntent(failCtx, idemKey, err.Error()); failErr != nil {
			slog.Error("fail direct payment intent", "err", failErr, "user_id", userID)
		}
		failCancel()
		return c.Send(b.texts.internalError)
	}
	metadata := paymentLinkMetadata(payment.Metadata, email, autoRenew)
	if err := b.storage.AttachDirectProviderPayment(ctx, idemKey, payment, metadata); err != nil {
		slog.Error("store pending yookassa payment", "err", err, "user_id", userID, "payment_id", payment.ID)
		return c.Send(b.texts.internalError)
	}

	markup := &tele.ReplyMarkup{}
	btn := markup.URL(b.texts.payOpenButton, payment.ConfirmationURL)
	markup.Inline(markup.Row(btn))
	slog.Info("payment link created",
		"event", "payment_link_created", "phase", "payment",
		"user_id", userID, "chat_id", userID, "payment_id", payment.ID, "auto_renew", autoRenew)
	return c.Send(b.texts.payLinkReady, markup)
}

// onPreCheckout answers pre_checkout_query synchronously (<=10s, §5.5): only
// in-memory validation of the payload format, no DB. The subscription business logic is
// in onSuccessfulPayment (§5.4) under the protection of UNIQUE(telegram_payment_charge_id).
func (b *Bot) onPreCheckout(c tele.Context) error {
	q := c.PreCheckoutQuery()
	// nil-guard on the payment path: telebot routes here only when a query exists,
	// but a malformed update must not panic on q.Payload.
	if q == nil {
		slog.Error("onPreCheckout: nil pre_checkout_query")
		return nil
	}
	if !b.billing.ValidatePreCheckout(q.Payload) {
		slog.Warn("payment pre_checkout rejected",
			"event", "payment_pre_checkout_rejected", "phase", "payment",
			"currency", q.Currency, "amount", q.Total)
		if err := b.bot.Accept(q, b.texts.paymentReject); err != nil {
			slog.Error("accept pre checkout reject", "err", err)
			return err
		}
		return nil
	}
	if err := b.bot.Accept(q); err != nil {
		slog.Error("accept pre checkout",
			"event", "payment_pre_checkout_accept_failed", "phase", "payment",
			"err", err, "currency", q.Currency, "amount", q.Total)
		return err
	}
	slog.Info("payment pre_checkout accepted",
		"event", "payment_pre_checkout_accepted", "phase", "payment",
		"currency", q.Currency, "amount", q.Total)
	return nil
}

// onSuccessfulPayment activates the subscription by successful_payment (§5.4, idempotent).
// A write error is logged and reported to the user: the money is charged, activation needs manual
// intervention (Telegram will repeat the update, UNIQUE protects against a duplicate on retry).
func (b *Bot) onSuccessfulPayment(c tele.Context) error {
	sender := c.Sender()
	if sender == nil {
		slog.Error("onSuccessfulPayment: nil sender")
		return nil
	}
	userID := sender.ID
	p := c.Payment()
	// nil-guard on the payment path: dereferencing a nil payment (malformed
	// update / future telebot change) would otherwise panic and lose the update without a trace.
	if p == nil {
		slog.Error("onSuccessfulPayment: nil payment", "user_id", userID)
		return c.Send(b.texts.paymentError)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := b.storage.GetOrCreateUser(ctx, userID); err != nil {
		slog.Error("get or create payment user", "err", err, "user_id", userID)
		return c.Send(b.texts.paymentError)
	}
	// Pass the authenticated payer (c.Sender) - it is authoritative for user_id;
	// billing checks it against the owner from the payload (defense-in-depth against mis-credit).
	if err := b.billing.RecordPayment(ctx, userID, p); err != nil {
		slog.Error("record payment", "err", err, "user_id", userID,
			"telegram_charge_id", p.TelegramChargeID, "provider_charge_id", p.ProviderChargeID)
		return c.Send(b.texts.paymentError)
	}
	slog.Info("payment recorded",
		"event", "payment_successful", "phase", "payment",
		"user_id", userID, "chat_id", userID,
		"telegram_charge_id", p.TelegramChargeID, "provider_charge_id", p.ProviderChargeID,
		"currency", p.Currency, "amount", p.Total)
	return c.Send(b.texts.paymentSuccess)
}

func (b *Bot) waitPaymentEmail(userID int64, autoRenew bool) {
	b.paymentMu.Lock()
	defer b.paymentMu.Unlock()
	if b.awaitPaymentEmail == nil {
		b.awaitPaymentEmail = map[int64]struct{}{}
	}
	if b.awaitPaymentAutoRenew == nil {
		b.awaitPaymentAutoRenew = map[int64]bool{}
	}
	b.awaitPaymentEmail[userID] = struct{}{}
	b.awaitPaymentAutoRenew[userID] = autoRenew
}

func (b *Bot) takePaymentEmailWait(userID int64) (bool, bool) {
	b.paymentMu.Lock()
	defer b.paymentMu.Unlock()
	if _, ok := b.awaitPaymentEmail[userID]; !ok {
		return false, false
	}
	autoRenew := b.awaitPaymentAutoRenew[userID]
	delete(b.awaitPaymentEmail, userID)
	delete(b.awaitPaymentAutoRenew, userID)
	return true, autoRenew
}

func (b *Bot) onConsentAccept(c tele.Context) error {
	user, ok := c.Get("user").(User)
	if !ok || user.TelegramID == 0 {
		slog.Error("onConsentAccept: user missing in context")
		return c.Respond(&tele.CallbackResponse{Text: b.texts.internalError})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.storage.SetConsent(ctx, user.TelegramID); err != nil {
		slog.Error("set consent", "err", err, "user_id", user.TelegramID)
		return c.Respond(&tele.CallbackResponse{Text: b.texts.internalError})
	}
	slog.Info("telegram consent accepted",
		"event", "consent_accepted", "phase", "telegram",
		"user_id", user.TelegramID, "chat_id", user.TelegramID)
	b.removeButtons(c) // remove the buttons so they cannot be pressed again
	// Clear the "clock" best-effort; the main thing is to confirm consent with a message.
	// SF-1: a repeated consent in S2 is not an error, the greeting is shown anyway.
	if err := c.Respond(); err != nil {
		slog.Warn("respond callback", "err", err)
	}
	return c.Send(b.texts.welcome, b.menuMarkup)
}

func (b *Bot) onConsentDecline(c tele.Context) error {
	// A strict guard on user (as in onConsentAccept) is not needed here: there is no DB
	// write, and a zero User gives HasConsent()==false -> the regular decline branch.
	user, _ := c.Get("user").(User)
	if err := c.Respond(); err != nil {
		slog.Warn("respond callback", "err", err)
	}
	// The button was pressed on an old /start message when consent is already given (S2).
	if user.HasConsent() {
		b.removeButtons(c)
		return c.Send(b.texts.welcome, b.menuMarkup)
	}
	err := c.Send(b.texts.consentDeclined)
	b.trackAnalytics(user.TelegramID, analyticsConsentDeclined, "", nil)
	return err
}

func (b *Bot) trackAnalytics(userID int64, eventType, eventKey string, metadata map[string]string) {
	if b.storage == nil || userID == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.storage.TrackAnalyticsEvent(ctx, userID, eventType, eventKey, metadata); err != nil {
		slog.Warn("track analytics event", "event_type", eventType, "event_key", eventKey, "user_id", userID, "err", err)
	}
}

// removeButtons removes the inline keyboard from the callback's message (best-effort).
// The message text stays - for the 152-FZ consent it is important that the user sees
// what they agreed to.
func (b *Bot) removeButtons(c tele.Context) {
	if err := c.Edit(&tele.ReplyMarkup{}); err != nil {
		slog.Warn("remove consent buttons", "err", err)
	}
}

// SendText sends a plain text message to the chat chatID.
// The orchestrator needs it for the notify closure: the bot field is unexported,
// direct access from outside the package is impossible.
func (b *Bot) SendText(chatID int64, text string) error {
	_, err := b.bot.Send(tele.ChatID(chatID), text)
	if err != nil {
		return fmt.Errorf("send text to %d: %w", chatID, err)
	}
	return nil
}

// EditStatus edits a previously sent call status message (by id) in the user's private
// chat. The orchestrator needs it to update a single status during processing
// (accepted -> bot in meeting -> done) instead of sending a stream of new messages.
func (b *Bot) EditStatus(chatID, messageID int64, text string) error {
	_, err := b.bot.Edit(tele.StoredMessage{
		MessageID: strconv.Itoa(int(messageID)),
		ChatID:    chatID,
	}, text)
	if err != nil {
		return fmt.Errorf("edit status message %d in chat %d: %w", messageID, chatID, err)
	}
	return nil
}

// onText: text after consent. A meeting link - create a Job and confirm,
// otherwise hint at what to send.
func (b *Bot) onText(c tele.Context) error {
	user, ok := c.Get("user").(User)
	if !ok || user.TelegramID == 0 {
		slog.Error("onText: user missing in context")
		return c.Send(b.texts.internalError)
	}
	link, res := findMeetingLink(c.Text())
	if waitingEmail, autoRenew := b.takePaymentEmailWait(user.TelegramID); waitingEmail {
		email := strings.TrimSpace(c.Text())
		if !validPaymentEmail(email) {
			if res != linkNone {
				slog.Info("payment email wait cancelled by meeting link",
					"event", "payment_email_wait_cancelled", "phase", "payment",
					"user_id", user.TelegramID, "chat_id", user.TelegramID)
			} else {
				b.waitPaymentEmail(user.TelegramID, autoRenew)
				return c.Send(b.texts.payEmailInvalid)
			}
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := b.storage.SetPaymentEmail(ctx, user.TelegramID, email); err != nil {
				slog.Error("set payment email", "err", err, "user_id", user.TelegramID)
				return c.Send(b.texts.internalError)
			}
			return b.sendPaymentLink(c, user.TelegramID, email, autoRenew)
		}
	}

	switch res {
	case linkZoomWebApp:
		if sender := c.Sender(); sender != nil {
			slog.Info("telegram meeting link rejected",
				"event", "telegram_input_rejected", "phase", "telegram",
				"user_id", sender.ID, "chat_id", sender.ID, "reason", "zoom_web_app")
		}
		return c.Send(b.texts.zoomWebApp)
	case linkNone:
		if sender := c.Sender(); sender != nil {
			slog.Info("telegram text without meeting link",
				"event", "telegram_input_ignored", "phase", "telegram",
				"user_id", sender.ID, "chat_id", sender.ID, "text_len", len(c.Text()))
		}
		return c.Send(b.texts.sendLink)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	job, err := b.storage.CreateJob(ctx, user.TelegramID, link, b.activeProvider, b.trialLimitMinutes)
	if err != nil {
		switch {
		case errors.Is(err, ErrNoQuota):
			slog.Info("create job rejected: no quota",
				"event", "quota_reserve_rejected", "phase", "quota",
				"user_id", user.TelegramID, "chat_id", user.TelegramID,
				"provider", b.activeProvider, "meeting_host", meetingHost(link),
				"platform", meetingPlatform(link), "reason", "no_quota")
			return c.Send(b.texts.noQuota, b.payMarkup)
		case errors.Is(err, ErrDuplicateActiveJob):
			slog.Info("create job rejected: duplicate active job",
				"event", "job_create_rejected", "phase", "telegram",
				"user_id", user.TelegramID, "chat_id", user.TelegramID,
				"provider", b.activeProvider, "meeting_host", meetingHost(link),
				"platform", meetingPlatform(link), "reason", "duplicate_active_job")
			return c.Send(b.texts.duplicateActiveJob)
		case errors.Is(err, ErrTooManyActiveJobs):
			slog.Info("create job rejected: too many active jobs",
				"event", "job_create_rejected", "phase", "telegram",
				"user_id", user.TelegramID, "chat_id", user.TelegramID,
				"provider", b.activeProvider, "meeting_host", meetingHost(link),
				"platform", meetingPlatform(link), "reason", "too_many_active_jobs")
			return c.Send(b.texts.tooManyActiveJobs)
		default:
			slog.Error("create job",
				"event", "job_create_failed", "phase", "telegram",
				"err", err, "user_id", user.TelegramID, "chat_id", user.TelegramID,
				"provider", b.activeProvider, "meeting_host", meetingHost(link),
				"platform", meetingPlatform(link))
			return c.Send(b.texts.internalError)
		}
	}
	slog.Info("job created",
		"event", "job_created", "phase", "telegram",
		"job_id", job.ID, "user_id", user.TelegramID, "chat_id", user.TelegramID,
		"provider", job.Provider, "current_status", job.Status,
		"is_trial", job.IsTrial, "meeting_host", meetingHost(link), "platform", meetingPlatform(link))
	// The status message that the orchestrator then EDITS during processing
	// (accepted -> bot in meeting -> done) - one evolving message instead of
	// a stream of new ones. Sent via b.bot.Send to get its id and store it on the job.
	msg, serr := b.bot.Send(c.Recipient(), acceptedText(link))
	if serr != nil {
		slog.Error("send accepted status", "err", serr, "job_id", job.ID, "user_id", user.TelegramID)
		return nil // job created; processing goes on without the status message
	}
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	if err := b.storage.SetJobStatusMessage(sctx, job.ID, int64(msg.ID)); err != nil {
		slog.Error("store status message id", "err", err, "job_id", job.ID)
	}
	return nil
}

func paymentLinkMetadata(raw json.RawMessage, email string, autoRenew bool) json.RawMessage {
	metadata := map[string]any{}
	if len(raw) > 0 && string(raw) != "null" {
		_ = json.Unmarshal(raw, &metadata)
	}
	if email != "" {
		metadata["payment_email"] = email
	}
	if autoRenew {
		metadata["auto_renew"] = "true"
	}
	out, err := json.Marshal(metadata)
	if err != nil {
		return raw
	}
	return out
}

func validPaymentEmail(email string) bool {
	if len(email) == 0 || len(email) > 254 || strings.ContainsAny(email, " \t\r\n") {
		return false
	}
	local, domain, ok := strings.Cut(email, "@")
	if !ok || local == "" || domain == "" || strings.Contains(domain, "@") {
		return false
	}
	if strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") || !strings.Contains(domain, ".") {
		return false
	}
	return true
}

func meetingHost(link string) string {
	u, err := url.Parse(link)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// meetingPlatform - human-readable platform name by the link host (echo in the confirmation).
func meetingPlatform(link string) string {
	u, err := url.Parse(link)
	if err != nil {
		return "встречу"
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case strings.Contains(host, "zoom"):
		return "Zoom"
	case strings.Contains(host, "meet.google"):
		return "Google Meet"
	case strings.Contains(host, "telemost"):
		return "Телемост"
	default:
		return "встречу"
	}
}

// acceptedText - the first message of the status evolution (step 1): platform echo (the user sees
// that we recognized it correctly) + "sending the bot". Then the orchestrator edits this same message
// by phases: joining -> recording -> processing -> done (see updateStatus).
func acceptedText(link string) string {
	return fmt.Sprintf("Ссылку получил. Отправляю бота на %s.",
		meetingPlatform(link))
}

// Rough URL extractor from text: everything after https?://. The host and scheme
// are validated later via net/url below - it handles tricky cases
// (userinfo, port, case) more precisely, and entrusting that to a regexp is risky. (?i) - RFC 3986
// allows the scheme in any case.
var meetingLinkRe = regexp.MustCompile(`(?i)https?://\S+`)

// Whitelist of meeting hosts. Subdomains are allowed (us05web.zoom.us etc.),
// the check is a suffix match with a dot, so that evilzoom.us does not slip through.
var allowedMeetingHosts = []string{
	"zoom.us",
	"zoomgov.com",
	"meet.google.com",
	"telemost.yandex.ru",
	"telemost.yandex.com",
}

// linkResult - the outcome of parsing a meeting link from the user's text.
type linkResult int

const (
	linkNone       linkResult = iota // no meeting link in the text
	linkValid                        // valid join link, a Job can be created
	linkZoomWebApp                   // Zoom web client (app.zoom.us) - the bot cannot join through it
)

// findMeetingLink: the first meeting link from the text and its classification.
// linkZoomWebApp is separated to give the user a specific hint
// (live test 2026-06-15: app.zoom.us/wc/.../start passed the whitelist, but
// the s2t bot cannot join through it -> job failed; architecture.md §11 #3).
func findMeetingLink(text string) (string, linkResult) {
	raw := meetingLinkRe.FindString(text)
	if raw == "" {
		return "", linkNone
	}
	// \S+ greedily grabs trailing punctuation ("...j/1)") - cut it before parsing,
	// otherwise ')' ends up in Path and breaks validation by path shape.
	candidate := strings.TrimRight(raw, `.,;:!?)]}>"'»`)

	u, err := url.Parse(candidate)
	if err != nil {
		return "", linkNone
	}
	// https only: the http scheme lets downgrade scenarios through, real
	// meeting links from providers are always https.
	if u.Scheme != "https" {
		return "", linkNone
	}
	// Hostname() returns the host without port and userinfo (while Host has them).
	host := strings.ToLower(u.Hostname())
	// Yandex hands out Telemost links via two domains: telemost.yandex.ru and
	// telemost.360.yandex.ru (360 = Yandex sub-brand). Both lead to the same meeting
	// (confirmed by a live test 2026-06-19), but the provider accepts only the
	// base domain, so we normalize on input. We rebuild the link from u rather than by a
	// string replace on the source text: the host is already lowercase, and a link in
	// mixed case (Telemost.360.Yandex.RU) would otherwise not be normalized.
	if base, ok := normalizeTelemostHost(host); ok {
		host = base
		if p := u.Port(); p != "" {
			u.Host = base + ":" + p
		} else {
			u.Host = base
		}
		candidate = u.String()
	}
	if !isAllowedMeetingHost(host) {
		return "", linkNone
	}
	// app.zoom.us - the Zoom PWA/web client, not an invitation host. The bot cannot
	// join through it. Invitation links are always <account>.zoom.us/j/... or zoom.us/j/...
	if host == "app.zoom.us" {
		return "", linkZoomWebApp
	}
	// "" - for https://zoom.us, "/" - for https://zoom.us/. Neither is a valid
	// meeting link.
	if u.Path == "" || u.Path == "/" {
		return "", linkNone
	}
	return candidate, linkValid
}

func isAllowedMeetingHost(host string) bool {
	for _, h := range allowedMeetingHosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

// normalizeTelemostHost reduces telemost.360.yandex.{ru,com} to the base
// telemost.yandex.{ru,com}. Yandex 360 is a sub-brand, both domains lead to the same
// meeting (live test 2026-06-19), but the provider accepts only the base one.
// Takes an already lowercase host; the second result tells whether normalization happened.
func normalizeTelemostHost(host string) (string, bool) {
	const sub = "telemost.360.yandex."
	const base = "telemost.yandex."
	idx := strings.Index(host, sub)
	if idx == -1 {
		return host, false
	}
	return host[:idx] + base + host[idx+len(sub):], true
}
