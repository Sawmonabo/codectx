//go:build windows || (darwin && cgo)

package residency

import (
	"os"
	"testing"
)

// TestReadReportsThisProcessResidentSet protects the macOS and Windows
// readers the allocation adds this process's own residency back from. A
// missing resident set leaves the add-back out, so the product's own growth
// throttles it; a figure in the wrong unit adds back memory it does not hold.
// Mutations that fail it: Read returning every figure absent, as the other
// platforms do, or reporting pages or kibibytes where bytes are due, which
// reads a touched buffer as smaller than it is.
func TestReadReportsThisProcessResidentSet(t *testing.T) {
	const touched = 64 << 20
	buf := make([]byte, touched)
	for i := 0; i < len(buf); i += os.Getpagesize() {
		buf[i] = 1
	}
	r := Read()
	if r.Resident == nil {
		t.Fatal("this process's resident set is not reported")
	}
	if *r.Resident < touched {
		t.Fatalf("the resident set reads %d bytes while this process holds %d it has touched", *r.Resident, touched)
	}
	if r.Peak != nil && *r.Peak < *r.Resident {
		t.Fatalf("the resident peak %d is below the resident set %d", *r.Peak, *r.Resident)
	}
	buf[0]++ // keeps the buffer live through the reading
}
