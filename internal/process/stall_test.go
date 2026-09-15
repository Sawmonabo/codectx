package process

import (
	"context"
	"errors"
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
}
