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
	"time"
)

// Компайл-тайм ассерт: MymeetProvider реализует интерфейс.
// Дрейф сигнатуры поймает компилятор, а не runtime.
var _ TranscriptionProvider = (*MymeetProvider)(nil)

// MymeetProvider — резервный поставщик транскрибации (architecture.md §8).
// Горячий резерв на отказ Speech2Text: свап TRANSCRIPTION_PROVIDER=mymeet без
// пересборки. Формы ответов подтверждены живым E2E 2026-06-16 (docs/api/mymeet/,
// метки LIVE). Остаток открытого — форма failed, многоголосая диаризация (API.md §5).
type MymeetProvider struct {
	apiKey   string
	baseURL  string         // default https://backend.mymeet.ai
	template string         // template_name, default default-meeting
	tz       *time.Location // для local_date_time, default Europe/Moscow
	client   *http.Client   // таймаут снаружи через ctx; свой клиент (не дефолтный без таймаута)
}

// NewMymeet создаёт провайдер. apiKey никогда не попадает в логи и тексты ошибок.
func NewMymeet(apiKey, baseURL, template string, tz *time.Location) *MymeetProvider {
	return &MymeetProvider{
		apiKey:   apiKey,
		baseURL:  baseURL,
		template: template,
		tz:       tz,
		// Основной таймаут — ctx (providerTimeout в orchestrator.go). Timeout здесь —
		// страховка от будущих вызывающих с другим ctx, заведомо больше ctx-таймаута,
		// чтобы не срабатывать первым.
		client: &http.Client{Timeout: 60 * time.Second},
	}
}

