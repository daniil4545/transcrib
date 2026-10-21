package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

// F7 critical money node (db.md §5.4, plan §"Critical nodes under test"):
// payment idempotency (UNIQUE telegram_payment_charge_id), an independent
// expires_at = now()+30 days for every new subscription row (no chain from the user's other
// active periods), linking payments.subscription_id (a money-flow
// invariant). Plus two unit nodes without a DB: in-memory pre_checkout validation (§5.5) and
// the two-currency-unit trap in BuildInvoice (kopecks as an integer / rubles as a string).
// The glue (OnCheckout/OnPayment in tg.go) is not included here: manual E2E.

// --- unit helpers (NO DB) ---

// testBilling builds a Billing with a fake providerToken (needed only for
// BuildInvoice; it does not touch the DB). storage=nil: unit cases do not call it.
func testBilling(cfg Config) *Billing {
	if cfg.ProviderToken == "" {
		cfg.ProviderToken = "TEST:fake-provider-token"
	}
	return NewBilling(cfg, nil)
}

// --- integration helpers (Postgres) ---

// cleanupPayments removes the FK ON DELETE RESTRICT (payments -> users): newTestUser
// deletes only jobs+user, but payments/subscriptions would block DELETE user.
// Registered AFTER newTestUser -> by LIFO t.Cleanup runs BEFORE it.
func cleanupPayments(t *testing.T, s *Storage, userID int64) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		cleanupRecurringProfiles(t, s, userID)
		if _, err := s.pool.Exec(ctx, `DELETE FROM payments WHERE user_id = $1`, userID); err != nil {
			t.Logf("cleanup payments: %v", err)
		}
		if _, err := s.pool.Exec(ctx, `DELETE FROM subscriptions WHERE user_id = $1`, userID); err != nil {
			t.Logf("cleanup subscriptions: %v", err)
		}
	})
}

