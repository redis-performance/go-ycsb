#!/usr/bin/env bash
#
# Integration test for the feature-store workload (workloads/workload_feature_store)
# against real, dockerized Redis and MongoDB. Loads and runs the workload against
# both databases and fails if either reports errors or an unexpected op count.
#
# Same script for local dev and CI: by default it starts (and tears down) its own
# disposable Redis/Mongo containers, so `test/integration/feature_store.sh` and the
# CI job run identically. Point REDIS_ADDR/MONGODB_URL at already-running instances
# and set START_CONTAINERS=false to reuse existing databases instead.
#
# Usage:
#   test/integration/feature_store.sh
#
# Env overrides:
#   RECORDCOUNT      number of entities to load (default: 100000)
#   OPERATIONCOUNT   number of ops in the run phase (default: 3x RECORDCOUNT)
#   THREADCOUNT      client concurrency (default: 16)
#   REDIS_IMAGE      docker image for Redis (default: redis:8)
#   MONGO_IMAGE      docker image for MongoDB (default: mongo:7)
#   REDIS_PORT       host port to publish Redis on (default: 16379)
#   MONGO_PORT       host port to publish MongoDB on (default: 27118)
#   START_CONTAINERS whether to start/stop the containers (default: true)
#   REDIS_ADDR       redis.addr to use when START_CONTAINERS=false
#   MONGODB_URL      mongodb.url to use when START_CONTAINERS=false

set -euo pipefail

RECORDCOUNT=${RECORDCOUNT:-100000}
OPERATIONCOUNT=${OPERATIONCOUNT:-$((RECORDCOUNT * 3))}
THREADCOUNT=${THREADCOUNT:-16}
REDIS_IMAGE=${REDIS_IMAGE:-redis:8}
MONGO_IMAGE=${MONGO_IMAGE:-mongo:7}
REDIS_PORT=${REDIS_PORT:-16379}
MONGO_PORT=${MONGO_PORT:-27118}
START_CONTAINERS=${START_CONTAINERS:-true}
REDIS_ADDR=${REDIS_ADDR:-127.0.0.1:${REDIS_PORT}}
MONGODB_URL=${MONGODB_URL:-mongodb://127.0.0.1:${MONGO_PORT}/ycsb_it?w=1}

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$ROOT_DIR"

REDIS_CONTAINER=go-ycsb-it-redis
MONGO_CONTAINER=go-ycsb-it-mongo

cleanup() {
  if [ "$START_CONTAINERS" = "true" ]; then
    echo "==> stopping containers"
    docker rm -f "$REDIS_CONTAINER" "$MONGO_CONTAINER" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

if [ "$START_CONTAINERS" = "true" ]; then
  echo "==> starting $REDIS_IMAGE on :$REDIS_PORT and $MONGO_IMAGE on :$MONGO_PORT"
  docker rm -f "$REDIS_CONTAINER" "$MONGO_CONTAINER" >/dev/null 2>&1 || true
  docker run -d --rm --name "$REDIS_CONTAINER" -p "${REDIS_PORT}:6379" "$REDIS_IMAGE" >/dev/null
  docker run -d --rm --name "$MONGO_CONTAINER" -p "${MONGO_PORT}:27017" "$MONGO_IMAGE" >/dev/null

  echo "==> waiting for redis"
  for _ in $(seq 1 60); do
    if docker exec "$REDIS_CONTAINER" redis-cli ping 2>/dev/null | grep -q PONG; then
      break
    fi
    sleep 1
  done

  echo "==> waiting for mongodb"
  for _ in $(seq 1 60); do
    if docker exec "$MONGO_CONTAINER" mongosh --quiet --eval 'db.runCommand({ping:1})' >/dev/null 2>&1; then
      break
    fi
    sleep 1
  done
fi

echo "==> building go-ycsb"
make >/dev/null

run_phase() {
  local db=$1 phase=$2
  shift 2
  ./bin/go-ycsb "$phase" "$db" -P workloads/workload_feature_store \
    -p recordcount="$RECORDCOUNT" -p operationcount="$OPERATIONCOUNT" -p threadcount="$THREADCOUNT" \
    "$@"
}

# Fails if the phase's own output reports any *_ERROR ops, or if the given
# summary line's reported Count doesn't match the expected op count.
check_output() {
  local out=$1 line_prefix=$2 expect_count=$3

  if echo "$out" | grep -q '_ERROR'; then
    echo "FAIL: error operations present in output:"
    echo "$out" | grep '_ERROR'
    exit 1
  fi

  # go-ycsb also prints interim progress lines with the same op-name prefix
  # while a run is in flight; only the LAST match is the final summary.
  # The trailing `|| true` matters under `set -euo pipefail`: without it, a
  # pipeline that finds no match (e.g. the op never appeared in output at
  # all) exits non-zero and set -e would kill the script right here, before
  # the "no summary line found" diagnostic below ever runs.
  local got
  got=$(echo "$out" | grep -E "^${line_prefix}[[:space:]]" | tail -n 1 | grep -oE 'Count: [0-9]+' | grep -oE '[0-9]+' || true)
  if [ -z "$got" ]; then
    echo "FAIL: no '${line_prefix}' summary line found in output"
    echo "$out"
    exit 1
  fi
  if [ "$got" != "$expect_count" ]; then
    echo "FAIL: expected ${line_prefix} Count=${expect_count}, got ${got}"
    exit 1
  fi
  echo "OK: ${line_prefix} Count=${got}, no errors"
}

echo "==> [redis] load ($RECORDCOUNT entities)"
OUT=$(run_phase redis load -p redis.addr="$REDIS_ADDR" -p dropdata=true)
echo "$OUT" | tail -5
check_output "$OUT" INSERT "$RECORDCOUNT"

echo "==> [redis] run ($OPERATIONCOUNT ops)"
OUT=$(run_phase redis run -p redis.addr="$REDIS_ADDR")
echo "$OUT" | tail -5
check_output "$OUT" TOTAL "$OPERATIONCOUNT"

echo "==> [mongodb] load ($RECORDCOUNT entities)"
OUT=$(run_phase mongodb load -p mongodb.url="$MONGODB_URL")
echo "$OUT" | tail -5
check_output "$OUT" INSERT "$RECORDCOUNT"

echo "==> [mongodb] run ($OPERATIONCOUNT ops)"
OUT=$(run_phase mongodb run -p mongodb.url="$MONGODB_URL")
echo "$OUT" | tail -5
check_output "$OUT" TOTAL "$OPERATIONCOUNT"

echo "==> feature-store integration test passed"
