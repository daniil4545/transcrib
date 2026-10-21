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

// Compile-time assertion: Speech2TextProvider implements the interface.
// Signature drift is caught by the compiler, not at runtime.
var _ TranscriptionProvider = (*Speech2TextProvider)(nil)

// Speech2TextProvider is the primary transcription provider (architecture.md §8).
// It sends the bot to a meeting via the Meeting API and picks up the result when ready.
type Speech2TextProvider struct {
	apiKey   string
	baseURL  string       // default https://api.speech2text.ru
	client   *http.Client // timeout comes from ctx outside; our own client (not the default one without a timeout)
	mu       sync.Mutex
	lastPoll map[string]string
}

// NewSpeech2Text creates the provider. apiKey never appears in logs or error messages.
func NewSpeech2Text(apiKey, baseURL string) *Speech2TextProvider {
	return &Speech2TextProvider{
		apiKey:  apiKey,
		baseURL: baseURL,
		// The main timeout is ctx (providerTimeout in orchestrator.go). The Timeout here is
		// a safeguard against future callers with a different ctx (webhook handler in web.go),
		// deliberately larger than the ctx timeout so it does not fire first.
		client:   &http.Client{Timeout: 60 * time.Second},
		lastPoll: make(map[string]string),
	}
}

// Submit sends the bot to a meeting and returns the provider's external task id.
// POST /api/meetings/join: minimal payload.
func (p *Speech2TextProvider) Submit(ctx context.Context, job Job) (string, error) {
	payload := map[string]string{"meeting_url": job.MeetingURL}
	// bot_name is the display name of the bot participant in the meeting (brand; contract API.md §2.2).
	payload["bot_name"] = "transcrib"
	// Zoom puts the password in the query (?pwd=...), but s2t expects it as a separate
	// access_code field (support reply, 2026-06-11); without it the bot will not enter the meeting.
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

	// Any 2xx, not strictly 201: the shape of a successful response was not verified live
	// (API.md §0). If the real API answers 200, a hard check for 201 would turn
	// a successful Submit into an "error", then a retry every tick, a new bot in the same meeting
	// every 30 seconds, each burning paid minutes. An empty id is rejected below.
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

// meetingAccessCode extracts the meeting password from the pwd query parameter (Zoom).
// An empty string means there is no password or the URL does not parse (URL validation already passed in tg.go).
func meetingAccessCode(meetingURL string) string {
	u, err := url.Parse(meetingURL)
	if err != nil {
		return ""
	}
	return u.Query().Get("pwd")
}

// s2tStatusResp is the minimal response structure of GET /api/recognitions/{id}.
// Unknown fields are ignored (ordinary Unmarshal): the contract was not verified live (API.md §7).
type s2tStatusResp struct {
	Status struct {
		Value       string `json:"value"`
		Code        int    `json:"code"`
		Description string `json:"description"`
	} `json:"status"`
}

// Poll requests the task status and maps it to ProviderStatus.
// The mapping follows API.md §2.5/§9 strictly.
func (p *Speech2TextProvider) Poll(ctx context.Context, externalJobID string) (ProviderStatus, error) {
	// PathEscape: the id comes from the provider, but a junk id with a slash would silently
	// turn into a different route.
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

// mapS2TStatus maps a value+code pair to ProviderStatus per API.md §2.5/§9.
// An unknown value gives an error (the orchestrator logs and retries), not a silent Pending:
// better log noise than a forever-stuck job with no trace.
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
		// code 200 is success, code 204 is no speech detected. Both go to Done:
		// Fetch itself tells an empty result apart and returns ErrEmptyTranscript.
		return StatusDone, nil
	case "error", "cancelled":
		return StatusFailed, nil
	case "paused":
		// Not enough minutes on the account (API.md §7). The orchestrator waits as for Pending.
		return StatusPaused, nil
	default:
		// Contract drift or a new provider status: do not panic,
		// but return an error so the orchestrator logs the drift and retries.
		return StatusPending, fmt.Errorf("speech2text poll: unknown status value %q (code %d)", value, code)
	}
}

func normalizeS2TDescription(description string) string {
	description = strings.ToLower(description)
	return strings.ReplaceAll(description, "ё", "е")
}

// Fetch picks up the finished result with two requests (spec decision 9):
//   - result/txt -> TranscriptMarkdown (plain text, delivered to the user)
//   - result/json -> Raw (JSONB column; the column does not accept plain text)
func (p *Speech2TextProvider) Fetch(ctx context.Context, externalJobID string) (Result, error) {
	txt, err := p.fetchFormat(ctx, externalJobID, "txt")
	if err != nil {
		return Result{}, err // the speech2text prefix is already inside fetchFormat
	}

	transcript := string(txt)
	// done code 204 is "no speech detected" (API.md §2.5): s2t returns an empty body
	// or 204. TrimSpace: a body of only newlines is also "empty".
	if strings.TrimSpace(transcript) == "" {
		return Result{}, ErrEmptyTranscript
	}

	raw, err := p.fetchFormat(ctx, externalJobID, "json")
	if err != nil {
		// json is not critical for delivery, but the error must not vanish without a trace:
		// Fetch returns a nil error, and the orchestrator would have nothing to log.
		// Partial result: Raw = nil, so the orchestrator writes NULL.
		slog.Warn("speech2text: result/json failed, delivering without raw",
			"err", err, "external_job_id", externalJobID)
		return Result{TranscriptMarkdown: transcript, Raw: nil}, nil
	}
	durationSeconds := s2tDurationSeconds(raw)
	// nil raw on an empty response: the orchestrator puts NULL into JSONB.
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

// fetchFormat does GET /api/recognitions/{id}/result/{fmt} and returns the body.
// 204 No Content returns an empty []byte (the caller decides what to do with emptiness).
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

	// 204 = empty, not an error (API.md §2.5 done code 204).
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, s2tHTTPError("fetch "+format, resp)
	}

	// Limit against a faulty provider/proxy with a giant body: an unlimited
	// ReadAll would take down the only process on memory. A transcript of a 3-hour
	// meeting is a few MB; 32 MiB covers it with a margin.
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

// Cancel stops recording early: POST /api/meetings/{id}/stop (API.md §2.3).
// NOTE: per the support clarification of 2026-06-17, for a bot already recording /stop FINALIZES
// the fragment with a transcript, rather than "cancelling with no result". So manual/watchdog
// stop for a recording job waits for the transcript and bills the actual time; refund remains
// only if there is no result. Best-effort: "task not found" arrives as HTTP 400 (API.md §0);
// expected for an already finished job, we do not stay silent, we return the error, ignoring it is up to the caller.
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

// s2tHTTPError builds an error with the HTTP code and a truncated response body.
// Live fact (API.md §0): s2t errors are JSON {"name","message","code","status"},
// "task not found" arrives as HTTP 400, so the body in the error is the main
// source of diagnosis. The API key does not get into the body (it is in the request header).
// A free function: it does not need provider state.
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
