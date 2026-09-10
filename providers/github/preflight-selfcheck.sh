#!/usr/bin/env bash
# Self-check for providers/github/preflight.sh, against a stubbed `gh` and a
# real (local, empty) git repository — no network. Named preflight-selfcheck
# so ./check discovers it (*-selfcheck.sh) and so it cannot be mistaken for
# the script under test.
#
#   ./providers/github/preflight-selfcheck.sh
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

STAGE_LABELS_FULL='refining=status:refining
ready=status:ready'

# repo — a bare local git checkout with one origin remote pointed at $1.
repo() {
	local dir=$1 url=$2
	rm -rf "$dir"
	mkdir -p "$dir"
	git -C "$dir" init -q
	git -C "$dir" remote add origin "$url"
}

# run <stub-gh-body> — writes the stub, runs preflight.sh against it and the
# standard repo/labels fixture, and leaves $tmp/result.json for the caller.
run() {
	local stub=$1
	mkdir -p "$tmp/bin"
	printf '%s\n' "$stub" >"$tmp/bin/gh"
	chmod +x "$tmp/bin/gh"
	(
		export REPO="owner/repo"
		export STAGE_LABELS="$STAGE_LABELS_FULL"
		export CONVEYOR_WORKDIR="$tmp/repo"
		export CONVEYOR_RESULT="$tmp/result.json"
		echo '{}' | PATH="$tmp/bin:$PATH" ./preflight.sh
	)
}

status_of() { jq -r --arg n "$1" '.checks[] | select(.name == $n) | .status' "$tmp/result.json"; }
detail_of() { jq -r --arg n "$1" '.checks[] | select(.name == $n) | .detail' "$tmp/result.json"; }

# Build the happy-path stub without fighting quoting: write it with heredocs.
happy_gh_script() {
	local perm=$1 labels=$2
	cat <<EOF
#!/usr/bin/env bash
case "\$*" in
*"auth status"*) exit 0 ;;
*"repo view "*) echo '{"viewerPermission":"$perm","nameWithOwner":"owner/repo"}' ;;
*"label list"*) echo '$labels' ;;
*) echo "unhandled: \$*" >&2; exit 1 ;;
esac
EOF
}

ALL_LABELS='[{"name":"status:refining"},{"name":"status:ready"},{"name":"conveyor:blocked"},{"name":"conveyor"}]'
MISSING_ONE='[{"name":"status:refining"},{"name":"conveyor:blocked"},{"name":"conveyor"}]'

# --- everything present ------------------------------------------------
repo "$tmp/repo" "https://github.com/owner/repo.git"
run "$(happy_gh_script WRITE "$ALL_LABELS")"
rc=$?
check "exits 0 when everything is fine" "0" "$rc"
check "gh on PATH" pass "$(status_of "gh on PATH")"
check "gh auth status" pass "$(status_of "gh auth status")"
check "owner/repo visible" pass "$(status_of "owner/repo visible")"
check "repository permission" pass "$(status_of "repository permission")"
check "labels" pass "$(status_of "labels")"
check "workdir remote" pass "$(status_of "workdir remote")"

# --- gh absent -------------------------------------------------------------
# A PATH built from symlinks to every tool preflight.sh actually needs,
# except gh — excluding gh's whole directory would take jq, git and the rest
# of coreutils with it, since gh commonly lives alongside them in /usr/bin.
no_gh_dir="$tmp/no-gh-bin"
mkdir -p "$no_gh_dir"
for tool in bash jq git mktemp cat awk cut tr wc grep sed dirname basename sleep chmod rm mkdir env; do
	if p=$(command -v "$tool" 2>/dev/null); then
		ln -sf "$p" "$no_gh_dir/$tool"
	fi
done
no_gh_path="$no_gh_dir"
(
	export REPO="owner/repo"
	export STAGE_LABELS="$STAGE_LABELS_FULL"
	export CONVEYOR_WORKDIR="$tmp/repo"
	export CONVEYOR_RESULT="$tmp/result.json"
	echo '{}' | PATH="$no_gh_path" ./preflight.sh
)
check "gh absent -> fail" fail "$(status_of "gh on PATH")"

# --- unauthenticated -------------------------------------------------------
run '#!/usr/bin/env bash
case "$*" in
*"auth status"*) echo "not logged in" >&2; exit 1 ;;
*) exit 1 ;;
esac'
check "unauthenticated -> fail" fail "$(status_of "gh auth status")"

# --- repository invisible ---------------------------------------------
run '#!/usr/bin/env bash
case "$*" in
*"auth status"*) exit 0 ;;
*"repo view "*) echo "HTTP 404: Not Found" >&2; exit 1 ;;
*) exit 1 ;;
esac'
check "repo invisible -> fail" fail "$(status_of "owner/repo visible")"
check "permission skipped when repo invisible" skip "$(status_of "repository permission")"
check "labels skipped when repo invisible" skip "$(status_of "labels")"

# --- insufficient permission --------------------------------------------
run "$(happy_gh_script READ "$ALL_LABELS")"
check "READ permission -> fail" fail "$(status_of "repository permission")"

run "$(happy_gh_script TRIAGE "$ALL_LABELS")"
check "TRIAGE permission -> fail" fail "$(status_of "repository permission")"

run "$(happy_gh_script SOMETHING-NEW "$ALL_LABELS")"
check "unrecognised permission -> unknown" unknown "$(status_of "repository permission")"

