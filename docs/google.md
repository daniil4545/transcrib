# Google OAuth + Drive for transcrib

> **Canon for:** the Google integration (account linking via OAuth 2.0, delivering the result as a Google Doc in the user's Drive), the choice of the `drive.file` scope, the refresh token lifecycle (Testing mode, 7 days), `/reconnect`/`awaiting_reauth`, markdown to Doc conversion, the Testing to Production path (brand verification), compliance (152-FZ, Google User Data Policy).
> **Mirrors:** `architecture.md` §14 (post-0.1 Google delivery), `db.md` §3.2/§3.6/§5 (`google_accounts`, `reauth_from`, indexes).
> **Google API contract** (OAuth endpoints, parameters, token exchange/refresh, `invalid_grant`, Drive v3 multipart+convert, errors, rate limits, the Go-level `x/oauth2`, scope classification, verification): [`docs/api/google/API.md`](api/google/API.md). This file holds integration decisions and flow, not the contract.
> **Implementation.** The slice plan was deleted when the project moved to maintenance; the track lives in issue #13 (P2, post-0.1). The plan history is in git up to the tag `0.0.3.7`.
> **Do not duplicate here:** SQL and schema (`db.md` §3.2/§3.6/§5), the Job state machine and runtime limits (`architecture.md` §7/§13), the wire contract (`api/google/API.md`).
>
> **Status:** canon of the post-0.1 Google integration (F5 Google Doc + F8.5 `/reconnect`). Release 0.1 delivers the result as `.md` files in Telegram; the Google mode is implemented as a separate slice `plan-google-slice.md` through **Testing mode without brand verification**. Brand verification (Testing to Production) is the next step after the live Google test.

Version: 1.0 (2026-06-14). Source: multi-agent research of the official docs `developers.google.com` + `support.google.com` (5 facets, the contract is moved to [`api/google/API.md`](api/google/API.md)). This document describes delivering the result to Google Drive and linking the account. It accompanies `architecture.md` (modules `google.go`/`web.go`/`google_delivery.go`) and `db.md` (table `google_accounts`).

## 1. Overview

```
┌──────────┐   OAuth link       ┌────────────┐  code to callback    ┌───────────┐
│ user in  │ <----------------> │   Google   │ ───────────────────> │  web.go   │
│ Telegram │  taps, grants access│  accounts  │                      │ (callback)│
└────┬─────┘                    └────────────┘                      └────┬──────┘
     │ result (link to the Doc)                                           │ code -> refresh_token
     v                                                            ┌───────v────────┐
┌──────────────┐                                                  │   google.go    │
│google_delivery│  Deliver(ctx, chatID, filename, caption, content)│ OAuth + Drive  │
│     .go      │ ───────────────────────────────────────────────> │  (drive.file)  │
└──────────────┘                                                  └───────┬────────┘
                                                                          │ folder + Doc
                                                                          v
                                                                  ┌──────────────┐
                                                                  │ User's Drive │
                                                                  └──────────────┘
```

**Roles:**
- **Google accounts** is the OAuth server: shows the consent screen, issues `code`, exchanges it for tokens.
- **Google Drive** is the storage of the result. Documents live **with the user**, we **do not store** them (we do not become a personal data operator for the content, `architecture.md` §14/§15).
- **Our service:** `google.go` (OAuth flow + creating the folder/Doc), `web.go` (callback + waking `awaiting_reauth` jobs), `google_delivery.go` (implementation of `DocumentDelivery`: create a Doc + send the link to the user), `tg.go` (OAuth link in onboarding, `/reconnect`, push messages).

**What we store:** only `refresh_token` + `folder_id` in `google_accounts` (`db.md` §3.2). The access token lives in process memory and is renewed by `x/oauth2` itself. Finished documents and audio are **not stored**.

## 2. Setup (Google Cloud, Testing mode)

Done in Google Cloud Console, **without brand verification** (that is for Production, §7):

1. console.cloud.google.com: create a project.
2. Enable the **Google Drive API**.
3. OAuth consent screen: **External**, app name, support email; scope `drive.file`; add demo users to **Test users** (limit 100).
4. Credentials -> OAuth client ID (**Web application**), Authorized redirect URI = `GOOGLE_REDIRECT_URL`. Copy **Client ID + Secret** to the env file.

- **One OAuth client for both environments.** Test/prod differ by publishing status (Testing vs Production), not by a credential pair, unlike YooKassa (where test/live are different `provider_token`s). There is no "test to prod" credential swap; "live mode" = Publish app + brand verification.
- **Local G2 run:** a `localhost`/loopback redirect is allowed in Testing (`api/google/API.md` §7), or a tunnel to the running `web.go`.

## 3. The `drive.file` scope: choice and reasons

**The only scope is `https://www.googleapis.com/auth/drive.file`** (non-sensitive, Recommended). It gives per-file access: create files and read/modify **only** what the app created. The app creates the folder and the Docs, so they are inside the access zone; the rest of the user's Drive is inaccessible.

**Why this one (the main fork):** restricted scopes (`drive`, `drive.readonly`, `drive.metadata`) give access to the whole Drive and require a **CASA security assessment** (a heavy review, weeks). `drive.file` **avoids** them: no security assessment, only the light brand verification remains (~2-3 days, §7). The classification contract is in `api/google/API.md` §14.

**Consequence for the architecture:** we create documents and do not read anyone else's; "we do not store finished documents ourselves" (`architecture.md` §15) fits the scope naturally.

## 4. Integration flow

### 4.1. Linking (onboarding of the Google mode)

```
/start -> 152-FZ consent -> tg.go gives AuthURL(state) -> user authorizes on Google
  -> Google sends code to /oauth/google/callback -> web.go validates state
  -> google.go: Exchange(code) -> refresh_token -> create the folder «Транскрибации созвонов»
  -> storage.UpsertGoogleAccount(refresh_token, folder_id) -> tg.go: «Google привязан»
```

The folder name «Транскрибации созвонов» ("Call transcripts") and the message «Google привязан» ("Google linked") are user-facing and stay in Russian.

- **`state` is required:** CSRF protection + binding the OAuth session to the Telegram `userID`. Storing `state` in the MVP is in-memory (a userID-to-state map with TTL); DB-backed (`oauth_states`) comes by production (`plan-google-slice.md` tech debt).
- **`access_type=offline` + `prompt=consent`** in AuthURL are mandatory, otherwise no refresh token arrives (`api/google/API.md` §2.1).
- **Link enforcement is on only in the Google mode.** Release 0.1 with Telegram delivery does not block accepting meeting links for lack of a Google link. In the future Google mode, accepting links is blocked (`IsGoogleLinked`) until the account is linked.

### 4.2. Result delivery

`google_delivery.go` implements `DocumentDelivery` (`Deliver(ctx, chatID, filename, caption string, content []byte) error`): inside `Deliver` it creates the Doc through `google.go` and **sends the link to the user itself** (it holds a `notify` closure over `*tele.Bot`, the way `telegram_delivery.go` sends the file itself). The interface is **not changed**; the rationale is in `plan-google-slice.md` (log, item 4).

The current `orchestrator.go` for Telegram delivery hands over the transcript and summary in the final grouped delivery. The target post-0.1 Google semantics are specified in `plan-google-slice.md`: the base variant is separate Doc files in the user's folder without changing the `DocumentDelivery` interface.

### 4.3. `/reconnect` and `awaiting_reauth` (closes the refresh gap)

During delivery `CreateDoc` may get `invalid_grant` (the user revoked access or the token expired in Testing after 7 days) -> `ErrReauth`. Then:
- `SetJobAwaitingReauth`: status `awaiting_reauth` (**not `failed`**), the current status (`processing`|`summarizing`) is remembered in `reauth_from`. **The quota reservation is NOT rolled back** (the transcript is already with the provider, the user has paid).
- `tg.go` sends a push «переподключите Google: /reconnect» ("reconnect Google: /reconnect").
- After the repeated OAuth callback, `web.go` calls `WakeAwaitingReauthJobs(userID)`: it restores `reauth_from` (`UPDATE jobs SET status = reauth_from, reauth_from = NULL ...`). The orchestrator picks up **exactly from the point** where it failed, without duplicating an already delivered document.

The full split is in `architecture.md` §14; SQL and indexes in `db.md` §3.6/§5. The wire level of `invalid_grant` and Go detection are in `api/google/API.md` §6/§16.5.

## 5. Refresh tokens: Testing mode is the main nuance (analog of "recurring" for payments)

**In Testing mode refresh tokens expire after 7 days.** [OK] Confirmed by the official docs (`api/google/API.md` §5). Condition: External + publishing status Testing + an offline token. Our `drive.file` is **not basic** (what matters for the expiry rule is the basic / non-basic dichotomy, not non-sensitive; by the classification in §3 `drive.file` is non-sensitive), so the rule **applies**.

**What this means for the demo:** the refresh token dies every 7 days, so access to Drive breaks and a recovery path is needed. It is covered by two mechanisms:
1. **`/reconnect`** (§4.3): the user reconnects, Jobs resume from the point of failure.
2. **Refresh-token monitor:** a post-0.1 cron tick once a day sends a push «через 2 дня доступ к Google истечёт, обнови через /reconnect» ("Google access expires in 2 days, renew via /reconnect"). Belongs to the class of F9 push messages.

**It goes away only by moving the app to In production** (Publish app + brand verification, §7): then tokens live indefinitely, and the monitor and frequent `/reconnect` disappear.

> **Open question (see §11):** the monitor ticks on "was refreshed long ago", but the `google_accounts` schema (`db.md` §3.2) **has no `last_refresh_at` column**: either a column or another criterion is needed. Resolve when implementing the monitor.

## 6. Converting markdown to a native Google Doc

The Doc is created by **uploading markdown with conversion**: `POST /upload/drive/v3/files?uploadType=multipart`, metadata with `mimeType:"application/vnd.google-apps.document"` + content with `Content-Type: text/markdown`. Google converts on the server. The Docs API and `batchUpdate` are **not needed**: we stay within `drive.file`. The full wire contract is in `api/google/API.md` §10.

**[!] The only real confidence risk:** the conversion of exactly `text/markdown` was announced by Google (07.2024) but was not in the verbal summary of the import-format table. **Decision:**
- the main path is `text/markdown`;
- the **`text/html` fallback** (a stable import format) is ready behind a flag: we convert markdown to HTML on our side;
- on G2 check both with a live `GET /drive/v3/about?fields=importFormats`.

Images in markdown convert poorly; our transcripts/summaries have none, so it is irrelevant.

## 7. Testing to Production path (brand verification)

**This is a production track, NOT a demo blocker.** The demo works in Testing mode (§2). Moving to Production removes the 7-day token expiry (§5) and the "app not verified" screen. The detailed infra steps (nginx server block + Certbot, homepage, privacy on `transcrib.example.com`, Search Console, submit) are in `plan-google-slice.md` (section "Path to Production").

**Key finding:** our only scope `drive.file` is non-sensitive, so **only brand verification (~2-3 business days)** is needed, NOT the heavy sensitive/restricted scope verification with CASA. The "4-6 weeks" estimate in earlier canons applied to restricted scopes, which we do not have.

**Brand verification trigger:** External user type **+** showing a logo/app name on the consent screen. What Google checks (`api/google/API.md` §15.3):

| Element | Requirement |
|---|---|
| App name + logo | On the consent screen |
| Support email | Present (gmail is acceptable) |
| Homepage | Publicly accessible, relevant, not a Telegram/store link |
| Privacy policy | Visible to the user, **on the same domain as the homepage**, describes handling of Google data |
| Domain ownership | All top-level domains from the URIs are verified in Google Search Console |

**The really long step is not the Google review but our infrastructure:** a public homepage + privacy policy on the domain `transcrib.example.com` + nginx server block/Certbot TLS + domain verification in Search Console. It coincides with the infra part of the production finalization (Stage 3). The public version must be available at `https://transcrib.example.com/privacy/`.

## 8. Library: `golang.org/x/oauth2`

**OAuth goes through `golang.org/x/oauth2` + `golang.org/x/oauth2/google`.** It provides `oauth2.Config` (`AuthCodeURL`/`Exchange`) and `Client(ctx, token)`, which **renews** the access token from the refresh token **itself** and **signals** `invalid_grant` (`*oauth2.RetrieveError`) **itself**, which is exactly the `awaiting_reauth` trigger. We do not write a manual refresh. Names and sketches: `api/google/API.md` §16.

**Drive is raw `net/http`, without `google.golang.org/api/drive/v3`.** Two endpoints (folder + Doc) against a heavy dependency tree (`grpc`/`protobuf`/`gax`) is not worth it. The HTTP client comes from `oauth2.Config.Client` (auto-token). This matches the project stack (`net/http`) and is symmetric with `billing.go` (YooKassa is also raw net/http).

**Go pitfalls (`api/google/API.md` §16):**
- `&oauth2.Token{RefreshToken: rt}` forces a refresh because of the **empty AccessToken**, not because of a zero Expiry: do not set a fake AccessToken.
- `invalid_grant` arrives **lazily**: on the first Drive request, not from `Client()`. Hang the mapping to `ErrReauth` on the Drive call site.
- `mime/multipart` writes `form-data`; Drive needs `multipart/related` (a manual header + `CreatePart` without `Content-Disposition`).

## 9. Compliance

| Topic | Requirement |
|---|---|
| **Google API Services User Data Policy** | Baseline obligations for everyone: exact scope request, minimum permissions, a published privacy policy describing handling of Google data, use of data strictly per the policy. The strict **Limited Use** (security assessment, ban on transfer) is part of the **restricted** process; it does **not** apply in full to non-sensitive `drive.file`. |
| **Privacy policy** | For brand verification: on the same domain as the homepage; discloses access/use/store/share of Google user data; linked from the consent screen. Before Production, check that it is published at `https://transcrib.example.com/privacy/`. |
| **152-FZ** (an angle, not legal advice) | Personal data in the Google context: the refresh token (an indirect identifier), email (if requested; not required in the `drive.file` scenario), the content of the docs (transcripts are participants' personal data). Minimization: exactly `drive.file`, encrypt the refresh token in the DB (AES-GCM, production finalization: `db.md` §3.2 TODO), do not persist document content on our side. Cross-border transfer (Drive) and the subprocessor list go into the privacy policy + a notification to Roskomnadzor (a question for the lawyer/customer, not code). |

## 10. Top traps (full list: `api/google/API.md` §6/§10/§16)

1. **refresh_token arrives ONLY on the first consent:** a repeated OAuth without `prompt=consent` returns an empty refresh token. Always send `access_type=offline` + `prompt=consent`; if the refresh token is empty, do not overwrite the stored one with it.
2. **7-day expiry in Testing mode:** build in `/reconnect` + a monitor; it goes away only with Production.
3. **`invalid_grant` is not `failed`:** it is `awaiting_reauth` without rolling back the reservation; it arrives lazily on the first Drive request.
4. **redirect_uri exact match:** scheme/host/port/path/slash; a single constant in code and in the Console.
5. **multipart upload without `?fields=` returns only `id`:** no `webViewLink`. Add `?fields=id,webViewLink`.
6. **`multipart/related`, not `form-data`:** a manual request header (Go `mime/multipart`).
7. **A folder deleted by the user gives 404 on `parents`:** this is NOT `invalid_grant`. In the MVP `failed` is acceptable; later, recreate the folder.
8. **Markdown conversion:** target `mimeType: application/vnd.google-apps.document` + source `Content-Type: text/markdown`; `text/html` fallback.

## 11. Open questions

- **(monitor)** `google_accounts` has no `last_refresh_at`: add a column or pick another tick criterion. Decide when implementing the monitor (F9 class).
- **(markdown)** Live confirmation of `text/markdown` to Doc on a real account (`about.importFormats`); fallback `text/html` (§6).
- **(delivery default)** In release 0.1 the default stays `telegram`. Changing the default to `google` returns only inside a separate post-0.1 Google slice after the live Google test.
- **(`state` storage)** in-memory in the MVP versus a small `oauth_states` table (surviving a restart in the middle of onboarding): `plan-google-slice.md`.

## 12. Sources

The contract and Google primary sources (official `developers.google.com`/`support.google.com`, marks [OK]/[!]): [`docs/api/google/API.md`](api/google/API.md) §20. Here only integration decisions; facts are not duplicated.
