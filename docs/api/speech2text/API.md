# Speech2Text API: unified contract reference

Canonical Speech2Text API reference for `transcrib`. It merges the file flow, the Meeting API (recording a meeting through a bot) and the Webhook API into one document.

**Source of truth** is the official Speech2Text contract, reconciled on 2026-06-10 and condensed into this reference. Also: OpenAPI `https://api.speech2text.ru/openapi.json`, Python SDK `https://github.com/Speech2TextR/s2t-client-python`. Business/legal/pricing/risks: `docs/api/speech2text/README.md`.

> **STATUS (2026-06-10):** the Meeting API and Webhook API are now **officially documented** (the 03.06 PDF version did not have them; there was only our reconstruction). Sales confirmed the Meeting API rework (release today or tomorrow). The contract below follows the PDF; **live verification of the endpoints happens on test access during the first implementation of `speech2text.go`**. The earlier file `API_actual.md` (file flow only) and the earlier archived `API.md` (manual reconstruction of the Meeting API) are merged here.

## 0. Base URL, hosts, authentication

- **Host:** `https://api.speech2text.ru`.
- Confirmed: **host discrepancy closed (live probe 2026-06-11):** `POST /api/meetings/join` answers identically on both hosts (`api.speech2text.ru` and `speech2text.ru`). We standardize on `api.speech2text.ru`.
- Confirmed: **the endpoints of our flow exist (live probe 2026-06-11, without a real meeting):** `meetings/join` (400 `meeting_url is required` on an empty body), `recognitions/{id}`, `recognitions/{id}/result/txt`, `meetings/{id}/stop` all answer with validation, not a 404 route. `GET /api/user/amounts` returns 200 (300 minutes on the test account).
- Warning: **error format (live fact):** JSON `{"name","message","code","status"}`. "Task not found" arrives as **HTTP 400** (not 404!) with `message: "Not found recognition task"` for `recognitions/{id}`, `result/*` and `meetings/{id}/stop`. Error parsing must not rely on 404.
- Confirmed: **support answers (2026-06-11):** the normal "Recording queue" time is **~10 minutes** (longer is an incident); there are no Meeting API restrictions on the test plan; **pass the meeting password in a separate `access_code` field**, not in the `meeting_url` query (a Zoom `?pwd=` must be extracted; implemented in `speech2text.go`); `result/json` for a meeting task has the same format as for a file task (recording first, then transcription); **summary is not implemented in the API, web version only** (confirms the decision to use a separate `Summarizer`/OpenRouter for F3).
- Confirmed: **support answers (2026-06-19, the same 5 questions as for mymeet):** **concurrency/plan:** reference plan "24 Hours" 17,600 RUB/month: 24 h/day, **3 simultaneous streams out of the box**, diarization included, over the limit 0.40 RUB/min (~24 RUB/h), per second from the 15th second; an extra stream +100 RUB (the test account had 1 stream, which is not the plan ceiling); **`bot_name`:** meeting participants see our name, confirmed officially (out of the box); **summary:** web only, not in the API (our own through OpenRouter); **contract:** offer or a separate B2B at our choice, no conflict declared; **bots hanging:** declared fixed: the bot leaves if nobody is in the room for 10 min. CRITICAL: the live test of 2026-06-22 failed (zombie: the bot did not leave a persistent Telemost room, `code 30` for more than 20 min); later s2t was again chosen as the economically preferred prod path under live-risk.
- **Authentication** (on every request, two ways):
  - `Authorization: Bearer YOUR-API-KEY` is the universal one (the meeting and webhook examples use only this).
  - `?api-key=YOUR-API-KEY` in the query is for `recognitions/*`.
  - `transcrib` standardizes on **Bearer** in all calls.
- **API key** is in the personal account at `https://api.speech2text.ru/profile-api`. API plans: `https://speech2text.ru/rates-api`.

## 1. Flow for transcrib (Meeting, Poll, Fetch)

The target scenario is recording an online meeting. The file/link API (section 3) is for debugging and one-off cases.

```
Submit  -> POST /api/meetings/join                 -> external_id (id), status queued
Poll    -> GET  /api/recognitions/{id}             -> status {code, value, description}
Fetch   -> GET  /api/recognitions/{id}/result/{fmt}-> transcript (json / txt / docx)
Stop    -> POST /api/meetings/{id}/stop            -> status cancelled (early)
Webhook -> push to our URL on status change         -> safety net / polling replacement
```

