//go:build unix

package process

import (
	"bytes"
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
		} else if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v, want it to unwrap to context.Canceled", err)
		} else {
			var typed *model.Error
			if !errors.As(err, &typed) || typed.Code != model.CodeCanceled {
				// internal/cli reports an untyped error as the exit-2 usage
				// class, which would label a deliberate cancellation a bad
				// command line.
				t.Fatalf("Run returned %v, want a typed %s", err, model.CodeCanceled)
			}
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

// TestOutputOverTheCapCompletesAndIsFlagged protects the invariant that a
// capture bound bounds memory and nothing else.
//
// Requirement: a child that produces three times the bound still runs to
// completion and still has every record it wrote consumed; only the bytes this
// package would otherwise have to hold are dropped, and only for a stream it is
// capturing.
//
// Mutation that fails it: terminate the process tree at the capture bound,
// which turns a large repository into a refusal.
func TestOutputOverTheCapCompletesAndIsFlagged(t *testing.T) {
	requireExecutable(t, "/bin/sh")
	runner, dir := testRunner(t)

	// 3072 bytes of stdout against a 1024-byte capture bound.
	const (
		records   = 96
		recordLen = 32
		capBytes  = records * recordLen / 3
	)
	script := "i=0; while [ $i -lt 96 ]; do printf '%031d\\n' $i; i=$((i+1)); done"

	result, err := runner.Run(context.Background(), Spec{
		Path:           "/bin/sh",
		Args:           []string{"-c", script},
		Dir:            dir,
		MaxStdoutBytes: capBytes,
		MaxStderrBytes: capBytes,
		Grace:          5 * time.Second,
	})
	if err != nil {
		t.Fatalf("a child three times over its capture bound failed the run: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("exit code %d, want 0", result.ExitCode)
	}
	if !result.OutputTruncated {
		t.Error("Result does not report that captured bytes were dropped")
	}
	if int64(len(result.Stdout)) != capBytes {
		t.Errorf("captured %d bytes of stdout, want exactly the %d-byte bound", len(result.Stdout), capBytes)
	}

	// The same child, its stdout handed to a caller's writer: a writer is fed
	// every byte and paced by its own Write, so all 96 records arrive and
	// nothing is flagged. A framed protocol could not survive anything less.
	counter := &recordCounter{}
	result, err = runner.Run(context.Background(), Spec{
		Path:           "/bin/sh",
		Args:           []string{"-c", script},
		Dir:            dir,
		Stdout:         counter,
		MaxStdoutBytes: capBytes,
		MaxStderrBytes: capBytes,
		Grace:          5 * time.Second,
	})
	if err != nil {
		t.Fatalf("a child writing to a caller's sink failed the run: %v", err)
	}
	if counter.lines != records {
		t.Errorf("the sink consumed %d records, want all %d", counter.lines, records)
	}
	if result.OutputTruncated {
		t.Error("a caller-supplied sink was reported as truncated; its bytes are never dropped")
	}
	if result.StdoutBytes != records*recordLen {
		t.Errorf("StdoutBytes is %d, want %d", result.StdoutBytes, records*recordLen)
	}
}

// recordCounter counts the newline-terminated records a stream delivered.
type recordCounter struct{ lines int }

func (c *recordCounter) Write(b []byte) (int, error) {
	c.lines += bytes.Count(b, []byte{'\n'})
	return len(b), nil
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

	// The same run proves the other half of the boundary: a Spec that names no
	// environment gives the child none of the parent's. An os/exec Cmd with a
	// nil Env inherits the parent's, which would hand every analyzer this
	// process's tokens, proxy settings and paths.
	//
	// It also proves what the child does get: the UTF-8 locale. A child with
	// no locale names files through an ASCII path encoding and cannot open a
	// source file whose name holds a letter outside ASCII at all -- measured
	// against the real analysis payload, one such file failed a whole project.
	requireExecutable(t, "/bin/sh")
	t.Setenv("CODECTX_TEST_SECRET", "leaked")
	result, err = runner.Run(context.Background(), Spec{
		Path:           "/bin/sh",
		Args:           []string{"-c", `echo "${CODECTX_TEST_SECRET-absent}" "${LC_ALL-none}" "${LANG-none}"`},
		Dir:            dir,
		MaxStdoutBytes: 4096,
		MaxStderrBytes: 4096,
		Timeout:        30 * time.Second,
		Grace:          200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	locale := utf8Locale()
	if want := "absent " + locale + " " + locale; strings.TrimSpace(string(result.Stdout)) != want {
		t.Fatalf("child saw %q, want %q: an unlisted variable absent and the UTF-8 locale set", result.Stdout, want)
	}
}

// TestSpecRejectsUntrustedExecution protects the trust boundary: a relative or
// PATH-resolved command is not an approved executable, and running one would
// let whatever is first on PATH act with this tool's authority. A file that is
// not executable was never an approved tool either, and saying so names the
// profile instead of failing later with a bare permission error.
func TestSpecRejectsUntrustedExecution(t *testing.T) {
	runner, dir := testRunner(t)
	notExecutable := filepath.Join(dir, "tool")
	if err := os.WriteFile(notExecutable, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	for _, path := range []string{"echo", notExecutable} {
		_, err := runner.Run(context.Background(), Spec{
			Path:           path,
			Dir:            dir,
			MaxStdoutBytes: 16,
			MaxStderrBytes: 16,
			Timeout:        time.Second,
			Grace:          time.Second,
		})
		var typed *model.Error
		if !errors.As(err, &typed) || typed.Code != model.CodeTrustRequired {
			t.Fatalf("Run(%q) returned %v, want a typed %s", path, err, model.CodeTrustRequired)
		}
	}
}

// TestStdinIsBoundedAndReleased protects the two ways a bounded input goes
// wrong: input silently dropped at the bound, and the pipe descriptors leaking
// when the run fails before the copy ever starts. A server that leaks two
// descriptors per failed analyzer run stops being able to open files at all.
func TestStdinIsBoundedAndReleased(t *testing.T) {
	requireExecutable(t, "/bin/cat")
	runner, dir := testRunner(t)

	_, err := runner.Run(context.Background(), Spec{
		Path:           "/bin/cat",
		Dir:            dir,
		Stdin:          strings.NewReader(strings.Repeat("a", 4096)),
		MaxStdinBytes:  16,
		MaxStdoutBytes: 4096,
		MaxStderrBytes: 4096,
		Timeout:        30 * time.Second,
		Grace:          200 * time.Millisecond,
	})
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != model.CodeResourceLimit {
		t.Fatalf("Run returned %v for input over its bound, want a typed %s", err, model.CodeResourceLimit)
	}

	// A file with the execute bit that is not a valid image fails at exec,
	// which is the early return that runs after the stdin pipe exists.
	broken := filepath.Join(dir, "broken")
	if err := os.WriteFile(broken, []byte{0x00, 0x01, 0x02}, 0o700); err != nil {
		t.Fatalf("write: %v", err)
	}
	before := openDescriptors(t)
	for range 32 {
		_, err := runner.Run(context.Background(), Spec{
			Path:           broken,
			Dir:            dir,
			Stdin:          strings.NewReader("input"),
			MaxStdinBytes:  16,
			MaxStdoutBytes: 4096,
			MaxStderrBytes: 4096,
			Timeout:        30 * time.Second,
			Grace:          200 * time.Millisecond,
		})
		if err == nil {
			t.Fatal("Run reported success for a file that is not an executable image")
		}
	}
	if after := openDescriptors(t); after > before+4 {
		t.Fatalf("32 failed runs left %d open descriptors, up from %d", after, before)
	}

	// The same must hold when the run ends through one of the stop decisions
	// rather than by the child exiting: the copy is still in flight there, and
	// a run that returns without closing the write end leaves it parked on a
	// pipe forever. The wall clock is the stop decision used here because an
	// output bound no longer is one.
	requireExecutable(t, "/bin/sh")
	before = openDescriptors(t)
	for range 16 {
		_, err := runner.Run(context.Background(), Spec{
			Path:           "/bin/sh",
			Args:           []string{"-c", "while :; do echo aaaaaaaaaaaaaaaaaaaaaaaa; done"},
			Dir:            dir,
			Stdin:          &endlessReader{},
			MaxStdinBytes:  1 << 20,
			MaxStdoutBytes: 64,
			MaxStderrBytes: 64,
			Timeout:        200 * time.Millisecond,
			Grace:          50 * time.Millisecond,
		})
		if !errors.As(err, &typed) || typed.Code != model.CodeProviderTimeout {
			t.Fatalf("Run returned %v, want a typed %s", err, model.CodeProviderTimeout)
		}
	}
	if after := openDescriptors(t); after > before+4 {
		t.Fatalf("16 stopped runs left %d open descriptors, up from %d", after, before)
	}
}

// endlessReader supplies input the child will never finish reading, which is
// what keeps the copy in flight when the run is stopped.
type endlessReader struct{}

func (*endlessReader) Read(b []byte) (int, error) {
	for i := range b {
		b[i] = 'x'
	}
	return len(b), nil
}

// openDescriptors counts this process's open files, skipping the test when the
// platform offers no cheap way to ask.
func openDescriptors(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("open descriptors cannot be counted here: %v", err)
	}
	return len(entries)
}

// TestLiveSubprocessCountUnwindsOnFailure protects the Section 23 live
// subprocess figure against the one way it can go permanently wrong: a counter
// that is raised when a child starts and lowered only when the run succeeds.
//
// Failure mode: every timeout, cancellation, output refusal and non-zero exit
// would leave the count one higher than the truth, so a long-lived server
// eventually reports a machine full of children that all exited, and the memory
// accounting built on that figure refuses work forever. The count is therefore
// asserted to rise while a child is genuinely running -- without which the
// unwind assertion would pass on a counter that never counts -- and to return to
// its starting value after a run that failed.
func TestLiveSubprocessCountUnwindsOnFailure(t *testing.T) {
	requireExecutable(t, "/bin/sh")
	runner, dir := testRunner(t)
	if got := runner.LiveSubprocesses(); got != 0 {
		t.Fatalf("a fresh runner reports %d live children, want 0", got)
	}

	// A child that outlives its timeout: the increment has happened, the run
	// fails, and the decrement must still be on the way out.
	observed := make(chan int64, 1)
	go func() {
		var peak int64
		for i := 0; i < 200; i++ {
			if n := runner.LiveSubprocesses(); n > peak {
				peak = n
			}
			time.Sleep(5 * time.Millisecond)
		}
		observed <- peak
	}()
	_, err := runner.Run(context.Background(), Spec{
		Path:           "/bin/sh",
		Args:           []string{"-c", "sleep 30"},
		Dir:            dir,
		MaxStdoutBytes: 4096,
		MaxStderrBytes: 4096,
		Timeout:        300 * time.Millisecond,
		Grace:          200 * time.Millisecond,
	})
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != model.CodeProviderTimeout {
		t.Fatalf("Run returned %v, want a typed %s", err, model.CodeProviderTimeout)
	}
	if peak := <-observed; peak == 0 {
		t.Fatal("the live count never rose while a child was running, so the unwind below proves nothing")
	}
	if got := runner.LiveSubprocesses(); got != 0 {
		t.Fatalf("after a run that timed out the runner reports %d live children, want 0", got)
	}

	// A child that exits non-zero on its own is the other failure shape.
	if _, err := runner.Run(context.Background(), Spec{
		Path:           "/bin/sh",
		Args:           []string{"-c", "exit 3"},
		Dir:            dir,
		MaxStdoutBytes: 4096,
		MaxStderrBytes: 4096,
		Timeout:        30 * time.Second,
		Grace:          time.Second,
	}); err == nil {
		t.Fatal("a child that exited 3 was reported as a clean run")
	}
	if got := runner.LiveSubprocesses(); got != 0 {
		t.Fatalf("after a non-zero exit the runner reports %d live children, want 0", got)
	}
}

// TestAdmissionWaitsForHeadroom protects the class-C rule for the runner's
// byte budgets.
//
// Requirement: a run that does not fit the REMAINING budget waits for headroom
// and then runs, and only a reservation larger than the whole user-set budget
// is refused.
//
// Mutation that fails it: refuse a reservation that does not fit the remaining
// budget. A second analysis unit is then refused outright because a first is
// still holding memory, which turns capacity that exists a second later into a
// failed unit.
func TestAdmissionWaitsForHeadroom(t *testing.T) {
	requireExecutable(t, "/bin/sh")
	runner, err := NewRunner(Limits{MaxConcurrent: 4, MemoryBudgetBytes: 1000, DiskBudgetBytes: 1 << 30})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	dir := t.TempDir()
	// A deadline rather than Background: a regression that reintroduces a
	// deadlock must fail this test, not hang the package under -race.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	spec := func(seconds string) Spec {
		return Spec{Path: "/bin/sh", Args: []string{"-c", "sleep " + seconds}, Dir: dir,
			Grace: time.Second, MemoryReservationBytes: 600}
	}

	// The first run holds 600 of 1000 bytes; the second needs 600 and cannot
	// fit beside it.
	held := make(chan struct{})
	first := make(chan error, 1)
	go func() {
		close(held)
		_, err := runner.Run(ctx, spec("1"))
		first <- err
	}()
	<-held
	// Wait until the first run actually holds its reservation, so the second
	// is admitted through the queue rather than racing ahead of it.
	for {
		runner.mu.Lock()
		used := runner.memoryUsed
		runner.mu.Unlock()
		if used == 600 {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("the first run never took its reservation")
		}
		time.Sleep(time.Millisecond)
	}

	started := time.Now()
	if _, err := runner.Run(ctx, spec("0")); err != nil {
		t.Fatalf("a run that did not fit the remaining budget was refused instead of waiting: %v", err)
	}
	if waited := time.Since(started); waited < 100*time.Millisecond {
		t.Fatalf("the second run was admitted after %s; it must have waited for the first to release 600 bytes", waited)
	}
	if err := <-first; err != nil {
		t.Fatalf("first run: %v", err)
	}

	// Everything is handed back: no admission leaks past a completed run.
	runner.mu.Lock()
	used, running, queued := runner.memoryUsed, runner.running, runner.queue.Len()
	runner.mu.Unlock()
	if used != 0 || running != 0 || queued != 0 {
		t.Fatalf("after both runs: %d bytes reserved, %d running, %d queued; want all zero", used, running, queued)
	}

	// The one refusal that remains: a request larger than the whole user-set
	// budget, which no amount of waiting can satisfy. It is reported with
	// both numbers rather than waiting forever.
	_, err = runner.Run(ctx, Spec{Path: "/bin/sh", Args: []string{"-c", "true"}, Dir: dir,
		Grace: time.Second, MemoryReservationBytes: 1001})
	if err == nil || !strings.Contains(err.Error(), "over the runner budget of 1000") {
		t.Fatalf("a reservation larger than the whole budget = %v; want it refused and reported", err)
	}
}

// TestAdmissionLeavesTheQueueOnCancellation protects the waiter's exit path: a
// queued run whose context ends must leave the queue, or the queue grows with
// every abandoned caller and the head-of-line rule blocks on a run nobody is
// waiting for any more.
func TestAdmissionLeavesTheQueueOnCancellation(t *testing.T) {
	requireExecutable(t, "/bin/sh")
	runner, err := NewRunner(Limits{MaxConcurrent: 4, MemoryBudgetBytes: 1000, DiskBudgetBytes: 1 << 30})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	dir := t.TempDir()
	hold, release := context.WithCancel(context.Background())
	defer release()
	blocking := make(chan struct{})
	go func() {
		defer close(blocking)
		_, _ = runner.Run(hold, Spec{Path: "/bin/sh", Args: []string{"-c", "sleep 30"}, Dir: dir,
			Grace: time.Second, MemoryReservationBytes: 900})
	}()
	for {
		runner.mu.Lock()
		used := runner.memoryUsed
		runner.mu.Unlock()
		if used == 900 {
			break
		}
		time.Sleep(time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = runner.Run(ctx, Spec{Path: "/bin/sh", Args: []string{"-c", "true"}, Dir: dir,
		Grace: time.Second, MemoryReservationBytes: 900})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a queued run whose context ended returned %v, want the cancellation", err)
	}
	release()
	<-blocking
	runner.mu.Lock()
	queued, used := runner.queue.Len(), runner.memoryUsed
	runner.mu.Unlock()
	if queued != 0 || used != 0 {
		t.Fatalf("%d waiters and %d bytes left after a cancelled wait; want none", queued, used)
	}
}
