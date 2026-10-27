//go:build darwin || linux || windows

package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/daemon"
)

func TestDaemonStopTargetChild(t *testing.T) {
	backend := os.Getenv("GORTEX_TEST_STOP_TARGET_BACKEND")
	if backend == "" {
		return
	}
	lock := flock.New(backend + ".lock")
	held, err := lock.TryLock()
	if err != nil || !held {
		fmt.Fprintln(os.Stderr, "cannot acquire owned helper store lock", err)
		os.Exit(2)
	}
	if err := os.WriteFile(daemon.PIDFilePath(), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		os.Exit(3)
	}
	if err := daemon.WriteRuntimeState(daemon.RuntimeState{BackendPath: backend}); err != nil {
		os.Exit(4)
	}
	if err := os.WriteFile(daemon.SocketPath(), []byte("owned helper runtime endpoint"), 0o600); err != nil {
		os.Exit(5)
	}
	fmt.Fprintln(os.Stdout, "ready")
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		if scanner.Text() == "unlock" {
			if err := lock.Unlock(); err != nil {
				os.Exit(6)
			}
			fmt.Fprintln(os.Stdout, "unlocked")
		}
	}
	runtime.KeepAlive(lock)
	// Intentionally leave runtime files and the lock descriptor: actual OS
	// exit must release the lock; the parent tests safe stale-file cleanup.
	os.Exit(0)
}

func ownedStopTarget(t *testing.T) (*daemonStopTarget, func() error, func() error, func() error) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("GORTEX_DAEMON_PIDFILE", filepath.Join(dir, "daemon.pid"))
	t.Setenv("GORTEX_DAEMON_STATEFILE", filepath.Join(dir, "daemon.json"))
	t.Setenv("GORTEX_DAEMON_SOCKET", filepath.Join(dir, "daemon.sock"))
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDaemonStopTargetChild$")
	cmd.Env = append(os.Environ(), "GORTEX_TEST_STOP_TARGET_BACKEND="+filepath.Join(dir, "store.sqlite"), "GORACE=atexit_sleep_ms=0")
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	var once sync.Once
	var waitErr error
	wait := func() error { once.Do(func() { waitErr = cmd.Wait() }); return waitErr }
	t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Process.Kill(); _ = wait() })
	reader := bufio.NewReader(stdout)
	line, err := reader.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "ready\n", line)
	target, err := captureDaemonStopTarget(cmd.Process.Pid)
	require.NoError(t, err)
	unlock := func() error {
		if _, err := fmt.Fprintln(stdin, "unlock"); err != nil {
			return err
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		if line != "unlocked\n" {
			return fmt.Errorf("unexpected child unlock receipt: %q", line)
		}
		return nil
	}
	return target, stdin.Close, wait, unlock
}

func TestDaemonStopTargetRetainsDeclaredStoreAfterRuntimeRemoval(t *testing.T) {
	target, exit, wait, _ := ownedStopTarget(t)
	free, err := target.storeFree()
	require.NoError(t, err)
	require.False(t, free)
	require.ErrorContains(t, target.removeExitedRuntime(), "still running")
	require.NoError(t, exit())
	require.NoError(t, wait()) // Actual helper process reaped before cleanup.
	free, err = target.storeFree()
	require.NoError(t, err)
	require.True(t, free, "actual OS exit did not release the captured store lock")
	require.NoError(t, target.removeExitedRuntime())
	gone, err := target.runtimeGone()
	require.NoError(t, err)
	require.True(t, gone)
	// The pre-stop store identity remains usable after ReadRuntimeState can
	// no longer discover a live daemon or its removed state file.
	_, live := daemon.ReadRuntimeState()
	require.False(t, live)
	free, err = target.storeFree()
	require.NoError(t, err)
	require.True(t, free)
}

func TestDaemonStopTargetDoesNotFallbackWhenExpectedLockIsMissing(t *testing.T) {
	target, exit, wait, _ := ownedStopTarget(t)
	path := target.store.path
	require.NoError(t, exit())
	require.NoError(t, wait())
	require.NoError(t, os.Remove(path))
	free, err := target.storeFree()
	require.ErrorIs(t, err, os.ErrNotExist)
	require.False(t, free, "a known store with missing lock became a no-store fallback")
	_, err = os.Lstat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestDaemonStopTargetRefusesChangedRuntimeWithoutRemovingOtherFiles(t *testing.T) {
	target, exit, wait, _ := ownedStopTarget(t)
	require.NoError(t, exit())
	require.NoError(t, wait())
	// Rewrite in place: unchanged inode alone is not sufficient provenance.
	require.NoError(t, os.WriteFile(daemon.PIDFilePath(), []byte(strconv.Itoa(target.process.PID()+1)), 0o600))
	gone, err := target.runtimeGone()
	require.ErrorContains(t, err, "changed content")
	require.False(t, gone)
	require.ErrorContains(t, target.removeExitedRuntime(), "changed content")
	for _, file := range target.files {
		_, err := os.Lstat(file.path)
		require.NoError(t, err, "changed provenance must refuse the whole cleanup")
	}
}

func TestDaemonStopTargetLegacyNoStoreFallbackRequiresActualExit(t *testing.T) {
	known, exit, wait, _ := ownedStopTarget(t)
	require.NoError(t, os.Remove(daemon.RuntimeStatePath()))
	legacy, err := captureDaemonStopTarget(known.process.PID())
	require.NoError(t, err)
	require.True(t, legacy.noStore)
	require.Nil(t, legacy.store, "legacy fallback must not guess the default store")
	free, err := legacy.storeFree()
	require.NoError(t, err)
	require.False(t, free, "live process with undeclared store was called free")
	require.NoError(t, exit())
	require.NoError(t, wait())
	free, err = legacy.storeFree()
	require.NoError(t, err)
	require.True(t, free)
	require.NoError(t, legacy.removeExitedRuntime())
}

func TestDaemonStopRuntimeFileRefusesNewAndReplacedPaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.pid")
	missing, err := captureDaemonStopFile(path, true)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte("123"), 0o600))
	_, err = missing.present()
	require.ErrorContains(t, err, "appeared or changed identity")
	captured, err := captureDaemonStopFile(path, true)
	require.NoError(t, err)
	require.NoError(t, os.Rename(path, path+".old"))
	require.NoError(t, os.WriteFile(path, []byte("123"), 0o600))
	_, err = captured.present()
	require.ErrorContains(t, err, "changed identity", "identical content cannot certify a replacement inode")
	_, err = captureDaemonStopFile(t.TempDir(), false)
	require.ErrorContains(t, err, "unexpected daemon endpoint file type")
}
