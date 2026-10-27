//go:build !windows

package platform

import (
	"fmt"
	"syscall"
)

func signalProcessIdentity(p ProcessIdentity, force bool) error {
	same, err := p.Matches()
	if err != nil {
		return err
	}
	if !same {
		return fmt.Errorf("refusing to signal changed process incarnation %d", p.pid)
	}
	signal := syscall.SIGTERM
	if force {
		signal = syscall.SIGKILL
	}
	return syscall.Kill(p.pid, signal)
}
