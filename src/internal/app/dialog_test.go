package app

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestDialog_MockHappyPath drives one job through the whole state machine with the mock
// provider and the mock summarizer and prints the chat as the user would see it. The README
// "Demo" block is the output of this test (go test -run TestDialog -v). It needs no external
// keys: the provider, the summarizer, the status editor and the delivery are in-memory fakes.
func TestDialog_MockHappyPath(t *testing.T) {
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	ctx := context.Background()

	const link = "https://meet.google.com/abc-defg-hij"
	job, err := s.CreateJob(ctx, userID, link, "mock", 180)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	// The status message the bot sends right after accepting the link; the orchestrator
	// edits this same message by phases.
	const statusMsgID = 1001
	if err := s.SetJobStatusMessage(ctx, job.ID, statusMsgID); err != nil {
		t.Fatalf("set status message: %v", err)
	}

	var edits []string
	edit := func(_, _ int64, text string) error { edits = append(edits, text); return nil }
	notify := func(int64, string) error { return nil }
	delivery := &recordingDelivery{}
	o := NewOrchestrator(s, &MockProvider{Phased: true}, delivery, NewMockSummarizer(),
		notify, edit, time.Millisecond, time.Hour, 10*time.Minute, 3, 180)

	// One advance is one orchestrator tick for this job; the loop stops on a terminal status.
	for i := 0; i < 20; i++ {
		var cur Job
		err := s.pool.QueryRow(ctx,
			`SELECT id, user_id, meeting_url, status, external_job_id, provider, created_at,
			        status_message_id, summary_md, duration_seconds, last_error
			 FROM jobs WHERE id = $1`, job.ID,
		).Scan(&cur.ID, &cur.UserID, &cur.MeetingURL, &cur.Status, &cur.ExternalJobID, &cur.Provider,
			&cur.CreatedAt, &cur.StatusMessageID, &cur.SummaryMD, &cur.DurationSeconds, &cur.LastError)
		if err != nil {
			t.Fatalf("load job: %v", err)
		}
		if cur.Status == JobDone || cur.Status == JobFailed {
			break
		}
		o.advance(ctx, cur)
	}

	if status, _, _ := jobRow(t, s, job.ID); status != string(JobDone) {
		t.Fatalf("status %q, want done", status)
	}
	wantEdits := []string{
		"Бот подключается к встрече.",
		"Бот уже на встрече и записывает разговор.",
		"Запись завершилась. Расшифровываю разговор.",
		"Расшифровка готова. Готовлю краткий конспект.",
		"Готово. Расшифровка и конспект ниже.",
	}
	if strings.Join(edits, "|") != strings.Join(wantEdits, "|") {
		t.Fatalf("status edits:\n got %q\nwant %q", edits, wantEdits)
	}
	if len(delivery.groups) != 1 || len(delivery.groups[0].docs) != 2 {
		t.Fatalf("want one group of two files, got %+v", delivery.groups)
	}

	// The chat as the user sees it. Only file names and headings are printed: the
	// body of the mock transcript is a fixture, not the point of the demo.
	var chat strings.Builder
	fmt.Fprintf(&chat, "\nuser: %s\n", link)
	fmt.Fprintf(&chat, "bot:  %s\n", acceptedText(link))
	for _, e := range edits {
		fmt.Fprintf(&chat, "bot:  (edit) %s\n", e)
	}
	for _, d := range delivery.groups[0].docs {
		heading, _, _ := strings.Cut(d.content, "\n")
		fmt.Fprintf(&chat, "bot:  file %s (%s)\n", d.filename, heading)
	}
	t.Log(chat.String())
}
