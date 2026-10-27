//go:build acceptance

package main

import (
	"context"
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
