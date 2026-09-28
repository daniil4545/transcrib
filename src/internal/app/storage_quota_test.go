package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// Критические денежные узлы (db.md §5.1 резерв, §5.2 откат, §5.7 done-счётчик,
// §5.8 parallel calls; docs/testing.md). Точечные тесты на инварианты денег/квоты/гонки/
// идемпотентности — happy-path клея сюда не входит (ручной E2E). Интеграционные,
// бьют в настоящий Postgres через DATABASE_URL; без него — Skip (см. newTestStorage).

// insertSubscription вставляет активную строку подписки прямым INSERT (модель 1:1,
// db.md §3.3). Создание подписки через successful_payment (§5.4) — отдельный узел,
// здесь не тестируется; нам нужна лишь готовая платная квота для проверки резерва.
// Cleanup не нужен отдельно: subscriptions FK ON DELETE CASCADE с users, а
// newTestUser уже удаляет jobs (ON DELETE RESTRICT) до user — каскад срабатывает.
func insertSubscription(t *testing.T, s *Storage, userID int64, balance, limit int) int64 {
	t.Helper()
	var id int64
	err := s.pool.QueryRow(context.Background(),
		`INSERT INTO subscriptions (
		     user_id, paid_at, expires_at,
		     meetings_limit, meetings_balance, minutes_limit, minutes_balance
		 )
		 VALUES ($1, now(), now() + interval '30 days', $2, $3, $4, $5)
		 RETURNING id`,
		userID, limit, balance, limit*60, balance*60,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert subscription: %v", err)
	}
	return id
}

func TestHasActiveSubscription_IgnoresMeetingsBalance(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	active, err := s.HasActiveSubscription(ctx, userID)
	if err != nil {
		t.Fatalf("has active subscription without sub: %v", err)
	}
	if active {
		t.Fatal("active subscription = true без подписки")
	}

	insertSubscription(t, s, userID, 0, 15)
	active, err = s.HasActiveSubscription(ctx, userID)
	if err != nil {
		t.Fatalf("has active subscription with empty balance: %v", err)
	}
	if !active {
		t.Fatal("active subscription = false для оплаченного периода с нулевым балансом")
	}
}

// trialUsed читает users.trial_meetings_used напрямую.
func trialUsed(t *testing.T, s *Storage, userID int64) int {
	t.Helper()
	var used int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT trial_meetings_used FROM users WHERE telegram_id = $1`, userID,
	).Scan(&used); err != nil {
		t.Fatalf("read trial_meetings_used: %v", err)
	}
	return used
}

func trialMinutesUsed(t *testing.T, s *Storage, userID int64) int {
	t.Helper()
	var used int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT trial_minutes_used FROM users WHERE telegram_id = $1`, userID,
	).Scan(&used); err != nil {
		t.Fatalf("read trial_minutes_used: %v", err)
	}
	return used
}

func trialMinutesDone(t *testing.T, s *Storage, userID int64) int {
	t.Helper()
	var done int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT trial_minutes_done FROM users WHERE telegram_id = $1`, userID,
	).Scan(&done); err != nil {
		t.Fatalf("read trial_minutes_done: %v", err)
	}
	return done
}

// trialDone читает users.trial_meetings_done напрямую.
func trialDone(t *testing.T, s *Storage, userID int64) int {
	t.Helper()
	var done int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT trial_meetings_done FROM users WHERE telegram_id = $1`, userID,
	).Scan(&done); err != nil {
		t.Fatalf("read trial_meetings_done: %v", err)
	}
	return done
}

// subBalance читает meetings_balance подписки напрямую.
func subBalance(t *testing.T, s *Storage, subID int64) int {
	t.Helper()
	var bal int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT meetings_balance FROM subscriptions WHERE id = $1`, subID,
	).Scan(&bal); err != nil {
		t.Fatalf("read meetings_balance: %v", err)
	}
	return bal
}

func subMinutesBalance(t *testing.T, s *Storage, subID int64) int {
	t.Helper()
	var bal int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT minutes_balance FROM subscriptions WHERE id = $1`, subID,
	).Scan(&bal); err != nil {
		t.Fatalf("read minutes_balance: %v", err)
	}
	return bal
}

// setTrialUsed выставляет стартовый расход триала (резерв уже якобы сделан ранее),
// чтобы поставить юзера на границу лимита без прогона лишних CreateJob.
func setTrialUsed(t *testing.T, s *Storage, userID, used int64) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE users
		 SET trial_meetings_used = $2,
		     trial_minutes_used = $2 * 60
		 WHERE telegram_id = $1`,
		userID, used,
	); err != nil {
		t.Fatalf("set trial_meetings_used: %v", err)
	}
}

func jobReservedMinutes(t *testing.T, s *Storage, jobID int64) int {
	t.Helper()
	var reserved int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT reserved_minutes FROM jobs WHERE id = $1`, jobID,
	).Scan(&reserved); err != nil {
		t.Fatalf("read reserved_minutes: %v", err)
	}
	return reserved
}

func jobBilling(t *testing.T, s *Storage, jobID int64) (billed int, duration *int) {
	t.Helper()
	if err := s.pool.QueryRow(context.Background(),
		`SELECT billed_minutes, duration_seconds FROM jobs WHERE id = $1`, jobID,
	).Scan(&billed, &duration); err != nil {
		t.Fatalf("read job billing: %v", err)
	}
	return billed, duration
}

