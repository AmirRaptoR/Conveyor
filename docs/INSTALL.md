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
| `flock` | serializes production release installation | `flock --version` |
| `claude` CLI | the shipped `agents/claude/*` adapters run it headlessly | `claude --version` |

### Optional, tied to a capability

Skip whichever of these this box does not need — nothing below the line
depends on all of them at once.

| Prerequisite | Needed for | Prove it |
| --- | --- | --- |
| `codex` CLI | `agents/codex/review`, `agents/codex/status` | `codex --version` |
| OpenCode CLI **1.18.32** | `agents/opencode/{refine,implement,review,approve}` | `opencode --version` |
| Node.js | the UI test suite (`*.test.mjs`), run by `./check` | `node --version` |
| systemd | running the engine as a service (`deploy/conveyor.service.example`) | `systemctl --version` |
| Caddy | TLS in front of the board, required for Web Push (a secure origin) | `caddy version` |

OpenCode is pinned exactly because `opencode run --format json` is a CLI event
projection rather than a versioned protocol. Disable its auto-update and install
the supported version before enabling an OpenCode script:

```bash
OPENCODE_DISABLE_AUTOUPDATE=1 opencode upgrade 1.18.32 --method curl
opencode --version                         # must print 1.18.32
opencode auth list                         # the intended provider is connected
```

### OpenCode canary

Agent selection remains a source-script choice; the engine has no OpenCode
configuration. Canary one stage in one source by changing only that script
entry:

```yaml
scripts:
  refine:
    agent: opencode
    params:
      MODEL: openai/gpt-5.6-sol
      VARIANT: high
      AGENT: build
      PROMPT: "/refine $REF"
```

`MODEL` is required and uses `provider/model`; `VARIANT` is optional and names
that model's reasoning variant; `AGENT` defaults to `build` and must resolve to
a primary OpenCode agent. `conveyor preflight` runs
`agents/opencode/status` without a model call: it verifies the exact CLI pin,
resolved config, local readiness API, connected providers and primary agents.
OpenCode has no live quota endpoint, so the probe says that explicitly and
reports `limited` only after an adapter receives a structured, explicit quota
refusal. The cache expires after 15 minutes by default and a successful run
clears it immediately.

Run the canary through one complete item before changing refine, implement,
review or approve for another source. Rollback is one config edit, with no data
migration and no service downgrade:

```yaml
scripts:
  refine:
    agent: claude
    params:
      MODEL: claude-sonnet-5
      PROMPT: "/refine $REF"
```

Restart Conveyor after either edit so one process has one resolved adapter
configuration. Existing OpenCode session ids remain harmless opaque item data;
the Claude adapter cannot resume them and therefore leads with the recorded
answer plus the original prompt, which is the normal cross-agent fallback.

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

Install and enable the unit, but do not start it before a first release exists:

```bash
sudo cp deploy/conveyor.service.example /etc/systemd/system/conveyor.service
sudo $EDITOR /etc/systemd/system/conveyor.service
sudo systemctl daemon-reload
sudo systemctl enable conveyor
```

Then, from a clean checkout of the revision to deploy:

```bash
sudo env \
  CONVEYOR_PREFIX=/opt/conveyor \
  CONVEYOR_CONFIG=/var/lib/conveyor/conveyor.yaml \
  CONVEYOR_SERVICE=conveyor \
  "$(pwd)/deploy/install-release"
```

The installer performs these operations in order:

1. Build into a staging directory with the Git revision embedded.
2. Copy and hash all shipped agents and providers, then make the tree read-only.
3. Run the staged binary's own manifest check and validate the real config,
   failing if even one configured source cannot run.
4. Rename the complete staging directory into `releases/`, then run the selected
   rollback binary's own manifest verification and `validate -strict-sources`
   against the current config in that release's environment. An incompatible
   rollback refuses the deployment before `current` moves or systemd restarts.
5. Atomically swap `current`, restart the service and run `conveyor gate` through its passwordless local
   Unix socket. The gate validates the config through normal loading, requires
   every configured source to have a fresh successful listing, verifies the
   expected immutable revision/config identity, rejects a sticky run-persistence
   fault, probes `/api/state` and the embedded UI, and runs the same pure
   admission evaluator the scheduler uses (including global/source/stage/resource
   capacity) before `pipeline.Pick`. It claims no slot, writes no provider state
   and reserves no budget or storage.
