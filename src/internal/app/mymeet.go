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

// Compile-time assertion: MymeetProvider implements the interface.
// Signature drift is caught by the compiler, not at runtime.
var _ TranscriptionProvider = (*MymeetProvider)(nil)

// MymeetProvider is the fallback transcription provider (architecture.md §8).
// A hot standby for Speech2Text failure: swap TRANSCRIPTION_PROVIDER=mymeet without
// rebuilding. Response shapes confirmed by a live E2E on 2026-06-16 (docs/api/mymeet/,
// LIVE marks). What remains open: the failed shape, multi-voice diarization (API.md §5).
type MymeetProvider struct {
	apiKey   string
	baseURL  string         // default https://backend.mymeet.ai
	template string         // template_name, default default-meeting
	tz       *time.Location // for local_date_time, default Europe/Moscow
	client   *http.Client   // timeout comes from ctx outside; our own client (not the default one without a timeout)
}

// NewMymeet creates the provider. apiKey never appears in logs or error messages.
func NewMymeet(apiKey, baseURL, template string, tz *time.Location) *MymeetProvider {
	return &MymeetProvider{
		apiKey:   apiKey,
		baseURL:  baseURL,
		template: template,
		tz:       tz,
		// The main timeout is ctx (providerTimeout in orchestrator.go). The Timeout here is
		// a safeguard against future callers with a different ctx, deliberately larger than the ctx timeout,
		// so it does not fire first.
		client: &http.Client{Timeout: 60 * time.Second},
	}
}

// Submit sends the bot to a meeting and returns the provider's external task id.
// POST /api/record-meeting, cron="" means record right now (API.md §1.1).
// source is derived from the host, title/local_date_time are generated from the current time
// (mymeet requires them non-empty).
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
		"cron":            "", // empty = record right now (API.md §1.1)
	}
	// We deliberately do NOT send meeting_password. Incident 2026-06-17: the mymeet bot does not enter
	// Zoom in any form of the password; mymeet support confirmed the cause is external
	// (a Zoom update from ~06-13 blocks bots, to be fixed on the mymeet side, not ours).
	// A guessed form (a ?pwd= token in meeting_password) made it worse: a 404 limbo instead of
	// an honest fast failed. We will bring the field back when mymeet confirms the correct format.
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

	// 402 (no minutes) / 409 (already scheduled) arrive here, at Submit, not as a
	// Poll status: a known limitation of the fallback: submitJob retries forever, the quota is
	// reserved (plan-slice decision 6; codes in API.md §4). The 409 shape is known
	// (plain text with the meeting_id of the existing task), reusing the id is Stage 3.
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

	// The response shape is confirmed by a live test (API.md §1.1): a flat {"meeting_id"}.
	// An empty meeting_id -> error + log of the raw body (without the key, it is in the header).
	var result struct {
		MeetingID string `json:"meeting_id"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("mymeet submit decode: %w", err)
	}
	if result.MeetingID == "" {
		// Log the raw body (without the key, it is in the header) to analyze an unguessed shape.
		slog.Error("mymeet submit: empty meeting_id in response", "body", string(raw))
		return "", fmt.Errorf("mymeet submit: empty meeting_id in response")
	}
	return result.MeetingID, nil
}

// Poll requests the task status and maps it to ProviderStatus.
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

	// 404 on /status right after Submit means the meeting has not appeared yet (the bot has not joined yet
	// = in progress, API.md §1.2), NOT a failure. No ERROR spam on the normal bot join lag.
	// If the bot never joins, the job is finished off by the orphan-timeout on maxJobAge (orchestrator).
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

	// The Poll shape is a plain-text bare status string (API.md §1.2, LIVE). We normalize
	// robustly (TrimSpace + strip surrounding quotes + lower-case) in case
	// some statuses arrive in quotes. There is no JSON wrapper, so json.Decode is not applied.
	status := strings.ToLower(strings.Trim(strings.TrimSpace(string(raw)), `"`))
	return mapMymeetStatus(status)
}

// Fetch picks up the finished result: GET /api/video/report. The transcript by speaker is
// in followup_v2.chapters[].transcript[] (API.md §1.3, LIVE); the summary is
// in followup_v2.templates[] (rendered into SummaryMarkdown). The whole response -> Raw (JSONB).
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

	// The transcript by speaker is in followup_v2.chapters[].transcript[] (API.md §1.3, LIVE);
	// the report has NO top-level transcript. Decode + guards are inside mymeetTranscriptMarkdown.
	transcript := mymeetTranscriptMarkdown(raw)

	// Raw must be valid JSON (JSONB column). Otherwise nil -> the orchestrator writes NULL.
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

// Parsing structures for /api/video/report (API.md §1.3). followup_v2 is a pointer,
// to tell its absence (a broken/foreign shape) from an empty transcript.
type mymeetReport struct {
	Followup *mymeetFollowup `json:"followup_v2"`
}

type mymeetFollowup struct {
	Chapters    []mymeetChapter    `json:"chapters"`
	Templates   []mymeetTemplate   `json:"templates"`
	WordTimings []mymeetWordTiming `json:"word_timings"`
}

// mymeetTemplate is one processing template (default-meeting, tasks, etc.).
// Template/section names drift between mymeet versions; the parser does not depend on them.
type mymeetTemplate struct {
	Entities []mymeetEntity `json:"entities"`
}

