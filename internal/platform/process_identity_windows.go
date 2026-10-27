package platform

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

func processHandleBirth(handle windows.Handle) ([2]uint64, error) {
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
		return [2]uint64{}, err
	}
	return [2]uint64{uint64(creation.HighDateTime)<<32 | uint64(creation.LowDateTime), 0}, nil
}

func processBirth(pid int) (birth [2]uint64, err error) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return birth, err
	}
	defer func() { err = errors.Join(err, windows.CloseHandle(handle)) }()
	return processHandleBirth(handle)
}

func signalProcessIdentity(p ProcessIdentity, _ bool) (err error) {
	if p.pid <= 0 || p.birth == [2]uint64{} {
		return errors.New("invalid captured process identity")
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE, false, uint32(p.pid))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, windows.CloseHandle(handle)) }()
	birth, err := processHandleBirth(handle)
	if err != nil {
		return err
	}
	if birth != p.birth {
		return fmt.Errorf("refusing to signal changed process incarnation %d", p.pid)
	}
	return windows.TerminateProcess(handle, 1)
}
