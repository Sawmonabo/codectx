package sqlite_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/Sawmonabo/codectx/internal/model"
	store "github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// contendedBusyTimeout is what both opens below are given. It is short because
// the row is about whether an open WAITS on another process's write
// transaction at all, not about how long it is willing to: a shipped-default
// 5s would make the calibrating half of this test five seconds of nothing.
const contendedBusyTimeout = 200 * time.Millisecond

// TestLazyWriterOpenDoesNotWaitOnAWriteTransaction is the storage half of "a
// process that answers and indexes comes up beside an index another process is
// already running".
//
// Failure mode it protects: a writing open verifies the schema INSIDE an
// immediate write transaction, so a process opening the cache while another
// one holds the write transaction queues behind it and is refused
// `database is busy: begin` when the busy timeout runs out. That refusal is
// what the field reported, and it is the whole of what a writing open ever
// wrote. Mutation that must fail it: in open.go, `case opts.LazyWriter: err =
// s.adoptSchema(ctx)` -> `err = s.initSchema(ctx)`.
//
// The second half calibrates the first: with the same transaction held, an
// ordinary writing open of the same cache IS refused. Without it a green run
// would only show that nothing was contending.
func TestLazyWriterOpenDoesNotWaitOnAWriteTransaction(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "codectx.db")
	opts := store.Options{BusyTimeout: contendedBusyTimeout, ReadConnections: 2}

	// The cache exists and holds a schema, which is every cache a run can be
	// writing to. (An empty one is the case adoptSchema creates, and no run
	// can be holding a transaction on a cache no writing open has finished.)
	first, err := store.Open(ctx, path, opts)
	if err != nil {
		t.Fatalf("create the cache: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close the cache: %v", err)
	}

	// The other process's run, reduced to the one thing about it that matters
	// here: a write transaction held open on the same database.
	holder, err := sql.Open("sqlite", "file:"+path+"?_txlock=immediate")
	if err != nil {
		t.Fatalf("open the holding connection: %v", err)
	}
	defer holder.Close()
	tx, err := holder.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("hold a write transaction: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS held_open(x INTEGER)`); err != nil {
		t.Fatalf("make the held transaction a writer: %v", err)
	}
	defer tx.Rollback()

	lazy := opts
	lazy.LazyWriter = true
	serving, err := store.Open(ctx, path, lazy)
	if err != nil {
		t.Fatalf("a writer-deferred open was refused while another connection held the write transaction: %v -- "+
			"the open still writes, so a server cannot come up beside a running index", err)
	}
	if err := serving.Close(); err != nil {
		t.Fatalf("close the writer-deferred store: %v", err)
	}

	blocked, err := store.Open(ctx, path, opts)
	if err == nil {
		blocked.Close()
		t.Fatal("an ordinary writing open succeeded with a write transaction held: " +
			"nothing was contending, so the row above proves nothing")
	}
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != model.CodeWorkspaceBusy {
		t.Fatalf("an ordinary writing open failed with %v, want %s: the contention this row calibrates against is not the busy refusal",
			err, model.CodeWorkspaceBusy)
	}
	t.Logf("with a write transaction held: writer-deferred open ok, ordinary open %s: %s", typed.Code, typed.Message)
}
