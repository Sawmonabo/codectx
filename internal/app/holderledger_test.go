package app

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/index"
	"github.com/Sawmonabo/codectx/internal/ledger"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/snapshot"

	_ "modernc.org/sqlite"
)

// TestAHolderWhoseLedgerThisBuildCannotReadIsDiagnosedNotWaitedOut protects
// the invariant that a waiter which cannot read the holder's stamps AT ALL says
// so immediately, instead of calling the holder stalled.
//
// The failure mode: this build reads a ledger another schema wrote as absent,
// so the holder -- a process of another build -- never shows progress, and a
// waiter spends the whole liveness window and then reports the holder stalled:
// a wrong diagnosis of a healthy holder, after a wait that could never have
// succeeded. Both halves matter, so both are asserted: the busy refusal naming
// the holder, and that it arrived without spending the grace. It is retryable
// like every busy refusal: once the holder exits, the next holder replaces the
// ledger with its own schema's.
//
// Mutations it catches: the probe reading the foreign ledger as "no progress"
// (the wait spends the grace), or the refusal naming no holder.
//
// The fixture is a real ledger file whose fingerprint is then changed in place,
// because ledger.OpenReader reads ledger_meta and ledger.Fingerprint is derived
// from the schema: a faked reader would prove nothing about the real open.
func TestAHolderWhoseLedgerThisBuildCannotReadIsDiagnosedNotWaitedOut(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	forgeForeignLedger(t, ctx, dir)

	held, err := snapshot.LockWorkspace(ctx, dir, "index", snapshot.TryOnce())
	if err != nil {
		t.Fatalf("the holder must acquire: %v", err)
	}
	defer held.Close()

	start := time.Now()
	_, err = (&stack{dataDir: dir, operation: "refresh"}).acquire(ctx, index.HoldPatiently)
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != model.CodeWorkspaceBusy {
		t.Fatalf("a holder whose ledger carries a foreign fingerprint must end the wait with CTX_WORKSPACE_BUSY, got %v", err)
	}
	if waited := time.Since(start); waited > ledger.LiveWindow/10 {
		t.Fatalf("the wait ended after %s: the mismatch must end it at once, not after the %s grace", waited, ledger.LiveWindow)
	}
	if typed.Details[snapshot.DetailHolderPID] != strconv.Itoa(os.Getpid()) ||
		typed.Details[snapshot.DetailHolderOperation] != "index" {
		t.Fatalf("the diagnosis must name the holder's pid and operation, got %v", typed.Details)
	}
	if !typed.Retryable {
		t.Fatalf("the refusal must be retryable: the next holder replaces the foreign ledger")
	}
}

// forgeForeignLedger writes a real ledger beside dir and then rewrites its
// recorded fingerprint, leaving the file a honest artefact of another build.
func forgeForeignLedger(t *testing.T, ctx context.Context, dir string) {
	t.Helper()
	l := ledger.New(dir)
	if err := l.Attach(ctx); err != nil {
		t.Fatalf("write the ledger: %v", err)
	}
	if err := l.Detach(); err != nil {
		t.Fatalf("close the ledger: %v", err)
	}
	path := ledger.Path(dir)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("reopen the ledger: %v", err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `UPDATE ledger_meta SET fingerprint = 'a-build-this-one-is-not' WHERE singleton = 1`); err != nil {
		t.Fatalf("forge the fingerprint: %v", err)
	}
	// The reader opens the file read-only, which cannot recover a write-ahead
	// log: the forged row must be in the database file itself.
	if _, err := db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close the forged ledger: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ledger.FileName+"-wal")); err == nil {
		t.Fatalf("the forged ledger still has a write-ahead log beside it")
	}
}

// TestAResetHoldingTheWorkspaceReadsAsProgressing asks the waiting probe about
// a reset that holds the workspace while the run ledger it is removing is
// gone. The ledger can never show a reset progressing, so a waiter judging it
// by the ledger would spend the liveness window and refuse a reset that is
// still removing a large data directory. A holder of another operation with
// no ledger still reads as not progressing, so the grace still guards it.
//
// Mutation that fails it: drop the reset branch from ledgerProgress (the reset
// reads as not progressing).
func TestAResetHoldingTheWorkspaceReadsAsProgressing(t *testing.T) {
	ctx := context.Background()
	probe, closeProbe := ledgerProgress(t.TempDir())
	defer closeProbe()
	stage, progressing, err := probe(ctx, snapshot.WaitingHolder{Waiting: true, PID: os.Getpid(), Operation: resetOperation})
	if err != nil || !progressing || stage != resetOperation {
		t.Fatalf("probe(reset) = %q, %v, %v; want the reset progressing", stage, progressing, err)
	}
	if _, progressing, err := probe(ctx, snapshot.WaitingHolder{Waiting: true, PID: os.Getpid(), Operation: "index"}); err != nil || progressing {
		t.Fatalf("probe(index with no ledger) = %v, %v; want not progressing", progressing, err)
	}
}
