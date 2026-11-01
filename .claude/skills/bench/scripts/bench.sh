#!/bin/bash
# Throughput benchmark of lt itself. Usage: bench.sh [--update]
set -u
root=$(git rev-parse --show-toplevel) || exit 2
cd "$root"
dir=$(cd "$(dirname "$0")/.." && pwd)
update=0; [ "${1:-}" = "--update" ] && update=1
tmp=$(mktemp -d)
pids=()
cleanup() { for p in "${pids[@]}"; do kill "$p" 2>/dev/null; done; wait 2>/dev/null; rm -rf "$tmp"; }
trap cleanup EXIT INT TERM

port=$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()')
make build >/dev/null || exit 2
bin/testserver -http "127.0.0.1:$port" -grpc "" >/dev/null 2>&1 &
pids+=($!)
for _ in $(seq 1 50); do curl -s "127.0.0.1:$port/fast" >/dev/null && break; sleep 0.1; done

for rate in 10000 30000; do
  cat >"$tmp/b$rate.yaml" <<EOF
name: bench-$rate
scenarios:
  s:
    rate: $rate
    duration: 10s
    flow:
      - http: "http://127.0.0.1:$port/fast"
EOF
  bin/lt run "$tmp/b$rate.yaml" -q -o "$tmp/r$rate.json" >/dev/null 2>&1
done

python3 - "$tmp" "$dir/baseline.json" "$update" <<'EOF'
import json, os, platform, subprocess, sys
tmp, base_path, update = sys.argv[1], sys.argv[2], sys.argv[3] == "1"
now = {}
for rate in (10000, 30000):
    r = json.load(open(f"{tmp}/r{rate}.json"))
    s = r["summary"]
    now[str(rate)] = {"rps": round(s["rps"], 1), "cpu": round(s["gen"]["maxCpu"], 1),
                      "lagP99us": s["gen"]["maxLagP99"], "dropped": s["dropped"]}
machine = platform.machine() + " " + subprocess.run(["sysctl", "-n", "machdep.cpu.brand_string"],
                                                    capture_output=True, text=True).stdout.strip()
if update:
    json.dump({"machine": machine, "rates": now}, open(base_path, "w"), indent=2)
    print("baseline updated:", base_path)
    sys.exit(0)
base = json.load(open(base_path)) if os.path.exists(base_path) else None
bad = 0
print(f"{'rate':>6} {'req/s':>10} {'cpu%':>12} {'lag p99':>16} {'dropped':>8}  verdict")
for rate, n in now.items():
    b = base["rates"].get(rate) if base else None
    problems = []
    if n["rps"] < int(rate) * 0.98: problems.append("rate missed")
    if n["dropped"]: problems.append("dropped")
    if b:
        if n["cpu"] > b["cpu"] * 1.5 and n["cpu"] > 10: problems.append("cpu")
        if n["lagP99us"] > b["lagP99us"] * 3 and n["lagP99us"] > 1000: problems.append("lag")
    cpu = f"{n['cpu']}" + (f" ({b['cpu']})" if b else "")
    lag = f"{n['lagP99us']}µs" + (f" ({b['lagP99us']})" if b else "")
    print(f"{rate:>6} {n['rps']:>10} {cpu:>12} {lag:>16} {n['dropped']:>8}  {'REGRESSION: ' + ', '.join(problems) if problems else 'ok'}")
    bad += bool(problems)
if base and base.get("machine") != machine:
    print(f"note: baseline is from '{base.get('machine')}', this is '{machine}' — compare with care")
if not base:
    print("no baseline yet: run with --update to store one")
sys.exit(1 if bad else 0)
EOF
