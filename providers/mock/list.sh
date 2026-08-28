#!/usr/bin/env bash
# Mock source: five items in a file, so the engine can be exercised with no
# provider at all. State lives in $STORE; move.sh writes it, this reads it.
set -euo pipefail
STORE="${MOCK_STORE:-/tmp/conveyor-mock.json}"

if [[ ! -f "$STORE" ]]; then
  cat > "$STORE" <<'SEED'
[
 {"id":"mock:1","ref":"1","source":"mock","stage":"backlog","priority":0,
  "title":"Checkout hangs on payment step",
  "description":"Users report the spinner never resolves after card entry.",
  "labels":["bug"],"url":"https://example.test/1"},
 {"id":"mock:2","ref":"2","source":"mock","stage":"backlog","priority":2,
  "title":"Add CSV export to the reports page",
  "description":"Finance want the monthly table as a download.",
  "labels":["feature"],"url":"https://example.test/2"},
 {"id":"mock:3","ref":"3","source":"mock","stage":"ready","priority":1,
  "title":"Session expires without warning",
  "description":"Show a countdown and offer to extend.",
  "labels":["bug"],"url":"https://example.test/3"},
 {"id":"mock:4","ref":"4","source":"mock","stage":"done","priority":3,
  "title":"Bump dependencies","description":"Routine upgrade.",
  "labels":["chore"],"url":"https://example.test/4"},
 {"id":"mock:5","ref":"5","source":"mock","stage":"backlog","priority":null,
  "title":"Investigate slow cold start","description":"",
  "labels":[],"url":"https://example.test/5"}
]
SEED
fi

echo "listing mock items from $STORE" >&2
cp "$STORE" "$CONVEYOR_RESULT"
