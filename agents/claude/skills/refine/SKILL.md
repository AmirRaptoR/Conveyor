---
name: refine
description: Use when turning a single GitHub issue into a complete, implementable specification - the user gives an issue number and wants requirements clarified, acceptance criteria written, and the issue description filled in. Triggers on "/refine", "refine issue N", "flesh out #N", "write the spec for #N".
---

# Refine

Take one GitHub issue from rough note to implementable spec. Ask questions until it is genuinely clear, have Codex check the result once, write the spec into the issue description.

**Input:** an issue number. If the user did not give one, ask for it — do not guess from the backlog.

## The Iron Rules

1. **Read-only. Always.** Refinement never touches a file in the repo, never creates a branch, never opens a PR, never commits. Reading code to understand the problem is fine. Writing anything to the working tree is not. Codex runs with `-s read-only` for the same reason. Filing sibling issues and editing issue bodies/labels via `gh` is refinement's job, not "touching the repo" — none of it lands in the working tree.
2. **Never touch `conveyor:*` labels.** The stage label and the `conveyor:blocked` mark are the pipeline's own bookkeeping — `move` manages them by shape, not refinement.
3. **Original text is preserved** verbatim under `## Original request` at the bottom of the new body (the parent's, if this issue ends up split).
4. **Do not write a spec you are not sure of.** If it cannot be refined, label it `blocked` and stop. A confident-sounding wrong spec is worse than no spec.
5. **Codex gets exactly one round.** No back-and-forth, no second pass.
6. **A parent (split) issue is never itself implementable.** No acceptance criteria a PR could close, no `conveyor:*` stage label, and nothing in its body that reads as a closing keyword next to its own number — not even in a sentence denying it (`"does not close #101"` still matches a PR-body closing-keyword scanner; say "does not finish" or "is not the last PR for" instead). Only children get built and closed.

## Workflow

### 1. Load

```bash
gh issue view N --json number,title,body,labels,url,comments
```

Read the comments — half the requirements usually live there. Then read enough of the repo to know what the issue is actually touching.

### 2. Attended or unattended?

Check whether `AskUserQuestion` is available to you.

- **Available → attended.** A human can answer in real time. Proceed normally: ask real questions, get a real approval before writing, get a real yes before splitting.
- **Not available → unattended**, as in conveyor's `refining` stage, which deliberately does not grant that tool. Nobody is going to answer anything, ever, for this run. Every "ask" below becomes "decide it yourself from the codebase and the issue's own text, and log the call" — question, answer, and the reasoning, one row each, into a `## Refinement record` table you post as a comment in step 7. This is not a lower bar for the spec — an unattended run still owes acceptance criteria "verifiable without asking." It changes who resolves each ambiguity, not how many get resolved or how well.

### 3. Q&A until clear

Loop — live, batching 3-5 questions per `AskUserQuestion` call if attended; self-answered and logged per §2 if not — until every item below is answerable without guessing:

| Must be clear | Why |
|---|---|
| The problem, in the user's terms | Everything else follows from it |
| What is in scope | Prevents scope drift during implementation |
| What is explicitly out of scope | The half that never gets written down |
| Acceptance criteria, each independently verifiable | "Done" must not be a judgment call |
| Which module owns it | Assigns the work |
| Dependencies — other issues, services, decisions | Reveals blockers before coding starts |
| Failure and edge behavior | The part that gets discovered in prod |

**The 90% bar, concretely:** you are at 90% when every acceptance criterion could be handed to someone who has never seen the issue and they could verify it without asking you anything. Not a feeling — check the criteria one by one.

**If this issue's own text, its parent, or the codebase says it is sequenced behind another issue, write `Depends on #N` naming the immediate predecessor** — into the body, not only into "Technical notes" prose. The pipeline reads that exact line (`agents/_deps`, and the GitHub provider's own listing) to hold this item behind the one it names; recording it here is what makes an already-known dependency enforced instead of merely documented. Only the immediate predecessor: a chain of siblings each naming the one before it needs no closure computed. This is not the same job as inferring an unstated order across a family of sibling issues nothing declares — that is a later, separate stage's business.

Ask about **requirements**, not implementation. "What should happen when the token is already expired?" is refinement. "Should we use Redis or Postgres for this?" is design — only ask (or self-answer) if the answer changes the acceptance criteria.

