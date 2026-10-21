# Testing transcrib

Coverage canon. Maintained by the `tester` agent. Project principle: **targeted tests only on critical nodes**, the rest is manual E2E. Fewer tests that catch real bugs are better than many written for coverage.

Test stack: stdlib `testing`, table-driven + `t.Run`. Integration tests of critical nodes hit a real Postgres via `DATABASE_URL`; without it they `t.Skip` (a bare `go test` works without infrastructure). External services are `MockProvider` and recording fakes. Local DB-backed run: `docker compose up -d`, then `NO_DOTENV=1 DATABASE_URL=... make ci-check` from `src/`; without `DATABASE_URL` the integration tests are skipped. The CI workflow and the Deploy workflow start PostgreSQL 16, set `DATABASE_URL`, run `make migrate-up`, then `go test ./...`; the Deploy workflow does not build the image if the DB-backed gate failed.

## Check types and change gate

| Type | Meaning | Who changes it | Change gate |
|---|---|---|---|
| `contract` | The test protects a canon invariant from `docs/db.md`, `docs/architecture.md`, `docs/payments.md` or `docs/product.md`. | Not the implementation agent alone. | Only together with a canon/issue-spec change or explicit owner approval in the PR. |
| `acceptance` | A test or smoke is explicitly fixed in the issue as an acceptance contract before implementation. | Independent Tester/reviewer. | Must not be removed or weakened in an implementation PR without owner approval. |
| `implementation` | A local helper/parser/format/config test, useful for development but not standalone acceptance. | Code author. | Normal code review; does not replace acceptance. |
| `runtime` | A live dev/test smoke for an external boundary: Telegram, provider, OpenRouter, YooKassa, Coolify/env. | Tester/DevOps with artifacts. | In the PR/release report: SHA, job/payment id, commands, log window, result. |
| `gap` | The contract exists, but there is no automated or runtime check yet. | Via a follow-up issue. | Do not close as "covered"; add an acceptance contract before implementation. |
| `noise candidate` | The check looks weakly tied to a contract or duplicates a stronger test. | Only via a separate cleanup issue. | Do not delete in a feature PR "while at it". |

For money, quotas, state machine, migrations, idempotency, refund/recurring and provider/deploy
contracts, acceptance is written or checked by an independent Tester/reviewer. If a test turns out to be wrong,
the issue spec or canon changes; the test is not silently fitted to the implementation.

## Audit map

Source of truth for critical nodes: `docs/db.md` §5 and `docs/architecture.md` §8. In the table below
`Change gate = canon/owner` means: the tests must not be weakened without a change to the canon/issue spec or
explicit owner approval.

