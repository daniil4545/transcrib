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

// jsonEqual сравнивает два JSON семантически через распарсенные значения:
// JSONB канонизирует пробелы И переупорядочивает ключи объекта, поэтому
// побайтовое/Compact-сравнение ложно падает на многоключевом литерале.
// reflect.DeepEqual по распарсенному дереву нечувствителен к порядку ключей.
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

// Интеграционные тесты критических узлов (docs/testing.md: state machine, очередь).
// Требуют поднятого Postgres: DATABASE_URL берётся из окружения (make test
// подхватывает .env автоматически). Без DATABASE_URL тесты пропускаются —
// голый go test работает без инфраструктуры.

func newTestStorage(t *testing.T) *Storage {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL не задан — интеграционный тест пропущен")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect postgres: %v", err)
	}
	t.Cleanup(pool.Close)
	return NewStorage(pool)
}

// newTestUser создаёт юзера с уникальным отрицательным telegram_id: реальные
// телеграмные id положительные, поэтому тестовые строки не пересекаются с
// dev-данными. Cleanup подчищает сначала jobs (FK RESTRICT), потом юзера.
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

// jobRow читает строку джобы напрямую из БД для проверки результата перехода.
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

// mustClaim возвращает множество заклеймленных id: в dev-БД могут лежать чужие
// джобы (claim лишь bump'ает их updated_at, статус не трогает), поэтому тест
// проверяет только вхождение своих.
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

// TestClaimJobsForWork_Backoff — критический узел: created клеймится всегда,
// processing — только когда updated_at старше pollInterval, и сам claim
// сдвигает updated_at (без bump джоба поллилась бы каждым тиком).
func TestClaimJobsForWork_Backoff(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	// Разные юзеры упрощают проверку очереди: каждая Job живёт в независимой квоте.
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

	// Свежая processing (updated_at = now) под бэкофф не попадает, created — всегда.
	got := mustClaim(t, s, time.Minute)
	if !got[created.ID] {
		t.Error("created-джоба не заклеймлена")
	}
	if got[processing.ID] {
		t.Error("processing-джоба заклеймлена до истечения pollInterval")
	}

	// Состариваем processing — теперь должна клеймиться.
	if _, err := s.pool.Exec(ctx,
		`UPDATE jobs SET updated_at = now() - interval '2 minutes' WHERE id = $1`,
		processing.ID); err != nil {
		t.Fatalf("backdate job: %v", err)
	}
	got = mustClaim(t, s, time.Minute)
	if !got[processing.ID] {
		t.Error("processing-джоба не заклеймлена после истечения pollInterval")
	}

	// Claim сам bump'нул updated_at — повторный claim её уже не берёт.
	got = mustClaim(t, s, time.Minute)
	if got[processing.ID] {
		t.Error("claim не сдвинул updated_at: processing-джоба заклеймлена повторно")
	}
}

// TestClaimJobsForWork_SummarizingBackoff — критический узел: summarizing ведёт
// себя как processing (бэкофф), created по-прежнему клеймится без бэкоффа.
// Регрессия: переход с `= $2` на `= ANY($2)` не должен сломать created-ветку.
func TestClaimJobsForWork_SummarizingBackoff(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	// Две активные Job у разных юзеров.
	createdUser := newTestUser(t, s)
	sumUser := newTestUser(t, s)

	// Джоба created — клеймится без бэкоффа.
	created, err := s.CreateJob(ctx, createdUser, "https://meet.google.com/test-sum-a", "mock", 180)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	// Джоба, переведённая в summarizing прямо сейчас (свежая).
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

	// Свежая summarizing не клеймится; created — клеймится.
	got := mustClaim(t, s, time.Minute)
	if !got[created.ID] {
		t.Error("created-джоба не заклеймлена (регрессия ANY($2))")
	}
	if got[sumJob.ID] {
		t.Error("summarizing-джоба заклеймлена до истечения pollInterval")
	}

	// Состариваем summarizing — теперь должна клеймиться.
	if _, err := s.pool.Exec(ctx,
		`UPDATE jobs SET updated_at = now() - interval '2 minutes' WHERE id = $1`,
		sumJob.ID); err != nil {
		t.Fatalf("backdate job: %v", err)
	}
	got = mustClaim(t, s, time.Minute)
	if !got[sumJob.ID] {
		t.Error("summarizing-джоба не заклеймлена после истечения pollInterval")
	}

	// Claim сдвинул updated_at — повторный claim не берёт.
	got = mustClaim(t, s, time.Minute)
	if got[sumJob.ID] {
		t.Error("claim не сдвинул updated_at для summarizing: заклеймлена повторно")
	}
}

