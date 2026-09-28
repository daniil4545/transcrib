package app

import (
	"context"
	"encoding/json"
	"testing"
)

// TestMockProvider_HappyPath проверяет сценарий Submit -> Poll (до Done) -> Fetch.
func TestMockProvider_HappyPath(t *testing.T) {
	ctx := context.Background()
	job := Job{ID: 42, MeetingURL: "https://meet.google.com/abc-def"}

	p := &MockProvider{DoneAfterPolls: 2}

	// Submit
	extID, err := p.Submit(ctx, job)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if extID == "" {
		t.Fatal("Submit: пустой externalJobID")
	}

	// Первые DoneAfterPolls вызовов — Pending
	for i := range p.DoneAfterPolls {
		status, err := p.Poll(ctx, extID)
		if err != nil {
			t.Fatalf("Poll %d: %v", i+1, err)
		}
		if status != StatusPending {
			t.Fatalf("Poll %d: ожидали StatusPending, получили %d", i+1, status)
		}
	}

	// Следующий вызов — Done
	status, err := p.Poll(ctx, extID)
	if err != nil {
		t.Fatalf("Poll (done): %v", err)
	}
	if status != StatusDone {
		t.Fatalf("Poll (done): ожидали StatusDone, получили %d", status)
	}

	// Fetch
	result, err := p.Fetch(ctx, extID)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if result.TranscriptMarkdown == "" {
		t.Fatal("Fetch: пустой TranscriptMarkdown")
	}

	// Raw — валидный JSON (требование колонки JSONB)
	if !json.Valid(result.Raw) {
		t.Fatalf("Fetch: Raw не является валидным JSON: %s", result.Raw)
	}
}

// TestMockProvider_PerJobPollCounters — счётчики Poll независимы по externalJobID:
// прогресс одной джобы не двигает другую (единственная stateful-логика мока).
func TestMockProvider_PerJobPollCounters(t *testing.T) {
	ctx := context.Background()
	p := &MockProvider{DoneAfterPolls: 1}

	steps := []struct {
		extID string
		want  ProviderStatus
	}{
		{"a", StatusPending}, // a: первый Poll
		{"b", StatusPending}, // b: свой первый Poll, счётчик a не влияет
		{"a", StatusDone},    // a: второй Poll
		{"b", StatusDone},    // b: второй Poll, независимо от a
	}
	for i, step := range steps {
		status, err := p.Poll(ctx, step.extID)
		if err != nil {
			t.Fatalf("шаг %d (%s): %v", i+1, step.extID, err)
		}
		if status != step.want {
			t.Fatalf("шаг %d (%s): статус %d, ожидали %d", i+1, step.extID, status, step.want)
		}
	}
}

// TestMockProvider_Phased — режим Phased проводит джобу по фазам провайдера
// OnCall -> Transcribing -> Done (по одному Poll на фазу), затем держит Done.
// Нужен для теста полного потока orchestrator joining -> recording -> processing -> done.
func TestMockProvider_Phased(t *testing.T) {
	ctx := context.Background()
	p := &MockProvider{Phased: true}

	extID, err := p.Submit(ctx, Job{ID: 1})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	want := []ProviderStatus{StatusOnCall, StatusTranscribing, StatusDone, StatusDone}
	for i, w := range want {
		status, err := p.Poll(ctx, extID)
		if err != nil {
			t.Fatalf("Poll %d: %v", i+1, err)
		}
		if status != w {
			t.Fatalf("Poll %d: статус %d, ожидали %d", i+1, status, w)
		}
	}
}

// TestMockProvider_FailMode — контракт режима отказа фикстуры (FailOnPoll=true):
// единственный оставленный unit на «фейк умеет упасть по команде». Симметричный
// MockSummarizer.FailOnSummarize покрыт E2E через TestSummarizeJob_FailSummarize.
func TestMockProvider_FailMode(t *testing.T) {
	ctx := context.Background()
	job := Job{ID: 7}

	p := &MockProvider{FailOnPoll: true}

	extID, err := p.Submit(ctx, job)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	status, err := p.Poll(ctx, extID)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if status != StatusFailed {
		t.Fatalf("Poll: ожидали StatusFailed, получили %d", status)
	}
}
