// Package paced makes every file the embedded engine writes reach the disk
// as a self-clocked stream with a bounded window in flight.
//
// The engine writes at memory speed: a group commit appends its pages to the
// log in a second, a checkpoint copies them into the database in another,
// and a sort spills its runs as fast as they are produced. Left alone, those
// bytes sit dirty in the kernel's page cache until an fsync or the kernel's
// own flusher submits them all at once, as deep as the device queue allows.
// On a host whose disk path has a small bounce-buffer pool, or whose disk is
// slower than the burst, that submission stalls every process on the machine
// for as long as the backlog takes to drain -- measured at sixty seconds for
// a 1.8 GB commit on a virtual machine that bounces all disk traffic through
// a 64 MB pool.
//
// This package registers a file system shim as the process's default: it
// wraps the engine's own file system and, after every window of bytes
// written to a file, waits for the previous window to reach the disk and
// submits the new one. At most one window is in flight per file and at most
// two are dirty, whatever the transaction or the file size, and the writer
// runs at the disk's own rate rather than ahead of it. It is what a database
// engine's bytes-per-sync and checkpoint-flush settings do, applied to every
// write the engine makes -- log, checkpoint, journal, spill -- rather than to
// the files a caller remembers to name. Durability is unchanged: the engine's
// own syncs still decide what is durable when; they merely find the file
// already written.
//
// The window is a layout constant of this package, not a limit: it bounds
// what is outstanding, never what is written or how fast the disk is allowed
// to run.
package pacedvfs

import (
	"os"
	"path/filepath"

	"sync"
	"unsafe"

	"github.com/Sawmonabo/codectx/internal/paced"
	"modernc.org/libc"
	"modernc.org/libc/sys/types"
	sqlite3 "modernc.org/sqlite/lib"
)

// Window is the number of bytes written to one file between two waits, the
// process's one disk window.
const Window = paced.Window

// Name is the file system's registered name. Registration also makes it the
// process default, so a connection opened without naming a file system uses
// it; the name exists so a caller can ask the engine which one it got.
const Name = "paced"

// header precedes the wrapped file system's own file object inside every file
// the engine opens through this one. FpMethods must be first: the engine reads
// a file's method table from offset zero.
type header struct {
	FpMethods uintptr
	// since counts the bytes written since the last wait.
	since int64
	// fd is the descriptor the wait is issued on when the platform can wait
	// on a range of one file (mode == byRange); otherwise unused.
	fd int32
	// mode selects the wait: bySync asks the wrapped file to sync, which
	// every platform has; byRange waits on the file's own descriptor.
	mode int32
	// isLog is set for a write-ahead log, the file whose writes are the
	// store's spill signal. The engine names the file's role in the open
	// flags, so no path matching is involved.
	isLog int32
	// pooled is set for a temporary served from the scratch arena. See
	// enginetemp.go.
	pooled int32
	// logical is the length a pooled temporary has for the engine: what this
	// tenant has written, not what the surface holds. Reads past it are short
	// reads and a truncate moves it, so the previous tenant's bytes are
	// unreachable without a single byte being written over or freed.
	logical int64
	// name is the pooled surface's path, in memory the engine's allocator
	// owns: the wrapped file system keeps the pointer it was opened with, so
	// it lives as long as the file and is freed by xClose.
	name uintptr
}

const (
	bySync int32 = iota
	byRange
)

// headerSize is the offset of the wrapped file object inside ours; the
// wrapped file system expects its object aligned as its allocator would
// align it.
const headerSize = int32(unsafe.Sizeof(header{}))

var (
	once sync.Once
	tls  *libc.TLS
	// inner is the file system this one wraps: the engine's default before
	// registration. Its pointer is passed back to its own methods.
	inner uintptr
	// outer is the registered file system.
	outer uintptr
	// methods is the method table every file opened through outer carries.
	methods = sqlite3.Tsqlite3_io_methods{FiVersion: 3}
	// registerErr is the registration's failure, if any, reported by Register.
	registerErr error
)