| Contract source | Invariant | Tests/smoke | Type | Change gate |
|---|---|---|---|---|
| `architecture.md` §8.1, `db.md` §5.1 | `CreateJob` atomically reserves minutes, serializes the user, prevents double quota spending. | `storage_quota_test.go`: `TestCreateJob_TrialRace`, `TestCreateJob_SubscriptionRace`, `TestCreateJob_SubscriptionPriorityOverTrial`, `TestCreateJob_SelectsFullSubscriptionBeforeOlderPartial`, partial-minute tests. | `contract` | `canon/owner` |
| `db.md` §5.8, `architecture.md` §8.1 | Different active URLs are allowed within the quota and cap=4; the same URL is rejected before quota is spent. | `storage_quota_test.go`: `TestCreateJob_RateLimit*`, `TestCreateJob_ActiveCap*`; `storage_phases_test.go`: `TestCreateJob_RateLimit_Phases`. | `contract` | `canon/owner` |
| `architecture.md` §8.2 | Worker claim does not hand one Job to two workers; backoff is held via `updated_at`. | `storage_test.go`: `TestClaimJobsForWork_*`, `TestClaimJobsForWork_NoDoubleClaim`. | `contract` | `canon/owner` |
| `architecture.md` §8.2 | Submit/poll failure paths move the Job to the right statuses and return the quota only before the service is rendered. | `orchestrator_test.go`: `TestSubmit_*`, `TestPoll_JoiningStartTimeoutCancelsAndRefunds`, `TestPollJob_StaleTimeout`, `TestPollJob_FreshJobNotStale`. | `contract` | `canon/owner` |
| `architecture.md` §7/§8.2, `db.md` §5.3 | The state machine does not move a terminal Job again; phase transitions `joining/recording/processing/summarizing/done` are idempotent. | `orchestrator_test.go`: `TestAdvance_*`, `TestPhaseFlow_*`; `storage_phases_test.go`: `TestSetJobRecording_Phase`, `TestSetJobProcessing_Phase`, `TestSetJobDone_From*`. | `contract` | `canon/owner` |
| `architecture.md` §8.3, `db.md` §5.2 | An empty transcript does not charge the quota, does not start the summary and closes the Job as failed. | `speech2text_test.go`: `TestSpeech2TextFetch_EmptyTranscript`; `orchestrator_test.go`: `TestFinishTranscription_EmptyTranscriptRefunds`. The dev provider empty-meeting smoke stays runtime. | `contract`, `runtime` | `canon/owner` for unit, smoke evidence for runtime |
| `architecture.md` §8.4, `db.md` §5.7 | The summary path caches `summary_md`, does not repeat the paid LLM call on retry, `failed_summary` does not make an early refund, manual summary recovery does not re-bill the Job. | `orchestrator_test.go`: `TestSummarizeJob_*`, `TestSummarizeJob_DeliverRetryDoesNotResummarize`, duration tests; `tg_test.go`: redelivery/card tests; `storage_test.go`: `TestSetTerminalJobSummary_*`. | `contract` | `canon/owner` |
| `db.md` §5.2 | `FailJobWithQuotaRefund` rolls back the reserve exactly once and only from early statuses. | `storage_quota_test.go`: `TestFailJobWithQuotaRefund_*`, `TestSetJobFailedSummary_NoRefund`; `storage_phases_test.go`: `TestFailJobWithQuotaRefund_FromRecording`. | `contract` | `canon/owner` |
| `db.md` §5.3/§5.7 | Terminal billing rounds the duration, returns the unused part, charges the extra only from the original source, creates no debt. | `storage_quota_test.go`: `TestBilledMinutes`, `TestSetJobDone_*`, `TestSetJobFailedSummary_*`, `TestSetJobDone_ConcurrentDoubleDone`. | `contract` | `canon/owner` |
| `architecture.md` §8.5, `db.md` §5.4 | A direct payment intent is created in two phases, polling finalizes exactly one subscription and checks the provider response. | `billing_test.go`: `TestCreatePayment_*`; `storage_yookassa_test.go`: pending/finalize tests; `payment_poller_test.go`: finalize/mismatch/cancel tests. | `contract` | `canon/owner` |
| `db.md` §5.4 | A successful payment creates a separate 30-day subscription row; concurrent payments do not chain the term. | `billing_test.go`: `TestRecordSuccessfulPayment_NoActive`, `TestRecordSuccessfulPayment_IndependentOfActive`, `TestRecordSuccessfulPayment_ConcurrentSameUser`; `storage_yookassa_test.go`: independent/concurrent tests. | `contract` | `canon/owner` |
| `db.md` §5.4, `architecture.md` §8.5 | A recurring profile goes through `active/charging/charge_failed/blocked/disabled` without a double external POST and without reactivating disabled/blocked. | `recurring_test.go`, `payment_poller_test.go`, `storage_yookassa_test.go`, `tg_test.go`: recurring status, due claim, stale intent, disable, canceled reasons. | `contract` | `canon/owner` |
| `db.md` §5.5 | The legacy `pre_checkout_query` validates only the payload, quickly and without JOIN. | `billing_test.go`: `TestValidatePreCheckout`. | `contract` | `canon/owner` |
| `architecture.md` §8.6, `db.md` §5.9 | A refund is two-phase; a succeeded refund deactivates exactly the purchased subscription and disables recurring. | `storage_refund_test.go`: `TestBeginRefund_*`, `TestFinalizeRefund_*`; `billing_test.go`: `TestCreateRefund_PostsYookassaRequest`. A full YooKassa refund stays runtime/manual. | `contract`, `runtime` | `canon/owner` for unit, smoke evidence for runtime |
| `architecture.md` §8.7, `db.md` §5.2 | Cancel performs an owner check, idempotently fails the active Job, returns the reserve and does not touch other users' or done Jobs. | `storage_cancel_test.go`: `TestCancelJob_*`; provider Cancel via the Telegram panel stays runtime. | `contract`, `runtime` | `canon/owner` for storage, smoke evidence for runtime |
| `architecture.md` §8.8, `db.md` §5.6 | F9/nudges send first, then mark; dedup push classes and do not lose the retry on send failure. | `notifier_test.go`: `TestNotifyGroup_SendThenMark`, `Test*Dedup`, payment/sub low/empty tests. Real Telegram delivery stays runtime. | `contract`, `runtime` | `canon/owner` for storage, smoke evidence for runtime |
| `architecture.md` §8.5, `payments.md` | YooKassa REST request shape: amount, receipt, metadata, idempotence key, save payment method. | `billing_test.go`: `TestBuildInvoice_*`, `TestCreatePayment_*`, `TestCreateRecurringPayment_PostsYookassaRequest`, `TestGetPayment_ParsesYookassaResponse`. | `contract` | `canon/owner` |
| `architecture.md` §9/§10, provider API docs | Provider status mapping treats unknown status as contract drift; access code/link parsing keeps meeting credentials. | `speech2text_test.go`: `TestMapS2TStatus`, `TestMeetingAccessCode*`; `mymeet_test.go`: status/source/poll/fetch tests; `tg_test.go`: `TestFindMeetingLink`. | `contract`, `implementation` | `canon/owner` for status mapping; code review for parsers |
| `architecture.md` §8.4, OpenRouter API docs | OpenRouter summarizer handles non-2xx, body-level errors, choice errors, transient retry, empty input and map-reduce calls. | `openrouter_test.go`: `TestOpenRouterSummarize_*`, `TestSplitTranscript`; `openrouter_live_test.go` is opt-in live. | `contract`, `runtime` | `canon/owner` for parser contract; live-key smoke for runtime |
| `architecture.md` §5/§8.5, migration `00014` | Active recurring profiles align `next_charge_at` with effective subscription expiry; retry profiles are not touched. | `migration_test.go`: `TestMigration00014_AlignsOnlyActiveRecurringProfiles`. | `contract` | `canon/owner` |
| `architecture.md` post-0.1 Google target | `awaiting_reauth` and `/reconnect` resume delivery without duplicate document. | No Go implementation in release 0.1; covered only as future E2E checklist. | `gap` | Follow-up Google issue before implementation |
| `architecture.md` §8.2 provider unknown/paused status | `StatusPaused` and unknown provider statuses do not deliver, notify, or mutate Job incorrectly. | No dedicated unit by design; live provider behavior and logs are runtime evidence. | `runtime` | Smoke evidence; add test only if provider fake is needed by a future issue |

