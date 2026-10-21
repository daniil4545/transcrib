package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tele "gopkg.in/telebot.v4"
)

type cancelErrorProvider struct {
	MockProvider
}

func (p *cancelErrorProvider) Cancel(_ context.Context, externalJobID string) error {
	p.Cancelled = append(p.Cancelled, externalJobID)
	return errors.New("stop failed")
}

type summaryFetchProvider struct {
	MockProvider
	fetchCalls int
	result     Result
	err        error
}

type blockingSummaryFetchProvider struct {
	MockProvider
	started chan<- struct{}
	release <-chan struct{}
	result  Result
}

func (p *blockingSummaryFetchProvider) Fetch(ctx context.Context, _ string) (Result, error) {
	select {
	case p.started <- struct{}{}:
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	select {
	case <-p.release:
		return p.result, nil
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}

func (p *summaryFetchProvider) Fetch(_ context.Context, _ string) (Result, error) {
	p.fetchCalls++
	if p.err != nil {
		return Result{}, p.err
	}
	return p.result, nil
}

type summaryCountingSummarizer struct {
	calls   int
	summary string
	err     error
}

func (s *summaryCountingSummarizer) Summarize(_ context.Context, _ string) (string, error) {
	s.calls++
	if s.err != nil {
		return "", s.err
	}
	return s.summary, nil
}

func TestFindMeetingLink(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
		res  linkResult
	}{
		{"zoom vanity subdomain", "https://us05web.zoom.us/j/123", "https://us05web.zoom.us/j/123", linkValid},
		{"zoom for government", "https://gdbiw.zoomgov.com/wc/16152630335/join", "https://gdbiw.zoomgov.com/wc/16152630335/join", linkValid},
		{"google meet", "https://meet.google.com/abc-defg-hij", "https://meet.google.com/abc-defg-hij", linkValid},
		{"telemost ru", "https://telemost.yandex.ru/j/42", "https://telemost.yandex.ru/j/42", linkValid},
		{"telemost com", "https://telemost.yandex.com/j/42", "https://telemost.yandex.com/j/42", linkValid},
		// A Yandex 360 domain is normalized to the base one (live test 2026-06-19).
		{"telemost 360 ru normalized", "https://telemost.360.yandex.ru/j/42", "https://telemost.yandex.ru/j/42", linkValid},
		{"telemost 360 com normalized", "https://telemost.360.yandex.com/j/42", "https://telemost.yandex.com/j/42", linkValid},
		{"telemost 360 mixed case", "https://Telemost.360.Yandex.RU/j/42", "https://telemost.yandex.ru/j/42", linkValid},
		{"link inside text", "вот ссылка https://zoom.us/j/1 жду", "https://zoom.us/j/1", linkValid},
		{"glued prefix without space", "афцуhttps://telemost.yandex.ru/j/51064324837942", "https://telemost.yandex.ru/j/51064324837942", linkValid},
		{"leading bracket and trailing punct", "(https://zoom.us/j/1)", "https://zoom.us/j/1", linkValid},
		{"trailing punct in sentence", "Подключайся: https://zoom.us/j/1!", "https://zoom.us/j/1", linkValid},
		{"uppercase host accepted", "HTTPS://Zoom.US/j/1", "HTTPS://Zoom.US/j/1", linkValid},
		{"host with port accepted", "https://zoom.us:443/j/1", "https://zoom.us:443/j/1", linkValid},
		// Zoom web client (app.zoom.us): a separate outcome with a hint (live test 2026-06-15).
		{"zoom web client start", "https://app.zoom.us/wc/86144204796/start?fromPWA=1&pwd=abc", "", linkZoomWebApp},
		{"zoom web client join", "https://app.zoom.us/wc/123/join", "", linkZoomWebApp},
		{"zoom web client uppercase", "https://App.Zoom.US/wc/1/start", "", linkZoomWebApp},
		// Rejected as "no link".
		{"schemeless rejected", "zoom.us/j/1", "", linkNone},
		{"root page rejected", "https://telemost.yandex.ru/", "", linkNone},
		{"bare domain rejected", "https://zoom.us", "", linkNone},
		{"zoom marketing domain", "https://zoom.com/pricing", "", linkNone},
		{"foreign domain", "https://youtube.com/watch", "", linkNone},
		{"lookalike domain", "https://evilzoom.us/j/1", "", linkNone},
		{"http rejected", "http://zoom.us/j/1", "", linkNone},
		{"userinfo trick rejected", "https://zoom.us@attacker.com/j/1", "", linkNone},
		{"plain text", "привет", "", linkNone},
		{"empty", "", "", linkNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, res := findMeetingLink(tc.text)
			if res != tc.res {
				t.Fatalf("findMeetingLink(%q) res = %v, want %v", tc.text, res, tc.res)
			}
			if got != tc.want {
				t.Errorf("findMeetingLink(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}

func TestCallsHistoryLimit(t *testing.T) {
	if callsHistoryLimit != 7*24*time.Hour {
		t.Fatalf("callsHistoryLimit = %s, want 7 days", callsHistoryLimit)
	}
}

func TestMSKTimeAndFilenames(t *testing.T) {
	createdAt := time.Date(2026, 6, 24, 1, 39, 0, 0, time.UTC)

	if got, want := formatCallTime(createdAt), "24.06.2026 04:39"; got != want {
		t.Errorf("formatCallTime = %q, want %q", got, want)
	}
	if got, want := formatCallShortTime(createdAt), "24.06 04:39"; got != want {
		t.Errorf("formatCallShortTime = %q, want %q", got, want)
	}
	if got, want := transcriptFilename(createdAt), "transcript_20260624_0439.md"; got != want {
		t.Errorf("transcriptFilename = %q, want %q", got, want)
	}
	if got, want := summaryFilename(createdAt), "summary_20260624_0439.md"; got != want {
		t.Errorf("summaryFilename = %q, want %q", got, want)
	}
}

func TestValidPaymentEmail(t *testing.T) {
	cases := []struct {
		email string
		want  bool
	}{
		{"user@example.com", true},
		{" user@example.com ", false},
		{"user.name+tag@example.co", true},
		{"", false},
		{"userexample.com", false},
		{"user@", false},
		{"@example.com", false},
		{"user@example", false},
		{"user@example.", false},
		{"user@.example.com", false},
		{"user name@example.com", false},
		{"user@example.com\n", false},
	}
	for _, c := range cases {
		if got := validPaymentEmail(c.email); got != c.want {
			t.Errorf("validPaymentEmail(%q) = %v, want %v", c.email, got, c.want)
		}
	}
}

func TestFormatMinutesBalance(t *testing.T) {
	cases := []struct {
		minutes int
		want    string
	}{
		{0, "0 минут"},
		{1, "1 минута"},
		{2, "2 минуты"},
		{5, "5 минут"},
		{37, "37 минут"},
		{60, "1 час"},
		{61, "1 час 1 минута"},
		{122, "2 часа 2 минуты"},
		{900, "15 часов"},
	}
	for _, tc := range cases {
		if got := formatMinutesBalance(tc.minutes); got != tc.want {
			t.Errorf("formatMinutesBalance(%d) = %q, want %q", tc.minutes, got, tc.want)
		}
	}
}

func TestOnPaySubscription_RequestsEmailWhenMissing(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	b := &Bot{
		storage: s,
		billing: testBilling(Config{
			YookassaShopID:    "shop-1",
			YookassaSecretKey: "secret-1",
			YookassaBaseURL:   "http://yookassa.test",
			PaymentReturnURL:  "https://transcrib.example.com/pay/return",
		}),
		texts:                    defaultTexts(),
		recurringPaymentsEnabled: true,
	}
	c := &fakeTeleContext{sender: &tele.User{ID: userID}}

	if err := b.onPaySubscription(c); err != nil {
		t.Fatalf("onPaySubscription: %v", err)
	}
	if c.responded != 1 {
		t.Fatalf("Respond called %d times, want 1", c.responded)
	}
	if len(c.sent) != 1 || c.sent[0] != b.texts.payEmailRequest {
		t.Fatalf("sent = %#v, want an email request", c.sent)
	}
	waiting, autoRenew := b.takePaymentEmailWait(userID)
	if !waiting {
		t.Fatalf("payment email wait is not set")
	}
	if !autoRenew {
		t.Fatalf("autoRenew=false, want default-on true")
	}
}

func TestOnText_PaymentEmailCreatesPaymentLink(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)

	var gotEmail string
	billing := testBilling(Config{
		SubscriptionPriceKopecks: 199000,
		SubscriptionTitle:        "Подписка",
		SubscriptionDescription:  "15 встреч в месяц",
		ReceiptVatCode:           1,
		ReceiptPaymentSubject:    "service",
		ReceiptPaymentMode:       "full_payment",
		ReceiptMeasure:           "piece",
		YookassaShopID:           "shop-1",
		YookassaSecretKey:        "secret-1",
		YookassaBaseURL:          "http://yookassa.test",
		PaymentReturnURL:         "https://transcrib.example.com/pay/return",
	})
	billing.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body struct {
			SavePaymentMethod string `json:"save_payment_method"`
			Metadata          struct {
				UserID    string `json:"user_id"`
				AutoRenew string `json:"auto_renew"`
			} `json:"metadata"`
			Receipt struct {
				Customer struct {
					Email string `json:"email"`
				} `json:"customer"`
			} `json:"receipt"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode create payment request: %v", err)
		}
		gotEmail = body.Receipt.Customer.Email
		if body.SavePaymentMethod != "true" {
			t.Fatalf("save_payment_method = %q, want true", body.SavePaymentMethod)
		}
		if body.Metadata.UserID == "" || body.Metadata.AutoRenew != "true" {
			t.Fatalf("metadata = %#v, want user_id and auto_renew", body.Metadata)
		}

		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"pay-link-1",
			"status":"pending",
			"paid":false,
			"amount":{"value":"1990.00","currency":"RUB"},
			"confirmation":{"confirmation_url":"https://yookassa.test/confirm/pay-link-1"},
			"metadata":{"user_id":"42"}
		}`))
		return w.Result(), nil
	})}

	b := &Bot{
		storage: s,
		billing: billing,
		texts:   defaultTexts(),
	}
	b.waitPaymentEmail(userID, true)
	c := &fakeTeleContext{
		text:   "user@example.com",
		sender: &tele.User{ID: userID},
		values: map[string]interface{}{"user": User{TelegramID: userID}},
	}

	if err := b.onText(c); err != nil {
		t.Fatalf("onText: %v", err)
	}
	if gotEmail != "user@example.com" {
		t.Fatalf("YooKassa receipt email = %q, want user@example.com", gotEmail)
	}
	email, err := s.PaymentEmail(context.Background(), userID)
	if err != nil {
		t.Fatalf("PaymentEmail: %v", err)
	}
	if email != "user@example.com" {
		t.Fatalf("saved email = %q, want user@example.com", email)
	}

	rows := paymentRows(t, s, userID)
	if len(rows) != 1 {
		t.Fatalf("payments len = %d, want 1", len(rows))
	}
	if rows[0].status != string(PaymentPending) {
		t.Fatalf("payment status = %q, want pending", rows[0].status)
	}
	if len(c.sent) != 1 {
		t.Fatalf("Send called %d times, want 1", len(c.sent))
	}
	if c.sent[0] != b.texts.payLinkReady {
		t.Fatalf("payment text = %#v, want payLinkReady", c.sent[0])
	}
	if len(c.sentOpts) != 1 || len(c.sentOpts[0]) != 1 {
		t.Fatalf("Send opts = %#v, want markup", c.sentOpts)
	}
	markup, ok := c.sentOpts[0][0].(*tele.ReplyMarkup)
	if !ok {
		t.Fatalf("Send opt = %T, want *tele.ReplyMarkup", c.sentOpts[0][0])
	}
	if !hasInlineURL(markup, b.texts.payOpenButton, "https://yookassa.test/confirm/pay-link-1") {
		t.Fatalf("payment button with the YooKassa link not found")
	}
}

func TestBalance_UsesMinutes(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	_, err := s.pool.Exec(ctx,
		`UPDATE users
		 SET trial_minutes_used = 143,
		     trial_meetings_used = 3
		 WHERE telegram_id = $1`,
		userID,
	)
	if err != nil {
		t.Fatalf("set trial minutes: %v", err)
	}
	bal, err := s.Balance(ctx, userID, 180)
	if err != nil {
		t.Fatalf("trial Balance: %v", err)
	}
	if bal.IsSubscription || bal.Remaining != 37 || bal.Limit != 180 {
		t.Fatalf("trial balance = %+v, want 37/180 minutes without a subscription", bal)
	}

	_, err = s.pool.Exec(ctx,
		`INSERT INTO subscriptions (
		     user_id, paid_at, expires_at, meetings_limit, meetings_balance, minutes_limit, minutes_balance
		 )
		 VALUES ($1, now(), now() + interval '30 days', 15, 0, 900, 37)`,
		userID,
	)
	if err != nil {
		t.Fatalf("insert minute subscription: %v", err)
	}
	bal, err = s.Balance(ctx, userID, 180)
	if err != nil {
		t.Fatalf("subscription Balance: %v", err)
	}
	if !bal.IsSubscription || bal.Remaining != 37 || bal.Limit != 900 {
		t.Fatalf("subscription balance = %+v, want 37/900 minutes from minutes_balance", bal)
	}
}

func TestBalance_ActiveSubscriptionWithZeroMinutesStaysSubscription(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)

	_, err := s.pool.Exec(ctx,
		`INSERT INTO subscriptions (
		     user_id, paid_at, expires_at, meetings_limit, meetings_balance, minutes_limit, minutes_balance
		 )
		 VALUES ($1, now(), now() + interval '30 days', 15, 0, 900, 0)`,
		userID,
	)
	if err != nil {
		t.Fatalf("insert zero-minute subscription: %v", err)
	}
	bal, err := s.Balance(ctx, userID, 180)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if !bal.IsSubscription || bal.Remaining != 0 || bal.Limit != 900 || bal.ExpiresAt == nil {
		t.Fatalf("balance = %+v, want an active subscription 0/900", bal)
	}
}

