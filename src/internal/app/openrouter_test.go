package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestSplitTranscript: splitting on line boundaries, the invariant "join with \n == original",
// every chunk within the limit (except a single line longer than the limit, which goes as is).
func TestSplitTranscript(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		limit      int
		wantChunks int
	}{
		{"empty", "", 10, 0},
		{"single short line", "abc", 10, 1},
		{"two lines fit one chunk", "aaaa\nbbbb", 10, 1},
		{"split at line boundary", "aaaa\nbbbb\ncccc", 10, 2},
		{"oversized single line stays whole", strings.Repeat("a", 25), 10, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			chunks := splitTranscript(c.in, c.limit)
			if len(chunks) != c.wantChunks {
				t.Fatalf("chunks=%d, want %d (%q)", len(chunks), c.wantChunks, chunks)
			}
			// Invariant: joining the chunks with \n restores the original text.
			if got := strings.Join(chunks, "\n"); got != c.in {
				t.Fatalf("rejoined=%q, want %q", got, c.in)
			}
			// Every chunk is within the limit, except a single line without line breaks.
			for _, ch := range chunks {
				if len(ch) > c.limit && strings.Contains(ch, "\n") {
					t.Fatalf("chunk over limit %d with line break: %q", c.limit, ch)
				}
			}
		})
	}
}

// newTestSummarizer starts an httptest server with the given handler and returns an
// OpenRouterSummarizer configured for it.
func newTestSummarizer(t *testing.T, h http.HandlerFunc) *OpenRouterSummarizer {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &OpenRouterSummarizer{
		apiKey:  "test-key",
		baseURL: srv.URL,
		model:   "test/model",
		prompt:  "reduce-prompt",
		client:  srv.Client(),
	}
}

func TestOpenRouterSummarize_Happy(t *testing.T) {
	s := newTestSummarizer(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("auth header = %q", got)
		}
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"content":"# Конспект\n- тема"}}]}`))
	})
	got, err := s.Summarize(context.Background(), "Спикер 1: привет")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "# Конспект\n- тема" {
		t.Fatalf("content=%q", got)
	}
}

func TestOpenRouterSummarize_UsesProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request bypassed proxy")
	}))
	t.Cleanup(upstream.Close)

	proxyCalls := 0
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalls++
		if r.URL.String() != upstream.URL+"/chat/completions" {
			t.Errorf("proxied URL = %q, want %q", r.URL.String(), upstream.URL+"/chat/completions")
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("auth header = %q", got)
		}
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"content":"via proxy"}}]}`))
	}))
	t.Cleanup(proxy.Close)

	s := NewOpenRouterSummarizer("test-key", upstream.URL, "test/model", proxy.URL)
	got, err := s.Summarize(context.Background(), "Спикер 1: проверка proxy")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "via proxy" {
		t.Fatalf("content=%q", got)
	}
	if proxyCalls != 1 {
		t.Fatalf("proxy calls=%d, want 1", proxyCalls)
	}
}

func TestOpenRouterSummarize_Errors(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantSub string
	}{
		{"non-2xx", http.StatusTooManyRequests, `{"error":{"message":"rate limited"}}`, "HTTP 429"},
		{"error in 200 body", http.StatusOK, `{"error":{"code":502,"message":"upstream down"}}`, "200 body"},
		{"error in choice", http.StatusOK, `{"choices":[{"finish_reason":"error","error":{"message":"boom"}}]}`, "choice"},
		{"finish_reason length", http.StatusOK, `{"choices":[{"finish_reason":"length","message":{"content":"partial"}}]}`, "finish_reason"},
		{"empty content", http.StatusOK, `{"choices":[{"finish_reason":"stop","message":{"content":"   "}}]}`, "empty content"},
		{"no choices", http.StatusOK, `{"choices":[]}`, "no choices"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newTestSummarizer(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			})
			_, err := s.Summarize(context.Background(), "Спикер 1: привет")
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Fatalf("error %q does not contain %q", err.Error(), c.wantSub)
			}
		})
	}
}

