package admission

import "math"

// The arithmetic of the macOS and Windows readers. It carries no build tag, so
// it is the same code on every platform and is tested on every platform; the
// readers in machine_darwin.go and machine_windows.go only fetch the counters
// it combines.

// darwinAvailable is available memory on macOS from one kernel snapshot of
// page counts: free pages less speculative ones, plus file-backed pages,
// times the kernel's page size.
//
// It is the counterpart of Linux's MemAvailable, free memory plus the file
// cache the kernel can drop without writing to disk. Speculative pages are
// read-ahead, counted both inside the free count and among the file-backed
// pages, so they are taken out of free once. The free count less the
// speculative count is clamped at zero, because the kernel fills the two
// counters a moment apart and a host short of memory can report more
// speculative pages than free ones. Inactive pages are not counted: they
// include anonymous pages macOS reclaims only by compressing or swapping,
// which is the slowdown the allocation exists to prevent.
//
// The counts are in kernel pages, so pageSize is the kernel's page size and
// never this process's: a translated x86-64 build on a 16 KiB-page machine
// would otherwise multiply by 4 KiB and read low. A zero page size, or a
// product that does not fit int64, is not a reading.
func darwinAvailable(free, speculative, external, pageSize uint64) (int64, bool) {
	if pageSize == 0 {
		return 0, false
	}
	pages := uint64(0)
	if free > speculative {
		pages = free - speculative
	}
	if external > math.MaxUint64-pages {
		return 0, false
	}
	pages += external
	if pages > uint64(math.MaxInt64)/pageSize {
		return 0, false
	}
	return int64(pages * pageSize), true
}

// memoryStatusEx is the MEMORYSTATUSEX structure GlobalMemoryStatusEx fills.
// Its layout is the platform's; Length must hold its size before the call.
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// windowsAvailable is available memory on Windows from one
// GlobalMemoryStatusEx call: the smaller of available physical memory and
// the memory this process can still commit. returned is the call's result,
// and a zero return is a failed call, so not a reading.
//
// Available physical memory is the standby, free and zero lists, memory that
// can be reused without writing it to disk first: the counterpart of Linux's
// MemAvailable. The commit figure is the line past which allocations fail,
// and it is bounded by the commit limit of a job this process runs in, which
// is how a Windows container's memory limit is enforced (the Windows test
// TestJobMemoryLimitBoundsTheReading proves that bound). Their minimum mirrors
// Linux's smaller of MemAvailable and the control group's headroom. A figure
// that does not fit int64 is not a reading.
func windowsAvailable(returned uintptr, status memoryStatusEx) (int64, bool) {
	if returned == 0 {
		return 0, false
	}
	available := min(status.AvailPhys, status.AvailPageFile)
	if available > math.MaxInt64 {
		return 0, false
	}
	return int64(available), true
}
