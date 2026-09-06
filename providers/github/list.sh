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
#   LIMIT          max open issues to fetch per enrolling label (default: 200)
#   CLOSED_LIMIT   how many of the most recently closed issues to keep, after
#                  sorting (default: 100)
#   CLOSED_FETCH_LIMIT  how many closed issues to fetch before that sort
#                  (default: 1000) — larger than CLOSED_LIMIT on purpose, so
#                  the sort has more than an arbitrary CLOSED_LIMIT-sized page
#                  to choose the newest from.
#
# Stdin carries model.ListInput — in particular terminalStages, which of the
# stages STAGE_LABELS maps to are terminal. Listing needs this to tell a
# closed issue that finished from one merely closed mid-flight (see below);
# it is not read from source env: because the stage graph already lives in
# conveyor.yaml, and a second copy here would drift from it.
set -euo pipefail

: "${REPO:?REPO is required (set it in the source env: block)}"
list_input=$(cat)
[[ -n "$list_input" ]] || list_input='{}'
terminal_stages=$(jq -c '.terminalStages // []' <<<"$list_input")
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
#
# Open issues are fetched one enrolling label at a time — every mapped stage
# label, the mark, and the onboarding tag — and unioned, rather than one
# `--state open` call truncated at LIMIT before the enrolment filter runs:
# a call scoped to a label is server-side filtered, so an enrolled issue
# cannot be pushed out of the page by newer unrelated issues the way a single
# unfiltered call could. Results are deduplicated by number, since an issue
# wearing two enrolling labels would otherwise appear once per label.
#
# Closed issues are a separate listing, fetched well past CLOSED_LIMIT and
# sorted newest-closedAt-first *before* being cut down to it, so CLOSED_LIMIT
# is the N most recently closed rather than an arbitrary N `gh` happened to
# return first.
mapfile -t enroll_labels < <(jq -r --arg onboard "$ONBOARD_LABEL" --arg blocked "$BLOCKED_LABEL" \
	'(keys) + [$onboard, $blocked] | unique[]' <<<"$label_to_stage")

# dedup_by_number — first occurrence wins, in the order the calls happened,
# rather than unique_by's ascending sort: this feeds straight into the same
# jq pipeline the old single-call listing did, and that pipeline (and its
# tests) assume gh's own ordering survives, not a resort by id.
dedup_by_number='reduce .[] as $x ({seen: {}, out: []};
	if (.seen[$x.number | tostring] // false) then .
	else {seen: (.seen + {($x.number | tostring): true}), out: (.out + [$x])} end) | .out'

open_issues='[]'
for label in "${enroll_labels[@]}"; do
	page=$(gh issue list --repo "$REPO" --state open --label "$label" --limit "${LIMIT:-200}" \
		--json number,title,body,labels,url,assignees,state,closedAt)
	open_issues=$(jq -c -n --argjson a "$open_issues" --argjson b "$page" '$a + $b')
done
open_issues=$(jq -c "$dedup_by_number" <<<"$open_issues")

closed_issues=$(gh issue list --repo "$REPO" --state closed \
	--limit "${CLOSED_FETCH_LIMIT:-1000}" \
	--json number,title,body,labels,url,assignees,state,closedAt |
	jq --argjson n "${CLOSED_LIMIT:-100}" 'sort_by(.closedAt) | reverse | .[0:$n]')

jq -n --argjson open "$open_issues" --argjson closed "$closed_issues" '$open + $closed' |
	jq --arg source "$CONVEYOR_SOURCE" \
		--arg default "$DEFAULT_STAGE" \
		--arg blocked "$BLOCKED_LABEL" \
		--arg onboard "$ONBOARD_LABEL" \
		--argjson map "$label_to_stage" \
		--argjson terminal "$terminal_stages" '
		map(
			(.labels | map(.name)) as $names
			| ([$names[] | $map[.] // empty] | .[0]) as $mapped
			| (((.state // "OPEN") | ascii_downcase) == "closed") as $isClosed
			# A closed issue keeping a mapped stage that is not terminal stopped
			# mid-flight rather than finished — a person closed it, or it was
			# closed some other way than this pipeline completing it — so it is
			# marked in place rather than silently treated as done. The pipeline
			# must not fabricate a completion it did not perform.
			| ($isClosed and $mapped != null and (($terminal | index($mapped)) == null)) as $closedNonTerminal
			# Opt-in, and these are the three ways in: the pipeline put
			# it here, a person handed it over, or it stopped.
			| select($mapped != null
				or ($names | index($onboard)) != null
				or ($names | index($blocked)) != null)
			| select((($isClosed | not)) or $mapped != null)
			| ({
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
				blocked:     (($names | index($blocked) != null) or $closedNonTerminal),
				description: (.body // ""),
				url:         .url,
				labels:      $names,
				# null, not 0: "unranked" and "most urgent" must stay distinct.
				priority:    ([$names[] | capture("^priority:p(?<n>[0-3])$") | .n | tonumber] | .[0]),
				assignee:    (.assignees | map(.login) | .[0] // ""),
				# finishedAt is the engines word for when a source considers an
				# item done; an open issue has none. closedAt is GitHubs own
				# vocabulary, only meaningful once an issue is actually closed.
				finishedAt:  (if $isClosed then (.closedAt // "") else "" end)
			} + (if $closedNonTerminal then
				{blockReason: "closed on GitHub while still in stage \"\($mapped)\", which is not terminal; a person should reconcile it."}
			else {} end))
		)' >"$CONVEYOR_RESULT"

echo "listed $(jq length "$CONVEYOR_RESULT") item(s) (tag an issue $ONBOARD_LABEL to hand it over)" >&2
