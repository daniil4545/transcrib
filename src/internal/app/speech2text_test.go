package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestMeetingAccessCode: the Zoom password from ?pwd= is sent as a separate access_code
// field (s2t support reply, 2026-06-11); other links have no code.
func TestMeetingAccessCode(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"https://us05web.zoom.us/j/89978292641?pwd=s7PI9sJOiQX3gajkalJI4J6jQMeE38.1", "s7PI9sJOiQX3gajkalJI4J6jQMeE38.1"},
		{"https://meet.google.com/abc-defg-hij", ""},
		{"https://telemost.yandex.ru/j/18989266226692", ""},
		{"https://zoom.us/j/123?other=x", ""},
		{"://broken", ""},
	}
	for _, tt := range tests {
		if got := meetingAccessCode(tt.url); got != tt.want {
			t.Errorf("meetingAccessCode(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}

// TestMeetingAccessCode_AfterFindMeetingLink covers the seam of two functions:
// findMeetingLink grabs trailing punctuation with a greedy \S+ and trims it (TrimRight),
// while meetingAccessCode reads pwd from the already cleaned URL. Without the trim, a
// junk tail (`!`, `)`) would leak into access_code and break the password on the s2t
// side. The test exercises both together:
// phrase with a tail -> findMeetingLink -> meetingAccessCode -> clean code.
func TestMeetingAccessCode_AfterFindMeetingLink(t *testing.T) {
	tests := []struct {
		name   string
		phrase string
		want   string
	}{
		{
			name:   "trailing exclamation mark",
			phrase: "залетай https://us05web.zoom.us/j/89978292641?pwd=s7PI9sJOiQX3gajkalJI4J6jQMeE38.1! жду",
			want:   "s7PI9sJOiQX3gajkalJI4J6jQMeE38.1",
		},
		{
			name:   "link in parentheses",
			phrase: "встреча (https://us05web.zoom.us/j/123?pwd=abcDEF)",
			want:   "abcDEF",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			link, res := findMeetingLink(tt.phrase)
			if res != linkValid {
				t.Fatalf("findMeetingLink(%q) found no link", tt.phrase)
			}
			if got := meetingAccessCode(link); got != tt.want {
				t.Errorf("meetingAccessCode after findMeetingLink = %q, want %q (junk tail not trimmed?)", got, tt.want)
			}
		})
	}
}

// TestMapS2TStatus pins the s2t status mapping contract (API.md §2.5/§9)
// against drift in future edits. Pure function, so a table test.
func TestMapS2TStatus(t *testing.T) {
	// Success cases: a known status maps to a specific ProviderStatus.
	ok := []struct {
		value       string
		code        int
		description string
		want        ProviderStatus
	}{
		{"queued", 0, "Задание создано", StatusPending},
		{"queued", 30, "В очереди на запись", StatusQueuedRecording},
		{"queued", 30, "Бот присоединился к встрече", StatusJoiningCall},
		{"queued", 30, "Бот ведёт запись встречи", StatusOnCall},
		{"queued", 30, "Бот ведет запись встречи", StatusOnCall},
		{"done", 200, "", StatusDone},
		{"done", 204, "", StatusDone}, // no speech detected: Done, Fetch handles the empty result
		{"error", 404, "", StatusFailed},
		{"error", 502, "", StatusFailed},
		{"cancelled", 0, "", StatusFailed},
		{"paused", 102, "", StatusPaused},
	}
	for _, tt := range ok {
		got, err := mapS2TStatus(tt.value, tt.code, tt.description)
		if err != nil {
			t.Errorf("mapS2TStatus(%q, %d): unexpected error %v", tt.value, tt.code, err)
		}
		if got != tt.want {
			t.Errorf("mapS2TStatus(%q, %d) = %d, want %d", tt.value, tt.code, got, tt.want)
		}
	}

	// Error cases (contract drift): assert only that an error occurs, not the status:
	// the returned status with err != nil is an implementation detail, and binding to it
	// would cause a false failure on a harmless refactoring.
	bad := []struct {
		value       string
		code        int
		description string
	}{
		{"shiny-new-status", 0, ""},           // unknown status: an error, not a silent Pending
		{"queued", 99, ""},                    // unknown queued code: an error, to avoid showing a false recording
		{"queued", 30, "Новый текст статуса"}, // unknown code=30 description: an error, to avoid showing a false recording
		{"", 0, ""},
	}
	for _, tt := range bad {
		if _, err := mapS2TStatus(tt.value, tt.code, tt.description); err == nil {
			t.Errorf("mapS2TStatus(%q, %d): want a contract drift error, got nil", tt.value, tt.code)
		}
	}
}

func TestSpeech2TextFetch_EmptyTranscript(t *testing.T) {
	prov := NewSpeech2Text("test-key", "https://speech2text.test")
	prov.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/api/recognitions/ext-empty/result/txt" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusNoContent,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     make(http.Header),
			Request:    r,
		}, nil
	})}

	_, err := prov.Fetch(context.Background(), "ext-empty")
	if !errors.Is(err, ErrEmptyTranscript) {
		t.Fatalf("Fetch empty transcript err = %v, want ErrEmptyTranscript", err)
	}
}

func TestS2TDurationSeconds(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want int
	}{
		{
			name: "max_to_seconds_truncates_fraction",
			raw:  `{"chunks":[{"time":{"from":0,"to":12.9}},{"time":{"from":12.9,"to":37.8}}]}`,
			want: 37,
		},
		{
			name: "chunks_not_sorted",
			raw:  `{"chunks":[{"time":{"to":90.2}},{"time":{"to":30.1}}]}`,
			want: 90,
		},
		{
			name: "sub_second_is_unknown",
			raw:  `{"chunks":[{"time":{"to":0.8}}]}`,
			want: 0,
		},
		{
			name: "missing_to_is_unreliable",
			raw:  `{"chunks":[{"time":{"to":90.2}},{"time":{}}]}`,
			want: 0,
		},
		{
			name: "empty_chunks",
			raw:  `{"chunks":[]}`,
			want: 0,
		},
		{
			name: "missing_chunks",
			raw:  `{"speakers":[]}`,
			want: 0,
		},
		{
			name: "invalid_json",
			raw:  `{`,
			want: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := s2tDurationSeconds([]byte(tt.raw)); got != tt.want {
				t.Errorf("s2tDurationSeconds() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestSpeech2TextFetch_DurationSeconds(t *testing.T) {
	prov := NewSpeech2Text("test-key", "https://speech2text.test")
	prov.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body string
		switch r.URL.Path {
		case "/api/recognitions/ext-duration/result/txt":
			body = "Спикер 1: привет"
		case "/api/recognitions/ext-duration/result/json":
			body = `{"chunks":[{"time":{"from":0,"to":59.9}},{"time":{"from":60,"to":121.4}}]}`
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
			Request:    r,
		}, nil
	})}

	res, err := prov.Fetch(context.Background(), "ext-duration")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.DurationSeconds != 121 {
		t.Errorf("DurationSeconds = %d, want 121", res.DurationSeconds)
	}
}
