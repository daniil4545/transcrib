package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// jsonEqual compares two JSON values semantically via parsed values:
// JSONB canonicalizes whitespace AND reorders object keys, so a
// byte-wise/Compact comparison falsely fails on a multi-key literal.
// reflect.DeepEqual over the parsed tree is insensitive to key order.
func jsonEqual(a, b []byte) bool {
	var va, vb any
	if err := json.Unmarshal(a, &va); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &vb); err != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}

// Integration tests of critical nodes (docs/testing.md: state machine, queue).
// They require a running Postgres: DATABASE_URL is taken from the environment (make test
// picks up the env file automatically). Without DATABASE_URL the tests are skipped:
// a bare go test works without infrastructure.

func newTestStorage(t *testing.T) *Storage {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL is not set: integration test skipped")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect postgres: %v", err)
	}
	t.Cleanup(pool.Close)
	return NewStorage(pool)
}

// newTestUser creates a user with a unique negative telegram_id: real
// Telegram ids are positive, so test rows do not collide with
// dev data. Cleanup removes jobs first (FK RESTRICT), then the user.
func newTestUser(t *testing.T, s *Storage) int64 {
	t.Helper()
	id := -time.Now().UnixNano()
	if _, err := s.GetOrCreateUser(context.Background(), id); err != nil {
		t.Fatalf("create test user: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		if _, err := s.pool.Exec(ctx, `DELETE FROM analytics_events WHERE user_id = $1`, id); err != nil {
			t.Logf("cleanup analytics events: %v", err)
		}
		if _, err := s.pool.Exec(ctx, `DELETE FROM jobs WHERE user_id = $1`, id); err != nil {
			t.Logf("cleanup jobs: %v", err)
		}
		if _, err := s.pool.Exec(ctx, `DELETE FROM recurring_profiles WHERE user_id = $1`, id); err != nil {
			t.Logf("cleanup recurring profiles: %v", err)
		}
		if _, err := s.pool.Exec(ctx, `DELETE FROM users WHERE telegram_id = $1`, id); err != nil {
			t.Logf("cleanup user: %v", err)
		}
	})
	return id
}

// jobRow reads a job row directly from the DB to verify the result of a transition.
func jobRow(t *testing.T, s *Storage, id int64) (status string, extID *string, raw []byte) {
	t.Helper()
	err := s.pool.QueryRow(context.Background(),
		`SELECT status, external_job_id, raw_provider_response FROM jobs WHERE id = $1`, id,
	).Scan(&status, &extID, &raw)
	if err != nil {
		t.Fatalf("read job %d: %v", id, err)
	}
	return status, extID, raw
}

// mustClaim returns the set of claimed ids: the dev DB may hold foreign
// jobs (claim only bumps their updated_at and leaves the status alone), so the test
// checks only that its own jobs are included.
func mustClaim(t *testing.T, s *Storage, interval time.Duration) map[int64]bool {
	t.Helper()
	jobs, err := s.ClaimJobsForWork(context.Background(), interval, 500)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	ids := make(map[int64]bool, len(jobs))
	for _, j := range jobs {
		ids[j.ID] = true
	}
	return ids
}

// TestClaimJobsForWork_Backoff is a critical node: created is always claimed,
// processing only when updated_at is older than pollInterval, and the claim itself
// shifts updated_at (without the bump the job would be polled on every tick).
func TestClaimJobsForWork_Backoff(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	// Different users simplify the queue check: each Job lives in an independent quota.
	createdUser := newTestUser(t, s)
	processingUser := newTestUser(t, s)

	created, err := s.CreateJob(ctx, createdUser, "https://meet.google.com/test-claim-a", "mock", 180)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	processing, err := s.CreateJob(ctx, processingUser, "https://meet.google.com/test-claim-b", "mock", 180)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := s.SetJobSubmitted(ctx, processing.ID, "ext-claim-b"); err != nil {
		t.Fatalf("submit job: %v", err)
	}

	// A fresh processing job (updated_at = now) is not subject to backoff, created always is.
	got := mustClaim(t, s, time.Minute)
	if !got[created.ID] {
		t.Error("created job was not claimed")
	}
	if got[processing.ID] {
		t.Error("processing job was claimed before pollInterval elapsed")
	}

	// Age the processing job: now it must be claimed.
	if _, err := s.pool.Exec(ctx,
		`UPDATE jobs SET updated_at = now() - interval '2 minutes' WHERE id = $1`,
		processing.ID); err != nil {
		t.Fatalf("backdate job: %v", err)
	}
	got = mustClaim(t, s, time.Minute)
	if !got[processing.ID] {
		t.Error("processing job was not claimed after pollInterval elapsed")
	}

	// The claim itself bumped updated_at, so a repeated claim does not take it.
	got = mustClaim(t, s, time.Minute)
	if got[processing.ID] {
		t.Error("claim did not shift updated_at: processing job was claimed again")
	}
}

