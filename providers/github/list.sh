#!/usr/bin/env bash
# GitHub provider: open issues in $REPO, as conveyor items.
#
# Env (from the source's env: block):
#   REPO           owner/name — required
#   STAGE_LABELS   "stage=label" per line. The source owns the provider<->stage
#                  mapping; the engine never sees a label.
#   BLOCKED_LABEL  the label that means "a human must decide" (default: blocked).
#                  It is a mark, not a stage: an issue wearing it keeps whatever
#                  status:* it stopped on, and the scheduler leaves it alone.
#   IGNORE_LABELS  labels that mean "not conveyor's work", one per line or comma
#                  separated (default: conveyor:ignore). An issue wearing one is
#                  never listed, so it never reaches the board and nothing is
#                  ever run against it — the way to open an issue and work it by
#                  hand while the pipeline is running.
#   DEFAULT_STAGE  where an issue carrying no mapped label lands (default: backlog)
#   LIMIT          max issues to fetch (default: 200)
set -euo pipefail

: "${REPO:?REPO is required (set it in the source env: block)}"
DEFAULT_STAGE="${DEFAULT_STAGE:-backlog}"
BLOCKED_LABEL="${BLOCKED_LABEL:-blocked}"
IGNORE_LABELS="${IGNORE_LABELS:-conveyor:ignore}"

# Ignoring is not blocking, and the difference is the whole point of having
# both. A blocked issue is conveyor's, stopped, waiting for an answer, and it
# comes back the moment the mark is cleared. An ignored issue was never
# conveyor's: it is not on the board, it is not counted, and no stage will ever
# be run against it.
ignored=$(
	printf '%s\n' "$IGNORE_LABELS" |
		jq -R -s 'split("\n") | map(split(",")) | flatten
			| map(gsub("^\\s+|\\s+$"; "")) | map(select(length > 0))'
)

# STAGE_LABELS is written stage=label because that is the direction move.sh
# needs. Listing needs the reverse, so invert it here into {label: stage}.
label_to_stage=$(
	printf '%s\n' "${STAGE_LABELS:-}" |
		jq -R -s '
			split("\n")
			| map(select(test("=")))
			| map(split("=") | {key: (.[1] | gsub("^\\s+|\\s+$"; "")),
			                    value: (.[0] | gsub("^\\s+|\\s+$"; ""))})
			| from_entries'
)

echo "listing open issues in $REPO" >&2

gh issue list --repo "$REPO" --state open --limit "${LIMIT:-200}" \
	--json number,title,body,labels,url,assignees |
	jq --arg source "$CONVEYOR_SOURCE" \
		--arg default "$DEFAULT_STAGE" \
		--arg blocked "$BLOCKED_LABEL" \
		--argjson ignored "$ignored" \
		--argjson map "$label_to_stage" '
		map(
			(.labels | map(.name)) as $names
			| select(any($ignored[]; . as $skip | $names | index($skip)) | not)
			| {
				id:          "\($source):\(.number)",
				ref:         (.number | tostring),
				source:      $source,
				# First mapped label wins. An issue wearing two status labels is
				# already broken; picking deterministically beats erroring, and
				# the next move.sh clears the loser.
				stage:       ([$names[] | $map[.] // empty] | .[0] // $default),
				title:       .title,
				# The mark rides beside the stage, never instead of it: this is
				# what keeps an unblocked issue resuming where it stopped.
				blocked:     ($names | index($blocked) != null),
				description: (.body // ""),
				url:         .url,
				labels:      $names,
				# null, not 0: "unranked" and "most urgent" must stay distinct.
				priority:    ([$names[] | capture("^priority:p(?<n>[0-3])$") | .n | tonumber] | .[0]),
				assignee:    (.assignees | map(.login) | .[0] // "")
			}
		)' >"$CONVEYOR_RESULT"

skipping=""
if [[ -n "$IGNORE_LABELS" ]]; then
	skipping=", ignoring $(jq -r 'join(", ")' <<<"$ignored")"
fi
echo "listed $(jq length "$CONVEYOR_RESULT") item(s)$skipping" >&2