// jobQuota читает is_trial/subscription_id джобы для проверки направления резерва.
func jobQuota(t *testing.T, s *Storage, jobID int64) (isTrial bool, subID *int64) {
	t.Helper()
	if err := s.pool.QueryRow(context.Background(),
		`SELECT is_trial, subscription_id FROM jobs WHERE id = $1`, jobID,
	).Scan(&isTrial, &subID); err != nil {
		t.Fatalf("read job quota: %v", err)
	}
	return isTrial, subID
}

func TestBilledMinutes(t *testing.T) {
	cases := []struct {
		seconds int
		want    int
	}{
		{-1, 0},
		{0, 0},
		{1, 1},
		{59, 1},
		{60, 1},
		{61, 2},
		{119, 2},
		{120, 2},
	}
	for _, tc := range cases {
		if got := billedMinutes(tc.seconds); got != tc.want {
			t.Errorf("billedMinutes(%d) = %d, ожидали %d", tc.seconds, got, tc.want)
		}
	}
}

// TestCreateJob_TrialRace — резерв триала §5.1 под гонкой «два сообщения за 200мс».
// Юзер на границе: trialLimit=2, used=1 — остался ровно ОДИН слот. K горутин бьют
// CreateJob с синхронным стартом. Инвариант денег: ровно одна Job создаётся, все
// прочие получают ErrNoQuota, и trial_meetings_used == limit (не больше — атомарный
// UPDATE ... WHERE trial_meetings_used < limit физически закрывает гонку, иначе
// счётчик перевалил бы за лимит = выданная сверх лимита бесплатная встреча).
// Под -race. docs/testing.md / db.md §5.1.
func TestCreateJob_TrialRace(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	const (
		trialLimit = 2
		goroutines = 8
	)
	setTrialUsed(t, s, userID, trialLimit-1) // остался один слот

	var (
		mu        sync.Mutex
		okCount   int
		noQuota   int
		otherErrs []error
		start     = make(chan struct{})
		wg        sync.WaitGroup
	)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			<-start
			// Разные meeting_url: dedup-индекс не должен маскировать гонку резерва.
			_, err := s.CreateJob(ctx, userID, fmt.Sprintf("https://meet.google.com/trial-race-%d", g), "mock", trialLimit*60)
			mu.Lock()
			switch {
			case err == nil:
				okCount++
			case errors.Is(err, ErrNoQuota):
				noQuota++
			default:
				otherErrs = append(otherErrs, err)
			}
			mu.Unlock()
		}(g)
	}
	close(start)
	wg.Wait()

	for _, e := range otherErrs {
		t.Errorf("неожиданная ошибка CreateJob: %v", e)
	}
	if okCount != 1 {
		t.Errorf("создано Job = %d, ожидали ровно 1 (гонка резерва триала)", okCount)
	}
	if noQuota != goroutines-1 {
		t.Errorf("ErrNoQuota = %d, ожидали %d", noQuota, goroutines-1)
	}
	if got := trialUsed(t, s, userID); got != trialLimit {
		t.Errorf("trial_meetings_used = %d, ожидали %d (выдали бесплатную встречу сверх лимита)", got, trialLimit)
	}
}

// TestCreateJob_SubscriptionRace — резерв подписки §5.1 под гонкой. Подписка с
// meetings_balance=1; два параллельных CreateJob. Суть теста — FOR UPDATE на
// подзапросе SELECT ... FOR UPDATE: без него оба запроса выбрали бы одну строку и
// списали баланс дважды (до -1, что отловил бы и CHECK >= 0, но инвариант — именно
// «ровно одно списание»). Инвариант: ровно один Job против подписки, balance==0.
// Под -race. db.md §5.1.
func TestCreateJob_SubscriptionRace(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	setTrialUsed(t, s, userID, 3)
	subID := insertSubscription(t, s, userID, 1, 15) // ровно один платный слот

	const goroutines = 2
	var (
		mu        sync.Mutex
		okCount   int
		noQuota   int
		otherErrs []error
		start     = make(chan struct{})
		wg        sync.WaitGroup
	)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			<-start
			_, err := s.CreateJob(ctx, userID, fmt.Sprintf("https://meet.google.com/sub-race-%d", g), "mock", 180)
			mu.Lock()
			switch {
			case err == nil:
				okCount++
			case errors.Is(err, ErrNoQuota):
				noQuota++
			default:
				otherErrs = append(otherErrs, err)
			}
			mu.Unlock()
		}(g)
	}
	close(start)
	wg.Wait()

	for _, e := range otherErrs {
		t.Errorf("неожиданная ошибка CreateJob: %v", e)
	}
	if okCount != 1 {
		t.Errorf("создано Job = %d, ожидали ровно 1 (гонка резерва подписки)", okCount)
	}
	if noQuota != goroutines-1 {
		t.Errorf("ErrNoQuota = %d, ожидали %d", noQuota, goroutines-1)
	}
	if got := subBalance(t, s, subID); got != 0 {
		t.Errorf("meetings_balance = %d, ожидали 0 (двойное списание одной подписки)", got)
	}
}

