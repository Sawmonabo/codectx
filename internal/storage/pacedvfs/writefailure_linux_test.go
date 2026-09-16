package pacedvfs

import (
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"unsafe"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// The requirement: a write that fails part way through has still put its
// earlier pieces in the file, and the count that decides where the next wait
// falls must say so. Those bytes are in flight to the disk whether the write
// as a whole succeeded or not, so a file system that forgot them would let a
// window and a half go unwaited for -- which is the burst this shim exists to
// prevent.
//
// The failure is the kernel's, not a stub's: with the process's file-size
// limit set to one piece, the first piece fills the file to the limit and the
// second is refused outright. The limit is then restored and the rest of the
// window written, so the wait falls where the credited piece says it does.
//
// Mutation: return rc from xWrite before the counts are credited, so a failed
// write credits nothing and the wait comes a piece late.
func TestAWriteThatFailsPartWayCreditsThePiecesThatLanded(t *testing.T) {
	if err := Register(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "partial.db")
	f, free := openThrough(t, path)
	defer free()

	const n = 2 * unixWritePiece
	buf := libc.Xmalloc(tls, uint64(Window))
	if buf == 0 {
		t.Fatal("out of memory allocating the write buffer")
	}
	defer libc.Xfree(tls, buf)
	bytes := unsafe.Slice((*byte)(ptr(buf)), Window)
	for i := range bytes {
		bytes[i] = byte(i)
	}

	before := Windows()
	withFileSizeLimit(t, unixWritePiece, func() {
		if rc := xWrite(tls, f, buf, int32(n), 0); rc == sqlite3.SQLITE_OK {
			t.Fatalf("a write of %d bytes past the file-size limit was accepted", n)
		}
	})
	if got := Windows() - before; got != 0 {
		t.Fatalf("a failed write of %d bytes issued %d waits", n, got)
	}
	// The window is complete once the piece that landed is counted with it.
	if rc := xWrite(tls, f, buf, int32(Window-unixWritePiece), unixWritePiece); rc != sqlite3.SQLITE_OK {
		t.Fatalf("the rest of the window was refused with result code %d", rc)
	}
	if got := Windows() - before; got != 1 {
		t.Fatalf("%d bytes written, of which %d before a refusal, issued %d waits, not one",
			Window, unixWritePiece, got)
	}
}

// withFileSizeLimit runs fn with the process's file-size limit set to n bytes.
// Exceeding the limit raises SIGXFSZ at the offending thread; ignoring it
// leaves the write to report the refusal as an error.
func withFileSizeLimit(t *testing.T, n uint64, fn func()) {
	t.Helper()
	signal.Ignore(syscall.SIGXFSZ)
	defer signal.Reset(syscall.SIGXFSZ)
	var was syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &was); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: n, Max: was.Max}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &was); err != nil {
			t.Fatal(err)
		}
	}()
	fn()
}
