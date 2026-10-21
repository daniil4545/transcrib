package app

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the application configuration, read from environment variables.
type Config struct {
	DatabaseURL string
	BotToken    string

	// TrialLimitMinutes is how many free minutes we give before a subscription (F6, #7).
	// Default 180 = 3 hours.
	TrialLimitMinutes int
	// SubscriptionMinutesPerPeriod is the minutes in a paid subscription period.
	// Default 900 = 15 hours.
	SubscriptionMinutesPerPeriod int

	// JobMaxAge is the orphan/zombie-recovery threshold: a recording older than this
	// is stopped at the provider and then waits for the transcript; the no-result path returns the reservation.
	// Counted from created_at: JOB_MAX_AGE_HOURS must exceed the maximum meeting length.
	JobMaxAge time.Duration
	// JobStartTimeout is the overall start limit: created/joining older than this
	// fail with a quota refund, so the user does not wait forever for a stuck provider.
	// 0 disables the guard for emergency rollback.
	JobStartTimeout time.Duration
	// JobSubmitMaxAttempts is a best-effort in-process limit on Submit retries.
	// 0 disables the retry limit; the durable upper bound is still set by JobStartTimeout.
	JobSubmitMaxAttempts int

	// TranscriptionProvider selects the provider implementation.
	// Allowed values: "mock" (default), "speech2text", "mymeet".
	// Hot-swap: one line in .env without rebuilding the code.
	TranscriptionProvider string

	// SummarizerProvider selects the summary implementation (F3).
	// Allowed values: "off" (default), "mock", "openrouter".
	// Hot-swap: one line in .env without rebuilding the code.
	SummarizerProvider string

	// OpenRouterAPIKey is the OpenRouter key. Required when SummarizerProvider=openrouter;
	// it does not appear in logs or error messages.
	OpenRouterAPIKey string

	// OpenRouterModel is the summary model slug (api/openrouter/API.md §3).
	// Default google/gemini-2.5-flash; swap via .env without rebuilding.
	OpenRouterModel string

	// OpenRouterBaseURL is the API base URL. Default https://openrouter.ai/api/v1.
	OpenRouterBaseURL string

	// OpenRouterProxyURL is an optional HTTP/SOCKS proxy for OpenRouter only.
	// Do not use the global HTTPS_PROXY: it would also affect Telegram, s2t and YooKassa.
	OpenRouterProxyURL string

	// Speech2TextAPIKey is the speech2text.ru API key. Required when
	// TranscriptionProvider=speech2text; it does not appear in logs or error messages.
	Speech2TextAPIKey string

	// Speech2TextBaseURL is the API base URL. Overridden for tests or
	// when the host changes (api.speech2text.ru vs speech2text.ru discrepancy: API.md §0).
	Speech2TextBaseURL string

	// MymeetAPIKey is the mymeet.ai API key (fallback). Required when
	// TranscriptionProvider=mymeet; it does not appear in logs or error messages.
	MymeetAPIKey string

	// MymeetBaseURL is the mymeet API base URL. Default https://backend.mymeet.ai.
	MymeetBaseURL string

	// MymeetTemplate is template_name for record-meeting. Default default-meeting.
	MymeetTemplate string

	// MymeetTimezone is the timezone for local_date_time. Default Europe/Moscow.
	// Stored as a string: time.LoadLocation is called in main.go next to
	// the provider constructor (*time.Location does not serialize back to a string,
	// and tzdata validity is runtime-dependent).
	MymeetTimezone string

	// ProviderToken is the provider_token from BotFather (YooKassa test/live). Optional:
	// empty = payment facade (the demo without payment does not break), set = real sendInvoice.
	ProviderToken string

	// SubscriptionPriceKopecks is the subscription price in kopecks, gross (VAT included).
	// Default 199000 = 1990 RUB. CRITICAL: kopecks as an integer; the receipt gets rubles as a
	// string ("1990.00"): payments.md §4.4, two currency units in one invoice.
	SubscriptionPriceKopecks int64
	// SubscriptionTitle/SubscriptionDescription are the invoice storefront text.
	SubscriptionTitle       string
	SubscriptionDescription string
	// PaymentReturnURL is where YooKassa returns the user after a redirect payment.
	// Not the source of truth for payment: confirmation is done by polling GET /payments/{id}.
	PaymentReturnURL string
	// PaymentPollInterval is how often YooKassa pending payments are checked.
	PaymentPollInterval time.Duration
	// RecurringPaymentsEnabled is a separate switch for merchant-initiated auto-charges.
	// Default false: a live YooKassa shop requires a manager to enable auto-payments.
	RecurringPaymentsEnabled bool

	// ReceiptVatCode is the vat_code of the 54-FZ receipt item (1..12, API.md §10).
	// Default 1 is a placeholder until the customer confirms the legal form.
	ReceiptVatCode int
	// ReceiptPaymentSubject/ReceiptPaymentMode are required for FFD 1.05+ (API.md §9.2).
	ReceiptPaymentSubject string
	ReceiptPaymentMode    string
	ReceiptMeasure        string
	// ReceiptTaxSystemCode is the seller's tax system. Optional: 0 = we do NOT
	// add the field to the receipt (sending it with a single tax system = a fiscalization error, API.md §9.1/§10.2).
	ReceiptTaxSystemCode int

	// YookassaShopID/YookassaSecretKey are Basic Auth for direct REST refunds.
	// SecretKey does not appear in logs or error messages.
	YookassaShopID    string
	YookassaSecretKey string
	// YookassaBaseURL is the REST API base URL. Default https://api.yookassa.ru/v3.
	YookassaBaseURL string
}

