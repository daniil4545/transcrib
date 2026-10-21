# Google OAuth 2.0 + Drive API: unified contract reference

Canonical reference for the Google REST/HTTP contract for `transcrib`. It covers exactly what our integration needs: linking the user's Google account via the **OAuth 2.0 Web Server flow** (offline access, refresh token), **creating a folder** in the user's Drive and **uploading markdown as a native Google Doc** through Drive API v3 (scope `drive.file`), handling `invalid_grant` for `/reconnect`, scope classification, and the verification process.

**Source:** multi-agent research of the official docs `developers.google.com` + `support.google.com` (2026-06-14, 5 facets: OAuth web flow, Drive v3, scopes/verification, Go `x/oauth2`, compliance/traps). Every section links to its primary source (§20). Business/integration decisions, flow, compliance and traps are in `docs/google.md` (canon).

> **STATUS (2026-06-14):** the contract is assembled from the official documentation. Key facts were confirmed by an independent check against the official docs ([OK]). **The only real [!] is the conversion of exactly `text/markdown` to a Google Doc** (§10): Google announced the feature in 07.2024 and it is available through the Drive API, but the verbal summary of the import-format table did not list markdown, so run a live check of `about.importFormats` during the first implementation of `google.go` and keep the `text/html` fallback. Marks: [OK] confirmed by the official docs; [!] needs a live check / officially unclear.

---

## 0. Base URLs, environments, authentication model

| Purpose | URL | Confidence |
|---|---|---|
| OAuth: authorization | `https://accounts.google.com/o/oauth2/v2/auth` | [OK] |
| OAuth: token (code exchange + refresh) | `https://oauth2.googleapis.com/token` | [OK] |
| OAuth: revocation | `https://oauth2.googleapis.com/revoke` | [OK] |
| Drive: metadata (create a folder) | `https://www.googleapis.com/drive/v3/files` | [OK] |
| Drive: upload (create a file with content) | `https://www.googleapis.com/upload/drive/v3/files?uploadType=multipart` | [OK] |
| Drive: about (live check of import formats) | `https://www.googleapis.com/drive/v3/about?fields=importFormats` | [OK] |

- **Authentication model: OAuth 2.0 Bearer.** All Drive calls carry the header `Authorization: Bearer <access_token>`; the access token is short-lived (~1h) and is renewed from the refresh token through the token endpoint.
- **No API keys / service account.** We act **on behalf of the user** (their Drive, their files), so only user OAuth with a refresh token. A service account would create files in its own Drive, not in the user's Drive, which is not our case.
- **One OAuth client (Web application)** with a `client_id` + `client_secret` pair from Google Cloud Console. Test and live environments use **the same client**; the only difference is the publishing status (Testing vs Production, §15). This differs from YooKassa, where test/live are different credentials.

### 0.1. What we do NOT use (contract boundary)

- **Google Docs API** (`docs.googleapis.com`, `batchUpdate`) is not needed: we create the Doc by uploading markdown with conversion through Drive (§10), not by building the document operation by operation. The Docs API would need a separate `documents` scope.
- **Service account / domain-wide delegation** is not our scenario (see above).
- **Resumable / simple media upload:** our docs are small (tens to hundreds of KB), `uploadType=multipart` is enough (§10, 5 MB limit).
- **Restricted scopes** (`drive`, `drive.readonly`, `drive.metadata`) are deliberately avoided (§14), otherwise a heavy security assessment (CASA) is required.

---

## 1. transcrib integration map

```
┌──────────┐  AuthURL/consent   ┌────────────┐  code to callback    ┌───────────┐
│ user in  │ <----------------> │  Google    │ ───────────────────> │  web.go   │
│ Telegram │  taps the link     │  accounts  │  /oauth/google/...   │ (callback)│
└──────────┘                    └────────────┘                      └────┬──────┘
                                                                          │ code -> token
                                          ┌───────────────────────────────┤ (google.go)
                                          v                                v
                                  ┌──────────────┐  Bearer access  ┌──────────────┐
                                  │ oauth2.Config│ <── refresh ───>│token endpoint│
                                  │  .Client()   │  (auto-renewal) └──────────────┘
                                  └──────┬───────┘
                                         │ POST /drive/v3/files (folder)
                                         │ POST /upload/drive/v3/files (Doc)
                                         v
                                  ┌──────────────┐
                                  │ User's Drive │  webViewLink -> link to the user in Telegram
                                  └──────────────┘
```

