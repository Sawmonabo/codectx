//go:build darwin && cgo

package admission

import (
	"testing"

	"golang.org/x/sys/unix"
)

// TestObserveMachineReadsTheHost protects the macOS reader: a host whose
// memory is readable is observed, and its figure is a positive amount no
// larger than the machine's physical memory. Mutations that fail it: the
// reader left unobserved (every macOS admission then runs against the
// stand-in), multiplying by the wrong page size or reading the counters in
// the wrong order, which reads zero or more memory than the machine has.
func TestObserveMachineReadsTheHost(t *testing.T) {
	total, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		t.Fatalf("the machine's physical memory cannot be read to check against: %v", err)
	}
	m := ObserveMachine()
	if !m.Observed {
		t.Fatal("macOS available memory is unobserved")
	}
	if m.AvailableBytes <= 0 || uint64(m.AvailableBytes) > total {
		t.Fatalf("available memory is %d bytes on a machine with %d", m.AvailableBytes, total)
	}
}
