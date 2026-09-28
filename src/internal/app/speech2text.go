package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Компайл-тайм ассерт: Speech2TextProvider реализует интерфейс.
// Дрейф сигнатуры поймает компилятор, а не runtime.
var _ TranscriptionProvider = (*Speech2TextProvider)(nil)

// Speech2TextProvider — основной поставщик транскрибации (architecture.md §8).
// Отправляет бота на встречу через Meeting API и забирает результат по готовности.
type Speech2TextProvider struct {
	apiKey   string
	baseURL  string       // default https://api.speech2text.ru
	client   *http.Client // таймаут снаружи через ctx; свой клиент (не дефолтный без таймаута)
	mu       sync.Mutex
	lastPoll map[string]string
}

// NewSpeech2Text создаёт провайдер. apiKey никогда не попадает в логи и тексты ошибок.
func NewSpeech2Text(apiKey, baseURL string) *Speech2TextProvider {
	return &Speech2TextProvider{
		apiKey:  apiKey,
		baseURL: baseURL,
		// Основной таймаут — ctx (providerTimeout в orchestrator.go). Timeout здесь —
		// страховка от будущих вызывающих с другим ctx (webhook-handler в web.go),
		// заведомо больше ctx-таймаута, чтобы не срабатывать первым.
		client:   &http.Client{Timeout: 60 * time.Second},
		lastPoll: make(map[string]string),
	}
}

