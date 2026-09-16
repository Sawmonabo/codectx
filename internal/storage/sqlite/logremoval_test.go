package sqlite_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Sawmonabo/codectx/internal/paced"
	store "github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// The engine unlinks the write-ahead log from inside its own close, holding the
// database exclusively for the whole call. Freeing a large log in place there
// is seconds of held lock -- measured at 2.01 s for a 65 MiB log at the host
// pace -- and every other process on the workspace waits it out on the busy
// ladder at open, which is how a `codectx status` beside a finishing run came
// to take two seconds. So a removal under the database's directory must be a
// rename into the to-free set, never a free in place, and that set exists only
// where the directory has been registered.
//
// This asserts the structural fact rather than a duration: a process that has
// opened a database can queue a removal under its directory. Opening the store
// is the ONLY thing done here -- no unit is staged -- because the defect this
// protects against is exactly that the registration used to depend on a lexical
// staging having claimed the arena first, which a short run need never do.
//
// Mutation that fails it: remove the `scratch.For` registration from Open.
func TestOpenRegistersTheDatabaseDirectoryForPacedRemoval(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(context.Background(), filepath.Join(dir, "codectx.db"), store.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	victim := filepath.Join(dir, "codectx.db-wal.probe")
	if err := os.WriteFile(victim, []byte("log bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !paced.QueueForRemoval(victim) {
		t.Fatal("a removal under the database's directory was not queued, so the engine's own close " +
			"would free the write-ahead log in place while it holds the database exclusively")
	}
	if _, err := os.Stat(victim); !os.IsNotExist(err) {
		t.Fatalf("the queued file is still at its own path: %v", err)
	}
}