6. If restart or the candidate gate fails, atomically restore the old `current`
   and restart it. The verified candidate deployment binary then gates the
   active rollback server while requiring the old revision; this remains
   compatible with releases that predate the `gate` command. If either rollback
   restart or that gate fails, remove `current`, require the service stop to succeed, and verify
   systemd reports it inactive. A failed stop or still-active unit gets a
   distinct fatal diagnostic; the installer never claims containment in that
   state. On success the old target is retained as `previous`.

The defaults match `deploy/conveyor.service.example`. The gate reads `<config
directory>/data/api.sock`, which bypasses browser authentication but remains
accessible only to the service account. `CONVEYOR_GATE_TIMEOUT` changes its
default `60s` wait. Test or non-systemd environments may set `CONVEYOR_GATE` to
an executable receiving the expected revision; success means the complete gate
passed. `CONVEYOR_SYSTEMCTL` selects the service-control executable.

Run the installer as root (as above), or as the unit's `User=` when that account
is authorized to restart the unit. The built-in gate must be able to read the
service-owned `data/api.sock`; inability to verify the gate is deliberately a
failed deployment, never permission to leave an unverified release active. The
installer checks for `flock` and `jq` before it changes `current`.

`CONVEYOR_ALLOW_DIRTY=1` is an emergency/testing escape hatch for packaging a
modified checkout. Such a manifest records `modified: true`, and `/api/state`
and the masthead identify the release as `dirty`. Routine production deploys
must come from a clean checkout and should never set it.

The masthead and `/api/state.release` show the active revision, manifest schema,
config schema and canonical release directory. In production the badge must say
`release`, not `dev`.

### Migrating an existing checkout-based service

Replace the former `logs:` block before starting the new binary. It is rejected
rather than guessed because successful polling, status, model work and failures
now have intentionally different lifetimes:

```yaml
storage:
  maxBytes: 20GiB
  tempMaxBytes: 4GiB
  highWatermark: 80
  criticalWatermark: 95
  sweepAt: "04:00"
  retention: {model: 7d, failure: 7d, polling: 2d, status: 7d}
```

Size suffixes are binary (`KiB`, `MiB`, `GiB`, `TiB`). Check
`/api/state.storage` after restart; `high` or `critical` pauses new model stages
but deliberately leaves discovery and the board available.

Before starting autonomous mode after upgrading from the all-transition budget
ledger, add positive `budgets.maxRunsPerItem` and `budgets.maxRunsPerDay` values.
If `data/budgets.json` predates the model-only ledger, startup names the reset
command. Stop the service and run it as the service account:

```bash
conveyor budget-reset -c /var/lib/conveyor/conveyor.yaml \
  -reason 'migrate dispatch counts to model-only execution budgets'
```

The old file remains beside the new one as `budgets.json.reset-<UTC>`; the new
ledger records the reason and time. There is no automatic numerical migration
because old counts do not say which transitions invoked a model.

The data directory is not independently configurable: it is always `data/`
beside `conveyor.yaml`. Moving only the config would silently discard run
history, ordering, saved answers, pauses, budgets, push subscriptions and the
VAPID key. Move the config and data together. Relative `workdir:`, `agents:`,
`providers:` and `scripts.*.script:` paths also resolve from the config's
directory, so make operational paths absolute before moving the file.

Release mode overrides the top-level `agents:` and `providers:` roots, and it
refuses any resolved provider or stage executable outside the verified release.
Commit custom executables under `agents/` or `providers/` and reference them
with `agent:`/`provider:`; an external `scripts.*.script:` path is intentionally
not accepted by a production release. Inline `run:` bodies remain part of the
operator-owned config.

For an existing `/opt/conveyor/conveyor.yaml` installation, keep the checkout,
old binary, old config, old data and old unit until migration has passed:

