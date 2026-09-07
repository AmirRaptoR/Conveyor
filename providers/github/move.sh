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
LABEL_PREFIX="${LABEL_PREFIX:-conveyor:}"
BLOCKED_LABEL="${BLOCKED_LABEL:-${LABEL_PREFIX}blocked}"

payload=$(cat)
ref=$(jq -r '.item.ref' <<<"$payload")
to=$(jq -r '.stage' <<<"$payload")
blocked=$(jq -r '.blocked // false' <<<"$payload")
reason=$(jq -r '.blockedReason // ""' <<<"$payload")
kind=$(jq -r '.blockedKind // ""' <<<"$payload")
# Fetched, not taken from the payload. The payload's labels come from the last
# list, which is already stale by the second move of the same tick: the engine
# moves an item into a stage and out of it between two polls, so a cached label
# set makes the second move skip a removal and leave the issue wearing both.
view=$(gh issue view "$ref" --repo "$REPO" --json labels,body)
have=$(jq -r '.labels[].name' <<<"$view")
body=$(jq -r '.body // ""' <<<"$view")

trim() { sed 's/^[[:space:]]*//; s/[[:space:]]*$//'; }
wearing() { grep -qxF "$1" <<<"$have"; }

# STAGE_LABELS is "stage=label" per line, parsed as data: each line is split
# once on its first '=' with bash's own string operators, never spliced into a
# sed or grep pattern. A stage name containing '/', '.' or '[' broke the old
# `sed -n "s/^...${to}.../"` lookup outright — '/' collides with the command's
# own delimiter, and an unbalanced '[' is an invalid bracket expression — and a
# custom LABEL_PREFIX with the same characters matched more (or less) than its
# literal text under grep's own regex mode.
declare -A stage_label=()
mapping_values=()
while IFS= read -r line; do
	line=$(trim <<<"$line")
	[[ -z "$line" || "$line" != *=* ]] && continue
	key=$(trim <<<"${line%%=*}")
	val=$(trim <<<"${line#*=}")
	[[ -z "$key" ]] && continue
	stage_label["$key"]="$val"
	[[ -n "$val" ]] && mapping_values+=("$val")
done <<<"${STAGE_LABELS:-}"

# A label is in the namespace only if it begins with the prefix — a literal
# comparison, not a substring search and not a regex, so a label that merely
# contains the prefix (a repository's own "team-conveyor:keep") is left alone,
# and a prefix containing regex metacharacters (".", "[") matches only itself.
owned=()
while IFS= read -r label; do
	[[ -z "$label" ]] && continue
	[[ "$label" == "${LABEL_PREFIX}"* ]] && owned+=("$label")
done <<<"$have"

# Every label this provider manages: the right-hand side of every mapping, plus
# anything the issue is already wearing that begins with the prefix.
#
# The second half is what makes a rename safe. Managed used to be the mappings
# alone, so renaming a stage label orphaned the old one — the issue kept wearing
# it forever, and listing takes the first mapped label it finds, which is how an
# item ends up in a stage nobody put it in. Owning the whole namespace means a
# label this pipeline wrote is a label this pipeline can take off, whether or
# not it is still in the config.
#
# The mark is excluded: it is handled below, and removing it here would set and
# clear the same label in one call.
managed=$(
	{
		printf '%s\n' "${mapping_values[@]+"${mapping_values[@]}"}"
		printf '%s\n' "${owned[@]+"${owned[@]}"}"
	} | grep -v "^${BLOCKED_LABEL}\(:\| \|$\)" | sort -u || true
)
# The one this stage wants.
want="${stage_label[$to]:-}"

args=()
while IFS= read -r label; do
	[[ -z "$label" || "$label" == "$want" ]] && continue
	wearing "$label" && args+=(--remove-label "$label")
done <<<"$managed"

if [[ -n "$want" ]] && ! wearing "$want"; then
	args+=(--add-label "$want")
fi

# The mark, last, so the stage labels above read as one unchanged block.
#
# One label, not two. There used to be a `$BLOCKED_LABEL: <kind>` label beside
# it; it put the same fact in a third place (label, comment, board) and none of
# them was the one a person read. The kind now travels in the body section
# below, where the reason it belongs to already is.
marking=""
if [[ "$blocked" == "true" ]]; then
	if ! wearing "$BLOCKED_LABEL"; then
		args+=(--add-label "$BLOCKED_LABEL")
		marking="set"
	fi