// Submit отправляет бота на встречу, возвращает external id задачи у поставщика.
// POST /api/meetings/join — минимальный payload.
func (p *Speech2TextProvider) Submit(ctx context.Context, job Job) (string, error) {
	payload := map[string]string{"meeting_url": job.MeetingURL}
	// bot_name — отображаемое имя бота-участника на встрече (бренд; контракт API.md §2.2).
	payload["bot_name"] = "transcrib"
	// Zoom кладёт пароль в query (?pwd=...), но s2t ждёт его отдельным полем
	// access_code (ответ поддержки 2026-06-11) — без него бот не войдёт на встречу.
	if code := meetingAccessCode(job.MeetingURL); code != "" {
		payload["access_code"] = code
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("speech2text submit marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.baseURL+"/api/meetings/join", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("speech2text submit request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("speech2text submit do: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Любой 2xx, не строго 201: форма успешного ответа живьём не проверена
	// (API.md §0). Если реальный API отвечает 200, жёсткий чек на 201 превратил бы
	// успешный Submit в «ошибку» → ретрай каждый тик → новый бот на ту же встречу
	// каждые 30 секунд, каждый жжёт платные минуты. Пустой id отсечётся ниже.
	if resp.StatusCode/100 != 2 {
		return "", s2tHTTPError("submit", resp)
	}

	var result struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("speech2text submit decode: %w", err)
	}
	if result.ID == "" {
		return "", fmt.Errorf("speech2text submit: empty id in response")
	}
	return result.ID, nil
}

// meetingAccessCode достаёт пароль встречи из query-параметра pwd (Zoom).
// Пустая строка — пароля нет или URL не парсится (валидация URL уже прошла в tg.go).
func meetingAccessCode(meetingURL string) string {
	u, err := url.Parse(meetingURL)
	if err != nil {
		return ""
	}
	return u.Query().Get("pwd")
}

// s2tStatusResp — минимальная структура ответа GET /api/recognitions/{id}.
// Неизвестные поля игнорируются (обычный Unmarshal): живьём контракт не проверялся (API.md §7).
type s2tStatusResp struct {
	Status struct {
		Value       string `json:"value"`
		Code        int    `json:"code"`
		Description string `json:"description"`
	} `json:"status"`
}

// Poll запрашивает статус задачи и маппит его в ProviderStatus.
// Маппинг строго по API.md §2.5/§9.
func (p *Speech2TextProvider) Poll(ctx context.Context, externalJobID string) (ProviderStatus, error) {
	// PathEscape: id приходит от поставщика, но мусорный id со слэшем молча
	// превратился бы в другой маршрут.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		p.baseURL+"/api/recognitions/"+url.PathEscape(externalJobID), nil)
	if err != nil {
		return StatusPending, fmt.Errorf("speech2text poll request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return StatusPending, fmt.Errorf("speech2text poll do: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return StatusPending, s2tHTTPError("poll", resp)
	}

	var result s2tStatusResp
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return StatusPending, fmt.Errorf("speech2text poll decode: %w", err)
	}

	status, err := mapS2TStatus(result.Status.Value, result.Status.Code, result.Status.Description)
	p.logPollStatus(externalJobID, result.Status.Value, result.Status.Code, result.Status.Description, status)
	return status, err
}

func (p *Speech2TextProvider) logPollStatus(externalJobID, value string, code int, description string, status ProviderStatus) {
	key := fmt.Sprintf("%s/%d/%s", value, code, description)
	p.mu.Lock()
	if p.lastPoll[externalJobID] == key {
		p.mu.Unlock()
		return
	}
	p.lastPoll[externalJobID] = key
	p.mu.Unlock()

	slog.Info("speech2text poll status changed",
		"event", "provider_poll_status", "phase", "poll",
		"provider", "speech2text", "external_job_id", externalJobID,
		"value", value, "code", code, "description", description,
		"provider_status", status.String())
}

// mapS2TStatus переводит пару value+code в ProviderStatus по API.md §2.5/§9.
// Неизвестное value → ошибка (оркестратор залогирует и ретраит), не молчаливый Pending:
// лучше шум в логах, чем вечно залипшая джоба без следа.
func mapS2TStatus(value string, code int, description string) (ProviderStatus, error) {
	switch value {
	case "queued":
		switch code {
		case 0:
			return StatusPending, nil
		case 30:
			desc := normalizeS2TDescription(description)
			switch {
			case strings.Contains(desc, "очеред"):
				return StatusQueuedRecording, nil
			case strings.Contains(desc, "присоедини"):
				return StatusJoiningCall, nil
			case strings.Contains(desc, "ведет запись"):
				return StatusOnCall, nil
			default:
				return StatusPending, fmt.Errorf("speech2text poll: unknown queued code 30 description %q", description)
			}
		default:
			return StatusPending, fmt.Errorf("speech2text poll: unknown queued code %d", code)
		}
	case "done":
		// code 200 — успех, code 204 — речь не обнаружена. Оба переходят в Done:
		// Fetch сам отличит пустой результат и вернёт ErrEmptyTranscript.
		return StatusDone, nil
	case "error", "cancelled":
		return StatusFailed, nil
	case "paused":
		// Нехватка минут на счёте (API.md §7). Оркестратор ждёт как Pending.
		return StatusPaused, nil
	default:
		// Дрейф контракта или новый статус поставщика — не паниковать,
		// но вернуть ошибку, чтобы оркестратор логировал дрейф и ретраил.
		return StatusPending, fmt.Errorf("speech2text poll: unknown status value %q (code %d)", value, code)
	}
}

func normalizeS2TDescription(description string) string {
	description = strings.ToLower(description)
	return strings.ReplaceAll(description, "ё", "е")
}

// Fetch забирает готовый результат двумя запросами (решение 9 спеки):
//   - result/txt → TranscriptMarkdown (plain text, доставляем юзеру)
//   - result/json → Raw (JSONB-колонка; колонка не примет plain text)
func (p *Speech2TextProvider) Fetch(ctx context.Context, externalJobID string) (Result, error) {
	txt, err := p.fetchFormat(ctx, externalJobID, "txt")
	if err != nil {
		return Result{}, err // префикс speech2text уже внутри fetchFormat
	}

	transcript := string(txt)
	// done code 204 — «речь не обнаружена» (API.md §2.5): s2t возвращает пустой body
	// или 204. TrimSpace: тело из одних переносов — тоже «пусто».
	if strings.TrimSpace(transcript) == "" {
		return Result{}, ErrEmptyTranscript
	}

	raw, err := p.fetchFormat(ctx, externalJobID, "json")
	if err != nil {
		// json не критичен для доставки, но ошибка не должна исчезнуть без следа:
		// Fetch возвращает nil-ошибку, и оркестратору нечего будет логировать.
		// Частичный результат: Raw = nil → orchestrator запишет NULL.
		slog.Warn("speech2text: result/json failed, delivering without raw",
			"err", err, "external_job_id", externalJobID)
		return Result{TranscriptMarkdown: transcript, Raw: nil}, nil
	}
	durationSeconds := s2tDurationSeconds(raw)
	// nil raw при пустом ответе — orchestrator кладёт NULL в JSONB.
	if len(raw) == 0 {
		raw = nil
	}

	return Result{TranscriptMarkdown: transcript, Raw: raw, DurationSeconds: durationSeconds}, nil
}

func s2tDurationSeconds(raw []byte) int {
	var result struct {
		Chunks []struct {
			Time struct {
				To *float64 `json:"to"`
			} `json:"time"`
		} `json:"chunks"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return 0
	}
	var maxTo float64
	for _, chunk := range result.Chunks {
		if chunk.Time.To == nil || *chunk.Time.To <= 0 {
			return 0
		}
		if *chunk.Time.To > maxTo {
			maxTo = *chunk.Time.To
		}
	}
	if maxTo <= 0 {
		return 0
	}
	return int(maxTo)
}

// fetchFormat делает GET /api/recognitions/{id}/result/{fmt} и возвращает тело.
// 204 No Content возвращает пустой []byte (вызывающий решает что делать с пустотой).
func (p *Speech2TextProvider) fetchFormat(ctx context.Context, externalJobID, format string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		p.baseURL+"/api/recognitions/"+url.PathEscape(externalJobID)+"/result/"+format, nil)
	if err != nil {
		return nil, fmt.Errorf("speech2text fetch %s request: %w", format, err)
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("speech2text fetch %s do: %w", format, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 204 = пусто, не ошибка (API.md §2.5 done code 204).
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, s2tHTTPError("fetch "+format, resp)
	}

	// Лимит против сбойного провайдера/прокси с гигантским телом: безлимитный
	// ReadAll положил бы единственный процесс по памяти. Транскрипт 3-часовой
	// встречи — единицы МБ, 32 MiB перекрывает с запасом.
	const maxResult = 32 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResult+1))
	if err != nil {
		return nil, fmt.Errorf("speech2text fetch %s read: %w", format, err)
	}
	if len(data) > maxResult {
		return nil, fmt.Errorf("speech2text fetch %s: response exceeds %d bytes", format, maxResult)
	}
	return data, nil
}

// Cancel останавливает запись досрочно: POST /api/meetings/{id}/stop (API.md §2.3).
// ВНИМАНИЕ: по уточнению поддержки 2026-06-17 для уже пишущего бота /stop ФИНАЛИЗИРУЕТ
// фрагмент с транскриптом, а не «отменяет без результата». Поэтому manual/watchdog
// stop для recording-джобы ждёт transcript и биллит фактическое время; refund остаётся
// только если результата нет. Best-effort: «задача не найдена» приходит как HTTP 400 (API.md §0) —
// для уже завершённой джобы ожидаемо, не молчим, отдаём ошибку, игнорировать — за вызывающим.
func (p *Speech2TextProvider) Cancel(ctx context.Context, externalJobID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.baseURL+"/api/meetings/"+url.PathEscape(externalJobID)+"/stop", nil)
	if err != nil {
		return fmt.Errorf("speech2text cancel request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("speech2text cancel do: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode/100 != 2 {
		return s2tHTTPError("cancel", resp)
	}
	return nil
}

// s2tHTTPError формирует ошибку с HTTP-кодом и усечённым телом ответа.
// Живой факт (API.md §0): ошибки s2t — JSON {"name","message","code","status"},
// «задача не найдена» приходит как HTTP 400, поэтому тело в ошибке — главный
// источник диагноза. API-ключ в тело не попадает (он в заголовке запроса).
// Свободная функция: состояние провайдера ей не нужно.
func s2tHTTPError(op string, resp *http.Response) error {
	const maxBody = 256
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	bodyText := string(body)
	if err != nil {
		bodyText = "read body: " + err.Error()
	}
	return ProviderHTTPError{
		Provider:   "speech2text",
		Op:         op,
		StatusCode: resp.StatusCode,
		Body:       bodyText,
	}
}
