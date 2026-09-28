package app

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config — конфигурация приложения, читается из переменных окружения.
type Config struct {
	DatabaseURL string
	BotToken    string

	// TrialLimitMinutes — сколько бесплатных минут даём до подписки (F6, #7).
	// Дефолт 180 — 3 часа.
	TrialLimitMinutes int
	// SubscriptionMinutesPerPeriod — минут в оплаченном периоде подписки.
	// Дефолт 900 — 15 часов.
	SubscriptionMinutesPerPeriod int

	// JobMaxAge — порог orphan/zombie-recovery: recording старше этого времени
	// останавливается у provider и дальше ждёт transcript; no-result path возвращает резерв.
	// Отсчёт от created_at: JOB_MAX_AGE_HOURS обязан превышать макс. длину встречи.
	JobMaxAge time.Duration
	// JobStartTimeout — общий лимит старта: created/joining старше этого времени
	// фейлятся с возвратом квоты, чтобы юзер не ждал зависшего provider'а бесконечно.
	// 0 выключает guard для аварийного отката.
	JobStartTimeout time.Duration
	// JobSubmitMaxAttempts — best-effort лимит повторов Submit в памяти процесса.
	// 0 выключает retry-limit; durable верхнюю границу всё равно задаёт JobStartTimeout.
	JobSubmitMaxAttempts int

	// TranscriptionProvider — переключатель реализации поставщика.
	// Допустимые значения: "mock" (по умолчанию), "speech2text", "mymeet".
	// Hot-swap: одна строка в .env без пересборки кода.
	TranscriptionProvider string

	// SummarizerProvider — переключатель реализации саммари (F3).
	// Допустимые значения: "off" (по умолчанию), "mock", "openrouter".
	// Hot-swap: одна строка в .env без пересборки кода.
	SummarizerProvider string

	// OpenRouterAPIKey — ключ OpenRouter. Обязателен при SummarizerProvider=openrouter;
	// в логи и тексты ошибок не попадает.
	OpenRouterAPIKey string

	// OpenRouterModel — slug модели саммари (api/openrouter/API.md §3).
	// Дефолт google/gemini-2.5-flash; свап через .env без пересборки.
	OpenRouterModel string

	// OpenRouterBaseURL — базовый URL API. Дефолт https://openrouter.ai/api/v1.
	OpenRouterBaseURL string

	// OpenRouterProxyURL — опциональный HTTP/SOCKS proxy только для OpenRouter.
	// Не использовать глобальный HTTPS_PROXY: он зацепит Telegram, s2t и ЮKassa.
	OpenRouterProxyURL string

	// Speech2TextAPIKey — API-ключ speech2text.ru. Обязателен при
	// TranscriptionProvider=speech2text; в логи и тексты ошибок не попадает.
	Speech2TextAPIKey string

	// Speech2TextBaseURL — базовый URL API. Переопределяется для тестов или
	// при смене хоста (расхождение api.speech2text.ru vs speech2text.ru — API.md §0).
	Speech2TextBaseURL string

	// MymeetAPIKey — API-ключ mymeet.ai (fallback). Обязателен при
	// TranscriptionProvider=mymeet; в логи и тексты ошибок не попадает.
	MymeetAPIKey string

	// MymeetBaseURL — базовый URL API mymeet. Дефолт https://backend.mymeet.ai.
	MymeetBaseURL string

	// MymeetTemplate — template_name для record-meeting. Дефолт default-meeting.
	MymeetTemplate string

	// MymeetTimezone — таймзона для local_date_time. Дефолт Europe/Moscow.
	// Хранится строкой: time.LoadLocation вызывается в main.go рядом с
	// конструктором провайдера (*time.Location не сериализуется обратно в строку,
	// валидность tzdata рантайм-зависима).
	MymeetTimezone string

	// ProviderToken — provider_token из BotFather (тест/бой ЮKassa). Опционален:
	// пусто = платёжный фасад (демо без оплаты не ломается), задан = реальный sendInvoice.
	ProviderToken string

	// SubscriptionPriceKopecks — цена подписки в копейках, brutto (НДС внутри).
	// Дефолт 199000 = 1990 ₽. КРИТИЧНО: копейки целым; в receipt уходят рублями
	// строкой ("1990.00") — payments.md §4.4, две валютные единицы в одном инвойсе.
	SubscriptionPriceKopecks int64
	// SubscriptionTitle/SubscriptionDescription — витрина инвойса.
	SubscriptionTitle       string
	SubscriptionDescription string
	// PaymentReturnURL — куда ЮKassa вернёт пользователя после redirect-оплаты.
	// Не источник истины оплаты: подтверждение делает polling GET /payments/{id}.
	PaymentReturnURL string
	// PaymentPollInterval — частота проверки pending-платежей ЮKassa.
	PaymentPollInterval time.Duration
	// RecurringPaymentsEnabled — отдельный рубильник merchant-initiated автосписаний.
	// Дефолт false: live-магазин ЮKassa требует включения автоплатежей менеджером.
	RecurringPaymentsEnabled bool

	// ReceiptVatCode — vat_code позиции чека 54-ФЗ (1..12, API.md §10).
	// Дефолт 1 — заглушка до подтверждения юр.формы заказчиком.
	ReceiptVatCode int
	// ReceiptPaymentSubject/ReceiptPaymentMode — обязательны для FFD 1.05+ (API.md §9.2).
	ReceiptPaymentSubject string
	ReceiptPaymentMode    string
	ReceiptMeasure        string
	// ReceiptTaxSystemCode — СНО продавца. Опционален: 0 = поле в receipt НЕ
	// добавляем (слать при одной СНО = ошибка фискализации, API.md §9.1/§10.2).
	ReceiptTaxSystemCode int

	// YookassaShopID/YookassaSecretKey — Basic Auth для прямых REST-refund'ов.
	// SecretKey в логи и тексты ошибок не попадает.
	YookassaShopID    string
	YookassaSecretKey string
	// YookassaBaseURL — базовый URL REST API. Дефолт https://api.yookassa.ru/v3.
	YookassaBaseURL string
}

