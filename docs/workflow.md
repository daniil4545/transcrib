# Task workflow transcrib

The project is in maintenance, so this is a lightweight model: an issue as an executable spec,
the `take #NN` command, milestones in the tracker. Separate specs and phase gates
of the main pipeline do not apply here.

Development canon: a task is written up as a GitHub Issue, the owner gives the command `take #NN`, the agent
brings the issue to a PR into `dev`. The chat orchestrates execution, GitHub Issue/PR is the durable source of truth.

## Branches and deploy

- `dev` - the main working branch. A merge into `dev` triggers CI/CD and a deploy to Coolify dev.
- `feature/<slug>` - the branch of one issue, always cut from the current `dev`.
- Parallel issues run in separate git worktrees, one worktree per issue.
- `main` - prod candidate, updated only through a release PR `dev -> main`.
- A release tag (`0.0.1`, `0.0.2`, `0.1.0`) is the only ref for prod deploy.
- Prod deploy is only a manual run of the GitHub Actions `Deploy` on a tag with `target=prod`.

Forbidden without an explicit owner command: merging a PR, prod deploy, env/secrets, live DB mutation,
refund/payment action, destructive git/file operations.

## Main loop

1. An incoming bug/idea/remark becomes a GitHub Issue.
2. The agent investigates the facts: docs, code, logs, DB/runtime if needed.
3. The agent writes the issue as an executable spec.
4. For risky issues, the Architect, Tester and code reviewer check the spec before development; blockers
   go into the issue.
5. In a separate execution chat the owner writes: `take #NN`.
6. The agent executes the issue up to a PR into `dev` and asks questions if it sees problems.
7. For risky issues an acceptance review runs: tests, logic, architecture, integration with the codebase
   and runtime, if needed.
8. The owner reviews the PR and separately gives the merge command.
9. After the merge the agent checks the dev deploy, if the task needs a runtime check.

## Issue as a spec

The owner command `take #NN` means the issue is ready for execution. The `agent-ready` label may be
used as a backlog marker, but the agent does not require it if the command is given explicitly. No separate
spec document is created: the issue is self-sufficient.

Minimal issue structure:

- Goal: what result is needed.
- Context: why we do it, links to canons, incidents, PRs.
- Boundary: what we explicitly do not touch.
- Action algorithm: imperative steps for the agent.
- Contracts: statuses, fields, functions, API, env, migrations, if they change.
- Acceptance criteria: verifiable conditions.
- Acceptance contract: invariants and scenarios that protect the task. If an automatic
  acceptance test is needed, the issue fixes the test name/location, input data, expected result and what must not be
  changed without agreement.
- Checks: exact commands, smoke/E2E or the reason they are not run.
- Review gates: which independent checks are needed before merge: Tester, logic/domain, Architect,
  code review, DevOps/runtime. For a simple fix you may write `not needed`.
- Docs: which canons to update or explicitly `docs unchanged`.
- Stop conditions: when the agent must stop.
- PR DoD: what the PR description must contain.

If an issue is too large, the agent does not implement it in one diff. It splits it into sub-issues with
independent acceptance and dependencies. The command `take #NN` on an umbrella issue allows
decomposition, but code starts only on a separate sub-issue after the owner's command.
The milestones tracker stays at milestone level.

Criteria of a large issue: money, quotas, state machine, migrations, external API, provider runtime,
deploy/env, prod, many modules or a diff that is hard to review quickly.

The acceptance contract must appear before implementation. Tests from it are not the "tests of the agent"
that writes the code: they are part of the spec. If the contract is wrong or conflicts with the real code/canons,
the implementation agent stops and proposes a fix to the issue instead of rewriting the test to fit the code.

## Issue execution

The orchestrator agent in the execution chat:

1. Checks `gh`/git access and the current `dev`.
2. Reads the issue body, comments, related canons.
3. Creates or opens a dedicated worktree and the branch `feature/<issue-slug>` from `dev`.
4. Splits the work into subtasks and assigns executors only if needed.
5. If the issue requires an independent acceptance test, it is written before the implementation diff by a separate
   agent/commit or explicitly recorded as a manual smoke.
6. Implements the minimal diff for the issue, without adjacent improvements.
7. Does not delete or weaken acceptance tests without an explicit owner decision in the issue/PR.
8. Runs the checks from the issue and extra checks by risk (subagent review).
9. Updates docs only if a fact, contract, process, runtime or deploy changed.
10. Commits and pushes the feature branch.
11. Opens a PR into `dev`.
12. Fills in the PR: summary, issue/spec compliance, verification, acceptance review, risks, not tested.