// TestCreateJob_SubscriptionPriorityOverTrial — резерв §5.1: при активной подписке
// с остатком платная квота расходуется ПЕРВОЙ (триал не трогается). Инвариант:
// Job создаётся is_trial=false с заполненным subscription_id, balance-1,
// trial_meetings_used НЕ растёт (иначе юзер «терял» бы оплаченную встречу впустую).
func TestCreateJob_SubscriptionPriorityOverTrial(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	subID := insertSubscription(t, s, userID, 5, 15)

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/sub-priority", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	if job.IsTrial {
		t.Error("Job.IsTrial = true при активной подписке, ожидали резерв подписки")
	}
	gotTrial, gotSub := jobQuota(t, s, job.ID)
	if gotTrial {
		t.Error("в БД is_trial = true при активной подписке")
	}
	if gotSub == nil || *gotSub != subID {
		t.Errorf("subscription_id = %v, ожидали %d", gotSub, subID)
	}
	if got := subBalance(t, s, subID); got != 4 {
		t.Errorf("meetings_balance = %d, ожидали 4 (списание с подписки)", got)
	}
	if got := trialUsed(t, s, userID); got != 0 {
		t.Errorf("trial_meetings_used = %d, ожидали 0 (триал не должен трогаться)", got)
	}
}

func TestCreateJob_TrialPartialMinuteReserve(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	setTrialUsed(t, s, userID, 2) // 120 минут из 150 уже заняты

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/trial-partial-minutes", "mock", 150)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if !job.IsTrial {
		t.Fatal("Job ожидалась триальной")
	}
	if got := jobReservedMinutes(t, s, job.ID); got != 30 {
		t.Errorf("reserved_minutes = %d, ожидали 30", got)
	}
	if got := trialMinutesUsed(t, s, userID); got != 150 {
		t.Errorf("trial_minutes_used = %d, ожидали 150", got)
	}
}

func TestCreateJob_SubscriptionPartialMinuteReserveAndRefund(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	subID := insertSubscription(t, s, userID, 1, 15)
	if _, err := s.pool.Exec(ctx,
		`UPDATE subscriptions SET minutes_balance = 30 WHERE id = $1`,
		subID,
	); err != nil {
		t.Fatalf("set subscription minutes_balance: %v", err)
	}

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/sub-partial-minutes", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if job.IsTrial {
		t.Fatal("Job ожидалась подписочной")
	}
	if got := jobReservedMinutes(t, s, job.ID); got != 30 {
		t.Fatalf("reserved_minutes = %d, ожидали 30", got)
	}
	if got := subMinutesBalance(t, s, subID); got != 0 {
		t.Fatalf("minutes_balance после резерва = %d, ожидали 0", got)
	}

	refunded, err := s.FailJobWithQuotaRefund(ctx, job.ID, "provider failed")
	if err != nil {
		t.Fatalf("FailJobWithQuotaRefund: %v", err)
	}
	if !refunded {
		t.Fatal("refunded=false, ожидали true")
	}
	if got := subMinutesBalance(t, s, subID); got != 30 {
		t.Errorf("minutes_balance после отката = %d, ожидали 30", got)
	}
}

func TestCreateJob_SelectsFullSubscriptionBeforeOlderPartial(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	oldSubID := insertSubscription(t, s, userID, 1, 15)
	if _, err := s.pool.Exec(ctx,
		`UPDATE subscriptions
		 SET expires_at = now() + INTERVAL '10 days',
		     minutes_balance = 10
		 WHERE id = $1`,
		oldSubID,
	); err != nil {
		t.Fatalf("set old subscription partial minutes: %v", err)
	}
	newSubID := insertSubscription(t, s, userID, 15, 15)

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/sub-full-before-partial", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if job.IsTrial {
		t.Fatal("Job ожидалась подписочной")
	}
	_, gotSubID := jobQuota(t, s, job.ID)
	if gotSubID == nil || *gotSubID != newSubID {
		t.Fatalf("subscription_id = %v, ожидали новую полную подписку %d", gotSubID, newSubID)
	}
	if got := jobReservedMinutes(t, s, job.ID); got != 60 {
		t.Fatalf("reserved_minutes = %d, ожидали 60", got)
	}
	if got := subMinutesBalance(t, s, oldSubID); got != 10 {
		t.Fatalf("old minutes_balance = %d, ожидали 10", got)
	}
	if got := subMinutesBalance(t, s, newSubID); got != 840 {
		t.Fatalf("new minutes_balance после резерва = %d, ожидали 840", got)
	}

	if err := s.SetJobSubmitted(ctx, job.ID, "ext-sub-full-before-partial"); err != nil {
		t.Fatalf("SetJobSubmitted: %v", err)
	}
	if err := s.SetJobDone(ctx, job.ID, []byte(`{"transcript":"ok"}`), 50*60, 180); err != nil {
		t.Fatalf("SetJobDone: %v", err)
	}
	if got := subMinutesBalance(t, s, oldSubID); got != 10 {
		t.Fatalf("old minutes_balance после done = %d, ожидали 10", got)
	}
	if got := subMinutesBalance(t, s, newSubID); got != 850 {
		t.Fatalf("new minutes_balance после done = %d, ожидали 850", got)
	}
	billed, _ := jobBilling(t, s, job.ID)
	if billed != 50 {
		t.Fatalf("billed_minutes = %d, ожидали 50", billed)
	}
}

