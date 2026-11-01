#!/bin/bash
# End-to-end run: testserver (with the lt agent) + lt run with APM, tracing,
# monitoring and alerts; then assertions on the result. Usage: e2e.sh [outdir]
set -u
root=$(git rev-parse --show-toplevel) || exit 2
cd "$root"
out=${1:-$(mktemp -d)}
mkdir -p "$out"
pids=()
cleanup() { for p in "${pids[@]}"; do kill "$p" 2>/dev/null; done; wait 2>/dev/null; }
trap cleanup EXIT INT TERM

freeport() { python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()'; }
HTTP=$(freeport); APM=$(freeport)

make build >/dev/null || { echo "FAIL build"; exit 1; }
bin/testserver -http "127.0.0.1:$HTTP" -grpc "" -apm "http://127.0.0.1:$APM" >"$out/testserver.log" 2>&1 &
pids+=($!)
for _ in $(seq 1 50); do curl -s "127.0.0.1:$HTTP/fast" >/dev/null && break; sleep 0.1; done

echo "e2e: testserver :$HTTP, apm :$APM, output $out"
bin/lt run .claude/skills/e2e/scenarios/smoke.yaml -q --apm "127.0.0.1:$APM" \
  -e "baseUrl=http://127.0.0.1:$HTTP" -o "$out/result.json" --html "$out/report.html" --md "$out/summary.md" \
  >"$out/lt.log" 2>&1
code=$?
echo "lt exit code: $code (1 = a threshold failed or a limit stopped the run; >1 = error)"
[ -s "$out/result.json" ] || { echo "FAIL no result file"; tail -20 "$out/lt.log"; exit 1; }

python3 - "$out/result.json" <<'EOF'
import json, sys
r = json.load(open(sys.argv[1]))
s = r["summary"]; apm = r.get("apm") or {}; logs = r.get("logs") or {}
checks = [
    ("requests > 500", s["requests"] > 500),
    ("no dropped arrivals", s["dropped"] == 0),
    ("hitmap recorded", sum(1 for x in r["snapshots"] if x.get("hits")) >= 10),
    ("server hitmap recorded (APM)", sum(1 for x in r["snapshots"] if x.get("apmHits")) >= 5),
    ("server metrics recorded", any(x.get("server") for x in r["snapshots"])),
    ("traced exemplars", any(k.get("slowest") for k in s["keys"])),
    ("APM traces kept", len(apm.get("traces") or []) > 10),
    ("APM operations incl. SQL", any(o["kind"] == "db" for o in apm.get("operations") or [])),
    ("service map client->service->db", len((apm.get("map") or {}).get("nodes") or []) >= 3),
    ("error groups (panic)", any(g["source"] == "panic" for g in apm.get("errors") or [])),
    ("logs received with trace IDs", any(l.get("traceId") for l in logs.get("lines") or [])),
    ("alert fired", any(e["state"] == "firing" for e in r.get("alerts") or [])),
    ("findings present", len(r.get("insights") or []) > 0),
    ("threshold evaluated", len(r.get("thresholds") or []) == 1),
]
bad = 0
for name, ok in checks:
    print(("  PASS  " if ok else "  FAIL  ") + name)
    bad += not ok
print("findings:")
for i in r.get("insights") or []:
    print("   -", i["title"])
sys.exit(1 if bad else 0)
EOF
ok=$?
echo "result: $out/result.json"
echo "report: $out/report.html"
exit $ok
