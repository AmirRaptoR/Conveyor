---
name: implement
description: Use when building a refined GitHub issue - the user gives an issue number and wants a branch, TDD implementation against its acceptance criteria, and a PR ready for review. Review and merge are /review. Triggers on "/implement", "implement issue N", "build #N".
---

# Implement

One refined issue → branch → TDD → PR ready for review.

Review, fixes and merge are `/review`, which takes the same issue number.

**Input:** an issue number.

## The Iron Rules

1. **Acceptance criteria are the spec.** Every test traces to one. No test without a criterion, no criterion without a test.
2. **Never edit a test to make it pass.** A red test is information. If the test is wrong, say why and fix it deliberately — not to get to green.
3. **YAGNI.** Build what the criteria require. Not the abstraction you'd want if there were three more criteria.
4. **Never force-push. Never merge.** Merging is `/review`.
5. **A criterion that turns out impossible stops the work.** Do not reinterpret it into something achievable — ask.

## Workflow

### 1. Preflight

```bash
gh issue view N --json number,title,body,labels,url
git status --porcelain   # must be empty
```

- **No acceptance criteria in the body?** Stop. Run `/refine N` first — implementing an unrefined issue is guessing.
- **Dirty tree?** Stop and ask.
- Detect the test runner from the repo (package.json scripts, Makefile, pyproject, go.mod). Confirm the command that runs the full suite before relying on it.

### 2. Branch

**Defer to the branch you were handed.** Several checkouts of one repository can be live at once, each an item's own worktree — `main` may be checked out elsewhere entirely, and switching branch here moves this item's work onto whatever the working tree was already pointed at. Do not run `git checkout main`, `git switch`, or `git checkout -b`, by any command or delegation. Confirm the branch instead:

```bash
git branch --show-current
```

If you were launched already on a branch for this issue — in that issue's own worktree, on a rework from the pull request's branch, or otherwise placed there by whatever invoked this skill — stay on it and continue to step 3. Only when nothing has assigned a branch at all (an ad hoc, unmanaged checkout, invoked outside any pipeline) do you create one yourself, from the default branch, never switching a checkout that is already on something else:

```bash
git checkout -b feature/42-session-expiry-redirect
```

Type comes from the issue's `type:` label — `type:bug` → `bugfix`, `type:feature` → `feature`, `type:chore` → `chore`. Slug: 2-4 words, lowercase, hyphenated, from the title. Not the whole title.

### 3. TDD, criterion by criterion

For each acceptance criterion, in order:

1. **Red** — write the test that fails for the right reason. Run it. *See it fail.* A test that passes on first write is testing nothing; find out why before continuing.
2. **Green** — the minimum code that makes it pass. Not the general case. Not the extensible version.
3. **Commit** — `git commit -m "<criterion, as a sentence>"`, one commit per criterion.

After the first commit, push and open the draft PR (step 4), then keep going. The rest of the work lands on a visible branch.

**Do not** batch all the tests up front and then implement. Red-green-commit, one criterion at a time.

**If a criterion cannot be implemented as written** — it contradicts another, or the codebase makes it impossible — stop, state the conflict concretely, and ask. Do not pick the nearest achievable thing.

### 4. Draft PR, early

After the first commit:

```bash
git push -u origin HEAD
gh pr create --draft --title "<issue title>" --body "$(cat <<'EOF'
Closes #42

## Acceptance criteria
- [ ] criterion one
- [ ] criterion two
EOF
)"
```

Tick criteria off as their commits land.

### 5. Full suite, then ready

Before marking ready, run the **entire** existing suite, not just the new tests:

```bash
<full suite command>
gh pr ready
```

A regression found here is cheap. Found in review, it is not.

### 6. Wait for CI

```bash
gh pr checks --watch
```

Red CI that passes locally is a real failure — fix it, do not rerun hoping. If the repo has no CI, the local full suite is the bar.

### 7. Hand off

Mark the PR ready and stop. Review, fixes and merge belong to `/review`, which
takes the same issue number:

```bash
gh pr ready
gh pr view --json number,url
```

Leave `Closes #N` in the PR body — the merge that `/review` performs is what
closes the issue, and the link is what ties the work to the item.

**Do not review your own work here, and do not merge.** A separate reviewer is
the point: the model that wrote the code is the worst judge of whether it meets
the criteria.

## Delegate the mechanical parts

Spawn a subagent on a **faster model** when the job is high-volume, independent, and needs little context. A subagent starts cold and re-derives everything, so it only pays when the work is bigger than the briefing.

| Job shape | Model | Why |
|---|---|---|
| Same mechanical judgment over many independent items | `haiku` | Volume, no cross-item reasoning |
| Bounded contained task, some judgment | `sonnet` | Cheaper than opus, good enough |
| Needs the whole picture, or the items interact | keep it yourself | Splitting it loses the picture |

**Never delegate:** anything where the answer depends on items in another agent's chunk, and anything you would have to explain at length to hand over.

**In implementation:** exploration fans out, the TDD cycle does not.

- **Fan out** — codebase questions before you start ("where is session state written?", "what test helpers exist?"). Use the `Explore` agent, or `sonnet`, one per question, in parallel.
- **Keep yourself** — the whole red-green-commit cycle. Criteria look independent and are not: they touch the same files, and parallel agents produce conflicting edits and duplicated helpers. One agent, one branch, one criterion at a time.
- **`sonnet` is reasonable** for a self-contained chore whose criteria touch one file and need no repo-wide judgment.

Reviewing is not delegated from here at all — it is `/review`, a separate
skill run by a separate invocation.

## Red flags — stop

- Editing a test so it passes → stop, that is the one thing tests are for
- Writing code before the failing test → delete it, write the test
- A test that passed the moment you wrote it → find out why before moving on
- Adding an interface, factory, or config value no criterion asked for → YAGNI, cut it
- Reinterpreting a criterion to make it achievable → ask instead
- `git push --force` → never
- Merging with any check red → never
- A 4th Codex round → escalate instead
- Implementing an issue with no acceptance criteria → `/refine` first
- Running `git checkout main`, `git switch`, or `git checkout -b` on a checkout that was already handed a branch → stop, that is another item's work landing on this one

## Quick reference

| Task | Command |
|---|---|
| Issue | `gh issue view N --json number,title,body,labels` |
| Confirm branch | `git branch --show-current` |
| Branch (only if none was handed to you) | `git checkout -b feature/42-short-slug` |
| Draft PR | `gh pr create --draft --title "..." --body "Closes #42 ..."` |
| Mark ready | `gh pr ready` |
| Watch CI | `gh pr checks --watch` |
| Comment | `gh pr comment --body "..."` |
| Hand off | `gh pr ready` — then `/review <N>` |
