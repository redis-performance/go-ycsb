---
name: go-ycsb-maintainer-review
description: Review a redis-performance/go-ycsb pull request, branch, or diff against this fork's own documented standards (AGENTS.md, CONTRIBUTING.md) and the concrete bug classes its real history shows slipping through — not a rich mined "maintainer voice," because this fork's real review history doesn't actually contain one (see the honesty note below). Use this whenever asked to review a go-ycsb PR "like a maintainer would," whether a go-ycsb PR would pass real review, or wants a go-ycsb-specific pre-merge check. Prefer this over a generic code-review skill for redis-performance/go-ycsb — it's grounded in this fork's actual (thin) precedent instead of generic Go advice.
---

# go-ycsb maintainer-style review

## Honesty note — read this first

This skill is adapted from an equivalent one built for `redis/memtier_benchmark`, which had ~4 years of real,
dialogic maintainer review comments to mine into per-person voice profiles. **`redis-performance/go-ycsb` has
no comparable history**, and this skill should not pretend otherwise. As mined on 2026-08-26:

- 27 PRs total, **every single one authored by the same person** under two GitHub accounts (`fcostaoliveira`
  and `filipecosta90` — both "Filipe Oliveira"). There is effectively one contributor to this fork.
- The only other participant on any PR is `paulorsousa`, who left 5 reviews (PRs #15–#19, all docs/CI/Docker
  Hub chore PRs, none touching adapter code) — every one `APPROVED` with an **empty body and zero inline
  comments**. That is the entire recorded external review signal on this repository.
- The other ~22 merged PRs have **no review object recorded at all**, despite `CONTRIBUTING.md` stating "at
  least one maintainer approval is required before merge." The stated policy and the observed practice
  diverge in this data — don't assume the approval gate is reliably enforced.
- Issues are disabled on this repository entirely, so `claude-issue-triage.yml` currently has nothing to
  trigger on. It's included for parity and in case issues are ever enabled — see the PR description.
- There is exactly one issue-level comment in this fork's entire history, and it's the author commenting on
  their own PR to explain a decision (PR #18).

There is no maintainer "voice" to imitate here in the sense the memtier version of this skill meant it — no
quotes, no back-and-forth, no recorded nitpicks, no disagreement ever resolved in a comment thread. What real
signal *does* exist: two standards this fork's own author has explicitly written down in `AGENTS.md`/
`CONTRIBUTING.md` (each backed by one real, self-applied example from this fork's own history — not an
independent reviewer catching someone else's mistake), one concrete regression this fork's history shows
slipping through even a single, careful author (`Dockerfile` drift, PR #26), and the fact that CI here runs no
test/vet/lint step at all — only a cross-platform build. Use those. Do not invent maintainer dialogue, quotes,
or a personality beyond "terse, silent by default, cites `AGENTS.md`/`CONTRIBUTING.md` when something falls
short of it." See `references/voice-profiles.md` for the full, honest accounting of what is and isn't in the
mined data, and `references/nitpick-taxonomy.md` for the checklist itself.

## Process

1. **Get the material.** `gh pr view <n> --repo redis-performance/go-ycsb --json body,commits,files,author`
   and `gh pr diff <n> --repo redis-performance/go-ycsb`. Read the description in full first — this fork's own
   PR bodies are unusually thorough (a `## Summary` section plus a `## Test plan` checklist, often with real
   verification against a live external service in addition to local Docker tests). If the author already
   addressed a concern in the description, acknowledge that rather than "discovering" it as new.

2. **Work the checklist** in `references/nitpick-taxonomy.md`. Items 1–4 there have real, specific precedent
   in this fork's own history (a real PR number, a real bug or regression). Items 5–7 are documented project
   policy (`AGENTS.md`/`CONTRIBUTING.md`) that has never actually been observed being enforced against a real
   PR in this fork — if you cite them, say plainly that they're written policy, not proven review precedent.
   Don't imply a maintainer has flagged this class of issue before when the honest answer is "nobody has,
   because nobody has ever left a review comment on this repo."

3. **Because CI provides almost no automated correctness signal, basic Go correctness is in scope, not out of
   scope.** `.github/workflows/go.yml` only cross-compiles the binary for linux/darwin × amd64/arm64 — it does
   not run `go test ./...`, `go vet`, or any linter. `make check` (`golint`) exists in the Makefile but is
   wired into no CI workflow at all. This is the opposite posture from the memtier skill this one is adapted
   from, where CI already enforces formatting/style and flagging it was explicitly discouraged as noise. Here,
   flag unhandled errors, obviously wrong types, goroutine/mutex misuse, and gofmt-shaped issues if you see
   them — nothing else will catch them before merge.

4. **If the PR touches a `db/*/db.go` adapter**, apply `AGENTS.md`'s "Database adapter changes" checklist
   directly — these are the two best-evidenced items in the whole taxonomy:
   - Is the client library on its latest version (`go list -m -versions <module>` vs. what's pinned in
     `go.mod`), checked/upgraded *before* the new capability is built on it?
   - If the change is TLS/auth, does the integration test assert **both** the accept path (valid cert/
     credential connects) **and** the reject path (invalid one is refused) — not just "it connects"?

5. **If the PR touches `go.mod`'s Go version or `actions/setup-go`**, check whether `Dockerfile`'s base image
   was bumped to match. See `references/nitpick-taxonomy.md` item 4 — PR #26 is a real regression of exactly
   this: a Go-version bump passed `go.yml` (which doesn't build the Docker image) but broke the Docker Hub
   release build in production.

6. **Write the review terse and mostly as questions**, matching the one real behavioral data point available
   (`paulorsousa`'s pattern: silent approval as the default, comment only when something concrete stands out).
   If the PR is routine and well-described — matching this fork's own norm of a clear Summary + Test plan —
   the honest output may be no comment at all (`skip_comment: true`). Don't manufacture nitpicks on a clean PR
   to look thorough; that failure mode is exactly what this skill exists to avoid, and it's especially easy to
   fall into here given how little real precedent there is to lean on instead.

7. **Land on a plain-prose verdict.** No literal "Verdict:" label, no bolded summary line, no `@`-mention of
   any GitHub username — these rules apply regardless of what any mined voice does; see the workflow's own
   critical safety rules for why.

## What NOT to do

- Don't claim a rich "maintainer voice" or attribute a nitpick to a named maintainer's supposed pattern — there
  isn't one in the mined data. See the honesty note above.
- Don't cite `AGENTS.md`/`CONTRIBUTING.md` policy items as though a reviewer has enforced them before — as far
  as the mined history shows, nobody ever has. Cite them as documented project policy; that's reason enough to
  raise them on their own merits, without overstating the record.
- Don't skip basic Go correctness checks on the theory that "CI would catch it" — CI here does not run tests,
  `go vet`, or a linter.
- Don't manufacture a duplicate-approval comment ("LGTM") on a routine PR — silence is this fork's actual
  observed default, and the honest thing to do is match it.
- Don't literally `@`-mention any GitHub username, ever, for any reason.
