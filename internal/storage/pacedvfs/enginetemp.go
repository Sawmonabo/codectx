package pacedvfs

import (
	"sync"

	"github.com/Sawmonabo/codectx/internal/scratch"
)

// The engine makes its own temporary files -- a sort that does not fit in
// memory spills its runs to one, a statement that must be undoable writes its
// pre-image to another -- and deletes them when it is done. It never sees
// their names: it asks for an unnamed file with the delete-on-close flag, and
// the file system it is talking to invents a name, opens it and unlinks it on
// close.
//
// Left alone that is a create-and-free cycle per sort, of exactly the size the
// sort spilled, in the middle of a run. It is the same waste the scratch arena
// exists to remove everywhere else in the product, in the one place the
// product does not write the code.
//
// So this file system answers such an open with a surface from the arena. The
// engine gets a file that reads as empty and grows as it writes, and on close
// the surface goes back to the pool holding its bytes for the next sort to
// write over. Nothing is created and nothing is freed.
//
// The surface is empty to the engine and unchanged on the disk: the file
// system keeps the engine's own length for it and answers from that, so a
// read past what this tenant wrote is the short read a file system gives at
// the end of a file rather than the last tenant's bytes, and the engine's
// truncate back to zero is a reset of that length rather than a free.

var (
	poolOnce  sync.Once
	poolArena *scratch.Arena

	leasesMu sync.Mutex
	// leases holds the arena lease of every pooled temporary the engine has
	// open, keyed by the address of the file object the engine allocated for
	// it. It is here rather than in the file's own header because the header
	// lives in the engine's memory, which must never hold a Go pointer.
	leases = map[uintptr]*scratch.Lease{}
)

// Pool tells this file system which directory's arena to take the engine's
// temporary files from. It is the store's data directory, passed by whatever
// opens a store, and it must be called before the first connection.
//
// The first call wins, as registration does. The file system is the process's
// default and its pool is the process's too; a second store's call must not
// redirect the first store's temporaries into another data directory, which
// is what a last-one-wins pool would do to a server holding two workspaces
// open. Until a call arrives the engine's temporaries are unpooled, which is
// what a process that opens no store gets.
func Pool(dir string) {
	poolOnce.Do(func() { poolArena = scratch.For(dir) })
}

// takeTemp takes a surface for one engine temporary, or reports false when
// there is no pool or the arena cannot serve one. A failure here is never an
// error: the engine falls back to a file of its own, because a query that
// cannot open a temporary fails, and pooling is an economy, not a contract.
func takeTemp(pFile uintptr) (*scratch.Lease, bool) {
	if poolArena == nil {
		return nil, false
	}
	l, err := poolArena.Take(scratch.EngineTemp)
	if err != nil {
		return nil, false
	}
	leasesMu.Lock()
	leases[pFile] = l
	leasesMu.Unlock()
	return l, true
}

// releaseTemp gives the surface of one pooled temporary back to the pool,
// holding its bytes. It is what the engine's delete-on-close becomes.
func releaseTemp(pFile uintptr) {
	leasesMu.Lock()
	l := leases[pFile]
	delete(leases, pFile)
	leasesMu.Unlock()
	if l != nil {
		l.Release()
	}
}
