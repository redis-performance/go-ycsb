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
- Go 1.20 or later (`go version` to check)
- Optional: FoundationDB client library, RocksDB, or libsqlite3 for those database backends (the Makefile detects them automatically)

To install dependencies only:

```bash
go mod download
```

To build without make:

```bash
go build -o bin/go-ycsb cmd/go-ycsb/main.go
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