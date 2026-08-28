#!/usr/bin/env bash
# Mock provider write: record the new stage and the blocked mark. A real adapter
# would set a label, move a card, or update a work-item field here.
set -euo pipefail
STORE="${MOCK_STORE:-/tmp/conveyor-mock.json}"
payload=$(cat)
id=$(jq -r '.item.id'      <<<"$payload")
to=$(jq -r '.stage'        <<<"$payload")
blocked=$(jq '.blocked // false' <<<"$payload")

echo "move $id -> $to$([[ $blocked == true ]] && echo ' (blocked)')" >&2
tmp=$(mktemp)
jq --arg id "$id" --arg to "$to" --argjson blocked "$blocked" \
   'map(if .id == $id then .stage = $to | .blocked = $blocked else . end)' "$STORE" >"$tmp" && mv "$tmp" "$STORE"