// TestFailJobWithQuotaRefund_TrialDoubleTick — откат §5.2 под двойным тиком (guard
// по статусу). Триальная Job в processing. Первый FailJobWithQuotaRefund: refunded=
// true, trial_meetings_used откатился на 1. Второй (Job уже failed): refunded=false,
// счётчик НЕ меняется. Без guard'а второй тик вернул бы квоту дважды (lost-update,
// db.md §5.2). Инвариант денег: квота возвращается ровно один раз.
func TestFailJobWithQuotaRefund_TrialDoubleTick(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/trial-refund", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if got := trialUsed(t, s, userID); got != 1 {
		t.Fatalf("trial_meetings_used после резерва = %d, ожидали 1", got)
	}
	if err := s.SetJobSubmitted(ctx, job.ID, "ext-trial-refund"); err != nil {
		t.Fatalf("SetJobSubmitted: %v", err)
	}

	refunded, err := s.FailJobWithQuotaRefund(ctx, job.ID, "provider failed")
	if err != nil {
		t.Fatalf("FailJobWithQuotaRefund #1: %v", err)
	}
	if !refunded {
		t.Error("первый откат: refunded = false, ожидали true")
	}
	if got := trialUsed(t, s, userID); got != 0 {
		t.Errorf("trial_meetings_used после отката = %d, ожидали 0", got)
	}

	refunded, err = s.FailJobWithQuotaRefund(ctx, job.ID, "provider failed again")
	if err != nil {
		t.Fatalf("FailJobWithQuotaRefund #2: %v", err)
	}
	if refunded {
		t.Error("второй откат: refunded = true, ожидали false (guard по статусу)")
	}
	if got := trialUsed(t, s, userID); got != 0 {
		t.Errorf("trial_meetings_used после двойного отката = %d, ожидали 0 (квота вернулась дважды)", got)
	}
}

func TestFailJobWithQuotaRefund_FromCreated(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/created-refund", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if got := trialUsed(t, s, userID); got != 1 {
		t.Fatalf("trial_meetings_used после резерва = %d, ожидали 1", got)
	}

	refunded, err := s.FailJobWithQuotaRefund(ctx, job.ID, "submit failed")
	if err != nil {
		t.Fatalf("FailJobWithQuotaRefund: %v", err)
	}
	if !refunded {
		t.Error("refunded = false, ожидали true (откат из created)")
	}
	if got := trialUsed(t, s, userID); got != 0 {
		t.Errorf("trial_meetings_used после отката = %d, ожидали 0", got)
	}
	if got := jobStatus(t, s, job.ID); got != string(JobFailed) {
		t.Errorf("status = %q, ожидали %q", got, JobFailed)
	}
}

// TestFailJobWithQuotaRefund_SubscriptionSymmetry — откат §5.2 для подписки:
// подписочная Job (is_trial=false) в processing -> откат -> meetings_balance +1.
// Симметрия с триалом: то же поведение, другое направление возврата.
func TestFailJobWithQuotaRefund_SubscriptionSymmetry(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	subID := insertSubscription(t, s, userID, 5, 15)

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/sub-refund", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if job.IsTrial {
		t.Fatal("Job ожидалась подписочной (is_trial=false)")
	}
	if got := subBalance(t, s, subID); got != 4 {
		t.Fatalf("meetings_balance после резерва = %d, ожидали 4", got)
	}
	if err := s.SetJobSubmitted(ctx, job.ID, "ext-sub-refund"); err != nil {
		t.Fatalf("SetJobSubmitted: %v", err)
	}

	refunded, err := s.FailJobWithQuotaRefund(ctx, job.ID, "provider failed")
	if err != nil {
		t.Fatalf("FailJobWithQuotaRefund: %v", err)
	}
	if !refunded {
		t.Error("refunded = false, ожидали true")
	}
	if got := subBalance(t, s, subID); got != 5 {
		t.Errorf("meetings_balance после отката = %d, ожидали 5 (возврат на подписку)", got)
	}
}

// TestSetJobFailedSummary_NoRefund — §5.2: сбой саммари (summarizing -> failed_summary)
// НЕ откатывает резерв ни для триала, ни для подписки: транскрипт уже доставлен,
// встреча использована (симметрия с awaiting_reauth, db.md §5.2/§7.0). Проверяем
// обе ветки квоты — направление отката тут вообще не должно сработать.
func TestSetJobFailedSummary_NoRefund(t *testing.T) {
	ctx := context.Background()

	t.Run("trial", func(t *testing.T) {
		s := newTestStorage(t)
		userID := newTestUser(t, s)
		job, err := s.CreateJob(ctx, userID, "https://meet.google.com/fs-trial", "mock", 180)
		if err != nil {
			t.Fatalf("CreateJob: %v", err)
		}
		if err := s.SetJobSubmitted(ctx, job.ID, "ext-fs-trial"); err != nil {
			t.Fatalf("SetJobSubmitted: %v", err)
		}
		if err := s.SetJobSummarizing(ctx, job.ID, []byte(`{"transcript":"x"}`), 3600); err != nil {
			t.Fatalf("SetJobSummarizing: %v", err)
		}
		if err := s.SetJobFailedSummary(ctx, job.ID, "claude timeout", 180); err != nil {
			t.Fatalf("SetJobFailedSummary: %v", err)
		}
		if got := trialUsed(t, s, userID); got != 1 {
			t.Errorf("trial_meetings_used = %d, ожидали 1 (резерв НЕ откатывается на failed_summary)", got)
		}
	})

	t.Run("subscription", func(t *testing.T) {
		s := newTestStorage(t)
		userID := newTestUser(t, s)
		subID := insertSubscription(t, s, userID, 5, 15)
		job, err := s.CreateJob(ctx, userID, "https://meet.google.com/fs-sub", "mock", 180)
		if err != nil {
			t.Fatalf("CreateJob: %v", err)
		}
		if err := s.SetJobSubmitted(ctx, job.ID, "ext-fs-sub"); err != nil {
			t.Fatalf("SetJobSubmitted: %v", err)
		}
		if err := s.SetJobSummarizing(ctx, job.ID, []byte(`{"transcript":"x"}`), 3600); err != nil {
			t.Fatalf("SetJobSummarizing: %v", err)
		}
		if err := s.SetJobFailedSummary(ctx, job.ID, "claude timeout", 180); err != nil {
			t.Fatalf("SetJobFailedSummary: %v", err)
		}
		if got := subBalance(t, s, subID); got != 4 {
			t.Errorf("meetings_balance = %d, ожидали 4 (резерв НЕ откатывается на failed_summary)", got)
		}
	})
}

