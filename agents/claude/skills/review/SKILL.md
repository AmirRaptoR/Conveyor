---
name: review
description: Use when a refined and implemented issue has an open PR that needs reviewing - the user gives an issue number and wants Codex review and the findings judged and fixed. It does not merge; the approving stage does that. Triggers on "/review", "review issue N", "review the PR for #N".
---

# Review

One issue with an open PR → Codex review → fixes → hand it on.

**This does not merge.** Merging is a gate, not a judgement: checks green, no
hold, nothing unresolved, quiet for a while. That is `agents/claude/approve` in
the `approving` stage, and it runs after this one. Leaving the PR open is the
successful outcome here, and it is also what gives the pull request a window in
which a person's comment can still change something — a review that merged at
the end of its own run never had one.

**Input:** an issue number. If the user did not give one, ask for it — do not guess from the open PRs.

## The Iron Rules

1. **Acceptance criteria are the bar.** The question is whether the diff satisfies the criteria in the issue, not whether you would have written it that way.
2. **Four questions, in this order.** Acceptance criteria, architecture, test coverage, correctness. Nothing else.
   - **Criteria** — is each one actually met, or only partly?
   - **Architecture** — does this belong where it was put? Does it fit the boundaries the codebase already has, or does it reach across them, duplicate something that exists, or put a decision in a layer that cannot make it?
   - **Test coverage** — is there a test that *fails* if this breaks? Untested behaviour is unfinished behaviour, and a test that passes either way is worse than none.
   - **Correctness** — does it introduce a bug? Name the concrete failing input or state.

   Still ignore style, naming and formatting entirely. Architecture is not taste: "I would have named this differently" is style, "this reaches into another module's storage" is architecture.
3. **Codex is a reviewer, not an authority.** Judge every finding. A wrong one is not applied, and the reason goes on the record.
4. **Never leave red.** An unmet criterion, a failing local suite or red CI is not finished work — say so and stop, rather than handing it on.
5. **Never force-push.** Fixes are commits on the existing branch.
6. **Three rounds, then stop.** Three without convergence is a disagreement that needs a human, not a fourth round.

## Workflow

### 1. Find the PR

```bash
gh issue view <N> --json title,body,state
gh pr list --search "<N>" --state open --json number,headRefName,url
```

No open PR is not a review failure — it means the work has not been delivered
yet. Say so and stop; do not open one.

Check out the branch and confirm the tree is clean before touching anything.

### 2. Pull the criteria

Take the acceptance criteria from the issue body. They are the review's spec:
every finding is either "criterion N is unmet" or "this is a correctness bug".
If the issue has no criteria, say so and stop — there is nothing to review
against, and that is a refinement problem.

### 3. Wait for CI

```bash
gh pr checks --watch
```

Red CI is a finding like any other, and it is yours to fix. If the repo has no
CI, the local full suite is the bar.

**Run only the suites the diff can reach.** A repository whose trees share no
build — a .NET backend and a React frontend, say — cannot have one broken by a
change confined to the other, and running both costs the stage minutes it does
not have. Check what actually changed:

```bash
gh pr diff <PR> --name-only
```

Skip a suite only when **every** changed file sits inside one isolated tree.
Anything shared — build config, deploy, a lockfile, a root file — and you run
everything. The repo's own CLAUDE.md or AGENTS.md names its trees; if it names
none, run everything.

Say in the round comment which suite you skipped and why, so the record shows
what was and was not exercised.

**When in doubt, run it.** The asymmetry is the whole argument: a suite run
needlessly costs minutes, and a suite skipped wrongly costs a merge — `approving`
lands this branch on the strength of a review that never executed the code. Do
not extend this to subsystems *within* a tree. "The change touches no migration
so the database tests are safe to skip" is exactly the reasoning that breaks:
these are integration tests where the database is the substrate, not the
subject, and application logic breaks them with no schema change in sight.

### 4. Review round — at most 3