The current runtime gets the status polling-first through `GET /api/recognitions/{id}` in `orchestrator.go`.
The webhook `transcription.status_changed` remains future hardening for `web.go` (see §6 and §11).

## 2. Meeting API: recording a meeting through a bot

The bot joins the conference by link, records and transcribes automatically.

### 2.1 Supported platforms (8)

| Platform | Host |
|---|---|
| Zoom | `zoom.us` |
| Google Meet | `meet.google.com` |
| Yandex Telemost | `telemost.yandex.ru` |
| Sferum | `sferum.ru` |
| MTS Link | `my.mts-link.ru` |
| Bitrix24 | `bitrix24.ru/video/` |
| BigBlueButton | `bigbluebutton.ru/` |
| KTalk | `kontur.ru/talk` |

Telemost is covered: the key requirement of the spec is met. Zoom / Google Meet / Telemost are critical for us.

### 2.2 Start the bot

```
POST /api/meetings/join
Authorization: Bearer YOUR_API_KEY
Content-Type: application/json
```

| Parameter | Type | Req. | Description |
|---|---|---|---|
| `meeting_url` | string | yes | Meeting link |
| `title` | string | no | Meeting title |
| `lang` | string | no | `ru`, `en`, ... Default: from the account settings |
| `speakers` | int | no | Number of speakers (if known) |
| `bot_name` | string | no | Bot name in the conference |
| `access_code` | string | no | Meeting password |

Response `201 Created`:
```json
{
  "id": "abc123def456",
  "meeting_url": "https://zoom.us/j/123456789",
  "title": "Созвон с командой",
  "lang": "ru",
  "bot_name": "Транскрибатор",
  "status": "queued",
  "created_at": "2026-05-18 10:00:00"
}
```

