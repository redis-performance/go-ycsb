#!/usr/bin/env bash
#
# Integration test for measurement.interval_output_file against a real,
# dockerized Redis: a run with a 1s interval, stopped after RUN_SECONDS by
# timeout -s INT the way timed benchmarks end, must write one JSON line per
# operation per interval: the intervals tile the run from t=0, each record is
# self-consistent (running cum_count, ops = count/window_s, ordered latencies),
# and the counts add up to the final summary, including the last, partial
# interval. The checks tolerate a slow runner (late start, stalls, a long final
# drain, a wall-clock step), not malformed records.
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
timeout -k 5 120 "$WORK/go-ycsb" load redis "${common[@]}" -p threadcount=8 >/dev/null

echo "==> an interval below the 100ms minimum is rejected"
if timeout -k 5 60 "$WORK/go-ycsb" run redis "${common[@]}" -p operationcount=1 -p measurement.interval=50ms >"$WORK/bad.log" 2>&1; then
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
timeout -k 30 -s INT "$RUN_SECONDS" "$WORK/go-ycsb" run redis "${common[@]}" -p operationcount=1000000000 -p threadcount=16 \
  -p measurement.interval=1s -p measurement.interval_output_file="$WORK/intervals.jsonl" >"$WORK/run.log" 2>&1 || rc=$?
if [ "$rc" != 124 ]; then
  echo "FAIL: expected the run to be stopped by timeout (rc 124), got rc $rc"
  cat "$WORK/run.log"
  exit 1
fi

python3 - "$WORK/intervals.jsonl" "$WORK/run.log" <<'EOF'
import json, re, statistics, sys, time
from datetime import datetime, timezone

path, log = sys.argv[1], sys.argv[2]
recs = [json.loads(line) for line in open(path)]
text = open(log, errors="replace").read()
# the final summary: operation -> (Takes(s), Count)
final = {m.group(1): (float(m.group(2)), int(m.group(3))) for m in re.finditer(r"^(\w+)\s+- Takes\(s\): ([\d.]+), Count: (\d+)", text[text.find("Run finished"):], re.M)}
THREADS = 16  # the run's threadcount

def ts_seconds(ts):
    # RFC 3339 with up to nanoseconds (Go trims trailing zeros); datetime takes 6 digits at most
    whole, _, frac = ts.rstrip("Z").partition(".")
    sec = datetime.strptime(whole, "%Y-%m-%dT%H:%M:%S").replace(tzinfo=timezone.utc).timestamp()
    return sec + (float("0." + frac) if frac else 0.0)

fail = []
by = {}
for r in recs:
    by.setdefault(r["op"], []).append(r)
# ops cut short by the SIGINT can show up as *_ERROR
if not {"READ", "UPDATE", "TOTAL"} <= set(by) or any(op not in {"READ", "UPDATE", "TOTAL"} and not op.endswith("_ERROR") for op in by):
    fail.append(f"ops {sorted(by)}")
if "Run finished" not in text:
    fail.append("no final summary: the run didn't end cleanly on SIGINT")

# One cut per ts: every record of it carries the same t and window_s.
cuts = {}
for r in recs:
    c = cuts.setdefault(r["ts"], (r["t"], r["window_s"]))
    if (r["t"], r["window_s"]) != c:
        fail.append(f"records at ts {r['ts']} disagree: t/window_s {c} vs {(r['t'], r['window_s'])} ({r['op']})")
cut = sorted((t, w, ts) for ts, (t, w) in cuts.items())
ts_s = [ts_seconds(ts) for _, _, ts in cut]
# The cuts tile the run from its start: the first ends one window after t=0,
# and each starts where the previous ended (no gap, no overlap).
if cut and abs(cut[0][0] - cut[0][1]) > 0.25:
    fail.append(f"first interval ends at t={cut[0][0]} after a {cut[0][1]}s window: t doesn't start at 0")
for (t0, _, _), (t1, w1, ts1) in zip(cut, cut[1:]):
    if abs((t1 - w1) - t0) > 1e-3:
        fail.append(f"interval {ts1} starts at t={t1 - w1:.3f}s, the previous ended at t={t0:.3f}s")
# ts is the wall clock at each cut: increasing, today, and advancing with t
# (loosely: the wall clock can step, t is monotonic).
if any(b <= a for a, b in zip(ts_s, ts_s[1:])):
    fail.append("ts isn't increasing with t")
if any(abs(s - time.time()) > 86400 for s in ts_s):
    fail.append(f"ts {cut[0][2]} .. {cut[-1][2]} isn't within a day of now")
