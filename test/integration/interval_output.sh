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
#   RUN_SECONDS      length of the run before SIGINT (default: 6); the checks
#                    don't depend on it, only on what the run itself recorded
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

echo "==> run with a 1s interval, SIGINT after ${RUN_SECONDS}s"
rc=0
timeout -s INT "$RUN_SECONDS" "$WORK/go-ycsb" run redis "${common[@]}" -p operationcount=1000000000 -p threadcount=16 \
  -p measurement.interval=1s -p measurement.interval_output_file="$WORK/intervals.jsonl" >"$WORK/run.log" 2>&1 || rc=$?
if [ "$rc" != 124 ]; then
  echo "FAIL: expected the run to be stopped by timeout (rc 124), got rc $rc"
  cat "$WORK/run.log"
  exit 1
fi

python3 - "$WORK/intervals.jsonl" "$WORK/run.log" <<'EOF'
import json, re, statistics, sys

path, log = sys.argv[1], sys.argv[2]
recs = [json.loads(line) for line in open(path)]
text = open(log, errors="replace").read()
# the final summary: operation -> (Takes(s), Count)
final = {m.group(1): (float(m.group(2)), int(m.group(3))) for m in re.finditer(r"^(\w+)\s+- Takes\(s\): ([\d.]+), Count: (\d+)", text[text.find("Run finished"):], re.M)}
by = {}
for r in recs:
    by.setdefault(r["op"], []).append(r)
fail = []
# ops cut short by the SIGINT can show up as *_ERROR in the last interval
if not {"READ", "UPDATE", "TOTAL"} <= set(by) or any(op not in {"READ", "UPDATE", "TOTAL"} and not op.endswith("_ERROR") for op in by):
    fail.append(f"ops {sorted(by)}")
if "Run finished" not in text:
    fail.append("no final summary: the run didn't end cleanly on SIGINT")
for op, rs in by.items():
    if op.endswith("_ERROR"):
        continue
    # Checked against what the run recorded, not against RUN_SECONDS: a slow
    # runner may start late or stop late, and that must not fail the test.
    takes, count = final.get(op, (None, None))
    total = sum(r["count"] for r in rs)
    if total != rs[-1]["cum_count"] or total != count:
        fail.append(f"{op}: intervals sum to {total}, last cum_count {rs[-1]['cum_count']}, final summary {count}")
    if any(b["t"] <= a["t"] for a, b in zip(rs, rs[1:])):
        fail.append(f"{op}: t isn't increasing")
    # the windows tile the run: back to back from the start, no gap, no overlap
    span = sum(r["window_s"] for r in rs)
    if abs(span - rs[-1]["t"]) > 1e-3:
        fail.append(f"{op}: windows sum to {span:.3f}s, but the last ends at t={rs[-1]['t']:.3f}s")
    # ... and cover the run's own measured duration (the final summary's
    # Takes(s), timed from the operation's first sample) to within one window
    if takes is None or abs(span - takes) > 1.0:
        fail.append(f"{op}: windows sum to {span:.3f}s, the final summary says Takes(s) {takes}")
    # 1s windows; a late tick on a busy runner stretches one, so bound each loosely
    # and the typical one tightly
    ws = [r["window_s"] for r in rs]
    if not all(0.5 <= w <= 1.5 for w in ws[:-1]) or not 0 < ws[-1] <= 1.5 or (len(ws) > 1 and not 0.95 <= statistics.median(ws[:-1]) <= 1.05):
        fail.append(f"{op}: windows {[round(w, 3) for w in ws]}")
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
