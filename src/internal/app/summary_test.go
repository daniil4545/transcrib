package app

import (
	"context"
	"strings"
	"testing"
)

// Проводка промпта: первая строка summary_prompt.txt обязана попадать в вывод
// MockSummarizer. Это критерий приёмки E2E (замена промпта меняет результат) на
// уровне unit — без него embed->output-связь покрыта только ручным прогоном.
func TestMockSummarizer_PromptWiredIntoOutput(t *testing.T) {
	m := NewMockSummarizer()

	out, err := m.Summarize(context.Background(), "любой транскрипт")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}

	firstLine := strings.SplitN(defaultSummaryPrompt, "\n", 2)[0]
	if !strings.Contains(out, firstLine) {
		t.Errorf("первая строка промпта не попала в вывод\nпромпт: %q\nвывод: %q", firstLine, out)
	}
}
