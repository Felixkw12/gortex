# Release-candidate checks

For every RC tag, run both native acceptance flows before claiming native
Windows worktree edits are supported. These are manual, explicit private-daemon
tests; a green ordinary Windows build or untagged CI run is not this gate.

```sh
RC_TAG=v0.64.12-rc.1 # use the actual candidate tag
gh workflow run bench-arm.yml --ref "$RC_TAG" -f suite=acceptance-native-windows
gh workflow run bench-arm.yml --ref "$RC_TAG" -f suite=acceptance-native
```

The Windows job builds the tagged native binary with the safe C/C++ flags and
runs `TestIssue767WorktreeReadinessIntegration` ten times. Each repetition runs
the same-drive case and attempts the cross-drive case using a fresh writable
directory on another fixed local drive. If none is available, the output must
contain the cross-drive skip and its reason; never report that variant as run.
The existing Ubuntu flow runs the extended test alongside its other drivers.

Review the always-uploaded `acceptance-native-windows-*` evidence, including
build metadata/hash, test output, private daemon/shutdown logs, edit responses
and before/preview/after source snapshots. Confirm:

- 10/10 same-drive repetitions stopped and restarted without a forced kill.
- Every after-restart dry run and real edit was admitted within 3 seconds of
  starting `gortex call edit`, without `view_read_only` or lane-busy refusals.
- Dry runs left bytes unchanged; the second real marker was on disk and
  searchable through the exact worktree view, absent from the primary.
- Cross-drive results or a reasoned skip are visible, and Ubuntu acceptance
  passed. Mutation failures were retained without automatic retry.

Post both workflow run links and an explicit outcome on [#1036](https://github.com/zzet/gortex/issues/1036)
for that RC: **did not reproduce**, **reproduced**, or **incomplete**. Build,
setup or censored failures are incomplete evidence, not a successful support
gate; keep their artifacts. Branch runs help validate the harness but do not
replace the run pinned to the actual RC tag.

If the smoke reproduces [#777](https://github.com/zzet/gortex/issues/777), the
milestone's release notes must say **"Native Windows edits are unsupported in
this release."** Open and link the Windows path-identity hardening follow-up,
assign it to the next patch milestone, and record the trigger on #1036. Do not
silently retry edits, relax the admission deadline, or label WSL2 results as
native Windows evidence.
