//go:build acceptance

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
)

func TestIssue767GracefulStopRejectsForcedSuccessfulExit(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		err          error
		wantError    bool
	}{
		{"graceful", "daemon stopped", nil, false},
		{"already_down", "daemon is not running", nil, false},
		{"draining_advisory", "draining 2 WAL frames, up to 15s before force-killing", nil, false},
		{"forced_exit_zero", "graceful shutdown timed out — force-killing", nil, true},
		{"stalled_close_exit_zero", "close made no progress for 1m — force-killing", nil, true},
		{"pid_fallback_exit_zero", "stopped (force-killed)", nil, true},
		{"bounded_cancel", "partial output", context.DeadlineExceeded, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := issue767GracefulStopResult([]byte(tc.output), tc.err); (err != nil) != tc.wantError {
				t.Fatalf("stop result: %v; wantError=%v", err, tc.wantError)
			}
		})
	}
}

func TestIssue767CleanupCancellationRequiresSuccessfulProcessState(t *testing.T) {
	// Use actual native child exit states rather than inventing an exit code.
	// Git is already a prerequisite for this acceptance fixture.
	states := make(map[string]*os.ProcessState)
	for name, argument := range map[string]string{"zero": "--version", "nonzero": "--issue767-intentional-failure"} {
		cmd := exec.Command("git", "-c", "gc.auto=0", "-c", "maintenance.auto=false", argument)
		_ = cmd.Run()
		if cmd.ProcessState == nil {
			t.Fatal("fixture child did not start")
		}
		states[name] = cmd.ProcessState
	}
	for _, tc := range []struct {
		name, state string
		err         error
		wantClean   bool
	}{
		{"normal", "zero", nil, true},
		{"cleanup_canceled_zero", "zero", context.Canceled, true},
		{"deadline_graceful_zero", "zero", context.DeadlineExceeded, true},
		{"canceled_nonzero", "nonzero", context.Canceled, false},
		{"nonzero", "nonzero", errors.New("exit failed"), false},
		{"other_wait_error", "zero", errors.New("pipe failure"), false},
		{"missing_state", "missing", context.Canceled, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if clean := issue767CleanChildExit(tc.err, states[tc.state]); clean != tc.wantClean {
				t.Fatalf("clean=%t; want=%t", clean, tc.wantClean)
			}
		})
	}
}
