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
#   CLOSED_LIMIT   how many of the most recently closed issues to keep in the
#                  visible history ledger after sorting (default: 100).
#                  Completed issues referenced by listed work are resolved
#                  separately and retained even when older than this window.
#   CLOSED_FETCH_LIMIT  how many closed issues to fetch before that sort
#                  (default: 1000) — larger than CLOSED_LIMIT on purpose, so
#                  the sort has more than an arbitrary CLOSED_LIMIT-sized page
#                  to choose the newest from.
#   DEPENDENCY_LOOKUP_LIMIT  maximum completed references outside the ordinary
#                  listing to resolve directly per poll (default: 50). Extra
#                  references remain missing and therefore fail closed.
#   RELATIONSHIP_LOOKUP_LIMIT maximum marker-only parents outside the ordinary
#                  listing to confirm per poll (default: 50). Native GitHub
#                  relationships need no confirmation call.
#   SUBISSUE_LOOKUP_LIMIT maximum parents whose truncated native child set is
#                  completed through the paginated API per poll (default: 20).
#                  Extra parents retain the embedded partial set and warn.
#
# Parent/child structure comes from GitHub native sub-issues. The exact first
# body line `Parent: #N` is a durable fallback marker for older/imported data;
# no other issue-number prose is treated as parenthood.
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
# Where a closed-as-completed issue belongs, whatever label it is wearing: the
# first terminal stage in the pipeline's own stage order. `stages` arrives in
# that order, so this is "the end of the line" and not a guess. Empty when the
# pipeline declares no terminal stage at all, in which case the reconciliation
# below stands down rather than inventing a destination.
done_stage=$(jq -r 'first(.stages[]? | select(. as $s | ($ARGS.named.t | index($s)))) // ""' \
	--argjson t "$terminal_stages" <<<"$list_input")
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
# needs. Listing needs the reverse, so invert it here into {label: stage} —
# via _stage_labels.sh, the one parse onboard.sh and preflight.sh also use.
source "$(dirname "${BASH_SOURCE[0]}")/_stage_labels.sh"
label_to_stage=$(stage_labels_json <<<"${STAGE_LABELS:-}")

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

# The issues travel through files, one JSON object per line, and never through
# argv. Linux caps a *single* argument at MAX_ARG_STRLEN — 128 KiB, however
# much total ARG_MAX is left — so accumulating the listing in a shell variable
# and handing it to `jq --argjson` failed with "Argument list too long" as soon
# as a repository outgrew that: jq never exec'd, the script exited 126, and the
# source listed nothing. Issue bodies are whole specifications here, so this is
# ordinary size, not an extreme: one enrolled repo's hundred most recently
# closed issues serialise to 560 KB. Only the small, bounded values below
# (the stage map, the source name) still ride on the command line.
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# GitHub answers 503 now and then, and one of those used to cost the whole
# listing: set -e failed the script, the engine marked the source stale, and
# nothing routed through that repository was dispatched until the next poll
# succeeded. The listing is all-or-nothing on purpose — a partial union would
# read as "those items are gone" — so the retry is here, at the one call that
# actually flakes, rather than a partial result being accepted upstream.
gh_issues() {
	local attempt=1 out
	while :; do
		# stderr through a file, not 2>&1: gh writes progress there on calls
		# that succeed, and folding it into stdout would hand jq a JSON
		# document with a line of English in front of it.
		if out=$(gh issue list --repo "$REPO" "$@" \
			--json number,title,body,labels,url,assignees,state,stateReason,closedAt,parent,subIssues \
			2>"$work/gh.err"); then
			printf '%s' "$out"
			return 0
		fi
		if ((attempt >= ${GH_ATTEMPTS:-3})); then
			echo "gh issue list $* failed after $attempt attempts: $(cat "$work/gh.err")" >&2
			return 1
		fi
		echo "gh issue list $* failed (attempt $attempt): $(cat "$work/gh.err")" >&2
		sleep $((attempt * 2))
		attempt=$((attempt + 1))
	done
}

