#!/usr/bin/env bash
# Create every label this provider needs in a repository, once.
#
#   REPO=owner/name STAGE_LABELS="$(…)" providers/github/onboard.sh
#
# Not a verb: `provider:` resolves only list and move, so a third file here can
# never be mistaken for a stage script — the same precedent selfcheck.sh sets.
#
# move.sh does create a missing label when it turns out it needs one, so a
# stage added to the config after a repository was onboarded still works. That
# is a recovery, not a plan: it means the first item into a new stage is also
# the first time anyone finds out whether the label write succeeds, and it
# creates the label with a generic colour and description. This is the one-shot
# version, so onboarding a repository is one command rather than a discovery
# spread over a week.
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

# The mark. One label, and no per-kind labels beside it: which kind of stop it
# was rides in the section move.sh writes into the issue body, next to the
# reason it belongs to. There used to be a label per kind, which put the same
# fact in a third place and made "everything blocked" two queries instead of
# one; move.sh takes any left over off the next time it writes.
label "$BLOCKED_LABEL" D93F0B "Conveyor: blocked — read the reason at the top of the issue"

echo "$made created, $kept already there"
