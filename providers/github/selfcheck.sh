#!/usr/bin/env bash
# Self-check for the github provider. Runs list.sh against a stubbed `gh` and
# move.sh in dry-run, so it touches no network and no repository.
#
#   ./providers/github/selfcheck.sh
#
# Named so it cannot be mistaken for a verb: config resolves list.* and move.*,
# and this matches neither.
set -euo pipefail
cd "$(dirname "$0")"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fail=0

check() { # check <label> <expected> <actual>
	if [[ "$2" == "$3" ]]; then
		echo "  ok   $1"
	else
		echo "  FAIL $1"
		echo "       want: $2"
		echo "       got:  $3"
		fail=1
	fi
}

export REPO="owner/repo"
# Explicit, and older than the prefix on purpose: a source that named its labels
# before LABEL_PREFIX existed must keep working exactly as it did.
export BLOCKED_LABEL="blocked"
export STAGE_LABELS='refining=status:refining
ready=status:ready
in-progress=status:in-progress'

# --- list.sh: provider shape -> item shape ---------------------------------
mkdir -p "$tmp/stub"
# list.sh asks gh for open and closed issues in two separate calls, so the
# stub has to answer each with only that state's issues — same as the real
# API. Answering both calls with the whole fixture double-listed every issue
# that a mapped label or the open state let through, which masked failures in
# every check below that selects a single issue by ref: two identical lines
# for its field never equal the single value being checked for.
cat >"$tmp/stub/gh" <<'STUB'
#!/usr/bin/env bash
data='[
 {"state":"OPEN","number":7,"title":"Checkout hangs","body":"spinner",
  "labels":[{"name":"bug"},{"name":"priority:p0"},{"name":"status:refining"}],
  "url":"https://example.test/7","assignees":[{"login":"amir"}]},
 {"state":"OPEN","number":9,"title":"Unranked","body":"","labels":[],
  "url":"https://example.test/9","assignees":[]},
 {"state":"OPEN","number":11,"title":"Needs a decision","body":"",
  "labels":[{"name":"status:in-progress"},{"name":"blocked"}],
  "url":"https://example.test/11","assignees":[]},
 {"state":"OPEN","number":13,"title":"Mine, not the pipeline'"'"'s","body":"",
  "labels":[{"name":"conveyor:ignore"},{"name":"status:ready"}],
  "url":"https://example.test/13","assignees":[]},
 {"state":"OPEN","number":19,"title":"Handed to the pipeline","body":"",
  "labels":[{"name":"conveyor"},{"name":"enhancement"}],
  "url":"https://example.test/19","assignees":[]},
 {"number":15,"title":"Shipped and closed","body":"","state":"CLOSED",
  "labels":[{"name":"status:ready"}],
  "url":"https://example.test/15","assignees":[],"closedAt":"2026-08-30T12:00:00Z"},
 {"number":17,"title":"Closed years ago, never ours","body":"","state":"CLOSED",
  "labels":[{"name":"bug"}],
  "url":"https://example.test/17","assignees":[]},
 {"number":21,"title":"Tagged, then closed by hand","body":"","state":"CLOSED",
  "labels":[{"name":"conveyor"}],
  "url":"https://example.test/21","assignees":[]},
 {"number":27,"title":"Closed mid-flight","body":"","state":"CLOSED",
  "labels":[{"name":"status:in-progress"}],
  "url":"https://example.test/27","assignees":[],"closedAt":"2026-08-29T00:00:00Z"}
]'
case "$*" in
	*"--state open"*)   jq '[.[] | select(.state == "OPEN")]' <<<"$data" ;;
	*"--state closed"*) jq '[.[] | select(.state == "CLOSED")]' <<<"$data" ;;
	*)                  echo "$data" ;;
esac
STUB
chmod +x "$tmp/stub/gh"

echo "list.sh"
# "ready" is this fixture's one terminal stage — ref 15 (closed, mapped
# "ready") is the "finished" case; ref 27 (closed, mapped "in-progress", which
# is not terminal) is the "stopped mid-flight" case below.
echo '{"terminalStages":["ready"]}' |
	PATH="$tmp/stub:$PATH" CONVEYOR_SOURCE=midgame CONVEYOR_RESULT="$tmp/out.json" \
		./list.sh 2>/dev/null

check "maps a status label to its stage" \
	"refining" "$(jq -r '.[0].stage' "$tmp/out.json")"
# Listing is opt-in. An issue nobody tagged is not conveyor's — not on the
# board, not in a count, and no stage is ever run against it.
check "an untagged issue is not listed at all" \
	"" "$(jq -r '.[] | select(.ref == "9") | .ref' "$tmp/out.json")"
