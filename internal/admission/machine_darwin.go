//go:build darwin && cgo

package admission

/*
#include <mach/mach.h>
#include <mach/vm_page_size.h>
#include <stdint.h>

// codectx_vm_pages takes one HOST_VM_INFO64 snapshot of the host's page
// counts and the kernel's page size. It returns 0 where the call does not
// succeed. The host port right mach_host_self hands out is given back, so
// reading before every admission leaks no port reference.
static int codectx_vm_pages(uint64_t *free_pages, uint64_t *speculative, uint64_t *external, uint64_t *page_size) {
	vm_statistics64_data_t vm;
	mach_msg_type_number_t count = HOST_VM_INFO64_COUNT;
	mach_port_t host = mach_host_self();
	kern_return_t kr = host_statistics64(host, HOST_VM_INFO64, (host_info64_t)&vm, &count);
	mach_port_deallocate(mach_task_self(), host);
	if (kr != KERN_SUCCESS) {
		return 0;
	}
	*free_pages = vm.free_count;
	*speculative = vm.speculative_count;
	*external = vm.external_page_count;
	*page_size = vm_kernel_page_size;
	return 1;
}
*/
import "C"

// ObserveMachine reports the memory available to a new workload on macOS:
// free pages less speculative ones, plus file-backed pages, from one kernel
// snapshot, times the kernel's page size (darwinAvailable). macOS has no
// control groups, so there is no second limit to take the smaller of. A
// snapshot the kernel does not hand back is unobserved, never zero.
func ObserveMachine() Machine {
	var free, speculative, external, pageSize C.uint64_t
	if C.codectx_vm_pages(&free, &speculative, &external, &pageSize) == 0 {
		return Machine{}
	}
	available, ok := darwinAvailable(uint64(free), uint64(speculative), uint64(external), uint64(pageSize))
	if !ok {
		return Machine{}
	}
	return Machine{AvailableBytes: available, Observed: true}
}