Keep looping while questions remain. Do not stop at "clear enough".

**If it cannot get clear** — the user does not know yet, it depends on an unmade decision, it needs external input that reading the codebase cannot supply:

```bash
gh label create blocked --color B60205 --description "Cannot be refined yet" 2>/dev/null
gh issue edit N --add-label blocked
gh issue comment N --body "Refinement blocked: <one line why>. Open: <the questions>"
```

Then stop. Do not write a partial spec. `blocked` is a plain label — it is not in the `conveyor:*` namespace and does not replace whatever stage the issue carries. This is the genuine escape hatch for "even reading the codebase can't answer this" — do not reach for it just because the run is unattended; §2 exists precisely so unattended runs don't need it for ordinary requirement ambiguity.

### 4. Size check — before drafting anything

Decide whether this issue is actually N deliverables wearing one number. **Oversized** means the scope repeats a similar unit of work — games, endpoints, migrations, screens, tenants, integrations — more than about 3-4 times, such that no single PR could plausibly close the whole issue under this repo's normal review size. That is a different question from "touches many files": one coherent change across a large codebase is not oversized; the same small change done N times over independent units is.

If **not** oversized, skip to step 5.

If oversized:

- **Attended:** propose the batch boundaries to the user and get their sign-off before drafting anything further. This is the one place in this skill that still needs a live yes even once §2 would otherwise let you self-answer — the batches are a structural decision about what gets built when, not a requirement detail buried in the issue.
- **Unattended:** decide the batches yourself, same self-answer-and-record discipline as §2, then act immediately, before any spec gets drafted:
  1. **This issue becomes the parent — a tracking issue, not a deliverable.** It keeps its Problem statement and Out-of-scope/Technical-notes framing, but gets no acceptance criteria of its own, no `conveyor:*` label, and no PR is ever meant to close it (Iron Rule 6). If it currently carries a `conveyor:*` stage label, remove it — a parent is not a unit of work for the pipeline to pick up.
  2. **File one child issue per batch**, each drafted through the normal template in step 5 as a complete, self-contained spec — its own Problem/Scope/Acceptance criteria, sized so one PR can close it. Reference the parent in each child's body as `Parent: #N` (never a closing keyword). Onboard children to conveyor normally (`conveyor:backlog`, or whatever stage matches work already in flight for that slice).
  3. **Link every child to the parent with GitHub's native sub-issue relationship**, not just the text mention — it's what drives the parent's progress bar:
     ```bash
     gh api -X POST repos/O/R/issues/<parent>/sub_issues -F sub_issue_id=$(gh api repos/O/R/issues/<child> -q .id)
     ```
  4. **If a PR already exists against the parent** (an implementer got ahead of a refine that should have split earlier): make the child whose scope matches that PR's actual diff, retarget the PR's body to `Closes #<that child>` instead of the parent, and give that child the `conveyor:*` label matching wherever the PR already sits in the pipeline (e.g. `conveyor:review`), so the pipeline resumes there instead of restarting refinement on it.
  5. Post the batch rationale — why these boundaries, what's already done vs. not — as part of the `## Refinement record` comment on the parent in step 7.
- **Discovered mid-draft:** if you only realize the scope is oversized after starting to draft one spec, stop drafting the monolith right there and restart at this step — do not finish and publish a spec you already know no single PR can close. Never fall back to writing permissive language like *"land the framework plus a first batch in one PR and the rest in follow-ups, as long as everything is done before this issue is closed"* into a single issue's spec — that sentence describes exactly the shape (partial PRs against one un-splittable issue) that has no mechanism to ever be marked done. If the work is really meant to land in slices, that's the definition of oversized: split it, don't narrate it.

### 5. Draft the spec

For the issue itself if not split, or for each child if it was:

```markdown
## Problem
What is wrong or missing, and who it affects. Two to four sentences.

## Scope
What this issue covers. Bullets.

## Out of scope
What it deliberately does not cover, and where that work lives instead.

## Acceptance criteria
- [ ] Independently verifiable statement
- [ ] Another one
- [ ] Edge case / failure behavior

## Technical notes
Constraints, affected components, relevant existing code paths, dependencies on other issues.

## Open questions
Anything still unresolved that does not block starting. Empty section means none — say "None".
```

Complete and thorough. Every acceptance criterion is checkable. No "should work well", no "handle errors properly".

