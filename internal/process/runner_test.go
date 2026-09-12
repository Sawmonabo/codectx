//go:build unix

package process

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// testRunner is the one shared process fixture: a runner with budgets large
// enough not to interfere, and a private working directory.
func testRunner(t *testing.T) (*Runner, string) {
	t.Helper()
	r, err := NewRunner(Limits{MaxConcurrent: 2, MemoryBudgetBytes: 1 << 30, DiskBudgetBytes: 1 << 30})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return r, t.TempDir()
}

func requireExecutable(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Skipf("%s is unavailable in this environment: %v", path, err)
	}
}

// TestCancellationKillsTheProcessTree protects the Section 21 process-tree
// control: cancelling a run must terminate every descendant, not just the
// direct child. A surviving grandchild keeps holding memory, disk and file
// descriptors that the resource accounting has already released, and can still
// write into a materialization the caller believes is finished.
func TestCancellationKillsTheProcessTree(t *testing.T) {
	requireExecutable(t, "/bin/sh")
	runner, dir := testRunner(t)
	pidFile := filepath.Join(dir, "grandchild.pid")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := runner.Run(ctx, Spec{
			Path: "/bin/sh",
			// $0 is the third argument: the shell writes the background
			// process's PID there so the test can observe the grandchild.
			Args:           []string{"-c", `sleep 30 & echo $! > "$0"; sleep 30`, pidFile},
			Dir:            dir,
			MaxStdoutBytes: 4096,
			MaxStderrBytes: 4096,
			Timeout:        30 * time.Second,
			Grace:          200 * time.Millisecond,
		})
		done <- err
	}()

	pid := waitForPID(t, pidFile)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run reported success for a cancelled child")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d survived cancellation of its process group", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitForPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the grandchild never recorded its PID in %s", path)
	return 0
}

// TestOutputLimitTerminates protects the bounded-output invariant: a child that
// writes without end must be stopped at the configured ceiling and reported as
// a typed resource limit. Draining it into memory instead would let any
// analyzer exhaust the serving process.
func TestOutputLimitTerminates(t *testing.T) {
	requireExecutable(t, "/bin/sh")
	runner, dir := testRunner(t)

	result, err := runner.Run(context.Background(), Spec{
		Path:           "/bin/sh",
		Args:           []string{"-c", "while :; do echo aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa; done"},
		Dir:            dir,
		MaxStdoutBytes: 4096,
		MaxStderrBytes: 4096,
		Timeout:        30 * time.Second,
		Grace:          200 * time.Millisecond,
	})
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != model.CodeResourceLimit {
		t.Fatalf("Run returned %v, want a typed %s", err, model.CodeResourceLimit)
	}
	if !result.OutputTruncated {
		t.Error("Result does not report the truncation that caused the failure")
	}
	if int64(len(result.Stdout)) > 4096 {
		t.Errorf("captured %d bytes of stdout, over the 4096-byte limit", len(result.Stdout))
	}
}

// TestArgvReachesTheChildLiterally protects the no-shell rule: every argument
// must arrive exactly as written, so a repository-supplied value can never be
// reinterpreted as an expansion, a glob or a second command.
func TestArgvReachesTheChildLiterally(t *testing.T) {
	requireExecutable(t, "/bin/echo")
	runner, dir := testRunner(t)

	args := []string{"$HOME", "a b", "*", "; rm -rf /", "$(id)", "`id`", "a\nb"}
	result, err := runner.Run(context.Background(), Spec{
		Path:           "/bin/echo",
		Args:           args,
		Dir:            dir,
		MaxStdoutBytes: 4096,
		MaxStderrBytes: 4096,
		Timeout:        30 * time.Second,
		Grace:          200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code %d, stderr %q", result.ExitCode, result.Stderr)
	}
	want := strings.Join(args, " ") + "\n"
	if string(result.Stdout) != want {
		t.Fatalf("child received %q, want %q", result.Stdout, want)
	}
}

// TestSpecRejectsUntrustedExecution protects the trust boundary: a relative or
// PATH-resolved command is not an approved executable, and running one would
// let whatever is first on PATH act with this tool's authority.
func TestSpecRejectsUntrustedExecution(t *testing.T) {
	runner, dir := testRunner(t)
	_, err := runner.Run(context.Background(), Spec{
		Path:           "echo",
		Dir:            dir,
		MaxStdoutBytes: 16,
		MaxStderrBytes: 16,
		Timeout:        time.Second,
		Grace:          time.Second,
	})
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != model.CodeTrustRequired {
		t.Fatalf("Run returned %v, want a typed %s", err, model.CodeTrustRequired)
	}
}
