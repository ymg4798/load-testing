---
name: bench
description: Measures lt's own load generation at 10k and 30k requests/s on this machine (achieved rate, generator CPU, scheduling lag) and compares with the stored baseline. Use when engine, metrics, protocol steps or other hot paths change.
allowed-tools: Bash(bash .claude/skills/bench/scripts/bench.sh*)
---

```bash
bash .claude/skills/bench/scripts/bench.sh            # compare with baseline.json (about 40 s)
bash .claude/skills/bench/scripts/bench.sh --update   # store this run as the new baseline (ask the user first)
```

The script starts `testserver` on a free port, runs 10 s at each rate, and prints a table:
achieved req/s, generator max CPU, worst-second scheduling lag p99, dropped. It exits 1 when a rate is
missed by more than 2%, CPU is more than 1.5× baseline, or lag p99 is more than 3× baseline (and above 1 ms).

Laptops are noisy: re-run a failing result once before calling it a regression, and close heavy apps.
Numbers are only comparable on the same machine; `baseline.json` records which machine it came from.
