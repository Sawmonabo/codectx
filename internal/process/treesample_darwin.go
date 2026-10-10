//go:build darwin && cgo

package process

/*
#include <errno.h>
#include <libproc.h>
#include <stdint.h>
#include <sys/resource.h>

// codectx_pgrp_pids lists the process IDs in process group pgid into pids,
// which holds bytes bytes, and returns the bytes it filled, or a negative
// value where the call fails.
static int codectx_pgrp_pids(int pgid, int *pids, int bytes) {
	return proc_listpids(PROC_PGRP_ONLY, (uint32_t)pgid, pids, bytes);
}

// codectx_pid_memory reads one process's physical footprint and resident set,
// in bytes. It returns 0 on success and the call's errno otherwise.
static int codectx_pid_memory(int pid, uint64_t *footprint, uint64_t *resident) {
	struct rusage_info_v2 info;
	if (proc_pid_rusage(pid, RUSAGE_INFO_V2, (rusage_info_t *)&info) != 0) {
		return errno;
	}
	*footprint = info.ri_phys_footprint;
	*resident = info.ri_resident_size;
	return 0;
}
*/
import "C"

import (
	"math"
	"syscall"
	"unsafe"
)

// treeSweeper sweeps the child's process group. The child is a group leader,
// so its process ID is the group ID, and a descendant that re-parents away
// from it stays in the group; membership by group is also exactly what the
// runner terminates.
func treeSweeper(pgid int) func() treeSweep {
	return func() treeSweep { return sweepGroup(pgid) }
}

// sweepGroup sums the anonymous resident memory of every live process in the
// group. A process's figure is the smaller of its physical footprint and its
// resident set. The footprint is the memory the process dirtied for itself,
// which is what is gone from the host's free and file-backed pages, but it
// also counts compressed pages that are no longer resident; the resident set
// is what is in memory now, but it also counts the shared, file-backed pages
// of the executable and libraries, which stay in the host's available figure.
// The anonymous resident part is within both, so the smaller of the two is
// the figure that never adds back more than the child holds, and adding back
// too much would widen the allocation past what the host has.
//
// A process that exited between the listing and its reading is absent, not
// an error. One whose figure is denied makes the sweep's figure unavailable.
func sweepGroup(pgid int) treeSweep {
	sums := treeSweep{read: true}
	for _, pid := range groupPIDs(pgid) {
		if pid <= 0 {
			continue
		}
		var footprint, resident C.uint64_t
		switch errno := C.codectx_pid_memory(C.int(pid), &footprint, &resident); syscall.Errno(errno) {
		case 0:
		case syscall.ESRCH:
			continue
		default:
			sums.found = true
			sums.read = false
			continue
		}
		sums.found = true
		held := min(uint64(footprint), uint64(resident))
		if held > uint64(math.MaxInt64-sums.anon) {
			sums.read = false
			continue
		}
		sums.anon += int64(held)
	}
	return sums
}

// groupPIDs lists the group's process IDs. The listing grows until it holds
// the whole group, so a group of any size is read in full; an empty list is
// a group that has no members, or one the call could not list.
func groupPIDs(pgid int) []C.int {
	size := int(unsafe.Sizeof(C.int(0)))
	need := int(C.codectx_pgrp_pids(C.int(pgid), nil, 0))
	if need <= 0 {
		return nil
	}
	for {
		// Room for the group as last counted and as many again, so a group
		// that grew since the count is still read in one more call.
		pids := make([]C.int, 2*need/size+1)
		bytes := len(pids) * size
		filled := int(C.codectx_pgrp_pids(C.int(pgid), &pids[0], C.int(bytes)))
		if filled < 0 {
			return nil
		}
		if filled < bytes {
			return pids[:filled/size]
		}
		need = filled
	}
}
