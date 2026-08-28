# Onboarding a source

`stages:` in `conveyor.yaml` is the state machine. What a stage *does* is
per-source, in `conveyor.d/<source>/<stage>` beside the config.

`conveyor.d/_source/` here is the starting template. Copy it, named for the
source you are enrolling, then edit it:

```bash
cp -r examples/conveyor.d/_source ~/codes/conveyor.d/midgame
```

Then add the source to your `conveyor.yaml` with its `REPO` and label mapping.
`conveyor validate` names any `work:` stage the source has not implemented; a
source missing a script is skipped and reported, never quietly given generic
behaviour.

## Why not inside the repo being worked

The scripts operate on a repo but do not live in it. `in-progress` tells an
agent to commit and push — an untracked `.conveyor/` sitting in that repo is
exactly what `git add -A` sweeps into the pull request. Keeping them beside the
config also means enrolling somebody else's repository writes nothing into it.

Nothing is lost by moving them out: the script still *runs* in the source's
workdir, so `claude` there still resolves that repo's own `.claude/skills/`.
`Script` and `Workdir` are separate things.

## What is in the template

| | |
| --- | --- |
| `refining` | Claude rewrites the issue into something implementable, then stops. |
| `in-progress` | Claude implements it and opens a PR. |
| `testing` | Codex reviews that PR — deliberately not the model that wrote it. |

The prompts are inline rather than in skills, so a freshly enrolled source works
with nothing installed. Move one into the repo's `.claude/skills/` when it
outgrows the script; conveyor neither knows nor cares which you did.

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