// LoadConfig reads the configuration from the environment and validates required fields.
func LoadConfig() (Config, error) {
	provider := os.Getenv("TRANSCRIPTION_PROVIDER")
	if provider == "" {
		provider = "mock"
	}

	summarizerProvider := os.Getenv("SUMMARIZER_PROVIDER")
	if summarizerProvider == "" {
		// Safe default is off (transcript only): mock delivers a fake
		// summary stub, which must not be given to a user without explicit
		// enabling. For development, set the summary provider explicitly in .env (mock).
		summarizerProvider = "off"
	}

	baseURL := os.Getenv("SPEECH2TEXT_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.speech2text.ru"
	}
	// A trailing slash from env would give "...ru//api/..." in all provider URLs.
	baseURL = strings.TrimSuffix(baseURL, "/")

	mymeetBaseURL := os.Getenv("MYMEET_BASE_URL")
	if mymeetBaseURL == "" {
		mymeetBaseURL = "https://backend.mymeet.ai"
	}
	mymeetBaseURL = strings.TrimSuffix(mymeetBaseURL, "/")

	mymeetTemplate := os.Getenv("MYMEET_TEMPLATE")
	if mymeetTemplate == "" {
		mymeetTemplate = "default-meeting"
	}
	mymeetTimezone := os.Getenv("MYMEET_TIMEZONE")
	if mymeetTimezone == "" {
		mymeetTimezone = "Europe/Moscow"
	}

	openRouterBaseURL := os.Getenv("OPENROUTER_BASE_URL")
	if openRouterBaseURL == "" {
		openRouterBaseURL = "https://openrouter.ai/api/v1"
	}
	openRouterBaseURL = strings.TrimSuffix(openRouterBaseURL, "/")

	openRouterModel := os.Getenv("OPENROUTER_MODEL")
	if openRouterModel == "" {
		openRouterModel = "google/gemini-2.5-flash"
	}
	openRouterProxyURL := strings.TrimSpace(os.Getenv("OPENROUTER_PROXY_URL"))

	yookassaBaseURL := os.Getenv("YOOKASSA_BASE_URL")
	if yookassaBaseURL == "" {
		yookassaBaseURL = "https://api.yookassa.ru/v3"
	}
	yookassaBaseURL = strings.TrimSuffix(yookassaBaseURL, "/")

	trialLimitMinutes, err := minutesFromHoursEnv("TRIAL_HOURS_LIMIT", 3)
	if err != nil {
		return Config{}, err
	}
	subscriptionMinutesPerPeriod, err := minutesFromHoursEnv("SUBSCRIPTION_HOURS_PER_PERIOD", 15)
	if err != nil {
		return Config{}, err
	}
	jobMaxAgeHours, err := atoiDefault("JOB_MAX_AGE_HOURS", 6)
	if err != nil {
		return Config{}, err
	}
	jobStartTimeoutMinutes, err := atoiNonNegativeDefault("JOB_START_TIMEOUT_MINUTES", 10)
	if err != nil {
		return Config{}, err
	}
	jobSubmitMaxAttempts, err := atoiNonNegativeDefault("JOB_SUBMIT_MAX_ATTEMPTS", 3)
	if err != nil {
		return Config{}, err
	}
	paymentPollSeconds, err := atoiDefault("PAYMENT_POLL_INTERVAL_SECONDS", 30)
	if err != nil {
		return Config{}, err
	}
	if paymentPollSeconds <= 0 {
		return Config{}, fmt.Errorf("config: PAYMENT_POLL_INTERVAL_SECONDS must be > 0, got %d", paymentPollSeconds)
	}
	recurringPaymentsEnabled, err := boolEnvDefault("RECURRING_PAYMENTS_ENABLED", false)
	if err != nil {
		return Config{}, err
	}

	priceKopecks, err := atoi64Default("SUBSCRIPTION_PRICE_KOPECKS", 199000)
	if err != nil {
		return Config{}, err
	}
	vatCode, err := atoiDefault("RECEIPT_VAT_CODE", 1)
	if err != nil {
		return Config{}, err
	}
	// tax_system_code allows 0 (= do not send the field), so not via atoiDefault (which validates >0).
	taxSystemCode, err := atoiOptional("RECEIPT_TAX_SYSTEM_CODE")
	if err != nil {
		return Config{}, err
	}

	subscriptionTitle := os.Getenv("SUBSCRIPTION_TITLE")
	if subscriptionTitle == "" {
		subscriptionTitle = "Подписка transcrib"
	}
	subscriptionDescription := os.Getenv("SUBSCRIPTION_DESCRIPTION")
	if subscriptionDescription == "" {
		subscriptionDescription = "15 часов на 1 месяц"
	}
	paymentSubject := strings.TrimSpace(os.Getenv("RECEIPT_PAYMENT_SUBJECT"))
	if paymentSubject == "" {
		paymentSubject = "service"
	}
	paymentMode := strings.TrimSpace(os.Getenv("RECEIPT_PAYMENT_MODE"))
	if paymentMode == "" {
		paymentMode = "full_payment"
	}
	receiptMeasure := strings.TrimSpace(os.Getenv("RECEIPT_MEASURE"))
	if receiptMeasure == "" {
		receiptMeasure = "piece"
	}

	cfg := Config{
		DatabaseURL:                  os.Getenv("DATABASE_URL"),
		BotToken:                     os.Getenv("BOT_TOKEN"),
		TranscriptionProvider:        provider,
		Speech2TextAPIKey:            os.Getenv("SPEECH2TEXT_API_KEY"),
		Speech2TextBaseURL:           baseURL,
		MymeetAPIKey:                 os.Getenv("MYMEET_API_KEY"),
		MymeetBaseURL:                mymeetBaseURL,
		MymeetTemplate:               mymeetTemplate,
		MymeetTimezone:               mymeetTimezone,
		SummarizerProvider:           summarizerProvider,
		OpenRouterAPIKey:             os.Getenv("OPENROUTER_API_KEY"),
		OpenRouterModel:              openRouterModel,
		OpenRouterBaseURL:            openRouterBaseURL,
		OpenRouterProxyURL:           openRouterProxyURL,
		TrialLimitMinutes:            trialLimitMinutes,
		SubscriptionMinutesPerPeriod: subscriptionMinutesPerPeriod,
		JobMaxAge:                    time.Duration(jobMaxAgeHours) * time.Hour,
		JobStartTimeout:              time.Duration(jobStartTimeoutMinutes) * time.Minute,
		JobSubmitMaxAttempts:         jobSubmitMaxAttempts,
		ProviderToken:                os.Getenv("PROVIDER_TOKEN"),
		SubscriptionPriceKopecks:     priceKopecks,
		SubscriptionTitle:            subscriptionTitle,
		SubscriptionDescription:      subscriptionDescription,
		PaymentReturnURL:             strings.TrimSpace(os.Getenv("PAYMENT_RETURN_URL")),
		PaymentPollInterval:          time.Duration(paymentPollSeconds) * time.Second,
		RecurringPaymentsEnabled:     recurringPaymentsEnabled,
		ReceiptVatCode:               vatCode,
		ReceiptPaymentSubject:        paymentSubject,
		ReceiptPaymentMode:           paymentMode,
		ReceiptMeasure:               receiptMeasure,
		ReceiptTaxSystemCode:         taxSystemCode,
		YookassaShopID:               os.Getenv("YOOKASSA_SHOP_ID"),
		YookassaSecretKey:            os.Getenv("YOOKASSA_SECRET_KEY"),
		YookassaBaseURL:              yookassaBaseURL,
	}

	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("config: DATABASE_URL is required")
	}
	if cfg.BotToken == "" {
		return Config{}, fmt.Errorf("config: BOT_TOKEN is required")
	}
	if cfg.SubscriptionPriceKopecks > int64(maxInt()) {
		return Config{}, fmt.Errorf("config: SUBSCRIPTION_PRICE_KOPECKS=%d exceeds max int for Telegram price amount", cfg.SubscriptionPriceKopecks)
	}
	if cfg.ReceiptVatCode < 1 || cfg.ReceiptVatCode > 12 {
		return Config{}, fmt.Errorf("config: RECEIPT_VAT_CODE must be in 1..12, got %d", cfg.ReceiptVatCode)
	}
	if cfg.ReceiptTaxSystemCode < 0 || cfg.ReceiptTaxSystemCode > 6 {
		return Config{}, fmt.Errorf("config: RECEIPT_TAX_SYSTEM_CODE must be in 0..6, got %d", cfg.ReceiptTaxSystemCode)
	}
	if !validReceiptPaymentSubject(cfg.ReceiptPaymentSubject) {
		return Config{}, fmt.Errorf("config: RECEIPT_PAYMENT_SUBJECT=%q is unsupported", cfg.ReceiptPaymentSubject)
	}
	if !validReceiptPaymentMode(cfg.ReceiptPaymentMode) {
		return Config{}, fmt.Errorf("config: RECEIPT_PAYMENT_MODE=%q is unsupported", cfg.ReceiptPaymentMode)
	}
	if !validReceiptMeasure(cfg.ReceiptMeasure) {
		return Config{}, fmt.Errorf("config: RECEIPT_MEASURE=%q is unsupported", cfg.ReceiptMeasure)
	}
	hasPaymentConfig := cfg.YookassaShopID != "" || cfg.YookassaSecretKey != "" || cfg.PaymentReturnURL != ""
	if hasPaymentConfig && (cfg.YookassaShopID == "" || cfg.YookassaSecretKey == "" || cfg.PaymentReturnURL == "") {
		return Config{}, fmt.Errorf("config: YOOKASSA_SHOP_ID, YOOKASSA_SECRET_KEY and PAYMENT_RETURN_URL must be set together for payment links")
	}

	switch cfg.TranscriptionProvider {
	case "mock", "speech2text", "mymeet":
		// allowed values
	default:
		return Config{}, fmt.Errorf("config: TRANSCRIPTION_PROVIDER=%q unknown; allowed: mock, speech2text, mymeet", cfg.TranscriptionProvider)
	}

	if cfg.TranscriptionProvider == "speech2text" && cfg.Speech2TextAPIKey == "" {
		return Config{}, fmt.Errorf("config: SPEECH2TEXT_API_KEY is required when TRANSCRIPTION_PROVIDER=speech2text")
	}

	if cfg.TranscriptionProvider == "mymeet" && cfg.MymeetAPIKey == "" {
		return Config{}, fmt.Errorf("config: MYMEET_API_KEY is required when TRANSCRIPTION_PROVIDER=mymeet")
	}

	switch cfg.SummarizerProvider {
	case "mock", "off":
		// "off": F3 summary is out of scope (the flow ends with transcript delivery).
	case "openrouter":
		if cfg.OpenRouterAPIKey == "" {
			return Config{}, fmt.Errorf("config: OPENROUTER_API_KEY is required when SUMMARIZER_PROVIDER=openrouter")
		}
		if cfg.OpenRouterProxyURL != "" {
			proxyURL, err := url.Parse(cfg.OpenRouterProxyURL)
			if err != nil || proxyURL.Scheme == "" || proxyURL.Host == "" {
				return Config{}, fmt.Errorf("config: OPENROUTER_PROXY_URL is invalid")
			}
			switch proxyURL.Scheme {
			case "http", "https", "socks5":
			default:
				return Config{}, fmt.Errorf("config: OPENROUTER_PROXY_URL scheme %q unsupported; allowed: http, https, socks5", proxyURL.Scheme)
			}
		}
	default:
		return Config{}, fmt.Errorf("config: SUMMARIZER_PROVIDER=%q unknown; allowed: off, mock, openrouter", cfg.SummarizerProvider)
	}

	return cfg, nil
}

