# SQLite shutdown checkpoint policy

Closing the store skips its final WAL `TRUNCATE` checkpoint by default. Committed WAL frames remain available for recovery on the next open. Every graph-store connection disables SQLite's implicit last-connection checkpoint, so closing the connection cannot secretly repeat that copy.

The close still joins its store owners, seals deferred bulk indexes, restores and releases the pinned bulk connection, and closes both pools. This policy does not change normal passive checkpoints, WAL reclaim, an explicit `CheckpointWAL`, or explicit bulk-finalization operations. It also does not change SQLite's existing `NORMAL`/bulk `OFF` synchronous settings or promise protection beyond those settings against power loss.

Set these values in the **daemon's environment when starting it**. Setting them only on a later `gortex daemon stop` command cannot change the running daemon's policy.

| Environment | Final checkpoint |
| --- | --- |
| Neither setting present | Skip |
| `GORTEX_SQLITE_CLOSE_CHECKPOINT=full` | Full explicit drain |
| `GORTEX_SQLITE_CLOSE_CHECKPOINT=skip` | Skip, including when a legacy cap is present |
| Only `GORTEX_SQLITE_CLOSE_CHECKPOINT_MAX_S=N`, `N > 0` | Full drain with the existing cap of `min(N, estimate)` |
| Only `GORTEX_SQLITE_CLOSE_CHECKPOINT_MAX_S=0` | Full drain without an imposed deadline, preserving this explicit legacy setting |

Positive legacy caps retain their 24-hour maximum. A `full` policy also honors a valid legacy cap. Negative or malformed legacy values produce a warning and do not enable a drain. An invalid policy produces a warning and falls back to a valid legacy opt-in, or to skip.

For example, an operator who wants the historical full drain can start the daemon with:

```sh
GORTEX_SQLITE_CLOSE_CHECKPOINT=full gortex daemon start --detach
```

The policy is captured once for each owning close. The storage status includes `close_checkpoint_policy` (`skip` or `full`); absence identifies an older or unknown backend. A skipped checkpoint has a zero close-checkpoint estimate while WAL byte and pending-frame telemetry still describe the real backlog. Skip does not create a completed checkpoint-progress report or teach a synthetic drain rate.

A positive deadline can interrupt SQLite admission or SQL work; it cannot guarantee interruption of a filesystem sync already in progress. These settings alone do not establish an overall daemon-stop latency bound. Native stop, immediate-reopen, and warm graph-call acceptance remain separate measurements.
