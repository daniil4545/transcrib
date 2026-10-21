# OpenRouter API: contract digest for transcrib

Working reference for implementing `internal/app/openrouter.go` (an implementation of the `Summarizer` interface, `summary.go`). Only what our scenario needs: one synchronous chat-completion call that turns a ready markdown transcript into a markdown digest (F3). Business/legal/economics: `docs/api/openrouter/README.md`.

**Source of truth** is the official documentation [openrouter.ai/docs](https://openrouter.ai/docs/api/reference/overview). Contract-critical and numeric facts went through adversarial verification, and some were confirmed by a live `curl` to the API (agent swarm 2026-06-22). Markers: "(LIVE)" means confirmed by an API call; "(DOCS)" means from the documentation; "(Warning: unverified)" means the docs have a template variable / it is not rendered.

OpenRouter is **OpenAI-compatible**: the request/response schemas follow the OpenAI Chat API with small differences (marked below).

## 0. Base and authentication

- **Base URL:** `https://openrouter.ai/api/v1` (LIVE)
- **Auth:** the header `Authorization: Bearer <OPENROUTER_API_KEY>` on every request (LIVE). The key goes only in the header and does not end up in logs or error texts (as in `speech2text.go`).
- **Content-Type:** `application/json`.
- **Optional attribution (cosmetic, does not affect the summary):** `HTTP-Referer: <url>` and `X-Title: <app>` (canonically `X-OpenRouter-Title`, `X-Title` is an alias): only for app ranking on openrouter.ai/rankings. Not routing, not privacy. **Can be omitted.**
- **Key:** self-service on openrouter.ai, prepaid credits. No B2B/moderation needed.

## 1. Our flow: one `chat/completions` call

One endpoint covers the summary scenario. For a long transcript, map-reduce over the same call (§4).

### 1.1 Request

```
POST /api/v1/chat/completions
Authorization: Bearer <key>
Content-Type: application/json
```

Body (the minimum for our scenario):

| Field | Type | Req. | Purpose |
|---|---|---|---|
| `model` | string | yes | Model slug, e.g. `google/gemini-2.5-flash` (the exact slug is critical; see §3) |
| `messages` | array | yes | `[{role, content}]`; for us `system` (the `summary_prompt.txt` prompt) + `user` (the transcript) |
| `max_tokens` | int | no | Response length ceiling (alias `max_completion_tokens`) |
| `temperature` | float | no | 0.0-2.0, default 1.0. For the digest we set it low (0.2-0.3) for determinism |
| `stream` | bool | no | Default `false`, our case (synchronous response) |

Message: `{"role": "system"|"user"|"assistant", "content": "<text>"}`. A string `content` is enough for us.

Example:
```json
{
  "model": "google/gemini-2.5-flash",
  "messages": [
    {"role": "system", "content": "<contents of summary_prompt.txt>"},
    {"role": "user", "content": "<markdown transcript>"}
  ],
  "temperature": 0.3,
  "max_tokens": 4096
}
```

> Warning: **Go trap `omitempty` + `temperature: 0`.** If the field `Temperature float64` is tagged `json:",omitempty"`, the value `0.0` is NOT serialized, so the API applies the default `1.0` (non-deterministic). If `temperature: 0` is needed, send the field without `omitempty` (or use a pointer). We use 0.3, so it does not bite us, but we flag it in the code.

### 1.2 Response (synchronous success)

```json
{
  "id": "gen-...",
  "model": "google/gemini-2.5-flash",
  "choices": [
    {
      "index": 0,
      "finish_reason": "stop",
      "message": {"role": "assistant", "content": "# Конспект встречи..."}
    }
  ],
  "usage": {"prompt_tokens": 312, "completion_tokens": 187, "total_tokens": 499}
}
```

(The sample `content` "# Конспект встречи..." means "# Meeting digest..."; it is a Russian model reply, kept as in the example.)

- **Our result is `choices[0].message.content`** (the digest markdown).
- `model` in the response is the model ACTUALLY used (it may differ from the requested one on routing fallback). (DOCS)
- `usage` is for the cost log; keep it nil-safe in Go (some providers may not return it). (DOCS)

`finish_reason` (LIVE/DOCS):

| Value | Meaning for us |
|---|---|
| `stop` | Normal: the model finished on its own. Expected. |
| `length` | **Truncated** by `max_tokens`: the digest is incomplete. Raise `max_tokens` or flag an error. |
| `content_filter` | Blocked by moderation. |
| `error` | Processing error (see below about 200+error). |
| `tool_calls` | Not our scenario (we send no tools). |

For the summary: any `finish_reason != "stop"` is treated as a failure (our `Summarize` contract: error, then `failed_summary`).

> Warning: **(DOCS) `200 OK` can carry an error in the body.** In a synchronous response an upstream error arrives as `choices[0].finish_reason == "error"` + `choices[0].error` with HTTP 200. So check not only the HTTP code but also `finish_reason`/the presence of `content`. Empty `content` with `finish_reason != "stop"` is an error.

## 2. Error codes and body

**Error body (DOCS):** `{"error": {"code": <int>, "message": <string>, "metadata": {...}}}`. `code` duplicates the HTTP status as a number; `metadata` is optional (for 403 moderation: `reasons[]`, `flagged_input`; for 502: `provider_name`). The body is for diagnosis in the text of our error (like `s2tHTTPError`); the key does not show up in it (it is in the header).

| Code | Meaning | Retry |
|---|---|---|
| 400 | Invalid request; **including context exceeded** | No: fix the payload (for context: chunking, §4) |
| 401 | Invalid/expired key | No |
| 402 | Out of credits | No: top up |
| 403 | Moderation / no permissions | No |
| 408 | Request timeout | Yes (backoff) |
| 429 | Rate limit | Yes (backoff; `Retry-After` best-effort) |
| 500 / 502 / 503 / 504 | Internal error / model unavailable / no provider / gateway timeout | Yes (backoff) |

> 404 / 413 / 422 are also possible (DOCS): rare for our payload, handled by the common "non-2xx means error" branch.
>
> Warning: `Retry-After` is declared for 429 and 503 (errors doc) but is not documented in the limits doc, so it is **best-effort**: parse it if present, otherwise use a fixed backoff.
>
> Warning: **(unverified)** The free-model limit numbers and the payment fee in the docs are JS templates with no concrete values; we do not include them in code/calculations. Paid models have no hard public RPM (the limit is global per account, extra keys do not help).

**The `Summarize` contract:** the orchestrator moves a `Summarize` error to `failed_summary`, but first delivers the transcript-only file and explains that specifically the digest was not produced. Before that, the OpenRouter client does a bounded retry inside each `chat` step: transient HTTP `408/429/5xx`, transient `error.metadata.error_type`, `Retry-After` best-effort, request/read/decode failures and the known signature `JSON error injected into SSE stream`. Permanent `400/401/402/403` and permanent `error_type` are not retried.

## 3. Models

Numbers are LIVE `GET /api/v1/models`, 2026-06-22 (full table and economics: `README.md`):

| Slug | Context | Max output | $in/1M | $out/1M |
|---|---|---|---|---|
| `google/gemini-2.5-flash` (default) | 1,048,576 | 65,535 | $0.30 | $2.50 |
| `openai/gpt-4o-mini` | 128,000 | 16,384 | $0.15 | $0.60 |
| `deepseek/deepseek-chat` | 131,072 | 16,000 | $0.20 | $0.80 |

> The slug must be exact: `anthropic/claude-sonnet-4.5` (with a dot), not `-4-5`; preview variants of gemini are unavailable on OpenRouter, use the GA slug `google/gemini-2.5-flash`.
>
> **Thinking in gemini-2.5-flash:** on by default, reasoning tokens are billed as output ($2.50/1M). Controlled by a nested `reasoning` object (NOT `max_tokens_for_reasoning`): budget `{"reasoning": {"max_tokens": N}}`, disable `{"reasoning": {"effort": "none"}}`. For the summary: tune on a live transcript (cost/latency vs quality); v1 keeps the default.
>
> Programmatic model discovery: `GET /api/v1/models` (fields `id`, `context_length`, `top_provider.max_completion_tokens`, `pricing.prompt`/`pricing.completion` as per-token strings, multiply by 1,000,000 for the price per 1M). Routing suffixes `:floor` (cheap provider) / `:nitro` (max throughput).

## 4. Integration sketch (`openrouter.go`)

Implements `Summarizer` (`summary.go`). `orchestrator.go` does not know about OpenRouter: it sees the interface. Mirrors the style of `speech2text.go` (raw `net/http`, its own `*http.Client`, `io.LimitReader` on the body, key in the header). No SDK and no new dependencies.

```
NewOpenRouterSummarizer(apiKey, baseURL, model, proxyURL) -> prompts from summary_prompt.txt + summary_map_prompt.txt (go:embed)

Summarize(ctx, transcript) (markdown, error):
  if len(transcript) <= maxChunkChars:                 // short meeting: one call
      return chat(ctx, defaultSummaryPrompt, transcript)
  // map-reduce (long meeting):
  chunks := splitByLines(transcript, maxChunkChars)    // split by lines, do not break utterances
  partials := for each chunk: chat(ctx, mapPrompt, chunk)   // compress the fragment
  return chat(ctx, defaultSummaryPrompt, join(partials))    // merge into the final digest

chat(ctx, system, user):
  up to 3 attempts inside one map/reduce step, ctx bounds the whole loop
  POST /chat/completions {model, messages:[system,user], temperature:0.3, max_tokens}
  non-2xx          -> transient HTTP is retried, permanent HTTP is an immediate error
  choices[].error / top-level error -> transient error_type/code/message is retried
  finish_reason!=stop / empty content -> error
  return choices[0].message.content
```

Slice decisions (per the owner's choice of 2026-06-22):
- **Model:** `google/gemini-2.5-flash` by default, env `OPENROUTER_MODEL` (swap by one line).
- **Proxy:** optional env `OPENROUTER_PROXY_URL`; applied only to `OpenRouterSummarizer`
  through a separate `http.Transport.Proxy`. A global `HTTPS_PROXY` is not the target solution:
  it would affect Telegram, Speech2Text and YooKassa.
- **Chunking:** map-reduce **by a size threshold** (`maxChunkChars`), not by the context limit: with gemini's 1M context a limit trigger would never fire (dead code). Short meetings get one call; long ones are split. The map prompt is a separate embed `summary_map_prompt.txt` (compress a fragment); reduce uses the existing `summary_prompt.txt`.
- **Timeout.** The current `summaryTimeout = 180s` (`orchestrator.go`) wraps the whole `Summarize`. Map-reduce makes N+1 sequential calls, so the timeout must cover the whole cycle, not a single HTTP call.
- **Retry.** The retry lives inside `chat`, not around the whole `Summarize`: in map-reduce the chunks that already succeeded are not recomputed, only the failed HTTP call is repeated. After retries are exhausted the behavior is as before: the error returns to the orchestrator, which delivers the transcript-only file and sets `failed_summary`.

Config (`config.go`, modeled on s2t): `SUMMARIZER_PROVIDER` += `openrouter`; `OPENROUTER_API_KEY` (required when provider=openrouter), `OPENROUTER_MODEL` (default `google/gemini-2.5-flash`), `OPENROUTER_BASE_URL` (default `https://openrouter.ai/api/v1`), `OPENROUTER_PROXY_URL` (optional). Wiring: `main.go` `case "openrouter"`; `.env.example` gets new lines.

## 5. Open / unverified questions

- **Legal basis for the cross-border transfer of personal data (152-FZ):** prod/dev already use OpenRouter as the `Summarizer`; until the customer decides, this is a live-risk before a wide rollout to users, not a technical runtime blocker.
- **Empty/whitespace transcript:** the API behavior (400 or 200 with a trivial answer) is not recorded; the guard against empty input is on our side before the call.
- **Detecting "context exceeded"** among other 400s: the docs have no separate `error.code`, distinguish by `error.message`; practically unreachable on the default 1M model.
- **`Retry-After`:** presence is not guaranteed (best-effort, §2).
- **Gemini thinking:** whether to disable it for cost (§3): on a live test.

## 6. Sources

[chat completions](https://openrouter.ai/docs/api/api-reference/chat/send-chat-completion-request), [overview/auth](https://openrouter.ai/docs/api/reference/overview), [errors-and-debugging](https://openrouter.ai/docs/api/reference/errors-and-debugging), [limits](https://openrouter.ai/docs/api/reference/limits), [provider-selection](https://openrouter.ai/docs/guides/routing/provider-selection), [structured-outputs](https://openrouter.ai/docs/guides/features/structured-outputs), [reasoning-tokens](https://openrouter.ai/docs/guides/best-practices/reasoning-tokens), [models API](https://openrouter.ai/docs/api/api-reference/models/get-models), models [gemini-2.5-flash](https://openrouter.ai/google/gemini-2.5-flash) / [gpt-4o-mini](https://openrouter.ai/openai/gpt-4o-mini) / [deepseek-chat](https://openrouter.ai/deepseek/deepseek-chat).
