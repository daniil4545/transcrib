package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestMapMymeetStatus фиксирует контракт маппинга статусов mymeet (API.md §1.2,
// план-слайс «Критические узлы под тест») от дрейфа. Неизвестный статус обязан
// давать ошибку, а не молчаливый Pending — иначе джоба вечно залипнет без следа.
func TestMapMymeetStatus(t *testing.T) {
	// Успешные ветки: известный статус -> конкретный ProviderStatus (LIVE-маппинг
	// API.md §1.2). Фазы развёрнуты: JoiningCall (connecting/queued, бот подключается),
	// OnCall (recording, бот пишет), Transcribing (processing, бот вышел), Done
	// (new/processed), Failed.
	ok := []struct {
		value string
		want  ProviderStatus
	}{
		{"new", StatusDone},                // LIVE: new = ГОТОВО к Fetch, не «в очереди»
		{"recording", StatusOnCall},        // LIVE: бот пишет звонок -> Job recording
		{"connecting", StatusJoiningCall},  // LIVE 2026-06-17: бот подключается (фаза до recording)
		{"queued", StatusJoiningCall},      // бот подключается / в очереди
		{"processing", StatusTranscribing}, // бот вышел, фоновая обработка -> Job processing
		{"processed", StatusDone},
		{"failed", StatusFailed},
	}
	for _, tt := range ok {
		t.Run("ok/"+tt.value, func(t *testing.T) {
			got, err := mapMymeetStatus(tt.value)
			if err != nil {
				t.Fatalf("mapMymeetStatus(%q): неожиданная ошибка %v", tt.value, err)
			}
			if got != tt.want {
				t.Errorf("mapMymeetStatus(%q) = %d, ожидали %d", tt.value, got, tt.want)
			}
		})
	}

	// Негативные ветки (дрейф контракта): фиксируем факт ошибки. StatusPaused у mymeet
	// недостижим (нехватка минут — это HTTP 402 на Submit, не Poll-статус), потому
	// "paused" — такой же неизвестный статус, как любой другой.
	bad := []string{
		"paused", // у mymeet не приходит на Poll -> должен быть unknown, не StatusPaused
		"shiny-new-status",
		"done",       // s2t-словарь, не mymeet -> не должен молча маппиться
		"PROCESSING", // регистр снимает Poll (ToLower), сюда приходит уже lower; верхний = unknown
		"",
	}
	for _, value := range bad {
		t.Run("unknown/"+value, func(t *testing.T) {
			got, err := mapMymeetStatus(value)
			if err == nil {
				t.Errorf("mapMymeetStatus(%q): ожидали ошибку дрейфа контракта, получили nil", value)
			}
			// Негативный кейс проверяет устойчивость: при ошибке статус НЕ должен
			// быть Done/Failed (это привело бы к ложной доставке/откату). Pending
			// при err — безопасная деталь (оркестратор смотрит на err первым).
			if got == StatusDone || got == StatusFailed {
				t.Errorf("mapMymeetStatus(%q) при ошибке вернул терминальный %d — риск ложной доставки/отката", value, got)
			}
		})
	}
}

// TestMymeetSource фиксирует маппинг host -> source (значения openapi.json, API.md
// §1.1). Граничные значения: точные хосты, поддомены zoom, Телемост -> yandextelemost
// (LIVE: НЕ telemost — с ним бот не заходит). Неизвестный хост -> ошибка: бота в
// никуда не шлём (жжёт минуты mymeet).
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
				t.Fatalf("mymeetSource(%q): неожиданная ошибка %v", tt.url, err)
			}
			if got != tt.want {
				t.Errorf("mymeetSource(%q) = %q, ожидали %q", tt.url, got, tt.want)
			}
		})
	}

	// Негативные: неизвестный/пустой хост -> ошибка (не пустой source, не fallback gmeet).
	bad := []struct {
		name string
		url  string
	}{
		{"unknown_host", "https://teams.microsoft.com/l/meetup-join/xyz"},
		{"webex", "https://example.webex.com/meet/room"},
		{"empty_host", "/just/a/path"},
		{"bare_word_no_host", "meet.google.com/abc"}, // без схемы -> Hostname() пустой
	}
	for _, tt := range bad {
		t.Run("err/"+tt.name, func(t *testing.T) {
			got, err := mymeetSource(tt.url)
			if err == nil {
				t.Errorf("mymeetSource(%q): ожидали ошибку, получили source=%q", tt.url, got)
			}
		})
	}
}

