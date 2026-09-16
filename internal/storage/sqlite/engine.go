package sqlite

import (
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
