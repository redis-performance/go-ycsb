#!/usr/bin/env bash
#
# Integration test for db/couchbase/db.go against a real, dockerized Couchbase
# Community Edition node.
#
# Community Edition has no cluster-init tooling beyond its REST API (unlike
# Redis/Mongo, which are usable the instant the container is up), so this
# script does the same first-run setup the Capella/self-managed operator
# normally does once by hand: set memory quotas, pick services, set the admin
# credential, and create a bucket - all before go-ycsb ever connects.
#
# Runs the core workload (exercises Scan, which needs at least a few dozen
# keys to return anything) and the feature-store workload (exercises the
# adapter's 50-field, HGETALL-shaped Read path), then a negative-path check
# that couchbase.auto_create_collection=false correctly refuses to write into
# a collection that doesn't exist rather than silently succeeding or hanging.
#
# Deliberately NOT tested here: couchbase.durability=majority. Durability
# levels above "none" require the write to be replicated to another node
# before being acknowledged, which a single-node cluster - Community Edition
# or Enterprise, this isn't a CE limitation - can never satisfy (confirmed by
# hand: the server correctly returns "DurabilityImpossible", not a hang or a
# false success). TLS (couchbases://) is also not covered here: Community
# Edition has no native TLS, and this adapter's TLS path was instead verified
# by hand against a live Capella cluster (couchbases:// connection, real
# publicly-trusted certificate, no CA override needed) - see the PR
# description for that verification.
#
# Same script for local dev and CI: by default it starts (and tears down) its
# own disposable Couchbase container, so `test/integration/couchbase.sh` and
# the CI job run identically.
#
# Usage:
#   test/integration/couchbase.sh
#
# Env overrides:
#   RECORDCOUNT      number of records/entities to load (default: 20000)
#   OPERATIONCOUNT   number of ops in the run phase (default: 3x RECORDCOUNT)
#   THREADCOUNT      client concurrency (default: 8)
#   COUCHBASE_IMAGE  docker image for Couchbase (default: couchbase:community-7.6.2)
#   START_CONTAINER  whether to start/stop/init the container (default: true)
#   COUCHBASE_CONNSTR couchbase.connection_string to use when START_CONTAINER=false
#                      (the bucket named below must already exist)
#   COUCHBASE_BUCKET  bucket name (default: ycsb)
#   COUCHBASE_USER    admin username (default: Administrator)
#   COUCHBASE_PASS    admin password (default: password123)
#
# Couchbase's cluster map advertises the node's own address to clients, which
# breaks the usual `-p host:container_port` docker port mapping (the client
# connects to the mapped port, gets redirected to the container's internal
# address, and hangs) - so this test runs the container with --network host
# instead, same as it would in a real single-node deployment.

set -euo pipefail

