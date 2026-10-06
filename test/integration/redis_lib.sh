#!/usr/bin/env bash
#
# Shared helpers for the redis integration tests: disposable single Redis and
# Redis Cluster containers, under names unique to the run. Sourced, not run.
#
# Callers set REDIS_IMAGE and SUFFIX (unique per run), call redis_start_single
# and/or redis_start_cluster, and redis_cleanup on exit. redis_start_cluster
# needs python3 (free_ports): callers check for it, and skip without it.

REDIS_CONTAINERS=()

redis_cleanup() {
  if [ ${#REDIS_CONTAINERS[@]} -gt 0 ]; then
    docker rm -f "${REDIS_CONTAINERS[@]}" >/dev/null 2>&1 || true
  fi
}

# wait_for <what> <command...>: retries the command for up to 60 s.
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

# redis_start_single <name>: starts a single Redis; sets SINGLE_PORT, its port
# on the host (it listens on 6379 in the container).
redis_start_single() {
  local name=$1
  echo "==> starting $REDIS_IMAGE (single, $name)"
  docker run -d --rm --name "$name" -p "127.0.0.1::6379" "$REDIS_IMAGE" >/dev/null
  REDIS_CONTAINERS+=("$name")
  SINGLE_PORT=$(docker port "$name" 6379/tcp | head -n 1 | sed 's/.*://')
  wait_for "single redis" docker exec "$name" redis-cli ping
}

# free_ports <n>: n free TCP ports on 127.0.0.1 (needs python3).
free_ports() {
  python3 -c 'import socket, sys
ss = [socket.socket() for _ in range(int(sys.argv[1]))]
for s in ss:
    s.bind(("127.0.0.1", 0))
print(" ".join(str(s.getsockname()[1]) for s in ss))' "$1"
}

# redis_start_cluster <name> <masters> <replicas per master>: starts a Redis
# Cluster in one container; sets CLUSTER_PORTS and CLUSTER_ADDR (";"-joined,
# for redis.addr).
#
# The nodes announce 127.0.0.1 and the host ports they are published on,
# which are also the ports they listen on in the container: then the nodes
# reach each other, and the client on the host reaches every node, at the
# address the cluster gives for it. Their bus ports stay in the container.
redis_start_cluster() {
  local name=$1 masters=$2 replicas=$3
  local n=$((masters * (replicas + 1))) started=0
  for _ in 1 2 3; do
    CLUSTER_PORTS=$(free_ports "$n")
    local publish=()
    for p in $CLUSTER_PORTS; do publish+=(-p "127.0.0.1:$p:$p"); done
    echo "==> starting $REDIS_IMAGE (cluster $name, nodes on $CLUSTER_PORTS)"
    # shellcheck disable=SC2016 # expanded by the container's sh
    if docker run -d --rm --name "$name" "${publish[@]}" -e PORTS="$CLUSTER_PORTS" "$REDIS_IMAGE" sh -c '
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
    docker rm -f "$name" >/dev/null 2>&1 || true
  done
  if [ "$started" != 1 ]; then
    echo "FAIL: could not start the cluster container" >&2
    return 1
  fi
  REDIS_CONTAINERS+=("$name")
  local nodes=()
  for p in $CLUSTER_PORTS; do
    wait_for "cluster node $p" docker exec "$name" redis-cli -p "$p" ping
    nodes+=("127.0.0.1:$p")
  done
  docker exec "$name" redis-cli --cluster create "${nodes[@]}" --cluster-replicas "$replicas" --cluster-yes >/dev/null
  for p in $CLUSTER_PORTS; do
    wait_for "cluster state on $p" sh -c "docker exec '$name' redis-cli -p '$p' cluster info | grep -q 'cluster_state:ok'"
  done
  CLUSTER_ADDR=$(IFS=';'; echo "${nodes[*]}")
}

# redis_cli <container> <port> <args...>
redis_cli() {
  local name=$1 port=$2
  shift 2
  docker exec "$name" redis-cli -p "$port" "$@" | tr -d '\r'
}