(The sample values `title` = "Call with the team" and `bot_name` = "Transcriber" are kept in Russian as in the provider's example.)

Errors: `400` (`meeting_url` missing / invalid URL), `400` (platform not supported), `401` (invalid/missing key).

### 2.3 Stop the bot early

```
POST /api/meetings/{id}/stop
Authorization: Bearer YOUR_API_KEY
```

Stops the bot early. Response `200 OK` is the task object (the live probe returned status `cancelled`).

Errors: `400` (task not found / already finished / already cancelled), `401`, `403` (no access to the task).

> This closes the earlier gap "no bot stop endpoint".

> **CRITICAL: REVERSAL OF THE `/stop` CONTRACT (support, 2026-06-17).** Support clarified the semantics: it is an **early finalization with a transcript**, NOT a pure cancellation as previously assumed:
> 1. **Auto-exit on timeout does NOT cancel the task.** Even for an empty meeting a result is produced: the task moves to `done`, and the transcript is built from the recorded fragment. (This closes the earlier open question: auto-exit yields a transcript.)
> 2. **`/stop` on a bot that is ALREADY recording finalizes the fragment:** the bot leaves the conference, and the recorded part goes to transcription. So `/stop` is an early completion **with** a transcript, not a "cancel without result".
> 3. **The `code 30` hang (the bot does not join, minutes are not charged) is a confirmed s2t bug, in progress.** After the fix the bot leaves automatically after ~10 min, as declared.
>
> **Consequence for the code (updated 2026-07-03):** manual stop and watchdog stop for an s2t task that is already recording are no longer a refund scenario. The code sets the marker `manual_stop|watchdog_stop`, calls `/stop`, moves the Job to `processing` and waits for the result. If the final provider status after `/stop` arrives as `cancelled`, the worker still tries `Fetch`, but only with a stop marker. A non-empty transcript counts as a rendered service and goes into the normal terminal billing; an empty transcript or the impossibility to get the result leads to `failed` with the reserve refunded.

### 2.4 Tracking the status of a meeting task

The same endpoint as for files:
```
GET /api/recognitions/{id}
Authorization: Bearer YOUR_API_KEY
```

Task object (fields: see §4). For meetings `resource.type = "link"`, `resource.url` = the meeting link. `status` is an object `{code, value, description}`.

### 2.5 Meeting statuses (`status.value` / `status.code`)

The `description` strings are literal provider values and stay in Russian; the English meaning is given in the table.

| value | code | Description |
|---|---|---|
| `queued` | 0 | Task created |
| `queued` | 30 | Bot is in the recording queue (`description: "В очереди на запись"`, "In the recording queue", live fact 2026-06-15) |
| `queued` | 30 | Bot has joined the meeting |
| `queued` | 30 | Bot is recording the meeting |
| `paused` | 102 | Paused: the available minutes limit is exhausted |
| `done` | 200 | Recognition succeeded |
| `done` | 204 | Finished: no speech detected |
| `error` | 404 | Bot could not join the meeting |
| `error` | 406 | Invalid file format |
| `error` | 407 | Duration limit exceeded |
| `error` | 501 | Recognition error |
| `error` | 502 | Error loading from an external source |
| `cancelled` | none | Stopped through `/stop` (§2.3) |

`code` 30 is repeated for THREE sub-states (in the recording queue / joined / recording): distinguish them by `description`. For our state machine: `queued/code=0` and `queued/code=30 "В очереди на запись"` map to `Pending`/`joining`; `queued/code=30 "Бот присоединился..."` ("Bot has joined...") or `"Бот ведёт запись..."` ("Bot is recording...") map to `OnCall`/`recording`; `done` leads to Fetch; `error`/`cancelled` lead to `failed_*`, except `cancelled` after our `/stop` with the marker `manual_stop|watchdog_stop`: then we try `Fetch` first; `paused` means not enough minutes (see §7). An unknown `description` at `code=30` is treated as contract drift and left as a Poll error, so as not to show a false recording.

> **Incident "stuck recording workers" (repeat on 2026-06-15, like 11 June).** Telemost tasks got stuck in `code 30 / "В очереди на запись"` and did not move for hours; `/api/user/amounts` showed `used: 0` of 300 (the bot did not record a second): the bot never joined the meeting. This is NOT our bug and NOT a lack of minutes: s2t accepts the task but does not dispatch the bot from the recording queue. On 11 June it was fixed by restarting workers on the s2t side. Diagnostic symptom: the task hangs in `queued/30 "В очереди на запись"` and `amounts.used` does not grow. Live examples: `l80ndH2qLDNELXRNxDLwuSnYb-OmBDja`, earlier `_IN2JjDhnzJZqfDSIzYmjoBY3jQzwaT5`.

> **CRITICAL: Incident "the bot does not leave a persistent room" (LIVE 2026-06-17, acknowledged by s2t).** Different from "did not join": here the bot joined and records (`code 30 "Бот ведёт запись встречи"`, minutes tick), but after the user leaves the Telemost room `/j/<id>` the bot **does not leave on its own**: the room is persistent, does not close, there is no "meeting ended" signal, and auto-exit (§2.3 item 1) does not trigger. The task hangs in `code 30` indefinitely; now our watchdog (`JOB_MAX_AGE_HOURS`, counted from `created_at`) finishes it: it calls `POST /stop`, waits for the transcript and charges the actual duration when the result is non-empty; a refund remains only for no-result. **s2t acknowledged the bug and promised a fix** (ticket 2026-06-17). Live example: task `i57jv02UkZ3RnUo_t9rAP4UHxsgpdW6g` (Telemost, hung in `code 30` after the user left). Workaround until the fix: `POST /stop` finalizes with a transcript (§2.3 item 2). Consequence: §2.3 item 1, "auto-exit yields `done`", **does not cover persistent rooms**, which do not signal "ended".

> **CRITICAL: Historical incident, the fix did not work (LIVE 2026-06-22).** After the s2t-declared auto-exit fix (2026-06-19: "the bot leaves if nobody is in the room for 10 min") a live retest was run on prod: a persistent Telemost room, the user ended the call, and the task hung in `code 30 "Бот ведет запись встречи"`, `result: null` for more than 20 min (auto-exit did not trigger). A direct `GET /api/recognitions/{id}` confirmed it. At that point prod was temporarily rolled back to mymeet. The zombie risk is closed by our watchdog/stop flow, not by trusting the provider's auto-exit.

### 2.6 Meeting result

After `done` (code 200) the task object gets a `result` with links to the formats:
```json
"result": {
  "raw":  ".../api/recognitions/{id}/result/raw",
  "txt":  ".../api/recognitions/{id}/result/txt",
  "srt":  ".../result/srt", "vtt": ".../result/vtt",
  "json": ".../result/json", "xml": ".../result/xml"
}
```
You can follow these URLs or call `GET /api/recognitions/{id}/result/{format}` directly (§5).

**Result availability period:** the exact TTL is not stated in the API documentation. In the prod incident
of 2026-07-14, a result request after about 7 days returned HTTP 410 ("the result lifetime
has expired"). Therefore the product contract treats `result/*` links as available for no longer than a week.
The period of up to 90 calendar days stated in the privacy policy refers to the processing of audio data and does not
guarantee that the result is available through the API.

For a meeting with a password: pass `access_code` at creation; otherwise the task ends with `error`.

## 3. File and link API (debugging / fallback)

Almost not needed in the transcrib architecture (calls come through the Meeting API), but useful for debugging and one-off uploads.

### 3.1 File upload

```
POST /api/recognitions/task/file
Content-Type: multipart/form-data
```

Fields: `file` (required), `lang`, `speakers`, `min_speakers`/`max_speakers`, `multi_channel` (`1` is stereo, one channel per speaker; usually PBX recordings).

Allowed extensions (extended list from the new PDF): `3gp, 3gpp, 7z, aac, adts, amr, avi, awb, caf, dav, f4v, flv, m4a, m4p, m4v, mkv, mov, mp3, mp4, mpeg, mpeg4, mpg, mpga, mts, mxf, oga, ogg, opus, rar, ts, wav, webm, wma, wmv, zip`.

### 3.2 Upload by link

```
POST /api/recognitions/task/link
Content-Type: application/json
{"lang": "ru", "url": "https://example.com/audio.mp3"}
```
The parameters are the same. External sources are also supported (in the PDF example: YouTube Shorts).

### 3.3 Task object (file/link response)

```json
{
  "id": "y8mNppLfE9p9IWZISs-ZymkSoD48Gyxx",
  "created": null,
  "file_source_id": "19606360",
  "options": {"lang": "ru", "speakers": null, "multi_channel": null},
  "file_meta": {"mime": "audio/mpeg", "format": "MPEG Audio", "audio_format": "MPEG Audio", "channels": 1, "duration": "00:00:59"},
  "resource": {"type": "file", "name": "mp3test_dialog.mp3"},
  "status": {"code": 100, "description": "В очереди на распознание"},
  "payment": {"source": 1, "price": 0},
  "result": null
}
```
(`description` "В очереди на распознание" means "In the recognition queue".) For a link task `resource.type = "link"`, `resource.url` is present, and the initial `status.code` = 0 ("Задание создано", "Task created"). `file_meta` can be `[]` until the file is processed.

## 4. Status check and the task object schema

```
GET /api/recognitions/{TASK-ID}
```

Returns the task object. Field descriptions:

| Field | Type | Description |
|---|---|---|
| `id` | string | Task identifier (hash) |
| `created` | string | Creation date/time (UTC) |
| `file_source_id` | int | Internal source ID |
| `options.lang` | string | Recognition language |
| `options.speakers` | int\|null | Number of speakers (null is auto) |
| `options.multi_channel` | int | Multi-channel mode (0 is off) |
| `file_meta` | object\|array | File metadata (mime, format, channels, duration); `[]` until processed |
| `resource.type` | string | `file` or `link` (meetings are `link`) |
| `resource.url` / `.name` | string | Meeting URL / file name |
| `status` | object | Current status (see below) |
| `payment.source` | int | Payment source: 1 is the plan package, 2 is a charge from the personal account |
| `payment.price` | float | Task cost |
| `result` | object\|null | Links to the result formats (after `done`) |

**Numeric statuses of the file/link API** (field `status.code`, without `value`):

| code | Description |
|---|---|
| 0 | Task created |
| 80 | Content received |
| 100 | Recognition in progress / queued |
| 200 | Recognition succeeded |
| 501 | Recognition error |

In the Meeting API `status` additionally contains a text `value` (§2.5). The mapping summary is in §9.

Confirmed: **live shape of the task object (recorded 2026-06-11, a meeting task on Telemost):**
```json
{"id":"J_aWF...","created":"2026-06-11 13:21:31","file_source_id":22627675,
 "options":{"lang":null,"speakers":null,"multi_channel":0},"file_meta":[],
 "resource":{"type":"link","url":"https://telemost.yandex.ru/j/..."},
 "status":{"code":30,"value":"queued","description":"В очереди на запись"},
 "payment":{"source":1,"price":0},"result":null}
```
It matches the table above. Warning: a nuance against the PDF: `code 30` in the live API may mean "В очереди на запись" ("In the recording queue"; the bot is NOT in the meeting yet) rather than "the bot joined" from §2.5. So `speech2text.go` distinguishes `code=30` by `description`: the queue stays `Pending`, joining/recording moves to `OnCall`.

## 5. Getting the result

```
GET /api/recognitions/{TASK-ID}/result/{FORMAT}
```

Formats: `raw`, `txt`, `srt`, `vtt`, `json`, `xml`, `plain-txt`, `plain-txt-no-speaker`, `docx`.

For transcrib the main ones are `json` (segments with speakers and timecodes) or `txt` (a ready line "Спикер N: hh:mm:ss - text", where "Спикер N" means "Speaker N"). The format for assembling a Google Doc / `.md` is decided during the development of `google.go`.

Confirmed: **the `result/json` structure was recorded by a live test on 2026-06-11** (meeting task, Telemost; the raw response is saved to `jobs.raw_provider_response JSONB`):
```json
{
  "chunks":   [ {"text": "...", "time": {"from": 301.3, "to": 302.7}, "speaker": 4} ],
  "speakers": [ {"id": 4, "name": "Спикер 3"} ],
  "languages": ["en"]
}
```
- `chunks[]` are segments: `text` (string), `time.from`/`time.to` (seconds, float), `speaker` (int id referring to the `speakers` directory).
- `speakers[]` is the directory: `id` plus a generated name "Спикер N" ("Speaker N"); there are no real names (confirms the anti-scope in `product.md` §6).
- `languages[]` are the detected languages.
- Warning: there is no `confidence` field. Warning: on a silent recording the ASR hallucinates short "You." segments with `languages: ["en"]` and produces phantom speakers (8 "speakers" in an empty meeting); diarization can be evaluated only on real speech.

## 6. Webhook API

Base URL: `/api/webhook`. Authorization: `Authorization: Bearer {api_key}`. One client = one webhook.

| Method | Purpose |
|---|---|
| `POST /api/webhook/create` | Create. Body: `url` (required), `is_enabled` (default true), `events` (non-empty array) |
| `GET /api/webhook/get` | Get the current client's webhook |
| `POST /api/webhook/update` | Update (any subset of fields) |
| `POST /api/webhook/delete` | Delete (no body needed) |

The only event: `transcription.status_changed` (on any status change).

The create/get/update response is `{webhook_uid, url, is_enabled, events}`. `400` errors: `URL is required`, `Invalid URL format`, `Events array is required and must not be empty`, `Webhook already exists`, `Client not found`, `Webhook not found`, `At least one field must be provided for update`, `URL cannot be empty`. A deleted webhook can be created again but cannot be updated.

### 6.1 Incoming payload (Speech2Text to our URL)

```json
{
  "event": "transcription.status_changed",
  "timestamp": "2026-03-23T10:15:23Z",
  "transcription": {
    "id": "a1b2c3d4-...",
    "status": "done",
    "previous_status": "processing",
    "error_message": null
  },
  "secret_token": "your-secret-token",
  "webhook_uid": "550e8400-e29b-41d4-a716-446655440000"
}
```

`transcription.status` holds text statuses: `created`, `processing`, `paused`, `done`, `error`, `deleted`.

> **Important (an old question is closed):** the PDF states explicitly that `secret_token` "will be filled in with **your API access key**". So there is no separate secret: we compare the incoming `secret_token` with our own API key (constant-time, `subtle.ConstantTimeCompare`), and on mismatch return `403`. TLS is mandatory through the current nginx edge. This is weaker than HMAC: the token is in the payload, and anyone who learns it can send forged events. Store it next to the secrets.

### 6.2 Retries and handling

Our endpoint must return HTTP 2xx. Otherwise there are retries: after 5 / 10 / 15 / 20 minutes, the 5th attempt is final and the delivery status becomes `failed`.

Consequence for the architecture: the handler is idempotent (one event up to 5 times); quick-ack (parse, validate `secret_token`, write to the DB queue, 200); the polling safety net in `orchestrator.go` finishes the status through `GET /api/recognitions/{id}` if the webhook did not arrive.

## 7. Minute limits and payment from the personal account

```
GET /api/user/amounts
```
Response: `{"minutes": {"available": 360, "used": 0}, "amount": 0}`: the remaining plan minutes and the personal account balance.

When minutes run out, the task moves to `paused` (code 102). Then payment from the personal account:
```
GET   /api/recognitions/{TASK-ID}/price   # {"duration":1.9,"pricePerMinute":0.4,"price":0.76}
PATCH /api/recognitions/{TASK-ID}/pay     # charge and resume
```

`pricePerMinute` in the PDF example is `0.4 RUB/min` (probably the test access rate; the production one follows the `rates-api` plan). Consequence: `billing.go` forecasts quota exhaustion through `/user/amounts`; on `paused` it either tops up the personal account or stops the Job with a clear error.

## 8. Deletion and task list

```
DELETE /api/recognitions/{TASK-ID}
```
Warning: only while the task is **queued**, not in progress. After `processing`/`done` the audio/transcript cannot be erased programmatically.

Consequence for 152-FZ: "delete the audio after success" is not performed automatically. Either we ask support for a separate deletion endpoint after `done`, or we account for the fact that the privacy policy allows processing of audio data for up to 90 calendar days. This period does not guarantee result availability through the API; the product result TTL is described in §2.6.

```
GET /api/recognitions?page=PAGE&per-page=LIMIT   # page>=1, per-page 10..1000 (default 50)
GET /api/recognitions/processing | /complete | /errors
```
Needed mostly for admin scripts; production state is stored in our Postgres.

## 9. Status summary (file vs meeting)

| Numeric (file, `status.code`) | Text (meeting, `status.value`) | Meaning |
|---|---|---|
| 0 | `queued` (0) | Task created |
| 80 | none | Content received |
| 100 | `queued` (30) | Processing / bot joined / recording |
| 200 | `done` (200) | Success |
| none | `done` (204) | Finished, no speech detected |
| 501 | `error` (501) | Recognition error |
| none | `error` (404/406/407/502) | Did not join / format / duration limit / loading from source |
| none | `paused` (102) | Not enough minutes |
| none | `cancelled` | Stopped through `/stop` |

The webhook delivers text statuses (`created`/`processing`/`paused`/`done`/`error`/`deleted`). The exact mapping of numeric to text codes is to be confirmed empirically during the first implementation.

## 10. What the documentation does NOT cover (follow-up to support)

Most of the earlier gaps are closed by the new PDF (stop endpoint, statuses, secret_token, response schemas). What remains:

- **Internal structure of `result/json`** (segment fields: speaker, text, timecodes, confidence): to be learned by a live test.
- **Deleting a task after `done`** (for 152-FZ): `DELETE` works only while queued.
- **The bot's alone-in-room timeout** in a meeting, behavior in a waiting room.
- **An endpoint "is the bot in the meeting now"**: there is none, but the status `queued`/30 "recording" covers it.
- **Limit of simultaneous streams** above the plan: at the plan level, not in the API.
- **SLA, DPA, data isolation between customers** (after the incident in reviews: `README.md`, section "Legal").
- **Host discrepancy** `api.speech2text.ru` vs `speech2text.ru` in meeting/webhook (§0).

## 11. Sketch of speech2text.go (backup implementation)

Implements `TranscriptionProvider` (`internal/app/transcription.go`). Speech2Text is the economically preferred provider and the current prod/dev provider; the live-risk of persistent rooms is closed by our watchdog/stop flow. `mymeet.go` stays the working fallback behind the same interface. In the walking skeleton the real provider is replaced by `mock.go` (`TRANSCRIPTION_PROVIDER=mock`).

```
Submit(ctx, job)        -> POST /api/meetings/join                 -> external_id (id)
Poll(ctx, externalID)   -> GET  /api/recognitions/{id}             -> status.value -> our Status
Fetch(ctx, externalID)  -> GET  /api/recognitions/{id}/result/json -> transcript + speakers
Cancel(ctx, externalID) -> POST /api/meetings/{id}/stop            -> fragment finalization with a transcript (§2.3; billable if the transcript is non-empty)
EnsureWebhook(ctx, url) -> get -> create/update (idempotent)
Amounts(ctx)            -> GET  /api/user/amounts                  -> quota forecast
```
`domain.Job`: `ExternalJobID` (S2T id), `Status` (mapping of their statuses into our state machine); the meeting password (`access_code`) is extracted from the `pwd` query parameter of the link through `meetingAccessCode`; there is no separate `MeetingPassword` field in `Job`.

> **Implementation status:** `speech2text.go` implements only `Submit`/`Poll`/`Fetch`/`Cancel` (the current `TranscriptionProvider` interface). `EnsureWebhook`/`Amounts` from the sketch above are **NOT implemented** (webhook intake §6 and quota forecast §7 are the Stage 3 plan; `web.go` does not exist yet).

## 12. Code examples

The PDF has ready examples in PHP / Python / JavaScript for the file flow (pp. 22-24). For Go they are needed as a contract reference, not as code. Official Python SDK: `https://github.com/Speech2TextR/s2t-client-python`. Language codes for `lang`: Appendix 1 in the PDF.
