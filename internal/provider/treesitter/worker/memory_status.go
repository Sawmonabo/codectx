package worker

import (
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
	"github.com/Sawmonabo/codectx/internal/residency"
)

// host is what the file boundary asks of the platform. nativeHost
// (memory_linux.go, memory_other.go) supplies it.
type host struct {
	// read is one reading of the process's resident figures, each absent
	// where the platform does not report it.
	read func() residency.Reading
	// returnHeap returns the Go heap's and the C heap's freed pages to the
	// kernel, and reports false where the C library offers no way to or the
	// C heap could not be held to one arena.
	returnHeap func() bool
	// resetPeak sets the kernel's resident peak back to the current resident
	// set, and reports false where the write fails or the platform has no
	// resettable per-process peak.
	resetPeak func() bool
}

// meter takes the worker's reading of itself at each file boundary. It holds
// the base the file in flight is measured from.
type meter struct {
	host host
	// from is the resident set the file in flight started from, recorded only
	// when the boundary before it returned the heaps and reset the peak;
	// otherwise nil, and the file's need is unavailable.
	from *uint64
}

// boundary closes one file's measurement and opens the next: it reads the
// peak the file reached, returns the freed heaps, resets the peak and reads
// the new base. The need is the peak less the base the file started from. It
// is absent, never zero, where either reading is missing, where the previous
// boundary could not return the heap or reset the peak (the peak then spans
// earlier files, or the file reused memory the heap kept, and the reading is
// censored), and where it computes below zero, which only a reading taken
// inconsistently yields. BaseBytes and AnonBytes are the resident set once the
// heap has been returned, reported whenever they can be read.
func (m *meter) boundary() wire.Memory {
	var mem wire.Memory
	if m.from != nil {
		if peak := m.host.read().Peak; peak != nil && *peak >= *m.from {
			need := *peak - *m.from
			mem.NeedBytes = &need
		}
	}
	returned := m.host.returnHeap()
	reset := m.host.resetPeak()
	m.from = nil
	r := m.host.read()
	mem.BaseBytes, mem.AnonBytes = r.Resident, r.Anon
	if returned && reset && r.Resident != nil {
		m.from = r.Resident
	}
	return mem
}
