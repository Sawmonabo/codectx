//go:build windows

package residency

import (
	"syscall"
	"unsafe"
)

// getProcessMemoryInfo is kernel32's K32GetProcessMemoryInfo, the process
// memory counters, loaded on first use. kernel32 is a known system library,
// mapped into every process from the system directory, so loading it by name
// cannot pick up another copy.
var getProcessMemoryInfo = syscall.NewLazyDLL("kernel32.dll").NewProc("K32GetProcessMemoryInfo")

// processMemoryCounters is the PROCESS_MEMORY_COUNTERS structure the call
// fills. Its layout is the platform's; Cb must hold its size before the call.
type processMemoryCounters struct {
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
}

// Read reads this process's memory counters once: the working set as the
// resident set and its lifetime peak, which Windows cannot reset. Windows
// reports no anonymous part of the working set in these counters, so Anon is
// absent. Every figure is absent where the call fails.
func Read() Reading {
	if getProcessMemoryInfo.Find() != nil {
		return Reading{}
	}
	self, err := syscall.GetCurrentProcess()
	if err != nil {
		return Reading{}
	}
	var c processMemoryCounters
	c.Cb = uint32(unsafe.Sizeof(c))
	if ok, _, _ := getProcessMemoryInfo.Call(uintptr(self), uintptr(unsafe.Pointer(&c)), uintptr(c.Cb)); ok == 0 {
		return Reading{}
	}
	return Reading{
		Peak:     figure(uint64(c.PeakWorkingSetSize)),
		Resident: figure(uint64(c.WorkingSetSize)),
	}
}
