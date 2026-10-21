package app

import (
	"context"
	"encoding/json"
	"fmt"
)

// The contract is pinned before wiring in S5: tests call the methods directly,
// and without an assertion the compiler would not catch signature drift.
var _ TranscriptionProvider = (*MockProvider)(nil)

// MockProvider implements TranscriptionProvider for local E2E without an external API.
// Used from a single orchestrator goroutine, so no synchronization is needed.
type MockProvider struct {
	// DoneAfterPolls is how many Poll calls return StatusPending before StatusDone.
	// 0 (zero value) means StatusDone on the first call.
	DoneAfterPolls int

	// FailOnPoll: if true, Poll returns StatusFailed (failure mode for S5 E2E).
	// The zero value gives the happy path.
	FailOnPoll bool

	// Phased: if true, Poll walks the job through the phases OnCall -> Transcribing -> Done
	// (one call per phase), so the orchestrator test runs the full path
	// joining -> recording -> processing -> done. The zero value (false) keeps the
	// original Pending*DoneAfterPolls -> Done behavior, so existing tests do not break.
	Phased bool

	pollCount map[string]int // Poll call counter per externalJobID

	// Cancelled records the externalJobIDs for which Cancel was called (orphan-timeout check).
	Cancelled []string
}

// Submit returns a deterministic fake id without an external call.
func (m *MockProvider) Submit(_ context.Context, job Job) (string, error) {
	return fmt.Sprintf("mock-job-%d", job.ID), nil
}

// Poll returns StatusFailed if FailOnPoll. In Phased mode it walks the job through the phases
// OnCall -> Transcribing -> Done (one call per phase). Otherwise StatusPending for the first
// DoneAfterPolls calls, then StatusDone.
func (m *MockProvider) Poll(_ context.Context, externalJobID string) (ProviderStatus, error) {
	if m.FailOnPoll {
		return StatusFailed, nil
	}
	if m.pollCount == nil {
		m.pollCount = make(map[string]int)
	}
	m.pollCount[externalJobID]++
	if m.Phased {
		switch m.pollCount[externalJobID] {
		case 1:
			return StatusOnCall, nil
		case 2:
			return StatusTranscribing, nil
		default:
			return StatusDone, nil
		}
	}
	if m.pollCount[externalJobID] <= m.DoneAfterPolls {
		return StatusPending, nil
	}
	return StatusDone, nil
}

// Fetch returns a canned transcript and valid JSON in Raw.
// Called on StatusDone and on a stop marker after provider failure/cancel.
func (m *MockProvider) Fetch(_ context.Context, externalJobID string) (Result, error) {
	transcript := "# Транскрипт встречи\n\n" +
		"**заказчик:** Так, погнали. Первое — оплата. Дима, что по ЮKassa?\n\n" +
		"**Дима:** Магазин почти подтвердили, жду ответ модерации. Думаю, к пятнице будет точно.\n\n" +
		"**заказчик:** Ок. Как подтвердят — сразу обнови конфиг и выкатывай на прод, не тяни.\n\n" +
		"**Дима:** Сделаю в тот же день.\n\n" +
		"**заказчик:** Анна, на тебе тексты лендинга. Нужны к среде, иначе к запуску не успеваем.\n\n" +
		"**Анна:** Окей, к среде сделаю. Кстати, по триалу так и не решили — три встречи или пять?\n\n" +
		"**Маша:** Я за пять. Три — это вообще мало, человек не распробует и отвалится.\n\n" +
		"**заказчик:** Нет, оставляем три. Пять нам в минус по марже, уже считали. Три — и точка.\n\n" +
		"**Маша:** Ну ок, как скажешь. Просто потом не удивляйтесь конверсии.\n\n" +
		"**Дима:** Ещё момент — s2t опять зомбит, бот не выходит из комнаты. Третий раз за неделю.\n\n" +
		"**заказчик:** Да достало уже. Маша, напиши их поддержке жёстко, пусть чинят воркеры. Пока сидим на mymeet.\n\n" +
		"**Маша:** Напишу сегодня.\n\n" +
		"**заказчик:** Всё, по коням.\n"

	// Raw must be valid JSON: a JSONB column does not accept plain text.
	// json.Marshal instead of a hand-written literal: validity does not depend on the id content.
	raw, err := json.Marshal(map[string]any{
		"job_id":   externalJobID,
		"status":   "done",
		"speakers": 2,
	})
	if err != nil {
		return Result{}, fmt.Errorf("mock raw marshal: %w", err)
	}

	return Result{
		TranscriptMarkdown: transcript,
		Raw:                raw,
	}, nil
}

// Cancel is a no-op for the mock (there is no external bot), but records the call for assertions.
func (m *MockProvider) Cancel(_ context.Context, externalJobID string) error {
	m.Cancelled = append(m.Cancelled, externalJobID)
	return nil
}
