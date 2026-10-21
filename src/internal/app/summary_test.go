package app

import (
	"context"
	"strings"
	"testing"
)

// Prompt wiring: the first line of summary_prompt.txt must appear in the output of
// MockSummarizer. This is the E2E acceptance criterion (replacing the prompt changes
// the result) at unit level: without it the embed->output link is covered only by a
// manual run.
func TestMockSummarizer_PromptWiredIntoOutput(t *testing.T) {
	m := NewMockSummarizer()

	out, err := m.Summarize(context.Background(), "любой транскрипт")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}

	firstLine := strings.SplitN(defaultSummaryPrompt, "\n", 2)[0]
	if !strings.Contains(out, firstLine) {
		t.Errorf("first prompt line is missing from the output\nprompt: %q\noutput: %q", firstLine, out)
	}
}
