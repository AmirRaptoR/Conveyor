#!/usr/bin/env bash
# Mock provider write: record the new stage. A real adapter would set a label,
# move a card, or update a work-item field here.
set -euo pipefail
STORE="${MOCK_STORE:-/tmp/agent-team-mock.json}"
payload=$(cat)
id=$(jq -r '.item.id' <<<"$payload")
to=$(jq -r '.stage'   <<<"$payload")

echo "move $id -> $to" >&2
tmp=$(mktemp)
jq --arg id "$id" --arg to "$to" \
   'map(if .id == $id then .stage = $to else . end)' "$STORE" >"$tmp" && mv "$tmp" "$STORE"
