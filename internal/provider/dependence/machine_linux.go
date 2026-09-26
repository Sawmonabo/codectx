//go:build linux

package dependence

import (
	"bufio"
	"os"
	"strconv"
	"strings"

	"github.com/Sawmonabo/codectx/internal/config"
)

// maxMeminfoBytes bounds what is read from the kernel's report. The field is
// near the top of the file and the file is small; the bound exists because
// Section 6 requires one on every read.
const maxMeminfoBytes = 64 * kiB

// ObserveMachine reports the memory available to a new workload of this
// process: the smaller of the host's MemAvailable and the headroom this
// process's control groups leave it (config.CgroupMemoryHeadroom).
//
// Both are read because they are different limits. MemAvailable is the
// kernel's own estimate of what the host can give without swapping; a process
// in a limited group can take no more than the group's limit less its usage,
// however much the host has free, and a container on a large host reading
// MemAvailable alone would admit children against memory its group will
// kill them for. Either may be unobserved, and an unobserved one is not zero
// (Section 22): the other then stands alone, and the machine is unobserved
// only when neither was read.
func ObserveMachine() Machine {
	available, availableOK := memAvailable()
	headroom, headroomOK := config.CgroupMemoryHeadroom()
	switch {
	case availableOK && headroomOK:
		return Machine{AvailableBytes: min(available, headroom), Observed: true}
	case availableOK:
		return Machine{AvailableBytes: available, Observed: true}
	case headroomOK:
		return Machine{AvailableBytes: headroom, Observed: true}
	}
	return Machine{}
}

// memAvailable is MemAvailable from /proc/meminfo, and false where the file or
// the field cannot be read.
func memAvailable() (int64, bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	defer f.Close()
	s := bufio.NewScanner(&limitedReader{r: f, left: maxMeminfoBytes})
	for s.Scan() {
		rest, ok := strings.CutPrefix(s.Text(), "MemAvailable:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return 0, false
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || kb < 0 || kb > (1<<53)/kiB {
			return 0, false
		}
		return kb * kiB, true
	}
	return 0, false
}

// limitedReader stops a read at a fixed byte count without reporting an error
// the scanner would have to distinguish from a short file.
type limitedReader struct {
	r    *os.File
	left int64
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.left <= 0 {
		return 0, os.ErrClosed
	}
	if int64(len(p)) > l.left {
		p = p[:l.left]
	}
	n, err := l.r.Read(p)
	l.left -= int64(n)
	return n, err
}
