package snapshot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

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

	held, err := LockWorkspace(ctx, dir, "watch", 0)
	if err != nil {
		t.Fatalf("the first acquisition must succeed: %v", err)
	}
	defer held.Close()

	_, err = LockWorkspace(ctx, dir, "index", 0)
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
	_, err = LockWorkspace(ctx, dir, "index", 0)
	if !errors.As(err, &typed) || typed.Code != model.CodeWorkspaceBusy {
		t.Fatalf("an unreadable record must still be the busy refusal, got %v", err)
	}
	if len(typed.Details) != 0 || !contains(typed.Message, "did not record") {
		t.Fatalf("an unreadable record must name no holder, got %q %v", typed.Message, typed.Details)
	}

	// An operation name carrying a newline records one line and not two: a
	// second line would forge a record for the waiter to parse instead.
	forged := t.TempDir()
	l, err := LockWorkspace(ctx, forged, "index\n999 refresh", 0)
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