// Submit отправляет бота на встречу, возвращает external id задачи у поставщика.
// POST /api/record-meeting, cron="" — записать прямо сейчас (API.md §1.1).
// source выводим из хоста, title/local_date_time генерим из текущего времени
// (mymeet требует их непустыми).
func (p *MymeetProvider) Submit(ctx context.Context, job Job) (string, error) {
	source, err := mymeetSource(job.MeetingURL)
	if err != nil {
		return "", fmt.Errorf("mymeet submit: %w", err)
	}

	now := time.Now().In(p.tz)
	payload := map[string]string{
		"link":            job.MeetingURL,
		"source":          source,
		"title":           "Встреча " + now.Format("2006-01-02 15:04"),
		"local_date_time": now.Format(time.RFC3339),
		"template_name":   p.template,
		"cron":            "", // пусто = записать прямо сейчас (API.md §1.1)
	}
	// meeting_password сознательно НЕ шлём. Инцидент 2026-06-17: бот mymeet не входит в
	// Zoom ни в какой форме пароля — поддержка mymeet подтвердила, что причина внешняя
	// (обновление Zoom от ~13.06 блокирует ботов, чинится на стороне mymeet, не у нас).
	// Угаданная форма (?pwd=-токен в meeting_password) делала хуже: 404-лимбо вместо
	// честного быстрого failed. Вернём поле, когда mymeet подтвердит правильный формат.
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("mymeet submit marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.baseURL+"/api/record-meeting", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("mymeet submit request: %w", err)
	}
	req.Header.Set("X-API-KEY", p.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("mymeet submit do: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 402 (нет минут) / 409 (уже запланирована) приходят сюда, на Submit, а не как
	// Poll-статус — известное ограничение резерва: submitJob ретраит вечно, квота
	// зарезервирована (план-слайс решение 6; коды — API.md §4). Форма 409 известна
	// (plain-text с meeting_id существующей задачи), переиспользование id — Этап 3.
	if resp.StatusCode/100 != 2 {
		return "", mymeetHTTPError("submit", resp)
	}

	const maxBody = 32 << 20
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return "", fmt.Errorf("mymeet submit read: %w", err)
	}
	if len(raw) > maxBody {
		return "", fmt.Errorf("mymeet submit: response exceeds %d bytes", maxBody)
	}

	// Форма ответа подтверждена живым тестом (API.md §1.1): плоский {"meeting_id"}.
	// Пустой meeting_id -> ошибка + лог сырого тела (без ключа — он в заголовке).
	var result struct {
		MeetingID string `json:"meeting_id"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("mymeet submit decode: %w", err)
	}
	if result.MeetingID == "" {
		// Лог сырого тела (без ключа — он в заголовке) для разбора неугаданной формы.
		slog.Error("mymeet submit: empty meeting_id in response", "body", string(raw))
		return "", fmt.Errorf("mymeet submit: empty meeting_id in response")
	}
	return result.MeetingID, nil
}

// Poll запрашивает статус задачи и маппит его в ProviderStatus.
// GET /api/meeting/status?meeting_id=<uuid>.
func (p *MymeetProvider) Poll(ctx context.Context, externalJobID string) (ProviderStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		p.baseURL+"/api/meeting/status?meeting_id="+url.QueryEscape(externalJobID), nil)
	if err != nil {
		return StatusPending, fmt.Errorf("mymeet poll request: %w", err)
	}
	req.Header.Set("X-API-KEY", p.apiKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return StatusPending, fmt.Errorf("mymeet poll do: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 404 на /status сразу после Submit — встреча ещё не появилась (бот ещё не зашёл
	// = в работе, API.md §1.2), НЕ сбой. Не ERROR-спам на штатном лаге захода бота.
	// Если бот так и не зайдёт, джобу добьёт orphan-timeout по maxJobAge (orchestrator).
	if resp.StatusCode == http.StatusNotFound {
		slog.Warn("mymeet poll: meeting not yet visible (bot joining or failed to join)",
			"external_job_id", externalJobID)
		return StatusJoiningCall, nil
	}
	if resp.StatusCode != http.StatusOK {
		return StatusPending, mymeetHTTPError("poll", resp)
	}

	const maxBody = 32 << 20
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return StatusPending, fmt.Errorf("mymeet poll read: %w", err)
	}
	if len(raw) > maxBody {
		return StatusPending, fmt.Errorf("mymeet poll: response exceeds %d bytes", maxBody)
	}

	// Форма Poll — plain-text голая строка статуса (API.md §1.2, LIVE). Нормализуем
	// робастно (TrimSpace + снять обрамляющие кавычки + lower-case) — на случай, если
	// часть статусов придёт в кавычках. JSON-обёртки нет, json.Decode не применяем.
	status := strings.ToLower(strings.Trim(strings.TrimSpace(string(raw)), `"`))
	return mapMymeetStatus(status)
}

// Fetch забирает готовый результат: GET /api/video/report. Транскрипт по спикерам —
// в followup_v2.chapters[].transcript[] (API.md §1.3, LIVE); саммари —
// в followup_v2.templates[] (рендерится в SummaryMarkdown). Весь ответ -> Raw (JSONB).
func (p *MymeetProvider) Fetch(ctx context.Context, externalJobID string) (Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		p.baseURL+"/api/video/report?meeting_id="+url.QueryEscape(externalJobID), nil)
	if err != nil {
		return Result{}, fmt.Errorf("mymeet fetch request: %w", err)
	}
	req.Header.Set("X-API-KEY", p.apiKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("mymeet fetch do: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return Result{}, mymeetHTTPError("fetch", resp)
	}

	const maxBody = 32 << 20
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return Result{}, fmt.Errorf("mymeet fetch read: %w", err)
	}
	if len(raw) > maxBody {
		return Result{}, fmt.Errorf("mymeet fetch: response exceeds %d bytes", maxBody)
	}

	// Транскрипт по спикерам — в followup_v2.chapters[].transcript[] (API.md §1.3, LIVE);
	// top-level transcript у report НЕТ. Декод + guard'ы — внутри mymeetTranscriptMarkdown.
	transcript := mymeetTranscriptMarkdown(raw)

	// Raw обязан быть валидным JSON (колонка JSONB). Иначе nil -> orchestrator пишет NULL.
	var rawJSON []byte
	if json.Valid(raw) {
		rawJSON = raw
	}

	return Result{
		TranscriptMarkdown: transcript,
		Raw:                rawJSON,
		SummaryMarkdown:    mymeetSummaryMarkdown(raw),
		DurationSeconds:    mymeetDurationSeconds(raw),
	}, nil
}

// Структуры разбора /api/video/report (API.md §1.3). followup_v2 — указатель,
// чтобы отличить его отсутствие (сломанная/чужая форма) от пустого транскрипта.
type mymeetReport struct {
	Followup *mymeetFollowup `json:"followup_v2"`
}

type mymeetFollowup struct {
	Chapters    []mymeetChapter    `json:"chapters"`
	Templates   []mymeetTemplate   `json:"templates"`
	WordTimings []mymeetWordTiming `json:"word_timings"`
}

// mymeetTemplate — один шаблон обработки (default-meeting, tasks и др.).
// Имена шаблонов/секций дрейфуют между версиями mymeet — парсер не зависит от них.
type mymeetTemplate struct {
	Entities []mymeetEntity `json:"entities"`
}

// mymeetEntity — одна секция шаблона (обзор, задачи и т.п.).
// Text — json.RawMessage, потому что тип зависит от is_json:
// false -> строка готового markdown, true -> объект {"actions":[...]}.
type mymeetEntity struct {
	Name   string          `json:"name"`
	IsJSON bool            `json:"is_json"`
	Text   json.RawMessage `json:"text"`
}

// mymeetTasks — форма entity с is_json=true (список задач).
type mymeetTasks struct {
	Actions []mymeetAction `json:"actions"`
}

type mymeetAction struct {
	Name string `json:"name"`
	Done bool   `json:"done"`
}

type mymeetWordTiming struct {
	EndTime float64 `json:"end_time"`
}

type mymeetChapter struct {
	Name       string          `json:"name"`
	Transcript []mymeetSegment `json:"transcript"`
}

type mymeetSegment struct {
	Speaker   mymeetSpeaker `json:"speaker"`
	Text      string        `json:"text"`
	Timestamp string        `json:"timestamp"`
}

type mymeetSpeaker struct {
	// Отображаемое имя участника звонка (API.md §1.3). Обезличивание (антискоуп) —
	// продуктовое решение на уровне Result/доставки перед публичным запуском, не здесь.
	Speaker string `json:"speaker"`
}

// mymeetTranscriptMarkdown рендерит транскрипт по спикерам из ответа /api/video/report.
// Три guard'а (зеркало speech2text.go: юзеру не утекает ни пустой файл, ни сырой JSON):
//   - не JSON / нет followup_v2 (сломанная/чужая форма) -> «не удалось разобрать» + Error
//   - структура ок, но речи нет (нет глав / все text пустые)  -> «речь не обнаружена»
//   - иначе -> markdown: на главу «## name» (если непусто), сегмент «[timestamp] speaker: text»
func mymeetTranscriptMarkdown(raw []byte) string {
	var report mymeetReport
	if err := json.Unmarshal(raw, &report); err != nil || report.Followup == nil {
		// Сырой ответ в Raw для разбора; юзеру — осмысленная заглушка, не JSON-дамп.
		slog.Error("mymeet fetch: cannot parse report (followup_v2 missing)", "err", err)
		return "_Не удалось разобрать ответ поставщика. Обратитесь в поддержку._"
	}

	var blocks []string
	for _, ch := range report.Followup.Chapters {
		var lines []string
		for _, seg := range ch.Transcript {
			if strings.TrimSpace(seg.Text) == "" {
				continue // пустой сегмент пропускаем (тишина/служебный)
			}
			lines = append(lines, fmt.Sprintf("[%s] %s: %s", seg.Timestamp, seg.Speaker.Speaker, seg.Text))
		}
		if len(lines) == 0 {
			continue // глава без реального текста — пропускаем вместе с заголовком
		}
		if name := strings.TrimSpace(ch.Name); name != "" {
			blocks = append(blocks, "## "+name+"\n"+strings.Join(lines, "\n"))
		} else {
			blocks = append(blocks, strings.Join(lines, "\n"))
		}
	}
	if len(blocks) == 0 {
		// followup_v2 есть, но речи нет (пустые сегменты / нет глав) — зеркало s2t.
		return "_Речь на встрече не обнаружена._"
	}
	return strings.Join(blocks, "\n\n")
}

func mymeetDurationSeconds(raw []byte) int {
	var report mymeetReport
	if err := json.Unmarshal(raw, &report); err != nil || report.Followup == nil {
		return 0
	}

	var maxEnd float64
	for _, word := range report.Followup.WordTimings {
		if word.EndTime > maxEnd {
			maxEnd = word.EndTime
		}
	}
	return int(maxEnd)
}

// mymeetSummaryMarkdown рендерит саммари из followup_v2.templates[].entities[].
// Имя-агностичный парсер: ветвится ТОЛЬКО по is_json, имена секций игнорирует
// (они дрейфуют между версиями mymeet — зафиксировано живой пробой 2026-06-18).
// is_json=false -> готовый markdown-блок; is_json=true -> чек-лист задач.
// Пустая строка = «саммари нет» — оркестратор уйдёт штатным путём (Summarizer).
func mymeetSummaryMarkdown(raw []byte) string {
	var report mymeetReport
	if err := json.Unmarshal(raw, &report); err != nil || report.Followup == nil {
		return ""
	}

	var summaryBlocks []string
	var taskLines []string

	for _, tmpl := range report.Followup.Templates {
		for _, e := range tmpl.Entities {
			if !e.IsJSON {
				var s string
				if err := json.Unmarshal(e.Text, &s); err != nil {
					continue
				}
				if t := strings.TrimSpace(s); t != "" {
					summaryBlocks = append(summaryBlocks, t)
				}
			} else {
				var tasks mymeetTasks
				if err := json.Unmarshal(e.Text, &tasks); err != nil {
					continue // непарсимый is_json=true — пропускаем, не суём сырой JSON
				}
				for _, a := range tasks.Actions {
					if name := strings.TrimSpace(a.Name); name != "" {
						mark := "- [ ] "
						if a.Done {
							mark = "- [x] "
						}
						taskLines = append(taskLines, mark+name)
					}
				}
			}
		}
	}

	if len(summaryBlocks) == 0 && len(taskLines) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("# Саммари встречи\n\n")
	b.WriteString(strings.Join(summaryBlocks, "\n\n"))
	if len(taskLines) > 0 {
		if len(summaryBlocks) > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("## Задачи\n")
		b.WriteString(strings.Join(taskLines, "\n"))
	}
	return b.String()
}

// Cancel: программной отмены активной записи у mymeet нет (план-слайс решение 2,
// API.md §1.3/§5). LIVE-подтверждено: DELETE /api/video/report активную запись НЕ
// прерывает (404 «Meeting not found»), а роута DELETE /api/delete-meeting не
// существует. delete не дёргаем (это удаление данных, не остановка). Бот жжёт
// минуты mymeet до конца встречи; квота юзеру возвращается вызывающим.
func (p *MymeetProvider) Cancel(ctx context.Context, externalJobID string) error {
	// Эндпоинт остановки бота — за письмом mymeet (kick / кнопка ЛК ~июль). До тех пор
	// orphan-recovery деградирован (джоба в БД фейлится, бот не останавливается).
	slog.Warn("mymeet: cancel not supported, bot keeps recording until meeting ends",
		"external_job_id", externalJobID)
	return fmt.Errorf("mymeet: cancel not documented (no stop endpoint)")
}

// mymeetSource выводит source по хосту встречи. Значения — из openapi.json (API.md
// §1.1): живой тест 2026-06-16 показал, что Телемост принимает только yandextelemost;
// telemost из MCP-клиента backend проглатывает с 200, но бота не ставит. Неизвестный
// хост -> ошибка: бота в никуда не шлём, fallback gmeet из MCP не зеркалим.
func mymeetSource(meetingURL string) (string, error) {
	u, err := url.Parse(meetingURL)
	if err != nil {
		return "", fmt.Errorf("mymeet source: parse url: %w", err)
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case host == "meet.google.com":
		return "gmeet", nil
	case host == "zoom.us" || host == "zoom.com" ||
		strings.HasSuffix(host, ".zoom.us") || strings.HasSuffix(host, ".zoom.com"):
		return "zoom", nil
	case strings.Contains(host, "telemost.yandex"):
		// yandextelemost (openapi), НЕ telemost (MCP): LIVE-подтверждено — с telemost
		// бот не заходит (API.md §1.1).
		return "yandextelemost", nil
	default:
		return "", fmt.Errorf("mymeet source: unsupported host %q", host)
	}
}

// mapMymeetStatus переводит сырой статус mymeet в ProviderStatus (API.md §1.2, LIVE
// 2026-06-16; connecting добавлен LIVE 2026-06-17). Жизненный цикл фаз: connecting ->
// recording -> processing -> new(готово) -> [processed после просмотра] разворачивается
// на статусы джобы один-к-одному: connecting/queued (бот подключается) -> JoiningCall (Job
// joining); recording (бот пишет звонок) -> OnCall (Job recording); processing (бот вышел,
// фоновая расшифровка) -> Transcribing (Job processing). ВАЖНО: new = ГОТОВО к Fetch,
// не «в очереди» (mymeet сам в processed не уходит). Неизвестный статус -> ошибка
// (оркестратор залогирует дрейф и ретраит): лучше шум в логах, чем молча пропустить
// неопознанный терминальный статус (форма failed пока неизвестна). StatusPaused недостижим:
// нехватка минут mymeet — это HTTP 402 на Submit (API.md §4), а не Poll-статус.
func mapMymeetStatus(raw string) (ProviderStatus, error) {
	switch raw {
	case "connecting", "queued":
		return StatusJoiningCall, nil
	case "recording":
		return StatusOnCall, nil
	case "processing":
		return StatusTranscribing, nil
	case "new", "processed":
		return StatusDone, nil
	case "failed":
		return StatusFailed, nil
	default:
		return StatusPending, fmt.Errorf("mymeet poll: unknown status %q", raw)
	}
}

// mymeetHTTPError формирует ошибку с HTTP-кодом и усечённым телом ответа.
// API-ключ в тело не попадает (он в заголовке запроса). Свободная функция:
// состояние провайдера ей не нужно.
func mymeetHTTPError(op string, resp *http.Response) error {
	const maxBody = 256
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	bodyText := string(body)
	if err != nil {
		bodyText = "read body: " + err.Error()
	}
	return ProviderHTTPError{
		Provider:   "mymeet",
		Op:         op,
		StatusCode: resp.StatusCode,
		Body:       bodyText,
	}
}