// TestSetJobDone_TrialCounterIdempotent — done §5.7: триальная Job summarizing->done
// финализирует billed_minutes один раз и сохраняет raw_provider_response; повторный
// SetJobDone не двигает счетчики второй раз.
func TestSetJobDone_TrialCounterIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/done-trial", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.SetJobSubmitted(ctx, job.ID, "ext-done-trial"); err != nil {
		t.Fatalf("SetJobSubmitted: %v", err)
	}
	if err := s.SetJobSummarizing(ctx, job.ID, []byte(`{"transcript":"draft"}`), 3600); err != nil {
		t.Fatalf("SetJobSummarizing: %v", err)
	}

	// Один ключ: JSONB переупорядочивает ключи, а jsonEqual только Compact'ит
	// (не сортирует) — многоключевой литерал дал бы ложное падение по порядку.
	raw := []byte(`{"transcript":"final"}`)
	if err := s.SetJobDone(ctx, job.ID, raw, 3600, 180); err != nil {
		t.Fatalf("SetJobDone #1: %v", err)
	}
	status, _, gotRaw := jobRow(t, s, job.ID)
	if status != string(JobDone) {
		t.Errorf("status = %q, ожидали %q", status, JobDone)
	}
	if !jsonEqual(gotRaw, raw) {
		t.Errorf("raw_provider_response = %q, ожидали эквивалент %q", gotRaw, raw)
	}
	if got := trialDone(t, s, userID); got != 1 {
		t.Errorf("trial_meetings_done после done = %d, ожидали 1", got)
	}
	// При duration 3600 финальный расход равен резерву: refund/extra не нужны.
	if got := trialUsed(t, s, userID); got != 1 {
		t.Errorf("trial_meetings_used после done = %d, ожидали 1", got)
	}

	// Повторный done (второй поллинг провайдера) — guard не даёт двинуть счётчик.
	if err := s.SetJobDone(ctx, job.ID, []byte(`{"transcript":"dup"}`), 3600, 180); err != nil {
		t.Fatalf("SetJobDone #2: %v", err)
	}
	if got := trialDone(t, s, userID); got != 1 {
		t.Errorf("trial_meetings_done после повторного done = %d, ожидали 1 (guard по статусу)", got)
	}
}

func TestSetJobDone_TrialShortDurationRefundsMinutes(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/done-trial-short", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.SetJobSubmitted(ctx, job.ID, "ext-done-trial-short"); err != nil {
		t.Fatalf("SetJobSubmitted: %v", err)
	}
	if err := s.SetJobDone(ctx, job.ID, []byte(`{"transcript":"short"}`), 10, 180); err != nil {
		t.Fatalf("SetJobDone: %v", err)
	}

	if got := trialMinutesUsed(t, s, userID); got != 1 {
		t.Errorf("trial_minutes_used = %d, ожидали 1", got)
	}
	if got := trialMinutesDone(t, s, userID); got != 1 {
		t.Errorf("trial_minutes_done = %d, ожидали 1", got)
	}
	billed, duration := jobBilling(t, s, job.ID)
	if billed != 1 {
		t.Errorf("billed_minutes = %d, ожидали 1", billed)
	}
	if duration == nil || *duration != 10 {
		t.Errorf("duration_seconds = %v, ожидали 10", duration)
	}
}

func TestSetJobDone_SubscriptionExtraFromOriginalSource(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	subID := insertSubscription(t, s, userID, 2, 15)

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/done-sub-extra", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.SetJobSubmitted(ctx, job.ID, "ext-done-sub-extra"); err != nil {
		t.Fatalf("SetJobSubmitted: %v", err)
	}
	if err := s.SetJobDone(ctx, job.ID, []byte(`{"transcript":"long"}`), 90*60, 180); err != nil {
		t.Fatalf("SetJobDone: %v", err)
	}

	if got := subMinutesBalance(t, s, subID); got != 30 {
		t.Errorf("minutes_balance = %d, ожидали 30", got)
	}
	billed, _ := jobBilling(t, s, job.ID)
	if billed != 90 {
		t.Errorf("billed_minutes = %d, ожидали 90", billed)
	}
}

func TestSetJobDone_SubscriptionExtraNotEnoughNoDebt(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	subID := insertSubscription(t, s, userID, 1, 15)

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/done-sub-no-extra", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.SetJobSubmitted(ctx, job.ID, "ext-done-sub-no-extra"); err != nil {
		t.Fatalf("SetJobSubmitted: %v", err)
	}
	if err := s.SetJobDone(ctx, job.ID, []byte(`{"transcript":"long"}`), 90*60, 180); err != nil {
		t.Fatalf("SetJobDone: %v", err)
	}

	if got := subMinutesBalance(t, s, subID); got != 0 {
		t.Errorf("minutes_balance = %d, ожидали 0", got)
	}
	billed, _ := jobBilling(t, s, job.ID)
	if billed != 60 {
		t.Errorf("billed_minutes = %d, ожидали 60 без долга и частичного extra", billed)
	}
}