# ...and the tag is the whole of onboarding. The bare namespace word, with no
# stage named beside it, is how a person hands an issue over: it says "yours"
# and nothing else, so the issue lands wherever new work lands.
check "the bare tag onboards into DEFAULT_STAGE" \
	"backlog" "$(jq -r '.[] | select(.ref == "19") | .stage' "$tmp/out.json")"
check "priority:p0 becomes 0" \
	"0" "$(jq -r '.[0].priority' "$tmp/out.json")"
# null, not 0 — the model keeps "unranked" and "most urgent" distinct.
check "no priority label stays null" \
	"null" "$(jq -r '.[] | select(.ref == "11") | .priority' "$tmp/out.json")"
check "id is source-qualified" \
	"midgame:7" "$(jq -r '.[0].id' "$tmp/out.json")"
# The mark is beside the stage, never instead of it: an unblocked issue must
# resume where it stopped, which it cannot do if the stage was overwritten.
check "the blocked label becomes a mark" \
	"true" "$(jq -r '.[] | select(.ref == "11") | .blocked' "$tmp/out.json")"
check "a marked issue keeps the stage it stopped in" \
	"in-progress" "$(jq -r '.[] | select(.ref == "11") | .stage' "$tmp/out.json")"
check "an unmarked issue is not blocked" \
	"false" "$(jq -r '.[0].blocked' "$tmp/out.json")"

# There is no ignore list any more, because not listing is what happens by
# default: an issue is left alone by saying nothing about it. The old opt-out
# label is now an ordinary word a repository may keep or delete, and it decides
# nothing — #13 is on the board because it wears a stage label, and that is the
# only question asked of it.
check "the old ignore label no longer keeps an issue off the board" \
	"ready" "$(jq -r '.[] | select(.ref == "13") | .stage' "$tmp/out.json")"

# The variable is gone, not merely defaulted: a config still setting it must not
# quietly change what is listed.
echo '{"terminalStages":["ready"]}' |
	PATH="$tmp/stub:$PATH" CONVEYOR_SOURCE=midgame CONVEYOR_RESULT="$tmp/named.json" \
		IGNORE_LABELS="status:ready, hold" ./list.sh 2>/dev/null
check "IGNORE_LABELS is read by nothing" \
	"$(jq -cS . "$tmp/out.json")" "$(jq -cS . "$tmp/named.json")"

# --- a flaky call does not cost the whole listing ---------------------------
#
# GitHub answers 503 now and then. One of those used to fail the script, and a
# failed listing marks the source stale: nothing routed through that repository
# was dispatched until a later poll succeeded. The listing stays all-or-nothing
# — a partial union would read as "those items are gone" — so the retry sits at
# the one call that actually flakes.
echo "list.sh (a flaky gh)"
cat >"$tmp/stub/gh" <<'STUB'
#!/usr/bin/env bash
n=$(cat "$ATTEMPTS" 2>/dev/null || echo 0)
n=$((n + 1)); echo "$n" >"$ATTEMPTS"
if [[ "$*" == *"--state open"* && "$n" -le "${FAIL_FIRST:-0}" ]]; then
	echo "HTTP 503: 503 Service Unavailable (https://api.github.com/graphql)" >&2
	exit 1
fi
case "$*" in
	*"--state open"*)
		echo '[{"state":"OPEN","number":51,"title":"Survived a 503","body":"",
		        "labels":[{"name":"status:refining"}],
		        "url":"https://example.test/51","assignees":[]}]' ;;
	*) echo '[]' ;;
esac
STUB
chmod +x "$tmp/stub/gh"
export ATTEMPTS="$tmp/attempts"

: >"$ATTEMPTS"
echo '{"terminalStages":["ready"]}' |
	PATH="$tmp/stub:$PATH" CONVEYOR_SOURCE=midgame CONVEYOR_RESULT="$tmp/flaky.json" \
		FAIL_FIRST=1 GH_ATTEMPTS=3 ./list.sh 2>/dev/null
check "a transient failure is retried, not fatal" \
	"refining" "$(jq -r '.[] | select(.ref == "51") | .stage' "$tmp/flaky.json")"

# Bounded: a repository that is genuinely unreachable fails the listing rather
# than retrying forever, so the engine can mark the source stale and say so.
: >"$ATTEMPTS"
rc=0
echo '{"terminalStages":["ready"]}' |
	PATH="$tmp/stub:$PATH" CONVEYOR_SOURCE=midgame CONVEYOR_RESULT="$tmp/never.json" \
		FAIL_FIRST=99 GH_ATTEMPTS=2 ./list.sh >/dev/null 2>&1 || rc=$?
