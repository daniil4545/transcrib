package app

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Компайл-тайм ассерт: OpenRouterSummarizer реализует интерфейс Summarizer.
var _ Summarizer = (*OpenRouterSummarizer)(nil)

//go:embed summary_map_prompt.txt
var defaultMapPrompt string

// maxChunkChars — порог map-reduce: транскрипт длиннее режем на куски, саммаризируем
// по частям и сводим. Порог по РАЗМЕРУ, а не по лимиту контекста модели: при 1M-контексте
// gemini лимитный триггер не сработал бы никогда (мёртвый код). ~60k символов (~20k токенов
// для русского) — короткие встречи идут одним вызовом, длинные дробятся.
const maxChunkChars = 60000

const (
	openRouterMaxAttempts = 3
	openRouterBaseBackoff = 50 * time.Millisecond
	openRouterMaxBackoff  = 500 * time.Millisecond
)

// OpenRouterSummarizer — реализация Summarizer (F3) через OpenRouter (API.md).
// OpenAI-совместимый chat-completion: один вызов на короткой встрече, map-reduce на длинной.
type OpenRouterSummarizer struct {
	apiKey  string
	baseURL string // default https://openrouter.ai/api/v1
	model   string // напр. google/gemini-2.5-flash; свап через .env без пересборки
	prompt  string // reduce-промпт (summary_prompt.txt); map-промпт — defaultMapPrompt
	client  *http.Client
}

// NewOpenRouterSummarizer создаёт провайдер. apiKey только в заголовке Authorization,
// в логи и тексты ошибок не попадает. prompt берётся из встроенного summary_prompt.txt.
func NewOpenRouterSummarizer(apiKey, baseURL, model, proxyURL string) *OpenRouterSummarizer {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if proxyURL != "" {
		u, err := url.Parse(proxyURL)
		if err == nil {
			transport.Proxy = http.ProxyURL(u)
		}
	}

	return &OpenRouterSummarizer{
		apiKey:  apiKey,
		baseURL: baseURL,
		model:   model,
		prompt:  defaultSummaryPrompt,
		// Реальный предел — ctx (summaryTimeout в orchestrator оборачивает весь Summarize,
		// включая N вызовов map-reduce). Timeout здесь — страховка от одиночного зависшего
		// вызова, заведомо больше ctx-таймаута, чтобы не срабатывать первым.
		client: &http.Client{Timeout: 5 * time.Minute, Transport: transport},
	}
}

// Summarize превращает markdown-транскрипт в markdown-конспект. Короткая встреча — один
// вызов; длинная (> maxChunkChars) — map-reduce: сжать куски map-промптом, свести reduce-промптом.
func (s *OpenRouterSummarizer) Summarize(ctx context.Context, transcript string) (string, error) {
	if strings.TrimSpace(transcript) == "" {
		// Пустой вход не шлём в API (поведение на пустоте не зафиксировано, API.md §5).
		return "", fmt.Errorf("openrouter summarize: empty transcript")
	}

	if len(transcript) <= maxChunkChars {
		return s.chat(ctx, s.prompt, transcript)
	}

	chunks := splitTranscript(transcript, maxChunkChars)
	partials := make([]string, 0, len(chunks))
	for i, chunk := range chunks {
		part, err := s.chat(ctx, defaultMapPrompt, chunk)
		if err != nil {
			return "", fmt.Errorf("map chunk %d/%d: %w", i+1, len(chunks), err)
		}
		partials = append(partials, part)
	}
	return s.chat(ctx, s.prompt, strings.Join(partials, "\n\n"))
}

