# mymeet.ai: working fallback provider

Dossier on mymeet.ai. API contract: [`API.md`](API.md), original spec: [`openapi.json`](openapi.json). Speech2Text is economically preferred at scale and the current prod path under live-risk; mymeet is the working fallback, switched by one line.

Research dated 2026-06-03; API price obtained 2026-06-10.

## Status: working fallback

mymeet technically covers the use case fully, and its **API is clearly production-grade** (public Swagger, GitHub API-Docs, MCP server); E2E passed live. Right now it is the fallback in case Speech2Text degrades. Warning: for long-term public use mymeet requires lifting clause 7.4 (a B2B contract, weeks). On economics at scale it loses to Speech2Text for two reasons:

1. **Economics.** The API plan is per minute: 1.5 RUB/min (12 months) / 2 RUB/min (6 months) = 90-120 RUB/h, linear with no minimum. At the target scale of 1000-3000 h/month that is **3-7x more expensive** than the Speech2Text flat ceiling and eats the margin of the 1,990 RUB subscription for 15 hours (§2). It is competitive only in a pilot with small volume (break-even ~70-90 h/month).
2. **Legal risk of clause 7.4 of the offer:** a ban on "creating a competing product". mymeet has its own `@mymeetaibot` (~$1.3M ARR); we are a direct competitor for the same Telegram users, so clause 7.4 protects their real revenue, not a formality. Without a B2B contract lifting clause 7.4, the service cannot be published.

Plus **weak diarization on single-channel audio** (VC.ru test ~70% against the claimed 96-98%): a general ASR risk, but for mymeet it is documented by reviews.

**mymeet advantage:** the summary comes **free from a template** (11 templates, processed on their side in Russia): F3 without OpenRouter and without cross-border transfer of personal data. By default F3 in the MVP is covered by our own `Summarizer` through OpenRouter, not by a mymeet template; a template summary removes the cross-border transfer of personal data, which is an additional plus for mymeet.

**Fitness verdict: 3/5:** fine as a working fallback, loses to S2T on economics at scale, plus the legal risks of clause 7.4.

## API (brief)

Full contract: `API.md`. Key points (**verified by a live test with a production key on 2026-06-16**): live join `POST /api/record-meeting` -> `{"meeting_id"}`, **the source for Telemost is `yandextelemost`** (openapi), NOT `telemost` (MCP client): the backend accepts `telemost` with `200` but does not place a bot -> a permanent `404`; the source of truth for `source` is openapi. Polling `GET /api/meeting/status` -> **plain-text status** (not JSON), an undocumented **`recording`** actually arrives, **there are no webhooks**, a freshly created meeting first returns `404`; the result `GET /api/video/report` -> transcript in `followup_v2.chapters[].transcript[]` (segment `{speaker:{...,speaker}, text, timestamp}`; the top-level `result.transcript` from the MCP was **refuted**) + summary from `followup_v2.templates[]` (the mymeet-summary slice, merged 2026-06-18: delivered as a second file, see `API.md` §1.3); or `GET /api/storage/download?format=md` -> a ready .md into the chat. Deletion is `DELETE /api/video/report` (there is no `delete-meeting` route); there is no programmatic cancellation of an active recording. Auth is `X-API-KEY`. A custom summary prompt is not supported: only `template_name` from the enum.

## 2. Economics (why mymeet is expensive at scale)

### 2.1 API plan per minute (confirmed by sales 2026-06-10)

| Prepayment | Rate | RUB/hour |
|---|---|---|
| 6 months | 2 RUB/min | 120 RUB/hour |
| 12 months | 1.5 RUB/min | 90 RUB/hour |

Linear, no monthly minimum; sales confirmed our volume ("you pass with more than enough"). Retail subscriptions (Lite 850 RUB/500 min, Pro 2,490 RUB) do not fit our model. Invoice payment for legal entities; the reseller program (32-45%) is not what we need (we need white-label).

Break-even ~70-90 h/month: below it mymeet (per-minute) is cheaper, above it Speech2Text (flat); at scale the gap is 3-7x.


## 3. Legal and risks

- ООО «МАЙМИТ АИ» (MyMeet AI LLC), INN 9705223482, OGRN 1247700316038, Moscow, registered 15.04.2024. Servers in Russia, 152-FZ, audio is deleted right after transcription.
- **Clause 7.4 of the offer** ([offer-for-access](https://mymeet.ai/documents/offer-for-access)): a ban on derivative products / a competitor; a B2B contract is needed. `openapi.json` refers to a separate ToS ([Google Doc](https://docs.google.com/document/d/1drhWmEGdfIzwBiW_MPE2IAq6H0Nauo_51AMVb5vc89Q/edit)); which one governs the API is to be clarified.
- The summary LLM is not disclosed (if foreign, a cross-border transfer of personal data). Roskomnadzor register of personal data operators: not confirmed.
- Telemost: public only (corporate Yandex 360 does not work); the bot name can be customized only on Business.

**Competition (context of clause 7.4):** `@mymeetaibot` is a mature B2C product in Telegram (~15-30k users, $1.3M ARR, 8 platforms, PDF into the chat, AI chat about the meeting). Their API is secondary to the note-taker bot, so giving it to a competitor is structurally unattractive, hence the weight of clause 7.4. (Speech2Text does not suffer from this: its TG bot is not core.)

## 4. Open questions (if we activate mymeet)

- **Clause 7.4:** whether our scenario (an orchestrator on top of the API) is allowed, which contract secures it, which document governs the API (offer vs ToS).
- Fixing the 1.5-2 RUB/min rate by contract, a volume discount; cashless payment for sole proprietor/LLC (invoice/act/UPD/VAT); raising the concurrent limit (default 2 per account, `API.md` §5) on B2B/wholesale.
- Technical: what remains after the live E2E of 2026-06-16 is the shape of `failed`/`failure_reason` and diarization on a multi-voice call (`API.md` §5; the `report` schema, idempotency 409, concurrency are already closed).

## 5. Sources

[mymeet.ai](https://mymeet.ai/) (+ /pricing, /partners, /changelog, /documents/offer-for-access), [API-Docs](https://github.com/MyMeetAI/API-Docs), [MCP server](https://github.com/MyMeetAI/mymeet-mcp-server), [vc.ru review](https://vc.ru/ai/2182325-tekhnologii-ii-dlya-vstrech-obzor-mymeet-navstreche-i-followup), [checko.ru legal entity](https://checko.ru/company/maymit-1247700316038).