func TestOnBalance_ShowsRecurringStatus(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)

	_, err := s.pool.Exec(ctx,
		`INSERT INTO subscriptions (
		     user_id, paid_at, expires_at, meetings_limit, meetings_balance, minutes_limit, minutes_balance
		 )
		 VALUES ($1, now(), now() + interval '30 days', 15, 15, 900, 900)`,
		userID,
	)
	if err != nil {
		t.Fatalf("insert subscription: %v", err)
	}
	nextCharge := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	insertRecurringProfile(t, s, userID, "pm-ui-balance", "active", nextCharge)

	b := &Bot{storage: s, billing: testBilling(Config{YookassaShopID: "shop-1", YookassaSecretKey: "secret-1", YookassaBaseURL: "http://yookassa.test", PaymentReturnURL: "https://transcrib.example.com/pay/return"}), texts: defaultTexts(), trialLimitMinutes: 180, recurringPaymentsEnabled: true}
	c := &fakeTeleContext{values: map[string]interface{}{"user": User{TelegramID: userID}}}

	if err := b.onBalance(c); err != nil {
		t.Fatalf("onBalance: %v", err)
	}
	if len(c.sent) != 1 {
		t.Fatalf("Send called %d times, want 1", len(c.sent))
	}
	text, ok := c.sent[0].(string)
	if !ok {
		t.Fatalf("Send got %T, want string", c.sent[0])
	}
	if !strings.Contains(text, "Автопродление: включено") || !strings.Contains(text, "01.08.2026") {
		t.Fatalf("balance text = %q, want the auto-renewal status", text)
	}
	if len(c.sentOpts) != 1 || len(c.sentOpts[0]) != 1 {
		t.Fatalf("Send opts = %#v, want the disable button", c.sentOpts)
	}
	markup, ok := c.sentOpts[0][0].(*tele.ReplyMarkup)
	if !ok {
		t.Fatalf("Send opt = %T, want *tele.ReplyMarkup", c.sentOpts[0][0])
	}
	if !hasInlineButton(markup, "Отключить автопродление") {
		t.Fatalf("auto-renewal disable button not found")
	}
}

