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

# Exercises Update()'s fieldPathSafe-driven fallback to Get+merge+Replace
# (db/couchbase/db.go) end to end: a dotted field name isn't safe to use as
# a Couchbase subdocument path (MutateIn's fast path would otherwise parse
# "event.ts" as nested field "ts" inside object "event"), so this must route
# through the CAS-protected fallback instead and still succeed cleanly. Unit
# tests only cover the regexp in isolation; this is what actually catches a
# regression in the branch-selection logic itself.
echo "==> [unsafe field name] load+run with lastfieldname=event.ts (exercises Update's fieldPathSafe fallback)"
OUT=$(run_phase load workloads/workload_template -p table=dottedfieldtest -p fieldcount=3 \
  -p lastfieldname=event.ts -p lastfieldvaluetype=timestamp -p fieldvaluetype=numeric \
  -p recordcount=1000 -p operationcount=1000)
echo "$OUT" | tail -5
check_output "$OUT" INSERT 1000
OUT=$(run_phase run workloads/workload_template -p table=dottedfieldtest -p fieldcount=3 \
  -p lastfieldname=event.ts -p lastfieldvaluetype=timestamp -p fieldvaluetype=numeric \
  -p recordcount=1000 -p operationcount=1000 -p readproportion=0 -p updateproportion=1)
echo "$OUT" | tail -5
check_output "$OUT" TOTAL 1000

# go-ycsb's built-in dataintegrity mechanism verifies every field VALUE
# returned by Read is byte-exact what was written - real, valuable coverage,
# but note what it does NOT catch: verifyRow (pkg/workload/core.go) only
# checks fields actually present in a Read's result map, so it would not by
# itself have caught round 2's bug (Update silently dropping every
# unmentioned field) - a document missing fields still passes verifyRow for
# whichever fields survived. See the dedicated field-count check below for
# that regression specifically. dataintegrity requires
# fieldlengthdistribution=constant (go-ycsb's own requirement, since the
# deterministic value's length must be reproducible without replaying the
# load phase's exact RNG sequence).
echo "==> [dataintegrity] Read/Update must return byte-exact deterministic field values"
OUT=$(run_phase load workloads/workload_template -p table=ditest -p dataintegrity=true \
  -p fieldlengthdistribution=constant -p fieldlength=20 -p recordcount=2000 -p operationcount=2000)
echo "$OUT" | tail -5
check_output "$OUT" INSERT 2000
OUT=$(run_phase run workloads/workload_template -p table=ditest -p dataintegrity=true \
  -p fieldlengthdistribution=constant -p fieldlength=20 -p recordcount=2000 -p operationcount=6000 \
  -p readallfields=true)
echo "$OUT" | tail -5
check_output "$OUT" TOTAL 6000

# read_raw_field_count fetches a document directly via cbc (bypassing this
# adapter's own Read entirely, so a bug in Read masking a bug in Update
# cannot hide anything) and prints its top-level field count. Retries: cbc
# opens a brand-new libcouchbase connection per invocation with no retry of
# its own, and a transient bootstrap hiccup under the load the preceding
# phases just generated has been observed in practice - without this, that
# would abort the whole script with an opaque Python traceback (from
# feeding empty input to json.load under `set -euo pipefail`) instead of
# either passing or producing a clear diagnostic.
read_raw_field_count() {
  local key=$1 table=$2 attempt cbc_out cbc_err json_line rc
  local err_file
  err_file=$(mktemp)
  for attempt in 1 2 3 4 5; do
    set +e
    cbc_out=$(docker exec "$CONTAINER" cbc cat "$key" -u "$COUCHBASE_USER" -P "$COUCHBASE_PASS" \
      -U "couchbase://127.0.0.1/${COUCHBASE_BUCKET}" --collection "$table" 2>"$err_file")
    rc=$?
    cbc_err=$(cat "$err_file")
    set -e
    if [ "$rc" -eq 0 ]; then
      json_line=$(echo "$cbc_out" | grep '^{' || true)
      if [ -n "$json_line" ]; then
        rm -f "$err_file"
        echo "$json_line" | python3 -c "import json,sys; print(len(json.load(sys.stdin)))"
        return 0
      fi
    fi
    echo "  (attempt $attempt/5: cbc cat '$key' from '$table' had no document yet - ${cbc_err:-no stderr output}) " >&2
    sleep 1
  done
  rm -f "$err_file"
  echo "FAIL: cbc cat never returned document '$key' from collection '$table' after 5 attempts" >&2
  return 1
}