The agent does not ask the owner during the work if the issue contains a sufficient spec and there is no stop
condition. Adjacent problems found are filed as follow-up issues, not fixed in the current diff.

## Parallel work

Several issues can run at once in different chats. Each execution chat works only in
its own worktree and its own branch:

```text
../transcrib-issue-NN   -> feature/issue-NN-<slug>
../transcrib-issue-MM   -> feature/issue-MM-<slug>
```

Rules:

- before edits the agent checks `git status` and the current worktree;
- one agent does not edit files in another agent's worktree;
- the shared repo checkout is not used for parallel implementation;
- if the needed worktree is already taken by another issue, the agent creates a new one or stops;
- overlaps between tasks are not resolved locally "while at it";
- branch divergences are resolved at the PR stage: rebase/merge from a fresh `dev`, review and conflict resolution.

Why: worktree isolation removes accidental mixing of diffs, and the PR stays the normal place for
resolving conflicts between independent tasks.

## Agent roles

- Orchestrator: runs the chat, holds the issue boundaries, accepts reports, performs git/GitHub actions.
- Worker: writes a narrow piece of code for the given files and contracts.
- Tester: independently records and checks acceptance tests/smoke, edge cases, race/idempotency,
  critical scenarios.
- Code reviewer: looks for bugs, extra code, weak tests, mismatch with the issue.
- Architect: checks architecture, DB, money, state machine, provider/deploy contracts.
- DevOps: server, DB, Docker, deploy/runtime via `deploy/safe-ssh.sh`.
- Docs auditor: docs sync, if canons or runtime facts changed.

By default the orchestrator does everything itself. Subagents are engaged only by risk or when the issue
says so directly.

For money, quotas, state machine, migrations, provider runtime, deploy/env and prod gates, implementation and
acceptance must be separated: the Worker writes the code, a separate Tester/reviewer checks acceptance. For
small fixes the orchestrator's self-check is enough, unless the issue directly requires review gates.

## Verification

Before the PR the agent must explicitly record:

```text
Verified:
- go test ...: pass
- go vet ./...: pass
- manual E2E: not run, reason ...
```

For Go changes that touch `internal/app`, the state machine, DB, provider, billing,
deploy/config or runtime behavior, the local gate must match CI:

```sh
cd src
NO_DOTENV=1 DATABASE_URL=postgres://transcrib:transcrib@localhost:5432/transcrib?sslmode=disable make ci-check
```

`go test ./...` without `DATABASE_URL` does not count as a full gate: DB-backed tests are skipped.
`NO_DOTENV=1` is needed for AI agents in a sandbox so they do not read local `.env` files with secrets.
The deploy workflow must run the same DB-backed gate before building the image, otherwise a dev/prod rollout
is not considered protected.

Critical nodes need targeted tests: money, quota reserve, idempotency, state machine,
races. For Telegram UI, provider runtime, deploy and config a manual E2E or dev smoke is acceptable.

Acceptance tests recorded in the issue must not be changed in an implementation PR without explicit agreement.
If a test changes, the PR must explain which contract changed and where it was agreed.

A task is not closed if the acceptance criteria and review gates from the issue are not checked, or unchecked items
are not named explicitly.

## Context and long tasks

GitHub Issue and PR hold the state of the work. The chat is not the only source of truth.

If the task is long, before a compact or a pause the agent writes a checkpoint in an issue comment:

```text
Branch:
Done:
Changed files:
Verified:
Open risks:
Next step:
```

After a compact or in a new chat the agent continues from the issue + the last checkpoint.

For an execution chat the goal is phrased as: `bring issue #NN to a PR into dev`. Goal mode helps
drive the session but does not replace the issue.

## Closing a PR

A PR is ready for owner review if:

- the scope matches the issue;
- CI/local checks are green or exceptions are explicitly explained;
- the acceptance contract is satisfied and its tests are not weakened without agreement;
- the review gates from the issue are passed: Tester, logic/domain, Architect, code review, DevOps/runtime or
  explicitly `not needed`;
- docs are synchronized if a contract changed;
- the PR lists risks and unchecked items;
- there are no blockers from the reviewer/architect/tester.

Merge into `dev` happens only on an explicit owner command. After the merge the agent checks GitHub
Actions and Coolify dev, if the task needs a runtime check.

## Release

A release is a separate task:

1. Assemble the release PR `dev -> main`.
2. Reconcile the included issues/PRs.
3. Check that the acceptance contract and review gates are closed in the included risky issues.
4. Run the needed smoke tests and `deploy/verify-deploy.sh`.
5. Record the release acceptance report: SHA, issues/PRs, checks, runtime smoke, risks, rollback.
6. A merge into `main` does not roll out prod.
7. Prod deploy only by tag and an explicit owner command.