check "a call that never succeeds fails the listing" "yes" "$([[ $rc -ne 0 ]] && echo yes || echo no)"
check "     after exactly the attempts it was given" "2" "$(cat "$ATTEMPTS")"

# --- the issue status is the truth about the item ---------------------------
#
# A pull request merged by hand closes the issue without this pipeline moving
# anything. The item used to be marked in place for it, and the mark could
# never be cleared: a merged PR never becomes open again, so nothing would ever
# hand it back, and the board filled with red cards for work that had shipped.
# The status settles it now — and the labels are reconciled to match, through
# move.sh rather than a second copy of its rules.
echo "list.sh (status is the truth)"
cat >"$tmp/stub/gh" <<'STUB'
#!/usr/bin/env bash
data='[
 {"state":"CLOSED","stateReason":"COMPLETED","number":41,"title":"Merged by hand",
  "body":"","labels":[{"name":"status:in-progress"}],
  "url":"https://example.test/41","assignees":[],"closedAt":"2026-09-01T00:00:00Z"},
 {"state":"CLOSED","stateReason":"NOT_PLANNED","number":43,"title":"Abandoned",
  "body":"","labels":[{"name":"status:in-progress"}],
  "url":"https://example.test/43","assignees":[],"closedAt":"2026-09-01T00:00:00Z"},
 {"state":"OPEN","stateReason":"REOPENED","number":45,"title":"Reopened after shipping",
  "body":"","labels":[{"name":"status:ready"}],
  "url":"https://example.test/45","assignees":[]},
 {"state":"CLOSED","stateReason":"COMPLETED","number":47,"title":"Finished properly",
  "body":"","labels":[{"name":"status:ready"}],
  "url":"https://example.test/47","assignees":[],"closedAt":"2026-09-02T00:00:00Z"}
]'
case "$*" in
	*"issue edit"*)     echo "RECONCILED: gh $*" >>"$RECONCILED" ;;
	*"--state open"*)   jq '[.[] | select(.state == "OPEN")]' <<<"$data" ;;
	*"--state closed"*) jq '[.[] | select(.state == "CLOSED")]' <<<"$data" ;;
	*"--json labels,body"*)
		jq -n --arg l "status:in-progress" '{labels: [{name: $l}], body: ""}' ;;
	*)                  echo "$data" ;;
esac
STUB
chmod +x "$tmp/stub/gh"
export RECONCILED="$tmp/reconciled"
: >"$RECONCILED"
echo '{"stages":["backlog","in-progress","ready"],"terminalStages":["ready"]}' |
	PATH="$tmp/stub:$PATH" CONVEYOR_SOURCE=midgame CONVEYOR_RESULT="$tmp/truth.json" \
		./list.sh 2>/dev/null

check "closed as completed is finished, whatever label it wears" \
	"ready" "$(jq -r '.[] | select(.ref == "41") | .stage' "$tmp/truth.json")"
check "     and is not marked" \
	"false" "$(jq -r '.[] | select(.ref == "41") | .blocked' "$tmp/truth.json")"
# The end of the line is the pipeline's own first terminal stage, read off the
# stage order it was handed — not a name this script keeps a copy of.
check "     landing in the first terminal stage in pipeline order" \
	"ready" "$(jq -r '.[] | select(.ref == "47") | .stage' "$tmp/truth.json")"
# Closed as anything but completed is abandoned: off the board entirely. Not
# marked, not counted, and no stage is ever run against it again.
check "closed as not planned leaves the board" \
	"" "$(jq -r '.[] | select(.ref == "43") | .ref' "$tmp/truth.json")"
# Open while labelled done is the mirror image, and it is not silently
# believed either way: the item keeps its stage and is marked for a person.
check "open in a terminal stage keeps its stage" \
	"ready" "$(jq -r '.[] | select(.ref == "45") | .stage' "$tmp/truth.json")"
check "     and is marked" \
	"true" "$(jq -r '.[] | select(.ref == "45") | .blocked' "$tmp/truth.json")"
check "     with a reason naming the contradiction" \
	"yes" "$(jq -r '.[] | select(.ref == "45") | .blockReason' "$tmp/truth.json" | grep -q 'not finished' && echo yes || echo no)"
# The labels catch up, so the next listing derives the same answer from the
# labels alone and this reconciliation happens once per item, not every poll.
check "a mismatched label is reconciled through move.sh" \
	"yes" "$(grep -q -- "--add-label status:ready" "$RECONCILED" && echo yes || echo no)"
check "an item whose labels already agree is left alone" \
	"" "$(grep -c "issue edit 47" "$RECONCILED" | grep -v '^0$' || true)"
