# transcrib product passport

> **Canon for:** product requirements (F0-F9), JTBD, target audience, anti-scope, monetization, metrics, roadmap, risks.
> **Mirrors (link to this file):** `architecture.md`, `../README.md`.
> **Do not duplicate here:** architecture details (`architecture.md`), DB schema (`db.md`), payment flow (`payments.md`).
>

## 1. Product essence

A bot that automatically joins online meetings (Zoom, Google Meet, Yandex Telemost), records the conversation, creates a structured summary with tasks and sends everything to a Telegram bot.

## 2. Job-to-be-done

**Main job:** "Do not lose what was decided at the meeting, and do not spend time on manual note-taking".

- **Layer 1: Time saving.** The manager does not take notes, does not export the recording from Zoom, does not forward it manually. The bot sent it, then forward to the team chat. 30 seconds instead of 15 minutes.
- **Layer 2: Control of agreements.** The summary with tasks is visible to the whole team. No excuse of "I did not hear". Tasks do not get lost.
- **Layer 3: Knowledge base (wow factor).** Accumulated transcripts become management infrastructure: employee development maps, role portraits, feedback about oneself as a manager.

## 3. Target audience

### Main segment

| Parameter | Description |
|---|---|
| Role | Team managers (5-70 people), entrepreneurs |
| Calls per week | 5-20 (stand-ups, 1-on-1, strategy sessions) |
| Meeting platforms | Zoom, Google Meet, Yandex Telemost |
| Messenger | Telegram (main work channel) |
| Willingness to pay | Yes, used to SaaS subscriptions |
| Acquisition channel | Personal brand (50K+ followers combined, growing) |

### Secondary segment (viral)

Managers, curators, salespeople: employees of the entrepreneurs from the main segment. The owner buys for themselves, then rolls it out to the team. One payer = several users.

### Anti-persona: who we do NOT sell to

- Solo freelancers without a team (nobody to forward the summary to).
- Corporations with strict security requirements (they will not let third-party bots in).
- Those who do not use Telegram as a work tool.

## 4. User scenarios

- **US-1.** As a manager, I want to send the bot a meeting link and receive a summary in Telegram, so that I do not spend time on manual note-taking.
- **US-2.** As a manager, I want to forward the summary to the team chat with one tap, so that the whole team sees the agreements and tasks.
- **US-3.** As a manager, I want to get the full transcript in a Google Doc, so that I can use it for work with AI (development maps, vacancies, feedback).
- **US-4.** As an entrepreneur, I want to roll the bot out to my managers, so that I can control the quality of their meetings.
- **US-5.** As a user, I want to set up the bot in 2 minutes through Telegram without registering on a website.

### 4.1. Onboarding

1. `/start`: the bot creates the user by `telegram_id` and briefly explains what it can do.
2. **Personal data processing consent (152-FZ)**: the bot shows the consent text with two buttons "Согласен" / "Не согласен" ("Agree" / "Disagree"). Without consent any other command replies "нужно согласие, нажми /start" ("consent required, press /start"). On consent, `consent_at = now()` is recorded.
3. In release 0.1 the bot leads the user through consent and payment after the trial is used up. Google linking is enabled only in the future Google mode; until then, accepting Google links is not blocked.

### 4.2. Google linking (post-0.1)

1. The bot gives an OAuth authorization link (scope `drive.file`).
2. The user agrees, the bot receives the tokens, creates the folder "Транскрибации созвонов" ("Call transcripts") in the user's Drive, and saves the user's `folder_id`.
3. The bot confirms success.

### 4.3. Trial and payment (F6, F7)

1. The first 3 hours are free. Inside the system accounting is kept in minutes.
2. After the trial is used up, or earlier on request, the bot offers payment via a direct YooKassa link (see `docs/payments.md`).
3. After a successful payment the subscription is active with a limit of 15 hours/month; publicly we say "month", internally the period counts as 30 days.
4. The result duration comes from the provider in seconds; on billing the seconds are dropped, but a non-zero meeting shorter than 60 seconds charges 1 minute.

### 4.4. Main scenario: happy path