// paymentRows returns all payments of a user: status and subscription_id (to
// check the linking of the money flow). Ordered by id for determinism.
func paymentRows(t *testing.T, s *Storage, userID int64) []struct {
	status string
	subID  *int64
} {
	t.Helper()
	rows, err := s.pool.Query(context.Background(),
		`SELECT status, subscription_id FROM payments WHERE user_id = $1 ORDER BY id`, userID)
	if err != nil {
		t.Fatalf("query payments: %v", err)
	}
	defer rows.Close()
	var out []struct {
		status string
		subID  *int64
	}
	for rows.Next() {
		var r struct {
			status string
			subID  *int64
		}
		if err := rows.Scan(&r.status, &r.subID); err != nil {
			t.Fatalf("scan payment: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate payments: %v", err)
	}
	return out
}

// subRows returns a user's subscriptions in ascending id order.
func subRows(t *testing.T, s *Storage, userID int64) []struct {
	id              int64
	expires         time.Time
	meetingsLimit   int
	meetingsBalance int
	minutesLimit    int
	minutesBalance  int
} {
	t.Helper()
	rows, err := s.pool.Query(context.Background(),
		`SELECT id, expires_at, meetings_limit, meetings_balance, minutes_limit, minutes_balance
		 FROM subscriptions WHERE user_id = $1 ORDER BY id`, userID)
	if err != nil {
		t.Fatalf("query subscriptions: %v", err)
	}
	defer rows.Close()
	var out []struct {
		id              int64
		expires         time.Time
		meetingsLimit   int
		meetingsBalance int
		minutesLimit    int
		minutesBalance  int
	}
	for rows.Next() {
		var r struct {
			id              int64
			expires         time.Time
			meetingsLimit   int
			meetingsBalance int
			minutesLimit    int
			minutesBalance  int
		}
		if err := rows.Scan(&r.id, &r.expires, &r.meetingsLimit, &r.meetingsBalance, &r.minutesLimit, &r.minutesBalance); err != nil {
			t.Fatalf("scan subscription: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate subscriptions: %v", err)
	}
	return out
}

// =========================== UNIT (no DB) ===========================

// TestValidatePreCheckout: §5.5: pre_checkout validates ONLY the payload format
// in-memory (<=500ms, no DB JOIN). Equivalence classes: a valid "sub:<id>"
// -> true; invalid classes (foreign prefix, garbage, empty, non-numeric id,
// non-positive id) -> false. The negative checks that the system held up and did not
// crash: subscription business logic lives in successful_payment, not here.
func TestValidatePreCheckout(t *testing.T) {
	b := testBilling(Config{})
	cases := []struct {
		name    string
		payload string
		want    bool
	}{
		{"valid", "sub:12345", true},
		{"valid_minimal", "sub:1", true},
		{"wrong_prefix", "order:12345", false},
		{"no_prefix", "12345", false},
		{"empty", "", false},
		{"prefix_only", "sub:", false},
		{"non_numeric", "sub:abc", false},
		{"zero_id", "sub:0", false},
		{"negative_id", "sub:-5", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := b.ValidatePreCheckout(c.payload); got != c.want {
				t.Errorf("ValidatePreCheckout(%q) = %v, expected %v", c.payload, got, c.want)
			}
		})
	}
}

// TestBuildInvoice_TwoCurrencyUnits: THE TWO-CURRENCY-UNIT TRAP (payments.md
// §4.4, API.md §9): one invoice carries the price in TWO forms. Prices[0].Amount is in
// KOPECKS as an integer (199000), while receipt.items[0].amount.value is in RUBLES as a string
// with two decimals ("1990.00"). Mixing them up = a receipt for 19.90 while 1990 is charged.
// We check both forms + the mandatory FFD item fields (vat_code/payment_subject/
// payment_mode). The taxSystemCode boundary (0 vs >0) is a separate subtest.
func TestBuildInvoice_TwoCurrencyUnits(t *testing.T) {
	const priceKopecks = 199000
	b := testBilling(Config{
		SubscriptionPriceKopecks: priceKopecks,
		SubscriptionTitle:        "Подписка",
		SubscriptionDescription:  "15 часов на 1 месяц",
		ReceiptVatCode:           1,
		ReceiptPaymentSubject:    "service",
		ReceiptPaymentMode:       "full_payment",
		ReceiptTaxSystemCode:     0, // tax system not set -> we do NOT add the tax_system_code field
	})

	inv := b.BuildInvoice(42)

	// Form 1: kopecks as an integer in Prices.
	if len(inv.Prices) != 1 {
		t.Fatalf("Prices len = %d, expected 1", len(inv.Prices))
	}
	if inv.Prices[0].Amount != priceKopecks {
		t.Errorf("Prices[0].Amount = %d, expected %d (kopecks as an integer)", inv.Prices[0].Amount, priceKopecks)
	}
	if inv.Currency != "RUB" {
		t.Errorf("Currency = %q, expected RUB", inv.Currency)
	}
	if inv.Payload != "sub:42" {
		t.Errorf("Payload = %q, expected sub:42", inv.Payload)
	}
	if inv.Start != "subscribe" {
		t.Errorf("Start = %q, expected subscribe", inv.Start)
	}

	// Form 2: rubles as a string in provider_data (Data is a JSON string).
	receipt := receiptData(t, inv.Data)
	item := receipt.Items[0]
	amount, ok := item["amount"].(map[string]any)
	if !ok {
		t.Fatalf("receipt.items[0].amount is not an object: %#v", item["amount"])
	}
	if got := amount["value"]; got != "1990.00" {
		t.Errorf("amount.value = %v (%T), expected \"1990.00\" (rubles as a string)", got, got)
	}
	if got := amount["currency"]; got != "RUB" {
		t.Errorf("amount.currency = %v, expected RUB", got)
	}

	// Mandatory FFD item fields (API.md §9.2): without them the receipt will not pass fiscalization.
	for _, k := range []string{"vat_code", "payment_subject", "payment_mode"} {
		if _, present := item[k]; !present {
			t.Errorf("receipt.items[0].%s is missing, expected it to be present", k)
		}
	}
	if got := item["quantity"]; got != "1.00" {
		t.Errorf("receipt.items[0].quantity = %v (%T), expected the string \"1.00\" for Telegram provider_data", got, got)
	}

	// Tax system boundary: with ReceiptTaxSystemCode==0 the tax_system_code field must NOT exist.
	if _, present := receipt.Root["tax_system_code"]; present {
		t.Errorf("receipt.tax_system_code is present with ReceiptTaxSystemCode=0, expected absence (API.md §9.1)")
	}
	if _, present := item["tax_system_code"]; present {
		t.Errorf("tax_system_code is present with ReceiptTaxSystemCode=0, expected absence (API.md §9.1)")
	}
}

// TestBuildInvoice_TaxSystemCodePresent: tax system boundary (API.md §9.1): with
// ReceiptTaxSystemCode>0 the tax_system_code field must be present at the receipt
// level, not inside items[0]. The paired boundary to the zero case above.
func TestBuildInvoice_TaxSystemCodePresent(t *testing.T) {
	b := testBilling(Config{
		SubscriptionPriceKopecks: 199000,
		SubscriptionTitle:        "Подписка",
		SubscriptionDescription:  "15 часов на 1 месяц",
		ReceiptVatCode:           1,
		ReceiptPaymentSubject:    "service",
		ReceiptPaymentMode:       "full_payment",
		ReceiptTaxSystemCode:     2,
	})

	receipt := receiptData(t, b.BuildInvoice(7).Data)
	got, present := receipt.Root["tax_system_code"]
	if !present {
		t.Fatalf("receipt.tax_system_code is missing with ReceiptTaxSystemCode=2, expected it to be present")
	}
	// JSON numbers are parsed as float64.
	if got != float64(2) {
		t.Errorf("receipt.tax_system_code = %v (%T), expected 2", got, got)
	}
	if _, present := receipt.Items[0]["tax_system_code"]; present {
		t.Errorf("receipt.items[0].tax_system_code is present, expected the tax system only at the receipt level")
	}
}

// TestKopecksToRubles_Padding: the "two currency units" boundary at the formatter level
// (payments.md §4.4): kopecks -> rubles as a string via integer divmod %d.%02d.
// TestBuildInvoice_* check only amounts that are multiples of 100 (199000 -> "1990.00") and do not
// catch the zero-pad bug: with %d.%d five kopecks would give "1990.5" (= 1990.50 rubles) instead of
// "1990.05". Remainder equivalence classes in brief: 0 / single-digit kopecks (a
// leading zero is needed) / two-digit / zero amount. db.md §5.4 / API.md §9.
func TestKopecksToRubles_Padding(t *testing.T) {
	cases := []struct {
		name    string
		kopecks int64
		want    string
	}{
		{"divisible_by_100", 199000, "1990.00"},
		{"single_digit_kopecks_needs_pad", 199005, "1990.05"},
		{"double_digit_kopecks", 199099, "1990.99"},
		{"sub_ruble", 5, "0.05"},
		{"zero", 0, "0.00"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := kopecksToRubles(c.kopecks); got != c.want {
				t.Errorf("kopecksToRubles(%d) = %q, expected %q", c.kopecks, got, c.want)
			}
		})
	}
}