### 6. Codex review — one round

Write the draft to a temporary file, then:

```bash
spec_file=$(mktemp)
codex_out=$(mktemp)
cat > "$spec_file" <<'EOF'
<the drafted spec>
EOF

codex exec -s read-only -C "$(git rev-parse --show-toplevel)" \
  -o "$codex_out" \
  "Review this specification for GitHub issue #N against the actual codebase. \
Do not modify any file. Report only: (1) requirements that contradict or duplicate existing code, \
(2) missing acceptance criteria or unhandled edge cases, (3) ambiguities an implementer would have to guess at. \
Be specific and cite file paths. If the spec is sound, say so. \
Spec follows:

$(cat "$spec_file")"
```

Read `"$codex_out"`. Judge each point — Codex is a reviewer, not an authority. Wrong points get dropped, silently.

**Merging its output:**
- A point that is clearly right and unambiguous → fold into the spec, and note it in the changelog comment.
- A point that is right but adds a requirement the user has not agreed to → add under `## Codex review` as an open item, do not silently expand scope.
- A point needing a user decision → if attended, ask it in the approval step; if unattended, self-answer and log it per §2 — do not start a new Q&A loop either way.

```markdown
## Codex review
- Codex: `auth/session.go:88` already implements the refresh path — criterion 3 may be redundant.
- Codex: no criterion covers concurrent refresh from two tabs.
```

Always attribute with the `Codex:` prefix. Never present its findings as your own.

### 7. Write

If attended: show the final body, get one approval, then write. If unattended: there is no one to approve — write directly, and lean on the record comment (below) as the after-the-fact check a human can overrule.

```bash
orig_file=$(mktemp)
body_file=$(mktemp)
gh issue view N --json body -q .body > "$orig_file"
{ cat "$spec_file"; printf '\n\n---\n\n## Original request\n\n'; cat "$orig_file"; } > "$body_file"
gh issue edit N --body-file "$body_file"
```

Do this once per issue that got a spec — the parent (if split, its Problem/Out-of-scope framing only, per Iron Rule 6) and every child.

If the title no longer describes the refined scope, propose a new one (attended) or just set it (unattended, logged).

### 8. After

- **Refinement record** — for an unattended run, post the full self-answered Q&A (and the size-check decision, if it split) as a comment on the parent or issue: question, answer, and the one-line basis for each, so a human can overrule any of it. For an attended run, post the Q&A transcript the same way — the reasoning behind the spec goes on the record either way.
- **Re-check labels** — refinement often changes what a thing is. Propose `module:`/`priority:`/`type:` corrections. Never `conveyor:*`. A parent issue keeps none of the `conveyor:*` labels (Iron Rule 6).

## Red flags — stop

- About to edit a file, create a branch, or open a PR → you are refining, not implementing. Stop.
- Ran `codex exec` without `-s read-only` → stop, rerun correctly
- Second Codex round → one round only
- Writing a spec while questions remain unanswered (and unattended can't resolve them from the codebase) → label `blocked` instead
- An acceptance criterion you cannot describe how to verify → it is not a criterion
- Presenting a Codex point without attribution → attribute it
- Answering your own question to keep momentum when attended → ask the user instead; unattended self-answering (§2) is the sanctioned exception, not an excuse to skip asking when you could
- A parent issue with acceptance criteria, a `conveyor:*` label, or a sentence that reads as a closing keyword next to its own number → fix it, Iron Rule 6
- Writing "land in slices... before this issue is closed" into a single, un-split issue → that is the size check (§4) telling you to split, not a sentence to write down

## Quick reference

| Task | Command |
|---|---|
| Load issue + comments | `gh issue view N --json number,title,body,labels,url,comments` |
| Codex review, read-only | `codex exec -s read-only -o OUT.md "PROMPT"` |
| Set body | `gh issue edit N --body-file FILE` |
| Mark blocked | `gh issue edit N --add-label blocked` |
| Strip a parent's conveyor label | `gh issue edit N --remove-label "conveyor:<stage>" --remove-label conveyor` |
| Link child to parent (native sub-issue) | `gh api -X POST repos/O/R/issues/<parent>/sub_issues -F sub_issue_id=$(gh api repos/O/R/issues/<child> -q .id)` |
| Transcript / refinement record | `gh issue comment N --body-file FILE` |