RECORDCOUNT=${RECORDCOUNT:-20000}
OPERATIONCOUNT=${OPERATIONCOUNT:-$((RECORDCOUNT * 3))}
THREADCOUNT=${THREADCOUNT:-8}
COUCHBASE_IMAGE=${COUCHBASE_IMAGE:-couchbase:community-7.6.2}
START_CONTAINER=${START_CONTAINER:-true}
COUCHBASE_BUCKET=${COUCHBASE_BUCKET:-ycsb}
COUCHBASE_USER=${COUCHBASE_USER:-Administrator}
COUCHBASE_PASS=${COUCHBASE_PASS:-password123}
COUCHBASE_CONNSTR=${COUCHBASE_CONNSTR:-couchbase://127.0.0.1}

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$ROOT_DIR"

CONTAINER=go-ycsb-it-couchbase
MGMT_URL=http://127.0.0.1:8091

cleanup() {
  if [ "$START_CONTAINER" = "true" ]; then
    echo "==> stopping container"
    docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

if [ "$START_CONTAINER" = "true" ]; then
  echo "==> starting $COUCHBASE_IMAGE"
  docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
  docker run -d --rm --name "$CONTAINER" --network host "$COUCHBASE_IMAGE" >/dev/null

  echo "==> waiting for the management API"
  for _ in $(seq 1 60); do
    if [ "$(curl -s -o /dev/null -w '%{http_code}' "$MGMT_URL/pools" 2>/dev/null)" = "200" ]; then
      break
    fi
    sleep 1
  done

  echo "==> initializing cluster (quotas, services, admin credential, index storage mode)"
  curl -sf -X POST "$MGMT_URL/pools/default" \
    -d memoryQuota=512 -d indexMemoryQuota=256 -d ftsMemoryQuota=256 >/dev/null
  curl -sf -X POST "$MGMT_URL/node/controller/setupServices" -d services=kv,n1ql,index,fts >/dev/null
  curl -sf -X POST "$MGMT_URL/settings/web" \
    -d "password=${COUCHBASE_PASS}" -d "username=${COUCHBASE_USER}" -d port=8091 >/dev/null
  # Community Edition's indexer only supports the forestdb storage backend
  # (plasma, the default suggested elsewhere in Couchbase's own docs, is
  # Enterprise-only and this call fails against it).
  curl -sf -u "${COUCHBASE_USER}:${COUCHBASE_PASS}" -X POST "$MGMT_URL/settings/indexes" \
    -d storageMode=forestdb >/dev/null

  echo "==> creating bucket '$COUCHBASE_BUCKET'"
  curl -sf -u "${COUCHBASE_USER}:${COUCHBASE_PASS}" -X POST "$MGMT_URL/pools/default/buckets" \
    -d "name=${COUCHBASE_BUCKET}" -d bucketType=couchbase -d ramQuotaMB=256 -d flushEnabled=1 >/dev/null
  sleep 3
fi

echo "==> building go-ycsb"
make >/dev/null

run_phase() {
  local phase=$1 workload=$2
  shift 2
  ./bin/go-ycsb "$phase" couchbase -P "$workload" \
    -p couchbase.connection_string="$COUCHBASE_CONNSTR" \
    -p couchbase.username="$COUCHBASE_USER" -p couchbase.password="$COUCHBASE_PASS" \
    -p couchbase.bucket="$COUCHBASE_BUCKET" \
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

echo "==> [core workload, table=usertable] load ($RECORDCOUNT records)"
OUT=$(run_phase load workloads/workload_template -p table=usertable)
echo "$OUT" | tail -5
check_output "$OUT" INSERT "$RECORDCOUNT"

echo "==> [core workload, table=usertable] run ($OPERATIONCOUNT ops, default 95% read / 5% update)"
OUT=$(run_phase run workloads/workload_template -p table=usertable)
echo "$OUT" | tail -5
check_output "$OUT" TOTAL "$OPERATIONCOUNT"

# A dedicated, deliberately tiny Scan check: gocb's KV range scan (the only
# way this adapter implements Scan, since Community Edition has no secondary
# index service worth relying on) dispatches to every vbucket per call and is
# an order of magnitude slower per-op than a point Get - fine for correctness
# here, but running it at RECORDCOUNT/OPERATIONCOUNT scale would make this
# job the slowest thing in CI for no additional coverage.
# threadcount=1: gocb's own docs describe Scan (KV range scan) as meant "for
# low concurrency batch queries" - running it concurrently from many threads
# against the same collection is outside that intended use and has been
# observed to stall well past its own timeout (see db/couchbase/db.go's Scan
# doc comment). This check is about Scan's correctness, not its concurrent
# throughput, so it deliberately stays within the supported usage.
echo "==> [core workload, table=usertable] Scan smoke test (100 scan ops, threadcount=1)"
OUT=$(run_phase run workloads/workload_template -p table=usertable \
  -p recordcount="$RECORDCOUNT" -p operationcount=100 -p threadcount=1 \
  -p readproportion=0 -p updateproportion=0 -p scanproportion=1 -p maxscanlength=10)
echo "$OUT" | tail -5
check_output "$OUT" TOTAL 100

echo "==> [feature-store workload] load ($RECORDCOUNT entities)"
OUT=$(run_phase load workloads/workload_feature_store)
echo "$OUT" | tail -5
check_output "$OUT" INSERT "$RECORDCOUNT"

echo "==> [feature-store workload] run ($OPERATIONCOUNT ops)"
OUT=$(run_phase run workloads/workload_feature_store)
echo "$OUT" | tail -5
check_output "$OUT" TOTAL "$OPERATIONCOUNT"

echo "==> [negative] couchbase.auto_create_collection=false against a nonexistent collection must fail, not hang or silently succeed"
set +e
OUT=$(run_phase load workloads/workload_template -p table=does_not_exist_and_wont_be_created \
  -p couchbase.auto_create_collection=false -p recordcount=5 -p operationcount=5 -p threadcount=1 2>&1)
set -e
if ! echo "$OUT" | grep -q 'INSERT_ERROR'; then
  echo "FAIL: expected INSERT_ERROR against a nonexistent collection with auto_create_collection=false, got:"
  echo "$OUT"
  exit 1
fi
echo "OK: auto_create_collection=false correctly rejected writes to a nonexistent collection"

# Single-node CE can't reproduce the actual multi-node ErrScopeNotFound race
# ensureCollection retries around (that needs CreateScope and CreateCollection
# to land on two different, not-yet-mutually-consistent nodes - see
# db/couchbase/db.go's ensureCollection doc comment) - but this at least
# exercises the CreateScope call and the non-default-scope path end to end,
# so it isn't completely untested.
echo "==> [non-default scope] load+run against couchbase.scope=go_ycsb_it_scope"
OUT=$(run_phase load workloads/workload_template -p table=usertable -p couchbase.scope=go_ycsb_it_scope -p recordcount=1000 -p operationcount=1000)
echo "$OUT" | tail -5
check_output "$OUT" INSERT 1000
OUT=$(run_phase run workloads/workload_template -p table=usertable -p couchbase.scope=go_ycsb_it_scope -p recordcount=1000 -p operationcount=1000)
echo "$OUT" | tail -5
check_output "$OUT" TOTAL 1000

echo "==> couchbase integration test passed"