1. The user sends a meeting link to the bot chat.
2. The bot checks rights (trial or an active subscription with remaining minutes) and reserves up to 60 minutes from a single source.
3. Through the transcription provider API (current prod: s2t under observation; mymeet: working fallback) a bot participant joins the call.
4. When the call ends, the bot receives the transcript by speaker; the F3 summary is a separate step through OpenRouter, if the provider did not return a ready summary.
5. It assembles two markdown artifacts: the full transcript and the summary.
6. It sends the user the final message and the files in Telegram. Google Doc remains a post-0.1 mode.

### 4.5. Balance check

`/balance`: the bot shows the remaining hours/minutes and the date of the next reset.

## 5. MVP functionality: what we DO

| # | Function | Description |
|---|---|---|
| F0 | Personal data consent | On the first `/start` a mandatory step: the consent text (152-FZ) and two buttons. Without consent, accepting links is blocked. `consent_at` is recorded in the DB. |
| F1 | Joining a meeting | The user sends a meeting link to the bot chat. The bot joins as a participant through the transcription provider API. |
| F2 | Recording and transcription | The bot records audio and converts it to text with speaker diarization; participants' display names may come from the meeting platform or the transcription provider. |
| F3 | Summary with tasks | AI generates a structured summary: key topics, decisions, tasks with owners. On the Speech2Text path the summary is made by our `Summarizer` through OpenRouter; mymeet may return a provider-summary from a template. |
| F4 | Sending to Telegram | The summary + full transcript arrive as a message in Telegram. |
| F5 | Google Doc | Two Google Docs (transcript + summary) are created automatically, links in Telegram. *Post-0.1; release 0.1 delivers the result as `.md` files in Telegram.* |
| F6 | Free trial | The first 3 hours are free, then an offer to pay for a subscription. |
| F7 | Payment through Telegram | Subscription payment from the bot via a direct YooKassa link: the bot sends a payment button, YooKassa accepts the card and returns the status through the API. With `RECURRING_PAYMENTS_ENABLED=true` auto-renewal is on by default after payment; with `false` the payment is one-off. The payment method is saved only if YooKassa returned `payment_method.saved=true`. See `docs/payments.md`. |
| F8 | Setup through Telegram | All subscription setup goes through the chat with the bot. Google OAuth is added in the post-0.1 Google mode. |
| F8.5 | Google reconnect | The `/reconnect` command: repeated OAuth. Post-0.1: used on access revocation, refresh token expiry in Testing mode (every 7 days), 401 on refresh during a Job. After reauth all the user's Jobs in status `awaiting_reauth` are "woken up". |
| F9 | Subscription pushes | Transactional nudges in Telegram: the trial is ending, the user looked at payment and did not pay, less than an hour of paid time is left, paid hours ran out before the calendar expiry, the subscription expires in 3 days, the subscription expired. After a billable terminal path with a zero balance the bot sends a separate message `Баланс закончился` ("Balance is used up"). For subscriptions with the recurring worker enabled, the worker auto-renews the period; `/balance` shows the auto-renewal status, lets the user disable it and start payment for a new period. |

## 6. Anti-scope: what we do NOT do in the MVP

Protection against scope creep. Any addition requires a conscious decision and a justification.

- Guaranteed anonymization of speakers into "Speaker N".
- Auto-join from a calendar (v1.1).
- Pulling the meeting title from a calendar.
- Mobile app: everything goes through Telegram.
- Web interface / personal account.
- Editing the transcript inside the service.
- Integrations with CRM, Notion, Trello, Asana and the like (v2.0).
- Video recording (audio transcription only).
- Built-in AI chat for questions about a meeting.
- Multilingual support / language auto-detection.
- Team accounts and roles.
- Meeting analytics / dashboards.
- Referral links (v2.0).
- Multiple tariffs (v2.0).
- Partial refunds and self-service refunds. For the MVP refunds are performed manually by an operator through the YooKassa dashboard/support; REST refund remains a technical smoke/fallback without a user-facing admin flow.
- Anti-fraud protection against trial exploitation through many Telegram accounts (the risk is low in the closed beta).
- A separate `/account` command: `/balance` covers the basic need.

## 7. Supported platforms

