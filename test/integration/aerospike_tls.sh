#!/usr/bin/env bash
#
# Integration test for db/aerospike/db.go's TLS support (aerospike.tls,
# aerospike.tls.ca, aerospike.tls.skip.verify, aerospike.tls.name).
#
# Aerospike Community Edition (the only freely available Docker image) does
# NOT support native server-side TLS at all - it's an Enterprise-only
# feature ("'tls-port' is enterprise-only"), so this can't stand up a
# genuinely TLS-native Aerospike server the way test/integration/cassandra_tls.sh
# does for ScyllaDB. Instead this puts a transparent TLS-terminating proxy
# (socat) in front of a real Community Edition backend, and asserts on
# CONNECTION outcome (does aerospikeCreator.Create() succeed or fail), which
# is exactly where this adapter's TLS verification logic lives - the same
# class of bug this test guards against (verification silently disabled)
# would flip these assertions regardless of what's on the other end of the
# TLS handshake. It deliberately does NOT assert on data-plane op success:
# the proxy's address-rewriting (needed so Aerospike's own peer-discovery
# doesn't route around it) is a test-harness artifact that can desync the
# client's partition map, unrelated to whether TLS verification is correct.
#
# Usage:
#   test/integration/aerospike_tls.sh
#
# Env overrides:
#   AEROSPIKE_IMAGE   docker image for Aerospike Community Edition (default: aerospike/aerospike-server:latest)
#   PROXY_IMAGE       docker image providing socat for the TLS-terminating proxy (default: alpine:3.20)
#   AEROSPIKE_PORT    host port to publish the TLS proxy on (default: 14333)
#   START_CONTAINERS  whether to start/stop the containers (default: true)

set -euo pipefail

AEROSPIKE_IMAGE=${AEROSPIKE_IMAGE:-aerospike/aerospike-server:latest}
PROXY_IMAGE=${PROXY_IMAGE:-alpine:3.20}
AEROSPIKE_PORT=${AEROSPIKE_PORT:-14333}
START_CONTAINERS=${START_CONTAINERS:-true}

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$ROOT_DIR"

# Docker (in particular a snap-packaged docker daemon, as used in some dev
# environments) can only bind-mount paths under $HOME, so this deliberately
# lives under the repo checkout rather than /tmp.
WORK_DIR="$ROOT_DIR/.aerospike-tls-it"
CERTS_DIR="$WORK_DIR/certs"
NETWORK=go-ycsb-it-aerospike-net
BACKEND=go-ycsb-it-aerospike-backend
PROXY=go-ycsb-it-aerospike-proxy
TLS_NAME=go-ycsb-it-aerospike

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

echo "==> generating test CA + server cert with a matching SAN (and an unrelated CA to test rejection)"
openssl req -x509 -newkey rsa:2048 -days 1 -nodes \
  -keyout "$CERTS_DIR/ca.key" -out "$CERTS_DIR/ca.crt" -subj "/CN=go-ycsb-it-ca" >/dev/null 2>&1
cat > "$CERTS_DIR/san.cnf" <<EOF
[req]
distinguished_name = req_distinguished_name
x509_extensions = v3_req
prompt = no
[req_distinguished_name]
CN = ${TLS_NAME}
[v3_req]
subjectAltName = @alt_names
[alt_names]
DNS.1 = ${TLS_NAME}
EOF
openssl req -newkey rsa:2048 -nodes \
  -keyout "$CERTS_DIR/server.key" -out "$CERTS_DIR/server.csr" -config "$CERTS_DIR/san.cnf" >/dev/null 2>&1
openssl x509 -req -in "$CERTS_DIR/server.csr" -CA "$CERTS_DIR/ca.crt" -CAkey "$CERTS_DIR/ca.key" \
  -CAcreateserial -out "$CERTS_DIR/server.crt" -days 1 -extensions v3_req -extfile "$CERTS_DIR/san.cnf" >/dev/null 2>&1
cat "$CERTS_DIR/server.crt" "$CERTS_DIR/server.key" > "$CERTS_DIR/server-combined.pem"
openssl req -x509 -newkey rsa:2048 -days 1 -nodes \
  -keyout "$CERTS_DIR/wrong-ca.key" -out "$CERTS_DIR/wrong-ca.crt" -subj "/CN=go-ycsb-it-wrong-ca" >/dev/null 2>&1

if [ "$START_CONTAINERS" = "true" ]; then
  echo "==> starting $AEROSPIKE_IMAGE (Community Edition, plaintext) behind a TLS-terminating proxy"
  docker rm -f "$BACKEND" "$PROXY" >/dev/null 2>&1 || true
  docker network rm "$NETWORK" >/dev/null 2>&1 || true
  docker network create "$NETWORK" >/dev/null

  cat > "$WORK_DIR/aerospike.conf" <<EOF
