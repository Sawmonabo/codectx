//go:build linux

package process

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/paced"
)

// TestNamedOutputsAreHandedToTheDiskOneWindowAtATime runs a child that writes
// 64 MiB to a named output as fast as it can and counts the windows the pacer
// handed to the disk.
//
// Requirement: a child's output reaches the disk as it is written. A child
// writes at memory speed and dirties gigabytes that nothing submits until the
// kernel's flusher wakes; measured, an analysis child dirtied 1.5 GB at
// 50 MB/s and the kernel then submitted 714 MB in one second with 407
// requests in flight, which stalls every other process on the machine for as
// long as the backlog takes. The child cannot pace itself, so the runner
// paces its named outputs from outside, one window in flight per file.
//
// The window count is the writeback proof: each window is submitted and
// waited for in one call that returns only once that range has been written
// back, so a count of eight windows for a 64 MiB file is eight ranges the
// disk has taken before the runner returned.
//
// Mutation that fails it: never start the pacer in Runner.run (`var outputs
// *paced.Outputs`), which leaves the child's output entirely to the kernel.
func TestNamedOutputsAreHandedToTheDiskOneWindowAtATime(t *testing.T) {
	requireExecutable(t, "/bin/sh")
	runner, dir := testRunner(t)
	out := filepath.Join(dir, "child-output")
	const size = 64 << 20

	before := paced.OutputWindows()
	result, err := runner.Run(context.Background(), Spec{
		Path:          "/bin/sh",
		Args:          []string{"-c", "head -c 67108864 /dev/zero > " + out},
		Dir:           dir,
		ProgressFiles: []string{out},
		Grace:         5 * time.Second,
	})
	if err != nil {
		t.Fatalf("the child failed the run: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code %d, want 0", result.ExitCode)
	}
	if st, err := os.Stat(out); err != nil || st.Size() != size {
		t.Fatalf("the child did not write its %d-byte output: %v", size, err)
	}
	// Eight whole windows make up the file; the bound admits one lost to a
	// poll that saw the file half written and handed its tail over at the end.
	if windows := paced.OutputWindows() - before; windows < 7 {
		t.Errorf("the pacer handed %d windows of a %d MiB output to the disk, want at least 7: the child's output was left to the kernel",
			windows, size>>20)
	}
}