func init() {
	methods.FxClose = fnClose(xClose)
	methods.FxRead = fnReadWrite(xRead)
	methods.FxWrite = fnReadWrite(xWrite)
	methods.FxTruncate = fnTruncate(xTruncate)
	methods.FxSync = fnInt(xSync)
	methods.FxFileSize = fnPtr(xFileSize)
	methods.FxLock = fnInt(xLock)
	methods.FxUnlock = fnInt(xUnlock)
	methods.FxCheckReservedLock = fnPtr(xCheckReservedLock)
	methods.FxFileControl = fnIntPtr(xFileControl)
	methods.FxSectorSize = fnNone(xSectorSize)
	methods.FxDeviceCharacteristics = fnNone(xDeviceCharacteristics)
	methods.FxShmMap = fnShmMap(xShmMap)
	methods.FxShmLock = fnShmLock(xShmLock)
	methods.FxShmBarrier = fnBarrier(xShmBarrier)
	methods.FxShmUnmap = fnInt(xShmUnmap)
	methods.FxFetch = fnFetch(xFetch)
	methods.FxUnfetch = fnUnfetch(xUnfetch)
}

// Register installs the paced file system as the process default. It is
// idempotent and safe to call from every package that opens a database; the
// first call does the work and every call reports its outcome. It must run
// before the first connection is opened, which is why the packages that open
// one call it from their own open paths.
func Register() error {
	once.Do(register)
	return registerErr
}

func register() {
	tls = libc.NewTLS()
	inner = sqlite3.Xsqlite3_vfs_find(tls, 0)
	if inner == 0 {
		registerErr = errNoDefault
		return
	}
	src := (*sqlite3.Tsqlite3_vfs)(ptr(inner))
	name, err := libc.CString(Name)
	if err != nil {
		registerErr = err
		return
	}
	outer = libc.Xmalloc(tls, types.Size_t(unsafe.Sizeof(sqlite3.Tsqlite3_vfs{})))
	if outer == 0 {
		libc.Xfree(tls, name)
		registerErr = errNoMemory
		return
	}
	// Every method but xOpen and xDelete is the wrapped file system's own, called with
	// our pointer: those methods read nothing from the file system object
	// that differs between the two, and pAppData is copied so the ones that
	// read it (the wrapped open's locking-style finder) find what they expect.
	dst := (*sqlite3.Tsqlite3_vfs)(ptr(outer))
	*dst = *src
	dst.FpNext = 0
	dst.FzName = name
	dst.FszOsFile = src.FszOsFile + headerSize
	dst.FxOpen = *(*uintptr)(unsafe.Pointer(&struct {
		f func(*libc.TLS, uintptr, uintptr, uintptr, int32, uintptr) int32
	}{xOpen}))
	dst.FxDelete = *(*uintptr)(unsafe.Pointer(&struct {
		f func(*libc.TLS, uintptr, uintptr, int32) int32
	}{xDelete}))
	if rc := sqlite3.Xsqlite3_vfs_register(tls, outer, 1); rc != sqlite3.SQLITE_OK {
		libc.Xfree(tls, outer)
		libc.Xfree(tls, name)
		outer = 0
		registerErr = registrationError(rc)
	}
}

// ptr turns an address the engine handed us -- memory its own allocator owns,
// never the Go heap -- back into a pointer.
func ptr(p uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&p)) }

// wrapped returns the file object the wrapped file system owns inside ours.
func wrapped(pFile uintptr) uintptr { return pFile + uintptr(headerSize) }

// innerMethods returns the wrapped file's method table.
func innerMethods(pFile uintptr) *sqlite3.Tsqlite3_io_methods {
	return (*sqlite3.Tsqlite3_io_methods)(ptr((*sqlite3.Tsqlite3_file)(ptr(wrapped(pFile))).FpMethods))
}

func xOpen(tls *libc.TLS, pVfs, zName, pFile uintptr, flags int32, pOutFlags uintptr) int32 {
	h := (*header)(ptr(pFile))
	*h = header{}
	// A file the engine did not name and asks to have unlinked on close is
	// its own temporary; it comes from the pool. See enginetemp.go.
	if zName == 0 && flags&sqlite3.SQLITE_OPEN_DELETEONCLOSE != 0 && openPooled(tls, pFile, flags, pOutFlags) {
		return sqlite3.SQLITE_OK
	}
	open := (*sqlite3.Tsqlite3_vfs)(ptr(inner)).FxOpen
	rc := (*(*func(*libc.TLS, uintptr, uintptr, uintptr, int32, uintptr) int32)(unsafe.Pointer(&struct{ uintptr }{open})))(tls, inner, zName, wrapped(pFile), flags, pOutFlags)
	if rc != sqlite3.SQLITE_OK {
		// The engine closes a file only when its method table is set; a
		// failed open leaves ours unset so the wrapped file, which set its
		// own to zero on failure, is not closed twice.
		h.FpMethods = 0
		return rc
	}
	h.FpMethods = uintptr(unsafe.Pointer(&methods))
	h.mode, h.fd = waitMode(wrapped(pFile), zName)
	if flags&sqlite3.SQLITE_OPEN_WAL != 0 {
		h.isLog = 1
	}
	return sqlite3.SQLITE_OK
}