func validReceiptPaymentSubject(v string) bool {
	switch strings.TrimSpace(v) {
	case "commodity", "excise", "job", "service", "gambling_bet", "gambling_prize",
		"lottery", "lottery_prize", "intellectual_activity", "payment",
		"agent_commission", "property_right", "non_operating_gain",
		"insurance_premium", "sales_tax", "resort_fee", "composite", "another",
		"fine", "tax", "lien", "cost", "pension_insurance_without_payouts",
		"pension_insurance_with_payouts", "health_insurance_without_payouts",
		"health_insurance_with_payouts", "health_insurance", "casino",
		"agent_withdrawals", "non_marked_excise", "marked_excise", "marked",
		"non_marked":
		return true
	default:
		return false
	}
}

func validReceiptPaymentMode(v string) bool {
	switch strings.TrimSpace(v) {
	case "full_prepayment", "partial_prepayment", "advance", "full_payment", "partial_payment", "credit", "credit_payment":
		return true
	default:
		return false
	}
}

func validReceiptMeasure(v string) bool {
	switch strings.TrimSpace(v) {
	case "piece", "gram", "kilogram", "ton", "centimeter", "decimeter", "meter",
		"square_centimeter", "square_decimeter", "square_meter", "milliliter",
		"liter", "cubic_meter", "kilowatt_hour", "gigacalorie", "day", "hour",
		"minute", "second", "kilobyte", "megabyte", "gigabyte", "terabyte",
		"another":
		return true
	default:
		return false
	}
}

