#!/usr/bin/env bash
# Start the Ship Reports dashboard. Picks a free port and prints the URL.
exec node "$(dirname "$0")/server.js" "$@"