// openPooled answers an unnamed delete-on-close open with a surface from the
// arena, and reports false when there is no pool, the arena cannot serve one,
// or the wrapped file system will not open it -- in every one of which the
// caller falls back to the engine's own temporary.
//
// Two flags go before the wrapped open: delete-on-close, because the whole
// point is that this file is not unlinked, and exclusive, because the surface
// already exists and exclusive is what asks for a file that does not.
func openPooled(tls *libc.TLS, pFile uintptr, flags int32, pOutFlags uintptr) bool {
	lease, ok := takeTemp(pFile)
	if !ok {
		return false
	}
	name, err := libc.CString(lease.Path())
	if err != nil {
		releaseTemp(pFile)
		return false
	}
	open := (*sqlite3.Tsqlite3_vfs)(ptr(inner)).FxOpen
	flags &^= sqlite3.SQLITE_OPEN_DELETEONCLOSE | sqlite3.SQLITE_OPEN_EXCLUSIVE
	rc := (*(*func(*libc.TLS, uintptr, uintptr, uintptr, int32, uintptr) int32)(unsafe.Pointer(&struct{ uintptr }{open})))(tls, inner, name, wrapped(pFile), flags, pOutFlags)
	if rc != sqlite3.SQLITE_OK {
		libc.Xfree(tls, name)
		releaseTemp(pFile)
		return false
	}
	h := (*header)(ptr(pFile))
	h.FpMethods = uintptr(unsafe.Pointer(&methods))
	h.mode, h.fd = waitMode(wrapped(pFile), name)
	h.pooled, h.logical, h.name = 1, 0, name
	return true
}

// xDelete removes a file the way paced.Remove does, through the same helper:
// the file is renamed into the to-free set that serves it and the space is
// given back afterwards, at the pace, so the log's reset and a journal's
// removal never free gigabytes at once and never hold the caller either.
//
// Holding the caller is the whole reason the rename comes first. The engine
// deletes the write-ahead log from inside its WAL close, which holds the
// database file exclusively for the length of the call: a log emptied here a
// window at a time keeps that lock for a quarter second per window, and every
// other process's first read waits it out on the busy ladder. What the engine
// waits for is the name being gone, which a rename gives it at the cost of a
// directory entry.
//
// A path no set serves has nowhere to be renamed to, so it is emptied here, a
// window at a time, before the wrapped delete unlinks it; a file no larger
// than a window is unlinked whole rather than paying an open, a truncate and
// a sync for four kilobytes. The engine also deletes speculatively, naming
// files that are not there; that is not a failure.
func xDelete(tls *libc.TLS, pVfs, zName uintptr, syncDir int32) int32 {
	if zName != 0 {
		name := libc.GoString(zName)
		// A file one window or smaller is left to the wrapped delete, which
		// unlinks it whole: renaming a four-kilobyte journal would cost a
		// directory entry and a reclaimer's turn to give back nothing. The
		// existence check is the one ShrinkForRemoval makes anyway, made here
		// so that a speculative delete of a file that is not there reaches the
		// wrapped delete and is reported exactly as it always was.
		st, sterr := os.Lstat(name)
		if sterr == nil && st.Mode().IsRegular() && st.Size() > paced.Window && paced.QueueForRemoval(name) {
			// The rename is the removal, so the durability the engine asked
			// for is about the name it deleted: this syncs the directory
			// that name was in, which is what the wrapped delete would have
			// synced. The set the file now sits in is the reclaimer's.
			if syncDir != 0 {
				if err := syncDirectoryOf(name); err != nil {
					return sqlite3.SQLITE_IOERR_DIR_FSYNC
				}
			}
			return sqlite3.SQLITE_OK
		}
		if err := paced.ShrinkForRemoval(name); err != nil {
			return sqlite3.SQLITE_IOERR_DELETE
		}
	}
	del := (*sqlite3.Tsqlite3_vfs)(ptr(inner)).FxDelete
	return (*(*func(*libc.TLS, uintptr, uintptr, int32) int32)(unsafe.Pointer(&struct{ uintptr }{del})))(tls, inner, zName, syncDir)
}