check "reconcile never reaches the engine" \
	"null" "$(jq -r '.[0].reconcile' "$tmp/truth.json")"

check "a closed issue this pipeline labelled is still listed" \
	"ready" "$(jq -r '.[] | select(.ref == "15") | .stage' "$tmp/out.json")"
# finishedAt is the engine's word for when the source considers an item done,
# projected from closedAt — the first timestamp any listing has ever carried.
check "a closed issue's item carries finishedAt from closedAt" \
	"2026-08-30T12:00:00Z" "$(jq -r '.[] | select(.ref == "15") | .finishedAt' "$tmp/out.json")"
check "an open issue's item carries an empty finishedAt" \
	"" "$(jq -r '.[] | select(.ref == "7") | .finishedAt' "$tmp/out.json")"
# --- F09: closed-issue routing depends on whether the mapped stage is terminal
# "ready" is terminal here, so ref 15 above is unmarked and simply finished.
check "closed + terminal stage is unmarked"    \
	"false" "$(jq -r '.[] | select(.ref == "15") | .blocked' "$tmp/out.json")"
# "in-progress" is not, so ref 27 stopped mid-flight: same stage, marked, with
# a reason a person can read without opening the logs — not silently finished.
check "closed + non-terminal stage keeps its stage" \
	"in-progress" "$(jq -r '.[] | select(.ref == "27") | .stage' "$tmp/out.json")"
check "     and is marked"                     \
	"true" "$(jq -r '.[] | select(.ref == "27") | .blocked' "$tmp/out.json")"
check "     with a human-readable reason"      \
	"true" "$([[ -n "$(jq -r '.[] | select(.ref == "27") | .blockReason' "$tmp/out.json")" ]] && echo true)"
check "a closed issue it never labelled is left in history" \
	"" "$(jq -r '.[] | select(.ref == "17") | .ref' "$tmp/out.json")"
# The onboarding tag opens the door; it does not reopen a closed issue. A stage
# label is what says the pipeline actually had this one, and without it a closed
# issue wearing the tag would come back as new work every poll.
check "the tag does not drag a closed issue back onto the board" \
	"" "$(jq -r '.[] | select(.ref == "21") | .ref' "$tmp/out.json")"

# --- F09: an enrolled open issue is found no matter how much unrelated open
# work sits ahead of it ------------------------------------------------------
#
# Each enrolling label is its own server-side-filtered `gh` call, unioned —
# never one `--state open --limit N` call truncated before enrolment is even
# checked. The stub below plays out the old bug directly: the one unfiltered
# call this test can still provoke (a caller with no --label at all) returns
# 200 newer unrelated issues and never the enrolled one, which is only ever
# visible through its own label's call.
echo "discovery completeness"
(
	export STAGE_LABELS='refining=status:refining'
	cat >"$tmp/stub/gh" <<'STUB'
#!/usr/bin/env bash
case "$*" in
*"--state open --label status:refining"*)
	echo '[{"number":9001,"title":"The enrolled one","body":"","state":"OPEN","labels":[{"name":"status:refining"}],"url":"u","assignees":[]}]'
	;;
*"--state open --label"*) echo '[]' ;;
*"--state closed"*)       echo '[]' ;;
*"--state open"*)
	jq -n '[range(200) | {number: (9500 - .), title: "unrelated", body: "", state: "OPEN", labels: [], url: "u", assignees: []}]'
	;;
*) echo "stub gh: unhandled: $*" >&2; exit 97 ;;
esac
STUB
	echo '{}' | PATH="$tmp/stub:$PATH" CONVEYOR_SOURCE=midgame CONVEYOR_RESULT="$tmp/deep.json" \
		./list.sh 2>/dev/null
	check "an enrolled issue behind 200 unrelated ones is still found" \
		"9001" "$(jq -r '.[] | select(.stage == "refining") | .ref' "$tmp/deep.json")"
) || fail=1

# --- F11: a listing bigger than one command-line argument still lists -------
#
# The issue payload never travels through argv. Linux caps a *single* argument
# at MAX_ARG_STRLEN — 128 KiB, whatever total ARG_MAX is left — so handing the
# accumulated JSON to `jq --argjson` died with "Argument list too long" as soon
# as a repository's issues outgrew that: one real repo's hundred most recently
# closed issues serialise to 560 KB. jq never exec'd, the script exited 126 and
# the source listed nothing, so a whole board stalled because a repository was
# busy. Both directions are checked, because the open side blew up in a
# different place from the closed one: the open accumulator crossed the cap
# part-way through the per-label union, the closed side on its single blob.
echo "listing past the argv limit"
(
	export STAGE_LABELS='refining=status:refining'
	# The bodies are built inside jq rather than passed to it: a stub that
	# needs argv to describe the payload cannot test getting off argv.
	cat >"$tmp/stub/gh" <<'STUB'
#!/usr/bin/env bash
case "$*" in
*"--state open"*)
	jq -n '[{number:9001,title:"fat and open",body:("x" * 200000),state:"OPEN",
	         labels:[{name:"status:refining"}],url:"u",assignees:[]}]'
	;;
