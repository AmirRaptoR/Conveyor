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
cat >"$tmp/stub/gh" <<'STUB'
#!/usr/bin/env bash
cat <<'JSON'
[
 {"state":"OPEN","number":7,"title":"Checkout hangs","body":"spinner",
  "labels":[{"name":"bug"},{"name":"priority:p0"},{"name":"status:refining"}],
  "url":"https://example.test/7","assignees":[{"login":"amir"}]},
 {"state":"OPEN","number":9,"title":"Unranked","body":"","labels":[],
  "url":"https://example.test/9","assignees":[]},
 {"state":"OPEN","number":11,"title":"Needs a decision","body":"",
  "labels":[{"name":"status:in-progress"},{"name":"blocked"}],
  "url":"https://example.test/11","assignees":[]},
 {"state":"OPEN","number":13,"title":"Mine, not the pipeline's","body":"",
  "labels":[{"name":"conveyor:ignore"},{"name":"status:ready"}],
  "url":"https://example.test/13","assignees":[]},
 {"state":"OPEN","number":19,"title":"Handed to the pipeline","body":"",
  "labels":[{"name":"conveyor"},{"name":"enhancement"}],
  "url":"https://example.test/19","assignees":[]},
 {"number":15,"title":"Shipped and closed","body":"","state":"CLOSED",
  "labels":[{"name":"status:ready"}],
  "url":"https://example.test/15","assignees":[]},
 {"number":17,"title":"Closed years ago, never ours","body":"","state":"CLOSED",
  "labels":[{"name":"bug"}],
  "url":"https://example.test/17","assignees":[]},
 {"number":21,"title":"Tagged, then closed by hand","body":"","state":"CLOSED",
  "labels":[{"name":"conveyor"}],
  "url":"https://example.test/21","assignees":[]}
]
JSON
STUB
chmod +x "$tmp/stub/gh"

echo "list.sh"
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
PATH="$tmp/stub:$PATH" CONVEYOR_SOURCE=midgame CONVEYOR_RESULT="$tmp/named.json" \
	IGNORE_LABELS="status:ready, hold" ./list.sh 2>/dev/null
check "IGNORE_LABELS is read by nothing" \
	"$(jq -cS . "$tmp/out.json")" "$(jq -cS . "$tmp/named.json")"

check "a closed issue this pipeline labelled is still listed" \
	"ready" "$(jq -r '.[] | select(.ref == "15") | .stage' "$tmp/out.json")"
check "a closed issue it never labelled is left in history" \
	"" "$(jq -r '.[] | select(.ref == "17") | .ref' "$tmp/out.json")"
# The onboarding tag opens the door; it does not reopen a closed issue. A stage
# label is what says the pipeline actually had this one, and without it a closed
# issue wearing the tag would come back as new work every poll.
check "the tag does not drag a closed issue back onto the board" \
	"" "$(jq -r '.[] | select(.ref == "21") | .ref' "$tmp/out.json")"

# --- move.sh: stage -> label writes ----------------------------------------
# move.sh asks GitHub for the issue's current labels, so the stub answers that.
# $LABELS is the set the fake issue is wearing for each case below.
echo "move.sh (dry run)"
cat >"$tmp/stub/gh" <<'STUB'
#!/usr/bin/env bash
# One label per line: "blocked: limit" is one label, not two.
if [[ -n "$LABELS" ]]; then printf '%s\n' "$LABELS"; fi
STUB
chmod +x "$tmp/stub/gh"

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

# The kind rides beside the mark, never instead of it: "everything blocked" has
# to stay one query and one label to remove.
export LABELS="status:in-progress"
check "a kind adds its own label beside the mark" \
	"gh issue edit 9 --repo owner/repo --add-label blocked --add-label blocked: decision" \
	"$(echo '{"item":{"ref":"9"},"stage":"in-progress","blocked":true,"blockedKind":"decision"}' | dry | head -1)"