- **OAuth flow (`google.go` + `web.go`):** we build the `AuthURL`, the user authorizes on the Google side, Google sends `code` to our public callback, we exchange `code` for tokens and store **only the refresh token** + `folder_id`.
- **Drive calls (`google.go`):** the HTTP client from `oauth2.Config.Client(ctx, token)` injects and renews the access token itself. Two operations: create a folder, upload a Doc.
- **There is no analog of `provider_payment_charge_id`:** Google moves no money, all interaction is about access to the user's Drive.

---

## 2. OAuth 2.0: authorization request (Web Server flow)

`GET https://accounts.google.com/o/oauth2/v2/auth?<params>`

| Parameter | Req. | What it does |
|---|---|---|
| `client_id` | **yes** | OAuth client ID from Cloud Console |
| `redirect_uri` | **yes** | **Exact** match with the registered Authorized redirect URI |
| `response_type` | **yes** | `code` for the web server flow |
| `scope` | **yes** | Space-delimited; we have one: `https://www.googleapis.com/auth/drive.file` |
| `access_type` | **critical** | `offline` is **required for a refresh token to be returned** (default `online`) |
| `prompt` | **critical** | `consent` forces the consent screen so the refresh token arrives **on every** authorization, not only the first |
| `state` | **yes (for us)** | Arbitrary string: CSRF protection + binding the OAuth session to the Telegram `userID`. Returned in the callback |
| `include_granted_scopes` | no | Incremental authorization; we have one scope, so it is **not needed** |
| `login_hint` | no | Email/ID hint |

### 2.1. [OK] The critical combination for a guaranteed refresh token

**`access_type=offline` + `prompt=consent`**: both are required.

- `access_type=offline` instructs the server to return a refresh token. But by itself the refresh token is issued **only on the user's first consent**.
- `prompt=consent` forces the consent screen again, so the refresh token arrives **every time**.
- Without `prompt=consent`, re-authorizing a user who already consented returns a `code` whose exchange **will not give** a `refresh_token` (only `access_token`). This is trap #1 (`docs/google.md`).

Quote from the official docs: *"This value [offline] instructs the Google authorization server to return a refresh token … the first time …"* / *"The refresh_token is only returned on the first authorization."*

---

## 3. Code exchange (token endpoint)

`POST https://oauth2.googleapis.com/token`, `Content-Type: application/x-www-form-urlencoded`

**Request fields (all required):** [OK]
- `code`: the authorization code from the callback
- `client_id`, `client_secret`
- `redirect_uri`: same as the original
- `grant_type=authorization_code`

**Response fields (JSON):** [OK]
| Field | Description |
|---|---|
| `access_token` | Bearer token for Drive calls |
| `expires_in` | Access token TTL in seconds (`3920` in the docs example; in practice ~1h). [!] "1 hour" is not stated in words, so **read it from `expires_in`, do not hardcode** |
| `refresh_token` | **Only** with `access_type=offline` (+ `prompt=consent` for repeats). **This is all we store** |
| `scope` | Granted scopes (space-delimited, case-sensitive) |
| `token_type` | Always `Bearer` |
| `id_token` | JWT, **only** if OpenID scopes are requested (`openid`/`email`/`profile`). With `drive.file` alone it **will not arrive** [!] |

- **The authorization code is single-use.** Exchanging the same `code` again gives `invalid_grant`, and Google may revoke the tokens already issued for it (a trap). On failure, send the user through a new OAuth flow; do not retry the exchange.

---

## 4. Refresh token and access token renewal

