#!/usr/bin/env bash
#
# Integration test for db/cosmosdb/db.go's TLS support (cosmosdb.insecure_skip_verify).
#
# The Cosmos DB (vNext) Linux emulator this repo's main cosmosdb integration
# test uses serves plain HTTP, not HTTPS, so test/integration/cosmosdb.sh
# never exercises cosmosdb.insecure_skip_verify in either direction - a
# property whose entire job is to disable certificate verification is
# exactly the kind of thing this repo's AGENTS.md asks to be tested in both
# directions (an earlier adapter here shipped with certificate verification
# silently disabled). This test puts a TLS-terminating proxy (socat, with a
# throwaway self-signed cert) in front of the same plaintext emulator,
# mirroring test/integration/aerospike_tls.sh's approach for Aerospike
# Community Edition (which also has no native TLS of its own). Unlike
# Aerospike's tls.ca (pinned CA), this adapter has no CA-pinning option -
# only skip-verify - so this only asserts connection outcome in the two
# directions that option actually controls: rejected by default against an
# untrusted cert, accepted with insecure_skip_verify=true.
#
# Usage:
#   test/integration/cosmosdb_tls.sh
#
# Env overrides:
#   COSMOSDB_IMAGE    docker image for the emulator (default: mcr.microsoft.com/cosmosdb/linux/azure-cosmos-emulator:vnext-preview)
#   PROXY_IMAGE       docker image providing socat for the TLS-terminating proxy (default: alpine:3.20)
#   COSMOS_TLS_PORT   host port to publish the TLS proxy on (default: 18443)
#   START_CONTAINERS  whether to start/stop the containers (default: true)

set -euo pipefail

COSMOSDB_IMAGE=${COSMOSDB_IMAGE:-mcr.microsoft.com/cosmosdb/linux/azure-cosmos-emulator:vnext-preview}
PROXY_IMAGE=${PROXY_IMAGE:-alpine:3.20}
COSMOS_TLS_PORT=${COSMOS_TLS_PORT:-18443}
START_CONTAINERS=${START_CONTAINERS:-true}
# Well-known, publicly-documented emulator key - not a real secret. Same
# value test/integration/cosmosdb.sh uses.
COSMOSDB_KEY=${COSMOSDB_KEY:-'C2y6yDjf5/R+ob0N8A7Cgv30VRDJIWEHLM+4QDU5DE2nQ9nDuVTqobD4b8mGGyPMbIZnqyMsEcaGQy67XIw/Jw=='}
COSMOSDB_DATABASE=${COSMOSDB_DATABASE:-ycsb}

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$ROOT_DIR"

# Docker (in particular a snap-packaged docker daemon) can only bind-mount
# paths under $HOME, so this deliberately lives under the repo checkout
# rather than /tmp - see test/integration/aerospike_tls.sh for the same note.
WORK_DIR="$ROOT_DIR/.cosmosdb-tls-it"
CERTS_DIR="$WORK_DIR/certs"
NETWORK=go-ycsb-it-cosmosdb-tls-net
BACKEND=go-ycsb-it-cosmosdb-tls-backend
PROXY=go-ycsb-it-cosmosdb-tls-proxy

cleanup() {
  if [ "$START_CONTAINERS" = "true" ]; then
    echo "==> stopping containers"
    docker rm -f "$BACKEND" "$PROXY" >/dev/null 2>&1 || true
    docker network rm "$NETWORK" >/dev/null 2>&1 || true
  fi
  rm -rf "$WORK_DIR"
}
trap cleanup EXIT

rm -rf "$WORK_DIR"
mkdir -p "$CERTS_DIR"

echo "==> generating a throwaway self-signed server cert (SAN=127.0.0.1,localhost - matches how the client connects)"
cat > "$CERTS_DIR/san.cnf" <<EOF
[req]
distinguished_name = req_distinguished_name
x509_extensions = v3_req
prompt = no
[req_distinguished_name]
CN = go-ycsb-it-cosmosdb-tls
[v3_req]
subjectAltName = @alt_names
[alt_names]
DNS.1 = localhost
IP.1 = 127.0.0.1
EOF
openssl req -x509 -newkey rsa:2048 -days 1 -nodes \
  -keyout "$CERTS_DIR/server.key" -out "$CERTS_DIR/server.crt" -config "$CERTS_DIR/san.cnf" >/dev/null 2>&1
cat "$CERTS_DIR/server.crt" "$CERTS_DIR/server.key" > "$CERTS_DIR/server-combined.pem"

