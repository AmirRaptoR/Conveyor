#!/usr/bin/env bash
# GitHub provider preflight: read-only readiness checks for one source. Never
# writes anything — no label is created here; a missing one is reported with
# a `fix:` naming the onboard.sh invocation that would create it, run by a
# person. See docs/CONTRACTS.md §3.
#
# Env (from the source's env: + provider.params:, same as list.sh):
#   REPO           owner/name — required
#   LABEL_PREFIX   default "conveyor:"
#   STAGE_LABELS   "stage=label" per line — required; without it nothing can
#                  be moved, so its absence is a fail rather than a skip
#   BLOCKED_LABEL  default "${LABEL_PREFIX}blocked"
#
# $CONVEYOR_WORKDIR is checked for a remote resolving to $REPO.
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
source ./_stage_labels.sh

checks='[]'
add_check() { # add_check <name> <status> <detail> [<fix>]
	local name=$1 status=$2 detail=$3 fix=${4:-}
	checks=$(jq -c --arg n "$name" --arg s "$status" --arg d "$detail" --arg f "$fix" \
		'. + [{name: $n, status: $s, detail: $d} + (if $f != "" then {fix: $f} else {} end)]' <<<"$checks")
}

finish() {
	jq -n --argjson checks "$checks" '{checks: $checks}' >"$CONVEYOR_RESULT"
	exit 0
}

LABEL_PREFIX="${LABEL_PREFIX:-conveyor:}"
BLOCKED_LABEL="${BLOCKED_LABEL:-${LABEL_PREFIX}blocked}"
ONBOARD_LABEL="${LABEL_PREFIX%[^[:alnum:]]}"

if [[ -z "${REPO:-}" ]]; then
	add_check "REPO configured" fail "REPO is required (set it in the source env: block)"
	finish
fi

# --- gh on PATH --------------------------------------------------------
if ! command -v gh >/dev/null 2>&1; then
	add_check "gh on PATH" fail "gh is not installed or not on PATH"
	finish
fi
add_check "gh on PATH" pass "$(command -v gh)"

# gh_retry <label> <output-var> -- <gh args...> — a read-only gh call,
# retried on transient failure the same way list.sh's gh_issues is: a 503 or
# a network blip must not read as a missing permission or a missing label.
gh_retry() {
	local label=$1 outvar=$2 attempt=1 out err
	shift 2
	[[ "$1" == "--" ]] && shift
	while :; do
		if out=$(gh "$@" 2>/tmp/gh-preflight-err.$$); then
			printf -v "$outvar" '%s' "$out"
			rm -f /tmp/gh-preflight-err.$$
			return 0
		fi
		err=$(cat /tmp/gh-preflight-err.$$ 2>/dev/null || true)
		rm -f /tmp/gh-preflight-err.$$
		if ((attempt >= ${GH_ATTEMPTS:-3})); then
			printf -v "$outvar" '%s' "$err"
			return 1
		fi
		sleep $((attempt * 2))
		attempt=$((attempt + 1))
	done
}

# --- gh auth status -----------------------------------------------------
if ! gh_retry "auth" auth_err -- auth status; then
	add_check "gh auth status" fail "not authenticated: $auth_err"
	finish
fi
add_check "gh auth status" pass ""

# --- repository visible + permission ------------------------------------
if ! gh_retry "repo view" repo_json -- repo view "$REPO" --json viewerPermission,nameWithOwner; then
	add_check "$REPO visible" fail "$repo_json"
	# Permission cannot be answered without the repo, and labels cannot be
	# listed either — reported once, not repeated per downstream check.
	add_check "repository permission" skip "not attempted: $REPO is not visible"
	add_check "labels" skip "not attempted: $REPO is not visible"
