package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    slog.Level
		wantErr bool
	}{
		{name: "default", raw: "", want: slog.LevelInfo},
		{name: "info", raw: "info", want: slog.LevelInfo},
		{name: "debug", raw: "debug", want: slog.LevelDebug},
		{name: "warn", raw: "warn", want: slog.LevelWarn},
		{name: "error", raw: "error", want: slog.LevelError},
		{name: "trim and case", raw: " DEBUG ", want: slog.LevelDebug},
		{name: "bad", raw: "trace", want: slog.LevelInfo, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseLogLevel(tt.raw)
			if tt.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHealthHandler(t *testing.T) {
	tests := []struct {
		name       string
		pingErr    error
		wantStatus int
	}{
		{name: "ready", wantStatus: http.StatusOK},
		{name: "database unavailable", pingErr: errors.New("connection refused"), wantStatus: http.StatusServiceUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := healthHandler(func(ctx context.Context) error {
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 2*time.Second {
					t.Fatal("ping context must have a two-second timeout")
				}
				return tt.pingErr
			})
			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, req)

			if recorder.Code != tt.wantStatus {
				t.Fatalf("got status %d, want %d", recorder.Code, tt.wantStatus)
			}
		})
	}
}

func TestHealthHandlerRejectsOtherPath(t *testing.T) {
	handler := healthHandler(func(context.Context) error {
		t.Fatal("ping must not run for another path")
		return nil
	})
	req := httptest.NewRequest(http.MethodGet, "/other", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("got status %d, want %d", recorder.Code, http.StatusNotFound)
	}
}

func TestCheckHealth(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		wantErr    bool
	}{
		{name: "healthy", statusCode: http.StatusOK},
		{name: "unhealthy", statusCode: http.StatusServiceUnavailable, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.statusCode)
			}))
			defer server.Close()

			err := checkHealth(context.Background(), server.URL)
			if tt.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestRunCommandHealthcheck(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if err := runCommand([]string{"healthcheck", server.URL}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := runCommand([]string{"healthcheck"}); err == nil {
		t.Fatal("expected usage error")
	}
}

func TestRunHealthServerShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := runHealthServer(ctx, "127.0.0.1:0", http.NotFoundHandler()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunHealthServerListenError(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = listener.Close() }()

	err = runHealthServer(context.Background(), listener.Addr().String(), http.NotFoundHandler())
	if err == nil {
		t.Fatal("expected listen error")
	}
}
