# Ship Reports

A local dashboard over `~/codes/reports` — what each project has shipped, what is
live where, and what the `ship-issues` loop is doing right now.

## Running it

Docker Compose is how it stays up — `restart: unless-stopped` means the daemon
brings it back after a crash or a reboot:

```bash
docker compose up -d --build     # → http://127.0.0.1:7788
docker compose logs -f
docker compose down
```

The compose file wires one folder: `~/codes/reports`, mounted read-only at
`/reports`. Point it somewhere else with `SHIP_REPORTS_HOST_ROOT=/other/reports
docker compose up -d`. The port is published to `127.0.0.1` only, so exposure is
the same as running it bare.

Bare metal still works and needs nothing installed:

```bash
node server.js            # → http://127.0.0.1:7788
node server.js --port 9000 --open
node server.js --root /some/other/reports
node server.js --host 0.0.0.0   # or SHIP_REPORTS_HOST
```

Zero dependencies, read-only, binds to loopback, no auth. Port 7788 is the first
choice; it falls back to 7799, 8123, 8899, then any free port, and prints the URL
it took. In the container the port is pinned to 7788 so the published mapping is
predictable.

To expose it (single-user, no auth — anyone with the URL sees everything):

```bash
cloudflared tunnel --url http://localhost:7788
```

## What it shows

Per project: the live cycle (state, issue, elapsed time, agreed acceptance
criteria, step progress), what is deployed to each environment, shipped issues
with before/after and screenshots, the release log, and recent activity.

## How it stays live

`fs.watch` on the reports tree, debounced, pushed to the browser over SSE
(`/events`). Falls back to a 3-second signature poll where recursive watching is
unavailable. The client re-renders on each push and keeps expanded reports open.

## Endpoints

| Path | Purpose |
| --- | --- |
| `/` | the dashboard |
| `/api/state` | full aggregated state as JSON |
| `/api/issue/<project>/<number>` | one full report, for older issues trimmed out of `/api/state` |
| `/files/<project>/media/...` | screenshots and evidence |
| `/events` | SSE stream of state updates |

Writes belong to `reports.py` alone; this server never modifies the reports tree.
The data contract is `~/codes/reports/SCHEMA.md`.
