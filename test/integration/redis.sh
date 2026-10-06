#!/usr/bin/env bash
#
# Integration test for the redis adapter against a real, dockerized single
# Redis and a 3-master Redis Cluster: a short load and run of the
# feature-store workload on each, with the adapter's defaults. After the
# load, INSERT must equal DBSIZE (summed over the masters) and the load's
# count; the run must report no errors. On the cluster it also checks the
# defaults that keep go-redis v9.8.0's behaviour on the wire: no COMMAND
# lookups and no CLIENT MAINT_NOTIFICATIONS reach the nodes, and the nodes
# replied no errors.
#
# Same script for local dev and CI: it starts (and tears down) its own
# disposable containers, under names unique to the run. Without docker or
# python3 it skips, or in CI ($CI set) fails.
#
# Usage:
#   test/integration/redis.sh
#
# Env overrides:
#   RECORDCOUNT      records to load (default: 20000)
#   OPERATIONCOUNT   operations in the run phase (default: 20000)
#   THREADCOUNT      client concurrency (default: 16)
#   REDIS_IMAGE      docker image for Redis (default: redis:8)

set -euo pipefail

RECORDCOUNT=${RECORDCOUNT:-20000}
OPERATIONCOUNT=${OPERATIONCOUNT:-20000}
THREADCOUNT=${THREADCOUNT:-16}
REDIS_IMAGE=${REDIS_IMAGE:-redis:8}

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$ROOT_DIR"
# shellcheck source=test/integration/redis_lib.sh
. test/integration/redis_lib.sh

redis_require_tools

WORK=$(mktemp -d)
# unique per run, so concurrent runs (two people, two CI jobs) don't collide
SUFFIX=$(basename "$WORK" | tr -c 'a-zA-Z0-9\n' '-')
cleanup() {
  redis_cleanup
  rm -rf "$WORK"
}
trap cleanup EXIT

SINGLE=go-ycsb-it-redis-single-$SUFFIX
CLUSTER=go-ycsb-it-redis-cluster-$SUFFIX
redis_start_single "$SINGLE"
redis_start_cluster "$CLUSTER" 3 0

echo "==> building go-ycsb"
make >/dev/null

# count <log> <op>: the final summary's Count for op (0 if none).
count() {
  sed -n '/Run finished/,$p' "$1" | grep -E "^$2 " | tail -n 1 | grep -oE 'Count: [0-9]+' | grep -oE '[0-9]+' || echo 0
}

# check_phase <log> <phase> <op> <want>: the phase ended with its summary,
# counted want ops and reported no *_ERROR.
check_phase() {
  local log=$1 phase=$2 op=$3 want=$4
  if ! grep -q 'Run finished' "$log"; then
    echo "FAIL: $phase: no final summary"
    cat "$log"
    exit 1
  fi
  if sed -n '/Run finished/,$p' "$log" | grep -qE '^[A-Z_]+_ERROR '; then
    echo "FAIL: $phase: errors:"
    sed -n '/Run finished/,$p' "$log" | grep -E '^[A-Z_]+_ERROR '
    exit 1
  fi
  local got
  got=$(count "$log" "$op")
  if [ "$got" != "$want" ]; then
    echo "FAIL: $phase: $op Count=$got, want $want"
    exit 1
  fi
  echo "OK: $phase: $op Count=$got, no errors"
}

# masters_dbsize <container> <ports...>: DBSIZE summed over the masters.
masters_dbsize() {
  local name=$1 total=0
  shift
  for p in "$@"; do
    if [ "$(redis_cli "$name" "$p" role | head -n 1)" = master ]; then
      total=$((total + $(redis_cli "$name" "$p" dbsize)))
    fi
  done
  echo "$total"
}

# run_mode <mode> <redis.addr> <container> <ports...>: the ports are the
# nodes' ports inside the container, which redis_cli talks to.
run_mode() {
  local mode=$1 addr=$2 container=$3
  shift 3
  local ports=("$@")
  for p in "${ports[@]}"; do redis_cli "$container" "$p" config resetstat >/dev/null; done
  local args=(-P workloads/workload_feature_store -p redis.mode="$mode" -p redis.addr="$addr"
    -p recordcount="$RECORDCOUNT" -p threadcount="$THREADCOUNT")

  echo "==> [$mode] load ($RECORDCOUNT records)"
  ./bin/go-ycsb load redis "${args[@]}" -p dropdata=true >"$WORK/$mode-load.log" 2>&1
  check_phase "$WORK/$mode-load.log" "$mode load" INSERT "$RECORDCOUNT"
  local inserted dbsize
  inserted=$(count "$WORK/$mode-load.log" INSERT)
  dbsize=$(masters_dbsize "$container" "${ports[@]}")
  if [ "$dbsize" != "$inserted" ]; then
    echo "FAIL: [$mode] DBSIZE $dbsize after INSERT Count=$inserted"
    exit 1
  fi
  echo "OK: [$mode] DBSIZE $dbsize = INSERT"

  echo "==> [$mode] run ($OPERATIONCOUNT operations)"
  ./bin/go-ycsb run redis "${args[@]}" -p operationcount="$OPERATIONCOUNT" >"$WORK/$mode-run.log" 2>&1
  check_phase "$WORK/$mode-run.log" "$mode run" TOTAL "$OPERATIONCOUNT"

  # the defaults that keep go-redis v9.8.0's behaviour on the wire
  for p in "${ports[@]}"; do
    local stats errors
    stats=$(redis_cli "$container" "$p" info commandstats)
    if echo "$stats" | grep -qE '^cmdstat_command(\||:)'; then
      echo "FAIL: [$mode] node $p got COMMAND lookups (redis.routing_policies is off by default)"
      exit 1
    fi
    if echo "$stats" | grep -q 'maint_notifications'; then
      echo "FAIL: [$mode] node $p got CLIENT MAINT_NOTIFICATIONS (off by default)"
      exit 1
    fi
    errors=$(redis_cli "$container" "$p" info stats | grep '^total_error_replies:' | cut -d: -f2)
    if [ "${errors:-0}" != 0 ]; then
      echo "FAIL: [$mode] node $p replied $errors errors:"
      redis_cli "$container" "$p" info errorstats
      exit 1
    fi
  done
  echo "OK: [$mode] no COMMAND, no CLIENT MAINT_NOTIFICATIONS, no error replies"
}

# the single Redis listens on 6379 in its container, on $SINGLE_PORT on the host
run_mode single "127.0.0.1:$SINGLE_PORT" "$SINGLE" 6379
# the cluster nodes listen on the same ports in the container and on the host
# shellcheck disable=SC2086 # the ports are a list
run_mode cluster "$CLUSTER_ADDR" "$CLUSTER" $CLUSTER_PORTS

echo "==> redis integration test passed"