## Test file audit

| Domain | Files | Current role | Notes |
|---|---|---|---|
| Billing and YooKassa HTTP boundary | `billing_test.go`, `payment_poller_test.go`, `storage_yookassa_test.go` | `contract` | Protected: money, receipts, polling, pending intent, independent periods. |
| Quota and storage transactions | `storage_quota_test.go`, `storage_test.go`, `storage_phases_test.go`, `storage_cancel_test.go`, `storage_refund_test.go` | `contract` | Protected: DB physics, guards, races, refund/cancel/terminal billing. |
| Worker state machine | `orchestrator_test.go` | `contract` | Protected when assertions cover status, delivery, notify, quota and summary retry semantics. |
| Recurring | `recurring_test.go` plus payment/storage tests | `contract` | Protected until recurring runtime is deliberately redesigned. |
| Provider adapters | `speech2text_test.go`, `mymeet_test.go` | `contract`, `implementation` | Status mapping is protected; URL/access-code/render helpers are implementation unless tied to provider API docs. |
| Summary | `openrouter_test.go`, `summary_test.go`, `openrouter_live_test.go` | `contract`, `implementation`, `runtime` | OpenRouter parser/map-reduce is protected; mock prompt wiring is implementation; live test is opt-in runtime. |
| Telegram UX glue | `tg_test.go` | `contract`, `implementation`, `runtime` | Payment/card/balance behavior is protected; formatting/support-copy tests are implementation or noise candidates. |
| Notifications | `notifier_test.go` | `contract` | Protected for dedup/send-then-mark; analytics metadata helper is implementation. |
| Config/bootstrap/mocks | `main_test.go`, `mock_test.go` | `implementation`, `noise candidate` | Useful guardrails, but not release acceptance by themselves. |

