# Cross-cutting checklist — redis-performance/go-ycsb, honestly graded by evidence strength

8 categories. **Items 1–4 have real, specific precedent** (a real PR number, a real bug or regression found in
this fork's own history). **Items 5–7 are documented project policy** (`AGENTS.md`/`CONTRIBUTING.md`) that has
never been observed being enforced against an actual PR — this repo has zero recorded review comments, so
there is no "a maintainer once flagged this" to cite honestly. Item 8 is a real, useful, but single-sample
signal from one PR's self-described process — treat it accordingly. See `voice-profiles.md` for why the
evidence base here is this thin.

1. **Client library must be current before a new capability is built on it.** Real precedent: PR #23 upgraded
   `aerospike-client-go` from v1.35.2 (2018) to `/v7` v7.10.2 *before* adding TLS support, because — in the
   PR's own words — "patching TLS onto an 8-year-old client ahead of a real load didn't make sense." This is
   now codified in `AGENTS.md`'s "Database adapter changes" section: run `go list -m -versions <module>`
   against what's pinned in `go.mod`; if stale, upgrade it first, as its own commit. One real, self-applied
   example — not an independent reviewer catching someone else's stale dependency.

2. **A TLS/auth capability's integration test must assert both the accept AND the reject path.** Real
   precedent: PR #22 added Cassandra TLS support; `AGENTS.md` explicitly notes that an earlier iteration of
   this class of change "shipped with certificate verification silently disabled," and only an assertion that
   an *invalid* CA is actually rejected — not just "a valid one connects" — would have caught it. This is the
   single strongest, most concrete real bug-class precedent in this fork's history. Check any TLS/auth-adjacent
   PR's test script for both directions, not just the happy path.

3. **New/changed adapter behavior should follow the existing integration-test pattern.** Real, repeated
   precedent across PR #21 (feature-store), #22 (cassandra-tls), #23 (aerospike-tls), and #25 (couchbase): a
   `test/integration/<name>.sh` script that stands up (and tears down) its own disposable Docker container(s),
   a matching `make test-integration-<name>` target, and a job in `.github/workflows/integration.yml`. A new
   adapter capability that doesn't follow this pattern is inconsistent with everything else in the fork's
   actual history, not just a style preference.

4. **Dockerfile's Go base image must track go.mod's Go version / `actions/setup-go`.** Real regression: PR #26
   — PR #25's `go.mod` bump (to Go 1.24, transitively required by a new dependency) was reflected in
   `go.yml`'s `actions/setup-go` version but missed in `Dockerfile`'s `golang:1.20-alpine3.16` base image. This
   passed `go.yml` CI (which only cross-compiles the binary, never builds the Docker image) and broke the
   Docker Hub release build in production instead. Any PR touching `go.mod`'s Go version should be checked
   against `Dockerfile` directly — CI will not catch a mismatch here.

5. **CI provides almost no automated correctness signal — treat that as raising the bar on manual review, not
   lowering it.** `go.yml` only cross-compiles for linux/darwin × amd64/arm64; it runs no `go test ./...`, no
   `go vet`, no linter. `make check` (`golint`) exists in the Makefile but is wired into neither `go.yml` nor
   `integration.yml` — it only runs if a contributor remembers to invoke it locally. `CONTRIBUTING.md` says as
   much explicitly ("CI... does not run `go test ./...` — that remains the contributor's responsibility
   locally"). Practically: a PR with no test-plan/verification section in its description has essentially zero
   automated backstop, and basic Go correctness (unhandled errors, obviously wrong types, goroutine/mutex
   misuse, gofmt-shaped issues) is worth checking directly rather than assumed to be covered elsewhere.

6. **No dead/commented-out code, no unrelated reformatting, no new dependency without flagging it.** Verbatim
   from `AGENTS.md`/`CONTRIBUTING.md`: "Do not add comments that describe *what* the code does — only add
   comments when the *why* is non-obvious"; "Do not introduce new dependencies without checking with the
   maintainer"; "No dead code, no commented-out blocks"; "Do not reformat files unrelated to your change."
   Documented policy, not observed precedent — there is no recorded instance of a reviewer citing these against
   a real PR. Raise them as project policy on their own merits, not as "you're repeating a mistake others made."

7. **Comments should explain "why," not "what."** Same status as item 6 — a written `AGENTS.md` rule, never
   seen enforced in a real review comment because this fork has no recorded review comments at all.

8. **Known real bug classes in this codebase, from one PR's own self-described adversarial pass.** PR #21's
   description says a "9-agent adversarial review" the author ran against their own change found 8 issues
   before merge: an overflow panic, a silent typo-fallback, silent no-ops, a `set -e` bug in a test shell
   script, and doc drift; the same PR also fixed a pre-existing off-by-one in the core workload's zipfian
   key-range chooser. This is real and useful — it tells you what kinds of bugs actually occur in this
   codebase — but it is the PR author's own account of their own self-review process on a single PR, not an
   independent reviewer's catch, and it's a sample size of one. Worth checking for (silent fallbacks on
   unrecognized config values, off-by-one errors in generators/range logic, `set -e` correctness in new shell
   test scripts, integer overflow on counters), but don't cite it as "this project has a policy against X" —
   it's one instance of one author catching their own mistakes, not institutional doctrine.
