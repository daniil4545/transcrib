package app

import (
	"context"
	"encoding/json"
	"testing"
)

// TestMockProvider_HappyPath covers the Submit -> Poll (until Done) -> Fetch scenario.
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
		t.Fatal("Submit: empty externalJobID")
	}

	// The first DoneAfterPolls calls return Pending
	for i := range p.DoneAfterPolls {
		status, err := p.Poll(ctx, extID)
		if err != nil {
			t.Fatalf("Poll %d: %v", i+1, err)
		}
		if status != StatusPending {
			t.Fatalf("Poll %d: want StatusPending, got %d", i+1, status)
		}
	}

	// The next call returns Done
	status, err := p.Poll(ctx, extID)
	if err != nil {
		t.Fatalf("Poll (done): %v", err)
	}
	if status != StatusDone {
		t.Fatalf("Poll (done): want StatusDone, got %d", status)
	}

	// Fetch
	result, err := p.Fetch(ctx, extID)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if result.TranscriptMarkdown == "" {
		t.Fatal("Fetch: empty TranscriptMarkdown")
	}

	// Raw is valid JSON (JSONB column requirement)
	if !json.Valid(result.Raw) {
		t.Fatalf("Fetch: Raw is not valid JSON: %s", result.Raw)
	}
}

// TestMockProvider_PerJobPollCounters: Poll counters are independent per externalJobID,
// so progress of one job does not move another (the only stateful logic of the mock).
func TestMockProvider_PerJobPollCounters(t *testing.T) {
	ctx := context.Background()
	p := &MockProvider{DoneAfterPolls: 1}

	steps := []struct {
		extID string
		want  ProviderStatus
	}{
		{"a", StatusPending}, // a: first Poll
		{"b", StatusPending}, // b: its own first Poll, a's counter has no effect
		{"a", StatusDone},    // a: second Poll
		{"b", StatusDone},    // b: second Poll, independent of a
	}
	for i, step := range steps {
		status, err := p.Poll(ctx, step.extID)
		if err != nil {
			t.Fatalf("step %d (%s): %v", i+1, step.extID, err)
		}
		if status != step.want {
			t.Fatalf("step %d (%s): status %d, want %d", i+1, step.extID, status, step.want)
		}
	}
}

// TestMockProvider_Phased: Phased mode walks a job through the provider phases
// OnCall -> Transcribing -> Done (one Poll per phase), then stays in Done.
// Needed for the full orchestrator flow test joining -> recording -> processing -> done.
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
			t.Fatalf("Poll %d: status %d, want %d", i+1, status, w)
		}
	}
}

// TestMockProvider_FailMode: contract of the fixture failure mode (FailOnPoll=true).
// It is the only remaining unit test for "the fake can fail on demand". The symmetric
// MockSummarizer.FailOnSummarize is covered E2E by TestSummarizeJob_FailSummarize.
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
		t.Fatalf("Poll: want StatusFailed, got %d", status)
	}
}
