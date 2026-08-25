FDB_CHECK := $(shell command -v fdbcli 2> /dev/null)
ROCKSDB_CHECK := $(shell echo "int main() { return 0; }" | gcc -lrocksdb -x c++ -o /dev/null - 2>/dev/null; echo $$?)
SQLITE_CHECK := $(shell echo "int main() { return 0; }" | gcc -lsqlite3 -x c++ -o /dev/null - 2>/dev/null; echo $$?)

TAGS =

ifdef FDB_CHECK
	TAGS += foundationdb
endif

ifneq ($(shell go env GOOS), $(shell go env GOHOSTOS))
	CROSS_COMPILE := 1
endif
ifneq ($(shell go env GOARCH), $(shell go env GOHOSTARCH))
	CROSS_COMPILE := 1
endif

ifndef CROSS_COMPILE

ifeq ($(SQLITE_CHECK), 0)
	TAGS += libsqlite3
endif

ifeq ($(ROCKSDB_CHECK), 0)
	TAGS += rocksdb
	CGO_CXXFLAGS := "${CGO_CXXFLAGS} -std=c++11"
	CGO_FLAGS += CGO_CXXFLAGS=$(CGO_CXXFLAGS)
endif

endif

default: build

build: export GO111MODULE=on
build:
ifeq ($(TAGS),)
	$(CGO_FLAGS) go build -o bin/go-ycsb cmd/go-ycsb/*
else
	$(CGO_FLAGS) go build -tags "$(TAGS)" -o bin/go-ycsb cmd/go-ycsb/*
endif

check:
	golint -set_exit_status db/... cmd/... pkg/...

# Runs the feature-store workload's load+run phases against dockerized Redis
# and MongoDB, failing on any op error or unexpected op count. Same target
# locally and in CI (see .github/workflows/integration.yml).
test-integration-feature-store:
	./test/integration/feature_store.sh

# Runs db/cassandra/db.go's TLS support against a dockerized, TLS-enabled
# ScyllaDB node, asserting both that a connection using the correct CA
# succeeds AND that one using an unrelated CA is rejected - the second
# assertion is what catches a TLS config that encrypts but never actually
# verifies the server. Same target locally and in CI.
test-integration-cassandra-tls:
	./test/integration/cassandra_tls.sh

# Runs db/aerospike/db.go's TLS support against a dockerized Aerospike
# Community Edition node behind a TLS-terminating proxy (Community Edition
# has no native TLS support of its own - it's Enterprise-only), asserting
# on connection outcome: the correct CA connects, an unrelated CA is
# rejected, and skip.verify explicitly bypasses that rejection. Same target
# locally and in CI.
test-integration-aerospike-tls:
	./test/integration/aerospike_tls.sh

# Runs db/couchbase/db.go's core and feature-store load+run phases, plus a
# Scan smoke test and an auto_create_collection=false negative check, against
# a dockerized Couchbase Community Edition node. Same target locally and in
# CI. The adapter's TLS path (couchbases://, for Capella) has no CE
# equivalent to test in CI - Community Edition has no native TLS - and was
# instead verified by hand against a live Capella cluster; see the PR
# description that introduced this adapter for that verification.
test-integration-couchbase:
	./test/integration/couchbase.sh

# Unit tests for db/couchbase specifically (not `go test ./...` - this repo
# doesn't run that in CI repo-wide, see CONTRIBUTING.md - but these guard
# real, previously-shipped bugs found by adversarial review: a silent
# plaintext-TLS downgrade, a subdocument-path field-name injection, and
# Update's branch-selection logic) so they run alongside the Docker
# integration test on every push/PR instead of only if a contributor
# remembers to run them locally.
test-couchbase:
	go test ./db/couchbase/...