type parsedReceipt struct {
	Root  map[string]any
	Items []map[string]any
}

// receiptData parses the provider_data JSON and extracts the whole receipt.
func receiptData(t *testing.T, data string) parsedReceipt {
	t.Helper()
	if data == "" {
		t.Fatal("provider_data (Data) is empty: receipt not assembled")
	}
	var parsed struct {
		Receipt map[string]any `json:"receipt"`
	}
	if err := json.Unmarshal([]byte(data), &parsed); err != nil {
		t.Fatalf("provider_data does not parse: %v (%q)", err, data)
	}
	rawItems, ok := parsed.Receipt["items"].([]any)
	if !ok {
		t.Fatalf("receipt.items is not an array: %#v", parsed.Receipt["items"])
	}
	items := make([]map[string]any, 0, len(rawItems))
	for _, rawItem := range rawItems {
		item, ok := rawItem.(map[string]any)
		if !ok {
			t.Fatalf("receipt.items contains a non-object: %#v", rawItem)
		}
		items = append(items, item)
	}
	if len(items) != 1 {
		t.Fatalf("receipt.items len = %d, expected 1", len(items))
	}
	return parsedReceipt{Root: parsed.Receipt, Items: items}
}

func TestLoadConfig_BillingValidation(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"vat_low", map[string]string{"RECEIPT_VAT_CODE": "0"}},
		{"vat_high", map[string]string{"RECEIPT_VAT_CODE": "13"}},
		{"tax_high", map[string]string{"RECEIPT_TAX_SYSTEM_CODE": "7"}},
		{"payment_subject_invalid", map[string]string{"RECEIPT_PAYMENT_SUBJECT": "bogus"}},
		{"payment_mode_invalid", map[string]string{"RECEIPT_PAYMENT_MODE": "bogus"}},
		{"measure_invalid", map[string]string{"RECEIPT_MEASURE": "service"}},
		{"payment_links_shop_only", map[string]string{"YOOKASSA_SHOP_ID": "shop-1"}},
		{"payment_links_secret_only", map[string]string{"YOOKASSA_SECRET_KEY": "secret-1"}},
		{"payment_links_return_only", map[string]string{"PAYMENT_RETURN_URL": "https://transcrib.example.com/payments/return"}},
	}
	if strconv.IntSize < 64 {
		cases = append(cases, struct {
			name string
			env  map[string]string
		}{
			name: "price_over_int",
			env:  map[string]string{"SUBSCRIPTION_PRICE_KOPECKS": strconv.FormatInt(int64(maxInt())+1, 10)},
		})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setMinimalConfig(t)
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			if _, err := LoadConfig(); err == nil {
				t.Fatal("LoadConfig() err = nil, expected a validation error")
			}
		})
	}
}

func TestLoadConfig_YookassaDefaults(t *testing.T) {
	setMinimalConfig(t)
	t.Setenv("RECEIPT_MEASURE", "")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.YookassaBaseURL != "https://api.yookassa.ru/v3" {
		t.Errorf("YookassaBaseURL = %q, expected default https://api.yookassa.ru/v3", cfg.YookassaBaseURL)
	}
	if cfg.ReceiptMeasure != "piece" {
		t.Errorf("ReceiptMeasure = %q, expected default piece", cfg.ReceiptMeasure)
	}
}

func TestLoadConfig_ReceiptEnumsTrimmed(t *testing.T) {
	setMinimalConfig(t)
	t.Setenv("RECEIPT_PAYMENT_SUBJECT", " service ")
	t.Setenv("RECEIPT_PAYMENT_MODE", " full_payment ")
	t.Setenv("RECEIPT_MEASURE", " piece ")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ReceiptPaymentSubject != "service" {
		t.Errorf("ReceiptPaymentSubject = %q, expected service", cfg.ReceiptPaymentSubject)
	}
	if cfg.ReceiptPaymentMode != "full_payment" {
		t.Errorf("ReceiptPaymentMode = %q, expected full_payment", cfg.ReceiptPaymentMode)
	}
	if cfg.ReceiptMeasure != "piece" {
		t.Errorf("ReceiptMeasure = %q, expected piece", cfg.ReceiptMeasure)
	}
}

func TestLoadConfig_OpenRouterProxyURL(t *testing.T) {
	valid := []string{
		"http://10.0.0.1:20171",
		"https://proxy.example.net:443",
		"socks5://10.0.0.1:20170",
	}
	for _, proxyURL := range valid {
		t.Run("valid_"+proxyURL, func(t *testing.T) {
			setMinimalConfig(t)
			t.Setenv("SUMMARIZER_PROVIDER", "openrouter")
			t.Setenv("OPENROUTER_API_KEY", "test-key")
			t.Setenv("OPENROUTER_PROXY_URL", " "+proxyURL+" ")

			cfg, err := LoadConfig()
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if cfg.OpenRouterProxyURL != proxyURL {
				t.Errorf("OpenRouterProxyURL = %q, want %q", cfg.OpenRouterProxyURL, proxyURL)
			}
		})
	}

	invalid := []string{
		"10.0.0.1:20171",
		"http://",
		"ftp://proxy.example.net:21",
	}
	for _, proxyURL := range invalid {
		t.Run("invalid_"+proxyURL, func(t *testing.T) {
			setMinimalConfig(t)
			t.Setenv("SUMMARIZER_PROVIDER", "openrouter")
			t.Setenv("OPENROUTER_API_KEY", "test-key")
			t.Setenv("OPENROUTER_PROXY_URL", proxyURL)

			if _, err := LoadConfig(); err == nil {
				t.Fatal("LoadConfig() err = nil, want proxy validation error")
			}
		})
	}
}

