#!/usr/bin/env bash
#
# Integration test for measurement.interval_output_file against a real,
# dockerized Redis: a run with a 1s interval, stopped after RUN_SECONDS by
# timeout -s INT the way timed benchmarks end, must write one JSON line per
# operation per interval: the intervals tile the run from t=0, each record is
# self-consistent (running cum_count, ops = count/window_s, ordered latencies),
# and the counts add up to the final summary, including the last, partial
# interval. The checks (check_interval_output.py) tolerate a slow runner (late
# start, stalls, a long final drain, a wall-clock step), not malformed records.
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
THREADS=16
rc=0
LAUNCH=$(date +%s.%N) # the checker needs it to tell where ts is taken
timeout -k 30 -s INT "$RUN_SECONDS" "$WORK/go-ycsb" run redis "${common[@]}" -p operationcount=1000000000 -p threadcount="$THREADS" \
  -p measurement.interval=1s -p measurement.interval_output_file="$WORK/intervals.jsonl" >"$WORK/run.log" 2>&1 || rc=$?
if [ "$rc" != 124 ]; then
  echo "FAIL: expected the run to be stopped by timeout (rc 124), got rc $rc"
  cat "$WORK/run.log"
  exit 1
fi

python3 test/integration/check_interval_output.py "$WORK/intervals.jsonl" "$WORK/run.log" "$THREADS" "$LAUNCH" 1
