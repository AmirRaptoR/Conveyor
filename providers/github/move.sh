#!/usr/bin/env bash
# GitHub provider write: reflect the item's stage as a status:* label, and its
# blocked mark as a label of its own.
#
# Both facts, every call. The mark is not a place — an issue that needs a human
# keeps the status:* it stopped on and additionally wears $BLOCKED_LABEL — so
# setting and clearing it are this same script with a different `.blocked`.
#
# The engine writes provider state BEFORE running a stage script, so this runs
# often and must be idempotent: an item already wearing the right labels is a
# no-op, and a no-op must exit 0 — source.Move treats anything else as failure.
#
# Env: REPO (required), STAGE_LABELS, BLOCKED_LABEL (default: blocked),
#      CONVEYOR_DRY_RUN (log, write nothing).
set -euo pipefail

: "${REPO:?REPO is required (set it in the source env: block)}"
BLOCKED_LABEL="${BLOCKED_LABEL:-blocked}"

payload=$(cat)
ref=$(jq -r '.item.ref' <<<"$payload")
to=$(jq -r '.stage' <<<"$payload")
blocked=$(jq -r '.blocked // false' <<<"$payload")
reason=$(jq -r '.blockedReason // ""' <<<"$payload")
# Fetched, not taken from the payload. The payload's labels come from the last
# list, which is already stale by the second move of the same tick: the engine
# moves an item into a stage and out of it between two polls, so a cached label
# set makes the second move skip a removal and leave the issue wearing both.
have=$(gh issue view "$ref" --repo "$REPO" --json labels --jq '.labels[].name')

trim() { sed 's/^[[:space:]]*//; s/[[:space:]]*$//'; }
wearing() { grep -qxF "$1" <<<"$have"; }

# Every label this provider manages — the right-hand side of every mapping.
managed=$(printf '%s\n' "${STAGE_LABELS:-}" | sed -n 's/^[^=]*=//p' | trim)
# The one this stage wants.
want=$(printf '%s\n' "${STAGE_LABELS:-}" |
	sed -n "s/^[[:space:]]*${to}[[:space:]]*=//p" | trim | head -1)

args=()
while IFS= read -r label; do
	[[ -z "$label" || "$label" == "$want" ]] && continue
	wearing "$label" && args+=(--remove-label "$label")
done <<<"$managed"

if [[ -n "$want" ]] && ! wearing "$want"; then
	args+=(--add-label "$want")
fi

# The mark, last, so the stage labels above read as one unchanged block.
marking=""
if [[ "$blocked" == "true" ]]; then
	if ! wearing "$BLOCKED_LABEL"; then
		args+=(--add-label "$BLOCKED_LABEL")
		marking="set"
	fi
elif wearing "$BLOCKED_LABEL"; then
	args+=(--remove-label "$BLOCKED_LABEL")
fi

if [[ ${#args[@]} -eq 0 ]]; then
	# Either the stage maps to no label (backlog, done — nothing to write), or
	# the issue is already correct. Both are success.
	echo "issue #$ref already reflects '$to'; nothing to write" >&2
	exit 0
fi

echo "issue #$ref -> $to${want:+ ($want)}${marking:+ + $BLOCKED_LABEL}" >&2

if [[ -n "${CONVEYOR_DRY_RUN:-}" ]]; then
	echo "DRY RUN: gh issue edit $ref --repo $REPO ${args[*]}" >&2
	[[ "$marking" == "set" && -n "$reason" ]] &&
		echo "DRY RUN: gh issue comment $ref --repo $REPO --body <reason>" >&2
	exit 0
fi

gh issue edit "$ref" --repo "$REPO" "${args[@]}" >&2

# Why, said once, where the person who clears the label will read it. Only on
# the transition into the mark: a marked item is never handed out again, so this
# cannot repeat, and a failed comment must not fail the move — the label is the
# state, the comment is only the explanation.
if [[ "$marking" == "set" && -n "$reason" ]]; then
	body="**Blocked** — $reason

Remove the \`$BLOCKED_LABEL\` label to hand this back to the pipeline; it resumes in \`$to\`."

	# The same stop, said twice, is noise. A mark that is cleared and comes
	# straight back — an hourly stall retry, or a person handing the board back
	# before the thing that stopped it was fixed — enters the mark again, and
	# without this each pass leaves another identical comment on the same issue.
	# Compared against every comment, not just the newest: a reply underneath
	# ours does not make the explanation new.
	if gh issue view "$ref" --repo "$REPO" --json comments \
		--jq '.comments[].body' 2>/dev/null | grep -qxF "$(head -1 <<<"$body")"; then
		echo "the same reason is already commented on #$ref; not repeating it" >&2
	else
		gh issue comment "$ref" --repo "$REPO" --body "$body" >&2 ||
			echo "could not comment the reason; the label is set regardless" >&2
	fi
fi