// splitTranscript режет транскрипт на куски не длиннее limit, разбивая строго по границам
// строк — реплики спикеров не рвутся. Одиночная строка длиннее limit (гигантская реплика
// без переносов) уходит отдельным куском как есть.
func splitTranscript(transcript string, limit int) []string {
	lines := strings.Split(transcript, "\n")
	var chunks []string
	var b strings.Builder
	for _, line := range lines {
		// +1 на перенос между строками; не флашим пустой буфер (иначе теряли бы строку).
		if b.Len() > 0 && b.Len()+1+len(line) > limit {
			chunks = append(chunks, b.String())
			b.Reset()
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
	}
	if b.Len() > 0 {
		chunks = append(chunks, b.String())
	}
	return chunks
}

// orRequest/orResponse/orError — минимальные структуры контракта chat/completions (API.md §1).
type orRequest struct {
	Model       string      `json:"model"`
	Messages    []orMessage `json:"messages"`
	Temperature float64     `json:"temperature"` // без omitempty: 0.0 — валидное значение, omitempty молча дал бы дефолт 1.0
	MaxTokens   int         `json:"max_tokens,omitempty"`
}

type orMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type orResponse struct {
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content string `json:"content"`
		} `json:"message"`
		Error *orError `json:"error"` // HTTP 200 может нести ошибку прямо в choice (API.md §1.2)
	} `json:"choices"`
	Error *orError `json:"error"` // ...или на верхнем уровне тела 200
}

// orError — тело ошибки {"error":{"code","message"}}. Code разнотипен (int в chat, string в
// beta Responses API), поэтому RawMessage — Unmarshal не упадёт; нам важен message для диагноза.
type orError struct {
	Code     json.RawMessage            `json:"code"`
	Message  string                     `json:"message"`
	Metadata map[string]json.RawMessage `json:"metadata"`
}

func (e *orError) errorType() string {
	if e == nil || e.Metadata == nil {
		return ""
	}
	var errorType string
	if data, ok := e.Metadata["error_type"]; ok {
		_ = json.Unmarshal(data, &errorType)
	}
	return errorType
}

func (e *orError) statusCode() int {
	if e == nil || len(e.Code) == 0 {
		return 0
	}
	var code int
	if err := json.Unmarshal(e.Code, &code); err == nil {
		return code
	}
	var codeText string
	if err := json.Unmarshal(e.Code, &codeText); err == nil {
		switch codeText {
		case "408":
			return http.StatusRequestTimeout
		case "429":
			return http.StatusTooManyRequests
		case "500":
			return http.StatusInternalServerError
		case "502":
			return http.StatusBadGateway
		case "503":
			return http.StatusServiceUnavailable
		case "504":
			return http.StatusGatewayTimeout
		default:
			return 0
		}
	}
	return 0
}

type openRouterCallError struct {
	err        error
	retry      bool
	retryAfter time.Duration
}

func (e *openRouterCallError) Error() string { return e.err.Error() }
func (e *openRouterCallError) Unwrap() error { return e.err }

func newOpenRouterCallError(err error, retry bool, retryAfter time.Duration) error {
	return &openRouterCallError{err: err, retry: retry, retryAfter: retryAfter}
}

// chat делает один вызов chat/completions и возвращает текст ответа. Любой не-stop
// finish_reason, ошибка в теле 200 или пустой content — ошибка (контракт Summarize:
// orchestrator уведёт джобу в failed_summary, транскрипт уже у юзера).
func (s *OpenRouterSummarizer) chat(ctx context.Context, systemPrompt, userContent string) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= openRouterMaxAttempts; attempt++ {
		content, err := s.chatOnce(ctx, systemPrompt, userContent)
		if err == nil {
			return content, nil
		}
		lastErr = err

		var callErr *openRouterCallError
		if !errors.As(err, &callErr) || !callErr.retry || attempt == openRouterMaxAttempts {
			return "", err
		}
		if err := sleepOpenRouterRetry(ctx, openRouterRetryDelay(attempt, callErr.retryAfter)); err != nil {
			return "", err
		}
	}
	return "", lastErr
}

