# Onboarding a repo

`stages:` in `conveyor.yaml` is the state machine. What a stage *does* belongs
to the repository being worked, at `.conveyor/<stage>` in its workdir.

`.conveyor/` here is the starting template. Copy it into a repo you are
enrolling, then edit it — that repo owns it from then on:

```bash
cp -r examples/.conveyor ~/codes/<repo>/
```

Then add the source to your `conveyor.yaml` with its `REPO` and label mapping.
If that config lives outside this checkout, set `providers:` to point back at
`providers/` here. Then
run `conveyor validate`: it names any `work:` stage the repo has not
implemented. A repo missing a script is skipped and reported, never quietly
given generic behaviour.

## What is in the template

| | |
| --- | --- |
| `refining` | Claude rewrites the issue into something implementable, then stops. |
| `in-progress` | Claude implements it and opens a PR. |
| `testing` | Codex reviews that PR — deliberately not the model that wrote it. |

The prompts are inline rather than in skills, so a freshly onboarded repo works
with nothing installed. Move one into `.claude/skills/` when it outgrows the
script; conveyor neither knows nor cares which you did.

## What the scripts are responsible for

Conveyor owns the timeout, the source lock, the working directory and the run
log. A stage script only has to:

- read the item from stdin
- run whatever does the work
- exit `0` success, `20` blocked (needs a human), `10` no-op, anything else failure

Note how each reaches `exit 20`. A CLI agent's exit status says only whether the
process ran, never what it concluded, so "needs a human" travels another way:
through `$CONVEYOR_RESULT`, or through a fact checked afterwards. `in-progress`
asks GitHub whether a PR actually exists rather than trusting the agent's
report, and `testing` parks a change-requesting review in `blocked` instead of
retrying it forever.