// LoadConfig читает конфигурацию из окружения и валидирует обязательные поля.
func LoadConfig() (Config, error) {
	provider := os.Getenv("TRANSCRIPTION_PROVIDER")
	if provider == "" {
		provider = "mock"
	}

	summarizerProvider := os.Getenv("SUMMARIZER_PROVIDER")
	if summarizerProvider == "" {
		// Безопасный дефолт — off (только транскрипт): mock доставляет фейковый
		// конспект-заглушку, что недопустимо отдавать пользователю без явного
		// включения. Для разработки саммари провайдер задаётся в .env явно (mock).
		summarizerProvider = "off"
	}

	baseURL := os.Getenv("SPEECH2TEXT_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.speech2text.ru"
	}
	// Хвостовой слэш из env дал бы "...ru//api/..." во всех URL провайдера.
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
	// tax_system_code допускает 0 (= не слать поле), поэтому не через atoiDefault (тот валидирует >0).
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
		// допустимые значения
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
		// "off" — саммари F3 вне скоупа (поток заканчивается доставкой транскрипта).
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

// atoiDefault читает int-переменную окружения с дефолтом и валидацией >0.
// Пустая переменная — дефолт; непарсимое или неположительное значение — ошибка
// конфига (лимиты квот не должны молча сбрасываться в 0 и блокировать приём).
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

// atoiNonNegativeDefault читает int-переменную, где 0 — явный выключатель guard'а.
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

// atoi64Default — как atoiDefault, но для int64 (цены в копейках выходят за
// гарантированный диапазон int на 32-битных платформах; >0 — деньги не нулевые).
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

// atoiOptional — int-переменная, где 0 (или пусто) — легитимное «не задано».
// Для tax_system_code: пустое значение НЕ должно становиться вторым валидным кодом.
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