// chatOnce делает один вызов chat/completions и валидирует один ответ. Retry-решение
// держим снаружи, чтобы map-reduce не пересчитывал уже успешные chunks.
func (s *OpenRouterSummarizer) chatOnce(ctx context.Context, systemPrompt, userContent string) (string, error) {
	body, err := json.Marshal(orRequest{
		Model:       s.model,
		Temperature: 0.3, // низкая — конспект детерминированнее, без креатива
		MaxTokens:   8192,
		Messages: []orMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userContent},
		},
	})
	if err != nil {
		return "", fmt.Errorf("openrouter marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("openrouter request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return "", newOpenRouterCallError(fmt.Errorf("openrouter do: %w", err), ctx.Err() == nil, 0)
	}
	defer func() { _ = resp.Body.Close() }()

	// Лимит против сбойного провайдера/прокси с гигантским телом: ответ саммари — единицы КБ,
	// 1 MiB перекрывает с запасом. Тело читаем целиком: и для парса, и для текста ошибки.
	const maxBody = 1 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return "", newOpenRouterCallError(fmt.Errorf("openrouter read: %w", err), true, 0)
	}
	if len(data) > maxBody {
		return "", fmt.Errorf("openrouter: response exceeds %d bytes", maxBody)
	}

	if resp.StatusCode/100 != 2 {
		// Тело несёт {"error":{"message"}} — главный источник диагноза. Ключ в нём не светится (он в заголовке).
		return "", newOpenRouterCallError(
			fmt.Errorf("openrouter: HTTP %d: %s", resp.StatusCode, data[:min(len(data), 256)]),
			openRouterHTTPRetryable(resp.StatusCode),
			parseRetryAfter(resp.Header.Get("Retry-After")),
		)
	}

	var result orResponse
	if err := json.Unmarshal(data, &result); err != nil {
		return "", newOpenRouterCallError(fmt.Errorf("openrouter decode: %w", err), true, 0)
	}
	if result.Error != nil {
		return "", newOpenRouterCallError(
			fmt.Errorf("openrouter: error in 200 body: %s", result.Error.Message),
			openRouterErrorRetryable(result.Error),
			0,
		)
	}
	if len(result.Choices) == 0 {
		return "", fmt.Errorf("openrouter: no choices in response")
	}
	choice := result.Choices[0]
	if choice.Error != nil {
		return "", newOpenRouterCallError(
			fmt.Errorf("openrouter: error in choice: %s", choice.Error.Message),
			openRouterErrorRetryable(choice.Error),
			0,
		)
	}
	// finish_reason != stop (length/content_filter/error) — конспект неполный/сбойный (API.md §1.2).
	if choice.FinishReason != "" && choice.FinishReason != "stop" {
		return "", fmt.Errorf("openrouter: finish_reason=%q (incomplete)", choice.FinishReason)
	}
	content := strings.TrimSpace(choice.Message.Content)
	if content == "" {
		return "", fmt.Errorf("openrouter: empty content in response")
	}
	return content, nil
}

func openRouterHTTPRetryable(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func openRouterErrorRetryable(err *orError) bool {
	switch err.errorType() {
	case "rate_limit_exceeded", "provider_overloaded", "provider_unavailable", "timeout", "server", "unmapped":
		return true
	case "context_length_exceeded", "invalid_request", "invalid_prompt", "permission_denied", "payment_required", "content_policy_violation", "refusal":
		return false
	}
	if openRouterHTTPRetryable(err.statusCode()) {
		return true
	}
	if strings.Contains(strings.ToLower(err.Message), "json error injected into sse stream") {
		return true
	}
	return false
}

func openRouterRetryDelay(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return retryAfter
	}
	delay := openRouterBaseBackoff << (attempt - 1)
	if delay > openRouterMaxBackoff {
		delay = openRouterMaxBackoff
	}
	// Небольшой jitter разводит одновременные job'ы без добавления глобального random state.
	return delay + time.Duration(time.Now().UnixNano()%int64(delay/4+1))
}

func parseRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := time.ParseDuration(value + "s"); err == nil && seconds > 0 {
		return seconds
	}
	if when, err := http.ParseTime(value); err == nil {
		delay := time.Until(when)
		if delay > 0 {
			return delay
		}
	}
	return 0
}

func sleepOpenRouterRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