service {
	proto-fd-max 15000
	cluster-name gotest
}
logging {
	console {
		context any info
	}
}
network {
	service {
		address any
		port 3000
		access-address ${PROXY}
		access-port ${AEROSPIKE_PORT}
	}
	heartbeat {
		mode mesh
		port 3002
		interval 150
		timeout 10
	}
	fabric {
		port 3001
	}
	admin {
		address any
		port 3003
	}
}
namespace test {
	replication-factor 1
	storage-engine memory {
		data-size 1G
	}
}
EOF

  # The proxy MUST start before the backend: aerospike.conf's access-address
  # points at the proxy's container name, and the aerospike server resolves
  # (and validates) that name via Docker's embedded DNS synchronously at its
  # own startup - if the proxy container doesn't exist yet, that resolution
  # fails and the server refuses to start ("Invalid access address").
  docker run -d --name "$PROXY" --network "$NETWORK" \
    -p "${AEROSPIKE_PORT}:${AEROSPIKE_PORT}" \
    -v "$CERTS_DIR/server-combined.pem:/certs/server.pem:ro" \
    "$PROXY_IMAGE" sh -c "apk add --no-cache socat >/dev/null 2>&1 && exec socat OPENSSL-LISTEN:${AEROSPIKE_PORT},cert=/certs/server.pem,verify=0,fork,bind=0.0.0.0 TCP:${BACKEND}:3000" >/dev/null

  echo "==> waiting for the TLS proxy to start listening"
  proxy_ready=false
  for _ in $(seq 1 60); do
    if (exec 3<>"/dev/tcp/127.0.0.1/${AEROSPIKE_PORT}") 2>/dev/null; then
      exec 3<&- 3>&-
      proxy_ready=true
      break
    fi
    sleep 1
  done
  if [ "$proxy_ready" != "true" ]; then
    echo "FAIL: TLS proxy on :${AEROSPIKE_PORT} never started listening"
    docker logs "$PROXY" 2>&1 | tail -30
    exit 1
  fi

  docker run -d --name "$BACKEND" --network "$NETWORK" --ulimit nofile=15000:15000 \
    -v "$WORK_DIR/aerospike.conf:/etc/aerospike/aerospike.template.conf:ro" \
    "$AEROSPIKE_IMAGE" >/dev/null

  echo "==> waiting for the aerospike backend to form a single-node cluster"
  backend_ready=false
  for _ in $(seq 1 60); do
    if docker logs "$BACKEND" 2>&1 | grep -q "rebalanced"; then
      backend_ready=true
      break
    fi
    if [ "$(docker inspect -f '{{.State.Running}}' "$BACKEND" 2>/dev/null)" != "true" ]; then
      break
    fi
    sleep 2
  done
  if [ "$backend_ready" != "true" ]; then
    echo "FAIL: aerospike backend never became ready"
    docker logs "$BACKEND" 2>&1 | tail -30
    exit 1
  fi
fi

echo "==> building go-ycsb"
make >/dev/null

# Runs go-ycsb and returns its exit code without tripping set -e, capturing
# combined output into $OUT for the assertion helpers below.
run() {
  set +e
  OUT=$(./bin/go-ycsb "$@" -p aerospike.host=127.0.0.1 -p aerospike.port="$AEROSPIKE_PORT" \
    -p recordcount=5 -p operationcount=5 -p threadcount=1 2>&1)
  STATUS=$?
  set -e
}

# Asserts the connection (aerospikeCreator.Create) succeeded - i.e. TLS
# verification passed - regardless of what any subsequent data-plane op did.
expect_connect() {
  local desc=$1
  if echo "$OUT" | grep -q 'create db aerospike failed'; then
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
  if ! echo "$OUT" | grep -q 'create db aerospike failed'; then
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

echo "==> [1/3] connect with the correct CA + matching tls.name - TLS verification must pass"
run run aerospike -p aerospike.tls=true -p aerospike.tls.ca="$CERTS_DIR/ca.crt" -p aerospike.tls.name="$TLS_NAME"
expect_connect "connect with correct CA"

echo "==> [2/3] connect with an unrelated CA - must be REJECTED at the TLS layer"
run run aerospike -p aerospike.tls=true -p aerospike.tls.ca="$CERTS_DIR/wrong-ca.crt" -p aerospike.tls.name="$TLS_NAME"
expect_tls_reject "connect with wrong CA"

echo "==> [3/3] connect with an unrelated CA + tls.skip.verify=true - must succeed (explicit bypass)"
run run aerospike -p aerospike.tls=true -p aerospike.tls.ca="$CERTS_DIR/wrong-ca.crt" -p aerospike.tls.skip.verify=true
expect_connect "connect with wrong CA + skip.verify=true"

echo "==> aerospike TLS integration test passed"
