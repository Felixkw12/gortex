package platform

import (
	"bytes"
	"errors"
	"os"
	"strconv"
)

func processBirth(pid int) ([2]uint64, error) {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return [2]uint64{}, err
	}
	return procStatBirth(stat)
}

func procStatBirth(stat []byte) ([2]uint64, error) {
	// comm may contain spaces and closing parentheses, as in the zombie probe.
	end := bytes.LastIndexByte(stat, ')')
	if end < 0 {
		return [2]uint64{}, errors.New("missing process comm boundary")
	}
	fields := bytes.Fields(stat[end+1:])
	// fields[0] is stat field 3 (state); starttime is field 22.
	if len(fields) <= 19 || len(fields[0]) != 1 {
		return [2]uint64{}, errors.New("missing process starttime")
	}
	start, err := strconv.ParseUint(string(fields[19]), 10, 64)
	if err != nil || start == 0 {
		return [2]uint64{}, errors.New("invalid process starttime")
	}
	return [2]uint64{start, 0}, nil
}
