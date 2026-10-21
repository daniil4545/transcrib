package app

import (
	"context"
	_ "embed"
	"errors"
	"strings"
)

// Summarizer isolates the orchestrator from the summary provider (F3). Implementations:
// MockSummarizer and OpenRouterSummarizer; the active one is chosen via SUMMARIZER_PROVIDER.
type Summarizer interface {
	// Summarize turns a finished markdown transcript into a markdown summary (F3:
	// key topics, decisions, tasks with owners). Called at the summarizing step.
	Summarize(ctx context.Context, transcript string) (markdown string, err error)
}

//go:embed summary_prompt.txt
var defaultSummaryPrompt string

var _ Summarizer = (*MockSummarizer)(nil)

// MockSummarizer is a network-free summary for local E2E without a key.
type MockSummarizer struct {
	prompt string

	// FailOnSummarize: if true, Summarize returns an error (failure mode for E2E).
	// The zero value gives the happy path.
	FailOnSummarize bool
}

// NewMockSummarizer encapsulates reading the embedded prompt, so main.go knows nothing about embed.
func NewMockSummarizer() *MockSummarizer { return &MockSummarizer{prompt: defaultSummaryPrompt} }

// Summarize returns a fixed summary; the first line of the prompt goes into the output
// so that E2E confirms that replacing summary_prompt.txt changes the result.
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
