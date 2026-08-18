# Contributing

We treat this repo as "Open Source" within Redis: anyone who clears the bar below is welcome to contribute.

## Local setup

```bash
git clone git@github.com:redis-performance/go-ycsb.git
cd go-ycsb

# Build the binary (outputs to bin/go-ycsb)
make

# Give it a try
./bin/go-ycsb --help
```

Requirements:
- Go 1.20 or later (`go version` to check)
- Optional: FoundationDB client library, RocksDB, or libsqlite3 for those database backends (the Makefile detects them automatically)

## Branch naming

```
<type>/<short-description>
```

Types: `feat`, `fix`, `refactor`, `test`, `docs`, `chore`

Example: `feat/add-pipeline-mode`

## Coding standards

- Keep changes focused; one logical change per PR.
- Follow the conventions already present in the codebase (formatting, naming, error handling).
- No dead code, no commented-out blocks.

## Submitting changes

1. Fork or create a branch from `master`.
2. Make your changes with clear, atomic commits.
3. Open a pull request against `master` with a descriptive title and summary.
4. Address review comments promptly; force-push to the same branch to update.

## Testing

- All new behaviour must be covered by tests.
- Existing tests must pass: run the test suite locally before opening a PR.
- Coverage should not decrease.

Run the full test suite with:

```bash
go test ./...
```

Note: CI (`.github/workflows/go.yml`) cross-compiles the binary for linux/darwin on amd64/arm64 but does not run the test suite — running `go test ./...` locally before opening a PR is the contributor's responsibility.

## Review process

- At least one maintainer approval is required before merge.
- CI must be green (the cross-platform build must succeed).
- Maintainers may request changes or close PRs that do not meet the bar — this is normal and not personal.