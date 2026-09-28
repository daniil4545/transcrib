package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/daniil4545/transcrib/internal/app"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	level, err := parseLogLevel(os.Getenv("LOG_LEVEL"))
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	if err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}

	if err := runCommand(os.Args[1:]); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func runCommand(args []string) error {
	if len(args) == 0 {
		return run()
	}
	if len(args) != 2 || args[0] != "healthcheck" {
		return fmt.Errorf("usage: transcrib [healthcheck <url>]")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return checkHealth(ctx, args[1])
}

func parseLogLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("config: LOG_LEVEL=%q unknown; allowed: debug, info, warn, error", raw)
	}
}

func run() error {
	cfg, err := app.LoadConfig()
	if err != nil {
		return err
	}

	// Контекст живёт до первого SIGINT/SIGTERM, по сигналу отменяется.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect postgres pool: %w", err)
	}
	defer pool.Close()

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		return fmt.Errorf("ping postgres: %w", err)
	}

	storage := app.NewStorage(pool)
	billing := app.NewBilling(cfg, storage)

	// Строим реализации ВСЕХ сконфигуренных поставщиков (по наличию ключа), не только
	// активного: external_job_id принадлежит пространству поставщика-создателя, поэтому
	// повторная доставка и отмена в панели «Мои звонки» резолвят нужного по job.provider,
	// а не по активному TRANSCRIPTION_PROVIDER (иначе после смены провайдера готовая
	// джоба недоставляема — её id чужой текущему поставщику).
	providers := map[string]app.TranscriptionProvider{
		// DoneAfterPolls: 1 — один тик Pending, потом Done; прогресс виден в логах.
		"mock": &app.MockProvider{DoneAfterPolls: 1},
	}
	if cfg.Speech2TextAPIKey != "" {
		providers["speech2text"] = app.NewSpeech2Text(cfg.Speech2TextAPIKey, cfg.Speech2TextBaseURL)
	}
	if cfg.MymeetAPIKey != "" {
		// LoadLocation здесь, не в LoadConfig: tzdata рантайм-зависима, ловим
		// рядом с конструктором. Битый MYMEET_TIMEZONE = информативный отказ старта.
		loc, err := time.LoadLocation(cfg.MymeetTimezone)
		if err != nil {
			return fmt.Errorf("load MYMEET_TIMEZONE %q: %w", cfg.MymeetTimezone, err)
		}
		providers["mymeet"] = app.NewMymeet(cfg.MymeetAPIKey, cfg.MymeetBaseURL, cfg.MymeetTemplate, loc)
	}
	// Активный (для новых джоб и оркестратора) — LoadConfig уже проверил, что ключ
	// активного поставщика задан, значит он в мапе.
	provider := providers[cfg.TranscriptionProvider]
	slog.Info("startup configured",
		"event", "startup_configured",
		"phase", "startup",
		"provider", cfg.TranscriptionProvider,
		"summarizer", cfg.SummarizerProvider,
		"providers_count", len(providers),
	)

	// Hot-swap провайдера саммари = строка SUMMARIZER_PROVIDER в .env.
	var summarizer app.Summarizer
	switch cfg.SummarizerProvider {
	case "mock":
		summarizer = app.NewMockSummarizer()
	case "openrouter":
		summarizer = app.NewOpenRouterSummarizer(cfg.OpenRouterAPIKey, cfg.OpenRouterBaseURL, cfg.OpenRouterModel, cfg.OpenRouterProxyURL)
	}

	bot, err := app.NewBot(cfg, storage, billing, providers, summarizer)
	if err != nil {
		return fmt.Errorf("create bot: %w", err)
	}

	delivery := app.NewTelegramDelivery(bot)
	orch := app.NewOrchestrator(storage, provider, delivery, summarizer, bot.SendText, bot.EditStatus, 30*time.Second, cfg.JobMaxAge, cfg.JobStartTimeout, cfg.JobSubmitMaxAttempts, cfg.TrialLimitMinutes)
	// F9-пуши (notifier.go): суточная логика, 1ч-тик достаточно — отдельный конфиг
	// был бы лишним (architecture.md §7.2).
	notifier := app.NewNotifier(storage, bot.SendText, cfg.TrialLimitMinutes, time.Hour)
	paymentPoller := app.NewPaymentPoller(storage, billing, bot.SendText, cfg.PaymentPollInterval)
	recurringWorker := app.NewRecurringWorker(storage, billing, cfg.PaymentPollInterval, cfg.RecurringPaymentsEnabled)

	// Оркестратор и notifier стартуют до bot.Start(): bot.Start() блокирует,
	// поэтому горутины запускаем первыми. ctx отменяется по SIGINT/SIGTERM —
	// обе завершатся по ctx.Done().
	go orch.Run(ctx)
	go notifier.Run(ctx)
	go paymentPoller.Run(ctx)
	go recurringWorker.Run(ctx)

	healthErr := make(chan error, 1)
	go func() {
		err := runHealthServer(ctx, "127.0.0.1:8080", healthHandler(pool.Ping))
		healthErr <- err
		if err != nil {
			stop()
		}
	}()

	// По сигналу завершения останавливаем long polling — Start() разблокируется.
	go func() {
		<-ctx.Done()
		slog.Info("shutdown signal received", "event", "shutdown_signal", "phase", "shutdown")
		bot.Stop()
	}()

	// Провайдер в стартовом логе: при hot-swap через .env первое, что нужно
	// видеть — кто реально включён.
	slog.Info("transcrib started", "event", "startup_complete", "phase", "startup", "provider", cfg.TranscriptionProvider, "summarizer", cfg.SummarizerProvider)
	bot.Start()
	stop()
	if err := <-healthErr; err != nil {
		return fmt.Errorf("health server: %w", err)
	}
	slog.Info("shutdown complete", "event", "shutdown_complete", "phase", "shutdown")
	return nil
}

func healthHandler(ping func(context.Context) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := ping(ctx); err != nil {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

func runHealthServer(ctx context.Context, addr string, handler http.Handler) error {
	server := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		err := <-errCh
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func checkHealth(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("request health endpoint: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health endpoint returned %s", resp.Status)
	}
	return nil
}
