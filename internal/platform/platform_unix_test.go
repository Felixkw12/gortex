//go:build !windows

package platform

import (
	"os"
	"testing"
)

// A process owned by another user can't be signalled (kill returns
// EPERM) but still exists, so ProcessAlive must report it as alive.
func TestProcessAliveOtherUser(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can signal every process; EPERM path not reachable")
	}
	// PID 1 (init) always exists and is owned by root.
	if !ProcessAlive(1) {
		t.Error("ProcessAlive(1) = false, want true for a process owned by another user")
	}
}
