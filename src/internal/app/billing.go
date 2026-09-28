package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	tele "gopkg.in/telebot.v4"
)

// payloadPrefix — префикс invoice_payload, связывающий pre_checkout с successful_payment.
// Формат "sub:<userID>": кодирует владельца платежа, валидируется in-memory без БД (§5.5).
const payloadPrefix = "sub:"

// Billing — платёжная обвязка F7 core: сборка инвойса (ловушка двух валютных
// единиц), in-memory валидация pre_checkout, маппинг successful_payment в storage.
// Не интерфейс: ЮKassa одна, второй реализации нет (принцип 4). Тонкий — весь SQL
// в storage.go (payments.md, plan §«Согласованные решения»).
type Billing struct {
	storage                      *Storage
	providerToken                string
	priceKopecks                 int64
	subscriptionMinutesPerPeriod int
	vatCode                      int
	taxSystemCode                int // 0 = поле tax_system_code в receipt НЕ добавляем (API.md §9.1)
	paymentSubject               string
	paymentMode                  string
	receiptMeasure               string
	title                        string
	description                  string
	paymentReturnURL             string
	yookassaShopID               string
	yookassaSecretKey            string
	yookassaBaseURL              string
	httpClient                   *http.Client
}

type YookassaPayment struct {
	ID                 string
	Status             string
	Paid               bool
	AmountKopecks      int64
	Currency           string
	ConfirmationURL    string
	PaymentMethodID    string
	PaymentMethodSaved bool
	CancellationReason string
	ProviderPaidAt     *time.Time
	Metadata           json.RawMessage
	Raw                json.RawMessage
}

type RefundResult struct {
	ProviderRefundID string
	Status           RefundStatus
	Raw              []byte
}

func NewBilling(cfg Config, s *Storage) *Billing {
	return &Billing{
		storage:                      s,
		providerToken:                cfg.ProviderToken,
		priceKopecks:                 cfg.SubscriptionPriceKopecks,
		subscriptionMinutesPerPeriod: cfg.SubscriptionMinutesPerPeriod,
		vatCode:                      cfg.ReceiptVatCode,
		taxSystemCode:                cfg.ReceiptTaxSystemCode,
		paymentSubject:               cfg.ReceiptPaymentSubject,
		paymentMode:                  cfg.ReceiptPaymentMode,
		receiptMeasure:               cfg.ReceiptMeasure,
		title:                        cfg.SubscriptionTitle,
		description:                  cfg.SubscriptionDescription,
		paymentReturnURL:             cfg.PaymentReturnURL,
		yookassaShopID:               cfg.YookassaShopID,
		yookassaSecretKey:            cfg.YookassaSecretKey,
		yookassaBaseURL:              cfg.YookassaBaseURL,
		httpClient:                   &http.Client{Timeout: 15 * time.Second},
	}
}

// Enabled сообщает, включён ли реальный приём оплаты по прямой ссылке ЮKassa.
// Telegram providerToken остаётся только для legacy sendInvoice-тестов.
func (b *Billing) Enabled() bool {
	return b.CanCreatePayment()
}

func (b *Billing) CanCreatePayment() bool {
	return b.yookassaShopID != "" && b.yookassaSecretKey != "" && b.yookassaBaseURL != "" && b.paymentReturnURL != ""
}

func (b *Billing) CanPollPayments() bool {
	return b.yookassaShopID != "" && b.yookassaSecretKey != "" && b.yookassaBaseURL != ""
}

// BuildInvoice собирает sendInvoice-инвойс с чеком 54-ФЗ.
//
// КРИТИЧНАЯ ЛОВУШКА ДВУХ ВАЛЮТНЫХ ЕДИНИЦ (payments.md §4.4, API.md §9): в одном
// инвойсе цена идёт в ДВУХ формах — Prices[].Amount в КОПЕЙКАХ целым (199000), а
// в receipt.items[].amount.value РУБЛЯМИ строкой с двумя знаками ("1990.00").
// Перепутать = чек на 19.90 при оплате 1990. Конвертация — целочисленная
// (kopecksToRubles), без float, чтобы не потерять точность на копейках.
//
// customer/email в provider_data НЕ кладём: NeedEmail+SendEmail заставляют Telegram
// собрать email в форме оплаты и самому проставить customer в чек (зашить пустой = ошибка).
func (b *Billing) BuildInvoice(userID int64) *tele.Invoice {
	return &tele.Invoice{
		Token:       b.providerToken,
		Title:       b.title,
		Description: b.description,
		Payload:     payloadPrefix + strconv.FormatInt(userID, 10),
		Currency:    "RUB",
		Start:       "subscribe",
		// Amount тип int (telebot.v4): явный каст из int64-копеек.
		Prices:    []tele.Price{{Label: b.title, Amount: int(b.priceKopecks)}},
		Data:      b.providerData(),
		NeedEmail: true,
		SendEmail: true,
	}
}