*"--state closed"*)
	jq -n '[{number:9002,title:"fat and closed",body:("x" * 200000),state:"CLOSED",
	         labels:[{name:"status:refining"}],url:"u",assignees:[],
	         closedAt:"2026-08-30T12:00:00Z"}]'
	;;
*) echo "stub gh: unhandled: $*" >&2; exit 97 ;;
esac
STUB
	echo '{"terminalStages":["refining"]}' |
		PATH="$tmp/stub:$PATH" CONVEYOR_SOURCE=midgame CONVEYOR_RESULT="$tmp/fat.json" \
			./list.sh 2>/dev/null
	check "an open issue too big for argv is listed" \
		"9001" "$(jq -r '.[] | select(.ref == "9001") | .ref' "$tmp/fat.json")"
	check "a closed issue too big for argv is listed" \
		"9002" "$(jq -r '.[] | select(.ref == "9002") | .ref' "$tmp/fat.json")"
	# The union still deduplicates: every enrolling label's call answered with
	# the same open issue, and it must appear once.
	check "the oversized open issue is listed once, not once per label" \
		"1" "$(jq '[.[] | select(.ref == "9001")] | length' "$tmp/fat.json")"
) || fail=1

# --- move.sh: stage -> label writes ----------------------------------------
# move.sh asks GitHub for the issue's current labels, so the stub answers that.
# $LABELS is the set the fake issue is wearing for each case below.
echo "move.sh (dry run)"
# $LABELS is one label per line ("blocked: limit" is one label, not two) and
# $BODY is the issue body; the stub assembles the JSON move.sh actually reads.
issue_view_stub() {
	cat >"$tmp/stub/gh" <<'STUB'
#!/usr/bin/env bash
jq -n --arg l "${LABELS:-}" --arg b "${BODY:-}" \
	'{labels: ($l | split("\n") | map(select(. != "")) | map({name: .})), body: $b}'
STUB
	chmod +x "$tmp/stub/gh"
}
issue_view_stub
export BODY=""

dry() { PATH="$tmp/stub:$PATH" CONVEYOR_DRY_RUN=1 ./move.sh 2>&1 | grep -oP '(?<=DRY RUN: ).*' || true; }

export LABELS=$'bug\nstatus:refining'
check "swaps the old status label for the new" \
	"gh issue edit 7 --repo owner/repo --remove-label status:refining --add-label status:ready" \
	"$(echo '{"item":{"ref":"7"},"stage":"ready"}' | dry)"
# The engine writes provider state before every stage run, so this path is hot.
export LABELS="status:ready"
check "already-correct label writes nothing" \
	"" \
	"$(echo '{"item":{"ref":"7"},"stage":"ready"}' | dry)"
export LABELS="status:ready"
check "unmapped stage still clears a stale label" \
	"gh issue edit 7 --repo owner/repo --remove-label status:ready" \
	"$(echo '{"item":{"ref":"7"},"stage":"backlog"}' | dry)"
export LABELS=""
check "fresh issue only adds" \
	"gh issue edit 9 --repo owner/repo --add-label status:in-progress" \
	"$(echo '{"item":{"ref":"9"},"stage":"in-progress"}' | dry)"

# The mark: set and cleared by the same call, and never at the cost of the
# status label — that is what makes unblocking resume rather than restart.
export LABELS="status:in-progress"
check "blocking marks in place and keeps the stage" \
	"gh issue edit 9 --repo owner/repo --add-label blocked" \
	"$(echo '{"item":{"ref":"9"},"stage":"in-progress","blocked":true}' | dry | head -1)"
export LABELS=$'status:in-progress\nblocked'
check "a set mark is not set twice" \
	"" \
	"$(echo '{"item":{"ref":"9"},"stage":"in-progress","blocked":true}' | dry)"

# The kind used to be a label of its own beside the mark. It is not any more —
# it rides in the body section with the reason it belongs to — but a repository
# that ran the old code is still wearing them, so they come off wherever found.
export LABELS="status:in-progress"
check "a kind adds no label of its own" \
	"gh issue edit 9 --repo owner/repo --add-label blocked --body-file <body>" \
	"$(echo '{"item":{"ref":"9"},"stage":"in-progress","blocked":true,"blockedKind":"decision","blockedReason":"pick one"}' | dry | head -1)"
