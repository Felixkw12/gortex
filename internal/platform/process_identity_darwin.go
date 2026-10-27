package platform

import "golang.org/x/sys/unix"

func processBirth(pid int) ([2]uint64, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return [2]uint64{}, err
	}
	return [2]uint64{uint64(info.Proc.P_starttime.Sec), uint64(info.Proc.P_starttime.Usec)}, nil
}