| Platform | MVP status | Note |
|---|---|---|
| Zoom | Yes | Main platform of the audience |
| Google Meet | Yes | Second most popular |
| Yandex Telemost | Yes | Critical for the Russian market |
| Microsoft Teams | No | Can be added later on request |

**Interface:** Telegram bot only. No website, personal account or app.

**Language:** Russian. English and others are not in the MVP.

## 8. Monetization

A monthly subscription with an hour limit. One tariff at the start.

| Parameter | Value |
|---|---|
| Free trial | 3 hours without payment |
| Subscription price | 1 990 ₽/month for 15 hours |
| Limit | 15 hours/month (cap to protect margin) |
| Payment | From Telegram via a direct YooKassa link |

**Launch price: 1 990 ₽/month** (customer decision 2026-06-19). The margin depends on the transcription provider tariff (see below).

### 8.1. Hourly accounting contract

- The user sees hours: 3 hours of trial and 15 hours in the monthly subscription.
- Runtime and DB count only minutes.
- At Job start up to 60 minutes are reserved from a single source: the nearest active subscription, otherwise the trial.
- Before the point of service delivery, a cancel/provider/start error returns the whole reserve.
- On `done` or `failed_summary` billing is finalized by the actual duration of the result.
- If the duration is less than the reserve, the unused reserve returns to the original source.
- If the duration is greater than the reserve, the extra is taken only from the original source and only with a sufficient active balance.
- The next subscription is not charged for the overrun of the current meeting; no debt or negative balance is created.
- We do not warn about a possible overrun before the call. If after a billable terminal path the balance became 0, we send a separate message `Баланс закончился` ("Balance is used up").

**Additional risk:** the financial model assumes a usage-based tariff at the transcription provider. At mymeet the API tariff is per minute: 1.5-2 ₽/min = 90-120 ₽/h (linear), which gives a **negative margin** at scale; s2t is a ruble flat "24 Hours" plan (17 600 ₽/month: 24 h/day, 3 streams, 0.40 ₽/min above the limit, about 24 ₽/h), effectively about 24-35 ₽/h at scale, the margin is positive. This economics made s2t the economically preferred choice (final 2026-06-19), but s2t remains a live-risk track because of zombie/empty results. Calculations: `docs/api/mymeet/README.md` §2.1, S2T tariffs: `docs/api/speech2text/README.md` (the "Pricing" section). The launch price is fixed (1 990 ₽); margin sustainability depends on:
1. Utilization of the s2t flat "24 Hours" plan (economically preferred, holds the margin at scale; worth monitoring actual utilization). The mymeet fallback (per-minute) is loss-making at scale, but is needed as a functional safety net.
2. Accounting for VAT +22% on the acquiring commission (in effect since 2026-01-01, see `docs/payments.md` §3).

The early plan "closed beta free / up to 500 ₽" was replaced by the decision to launch at 1 990 ₽ (2026-06-19); early cohorts may be encouraged with a discount at the customer's discretion.

## 9. MVP success metrics

Evaluation 3 months after launch:

| Metric | Target value | How we measure |
|---|---|---|
| Trial to subscription conversion | ≥ 15% | Paid / finished trial |
| Retention (month 2) | ≥ 60% | Renewed / paid in month 1 |
| NPS | ≥ 40 | Survey after 2 weeks of use |
| Meetings per user | ≥ 3 per week | Average number of processed meetings |

## 10. Roadmap

**Strategy after the June 5 meeting:** we go in slices from a minimal working product to the full F0-F9. Each stage adds one subsystem. This lets us get a working bot by June 30 without depending on long-running external processes (YooKassa, Google verification, B2B contracts with providers).

### Release 0.1 / MVP candidate (2026-06-25)

A working prod/test setup to check the hypothesis and collect feedback from 5-10 close users. Payment was pulled into the MVP (decision 2026-06-14) and verified on the test bot up to a successful payment.