func TestOnBalance_RecurringDisabledFlagDoesNotPromiseNextCharge(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)

	_, err := s.pool.Exec(ctx,
		`INSERT INTO subscriptions (
		     user_id, paid_at, expires_at, meetings_limit, meetings_balance, minutes_limit, minutes_balance
		 )
		 VALUES ($1, now(), now() + interval '30 days', 15, 0, 900, 0)`,
		userID,
	)
	if err != nil {
		t.Fatalf("insert subscription: %v", err)
	}
	insertRecurringProfile(t, s, userID, "pm-disabled-flag", "active", time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC))

	b := &Bot{
		storage: s,
		billing: testBilling(Config{
			YookassaShopID:    "shop-1",
			YookassaSecretKey: "secret-1",
			YookassaBaseURL:   "http://yookassa.test",
			PaymentReturnURL:  "https://transcrib.example.com/pay/return",
		}),
		texts:                    defaultTexts(),
		trialLimitMinutes:        180,
		recurringPaymentsEnabled: false,
	}
	c := &fakeTeleContext{values: map[string]interface{}{"user": User{TelegramID: userID}}}

	if err := b.onBalance(c); err != nil {
		t.Fatalf("onBalance: %v", err)
	}
	text, ok := c.sent[0].(string)
	if !ok {
		t.Fatalf("Send got %T, want string", c.sent[0])
	}
	if !strings.Contains(text, "Автопродление сейчас выключено.") || strings.Contains(text, "Следующее списание") {
		t.Fatalf("balance text = %q, want a neutral recurring status without the next charge", text)
	}
	markup, ok := c.sentOpts[0][0].(*tele.ReplyMarkup)
	if !ok {
		t.Fatalf("Send opt = %T, want *tele.ReplyMarkup", c.sentOpts[0][0])
	}
	if !hasInlineButton(markup, "Отключить автопродление") || !hasInlineButton(markup, "Обновить подписку") {
		t.Fatalf("markup = %#v, want disable and renew controls", markup.InlineKeyboard)
	}
}