// TestClaimJobsForWork_SummarizingBackoff is a critical node: summarizing behaves
// like processing (backoff), created is still claimed without backoff.
// Regression: changing `= $2` to `= ANY($2)` must not break the created branch.
func TestClaimJobsForWork_SummarizingBackoff(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	// Two active Jobs for different users.
	createdUser := newTestUser(t, s)
	sumUser := newTestUser(t, s)

	// A created job is claimed without backoff.
	created, err := s.CreateJob(ctx, createdUser, "https://meet.google.com/test-sum-a", "mock", 180)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	// A job moved to summarizing right now (fresh).
	sumJob, err := s.CreateJob(ctx, sumUser, "https://meet.google.com/test-sum-b", "mock", 180)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := s.SetJobSubmitted(ctx, sumJob.ID, "ext-sum-b"); err != nil {
		t.Fatalf("submit job: %v", err)
	}
	raw := []byte(`{"status":"done"}`)
	if err := s.SetJobSummarizing(ctx, sumJob.ID, raw, 3600); err != nil {
		t.Fatalf("set summarizing: %v", err)
	}

	// A fresh summarizing job is not claimed; created is.
	got := mustClaim(t, s, time.Minute)
	if !got[created.ID] {
		t.Error("created job was not claimed (ANY($2) regression)")
	}
	if got[sumJob.ID] {
		t.Error("summarizing job was claimed before pollInterval elapsed")
	}

	// Age the summarizing job: now it must be claimed.
	if _, err := s.pool.Exec(ctx,
		`UPDATE jobs SET updated_at = now() - interval '2 minutes' WHERE id = $1`,
		sumJob.ID); err != nil {
		t.Fatalf("backdate job: %v", err)
	}
	got = mustClaim(t, s, time.Minute)
	if !got[sumJob.ID] {
		t.Error("summarizing job was not claimed after pollInterval elapsed")
	}

	// The claim shifted updated_at, so a repeated claim does not take it.
	got = mustClaim(t, s, time.Minute)
	if got[sumJob.ID] {
		t.Error("claim did not shift updated_at for summarizing: claimed again")
	}
}

// TestClaimJobsForWork_FailedSummaryExcluded: failed_summary is not returned by
// ClaimJobsForWork (excluded by the partial index jobs_worker_idx).
func TestClaimJobsForWork_FailedSummaryExcluded(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	j, err := s.CreateJob(ctx, userID, "https://meet.google.com/test-sum-c", "mock", 180)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := s.SetJobSubmitted(ctx, j.ID, "ext-sum-c"); err != nil {
		t.Fatalf("submit job: %v", err)
	}
	if err := s.SetJobSummarizing(ctx, j.ID, []byte(`{"status":"done"}`), 3600); err != nil {
		t.Fatalf("set summarizing: %v", err)
	}
	if err := s.SetJobFailedSummary(ctx, j.ID, "summarizer timeout", 180); err != nil {
		t.Fatalf("set failed_summary: %v", err)
	}

	// Even an aged failed_summary must not be claimed.
	if _, err := s.pool.Exec(ctx,
		`UPDATE jobs SET updated_at = now() - interval '1 hour' WHERE id = $1`,
		j.ID); err != nil {
		t.Fatalf("backdate job: %v", err)
	}
	got := mustClaim(t, s, time.Millisecond)
	if got[j.ID] {
		t.Error("failed_summary job was claimed: it must be excluded")
	}
}

