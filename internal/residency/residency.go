// Package residency reads this process's own resident memory from the kernel.
// It is the one reader of those figures in the product: the parent reads its
// resident set and its peak through it, and a parser worker, a separate
// process, measures each file's need through it. It imports nothing outside the
// standard library, so every one of them can depend on it.
//
// A figure the platform does not report is absent (nil), never zero: a
// missing reading is not a process holding no memory.
package residency

import (
	"math"
	"os"
	"strconv"
	"strings"
)

// statusPath is the kernel's per-process status file. Outside Linux it does
// not exist, and every figure is absent.
const statusPath = "/proc/self/status"

// Reading is one read of the status file, in bytes. Each figure is nil where
// the file cannot be read or does not carry it in the form expected, and each
// present figure fits int64.
type Reading struct {
	// Peak is the resident high-water mark (VmHWM): the kernel's own, over
	// the whole process life or since it was last reset.
	Peak *uint64
	// Resident is the resident set now (VmRSS).
	Resident *uint64
	// Anon is the anonymous part of the resident set (RssAnon).
	Anon *uint64
}

// Read reads this process's status file once. Every figure is absent where the
// file cannot be read.
func Read() Reading {
	raw, err := os.ReadFile(statusPath)
	if err != nil {
		return Reading{}
	}
	return Parse(string(raw))
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
