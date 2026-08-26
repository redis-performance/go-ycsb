# Voice profiles — redis-performance/go-ycsb, and why this section is short

Mined from actual GitHub data (`gh pr list --state all`, `gh api .../pulls/<n>/reviews`, `.../pulls/<n>/comments`,
`.../issues/<n>/comments`) across all 27 PRs in this fork's history as of 2026-08-26. Unlike the memtier_benchmark
version of this skill, there is no rich, multi-person, dialogic review corpus here to mine into real voice
profiles. This file says exactly what is and isn't in the data — do not read more into it than is written here.

## The only reviewer: paulorsousa

5 reviews total, on PRs #15–#19 — all docs/CI/Docker-Hub-publish chore PRs (`Add CONTRIBUTING.md and AGENTS.md`,
`publish Docker image`, `bump GitHub Actions to node24-compatible versions`, a since-closed Docker registry
migration attempt, and a README badge PR). Every single review is `APPROVED`, with an **empty body**, and there
are **zero inline review comments** from this account anywhere in the fork's history. That is the entire
external review signal this repository has ever produced.

There is no tone, register, or nitpick pattern to describe here beyond "approves quickly, writes nothing." Do
not invent quotes or a personality for this account. If a task asks you to write "as paulorsousa would," the
honest answer is: a silent `APPROVED`, nothing else — there is no text in the record to imitate beyond that.

## The other ~22 merged PRs: no review recorded at all

`CONTRIBUTING.md` states "at least one maintainer approval is required before merge," but the majority of this
fork's merged PRs (roughly 22 of 27) have **no review object recorded** via the GitHub API at all — not a
`COMMENTED` review, not an `APPROVED` with no body, nothing. Either these were merged without the stated gate
being enforced, or approval happened through some channel this data doesn't capture. Either way: don't assume
the "one maintainer approval" rule is reliably enforced in practice here, and don't cite it as proof that a
given historical PR was actually reviewed by someone.

## Author conventions: fcostaoliveira / filipecosta90 — one person, two accounts, all 27 PRs

Every PR in this fork's history was authored by the same person under two GitHub identities (both display as
"Filipe Oliveira" — one Redis-affiliated, one personal). This is a contributor-side convention, not a
maintainer's *reviewing* voice, but it's the closest thing to an institutional writing style this fork has, and
it's useful for judging what a well-formed contribution here normally looks like:

- PR bodies consistently use a `## Summary` section (bulleted, specific) and often a `## Test plan` checklist
  section with real, concrete verification steps — not just "tested locally."
- Verification frequently goes beyond local Docker: e.g. PR #25 (couchbase) reports a real Couchbase Capella
  cluster test (created via the Capella API, deleted immediately after), and PR #22 (cassandra TLS) reports
  verification against a live ScyllaDB Cloud cluster, in addition to the Docker-based integration test that
  actually runs in CI.
- PR bodies cite specific prior PR numbers as precedent for follow-on work — e.g. PR #23's body opens with
  "same gap the cassandra driver had before its TLS fix (#22)."
- Several PR bodies are explicitly marked "🤖 Generated with Claude Code."
- The one issue-level comment in this fork's entire history (on PR #18) is the same author explaining, to
  themselves, why they're closing their own PR — "Closing — registry migration is not the right fix. Stale
  Docker Hub secret will be updated directly."

## Honest bottom line

If asked to review "in the voice of a go-ycsb maintainer," the honest answer this data supports is: terse,
silent by default on anything routine and clearly described (matching `paulorsousa`'s only observed behavior),
and prepared to cite this fork's own written standards (`AGENTS.md`/`CONTRIBUTING.md`) — with an honest caveat
that those standards are documented policy, not something ever seen enforced against a real PR — when something
falls short of them. There is no richer personality, no recorded disagreement, no back-and-forth to draw on.
Manufacturing one would misrepresent this fork's actual history, which is the one thing this skill must not do.
