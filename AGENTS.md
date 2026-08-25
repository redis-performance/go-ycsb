# Agent guidelines

Instructions for AI coding agents (Claude Code, Copilot, Cursor, etc.) working in this repo.

## Project overview

go-ycsb is a Go port of the Yahoo Cloud Serving Benchmark (YCSB). It supports all standard YCSB generators and the Core workload, enabling CRUD performance benchmarking across multiple databases — with a particular focus on Redis and Redis Cluster. The tool provides load and run subcommands, configurable workload files, and pluggable database backends (Redis, MySQL, PostgreSQL, MongoDB, Cassandra, DynamoDB, and more).

## Local setup

```bash
git clone git@github.com:redis-performance/go-ycsb.git
cd go-ycsb

# Build the binary (outputs to bin/go-ycsb)
make

# Verify the build
./bin/go-ycsb --help
```

Requirements:
- Go 1.24 or later (`go version` to check)
- Optional: FoundationDB client library, RocksDB, or libsqlite3 for those database backends (the Makefile detects them automatically)

To install dependencies only:

```bash
go mod download
```

To build without make:

```bash
go build -o bin/go-ycsb cmd/go-ycsb/
```

## Branch naming

Same as human contributors: `<type>/<short-description>` (e.g. `fix/off-by-one-in-pipeline`).

## Coding standards

- Match the style already in the file you are editing.
- Prefer clear, minimal changes over large refactors unless explicitly asked.
- Do not add comments that describe *what* the code does — only add comments when the *why* is non-obvious.
- Do not introduce new dependencies without checking with the maintainer.

## Running tests

```bash
go test ./...
```

Always run tests before declaring a task complete.

## Database adapter changes

Any change to a `db/*/db.go` adapter (a new capability like TLS/auth, a protocol change, etc.) must do both of the following — not optional, and not just "if convenient":

1. **Check the client library is on its latest version before building on it.** Run `go list -m -versions <module>` (or check the module's releases) against what's pinned in `go.mod`. If it's stale, upgrade it first, as its own commit, before adding the new capability — see `db/aerospike/db.go`'s history (PR #23) for an example: the pinned client was 6 major versions and 8 years old, and the TLS work was built on the upgraded client, not patched onto the old one.
2. **Add a Docker-based integration test wired into CI**, following the existing pattern in `test/integration/*.sh` + the matching `make test-integration-*` target + a job in `.github/workflows/integration.yml`. For a TLS/auth change specifically, the test must assert on BOTH directions — a valid credential/cert is accepted AND an invalid one is rejected — not just "it connects". See `test/integration/cassandra_tls.sh` (PR #22): a version of that adapter's TLS support shipped with certificate verification silently disabled, and a test that only checked "TLS connects" would have passed against that broken code; the wrong-CA-must-be-rejected assertion is what actually catches this bug class.

## How to submit changes

1. Create a branch: `git checkout -b <type>/<description>`.
2. Commit with a clear message focused on *why*, not *what*.
3. Open a pull request against `master`.
4. Do **not** push directly to `master`.

## What to avoid

- Do not reformat files unrelated to your change.
- Do not remove error handling or tests.
- Do not commit secrets, credentials, or large binary files.
- Do not amend published commits.