else
	add_check "$REPO visible" pass "$(jq -r '.nameWithOwner' <<<"$repo_json")"

	perm=$(jq -r '.viewerPermission // ""' <<<"$repo_json")
	case "$perm" in
	ADMIN | MAINTAIN | WRITE)
		add_check "repository permission" pass "$perm"
		;;
	TRIAGE | READ)
		add_check "repository permission" fail "$perm — labels and issue edits need write"
		;;
	"")
		add_check "repository permission" unknown "gh did not report a viewerPermission"
		;;
	*)
		add_check "repository permission" unknown "unrecognised permission $perm"
		;;
	esac

	# --- labels --------------------------------------------------------
	if [[ -z "${STAGE_LABELS:-}" ]]; then
		add_check "STAGE_LABELS configured" fail "STAGE_LABELS is required; without it nothing can be moved"
	else
		add_check "STAGE_LABELS configured" pass ""
	fi

	mapfile -t needed < <(
		{
			stage_labels_pairs <<<"${STAGE_LABELS:-}" | cut -f2
			printf '%s\n' "$BLOCKED_LABEL" "$ONBOARD_LABEL"
		} | awk '!seen[$0]++'
	)

	if gh_retry "labels" labels_json -- label list --repo "$REPO" --json name --limit 200; then
		mapfile -t missing < <(
			for n in "${needed[@]}"; do
				jq -e --arg n "$n" 'any(.[]; .name == $n)' <<<"$labels_json" >/dev/null || printf '%s\n' "$n"
			done
		)
		if [[ ${#missing[@]} -eq 0 ]]; then
			add_check "labels" pass "all ${#needed[@]} label(s) exist"
		else
			fix="providers/github/onboard.sh"
			fix_env="REPO=$REPO STAGE_LABELS=\$'$(stage_labels_pairs <<<"${STAGE_LABELS:-}" | awk -F'\t' '{printf "%s=%s\\n", $1, $2}')'"
			[[ "$LABEL_PREFIX" != "conveyor:" ]] && fix_env="LABEL_PREFIX=$LABEL_PREFIX $fix_env"
			[[ "$BLOCKED_LABEL" != "${LABEL_PREFIX}blocked" ]] && fix_env="BLOCKED_LABEL=$BLOCKED_LABEL $fix_env"
			add_check "labels" fail "missing: $(
				IFS=', '
				echo "${missing[*]}"
			)" "$fix_env $fix"
		fi
	else
		add_check "labels" fail "could not list labels: $labels_json"
	fi
fi

# --- workdir remote resolves to $REPO ------------------------------------
workdir="${CONVEYOR_WORKDIR:-}"
if [[ -z "$workdir" || ! -d "$workdir" ]]; then
	add_check "workdir remote" fail "CONVEYOR_WORKDIR is not a directory"
else
	remotes=$(git -C "$workdir" remote 2>/dev/null || true)
	remote_name=""
	if grep -qx "origin" <<<"$remotes"; then
		remote_name="origin"
	elif [[ "$(wc -l <<<"$remotes")" == "1" && -n "$remotes" ]]; then
		remote_name="$remotes"
	fi
	if [[ -z "$remote_name" ]]; then
		add_check "workdir remote" fail "no 'origin' remote and not exactly one remote in $workdir (remotes: $(tr '\n' ',' <<<"$remotes"))"
	else
		url=$(git -C "$workdir" remote get-url "$remote_name" 2>/dev/null || true)
		# Accept HTTPS and SSH forms, an optional .git suffix (stripped
		# before matching — POSIX ERE, which bash's [[ =~ ]] uses, has no
		# non-greedy quantifier to make an inline (\.git)? behave), and
		# compare owner/name case-insensitively:
		#   https://github.com/OWNER/NAME(.git)?
		#   git@github.com:OWNER/NAME(.git)?
		#   ssh://git@github.com/OWNER/NAME(.git)?
		bare="${url%/}"
		bare="${bare%.git}"
		host=""
		ownerName=""
		if [[ "$bare" =~ ^https?://([^/]+)/([^/]+)/([^/]+)$ ]]; then
			host="${BASH_REMATCH[1]}"
			ownerName="${BASH_REMATCH[2]}/${BASH_REMATCH[3]}"
		elif [[ "$bare" =~ ^git@([^:]+):([^/]+)/([^/]+)$ ]]; then
			host="${BASH_REMATCH[1]}"
			ownerName="${BASH_REMATCH[2]}/${BASH_REMATCH[3]}"
		elif [[ "$bare" =~ ^ssh://git@([^/]+)/([^/]+)/([^/]+)$ ]]; then
			host="${BASH_REMATCH[1]}"
			ownerName="${BASH_REMATCH[2]}/${BASH_REMATCH[3]}"
		fi
		if [[ -z "$ownerName" ]]; then
			add_check "workdir remote" fail "remote '$remote_name' ($url) does not look like a GitHub remote"
		elif [[ "${ownerName,,}" != "${REPO,,}" ]]; then
			add_check "workdir remote" fail "remote '$remote_name' names ${ownerName}, not $REPO"
		elif [[ -n "$host" && "$host" != "github.com" ]]; then
			add_check "workdir remote" warn "on host $host, not github.com"
		else
			add_check "workdir remote" pass "$remote_name -> $ownerName"
		fi
	fi
fi

finish
