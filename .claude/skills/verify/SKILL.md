---
name: verify
description: Full verification gate for lt — gofmt, go vet, go test -race on all packages, JS syntax of the pages, example validation and cross-platform builds. Use before every commit and when asked whether the code is in a good state.
allowed-tools: Bash(bash .claude/skills/verify/scripts/verify.sh*)
---

Run:

```bash
bash .claude/skills/verify/scripts/verify.sh            # everything (about 2 minutes)
bash .claude/skills/verify/scripts/verify.sh --quick    # skip race detector and cross builds
```

The script prints one line per gate and exits non-zero if any gate fails, with the failing output after the
summary. Report PASS/FAIL per gate. For failures, show the trimmed output and the smallest reproduction
(`go test -run TestName ./pkg/`); do not paper over a failure by skipping or loosening a test.