# --- one stage label missing --------------------------------------------
run "$(happy_gh_script WRITE "$MISSING_ONE")"
check "missing label -> fail" fail "$(status_of "labels")"
check "missing label detail names it" "missing: status:ready" "$(detail_of "labels")"
fix=$(jq -r '.checks[] | select(.name == "labels") | .fix' "$tmp/result.json")
check "fix names onboard.sh" "true" "$([[ "$fix" == *"onboard.sh"* ]] && echo true || echo false)"
check "fix carries REPO" "true" "$([[ "$fix" == *"REPO=owner/repo"* ]] && echo true || echo false)"

# --- a remote naming a different repository --------------------------------
repo "$tmp/repo" "https://github.com/someone-else/other.git"
run "$(happy_gh_script WRITE "$ALL_LABELS")"
check "wrong remote -> fail" fail "$(status_of "workdir remote")"

# The three accepted forms, an optional .git, case-insensitive owner/name —
# all resolve to the *same* repository as $REPO.
for url in \
	"https://github.com/Owner/Repo" \
	"https://github.com/owner/repo.git" \
	"git@github.com:owner/repo.git" \
	"ssh://git@github.com/owner/repo"; do
	repo "$tmp/repo" "$url"
	run "$(happy_gh_script WRITE "$ALL_LABELS")"
	check "remote form $url resolves" pass "$(status_of "workdir remote")"
done

# A host other than github.com is a warn, not a fail.
repo "$tmp/repo" "https://github.example.com/owner/repo"
run "$(happy_gh_script WRITE "$ALL_LABELS")"
check "non-github.com host -> warn" warn "$(status_of "workdir remote")"

# --- a gh call that fails transiently and succeeds on retry ---------------
repo "$tmp/repo" "https://github.com/owner/repo.git"
attempts_file="$tmp/attempts"
echo 0 >"$attempts_file"
cat >"$tmp/bin/gh" <<EOF
#!/usr/bin/env bash
case "\$*" in
*"auth status"*) exit 0 ;;
*"repo view "*)
	n=\$(cat "$attempts_file")
	n=\$((n + 1))
	echo "\$n" >"$attempts_file"
	if [[ "\$n" -lt 2 ]]; then
		echo "HTTP 503: Service Unavailable" >&2
		exit 1
	fi
	echo '{"viewerPermission":"WRITE","nameWithOwner":"owner/repo"}'
	;;
*"label list"*) echo '$ALL_LABELS' ;;
*) exit 1 ;;
esac
EOF
chmod +x "$tmp/bin/gh"
(
	export REPO="owner/repo"
	export STAGE_LABELS="$STAGE_LABELS_FULL"
	export CONVEYOR_WORKDIR="$tmp/repo"
	export CONVEYOR_RESULT="$tmp/result.json"
	export GH_ATTEMPTS=3
	echo '{}' | PATH="$tmp/bin:$PATH" ./preflight.sh
)
check "transient failure retried, not fatal" pass "$(status_of "owner/repo visible")"
check "retried at least twice" "true" "$([[ "$(cat "$attempts_file")" -ge 2 ]] && echo true || echo false)"

# --- read-only: no gh subcommand outside an allowlist ---------------------
cat >"$tmp/bin/gh" <<'EOF'
#!/usr/bin/env bash
case "$1" in
auth|repo|label) ;;
*) echo "preflight.sh called a non-read-only gh subcommand: $*" >&2; exit 99 ;;
esac
case "$*" in
*"auth status"*) exit 0 ;;
*"repo view "*) echo '{"viewerPermission":"WRITE","nameWithOwner":"owner/repo"}' ;;
*"label list"*) echo '[{"name":"status:refining"},{"name":"status:ready"},{"name":"conveyor:blocked"},{"name":"conveyor"}]' ;;
*"label create"*) echo "preflight.sh tried to create a label: $*" >&2; exit 99 ;;
*"label edit"*) echo "preflight.sh tried to edit a label: $*" >&2; exit 99 ;;
*) exit 0 ;;
esac
EOF
chmod +x "$tmp/bin/gh"
(
	export REPO="owner/repo"
	export STAGE_LABELS="$STAGE_LABELS_FULL"
	export CONVEYOR_WORKDIR="$tmp/repo"
	export CONVEYOR_RESULT="$tmp/result.json"
	echo '{}' | PATH="$tmp/bin:$PATH" ./preflight.sh
)
check "no gh subcommand outside the read-only allowlist" "0" "$?"

# --- list.sh, onboard.sh and preflight.sh agree on STAGE_LABELS ------------
# One awkward input — a comment, a blank line, an empty right-hand side,
# trailing whitespace, and a label containing "=" — fed through the one
# shared parse (_stage_labels.sh) both list.sh (stage_labels_json) and
# onboard.sh/preflight.sh (stage_labels_pairs) call. Proves the three scripts
# cannot drift apart, rather than merely asserting they happen to agree today.
source ./_stage_labels.sh
awkward='# a comment on its own line
refining=status:refining   # trailing comment
   ready = status:ready
empty=
weird=a=b

'
pairs_got=$(stage_labels_pairs <<<"$awkward" | sort)
pairs_want=$(printf 'refining\tstatus:refining\nready\tstatus:ready\nweird\ta=b\n' | sort)
check "stage_labels_pairs on an awkward input" "$pairs_want" "$pairs_got"

json_got=$(stage_labels_json <<<"$awkward" | jq -Sc .)
json_want=$(jq -Sc -n '{"status:refining":"refining","status:ready":"ready","a=b":"weird"}')
check "stage_labels_json agrees with stage_labels_pairs" "$json_want" "$json_got"

if [[ "$fail" -ne 0 ]]; then
	echo "preflight-selfcheck: FAILED"
	exit 1
fi
echo "all checks passed"