func TestSetJobDone_TrialExtraFromOriginalSource(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/done-trial-extra", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.SetJobSubmitted(ctx, job.ID, "ext-done-trial-extra"); err != nil {
		t.Fatalf("SetJobSubmitted: %v", err)
	}
	if err := s.SetJobDone(ctx, job.ID, []byte(`{"transcript":"long"}`), 90*60, 180); err != nil {
		t.Fatalf("SetJobDone: %v", err)
	}

	if got := trialMinutesUsed(t, s, userID); got != 90 {
		t.Errorf("trial_minutes_used = %d, ожидали 90", got)
	}
	if got := trialMinutesDone(t, s, userID); got != 90 {
		t.Errorf("trial_minutes_done = %d, ожидали 90", got)
	}
	billed, _ := jobBilling(t, s, job.ID)
	if billed != 90 {
		t.Errorf("billed_minutes = %d, ожидали 90", billed)
	}
}

func TestSetJobDone_TrialExtraNotEnoughNoDebt(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/done-trial-no-extra", "mock", 60)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.SetJobSubmitted(ctx, job.ID, "ext-done-trial-no-extra"); err != nil {
		t.Fatalf("SetJobSubmitted: %v", err)
	}
	if err := s.SetJobDone(ctx, job.ID, []byte(`{"transcript":"long"}`), 90*60, 60); err != nil {
		t.Fatalf("SetJobDone: %v", err)
	}

	if got := trialMinutesUsed(t, s, userID); got != 60 {
		t.Errorf("trial_minutes_used = %d, ожидали 60 без долга", got)
	}
	if got := trialMinutesDone(t, s, userID); got != 60 {
		t.Errorf("trial_minutes_done = %d, ожидали 60 без долга", got)
	}
	billed, _ := jobBilling(t, s, job.ID)
	if billed != 60 {
		t.Errorf("billed_minutes = %d, ожидали 60 без долга и частичного extra", billed)
	}
}

func TestSetJobFailedSummary_UsesStoredDuration(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/failed-summary-duration", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.SetJobSubmitted(ctx, job.ID, "ext-failed-summary-duration"); err != nil {
		t.Fatalf("SetJobSubmitted: %v", err)
	}
	if err := s.SetJobSummarizing(ctx, job.ID, []byte(`{"transcript":"x"}`), 15*60); err != nil {
		t.Fatalf("SetJobSummarizing: %v", err)
	}
	if err := s.SetJobFailedSummary(ctx, job.ID, "summary failed", 180); err != nil {
		t.Fatalf("SetJobFailedSummary: %v", err)
	}

	if got := trialMinutesUsed(t, s, userID); got != 15 {
		t.Errorf("trial_minutes_used = %d, ожидали 15", got)
	}
	if got := trialMinutesDone(t, s, userID); got != 15 {
		t.Errorf("trial_minutes_done = %d, ожидали 15", got)
	}
	billed, duration := jobBilling(t, s, job.ID)
	if billed != 15 {
		t.Errorf("billed_minutes = %d, ожидали 15", billed)
	}
	if duration == nil || *duration != 15*60 {
		t.Errorf("duration_seconds = %v, ожидали %d", duration, 15*60)
	}
}

func TestSetJobFailedSummary_Idempotent(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/failed-summary-idem", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.SetJobSubmitted(ctx, job.ID, "ext-failed-summary-idem"); err != nil {
		t.Fatalf("SetJobSubmitted: %v", err)
	}
	if err := s.SetJobSummarizing(ctx, job.ID, []byte(`{"transcript":"x"}`), 15*60); err != nil {
		t.Fatalf("SetJobSummarizing: %v", err)
	}
	if err := s.SetJobFailedSummary(ctx, job.ID, "summary failed", 180); err != nil {
		t.Fatalf("SetJobFailedSummary #1: %v", err)
	}
	if err := s.SetJobFailedSummary(ctx, job.ID, "summary failed again", 180); err != nil {
		t.Fatalf("SetJobFailedSummary #2: %v", err)
	}

	if got := trialMinutesUsed(t, s, userID); got != 15 {
		t.Errorf("trial_minutes_used = %d, ожидали 15", got)
	}
	if got := trialMinutesDone(t, s, userID); got != 15 {
		t.Errorf("trial_minutes_done = %d, ожидали 15", got)
	}
	billed, _ := jobBilling(t, s, job.ID)
	if billed != 15 {
		t.Errorf("billed_minutes = %d, ожидали 15", billed)
	}
}

func TestSetJobDone_SubscriptionDeactivatedNoExtra(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	subID := insertSubscription(t, s, userID, 2, 15)

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/done-sub-deactivated", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.SetJobSubmitted(ctx, job.ID, "ext-done-sub-deactivated"); err != nil {
		t.Fatalf("SetJobSubmitted: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE subscriptions SET expires_at = now() WHERE id = $1`, subID); err != nil {
		t.Fatalf("deactivate subscription: %v", err)
	}
	if err := s.SetJobDone(ctx, job.ID, []byte(`{"transcript":"long"}`), 90*60, 180); err != nil {
		t.Fatalf("SetJobDone: %v", err)
	}

	if got := subMinutesBalance(t, s, subID); got != 60 {
		t.Errorf("minutes_balance = %d, ожидали 60 (extra не списывается с деактивированной подписки)", got)
	}
	billed, _ := jobBilling(t, s, job.ID)
	if billed != 60 {
		t.Errorf("billed_minutes = %d, ожидали 60 без extra", billed)
	}
}

