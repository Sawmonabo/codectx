// Package residency reads this process's own resident memory from the kernel.
// It is the one reader of those figures in the product: the parent reads its
// resident set and its peak through it, and a parser worker, a separate
// process, measures each file's need through it. It imports nothing outside the
// standard library, so every one of them can depend on it.
//
// Linux reads the per-process status file (residency_linux.go), macOS the
// task's own memory counters (residency_darwin.go, a cgo build) and Windows
// the process memory counters (residency_windows.go). Elsewhere every figure
// is absent (residency_other.go).
//
// A figure the platform does not report is absent (nil), never zero: a
// missing reading is not a process holding no memory.
package residency

import (
	"math"
	"strconv"
	"strings"
)

// Reading is one reading of this process's resident memory, in bytes. Each
// figure is nil where the platform does not report it or the reading failed,
// and each present figure fits int64.
type Reading struct {
	// Peak is the resident high-water mark: the kernel's own, over the whole
	// process life or, on Linux, since it was last reset (VmHWM).
	Peak *uint64
	// Resident is the resident set now (VmRSS on Linux).
	Resident *uint64
	// Anon is the anonymous part of the resident set (RssAnon on Linux).
	Anon *uint64
}

// figure is a reported count of bytes as a Reading field: absent where it
// does not fit int64, which no real resident figure exceeds.
func figure(bytes uint64) *uint64 {
	if bytes > math.MaxInt64 {
		return nil
	}
	return &bytes
}

// Parse reads VmHWM, VmRSS and RssAnon, which the kernel states in kB
// (kibibytes), from the text of a per-process status file. A line that is
// missing, malformed, in another unit or too large for int64 bytes leaves its
// figure absent.
func Parse(text string) Reading {
	var r Reading
	for line := range strings.SplitSeq(text, "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		var field **uint64
		switch name {
		case "VmHWM":
			field = &r.Peak
		case "VmRSS":
			field = &r.Resident
		case "RssAnon":
			field = &r.Anon
		default:
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) != 2 || fields[1] != "kB" {
			continue
		}
		kib, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil || kib > math.MaxInt64/1024 {
			continue
		}
		bytes := kib * 1024
		*field = &bytes
	}
	return r
}
