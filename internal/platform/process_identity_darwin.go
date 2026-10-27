package platform

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func processBirth(pid int) ([2]uint64, error) {
	birth, _, err := processBirthAndRunning(pid)
	return birth, err
}

func processBirthAndRunning(pid int) ([2]uint64, bool, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		if errors.Is(err, unix.ESRCH) {
			err = errors.Join(os.ErrProcessDone, err)
		}
		return [2]uint64{}, false, err
	}
	return [2]uint64{uint64(info.Proc.P_starttime.Sec), uint64(info.Proc.P_starttime.Usec)}, info.Proc.P_stat != darwinProcessZombie, nil
}