**Renewing the access token:** [OK]
`POST https://oauth2.googleapis.com/token`, `application/x-www-form-urlencoded`:
- `client_id`, `client_secret`
- `grant_type=refresh_token`
- `refresh_token`

Response: a new `access_token`, `expires_in`, `scope`, `token_type`. **No new `refresh_token` arrives**; the old one is reused.

- **On the Go side `x/oauth2` does this itself** (§16): `oauth2.Config.Client(ctx, token)` returns an `*http.Client` that caches the access token in memory and refreshes it on expiry. We do not write a manual refresh.
- **Storage:** only the `refresh_token` is in the DB (`google_accounts.refresh_token`, `db.md` §3.2). The access token is persisted nowhere; it lives in process memory.

---

## 5. Testing mode: 7-day expiry of refresh tokens (critical node)

[OK] **THE RULE APPLIES in 2026**, confirmed by two official sources.

Conditions for a refresh token to expire after **7 days**:
- the OAuth consent screen user type is **External**;
- the publishing status is **Testing**;
- the client requested `access_type=offline` and received a refresh token.

Quote: *"Authorizations by a test user will expire seven days from the time of consent. If your OAuth client requests an offline access type and receives a refresh token, that token will also expire."*

**The only exception (does NOT save us):** the rule does not apply if the app requests **only** basic scopes (`userinfo.email`, `userinfo.profile`, `openid` / Sign in with Google). Our `drive.file` is **sensitive, not basic**, so the 7-day expiry **applies**.

**Consequence for the MVP demo:** in Testing mode the refresh token dies every 7 days, so `/reconnect` (§6) is mandatory in scope. It goes away only by moving the app **In production** (Publish app, see `docs/google.md` about brand verification).

### 5.1. Expiry/revocation of refresh tokens outside Testing (Production) [OK]

A refresh token is invalidated if:
- the user **revoked access** for the app;
- the refresh token was **not used for 6 months**;
- the user changed the password **and the token contains Gmail scopes** (not our case: `drive.file`, not Gmail);
- the limit of **100 live refresh tokens per (Google account x OAuth client)** is exceeded: the oldest is silently invalidated;
- a Workspace admin restricted the scope (`admin_policy_enforced`).

---

## 6. `invalid_grant`: the `/reconnect` trigger

[OK] The main signal that the refresh token is dead and a repeated consent is needed. It arrives on renewal (§4) when:
- the user **revoked access** for the app;
- **7-day expiry in Testing mode** (our main demo case);
- 6 months of inactivity; the limit of 100 exceeded; password change with Gmail scopes (not our case); admin policy.

[!] Additionally (secondary sources): `invalid_grant` also happens on **clock skew** (server clock desync), on **re-exchanging an already used `code`**, and on undocumented security heuristics. On `invalid_grant` a repeat of the same request always fails; only a new consent flow fixes it.

**Handling:** on `invalid_grant` during refresh, the domain `ErrReauth`, the Job goes to `awaiting_reauth` (not `failed`), and the user gets a push "reconnect via /reconnect". Architectural split: `architecture.md` §7.0, §9; SQL: `db.md` §3.6/§5. Go detection (`errors.As` on `*oauth2.RetrieveError`, `ErrorCode == "invalid_grant"`): §16.

> **Important (§16):** the error surfaces **lazily**: not from `Config.Client(...)` but from the **first real Drive request** (or an explicit `TokenSource.Token()`). Hang the mapping to `ErrReauth` on the Drive call site, not on the client constructor.

---

## 7. redirect_uri, localhost, OOB, PKCE


- **Exact match is required** [OK]: `redirect_uri` must match the registered one exactly (scheme, host, port, path, no trailing-slash difference). A mismatch gives `redirect_uri_mismatch`.
- **localhost / loopback is allowed** [OK]: *"Localhost URIs (including localhost IP address URIs) are exempt from [the HTTPS] rule."* For a local G2 run, `http://localhost:PORT/...` / `http://127.0.0.1:PORT/...` is acceptable. The production callback is **HTTPS** (`https://transcrib.example.com/oauth/google/callback`).
- **OOB (`urn:ietf:wg:oauth:2.0:oob`) is fully blocked since 31.01.2023** [OK]. Irrelevant (we have a full web callback), but it cannot be used.
- **PKCE** [!]: for a confidential web app with a `client_secret` it is **not required** (Google requires `client_secret` on exchange, PKCE does not replace it). Recommended as hardening; the MVP can skip it. The OAuth 2.1 trend is PKCE for everyone, but Google does not require it for web + secret.

