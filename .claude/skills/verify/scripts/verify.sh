#!/bin/bash
# Verification gate for lt. Usage: verify.sh [--quick]
set -u
cd "$(git rev-parse --show-toplevel)" || exit 2
quick=0; [ "${1:-}" = "--quick" ] && quick=1
log=$(mktemp -d)
fail=0
gate() { # gate <name> <command...>
  local name=$1; shift
  local start=$SECONDS
  if "$@" >"$log/$name.out" 2>&1; then
    printf '  PASS  %-10s %3ss\n' "$name" $((SECONDS - start))
  else
    printf '  FAIL  %-10s %3ss\n' "$name" $((SECONDS - start)); fail=1
  fi
}

fmt_check() { out=$(gofmt -l $(git ls-files '*.go'; git ls-files --others --exclude-standard '*.go')); [ -z "$out" ] || { echo "$out"; return 1; }; }
js_check() { for f in internal/web/assets/app.js internal/web/assets/trend.js; do node -e "new Function(require('fs').readFileSync('$f','utf8'))" || return 1; done; }
examples_check() {
  make build >/dev/null || return 1
  for f in examples/*.yaml; do
    case "$f" in examples/watch.yaml|examples/logs.yaml) continue ;; esac
    # grpc reflection and DB steps need servers; validate the file shape only.
    grep -q 'reflect: true\|driver:' "$f" && continue
    bin/lt validate "$f" >/dev/null || { echo "invalid: $f"; return 1; }
  done
}

echo "verify ($([ $quick = 1 ] && echo quick || echo full))"
gate gofmt fmt_check
gate vet go vet ./...
if [ $quick = 1 ]; then
  gate test go test -count=1 ./...
else
  gate test-race go test -race -count=1 ./...
fi
command -v node >/dev/null && gate js js_check
gate examples examples_check
[ $quick = 1 ] || gate dist make dist

if [ $fail = 1 ]; then
  echo
  for f in "$log"/*.out; do
    name=$(basename "$f" .out)
    grep -q "FAIL\|invalid\|error\|\.go:" "$f" 2>/dev/null || [ "$name" = gofmt ] || continue
    [ -s "$f" ] || continue
    echo "---- $name ----"
    grep -v '^ok \|no test files' "$f" | tail -40
  done
fi
rm -rf "$log"
exit $fail