# gh_issue <number> — resolve one dependency that fell outside CLOSED_LIMIT.
# A genuine missing issue is reported with status 2 so the engine can expose
# it as invalid state; transient/API failures still fail the whole listing,
# preserving last-good state rather than briefly inventing a missing node.
gh_issue() {
	local ref=$1 attempt=1 out
	while :; do
		if out=$(gh issue view "$ref" --repo "$REPO" \
			--json number,title,body,labels,url,assignees,state,stateReason,closedAt,parent,subIssues \
			2>"$work/gh.err"); then
			printf '%s' "$out"
			return 0
		fi
		if grep -qiE 'not found|could not resolve|HTTP 404' "$work/gh.err"; then
			return 2
		fi
		if ((attempt >= ${GH_ATTEMPTS:-3})); then
			echo "gh issue view $ref failed after $attempt attempts: $(cat "$work/gh.err")" >&2
			return 1
		fi
		echo "gh issue view $ref failed (attempt $attempt): $(cat "$work/gh.err")" >&2
		sleep $((attempt * 2))
		attempt=$((attempt + 1))
	done
}

# gh_subissues <number> — fetch every page only when the compact issue-list
# response says its embedded nodes were truncated. Most issues never pay for
# this call; large tracking parents still cannot silently lose child edges.
gh_subissues() {
	local ref=$1 attempt=1 out
	while :; do
		if out=$(gh api --paginate --slurp "repos/$REPO/issues/$ref/sub_issues?per_page=100" \
			2>"$work/gh.err"); then
			printf '%s' "$out"
			return 0
		fi
		if ((attempt >= ${GH_ATTEMPTS:-3})); then
			echo "gh api sub-issues for #$ref failed after $attempt attempts: $(cat "$work/gh.err")" >&2
			if grep -qiE 'not found|could not resolve|HTTP 404|resource not accessible|requires? .*scope|sub-issues .*disabled' "$work/gh.err"; then
				return 2
			fi
			return 1
		fi
		echo "gh api sub-issues for #$ref failed (attempt $attempt): $(cat "$work/gh.err")" >&2
		sleep $((attempt * 2))
		attempt=$((attempt + 1))
	done
}

: >"$work/open.jsonl"
for label in "${enroll_labels[@]}"; do
	gh_issues --state open --label "$label" --limit "${LIMIT:-200}" |
		jq -c '.[]' >>"$work/open.jsonl"
done

# Appending in label order keeps the union in the order the calls happened,
# which is what dedup_by_number's "first occurrence wins" is defined against.
jq -s -c "$dedup_by_number | .[]" "$work/open.jsonl" >"$work/items.jsonl"

gh_issues --state closed --limit "${CLOSED_FETCH_LIMIT:-1000}" |
	jq -c --argjson n "${CLOSED_LIMIT:-100}" \
		'sort_by(.closedAt) | reverse | .[0:$n] | .[]' >>"$work/items.jsonl"

