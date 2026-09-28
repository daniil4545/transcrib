package app

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Критические узлы слайса job-phases (plan-job-phases-slice.md «Storage»): фазовые
// переходы joining→recording→processing, точки входа в done для s2t (из recording) и
// mymeet (из processing), откат квоты из recording, и R2a parallel calls. Интеграционные,
// бьют в Postgres через DATABASE_URL;
// без него — Skip (newTestStorage).

// jobStatus читает только статус джобы (фазовые тесты не смотрят raw/ext).
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

// TestSetJobRecording_Phase — переход joining→recording (провайдер OnCall). moved=true
// на фактическом переходе; повторный вызов (уже recording) — moved=false (guard по
// статусу), статус-сообщение не редактируется второй раз.
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
	// После submit джоба в joining (а не processing — сдвиг семантики слайса).
	if got := jobStatus(t, s, job.ID); got != string(JobJoining) {
		t.Fatalf("status после submit = %q, ожидали %q", got, JobJoining)
	}

	moved, err := s.SetJobRecording(ctx, job.ID)
	if err != nil {
		t.Fatalf("SetJobRecording #1: %v", err)
	}
	if !moved {
		t.Error("первый SetJobRecording: moved=false, ожидали true (joining→recording)")
	}
	if got := jobStatus(t, s, job.ID); got != string(JobRecording) {
		t.Errorf("status = %q, ожидали %q", got, JobRecording)
	}

	// Повторный тик: джоба уже recording — guard не двигает, moved=false.
	moved, err = s.SetJobRecording(ctx, job.ID)
	if err != nil {
		t.Fatalf("SetJobRecording #2: %v", err)
	}
	if moved {
		t.Error("повторный SetJobRecording: moved=true, ожидали false (guard по статусу)")
	}
}

// TestSetJobProcessing_Phase — переход в processing (провайдер Transcribing, mymeet).
// Принимает вход из recording (обычный путь) и из joining (поллинг перескочил
// recording — короткая встреча). Повторный вызов — moved=false.
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
			t.Error("SetJobProcessing из recording: moved=false, ожидали true")
		}
		if got := jobStatus(t, s, job.ID); got != string(JobProcessing) {
			t.Errorf("status = %q, ожидали %q", got, JobProcessing)
		}

		// Повторный тик — уже processing, moved=false.
		moved, err = s.SetJobProcessing(ctx, job.ID)
		if err != nil {
			t.Fatalf("SetJobProcessing #2: %v", err)
		}
		if moved {
			t.Error("повторный SetJobProcessing: moved=true, ожидали false (guard по статусу)")
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
		// Без SetJobRecording: тик перескочил фазу recording, джоба ещё joining.
		moved, err := s.SetJobProcessing(ctx, job.ID)
		if err != nil {
			t.Fatalf("SetJobProcessing: %v", err)
		}
		if !moved {
			t.Error("SetJobProcessing из joining: moved=false, ожидали true (guard включает joining)")
		}
		if got := jobStatus(t, s, job.ID); got != string(JobProcessing) {
			t.Errorf("status = %q, ожидали %q", got, JobProcessing)
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
		t.Fatal("SetJobProcessing moved=false, ожидали true")
	}
	billed, duration := jobBilling(t, s, job.ID)
	if billed != 0 {
		t.Fatalf("billed_minutes до done = %d, ожидали 0", billed)
	}
	if duration == nil {
		t.Fatal("duration_seconds is nil, ожидали lifecycle duration")
	}
	if *duration < int((7*time.Minute).Seconds()) || *duration > int((8*time.Minute).Seconds()) {
		t.Fatalf("duration_seconds = %d, ожидали около 7m10s", *duration)
	}

	if err := s.SetJobDone(ctx, job.ID, []byte(`{"transcript":"ok"}`), 0, 180); err != nil {
		t.Fatalf("SetJobDone: %v", err)
	}
	billed, duration = jobBilling(t, s, job.ID)
	if billed != 8 {
		t.Fatalf("billed_minutes = %d, ожидали 8 (ceil по минутам)", billed)
	}
	if duration == nil {
		t.Fatal("duration_seconds после done is nil")
	}
}

// TestSetJobDone_FromRecording — s2t-путь: джоба идёт joining→recording→done, минуя
// processing (s2t не отдаёт Transcribing). Guard SetJobDone включает recording, поэтому
// finishTranscription закрывает джобу прямо из recording. Триальный счётчик инкрементится.
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
		t.Fatalf("SetJobDone из recording: %v", err)
	}
	status, _, gotRaw := jobRow(t, s, job.ID)
	if status != string(JobDone) {
		t.Errorf("status = %q, ожидали %q", status, JobDone)
	}
	if !jsonEqual(gotRaw, raw) {
		t.Errorf("raw = %q, ожидали эквивалент %q", gotRaw, raw)
	}
	if got := trialDone(t, s, userID); got != 1 {
		t.Errorf("trial_meetings_done = %d, ожидали 1 (done из recording инкрементит счётчик)", got)
	}
}

