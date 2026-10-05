#!/usr/bin/env bash
#
# Integration test for the stop-signal handling against a real, dockerized
# Redis: a run stopped with SIGINT must end with its final summary
# ("Run finished"), also when the same SIGINT arrives twice back to back, which
# is how GNU timeout(1) delivers it (to the command, then to its process group).
#
# Same script for local dev and CI: by default it starts (and tears down) its own
# disposable Redis container. Set START_CONTAINERS=false and REDIS_ADDR to reuse
# an existing Redis.
#
# Usage:
#   test/integration/stop_signal.sh
#
# Env overrides:
#   TRIES_BACK_TO_BACK runs of the back-to-back case (default: 20)
#   TRIES            runs of the timeout -s INT case (default: 10)
#   REDIS_IMAGE      docker image for Redis (default: redis:8)
#   REDIS_PORT       host port to publish Redis on (default: a free port docker picks)
#   START_CONTAINERS whether to start/stop the container (default: true)
#   REDIS_ADDR       redis.addr to use when START_CONTAINERS=false

set -euo pipefail

TRIES_BACK_TO_BACK=${TRIES_BACK_TO_BACK:-20}
TRIES=${TRIES:-10}
# Upper bound on one stopped run: the stop handler force-exits after 10 s, so
# anything longer is a hang, which fails the try instead of stalling the job.
RUN_LIMIT=30
REDIS_IMAGE=${REDIS_IMAGE:-redis:8}
REDIS_PORT=${REDIS_PORT:-}
START_CONTAINERS=${START_CONTAINERS:-true}

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$ROOT_DIR"

WORK=$(mktemp -d)
# unique per run, so concurrent runs (two people, two CI jobs) don't collide
REDIS_CONTAINER=go-ycsb-it-signal-redis-$(basename "$WORK")

pid=
cleanup() {
  # a run still in the background (the script itself was killed) goes too
  if [ -n "$pid" ]; then kill -KILL "$pid" 2>/dev/null || true; fi
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
  ready=0
  for _ in $(seq 1 30); do
    docker exec "$REDIS_CONTAINER" redis-cli ping >/dev/null 2>&1 && { ready=1; break; }
    sleep 1
  done
  if [ "$ready" != 1 ]; then
    echo "FAIL: Redis did not become ready in 30 s" >&2
    exit 1
  fi
fi
REDIS_ADDR=${REDIS_ADDR:-127.0.0.1:${REDIS_PORT:-16380}}

echo "==> building go-ycsb"
go build -o "$WORK/go-ycsb" ./cmd/go-ycsb

common=(-P workloads/workloada -p "redis.addr=${REDIS_ADDR}" -p recordcount=10000)
timeout -k 5 120 "$WORK/go-ycsb" load redis "${common[@]}" -p threadcount=8 >/dev/null

check() { # $1: log, $2: case, $3: try, $4: rc
  if ! grep -q "Run finished" "$1" || grep -q "again to exit" "$1"; then
    echo "FAIL: $2 lost the final summary (try $3, rc $4)"
    cat "$1"
    exit 1
  fi
}

echo "==> the same SIGINT delivered twice, back to back, still ends with the final summary"
# The old handler lost the summary in about 1 of 5 such runs, and in about 2
# of 5 runs stopped by timeout -s INT (below), measured on a laptop. So each
# case on its own misses a regression that only breaks it in about
# 0.8^20 = 1.2% (20 tries) and 0.6^10 = 0.6% (10 tries) of runs.
for try in $(seq 1 "$TRIES_BACK_TO_BACK"); do
  "$WORK/go-ycsb" run redis "${common[@]}" -p operationcount=1000000000 -p threadcount=8 >"$WORK/dup.log" 2>&1 &
  pid=$!
  sleep 2
  kill -INT "$pid" 2>/dev/null || true
  kill -INT "$pid" 2>/dev/null || true
  # bound the wait: a hung run is killed, and then fails the check below.
  # Killing the watchdog also stops its sleep (the TERM trap), and its output
  # goes nowhere, so nothing left over holds the script's stdout open.
  (
    sleep "$RUN_LIMIT" &
    s=$!
    trap 'kill "$s" 2>/dev/null; exit 0' TERM
    wait "$s"
    kill -KILL "$pid" 2>/dev/null
  ) >/dev/null 2>&1 &
  watchdog=$!
  rc=0
  wait "$pid" || rc=$?
  kill "$watchdog" 2>/dev/null || true
  wait "$watchdog" 2>/dev/null || true
  pid=
  if [ "$rc" = 137 ]; then
    echo "FAIL: a SIGINT delivered twice: hung: watchdog killed go-ycsb after ${RUN_LIMIT} s (try $try)"
    cat "$WORK/dup.log"
    exit 1
  fi
  check "$WORK/dup.log" "a SIGINT delivered twice" "$try" "$rc"
done

echo "==> a run bounded by timeout -s INT ends with the final summary"
for try in $(seq 1 "$TRIES"); do
  rc=0
  timeout -k "$RUN_LIMIT" -s INT 3 "$WORK/go-ycsb" run redis "${common[@]}" -p operationcount=1000000000 -p threadcount=8 >"$WORK/timeout.log" 2>&1 || rc=$?
  if [ "$rc" = 137 ]; then
    echo "FAIL: a run stopped by timeout -s INT: hung: timeout killed go-ycsb ${RUN_LIMIT} s after the SIGINT (try $try)"
    cat "$WORK/timeout.log"
    exit 1
  fi
  if [ "$rc" != 124 ]; then
    echo "FAIL: expected the run to be stopped by timeout (rc 124), got rc $rc (try $try)"
    cat "$WORK/timeout.log"
    exit 1
  fi
  check "$WORK/timeout.log" "a run stopped by timeout -s INT" "$try" "$rc"
done

echo "ok: $TRIES_BACK_TO_BACK back-to-back and $TRIES timeout runs ended with the final summary"
