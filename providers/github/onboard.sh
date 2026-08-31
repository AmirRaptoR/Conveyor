#!/usr/bin/env bash
# Create every label this provider needs in a repository, once.
#
#   REPO=owner/name STAGE_LABELS="$(…)" providers/github/onboard.sh
#
# Not a verb: `provider:` resolves only list and move, so a third file here can
# never be mistaken for a stage script — the same precedent selfcheck.sh sets.
#
# Labels are created lazily today, in two places and only when they are first
# needed: move.sh writes a stage label the first time an item reaches that
# stage, and a `$BLOCKED_LABEL: <kind>` label the first time a script stops that
# way. That works, and it means the first stop of a new kind in a new repository
# is also the first time anyone finds out whether the label write succeeds. This
# is the one-shot version, so onboarding a repository is one command rather than
# a discovery spread over a week.
#
# Idempotent: an existing label is not an error, and neither its colour nor its
# description is overwritten. Someone may have recoloured a label on purpose,
# and a tool that resets that every time it runs is a tool people stop running.
set -euo pipefail

: "${REPO:?REPO is required (owner/name)}"
: "${STAGE_LABELS:?STAGE_LABELS is required (stage=label lines, as in the source provider params)}"

LABEL_PREFIX="${LABEL_PREFIX:-conveyor:}"
BLOCKED_LABEL="${BLOCKED_LABEL:-${LABEL_PREFIX}blocked}"

# The onboarding tag, derived exactly as list.sh derives it (list.sh:43): the
# namespace word with its separator taken off. Same fact, one definition —
# computing it differently here is how the two drift apart.
ONBOARD_LABEL="${LABEL_PREFIX%[^[:alnum:]]}"

# The kinds the shipped scripts stop with. agents/_blocked documents the first
# five; the rest are approve's. A kind not listed here still works — move.sh
# creates its label on first use — so this list being incomplete costs nothing
# but the one-shot.
KINDS=(decision limit worktree no-output input timeout error human-review checks conflict merge)

made=0 kept=0

# label <name> <colour> <description> — create it, or leave the existing one
# entirely alone. `gh label create` fails when the label exists; that is the
# expected case on a second run and is not reported as an error.
label() {
	local name=$1 colour=$2 desc=$3
	if gh label create "$name" --repo "$REPO" --color "$colour" --description "$desc" >/dev/null 2>&1; then
		echo "  created  $name"
		made=$((made + 1))
	else
		echo "  exists   $name"
		kept=$((kept + 1))
	fi
}

echo "onboarding $REPO"

# The tag that hands an issue over. Outside the namespace deliberately — see
# list.sh — so it is described as the gesture it is rather than as a stage.
label "$ONBOARD_LABEL" 1D76DB "Hand this issue over to the conveyor pipeline (onboarding tag)"

# Every right-hand side of STAGE_LABELS. Read the same way list.sh and move.sh
# read it, comments and blank lines included, so a config that works for them
# works here.
while IFS= read -r line; do
	line="${line%%#*}"
	line="${line#"${line%%[![:space:]]*}"}"
	line="${line%"${line##*[![:space:]]}"}"
	[[ -z "$line" || "$line" != *=* ]] && continue
	stage="${line%%=*}"
	name="${line#*=}"
	[[ -z "$name" ]] && continue
	label "$name" 1D76DB "Conveyor: $stage"
done <<<"$STAGE_LABELS"

# The mark, and one label per kind of stop. Same colour and the same description
# wording move.sh:118-119 writes, so the one-shot and the lazy path cannot
# produce two different-looking labels for the same kind.
label "$BLOCKED_LABEL" D93F0B "Conveyor: blocked"
for kind in "${KINDS[@]}"; do
	label "$BLOCKED_LABEL: $kind" d4a72c \
		"Blocked: $kind. Remove the $BLOCKED_LABEL label to hand it back."
done

echo "$made created, $kept already there"
