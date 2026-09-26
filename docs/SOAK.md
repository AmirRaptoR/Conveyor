# Seven-Day Unattended Soak

The soak window is exactly seven consecutive 24-hour periods. Run it against an
immutable release with production retention configured at no more than seven
days; Conveyor's structured audit remains exactly seven days even when polling
runs are retained for less.

## Procedure

1. Deploy with `deploy/install-release` and retain its successful gate JSON.
   Deployment deliberately does not start or continue a soak on your behalf.
2. Explicitly reset the persisted soak identity and record its baseline:

   ```bash
   /opt/conveyor/current/conveyor soak-start \
     -c /var/lib/conveyor/conveyor.yaml > soak-start.json
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
true only when the explicitly started soak identity names the running revision,
its validated audit evidence continuity is intact, at least 168 hours have
elapsed since that start, the watchdog has no open stall, no source-stale,
revision-coherence or repeated-blocker finding exists, storage is not critical,
and `humanInterventions` is zero. A deployment never resets or blesses the
observation clock: run `soak-start` after every candidate you intend to soak,
including a same-revision redeploy. Restarting without that command preserves
the current record but cannot repair broken evidence continuity. The report
always includes the soak identity, revision, evidence health, exact window,
success rate, completions per day, retries per completion, mean
blocked-to-recovered time, wasted model runs and human interventions. A failed
final gate or fewer than 168 elapsed hours is a procedure failure even if the
report itself says pass.
