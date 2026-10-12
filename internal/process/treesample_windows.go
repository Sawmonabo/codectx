//go:build windows

package process

import (
	"errors"
	"math"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	// getProcessMemoryInfo is K32GetProcessMemoryInfo, a process's memory
	// counters, and isProcessInJob is IsProcessInJob; neither is wrapped by
	// the system package.
	getProcessMemoryInfo = kernel32.NewProc("K32GetProcessMemoryInfo")
	isProcessInJob       = kernel32.NewProc("IsProcessInJob")
)

// processMemoryCountersEx is the PROCESS_MEMORY_COUNTERS_EX structure the
// call fills. Its layout is the platform's; Cb must hold its size.
type processMemoryCountersEx struct {
	Cb                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
	PrivateUsage               uintptr
}

// treeSweeper sweeps the job the child was assigned to (jobObject.started),
// which holds every descendant the child starts, whatever its parent.
func treeSweeper(job treeTarget) func() treeSweep {
	return func() treeSweep { return sweepJob(windows.Handle(job)) }
}

// sweepJob sums the anonymous resident memory of every process in the job.
// A process's figure is the smaller of its working set and its private
// commit. The working set is what is in memory now, but it also counts the
// shared pages of the executable and libraries, which stay on the host's
// standby list once dropped; the private commit is the memory the process
// holds for itself, but it also counts committed pages it never touched. The
// anonymous resident part is within both, so the smaller of the two is the
// figure that never adds back more than the child holds, and adding back too
// much would widen the allocation past what the host has.
//
// A process that exited between the listing and its reading is absent, not an
// error, and so is one whose ID a new process outside the job took in that
// moment. One whose counters are denied makes the sweep's figure unavailable.
func sweepJob(job windows.Handle) treeSweep {
	pids, ok := jobPIDs(job)
	if !ok {
		return treeSweep{}
	}
	sums := treeSweep{read: true}
	for _, pid := range pids {
		held, member, readable := processHeld(job, uint32(pid))
		if !member {
			continue
		}
		sums.found = true
		if !readable || held > uint64(math.MaxInt64-sums.anon) {
			sums.read = false
			continue
		}
		sums.anon += int64(held)
	}
	return sums
}

// processHeld reads one process's figure. member is false for a process that
// is gone or is not in the job; readable is false where the process is in the
// job and its counters could not be read.
func processHeld(job windows.Handle, pid uint32) (held uint64, member, readable bool) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		// The ID no longer names a process: it exited after the listing.
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return 0, false, false
		}
		return 0, true, false
	}
	defer windows.CloseHandle(h)
	// The ID is checked against the job through the handle now held, so a
	// process that took a departed member's ID is not counted.
	var inJob int32
	if r, _, _ := isProcessInJob.Call(uintptr(h), uintptr(job), uintptr(unsafe.Pointer(&inJob))); r == 0 {
		return 0, true, false
	}
	if inJob == 0 {
		return 0, false, false
	}
	var c processMemoryCountersEx
	c.Cb = uint32(unsafe.Sizeof(c))
	if r, _, _ := getProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&c)), uintptr(c.Cb)); r == 0 {
		return 0, true, false
	}
	return min(uint64(c.WorkingSetSize), uint64(c.PrivateUsage)), true, true
}

// jobPIDs lists the process IDs in the job (JobObjectBasicProcessIdList).
// The list grows until it holds every process the job reports, so a job of
// any size is read in full.
func jobPIDs(job windows.Handle) ([]uintptr, bool) {
	// The structure is two 32-bit counts, the processes assigned and the IDs
	// in the list, followed by the list of pointer-sized IDs.
	header := int(8 / unsafe.Sizeof(uintptr(0)))
	room := 1
	for {
		buf := make([]uintptr, header+room)
		err := windows.QueryInformationJobObject(job, windows.JobObjectBasicProcessIdList,
			uintptr(unsafe.Pointer(&buf[0])), uint32(len(buf))*uint32(unsafe.Sizeof(buf[0])), nil)
		counts := (*[2]uint32)(unsafe.Pointer(&buf[0]))
		assigned, listed := int(counts[0]), int(counts[1])
		if err != nil && !errors.Is(err, windows.ERROR_MORE_DATA) {
			return nil, false
		}
		if err == nil && listed >= assigned {
			return buf[header : header+listed], true
		}
		// Room for the job as last counted and as many again, so a job that
		// grew since the count is still read in one more call.
		room = 2*max(assigned, listed) + 1
	}
}
