#!/usr/bin/env bash
# Canonical parsing of STAGE_LABELS, sourced by list.sh, onboard.sh and
# preflight.sh so the three scripts can never disagree about what one
# STAGE_LABELS string means.
#
# Not a verb: config resolves list, move and preflight by name under a
# provider directory, and a leading underscore keeps this from ever being
# mistaken for one of them — the same care onboard.sh and selfcheck.sh
# already document for their own non-verb names.
#
# STAGE_LABELS is "stage=label" lines. A line is: comments (# to end of
# line) stripped, then leading/trailing whitespace trimmed; blank after that,
# or carrying no "=", or carrying an empty label (right-hand side) is
# skipped. A label may itself contain "=" — only the first one splits the
# line.

_trim() {
	local s=$1
	s="${s#"${s%%[![:space:]]*}"}"
	s="${s%"${s##*[![:space:]]}"}"
	printf '%s' "$s"
}

# stage_labels_pairs — read STAGE_LABELS on stdin, print "stage<TAB>label"
# per line, canonically parsed.
stage_labels_pairs() {
	local line stage name
	while IFS= read -r line; do
		line="${line%%#*}"
		line=$(_trim "$line")
		[[ -z "$line" || "$line" != *=* ]] && continue
		stage=$(_trim "${line%%=*}")
		name=$(_trim "${line#*=}")
		[[ -z "$name" ]] && continue
		printf '%s\t%s\n' "$stage" "$name"
	done
}

# stage_labels_json — read STAGE_LABELS on stdin, print {"<label>":"<stage>"}
# JSON: the label->stage direction list.sh's listing needs.
stage_labels_json() {
	stage_labels_pairs | jq -R -s '
		split("\n") | map(select(length > 0))
		| map(split("\t") | {key: .[1], value: .[0]})
		| from_entries'
}