- **In the scope of release 0.1:** F0 (consent), F1 (link intake), F2 (transcription through a working API), F3 (AI summary through `Summarizer`), F4 (sending to Telegram as a file + message), F6 (trial), F7 (YooKassa payment), F8min (`/start`, `/balance`, `/help`), F9 (pushes).
- **Post-0.1:** F5 (Google Doc) and F8.5 (`/reconnect`/`awaiting_reauth`) remain a separate Google slice through Testing mode. Release 0.1 delivers the result as `.md` files in Telegram.
- **Payment (decision 2026-06-14, updated #26/#27/#9):** F6 trial + F7 YooKassa direct payment links (`docs/payments.md`) work under the contract of 3 hours of trial and 15 hours/month with minute accounting. The branch `feature/yookassa-payment-links` added the email for the receipt, `POST /v3/payments`, the `confirmation_url` button, polling `GET /v3/payments/{id}`, full refund by `payment.id`. The old Telegram Payments E2E up to `successful_payment` remains baseline history, but is not the target go-live flow. The test payment-link smoke, the technical refund smoke and the prod live payment-link + recurring smoke have passed; the successful prod payments so far were owner smoke, the first external payment remains a monitoring event.
- **F3 summary:** s2t returns only the transcript, we produce the summary ourselves through OpenRouter; the mymeet fallback may return a ready summary from a template. We check prompt quality with live runs.
- **Telemost** is supported by both providers. It needs no separate stage.
- **Out of scope of release 0.1:** Google delivery, Google Production (brand verification), v1.1 features.

### Google Drive: post-0.1

F5 + F8.5 are implemented after release 0.1 (issue #13, P2): a Google Doc in the user's Drive, `/reconnect`, `awaiting_reauth`, Testing mode. Then the move to Production: brand verification for the non-sensitive `drive.file` takes **about 2-3 working days of Google review**, not 4-6 weeks (that estimate applied to sensitive/restricted scopes, which we do not have). The real prerequisite is our infrastructure (homepage + privacy on the domain + Search Console); canon: `docs/google.md` §7. After Production the 7-day expiry of Testing mode refresh tokens goes away.

### v1.1 (after 50 users): hypothesis

- Automatic joining of meetings from Google Calendar.
- Customization of the summary format (templates).
- Microsoft Teams support.

### v2.0 (distant horizon): hypothesis

- Knowledge base of meetings with AI search.
- Integration with task trackers (Notion, Trello).
- Team accounts with roles and a dashboard.
- Multilingual support.

*v1.1 and v2.0 are only hypotheses, not commitments. The decision on development is based on MVP metrics and feedback.*

## 11. Risks and assumptions

### Assumptions the plan relies on

- The audience is ready to pay about 2 000 ₽/month for a meeting tool.
- Managers hold enough online meetings for regular use.
- People are ready to let a third-party bot into their meetings (the privacy barrier is surmountable).

### Risks

| Risk | Probability | Consequences | Mitigation |
|---|---|---|---|
| Privacy barrier: people do not want to let the bot in | Medium | Low conversion | Trial + user cases |
| Transcription/diarization quality is insufficient | **Medium** (see below) | Churn after the trial, NPS<40 | Diarization test on the current prod provider and fallback; at <85%, LLM post-processing of the transcript to relabel speakers |
| Competitors cut prices to zero | Medium | Margin pressure | Differentiation through Telegram UX |
| The developer misses deadlines | Medium | Launch delay | Weekly sprints, demo every 2 weeks |
| s2t reliability (economically preferred, chosen by economics 2026-06-19) | **Confirmed live risk** (zombie/empty results in manual tests) | Meeting errors, quota refunds, poor UX | Mymeet fallback through `TranscriptionProvider`; live s2t/OpenRouter series before the public launch; plan B: Recall.ai (without Telemost) or our own Whisper pipeline |
| Recall.ai changes prices/terms | Low | Cost growth | Monitoring of alternatives |
| The audience has few meetings | Medium | Low usage, churn | The trial will show the real frequency |

**Rationale for the medium diarization risk:** for s2t there are no independent WER/diarization metrics at all (only the claimed ~97%); for mymeet an independent VC.ru test showed about 70% real accuracy on single-stream Zoom/Meet/Telemost audio (claimed 96-98%). This hits the key value proposition "transcript by speaker" directly. A mandatory step is our own test of the current prod provider and the fallback with a 3-speaker meeting. At <85%, connect LLM post-processing to relabel speakers by context.
