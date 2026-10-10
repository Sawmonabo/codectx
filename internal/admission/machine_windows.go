//go:build windows

package admission

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// globalMemoryStatusEx is kernel32's GlobalMemoryStatusEx, loaded from the
// system directory on first use.
var globalMemoryStatusEx = windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")

// ObserveMachine reports the memory available to a new workload on Windows:
// the smaller of available physical memory and the memory this process can
// still commit, from one GlobalMemoryStatusEx call (windowsAvailable). A
// failed call, or a procedure that cannot be found, is unobserved, never
// zero.
func ObserveMachine() Machine {
	if globalMemoryStatusEx.Find() != nil {
		return Machine{}
	}
	var status memoryStatusEx
	status.Length = uint32(unsafe.Sizeof(status))
	returned, _, _ := globalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&status)))
	available, ok := windowsAvailable(returned, status)
	if !ok {
		return Machine{}
	}
	return Machine{AvailableBytes: available, Observed: true}
}
