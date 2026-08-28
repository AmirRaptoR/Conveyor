#!/usr/bin/env bash
# GitHub provider: open issues in $REPO, as conveyor items.
#
# Env (from the source's env: block):
#   REPO           owner/name — required
#   STAGE_LABELS   "stage=label" per line. The source owns the provider<->stage
#                  mapping; the engine never sees a label.
#   DEFAULT_STAGE  where an issue carrying no mapped label lands (default: backlog)
#   LIMIT          max issues to fetch (default: 200)
set -euo pipefail

: "${REPO:?REPO is required (set it in the source env: block)}"
DEFAULT_STAGE="${DEFAULT_STAGE:-backlog}"

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
		--argjson map "$label_to_stage" '
		map(
			(.labels | map(.name)) as $names
			| {
				id:          "\($source):\(.number)",
				ref:         (.number | tostring),
				source:      $source,
				# First mapped label wins. An issue wearing two status labels is
				# already broken; picking deterministically beats erroring, and
				# the next move.sh clears the loser.
				stage:       ([$names[] | $map[.] // empty] | .[0] // $default),
				title:       .title,
				description: (.body // ""),
				url:         .url,
				labels:      $names,
				# null, not 0: "unranked" and "most urgent" must stay distinct.
				priority:    ([$names[] | capture("^priority:p(?<n>[0-3])$") | .n | tonumber] | .[0]),
				assignee:    (.assignees | map(.login) | .[0] // "")
			}
		)' >"$CONVEYOR_RESULT"

echo "listed $(jq length "$CONVEYOR_RESULT") item(s)" >&2