elif wearing "$BLOCKED_LABEL"; then
	args+=(--remove-label "$BLOCKED_LABEL")
fi

# Left over from when the kind was a label of its own: take them off wherever
# they are still worn, so a repository converges without anyone editing labels
# by hand. Nothing writes them any more.
while IFS= read -r label; do
	[[ "$label" == "$BLOCKED_LABEL: "* ]] && args+=(--remove-label "$label")
done <<<"$have"

# --- why, as a field on the item ------------------------------------------
#
# The reason lives in a marked section at the top of the issue body: one
# section, rewritten in place, removed the moment the mark goes. It used to be
# a comment, which is append-only — the reason that matters is the current one,
# and a trail of near-identical comments is how an issue stops being readable.
#
# The open marker carries the same fact as base64 JSON so list.sh can read it
# back exactly (list.sh:@base64d) and the board can say why an item stopped
# without walking run history. Base64 because a reason is free text and a
# reason containing "-->" would otherwise end the comment early.
BLOCK_CLOSE='<!-- /conveyor:block -->'

strip_block() {
	# `endm`, not `close`: close is an awk builtin, and -v close=… is a hard
	# "cannot command line assign" error rather than a shadowed name.
	awk -v endm="$BLOCK_CLOSE" '
		/^<!-- conveyor:block / { inb = 1; next }
		inb && $0 == endm { inb = 0; blank = 1; next }
		inb { next }
		blank && $0 == "" { next }
		{ blank = 0; print }'
}

rest=$(strip_block <<<"$body")
# Only when there is something to say. A mark with no reason is a script that
# said nothing; rewriting someone's issue body to tell them so is noise, and
# the label already carries the whole of what is known.
if [[ "$blocked" == "true" && -n "$reason" ]]; then
	meta=$(jq -cn --arg k "$kind" --arg s "$to" --arg r "$reason" \
		'{kind: $k, stage: $s, reason: $r}' | base64 -w0)
	want_body=$(
		printf '<!-- conveyor:block %s -->\n' "$meta"
		printf '> **Blocked in `%s`**%s\n>\n' "$to" "${kind:+ — $kind}"
		sed 's/^/> /' <<<"$reason"
		printf '>\n> _Remove the `%s` label to hand this back; it resumes in `%s`._\n' \
			"$BLOCKED_LABEL" "$to"
		printf '%s\n' "$BLOCK_CLOSE"
		# An `if`, not `[[ … ]] && printf`: this is the last command in the
		# substitution, so under `set -e` a false test would fail the whole
		# assignment on the one case that is entirely normal — an empty body.
		if [[ -n "$rest" ]]; then printf '\n%s\n' "$rest"; fi
	)
else
	want_body="$rest"
fi

if [[ "$want_body" != "$body" ]]; then
	bodyfile=$(mktemp)
	trap 'rm -f "$bodyfile"' EXIT
	printf '%s\n' "$want_body" >"$bodyfile"
	args+=(--body-file "$bodyfile")
fi

if [[ ${#args[@]} -eq 0 ]]; then
	# Either the stage maps to no label (backlog, done — nothing to write), or
	# the issue is already correct. Both are success.
	echo "issue #$ref already reflects '$to'; nothing to write" >&2
	exit 0
fi

echo "issue #$ref -> $to${want:+ ($want)}${marking:+ + $BLOCKED_LABEL}${kind:+ [$kind]}" >&2

# The body travels through a temp file, whose name is noise; a log that says
# `--body-file /tmp/tmp.9fK2` tells nobody anything, and a dry run has to be
# reproducible from one run to the next to be worth diffing.
shown=("${args[@]}")
if [[ -n "${bodyfile:-}" ]]; then
	shown=("${shown[@]/#"$bodyfile"/<body>}")
fi

if [[ -n "${CONVEYOR_DRY_RUN:-}" ]]; then
	echo "DRY RUN: gh issue edit $ref --repo $REPO ${shown[*]}" >&2
	exit 0
fi

gh issue edit "$ref" --repo "$REPO" "${args[@]}" >&2