func TestSetJobSummarizing_AfterCancelNoop(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/late-summarizing-cancel", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.SetJobSubmitted(ctx, job.ID, "ext-late-summarizing-cancel"); err != nil {
		t.Fatalf("SetJobSubmitted: %v", err)
	}
	if _, _, refunded, err := s.CancelJob(ctx, userID, job.ID); err != nil || !refunded {
		t.Fatalf("CancelJob refunded=%v err=%v", refunded, err)
	}
	if err := s.SetJobSummarizing(ctx, job.ID, []byte(`{"transcript":"late"}`), 15*60); err != nil {
		t.Fatalf("SetJobSummarizing late: %v", err)
	}

	if got := jobStatus(t, s, job.ID); got != string(JobFailed) {
		t.Errorf("status = %q, ожидали failed", got)
	}
	billed, duration := jobBilling(t, s, job.ID)
	if billed != 0 {
		t.Errorf("billed_minutes = %d, ожидали 0", billed)
	}
	if duration != nil {
		t.Errorf("duration_seconds = %v, ожидали nil", duration)
	}
	if got := trialMinutesUsed(t, s, userID); got != 0 {
		t.Errorf("trial_minutes_used = %d, ожидали 0 после cancel", got)
	}
}

// TestCreateJob_RateLimit — R2a: активная Job больше не блокирует другой URL.
// Ограничители теперь квота и same-URL dedup. Тот же URL должен вернуть
// ErrDuplicateActiveJob, а резерв триала обязан откатиться транзакционным rollback.
func TestCreateJob_RateLimit(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	const firstURL = "https://meet.google.com/rl-first"
	first, err := s.CreateJob(ctx, userID, firstURL, "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob #1: %v", err)
	}

	second, err := s.CreateJob(ctx, userID, "https://meet.google.com/rl-second", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob #2 с другим URL: %v", err)
	}
	if second.ID == 0 {
		t.Fatal("вторая Job с другим URL не создана")
	}
	if got := trialUsed(t, s, userID); got != 2 {
		t.Fatalf("trial_meetings_used = %d, ожидали 2 после двух разных URL", got)
	}

	_, err = s.CreateJob(ctx, userID, firstURL, "mock", 180)
	if !errors.Is(err, ErrDuplicateActiveJob) {
		t.Fatalf("CreateJob duplicate err = %v, ожидали ErrDuplicateActiveJob", err)
	}
	if got := trialUsed(t, s, userID); got != 2 {
		t.Errorf("trial_meetings_used = %d, ожидали 2 (duplicate должен откатить reserve)", got)
	}
	if first.ID == second.ID {
		t.Error("две разные встречи получили одинаковый job ID")
	}
}

// TestCreateJob_DuplicateActiveJobRollsBackSubscriptionReserve — same-URL duplicate
// не должен расходовать подписочную встречу. Основной путь отказывает до reserve;
// unique-violation fallback в CreateJob оставлен как страховочный rollback-path.
func TestCreateJob_DuplicateActiveJobRollsBackSubscriptionReserve(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	subID := insertSubscription(t, s, userID, 2, 15)

	const url = "https://meet.google.com/sub-dedup-rollback"
	if _, err := s.CreateJob(ctx, userID, url, "mock", 180); err != nil {
		t.Fatalf("CreateJob #1: %v", err)
	}
	if got := subBalance(t, s, subID); got != 1 {
		t.Fatalf("meetings_balance после первого резерва = %d, ожидали 1", got)
	}

	_, err := s.CreateJob(ctx, userID, url, "mock", 180)
	if !errors.Is(err, ErrDuplicateActiveJob) {
		t.Fatalf("CreateJob duplicate err = %v, ожидали ErrDuplicateActiveJob", err)
	}
	if got := subBalance(t, s, subID); got != 1 {
		t.Errorf("meetings_balance = %d, ожидали 1 (duplicate должен откатить reserve)", got)
	}
}

// TestCreateJob_DuplicateActiveJobWinsOverNoQuota — UX-инвариант R2a: повтор той
// же ссылки должен получить ErrDuplicateActiveJob даже если квоты уже не осталось.
func TestCreateJob_DuplicateActiveJobWinsOverNoQuota(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	const url = "https://meet.google.com/trial-dedup-no-quota"
	if _, err := s.CreateJob(ctx, userID, url, "mock", 60); err != nil {
		t.Fatalf("CreateJob #1: %v", err)
	}
	if got := trialUsed(t, s, userID); got != 1 {
		t.Fatalf("trial_meetings_used = %d, ожидали 1", got)
	}

	_, err := s.CreateJob(ctx, userID, url, "mock", 60)
	if !errors.Is(err, ErrDuplicateActiveJob) {
		t.Fatalf("CreateJob duplicate exhausted err = %v, ожидали ErrDuplicateActiveJob", err)
	}
	if got := trialUsed(t, s, userID); got != 1 {
		t.Errorf("trial_meetings_used = %d, ожидали 1", got)
	}
}

