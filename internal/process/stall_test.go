package process

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestStalledTreeIsTerminatedAndProgressingTreeIsNot protects the one
// invariant the wall-clock timeout cannot express (Q4): with no wall clock at
// all, a wedged child must still be stopped, and a slow-but-working child must
// still be allowed to finish. Both halves are needed -- a detector that only
// killed would be a timeout under another name.
func TestStalledTreeIsTerminatedAndProgressingTreeIsNot(t *testing.T) {
	requireExecutable(t, "/bin/sh")
	runner, dir := testRunner(t)

	spec := func(script string) Spec {
		return Spec{
			Path: "/bin/sh", Args: []string{"-c", script}, Dir: dir,
			MaxStdoutBytes: 1 << 20, MaxStderrBytes: 1 << 20,
			Timeout: 0, StallTimeout: 300 * time.Millisecond, Grace: 2 * time.Second,
		}
	}

	// A child that neither writes nor computes is wedged, whatever the
	// repository's size.
	result, err := runner.Run(context.Background(), spec("sleep 30"))
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != model.CodeProviderTimeout {
		t.Fatalf("a stalled child returned %v, want a typed %s", err, model.CodeProviderTimeout)
	}
	if got := typed.Details["stop_reason"]; got != "stalled" {
		t.Errorf("stop_reason is %q, want \"stalled\"; a stall must be distinguishable from a wall-clock timeout", got)
	}
	if !result.TimedOut {
		t.Error("Result does not report the stall that terminated the tree")
	}

	// The same detector, the same budget, a child that writes one line every
	// 50 ms for well past the stall timeout: progress, not elapsed time, is
	// what the watchdog watches.
	result, err = runner.Run(context.Background(), spec("i=0; while [ $i -lt 20 ]; do echo working; sleep 0.05; i=$((i+1)); done"))
	if err != nil {
		t.Fatalf("a slow but progressing child was terminated: %v", err)
	}
	if result.TimedOut {
		t.Error("Result reports a timeout for a child that made progress throughout")
	}
	if result.StdoutBytes == 0 {
		t.Fatal("the progressing child produced no output, so the fixture proves nothing")
	}

	// The third shape is the one the two above cannot express and the one the
	// real indexers have: a child that says nothing on either pipe, burns no
	// measurable CPU, and writes its answer straight to its output file. Its
	// only signal is that file's growth, and on a platform with no tree
	// sampling it is the only signal there is.
	quiet := spec("")
	quiet.StallTimeout = 600 * time.Millisecond
	quiet.ProgressFiles = []string{filepath.Join(dir, "progress.bin")}
	quiet.Args = []string{"-c", strings.Repeat("sleep 0.25; printf 1234567890 >> progress.bin; ", 8)}
	result, err = runner.Run(context.Background(), quiet)
	if err != nil {
		t.Fatalf("a quiet child that grew its output file was terminated: %v", err)
	}
	if result.TimedOut {
		t.Error("Result reports a timeout for a child whose output file grew throughout")
	}
	if result.StdoutBytes != 0 || result.StderrBytes != 0 {
		t.Fatal("the quiet child spoke on a pipe, so the fixture does not test the output-file signal")
	}
	if info, err := os.Stat(quiet.ProgressFiles[0]); err != nil || info.Size() != 80 {
		t.Fatalf("the quiet child wrote %v (%v), so the fixture proves nothing", info, err)
	}

	// Where the platform samples a running tree, CPU time is a signal in its
	// own right, and it is only a signal if it is refreshed at least as often
	// as the watchdog polls: this child says nothing and writes nothing for
	// well over a stall timeout short enough to poll faster than the sampler's
	// own default period.
	if treeSampled {
		burn := spec("i=0; while [ $i -lt 3000000 ]; do i=$((i+1)); done")
		burn.StallTimeout = 200 * time.Millisecond
		result, err = runner.Run(context.Background(), burn)
		if err != nil {
			t.Fatalf("a silent CPU-burning child was terminated after %v: %v", result.Duration, err)
		}
		if result.Duration < 400*time.Millisecond {
			t.Fatalf("the CPU-burning child ran %v, under two stall windows, so the fixture proves nothing", result.Duration)
		}
	}
}