---

## 8. Drive API v3: two operations, two URL bases [OK]

| Operation | Endpoint | Body |
|---|---|---|
| Create a folder (metadata only) | `POST https://www.googleapis.com/drive/v3/files` | JSON File resource |
| Create a Doc from markdown (upload + convert) | `POST https://www.googleapis.com/upload/drive/v3/files?uploadType=multipart` | multipart/related: metadata + content |

`uploadType`: `media` (content only), `multipart` (metadata + content, our case), `resumable` (large files). For the `/upload` URL the parameter is required.

---

## 9. Creating a folder [OK]

The folder name is Russian on purpose: it is created in the user's Drive and is user-facing ("Транскрибации созвонов" means "Call transcripts").

```http
POST https://www.googleapis.com/drive/v3/files?fields=id,name,webViewLink
Authorization: Bearer <access_token>
Content-Type: application/json; charset=UTF-8

{ "name": "Транскрибации созвонов",
  "mimeType": "application/vnd.google-apps.folder" }
```

- `parents` is **not required**: without it the folder goes to the root of the user's My Drive. For a one-level app folder we omit `parents`.
- The response is a File resource. Without `?fields=` a trimmed set arrives; to be sure of the needed fields we request `?fields=id,name,webViewLink`.
- **We store the folder `id`** in `google_accounts.folder_id`. All Docs are created with `parents:[folder_id]`.

```json
{ "id": "1AbC...", "name": "Транскрибации созвонов",
  "webViewLink": "https://drive.google.com/drive/folders/1AbC..." }
```

---

## 10. Multipart upload + conversion of markdown to a native Google Doc (THE MAIN POINT)

The Doc name is Russian on purpose (user-facing: "Транскрипт — Имя встречи" means "Transcript - Meeting name").

```http
POST https://www.googleapis.com/upload/drive/v3/files?uploadType=multipart&fields=id,webViewLink
Authorization: Bearer <access_token>
Content-Type: multipart/related; boundary=foo_bar_baz

--foo_bar_baz
Content-Type: application/json; charset=UTF-8

{ "name": "Транскрипт — Имя встречи",
  "parents": ["<folder_id>"],
  "mimeType": "application/vnd.google-apps.document" }
--foo_bar_baz
Content-Type: text/markdown

# Heading
**bold** text, lists, etc.
--foo_bar_baz--
```

### 10.1. Strict format rules (RFC 2387 / Google) [OK]

- The request header is `Content-Type: multipart/related; boundary=<string>` (**not** `form-data`!).
- **Part 1 is the metadata, FIRST**, `Content-Type: application/json; charset=UTF-8`.
- **Part 2 is the content, SECOND**, `Content-Type: text/markdown` (the source format of the content).
- Every boundary in the body is preceded by `--`; the final one is closed with `--` on both sides (`--foo_bar_baz--`).

### 10.2. Conversion mechanism [OK]

The target format is set by the `mimeType` field in the **metadata** (`application/vnd.google-apps.document`), while the `Content-Type` of the second part is the **source** format (`text/markdown`). Drive converts the source into a native Google Doc on the server.

### 10.3. [!] Support status of `text/markdown`: THE ONLY real confidence blocker

- Markdown import/export in Google Docs was officially added by Google on **31.07.2024** and is available through the Drive API (official Google Workspace Updates blog) [OK].
- **BUT:** at the time of the check, the verbal summary of the official import-format table listed as convertible to Docs: `.docx`, OpenDocument Text, **`text/html`**, RTF, **`text/plain`**, while `text/markdown` **did not appear** in that digest. The mismatch between the announcement and the table summary gives the [!] mark.
- **Canonical live check (mandatory on G2):** `GET /drive/v3/about?fields=importFormats` to make sure the map contains `"text/markdown": ["application/vnd.google-apps.document"]`. The import-format map is account-specific, so this is the only 100% reliable runtime check.

