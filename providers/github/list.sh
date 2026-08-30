#!/usr/bin/env bash
# GitHub provider: the issues in $REPO somebody handed to the pipeline, as
# conveyor items.
#
# Listing is opt-in. An issue is conveyor's because it wears a label saying so —
# the onboarding tag, a stage label, or the mark — and an issue nobody labelled
# is left entirely alone: not on the board, not in a count, and no stage is ever
# run against it. There is no opt-out label, because saying nothing is the
# opt-out, and a repository can open an issue and work it by hand while the
# pipeline runs beside it.
#
# Env (from the source's env: block):
#   REPO           owner/name — required
#   LABEL_PREFIX   what every label this pipeline owns begins with (default:
#                  "conveyor:"). One namespace, so a repository can tell at a
#                  glance which labels a machine writes and which are its own,
#                  and so move.sh can find every label it manages by shape
#                  rather than by a list that goes stale the moment a stage is
#                  renamed.
#   STAGE_LABELS   "stage=label" per line. The source owns the provider<->stage
#                  mapping; the engine never sees a label.
#   BLOCKED_LABEL  the label that means "a human must decide"
#                  (default: ${LABEL_PREFIX}blocked). It is a mark, not a stage:
#                  an issue wearing it keeps whatever stage label it stopped on,
#                  and the scheduler leaves it alone.
#   DEFAULT_STAGE  where an onboarded issue carrying no mapped label lands
#                  (default: backlog)
#   LIMIT          max issues to fetch (default: 200)
set -euo pipefail

: "${REPO:?REPO is required (set it in the source env: block)}"
DEFAULT_STAGE="${DEFAULT_STAGE:-backlog}"
LABEL_PREFIX="${LABEL_PREFIX:-conveyor:}"
BLOCKED_LABEL="${BLOCKED_LABEL:-${LABEL_PREFIX}blocked}"

# The onboarding tag: the namespace word with its separator taken off, so
# "conveyor:" gives "conveyor". Derived rather than configured, because it is
# the same fact as LABEL_PREFIX and renaming one must rename the other.
#
# Outside the namespace on purpose. move.sh manages `${LABEL_PREFIX}*` by shape
# and would take a tag inside it off at the first stage that maps to no label —
# the issue would leave the board halfway through its own journey.
ONBOARD_LABEL="${LABEL_PREFIX%[^[:alnum:]]}"

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

echo "listing issues in $REPO" >&2

# Closed issues are listed too, and only the ones this pipeline labelled.
#
# A finished item is a closed issue: the pull request says "Closes #N" and
# GitHub closes it on merge. Listing open issues alone meant the last stages
# had nobody in them — an item did not arrive in `done`, it disappeared on the
# way, and a board whose final column is always empty cannot be read as
# finishing anything.
#
# The asymmetry below is deliberate. An open issue that was handed over but has
# no stage label yet is new work and lands in DEFAULT_STAGE; a *closed* one with
# no stage label is finished, and letting the tag alone put it back on the board
# would re-list it as new work on every poll.
gh issue list --repo "$REPO" --state all --limit "${LIMIT:-200}" \
	--json number,title,body,labels,url,assignees,state |
	jq --arg source "$CONVEYOR_SOURCE" \
		--arg default "$DEFAULT_STAGE" \
		--arg blocked "$BLOCKED_LABEL" \
		--arg onboard "$ONBOARD_LABEL" \
		--argjson map "$label_to_stage" '
		map(
			(.labels | map(.name)) as $names
			| ([$names[] | $map[.] // empty] | .[0]) as $mapped
			# Opt-in, and these are the three ways in: the pipeline put
			# it here, a person handed it over, or it stopped.
			| select($mapped != null
				or ($names | index($onboard)) != null
				or ($names | index($blocked)) != null)
			| select(((.state // "OPEN") | ascii_downcase) == "open" or $mapped != null)
			| {
				id:          "\($source):\(.number)",
				ref:         (.number | tostring),
				source:      $source,
				# First mapped label wins. An issue wearing two status labels is
				# already broken; picking deterministically beats erroring, and
				# the next move.sh clears the loser.
				stage:       ($mapped // $default),
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

echo "listed $(jq length "$CONVEYOR_RESULT") item(s) (tag an issue $ONBOARD_LABEL to hand it over)" >&2
