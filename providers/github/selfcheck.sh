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
export STAGE_LABELS='refining=status:refining
ready=status:ready
in-progress=status:in-progress'

# --- list.sh: provider shape -> item shape ---------------------------------
mkdir -p "$tmp/stub"
cat >"$tmp/stub/gh" <<'STUB'
#!/usr/bin/env bash
cat <<'JSON'
[
 {"number":7,"title":"Checkout hangs","body":"spinner",
  "labels":[{"name":"bug"},{"name":"priority:p0"},{"name":"status:refining"}],
  "url":"https://example.test/7","assignees":[{"login":"amir"}]},
 {"number":9,"title":"Unranked","body":"","labels":[],
  "url":"https://example.test/9","assignees":[]},
 {"number":11,"title":"Needs a decision","body":"",
  "labels":[{"name":"status:in-progress"},{"name":"blocked"}],
  "url":"https://example.test/11","assignees":[]}
]
JSON
STUB
chmod +x "$tmp/stub/gh"

echo "list.sh"
PATH="$tmp/stub:$PATH" CONVEYOR_SOURCE=midgame CONVEYOR_RESULT="$tmp/out.json" \
	./list.sh 2>/dev/null

check "maps a status label to its stage" \
	"refining" "$(jq -r '.[0].stage' "$tmp/out.json")"
check "unlabelled issue falls to DEFAULT_STAGE" \
	"backlog" "$(jq -r '.[1].stage' "$tmp/out.json")"
check "priority:p0 becomes 0" \
	"0" "$(jq -r '.[0].priority' "$tmp/out.json")"
# null, not 0 — the model keeps "unranked" and "most urgent" distinct.
check "no priority label stays null" \
	"null" "$(jq -r '.[1].priority' "$tmp/out.json")"
check "id is source-qualified" \
	"midgame:7" "$(jq -r '.[0].id' "$tmp/out.json")"
# The mark is beside the stage, never instead of it: an unblocked issue must
# resume where it stopped, which it cannot do if the stage was overwritten.
check "the blocked label becomes a mark" \
	"true" "$(jq -r '.[2].blocked' "$tmp/out.json")"
check "a marked issue keeps the stage it stopped in" \
	"in-progress" "$(jq -r '.[2].stage' "$tmp/out.json")"
check "an unmarked issue is not blocked" \
	"false" "$(jq -r '.[0].blocked' "$tmp/out.json")"

# --- move.sh: stage -> label writes ----------------------------------------
# move.sh asks GitHub for the issue's current labels, so the stub answers that.
# $LABELS is the set the fake issue is wearing for each case below.
echo "move.sh (dry run)"
cat >"$tmp/stub/gh" <<'STUB'
#!/usr/bin/env bash
for l in $LABELS; do echo "$l"; done
STUB
chmod +x "$tmp/stub/gh"

dry() { PATH="$tmp/stub:$PATH" CONVEYOR_DRY_RUN=1 ./move.sh 2>&1 | grep -oP '(?<=DRY RUN: ).*' || true; }

export LABELS="bug status:refining"
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
export LABELS="status:in-progress blocked"
check "a set mark is not set twice" \
	"" \
	"$(echo '{"item":{"ref":"9"},"stage":"in-progress","blocked":true}' | dry)"
export LABELS="status:in-progress blocked"
check "entering a stage clears the mark" \
	"gh issue edit 9 --repo owner/repo --remove-label blocked" \
	"$(echo '{"item":{"ref":"9"},"stage":"in-progress","blocked":false}' | dry)"
export LABELS="status:in-progress"
check "a reason is commented when the mark goes on" \
	"gh issue comment 9 --repo owner/repo --body <reason>" \
	"$(echo '{"item":{"ref":"9"},"stage":"in-progress","blocked":true,"blockedReason":"needs a product call"}' | dry | tail -1)"

[[ $fail -eq 0 ]] && echo "all checks passed" || echo "FAILURES"
exit $fail