## Runtime acceptance matrix

Runtime checks validate the real integration boundary. They must be reported with SHA, command/log
window, ids and result. They are not replaced by `go test`.

| Boundary | Scenario | Required artifacts |
|---|---|---|
| Telegram + provider happy path | `/start`, meeting URL, provider bot joins, transcript and summary delivered as `.md`. | dev SHA, Job id, Telegram bot, app log window, delivered filenames. |
| Speech2Text readiness | success, empty meeting, provider/start-timeout, parallel different URLs, duplicate same URL, active cap. | Job ids, provider status/log events, quota before/after for refund cases. |
| OpenRouter live | real summary with configured model; provider/API failure gives transcript-only and `failed_summary`. | Job id, model/provider env from logs, summary status, no duplicate LLM call on retry if observed. |
| YooKassa direct payment links | email capture, payment link, test payment, polling subscription `900/900`, canceled/unpaid path. | payment id, `payments` row status, subscription id, `/balance`, app log window. |
| Recurring before enabling flag | saved payment method, recurring success, retryable failure, revoked/blocked, disable flow. | payment/profile ids, profile statuses, `RECURRING_PAYMENTS_ENABLED` value, log events. |
| Refund MVP | manual refund in the YooKassa merchant dashboard plus linked subscription deactivation, no unrelated subscription touched. | provider payment/refund id, subscription id, DB before/after, operator action note. |
| Coolify/dev deploy | `origin/dev` image is actually running, migrate ok, no config/db/restart-loop errors. | `deploy/safe-ssh.sh coolify-dev-health`, logs window, expected SHA. |
| Release/main gate | release PR `dev -> main` has green CI, prebeta check, smoke evidence and rollback note. | release acceptance report with SHA, issues/PRs, checks, risks, rollback. |

## Noise candidates and follow-up

Noise candidates are not deleted in feature PRs. They need a separate cleanup issue with rationale and
owner approval.

| Candidate | Why it may be weak | Kept until |
|---|---|---|
| `main_test.go::TestParseLogLevel` | Small config parser guard, not a product/architecture acceptance test. | Config bootstrap changes or cleanup issue. |
| `mock_test.go` happy/counter details | Protects dev fake behavior, not user-facing contract. | MockProvider shape changes. |
| `summary_test.go::TestMockSummarizer_PromptWiredIntoOutput` | Checks mock wiring and embedded prompt coupling, not summary quality. | Summary test strategy cleanup. |
| `tg_test.go` pure formatting/support-copy tests | Useful UX guardrails, but can become copy-locking noise if product text changes. | Product copy issue or UX contract update. |
| URL/email parser edge cases not linked to provider/API docs | Some are good regression tests, but change gate should be code review, not canon. | Parser refactor issue. |
| Config default tests in `billing_test.go` | Valuable when env contract changes, but not independent acceptance. | Env/config contract update. |

## Known gaps

- Google delivery `awaiting_reauth` / `/reconnect` has no Go implementation and no automated coverage
  in release 0.1. Add acceptance contract in the Google issue before coding.
- Full external `BeginRefund -> YooKassa -> FinalizeRefund` is a manual MVP process. Unit tests cover DB
  phases and request shape; runtime evidence must come from a test/live refund smoke.
- Provider `StatusPaused` and unknown status behavior is intentionally runtime/log checked until a future
  issue needs a richer provider fake.
- Telegram panel glue for provider-specific redelivery/cancel after provider switch remains runtime E2E:
  storage round-trip is covered, real button flow is not.

## Required commands

Use a temp Go cache in Codex/sandbox sessions:

```sh
cd src
GOCACHE=/private/tmp/transcrib-gocache go test -list . ./...
GOCACHE=/private/tmp/transcrib-gocache go test ./...
```

For DB-affecting slices, CI already runs Postgres-backed tests with migrations. If a local schema
roundtrip is part of the issue, run it on a throwaway database: `goose up`, `goose down`, `goose up`.