// providerData сериализует receipt 54-ФЗ в JSON-строку для Telegram (поле provider_data).
// value — рубли строкой (см. ловушку в BuildInvoice). tax_system_code добавляем
// только при taxSystemCode>0: при одной СНО поле = ошибка фискализации (API.md §9.1/§10.2).
func (b *Billing) providerData() string {
	item := map[string]any{
		"description":     b.description,
		"quantity":        "1.00",
		"amount":          map[string]string{"value": kopecksToRubles(b.priceKopecks), "currency": "RUB"},
		"vat_code":        b.vatCode,
		"payment_subject": b.paymentSubject,
		"payment_mode":    b.paymentMode,
	}
	receipt := map[string]any{"items": []any{item}}
	if b.taxSystemCode > 0 {
		receipt["tax_system_code"] = b.taxSystemCode
	}
	data := map[string]any{"receipt": receipt}
	// json.Marshal на map[string]any с заданными типами ключей не падает; ошибку
	// игнорировать нельзя по стандартам — но здесь источник данных контролируемый.
	raw, err := json.Marshal(data)
	if err != nil {
		// Недостижимо при текущем составе data (нет каналов/функций); логировать
		// нечем (нет ctx), вернуть пустую строку безопаснее паники — Telegram
		// отклонит инвойс без чека, ошибка станет видна в E2E.
		return ""
	}
	return string(raw)
}