// TestCreateJob_ActiveCap — R2a provider-safety cap: 4 активных разных URL
// разрешены, 5-й отклоняется до резерва квоты. Duplicate важнее cap для UX.
func TestCreateJob_ActiveCap(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	var firstURL string
	for i := 0; i < maxActiveJobsPerUser; i++ {
		url := fmt.Sprintf("https://meet.google.com/cap-%d", i)
		if i == 0 {
			firstURL = url
		}
		if _, err := s.CreateJob(ctx, userID, url, "mock", 600); err != nil {
			t.Fatalf("CreateJob #%d: %v", i+1, err)
		}
	}
	if got := trialUsed(t, s, userID); got != maxActiveJobsPerUser {
		t.Fatalf("trial_meetings_used = %d, ожидали %d", got, maxActiveJobsPerUser)
	}

	_, err := s.CreateJob(ctx, userID, "https://meet.google.com/cap-5", "mock", 600)
	if !errors.Is(err, ErrTooManyActiveJobs) {
		t.Fatalf("CreateJob #5 err = %v, ожидали ErrTooManyActiveJobs", err)
	}
	if got := trialUsed(t, s, userID); got != maxActiveJobsPerUser {
		t.Errorf("trial_meetings_used = %d, ожидали %d (cap не должен списывать квоту)", got, maxActiveJobsPerUser)
	}

	_, err = s.CreateJob(ctx, userID, firstURL, "mock", 600)
	if !errors.Is(err, ErrDuplicateActiveJob) {
		t.Fatalf("CreateJob duplicate at cap err = %v, ожидали ErrDuplicateActiveJob", err)
	}
}

// TestCreateJob_ActiveCapRace — cap должен держаться под параллельными разными URL.
// Лок строки users сериализует CreateJob одного пользователя; без него обычный COUNT
// мог бы пропустить больше maxActiveJobsPerUser.
func TestCreateJob_ActiveCapRace(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	const goroutines = 8
	var (
		mu        sync.Mutex
		okCount   int
		capCount  int
		otherErrs []error
		start     = make(chan struct{})
		wg        sync.WaitGroup
	)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			<-start
			_, err := s.CreateJob(ctx, userID, fmt.Sprintf("https://meet.google.com/cap-race-%d", g), "mock", 600)
			mu.Lock()
			switch {
			case err == nil:
				okCount++
			case errors.Is(err, ErrTooManyActiveJobs):
				capCount++
			default:
				otherErrs = append(otherErrs, err)
			}
			mu.Unlock()
		}(g)
	}
	close(start)
	wg.Wait()

	for _, e := range otherErrs {
		t.Errorf("неожиданная ошибка CreateJob: %v", e)
	}
	if okCount != maxActiveJobsPerUser {
		t.Errorf("создано Job = %d, ожидали %d", okCount, maxActiveJobsPerUser)
	}
	if capCount != goroutines-maxActiveJobsPerUser {
		t.Errorf("ErrTooManyActiveJobs = %d, ожидали %d", capCount, goroutines-maxActiveJobsPerUser)
	}
	if got := trialUsed(t, s, userID); got != maxActiveJobsPerUser {
		t.Errorf("trial_meetings_used = %d, ожидали %d", got, maxActiveJobsPerUser)
	}
}

// TestSetJobDone_ConcurrentDoubleDone — done §5.7 под истинной конкуренцией: K тиков
// одновременно зовут SetJobDone на одной summarizing-Job (webhook+поллинг / 2 инстанса).
// Физика отличается от последовательного re-poll: row-lock на строке jobs сериализует
// UPDATE'ы, проигравшие перечитывают guard против закоммиченного 'done'. Инвариант:
// trial_meetings_done инкрементится ровно раз (lost-update нет). Под -race.
func TestSetJobDone_ConcurrentDoubleDone(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/done-concurrent", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := s.SetJobSubmitted(ctx, job.ID, "ext-done-concurrent"); err != nil {
		t.Fatalf("SetJobSubmitted: %v", err)
	}
	if err := s.SetJobSummarizing(ctx, job.ID, []byte(`{"transcript":"x"}`), 3600); err != nil {
		t.Fatalf("SetJobSummarizing: %v", err)
	}

	const goroutines = 8
	var (
		start = make(chan struct{})
		wg    sync.WaitGroup
	)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // синхронный старт: максимизируем наложение транзакций
			if err := s.SetJobDone(ctx, job.ID, []byte(`{"transcript":"final"}`), 3600, 180); err != nil {
				t.Errorf("SetJobDone: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := trialDone(t, s, userID); got != 1 {
		t.Errorf("trial_meetings_done = %d, ожидали 1 (конкурентный двойной done -> один инкремент)", got)
	}
}

// TestSetJobDone_SubscriptionNoTrialCounter — done §5.7: подписочная Job (is_trial=false)
// переходит в done, но trial_meetings_done НЕ растёт (фильтр closed.is_trial=true). Иначе
// платный юзер получил бы ложный F9-пуш «триал заканчивается».
func TestSetJobDone_SubscriptionNoTrialCounter(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)
	insertSubscription(t, s, userID, 5, 15)

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/done-sub", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if job.IsTrial {
		t.Fatal("Job ожидалась подписочной (is_trial=false)")
	}
	if err := s.SetJobSubmitted(ctx, job.ID, "ext-done-sub"); err != nil {
		t.Fatalf("SetJobSubmitted: %v", err)
	}
	if err := s.SetJobSummarizing(ctx, job.ID, []byte(`{"transcript":"x"}`), 3600); err != nil {
		t.Fatalf("SetJobSummarizing: %v", err)
	}
	if err := s.SetJobDone(ctx, job.ID, []byte(`{"transcript":"final"}`), 3600, 180); err != nil {
		t.Fatalf("SetJobDone: %v", err)
	}

	status, _, _ := jobRow(t, s, job.ID)
	if status != string(JobDone) {
		t.Errorf("status = %q, ожидали %q", status, JobDone)
	}
	if got := trialDone(t, s, userID); got != 0 {
		t.Errorf("trial_meetings_done = %d, ожидали 0 (подписочная Job не питает триал-счётчик)", got)
	}
}
