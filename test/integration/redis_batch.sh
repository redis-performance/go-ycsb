#!/usr/bin/env bash
#
# Integration test for the redis adapter's batched load (batch.size) against a
# real, dockerized single Redis and a 6-node Redis Cluster (3 masters, 3
# replicas): db/redis's TestRedisBatchLoad loads the feature-store workload
# with batch.size 1, 7 and 100 and checks DBSIZE, the key set and every value
# against batch.size=1; then a batched load's interval output is checked
# (check_interval_output.py, Python 3.10+).
#
# Same script for local dev and CI: it starts (and tears down) its own
# disposable containers, under names unique to the run (redis_lib.sh). Without
# docker or python3 it skips, or in CI ($CI set) fails.
#
# Usage:
#   test/integration/redis_batch.sh
#
# Env overrides:
#   REDIS_IMAGE      docker image for Redis (default: redis:8)

set -euo pipefail

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

redis_start_single "go-ycsb-it-batch-single-$SUFFIX"
redis_start_cluster "go-ycsb-it-batch-cluster-$SUFFIX" 3 1

echo "==> building go-ycsb"
make >/dev/null

export GO_YCSB_BIN="$ROOT_DIR/bin/go-ycsb"
export REDIS_BATCH_IT_SINGLE="127.0.0.1:$SINGLE_PORT"
export REDIS_BATCH_IT_CLUSTER="$CLUSTER_ADDR"

echo "==> batched loads vs. batch.size=1 (single $REDIS_BATCH_IT_SINGLE, cluster $REDIS_BATCH_IT_CLUSTER)"
go test -count=1 -v -run 'TestRedisBatchLoad' ./db/redis/

# A batch's records are one sample each for INSERT and TOTAL, and BATCH_INSERT
# none for TOTAL: the interval checker's TOTAL bookkeeping must hold.
echo "==> interval output of a batched load"
LAUNCH=$(python3 -c 'import time; print(time.time())')
./bin/go-ycsb load redis -P workloads/workload_feature_store -p redis.addr="$REDIS_BATCH_IT_SINGLE" \
  -p recordcount=200000 -p threadcount=16 -p batch.size=100 -p dropdata=true -p silence=true \
  -p measurement.interval=200ms -p measurement.interval_output_file="$WORK/intervals.jsonl" >"$WORK/load.log" 2>&1
python3 test/integration/check_interval_output.py "$WORK/intervals.jsonl" "$WORK/load.log" 16 "$LAUNCH" 0.2 100 INSERT,BATCH_INSERT,TOTAL

echo "==> redis batch integration test passed"
