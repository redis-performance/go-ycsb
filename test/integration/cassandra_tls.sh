#!/usr/bin/env bash
#
# Integration test for db/cassandra/db.go's TLS support (cassandra.tls,
# cassandra.tls.ca, cassandra.tls.skip.verify, cassandra.tls.disable_host_lookup)
# against a real, dockerized, TLS-enabled ScyllaDB node.
#
# This exists specifically to catch a regression class that unit tests can't:
# a TLS config that encrypts the connection but doesn't actually verify the
# server's certificate. It asserts BOTH directions:
#   - a connection using the correct CA succeeds
#   - a connection using an unrelated ("wrong") CA is REJECTED
# A TLS setup that accepts any certificate would pass the first assertion and
# silently fail the second - which is exactly the bug this test is guarding
# against (see the PR discussion / commit history for db/cassandra/db.go).
#
# Same script for local dev and CI: by default it starts (and tears down) its
# own disposable Scylla container and a temporary CA/cert set, so
# `test/integration/cassandra_tls.sh` and the CI job run identically.
#
# Usage:
#   test/integration/cassandra_tls.sh
#
# Env overrides:
#   SCYLLA_IMAGE      docker image for ScyllaDB (default: scylladb/scylla:5.4)
#   SCYLLA_PORT       host port to publish the encrypted CQL port on (default: 19042)
#   START_CONTAINER   whether to start/stop the container (default: true)
#   CASSANDRA_CLUSTER cassandra.cluster to use when START_CONTAINER=false
#                      (must already have TLS enabled and CERTS_DIR's server
#                      cert/key configured server-side)

set -euo pipefail

SCYLLA_IMAGE=${SCYLLA_IMAGE:-scylladb/scylla:5.4}
SCYLLA_PORT=${SCYLLA_PORT:-19042}
START_CONTAINER=${START_CONTAINER:-true}
CASSANDRA_CLUSTER=${CASSANDRA_CLUSTER:-127.0.0.1:${SCYLLA_PORT}}

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$ROOT_DIR"

# Docker (in particular a snap-packaged docker daemon, as used in some dev
# environments) can only bind-mount paths under $HOME, so this deliberately
# lives under the repo checkout rather than /tmp.
WORK_DIR="$ROOT_DIR/.cassandra-tls-it"
CERTS_DIR="$WORK_DIR/certs"
CONTAINER=go-ycsb-it-scylla-tls

cleanup() {
  if [ "$START_CONTAINER" = "true" ]; then
    echo "==> stopping container"
    docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
  fi
  rm -rf "$WORK_DIR"
}
trap cleanup EXIT

rm -rf "$WORK_DIR"
mkdir -p "$CERTS_DIR"

echo "==> generating test CA + server cert (and an unrelated CA to test rejection)"
openssl req -x509 -newkey rsa:2048 -days 1 -nodes \
  -keyout "$CERTS_DIR/ca.key" -out "$CERTS_DIR/ca.crt" -subj "/CN=go-ycsb-it-ca" >/dev/null 2>&1
openssl req -newkey rsa:2048 -nodes \
  -keyout "$CERTS_DIR/server.key" -out "$CERTS_DIR/server.csr" -subj "/CN=go-ycsb-it-scylla" >/dev/null 2>&1
openssl x509 -req -in "$CERTS_DIR/server.csr" -CA "$CERTS_DIR/ca.crt" -CAkey "$CERTS_DIR/ca.key" \
  -CAcreateserial -out "$CERTS_DIR/server.crt" -days 1 >/dev/null 2>&1
openssl req -x509 -newkey rsa:2048 -days 1 -nodes \
  -keyout "$CERTS_DIR/wrong-ca.key" -out "$CERTS_DIR/wrong-ca.crt" -subj "/CN=go-ycsb-it-wrong-ca" >/dev/null 2>&1

if [ "$START_CONTAINER" = "true" ]; then
  echo "==> building TLS-enabled scylla.yaml"
  docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
  docker run --rm --entrypoint cat "$SCYLLA_IMAGE" /etc/scylla/scylla.yaml > "$WORK_DIR/scylla.yaml"
  cat >> "$WORK_DIR/scylla.yaml" <<'EOF'
