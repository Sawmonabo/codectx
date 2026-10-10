//go:build darwin && cgo

package residency

/*
#include <mach/mach.h>
#include <stdint.h>

// codectx_task_vm reads this task's own memory counters (TASK_VM_INFO), in
// bytes. It returns 0 where the call does not succeed.
static int codectx_task_vm(uint64_t *resident, uint64_t *peak, uint64_t *internal) {
	task_vm_info_data_t info;
	mach_msg_type_number_t count = TASK_VM_INFO_COUNT;
	if (task_info(mach_task_self(), TASK_VM_INFO, (task_info_t)&info, &count) != KERN_SUCCESS) {
		return 0;
	}
	*resident = info.resident_size;
	*peak = info.resident_size_peak;
	*internal = info.internal;
	return 1;
}
*/
import "C"

// Read reads this task's memory counters once: the resident set, its
// lifetime peak, and the resident anonymous ("internal") pages as the
// anonymous part. The macOS peak cannot be reset, so it spans the whole
// process life. Every figure is absent where the call fails.
func Read() Reading {
	var resident, peak, internal C.uint64_t
	if C.codectx_task_vm(&resident, &peak, &internal) == 0 {
		return Reading{}
	}
	return Reading{
		Peak:     figure(uint64(peak)),
		Resident: figure(uint64(resident)),
		Anon:     figure(uint64(internal)),
	}
}