**Round 1 reviews the whole diff. Every round after it reviews only what changed
since the round before.** Everything earlier was already judged and either fixed
or dismissed on the record; sending it again buys a second opinion on a question
already answered, and costs the same as the first. This is not a small saving —
a single Codex round has been measured at ten minutes on a real PR, a third of
the stage's whole budget, so three full-diff rounds cannot fit however fast the
tests are.

Record the head you reviewed, and use it as the base for the next round:

```bash
reviewed=$(git rev-parse HEAD)          # after round 1's fixes are pushed
codex exec review --base "$reviewed" "..."   # round 2 sees only the new commits
```

Ask Codex first — round 1 against the PR's base branch, later rounds against
`$reviewed`:

```bash
codex exec review --base <main | $reviewed> \
  "Review this PR against these acceptance criteria:

<criteria>

Report ONLY these four things, in this order:
1. Criteria — does the change actually satisfy each one? Name any unmet or partially met.
2. Architecture — does this belong where it was put? Flag anything that crosses a
   module boundary, duplicates something that already exists, or puts a decision in
   a layer that cannot make it.
3. Test coverage — is there a test that FAILS if this behaviour breaks? Name any
   behaviour here that no test would catch, and any test that would pass either way.
4. Correctness — does it introduce a bug? Give the concrete failing input or state.

Ignore style, naming and formatting entirely. Judge architecture, not taste.
If all four are clean, say so."
```

**If Codex is unavailable** — not installed, not authenticated, or it errors —
review the diff yourself against the same two questions and the same
exclusions. Say plainly in the PR comment that this round was self-reviewed, so
the record shows no second opinion was obtained. A self-review is weaker
evidence: you are judging code you may have just written.

For each finding, judge it:

- **Valid** → fix it, commit, push. A correctness bug gets its own test first.
- **Invalid** → do not fix it, and write one line of why.

Post the round on the record before starting the next:

```bash
gh pr comment <PR> --body "Review round 1 (codex) — reviewed through $reviewed:
- <finding> — fixed in abc1234
- <finding> — not applied: <why>"
```

Naming the sha you reviewed through is what makes the next round cheap, and it
survives you: a run that is killed part-way leaves the record on the PR, so
whoever picks the item up next starts from that commit instead of reviewing the
whole thing again.

Then re-run the review over the commits since that sha. Repeat until clean, to a
maximum of three rounds.

**After 3 rounds without convergence**, post what remains disputed, leave the PR
open, and stop. Do not merge.

### 5. Hand it on

Requires all of: full local suite green, `gh pr checks` green, review clean,
every criterion ticked. Then **stop, with the PR open**. Do not merge.

The one thing to check before you finish: the PR body must say `Closes #N`.
That is the link that ties the work to the item, and the `approving` stage finds
the pull request by exactly that reference — a PR without it is a PR the gate
cannot see. Add it if it is missing; never close the issue by hand.

What happens next, so you can leave it confidently: `approving` re-checks the
PR on every poll and merges it once it is not a draft, has no conflict, has
green checks, carries no `conveyor:blocked` hold, has no unresolved review
threads, and has been quiet for ten minutes. Any new comment or push resets that
clock.

## Red flags — stop

- **No open PR** → the work is not delivered. Not your problem to fix.
- **No acceptance criteria in the issue** → nothing to review against. Send it back to refinement.
- **A finding you cannot reproduce** → do not "fix" it. Say you could not reproduce it and why.
- **Wanting a fourth round** → that is a human decision, not another loop.
- **Tempted to merge** → not this stage. If a PR looks ready, that is exactly the state to hand on.
- **Tempted to widen scope** → a review fixes what it found. Anything else is a new issue.

## Quick reference

```bash
gh pr list --search "<N>" --state open      # find it
gh pr checks --watch                        # CI must be green
codex exec review --base main "..."         # round 1: the whole diff
codex exec review --base "$reviewed" "..."  # rounds 2-3: only what changed since
gh pr comment <PR> --body "..."             # every round on the record
# no merge here: `approving` does that once the PR has settled
```
