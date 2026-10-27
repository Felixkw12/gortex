package platform

import (
	"errors"
	"fmt"
)

// ErrProcessIdentityChanged means a PID names a different process incarnation.
var ErrProcessIdentityChanged = errors.New("process incarnation changed")

// ProcessIdentity names one observed process incarnation, rather than every
// process that may eventually receive the same PID. Its zero value is invalid.
type ProcessIdentity struct {
	pid   int
	birth [2]uint64
}

// CaptureProcessIdentity fails when the native birth identity cannot be read.
// Unlike ProcessAlive, it can identify an exited but not yet reaped Unix child.
func CaptureProcessIdentity(pid int) (ProcessIdentity, error) {
	if pid <= 0 || int64(pid) > 1<<31-1 {
		return ProcessIdentity{}, errors.New("invalid process PID")
	}
	birth, err := processBirth(pid)
	if err != nil {
		return ProcessIdentity{}, fmt.Errorf("capture process %d identity: %w", pid, err)
	}
	if birth == [2]uint64{} {
		return ProcessIdentity{}, fmt.Errorf("process %d has no usable birth identity", pid)
	}
	return ProcessIdentity{pid: pid, birth: birth}, nil
}

// PID returns the captured PID; it is not by itself proof of ownership.
func (p ProcessIdentity) PID() int { return p.pid }

// Matches reports whether the native PID still names the captured incarnation.
// Missing, inaccessible or unsupported native metadata is an error, not proof
// that it is safe to signal or remove another process's runtime files.
func (p ProcessIdentity) Matches() (bool, error) {
	if p.pid <= 0 || p.birth == [2]uint64{} {
		return false, errors.New("invalid captured process identity")
	}
	birth, err := processBirth(p.pid)
	return birth == p.birth && err == nil, err
}

// Running checks both identity and native execution state. Exited Unix zombies
// are not running, but remain matchable until reaped. A missing process reports
// os.ErrProcessDone; inaccessible or unsupported metadata remains an error.
func (p ProcessIdentity) Running() (bool, error) {
	if p.pid <= 0 || p.birth == [2]uint64{} {
		return false, errors.New("invalid captured process identity")
	}
	birth, running, err := processBirthAndRunning(p.pid)
	if err != nil {
		return false, err
	}
	if birth != p.birth {
		return false, ErrProcessIdentityChanged
	}
	return running, nil
}

// Terminate requests termination only after checking this incarnation. On Unix
// the check and PID signal are separate syscalls; this is not atomic pidfd
// signaling. On Windows both operations use the same native process handle.
func (p ProcessIdentity) Terminate() error { return signalProcessIdentity(p, false) }

// Kill forcibly terminates this incarnation, with the same identity safeguards
// and Unix check-to-signal limitation as Terminate.
func (p ProcessIdentity) Kill() error { return signalProcessIdentity(p, true) }