export LABELS=$'status:in-progress\nblocked\nblocked: limit'
check "a different kind replaces the old one" \
	"gh issue edit 9 --repo owner/repo --remove-label blocked: limit --add-label blocked: decision" \
	"$(echo '{"item":{"ref":"9"},"stage":"in-progress","blocked":true,"blockedKind":"decision"}' | dry | head -1)"
export LABELS=$'status:in-progress\nblocked\nblocked: limit'
check "clearing the mark takes the kind with it" \
	"gh issue edit 9 --repo owner/repo --remove-label status:in-progress --remove-label blocked --remove-label blocked: limit" \
	"$(echo '{"item":{"ref":"9"},"stage":"backlog"}' | dry | head -1)"

export LABELS=$'status:in-progress\nblocked'
check "entering a stage clears the mark" \
	"gh issue edit 9 --repo owner/repo --remove-label blocked" \
	"$(echo '{"item":{"ref":"9"},"stage":"in-progress","blocked":false}' | dry)"
export LABELS="status:in-progress"
check "a reason is commented when the mark goes on" \
	"gh issue comment 9 --repo owner/repo --body <reason>" \
	"$(echo '{"item":{"ref":"9"},"stage":"in-progress","blocked":true,"blockedReason":"needs a product call"}' | dry | tail -1)"

# --- move.sh: the reason is said once ---------------------------------------
# Not in dry run: this path asks GitHub what it already said, so the stub has to
# answer two different questions. The hourly stall retry clears marks and lets
# them come back, so without this every pass leaves another identical comment.
echo "move.sh (repeat suppression)"
cat >"$tmp/stub/gh" <<'STUB'
#!/usr/bin/env bash
case "$*" in
	*"--json comments"*) cat "$COMMENTS" ;;
	*"--json labels"*)   if [[ -n "$LABELS" ]]; then printf '%s\n' "$LABELS"; fi ;;
	*)                   echo "CALLED: gh $*" ;;
esac
STUB
chmod +x "$tmp/stub/gh"

wet() { PATH="$tmp/stub:$PATH" ./move.sh 2>&1 | grep -E '^(CALLED|the same reason)' || true; }
export LABELS="status:in-progress"
marking='{"item":{"ref":"9"},"stage":"in-progress","blocked":true,"blockedReason":"the checkout is dirty"}'

export COMMENTS="$tmp/none"; echo -n "" >"$tmp/none"
check "a first mark says why" \
	"yes" "$(echo "$marking" | wet | grep -q 'gh issue comment' && echo yes || echo no)"

export COMMENTS="$tmp/said"
printf '**Blocked** — the checkout is dirty\n' >"$tmp/said"
check "the same reason is not commented twice" \
	"the same reason is already commented on #9; not repeating it" \
	"$(echo "$marking" | wet | grep '^the same reason')"

export COMMENTS="$tmp/other"
printf '**Blocked** — something else entirely\n' >"$tmp/other"
check "a different reason is still said" \
	"yes" "$(echo "$marking" | wet | grep -q 'gh issue comment' && echo yes || echo no)"

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
cat <<'JSON'
[
 {"number":21,"title":"Stopped","body":"","state":"OPEN",
  "labels":[{"name":"conveyor:refining"},{"name":"conveyor:blocked"}],
  "url":"https://example.test/21","assignees":[]},
 {"number":23,"title":"Not ours","body":"","state":"OPEN",
  "labels":[{"name":"conveyor:ignore"}],
  "url":"https://example.test/23","assignees":[]},
 {"number":25,"title":"Handed over","body":"","state":"OPEN",
  "labels":[{"name":"conveyor"}],
  "url":"https://example.test/25","assignees":[]}
]
JSON
STUB
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
cat >"$tmp/stub/gh" <<'STUB'
#!/usr/bin/env bash
if [[ -n "$LABELS" ]]; then printf '%s\n' "$LABELS"; fi
STUB
chmod +x "$tmp/stub/gh"

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

[[ $fail -eq 0 ]] && echo "all checks passed" || echo "FAILURES"
exit $fail