func TestLoadConfig_HourQuotaDefaults(t *testing.T) {
	setMinimalConfig(t)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.TrialLimitMinutes != 180 {
		t.Errorf("TrialLimitMinutes = %d, expected 180", cfg.TrialLimitMinutes)
	}
	if cfg.SubscriptionMinutesPerPeriod != 900 {
		t.Errorf("SubscriptionMinutesPerPeriod = %d, expected 900", cfg.SubscriptionMinutesPerPeriod)
	}
	if cfg.JobMaxAge != 6*time.Hour {
		t.Errorf("JobMaxAge = %s, expected 6h", cfg.JobMaxAge)
	}
	if cfg.SubscriptionDescription != "15 часов на 1 месяц" {
		t.Errorf("SubscriptionDescription = %q, expected the hours wording", cfg.SubscriptionDescription)
	}
}

func TestLoadConfig_HourQuotaEnv(t *testing.T) {
	setMinimalConfig(t)
	t.Setenv("TRIAL_HOURS_LIMIT", "4")
	t.Setenv("SUBSCRIPTION_HOURS_PER_PERIOD", "12")
	t.Setenv("TRIAL_MEETINGS_LIMIT", "99")
	t.Setenv("MEETINGS_PER_PERIOD", "99")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.TrialLimitMinutes != 240 {
		t.Errorf("TrialLimitMinutes = %d, expected 240", cfg.TrialLimitMinutes)
	}
	if cfg.SubscriptionMinutesPerPeriod != 720 {
		t.Errorf("SubscriptionMinutesPerPeriod = %d, expected 720", cfg.SubscriptionMinutesPerPeriod)
	}
}

func TestLoadConfig_HourQuotaValidation(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"trial_zero", map[string]string{"TRIAL_HOURS_LIMIT": "0"}},
		{"subscription_zero", map[string]string{"SUBSCRIPTION_HOURS_PER_PERIOD": "0"}},
		{"trial_overflow", map[string]string{"TRIAL_HOURS_LIMIT": strconv.Itoa(maxInt()/60 + 1)}},
		{"subscription_overflow", map[string]string{"SUBSCRIPTION_HOURS_PER_PERIOD": strconv.Itoa(maxInt()/60 + 1)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setMinimalConfig(t)
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			if _, err := LoadConfig(); err == nil {
				t.Fatal("LoadConfig() err = nil, expected a validation error")
			}
		})
	}
}

func TestNewBilling_UsesSubscriptionMinutes(t *testing.T) {
	b := testBilling(Config{
		SubscriptionMinutesPerPeriod: 900,
	})
	if b.subscriptionMinutesPerPeriod != 900 {
		t.Errorf("subscriptionMinutesPerPeriod = %d, expected 900", b.subscriptionMinutesPerPeriod)
	}
}

func TestCreateRefund_PostsYookassaRequest(t *testing.T) {
	var gotPath, gotIDKey, gotAmount, gotPaymentID string
	var gotAuth bool
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		user, pass, ok := r.BasicAuth()
		gotAuth = ok && user == "shop-1" && pass == "secret-1"
		gotIDKey = r.Header.Get("Idempotence-Key")

		var body struct {
			PaymentID string `json:"payment_id"`
			Amount    struct {
				Value    string `json:"value"`
				Currency string `json:"currency"`
			} `json:"amount"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode refund request: %v", err)
		}
		gotPaymentID = body.PaymentID
		gotAmount = body.Amount.Value
		if body.Amount.Currency != "RUB" {
			t.Errorf("amount.currency = %q, expected RUB", body.Amount.Currency)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"refund-1","status":"succeeded"}`))
	})

	b := testBilling(Config{
		SubscriptionPriceKopecks: 199000,
		YookassaShopID:           "shop-1",
		YookassaSecretKey:        "secret-1",
		YookassaBaseURL:          "http://yookassa.test",
	})
	b.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Result(), nil
	})}

	res, err := b.CreateRefund(context.Background(), "payment-1", 199000, "idem-1")
	if err != nil {
		t.Fatalf("CreateRefund: %v", err)
	}
	if gotPath != "/refunds" {
		t.Errorf("path = %q, expected /refunds", gotPath)
	}
	if !gotAuth {
		t.Error("BasicAuth did not match")
	}
	if gotIDKey != "idem-1" {
		t.Errorf("Idempotence-Key = %q, expected idem-1", gotIDKey)
	}
	if gotPaymentID != "payment-1" {
		t.Errorf("payment_id = %q, expected payment-1", gotPaymentID)
	}
	if gotAmount != "1990.00" {
		t.Errorf("amount.value = %q, expected 1990.00", gotAmount)
	}
	if res.ProviderRefundID != "refund-1" {
		t.Errorf("ProviderRefundID = %q, expected refund-1", res.ProviderRefundID)
	}
	if res.Status != RefundSucceeded {
		t.Errorf("Status = %q, expected succeeded", res.Status)
	}
	if len(res.Raw) == 0 {
		t.Error("Raw is empty, expected the raw YooKassa response")
	}
}