### 10.4. Fallback `text/html` [OK]

If `text/markdown` does not work in prod: convert markdown to HTML on our side and upload part 2 with `Content-Type: text/html`. `text/html` is a stably supported import format (present in all versions of the table) and keeps headings/lists/bold. The last resort is `text/plain` (no formatting). A markdown-to-Doc catch: images (base64) convert poorly; our transcripts/summaries have no images, so it is irrelevant.

**Decision for the doc:** `text/markdown` is the main path; `text/html` is a ready fallback behind a flag; check both through `about.importFormats` during integration.

---

## 11. `webViewLink` needs `?fields=` [OK]

- Multipart upload and metadata create **return only `id` by default** (`webViewLink` is output-only and not in the default set).
- To get the link to the Doc, **`?fields=id,webViewLink` is required** (for a folder, `?fields=id,name,webViewLink`).
- `webViewLink` is the link to open the file in a browser, and this is what we give the user in Telegram.

---

## 12. Drive errors [OK]

The error body is a single JSON structure:

```json
{ "error": { "code": 403, "message": "<text>",
    "errors": [ { "domain": "usageLimits", "reason": "userRateLimitExceeded", "message": "..." } ] } }
```

| HTTP | `reason` | Meaning / reaction |
|---|---|---|
| **401** | `authError` | Token expired/invalid/wrong scope: refresh the access token; if that fails (`invalid_grant`), `ErrReauth` -> `awaiting_reauth`, **not `failed`** (§6) |
| 403 | `userRateLimitExceeded` | Per-user limit: exponential backoff |
| 403 | `rateLimitExceeded` / `dailyLimitExceeded` | Project/daily limit: backoff |
| 403 | `storageQuotaExceeded` | The user ran out of Drive space: **do not retry**, tell the user |
| 403 | `insufficientFilePermissions` / `appNotAuthorizedToFile` | No permission on the file/ACL |
| **404** | `notFound` | The parent folder was deleted by the user: **this is NOT `invalid_grant`** (the token is valid). Recreate the folder and update `folder_id`; for the MVP it is acceptable to end in `failed` (`docs/google.md` tech debt) |

Retry **only** rate limits (429 / 403-rate) with backoff. `storageQuotaExceeded`, `404`, `401` get no blind retry.

---

## 13. Rate limits / quotas [OK]

- **Per project:** 1,000,000 quota units/min.
- **Per user per project:** 325,000 quota units/min.
- **Per day per project:** 400,000,000 quota units.
- Cost: read ~5, list ~100, edit ~50 units.
- **Backoff:** truncated exponential, `min((2^n)+random_ms, maximum_backoff)`, `maximum_backoff` ~32-64s.
- Our profile (a handful of requests per meeting) is orders of magnitude below the limits; the only real risk is bursts, covered by backoff on 403/429.

### 13.1. Upload size limits [OK]

- **multipart upload is limited to 5 MB** of content. Above that, `uploadType=resumable`.
- Our docs (transcript/summary) are tens to hundreds of KB, so **multipart is enough**; resumable is not needed.

---

## 14. Scopes: classification of `drive.file` [OK]

| Parameter | Value |
|---|---|
| Scope | `https://www.googleapis.com/auth/drive.file` |
| Class | **Non-sensitive** (and **Recommended**) |
| What it gives | Per-file access: create new files and read/modify **only** files and folders created/opened by the app. No access to the rest of the user's Drive |
| App (scope) verification | **NOT required** for the scope itself |
| Security assessment (CASA) | **NOT required** |

**Confirmed: `drive.file` is NOT restricted.** Restricted scopes (need CASA, access to the whole Drive): `drive`, `drive.readonly`, `drive.metadata`. We **deliberately do not use** them; this is the key fork, avoided by the choice of scope.