func TestOnBalance_TrialShowsPaymentAndDisableControls(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	b := &Bot{
		storage: s,
		billing: testBilling(Config{
			YookassaShopID:    "shop-1",
			YookassaSecretKey: "secret-1",
			YookassaBaseURL:   "http://yookassa.test",
			PaymentReturnURL:  "https://transcrib.example.com/pay/return",
		}),
		texts:             defaultTexts(),
		trialLimitMinutes: 180,
	}
	c := &fakeTeleContext{values: map[string]interface{}{"user": User{TelegramID: userID}}}

	if err := b.onBalance(c); err != nil {
		t.Fatalf("onBalance: %v", err)
	}
	markup, ok := c.sentOpts[0][0].(*tele.ReplyMarkup)
	if !ok {
		t.Fatalf("Send opt = %T, want *tele.ReplyMarkup", c.sentOpts[0][0])
	}
	if !hasInlineButton(markup, "Отключить автопродление") || !hasInlineButton(markup, "Оформить подписку") || hasInlineButton(markup, "Обновить подписку") {
		t.Fatalf("markup = %#v, want disable and payment controls for trial", markup.InlineKeyboard)
	}
}

func TestOnPayAutoRenew_DisabledFlagDoesNotStartOneTimePayment(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	b := &Bot{
		storage: s,
		billing: testBilling(Config{
			YookassaShopID:    "shop-1",
			YookassaSecretKey: "secret-1",
			YookassaBaseURL:   "http://yookassa.test",
			PaymentReturnURL:  "https://transcrib.example.com/pay/return",
		}),
		texts:                    defaultTexts(),
		recurringPaymentsEnabled: false,
	}
	c := &fakeTeleContext{sender: &tele.User{ID: userID}}

	if err := b.onPayAutoRenew(c); err != nil {
		t.Fatalf("onPayAutoRenew: %v", err)
	}
	if len(c.sent) != 1 || c.sent[0] != b.texts.autoRenewUnavailable {
		t.Fatalf("sent = %#v, want unavailable text", c.sent)
	}
	waiting, _ := b.takePaymentEmailWait(userID)
	if waiting {
		t.Fatalf("legacy pay_auto_renew started the payment flow while recurring is disabled")
	}
}

func TestOnDisableAutoRenew_DisablesProfile(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)
	insertRecurringProfile(t, s, userID, "pm-ui-disable", "active", time.Now().Add(24*time.Hour))

	b := &Bot{storage: s, texts: defaultTexts()}
	c := &fakeTeleContext{values: map[string]interface{}{"user": User{TelegramID: userID}}}

	if err := b.onDisableAutoRenew(c); err != nil {
		t.Fatalf("onDisableAutoRenew: %v", err)
	}
	if len(c.sent) != 1 || c.sent[0] != b.texts.autoRenewDisabled {
		t.Fatalf("sent = %#v, want a disable confirmation", c.sent)
	}
	profile, ok, err := s.RecurringProfile(context.Background(), userID)
	if err != nil {
		t.Fatalf("RecurringProfile: %v", err)
	}
	if !ok {
		t.Fatalf("recurring profile not found")
	}
	if profile.Status != "disabled" || profile.DisabledAt == nil {
		t.Fatalf("profile = %+v, want disabled with disabled_at", profile)
	}
}

func TestRenderCallCard_ShowsSummaryButtonFromCache(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job := summarisingJob(t, s, userID, "ext-card-summary")
	if err := s.SetJobSummary(context.Background(), job.ID, "готовый конспект"); err != nil {
		t.Fatalf("set summary: %v", err)
	}
	if err := s.SetJobDone(context.Background(), job.ID, nil, 3600, 180); err != nil {
		t.Fatalf("set done: %v", err)
	}

	b := &Bot{storage: s, texts: defaultTexts()}
	_, markup, err := b.renderCallCard(context.Background(), userID, job.ID, 0)
	if err != nil {
		t.Fatalf("renderCallCard: %v", err)
	}

	if !hasInlineButton(markup, "📝 Получить конспект") {
		t.Fatalf("summary button not found in the card of a done job with summary_md")
	}
}

func TestRenderCallCard_FailedSummaryShowsSummaryButton(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job := summarisingJob(t, s, userID, "ext-card-failed-summary")
	if err := s.SetJobFailedSummary(context.Background(), job.ID, "summary failed", 180); err != nil {
		t.Fatalf("set failed_summary: %v", err)
	}

	b := &Bot{storage: s, texts: defaultTexts()}
	text, markup, err := b.renderCallCard(context.Background(), userID, job.ID, 0)
	if err != nil {
		t.Fatalf("renderCallCard: %v", err)
	}

	if !strings.Contains(text, "расшифровка готова") {
		t.Fatalf("failed_summary card does not explain the partial success: %q", text)
	}
	if !hasInlineButton(markup, "📄 Получить транскрипт") {
		t.Fatalf("transcript button not found in the failed_summary card")
	}
	if !hasInlineButton(markup, "📝 Получить конспект") {
		t.Fatalf("summary button not found in the failed_summary card")
	}
}

func TestRenderCallCard_DoneWithoutSummaryShowsSummaryButton(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job := summarisingJob(t, s, userID, "ext-card-done-no-summary")
	if err := s.SetJobDone(context.Background(), job.ID, nil, 3600, 180); err != nil {
		t.Fatalf("set done: %v", err)
	}

	b := &Bot{storage: s, texts: defaultTexts()}
	_, markup, err := b.renderCallCard(context.Background(), userID, job.ID, 0)
	if err != nil {
		t.Fatalf("renderCallCard: %v", err)
	}

	if !hasInlineButton(markup, "📝 Получить конспект") {
		t.Fatalf("summary button not found in the card of a done job without summary_md")
	}
}