func TestCreatePayment_PostsYookassaRequest(t *testing.T) {
	var gotPath, gotMethod, gotIDKey string
	var gotAuth bool
	var body struct {
		Amount struct {
			Value    string `json:"value"`
			Currency string `json:"currency"`
		} `json:"amount"`
		Capture      bool `json:"capture"`
		Confirmation struct {
			Type      string `json:"type"`
			ReturnURL string `json:"return_url"`
		} `json:"confirmation"`
		Description string `json:"description"`
		Metadata    struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
		Receipt struct {
			Customer struct {
				Email string `json:"email"`
			} `json:"customer"`
			Items []struct {
				Description string  `json:"description"`
				Quantity    float64 `json:"quantity"`
				Amount      struct {
					Value    string `json:"value"`
					Currency string `json:"currency"`
				} `json:"amount"`
				VatCode        int    `json:"vat_code"`
				PaymentSubject string `json:"payment_subject"`
				PaymentMode    string `json:"payment_mode"`
				Measure        string `json:"measure"`
			} `json:"items"`
		} `json:"receipt"`
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		user, pass, ok := r.BasicAuth()
		gotAuth = ok && user == "shop-1" && pass == "secret-1"
		gotIDKey = r.Header.Get("Idempotence-Key")
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode payment request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"payment-1",
			"status":"pending",
			"paid":false,
			"amount":{"value":"1990.00","currency":"RUB"},
			"confirmation":{"type":"redirect","confirmation_url":"https://yookassa.test/pay/payment-1"},
			"metadata":{"user_id":"42"}
		}`))
	})

	b := testBilling(Config{
		SubscriptionPriceKopecks: 199000,
		SubscriptionDescription:  "15 встреч на 1 месяц",
		ReceiptVatCode:           1,
		ReceiptPaymentSubject:    "service",
		ReceiptPaymentMode:       "full_payment",
		ReceiptMeasure:           "piece",
		PaymentReturnURL:         "https://transcrib.example.com/payments/return",
		YookassaShopID:           "shop-1",
		YookassaSecretKey:        "secret-1",
		YookassaBaseURL:          "http://yookassa.test",
	})
	b.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Result(), nil
	})}

	payment, err := b.CreatePayment(context.Background(), 42, "user@example.com", "idem-1", false)
	if err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, expected POST", gotMethod)
	}
	if gotPath != "/payments" {
		t.Errorf("path = %q, expected /payments", gotPath)
	}
	if !gotAuth {
		t.Error("BasicAuth did not match")
	}
	if gotIDKey != "idem-1" {
		t.Errorf("Idempotence-Key = %q, expected idem-1", gotIDKey)
	}
	if body.Amount.Value != "1990.00" || body.Amount.Currency != "RUB" {
		t.Errorf("amount = %#v, expected 1990.00 RUB", body.Amount)
	}
	if !body.Capture {
		t.Error("capture=false, expected true")
	}
	if body.Confirmation.Type != "redirect" || body.Confirmation.ReturnURL != "https://transcrib.example.com/payments/return" {
		t.Errorf("confirmation = %#v, expected redirect + return_url", body.Confirmation)
	}
	if body.Metadata.UserID != "42" {
		t.Errorf("metadata.user_id = %q, expected 42", body.Metadata.UserID)
	}
	if body.Receipt.Customer.Email != "user@example.com" {
		t.Errorf("receipt.customer.email = %q", body.Receipt.Customer.Email)
	}
	if len(body.Receipt.Items) != 1 {
		t.Fatalf("receipt.items len = %d, expected 1", len(body.Receipt.Items))
	}
	item := body.Receipt.Items[0]
	if item.Amount.Value != "1990.00" || item.Amount.Currency != "RUB" {
		t.Errorf("item.amount = %#v, expected 1990.00 RUB", item.Amount)
	}
	if item.Quantity != 1 {
		t.Errorf("receipt item quantity = %v, expected the number 1", item.Quantity)
	}
	if item.VatCode != 1 || item.PaymentSubject != "service" || item.PaymentMode != "full_payment" || item.Measure != "piece" {
		t.Errorf("receipt item fiscal fields = %#v", item)
	}
	if payment.ID != "payment-1" {
		t.Errorf("payment.ID = %q, expected payment-1", payment.ID)
	}
	if payment.Status != "pending" || payment.Paid {
		t.Errorf("payment status/paid = %q/%v, expected pending/false", payment.Status, payment.Paid)
	}
	if payment.AmountKopecks != 199000 || payment.Currency != "RUB" {
		t.Errorf("payment amount = %d %s, expected 199000 RUB", payment.AmountKopecks, payment.Currency)
	}
	if payment.ConfirmationURL != "https://yookassa.test/pay/payment-1" {
		t.Errorf("ConfirmationURL = %q", payment.ConfirmationURL)
	}
	if len(payment.Raw) == 0 {
		t.Error("Raw is empty, expected the raw YooKassa response")
	}
}

func TestCreatePayment_SavePaymentMethodOptIn(t *testing.T) {
	var body struct {
		SavePaymentMethod string `json:"save_payment_method"`
		Metadata          struct {
			UserID    string `json:"user_id"`
			AutoRenew string `json:"auto_renew"`
		} `json:"metadata"`
	}

	b := testBilling(Config{
		SubscriptionPriceKopecks: 199000,
		SubscriptionDescription:  "15 встреч на 1 месяц",
		ReceiptVatCode:           1,
		ReceiptPaymentSubject:    "service",
		ReceiptPaymentMode:       "full_payment",
		ReceiptMeasure:           "piece",
		PaymentReturnURL:         "https://transcrib.example.com/payments/return",
		YookassaShopID:           "shop-1",
		YookassaSecretKey:        "secret-1",
		YookassaBaseURL:          "http://yookassa.test",
	})
	b.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode payment request: %v", err)
		}
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"payment-recurring-1",
			"status":"pending",
			"paid":false,
			"amount":{"value":"1990.00","currency":"RUB"},
			"confirmation":{"confirmation_url":"https://yookassa.test/pay/payment-recurring-1"},
			"metadata":{"user_id":"42","auto_renew":"true"}
		}`))
		return w.Result(), nil
	})}

	if _, err := b.CreatePayment(context.Background(), 42, "user@example.com", "idem-recurring-1", true); err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}
	if body.SavePaymentMethod != "true" {
		t.Fatalf("save_payment_method = %q, expected the string true for recurring", body.SavePaymentMethod)
	}
	if body.Metadata.UserID != "42" || body.Metadata.AutoRenew != "true" {
		t.Fatalf("metadata = %#v, expected user_id=42 auto_renew=true", body.Metadata)
	}
}

