//go:build linux

package dependence

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// maxMeminfoBytes bounds what is read from the kernel's report. The field is
// near the top of the file and the file is small; the bound exists because
// Section 6 requires one on every read.
const maxMeminfoBytes = 64 * kiB

// ObserveMachine reports the memory the host says is available for a new
// workload. MemAvailable is the kernel's own estimate of what can be taken
// without swapping, which is the figure the allocation is meant to be derived
// from; a missing or unreadable value is reported as unobserved rather than
// as zero bytes free (Section 22).
func ObserveMachine() Machine {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return Machine{}
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
			return Machine{}
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || kb < 0 || kb > (1<<53)/kiB {
			return Machine{}
		}
		return Machine{AvailableBytes: kb * kiB, Observed: true}
	}
	return Machine{}
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
