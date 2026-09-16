package sqlite_test

import (
	"bytes"
	"crypto/rand"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	store "github.com/Sawmonabo/codectx/internal/storage/sqlite"
	"modernc.org/sqlite/lib"
)

// The requirement: a delta-state artifact of any size round-trips through
// the store as a sequence of parts, so a manifest or key set the size of a
// large unit is never held whole and has no bound. Mutation: store the whole
// reader in one row (read it all before the insert) and the part count for
// an artifact two and a half parts long is 1.
func TestDeltaStateRoundTripsInParts(t *testing.T) {
	f := newFixture(t, filepath.Join(t.TempDir(), "codectx.db"))
	a := f.file("pkg/a.go", "package pkg\nfunc F() {}\n")
	b := f.file("pkg/b.go", "package pkg\nfunc F() { F() }\n")
	snap := f.snapshot("one", a, b)
	gen, err := f.s.BeginGeneration(f.ctx, f.repo, snap.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatal(err)
	}
	run := f.run(gen)
	w := f.beginScope(gen, run, configHash, a, b)
	f.fillScope(w, run, a, b)

	artifact := make([]byte, 2*store.DeltaStatePartBytes+store.DeltaStatePartBytes/2)
	if _, err := rand.Read(artifact); err != nil {
		t.Fatal(err)
	}
	if err := w.PutDeltaState(f.ctx, "first", bytes.NewReader([]byte("superseded"))); err != nil {
		t.Fatal(err)
	}
	if err := w.PutDeltaState(f.ctx, "first", bytes.NewReader(artifact)); err != nil {
		t.Fatal(err)
	}
	if err := w.PutDeltaState(f.ctx, "empty", bytes.NewReader(nil)); err == nil {
		t.Fatal("an empty artifact was stored; a refresh could not tell it from none")
	} else if !errors.As(err, new(*model.Error)) || err.(*model.Error).Code != model.CodeArgumentInvalid {
		t.Fatalf("empty artifact: %v; want CTX_ARGUMENT_INVALID", err)
	}
	if err := f.s.SealUnit(f.ctx, w); err != nil {
		t.Fatalf("SealUnit: %v", err)
	}
	flushed(t, f.s)

	parts, err := f.s.DeltaStateParts(f.ctx, w.UnitID(), "first")
	if err != nil {
		t.Fatal(err)
	}
	if parts != 3 {
		t.Fatalf("the artifact is stored in %d parts; two and a half parts of bytes take 3", parts)
	}
	var got bytes.Buffer
	if err := f.s.DeltaState(f.ctx, w.UnitID(), "first", &got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), artifact) {
		t.Fatalf("read back %d bytes that differ from the %d stored", got.Len(), len(artifact))
	}
	err = f.s.DeltaState(f.ctx, w.UnitID(), "never", &got)
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Details["reason"] != store.ReasonNotFound {
		t.Fatalf("an absent kind: %v; want CTX_ARGUMENT_INVALID with reason not_found", err)
	}
}

// The requirement: a write the filesystem refused is reported as a full disk
// only when the disk is full, and otherwise as the device's refusal, with
// the engine's own message and code either way. Mutation: make attribute
// return its argument unchanged and every refusal reads as a full disk.
func TestARefusedWriteIsSettledAgainstTheDisk(t *testing.T) {
	f := newFixture(t, filepath.Join(t.TempDir(), "codectx.db"))
	refused := store.WriteRefused("commit", sqlite3.SQLITE_IOERR_WRITE, "disk I/O error")

	restore := f.s.SetFreeBytes(func(string) (uint64, bool) { return 4 << 30, true })
	err := f.s.Attribute(refused)
	restore()
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != model.CodeInternal {
		t.Fatalf("with 4 GiB free the refusal is %v; want CTX_INTERNAL", err)
	}
	for _, want := range []string{"disk I/O error", "engine code 778", "4294967296 bytes were free"} {
		if !strings.Contains(typed.Message, want) {
			t.Fatalf("message %q does not say %q", typed.Message, want)
		}
	}
	if typed.Details["engine_code"] != "778" || typed.Details["free_bytes"] == "" {
		t.Fatalf("details %v carry neither the engine code nor the measurement", typed.Details)
	}

	restore = f.s.SetFreeBytes(func(string) (uint64, bool) { return 12 << 10, true })
	err = f.s.Attribute(refused)
	restore()
	if !errors.As(err, &typed) || typed.Code != model.CodeDiskFull {
		t.Fatalf("with 12 KiB free the refusal is %v; want CTX_DISK_FULL", err)
	}
	if !strings.Contains(typed.Message, "disk I/O error") || !strings.Contains(typed.Message, "12288 bytes are free") {
		t.Fatalf("message %q hides the engine's message or the measurement", typed.Message)
	}

	// Zero free is the definitive full disk, not an unmeasurable one: it is the
	// state the measurement exists to report, and reporting it as unmeasured
	// would send the operator to the device instead of to the space.
	restore = f.s.SetFreeBytes(func(string) (uint64, bool) { return 0, true })
	err = f.s.Attribute(refused)
	restore()
	if !errors.As(err, &typed) || typed.Code != model.CodeDiskFull {
		t.Fatalf("with nothing free the refusal is %v; want CTX_DISK_FULL", err)
	}
	if !strings.Contains(typed.Message, "0 bytes are free") {
		t.Fatalf("message %q does not report the full disk it measured", typed.Message)
	}

	restore = f.s.SetFreeBytes(func(string) (uint64, bool) { return 0, false })
	err = f.s.Attribute(refused)
	restore()
	if !errors.As(err, &typed) || typed.Code != model.CodeDiskFull || !strings.Contains(typed.Message, "could not be measured") {
		t.Fatalf("with no measurement the refusal is %v; want CTX_DISK_FULL saying the measurement was unavailable", err)
	}

	if err := f.s.Attribute(nil); err != nil {
		t.Fatalf("nil is settled as %v", err)
	}
	busy := &model.Error{Code: model.CodeWorkspaceBusy, Message: "busy"}
	if err := f.s.Attribute(busy); err != busy {
		t.Fatalf("an unrelated error was rewritten: %v", err)
	}
}