func TestRenderCallCard_StopActionsByPhase(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	b := &Bot{storage: s, texts: defaultTexts()}

	created, err := s.CreateJob(context.Background(), userID, "https://meet.google.com/card-created", "mock", 180)
	if err != nil {
		t.Fatalf("create created: %v", err)
	}
	_, markup, err := b.renderCallCard(context.Background(), userID, created.ID, 0)
	if err != nil {
		t.Fatalf("render created: %v", err)
	}
	if !hasInlineButton(markup, "✖ Отменить") {
		t.Fatalf("created card must show cancel")
	}

	recording, err := s.CreateJob(context.Background(), userID, "https://meet.google.com/card-recording", "mock", 180)
	if err != nil {
		t.Fatalf("create recording: %v", err)
	}
	if err := s.SetJobSubmitted(context.Background(), recording.ID, "ext-card-recording"); err != nil {
		t.Fatalf("submit recording: %v", err)
	}
	if _, err := s.SetJobRecording(context.Background(), recording.ID); err != nil {
		t.Fatalf("set recording: %v", err)
	}
	_, markup, err = b.renderCallCard(context.Background(), userID, recording.ID, 0)
	if err != nil {
		t.Fatalf("render recording: %v", err)
	}
	if !hasInlineButton(markup, "⏹ Остановить запись") {
		t.Fatalf("recording card must show stop")
	}
	if hasInlineButton(markup, "✖ Отменить") {
		t.Fatalf("recording card must not show the old cancel")
	}

	if _, err := s.SetJobProcessing(context.Background(), recording.ID); err != nil {
		t.Fatalf("set processing: %v", err)
	}
	_, markup, err = b.renderCallCard(context.Background(), userID, recording.ID, 0)
	if err != nil {
		t.Fatalf("render processing: %v", err)
	}
	if hasInlineButton(markup, "⏹ Остановить запись") || hasInlineButton(markup, "✖ Отменить") {
		t.Fatalf("processing card must not show stop/cancel")
	}

	if err := s.SetJobSummarizing(context.Background(), recording.ID, []byte(`{"ok":true}`), 60); err != nil {
		t.Fatalf("set summarizing: %v", err)
	}
	_, markup, err = b.renderCallCard(context.Background(), userID, recording.ID, 0)
	if err != nil {
		t.Fatalf("render summarizing: %v", err)
	}
	if hasInlineButton(markup, "⏹ Остановить запись") || hasInlineButton(markup, "✖ Отменить") {
		t.Fatalf("summarizing card must not show stop/cancel")
	}
}