func TestOpenRouterSummarize_RetriesTransientChoiceError(t *testing.T) {
	calls := 0
	s := newTestSummarizer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"error","error":{"message":"JSON error injected into SSE stream"}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"content":"# Конспект после retry"}}]}`))
	})

	got, err := s.Summarize(context.Background(), "Спикер 1: привет")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "# Конспект после retry" {
		t.Fatalf("content=%q", got)
	}
	if calls != 2 {
		t.Fatalf("calls=%d, want 2", calls)
	}
}

func TestOpenRouterSummarize_Permanent403NotRetried(t *testing.T) {
	calls := 0
	s := newTestSummarizer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"Access denied by security policy.","metadata":{"error_type":"permission_denied"}}}`))
	})

	_, err := s.Summarize(context.Background(), "Спикер 1: привет")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("error %q does not contain HTTP 403", err.Error())
	}
	if calls != 1 {
		t.Fatalf("calls=%d, want 1", calls)
	}
}

func TestOpenRouterRetryDelay_UsesRetryAfter(t *testing.T) {
	if got := openRouterRetryDelay(1, 3*time.Second); got != 3*time.Second {
		t.Fatalf("delay=%v, want Retry-After value", got)
	}
}

func TestOpenRouterSummarize_EmptyTranscript(t *testing.T) {
	// Empty input must not produce an HTTP call. t.Errorf, not t.Fatal: the handler
	// runs in its own goroutine, and t.Fatal from there is UB (Goexit of the wrong goroutine).
	s := newTestSummarizer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("HTTP call must not happen for empty transcript")
	})
	if _, err := s.Summarize(context.Background(), "   \n  "); err == nil {
		t.Fatal("expected error for empty transcript")
	}
}

func TestOpenRouterSummarize_MapReduce(t *testing.T) {
	// A transcript longer than maxChunkChars -> N map calls + 1 reduce.
	line := "Спикер 1: " + strings.Repeat("a", 990) + "\n"
	transcript := strings.Repeat(line, 70) // ~70k characters > maxChunkChars
	wantCalls := len(splitTranscript(transcript, maxChunkChars)) + 1
	if wantCalls < 3 {
		t.Fatalf("test setup: expected map-reduce (>=3 calls), got %d", wantCalls)
	}

	calls := 0
	s := newTestSummarizer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"content":"chunk-summary"}}]}`))
	})
	got, err := s.Summarize(context.Background(), transcript)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != wantCalls {
		t.Fatalf("calls=%d, want %d", calls, wantCalls)
	}
	if got != "chunk-summary" {
		t.Fatalf("content=%q", got)
	}
}

func TestOpenRouterSummarize_MapReduceRetriesOnlyFailedChunk(t *testing.T) {
	line := "Спикер 1: " + strings.Repeat("a", 990) + "\n"
	transcript := strings.Repeat(line, 70)
	chunks := splitTranscript(transcript, maxChunkChars)
	if len(chunks) != 2 {
		t.Fatalf("test setup: chunks=%d, want 2", len(chunks))
	}

	calls := 0
	s := newTestSummarizer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch calls {
		case 1:
			_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"content":"part-1"}}]}`))
		case 2:
			_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"error","error":{"message":"temporary provider error","metadata":{"error_type":"provider_overloaded"}}}]}`))
		case 3:
			_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"content":"part-2"}}]}`))
		case 4:
			_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"content":"final-summary"}}]}`))
		default:
			t.Errorf("unexpected extra call %d", calls)
			_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"content":"unexpected"}}]}`))
		}
	})

	got, err := s.Summarize(context.Background(), transcript)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "final-summary" {
		t.Fatalf("content=%q", got)
	}
	if calls != len(chunks)+2 {
		t.Fatalf("calls=%d, want %d", calls, len(chunks)+2)
	}
}

func TestOpenRouterSummarize_ContextCancelStopsRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	calls := 0
	s := newTestSummarizer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		cancel()
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"error","error":{"message":"temporary provider error","metadata":{"error_type":"timeout"}}}]}`))
	})

	_, err := s.Summarize(ctx, "Спикер 1: привет")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Fatalf("calls=%d, want 1", calls)
	}
}
