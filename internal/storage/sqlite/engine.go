package sqlite

import (
	"database/sql"
	"github.com/Sawmonabo/codectx/internal/storage/pacedvfs"
	"os"
	"strings"
	"unsafe"

	"modernc.org/libc"
	"modernc.org/libc/sys/types"
	sqlite3 "modernc.org/sqlite/lib"
)

// statementJournalSpillBytes is the size a statement journal may reach in
// memory before the engine moves it to a file. Every ingestion call runs in
// a savepoint so that a refused batch rolls back alone, and a savepoint
// records the prior image of each page the batch touches in a statement
// journal; content-addressed rows touch a fresh page each, so a batch's
// journal is about a page per row per index, a few mebibytes for a batch of
// the store's bounds. Held in memory it costs the batch a copy of those pages
// and nothing else; as a file, the engine's default past 64 KiB, it is
// rewritten from offset zero at every batch and reaches fifty times the
// bytes a run stores. The journal is released with the savepoint, so the
// memory a run holds for it is one batch's.
const statementJournalSpillBytes = 64 << 20

// engineConfigErr records a failure to configure the engine before the first
// connection; Open reports it rather than run with the default threshold.
var engineConfigErr error

// init sets the process-wide engine options that can only be set before the
// first connection is opened: the engine initialises itself inside the first
// open and refuses configuration after that.
func init() {
	tls := libc.NewTLS()
	defer tls.Close()
	args := libc.Xmalloc(tls, types.Size_t(unsafe.Sizeof(uintptr(0))))
	if args == 0 {
		engineConfigErr = internal("engine configuration: out of memory")
		return
	}
	defer libc.Xfree(tls, args)
	rc := sqlite3.Xsqlite3_config(tls, sqlite3.SQLITE_CONFIG_STMTJRNL_SPILL,
		libc.VaList(args, int32(statementJournalSpillBytes)))
	if rc != sqlite3.SQLITE_OK {
		engineConfigErr = internal("engine configuration: statement journal threshold refused, code " + itoa(int(rc)))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// SetTempDir names the directory the engine creates its temporary files in
// for the rest of the process: the spill files of a sort that outgrows its
// cache, the statement journal of a savepoint past statementJournalSpillBytes,
// and the temporary tables of a query too large for memory. The engine's own
// default is the first writable one of the process temp directory and the
// system's, which on a host whose temp directory is a memory filesystem turns
// a sort of a large table into a memory allocation of its size. The product
// calls this once at start-up with a directory under its data directory, so
// every temporary file lives on the disk the user gave the data and is
// bounded by that disk, not by memory.
//
// The directory is a process-wide engine setting. It is set through a
// throwaway connection, before the store or any other database of the process
// is opened, and never changed afterwards: the engine reads it without a lock
// whenever it opens a temporary file.
//
// It is also where the pool of those temporaries lives. The file-system shim
// serves the engine's unnamed delete-on-close opens from this directory's
// scratch arena, so what the engine would create and unlink once per sort is
// taken and given back instead; the directory the engine is told about is the
// fallback for a process that reaches a temporary before a store is opened.
func SetTempDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return internal("engine temporary directory: " + err.Error())
	}
	if err := pacedvfs.Register(); err != nil {
		return internal(err.Error())
	}
	// The engine's temporaries come from this directory's scratch pool rather
	// than being created and unlinked one per sort. The pool is the process's,
	// as the file system is, and the first store to open names it.
	pacedvfs.Pool(dir)
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return internal("engine temporary directory: " + err.Error())
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA temp_store_directory = '` + strings.ReplaceAll(dir, `'`, `''`) + `'`); err != nil {
		return internal("engine temporary directory: " + err.Error())
	}
	return nil
}
