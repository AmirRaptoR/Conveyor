# OpenCode event fixtures

`tool-run-1.18.32.jsonl` was captured from the pinned OpenCode CLI on
2026-09-24 with a real `openai/gpt-5.6-sol` run instructed to call `pwd` once
and then answer `done`. The session, part, message, snapshot, path, token and
provider metadata values were sanitized; event names, ordering and nested
shapes were retained.

The observed CLI projection emitted only the completed `tool_use`, not an
intermediate running state, and emitted complete text parts. The selfcheck also
drives a synthetic progressively growing text part so a future projection that
does repeat a part id does not lose its suffix.

`todowrite-synthetic.jsonl` is **synthetic, not captured**: no session
producing a real `todowrite` tool call was available to record from. It is
written to the same event shape `tool-run-1.18.32.jsonl` was captured in
(`tool_use` with `.part.tool`/`.part.state.status`/`.part.state.input`), with
`input.todos` items shaped `{id, content, status}` — the field names
`agents/opencode/_stream`'s mapping (`_plan_publish_opencode_todos`) reads.
Replace this file with a captured one, and update this note, the day a real
`todowrite` transcript is available.
