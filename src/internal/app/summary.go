package app

import (
	"context"
	_ "embed"
	"errors"
	"strings"
)

// Summarizer изолирует orchestrator от провайдера саммари (F3). Реализации:
// MockSummarizer и OpenRouterSummarizer; активная выбирается через SUMMARIZER_PROVIDER.
type Summarizer interface {
	// Summarize превращает готовый markdown-транскрипт в markdown-конспект (F3:
	// ключевые темы, решения, задачи с ответственными). Вызывается на шаге summarizing.
	Summarize(ctx context.Context, transcript string) (markdown string, err error)
}

//go:embed summary_prompt.txt
var defaultSummaryPrompt string

var _ Summarizer = (*MockSummarizer)(nil)

// MockSummarizer — конспект без сети, для локального E2E без ключа.
type MockSummarizer struct {
	prompt string

	// FailOnSummarize — если true, Summarize возвращает ошибку (режим отказа для E2E).
	// Нулевое значение даёт happy path.
	FailOnSummarize bool
}

// NewMockSummarizer инкапсулирует чтение встроенного промпта — main.go про embed не знает.
func NewMockSummarizer() *MockSummarizer { return &MockSummarizer{prompt: defaultSummaryPrompt} }

// Summarize возвращает фиксированный конспект; первая строка промпта попадает в вывод,
// чтобы E2E подтверждал: замена summary_prompt.txt меняет результат.
func (m *MockSummarizer) Summarize(_ context.Context, _ string) (string, error) {
	if m.FailOnSummarize {
		return "", errors.New("mock summarizer forced failure")
	}
	firstLine := strings.SplitN(m.prompt, "\n", 2)[0]
	return "# Конспект встречи\n\n" +
		"_(промпт: " + firstLine + ")_\n\n" +
		"## Ключевые темы\n- Тема 1\n\n## Решения\n- Решение 1\n\n" +
		"## Задачи\n- [ ] Задача 1 — ответственный\n", nil
}