// TestSetJobDone_FromProcessing — mymeet-путь без саммари (SUMMARIZER_PROVIDER=off):
// джоба joining→recording→processing→done, без summarizing. Guard включает processing.
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
		t.Fatalf("SetJobDone из processing: %v", err)
	}
	if got := jobStatus(t, s, job.ID); got != string(JobDone) {
		t.Errorf("status = %q, ожидали %q", got, JobDone)
	}
	if got := trialDone(t, s, userID); got != 1 {
		t.Errorf("trial_meetings_done = %d, ожидали 1", got)
	}
}

// TestSetJobDone_FromJoining — поллинг дискретен (тик 30с) и может перескочить обе
// фазы: один тик видит connecting (джоба в joining), следующий уже new/done (короткая
// встреча). finishTranscription тогда зовёт SetJobDone прямо из joining, минуя
// recording/processing. Guard SetJobDone расширен на joining именно под этот пропуск
// (plan-job-phases-slice.md «Storage», ответ на ревью M2). Триальный счётчик инкрементится
// ровно раз — переход one-shot под guard.
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
	// Без SetJobRecording/SetJobProcessing: тик перескочил обе фазы, джоба ещё joining.
	if got := jobStatus(t, s, job.ID); got != string(JobJoining) {
		t.Fatalf("status перед done = %q, ожидали %q", got, JobJoining)
	}

	raw := []byte(`{"transcript":"short"}`)
	if err := s.SetJobDone(ctx, job.ID, raw, 3600, 180); err != nil {
		t.Fatalf("SetJobDone из joining: %v", err)
	}
	status, _, gotRaw := jobRow(t, s, job.ID)
	if status != string(JobDone) {
		t.Errorf("status = %q, ожидали %q (done напрямую из joining)", status, JobDone)
	}
	if !jsonEqual(gotRaw, raw) {
		t.Errorf("raw = %q, ожидали эквивалент %q", gotRaw, raw)
	}
	if got := trialDone(t, s, userID); got != 1 {
		t.Errorf("trial_meetings_done = %d, ожидали 1 (done из joining инкрементит счётчик ровно раз)", got)
	}
}

// TestFailJobWithQuotaRefund_FromRecording — §5.2: сбой/watchdog в фазе recording
// (бот ещё на встрече) откатывает резерв квоты. Guard расширен на joining/recording,
// поэтому встреча, упавшая до выхода бота, возвращает триальную встречу юзеру.
func TestFailJobWithQuotaRefund_FromRecording(t *testing.T) {
	ctx := context.Background()
	s := newTestStorage(t)
	userID := newTestUser(t, s)

	job, err := s.CreateJob(ctx, userID, "https://meet.google.com/fail-from-rec", "mock", 180)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if got := trialUsed(t, s, userID); got != 1 {
		t.Fatalf("trial_meetings_used после резерва = %d, ожидали 1", got)
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
		t.Error("refunded=false, ожидали true (откат из recording)")
	}
	if got := trialUsed(t, s, userID); got != 0 {
		t.Errorf("trial_meetings_used после отката = %d, ожидали 0", got)
	}
	if got := jobStatus(t, s, job.ID); got != string(JobFailed) {
		t.Errorf("status = %q, ожидали %q", got, JobFailed)
	}
}

// TestCreateJob_RateLimit_Phases — R2a: фаза активной Job больше не блокирует
// другую ссылку. Квота остаётся денежным ограничителем, тот же URL режется dedup.
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
			t.Fatalf("CreateJob #2 при первой в recording: %v", err)
		}
		if second.ID == 0 {
			t.Error("вторая Job не создана при первой в recording")
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

		// Первая в processing (бот вышел) — вторая под другим URL проходит.
		second, err := s.CreateJob(ctx, userID, "https://meet.google.com/rl-proc-second", "mock", 180)
		if err != nil {
			t.Errorf("CreateJob #2 при первой в processing: %v, ожидали успех (§5.8 не держит)", err)
		}
		if second.ID == 0 {
			t.Error("вторая джоба не создана при первой в processing")
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
			t.Errorf("CreateJob тем же URL: err = %v, ожидали ErrDuplicateActiveJob", err)
		}
	})
}