// TestClaimJobsForWork_FailedSummaryExcluded — failed_summary не возвращается
// ClaimJobsForWork (исключён частичным индексом jobs_worker_idx).
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
	if err := s.SetJobFailedSummary(ctx, j.ID, "claude timeout", 180); err != nil {
		t.Fatalf("set failed_summary: %v", err)
	}

	// Даже состаренная failed_summary не должна клеймиться.
	if _, err := s.pool.Exec(ctx,
		`UPDATE jobs SET updated_at = now() - interval '1 hour' WHERE id = $1`,
		j.ID); err != nil {
		t.Fatalf("backdate job: %v", err)
	}
	got := mustClaim(t, s, time.Millisecond)
	if got[j.ID] {
		t.Error("failed_summary-джоба попала в claim: должна быть исключена")
	}
}

// TestClaimJobsForWork_NoDoubleClaim — критический узел: FOR UPDATE SKIP LOCKED
// не должен выдать один Job двум воркерам (docs/testing.md / db.md §5). Засеваем N
// состаренных processing-джоб, запускаем K горутин, одновременно бьющих
// ClaimJobsForWork маленьким limit'ом. Claim bump'ает updated_at в now(), а
// pollInterval взят большим (1ч) — поэтому раз заклеймленная джоба выпадает из
// окна бэкоффа и не может прийти повторно. Инвариант: ни один id не выдан дважды.
// Гонять под -race.
func TestClaimJobsForWork_NoDoubleClaim(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)

	const (
		jobCount   = 20
		goroutines = 8
		limit      = 3 // < jobCount: ни одна горутина не выгребает всё разом, лок реально проверяется
	)

	// Каждая из jobCount активных processing-джоб создаётся под отдельным юзером,
	// чтобы не расходовать общий trial одного пользователя.
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
	// Состариваем наши processing-джобы по их id, чтобы они проходили бэкофф-фильтр.
	if _, err := s.pool.Exec(ctx,
		`UPDATE jobs SET updated_at = now() - interval '1 hour' WHERE id = ANY($1)`,
		ids); err != nil {
		t.Fatalf("backdate jobs: %v", err)
	}

	var (
		mu     sync.Mutex
		counts = make(map[int64]int) // id -> сколько раз заклеймлен (только наши)
		start  = make(chan struct{})
		wg     sync.WaitGroup
	)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // синхронный старт: повышаем шанс реального наложения транзакций
			// Каждая горутина крутит claim, пока есть что брать. pollInterval=1ч:
			// единожды заклеймленная (updated_at -> now()) джоба больше не вернётся.
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
				// Чужие dev-джобы возможны (claim лишь bump'ает их), но наши
				// кончились — выходим, чтобы не крутить вечный цикл на чужих.
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
			t.Errorf("job %d заклеймлен %d раз — двойной захват под SKIP LOCKED", id, n)
		}
	}
	// Санити: все засеянные джобы должны быть выбраны ровно по разу
	// (иначе тест зелёный по тривиальной причине «никто ничего не взял»).
	if len(counts) != jobCount && doubled == 0 {
		t.Errorf("заклеймлено уникальных наших джоб %d, ожидали %d", len(counts), jobCount)
	}
}

// TestSetJobSummarizing — переход processing -> summarizing сохраняет raw.
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

// TestSetJobFailedSummary — переход summarizing -> failed_summary пишет last_error.
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

	// jobRow не читает last_error — проверяем напрямую.
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
