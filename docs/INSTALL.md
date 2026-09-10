# Install

Clone to a running board, on a machine that has never seen Conveyor before.
This is the checklist #70 exists to make possible — everything a shipped
stage depends on either lives in this repository already, or is named here
with the one command that proves it is present.

Nothing here installs a prerequisite. An installer that reaches for a package
manager is a different, riskier program than this one; every command below
only checks.

## Prerequisites

### Required

Every one of these is load-bearing for the pipeline this repository ships —
without any single one, `./check` or the sequence below fails at a named step
rather than later, unexplained.

| Prerequisite | Why | Prove it |
| --- | --- | --- |
| Go | builds the engine | `go version` |
| git | every stage's worktree, every source's checkout | `git --version` |
| bash | every provider and agent script | `bash --version` |
| `gh`, authenticated | every GitHub provider script and agent shells out to it | `gh auth status` |
| `jq` | every script parses the item and `$CONVEYOR_RESULT` with it | `jq --version` |
| `claude` CLI | the shipped `agents/claude/*` adapters run it headlessly | `claude --version` |

### Optional, tied to a capability

Skip whichever of these this box does not need — nothing below the line
depends on all of them at once.

| Prerequisite | Needed for | Prove it |
| --- | --- | --- |
| `codex` CLI | `agents/codex/review`, `agents/codex/status` | `codex --version` |
| Node.js | the UI test suite (`*.test.mjs`), run by `./check` | `node --version` |
| systemd | running the engine as a service (`deploy/conveyor.service.example`) | `systemctl --version` |
| Caddy | TLS in front of the board, required for Web Push (a secure origin) | `caddy version` |

## Sequence: clone to a running board

Each step names the command that proves it worked before moving to the next.

### 1. Clone

```bash
git clone https://github.com/AmirRaptoR/Conveyor.git
cd Conveyor
```

### 2. Build

```bash
go build -o conveyor ./cmd/conveyor
./conveyor -h   # prints usage — the binary runs
```

### 3. Install the skills

The shipped `agents/claude/*` adapters invoke `/refine`, `/implement` and
`/review` as slash commands; `claude` only finds those if they are installed
where it looks for skills, `~/.claude/skills` (see README.md, "Onboarding a
source" — the pipeline's own skills use the user-level path, not a worked
repository's).

```bash
agents/claude/install-skills
agents/claude/install-skills --check   # every line should read: installed <name>
```

### 4. Configure a real source

`conveyor.example.yaml` is the zero-dependency demo — its only source is
`mock`, and it needs nothing from this step. `conveyor.github.example.yaml`
is the template for a real repository: copy it outside the checkout (a
working config is machine state, never committed — see CLAUDE.md,
"Gotchas"), then fill in its two placeholders.

```bash
mkdir -p ~/codes
cp conveyor.github.example.yaml ~/codes/conveyor.yaml
$EDITOR ~/codes/conveyor.yaml
#   workdir: /path/to/a/real/checkout   ->  an actual local clone of the repo
#   REPO: OWNER/REPO                    ->  that repo's owner/name on GitHub
```

Filling in a real repository and onboarding its labels is #41's job (guided
enrollment) or an operator's by hand — never this file's or the installer's.
`providers/github/onboard.sh` creates the labels a repository needs in one
idempotent command, if you are doing it by hand:

```bash
REPO=OWNER/REPO STAGE_LABELS="$(cat <<'EOF'
refining=conveyor:refining
ready=conveyor:ready
in-progress=conveyor:in-progress
review=conveyor:review
approving=conveyor:approving
EOF
)" providers/github/onboard.sh
```

Because `conveyor.github.example.yaml` sits inside this checkout, its
`providers:` and `agents:` keys resolve automatically. A config kept outside
the checkout (as copied above) needs `providers:` pointed back at it — see
the comment at the top of `conveyor.example.yaml`.

### 5. Validate

```bash
./conveyor validate -c ~/codes/conveyor.yaml
```

A source that cannot run (a bad `workdir:`, a missing script) is reported by
name here, not discovered later as a stage that silently never runs.

### 6. Run it

```bash
./conveyor serve -c ~/codes/conveyor.yaml -addr 127.0.0.1:8090
```

Confirm it worked from another terminal:

```bash
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8090/
```

`200` or `401` is a pass — the board is answering, whether or not this config
set `auth.users` (`401` means it did, and is asking; see README.md,
"Configuration"). Anything else is not running yet.

### 7. Optional: run as a service

```bash
sudo cp deploy/conveyor.service.example /etc/systemd/system/conveyor.service
sudo $EDITOR /etc/systemd/system/conveyor.service   # fill in User, paths, PATH
sudo systemctl daemon-reload
sudo systemctl enable --now conveyor
systemctl status conveyor
```

See README.md's "Post-deploy check: `conveyor probe`" for verifying a restart
actually left the board reachable, and for what `Restart=always` does and
does not cover.

## Everything this does not do

- Does not install Go, git, `gh`, `jq`, `claude`, `codex`, Node, systemd or
  Caddy — only checks for them.
- Does not enforce a version or that `gh`/`claude`/`codex` are authenticated —
  `gh auth status` is the command that proves it; nothing here gates on the
  result.
- Does not fill in a real repository's `workdir:`/`REPO:`, onboard its labels
  one issue at a time, or validate that a particular source is correctly
  configured beyond `conveyor validate` — that is #41 (guided enrollment,
  supervised first run) or an operator's, by hand.
- Ships no binary, no `curl | sh`, no package. `go build` is the only build
  step, and it is one you run yourself.