func TestOnCancelJob_RecordingStopsAndKeepsReserve(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job, err := s.CreateJob(context.Background(), userID, "https://meet.google.com/callback-stop", "mock", 180)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.SetJobSubmitted(context.Background(), job.ID, "ext-callback-stop"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := s.SetJobRecording(context.Background(), job.ID); err != nil {
		t.Fatalf("recording: %v", err)
	}
	usedBefore := trialUsed(t, s, userID)
	provider := &MockProvider{}
	b := &Bot{
		storage:   s,
		providers: map[string]TranscriptionProvider{"mock": provider},
		texts:     defaultTexts(),
	}
	c := &fakeTeleContext{
		data:   jobPayload(job.ID, 0),
		values: map[string]interface{}{"user": User{TelegramID: userID}},
	}

	if err := b.onCancelJob(c); err != nil {
		t.Fatalf("onCancelJob: %v", err)
	}

	if c.responded != 1 {
		t.Fatalf("responded = %d, want 1", c.responded)
	}
	if len(provider.Cancelled) != 1 || provider.Cancelled[0] != "ext-callback-stop" {
		t.Fatalf("provider.Cancelled = %v, want [ext-callback-stop]", provider.Cancelled)
	}
	reloaded := reloadJob(t, s, job.ID)
	if reloaded.Status != JobProcessing {
		t.Fatalf("status = %q, want processing", reloaded.Status)
	}
	if reloaded.LastError == nil || *reloaded.LastError != jobStopManual {
		t.Fatalf("last_error = %v, want %q", reloaded.LastError, jobStopManual)
	}
	if used := trialUsed(t, s, userID); used != usedBefore {
		t.Fatalf("trial_used = %d, want %d (no immediate refund)", used, usedBefore)
	}
}

func TestOnCancelJob_RecordingStopErrorRefunds(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job, err := s.CreateJob(context.Background(), userID, "https://meet.google.com/callback-stop-error", "mock", 180)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.SetJobSubmitted(context.Background(), job.ID, "ext-callback-stop-error"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := s.SetJobRecording(context.Background(), job.ID); err != nil {
		t.Fatalf("recording: %v", err)
	}
	provider := &cancelErrorProvider{}
	b := &Bot{
		storage:   s,
		providers: map[string]TranscriptionProvider{"mock": provider},
		texts:     defaultTexts(),
	}
	c := &fakeTeleContext{
		data:   jobPayload(job.ID, 0),
		values: map[string]interface{}{"user": User{TelegramID: userID}},
	}

	if err := b.onCancelJob(c); err != nil {
		t.Fatalf("onCancelJob: %v", err)
	}

	if len(provider.Cancelled) != 1 || provider.Cancelled[0] != "ext-callback-stop-error" {
		t.Fatalf("provider.Cancelled = %v, want [ext-callback-stop-error]", provider.Cancelled)
	}
	if status, _, _ := jobRow(t, s, job.ID); status != string(JobFailed) {
		t.Fatalf("status = %q, want failed after stop error", status)
	}
	if used := trialUsed(t, s, userID); used != 0 {
		t.Fatalf("trial_used = %d, want refund", used)
	}
}

func TestOnRedeliver_FailedSummarySendsTranscript(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job := summarisingJob(t, s, userID, "ext-redeliver-failed-summary")
	if err := s.SetJobFailedSummary(context.Background(), job.ID, "summary failed", 180); err != nil {
		t.Fatalf("set failed_summary: %v", err)
	}
	job = reloadJob(t, s, job.ID)

	b := &Bot{
		storage:   s,
		providers: map[string]TranscriptionProvider{"mock": &MockProvider{}},
		texts:     defaultTexts(),
	}
	c := &fakeTeleContext{
		data:   jobPayload(job.ID, 0),
		values: map[string]interface{}{"user": User{TelegramID: userID}},
	}

	if err := b.onRedeliver(c); err != nil {
		t.Fatalf("onRedeliver: %v", err)
	}
	if c.responded != 1 {
		t.Fatalf("Respond called %d times, want 1", c.responded)
	}
	if len(c.sent) != 1 {
		t.Fatalf("Send called %d times, want 1", len(c.sent))
	}
	doc, ok := c.sent[0].(*tele.Document)
	if !ok {
		t.Fatalf("Send got %T, want *tele.Document", c.sent[0])
	}
	if doc.FileName != transcriptFilename(job.CreatedAt) {
		t.Errorf("file name %q, want %q", doc.FileName, transcriptFilename(job.CreatedAt))
	}
	body, err := io.ReadAll(doc.FileReader)
	if err != nil {
		t.Fatalf("read sent doc: %v", err)
	}
	if !strings.Contains(string(body), "# Транскрипт встречи") {
		t.Errorf("document does not look like a transcript: %q", string(body))
	}
}

func TestSupportText_ShowsContact(t *testing.T) {
	texts := defaultTexts()
	if !strings.Contains(texts.support, "@dasukhar") || !strings.Contains(texts.support, "support@example.com") {
		t.Fatalf("support text = %q, want support contacts", texts.support)
	}
}

func TestPaymentError_ShowsSupportContact(t *testing.T) {
	texts := defaultTexts()
	if !strings.Contains(texts.paymentError, "@dasukhar") || !strings.Contains(texts.paymentError, "support@example.com") {
		t.Fatalf("paymentError = %q, want support contacts", texts.paymentError)
	}
}

func TestOnRedeliverSummary_UsesCachedSummary(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job := summarisingJob(t, s, userID, "ext-redeliver-summary")
	const summary = "готовый конспект из summary_md"
	if err := s.SetJobSummary(context.Background(), job.ID, summary); err != nil {
		t.Fatalf("set summary: %v", err)
	}
	if err := s.SetJobDone(context.Background(), job.ID, nil, 3600, 180); err != nil {
		t.Fatalf("set done: %v", err)
	}
	job = reloadJob(t, s, job.ID)

	provider := &summaryFetchProvider{result: Result{TranscriptMarkdown: "must not be called"}}
	summarizer := &summaryCountingSummarizer{summary: "must not be called"}
	b := &Bot{
		storage: s,
		providers: map[string]TranscriptionProvider{
			"mock": provider,
		},
		summarizer: summarizer,
		texts:      defaultTexts(),
	}
	c := &fakeTeleContext{
		data:   jobPayload(job.ID, 0),
		values: map[string]interface{}{"user": User{TelegramID: userID}},
	}

	if err := b.onRedeliverSummary(c); err != nil {
		t.Fatalf("onRedeliverSummary: %v", err)
	}
	if c.responded != 1 {
		t.Fatalf("Respond called %d times, want 1", c.responded)
	}
	if len(c.sent) != 1 {
		t.Fatalf("Send called %d times, want 1", len(c.sent))
	}
	doc, ok := c.sent[0].(*tele.Document)
	if !ok {
		t.Fatalf("Send got %T, want *tele.Document", c.sent[0])
	}
	if doc.FileName != summaryFilename(job.CreatedAt) {
		t.Errorf("file name %q, want %q", doc.FileName, summaryFilename(job.CreatedAt))
	}
	if doc.Caption != "📝 Саммари встречи от "+formatCallTime(job.CreatedAt) {
		t.Errorf("caption %q", doc.Caption)
	}
	body, err := io.ReadAll(doc.FileReader)
	if err != nil {
		t.Fatalf("read sent doc: %v", err)
	}
	if string(body) != summary {
		t.Errorf("summary content = %q, want %q", string(body), summary)
	}
	if provider.fetchCalls != 0 {
		t.Fatalf("Fetch called %d times, want 0 for cached summary", provider.fetchCalls)
	}
	if summarizer.calls != 0 {
		t.Fatalf("Summarize called %d times, want 0 for cached summary", summarizer.calls)
	}
}

func TestOnRedeliverSummary_RestoresFailedSummaryWithGeneratedSummary(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job := summarisingJob(t, s, userID, "ext-redeliver-summary-restore")
	if err := s.SetJobFailedSummary(context.Background(), job.ID, "summary failed", 180); err != nil {
		t.Fatalf("set failed_summary: %v", err)
	}
	job = reloadJob(t, s, job.ID)

	provider := &summaryFetchProvider{result: Result{TranscriptMarkdown: "# Транскрипт встречи\n\nТекст"}}
	summarizer := &summaryCountingSummarizer{summary: "восстановленный конспект"}
	b := &Bot{
		storage:    s,
		providers:  map[string]TranscriptionProvider{"mock": provider},
		summarizer: summarizer,
		texts:      defaultTexts(),
	}
	c := &fakeTeleContext{
		data:   jobPayload(job.ID, 0),
		values: map[string]interface{}{"user": User{TelegramID: userID}},
	}

	if err := b.onRedeliverSummary(c); err != nil {
		t.Fatalf("onRedeliverSummary: %v", err)
	}
	if c.responded != 1 {
		t.Fatalf("Respond called %d times, want 1", c.responded)
	}
	if provider.fetchCalls != 1 {
		t.Fatalf("Fetch called %d times, want 1", provider.fetchCalls)
	}
	if summarizer.calls != 1 {
		t.Fatalf("Summarize called %d times, want 1", summarizer.calls)
	}
	assertSummaryDocument(t, c, job, "восстановленный конспект")
	reloaded := reloadJob(t, s, job.ID)
	if reloaded.Status != JobDone {
		t.Fatalf("status = %q, want done", reloaded.Status)
	}
	if reloaded.SummaryMD == nil || *reloaded.SummaryMD != "восстановленный конспект" {
		t.Fatalf("summary_md = %v, want the restored summary", reloaded.SummaryMD)
	}
	if reloaded.LastError != nil {
		t.Fatalf("last_error = %v, want NULL after recovery", *reloaded.LastError)
	}
}

func TestOnRedeliverSummary_UsesProviderSummaryWithoutSummarizer(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job := summarisingJob(t, s, userID, "ext-redeliver-provider-summary")
	if err := s.SetJobFailedSummary(context.Background(), job.ID, "summary failed", 180); err != nil {
		t.Fatalf("set failed_summary: %v", err)
	}
	job = reloadJob(t, s, job.ID)

	provider := &summaryFetchProvider{result: Result{
		TranscriptMarkdown: "# Транскрипт встречи\n\nТекст",
		SummaryMarkdown:    "provider summary",
	}}
	summarizer := &summaryCountingSummarizer{summary: "must not be called"}
	b := &Bot{
		storage:    s,
		providers:  map[string]TranscriptionProvider{"mock": provider},
		summarizer: summarizer,
		texts:      defaultTexts(),
	}
	c := &fakeTeleContext{
		data:   jobPayload(job.ID, 0),
		values: map[string]interface{}{"user": User{TelegramID: userID}},
	}

	if err := b.onRedeliverSummary(c); err != nil {
		t.Fatalf("onRedeliverSummary: %v", err)
	}
	if provider.fetchCalls != 1 {
		t.Fatalf("Fetch called %d times, want 1", provider.fetchCalls)
	}
	if summarizer.calls != 0 {
		t.Fatalf("Summarize called %d times, want 0", summarizer.calls)
	}
	assertSummaryDocument(t, c, job, "provider summary")
	if status, _, _ := jobRow(t, s, job.ID); status != string(JobDone) {
		t.Fatalf("status = %q, want done", status)
	}
}

func TestOnRedeliverSummary_FailureKeepsFailedSummary(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job := summarisingJob(t, s, userID, "ext-redeliver-summary-fail")
	if err := s.SetJobFailedSummary(context.Background(), job.ID, "summary failed", 180); err != nil {
		t.Fatalf("set failed_summary: %v", err)
	}
	job = reloadJob(t, s, job.ID)

	b := &Bot{
		storage:    s,
		providers:  map[string]TranscriptionProvider{"mock": &summaryFetchProvider{result: Result{TranscriptMarkdown: "# Транскрипт встречи\n\nТекст"}}},
		summarizer: &summaryCountingSummarizer{err: errors.New("summarizer failed")},
		texts:      defaultTexts(),
	}
	c := &fakeTeleContext{
		data:   jobPayload(job.ID, 0),
		values: map[string]interface{}{"user": User{TelegramID: userID}},
	}

	if err := b.onRedeliverSummary(c); err != nil {
		t.Fatalf("onRedeliverSummary: %v", err)
	}
	if len(c.sent) != 1 {
		t.Fatalf("Send called %d times, want 1 with an error message", len(c.sent))
	}
	if text, ok := c.sent[0].(string); !ok || text != b.texts.summaryNotAvailable {
		t.Fatalf("sent = %#v, want summaryNotAvailable", c.sent[0])
	}
	reloaded := reloadJob(t, s, job.ID)
	if reloaded.Status != JobFailedSummary {
		t.Fatalf("status = %q, want failed_summary", reloaded.Status)
	}
	if reloaded.SummaryMD != nil {
		t.Fatalf("summary_md = %v, want nil", *reloaded.SummaryMD)
	}
}

func TestRestoreJobSummary_AllowsDifferentJobsInParallel(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	first := summarisingJob(t, s, userID, "ext-summary-first")
	second := summarisingJob(t, s, userID, "ext-summary-second")
	for _, job := range []Job{first, second} {
		if err := s.SetJobFailedSummary(context.Background(), job.ID, "summary failed", 180); err != nil {
			t.Fatalf("set failed_summary for job %d: %v", job.ID, err)
		}
	}

	started := make(chan struct{}, 2)
	release := make(chan struct{})
	provider := &blockingSummaryFetchProvider{
		started: started,
		release: release,
		result:  Result{SummaryMarkdown: "provider summary"},
	}
	b := &Bot{
		storage:   s,
		providers: map[string]TranscriptionProvider{"mock": provider},
		texts:     defaultTexts(),
	}

	results := make(chan string, 2)
	go func() { results <- b.restoreJobSummary(&fakeTeleContext{}, first) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first summary restore did not start")
	}
	go func() { results <- b.restoreJobSummary(&fakeTeleContext{}, second) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("second summary restore was blocked by another job")
	}

	close(release)
	for range 2 {
		select {
		case got := <-results:
			if got != "provider summary" {
				t.Fatalf("restored summary = %q, want provider summary", got)
			}
		case <-time.After(time.Second):
			t.Fatal("summary restore did not finish")
		}
	}
}

func TestRestoreJobSummary_DeduplicatesSameJob(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	job := summarisingJob(t, s, userID, "ext-summary-same-job")
	if err := s.SetJobFailedSummary(context.Background(), job.ID, "summary failed", 180); err != nil {
		t.Fatalf("set failed_summary: %v", err)
	}

	started := make(chan struct{}, 2)
	release := make(chan struct{})
	provider := &blockingSummaryFetchProvider{
		started: started,
		release: release,
		result:  Result{SummaryMarkdown: "provider summary"},
	}
	b := &Bot{
		storage:   s,
		providers: map[string]TranscriptionProvider{"mock": provider},
		texts:     defaultTexts(),
	}

	results := make(chan string, 2)
	go func() { results <- b.restoreJobSummary(&fakeTeleContext{}, job) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first summary restore did not start")
	}
	waiterStarted := make(chan struct{})
	go func() {
		close(waiterStarted)
		results <- b.restoreJobSummary(&fakeTeleContext{}, job)
	}()
	<-waiterStarted
	select {
	case <-started:
		t.Fatal("same job started a second provider fetch")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	for range 2 {
		select {
		case got := <-results:
			if got != "provider summary" {
				t.Fatalf("restored summary = %q, want provider summary", got)
			}
		case <-time.After(time.Second):
			t.Fatal("same-job summary restore did not finish")
		}
	}
	select {
	case <-started:
		t.Fatal("same job performed more than one provider fetch")
	default:
	}
}

func TestOnText_IgnoredTextDoesNotLogContent(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	text := "пароль=secret https://private.example/path?token=hidden"
	b := &Bot{texts: defaultTexts()}
	c := &fakeTeleContext{
		text:   text,
		sender: &tele.User{ID: 123},
		values: map[string]interface{}{"user": User{TelegramID: 123}},
	}
	if err := b.onText(c); err != nil {
		t.Fatalf("onText: %v", err)
	}
	for _, secret := range []string{text, "пароль=secret", "private.example", "token=hidden"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("ignored text leaked into log: %q", logs.String())
		}
	}
	if !strings.Contains(logs.String(), "text_len=") {
		t.Fatalf("safe text length is missing from log: %q", logs.String())
	}
}

func assertSummaryDocument(t *testing.T, c *fakeTeleContext, job Job, want string) {
	t.Helper()
	if len(c.sent) != 1 {
		t.Fatalf("Send called %d times, want 1", len(c.sent))
	}
	doc, ok := c.sent[0].(*tele.Document)
	if !ok {
		t.Fatalf("Send got %T, want *tele.Document", c.sent[0])
	}
	if doc.FileName != summaryFilename(job.CreatedAt) {
		t.Errorf("file name %q, want %q", doc.FileName, summaryFilename(job.CreatedAt))
	}
	body, err := io.ReadAll(doc.FileReader)
	if err != nil {
		t.Fatalf("read sent doc: %v", err)
	}
	if string(body) != want {
		t.Errorf("summary content = %q, want %q", string(body), want)
	}
}

func hasInlineButton(markup *tele.ReplyMarkup, text string) bool {
	if markup == nil {
		return false
	}
	for _, row := range markup.InlineKeyboard {
		for _, btn := range row {
			if btn.Text == text {
				return true
			}
		}
	}
	return false
}

func hasInlineURL(markup *tele.ReplyMarkup, text string, url string) bool {
	if markup == nil {
		return false
	}
	for _, row := range markup.InlineKeyboard {
		for _, btn := range row {
			if btn.Text == text && btn.URL == url {
				return true
			}
		}
	}
	return false
}

type fakeTeleContext struct {
	data      string
	text      string
	sender    *tele.User
	values    map[string]interface{}
	sent      []interface{}
	sentOpts  [][]interface{}
	responded int
}

func (c *fakeTeleContext) Bot() tele.API                                { return nil }
func (c *fakeTeleContext) Update() tele.Update                          { return tele.Update{} }
func (c *fakeTeleContext) Message() *tele.Message                       { return nil }
func (c *fakeTeleContext) Callback() *tele.Callback                     { return nil }
func (c *fakeTeleContext) Query() *tele.Query                           { return nil }
func (c *fakeTeleContext) InlineResult() *tele.InlineResult             { return nil }
func (c *fakeTeleContext) ShippingQuery() *tele.ShippingQuery           { return nil }
func (c *fakeTeleContext) PreCheckoutQuery() *tele.PreCheckoutQuery     { return nil }
func (c *fakeTeleContext) Payment() *tele.Payment                       { return nil }
func (c *fakeTeleContext) Poll() *tele.Poll                             { return nil }
func (c *fakeTeleContext) PollAnswer() *tele.PollAnswer                 { return nil }
func (c *fakeTeleContext) ChatMember() *tele.ChatMemberUpdate           { return nil }
func (c *fakeTeleContext) ChatJoinRequest() *tele.ChatJoinRequest       { return nil }
func (c *fakeTeleContext) Migration() (int64, int64)                    { return 0, 0 }
func (c *fakeTeleContext) Topic() *tele.Topic                           { return nil }
func (c *fakeTeleContext) Boost() *tele.BoostUpdated                    { return nil }
func (c *fakeTeleContext) BoostRemoved() *tele.BoostRemoved             { return nil }
func (c *fakeTeleContext) PurchasedPaidMedia() *tele.PaidMediaPurchased { return nil }
func (c *fakeTeleContext) Sender() *tele.User                           { return c.sender }
func (c *fakeTeleContext) Chat() *tele.Chat                             { return nil }
func (c *fakeTeleContext) Recipient() tele.Recipient                    { return nil }
func (c *fakeTeleContext) Text() string                                 { return c.text }
func (c *fakeTeleContext) ThreadID() int                                { return 0 }
func (c *fakeTeleContext) Entities() tele.Entities                      { return nil }
func (c *fakeTeleContext) Data() string                                 { return c.data }
func (c *fakeTeleContext) Args() []string                               { return nil }
func (c *fakeTeleContext) Send(what interface{}, opts ...interface{}) error {
	c.sent = append(c.sent, what)
	c.sentOpts = append(c.sentOpts, opts)
	return nil
}
func (c *fakeTeleContext) SendAlbum(a tele.Album, opts ...interface{}) error { return nil }
func (c *fakeTeleContext) Reply(what interface{}, opts ...interface{}) error { return nil }
func (c *fakeTeleContext) Forward(msg tele.Editable, opts ...interface{}) error {
	return nil
}
func (c *fakeTeleContext) ForwardTo(to tele.Recipient, opts ...interface{}) error { return nil }
func (c *fakeTeleContext) Edit(what interface{}, opts ...interface{}) error       { return nil }
func (c *fakeTeleContext) EditCaption(caption string, opts ...interface{}) error  { return nil }
func (c *fakeTeleContext) EditOrSend(what interface{}, opts ...interface{}) error { return nil }
func (c *fakeTeleContext) EditOrReply(what interface{}, opts ...interface{}) error {
	return nil
}
func (c *fakeTeleContext) Delete() error                                { return nil }
func (c *fakeTeleContext) DeleteAfter(d time.Duration) *time.Timer      { return nil }
func (c *fakeTeleContext) Notify(action tele.ChatAction) error          { return nil }
func (c *fakeTeleContext) Ship(what ...interface{}) error               { return nil }
func (c *fakeTeleContext) Accept(errorMessage ...string) error          { return nil }
func (c *fakeTeleContext) Answer(resp *tele.QueryResponse) error        { return nil }
func (c *fakeTeleContext) AnswerGuest(result tele.Result) error         { return nil }
func (c *fakeTeleContext) Respond(resp ...*tele.CallbackResponse) error { c.responded++; return nil }
func (c *fakeTeleContext) RespondText(text string) error                { return nil }
func (c *fakeTeleContext) RespondAlert(text string) error               { return nil }
func (c *fakeTeleContext) Get(key string) interface{}                   { return c.values[key] }
func (c *fakeTeleContext) Set(key string, val interface{})              { c.values[key] = val }