// TestMymeetPoll_Normalize проверяет НАШУ логику нормализации ответа Poll
// (TrimSpace + снятие обрамляющих кавычек + lower-case). LIVE: статус приходит
// plain-text голой строкой; нормализация страхует и форму в кавычках — обе обязаны
// дать один ProviderStatus.
func TestMymeetPoll_Normalize(t *testing.T) {
	tests := []struct {
		name string
		body string // что отдаёт тестовый backend в теле /api/meeting/status
		want ProviderStatus
	}{
		{"plain_processing", "processing", StatusTranscribing},
		{"json_quoted_processing", `"processing"`, StatusTranscribing},
		{"json_quoted_with_whitespace", "  \"processing\"\n", StatusTranscribing},
		{"plain_uppercase", "PROCESSING", StatusTranscribing}, // lower-case — наша нормализация
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
				t.Fatalf("Poll(body=%q): неожиданная ошибка %v", tt.body, err)
			}
			if got != tt.want {
				t.Errorf("Poll(body=%q) = %d, ожидали %d", tt.body, got, tt.want)
			}
		})
	}
}

// TestMymeetPoll_NotFound: HTTP 404 на /status — встреча ещё не появилась (бот ещё
// не зашёл, API.md §1.2). Должно мапиться в StatusJoiningCall БЕЗ ошибки (транзиент, не
// сбой), иначе на штатном лаге захода бота сыпется ложный ERROR. Если бот так и не
// зайдёт — джобу добьёт orphan-timeout по maxJobAge (orchestrator), не Poll.
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
		t.Fatalf("Poll(404): ожидали nil-ошибку (транзиент «ещё не появилась»), получили %v", err)
	}
	if got != StatusJoiningCall {
		t.Errorf("Poll(404) = %d, ожидали StatusJoiningCall", got)
	}
}

// TestMymeetSummaryMarkdown — table-driven тесты парсера templates (без БД и сети).
// Покрывает: свежую форму, старую форму, пустые actions, нет templates, невалидный JSON,
// непарсимый is_json=true. Имена секций не фиксированы — парсер имя-агностичен.
func TestMymeetSummaryMarkdown(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			// Свежая форма (живая проба 2026-06-18): два шаблона, текстовые секции
			// в default-meeting + отдельный шаблон tasks с actions.
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
			// Старая форма: один шаблон, все секции (текст + задачи) внутри него.
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
			// Задачи с пустым actions — секция «Задачи» не появляется.
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
			// Нет поля templates -> пустая строка.
			name: "no_templates",
			raw:  `{"followup_v2":{"chapters":[]}}`,
			want: "",
		},
		{
			// followup_v2 == null -> пустая строка.
			name: "followup_null",
			raw:  `{"followup_v2":null}`,
			want: "",
		},
		{
			// Невалидный JSON -> пустая строка.
			name: "invalid_json",
			raw:  `{not valid json`,
			want: "",
		},
		{
			// tasks с непарсимым text (строка вместо объекта при is_json=true) ->
			// секция задач пропущена, не паника; текстовые блоки остаются.
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
			// Только задачи, без текстовых блоков -> заголовок «Задачи» без лишнего
			// пустого блока между ним и «# Саммари встречи» (ветка len(summaryBlocks)==0).
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
			// is_json=false, но text — объект, не строка (граница «text разнотипна по
			// is_json»): Unmarshal в string падает -> секция пропущена, валидная остаётся.
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
				t.Errorf("mymeetSummaryMarkdown =\n%q\nожидали\n%q", got, tt.want)
			}
		})
	}
}

// TestMymeetFetch_Render проверяет рендер транскрипта по спикерам из реального формата
// report (followup_v2.chapters[].transcript[], API.md §1.3, LIVE) — ТОЧНУЮ строку, не
// «содержит». Плюс два guard'а: нет followup_v2 -> «не удалось разобрать»; речи нет ->
// «речь не обнаружена». Защищает инвариант «продукт = транскрипт по спикерам» от
// регресса (старый код отдавал заглушку-техдолг вместо транскрипта).
func TestMymeetFetch_Render(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		want         string
		wantDuration int
		rawEmpty     bool // невалидный JSON -> Raw nil (NULL в JSONB)
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
			// Пустая глава (все сегменты пусты) отбрасывается вместе с заголовком —
			// в выводе только содержательная глава, без ведущего "\n\n".
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
			// Невалидный JSON -> guard 1 (json.Unmarshal падает) -> «не удалось разобрать»;
			// Raw nil (json.Valid=false -> NULL в JSONB).
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
				t.Fatalf("Fetch: неожиданная ошибка %v", err)
			}
			if res.TranscriptMarkdown != tt.want {
				t.Errorf("TranscriptMarkdown =\n%q\nожидали\n%q", res.TranscriptMarkdown, tt.want)
			}
			if res.DurationSeconds != tt.wantDuration {
				t.Errorf("DurationSeconds = %d, ожидали %d", res.DurationSeconds, tt.wantDuration)
			}
			// Raw несёт сырой ответ для F5/Google Doc; невалидный JSON -> nil (NULL в JSONB).
			if tt.rawEmpty {
				if len(res.Raw) != 0 {
					t.Errorf("Raw: ожидали пусто (невалидный JSON -> NULL), got len %d", len(res.Raw))
				}
			} else if len(res.Raw) == 0 {
				t.Errorf("Raw пуст — ожидали сырой ответ report")
			}
		})
	}
}
