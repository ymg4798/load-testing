# lt — project guide

`lt` is a load-testing and monitoring tool in Go: scenarios in YAML (+ JS), HTTP/WS/gRPC/SQL/Redis/GraphQL/TCP
steps, distributed agents, HTML reports and a live dashboard, APM (collector + Go agent), server/DB/container/log
monitoring and an always-on console (`lt watch`). Module `github.com/ymg4798/load-testing`, MIT, public on GitHub later.

- Design and reasons: @docs/DESIGN.md
- How the harness (agents, skills, hooks) works: @docs/HARNESS.md
- User-facing docs are `README.md` (Korean).

## Talking to the user
- Reply in **Korean**, plain words, outcome first. The user is a developer but not a load-testing specialist.
- Ask (AskUserQuestion) when a choice is genuinely theirs; otherwise pick the sensible default and say so.

## Commands
```bash
make build          # bin/lt, bin/testserver
make test           # go test -race ./...
make vet            # go vet + gofmt check
make dist           # 5 platform binaries
```
Skills: `/verify` (full gate), `/e2e` (real run with the test server, APM, logs), `/ui-check` (screenshots of
report pages), `/bench` (throughput regression), `/feature <request>` (the whole development cycle), `/release`.

## Git
- Work on a branch: `feat/<slug>`, `fix/<slug>`, `chore/<slug>`. Commit there after `/verify` passes.
- **Never** merge into `master`, push, tag or open a PR without asking the user first.
- Commit messages: imperative subject, short body with the why. End with the attribution line the system asks for.

## Code rules
- Go 1.27, gofmt, stdlib first. New dependencies only with a reason, MIT/BSD/Apache/MPL licenses, and an entry in
  `THIRD_PARTY_NOTICES.md`. Never copy code from k6 (AGPL) or other copyleft projects; ideas only.
- Measurement correctness comes first: open-model arrivals never wait for responses, latency excludes generator
  scheduling lag (reported apart), HDR histograms are merged, never averaged. Read "측정 원칙" in DESIGN.md before
  touching `internal/engine`, `internal/metrics` or a protocol step's timing.
- Shared types live in `internal/run` (results, APM, logs, alerts) and `internal/metrics` (snapshots, hits,
  active). Change them first, commit, then let parallel work build on them.
- Each package owns its files; parallel agents get disjoint file ownership (see the `/feature` skill).
- Comments: short and plain, explain why, not what. Tests next to the code (`*_test.go`), table tests preferred;
  anything needing Docker/DBs skips unless `LT_TEST_PG` / `LT_TEST_MYSQL` / `LT_TEST_REDIS` / `LT_DOCKER_TEST` is set.
- UI (`internal/web/assets`): plain JS (no build step), uPlot, CSS tokens for light/dark, dataviz rules (one axis per
  chart, fixed categorical order s1/s2/s3, status colors only for status). Verify with `/ui-check` screenshots.

## Running things
- Use the scratchpad or `mktemp -d` for outputs, never the repo root (`design-*` previews are gitignored).
- Ports: pick free ports (the skills' scripts do). Known taken here: 3000 (Grafana), 9090 (Prometheus),
  15432/13306/16379 (deploy/targets DBs when running), 3306 (local mysqld).
- Every process you start (testserver, lt run/watch/agent, docker compose) must be stopped before you finish.
  Check with `pgrep -fl 'testserver|bin/lt'`.
- Headless Chrome screenshots of live (SSE) pages hang: use `--timeout` and kill after a few seconds.

## Hooks in this repo
- After Edit/Write of a `.go` file: gofmt runs on it.
- On Stop / SubagentStop: `go vet` + `go test` for the Go packages changed since HEAD; failures send you back to fix.