func TestCreateRecurringPayment_PostsYookassaRequest(t *testing.T) {
	var gotPath, gotMethod, gotIDKey string
	var gotAuth bool
	var body struct {
		Amount struct {
			Value    string `json:"value"`
			Currency string `json:"currency"`
		} `json:"amount"`
		Capture         bool   `json:"capture"`
		PaymentMethodID string `json:"payment_method_id"`
		Description     string `json:"description"`
		Metadata        struct {
			UserID    string `json:"user_id"`
			AutoRenew string `json:"auto_renew"`
			Recurring string `json:"recurring"`
		} `json:"metadata"`
	}

	b := testBilling(Config{
		SubscriptionPriceKopecks: 199000,
		SubscriptionDescription:  "15 встреч на 1 месяц",
		YookassaShopID:           "shop-1",
		YookassaSecretKey:        "secret-1",
		YookassaBaseURL:          "http://yookassa.test",
	})
	b.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		user, pass, ok := r.BasicAuth()
		gotAuth = ok && user == "shop-1" && pass == "secret-1"
		gotIDKey = r.Header.Get("Idempotence-Key")
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode recurring request: %v", err)
		}
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"payment-recurring-2",
			"status":"pending",
			"paid":false,
			"amount":{"value":"1990.00","currency":"RUB"},
			"metadata":{"user_id":"42","auto_renew":"true","recurring":"true"}
		}`))
		return w.Result(), nil
	})}

	payment, err := b.CreateRecurringPayment(context.Background(), 42, "pm-1", "recurring-42-1")
	if err != nil {
		t.Fatalf("CreateRecurringPayment: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/payments" {
		t.Fatalf("request = %s %s, expected POST /payments", gotMethod, gotPath)
	}
	if !gotAuth {
		t.Fatal("BasicAuth did not match")
	}
	if gotIDKey != "recurring-42-1" {
		t.Fatalf("Idempotence-Key = %q", gotIDKey)
	}
	if body.Amount.Value != "1990.00" || body.Amount.Currency != "RUB" || !body.Capture {
		t.Fatalf("amount/capture = %#v/%v", body.Amount, body.Capture)
	}
	if body.PaymentMethodID != "pm-1" {
		t.Fatalf("payment_method_id = %q", body.PaymentMethodID)
	}
	if body.Metadata.UserID != "42" || body.Metadata.AutoRenew != "true" || body.Metadata.Recurring != "true" {
		t.Fatalf("metadata = %#v", body.Metadata)
	}
	if payment.ID != "payment-recurring-2" {
		t.Fatalf("payment.ID = %q", payment.ID)
	}
}

func TestGetPayment_ParsesYookassaResponse(t *testing.T) {
	var gotPath, gotMethod, gotIDKey string
	var gotAuth bool
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		user, pass, ok := r.BasicAuth()
		gotAuth = ok && user == "shop-1" && pass == "secret-1"
		gotIDKey = r.Header.Get("Idempotence-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"payment-1",
			"status":"succeeded",
			"paid":true,
			"amount":{"value":"1990.00","currency":"RUB"},
			"metadata":{"user_id":"42"}
		}`))
	})

	b := testBilling(Config{
		YookassaShopID:    "shop-1",
		YookassaSecretKey: "secret-1",
		YookassaBaseURL:   "http://yookassa.test",
	})
	b.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Result(), nil
	})}

	payment, err := b.GetPayment(context.Background(), "payment-1")
	if err != nil {
		t.Fatalf("GetPayment: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, expected GET", gotMethod)
	}
	if gotPath != "/payments/payment-1" {
		t.Errorf("path = %q, expected /payments/payment-1", gotPath)
	}
	if !gotAuth {
		t.Error("BasicAuth did not match")
	}
	if gotIDKey != "" {
		t.Errorf("Idempotence-Key = %q, expected an empty header for GET", gotIDKey)
	}
	if payment.ID != "payment-1" || payment.Status != "succeeded" || !payment.Paid {
		t.Errorf("payment = %#v, expected succeeded paid payment-1", payment)
	}
	if payment.AmountKopecks != 199000 || payment.Currency != "RUB" {
		t.Errorf("payment amount = %d %s, expected 199000 RUB", payment.AmountKopecks, payment.Currency)
	}
	if len(payment.Metadata) == 0 {
		t.Error("Metadata is empty, expected raw metadata")
	}
}

