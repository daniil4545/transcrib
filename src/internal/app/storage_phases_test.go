package app

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Critical nodes of the job-phases slice (plan-job-phases-slice.md "Storage"): phase
// transitions joining->recording->processing, entry points into done for s2t (from recording) and
// mymeet (from processing), quota rollback from recording, and R2a parallel calls. Integration tests
// that hit Postgres via DATABASE_URL;
// without it they Skip (newTestStorage).

// jobStatus reads only the job status (phase tests do not look at raw/ext).
func jobStatus(t *testing.T, s *Storage, id int64) string {
	t.Helper()
	var status string
	if err := s.pool.QueryRow(context.Background(),
		`SELECT status FROM jobs WHERE id = $1`, id,
	).Scan(&status); err != nil {
		t.Fatalf("read job %d status: %v", id, err)
	}
	return status
}

// TestSetJobRecording_Phase: the joining->recording transition (provider OnCall). moved=true
// on the actual transition; a repeated call (already recording) gives moved=false (status
// guard), and the status message is not edited a second time.
func TestSetJobRecording_Phase(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/phase-rec", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.SetJobSubmitted(ctx, job.ID, "ext-phase-rec"); err != nil {
		t.Fatalf("SetJobSubmitted: %v", err)
	}
	// After submit the job is in joining (not processing: a shift in slice semantics).
	if got := jobStatus(t, s, job.ID); got != string(JobJoining) {
		t.Fatalf("status after submit = %q, expected %q", got, JobJoining)
	}

	moved, err := s.SetJobRecording(ctx, job.ID)
	if err != nil {
		t.Fatalf("SetJobRecording #1: %v", err)
	}
	if !moved {
		t.Error("first SetJobRecording: moved=false, expected true (joining->recording)")
	}
	if got := jobStatus(t, s, job.ID); got != string(JobRecording) {
		t.Errorf("status = %q, expected %q", got, JobRecording)
	}

	// Repeated tick: the job is already recording, the guard does not move it, moved=false.
	moved, err = s.SetJobRecording(ctx, job.ID)
	if err != nil {
		t.Fatalf("SetJobRecording #2: %v", err)
	}
	if moved {
		t.Error("repeated SetJobRecording: moved=true, expected false (status guard)")
	}
}

// TestSetJobProcessing_Phase: the transition into processing (provider Transcribing, mymeet).
// Accepts input from recording (the normal path) and from joining (polling skipped
// recording: a short meeting). A repeated call gives moved=false.
func TestSetJobProcessing_Phase(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)

	t.Run("from recording", func(t *testing.T) {
		userID := newTestUser(t, s)
		job, err := s.CreateJob(ctx, userID, "https://meet.google.com/phase-proc-rec", "mock", 180)
		if err != nil {
			t.Fatalf("CreateJob: %v", err)
		}
		if err := s.SetJobSubmitted(ctx, job.ID, "ext-phase-proc-rec"); err != nil {
			t.Fatalf("SetJobSubmitted: %v", err)
		}
		if _, err := s.SetJobRecording(ctx, job.ID); err != nil {
			t.Fatalf("SetJobRecording: %v", err)
		}

		moved, err := s.SetJobProcessing(ctx, job.ID)
		if err != nil {
			t.Fatalf("SetJobProcessing #1: %v", err)
		}
		if !moved {
			t.Error("SetJobProcessing from recording: moved=false, expected true")
		}
		if got := jobStatus(t, s, job.ID); got != string(JobProcessing) {
			t.Errorf("status = %q, expected %q", got, JobProcessing)
		}

		// Repeated tick: already processing, moved=false.
		moved, err = s.SetJobProcessing(ctx, job.ID)
		if err != nil {
			t.Fatalf("SetJobProcessing #2: %v", err)
		}
		if moved {
			t.Error("repeated SetJobProcessing: moved=true, expected false (status guard)")
		}
	})

	t.Run("from joining (skipped recording)", func(t *testing.T) {
		userID := newTestUser(t, s)
		job, err := s.CreateJob(ctx, userID, "https://meet.google.com/phase-proc-join", "mock", 180)
		if err != nil {
			t.Fatalf("CreateJob: %v", err)
		}
		if err := s.SetJobSubmitted(ctx, job.ID, "ext-phase-proc-join"); err != nil {
			t.Fatalf("SetJobSubmitted: %v", err)
		}
		// Without SetJobRecording: the tick skipped the recording phase, the job is still joining.
		moved, err := s.SetJobProcessing(ctx, job.ID)
		if err != nil {
			t.Fatalf("SetJobProcessing: %v", err)
		}
		if !moved {
			t.Error("SetJobProcessing from joining: moved=false, expected true (the guard includes joining)")
		}
		if got := jobStatus(t, s, job.ID); got != string(JobProcessing) {
			t.Errorf("status = %q, expected %q", got, JobProcessing)
		}
	})
}

