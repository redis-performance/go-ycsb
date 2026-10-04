#!/usr/bin/env bash
#
# Integration test for measurement.interval_output_file against a real,
# dockerized Redis: a run with a 1s interval, stopped with SIGINT the way timed
# benchmarks end, must write one JSON line per operation per interval whose
# counts add up to the final summary, including the last, partial interval.
#
# Same script for local dev and CI: by default it starts (and tears down) its own
# disposable Redis container. Set START_CONTAINERS=false and REDIS_ADDR to reuse
# an existing Redis.
#
# Usage:
#   test/integration/interval_output.sh
#
# Env overrides:
#   RUN_SECONDS      length of the run before SIGINT (default: 6)
#   REDIS_IMAGE      docker image for Redis (default: redis:8)
#   REDIS_PORT       host port to publish Redis on (default: a free port docker picks)
#   START_CONTAINERS whether to start/stop the container (default: true)
#   REDIS_ADDR       redis.addr to use when START_CONTAINERS=false

set -euo pipefail

RUN_SECONDS=${RUN_SECONDS:-6}
REDIS_IMAGE=${REDIS_IMAGE:-redis:8}
REDIS_PORT=${REDIS_PORT:-}
START_CONTAINERS=${START_CONTAINERS:-true}

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$ROOT_DIR"

WORK=$(mktemp -d)
# unique per run, so concurrent runs (two people, two CI jobs) don't collide
REDIS_CONTAINER=go-ycsb-it-interval-redis-$(basename "$WORK")