func TestParseYookassaPayment_CancellationReason(t *testing.T) {
	payment, err := parseYookassaPayment([]byte(`{
		"id":"payment-canceled",
		"status":"canceled",
		"paid":false,
		"amount":{"value":"1990.00","currency":"RUB"},
		"cancellation_details":{"reason":"permission_revoked"}
	}`))
	if err != nil {
		t.Fatalf("parseYookassaPayment: %v", err)
	}
	if payment.CancellationReason != "permission_revoked" {
		t.Fatalf("CancellationReason = %q", payment.CancellationReason)
	}
}

func TestParseYookassaPayment_PaymentMethod(t *testing.T) {
	payment, err := parseYookassaPayment([]byte(`{
		"id":"payment-recurring-1",
		"status":"succeeded",
		"paid":true,
		"amount":{"value":"1990.00","currency":"RUB"},
		"payment_method":{"id":"pm-1","saved":true},
		"metadata":{"user_id":"42","auto_renew":"true"}
	}`))
	if err != nil {
		t.Fatalf("parseYookassaPayment: %v", err)
	}

	if payment.PaymentMethodID != "pm-1" {
		t.Fatalf("PaymentMethodID = %q, expected pm-1", payment.PaymentMethodID)
	}
	if !payment.PaymentMethodSaved {
		t.Fatal("PaymentMethodSaved=false, expected true")
	}
}

func TestRublesToKopecks(t *testing.T) {
	cases := []struct {
		value string
		want  int64
	}{
		{"1990.00", 199000},
		{"1990.05", 199005},
		{"0.99", 99},
	}
	for _, c := range cases {
		t.Run(c.value, func(t *testing.T) {
			got, err := rublesToKopecks(c.value)
			if err != nil {
				t.Fatalf("rublesToKopecks: %v", err)
			}
			if got != c.want {
				t.Errorf("rublesToKopecks(%q) = %d, expected %d", c.value, got, c.want)
			}
		})
	}
}

func setMinimalConfig(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/db")
	t.Setenv("BOT_TOKEN", "123:token")
	t.Setenv("TRANSCRIPTION_PROVIDER", "mock")
	t.Setenv("SUMMARIZER_PROVIDER", "off")
	t.Setenv("SPEECH2TEXT_API_KEY", "")
	t.Setenv("MYMEET_API_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("OPENROUTER_PROXY_URL", "")
	t.Setenv("RECEIPT_VAT_CODE", "1")
	t.Setenv("RECEIPT_TAX_SYSTEM_CODE", "")
	t.Setenv("RECEIPT_PAYMENT_SUBJECT", "service")
	t.Setenv("RECEIPT_PAYMENT_MODE", "full_payment")
	t.Setenv("RECEIPT_MEASURE", "piece")
	t.Setenv("SUBSCRIPTION_PRICE_KOPECKS", "199000")
	t.Setenv("PAYMENT_RETURN_URL", "")
	t.Setenv("YOOKASSA_SHOP_ID", "")
	t.Setenv("YOOKASSA_SECRET_KEY", "")
	t.Setenv("YOOKASSA_BASE_URL", "")
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// =========================== INTEGRATION (Postgres §5.4) ===========================

// TestRecordSuccessfulPayment_Idempotent: idempotency §5.4: a repeat of the same
// telegram_payment_charge_id (a successful_payment retry from Telegram) does NOT double
// either the payment or the subscription (ON CONFLICT DO NOTHING on UNIQUE charge_id). Money-flow
// invariants: exactly 1 payment (status succeeded), exactly 1 subscription,
// payments.subscription_id is NOT NULL (refund §5.9 always finds the subscription).
// db.md §5.4 / docs/testing.md.
func TestRecordSuccessfulPayment_Idempotent(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)

	in := PaymentInput{
		UserID:           userID,
		AmountKopecks:    199000,
		Currency:         "RUB",
		TelegramChargeID: "tg-1",
		ProviderChargeID: "yk-1",
		InvoicePayload:   "sub:1",
		RawPayload:       json.RawMessage(`{"k":"v"}`),
	}

	if err := s.RecordSuccessfulPayment(ctx, in, 900); err != nil {
		t.Fatalf("RecordSuccessfulPayment #1: %v", err)
	}
	// Repeat with the same charge_id: must be a no-op.
	if err := s.RecordSuccessfulPayment(ctx, in, 900); err != nil {
		t.Fatalf("RecordSuccessfulPayment #2 (repeat): %v", err)
	}

	pays := paymentRows(t, s, userID)
	if len(pays) != 1 {
		t.Fatalf("payments count = %d, expected 1 (idempotency by charge_id)", len(pays))
	}
	if pays[0].status != "succeeded" {
		t.Errorf("payment.status = %q, expected succeeded", pays[0].status)
	}
	if pays[0].subID == nil {
		t.Error("payments.subscription_id = NULL, expected a link (money-flow invariant)")
	}

	subs := subRows(t, s, userID)
	if len(subs) != 1 {
		t.Fatalf("subscriptions count = %d, expected 1 (a repeat does not spawn a subscription)", len(subs))
	}
	if subs[0].meetingsLimit != 15 || subs[0].meetingsBalance != 15 {
		t.Errorf("legacy meetings = %d/%d, expected 15/15", subs[0].meetingsBalance, subs[0].meetingsLimit)
	}
	if subs[0].minutesLimit != 900 || subs[0].minutesBalance != 900 {
		t.Errorf("minutes = %d/%d, expected 900/900", subs[0].minutesBalance, subs[0].minutesLimit)
	}
	if pays[0].subID != nil && *pays[0].subID != subs[0].id {
		t.Errorf("payment.subscription_id = %d, expected %d (link to its own subscription)", *pays[0].subID, subs[0].id)
	}
}

// TestRecordSuccessfulPayment_NoActive: a payment without an active subscription: the first
// period gives expires_at ~ now()+30 days.
func TestRecordSuccessfulPayment_NoActive(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)

	before := time.Now()
	in := PaymentInput{
		UserID:           userID,
		AmountKopecks:    199000,
		Currency:         "RUB",
		TelegramChargeID: "tg-noactive",
		InvoicePayload:   "sub:1",
	}
	if err := s.RecordSuccessfulPayment(ctx, in, 900); err != nil {
		t.Fatalf("RecordSuccessfulPayment: %v", err)
	}

	subs := subRows(t, s, userID)
	if len(subs) != 1 {
		t.Fatalf("subscriptions count = %d, expected 1", len(subs))
	}
	wantLo := before.Add(30 * 24 * time.Hour).Add(-1 * time.Hour)
	wantHi := time.Now().Add(30 * 24 * time.Hour).Add(1 * time.Hour)
	if subs[0].expires.Before(wantLo) || subs[0].expires.After(wantHi) {
		t.Errorf("expires_at = %v, expected ~ now()+30 days (in window %v..%v)", subs[0].expires, wantLo, wantHi)
	}
}

