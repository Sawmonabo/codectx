//go:build windows || (darwin && cgo)

package admission_test

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/admission"
	"github.com/Sawmonabo/codectx/internal/process"
	"github.com/Sawmonabo/codectx/internal/residency"
)

// touchChildEnv marks the re-run of this test binary that holds touched
// memory for the parent to measure.
const touchChildEnv = "CODECTX_ADMISSION_TOUCH_BYTES"

// TestOwnChildGrowthDoesNotThrottleTheAllocation protects the rule that the
// product's own growth never throttles it, on the platforms whose available
// memory is read alongside the product's residency: a child admitted on the
// ledger that takes N bytes lowers the host's available figure by about N,
// and the residency added back (Runner.ReservedResidentBytes, this process's
// resident set) must give nearly all of it back, so the augmented figure the
// allocation is derived from moves by much less than N. Mutations that fail
// it: a tree sampler that never reports the child's memory, or a residency
// reader that reports none, either of which lowers the figure by the whole N.
func TestOwnChildGrowthDoesNotThrottleTheAllocation(t *testing.T) {
	if v := os.Getenv(touchChildEnv); v != "" {
		holdTouched(t, v)
		return
	}
	const n = 512 << 20
	runner, err := process.NewRunner(process.Limits{})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	augmented := func() int64 {
		m := admission.ObserveMachine()
		if !m.Observed {
			t.Fatal("available memory is unobserved")
		}
		held := runner.ReservedResidentBytes()
		if r := residency.Read().Resident; r != nil {
			held += int64(*r)
		} else {
			t.Fatal("this process's resident set is not reported")
		}
		return m.AvailableBytes + held
	}
	before := augmented()

	ctx, cancel := context.WithCancel(context.Background())
	dir := t.TempDir()
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		_, _ = runner.Run(ctx, process.Spec{
			Path:                   self,
			Args:                   []string{"-test.run=^TestOwnChildGrowthDoesNotThrottleTheAllocation$", "-test.count=1"},
			Env:                    []string{touchChildEnv + "=" + strconv.Itoa(n)},
			Dir:                    dir,
			MaxStdoutBytes:         4096,
			MaxStderrBytes:         4096,
			Grace:                  time.Second,
			MemoryReservationBytes: n,
		})
	}()
	defer func() {
		cancel()
		<-exited
	}()

	// The child's memory is visible once a sweep has read most of it; the
	// deadline only bounds a broken sampler, it is not a pace.
	deadline := time.Now().Add(2 * time.Minute)
	for runner.ReservedResidentBytes() < n/2 {
		select {
		case <-exited:
			t.Fatal("the child exited before its memory was read")
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the child's resident memory was never reported (last %d bytes)", runner.ReservedResidentBytes())
		}
		time.Sleep(50 * time.Millisecond)
	}
	after := augmented()
	if drop := before - after; drop > n/4 {
		t.Fatalf("a child holding %d bytes lowered the augmented available figure by %d", n, drop)
	}
}

// holdTouched is the child: it takes and touches the bytes it is told to,
// then holds them until the parent ends the run.
func holdTouched(t *testing.T, v string) {
	n, err := strconv.Atoi(v)
	if err != nil {
		t.Fatalf("%s=%q: %v", touchChildEnv, v, err)
	}
	buf := make([]byte, n)
	for i := 0; i < len(buf); i += os.Getpagesize() {
		buf[i] = 1
	}
	fmt.Println("touched")
	for {
		time.Sleep(time.Second)
		buf[0]++
	}
}