func TestSetJobProcessing_StoresRecordingDuration(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/phase-duration", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.SetJobSubmitted(ctx, job.ID, "ext-phase-duration"); err != nil {
		t.Fatalf("SetJobSubmitted: %v", err)
	}
	if _, err := s.SetJobRecording(ctx, job.ID); err != nil {
		t.Fatalf("SetJobRecording: %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE jobs SET recording_started_at = now() - interval '7 minutes 10 seconds' WHERE id = $1`,
		job.ID,
	); err != nil {
		t.Fatalf("set recording_started_at: %v", err)
	}

	moved, err := s.SetJobProcessing(ctx, job.ID)
	if err != nil {
		t.Fatalf("SetJobProcessing: %v", err)
	}
	if !moved {
		t.Fatal("SetJobProcessing moved=false, expected true")
	}
	billed, duration := jobBilling(t, s, job.ID)
	if billed != 0 {
		t.Fatalf("billed_minutes before done = %d, expected 0", billed)
	}
	if duration == nil {
		t.Fatal("duration_seconds is nil, expected lifecycle duration")
	}
	if *duration < int((7*time.Minute).Seconds()) || *duration > int((8*time.Minute).Seconds()) {
		t.Fatalf("duration_seconds = %d, expected about 7m10s", *duration)
	}

	if err := s.SetJobDone(ctx, job.ID, []byte(`{"transcript":"ok"}`), 0, 180); err != nil {
		t.Fatalf("SetJobDone: %v", err)
	}
	billed, duration = jobBilling(t, s, job.ID)
	if billed != 8 {
		t.Fatalf("billed_minutes = %d, expected 8 (ceil by minutes)", billed)
	}
	if duration == nil {
		t.Fatal("duration_seconds after done is nil")
	}
}

// TestSetJobDone_FromRecording: the s2t path: the job goes joining->recording->done, bypassing
// processing (s2t does not report Transcribing). The SetJobDone guard includes recording, so
// finishTranscription closes the job straight from recording. The trial counter is incremented.
func TestSetJobDone_FromRecording(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/done-from-rec", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.SetJobSubmitted(ctx, job.ID, "ext-done-from-rec"); err != nil {
		t.Fatalf("SetJobSubmitted: %v", err)
	}
	if _, err := s.SetJobRecording(ctx, job.ID); err != nil {
		t.Fatalf("SetJobRecording: %v", err)
	}

	raw := []byte(`{"transcript":"s2t"}`)
	if err := s.SetJobDone(ctx, job.ID, raw, 3600, 180); err != nil {
		t.Fatalf("SetJobDone from recording: %v", err)
	}
	status, _, gotRaw := jobRow(t, s, job.ID)
	if status != string(JobDone) {
		t.Errorf("status = %q, expected %q", status, JobDone)
	}
	if !jsonEqual(gotRaw, raw) {
		t.Errorf("raw = %q, expected equivalent of %q", gotRaw, raw)
	}
	if got := trialDone(t, s, userID); got != 1 {
		t.Errorf("trial_meetings_done = %d, expected 1 (done from recording increments the counter)", got)
	}
}

