# Install

Clone to a running board, on a machine that has never seen Conveyor before.
This is the checklist #70 exists to make possible — everything a shipped
stage depends on either lives in this repository already, or is named here
with the one command that proves it is present.

Nothing here installs a prerequisite. An installer that reaches for a package
manager is a different, riskier program than this one; prerequisite commands
below only check, and the release installer only packages this checkout.

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

## Development sequence: clone to a running board

This sequence runs from a checkout and is intentionally reported as a `dev`
build on the board. Use the immutable production deployment below for an
unattended service; a checkout can change underneath a running process and is
not a release boundary.

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

`providers:` and `agents:` need nothing set here: unset, both resolve beside
the *binary* first (falling back to beside the config only if that lookup
finds no directory there), and `./conveyor` throughout this sequence is the
one built in step 2, inside this checkout — where the config being kept
outside the checkout does not change that. They only need pointing back at
this checkout for a `conveyor` binary copied somewhere without this
checkout's `providers/`/`agents/` alongside it — see the comment at the top
of `conveyor.example.yaml`.

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

### 7. Keep it running

Do not point a long-running service at this mutable checkout. Continue with the
production sequence below; its service unit and deploy command make the health
check and rollback part of every release.

## Production: immutable releases with automatic rollback

`deploy/install-release` packages the binary, `agents/` and `providers/` into
one read-only directory under `/opt/conveyor/releases`. It writes a manifest
containing the build revision, supported config schema, SHA-256 digest and
permission bits of every executable asset. Setting `CONVEYOR_RELEASE_DIR`
makes the process verify that manifest before it reads the config or starts a
server. A missing, edited or mixed-version file is therefore a startup error,
not a partly upgraded board.

Keep configuration and engine data outside the release tree. One conventional
layout is:

```text
/opt/conveyor/
  current  -> releases/<revision>
  previous -> releases/<previous-revision>
  releases/<revision>/{conveyor,release.json,agents/,providers/}
/var/lib/conveyor/
  conveyor.yaml
  data/
```

Prepare the directories and config once, granting the service/deploy user the
ownership appropriate for this machine:

```bash
sudo install -d -o conveyor -g conveyor /opt/conveyor/releases /var/lib/conveyor
sudo install -o conveyor -g conveyor -m 0600 conveyor.github.example.yaml /var/lib/conveyor/conveyor.yaml
sudo -u conveyor $EDITOR /var/lib/conveyor/conveyor.yaml
```

From a clean checkout of the revision to deploy:

```bash
CONVEYOR_PREFIX=/opt/conveyor \
CONVEYOR_CONFIG=/var/lib/conveyor/conveyor.yaml \
CONVEYOR_SERVICE=conveyor \
  deploy/install-release
```

The installer performs these operations in order:

1. Build into a staging directory with the Git revision embedded.
2. Copy and hash all shipped agents and providers, then make the tree read-only.
3. Run the staged binary's own manifest check and validate the real config.
4. Rename the complete staging directory into `releases/` and atomically swap
   `current`.
5. Restart the service and poll `/api/state`. HTTP 200 or an authentication
   challenge (401) proves the board is serving.
6. If restart or health checking fails, atomically restore the old `current`,
   restart it, and return failure. On success the old target is retained as
   `previous`.

The defaults match `deploy/conveyor.service.example`. Set `CONVEYOR_ADDR` when
the service listens somewhere other than `127.0.0.1:8090`. Automated deployment
systems may set `CONVEYOR_HEALTHCHECK` to an executable that accepts the state
URL; the built-in check uses `curl`. `CONVEYOR_SYSTEMCTL` similarly selects the
service-control executable, primarily for non-systemd test environments.

Install the unit after filling in its `User=` and PATH:

```bash
sudo cp deploy/conveyor.service.example /etc/systemd/system/conveyor.service
sudo $EDITOR /etc/systemd/system/conveyor.service
sudo systemctl daemon-reload
sudo systemctl enable --now conveyor
```

The masthead and `/api/state.release` show the active revision, manifest schema,
config schema and canonical release directory. In production the badge must say
`release`, not `dev`.

### Migrating an existing checkout-based service

Do not delete or alter the old checkout installation during the first deploy.
Copy its config to `/var/lib/conveyor/conveyor.yaml`, remove no `agents:` or
`providers:` keys yet (release mode safely overrides both), run the installer,
then switch the unit to the example above. This makes migration reversible:
the old unit and binary remain untouched until the new service passes its
health check.

To manually roll back later, repoint `current` atomically and restart:

```bash
old=$(readlink -f /opt/conveyor/previous)
sudo ln -s "$old" /opt/conveyor/.current.rollback
sudo mv -Tf /opt/conveyor/.current.rollback /opt/conveyor/current
sudo systemctl restart conveyor
```

Each release is read-only and content-verified, so never edit one in place.
Deploy a new revision instead. Old release directories may be removed only
after confirming neither `current` nor `previous` points to them.

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
  step, and it is one you run yourself; `deploy/install-release` only packages
  that local clean checkout.