# Direct regression guard for round 2's data-corruption bug (Update() used
# to call a full-document Replace with only the updated field(s), silently
# destroying every other field): load a document, hammer it with
# single-field Updates, then assert every original field is still present.
#
# lastfieldname=event.ts (unsafe as a subdocument path, see fieldPathSafe in
# db/couchbase/db.go) deliberately routes some of these single-field updates
# through Update's Get+merge+Replace fallback rather than only its MutateIn
# fast path - fieldcount=2 makes the unsafe field get picked on roughly half
# of the 30 update ops (P(never picked) = 0.5^30, negligible), so this
# exercises the fallback's own merge loop against a genuinely partial
# (single-field) update, the one scenario a future regression there could
# reintroduce this exact bug class in.
echo "==> [field preservation] Update() must not drop fields it wasn't asked to change (both the fast and fallback merge paths)"
FC_TABLE=fieldpreservetest
FC_FIELDCOUNT=2
OUT=$(run_phase load workloads/workload_template -p table="$FC_TABLE" -p fieldcount="$FC_FIELDCOUNT" \
  -p lastfieldname=event.ts -p lastfieldvaluetype=timestamp -p fieldvaluetype=numeric \
  -p insertorder=ordered -p recordcount=1 -p operationcount=1 -p threadcount=1)
echo "$OUT" | tail -5
check_output "$OUT" INSERT 1
OUT=$(run_phase run workloads/workload_template -p table="$FC_TABLE" -p fieldcount="$FC_FIELDCOUNT" \
  -p lastfieldname=event.ts -p lastfieldvaluetype=timestamp -p fieldvaluetype=numeric \
  -p insertorder=ordered -p recordcount=1 -p operationcount=30 -p threadcount=1 \
  -p readproportion=0 -p updateproportion=1)
echo "$OUT" | tail -5
check_output "$OUT" TOTAL 30

if ! got_fields=$(read_raw_field_count user0 "$FC_TABLE"); then
  exit 1
fi
if [ "$got_fields" != "$FC_FIELDCOUNT" ]; then
  echo "FAIL: expected all $FC_FIELDCOUNT fields to survive 30 single-field updates, found $got_fields"
  exit 1
fi
echo "OK: all $FC_FIELDCOUNT fields survived 30 single-field updates across both merge paths"

# Scan's timeout/cancellation handling has produced three real, confirmed
# bugs across rounds 2-4 of review, entirely because nothing exercised any
# of it: the "Scan smoke test" above never times out against a healthy local
# server. Forcing couchbase.scan_timeout absurdly low makes every scan fail
# for real, asserting Scan reports that cleanly (SCAN_ERROR, not a hang or a
# panic) instead of only being checked by inspection.
#
# This exercises Scan's outer resultCh/scanCtx.Done() race and its overall
# "never hang" contract - real coverage that was previously completely
# missing - but NOT specifically the resHandle force-close branch
# (db/couchbase/db.go's Scan, the "case res := <-resHandle" arm): reaching
# that branch requires col.Scan() to have already produced a live
# *gocb.ScanResult before scanCtx's deadline fires, and empirically, at a
# timeout this short, gocb's own internal Timeout option (set to the same
# value) resolves the whole call as an error before a *ScanResult ever
# exists, every time. The resHandle branch only matters for the specific
# pathological case documented in Scan's doc comment - gocb's result stream
# stalling past its own configured timeout under concurrent scan load -
# which, like the multi-node ErrScopeNotFound race tested informally
# elsewhere in this file, cannot be reliably forced on demand in a fast,
# deterministic CI test; it was found and fixed via real load, not
# reproduced synthetically.
echo "==> [scan timeout] couchbase.scan_timeout=1ms forces a real Scan failure - must fail cleanly, not hang"
set +e
OUT=$(run_phase run workloads/workload_template -p table=usertable \
  -p recordcount="$RECORDCOUNT" -p operationcount=20 -p threadcount=1 \
  -p readproportion=0 -p updateproportion=0 -p scanproportion=1 -p maxscanlength=10 \
  -p couchbase.scan_timeout=1ms 2>&1)
STATUS=$?
set -e
if [ "$STATUS" -ne 0 ]; then
  echo "FAIL: run phase exited non-zero ($STATUS) - expected go-ycsb to complete cleanly and report SCAN_ERROR ops, not crash or hang:"
  echo "$OUT"
  exit 1
fi
if ! echo "$OUT" | grep -q 'SCAN_ERROR'; then
  echo "FAIL: expected couchbase.scan_timeout=1ms to force at least one SCAN_ERROR (exercising Scan's timeout path), got:"
  echo "$OUT"
  exit 1
fi
echo "OK: scan_timeout=1ms correctly forced the timeout path (SCAN_ERROR present), process completed without hanging"

echo "==> couchbase integration test passed"