```bash
old_config=/opt/conveyor/conveyor.yaml
sudo cp -a /etc/systemd/system/conveyor.service /etc/systemd/system/conveyor.service.pre-release
sudo systemctl stop conveyor
sudo install -d -o conveyor -g conveyor /opt/conveyor/releases /var/lib/conveyor/data
sudo install -o conveyor -g conveyor -m 0600 "$old_config" /var/lib/conveyor/conveyor.yaml
sudo cp -a "$(dirname "$old_config")/data/." /var/lib/conveyor/data/
sudo chown -R conveyor:conveyor /var/lib/conveyor/data
sudo -u conveyor $EDITOR /var/lib/conveyor/conveyor.yaml  # make relative operational paths absolute
sudo cp deploy/conveyor.service.example /etc/systemd/system/conveyor.service
sudo $EDITOR /etc/systemd/system/conveyor.service         # fill in User and PATH
sudo systemctl daemon-reload
sudo systemctl enable conveyor
sudo env \
  CONVEYOR_PREFIX=/opt/conveyor \
  CONVEYOR_CONFIG=/var/lib/conveyor/conveyor.yaml \
  CONVEYOR_SERVICE=conveyor \
  "$(pwd)/deploy/install-release"
```

Installing and reloading the new unit *before* `install-release` is essential:
the installer's restart and health check must exercise the new `ExecStart` and
`CONVEYOR_RELEASE_DIR`, not the old checkout service. If this first deployment
fails, it stops the new unit. Restore the untouched old setup with:

```bash
sudo cp -a /etc/systemd/system/conveyor.service.pre-release /etc/systemd/system/conveyor.service
sudo systemctl daemon-reload
sudo systemctl start conveyor
```

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

### Repairing legacy PR ownership

New pull requests carry `<!-- conveyor:item N -->` in their body. Older PRs
remain valid when their closing reference and `issue-N` branch agree. If they
disagree, Conveyor stops every named item rather than guessing. Repair the
identity explicitly; for example, to migrate PR #172 from the legacy
`issue-128` branch to item #143:

```bash
set -euo pipefail
repo=OWNER/REPO
pr=172
old_item=128
item=143
new_branch=issue-143
meta=$(gh pr view "$pr" --repo "$repo" \
  --json title,body,baseRefName,headRefOid,isDraft)
base=$(jq -r .baseRefName <<<"$meta")
title=$(jq -r .title <<<"$meta")
head_oid=$(jq -r .headRefOid <<<"$meta")

# Create the replacement branch in $repo itself, independent of this shell's
# current checkout and its origin remote.
gh api -X POST "repos/$repo/git/refs" \
  -f ref="refs/heads/$new_branch" -f sha="$head_oid" >/dev/null
body=$(mktemp)
jq -r .body <<<"$meta" >"$body"
sed -i -E \
  -e "s/([Cc]lose[sd]?|[Ff]ix(e[sd])?|[Rr]esolve[sd]?)[:]?[[:space:]]*#$old_item/Closes #$item/g" \
  -e 's/<!--[[:space:]]*conveyor:item[[:space:]][0-9]+[[:space:]]*-->//g' \
  "$body"
printf '\n<!-- conveyor:item %s -->\n' "$item" >>"$body"

# Refuse to create a replacement that still names the old item or does not
# carry both required identities for the child.
! grep -Eiq "(close[sd]?|fix(e[sd])?|resolve[sd]?)[:]?[[:space:]]*#$old_item\\b|conveyor:item[[:space:]]+$old_item\\b" "$body"
grep -Eiq "(close[sd]?|fix(e[sd])?|resolve[sd]?)[:]?[[:space:]]*#$item\\b" "$body"
grep -Fq "<!-- conveyor:item $item -->" "$body"

draft_args=()
if jq -e .isDraft >/dev/null <<<"$meta"; then draft_args+=(--draft); fi
replacement=$(gh pr create --repo "$repo" --head "$new_branch" --base "$base" \
  --title "$title" --body-file "$body" "${draft_args[@]}")
[[ -n "$replacement" ]]
gh pr close "$pr" --repo "$repo" --comment "Replaced by $replacement for item #$item."
rm -f "$body"
```

Do not rename the open PR's head branch: GitHub closes that PR rather than
retargeting it. Verify the replacement body closes the same item and then hand that item back. If a
managed worktree for the old item still exists, let Conveyor's ordinary
positive-ownership cleanup remove it; do not delete an unmarked path by hand.

## Everything this does not do

- Does not install Go, git, `gh`, `jq`, `flock`, `claude`, `codex`, Node, systemd or
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