Architectural consequence: the app creates both the folder and the Docs, so they are inside the `drive.file` access zone; the user's other files are not. This matches the reminder "we do not store finished documents ourselves".

---

## 15. Consent screen, publishing statuses, brand verification [OK]

### 15.1. OAuth consent screen (Google Auth Platform -> Branding/Audience)

User type **External**; app name (shown on consent **after** verification); user support email; developer contact; authorized domains (all domains from homepage/privacy/redirect URI); scopes (`drive.file`); test users.

### 15.2. Testing vs In production

| | Testing | In production |
|---|---|---|
| Who can authorize | only **test users** (limit **100**) | any Google account |
| Screen | "unverified app" (tester: **Advanced -> Go to ... (unsafe)**) | clean, no warning |
| Refresh token | **expires in 7 days** (§5) | lives indefinitely |

### 15.3. App verification vs Brand verification

| Type | When required | Our case |
|---|---|---|
| **Brand verification** | External **AND** showing a logo/display name on consent | **needed** (only this one) |
| **App (scope) verification** | sensitive/restricted scopes (restricted + CASA) | **not needed** (no sensitive/restricted scopes) |

**Brand verification time: typically 2-3 business days** [OK] (not 4-6 weeks; that estimate is for sensitive/restricted scope verification).

**Brand verification requirements:**
- **Homepage:** publicly accessible (not behind a login), relevant to the app; not a Google Play / Facebook link.
- **Privacy policy:** **on the SAME domain** as the homepage; discloses handling of Google user data; linked from the consent screen.
- **Domain ownership:** all top-level domains from the URIs are verified in **Google Search Console** (the Cloud project account is owner/editor in Search Console).
- When the logo/name/homepage/privacy/domains change, **resubmit** and get re-approved.

### 15.4. The "100 new users" cap applies only to sensitive/restricted [OK]

The production cap "100 new users total" applies to apps with **sensitive/restricted** scopes in the unverified state. For **non-sensitive** scopes (`drive.file`) the cap **does NOT apply**. The limit of **100 test users** is separate: it belongs to the **Testing publishing status** (any scopes), do not confuse them.

---

## 16. Go contract (`golang.org/x/oauth2` + raw `net/http`) [OK]

### 16.1. Config + Endpoint

```go
import ("golang.org/x/oauth2"; "golang.org/x/oauth2/google")

cfg := &oauth2.Config{
    ClientID:     cid,
    ClientSecret: secret,
    Endpoint:     google.Endpoint, // ready-made variable, do not hardcode the URL
    RedirectURL:  "https://transcrib.example.com/oauth/google/callback",
    Scopes:       []string{"https://www.googleapis.com/auth/drive.file"},
}
```

`drive.DriveFileScope` from `google.golang.org/api/drive/v3` == the string `https://www.googleapis.com/auth/drive.file`: **hardcode the string**, do not pull in a package for one string.

### 16.2. AuthURL: offline + consent

```go
url := cfg.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce)
```

- `oauth2.AccessTypeOffline` == `access_type=offline`.
- `oauth2.ApprovalForce` == `prompt=consent` (in 2026 it is literally an alias of `SetAuthURLParam("prompt","consent")`; earlier it sent the deprecated `approval_prompt=force`). Idiomatic: `ApprovalForce`.

### 16.3. Exchange: store only the refresh token

```go
tok, err := cfg.Exchange(ctx, code) // *oauth2.Token{AccessToken, RefreshToken, Expiry, ...}
// store only tok.RefreshToken
```

### 16.4. Self-refreshing client from the stored refresh token

```go
tok := &oauth2.Token{RefreshToken: rt} // AccessToken is empty, Expiry is zero
client := cfg.Client(ctx, tok)         // the *http.Client refreshes access from refresh itself
```

[!] **Correction to intuition:** `&oauth2.Token{RefreshToken: rt}` forces a refresh **because of the empty `AccessToken`**, NOT because of the zero `Expiry` (a zero Expiry in `x/oauth2` is treated as "not expired"). Do not set a non-empty fake AccessToken with a zero Expiry, or the refresh will not fire.

