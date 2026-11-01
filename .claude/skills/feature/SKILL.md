---
name: feature
description: The whole development cycle for a change to lt — branch, plan, shared contracts, parallel implementation, verification, review, UI and performance checks, docs, commit — run by the main session with the project's agents. Use when the user asks for a new feature or a non-trivial change.
argument-hint: "<what to build>"
disable-model-invocation: true
---

# /feature $ARGUMENTS

You are the coordinator. Agents do the focused work; you own the plan, the shared contracts, integration,
and talking to the user (in Korean). Keep the user posted with one short line per phase.

## 0. Start clean
- `git status`: if there are uncommitted changes that are not yours, ask the user what to do with them.
- Branch from `master`: `git switch -c feat/<short-slug>` (or `fix/`, `chore/`).

## 1. Plan — `planner` agent
Delegate with the request ("$ARGUMENTS") and any context from the conversation. When the plan comes back:
- Put its **open questions** to the user with AskUserQuestion (recommended option first). Do not guess.
- Sanity-check the task split: disjoint file ownership, nothing in `cmd/lt/main.go` or `go.mod` assigned to two
  tasks. Fix the plan yourself if needed.

## 2. Contracts — you
Add the shared types/fields/interfaces from the plan (`internal/run`, `internal/metrics`, `internal/scenario`,
…), make it build (`go build ./...`, quick tests), and commit: `Add contracts for <feature>`.

## 3. Build — `implementer` agents, in parallel
One `implementer` per task, launched together in one message, in the background. Each prompt must be
self-contained: the goal, the contracts (paste the code), the files it owns, the files it must not touch, tests to
write, how to verify, and what to report. While they work, do integration that does not overlap their files
(flags in `cmd/lt`, docs drafts). If a task touches pages, use `ui-designer` for it instead of `implementer`.

When each reports back: read the report, check the claims you depend on (grep the API), wire it in.

## 4. Verify — `verifier` agent (or run the scripts yourself)
`/verify` must pass. Run `/e2e` when the change shows up in a run. Loop: failures → back to the implementer
that owns the file (SendMessage to it if it is still around, else a new implementer with the failure) → re-verify.

## 5. Review — `reviewer` agent
Give it the branch. Blocking findings go back to implementers (max two rounds; then ask the user how to
proceed). Non-blocking ones: fix if cheap, else list them for the user.

## 6. Pages and speed — when relevant
- Pages changed or new data shown: `/ui-check` on the `/e2e` report (ui-designer fixes what looks wrong).
- Engine, metrics, protocol steps, APM ingest or store changed: `perf-guard` agent (`/bench`).

## 7. Docs — `docs-writer` agent
README (Korean) and DESIGN updated for every new flag, config field, command or limit.

## 8. Finish
- `/verify` once more, then commit on the branch with a clear message (one commit per logical piece is fine).
- Stop every process started (`pgrep -fl 'testserver|bin/lt'`), remove stray files from the repo root.
- Tell the user, in Korean, outcome first: what they can do now (commands), what was verified and how, what was
  not verified, known limits. Then ask whether to merge the branch into `master` — never merge or push on
  your own.
