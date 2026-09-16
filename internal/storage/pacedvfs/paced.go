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
	"errors"
	"io/fs"
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
	return sqlite3.SQLITE_OK
}

// xDelete removes a file the way paced.Remove does: a file larger than a
// window is shrunk a window at a time before the wrapped delete unlinks it,
// so the log's reset and a journal's removal never free gigabytes at once.
func xDelete(tls *libc.TLS, pVfs, zName uintptr, syncDir int32) int32 {
	if zName != 0 {
		if err := paced.Shrink(libc.GoString(zName), 0); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return sqlite3.SQLITE_IOERR_DELETE
		}
	}
	del := (*sqlite3.Tsqlite3_vfs)(ptr(inner)).FxDelete
	return (*(*func(*libc.TLS, uintptr, uintptr, int32) int32)(unsafe.Pointer(&struct{ uintptr }{del})))(tls, inner, zName, syncDir)
}

func xClose(tls *libc.TLS, pFile uintptr) int32 {
	m := innerMethods(pFile)
	rc := (*(*func(*libc.TLS, uintptr) int32)(unsafe.Pointer(&struct{ uintptr }{m.FxClose})))(tls, wrapped(pFile))
	(*header)(ptr(pFile)).FpMethods = 0
	return rc
}

func xRead(tls *libc.TLS, pFile, zBuf uintptr, iAmt int32, iOfst int64) int32 {
	m := innerMethods(pFile)
	return (*(*func(*libc.TLS, uintptr, uintptr, int32, int64) int32)(unsafe.Pointer(&struct{ uintptr }{m.FxRead})))(tls, wrapped(pFile), zBuf, iAmt, iOfst)
}

// xWrite hands the bytes to the wrapped file and, once a window of them has
// accumulated, waits for the window before it and submits this one. The wait
// is the whole point: it is where the writer's pace becomes the disk's.
func xWrite(tls *libc.TLS, pFile, zBuf uintptr, iAmt int32, iOfst int64) int32 {
	m := innerMethods(pFile)
	rc := (*(*func(*libc.TLS, uintptr, uintptr, int32, int64) int32)(unsafe.Pointer(&struct{ uintptr }{m.FxWrite})))(tls, wrapped(pFile), zBuf, iAmt, iOfst)
	if rc != sqlite3.SQLITE_OK {
		return rc
	}
	h := (*header)(ptr(pFile))
	h.since += int64(iAmt)
	if h.since < Window {
		return rc
	}
	h.since = 0
	windows.Add(1)
	if h.mode == byRange {
		if waitRange(h.fd) {
			return rc
		}
		// A file system without range writeback answers every call the
		// same way; fall back to the wrapped sync for the file's life.
		h.mode = bySync
	}
	// The wrapped sync waits for every dirty page of the file: with at most
	// two windows dirty that is a bounded wait, and a platform without range
	// writeback has nothing finer.
	(*(*func(*libc.TLS, uintptr, int32) int32)(unsafe.Pointer(&struct{ uintptr }{m.FxSync})))(tls, wrapped(pFile), sqlite3.SQLITE_SYNC_NORMAL)
	return rc
}

// xTruncate shrinks a file a window at a time, syncing between steps, so
// that freeing a large log hands the filesystem one window of freed space
// at a time.
func xTruncate(tls *libc.TLS, pFile uintptr, size int64) int32 {
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
		truncations.Add(1)
	}
	(*header)(ptr(pFile)).since = 0
	return truncate(tls, wrapped(pFile), size)
}

func xSync(tls *libc.TLS, pFile uintptr, flags int32) int32 {
	m := innerMethods(pFile)
	(*header)(ptr(pFile)).since = 0
	return (*(*func(*libc.TLS, uintptr, int32) int32)(unsafe.Pointer(&struct{ uintptr }{m.FxSync})))(tls, wrapped(pFile), flags)
}

func xFileSize(tls *libc.TLS, pFile, pSize uintptr) int32 {
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