func (b *Billing) CreatePayment(ctx context.Context, userID int64, email string, idemKey string, saveMethod bool) (YookassaPayment, error) {
	if err := b.validatePaymentRequest(email, idemKey); err != nil {
		return YookassaPayment{}, fmt.Errorf("create payment: %w", err)
	}

	body := map[string]any{
		"amount": map[string]string{
			"value":    kopecksToRubles(b.priceKopecks),
			"currency": "RUB",
		},
		"capture": true,
		"confirmation": map[string]string{
			"type":       "redirect",
			"return_url": b.paymentReturnURL,
		},
		"description": b.description,
		"receipt":     b.directReceipt(email),
	}
	metadata := map[string]string{"user_id": strconv.FormatInt(userID, 10)}
	if saveMethod {
		body["save_payment_method"] = "true"
		metadata["auto_renew"] = "true"
	}
	body["metadata"] = metadata
	rawBody, err := json.Marshal(body)
	if err != nil {
		return YookassaPayment{}, fmt.Errorf("create payment marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.yookassaBaseURL+"/payments", bytes.NewReader(rawBody))
	if err != nil {
		return YookassaPayment{}, fmt.Errorf("create payment build request: %w", err)
	}
	req.SetBasicAuth(b.yookassaShopID, b.yookassaSecretKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Idempotence-Key", idemKey)

	raw, err := b.doYookassa(req, "create payment")
	if err != nil {
		return YookassaPayment{}, err
	}
	payment, err := parseYookassaPayment(raw)
	if err != nil {
		return YookassaPayment{}, fmt.Errorf("create payment parse response: %w", err)
	}
	if payment.ID == "" {
		return YookassaPayment{}, fmt.Errorf("create payment: empty payment id")
	}
	if payment.ConfirmationURL == "" {
		return YookassaPayment{}, fmt.Errorf("create payment: empty confirmation url")
	}
	return payment, nil
}

func (b *Billing) CreateRecurringPayment(ctx context.Context, userID int64, paymentMethodID string, idemKey string) (YookassaPayment, error) {
	if err := b.validateRecurringPaymentRequest(paymentMethodID, idemKey); err != nil {
		return YookassaPayment{}, fmt.Errorf("create recurring payment: %w", err)
	}

	body := map[string]any{
		"amount": map[string]string{
			"value":    kopecksToRubles(b.priceKopecks),
			"currency": "RUB",
		},
		"capture":           true,
		"payment_method_id": paymentMethodID,
		"description":       b.description,
		"metadata": map[string]string{
			"user_id":    strconv.FormatInt(userID, 10),
			"auto_renew": "true",
			"recurring":  "true",
		},
	}
	if b.storage != nil {
		email, err := b.storage.PaymentEmail(ctx, userID)
		if err != nil {
			return YookassaPayment{}, fmt.Errorf("create recurring payment load email: %w", err)
		}
		if strings.TrimSpace(email) == "" {
			return YookassaPayment{}, fmt.Errorf("payment email is required")
		}
		body["receipt"] = b.directReceipt(email)
	}
	rawBody, err := json.Marshal(body)
	if err != nil {
		return YookassaPayment{}, fmt.Errorf("create recurring payment marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.yookassaBaseURL+"/payments", bytes.NewReader(rawBody))
	if err != nil {
		return YookassaPayment{}, fmt.Errorf("create recurring payment build request: %w", err)
	}
	req.SetBasicAuth(b.yookassaShopID, b.yookassaSecretKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Idempotence-Key", idemKey)

	raw, err := b.doYookassa(req, "create recurring payment")
	if err != nil {
		return YookassaPayment{}, err
	}
	payment, err := parseYookassaPayment(raw)
	if err != nil {
		return YookassaPayment{}, fmt.Errorf("create recurring payment parse response: %w", err)
	}
	if payment.ID == "" {
		return YookassaPayment{}, fmt.Errorf("create recurring payment: empty payment id")
	}
	return payment, nil
}

func (b *Billing) GetPayment(ctx context.Context, paymentID string) (YookassaPayment, error) {
	if err := b.validateYookassaConfig("get payment"); err != nil {
		return YookassaPayment{}, err
	}
	if paymentID == "" {
		return YookassaPayment{}, fmt.Errorf("get payment: payment id is required")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.yookassaBaseURL+"/payments/"+paymentID, nil)
	if err != nil {
		return YookassaPayment{}, fmt.Errorf("get payment build request: %w", err)
	}
	req.SetBasicAuth(b.yookassaShopID, b.yookassaSecretKey)
	req.Header.Set("Accept", "application/json")

	raw, err := b.doYookassa(req, "get payment")
	if err != nil {
		return YookassaPayment{}, err
	}
	payment, err := parseYookassaPayment(raw)
	if err != nil {
		return YookassaPayment{}, fmt.Errorf("get payment parse response: %w", err)
	}
	if payment.ID == "" {
		return YookassaPayment{}, fmt.Errorf("get payment: empty payment id")
	}
	return payment, nil
}

func (b *Billing) validateRecurringPaymentRequest(paymentMethodID string, idemKey string) error {
	if err := b.validateYookassaConfig("create recurring payment"); err != nil {
		return err
	}
	if b.priceKopecks <= 0 {
		return fmt.Errorf("amount must be > 0, got %d", b.priceKopecks)
	}
	if strings.TrimSpace(paymentMethodID) == "" {
		return fmt.Errorf("payment method id is required")
	}
	if idemKey == "" {
		return fmt.Errorf("idempotence key is required")
	}
	return nil
}

func (b *Billing) validatePaymentRequest(email string, idemKey string) error {
	if err := b.validateYookassaConfig("create payment"); err != nil {
		return err
	}
	if b.paymentReturnURL == "" {
		return fmt.Errorf("PAYMENT_RETURN_URL is required")
	}
	if b.priceKopecks <= 0 {
		return fmt.Errorf("amount must be > 0, got %d", b.priceKopecks)
	}
	if strings.TrimSpace(email) == "" {
		return fmt.Errorf("email is required")
	}
	if idemKey == "" {
		return fmt.Errorf("idempotence key is required")
	}
	return nil
}

func (b *Billing) validateYookassaConfig(operation string) error {
	if b.yookassaShopID == "" {
		return fmt.Errorf("%s: YOOKASSA_SHOP_ID is required", operation)
	}
	if b.yookassaSecretKey == "" {
		return fmt.Errorf("%s: YOOKASSA_SECRET_KEY is required", operation)
	}
	if b.yookassaBaseURL == "" {
		return fmt.Errorf("%s: YOOKASSA_BASE_URL is required", operation)
	}
	return nil
}

func (b *Billing) directReceipt(email string) map[string]any {
	item := map[string]any{
		"description":     b.description,
		"quantity":        1,
		"amount":          map[string]string{"value": kopecksToRubles(b.priceKopecks), "currency": "RUB"},
		"vat_code":        b.vatCode,
		"payment_subject": b.paymentSubject,
		"payment_mode":    b.paymentMode,
		"measure":         b.receiptMeasure,
	}
	receipt := map[string]any{
		"customer": map[string]string{"email": email},
		"items":    []any{item},
	}
	if b.taxSystemCode > 0 {
		receipt["tax_system_code"] = b.taxSystemCode
	}
	return receipt
}

// CreateRefund делает только низкоуровневый REST-вызов ЮKassa. Storage-фазы
// BeginRefund/FinalizeRefund остаются снаружи, чтобы ретраи и admin-flow решались
// на уровне сценария, а не HTTP-хелпера.
func (b *Billing) CreateRefund(ctx context.Context, providerChargeID string, amountKopecks int64, idemKey string) (RefundResult, error) {
	if err := b.validateYookassaConfig("create refund"); err != nil {
		return RefundResult{}, err
	}
	if providerChargeID == "" {
		return RefundResult{}, fmt.Errorf("create refund: provider charge id is required")
	}
	if amountKopecks <= 0 {
		return RefundResult{}, fmt.Errorf("create refund: amount must be > 0, got %d", amountKopecks)
	}
	if idemKey == "" {
		return RefundResult{}, fmt.Errorf("create refund: idempotence key is required")
	}

	body := map[string]any{
		"payment_id": providerChargeID,
		"amount": map[string]string{
			"value":    kopecksToRubles(amountKopecks),
			"currency": "RUB",
		},
	}
	slog.Info("refund create started",
		"event", "refund_create_started", "phase", "refund",
		"provider_charge_id", providerChargeID, "amount", amountKopecks)
	rawBody, err := json.Marshal(body)
	if err != nil {
		return RefundResult{}, fmt.Errorf("create refund marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.yookassaBaseURL+"/refunds", bytes.NewReader(rawBody))
	if err != nil {
		return RefundResult{}, fmt.Errorf("create refund build request: %w", err)
	}
	req.SetBasicAuth(b.yookassaShopID, b.yookassaSecretKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Idempotence-Key", idemKey)

	raw, err := b.doYookassa(req, "create refund")
	if err != nil {
		slog.Error("refund create provider rejected",
			"event", "refund_create_failed", "phase", "refund",
			"provider_charge_id", providerChargeID, "amount", amountKopecks)
		return RefundResult{}, err
	}

	var parsed struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return RefundResult{}, fmt.Errorf("create refund parse response: %w", err)
	}
	status, err := parseYookassaRefundStatus(parsed.Status)
	if err != nil {
		return RefundResult{}, fmt.Errorf("create refund parse status: %w", err)
	}
	slog.Info("refund create completed",
		"event", "refund_create_completed", "phase", "refund",
		"provider_charge_id", providerChargeID, "provider_refund_id", parsed.ID,
		"refund_status", status, "amount", amountKopecks)
	return RefundResult{ProviderRefundID: parsed.ID, Status: status, Raw: raw}, nil
}

func (b *Billing) doYookassa(req *http.Request, operation string) ([]byte, error) {
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s request: %w", operation, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%s read response: %w", operation, err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("%s: yookassa status %d: %s", operation, resp.StatusCode, string(raw))
	}
	return raw, nil
}

func parseYookassaPayment(raw []byte) (YookassaPayment, error) {
	var parsed struct {
		ID           string         `json:"id"`
		Status       string         `json:"status"`
		Paid         bool           `json:"paid"`
		Amount       yookassaAmount `json:"amount"`
		CapturedAt   *time.Time     `json:"captured_at"`
		Confirmation struct {
			ConfirmationURL string `json:"confirmation_url"`
		} `json:"confirmation"`
		PaymentMethod struct {
			ID    string `json:"id"`
			Saved bool   `json:"saved"`
		} `json:"payment_method"`
		CancellationDetails struct {
			Reason string `json:"reason"`
		} `json:"cancellation_details"`
		Metadata json.RawMessage `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return YookassaPayment{}, err
	}
	amount, err := rublesToKopecks(parsed.Amount.Value)
	if err != nil {
		return YookassaPayment{}, fmt.Errorf("amount.value: %w", err)
	}
	return YookassaPayment{
		ID:                 parsed.ID,
		Status:             parsed.Status,
		Paid:               parsed.Paid,
		AmountKopecks:      amount,
		Currency:           parsed.Amount.Currency,
		ConfirmationURL:    parsed.Confirmation.ConfirmationURL,
		PaymentMethodID:    parsed.PaymentMethod.ID,
		PaymentMethodSaved: parsed.PaymentMethod.Saved,
		CancellationReason: parsed.CancellationDetails.Reason,
		ProviderPaidAt:     parsed.CapturedAt,
		Metadata:           parsed.Metadata,
		Raw:                json.RawMessage(raw),
	}, nil
}

type yookassaAmount struct {
	Value    string `json:"value"`
	Currency string `json:"currency"`
}

func parseYookassaRefundStatus(status string) (RefundStatus, error) {
	switch RefundStatus(status) {
	case RefundPending, RefundSucceeded, RefundCanceled:
		return RefundStatus(status), nil
	default:
		return "", fmt.Errorf("unknown refund status %q", status)
	}
}

// kopecksToRubles форматирует копейки в рубли-строку "<руб>.<коп>" (две цифры копеек).
// Целочисленно (НЕ float): float64 округлил бы крупные суммы и сломал чек.
func kopecksToRubles(kopecks int64) string {
	return fmt.Sprintf("%d.%02d", kopecks/100, kopecks%100)
}

func rublesToKopecks(value string) (int64, error) {
	rubles, kopecks, ok := strings.Cut(value, ".")
	if !ok {
		return 0, fmt.Errorf("missing decimal part")
	}
	if len(kopecks) != 2 {
		return 0, fmt.Errorf("decimal part must have 2 digits")
	}
	rub, err := strconv.ParseInt(rubles, 10, 64)
	if err != nil {
		return 0, err
	}
	kop, err := strconv.ParseInt(kopecks, 10, 64)
	if err != nil {
		return 0, err
	}
	return rub*100 + kop, nil
}

// ValidatePreCheckout — ТОЛЬКО in-memory проверка формата payload (§5.5, ≤500мс):
// префикс "sub:" + парсится userID. Без БД — бизнес-логика подписки в successful_payment.
func (b *Billing) ValidatePreCheckout(payload string) bool {
	if !strings.HasPrefix(payload, payloadPrefix) {
		return false
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(payload, payloadPrefix), 10, 64)
	return err == nil && id > 0
}

// RecordPayment маппит успешный tele.Payment в PaymentInput и пишет платёж+подписку
// (§5.4, идемпотентно по telegram_payment_charge_id). raw_payload — сырой платёж для
// аудита/fallback-lookup; metadata и provider_paid_at на MVP nil (telebot их не даёт).
//
// user_id берётся из payerID (аутентифицированный c.Sender) — он авторитетен. Payload
// формируется нами на сервере (BuildInvoice) и эхо-возвращается Telegram дословно,
// клиент его не подделает; но если payer и владелец payload разошлись (битый инвойс /
// будущая правка) — это аномалия, платёж не зачисляем вслепую (defense-in-depth).
func (b *Billing) RecordPayment(ctx context.Context, payerID int64, p *tele.Payment) error {
	payloadID, err := userIDFromPayload(p.Payload)
	if err != nil {
		return fmt.Errorf("record payment: %w", err)
	}
	if payloadID != payerID {
		return fmt.Errorf("record payment: payer %d != payload owner %d", payerID, payloadID)
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("record payment marshal raw: %w", err)
	}
	in := PaymentInput{
		UserID:           payerID,
		AmountKopecks:    int64(p.Total),
		Currency:         p.Currency,
		TelegramChargeID: p.TelegramChargeID,
		ProviderChargeID: p.ProviderChargeID,
		InvoicePayload:   p.Payload,
		RawPayload:       raw,
	}
	return b.storage.RecordSuccessfulPayment(ctx, in, b.subscriptionMinutesPerPeriod)
}

// userIDFromPayload извлекает владельца платежа из invoice_payload (формат "sub:<userID>").
func userIDFromPayload(payload string) (int64, error) {
	if !strings.HasPrefix(payload, payloadPrefix) {
		return 0, fmt.Errorf("invalid payment payload: missing prefix")
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(payload, payloadPrefix), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid payment payload: %w", err)
	}
	return id, nil
}