export LABELS=$'status:in-progress\nblocked\nblocked: limit'
check "an old kind label is taken off" \
	"yes" \
	"$(echo '{"item":{"ref":"9"},"stage":"in-progress","blocked":true,"blockedKind":"decision","blockedReason":"pick one"}' | dry | grep -q -- "--remove-label blocked: limit" && echo yes || echo no)"
export LABELS=$'status:in-progress\nblocked\nblocked: limit'
check "clearing the mark takes the old kind label with it" \
	"gh issue edit 9 --repo owner/repo --remove-label status:in-progress --remove-label blocked --remove-label blocked: limit" \
	"$(echo '{"item":{"ref":"9"},"stage":"backlog"}' | dry | head -1)"

export LABELS=$'status:in-progress\nblocked'
check "entering a stage clears the mark" \
	"gh issue edit 9 --repo owner/repo --remove-label blocked" \
	"$(echo '{"item":{"ref":"9"},"stage":"in-progress","blocked":false}' | dry)"
# --- move.sh: the reason is a field on the item -----------------------------
#
# It used to be a comment, which is append-only: an hourly stall retry clearing
# marks and letting them come straight back left another identical comment on
# the same issue every pass, and the reason that matters is the current one.
# It is one section of the body now — rewritten in place, gone when the mark
# goes — so there is nothing to repeat and nothing to suppress.
echo "move.sh (why, as a field)"

# Not dry: the file is what carries the body, so this path runs for real
# against a stub that records rather than writes.
cat >"$tmp/stub/gh" <<'STUB'
#!/usr/bin/env bash
if [[ "$*" == *"--json"* ]]; then
	jq -n --arg l "${LABELS:-}" --arg b "${BODY:-}" \
		'{labels: ($l | split("\n") | map(select(. != "")) | map({name: .})), body: $b}'
	exit 0
fi
while [[ $# -gt 0 ]]; do
	if [[ "$1" == "--body-file" ]]; then cat "$2" >"$WROTE"; fi
	shift
done
STUB
chmod +x "$tmp/stub/gh"
wrote() { WROTE="$tmp/wrote" PATH="$tmp/stub:$PATH" ./move.sh 2>/dev/null; cat "$tmp/wrote"; }
export WROTE="$tmp/wrote"

export LABELS="status:in-progress"
export BODY="The spec.

Second paragraph."
: >"$tmp/wrote"
out=$(echo '{"item":{"ref":"9"},"stage":"in-progress","blocked":true,"blockedKind":"decision","blockedReason":"pick a --> or b"}' | wrote)
check "the reason lands in the body, above the spec" \
	"yes" "$(grep -q '^> pick a --> or b$' <<<"$out" && echo yes || echo no)"
check "the spec itself survives" \
	"yes" "$(grep -q '^Second paragraph.$' <<<"$out" && echo yes || echo no)"
# The marker carries the same fact as base64 JSON, so list.sh reads back
# exactly what was written — a reason containing "-->" included, which is
# why it is encoded rather than written into the comment as it stands.
check "the marker round-trips the reason exactly" \
	"pick a --> or b" \
	"$(grep -oP '(?<=<!-- conveyor:block )[A-Za-z0-9+/=]+' <<<"$out" | base64 -d | jq -r .reason)"
check "and the kind with it" \
	"decision" \
	"$(grep -oP '(?<=<!-- conveyor:block )[A-Za-z0-9+/=]+' <<<"$out" | base64 -d | jq -r .kind)"

# Rewritten in place: the same stop said twice is one section, not two.
export BODY="$out"
: >"$tmp/wrote"
again=$(echo '{"item":{"ref":"9"},"stage":"in-progress","blocked":true,"blockedKind":"decision","blockedReason":"pick a --> or b"}' | wrote)
check "an unchanged reason rewrites nothing" "" "$again"

export BODY="$out"
: >"$tmp/wrote"
changed=$(echo '{"item":{"ref":"9"},"stage":"in-progress","blocked":true,"blockedKind":"limit","blockedReason":"out of quota"}' | wrote)
check "a different reason replaces the section rather than adding one" \
	"1" "$(grep -c '^<!-- conveyor:block ' <<<"$changed")"

# Clearing the mark takes the section with it, and leaves the spec alone.
export LABELS=$'status:in-progress\nblocked'
export BODY="$out"
: >"$tmp/wrote"
cleared=$(echo '{"item":{"ref":"9"},"stage":"in-progress","blocked":false}' | wrote)
check "unblocking removes the section" \
	"The spec.

Second paragraph." "$cleared"

issue_view_stub
export BODY=""

# --- a label that does not exist yet ---------------------------------------
#
# A stage added to the config after a repository was onboarded names a label
# that repository has never seen, and `gh issue edit --add-label` fails outright
# on one. That marked the item with an `error` the moment it reached the new
# stage, which is a confusing way to be told to run onboard.sh.
echo "move.sh (a label the repo does not have yet)"
cat >"$tmp/stub/gh" <<'STUB'
#!/usr/bin/env bash
case "$*" in
	*"--json"*)
		jq -n --arg l "${LABELS:-}" --arg b "${BODY:-}" \
			'{labels: ($l | split("\n") | map(select(. != "")) | map({name: .})), body: $b}' ;;
	*"label create"*)
		echo "CREATED: $3" >>"$CALLS"; exit 0 ;;
	*"issue edit"*)
		n=$(cat "$EDITS" 2>/dev/null || echo 0); n=$((n + 1)); echo "$n" >"$EDITS"
		if [[ "$n" -le "${EDIT_FAILS:-0}" ]]; then
			echo "could not add label: 'status:new' not found" >&2; exit 1
		fi
		echo "EDITED" >>"$CALLS" ;;