// TestRecordSuccessfulPayment_IndependentOfActive: with an already active subscription
// (expiring in ~30 days) a new payment creates a SECOND row with expires_at ~
// now()+30 days, while the old row stays untouched (its own term, its own balance).
// Periods are independent, with no chain-extension from the old expires_at.
func TestRecordSuccessfulPayment_IndependentOfActive(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)

	// Active subscription: insertSubscription sets expires_at = now()+30 days.
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
		TelegramChargeID: "tg-independent-active",
		InvoicePayload:   "sub:1",
	}
	if err := s.RecordSuccessfulPayment(ctx, in, 900); err != nil {
		t.Fatalf("RecordSuccessfulPayment: %v", err)
	}

	subs = subRows(t, s, userID)
	if len(subs) != 2 {
		t.Fatalf("subscriptions count = %d, expected 2 (repeat payment = new row)", len(subs))
	}
	// The new row is the one that is not oldSubID.
	newSub := subs[0]
	var oldSubStillThere bool
	for _, sub := range subs {
		if sub.id == oldSubID {
			oldSubStillThere = true
			if !sub.expires.Equal(oldExpires) {
				t.Errorf("old subscription expires_at = %v, expected unchanged %v", sub.expires, oldExpires)
			}
		} else {
			newSub = sub
		}
	}
	if !oldSubStillThere {
		t.Fatal("old subscription row disappeared")
	}
	if newSub.id == oldSubID {
		t.Fatal("new subscription row not found")
	}

	// The new period is independent of the old one: expires ~ now()+30 days, NOT old+30 days.
	wantLo := before.Add(30 * 24 * time.Hour).Add(-1 * time.Hour)
	wantHi := time.Now().Add(30 * 24 * time.Hour).Add(1 * time.Hour)
	if newSub.expires.Before(wantLo) || newSub.expires.After(wantHi) {
		t.Errorf("new expires_at = %v, expected ~ now()+30 days (window %v..%v), without a chain from the old subscription",
			newSub.expires, wantLo, wantHi)
	}

	// The payment is linked specifically to the new subscription.
	pays := paymentRows(t, s, userID)
	if len(pays) != 1 {
		t.Fatalf("payments count = %d, expected 1", len(pays))
	}
	if pays[0].subID == nil || *pays[0].subID != newSub.id {
		t.Errorf("payment.subscription_id = %v, expected %d (new row)", pays[0].subID, newSub.id)
	}
}

// TestRecordSuccessfulPayment_ConcurrentSameUser: several simultaneous
// successful payments of one user create independent subscription rows: each
// with its own expires_at ~ now()+30 days, without races/lost updates (previously this
// was a chain-serialization check; now new periods do not depend on each other,
// the invariant is the absence of errors and correct linking of every payment).
func TestRecordSuccessfulPayment_ConcurrentSameUser(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	cleanupPayments(t, s, userID)

	const payments = 4
	before := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, payments)
	for i := 0; i < payments; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			in := PaymentInput{
				UserID:           userID,
				AmountKopecks:    199000,
				Currency:         "RUB",
				TelegramChargeID: "tg-concurrent-" + strconv.Itoa(i),
				ProviderChargeID: "yk-concurrent-" + strconv.Itoa(i),
				InvoicePayload:   "sub:1",
			}
			errs <- s.RecordSuccessfulPayment(ctx, in, 900)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("RecordSuccessfulPayment concurrent: %v", err)
		}
	}

	subs := subRows(t, s, userID)
	if len(subs) != payments {
		t.Fatalf("subscriptions count = %d, expected %d", len(subs), payments)
	}
	wantLo := before.Add(30 * 24 * time.Hour).Add(-1 * time.Hour)
	wantHi := time.Now().Add(30 * 24 * time.Hour).Add(1 * time.Hour)
	for i, sub := range subs {
		if sub.expires.Before(wantLo) || sub.expires.After(wantHi) {
			t.Errorf("subscriptions[%d].expires = %v, expected ~ now()+30 days (window %v..%v), independent of the others",
				i, sub.expires, wantLo, wantHi)
		}
	}

	pays := paymentRows(t, s, userID)
	if len(pays) != payments {
		t.Fatalf("payments count = %d, expected %d", len(pays), payments)
	}
	for i, pay := range pays {
		if pay.subID == nil {
			t.Fatalf("payment %d subscription_id = NULL", i)
		}
	}
}
