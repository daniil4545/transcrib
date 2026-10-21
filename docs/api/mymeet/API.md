# mymeet.ai API: contract digest for transcrib

Working reference for implementing `internal/app/mymeet.go`. Only what our scenario needs: start a bot on a meeting by link, wait for processing, fetch the per-speaker transcript and the summary.

**Source of truth** is `docs/api/mymeet/openapi.json` (official OpenAPI 3.0, v1.1.0). Supplemented with facts from the repository [github.com/MyMeetAI/API-Docs](https://github.com/MyMeetAI/API-Docs) (README + `test_api.py`) and the official MCP server [github.com/MyMeetAI/mymeet-mcp-server](https://github.com/MyMeetAI/mymeet-mcp-server). Business/legal/risks: `docs/api/mymeet/README.md`.

> **Inspection of the MCP server sources 2026-06-16** (`src/client.ts`, `src/types.ts`, `src/tools/*.ts`). This is a working TS client that really calls `backend.mymeet.ai`; by the actual request/response shapes it **takes priority over the declarative `openapi.json`**, which is outdated in places. `test_api.py` from API-Docs is a stub (prints `response.text`, does not parse, does not extract `meeting_id`), with no response shapes. Significant MCP findings are marked "(MCP)" in the sections below.
>
> Where sources disagree: ~~request/response shapes: the MCP client code has priority~~ - **REFUTED by a live test (see below): for `source`, `openapi.json` was right (`yandextelemost`), not MCP (`telemost`). LIVE data has priority everywhere; in its absence, `openapi.json`.** Auth: the `X-API-KEY` header (confirmed by the MCP code and the test) versus the deprecated `api_key` in the body/query.
>
> **CRITICAL: live test with a production key, 2026-06-16** (curl against `backend.mymeet.ai`, key obtained). All our endpoints were probed: Submit, Poll, Fetch (report + download), Delete, List. Response shapes are now **confirmed by live responses** (marked "(LIVE)"), and some earlier assumptions from MCP/openapi were **refuted** (marked "(LIVE correction)"). Most important: the `report` schema is different (the transcript is in `followup_v2.chapters[].transcript[]`, not top-level), `status` and error bodies are **plain text, not JSON**, and a freshly created meeting returns `404` on `status` for a while. Details are in the sections.

## 0. Base and authentication

- **Base URL:** `https://backend.mymeet.ai/`
- **Auth:** the header `X-API-KEY: <key>` on every request (the recommended way, works on all endpoints). **(LIVE)** confirmed by the live test of 2026-06-16 on all endpoints called.
  - `api_key` in the request body (`record-meeting`, `video`) or in the query is **deprecated**, kept for backward compatibility, and will be removed. We write on `X-API-KEY` from the start.
- **Getting a key:** B2B only, on request through [sales](https://mymeet.ai/contact) (`hello@mymeet.ai`). No self-service. See `README.md` (section "Status").
- **ToS (from the spec):** [Google Doc](https://docs.google.com/document/d/1drhWmEGdfIzwBiW_MPE2IAq6H0Nauo_51AMVb5vc89Q/edit) differs from the public offer `mymeet.ai/documents/offer-for-access`. Which document governs API access is to be clarified (see `README.md` §3, key risk clause 7.4).

## 1. Our flow: Submit, Poll, Fetch

Three endpoints cover the transcrib scenario. The rest (section 3) is reference only.

### 1.1 Submit: start the bot on a meeting

```
POST /api/record-meeting
X-API-KEY: <key>
Content-Type: application/json
```

Body (`Recording`):

| Field | Type | Req. | Purpose |
|---|---|---|---|
| `link` | string | yes | Meeting URL |
| `source` | enum | yes | Platform (see below) |
| `title` | string | yes | Report title |
| `local_date_time` | ISO 8601 + tz | yes | Local time, e.g. `2026-04-25T15:30:00+03:00` |
| `template_name` | enum `Templates` | yes | Summary template |
| `cron` | string | no | UTC cron for a delayed start. **Empty = record right now** |
| `meeting_password` | string | no | Meeting password (Zoom) |
| `participants` | string | no | Comma-separated emails for the followup mailing |

`source` enum. **CRITICAL: RESOLVED BY THE LIVE TEST 2026-06-16: for Telemost use `yandextelemost`, NOT `telemost`:**
- `openapi.json` (8): `gmeet`, `zoom`, **`yandextelemost`**, `sberjazz`, `trueconf`, `konturtalk`, `msteams`, `jitsi`.
- MCP client `types.ts`/`detectSource()`: `gmeet`, `zoom`, `telemost`, `sberjazz`, `trueconf`, `konturtalk`, `teams`, `jitsi`.

> **CRITICAL: (LIVE correction, IMPORTANT) The earlier conclusion "take the MCP values" is REFUTED. The right one is the `openapi.json` value `yandextelemost`.** A direct test on a real personal Telemost: `source:"yandextelemost"` -> the bot **joined and records** (`status:recording` in `meeting/status` and `all-meetings`); `source:"telemost"` -> `record-meeting` answers `200 {"meeting_id"}`, **but does NOT assign a bot** -> the meeting hangs forever in `404` and does not appear in `all-meetings`. **A `200` from `record-meeting` does NOT prove the bot joined**: the backend silently swallows an invalid `source`. The sign of a real join is a meeting with `status:recording` in `all-meetings` (not the Submit response code). By analogy, Teams probably needs `msteams` (openapi), not `teams` (MCP); this was not tested separately, but the logic is the same. **Conclusion: for `source` the source of truth is `openapi.json`, not the MCP client.**

Host to `source` mapping. We take the host detection logic from MCP `detectSource()`, but the **`source` values come from `openapi.json`** (the LIVE lesson: critical for Telemost). We need the first three:
| Host contains | source (openapi) |
|---|---|
| `meet.google.com` | `gmeet` |
| `zoom.us` / `zoom.com` | `zoom` |
| `telemost.yandex` | **`yandextelemost`** (LIVE-confirmed, NOT `telemost`) |
| `teams.microsoft.com` / `teams.live.com` | `msteams` (openapi; MCP gives `teams`, not tested) |
| `jazz.sber` / `salute.online` | `sberjazz` |
| `trueconf.` | `trueconf` |
| `konturtalk.` | `konturtalk` |
| `jitsi` / `meet.jit.si` | `jitsi` |

MCP always sends `source` (if not set, `detectSource(link)`, fallback `gmeet`). For us an unknown host is an error (a bot sent nowhere burns minutes); we do not mirror the `gmeet` fallback.

`template_name` enum `Templates` (11): `default-meeting`, `sales-meeting`, `sales-coaching`, `hr-interview`, `research-interview`, `team-sync`, `article`, `lecture-notes`, `one-to-one`, `protocol`, `medicine`. The starting MVP default is `default-meeting` (fixed by config, not hardcoded).

**For the MVP:** `cron` is empty (the bot joins an ongoing call), `source` is derived from `link`, `template_name` comes from config. **We deliberately do NOT send `meeting_password`** (rolled back 2026-06-17): the reason the bot does not join Zoom is external, not the password form. Experiments on prod: the bot does not enter Zoom without a password (`connecting -> failed`), nor with a `?pwd=` token (`404` limbo, strictly worse), nor with the **correct flat passcode** (`connecting -> failed`). mymeet support confirmed: the Zoom update (~13.06) blocks bots, to be fixed on their side (ETA ~22.06). We will bring the field back when mymeet confirms the right format and cleans up its Zoom logic.

Responses: `200 OK`, `401`, `402` (minutes limit exhausted), `409` (meeting already scheduled).

> Confirmed: **(LIVE 2026-06-16) The `200` response schema is confirmed:** the body is a flat JSON `{"meeting_id":"<uuid>"}` (`content-type: application/json`), exactly as planned. `meeting_id` -> `jobs.external_job_id`. Decode into `{"meeting_id": ...}`; empty means an error + log of the raw body.
>
> Confirmed: **(LIVE) `409` idempotency (closes question §5 #2):** a repeated `record-meeting` with the same `link` -> `409`, body is **plain text** (`text/html`, NOT JSON): `Meeting already scheduled <link> , meeting_id=<uuid>`. The dedup key is the **`link`** (the message echoes the link). **The existing `meeting_id` is returned** in the text (`meeting_id=...`): on `409` we parse it from the body with a regexp and reuse it as `external_job_id` (we do not create a duplicate Job). The `link + local_date_time` pair was not distinguished (the time was the same in both requests), but the error text refers only to `link`.

### 1.2 Poll: processing status

```
GET /api/meeting/status?meeting_id=<uuid>
X-API-KEY: <key>
```

**CRITICAL: (LIVE 2026-06-16) The real lifecycle is NOT as in the MCP list.** The sequence observed on a live recording: `404` (scheduled, the bot has not joined) -> **`connecting`** (the bot is connecting, LIVE 2026-06-17) -> **`recording`** (the bot records the call) -> **`processing`** (transcription/summary) -> **`new`** (READY, the report is complete, not viewed) -> [after Fetch/viewing] **`processed`**. Key corrections to the MCP list (`new/queued/processing/processed/failed`):
- **`connecting`** is an undocumented status (LIVE 2026-06-17); the phase "the bot is connecting", BEFORE `recording`. Maps to Pending; without it `mapMymeetStatus` failed on every tick while the bot was joining (an incident on the first production test: ~2 min of poll errors, the job did not move).
- **`recording`** is an undocumented status, absent from the MCP list; it means "the bot entered and records".
- **CRITICAL: `new` = READY for Fetch, NOT "queued".** The mapping `new -> in progress` is refuted. In `new` the report is already complete (`is_transcript_empty:false`, chapters + transcript in place, verified). A meeting stably hangs in `new` and does not move to `processed` on its own; `processed` = "ready AND viewed" (the demo `upload` became `processed` only after our download).
- A freshly created meeting does NOT appear in `new`: before recording it is `404` (§1.2 above), and `new` comes only AFTER `processing`.

**Mapping for `mapMymeetStatus`:** `connecting`, `recording`, `processing`, `queued` -> **Pending** (in progress); **`new`, `processed` -> Done (ready for Fetch)**; `failed` -> Failed; **an unknown status -> Poll error** (a contract-drift signal; `recording` is now an explicit Pending branch, so we leave unknown as an error, not a silent Pending, otherwise we would miss an unrecognized form of `failed`).

> Confirmed: **(LIVE 2026-06-16) The response form is plain text, NOT JSON.** The assumption "MCP parses it as JSON" is refuted. A valid meeting -> the bare status text `new` (`content-type: text/html`, no quotes, no wrapper). `Poll` reads it robustly: body -> `TrimSpace` -> `ToLower` -> mapping. Do NOT apply JSON decoding (it would fail).
>
> Confirmed: **(LIVE) The error is `404`, not `400` (correction of §4).** An unknown AND invalid (`not-a-uuid`) `meeting_id` -> **`404` plain text** `There is no meeting with this id` (`text/html`). The backend does not validate the UUID format, it just says "not found". `400` was not observed for status.
>
> CRITICAL: **(LIVE, CRITICAL for `orchestrator.go`) A freshly created meeting returns `404` on `status` until the bot starts recording.** Right after `record-meeting` (which returned a valid `meeting_id`, `409` confirms it) `meeting/status` returns `404 "There is no meeting with this id"`: the meeting is in the state "scheduled, the bot has not joined yet", and the `status` AND `active/all-meetings` endpoints do not see it yet. **`404` on `status` must not be treated as `failed`**: it means "not appeared yet" (in progress).
>
> CRITICAL: **A bot that could NOT join does NOT produce the `failed` status: the meeting hangs in `404` forever.** Confirmed by a dead-link test: with an unreachable link `status` stayed `404` **for 8+ minutes in a row**, with no transition to `failed` (the mymeet bot itself leaves the meeting/waiting room within 5 min, but the meeting record stays in limbo, invisible to `status`). **Conclusion:** waiting for `failed` in the case "the bot did not join / was not let in" is useless; we need **our own watchdog timeout** in `orchestrator.go` (e.g. N minutes in `404` after Submit -> our `failed_*` + rollback of the quota reserve). We go to `failed_*` by provider status only on an explicit `failed`; the watchdog closes everything else.
>
Responses (LIVE): `200` (plain-text status) | `404` (unknown / not-yet-appeared meeting) | `401`.

There are no webhooks, only polling. The `orchestrator.go` scheduler tick polls the status of active Jobs every 30 sec. `processed` -> Fetch; `failed` -> Job to `failed_*`.

### 1.3 Fetch: get the result

Two options. `download?format=md` is a ready markdown for Telegram delivery, without our own rendering. `report` (structured JSON) is for rendering into our own Google Doc (F5, in the MVP through Testing mode).

**JSON report:**
```
GET /api/video/report?meeting_id=<uuid>
X-API-KEY: <key>
```
The response is a JSON object. Responses (LIVE): `200`. `401`/`403`/`404` were not rechecked on report (for status/download: an unknown id -> `404`).

> CRITICAL: **(LIVE 2026-06-16) The real `report` schema is NOT what MCP assumed.** There is NO transcript at the top level (`result.transcript` is refuted). Top-level: `feedback` (bool), `media_type` (str), **`followup_v2`** (object): the whole useful payload is inside `followup_v2`.
>
> **`followup_v2` (23 fields, the significant ones):**
> - **`chapters`** `[]` is the transcript split into semantic chapters. Each chapter: `{ "name": "<chapter title>", "transcript": [<segment>, ...] }`.
>   - **Segment** (LIVE): `{ "color": "#7000E0", "speaker": {"color","id","speaker":"<name>"}, "text": "<utterance>", "timestamp": "H:MM:SS" }`.
>   - **Our `Result.TranscriptMarkdown`:** walk `chapters[]` -> inside each `transcript[]` -> render `timestamp` + `speaker.speaker` + `text`. The speaker name is `segment.speaker.speaker`; diarization = `speaker.id`.
> - **`speakers`** `[]` is the speaker registry: `{color, id, speaker:"<name>"}`. **(LIVE) The name is taken from the call participant's display name** (in the live test the speaker came out as `Daniil S.`, a name from Telemost), NOT an anonymous "Speaker 1". The product/privacy canon now explicitly does not guarantee anonymization: real names may come from the meeting platform or the provider.
> - **`templates`** `[]` is an array of summary templates computed by mymeet during processing (free for us). Each template: `{name, title, id, is_default, entities:[<entity>]}`. **Entity** = `{name, is_json (bool), text, html_text}`.
>   - CRITICAL: **(LIVE 2026-06-18) Section names AND structure FLOAT between mymeet versions; do NOT depend on names.** A fresh meeting (`default-meeting`): entities `Overview` + `Key Topics`, and `tasks` arrives as a SEPARATE template `{name:"tasks"}`. An old demo: `summary_agenda` + `summary` + `tasks`, all three entities INSIDE one `default-meeting`. Conclusion: parse **name-agnostically** (branch on `is_json`), not by section/template names.
>   - CRITICAL: **`text` has a DIFFERENT JSON type depending on `is_json`:** with `is_json=false` it is a string of ready markdown (`##` headings, bullets, timecode links `[0:38](#00:00:38)`); with `is_json=true` (the `tasks` section) it is an OBJECT `{"actions":[{"id":N,"done":bool,"name":"<task>"}]}` (empty `{"actions":[]}` if there are no tasks). `html_text` is also of mixed type (string vs `[]`). In Go the `text` field must be `json.RawMessage`, NOT `string` (otherwise `Unmarshal` fails on tasks).
>   - **(slice mymeet-summary 2026-06-18) WE USE it as the provider's ready summary** through the optional `Result.SummaryMarkdown` (bypassing our `Summarizer`): non-json entities -> `text` as markdown; `is_json` tasks -> `actions[].name` into a checklist. We render our own headings (in Russian). We take `text` (markdown) and ignore `html_text`.
> - `word_timings` `[]` is word-level timing. **(LIVE) On a fresh live recording it is filled** (40 words on a short call); an element is `{start_time, end_time, word}` (time in seconds, float). On the old `upload` demo it was empty. For billing the provider returns the actual `DurationSeconds = max(end_time)`; if `word_timings` is empty, the duration stays `0` and storage applies the common fallback.
> - Other (not needed by us): `chat` `{history,suggestions}` (AI chat about the meeting), `keywords` `[]`, `chapters` metadata, `amo_*` (AmoCRM integration), `authors/participants/date/duration/source/status/name/...`.
> - Warning: `followup_v2` has its own `status` field, which for the demo meeting is `new` with a ready report; we do not rely on it for our Poll semantics, see §1.2.
>
> The raw response -> `jobs.raw_provider_response JSONB`.

**Ready file:**
```
GET /api/storage/download?meeting_id=<uuid>&format=<pdf|md|json|docx>&template_name=<...>&timezone=<...>
X-API-KEY: <key>
```
`format` defaults to `pdf`. `template_name` and `timezone` are optional. Responses (LIVE): `200`; an unknown id -> `404`.

> Confirmed: **(LIVE 2026-06-16) `download?format=md` is a ready complete document for Telegram delivery.** `200`, `content-type: text/markdown`, ~16 KB for a 17-minute meeting. Structure: `**<Title>**` / `**Brief Summary**:` / `**Summary by topics**:` (`### <chapter>` per `chapter`) / `**Action items:**` (`- <task> (**Assignee:** <name>)`) / `**Transcript:**` (blocks `H:MM:SS **<Speaker>:**` + text). So `md` already contains both the summary and the per-speaker transcript: for the MVP delivery to the chat (`telegram_delivery.go`) our own rendering is not needed. For our own Google Doc (F5) it is the opposite: we take the JSON `report` (see above) and ignore the summary.

**Meeting deletion (152-FZ):**

> CRITICAL: **(LIVE correction 2026-06-16) The working deletion route is `DELETE /api/video/report?meeting_id=<uuid>`, NOT `DELETE /api/delete-meeting`.** `DELETE /api/delete-meeting` returned the default Flask 404 "The requested URL was not found on the server" -> **the route does not exist** (MCP `client.ts` is wrong / the endpoint is not deployed). `DELETE /api/video/report` is the real route (an app-level `404 "Meeting not found"` on an unknown id, as in `openapi.json`).
>
> This deletes a **processed** meeting (the report), it does NOT stop an active recording. **(LIVE) confirmed: `DELETE /api/video/report` on a "scheduled"/active meeting -> `404 "Meeting not found"`** (there is no report yet), the meeting stays scheduled -> **there is no programmatic cancellation of an active recording** (see §5 #4, `Cancel`).

## 2. Integration sketch (inside mymeet.go)

Implements the `TranscriptionProvider` interface (`internal/app/transcription.go`). `orchestrator.go` does not know about mymeet.

```
Submit(ctx, job)        -> POST /api/record-meeting (cron="")  -> {"meeting_id"} (external_id); 409 -> parse meeting_id from the text
Poll(ctx, externalID)   -> GET  /api/meeting/status            -> plain-text status; 404 = not-yet-appeared/unknown (NOT failed)
Fetch(ctx, externalID)  -> GET  /api/video/report             -> followup_v2.chapters[].transcript[] -> TranscriptMarkdown; followup_v2.templates[] -> SummaryMarkdown (slice mymeet-summary)
                       (or GET /api/storage/download?format=md -> ready .md for delivery to the chat)
```

mymeet status mapping to our state machine: `queued/processing/`**`recording`** -> in progress; **`new`/`processed` -> ready for Fetch** (LIVE: `new` = ready, not "queued", §1.2); `failed` -> `failed_*`. **(LIVE)** a transient `404` on `status` right after Submit -> also "in progress", not `failed`. An unknown status string -> Poll error (a drift signal), not a silent Pending.

## 3. Other endpoints (outside the MVP scope)

Reference only, for the future. Full signatures and code examples are in `openapi.json`.

| Method and path | Purpose |
|---|---|
| `POST /api/video` | Upload a ready audio/video in 20 MB chunks (`Uploading`). Fallback for forwarded recordings and debugging. Has `speakers_number` (0 = auto) |
| `POST /api/generate-new-template` | Regenerate the summary with a different template for a finished meeting |
| `GET /api/workspaces/active/all-meetings` | Paginated meeting list (`page`, `perPage`). Admin needs |
| `PUT /api/meeting` | Rename a meeting (`meetingId`, `newName`) |
| `PUT /api/meeting/{meetingId}/summary` | Edit a summary subsection (`templateName`, `entityName`, `newSummaryText`) |
| `POST /api/clear-transcript` / `POST /api/undo-clear-transcript` | Clear/restore the transcript |

> **(LIVE 2026-06-16) The `all-meetings` shape:** `200` JSON `{"followups":[<meeting>], "total":N}` (the field is called `followups`, not `meetings`). `page` is **0-indexed** (default `0`; the request `page=1` gave an empty page with `total:1`). `<meeting>` = `{authors:[{avatar,login}], date (RFC1123 GMT), duration (min), id (uuid), is_transcript_empty (bool), link, locked, media_type, name, preview_image_url, source, speakers:[<names>], status}`. Useful for debugging (find the `meeting_id` of a finished meeting without knowing the id).

`Entities` enum (summary subsections referenced by the summary edit; indirectly, the report sections): `summary`, `summary_agenda`, `sales_general`, `sales_coach`, `hr_summary`, `questions_and_answers`, `research_insights`, `team_sync_agenda`, `summary_by_speaker`, `workshop-double`, `seo-article-double`, `one_to_one`, `med-anamnesis`, `protocol`, `template-recommendation`.

## 4. Error code catalog

Combined across endpoints (`record-meeting` + `video` + reading):

| Code | Meaning |
|---|---|
| 400 | Invalid request / `meeting_id` |
| 401 | Unauthorized (missing/invalid key) |
| 402 | Minutes limit exhausted |
| 403 | No rights to the meeting |
| 409 | Already scheduled / already uploaded |
| 414 | File > 3 GB |
| 415 | Unsupported file format |
| 426 | Not enough minutes to process the file |
| 432 | Plan upgrade required for files > 1 GB |
| 433 | Duration < 30 sec |

**(LIVE 2026-06-16) Clarifications on the actually observed codes:**
- Reading (`status`, `video/report`, `download`) on an unknown/invalid `meeting_id` -> **`404`** (not `400`), body **plain text** (`There is no meeting with this id` / `Meeting not found`), not JSON.
- `record-meeting` repeating the same `link` -> **`409`**, plain-text body with the `meeting_id` of the existing task (§1.1).
- A nonexistent route (e.g. `DELETE /api/delete-meeting`) -> the default Flask 404 (HTML "The requested URL was not found"). Distinguish it from the app-level `404` ("Meeting not found"): the first means there is no route, the second means there is no meeting.
- mymeet error bodies are **not JSON**: our client must not try to parse them as JSON.

## 5. Open technical questions

**The key is obtained and the live E2E passed on 2026-06-16, so most questions are closed** (see the "Closed" blocks below). What remains: the shape of `failed`, multi-voice diarization, the bot stop endpoint (a letter to mymeet).

**mymeet support answers (2026-06-16, correspondence about the docs, no key yet):**
- **Concurrent (#5 closed):** 2 parallel meetings per account; the logic is "1 account = 1 plan = 1 user". **RESOLVED 2026-06-19:** the limit of 2 is only on the test plan; on the API plan the parallel meeting limits are lifted, with no surcharge (billing is per processed minute, not per user). See the 2026-06-19 block below.
- **Bot timeouts:** alone, it leaves after <=5 min; in the waiting room, it leaves after <=5 min. (An advantage over s2t: the bot leaves on its own and does not hang in a persistent room.)
- **Cancelling an active recording (#4):** there is no programmatic API. The ways are kicking the bot out of the meeting manually / a button in the account (coming ~July) / deleting the meeting entirely. `DELETE /api/video/report` does **NOT** interrupt the recording (LIVE-confirmed). The route `DELETE /api/delete-meeting` **does not exist** (LIVE: the default Flask 404); the earlier sub-question "does delete-meeting interrupt" is dropped because no such endpoint exists.
- **failure_reason (#3) and the `report` schema (shape):** mymeet asks for screenshots; the answer comes with the live test / in correspondence.

**mymeet support answers (2026-06-19), the main blocker is removed:**
- **Concurrent: RESOLVED.** The limit of 2 is only on the test plan. On the **API plan the parallel meeting limits are disabled**; no surcharge is needed, because billing is per processed minute, not per user/employee. This removes mymeet's main strategic limitation (scale): the provider is fit to be primary, not only a pilot.
- **Bot branding: partial.** Full customization (logo, name, greeting) is on the **Business** plan. On the **API plan** it is discussed individually, with no general availability yet. -> the name "transcrib" on the in-call bot via the API is not out of the box and needs a separate agreement (on s2t it is just `bot_name`).
- **Custom summary prompt: available.** 11 ready templates + since 18.05 the "Custom report" feature: you can set your own prompts. An alternative to our own OpenRouter module for F3 (less code of ours, but the summary is tied to mymeet).
- **Zoom: ETA end of month.** A full fix is promised by the end of June (the bot does not enter because of the Zoom update ~13.06).
- **Contract: invoice-offer.** They work by invoice-offer; a separate B2B contract depends on the payment amount. They again expressed no concern about a "competing product" (clause 7.4).

**Closed by the live test with a production key (2026-06-16):**
- ~~The `200` response shape of `record-meeting`~~ -> flat JSON `{"meeting_id":"<uuid>"}` (§1.1, was #1).
- ~~`409` idempotency~~ -> by `link`, plain-text body with `meeting_id=<uuid>` of the existing task (§1.1, was #2).
- ~~The `/api/video/report` schema~~ -> transcript in `followup_v2.chapters[].transcript[]`, segment `{color, speaker:{...,speaker}, text, timestamp}` (§1.3). The top-level `result.transcript` from MCP is **refuted**.
- ~~`source` for Telemost~~ -> **`yandextelemost`** (openapi), NOT `telemost` (MCP). `telemost` -> `200` but the bot does not join; `yandextelemost` -> the bot joined and records (§1.1). **The source of truth for `source` is openapi, not MCP.**
- ~~The status list~~ -> an undocumented **`recording`** (the bot records the call) actually arrives, §1.2.
- ~~Eternal idempotency~~ -> `409` applies only while the meeting is actively "scheduled"; after the bot leaves (~5 min for one that did not join) the same `link` returns `200` again with a new `meeting_id` (§1.1).
- ~~Poll = JSON or plain text~~ -> **plain text** (§1.2); error bodies are plain text too.
- ~~Cancel through `DELETE`~~ (was #4) -> **there is no programmatic cancellation**: `DELETE /api/video/report` on an active meeting -> `404`, the recording is not interrupted (§1.3). `DELETE /api/delete-meeting` has no route.
- ~~`download?format=md`~~ -> a ready complete .md (summary + per-speaker transcript) for delivery to the chat (§1.3).

**Additionally closed by a live recording of a real call (2026-06-16, the bot in a personal Telemost):**
- ~~Status lifecycle~~ -> `404 -> connecting -> recording -> processing -> new (ready) -> [processed after viewing]`; **`new` = ready for Fetch**, `connecting`/`recording` are undocumented in-progress statuses (§1.2).
- ~~`word_timings` on a fresh recording~~ -> filled (40 words; `{start_time,end_time,word}`, §1.3).
- ~~Live bot join / diarization (one speaker)~~ -> the bot joined with `yandextelemost`, recorded real Russian speech, speaker `Daniil S.` (a name from the call). The transcript is readable.

**Still open:**
1. ~~The `failure_reason` field in `failed`~~ -> **the shape of `failed` is confirmed by the incident of 2026-06-17**: a bare plain-text string `failed` after ~3 min, WITHOUT `failure_reason` or details. mymeet gives no diagnostics of the cause: why exactly `failed` cannot be learned from the API. **The cause of this particular incident is external (NOT the password form):** mymeet support confirmed that the Zoom update (~13.06) blocks bots/assistants; the mymeet bot does not enter Zoom without a password, nor with a `?pwd=` token (-> `404` limbo), nor with the correct flat passcode (all -> `connecting -> failed`). It is fixed on the mymeet side, ETA ~22.06. That is why we do not send `meeting_password` now (§1.1).
2. Diarization on a **multi-voice** call (in the live test one person spoke -> one speaker). The quality of separating 2+ voices is not verified.
3. Behavior on an empty call (the bot joined, nobody spoke): `failed`, an empty transcript, or `new` with `is_transcript_empty:true`?

**Closed earlier (support / MCP, not live):**
- Concurrent limit: 2 only on the test plan; **on the API plan it is lifted without surcharge** (support 2026-06-19, billing per minute). The earlier "scale bottleneck" is closed.
- Transcription language: there is no parameter, only auto-detection (the MCP `RecordMeetingSchema` has no language field).
- Per-minute price of the API plan: 1.5 RUB/min (12 months) / 2 RUB/min (6 months), `README.md` §2.1; fixing the rate by contract is open.