client_encryption_options:
   enabled: true
   certificate: /etc/scylla/tls/server.crt
   keyfile: /etc/scylla/tls/server.key
EOF

  echo "==> starting $SCYLLA_IMAGE on :$SCYLLA_PORT (TLS-only CQL)"
  docker run -d --rm --name "$CONTAINER" \
    -p "${SCYLLA_PORT}:9042" \
    -v "$WORK_DIR/scylla.yaml:/etc/scylla/scylla.yaml:ro" \
    -v "$CERTS_DIR/server.crt:/etc/scylla/tls/server.crt:ro" \
    -v "$CERTS_DIR/server.key:/etc/scylla/tls/server.key:ro" \
    "$SCYLLA_IMAGE" --smp 1 --memory 750M --overprovisioned 1 --developer-mode 1 >/dev/null

  echo "==> waiting for scylla to start listening for encrypted CQL clients"
  for _ in $(seq 1 90); do
    if docker logs "$CONTAINER" 2>&1 | grep -q "Starting listening for CQL clients"; then
      break
    fi
    sleep 2
  done

  echo "==> creating keyspace 'test' (bootstrap, over TLS with verification off)"
  for _ in $(seq 1 30); do
    if docker exec -e SSL_VALIDATE=false "$CONTAINER" cqlsh --ssl -e \
      "CREATE KEYSPACE IF NOT EXISTS test WITH replication = {'class': 'SimpleStrategy', 'replication_factor': 1};" \
      >/dev/null 2>&1; then
      break
    fi
    sleep 2
  done
fi

echo "==> building go-ycsb"
make >/dev/null

# Runs go-ycsb and returns its exit code without tripping set -e, capturing
# combined output into $OUT for the assertion helpers below.
run() {
  set +e
  OUT=$(./bin/go-ycsb "$@" -p cassandra.cluster="$CASSANDRA_CLUSTER" \
    -p recordcount=50 -p operationcount=20 -p threadcount=4 2>&1)
  STATUS=$?
  set -e
}

expect_success() {
  local desc=$1
  if [ "$STATUS" -ne 0 ]; then
    echo "FAIL: $desc: expected success, got exit $STATUS"
    echo "$OUT"
    exit 1
  fi
  if echo "$OUT" | grep -q '_ERROR'; then
    echo "FAIL: $desc: succeeded but reported error ops"
    echo "$OUT"
    exit 1
  fi
  echo "OK: $desc"
}

expect_failure() {
  local desc=$1
  if [ "$STATUS" -eq 0 ]; then
    echo "FAIL: $desc: expected failure (untrusted cert must be rejected), but the connection succeeded"
    echo "$OUT"
    exit 1
  fi
  echo "OK: $desc (rejected as expected: $(echo "$OUT" | tail -1))"
}

echo "==> [1/5] load with the correct CA - must succeed"
run load cassandra -p cassandra.tls=true -p cassandra.tls.ca="$CERTS_DIR/ca.crt" -p dropdata=true
expect_success "load with correct CA"

echo "==> [2/5] run with the correct CA - must succeed"
run run cassandra -p cassandra.tls=true -p cassandra.tls.ca="$CERTS_DIR/ca.crt"
expect_success "run with correct CA"

echo "==> [3/5] run with an unrelated CA - must be REJECTED"
run run cassandra -p cassandra.tls=true -p cassandra.tls.ca="$CERTS_DIR/wrong-ca.crt"
expect_failure "run with wrong CA"

echo "==> [4/5] run with an unrelated CA + tls.skip.verify=true - must succeed (explicit bypass)"
run run cassandra -p cassandra.tls=true -p cassandra.tls.ca="$CERTS_DIR/wrong-ca.crt" -p cassandra.tls.skip.verify=true
expect_success "run with wrong CA + skip.verify=true"

echo "==> [5/5] run with no CA at all - must be REJECTED (self-signed CA isn't in the system trust store)"
run run cassandra -p cassandra.tls=true
expect_failure "run with no CA configured"

echo "==> cassandra TLS integration test passed"