// TestSetJobDone_FromProcessing: the mymeet path without a summary (SUMMARIZER_PROVIDER=off):
// the job goes joining->recording->processing->done, without summarizing. The guard includes processing.
func TestSetJobDone_FromProcessing(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/done-from-proc", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.SetJobSubmitted(ctx, job.ID, "ext-done-from-proc"); err != nil {
		t.Fatalf("SetJobSubmitted: %v", err)
	}
	if _, err := s.SetJobRecording(ctx, job.ID); err != nil {
		t.Fatalf("SetJobRecording: %v", err)
	}
	if _, err := s.SetJobProcessing(ctx, job.ID); err != nil {
		t.Fatalf("SetJobProcessing: %v", err)
	}

	if err := s.SetJobDone(ctx, job.ID, []byte(`{"transcript":"mymeet"}`), 3600, 180); err != nil {
		t.Fatalf("SetJobDone from processing: %v", err)
	}
	if got := jobStatus(t, s, job.ID); got != string(JobDone) {
		t.Errorf("status = %q, expected %q", got, JobDone)
	}
	if got := trialDone(t, s, userID); got != 1 {
		t.Errorf("trial_meetings_done = %d, expected 1", got)
	}
}

// TestSetJobDone_FromJoining: polling is discrete (30s tick) and may skip both
// phases: one tick sees connecting (job in joining), the next already sees new/done (a short
// meeting). finishTranscription then calls SetJobDone straight from joining, bypassing
// recording/processing. The SetJobDone guard is extended to joining precisely for this skip
// (plan-job-phases-slice.md "Storage", answer to review M2). The trial counter is incremented
// exactly once: the transition is one-shot under the guard.
func TestSetJobDone_FromJoining(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/done-from-join", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.SetJobSubmitted(ctx, job.ID, "ext-done-from-join"); err != nil {
		t.Fatalf("SetJobSubmitted: %v", err)
	}
	// Without SetJobRecording/SetJobProcessing: the tick skipped both phases, the job is still joining.
	if got := jobStatus(t, s, job.ID); got != string(JobJoining) {
		t.Fatalf("status before done = %q, expected %q", got, JobJoining)
	}

	raw := []byte(`{"transcript":"short"}`)
	if err := s.SetJobDone(ctx, job.ID, raw, 3600, 180); err != nil {
		t.Fatalf("SetJobDone from joining: %v", err)
	}
	status, _, gotRaw := jobRow(t, s, job.ID)
	if status != string(JobDone) {
		t.Errorf("status = %q, expected %q (done directly from joining)", status, JobDone)
	}
	if !jsonEqual(gotRaw, raw) {
		t.Errorf("raw = %q, expected equivalent of %q", gotRaw, raw)
	}
	if got := trialDone(t, s, userID); got != 1 {
		t.Errorf("trial_meetings_done = %d, expected 1 (done from joining increments the counter exactly once)", got)
	}
}

// TestFailJobWithQuotaRefund_FromRecording: §5.2: a failure/watchdog in the recording phase
// (the bot is still in the meeting) rolls back the quota reservation. The guard is extended to joining/recording,
// so a meeting that failed before the bot left returns the trial meeting to the user.
func TestFailJobWithQuotaRefund_FromRecording(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/fail-from-rec", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if got := trialUsed(t, s, userID); got != 1 {
		t.Fatalf("trial_meetings_used after reservation = %d, expected 1", got)
	}
	if err := s.SetJobSubmitted(ctx, job.ID, "ext-fail-from-rec"); err != nil {
		t.Fatalf("SetJobSubmitted: %v", err)
	}
	if _, err := s.SetJobRecording(ctx, job.ID); err != nil {
		t.Fatalf("SetJobRecording: %v", err)
	}

	refunded, err := s.FailJobWithQuotaRefund(ctx, job.ID, "bot dropped during recording")
	if err != nil {
		t.Fatalf("FailJobWithQuotaRefund: %v", err)
	}
	if !refunded {
		t.Error("refunded=false, expected true (rollback from recording)")
	}
	if got := trialUsed(t, s, userID); got != 0 {
		t.Errorf("trial_meetings_used after rollback = %d, expected 0", got)
	}
	if got := jobStatus(t, s, job.ID); got != string(JobFailed) {
		t.Errorf("status = %q, expected %q", got, JobFailed)
	}
}