func maxInt() int {
	return int(^uint(0) >> 1)
}

func boolEnvDefault(name string, def bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def, nil
	}
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("config: %s=%q must be boolean", name, raw)
	}
}

func minutesFromHoursEnv(name string, defHours int) (int, error) {
	hours, err := atoiDefault(name, defHours)
	if err != nil {
		return 0, err
	}
	if hours > maxInt()/60 {
		return 0, fmt.Errorf("config: %s=%d overflows minutes", name, hours)
	}
	return hours * 60, nil
}

// atoiDefault reads an int environment variable with a default and validation >0.
// Empty variable: the default; an unparseable or non-positive value is a config
// error (quota limits must not silently reset to 0 and block intake).
func atoiDefault(name string, def int) (int, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("config: %s=%q is not an integer: %w", name, raw, err)
	}
	if v <= 0 {
		return 0, fmt.Errorf("config: %s must be > 0, got %d", name, v)
	}
	return v, nil
}

// atoiNonNegativeDefault reads an int variable where 0 is an explicit guard switch.
func atoiNonNegativeDefault(name string, def int) (int, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("config: %s=%q is not an integer: %w", name, raw, err)
	}
	if v < 0 {
		return 0, fmt.Errorf("config: %s must be >= 0, got %d", name, v)
	}
	return v, nil
}

// atoi64Default is like atoiDefault, but for int64 (kopeck prices exceed the
// guaranteed int range on 32-bit platforms; >0: the money is non-zero).
func atoi64Default(name string, def int64) (int64, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("config: %s=%q is not an integer: %w", name, raw, err)
	}
	if v <= 0 {
		return 0, fmt.Errorf("config: %s must be > 0, got %d", name, v)
	}
	return v, nil
}

// atoiOptional is an int variable where 0 (or empty) is a legitimate "not set".
// For tax_system_code: an empty value must NOT become a second valid code.
func atoiOptional(name string) (int, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return 0, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("config: %s=%q is not an integer: %w", name, raw, err)
	}
	if v < 0 {
		return 0, fmt.Errorf("config: %s must be >= 0, got %d", name, v)
	}
	return v, nil
}