cleanup() {
  rm -rf "$WORK"
  if [ "$START_CONTAINERS" = "true" ]; then
    docker rm -f "$REDIS_CONTAINER" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

if [ "$START_CONTAINERS" = "true" ]; then
  echo "==> starting $REDIS_IMAGE"
  docker run -d --rm --name "$REDIS_CONTAINER" -p "127.0.0.1:${REDIS_PORT}:6379" "$REDIS_IMAGE" >/dev/null
  REDIS_PORT=$(docker port "$REDIS_CONTAINER" 6379/tcp | head -n 1 | sed 's/.*://')
  echo "    on 127.0.0.1:$REDIS_PORT"
  for _ in $(seq 1 30); do
    docker exec "$REDIS_CONTAINER" redis-cli ping >/dev/null 2>&1 && break
    sleep 1
  done
fi
REDIS_ADDR=${REDIS_ADDR:-127.0.0.1:${REDIS_PORT:-16380}}

echo "==> building go-ycsb"
go build -o "$WORK/go-ycsb" ./cmd/go-ycsb

common=(-P workloads/workloada -p "redis.addr=${REDIS_ADDR}" -p recordcount=10000)
"$WORK/go-ycsb" load redis "${common[@]}" -p threadcount=8 >/dev/null

echo "==> an interval below the 100ms minimum is rejected"
if "$WORK/go-ycsb" run redis "${common[@]}" -p operationcount=1 -p measurement.interval=50ms >"$WORK/bad.log" 2>&1; then
  echo "FAIL: measurement.interval=50ms was accepted"
  exit 1
fi
if ! grep -q "the minimum is 100ms" "$WORK/bad.log"; then
  echo "FAIL: measurement.interval=50ms failed, but not with the minimum-interval error:"
  cat "$WORK/bad.log"
  exit 1
fi

echo "==> the same SIGINT delivered twice still ends with the final summary"
# Back-to-back, as timeout(1) delivers it. The old handler lost the summary in
# about 9 of 10 such runs, so 3 tries catch a regression all but certainly.
for try in 1 2 3; do
  "$WORK/go-ycsb" run redis "${common[@]}" -p operationcount=1000000000 -p threadcount=8 >"$WORK/dup.log" 2>&1 &
  pid=$!
  sleep 2
  kill -INT "$pid" 2>/dev/null || true
  kill -INT "$pid" 2>/dev/null || true
  rc=0
  wait "$pid" || rc=$?
  if ! grep -q "Run finished" "$WORK/dup.log" || grep -q "again to exit" "$WORK/dup.log"; then
    echo "FAIL: a SIGINT delivered twice lost the final summary (try $try, rc $rc)"
    cat "$WORK/dup.log"
    exit 1
  fi
done

echo "==> run with a 1s interval, SIGINT after ${RUN_SECONDS}s"
rc=0
timeout -s INT "$RUN_SECONDS" "$WORK/go-ycsb" run redis "${common[@]}" -p operationcount=1000000000 -p threadcount=16 \
  -p measurement.interval=1s -p measurement.interval_output_file="$WORK/intervals.jsonl" >"$WORK/run.log" 2>&1 || rc=$?
if [ "$rc" != 124 ]; then
  echo "FAIL: expected the run to be stopped by timeout (rc 124), got rc $rc"
  cat "$WORK/run.log"
  exit 1
fi

python3 - "$WORK/intervals.jsonl" "$WORK/run.log" "$RUN_SECONDS" <<'EOF'
import json, re, sys

path, log, seconds = sys.argv[1], sys.argv[2], int(sys.argv[3])
recs = [json.loads(line) for line in open(path)]
text = open(log, errors="replace").read()
final = {m.group(1): int(m.group(2)) for m in re.finditer(r"^(\w+)\s+- Takes\(s\): [\d.]+, Count: (\d+)", text[text.find("Run finished"):], re.M)}
by = {}
for r in recs:
    by.setdefault(r["op"], []).append(r)
fail = []
# ops cut short by the SIGINT can show up as *_ERROR in the last interval
if not {"READ", "UPDATE", "TOTAL"} <= set(by) or any(op not in {"READ", "UPDATE", "TOTAL"} and not op.endswith("_ERROR") for op in by):
    fail.append(f"ops {sorted(by)}")
if "Run finished" not in open(log, errors="replace").read():
    fail.append("no final summary: the run didn't end cleanly on SIGINT")
for op, rs in by.items():
    if op.endswith("_ERROR"):
        continue
    n = len(rs)
    if not seconds - 1 <= n <= seconds + 1:
        fail.append(f"{op}: {n} intervals in a {seconds}s run")
    total = sum(r["count"] for r in rs)
    if total != rs[-1]["cum_count"] or total != final.get(op):
        fail.append(f"{op}: intervals sum to {total}, last cum_count {rs[-1]['cum_count']}, final summary {final.get(op)}")
    if any(b["t"] <= a["t"] for a, b in zip(rs, rs[1:])):
        fail.append(f"{op}: t isn't increasing")
    if not all(0.9 <= r["window_s"] <= 1.1 for r in rs[:-1]) or not 0 < rs[-1]["window_s"] <= 1.1:
        fail.append(f"{op}: windows {[round(r['window_s'], 3) for r in rs]}")
    for r in rs:
        if r["count"] > 0 and not r["min_us"] <= r["p50_us"] <= r["p99_us"] <= r["max_us"]:
            fail.append(f"{op}: percentiles out of order in {r}")
# TOTAL counts the successful operations: per interval it matches their sum up to
# the operation/TOTAL pairs a cut splits (at most one per client thread, 16),
# and exactly over the run.
threads = 16
windows = {}
for r in recs:
    w = windows.setdefault(r["ts"], {"ops": 0, "total": 0})
    if r["op"] == "TOTAL":
        w["total"] += r["count"]
    elif not r["op"].endswith("_ERROR"):
        w["ops"] += r["count"]
for ts, w in windows.items():
    if abs(w["ops"] - w["total"]) > threads:
        fail.append(f"interval {ts}: TOTAL {w['total']}, operations {w['ops']}")
if sum(w["ops"] for w in windows.values()) != sum(w["total"] for w in windows.values()):
    fail.append("TOTAL doesn't add up to the operations over the run")
if fail:
    print("FAIL:\n  " + "\n  ".join(fail))
    sys.exit(1)
print(f"ok: {len(recs)} interval records, {', '.join(f'{op} {len(rs)}' for op, rs in sorted(by.items()))}")
EOF
