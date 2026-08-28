#!/usr/bin/env bash
# Mock stage script: emits streaming logs so the UI's live pane has something to
# show, then writes structured data to the result file. Exit code drives the move.
set -euo pipefail
payload=$(cat)
title=$(jq -r '.item.title' <<<"$payload")

echo "refining: $title"
for step in "reading item" "mapping the codebase" "drafting criteria" "verifying"; do
  echo "  $step..."; sleep 0.4
done

# Empty description is the mock's stand-in for "not enough to work with".
if [[ -z "$(jq -r '.item.description // ""' <<<"$payload")" ]]; then
  echo "no description: cannot derive acceptance criteria" >&2
  jq -n '{blocked:"needs a description before it can be refined"}' >"$AGENT_TEAM_RESULT"
  exit 20
fi

jq -n '{criteria:["AC1 observable behaviour","AC2 error path"],priority:2}' >"$AGENT_TEAM_RESULT"
echo "refined ok"
