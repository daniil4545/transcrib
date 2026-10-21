# OpenRouter: target summary provider F3 (Summarizer)

Dossier on OpenRouter as an implementation of the `Summarizer` interface (`internal/app/summary.go`). API contract: [`API.md`](API.md). Transcription providers: `docs/api/speech2text/README.md` (current prod/dev provider) and `docs/api/mymeet/README.md` (working fallback).

Research dated 2026-06-22 (a swarm of agents + adversarial verification; contract-critical and numeric facts were confirmed by a live `curl` to the API and are marked "(LIVE)").

## Status: TARGET `Summarizer` implementation, replaces the unimplemented `claude.go`

The main transcription provider (s2t) does not return a summary through the API, so we build the F3 digest ourselves. By the owner's decision (2026-06-22) the `Summarizer` implementation lives in a separate file `openrouter.go` through OpenRouter, rather than a single-vendor `claude.go`. The default model is **`google/gemini-2.5-flash`** (configurable via env, swapped by one line). `OPENROUTER_PROXY_URL` support is in Git `main` starting from `0.0.3.5`; live prod `0.0.3.4` uses a temporary global proxy hotfix until the next deploy.

## What it is

OpenRouter is an LLM aggregator: a single **OpenAI-compatible** gateway (`/api/v1/chat/completions`) to hundreds of models from dozens of vendors through one key and one prepaid-credits account. For us this means one HTTP integration instead of a client per vendor, and changing the summary model with the `OPENROUTER_MODEL` string without code changes. The downside for our use case: data goes through servers in the USA (cross-border transfer of personal data, see below).

## Model and economics

Candidate comparison (numbers are **LIVE** `GET /api/v1/models`, 2026-06-22):

| Model | Slug | Context | Max output | $in / 1M | $out / 1M | Note |
|---|---|---|---|---|---|---|
| **Gemini 2.5 Flash** | `google/gemini-2.5-flash` | 1,048,576 | 65,535 | $0.30 | $2.50 | **Default.** The 1M context covers any meeting in full; strong Russian. Built-in thinking is on by default and billed as output ($2.50/1M); it can be limited/disabled (`API.md` §3). |
| GPT-4o mini | `openai/gpt-4o-mini` | 128,000 | 16,384 | $0.15 | $0.60 | Cheaper; the 128K context is enough for a meeting of up to ~1.5-2 h. |
| DeepSeek V3 | `deepseek/deepseek-chat` | 131,072 | 16,000 | $0.20 | $0.80 | The cheapest; Russian is acceptable, fewer verified RU benchmarks. |

Estimated digest cost on gemini-2.5-flash: a one-hour meeting is about 10-15k transcript tokens in and ~1-2k out, about **$0.01-0.02 per meeting** (without thinking; more with thinking). Against a 1990 RUB subscription the summary is practically free: the transcription provider economics matter more.

Pricing is per token, with no minimum; top-up is by prepaid credits. The payment fee and BYOK thresholds are rendered by JS templates in the public docs (exact numbers not confirmed), so we do not include them in calculations.

## Legal (152-FZ: the key risk)

A meeting transcript contains personal data (speech, participant names from mymeet). OpenRouter is an operator from **New York, USA**; a summary request is a **cross-border transfer of personal data** to the USA. This is exactly the same concern as for the rejected `claude.go` (`docs/api/speech2text/README.md`, "Counterargument"): a Russian summary provider (a mymeet template, YandexGPT, GigaChat) removes it, OpenRouter does not.

Mitigations on the API side (they reduce retention at the upstream but **do not remove the fact** of the cross-border transfer):
- `provider: {"data_collection": "deny"}`: routing only to providers that do not store or train on data (per their declared policy).
- `provider: {"zdr": true}`: only Zero Data Retention endpoints (list `GET /api/v1/endpoints/zdr`).
- Minimizing personal data before sending (anonymizing speaker names): on our side.

The legal basis for the cross-border transfer (consent/Roskomnadzor notification under 152-FZ) is a product-legal question for the customer before the public launch. The EU endpoint `eu.openrouter.ai` (data does not leave the EEA) is Enterprise only and irrelevant for Russian localization.

## Data privacy (default)

By default OpenRouter **does not store** prompts and responses, only metadata (tokens, latency, cost). Content logging (Input/Output Logging, retention of 3+ months) and the data discount (1% discount for the right to use data) are both **opt-in, off by default**. Each upstream provider has its own policy; OpenRouter is not responsible for it, hence `data_collection: deny` / `zdr` above.

## Open questions

- The legal basis for the cross-border transfer of personal data under 152-FZ (customer): a live-risk before a wide rollout to users; the current prod/dev runtime already uses the OpenRouter summary.
- Whether to disable thinking in gemini-2.5-flash for cost/latency vs digest quality (`API.md` §3): to be tuned on a live transcript.
- Anonymizing speaker names before sending (a shared track with the anti-scope, `docs/product.md` §6).

## Sources

[openrouter.ai/docs](https://openrouter.ai/docs/api/reference/overview) (quickstart, chat completions, errors-and-debugging, limits, provider-selection, structured-outputs, reasoning-tokens), [models](https://openrouter.ai/models) (`google/gemini-2.5-flash`, `openai/gpt-4o-mini`, `deepseek/deepseek-chat`), [privacy](https://openrouter.ai/privacy), [terms](https://openrouter.ai/terms), [ZDR](https://openrouter.ai/docs/guides/features/zdr), [Sovereign AI / EU](https://openrouter.ai/docs/guides/features/sovereign-ai).
