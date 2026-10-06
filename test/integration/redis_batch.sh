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
# disposable containers, under names unique to the run.
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

WORK=$(mktemp -d)
# unique per run, so concurrent runs (two people, two CI jobs) don't collide
SUFFIX=$(basename "$WORK" | tr -c 'a-zA-Z0-9\n' '-')
SINGLE_CONTAINER=go-ycsb-it-batch-single-$SUFFIX
CLUSTER_CONTAINER=go-ycsb-it-batch-cluster-$SUFFIX

cleanup() {
  rm -rf "$WORK"
  docker rm -f "$SINGLE_CONTAINER" "$CLUSTER_CONTAINER" >/dev/null 2>&1 || true
}
trap cleanup EXIT

wait_for() {
  local what=$1
  shift
  for _ in $(seq 1 60); do
    if "$@" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "FAIL: $what not ready" >&2
  return 1
}

echo "==> starting $REDIS_IMAGE (single)"
docker run -d --rm --name "$SINGLE_CONTAINER" -p "127.0.0.1::6379" "$REDIS_IMAGE" >/dev/null
SINGLE_PORT=$(docker port "$SINGLE_CONTAINER" 6379/tcp | head -n 1 | sed 's/.*://')
wait_for "single redis" docker exec "$SINGLE_CONTAINER" redis-cli ping

# The cluster's nodes announce 127.0.0.1 and the host ports they are published
# on, which are also the ports they listen on in the container: then the nodes
# reach each other, and the client on the host reaches every node, at the
# address the cluster gives for it. Their bus ports stay in the container.
free_ports() {
  python3 -c 'import socket
ss = [socket.socket() for _ in range(6)]
for s in ss:
    s.bind(("127.0.0.1", 0))
print(" ".join(str(s.getsockname()[1]) for s in ss))'
}

started=0
for _ in 1 2 3; do
  PORTS=$(free_ports)
  publish=()
  for p in $PORTS; do publish+=(-p "127.0.0.1:$p:$p"); done
  echo "==> starting $REDIS_IMAGE (cluster nodes on $PORTS)"
  # shellcheck disable=SC2016 # expanded by the container's sh
  if docker run -d --rm --name "$CLUSTER_CONTAINER" "${publish[@]}" -e PORTS="$PORTS" "$REDIS_IMAGE" sh -c '
      # the image loads its bundled modules only for its own redis-server
      json=/usr/local/lib/redis/modules/rejson.so
      load=; [ -f $json ] && load="--loadmodule $json"
      i=0
      for p in $PORTS; do
        i=$((i + 1))
        mkdir -p /data/$p
        redis-server --port $p --cluster-enabled yes --cluster-port $((20000 + i)) \
          --cluster-config-file nodes.conf --cluster-announce-ip 127.0.0.1 \
          --protected-mode no --save "" --appendonly no --dir /data/$p --daemonize yes $load
      done
      exec sleep infinity' >/dev/null; then
    started=1
    break
  fi
  docker rm -f "$CLUSTER_CONTAINER" >/dev/null 2>&1 || true
done
if [ "$started" != 1 ]; then
  echo "FAIL: could not start the cluster container" >&2
  exit 1
fi

nodes=()
for p in $PORTS; do
  wait_for "cluster node $p" docker exec "$CLUSTER_CONTAINER" redis-cli -p "$p" ping
  nodes+=("127.0.0.1:$p")
done
docker exec "$CLUSTER_CONTAINER" redis-cli --cluster create "${nodes[@]}" --cluster-replicas 1 --cluster-yes >/dev/null
for p in $PORTS; do
  wait_for "cluster state on $p" sh -c "docker exec '$CLUSTER_CONTAINER' redis-cli -p '$p' cluster info | grep -q 'cluster_state:ok'"
done

echo "==> building go-ycsb"
make >/dev/null

export GO_YCSB_BIN="$ROOT_DIR/bin/go-ycsb"
export REDIS_BATCH_IT_SINGLE="127.0.0.1:$SINGLE_PORT"
REDIS_BATCH_IT_CLUSTER=$(IFS=';'; echo "${nodes[*]}")
export REDIS_BATCH_IT_CLUSTER

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
