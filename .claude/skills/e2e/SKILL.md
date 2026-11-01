---
name: e2e
description: End-to-end run of lt against the bundled test server with APM, logs, monitoring, alerts and tracing, then checks the result file (requests, findings, traces, service map, error groups, logs) and leaves an HTML report. Use after changes visible in a run, before UI checks, or when asked to "try it for real".
allowed-tools: Bash(bash .claude/skills/e2e/scripts/e2e.sh*)
---

Run:

```bash
bash .claude/skills/e2e/scripts/e2e.sh [output-dir]
```

It builds `bin/`, starts `testserver` on free ports with the lt agent, runs
`.claude/skills/e2e/scenarios/smoke.yaml` (about 15 s: orders with SQL, slow reports, a mutex hot spot, panics,
flaky errors), asserts on the result and prints the paths of `result.json` and `report.html`. Everything it
starts is stopped on exit, even on failure.

Report the assertion lines (PASS/FAIL) and the findings it printed. If an assertion fails, open the result JSON
to find out why before blaming the code (a busy laptop can make timings noisy; re-run once).
Use the `report.html` path for `/ui-check`.
