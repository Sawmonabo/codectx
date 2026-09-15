// Package testenv holds test helpers whose subject is the HOST a test runs on
// rather than anything this product builds. It is imported only by tests.
package testenv

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// SkipIfLoaded skips t when the host is busy enough that a wall-clock
// measurement made on it describes the machine rather than the code.
//
// It guards the tests that assert a RATIO of two elapsed measurements. Those
// tests prove a real property -- a cost that does not grow with N, a reuse that
// pays for itself -- but the property is only observable when the two halves
// are measured under comparable conditions, and a host under contention
// staggers one half and not the other. Loosening the ratio instead would trade
// a flake for a proof that no longer forbids the regression it exists for, so
// the ratio stays and the measurement is refused on a loaded host.
//
// The threshold is half the CPU count: the run itself contributes to the load
// average, so a quiet machine still reports a nonzero figure, and half the
// cores leaves room for the measurement's own work without admitting a host
// that is genuinely oversubscribed.
//
// Where the load average cannot be read -- every platform without /proc -- the
// test runs. An unmeasurable host is not a loaded host, and skipping on one
// would silently disable these proofs wherever the file is absent.
func SkipIfLoaded(t *testing.T) {
	t.Helper()
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return
	}
	load, err := parseLoadavg(string(b))
	if err != nil {
		return
	}
	cpus := runtime.NumCPU()
	if overloaded(load, cpus) {
		t.Skipf("host load average is %.2f over %d CPUs: a wall-clock ratio measured here "+
			"describes the machine, not the code", load, cpus)
	}
}

// parseLoadavg reads the one-minute figure from the contents of /proc/loadavg.
func parseLoadavg(contents string) (float64, error) {
	field, _, _ := strings.Cut(strings.TrimSpace(contents), " ")
	return strconv.ParseFloat(field, 64)
}

// overloaded is the threshold decision, separated from the file read so it can
// be exercised without a host in a particular state.
func overloaded(load float64, cpus int) bool {
	return load > float64(cpus)/2
}
