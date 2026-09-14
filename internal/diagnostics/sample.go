package diagnostics

import (
	"context"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
)

// HostSampler is the Sampler implementation that reads this host.
//
// L0 ships the vertical slice: this process's resident set size and the bytes
// the Go runtime holds from the operating system, measured for real, with every
// metric this host cannot read left nil. L2 owns the rest of this file -- the
// simultaneous process-tree peak, the queue/cache/query reservations, the live
// subprocess count, pending events and the database, WAL, temp and CAS byte
// totals -- and fills them into the same report in Sample below.
//
// The rule L2 inherits, and the reason L0 proved it here first: a metric that
// cannot be measured stays nil. It is never defaulted to zero, because a zero
// resident set reads as a process using no memory, and Section 23 requires an
// unavailable figure be reported as unavailable.
type HostSampler struct {
	// parentRSS reads this process's resident set size, or returns nil where
	// the host exposes no such figure. It is a field rather than a direct
	// call so the unmeasurable path is provable on a host that can measure:
	// otherwise the invariant that guards every non-Linux platform could only
	// be tested on one.
	parentRSS func() *uint64
}

// NewHostSampler builds the sampler over the real host readers.
func NewHostSampler() *HostSampler { return &HostSampler{parentRSS: parentRSSBytes} }

// Sample reports what this host can measure. It returns no error for a metric
// it cannot read: the nil field is the answer, and an error would make an
// unmeasurable figure fail a call that is otherwise complete.
func (h *HostSampler) Sample(_ context.Context) (model.ResourceReport, error) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	// Sys is the total the runtime has obtained from the operating system,
	// which is the Go-managed figure Section 23 names -- not HeapAlloc, which
	// omits stacks, spans and the allocator's own bookkeeping.
	goManaged := ms.Sys
	return model.ResourceReport{
		ParentRSSBytes: h.parentRSS(),
		GoManagedBytes: &goManaged,
	}, nil
}

// parentRSSBytes reads this process's resident set size from /proc/self/statm,
// whose second field is the resident page count. A host without that file --
// every non-Linux platform, and a Linux host with /proc unmounted -- has no
// portable equivalent, so the figure is absent rather than zero.
func parentRSSBytes() *uint64 {
	raw, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return nil
	}
	fields := strings.Fields(string(raw))
	const residentIndex = 1
	if len(fields) <= residentIndex {
		return nil
	}
	pages, err := strconv.ParseUint(fields[residentIndex], 10, 64)
	if err != nil {
		return nil
	}
	bytes := pages * uint64(os.Getpagesize())
	return &bytes
}