esac
STUB
chmod +x "$tmp/stub/gh"
export CALLS="$tmp/calls" EDITS="$tmp/edits"
saved_labels=$STAGE_LABELS
export STAGE_LABELS='new=status:new'
export LABELS="" BODY=""

: >"$CALLS"; : >"$EDITS"
echo '{"item":{"ref":"61"},"stage":"new"}' | PATH="$tmp/stub:$PATH" EDIT_FAILS=1 ./move.sh 2>/dev/null
check "the missing label is created" "yes" "$(grep -q "^CREATED: status:new" "$CALLS" && echo yes || echo no)"
check "and the edit is retried once it exists" "yes" "$(grep -q "^EDITED" "$CALLS" && echo yes || echo no)"
check "     exactly twice, never in a loop" "2" "$(cat "$EDITS")"

# A failure nothing created explains is a failure. Retrying an identical call
# that broke for some other reason is how a provider write becomes a rate limit.
: >"$CALLS"; : >"$EDITS"
rc=0
echo '{"item":{"ref":"61"},"stage":"new"}' | PATH="$tmp/stub:$PATH" EDIT_FAILS=9 ./move.sh >/dev/null 2>&1 || rc=$?
check "a failure no missing label explains is still a failure" "yes" "$([[ $rc -ne 0 ]] && echo yes || echo no)"
check "     and it did not retry forever" "2" "$(cat "$EDITS")"

export STAGE_LABELS=$saved_labels
issue_view_stub

# --- the label namespace ----------------------------------------------------
#
# Every label this pipeline owns begins with one prefix, so a repository can see
# at a glance which of its labels a machine writes — and so move.sh can find
# what it manages by shape instead of by a list that goes stale the moment a
# stage is renamed.
echo "label namespace"
(
	unset BLOCKED_LABEL
	export STAGE_LABELS='refining=conveyor:refining
ready=conveyor:ready'
	cat >"$tmp/stub/gh" <<'STUB'
#!/usr/bin/env bash
data='[
 {"number":21,"title":"Stopped","body":"","state":"OPEN",
  "labels":[{"name":"conveyor:refining"},{"name":"conveyor:blocked"}],
  "url":"https://example.test/21","assignees":[]},
 {"number":23,"title":"Not ours","body":"","state":"OPEN",
  "labels":[{"name":"conveyor:ignore"}],
  "url":"https://example.test/23","assignees":[]},
 {"number":25,"title":"Handed over","body":"","state":"OPEN",
  "labels":[{"name":"conveyor"}],
  "url":"https://example.test/25","assignees":[]}
]'
case "$*" in
	*"--state open"*)   jq '[.[] | select(.state == "OPEN")]' <<<"$data" ;;
	*"--state closed"*) jq '[.[] | select(.state == "CLOSED")]' <<<"$data" ;;
	*)                  echo "$data" ;;
