package snapshot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestBusyRefusalNamesTheHolderAndSurvivesAnUnusableRecord protects the two
// halves of the lock file's disclosure, whose silent breakage leaves an
// operator with a refused command and no way to find the process to stop.
//
// The failure modes: a refusal that names neither the holder nor its pid, in
// the message or in the typed details, so "another codectx process" is all an
// operator is ever told; and a record the waiter cannot parse turning the
// refusal into an invented holder rather than an honest one -- or, worse, the
// courtesy write turning a successful acquisition into a failure.
func TestBusyRefusalNamesTheHolderAndSurvivesAnUnusableRecord(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	held, err := LockWorkspace(ctx, dir, "watch", TryOnce())
	if err != nil {
		t.Fatalf("the first acquisition must succeed: %v", err)
	}
	defer held.Close()

	_, err = LockWorkspace(ctx, dir, "index", TryOnce())
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != model.CodeWorkspaceBusy {
		t.Fatalf("a held workspace must be refused CTX_WORKSPACE_BUSY, got %v", err)
	}
	pid := strconv.Itoa(os.Getpid())
	if !contains(typed.Message, "watch") || !contains(typed.Message, pid) {
		t.Fatalf("the refusal must name the holder and its pid, got %q", typed.Message)
	}
	if typed.Details["holder_operation"] != "watch" || typed.Details["holder_pid"] != pid {
		t.Fatalf("the refusal must carry the holder in its details, got %v", typed.Details)
	}

	// A holder that recorded nothing usable: the refusal says so rather than
	// naming a holder it cannot read, and stays the same typed refusal.
	if err := os.WriteFile(filepath.Join(dir, lockFileName), []byte("not a record"), 0o600); err != nil {
		t.Fatalf("overwrite the record: %v", err)
	}
	_, err = LockWorkspace(ctx, dir, "index", TryOnce())
	if !errors.As(err, &typed) || typed.Code != model.CodeWorkspaceBusy {
		t.Fatalf("an unreadable record must still be the busy refusal, got %v", err)
	}
	if len(typed.Details) != 0 || !contains(typed.Message, "did not record") {
		t.Fatalf("an unreadable record must name no holder, got %q %v", typed.Message, typed.Details)
	}

	// An operation name carrying a newline records one line and not two: a
	// second line would forge a record for the waiter to parse instead.
	forged := t.TempDir()
	l, err := LockWorkspace(ctx, forged, "index\n999 refresh", TryOnce())
	if err != nil {
		t.Fatalf("acquire the forged-name workspace: %v", err)
	}
	recorded, err := os.ReadFile(filepath.Join(forged, lockFileName))
	if err != nil {
		t.Fatalf("read the record: %v", err)
	}
	if contains(string(recorded), "\n") {
		t.Fatalf("the record must be one line, got %q", recorded)
	}
	// Released, the record goes with the lock: a waiter that arrives before
	// the next holder records itself is told nobody did, not handed the name
	// of a process that has already let go.
	if err := l.Close(); err != nil {
		t.Fatalf("release the forged-name workspace: %v", err)
	}
	if left, err := os.ReadFile(filepath.Join(forged, lockFileName)); err != nil || len(left) != 0 {
		t.Fatalf("a released lock must leave no holder recorded, got %q %v", left, err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestAWaiterWaitsOutAProgressingHolderAndRefusesAStalledOne protects the rule
// that replaced the fixed lock wait, whose silent breakage is the defect that
// motivated it: a command refused although the workspace was being used
// correctly, because somebody's index took longer than a number nobody could
// choose.
//
// The failure modes: a bound creeping back in, so a holder that keeps working
// is abandoned after some duration and the arriving command is refused for no
// reason; a waiter that never gives up, so a holder whose process died leaves
// every later command hanging instead of reporting the retryable refusal that
// names it; and a waiter that goes silent, so the operator cannot tell a wait
// from a hang.
func TestAWaiterWaitsOutAProgressingHolderAndRefusesAStalledOne(t *testing.T) {
	ctx := context.Background()

	// The holder is progressing. It is held for eleven seconds, longer than
	// any fixed lock wait this product ever had (ten), and the grace below is
	// a fifth of a second -- so nothing but the probe's answer can be what
	// keeps the waiter waiting.
	dir := t.TempDir()
	held, err := LockWorkspace(ctx, dir, "index", TryOnce())
	if err != nil {
		t.Fatalf("the holder must acquire: %v", err)
	}
	const hold = 11 * time.Second
	stages := []string{"capture", "parse", "publish"}
	start := time.Now()
	probe := func(context.Context, WaitingHolder) (string, bool, error) {
		// One stage per third of the hold, so the observer below sees the
		// holder move rather than one stage repeated.
		at := int(3 * time.Since(start) / hold)
		return stages[min(at, len(stages)-1)], true, nil
	}
	var observed []WaitingHolder
	go func() {
		time.Sleep(hold)
		held.Close()
	}()
	lock, err := LockWorkspace(ctx, dir, "refresh",
		WaitWhileProgressing(200*time.Millisecond, probe, func(h WaitingHolder) { observed = append(observed, h) }))
	if err != nil {
		t.Fatalf("a waiter behind a progressing holder must not be refused: %v", err)
	}
	defer lock.Close()
	if waited := time.Since(start); waited < hold {
		t.Fatalf("the waiter acquired after %s, before the holder let go at %s", waited, hold)
	}

	// The observer was told who it was waiting for, and told again only when
	// that changed. The poll ran about a hundred and ten times, so an upper
	// bound of one report per stage plus the end is the whole claim; which of
	// the stages a poll happened to sample is timing and is not asserted.
	if len(observed) > len(stages)+1 {
		t.Fatalf("the waiter must report the holder on change and not per poll, got %v", observed)
	}
	for i := 1; i < len(observed); i++ {
		if observed[i] == observed[i-1] {
			t.Fatalf("report %d repeats the one before it: %v", i, observed)
		}
	}
	// Every report but the last names the holder, and its stages arrive in the
	// order the holder moved through them.
	next := 0
	for i, h := range observed[:len(observed)-1] {
		if !h.Waiting || h.PID != os.Getpid() || h.Operation != "index" {
			t.Fatalf("report %d must name the holder's pid and operation, got %v", i, h)
		}
		for next < len(stages) && stages[next] != h.Stage {
			next++
		}
		if next == len(stages) {
			t.Fatalf("report %d names %q, which is not the next stage the holder entered: %v", i, h.Stage, observed)
		}
	}
	if last := observed[len(observed)-1]; last.Waiting {
		t.Fatalf("the end of the wait must be reported as such, got %v", last)
	}

	// The holder has stopped showing a stamp. Once the grace has passed with
	// nothing advancing, the waiter reports the typed refusal and names it.
	stalled := t.TempDir()
	stuck, err := LockWorkspace(ctx, stalled, "index", TryOnce())
	if err != nil {
		t.Fatalf("the stalled holder must acquire: %v", err)
	}
	defer stuck.Close()
	_, err = LockWorkspace(ctx, stalled, "refresh",
		WaitWhileProgressing(200*time.Millisecond, func(context.Context, WaitingHolder) (string, bool, error) { return "", false, nil }, nil))
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != model.CodeWorkspaceBusy || !typed.Retryable {
		t.Fatalf("a holder that stopped progressing must end the wait with a retryable CTX_WORKSPACE_BUSY, got %v", err)
	}
	if typed.Details[DetailHolderOperation] != "index" || typed.Details[DetailHolderPID] != strconv.Itoa(os.Getpid()) {
		t.Fatalf("the refusal must name the holder's pid and operation, got %v", typed.Details)
	}
}

// TestAProbeErrorEndsTheWaitAtOnceWithThatError protects the invariant that a
// probe which cannot judge the holder at all ends the acquisition immediately
// with its own diagnosis.
//
// The failure mode: the error is swallowed into "no progress", so the waiter
// spends the whole grace and then reports the generic CTX_WORKSPACE_BUSY --
// blaming a holder that is working perfectly well, after a wait that could
// never have succeeded. The grace here is deliberately far longer than the
// assertion on elapsed time, so a wait that spent it cannot pass.
func TestAProbeErrorEndsTheWaitAtOnceWithThatError(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	held, err := LockWorkspace(ctx, dir, "index", TryOnce())
	if err != nil {
		t.Fatalf("the holder must acquire: %v", err)
	}
	defer held.Close()

	diagnosis := &model.Error{Code: model.CodeSchemaMismatch, Message: "the holder is another build"}
	var observed []WaitingHolder
	var seen WaitingHolder
	const grace = 30 * time.Second
	start := time.Now()
	_, err = LockWorkspace(ctx, dir, "refresh", WaitWhileProgressing(grace,
		func(_ context.Context, h WaitingHolder) (string, bool, error) {
			seen = h
			return "", false, diagnosis
		},
		func(h WaitingHolder) { observed = append(observed, h) }))
	if !errors.Is(err, diagnosis) {
		t.Fatalf("the probe's own error must be what the caller gets, got %v", err)
	}
	if waited := time.Since(start); waited > grace/10 {
		t.Fatalf("the wait ended after %s: a probe error must not spend the grace", waited)
	}
	// The probe is told who it is judging, so it can name the holder itself.
	if seen.PID != os.Getpid() || seen.Operation != "index" || !seen.Waiting || seen.Stage != "" {
		t.Fatalf("the probe must be handed the holder with no stage, got %v", seen)
	}
	// The waiting line is closed: the observer's last word is the end of the
	// wait, or the command's own output lands appended to a progress line.
	if len(observed) == 0 || observed[len(observed)-1].Waiting {
		t.Fatalf("the end of the wait must be reported as such, got %v", observed)
	}
}
