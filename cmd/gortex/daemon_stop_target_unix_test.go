//go:build darwin || linux

package main

import (
	"bytes"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDaemonStopTargetOwnsFreeStoreAndRuntimeBeforeZombieIsReaped(t *testing.T) {
	target, exit, wait, _ := ownedStopTarget(t)
	require.NoError(t, exit())
	// Independent native witness, before cmd.Wait: do not let the identity
	// implementation under test define its own zombie oracle.
	require.Eventually(t, func() bool {
		state, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(target.process.PID())).Output()
		return err == nil && strings.HasPrefix(strings.TrimSpace(string(state)), "Z")
	}, 5*time.Second, 10*time.Millisecond)
	free, err := target.storeFree()
	require.NoError(t, err)
	require.True(t, free)
	var out bytes.Buffer
	result, err := waitForDaemonTarget(&out, target, 0, false)
	require.NoError(t, err)
	require.False(t, result.forced)
	require.Contains(t, out.String(), "not yet been reaped by its parent")
	require.NotContains(t, out.String(), "timed out")
	gone, err := target.runtimeGone()
	require.NoError(t, err)
	require.True(t, gone)
	same, err := target.process.Matches()
	require.NoError(t, err)
	require.True(t, same, "fixture was reaped before the store-free/runtime proof")
	running, err := target.process.Running()
	require.NoError(t, err)
	require.False(t, running)
	require.NoError(t, wait()) // Only now release this exact child PID.
}
