//go:build darwin && cgo

package main

/*
#include <libproc.h>
#include <sys/resource.h>
static int load_process_usage(int pid, struct rusage_info_v4 *info) {
    return proc_pid_rusage(pid, RUSAGE_INFO_V4, (rusage_info_t *)info);
}
*/
import "C"

import "fmt"

type darwinProcessSampler struct{}

func newProcessSampler() processSampler { return darwinProcessSampler{} }

func (darwinProcessSampler) sample(pid int) map[string]Metric {
	var info C.struct_rusage_info_v4
	if status := C.load_process_usage(C.int(pid), &info); status != 0 {
		return absentProcessMetrics(fmt.Sprintf("proc_pid_rusage failed (%d)", status))
	}
	metrics := absentProcessMetrics("not provided by macOS proc_pid_rusage")
	metrics["cpu_seconds"] = measured((float64(info.ri_user_time)+float64(info.ri_system_time))/1e9, "s")
	metrics["rss_bytes"] = measured(float64(info.ri_resident_size), "bytes")
	metrics["phys_footprint_bytes"] = measured(float64(info.ri_phys_footprint), "bytes")
	metrics["logical_written_bytes"] = measured(float64(info.ri_logical_writes), "bytes")
	metrics["diskio_written_bytes"] = measured(float64(info.ri_diskio_byteswritten), "bytes")
	return metrics
}