// mymeetEntity is one template section (overview, tasks, etc.).
// Text is json.RawMessage because the type depends on is_json:
// false -> a string of ready markdown, true -> an object {"actions":[...]}.
type mymeetEntity struct {
	Name   string          `json:"name"`
	IsJSON bool            `json:"is_json"`
	Text   json.RawMessage `json:"text"`
}

// mymeetTasks is the entity shape with is_json=true (a task list).
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
	// The display name of the call participant (API.md §1.3). Anonymization (anti-scope) is
	// a product decision at the Result/delivery level before the public launch, not here.
	Speaker string `json:"speaker"`
}

// mymeetTranscriptMarkdown renders the transcript by speaker from the /api/video/report response.
// Three guards (a mirror of speech2text.go: neither an empty file nor raw JSON leaks to the user):
//   - not JSON / no followup_v2 (broken/foreign shape) -> "could not parse" + Error
//   - structure ok but no speech (no chapters / all text empty)  -> "no speech detected"
//   - otherwise -> markdown: "## name" per chapter (if non-empty), segment "[timestamp] speaker: text"
func mymeetTranscriptMarkdown(raw []byte) string {
	var report mymeetReport
	if err := json.Unmarshal(raw, &report); err != nil || report.Followup == nil {
		// Raw response goes to Raw for analysis; the user gets a meaningful stub, not a JSON dump.
		slog.Error("mymeet fetch: cannot parse report (followup_v2 missing)", "err", err)
		return "_Не удалось разобрать ответ поставщика. Обратитесь в поддержку._"
	}

	var blocks []string
	for _, ch := range report.Followup.Chapters {
		var lines []string
		for _, seg := range ch.Transcript {
			if strings.TrimSpace(seg.Text) == "" {
				continue // skip an empty segment (silence/service)
			}
			lines = append(lines, fmt.Sprintf("[%s] %s: %s", seg.Timestamp, seg.Speaker.Speaker, seg.Text))
		}
		if len(lines) == 0 {
			continue // a chapter without real text is skipped together with its heading
		}
		if name := strings.TrimSpace(ch.Name); name != "" {
			blocks = append(blocks, "## "+name+"\n"+strings.Join(lines, "\n"))
		} else {
			blocks = append(blocks, strings.Join(lines, "\n"))
		}
	}
	if len(blocks) == 0 {
		// followup_v2 is present, but there is no speech (empty segments / no chapters): a mirror of s2t.
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

// mymeetSummaryMarkdown renders the summary from followup_v2.templates[].entities[].
// A name-agnostic parser: branches ONLY on is_json, ignores section names
// (they drift between mymeet versions, recorded by a live probe on 2026-06-18).
// is_json=false -> a ready markdown block; is_json=true -> a task checklist.
// An empty string = "no summary": the orchestrator takes the normal path (Summarizer).
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
					continue // unparseable is_json=true: skip, do not push raw JSON
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

// Cancel: mymeet has no programmatic cancellation of an active recording (plan-slice decision 2,
// API.md §1.3/§5). LIVE-confirmed: DELETE /api/video/report does NOT interrupt
// an active recording (404 "Meeting not found"), and the route DELETE /api/delete-meeting
// does not exist. We do not call delete (it deletes data, it does not stop). The bot burns
// mymeet minutes until the end of the meeting; the user's quota is returned by the caller.
func (p *MymeetProvider) Cancel(ctx context.Context, externalJobID string) error {
	// The bot stop endpoint is pending a mymeet reply (kick / account button, ~July). Until then
	// orphan-recovery is degraded (the job fails in the DB, the bot is not stopped).
	slog.Warn("mymeet: cancel not supported, bot keeps recording until meeting ends",
		"external_job_id", externalJobID)
	return fmt.Errorf("mymeet: cancel not documented (no stop endpoint)")
}

// mymeetSource derives source from the meeting host. Values are from openapi.json (API.md
// §1.1): the live test on 2026-06-16 showed that Telemost accepts only yandextelemost;
// telemost from the MCP client is swallowed by the backend with 200, but no bot is placed. An unknown
// host -> error: we do not send a bot into nowhere, we do not mirror the MCP gmeet fallback.
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
		// yandextelemost (openapi), NOT telemost (MCP): LIVE-confirmed, with telemost
		// the bot does not join (API.md §1.1).
		return "yandextelemost", nil
	default:
		return "", fmt.Errorf("mymeet source: unsupported host %q", host)
	}
}

// mapMymeetStatus maps a raw mymeet status to ProviderStatus (API.md §1.2, LIVE
// 2026-06-16; connecting added LIVE 2026-06-17). Phase lifecycle: connecting ->
// recording -> processing -> new(ready) -> [processed after viewing] maps
// to job statuses one to one: connecting/queued (bot connecting) -> JoiningCall (Job
// joining); recording (bot is recording the call) -> OnCall (Job recording); processing (bot left,
// background transcription) -> Transcribing (Job processing). IMPORTANT: new = READY for Fetch,
// not "queued" (mymeet does not move to processed on its own). An unknown status -> error
// (the orchestrator logs the drift and retries): better log noise than silently missing
// an unrecognized terminal status (the failed shape is still unknown). StatusPaused is unreachable:
// mymeet's lack of minutes is HTTP 402 on Submit (API.md §4), not a Poll status.
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

// mymeetHTTPError builds an error with the HTTP code and a truncated response body.
// The API key does not get into the body (it is in the request header). A free function:
// it does not need provider state.
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
