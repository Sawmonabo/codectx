package worker

import (
	"math"
	"strconv"
	"strings"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// host is what the file boundary asks of the platform. nativeHost
// (memory_linux.go, memory_other.go) supplies it.
type host struct {
	// status is the text of the kernel's per-process status file, false
	// where it cannot be read.
	status func() (string, bool)
	// returnHeap returns the C heap's freed pages to the kernel, and reports
	// false where the C library offers no way to or the heap could not be
	// held to one arena.
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
	// when the boundary before it returned the C heap and reset the peak;
	// otherwise nil, and the file's need is unavailable.
	from *uint64
}

// boundary closes one file's measurement and opens the next: it reads the
// peak the file reached, returns the freed C heap, resets the peak and reads
// the new base. The need is the peak less the base the file started from. It
// is absent, never zero, where either reading is missing, where the previous
// boundary could not return the heap or reset the peak (the peak then spans
// earlier files, or the file reused memory the heap kept, and the reading is
// censored), and where it computes below zero, which only a reading taken
// inconsistently yields. BaseBytes and AnonBytes are the resident set once the
// heap has been returned, reported whenever they can be read.
func (m *meter) boundary() wire.Memory {
	var mem wire.Memory
	if text, ok := m.host.status(); ok && m.from != nil {
		if peak := parseStatus(text).peak; peak != nil && *peak >= *m.from {
			need := *peak - *m.from
			mem.NeedBytes = &need
		}
	}
	returned := m.host.returnHeap()
	reset := m.host.resetPeak()
	m.from = nil
	if text, ok := m.host.status(); ok {
		s := parseStatus(text)
		mem.BaseBytes, mem.AnonBytes = s.resident, s.anon
		if returned && reset {
			m.from = s.resident
		}
	}
	return mem
}

// statusReading is the three resident-set figures of one status read, each
// nil where the file does not carry it in the form expected.
type statusReading struct {
	peak, resident, anon *uint64
}

// parseStatus reads VmHWM, VmRSS and RssAnon, which the kernel states in kB
// (kibibytes), from the text of a per-process status file.
func parseStatus(text string) statusReading {
	var s statusReading
	for line := range strings.SplitSeq(text, "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		var field **uint64
		switch name {
		case "VmHWM":
			field = &s.peak
		case "VmRSS":
			field = &s.resident
		case "RssAnon":
			field = &s.anon
		default:
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) != 2 || fields[1] != "kB" {
			continue
		}
		kib, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil || kib > math.MaxUint64/1024 {
			continue
		}
		bytes := kib * 1024
		*field = &bytes
	}
	return s
}