// syncDirectoryOf makes a change to the directory holding path durable.
func syncDirectoryOf(path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// xClose closes the file and, for a pooled temporary, gives the surface back
// to the arena at its length. That release is what the engine's
// delete-on-close becomes: nothing is unlinked and nothing is freed.
func xClose(tls *libc.TLS, pFile uintptr) int32 {
	m := innerMethods(pFile)
	rc := (*(*func(*libc.TLS, uintptr) int32)(unsafe.Pointer(&struct{ uintptr }{m.FxClose})))(tls, wrapped(pFile))
	h := (*header)(ptr(pFile))
	h.FpMethods = 0
	if h.pooled != 0 {
		h.pooled = 0
		if h.name != 0 {
			libc.Xfree(tls, h.name)
			h.name = 0
		}
		releaseTemp(pFile)
	}
	return rc
}

// xRead reads from the file, and from a pooled temporary reads only what this
// tenant wrote: the surface still holds the previous tenant's bytes past that
// point, and a file system reports the end of a file by filling the rest of
// the buffer with zeroes and saying the read was short. Handing back what is
// physically there instead would give the engine another sort's records as
// its own, which is the one way pooling the engine's temporaries could be
// worse than letting it create them.
func xRead(tls *libc.TLS, pFile, zBuf uintptr, iAmt int32, iOfst int64) int32 {
	m := innerMethods(pFile)
	read := *(*func(*libc.TLS, uintptr, uintptr, int32, int64) int32)(unsafe.Pointer(&struct{ uintptr }{m.FxRead}))
	h := (*header)(ptr(pFile))
	if h.pooled == 0 {
		return read(tls, wrapped(pFile), zBuf, iAmt, iOfst)
	}
	avail := min(max(h.logical-iOfst, 0), int64(iAmt))
	if avail > 0 {
		if rc := read(tls, wrapped(pFile), zBuf, int32(avail), iOfst); rc != sqlite3.SQLITE_OK {
			return rc
		}
	}
	if avail == int64(iAmt) {
		return sqlite3.SQLITE_OK
	}
	libc.Xmemset(tls, zBuf+uintptr(avail), 0, types.Size_t(int64(iAmt)-avail))
	return sqlite3.SQLITE_IOERR_SHORT_READ
}

// xWrite hands the bytes to the wrapped file and, once a window of them has
// accumulated, waits for the window before it and submits this one. The wait
// is the whole point: it is where the writer's pace becomes the disk's.
// unixWritePiece is the most the wrapped file system writes in one call: it
// keeps only seventeen bits of a write's length, so a longer write would be
// cut to its low bits and a multiple of 128 KiB would become a write of
// nothing, which the engine reports as a full disk. The engine's memory
// journal writes chunks the size of the statement-journal spill threshold
// when it spills, so a threshold past this limit reaches the file system
// only through this split.
const unixWritePiece = 1 << 16

func xWrite(tls *libc.TLS, pFile, zBuf uintptr, iAmt int32, iOfst int64) int32 {
	m := innerMethods(pFile)
	write := *(*func(*libc.TLS, uintptr, uintptr, int32, int64) int32)(unsafe.Pointer(&struct{ uintptr }{m.FxWrite}))
	// A write that fails part way through has still put its earlier pieces in
	// the file, so they are credited before the failure is returned: the log
	// count is the store's spill signal and the window count decides where the
	// next wait falls, and both would be wrong by the pieces that landed.
	h := (*header)(ptr(pFile))
	// A write past a pooled temporary's logical length leaves everything
	// between the two unwritten, and the surface still holds the previous
	// tenant's bytes there: a read in that gap is below the high-water mark,
	// so it is served from the file rather than as the end of it, and the
	// engine gets another sort's records as its own -- well formed, and
	// wrong. A pager writing its pages at their own offsets does exactly
	// this. The gap is therefore zeroed here, which is what an ordinary file
	// system gives a reader of a hole, and those bytes are real writes so
	// they are paced with the rest.
	if h.pooled != 0 && iOfst > h.logical {
		filled, rc := fillGap(tls, pFile, write, h.logical, iOfst)
		h.logical += filled
		if rc != sqlite3.SQLITE_OK {
			return rc
		}
	}
	var rc int32
	done := int32(0)
	for done < iAmt {
		n := min(iAmt-done, unixWritePiece)
		if rc = write(tls, wrapped(pFile), zBuf+uintptr(done), n, iOfst+int64(done)); rc != sqlite3.SQLITE_OK {
			break
		}
		done += n
	}
	if h.isLog != 0 {
		logBytes.Add(int64(done))
	}
	if h.pooled != 0 {
		h.logical = max(h.logical, iOfst+int64(done))
	}
	h.since += int64(done)
	if rc != sqlite3.SQLITE_OK || h.since < Window {
		return rc
	}
	h.closeWindow(tls, pFile)
	return rc
}

// closeWindow waits for the window just filled to reach the disk and submits
// it. Every place that adds to a file's byte count reaches it, so a window
// bounds what is in flight whoever wrote the bytes.
func (h *header) closeWindow(tls *libc.TLS, pFile uintptr) {
	h.since = 0
	windows.Add(1)
	if h.mode == byRange {
		if waitRange(h.fd) {
			return
		}
		// A file system without range writeback answers every call the
		// same way; fall back to the wrapped sync for the file's life.
		h.mode = bySync
	}
	// The wrapped sync waits for every dirty page of the file: with at most
	// two windows dirty that is a bounded wait, and a platform without range
	// writeback has nothing finer.
	m := innerMethods(pFile)
	(*(*func(*libc.TLS, uintptr, int32) int32)(unsafe.Pointer(&struct{ uintptr }{m.FxSync})))(tls, wrapped(pFile), sqlite3.SQLITE_SYNC_NORMAL)
}

// xTruncate shrinks a file a window at a time, syncing between steps, so
// that freeing a large log hands the filesystem one window of freed space
// at a time. Every step's bytes, the last one included, are counted and paced
// with the rest of the process's freeing: a truncation gives space back just
// as an unlink does, and the host charges for it the same way.
func xTruncate(tls *libc.TLS, pFile uintptr, size int64) int32 {
	if h := (*header)(ptr(pFile)); h.pooled != 0 {
		// A pooled temporary's length is the engine's, not the surface's: the
		// engine shortening its temporary -- back to zero between two sorts,
		// most often -- is a reset of what it may read, not a request to give
		// the disk its blocks back. Shortening the file would free exactly
		// the space the pool exists to keep.
		h.logical = min(h.logical, size)
		return sqlite3.SQLITE_OK
	}
	m := innerMethods(pFile)
	truncate := *(*func(*libc.TLS, uintptr, int64) int32)(unsafe.Pointer(&struct{ uintptr }{m.FxTruncate}))
	sync := *(*func(*libc.TLS, uintptr, int32) int32)(unsafe.Pointer(&struct{ uintptr }{m.FxSync}))
	fileSize := *(*func(*libc.TLS, uintptr, uintptr) int32)(unsafe.Pointer(&struct{ uintptr }{m.FxFileSize}))
	var cur int64
	if rc := fileSize(tls, wrapped(pFile), uintptr(unsafe.Pointer(&cur))); rc != sqlite3.SQLITE_OK {
		return rc
	}
	for cur-size > Window {
		cur -= Window
		if rc := truncate(tls, wrapped(pFile), cur); rc != sqlite3.SQLITE_OK {
			return rc
		}
		if rc := sync(tls, wrapped(pFile), sqlite3.SQLITE_SYNC_NORMAL); rc != sqlite3.SQLITE_OK {
			return rc
		}
		// The file is on the disk as far as this step goes, so the window
		// counted since the last wait is spent.
		(*header)(ptr(pFile)).since = 0
		truncations.Add(1)
		paced.Freed(Window)
	}
	rc := truncate(tls, wrapped(pFile), size)
	if rc == sqlite3.SQLITE_OK {
		paced.Freed(cur - size)
	}
	return rc
}

// fillGap writes zeroes over [from, to) and reports how many bytes landed.
// It writes in the same pieces xWrite does, because the wrapped file system
// keeps only seventeen bits of a write's length, and it closes a window as
// soon as one fills: a gap is as large as the offset the engine jumped to,
// so filling it in one submission would be exactly the burst this file
// system exists to prevent.
func fillGap(tls *libc.TLS, pFile uintptr,
	write func(*libc.TLS, uintptr, uintptr, int32, int64) int32, from, to int64) (int64, int32) {
	zeroes := libc.Xmalloc(tls, types.Size_t(unixWritePiece))
	if zeroes == 0 {
		return 0, sqlite3.SQLITE_NOMEM
	}
	defer libc.Xfree(tls, zeroes)
	libc.Xmemset(tls, zeroes, 0, types.Size_t(unixWritePiece))
	h := (*header)(ptr(pFile))
	var filled int64
	for from+filled < to {
		n := min(int64(unixWritePiece), to-from-filled)
		if rc := write(tls, wrapped(pFile), zeroes, int32(n), from+filled); rc != sqlite3.SQLITE_OK {
			return filled, rc
		}
		filled += n
		h.since += n
		if h.since >= Window {
			h.closeWindow(tls, pFile)
		}
	}
	return filled, sqlite3.SQLITE_OK
}

func xSync(tls *libc.TLS, pFile uintptr, flags int32) int32 {
	m := innerMethods(pFile)
	(*header)(ptr(pFile)).since = 0
	return (*(*func(*libc.TLS, uintptr, int32) int32)(unsafe.Pointer(&struct{ uintptr }{m.FxSync})))(tls, wrapped(pFile), flags)
}

func xFileSize(tls *libc.TLS, pFile, pSize uintptr) int32 {
	if h := (*header)(ptr(pFile)); h.pooled != 0 {
		*(*int64)(ptr(pSize)) = h.logical
		return sqlite3.SQLITE_OK
	}
	m := innerMethods(pFile)
	return (*(*func(*libc.TLS, uintptr, uintptr) int32)(unsafe.Pointer(&struct{ uintptr }{m.FxFileSize})))(tls, wrapped(pFile), pSize)
}

func xLock(tls *libc.TLS, pFile uintptr, lock int32) int32 {
	m := innerMethods(pFile)
	return (*(*func(*libc.TLS, uintptr, int32) int32)(unsafe.Pointer(&struct{ uintptr }{m.FxLock})))(tls, wrapped(pFile), lock)
}

func xUnlock(tls *libc.TLS, pFile uintptr, lock int32) int32 {
	m := innerMethods(pFile)
	return (*(*func(*libc.TLS, uintptr, int32) int32)(unsafe.Pointer(&struct{ uintptr }{m.FxUnlock})))(tls, wrapped(pFile), lock)
}

func xCheckReservedLock(tls *libc.TLS, pFile, pResOut uintptr) int32 {
	m := innerMethods(pFile)
	return (*(*func(*libc.TLS, uintptr, uintptr) int32)(unsafe.Pointer(&struct{ uintptr }{m.FxCheckReservedLock})))(tls, wrapped(pFile), pResOut)
}

func xFileControl(tls *libc.TLS, pFile uintptr, op int32, pArg uintptr) int32 {
	m := innerMethods(pFile)
	return (*(*func(*libc.TLS, uintptr, int32, uintptr) int32)(unsafe.Pointer(&struct{ uintptr }{m.FxFileControl})))(tls, wrapped(pFile), op, pArg)
}

func xSectorSize(tls *libc.TLS, pFile uintptr) int32 {
	m := innerMethods(pFile)
	return (*(*func(*libc.TLS, uintptr) int32)(unsafe.Pointer(&struct{ uintptr }{m.FxSectorSize})))(tls, wrapped(pFile))
}

func xDeviceCharacteristics(tls *libc.TLS, pFile uintptr) int32 {
	m := innerMethods(pFile)
	return (*(*func(*libc.TLS, uintptr) int32)(unsafe.Pointer(&struct{ uintptr }{m.FxDeviceCharacteristics})))(tls, wrapped(pFile))
}

func xShmMap(tls *libc.TLS, pFile uintptr, iPg, pgsz, bExtend int32, pp uintptr) int32 {
	m := innerMethods(pFile)
	return (*(*func(*libc.TLS, uintptr, int32, int32, int32, uintptr) int32)(unsafe.Pointer(&struct{ uintptr }{m.FxShmMap})))(tls, wrapped(pFile), iPg, pgsz, bExtend, pp)
}

func xShmLock(tls *libc.TLS, pFile uintptr, offset, n, flags int32) int32 {
	m := innerMethods(pFile)
	return (*(*func(*libc.TLS, uintptr, int32, int32, int32) int32)(unsafe.Pointer(&struct{ uintptr }{m.FxShmLock})))(tls, wrapped(pFile), offset, n, flags)
}

func xShmBarrier(tls *libc.TLS, pFile uintptr) {
	m := innerMethods(pFile)
	(*(*func(*libc.TLS, uintptr))(unsafe.Pointer(&struct{ uintptr }{m.FxShmBarrier})))(tls, wrapped(pFile))
}

func xShmUnmap(tls *libc.TLS, pFile uintptr, deleteFlag int32) int32 {
	m := innerMethods(pFile)
	return (*(*func(*libc.TLS, uintptr, int32) int32)(unsafe.Pointer(&struct{ uintptr }{m.FxShmUnmap})))(tls, wrapped(pFile), deleteFlag)
}

func xFetch(tls *libc.TLS, pFile uintptr, iOfst int64, iAmt int32, pp uintptr) int32 {
	m := innerMethods(pFile)
	return (*(*func(*libc.TLS, uintptr, int64, int32, uintptr) int32)(unsafe.Pointer(&struct{ uintptr }{m.FxFetch})))(tls, wrapped(pFile), iOfst, iAmt, pp)
}

func xUnfetch(tls *libc.TLS, pFile uintptr, iOfst int64, p uintptr) int32 {
	m := innerMethods(pFile)
	return (*(*func(*libc.TLS, uintptr, int64, uintptr) int32)(unsafe.Pointer(&struct{ uintptr }{m.FxUnfetch})))(tls, wrapped(pFile), iOfst, p)
}

// The engine calls a method through a function pointer it reads from the
// table; a Go function becomes one by taking the address of its value, which
// is how the engine's own Go translation calls back into Go.
func fnClose(f func(*libc.TLS, uintptr) int32) uintptr {
	return *(*uintptr)(unsafe.Pointer(&struct {
		f func(*libc.TLS, uintptr) int32
	}{f}))
}

func fnReadWrite(f func(*libc.TLS, uintptr, uintptr, int32, int64) int32) uintptr {
	return *(*uintptr)(unsafe.Pointer(&struct {
		f func(*libc.TLS, uintptr, uintptr, int32, int64) int32
	}{f}))
}

func fnTruncate(f func(*libc.TLS, uintptr, int64) int32) uintptr {
	return *(*uintptr)(unsafe.Pointer(&struct {
		f func(*libc.TLS, uintptr, int64) int32
	}{f}))
}

func fnInt(f func(*libc.TLS, uintptr, int32) int32) uintptr {
	return *(*uintptr)(unsafe.Pointer(&struct {
		f func(*libc.TLS, uintptr, int32) int32
	}{f}))
}

func fnPtr(f func(*libc.TLS, uintptr, uintptr) int32) uintptr {
	return *(*uintptr)(unsafe.Pointer(&struct {
		f func(*libc.TLS, uintptr, uintptr) int32
	}{f}))
}

func fnIntPtr(f func(*libc.TLS, uintptr, int32, uintptr) int32) uintptr {
	return *(*uintptr)(unsafe.Pointer(&struct {
		f func(*libc.TLS, uintptr, int32, uintptr) int32
	}{f}))
}

func fnNone(f func(*libc.TLS, uintptr) int32) uintptr {
	return *(*uintptr)(unsafe.Pointer(&struct {
		f func(*libc.TLS, uintptr) int32
	}{f}))
}

func fnShmMap(f func(*libc.TLS, uintptr, int32, int32, int32, uintptr) int32) uintptr {
	return *(*uintptr)(unsafe.Pointer(&struct {
		f func(*libc.TLS, uintptr, int32, int32, int32, uintptr) int32
	}{f}))
}

func fnShmLock(f func(*libc.TLS, uintptr, int32, int32, int32) int32) uintptr {
	return *(*uintptr)(unsafe.Pointer(&struct {
		f func(*libc.TLS, uintptr, int32, int32, int32) int32
	}{f}))
}

func fnBarrier(f func(*libc.TLS, uintptr)) uintptr {
	return *(*uintptr)(unsafe.Pointer(&struct{ f func(*libc.TLS, uintptr) }{f}))
}

func fnFetch(f func(*libc.TLS, uintptr, int64, int32, uintptr) int32) uintptr {
	return *(*uintptr)(unsafe.Pointer(&struct {
		f func(*libc.TLS, uintptr, int64, int32, uintptr) int32
	}{f}))
}

func fnUnfetch(f func(*libc.TLS, uintptr, int64, uintptr) int32) uintptr {
	return *(*uintptr)(unsafe.Pointer(&struct {
		f func(*libc.TLS, uintptr, int64, uintptr) int32
	}{f}))
}