// TestClaimJobsForWork_NoDoubleClaim is a critical node: FOR UPDATE SKIP LOCKED
// must not hand one Job to two workers (docs/testing.md / db.md §5). We seed N
// aged processing jobs and start K goroutines that hit
// ClaimJobsForWork simultaneously with a small limit. Claim bumps updated_at to now(), and
// pollInterval is large (1h), so a job claimed once drops out of the
// backoff window and cannot come back. Invariant: no id is handed out twice.
// Run under -race.
func TestClaimJobsForWork_NoDoubleClaim(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)

	const (
		jobCount   = 20
		goroutines = 8
		limit      = 3 // < jobCount: no goroutine drains everything at once, so the lock is really exercised
	)

	// Each of the jobCount active processing jobs is created under a separate user,
	// so as not to spend one user's shared trial.
	seeded := make(map[int64]bool, jobCount)
	ids := make([]int64, 0, jobCount)
	for i := 0; i < jobCount; i++ {
		userID := newTestUser(t, s)
		j, err := s.CreateJob(ctx, userID, fmt.Sprintf("https://meet.google.com/race-%d", i), "mock", 180)
		if err != nil {
			t.Fatalf("create job %d: %v", i, err)
		}
		if err := s.SetJobSubmitted(ctx, j.ID, fmt.Sprintf("ext-race-%d", i)); err != nil {
			t.Fatalf("submit job %d: %v", i, err)
		}
		seeded[j.ID] = true
		ids = append(ids, j.ID)
	}
	// Age our processing jobs by id so that they pass the backoff filter.
	if _, err := s.pool.Exec(ctx,
		`UPDATE jobs SET updated_at = now() - interval '1 hour' WHERE id = ANY($1)`,
		ids); err != nil {
		t.Fatalf("backdate jobs: %v", err)
	}

	var (
		mu     sync.Mutex
		counts = make(map[int64]int) // id -> how many times claimed (ours only)
		start  = make(chan struct{})
		wg     sync.WaitGroup
	)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // synchronous start: raises the chance of real transaction overlap
			// Each goroutine loops claim while there is something to take. pollInterval=1h:
			// a job claimed once (updated_at -> now()) never comes back.
			for {
				jobs, err := s.ClaimJobsForWork(ctx, time.Hour, limit)
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				mine := 0
				mu.Lock()
				for _, j := range jobs {
					if seeded[j.ID] {
						counts[j.ID]++
						mine++
					}
				}
				mu.Unlock()
				// Foreign dev jobs are possible (claim only bumps them), but ours
				// are exhausted: exit so as not to spin forever on foreign ones.
				if mine == 0 {
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	doubled := 0
	for id, n := range counts {
		if n > 1 {
			doubled++
			t.Errorf("job %d claimed %d times: double claim under SKIP LOCKED", id, n)
		}
	}
	// Sanity: every seeded job must be picked exactly once
	// (otherwise the test is green for the trivial reason "nobody took anything").
	if len(counts) != jobCount && doubled == 0 {
		t.Errorf("claimed %d unique own jobs, expected %d", len(counts), jobCount)
	}
}

// TestSetJobSummarizing: the processing -> summarizing transition keeps raw.
func TestSetJobSummarizing(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	j, err := s.CreateJob(ctx, userID, "https://meet.google.com/test-sum-d", "mock", 180)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := s.SetJobSubmitted(ctx, j.ID, "ext-sum-d"); err != nil {
		t.Fatalf("submit job: %v", err)
	}
	raw := []byte(`{"transcript":"hello world"}`)
	if err := s.SetJobSummarizing(ctx, j.ID, raw, 3600); err != nil {
		t.Fatalf("SetJobSummarizing: %v", err)
	}

	status, _, gotRaw := jobRow(t, s, j.ID)
	if status != string(JobSummarizing) {
		t.Errorf("status = %q, want %q", status, JobSummarizing)
	}
	if !jsonEqual(gotRaw, raw) {
		t.Errorf("raw = %q, want equivalent %q", gotRaw, raw)
	}
}

// TestSetJobFailedSummary: the summarizing -> failed_summary transition writes last_error.
func TestSetJobFailedSummary(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	j, err := s.CreateJob(ctx, userID, "https://meet.google.com/test-sum-e", "mock", 180)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := s.SetJobSubmitted(ctx, j.ID, "ext-sum-e"); err != nil {
		t.Fatalf("submit job: %v", err)
	}
	if err := s.SetJobSummarizing(ctx, j.ID, []byte(`{"status":"done"}`), 3600); err != nil {
		t.Fatalf("set summarizing: %v", err)
	}
	reason := "claude api timeout"
	if err := s.SetJobFailedSummary(ctx, j.ID, reason, 180); err != nil {
		t.Fatalf("SetJobFailedSummary: %v", err)
	}

	// jobRow does not read last_error, so we check it directly.
	var status, lastErr string
	err = s.pool.QueryRow(ctx,
		`SELECT status, last_error FROM jobs WHERE id = $1`, j.ID,
	).Scan(&status, &lastErr)
	if err != nil {
		t.Fatalf("read job: %v", err)
	}
	if status != string(JobFailedSummary) {
		t.Errorf("status = %q, want %q", status, JobFailedSummary)
	}
	if lastErr != reason {
		t.Errorf("last_error = %q, want %q", lastErr, reason)
	}
}

func TestSetTerminalJobSummary_FailedSummaryToDoneNoRebilling(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	j, err := s.CreateJob(ctx, userID, "https://meet.google.com/test-terminal-summary", "mock", 180)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := s.SetJobSubmitted(ctx, j.ID, "ext-terminal-summary"); err != nil {
		t.Fatalf("submit job: %v", err)
	}
	if err := s.SetJobSummarizing(ctx, j.ID, []byte(`{"status":"done"}`), 3600); err != nil {
		t.Fatalf("set summarizing: %v", err)
	}
	if err := s.SetJobFailedSummary(ctx, j.ID, "summary failed", 180); err != nil {
		t.Fatalf("set failed_summary: %v", err)
	}

	beforeBilled, beforeReserved, beforeTrialDone := terminalSummaryCounters(t, s, j.ID, userID)
	if err := s.SetTerminalJobSummary(ctx, j.ID, "восстановленный конспект"); err != nil {
		t.Fatalf("SetTerminalJobSummary: %v", err)
	}
	afterBilled, afterReserved, afterTrialDone := terminalSummaryCounters(t, s, j.ID, userID)
	if beforeBilled != afterBilled || beforeReserved != afterReserved || beforeTrialDone != afterTrialDone {
		t.Fatalf("counters changed: before billed/reserved/trial_done=%d/%d/%d after=%d/%d/%d",
			beforeBilled, beforeReserved, beforeTrialDone, afterBilled, afterReserved, afterTrialDone)
	}

	var status, summary string
	var lastErr *string
	err = s.pool.QueryRow(ctx,
		`SELECT status, summary_md, last_error FROM jobs WHERE id = $1`, j.ID,
	).Scan(&status, &summary, &lastErr)
	if err != nil {
		t.Fatalf("read job: %v", err)
	}
	if status != string(JobDone) {
		t.Fatalf("status = %q, want done", status)
	}
	if summary != "восстановленный конспект" {
		t.Fatalf("summary_md = %q", summary)
	}
	if lastErr != nil {
		t.Fatalf("last_error = %v, want nil", *lastErr)
	}
}

func TestSetTerminalJobSummary_DoneStoresSummary(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	j, err := s.CreateJob(ctx, userID, "https://meet.google.com/test-terminal-summary-done", "mock", 180)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := s.SetJobSubmitted(ctx, j.ID, "ext-terminal-summary-done"); err != nil {
		t.Fatalf("submit job: %v", err)
	}
	if err := s.SetJobSummarizing(ctx, j.ID, []byte(`{"status":"done"}`), 3600); err != nil {
		t.Fatalf("set summarizing: %v", err)
	}
	if err := s.SetJobDone(ctx, j.ID, nil, 3600, 180); err != nil {
		t.Fatalf("set done: %v", err)
	}

	if err := s.SetTerminalJobSummary(ctx, j.ID, "поздний конспект"); err != nil {
		t.Fatalf("SetTerminalJobSummary: %v", err)
	}
	var status, summary string
	err = s.pool.QueryRow(ctx,
		`SELECT status, summary_md FROM jobs WHERE id = $1`, j.ID,
	).Scan(&status, &summary)
	if err != nil {
		t.Fatalf("read job: %v", err)
	}
	if status != string(JobDone) {
		t.Fatalf("status = %q, want done", status)
	}
	if summary != "поздний конспект" {
		t.Fatalf("summary_md = %q", summary)
	}
}

func TestSetTerminalJobSummary_IgnoresNonTerminal(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	j, err := s.CreateJob(ctx, userID, "https://meet.google.com/test-terminal-summary-created", "mock", 180)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := s.SetTerminalJobSummary(ctx, j.ID, "не должен сохраниться"); err != nil {
		t.Fatalf("SetTerminalJobSummary: %v", err)
	}
	reloaded := reloadJob(t, s, j.ID)
	if reloaded.Status != JobCreated {
		t.Fatalf("status = %q, want created", reloaded.Status)
	}
	if reloaded.SummaryMD != nil {
		t.Fatalf("summary_md = %v, want nil", *reloaded.SummaryMD)
	}
}

func terminalSummaryCounters(t *testing.T, s *Storage, jobID, userID int64) (billed, reserved, trialDone int) {
	t.Helper()
	err := s.pool.QueryRow(context.Background(),
		`SELECT billed_minutes, reserved_minutes FROM jobs WHERE id = $1`, jobID,
	).Scan(&billed, &reserved)
	if err != nil {
		t.Fatalf("read job counters: %v", err)
	}
	err = s.pool.QueryRow(context.Background(),
		`SELECT trial_minutes_done FROM users WHERE telegram_id = $1`, userID,
	).Scan(&trialDone)
	if err != nil {
		t.Fatalf("read user counters: %v", err)
	}
	return billed, reserved, trialDone
}
