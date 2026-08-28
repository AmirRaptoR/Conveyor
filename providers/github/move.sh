#!/usr/bin/env bash
# GitHub provider write: reflect the item's new stage as a status:* label.
#
# The engine writes provider state BEFORE running a stage script, so this runs
# often and must be idempotent: an item already wearing the right label is a
# no-op, and a no-op must exit 0 — source.Move treats anything else as failure.
#
# Env: REPO (required), STAGE_LABELS, CONVEYOR_DRY_RUN (log, write nothing).
set -euo pipefail

: "${REPO:?REPO is required (set it in the source env: block)}"

payload=$(cat)
ref=$(jq -r '.item.ref' <<<"$payload")
to=$(jq -r '.stage' <<<"$payload")
# Fetched, not taken from the payload. The payload's labels come from the last
# list, which is already stale by the second move of the same tick: the engine
# moves an item into a stage and out of it between two polls, so a cached label
# set makes the second move skip a removal and leave the issue wearing both.
have=$(gh issue view "$ref" --repo "$REPO" --json labels --jq '.labels[].name')

trim() { sed 's/^[[:space:]]*//; s/[[:space:]]*$//'; }

# Every label this provider manages — the right-hand side of every mapping.
managed=$(printf '%s\n' "${STAGE_LABELS:-}" | sed -n 's/^[^=]*=//p' | trim)
# The one this stage wants.
want=$(printf '%s\n' "${STAGE_LABELS:-}" |
	sed -n "s/^[[:space:]]*${to}[[:space:]]*=//p" | trim | head -1)

args=()
while IFS= read -r label; do
	[[ -z "$label" || "$label" == "$want" ]] && continue
	grep -qxF "$label" <<<"$have" && args+=(--remove-label "$label")
done <<<"$managed"

if [[ -n "$want" ]] && ! grep -qxF "$want" <<<"$have"; then
	args+=(--add-label "$want")
fi

if [[ ${#args[@]} -eq 0 ]]; then
	# Either the stage maps to no label (backlog, done — nothing to write), or
	# the issue is already correct. Both are success.
	echo "issue #$ref already reflects '$to'; nothing to write" >&2
	exit 0
fi

echo "issue #$ref -> $to${want:+ ($want)}" >&2

if [[ -n "${CONVEYOR_DRY_RUN:-}" ]]; then
	echo "DRY RUN: gh issue edit $ref --repo $REPO ${args[*]}" >&2
	exit 0
fi

gh issue edit "$ref" --repo "$REPO" "${args[@]}" >&2