esac
STUB
	echo '{}' |
		PATH="$tmp/stub:$PATH" CONVEYOR_SOURCE=midgame CONVEYOR_RESULT="$tmp/ns.json" \
			./list.sh 2>/dev/null
	check "the mark defaults into the namespace" \
		"true" "$(jq -r '.[] | select(.ref == "21") | .blocked' "$tmp/ns.json")"
	# The tag is the namespace word without its separator, so renaming the
	# prefix renames the tag and there is no second key to keep in step.
	check "the onboarding tag comes off the same prefix" \
		"backlog" "$(jq -r '.[] | select(.ref == "25") | .stage' "$tmp/ns.json")"
	# A label merely inside the namespace is not a way in. Three labels open the
	# door — a stage, the mark, the tag — and anything else a repository invents
	# under the prefix says nothing about whether this issue is the pipeline's.
	check "a namespace label that is none of the three does not onboard" \
		"" "$(jq -r '.[] | select(.ref == "23") | .ref' "$tmp/ns.json")"
) || fail=1

# A stage renamed in config used to orphan its old label forever: managed was
# the right-hand sides of the mappings alone, so the label a previous name wrote
# was never taken off again — and listing takes the first mapped label it finds,
# which is how an item ends up in a stage nobody put it in. Owning the whole
# prefix means a label this pipeline wrote is one it can still remove.
# The namespace section above rewrote the shared stub to answer `issue list`;
# move.sh asks it for `issue view --json labels`, so put that one back.
issue_view_stub

export LABELS=$'bug\nconveyor:refining'
saved_labels=$STAGE_LABELS
export STAGE_LABELS='ready=conveyor:ready'
out=$(echo '{"item":{"ref":"31"},"stage":"ready"}' | dry)
export STAGE_LABELS=$saved_labels
check "a label a renamed stage left behind is still removed" \
	"yes" "$(grep -q -- "--remove-label conveyor:refining" <<<"$out" && echo yes || echo no)"
check "and the new one is added" \
	"yes" "$(grep -q -- "--add-label conveyor:ready" <<<"$out" && echo yes || echo no)"
check "a label outside the prefix is left alone" \
	"no" "$(grep -q -- "--remove-label bug" <<<"$out" && echo yes || echo no)"

# --- F10: ownership is starts-with, not substring ---------------------------
#
# grep -F "$LABEL_PREFIX" matches the namespace anywhere in the label, not just
# at its start, so a repository's own "team-conveyor:keep" was swept into the
# managed set and removed on the next move. This reproduces that and pins the
# fix to a literal starts-with test.
echo "label ownership (starts-with, not substring)"
saved_labels=$STAGE_LABELS
saved_prefix=${LABEL_PREFIX:-}

export STAGE_LABELS='implementing=conveyor:implementing'
export LABEL_PREFIX="conveyor:"
export LABELS="team-conveyor:keep"
check "a label merely containing the prefix is left alone" \
	"" \
	"$(echo '{"item":{"ref":"41"},"stage":"backlog"}' | dry)"

export LABELS=$'team-conveyor:keep\nconveyor:implementing'
check "a label actually starting with the prefix is still removed" \
	"gh issue edit 41 --repo owner/repo --remove-label conveyor:implementing" \
	"$(echo '{"item":{"ref":"41"},"stage":"backlog"}' | dry)"

# A custom prefix with regex metacharacters must match only literally: '.'
# does not stand for "any character" and '[x]' is not a character class.
export STAGE_LABELS=""
export LABEL_PREFIX='conv.yor[x]:'
export LABELS=$'convXyor[x]:a\nconv.yor[x]:a'
check "a regex-special prefix matches only literally" \
	"gh issue edit 41 --repo owner/repo --remove-label conv.yor[x]:a" \
	"$(echo '{"item":{"ref":"41"},"stage":"backlog"}' | dry)"

export STAGE_LABELS=$saved_labels
export LABEL_PREFIX=$saved_prefix

# The mapping itself must be parsed as data: a stage name with '/', '.' or '['
# used to be spliced into a sed pattern, where '.' and an unbalanced '[' are
# regex metacharacters rather than literal text.
echo "stage names as data (no sed interpolation)"
saved_labels=$STAGE_LABELS
export STAGE_LABELS='feat/review=status:feat-review
a.b=status:dot
c[d]=status:bracket'
export LABELS=""
check "a stage name containing / resolves" \
	"gh issue edit 41 --repo owner/repo --add-label status:feat-review" \
	"$(echo '{"item":{"ref":"41"},"stage":"feat/review"}' | dry)"
check "a stage name containing . resolves literally" \
	"gh issue edit 41 --repo owner/repo --add-label status:dot" \
	"$(echo '{"item":{"ref":"41"},"stage":"a.b"}' | dry)"
check "a stage name containing [ resolves" \
	"gh issue edit 41 --repo owner/repo --add-label status:bracket" \
	"$(echo '{"item":{"ref":"41"},"stage":"c[d]"}' | dry)"
export STAGE_LABELS=$saved_labels

[[ $fail -eq 0 ]] && echo "all checks passed" || echo "FAILURES"
exit $fail
