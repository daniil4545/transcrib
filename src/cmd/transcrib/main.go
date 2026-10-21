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

	// The context lives until the first SIGINT/SIGTERM and is cancelled on the signal.
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

	// Build implementations of ALL configured providers (by key presence), not only
	// the active one: external_job_id belongs to the namespace of the provider that created it,
	// so redelivery and cancellation in the "My calls" panel resolve the right one by job.provider,
	// not by the active TRANSCRIPTION_PROVIDER (otherwise after a provider switch a finished
	// job cannot be delivered: its id is foreign to the current provider).
	providers := map[string]app.TranscriptionProvider{
		// DoneAfterPolls: 1 - one Pending tick, then Done; progress is visible in the logs.
		"mock": &app.MockProvider{DoneAfterPolls: 1},
	}
	if cfg.Speech2TextAPIKey != "" {
		providers["speech2text"] = app.NewSpeech2Text(cfg.Speech2TextAPIKey, cfg.Speech2TextBaseURL)
	}
	if cfg.MymeetAPIKey != "" {
		// LoadLocation is here, not in LoadConfig: tzdata is runtime-dependent, so we catch it
		// next to the constructor. A bad MYMEET_TIMEZONE = an informative startup failure.
		loc, err := time.LoadLocation(cfg.MymeetTimezone)
		if err != nil {
			return fmt.Errorf("load MYMEET_TIMEZONE %q: %w", cfg.MymeetTimezone, err)
		}
		providers["mymeet"] = app.NewMymeet(cfg.MymeetAPIKey, cfg.MymeetBaseURL, cfg.MymeetTemplate, loc)
	}
	// The active one (for new jobs and the orchestrator): LoadConfig has already checked that the
	// active provider's key is set, so it is in the map.
	provider := providers[cfg.TranscriptionProvider]
	slog.Info("startup configured",
		"event", "startup_configured",
		"phase", "startup",
		"provider", cfg.TranscriptionProvider,
		"summarizer", cfg.SummarizerProvider,
		"providers_count", len(providers),
	)

	// Hot-swapping the summary provider = the SUMMARIZER_PROVIDER line in .env.
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
	// F9 pushes (notifier.go): daily logic, a 1h tick is enough; a separate config
	// would be redundant (architecture.md §7.2).
	notifier := app.NewNotifier(storage, bot.SendText, cfg.TrialLimitMinutes, time.Hour)
	paymentPoller := app.NewPaymentPoller(storage, billing, bot.SendText, cfg.PaymentPollInterval)
	recurringWorker := app.NewRecurringWorker(storage, billing, cfg.PaymentPollInterval, cfg.RecurringPaymentsEnabled)

	// The orchestrator and notifier start before bot.Start(): bot.Start() blocks,
	// so the goroutines are started first. ctx is cancelled on SIGINT/SIGTERM;
	// both exit on ctx.Done().
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

	// On the shutdown signal, stop long polling so Start() unblocks.
	go func() {
		<-ctx.Done()
		slog.Info("shutdown signal received", "event", "shutdown_signal", "phase", "shutdown")
		bot.Stop()
	}()

	// Provider in the startup log: when hot-swapping via .env, the first thing to
	// see is which one is actually enabled.
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
