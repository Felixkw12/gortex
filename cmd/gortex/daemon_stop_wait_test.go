//go:build darwin || linux || windows

package main

import (
	"bytes"
	"errors"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/platform"
)

func TestDaemonStopFreeStoreAndGoneRuntimeNeverKillsLiveFinalizer(t *testing.T) {
	target, exit, wait, unlock := ownedStopTarget(t)
	require.NoError(t, unlock()) // Physical helper lock released, child still live.
	for _, file := range target.files {
		require.NoError(t, os.Remove(file.path))
	}
	previous := daemonKillCapturedProcess
	var kills atomic.Int32
	daemonKillCapturedProcess = func(platform.ProcessIdentity) error { kills.Add(1); return errors.New("unexpected kill") }
	t.Cleanup(func() { daemonKillCapturedProcess = previous })
	var out bytes.Buffer
	started := time.Now()
	result, err := waitForDaemonTarget(&out, target, 0, false)
	require.NoError(t, err)
	require.False(t, result.forced)
	require.Less(t, time.Since(started), 500*time.Millisecond, "free store/runtime should complete in its first poll")
	require.Zero(t, kills.Load())
	require.Contains(t, out.String(), "still exiting")
	require.NotContains(t, out.String(), "reaped", "live helper is not a proven zombie")
	require.NotContains(t, out.String(), "timed out")
	running, err := target.process.Running()
	require.NoError(t, err)
	require.True(t, running, "completion must not terminate the actual live finalizer fixture")
	require.NoError(t, exit())
	require.NoError(t, wait())
}

func TestDaemonStopForceActuallyReleasesAndReapsOnlyOwnedChild(t *testing.T) {
	unrelated, exitUnrelated, waitUnrelated, _ := ownedStopTarget(t)
	target, _, wait, _ := ownedStopTarget(t)
	var out bytes.Buffer
	result, err := waitForDaemonTarget(&out, target, 0, false)
	require.NoError(t, err)
	require.True(t, result.forced)
	require.Error(t, wait(), "actual force signal must terminate this exact helper")
	free, err := target.storeFree()
	require.NoError(t, err)
	require.True(t, free)
	gone, err := target.runtimeGone()
	require.NoError(t, err)
	require.True(t, gone)
	running, err := unrelated.process.Running()
	require.NoError(t, err)
	require.True(t, running)
	require.NoError(t, exitUnrelated())
	require.NoError(t, waitUnrelated())
}

func TestDaemonStopFailedKillRetainsErrorAndActualRuntime(t *testing.T) {
	target, exit, wait, _ := ownedStopTarget(t)
	failure := errors.New("injected captured kill failure")
	previous := daemonKillCapturedProcess
	daemonKillCapturedProcess = func(platform.ProcessIdentity) error { return failure }
	t.Cleanup(func() { daemonKillCapturedProcess = previous })
	_, err := waitForDaemonTarget(&bytes.Buffer{}, target, 0, false)
	require.ErrorIs(t, err, failure)
	for _, file := range target.files {
		present, err := file.present()
		require.NoError(t, err)
		require.True(t, present)
	}
	running, err := target.process.Running()
	require.NoError(t, err)
	require.True(t, running)
	require.NoError(t, exit())
	require.NoError(t, wait())
}

func TestDaemonStopSuccessfulSignalWithoutExitIsNotCompletion(t *testing.T) {
	target, exit, wait, _ := ownedStopTarget(t)
	previous := daemonKillCapturedProcess
	daemonKillCapturedProcess = func(platform.ProcessIdentity) error { return nil } // Deliberately no OS signal.
	t.Cleanup(func() { daemonKillCapturedProcess = previous })
	result, err := waitForDaemonTarget(&bytes.Buffer{}, target, 0, false)
	require.ErrorContains(t, err, "did not release its store and runtime")
	require.False(t, result.forced, "a fake signal receipt cannot certify a successful force-stop")
	running, stateErr := target.process.Running()
	require.NoError(t, stateErr)
	require.True(t, running)
	for _, file := range target.files {
		present, err := file.present()
		require.NoError(t, err)
		require.True(t, present)
	}
	require.NoError(t, exit())
	require.NoError(t, wait())
}

func TestDaemonStopUnknownProcessStateCannotSignalOrCleanRuntime(t *testing.T) {
	target, exit, wait, _ := ownedStopTarget(t)
	previousRunning, previousKill := daemonCapturedProcessRunning, daemonKillCapturedProcess
	var kills atomic.Int32
	daemonCapturedProcessRunning = func(platform.ProcessIdentity) (bool, error) { return false, platform.ErrProcessIdentityChanged }
	daemonKillCapturedProcess = func(platform.ProcessIdentity) error { kills.Add(1); return nil }
	t.Cleanup(func() { daemonCapturedProcessRunning, daemonKillCapturedProcess = previousRunning, previousKill })
	_, err := waitForDaemonTarget(&bytes.Buffer{}, target, 0, false)
	require.ErrorIs(t, err, platform.ErrProcessIdentityChanged)
	require.Zero(t, kills.Load())
	for _, file := range target.files {
		present, err := file.present()
		require.NoError(t, err)
		require.True(t, present)
	}
	require.NoError(t, exit())
	require.NoError(t, wait())
}

func TestDaemonStopChangedRuntimeCannotSignalCapturedProcess(t *testing.T) {
	target, exit, wait, _ := ownedStopTarget(t)
	require.NoError(t, os.WriteFile(daemon.PIDFilePath(), []byte(strconv.Itoa(target.process.PID()+1)), 0o600))
	previous := daemonKillCapturedProcess
	var kills atomic.Int32
	daemonKillCapturedProcess = func(platform.ProcessIdentity) error { kills.Add(1); return nil }
	t.Cleanup(func() { daemonKillCapturedProcess = previous })
	_, err := waitForDaemonTarget(&bytes.Buffer{}, target, 0, false)
	require.ErrorContains(t, err, "changed runtime ownership")
	require.Zero(t, kills.Load())
	running, stateErr := target.process.Running()
	require.NoError(t, stateErr)
	require.True(t, running)
	require.NoError(t, exit())
	require.NoError(t, wait())
}

func TestDaemonStopCLIFallbackJoinsActualOwnedProcessAndStoreLock(t *testing.T) {
	target, _, wait, _ := ownedStopTarget(t)
	var out bytes.Buffer
	// The fixture endpoint is a regular owned file, not a listener: this
	// deliberately exercises the CLI's identity-checked TERM fallback.
	require.NoError(t, stopRunningDaemon(&out))
	require.Error(t, wait(), "the actual helper must have received its own termination signal")
	free, err := target.storeFree()
	require.NoError(t, err)
	require.True(t, free)
	gone, err := target.runtimeGone()
	require.NoError(t, err)
	require.True(t, gone)
	require.Contains(t, out.String(), "stopped")
}