if any(abs((ts_s[i] - ts_s[0]) - (cut[i][0] - cut[0][0])) > 0.5 for i in range(len(cut))):
    fail.append("ts doesn't advance with t")
# 1s windows, checked loosely enough for a busy CI runner: a stall stretches one
# window and shortens the next (the ticker keeps its schedule) or drops a tick,
# so allow two stalls' worth of odd windows (or 1 in 5), none longer than 10 s; the median
# stays at 1 s when there are enough windows to tell. The last window runs
# until the final drain, so it is only bounded.
ws = [w for _, w, _ in cut]
odd = [w for w in ws[:-1] if not 0.5 <= w <= 1.5]
if len(odd) > max(4, len(ws) // 5) or any(not 0 < w <= 10 for w in ws):
    fail.append(f"windows {[round(w, 3) for w in ws]}")
if len(ws) - 1 >= 5 and not 0.95 <= statistics.median(ws[:-1]) <= 1.05:
    fail.append(f"median window {statistics.median(ws[:-1]):.3f}s, want 1s: {[round(w, 3) for w in ws]}")

order = ["min_us", "p50_us", "p90_us", "p95_us", "p99_us", "p999_us", "p9999_us", "max_us"]
seq = [ts for _, _, ts in cut]
for op, rs in by.items():
    # From its first sample on, an operation has a record at every cut.
    first = seq.index(rs[0]["ts"]) if rs[0]["ts"] in seq else -1
    if [r["ts"] for r in rs] != seq[first:]:
        fail.append(f"{op}: records skip intervals (it first appears at {rs[0]['ts']})")
    cum = 0
    for r in rs:
        cum += r["count"]
        if r["count"] < 0 or r["cum_count"] != cum:
            fail.append(f"{op}: count {r['count']}, cum_count {r['cum_count']}, want the running sum {cum} at {r['ts']}")
        if r["window_s"] > 0 and abs(r["ops"] - r["count"] / r["window_s"]) > 1e-6 * max(1.0, r["ops"]):
            fail.append(f"{op}: ops {r['ops']} isn't count/window_s at {r['ts']}")
        if r["count"] > 0:
            missing = [k for k in order + ["avg_us"] if k not in r]
            if missing:
                fail.append(f"{op}: {r['count']} samples at {r['ts']} but no {', '.join(missing)}")
                continue
            vals = [r[k] for k in order]
            if vals != sorted(vals) or not r["min_us"] <= r["avg_us"] <= r["max_us"]:
                fail.append(f"{op}: latencies out of order at {r['ts']}: {dict((k, r[k]) for k in order + ['avg_us'])}")
    # Summed against what the run recorded, not against RUN_SECONDS: a slow
    # runner may start late or stop late, and that must not fail the test.
    takes, count = final.get(op, (None, None))
    if count is not None and cum != count:
        fail.append(f"{op}: intervals sum to {cum}, the final summary says {count}")
    elif count is None and not op.endswith("_ERROR"):
        fail.append(f"{op}: not in the final summary")
    # ... and cover the operation's measured duration (Takes(s), timed from its
    # first sample) to within one window
    span = rs[-1]["t"] - (rs[0]["t"] - rs[0]["window_s"])
    if takes is not None and abs(span - takes) > max(1.0, max(ws)):
        fail.append(f"{op}: windows span {span:.3f}s, the final summary says Takes(s) {takes}")
# TOTAL counts the successful operations. Per interval it matches their sum up
# to the operation/TOTAL pairs a cut splits: they are recorded one after the
# other, so at most one pair per client thread is in flight. Over the run, exactly.
for t, _, ts in cut:
    n_ops = sum(r["count"] for r in recs if r["ts"] == ts and r["op"] not in ("TOTAL",) and not r["op"].endswith("_ERROR"))
    n_total = sum(r["count"] for r in recs if r["ts"] == ts and r["op"] == "TOTAL")
    if abs(n_ops - n_total) > THREADS:
        fail.append(f"interval {ts}: TOTAL {n_total}, operations {n_ops}")
n_ops = sum(r["count"] for r in recs if r["op"] != "TOTAL" and not r["op"].endswith("_ERROR"))
if n_ops != sum(r["count"] for r in by.get("TOTAL", [])):
    fail.append("TOTAL doesn't add up to the operations over the run")
if fail:
    print("FAIL:\n  " + "\n  ".join(fail))
    sys.exit(1)
print(f"ok: {len(recs)} interval records in {len(cut)} intervals, {', '.join(f'{op} {len(rs)}' for op, rs in sorted(by.items()))}")
EOF
