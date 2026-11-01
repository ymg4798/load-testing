#!/bin/bash
# Stop / SubagentStop: vet and test the Go packages changed since HEAD.
# Failing sends Claude back to fix (exit 2). Runs once per stop attempt:
# when Claude is already continuing because of this hook, let it stop.
input=$(cat)
if [ "$(jq -r '.stop_hook_active // false' <<<"$input")" = "true" ]; then
  exit 0
fi
cd "${CLAUDE_PROJECT_DIR:-.}" || exit 0
git rev-parse --verify -q HEAD >/dev/null || exit 0

files=$( { git diff --name-only HEAD -- '*.go'; git ls-files --others --exclude-standard -- '*.go'; } | sort -u)
[ -z "$files" ] && exit 0

pkgs=$(for f in $files; do [ -e "$f" ] && echo "./$(dirname "$f")"; done | sort -u)
[ -z "$pkgs" ] && exit 0

unformatted=$(gofmt -l $(for f in $files; do [ -e "$f" ] && echo "$f"; done) 2>/dev/null)
if [ -n "$unformatted" ]; then
  echo "gofmt needed: $unformatted" >&2
  exit 2
fi
if ! out=$(go vet $pkgs 2>&1); then
  echo "go vet failed for changed packages:" >&2
  echo "$out" | tail -30 >&2
  exit 2
fi
if ! out=$(go test -count=1 -timeout 180s $pkgs 2>&1); then
  echo "go test failed for changed packages ($pkgs):" >&2
  echo "$out" | grep -v '^ok ' | tail -40 >&2
  exit 2
fi
exit 0