`Config.TokenSource`/`Config.Client` already wrap the refresher in a `ReuseTokenSource` (in-memory access cache). Do NOT use `StaticTokenSource` (it does not refresh). There is no need to call `ReuseTokenSource` by hand.

### 16.5. Detecting reauth

```go
var ErrReauth = errors.New("google: refresh token invalid, reconnect required")

func classifyReauth(err error) error {
    var re *oauth2.RetrieveError
    if errors.As(err, &re) && re.ErrorCode == "invalid_grant" {
        return ErrReauth
    }
    return err
}
```

- `*oauth2.RetrieveError.ErrorCode` is present (golang/go#58125). Use `errors.As`, not a bare type assertion (the error may be wrapped).
- The error arrives **lazily**: on the first Drive request / explicit `Token()`. Wrap `classifyReauth` around the Drive call site.

### 16.6. multipart/related with raw net/http

`mime/multipart.Writer` writes the body bytes identically for `related` and `form-data`; the difference is the request header and the absence of `Content-Disposition` on the parts. We use `CreatePart` with an explicit `MIMEHeader` (only `Content-Type`) and set the request type to `multipart/related` by hand:

```go
func buildRelated(metadataJSON, content []byte, contentMime string) (*bytes.Buffer, string, error) {
    body := &bytes.Buffer{}
    w := multipart.NewWriter(body)
    mh := textproto.MIMEHeader{}; mh.Set("Content-Type", "application/json; charset=UTF-8")
    p1, _ := w.CreatePart(mh); p1.Write(metadataJSON)       // part 1: metadata (name, parents, target mimeType)
    mh2 := textproto.MIMEHeader{}; mh2.Set("Content-Type", contentMime) // text/markdown | text/html
    p2, _ := w.CreatePart(mh2); p2.Write(content)           // part 2: media
    w.Close()
    return body, "multipart/related; boundary=" + w.Boundary(), nil // request type is related, NOT form-data
}
```

net/http sets `Content-Length` for a `*bytes.Buffer` itself. `cfg.Client(...)` adds `Authorization`.

### 16.7. SDK `drive/v3` vs raw net/http

`google.golang.org/api/drive/v3` pulls in a heavy tree (`gax-go`, `grpc`, `protobuf`, `option`: dozens of packages) for two calls. For this project (principle "minimum dependencies, raw net/http") use **raw net/http**: the `oauth2` client + 2 manual POSTs. We must take `oauth2`/`oauth2/google` (light, pure stdlib + net/http), but not `drive/v3`. Symmetric with `billing.go` (YooKassa is also raw net/http).

---

## 17. Test mode / how to verify

- **There is no sandbox:** test with a real OAuth client in **Testing mode**: add demo users to Test users (limit 100) and click through "Advanced -> Go to ... (unsafe)".
- **Local G2 run:** a `localhost`/loopback redirect is allowed in Testing (§7); or a tunnel to the running callback.
- **Live checks on integration (G2):** (1) `about.importFormats` for `text/markdown` support (§10.3); (2) the `webViewLink` format in the response with `?fields=id,webViewLink` (§11); (3) the real `invalid_grant` path: revoke access in the Google account and make sure `*oauth2.RetrieveError{ErrorCode:"invalid_grant"}` arrives on a Drive request (§16.5); (4) that `id_token` does not arrive with `drive.file` alone (§3).
- Unit: the OAuth flow is mocked at the `*http.Client` / replaced transport level; Drive calls run against recorded responses.

---

## 18. What is NOT covered / open questions (follow-up)

To be closed by a live G2 run on a real Google Cloud client and a test user.

- **[!] Conversion of `text/markdown` to Doc** (§10.3) is the main live check. `about.importFormats`; the `text/html` fallback is ready.
- **Exact `expires_in`** of the access token (formally not "1 hour"): read it from the response, do not hardcode.
- **`invalid_grant` on clock skew / reused code:** observe in the test flow.
- **PKCE being optional** for web + secret: confirm with a trial exchange without `code_verifier`.
- **Absence of `id_token`** with `drive.file` alone: check in a real response.
- **The refresh-token monitor** needs to know "when it was last refreshed": the current `google_accounts` schema (`db.md` §3.2) has **no `last_refresh_at` column** that `architecture.md` §9 refers to. Record the mismatch in `docs/google.md` (open questions): either a column or another tick criterion is needed.

---

## 19. `google.go` sketch (OAuth + Drive with raw net/http)

```
AuthURL(state) string                          -> cfg.AuthCodeURL(state, offline, consent)
Exchange(ctx, userID, code) error              -> token endpoint -> create folder -> upsert google_accounts
CreateDoc(ctx, userID, title, markdown) (url, error)
                                                -> POST /upload/drive/v3/files?uploadType=multipart&fields=id,webViewLink
                                                -> ErrReauth on invalid_grant
```

- The HTTP client is `cfg.Client(ctx, &oauth2.Token{RefreshToken: rt})` (auto-refresh).
- Folder metadata: `mimeType:"application/vnd.google-apps.folder"`; Doc: `mimeType:"application/vnd.google-apps.document"` + `parents:[folder_id]`.
- Integration decisions, flow, compliance, traps: `docs/google.md`. Track status: issue #13 (P2, post-0.1).

---

## 20. Sources

Priority goes to the official `developers.google.com` / `support.google.com`; secondary sources are marked.

**OAuth 2.0:**
- Web Server flow (endpoints, parameters, offline+consent, refresh, localhost/HTTPS): https://developers.google.com/identity/protocols/oauth2/web-server
- Token expiration (expiry reasons, 6 months, limit of 100): https://developers.google.com/identity/protocols/oauth2#expiration
- OOB migration (deprecation): https://developers.google.com/identity/protocols/oauth2/resources/oob-migration

**Scopes / verification / publishing:**
- Requesting Minimum Scopes (classification): https://support.google.com/cloud/answer/13807380
- Choose Google Drive API scopes: https://developers.google.com/workspace/drive/api/guides/api-specific-auth
- Manage App Audience (Testing/test users, 7-day token, 100 cap): https://support.google.com/cloud/answer/15549945
- Submit for brand verification (timing, requirements): https://developers.google.com/identity/protocols/oauth2/production-readiness/brand-verification
- Configure OAuth consent screen: https://developers.google.com/workspace/guides/configure-oauth-consent
- Manage OAuth App Branding: https://support.google.com/cloud/answer/15549049

**Drive API v3:**
- files.create reference: https://developers.google.com/drive/api/reference/rest/v3/files/create
- Manage uploads (multipart, import formats, 5 MB limit): https://developers.google.com/workspace/drive/api/guides/manage-uploads
- Create folders: https://developers.google.com/workspace/drive/api/guides/folder
- Partial response (fields, webViewLink): https://developers.google.com/workspace/drive/api/guides/performance
- Resolve errors: https://developers.google.com/workspace/drive/api/guides/handle-errors
- Usage limits: https://developers.google.com/workspace/drive/api/guides/limits
- Markdown import/export (official Google blog): https://workspaceupdates.googleblog.com/2024/07/import-and-export-markdown-in-google-docs.html

**Go:**
- golang.org/x/oauth2: https://pkg.go.dev/golang.org/x/oauth2
- golang.org/x/oauth2/google: https://pkg.go.dev/golang.org/x/oauth2/google
- RetrieveError.ErrorCode: https://github.com/golang/go/issues/58125

**Compliance:**
- API Services User Data Policy / Limited Use: https://developers.google.com/terms/api-services-user-data-policy

**Secondary ([!], re-check):**
- invalid_grant edge cases: https://nango.dev/blog/google-oauth-invalid-grant-token-has-been-expired-or-revoked/
- multipart upload to Drive in Go: https://freshman.tech/snippets/go/multipart-upload-google-drive/
- 152-FZ (legal blogs, Russian-language): https://1ps.ru/blog/dirs/2025/, https://www.klerk.ru/blogs/roskom24/674017/
