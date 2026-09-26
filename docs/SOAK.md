# Seven-Day Unattended Soak

The soak window is exactly seven consecutive 24-hour periods. Run it against an
immutable release with production retention configured at no more than seven
days; Conveyor's structured audit remains exactly seven days even when polling
runs are retained for less.

## Procedure

1. Deploy with `deploy/install-release` and retain its successful gate JSON.
2. Record the UTC start and baseline state:

   ```bash
   date -u +%FT%TZ > soak-start.txt
   curl --silent --unix-socket /var/lib/conveyor/data/api.sock http://localhost/api/state > soak-start.json
   ```

3. Do not tick, start, steer, unblock, pause, resume, cancel, reorder or grant a
   budget override for seven full days. Ordinary source updates are expected.
4. At or after 168 hours, run the gate and generate both report forms:

   ```bash
   /opt/conveyor/current/conveyor gate -c /var/lib/conveyor/conveyor.yaml \
     -expected-revision "$(jq -r .release.revision soak-start.json)"
   /opt/conveyor/current/conveyor soak-report -c /var/lib/conveyor/conveyor.yaml \
     -format json > soak-report.json
   /opt/conveyor/current/conveyor soak-report -c /var/lib/conveyor/conveyor.yaml \
     -format markdown > soak-report.md
   ```

## Pass/Fail

The generated report is the required report, not a prose checklist. `pass` is
true only when the watchdog has no open stall, no source-stale,
revision-coherence or repeated-blocker finding exists, storage is not critical,
and `humanInterventions` is zero. The report always includes the exact window,
success rate, completions per day, retries per completion, mean
blocked-to-recovered time, wasted model runs and human interventions. A failed
final gate or fewer than 168 elapsed hours is a procedure failure even if the
report itself says pass.
