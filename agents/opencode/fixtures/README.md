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