if [ "$START_CONTAINERS" = "true" ]; then
  echo "==> starting $COSMOSDB_IMAGE (plaintext, network-internal only) behind a TLS-terminating proxy"
  docker rm -f "$BACKEND" "$PROXY" >/dev/null 2>&1 || true
  docker network rm "$NETWORK" >/dev/null 2>&1 || true
  docker network create "$NETWORK" >/dev/null

  docker run -d --name "$BACKEND" --network "$NETWORK" "$COSMOSDB_IMAGE" >/dev/null

  echo "==> waiting for the emulator backend to start listening"
  backend_ready=false
  for _ in $(seq 1 60); do
    if docker logs "$BACKEND" 2>&1 | grep -qi "now listening on"; then
      backend_ready=true
      break
    fi
    if [ "$(docker inspect -f '{{.State.Running}}' "$BACKEND" 2>/dev/null)" != "true" ]; then
      break
    fi
    sleep 2
  done
  if [ "$backend_ready" != "true" ]; then
    echo "FAIL: cosmosdb emulator backend never became ready"
    docker logs "$BACKEND" 2>&1 | tail -30
    exit 1
  fi

  docker run -d --name "$PROXY" --network "$NETWORK" \
    -p "${COSMOS_TLS_PORT}:${COSMOS_TLS_PORT}" \
    -v "$CERTS_DIR/server-combined.pem:/certs/server.pem:ro" \
    "$PROXY_IMAGE" sh -c "apk add --no-cache socat >/dev/null 2>&1 && exec socat OPENSSL-LISTEN:${COSMOS_TLS_PORT},cert=/certs/server.pem,verify=0,fork,bind=0.0.0.0 TCP:${BACKEND}:8081" >/dev/null

  echo "==> waiting for the TLS proxy to start listening"
  proxy_ready=false
  for _ in $(seq 1 60); do
    if (exec 3<>"/dev/tcp/127.0.0.1/${COSMOS_TLS_PORT}") 2>/dev/null; then
      exec 3<&- 3>&-
      proxy_ready=true
      break
    fi
    sleep 1
  done
  if [ "$proxy_ready" != "true" ]; then
    echo "FAIL: TLS proxy on :${COSMOS_TLS_PORT} never started listening"
    docker logs "$PROXY" 2>&1 | tail -30
    exit 1
  fi
fi

echo "==> building go-ycsb"
make >/dev/null

# Runs go-ycsb and returns its exit code without tripping set -e, capturing
# combined output into $OUT for the assertion helpers below. A single
# minimal record - this only needs to exercise cosmosDBCreator.Create()'s
# TLS handshake (CreateDatabase/database.Read), not a full workload.
run() {
  set +e
  OUT=$(./bin/go-ycsb load cosmosdb -P workloads/workload_template \
    -p cosmosdb.endpoint="https://127.0.0.1:${COSMOS_TLS_PORT}" -p cosmosdb.key="$COSMOSDB_KEY" \
    -p cosmosdb.database="$COSMOSDB_DATABASE" -p cosmosdb.auto_create_container=true \
    -p recordcount=1 -p operationcount=1 -p threadcount=1 "$@" 2>&1)
  STATUS=$?
  set -e
}

# Asserts the connection (cosmosDBCreator.Create) succeeded - i.e. TLS
# verification passed (or was explicitly bypassed).
expect_connect() {
  local desc=$1
  if echo "$OUT" | grep -q 'create db cosmosdb failed'; then
    echo "FAIL: $desc: expected the TLS connection to succeed, but Create() failed"
    echo "$OUT"
    exit 1
  fi
  echo "OK: $desc"
}

# Asserts the connection was rejected specifically at the TLS layer (an x509
# error), not for some unrelated reason.
expect_tls_reject() {
  local desc=$1
  if ! echo "$OUT" | grep -q 'create db cosmosdb failed'; then
    echo "FAIL: $desc: expected the TLS connection to be rejected (untrusted cert), but it succeeded"
    echo "$OUT"
    exit 1
  fi
  if ! echo "$OUT" | grep -qi 'x509'; then
    echo "FAIL: $desc: connection failed, but not with an x509/certificate error as expected"
    echo "$OUT"
    exit 1
  fi
  echo "OK: $desc (rejected as expected: $(echo "$OUT" | grep -i x509 | tail -1 | sed 's/^ *//'))"
}

echo "==> [1/2] connect to a self-signed cert with insecure_skip_verify unset (default false) - must be REJECTED at the TLS layer"
run -p cosmosdb.insecure_skip_verify=false
expect_tls_reject "connect without insecure_skip_verify"

echo "==> [2/2] connect to the same self-signed cert with insecure_skip_verify=true - must succeed (explicit bypass)"
run -p cosmosdb.insecure_skip_verify=true
expect_connect "connect with insecure_skip_verify=true"

echo "==> cosmosdb TLS integration test passed"
