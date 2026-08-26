#!/usr/bin/env bash
#
# Integration test for db/cosmosdb/db.go against a real, dockerized Azure
# Cosmos DB (vNext) Linux emulator.
#
# The emulator publishes a well-known account key (documented by Microsoft,
# not a secret: https://learn.microsoft.com/azure/cosmos-db/emulator-linux)
# and, in this preview image, serves plain HTTP rather than HTTPS - this
# adapter's cosmosdb.insecure_skip_verify exists for a real HTTPS
# self-signed-cert scenario and isn't needed against this specific image.
#
# Runs the core workload (exercises Scan, a cross-partition SQL query given
# this adapter's partition-key-per-record design) and the feature-store
# workload (50 fields, writeallfields=true - forces every Update through the
# Read+merge+Replace fallback path, since that's over Cosmos DB's 10-op
# PatchItem cap), then a direct field-preservation regression check and a
# negative auto_create_container=false check.
#
# Same script for local dev and CI: by default it starts (and tears down)
# its own disposable emulator container, so `test/integration/cosmosdb.sh`
# and the CI job run identically.
#
# Usage:
#   test/integration/cosmosdb.sh
#
# Env overrides:
#   RECORDCOUNT       number of records/entities to load (default: 2000)
#   OPERATIONCOUNT    number of ops in the run phase (default: 3x RECORDCOUNT)
#   THREADCOUNT       client concurrency (default: 8)
#   COSMOSDB_IMAGE    docker image for the emulator
#                      (default: mcr.microsoft.com/cosmosdb/linux/azure-cosmos-emulator:vnext-preview)
#   START_CONTAINER   whether to start/stop the container (default: true)
#   COSMOSDB_ENDPOINT cosmosdb.endpoint to use when START_CONTAINER=false
#   COSMOSDB_KEY      cosmosdb.key to use when START_CONTAINER=false
#
# RECORDCOUNT/OPERATIONCOUNT default far lower than the other adapters'
# integration tests: the emulator's per-op latency (tens of ms - it's not
# tuned for throughput, and every Scan is a cross-partition query by this
# adapter's own partition-key design) makes a 20000-record run noticeably
# slower here than the same size is against Couchbase/Redis/Mongo locally.

set -euo pipefail

