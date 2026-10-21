package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestMapMymeetStatus pins the mymeet status mapping contract (API.md §1.2,
// plan slice "Critical nodes under test") against drift. An unknown status must
// produce an error, not a silent Pending, otherwise the job hangs forever without a trace.
func TestMapMymeetStatus(t *testing.T) {
	// Success branches: known status -> specific ProviderStatus (LIVE mapping
	// API.md §1.2). The phases are expanded: JoiningCall (connecting/queued, the bot is connecting),
	// OnCall (recording, the bot is recording), Transcribing (processing, the bot has left), Done
	// (new/processed), Failed.
	ok := []struct {
		value string
		want  ProviderStatus
	}{
		{"new", StatusDone},                // LIVE: new = READY for Fetch, not "queued"
		{"recording", StatusOnCall},        // LIVE: the bot records the call -> Job recording
		{"connecting", StatusJoiningCall},  // LIVE 2026-06-17: the bot is connecting (phase before recording)
		{"queued", StatusJoiningCall},      // the bot is connecting / queued
		{"processing", StatusTranscribing}, // the bot has left, background processing -> Job processing
		{"processed", StatusDone},
		{"failed", StatusFailed},
	}
	for _, tt := range ok {
		t.Run("ok/"+tt.value, func(t *testing.T) {
			got, err := mapMymeetStatus(tt.value)
			if err != nil {
				t.Fatalf("mapMymeetStatus(%q): unexpected error %v", tt.value, err)
			}
			if got != tt.want {
				t.Errorf("mapMymeetStatus(%q) = %d, want %d", tt.value, got, tt.want)
			}
		})
	}

	// Negative branches (contract drift): assert only that an error occurs. StatusPaused is
	// unreachable for mymeet (lack of minutes is HTTP 402 on Submit, not a Poll status), so
	// "paused" is an unknown status like any other.
	bad := []string{
		"paused", // mymeet never returns it on Poll -> must be unknown, not StatusPaused
		"shiny-new-status",
		"done",       // s2t vocabulary, not mymeet -> must not map silently
		"PROCESSING", // Poll lowercases the status (ToLower), so only lower case arrives here; upper case = unknown
		"",
	}
	for _, value := range bad {
		t.Run("unknown/"+value, func(t *testing.T) {
			got, err := mapMymeetStatus(value)
			if err == nil {
				t.Errorf("mapMymeetStatus(%q): want a contract drift error, got nil", value)
			}
			// The negative case checks robustness: on error the status must NOT
			// be Done/Failed (that would cause a false delivery/rollback). Pending
			// with err is a safe detail (the orchestrator looks at err first).
			if got == StatusDone || got == StatusFailed {
				t.Errorf("mapMymeetStatus(%q) returned terminal %d on error: risk of false delivery/rollback", value, got)
			}
		})
	}
}

// TestMymeetSource pins the host -> source mapping (openapi.json values, API.md
// §1.1). Boundary values: exact hosts, zoom subdomains, Telemost -> yandextelemost
// (LIVE: NOT telemost, the bot cannot join with it). Unknown host -> error: we do not
// send the bot nowhere (it burns mymeet minutes).
func TestMymeetSource(t *testing.T) {
	ok := []struct {
		name string
		url  string
		want string
	}{
		{"gmeet", "https://meet.google.com/abc-defg-hij", "gmeet"},
		{"zoom_us", "https://zoom.us/j/123", "zoom"},
		{"zoom_com", "https://zoom.com/j/123", "zoom"},
		{"zoom_subdomain", "https://us05web.zoom.us/j/89978292641?pwd=x", "zoom"},
		{"zoom_com_subdomain", "https://company.zoom.com/j/123", "zoom"},
		{"telemost", "https://telemost.yandex.ru/j/18989266226692", "yandextelemost"},
	}
	for _, tt := range ok {
		t.Run("ok/"+tt.name, func(t *testing.T) {
			got, err := mymeetSource(tt.url)
			if err != nil {
				t.Fatalf("mymeetSource(%q): unexpected error %v", tt.url, err)
			}
			if got != tt.want {
				t.Errorf("mymeetSource(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}

	// Negative: unknown/empty host -> error (not an empty source, not a gmeet fallback).
	bad := []struct {
		name string
		url  string
	}{
		{"unknown_host", "https://teams.microsoft.com/l/meetup-join/xyz"},
		{"webex", "https://example.webex.com/meet/room"},
		{"empty_host", "/just/a/path"},
		{"bare_word_no_host", "meet.google.com/abc"}, // no scheme -> Hostname() is empty
	}
	for _, tt := range bad {
		t.Run("err/"+tt.name, func(t *testing.T) {
			got, err := mymeetSource(tt.url)
			if err == nil {
				t.Errorf("mymeetSource(%q): want an error, got source=%q", tt.url, got)
			}
		})
	}
}

// TestMymeetPoll_Normalize checks OUR normalization of the Poll response
// (TrimSpace + stripping surrounding quotes + lower-case). LIVE: the status arrives as
// a bare plain-text string; normalization also covers the quoted form, and both must
// yield the same ProviderStatus.
func TestMymeetPoll_Normalize(t *testing.T) {
	tests := []struct {
		name string
		body string // what the test backend returns in the /api/meeting/status body
		want ProviderStatus
	}{
		{"plain_processing", "processing", StatusTranscribing},
		{"json_quoted_processing", `"processing"`, StatusTranscribing},
		{"json_quoted_with_whitespace", "  \"processing\"\n", StatusTranscribing},
		{"plain_uppercase", "PROCESSING", StatusTranscribing}, // lower-case is our normalization
		{"json_quoted_processed", `"processed"`, StatusDone},
		{"plain_failed_trailing_newline", "failed\n", StatusFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, err := w.Write([]byte(tt.body)); err != nil {
					t.Errorf("test server write: %v", err)
				}
			}))
			t.Cleanup(srv.Close)

			p := NewMymeet("test-key", srv.URL, "default-meeting", time.UTC)
			got, err := p.Poll(context.Background(), "some-meeting-id")
			if err != nil {
				t.Fatalf("Poll(body=%q): unexpected error %v", tt.body, err)
			}
			if got != tt.want {
				t.Errorf("Poll(body=%q) = %d, want %d", tt.body, got, tt.want)
			}
		})
	}
}

// TestMymeetPoll_NotFound: HTTP 404 on /status means the meeting has not appeared yet (the bot has
// not joined, API.md §1.2). It must map to StatusJoiningCall WITHOUT an error (transient, not a
// failure), otherwise the normal bot join lag produces a false ERROR. If the bot never
// joins, the job is finished by the orphan timeout on maxJobAge (orchestrator), not by Poll.
func TestMymeetPoll_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		if _, err := w.Write([]byte("There is no meeting with this id")); err != nil {
			t.Errorf("test server write: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	p := NewMymeet("test-key", srv.URL, "default-meeting", time.UTC)
	got, err := p.Poll(context.Background(), "not-yet-visible-id")
	if err != nil {
		t.Fatalf("Poll(404): want a nil error (transient: not yet appeared), got %v", err)
	}
	if got != StatusJoiningCall {
		t.Errorf("Poll(404) = %d, want StatusJoiningCall", got)
	}
}

// TestMymeetSummaryMarkdown: table-driven tests of the templates parser (no DB, no network).
// Covers: the fresh form, the old form, empty actions, no templates, invalid JSON,
// unparsable is_json=true. Section names are not fixed: the parser is name-agnostic.
func TestMymeetSummaryMarkdown(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			// Fresh form (live probe 2026-06-18): two templates, text sections
			// in default-meeting + a separate tasks template with actions.
			name: "fresh_form_two_templates",
			raw: `{"followup_v2":{"chapters":[],"templates":[
				{"entities":[
					{"name":"Overview","is_json":false,"text":"## Обзор встречи\n\nОбсудили план."},
					{"name":"Key Topics","is_json":false,"text":"## Ключевые темы\n\n- Тема A\n- Тема B"}
				]},
				{"entities":[
					{"name":"tasks","is_json":true,"text":{"actions":[
						{"name":"Сделать отчёт","done":false},
						{"name":"Отправить письмо","done":true}
					]}}
				]}
			]}}`,
			want: "# Саммари встречи\n\n## Обзор встречи\n\nОбсудили план.\n\n## Ключевые темы\n\n- Тема A\n- Тема B\n\n## Задачи\n- [ ] Сделать отчёт\n- [x] Отправить письмо",
		},
		{
			// Old form: one template, all sections (text + tasks) inside it.
			name: "old_form_single_template",
			raw: `{"followup_v2":{"chapters":[],"templates":[
				{"entities":[
					{"name":"summary_agenda","is_json":false,"text":"## Повестка\n\nПункт 1."},
					{"name":"summary","is_json":false,"text":"## Итоги\n\nДоговорились."},
					{"name":"tasks","is_json":true,"text":{"actions":[
						{"name":"Написать протокол","done":false}
					]}}
				]}
			]}}`,
			want: "# Саммари встречи\n\n## Повестка\n\nПункт 1.\n\n## Итоги\n\nДоговорились.\n\n## Задачи\n- [ ] Написать протокол",
		},
		{
			// Tasks with empty actions: the "Задачи" (tasks) section does not appear.
			name: "empty_actions_no_tasks_section",
			raw: `{"followup_v2":{"chapters":[],"templates":[
				{"entities":[
					{"name":"summary","is_json":false,"text":"Краткое содержание."},
					{"name":"tasks","is_json":true,"text":{"actions":[]}}
				]}
			]}}`,
			want: "# Саммари встречи\n\nКраткое содержание.",
		},
		{
			// No templates field -> empty string.
			name: "no_templates",
			raw:  `{"followup_v2":{"chapters":[]}}`,
			want: "",
		},
		{
			// followup_v2 == null -> empty string.
			name: "followup_null",
			raw:  `{"followup_v2":null}`,
			want: "",
		},
		{
			// Invalid JSON -> empty string.
			name: "invalid_json",
			raw:  `{not valid json`,
			want: "",
		},
		{
			// tasks with an unparsable text (a string instead of an object when is_json=true) ->
			// the tasks section is skipped, no panic; text blocks remain.
			name: "tasks_unparseable_text",
			raw: `{"followup_v2":{"chapters":[],"templates":[
				{"entities":[
					{"name":"summary","is_json":false,"text":"Итоги встречи."},
					{"name":"tasks","is_json":true,"text":"это строка, не объект"}
				]}
			]}}`,
			want: "# Саммари встречи\n\nИтоги встречи.",
		},
		{
			// Tasks only, no text blocks -> the "Задачи" (tasks) heading without an extra
			// empty block between it and "# Саммари встречи" (meeting summary) (branch len(summaryBlocks)==0).
			name: "tasks_only_no_summary",
			raw: `{"followup_v2":{"chapters":[],"templates":[
				{"entities":[
					{"name":"tasks","is_json":true,"text":{"actions":[
						{"name":"Единственная задача","done":false}
					]}}
				]}
			]}}`,
			want: "# Саммари встречи\n\n## Задачи\n- [ ] Единственная задача",
		},
		{
			// is_json=false, but text is an object, not a string (the "text type varies with
			// is_json" boundary): Unmarshal into string fails -> the section is skipped, the valid one remains.
			name: "non_string_text_skipped",
			raw: `{"followup_v2":{"chapters":[],"templates":[
				{"entities":[
					{"name":"weird","is_json":false,"text":{"foo":1}},
					{"name":"summary","is_json":false,"text":"Валидный блок."}
				]}
			]}}`,
			want: "# Саммари встречи\n\nВалидный блок.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mymeetSummaryMarkdown([]byte(tt.raw))
			if got != tt.want {
				t.Errorf("mymeetSummaryMarkdown =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

// TestMymeetFetch_Render checks the per-speaker transcript render from the real report
// format (followup_v2.chapters[].transcript[], API.md §1.3, LIVE): the EXACT string, not
// "contains". Plus two guards: no followup_v2 -> "could not parse"; no speech ->
// "no speech detected". It protects the invariant "product = per-speaker transcript" from
// regression (the old code returned a tech-debt stub instead of a transcript).
func TestMymeetFetch_Render(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		want         string
		wantDuration int
		rawEmpty     bool // invalid JSON -> Raw nil (NULL in JSONB)
	}{
		{
			name: "two_speakers_one_chapter",
			body: `{"followup_v2":{"chapters":[{"name":"Вступление","transcript":[
				{"speaker":{"speaker":"Анна"},"text":"Привет всем.","timestamp":"0:00:00"},
				{"speaker":{"speaker":"Борис"},"text":"Здравствуйте.","timestamp":"0:00:05"}]}]}}`,
			want: "## Вступление\n[0:00:00] Анна: Привет всем.\n[0:00:05] Борис: Здравствуйте.",
		},
		{
			name: "two_chapters_blank_line_between",
			body: `{"followup_v2":{"chapters":[
				{"name":"Глава 1","transcript":[{"speaker":{"speaker":"Анна"},"text":"Раз.","timestamp":"0:00:00"}]},
				{"name":"Глава 2","transcript":[{"speaker":{"speaker":"Борис"},"text":"Два.","timestamp":"0:01:00"}]}]}}`,
			want: "## Глава 1\n[0:00:00] Анна: Раз.\n\n## Глава 2\n[0:01:00] Борис: Два.",
		},
		{
			name: "chapter_without_name",
			body: `{"followup_v2":{"chapters":[{"name":"","transcript":[
				{"speaker":{"speaker":"Анна"},"text":"Одна реплика.","timestamp":"0:00:01"}]}]}}`,
			want: "[0:00:01] Анна: Одна реплика.",
		},
		{
			name: "empty_speech_blank_segments",
			body: `{"followup_v2":{"chapters":[{"name":"Глава","transcript":[
				{"speaker":{"speaker":"Анна"},"text":"   ","timestamp":"0:00:01"}]}]}}`,
			want: "_Речь на встрече не обнаружена._",
		},
		{
			name: "no_followup_broken_form",
			body: `{"feedback":false,"media_type":"new"}`,
			want: "_Не удалось разобрать ответ поставщика. Обратитесь в поддержку._",
		},
		{
			// An empty chapter (all segments empty) is dropped together with its heading:
			// the output has only the substantive chapter, without a leading "\n\n".
			name: "empty_chapter_among_nonempty",
			body: `{"followup_v2":{"chapters":[
				{"name":"Пустая","transcript":[{"speaker":{"speaker":"Анна"},"text":"  ","timestamp":"0:00:00"}]},
				{"name":"Содержательная","transcript":[{"speaker":{"speaker":"Борис"},"text":"Есть текст.","timestamp":"0:01:00"}]}]}}`,
			want: "## Содержательная\n[0:01:00] Борис: Есть текст.",
		},
		{
			name: "duration_from_word_timings",
			body: `{"followup_v2":{"word_timings":[
				{"start_time":0.1,"end_time":12.4,"word":"Раз"},
				{"start_time":12.4,"end_time":73.9,"word":"два"}],
				"chapters":[{"name":"","transcript":[
				{"speaker":{"speaker":"Анна"},"text":"Раз два.","timestamp":"0:00:01"}]}]}}`,
			want:         "[0:00:01] Анна: Раз два.",
			wantDuration: 73,
		},
		{
			// Invalid JSON -> guard 1 (json.Unmarshal fails) -> "could not parse";
			// Raw nil (json.Valid=false -> NULL in JSONB).
			name:     "invalid_json",
			body:     `{not valid json`,
			want:     "_Не удалось разобрать ответ поставщика. Обратитесь в поддержку._",
			rawEmpty: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, err := w.Write([]byte(tt.body)); err != nil {
					t.Errorf("test server write: %v", err)
				}
			}))
			t.Cleanup(srv.Close)

			p := NewMymeet("test-key", srv.URL, "default-meeting", time.UTC)
			res, err := p.Fetch(context.Background(), "meeting-id")
			if err != nil {
				t.Fatalf("Fetch: unexpected error %v", err)
			}
			if res.TranscriptMarkdown != tt.want {
				t.Errorf("TranscriptMarkdown =\n%q\nwant\n%q", res.TranscriptMarkdown, tt.want)
			}
			if res.DurationSeconds != tt.wantDuration {
				t.Errorf("DurationSeconds = %d, want %d", res.DurationSeconds, tt.wantDuration)
			}
			// Raw carries the raw response for F5/Google Doc; invalid JSON -> nil (NULL in JSONB).
			if tt.rawEmpty {
				if len(res.Raw) != 0 {
					t.Errorf("Raw: want empty (invalid JSON -> NULL), got len %d", len(res.Raw))
				}
			} else if len(res.Raw) == 0 {
				t.Errorf("Raw is empty: want the raw report response")
			}
		})
	}
}
