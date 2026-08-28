#!/usr/bin/env bash
# Mock delivery. A real one would be:
#   claude -p "/deliver-issue --once $CONVEYOR_ITEM_REF" --max-turns 200
set -euo pipefail
payload=$(cat)
echo "implementing $(jq -r '.item.id' <<<"$payload")"
for step in "branch" "red test" "implement" "suite" "review" "merge"; do
  echo "  $step"; sleep 0.4
done
jq -n '{pr:123,merged:true}' >"$CONVEYOR_RESULT"
echo "done"