// TestCreateJob_RateLimit_Phases: R2a: the phase of an active Job no longer blocks
// another link. The quota stays the monetary limiter, the same URL is cut by dedup.
func TestCreateJob_RateLimit_Phases(t *testing.T) {
	ctx := context.Background()

	t.Run("recording allows second under different url", func(t *testing.T) {
		s := newTestStorage(t)
		userID := newTestUser(t, s)

		first, err := s.CreateJob(ctx, userID, "https://meet.google.com/rl-rec-first", "mock", 180)
		if err != nil {
			t.Fatalf("CreateJob #1: %v", err)
		}
		if err := s.SetJobSubmitted(ctx, first.ID, "ext-rl-rec-first"); err != nil {
			t.Fatalf("SetJobSubmitted: %v", err)
		}
		if _, err := s.SetJobRecording(ctx, first.ID); err != nil {
			t.Fatalf("SetJobRecording: %v", err)
		}

		second, err := s.CreateJob(ctx, userID, "https://meet.google.com/rl-rec-second", "mock", 180)
		if err != nil {
			t.Fatalf("CreateJob #2 with the first in recording: %v", err)
		}
		if second.ID == 0 {
			t.Error("second Job not created while the first is in recording")
		}
	})

	t.Run("processing releases second under different url", func(t *testing.T) {
		s := newTestStorage(t)
		userID := newTestUser(t, s)

		first, err := s.CreateJob(ctx, userID, "https://meet.google.com/rl-proc-first", "mock", 180)
		if err != nil {
			t.Fatalf("CreateJob #1: %v", err)
		}
		if err := s.SetJobSubmitted(ctx, first.ID, "ext-rl-proc-first"); err != nil {
			t.Fatalf("SetJobSubmitted: %v", err)
		}
		if _, err := s.SetJobRecording(ctx, first.ID); err != nil {
			t.Fatalf("SetJobRecording: %v", err)
		}
		if _, err := s.SetJobProcessing(ctx, first.ID); err != nil {
			t.Fatalf("SetJobProcessing: %v", err)
		}

		// First in processing (the bot has left): the second under a different URL passes.
		second, err := s.CreateJob(ctx, userID, "https://meet.google.com/rl-proc-second", "mock", 180)
		if err != nil {
			t.Errorf("CreateJob #2 with the first in processing: %v, expected success (§5.8 does not hold)", err)
		}
		if second.ID == 0 {
			t.Error("second job not created while the first is in processing")
		}
	})

	t.Run("same url cut by dedup even in processing", func(t *testing.T) {
		s := newTestStorage(t)
		userID := newTestUser(t, s)

		const url = "https://meet.google.com/rl-dedup-same"
		first, err := s.CreateJob(ctx, userID, url, "mock", 180)
		if err != nil {
			t.Fatalf("CreateJob #1: %v", err)
		}
		if err := s.SetJobSubmitted(ctx, first.ID, "ext-rl-dedup-same"); err != nil {
			t.Fatalf("SetJobSubmitted: %v", err)
		}
		if _, err := s.SetJobRecording(ctx, first.ID); err != nil {
			t.Fatalf("SetJobRecording: %v", err)
		}
		if _, err := s.SetJobProcessing(ctx, first.ID); err != nil {
			t.Fatalf("SetJobProcessing: %v", err)
		}

		_, err = s.CreateJob(ctx, userID, url, "mock", 180)
		if !errors.Is(err, ErrDuplicateActiveJob) {
			t.Errorf("CreateJob with the same URL: err = %v, expected ErrDuplicateActiveJob", err)
		}
	})
}
