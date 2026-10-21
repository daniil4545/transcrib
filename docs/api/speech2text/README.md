# Speech2Text: the economically preferred prod provider, under observation

Dossier: status, pricing, quality, legal, risks. API contract (file flow + Meeting API + Webhook): [`API.md`](API.md). Mymeet stays the working fallback: `../mymeet/README.md`.

Research dated 2026-06-03; API documentation obtained 2026-06-10.

## Status: prod/live-risk

Speech2Text is the **economically preferred** transcription provider at scale and the current prod path, but it remains live-risk: after the failure on 2026-06-22 (a zombie bot in a persistent room), support claimed fixes on 2026-06-24, and manual checks showed partial progress and new problems (an empty Google Meet, status/UX, summary quality). The economics are decided by the reference plan "24 Hours" (flat 17,600 RUB/month: 24 h/day, 3 parallel streams out of the box, diarization included, 0.40 RUB/min over the limit = about 24 RUB/h, an extra stream +100 RUB). It keeps a positive margin at a 1990 RUB subscription (~120-370 RUB/user), while per-minute mymeet (1.5-2 RUB/min) eats the margin at scale. Mymeet stays a working fallback; flipping is one line, `TRANSCRIPTION_PROVIDER`. Rationale for the economic choice:

- **Economics hold at scale.** Flat ruble plans beat per-minute mymeet at the target volume; the break-even is after **~70-90 h/month**, and at the pilot stage (tens of hours) mymeet is cheaper.
- **No structural conflict.** The TG bot @speech2textbot is not their core product (the main channel is web/extension/corporate), and the company is interested in providing the API. Mymeet is the opposite: it has its own @mymeetaibot ($1.3M ARR) plus **clause 7.4 of the offer**, "ban on a competing product".
- **Complete contract.** Meeting API + Webhook are officially documented (PDF 2026-06-10): `POST /api/meetings/join`, `POST /api/meetings/{id}/stop`, webhook `transcription.status_changed`. 8 platforms including Telemost.
- **Legal maturity.** ООО «Современные речевые технологии» (Modern Speech Technologies LLC; INN 9704223433, registered 2023), servers in Russia, Russian domestic software registry (no. 27300 of 2025-03-21), 152-FZ.

**Caveat (execution risk):** s2t already failed live tests on 2026-06-22; the current prod mode needs a series of live runs and fast debugging tools. Mymeet is kept as a fallback behind the same `TranscriptionProvider`.

**Counterargument (for honesty toward the customer):** S2T returns only the transcript; we build the summary (F3) through OpenRouter (`openrouter.go`), and that is a cross-border transfer of personal data to the USA. With mymeet the summary is generated from a template on their side (Russia). Mitigation: use a Russian LLM (YandexGPT/GigaChat) or obtain consent/a legal basis under 152-FZ.

**Open for a live test:** empty results, machine-distinguishable recording phases, the `result/json` structure on different platforms, audio deletion after `done`. Closed by the 2026-06-11 probe: the meeting endpoints are alive; the host is standardized as `api.speech2text.ru` (`API.md` §0).

## Pricing

Corporate plans (rubles), [speech2text.ru/rates](https://speech2text.ru/rates):

| Plan | Price/month | Limit | Streams |
|---|---|---|---|
| Corp. Start | 0 RUB + 2 RUB/min | none | 1 |
| Corp. Standard | 8,300 RUB | 12 h/day (~360 h/month) | 3 |
| Corp. Professional | 16,400 RUB | 24 h/day (~720 h/month) | 6 |

Sales offer for our case (2026-06-10): 24 h/day: 17,600 RUB/month; 48 h/day: 35,200 RUB/month (~1,440 h/month). Pay-as-you-go on Corp. Start (2 RUB/min) is convenient for a start without a minimum; over the limit on the other corporate plans it is 1 RUB/min. The API is available on corporate plans; there is no separate API plan. Reseller/white-label is not publicly offered: only through a direct B2B contract. Open: what counts as "hours per day", the stream limit above 6, the price for an active partner.

## Quality

- Russian recognition ~97% (claimed), no independent WER tests; we will measure on test access.
- Diarization: `speakers` / `min_speakers` / `max_speakers`, format "Спикер N: 0:00:00 - text" (the label means "Speaker N"), numbering is stable within a session. **Diarization quality on single-channel audio is the main product risk.** S2T has **no independent WER/diarization metrics at all** (only the claimed ~97%; mymeet at least has an external VC.ru test, ~70%).
- Summary: a built-in function exists, but per the spec transcrib builds its own through OpenRouter: S2T returns the transcript.

## Legal

- ООО «Современные речевые технологии» (Modern Speech Technologies LLC), INN 9704223433, OGRN 1237700673121, Moscow. Servers in Russia, no cross-border transfer (on the S2T side itself). Russian domestic software registry (no. 27300). 152-FZ in the policy. FSTEC certification is not confirmed; registration in the Roskomnadzor register of personal data operators: to be checked.
- **Deleting audio after `done` is programmatically impossible** (`DELETE` works only while queued). The privacy policy allows processing of audio data for up to 90 calendar days. This is not the result availability period: the live incident of 2026-07-14 showed HTTP 410 after about 7 days; the product contract limits history to one week (see `API.md` §2.6).
- A B2B contract is mentioned at /integration; cashless payment/invoice/act/UPD is standard for an LLC, details to be clarified.
- Participants' consent to recording is the user's responsibility (Art. 11 of 152-FZ), to be written into our user agreement.
- **Risk (otzovik review):** one case of fragments of someone else's audio being served is a potential personal data leak. Raise data isolation between customers at the contract stage.

## Architectural consequences (for `speech2text.go`)

- **The current runtime is polling-first.** `orchestrator.go` polls `GET /api/recognitions/{id}`; the webhook `transcription.status_changed` remains future hardening for `web.go` and must validate `secret_token`, write to the DB queue by key `(webhook_uid, transcription.id, status)` and answer 200.
- **`paused`** (not enough minutes, code 102): we monitor `/api/user/amounts`, keep a balance on the personal account against overflow (pay-per-task), or handle it in the state machine.
- **The result is `result/json`** (segments with speakers), fallback `txt`. The exact schema is to be learned on test access; the raw response goes to `jobs.raw_provider_response`.
- Interface sketch: `API.md` §11.

## Open questions

They do not block development (mock + raw JSONB). For the test/contract stage:
- The live s2t/OpenRouter series must be repeated after the current UX/provider fixes; s2t support is not treated as a source of truth without a green run.
- The `result/json` structure, the bot's alone-in-room timeout (the claimed auto-exit did not work on 2026-06-22), behavior in a waiting room.
- Programmatic deletion after `done` (152-FZ), data isolation between customers, DPA, SLA.
- The production plan for an active partner, the stream limit above 6, closing documents.

## Sources

[speech2text.ru](https://speech2text.ru/) (+ /integration, /rates, /privacy-policy, /meetings, /partner, /contact), [Habr code example](https://habr.com/ru/articles/926918/), [vc.ru test](https://vc.ru/services/1447638-kak-za-10-minut-rasshifrovat-chasovoe-audio-lyuboi-slozhnosti-testiruem-servis-perevoda-audio-v-tekst-speech2text), [otzovik reviews](https://otzovik.com/reviews/speech2text_ru-servis_rasshifrovki_audio_v_tekst/).