RECORDCOUNT=${RECORDCOUNT:-2000}
OPERATIONCOUNT=${OPERATIONCOUNT:-$((RECORDCOUNT * 3))}
THREADCOUNT=${THREADCOUNT:-8}
COSMOSDB_IMAGE=${COSMOSDB_IMAGE:-mcr.microsoft.com/cosmosdb/linux/azure-cosmos-emulator:vnext-preview}
START_CONTAINER=${START_CONTAINER:-true}
# Well-known, publicly-documented emulator key - not a real secret.
COSMOSDB_KEY=${COSMOSDB_KEY:-'C2y6yDjf5/R+ob0N8A7Cgv30VRDJIWEHLM+4QDU5DE2nQ9nDuVTqobD4b8mGGyPMbIZnqyMsEcaGQy67XIw/Jw=='}
COSMOSDB_ENDPOINT=${COSMOSDB_ENDPOINT:-http://127.0.0.1:8081}
COSMOSDB_DATABASE=${COSMOSDB_DATABASE:-ycsb}

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$ROOT_DIR"

CONTAINER=go-ycsb-it-cosmosdb
# Docker (in particular a snap-packaged docker daemon) can only bind-mount
# paths under $HOME, so this deliberately lives under the repo checkout
# rather than /tmp - see test/integration/cassandra_tls.sh for the same note.
# Nothing here is actually bind-mounted into the container, but the
# field-preservation probe program below is kept in-repo-tree for
# consistency with the other integration tests' WORK_DIR convention.
WORK_DIR="$ROOT_DIR/.cosmosdb-it"

cleanup() {
  if [ "$START_CONTAINER" = "true" ]; then
    echo "==> stopping container"
    docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
  fi
  rm -rf "$WORK_DIR"
}
trap cleanup EXIT

rm -rf "$WORK_DIR"
mkdir -p "$WORK_DIR"

if [ "$START_CONTAINER" = "true" ]; then
  echo "==> starting $COSMOSDB_IMAGE"
  docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
  docker run -d --name "$CONTAINER" -p 8081:8081 -p 1234:1234 "$COSMOSDB_IMAGE" >/dev/null

  echo "==> waiting for the emulator gateway"
  for _ in $(seq 1 60); do
    if [ "$(curl -s -o /dev/null -w '%{http_code}' "$COSMOSDB_ENDPOINT/" 2>/dev/null)" = "200" ]; then
      break
    fi
    sleep 2
  done
fi

echo "==> building go-ycsb"
make >/dev/null

run_phase() {
  local phase=$1 workload=$2
  shift 2
  ./bin/go-ycsb "$phase" cosmosdb -P "$workload" \
    -p cosmosdb.endpoint="$COSMOSDB_ENDPOINT" -p cosmosdb.key="$COSMOSDB_KEY" \
    -p cosmosdb.database="$COSMOSDB_DATABASE" -p cosmosdb.auto_create_container=true \
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

# A dedicated, deliberately tiny Scan check: every Scan is a cross-partition
# SQL query given this adapter's partition-key-per-record design (see
# db/cosmosdb/db.go's Scan doc comment) - correct, but slower per-op than a
# point read, so this stays small rather than running at RECORDCOUNT scale.
echo "==> [core workload, table=usertable] Scan smoke test (50 scan ops)"
OUT=$(run_phase run workloads/workload_template -p table=usertable \
  -p recordcount="$RECORDCOUNT" -p operationcount=50 -p threadcount=4 \
  -p readproportion=0 -p updateproportion=0 -p scanproportion=1 -p maxscanlength=10)
echo "$OUT" | tail -5
check_output "$OUT" TOTAL 50

echo "==> [feature-store workload] load ($RECORDCOUNT entities)"
OUT=$(run_phase load workloads/workload_feature_store)
echo "$OUT" | tail -5
check_output "$OUT" INSERT "$RECORDCOUNT"

echo "==> [feature-store workload] run ($OPERATIONCOUNT ops, forces Update's Read+merge+Replace fallback: 51 fields > the 10-op PatchItem cap)"
OUT=$(run_phase run workloads/workload_feature_store)
echo "$OUT" | tail -5
check_output "$OUT" TOTAL "$OPERATIONCOUNT"

echo "==> [negative] cosmosdb.auto_create_container=false against a nonexistent container must fail, not hang or silently succeed"
set +e
OUT=$(./bin/go-ycsb load cosmosdb -P workloads/workload_template \
  -p cosmosdb.endpoint="$COSMOSDB_ENDPOINT" -p cosmosdb.key="$COSMOSDB_KEY" \
  -p cosmosdb.database="$COSMOSDB_DATABASE" -p cosmosdb.auto_create_container=false \
  -p table=does_not_exist_and_wont_be_created -p recordcount=5 -p operationcount=5 -p threadcount=1 2>&1)
set -e
if ! echo "$OUT" | grep -q 'INSERT_ERROR'; then
  echo "FAIL: expected INSERT_ERROR against a nonexistent container with auto_create_container=false, got:"
  echo "$OUT"
  exit 1
fi
echo "OK: auto_create_container=false correctly rejected writes to a nonexistent container"

# Direct regression guard for the exact data-corruption bug class
# db/couchbase/db.go's Update hit in this repo's history (a full-document
# replace silently dropping every field not included in the call): load a
# document, hammer it with single-field Updates (the PatchItem fast path,
# <=10 ops), then read the raw stored document back via a standalone probe
# program that calls azcosmos.ReadItem directly - bypassing this adapter's
# Update() entirely, so a bug there can't be masked by anything Update
# itself does - and assert every original field is still present.
echo "==> [field preservation] Update() must not drop fields it wasn't asked to change"
FC_TABLE=fieldpreservetest
FC_FIELDCOUNT=10
OUT=$(run_phase load workloads/workload_template -p table="$FC_TABLE" -p fieldcount="$FC_FIELDCOUNT" \
  -p insertorder=ordered -p recordcount=1 -p operationcount=1 -p threadcount=1)
echo "$OUT" | tail -5
check_output "$OUT" INSERT 1
OUT=$(run_phase run workloads/workload_template -p table="$FC_TABLE" -p fieldcount="$FC_FIELDCOUNT" \
  -p insertorder=ordered -p recordcount=1 -p operationcount=30 -p threadcount=1 \
  -p readproportion=0 -p updateproportion=1)
echo "$OUT" | tail -5
check_output "$OUT" TOTAL 30

cat > "$WORK_DIR/probe.go" <<EOF
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos"
)

func main() {
	cred, err := azcosmos.NewKeyCredential("$COSMOSDB_KEY")
	if err != nil {
		panic(err)
	}
	client, err := azcosmos.NewClientWithKey("$COSMOSDB_ENDPOINT", cred, nil)
	if err != nil {
		panic(err)
	}
	container, err := client.NewContainer("$COSMOSDB_DATABASE", "$FC_TABLE")
	if err != nil {
		panic(err)
	}
	res, err := container.ReadItem(context.Background(), azcosmos.NewPartitionKeyString("user0"), "user0", nil)
	if err != nil {
		panic(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(res.Value, &doc); err != nil {
		panic(err)
	}
	systemFields := map[string]bool{"id": true, "_rid": true, "_self": true, "_etag": true, "_attachments": true, "_ts": true}
	count := 0
	for k := range doc {
		if !systemFields[k] {
			count++
		}
	}
	fmt.Println(count)
	if count != $FC_FIELDCOUNT {
		os.Exit(1)
	}
}
EOF

got_fields=$(cd "$ROOT_DIR" && go run "$WORK_DIR/probe.go")
if [ "$got_fields" != "$FC_FIELDCOUNT" ]; then
  echo "FAIL: expected all $FC_FIELDCOUNT fields to survive 30 single-field updates, found $got_fields"
  exit 1
fi
echo "OK: all $FC_FIELDCOUNT fields survived 30 single-field updates"

echo "==> cosmosdb integration test passed"
