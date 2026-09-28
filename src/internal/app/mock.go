package app

import (
	"context"
	"encoding/json"
	"fmt"
)

// Контракт закреплён до проводки в S5: тесты зовут методы напрямую,
// и без ассерта дрейф сигнатуры компилятор не поймал бы.
var _ TranscriptionProvider = (*MockProvider)(nil)

// MockProvider реализует TranscriptionProvider для локального E2E без внешнего API.
// Используется из одной горутины оркестратора — синхронизация не нужна.
type MockProvider struct {
	// DoneAfterPolls — сколько вызовов Poll вернут StatusPending до StatusDone.
	// 0 (zero value) — StatusDone сразу на первом вызове.
	DoneAfterPolls int

	// FailOnPoll — если true, Poll возвращает StatusFailed (режим отказа для S5 E2E).
	// Нулевое значение даёт happy path.
	FailOnPoll bool

	// Phased — если true, Poll проводит джобу по фазам OnCall -> Transcribing -> Done
	// (по одному вызову на фазу), чтобы тест orchestrator прогнал полный путь
	// joining -> recording -> processing -> done. Нулевое значение (false) сохраняет
	// исходное поведение Pending*DoneAfterPolls -> Done — существующие тесты не ломаются.
	Phased bool

	pollCount map[string]int // счётчик вызовов Poll по externalJobID

	// Cancelled записывает externalJobID, для которых вызван Cancel (проверка orphan-timeout).
	Cancelled []string
}

// Submit возвращает детерминированный фейковый id без внешнего вызова.
func (m *MockProvider) Submit(_ context.Context, job Job) (string, error) {
	return fmt.Sprintf("mock-job-%d", job.ID), nil
}

// Poll возвращает StatusFailed если FailOnPoll. В режиме Phased ведёт джобу по фазам
// OnCall -> Transcribing -> Done (по одному вызову на фазу). Иначе — StatusPending первые
// DoneAfterPolls вызовов, затем StatusDone.
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

// Fetch возвращает заготовленный транскрипт и валидный JSON в Raw.
// Вызывается при StatusDone и при stop-marker после provider failure/cancel.
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

	// Raw обязан быть валидным JSON — колонка JSONB не примет plain text.
	// json.Marshal вместо ручного литерала: валидность не зависит от содержимого id.
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

// Cancel — no-op для mock (внешнего бота нет), но записывает вызов для проверок.
func (m *MockProvider) Cancel(_ context.Context, externalJobID string) error {
	m.Cancelled = append(m.Cancelled, externalJobID)
	return nil
}