jq -s --arg source "$CONVEYOR_SOURCE" \
		--arg repo "$REPO" \
		--arg default "$DEFAULT_STAGE" \
		--arg done "$done_stage" \
		--arg blocked "$BLOCKED_LABEL" \
		--arg onboard "$ONBOARD_LABEL" \
		--argjson map "$label_to_stage" \
		--argjson terminal "$terminal_stages" '
		# The section move.sh writes into an issue body: why it stopped, in a
		# form both a person and this script can read. The open marker carries
		# base64 JSON so the reason comes back exactly as it was written, "-->"
		# and all; the blockquote under it is the copy a person sees.
		def strip_block: sub("(?s)<!-- conveyor:block .*?<!-- /conveyor:block -->\n*"; "");
		def block_said:
			(capture("<!-- conveyor:block (?<b>[A-Za-z0-9+/=]+) -->")
				| .b | @base64d | fromjson) // {};
		# Parent: #N is a machine marker only when it is the complete first
		# metadata line. Ordinary prose, quotes and later historical copies are
		# deliberately invisible. Native GitHub relationships win when present.
		def parent_marker:
			([capture("^Parent:[ \\t]*#(?<n>[1-9][0-9]*)[ \\t]*(?:\\r?\\n|$)"; "i")] | .[0].n // "");
		def strip_parent_marker:
			sub("^Parent:[ \\t]*#[1-9][0-9]*[ \\t]*(?:\\r?\\n)*"; ""; "i");
		def github_ref:
			([capture("^https://github\\.com/(?<owner>[^/]+)/(?<name>[^/]+)/issues/(?<ref>[0-9]+)$")] | .[0] // null);
		def native_id($source; $repo):
			(.url // "" | github_ref) as $u
			| if $u != null and (("\($u.owner)/\($u.name)" | ascii_downcase) == ($repo | ascii_downcase))
				then "\($source):\($u.ref | tonumber)" else "" end;
		map(
			(.labels | map(.name)) as $names
			| ([$names[] | $map[.] // empty] | .[0]) as $mapped
			| (((.state // "OPEN") | ascii_downcase) == "closed") as $isClosed
			| ((.stateReason // "") | ascii_downcase) as $why
			| (.body // "") as $body
			| ($body | parent_marker) as $markerParentRef
			| (if $markerParentRef == "" then "" else "\($source):\($markerParentRef | tonumber)" end) as $markerParent
			| (.parent // null) as $nativeParentRaw
			| (if $nativeParentRaw == null then "" else ($nativeParentRaw | native_id($source; $repo)) end) as $nativeParent
			| ([.subIssues.nodes[]? | native_id($source; $repo) | select(. != "")] | unique | sort) as $nativeChildren
			| ((.subIssues.totalCount // 0) > ([.subIssues.nodes[]?] | length)) as $nativeChildrenTruncated
			| ([if $nativeParentRaw != null and $nativeParent == "" then
				"native parent " + ($nativeParentRaw.url // "<no URL>") + " is in another repository; cross-repository relationships are not representable until repo-to-source mapping is supported, so this link is omitted"
				else empty end]
				+ [if $nativeChildrenTruncated then empty else .subIssues.nodes[]? end
					| select((native_id($source; $repo)) == "")
					| "native child " + (.url // "<no URL>") + " is in another repository; cross-repository relationships are not representable until repo-to-source mapping is supported, so this link is omitted"]
				+ [if $nativeParent != "" and $markerParent != "" and $nativeParent != $markerParent then
					"native parent " + $nativeParent + " disagrees with Parent marker " + $markerParent + "; native GitHub relationship wins"
				else empty end]) as $relationWarnings
			| (if $nativeParent != "" then $nativeParent else $markerParent end) as $parent
			# The sequence, translated out of GitHubs vocabulary and into
			# item ids — the engine must never see a "#". Same syntax
			# agents/_deps reads, and stated twice on purpose: providers and
			# agents are separately-resolvable roots, and coupling them to
			# share a regex would be worse than the duplication. The shared
			# fixture corpus both selfchecks read is what keeps the two from
			# drifting.
			#
			# The body is split into sentences — at a line break, or at
			# "." / "!" / "?" followed by whitespace — before a sentence is
			# checked for a keyword, so a declaration reached only after an
			# earlier, unrelated sentence on the same line is still read, and
			# a sentence that merely shares a line with a declaration is
			# not. "depends on" / "blocked by" / "blocks on" match anywhere
			# in the sentence; "requires" / "after" are ordinary English
			# words, so they match only where the sentence opens (after
			# markdown noise). Only the numbers from the keyword onward,
			# within that sentence, are taken.
			#
			# The anchored branch is checked first: when a sentence opens
			# with "requires"/"after", that keyword is always the earliest
			# possible match in it, so the cut is taken there even if
			# "depends on"/"blocked by"/"blocks on" also occurs later in the
			# same sentence. Only a sentence that does not open with the
			# anchored pair falls through to the phrase-anywhere cut.
			#
			# A declaration may also open a list: a keyword sentence that
			# names nothing itself and ends its line ("Depends on:") donates
			# the numbers from each markdown list item on the lines that
			# follow — first sentence of each — until the first line that
			# is not a list item. Blank lines before the first item are
			# allowed; one after the list has started ends it. That is the
			# shape a refined issue writes, and it used to declare nothing.
			| ([
				$body
				| reduce ([splits("\n")] | .[]) as $rawline
					({pending: false, inlist: false, refs: []};
					($rawline | sub("\r$"; "")) as $line
					| if ($line | test("^[ \t]*$")) then .inlist = false
					else
						(if (.pending or .inlist)
							and ($line | test("^[ \t]*(?:[-*+]|[0-9]+[.)])[ \t]+")) then
							.inlist = true | .pending = false
							| .refs += ($line
								| sub("^[ \t]*(?:[-*+]|[0-9]+[.)])[ \t]+"; "")
								| ([splits("(?<=[.!?])[ \t]+")] | .[0] // "")
								| [scan("#[0-9]+")])
						else .inlist = false | .pending = false end)
						| ($line | [splits("(?<=[.!?])[ \t]+")]) as $ss
						| reduce range(0; $ss | length) as $i (.;
							($ss[$i]
								| if test("^[\\s*_~`>+-]*(requires|after)\\b"; "i") then
									sub("^[\\s*_~`>+-]*(?:requires|after)\\b"; ""; "i")
								elif test("\\b(depends on|blocked by|blocks on)\\b"; "i") then
									sub("^.*?\\b(?:depends on|blocked by|blocks on)\\b"; ""; "i")
								else null end) as $rest
							| if $rest == null then .
							else .refs += ($rest | [scan("#[0-9]+")])
								| if $i == ($ss | length) - 1
									and ($rest | test("^[\\s:*_~`.!?-]*$")) then .pending = true
								else . end
							end)
					end)
				| .refs[]
			] | map(ltrimstr("#") | tonumber | tostring) | unique | map("\($source):\(.)")) as $deps
			# Opt-in, and these are the three ways in: the pipeline put
			# it here, a person handed it over, or it stopped.
			| select($mapped != null
				or ($names | index($onboard)) != null
				or ($names | index($blocked)) != null)
			| select((($isClosed | not)) or $mapped != null)
			# **The issue status is the truth about the item.** Closed as
			# anything but completed is abandoned: off the board entirely,
			# not marked, not counted, no stage ever run against it again.
			| select(($isClosed and ($why == "not_planned" or $why == "duplicate")) | not)
			# Closed as completed is finished, whatever stage label it is
			# still wearing. A pull request merged by hand closes the issue
			# without this pipeline moving anything, and the item used to be
			# marked in place for it — a mark nothing could clear, since a
			# merged PR never becomes open again. Reconciled below, so the
			# label catches up with the status rather than outliving it.
			| ($isClosed and $done != "") as $finished
			# The mirror image: open, but wearing a terminal stage label. It
			# is not finished, whatever the label says. Marked where it
			# stands rather than dragged back to the start — an item that
			# reached the end of the line has had work done on it, and
			# deciding how much of that still stands is for a person to say.
			| (($isClosed | not) and $mapped != null
				and ($terminal | index($mapped)) != null) as $reopened
			# And the fallback, for a pipeline that declares no terminal stage
			# at all: there is nowhere to send a finished item, so a closed
			# issue in a stage that is not the end of the line is reported as
			# having stopped there rather than as work this pipeline did.
			| ($isClosed and $done == "" and $mapped != null
				and (($terminal | index($mapped)) == null)) as $closedNonTerminal
			| (if $finished then $done else ($mapped // $default) end) as $stage
			| ({
				id:          "\($source):\(.number)",
				ref:         (.number | tostring),
				source:      $source,
				# First mapped label wins. An issue wearing two status labels is
				# already broken; picking deterministically beats erroring, and
				# the next move.sh clears the loser.
				stage:       $stage,
				title:       .title,
				# The mark rides beside the stage, never instead of it: this is
				# what keeps an unblocked issue resuming where it stopped. A
				# finished item wears no mark whatever its labels say — the
				# status settled it.
				blocked:     (if $finished then false
				              elif $reopened or $closedNonTerminal then true
				              else ($names | index($blocked)) != null end),
				# The block section is stripped: it is this pipeline talking to
				# a person, not part of the specification, and an agent handed
				# the prompt must not read its own last stop as a requirement.
				description: ($body | strip_block | strip_parent_marker),
				url:         .url,
				labels:      $names,
				# null, not 0: "unranked" and "most urgent" must stay distinct.
				priority:    ([$names[] | capture("^priority:p(?<n>[0-3])$") | .n | tonumber] | .[0]),
				assignee:    (.assignees | map(.login) | .[0] // ""),
				# finishedAt is the engines word for when a source considers an
				# item done; an open issue has none. closedAt is GitHubs own
				# vocabulary, only meaningful once an issue is actually closed.
				finishedAt:  (if $isClosed then (.closedAt // "") else "" end)
			}
			+ (if $parent != "" then {parent: $parent} else {} end)
			+ (if ($nativeChildren | length) > 0 then {children: $nativeChildren} else {} end)
			+ (if $markerParent != "" and $nativeParent == "" then {_markerParentRef: $markerParentRef} else {} end)
			+ (if $nativeChildrenTruncated then {_nativeChildrenTruncated: true} else {} end)
			+ (if ($relationWarnings | length) > 0 then {_relationshipWarnings: $relationWarnings} else {} end)
			+ (if $reopened then
				{blockKind: "status",
				 blockReason: "open on GitHub while labelled \"\($mapped)\", the end of the line: it was reopened, or it was never actually closed. Its status says it is not finished."}
			elif $closedNonTerminal then
				{blockKind: "status",
				 blockReason: "closed on GitHub while still in stage \"\($mapped)\", which is not terminal; a person should reconcile it."}
			else
				# Why it stopped, read straight off the item. The board no
				# longer depends on a run-history walk to say it, and it
				# survives a restart, a retention sweep and a mark somebody
				# set by hand.
				({blockKind: ($body | block_said | .kind // ""),
				  blockReason: ($body | block_said | .reason // "")}
					| with_entries(select(.value != "")))
			end)
			# Omitted entirely when the body declares nothing, rather
			# than an empty array: absence is what the item schema
			# already uses for "this field has nothing to say".
			+ (if ($deps | length) > 0 then {dependsOn: $deps} else {} end)
			# What the labels would have to say for this listing to be
			# reproducible. Only ever set when they do not already say it.
			+ (if $stage != ($mapped // $default) then {reconcile: $stage} else {} end))
		)' "$work/items.jsonl" >"$work/listed.json"

# CLOSED_LIMIT bounds the history ledger, not dependency correctness. Resolve
# any referenced node absent from that bounded slice. A completed issue is
# added back as a terminal dependency-only record; an open/unresolvable issue
# remains absent so NewDeps reports the declaration as invalid. The number of
# extra calls and records is bounded by references on the already-bounded
# listing, never by repository history.
mapfile -t missing_refs < <(jq -r '
	[.[].id] as $ids
	| [.[].dependsOn[]? | select(. as $d | ($ids | index($d) | not))]
	| unique[] | split(":")[-1]
' "$work/listed.json")
dependency_lookup_limit=${DEPENDENCY_LOOKUP_LIMIT:-50}
if [[ ! "$dependency_lookup_limit" =~ ^[0-9]+$ ]]; then
	echo "DEPENDENCY_LOOKUP_LIMIT must be a non-negative integer, got: $dependency_lookup_limit" >&2
	exit 1
fi
if ((${#missing_refs[@]} > dependency_lookup_limit)); then
	echo "dependency lookup limit $dependency_lookup_limit reached; remaining references stay missing and fail closed" >&2
fi
lookup_refs=("${missing_refs[@]:0:dependency_lookup_limit}")
for ref in "${lookup_refs[@]}"; do
	issue=""
	if issue=$(gh_issue "$ref"); then
		if [[ -n "$done_stage" ]] && jq -e '
			((.state // "") | ascii_downcase) == "closed"
			and ((.stateReason // "completed") | ascii_downcase) == "completed"
		' <<<"$issue" >/dev/null; then
			jq -c --arg source "$CONVEYOR_SOURCE" --arg done "$done_stage" '
				(.labels | map(.name)) as $names
				| {id: "\($source):\(.number)", ref: (.number | tostring), source: $source,
				   stage: $done, title: .title, blocked: false,
				   description: (.body // ""), url: .url, labels: $names,
				   priority: ([$names[] | capture("^priority:p(?<n>[0-3])$") | .n | tonumber] | .[0]),
				   assignee: (.assignees | map(.login) | .[0] // ""),
				   finishedAt: (.closedAt // "")}
			' <<<"$issue" >"$work/resolved.json"
			jq -s '.[0] + [.[1]]' "$work/listed.json" "$work/resolved.json" >"$work/listed.next"
			mv "$work/listed.next" "$work/listed.json"
		fi
	else
		status=$?
		if ((status != 2)); then exit "$status"; fi
	fi
done

# `gh issue list --json subIssues` embeds a bounded connection. Complete only
# the parents whose totalCount proves that connection was truncated.
mapfile -t truncated_parent_refs < <(jq -r '.[] | select(._nativeChildrenTruncated) | .ref' "$work/listed.json")
subissue_lookup_limit=${SUBISSUE_LOOKUP_LIMIT:-20}
if [[ ! "$subissue_lookup_limit" =~ ^[0-9]+$ ]]; then
	echo "SUBISSUE_LOOKUP_LIMIT must be a non-negative integer, got: $subissue_lookup_limit" >&2
	exit 1
fi
for ref in "${truncated_parent_refs[@]:0:subissue_lookup_limit}"; do
	if pages=$(gh_subissues "$ref"); then
		:
	else
		status=$?
		if ((status != 2)); then exit "$status"; fi
		jq --arg ref "$ref" '
			map(if .ref == $ref then
				._relationshipWarnings = ((._relationshipWarnings // []) +
					["GitHub would not return the complete native child set; the embedded partial set is retained"])
			else . end)
		' "$work/listed.json" >"$work/listed.next"
		mv "$work/listed.next" "$work/listed.json"
		continue
	fi
	children=$(jq -cn --argjson pages "$pages" --arg source "$CONVEYOR_SOURCE" --arg repo "$REPO" '
		[$pages[][]
			| (.html_url // .url // "") as $url
			| ([$url | capture("^https://github\\.com/(?<owner>[^/]+)/(?<name>[^/]+)/issues/[0-9]+$")] | .[0] // null) as $u
			| select($u != null and (((($u.owner + "/" + $u.name) | ascii_downcase) == ($repo | ascii_downcase))))
			| $source + ":" + (.number | tonumber | tostring)]
		| unique | sort
	')
	cross=$(jq -cn --argjson pages "$pages" --arg repo "$REPO" '
		[$pages[][]
			| (.html_url // .url // "") as $url
			| ([$url | capture("^https://github\\.com/(?<owner>[^/]+)/(?<name>[^/]+)/issues/[0-9]+$")] | .[0] // null) as $u
			| select(($u != null and (((($u.owner + "/" + $u.name) | ascii_downcase) == ($repo | ascii_downcase)))) | not)
			| "native child " + (if $url == "" then "<no URL>" else $url end)
				+ " is in another repository; cross-repository relationships are not representable until repo-to-source mapping is supported, so this link is omitted"]
	')
	jq --arg ref "$ref" --argjson children "$children" --argjson cross "$cross" '
		map(if .ref == $ref then
			.children = $children
			| ._relationshipWarnings = ((._relationshipWarnings // []) + $cross)
		else . end)
	' "$work/listed.json" >"$work/listed.next"
	mv "$work/listed.next" "$work/listed.json"
done
if ((${#truncated_parent_refs[@]} > subissue_lookup_limit)); then
	for ref in "${truncated_parent_refs[@]:subissue_lookup_limit}"; do
		jq --arg ref "$ref" --argjson n "$subissue_lookup_limit" '
			map(if .ref == $ref then
				._relationshipWarnings = ((._relationshipWarnings // []) +
					["sub-issue lookup limit " + ($n|tostring) + " reached; the embedded partial child set is retained"])
			else . end)
		' "$work/listed.json" >"$work/listed.next"
		mv "$work/listed.next" "$work/listed.json"
	done
fi

# A strict Parent: #N marker is durable fallback metadata, not proof that the
# target still exists. Confirm marker-only targets that are outside the visible
# listing. Native relationships already carry a GitHub object and need no
# extra lookup. Confirmed 404s become actionable warnings; transient failures
# still fail this listing so last-good state survives.
mapfile -t marker_parent_refs < <(jq -r '
	[.[].id] as $ids
	| [.[] | select(._markerParentRef != null)
		| select(.parent as $p | ($ids | index($p) | not))
		| ._markerParentRef]
	| unique[]
' "$work/listed.json")
relationship_lookup_limit=${RELATIONSHIP_LOOKUP_LIMIT:-50}
if [[ ! "$relationship_lookup_limit" =~ ^[0-9]+$ ]]; then
	echo "RELATIONSHIP_LOOKUP_LIMIT must be a non-negative integer, got: $relationship_lookup_limit" >&2
	exit 1
fi
for ref in "${marker_parent_refs[@]:0:relationship_lookup_limit}"; do
	if gh_issue "$ref" >/dev/null; then
		:
	else
		status=$?
		if ((status != 2)); then exit "$status"; fi
		jq --arg ref "$ref" --arg source "$CONVEYOR_SOURCE" '
			map(if ._markerParentRef == $ref then
				._relationshipWarnings = ((._relationshipWarnings // []) +
					["Parent marker names missing issue " + $source + ":" + $ref + "; repair or remove the first-line marker"])
			else . end)
		' "$work/listed.json" >"$work/listed.next"
		mv "$work/listed.next" "$work/listed.json"
	fi
done
if ((${#marker_parent_refs[@]} > relationship_lookup_limit)); then
	for ref in "${marker_parent_refs[@]:relationship_lookup_limit}"; do
		jq --arg ref "$ref" --argjson n "$relationship_lookup_limit" '
			map(if ._markerParentRef == $ref then
				._relationshipWarnings = ((._relationshipWarnings // []) +
					["relationship lookup limit " + ($n|tostring) + " reached; Parent marker " + $ref + " could not be verified"])
			else . end)
		' "$work/listed.json" >"$work/listed.next"
		mv "$work/listed.next" "$work/listed.json"
	done
fi

# A listed child's parent declaration is the fallback source for a parent's
# child set when GitHub returned no native sub-issues. A non-empty native set
# remains authoritative. Sorting after inversion makes response order inert.
jq '
	. as $all
	| map(. as $item
		| ([$all[] | select(.parent == $item.id) | .id] | unique | sort) as $inverse
		| if ((.children // []) | length) == 0 and ($inverse | length) > 0
			then .children = $inverse
			else . end)
' "$work/listed.json" >"$work/listed.next"
mv "$work/listed.next" "$work/listed.json"

# The labels catch up with the status, using the one script that writes labels
# rather than a second copy of its rules here. Rare — an item needs it once,
# after which its labels say what its status says — and never fatal: a listing
# that could not reconcile is still a correct listing, because every field
# above was derived from the status and not from the label.
if jq -e 'any(.reconcile != null)' "$work/listed.json" >/dev/null; then
	while IFS=$'\t' read -r n stage; do
		echo "issue #$n is $stage by its status but not by its labels; reconciling" >&2
		jq -cn --arg r "$n" --arg s "$stage" '{item: {ref: $r}, stage: $s, blocked: false}' |
			"$(dirname "${BASH_SOURCE[0]}")/move.sh" ||
			echo "could not reconcile #$n; the listing stands regardless" >&2
	done < <(jq -r '.[] | select(.reconcile) | "\(.ref)\t\(.reconcile)"' "$work/listed.json")
fi

jq '
	([.[] as $item | $item._relationshipWarnings[]?
		| {itemId: $item.id, reason: .}]) as $warnings
	| (map(del(.reconcile, ._relationshipWarnings, ._markerParentRef, ._nativeChildrenTruncated))) as $items
	| if ($warnings | length) > 0 then {items: $items, warnings: ($warnings | unique | sort_by(.itemId, .reason))}
		else $items end
' "$work/listed.json" >"$CONVEYOR_RESULT"

echo "listed $(jq 'if type == "array" then length else (.items | length) end' "$CONVEYOR_RESULT") item(s) (tag an issue $ONBOARD_LABEL to hand it over)" >&2
