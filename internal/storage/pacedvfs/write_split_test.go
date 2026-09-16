package pacedvfs

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// The requirement: a write of any length reaches the disk WHOLE. The wrapped
// file system keeps only seventeen bits of a write's length -- it masks the
// count with 0x1ffff before handing it to the kernel -- so a 128 KiB write
// would reach the file as a write of nothing and come back as "database or
// disk is full", and a longer one would be cut to its low bits. The engine's
// memory journal spills in chunks the size of the statement-journal threshold,
// which the store sets far past that limit, so the split in xWrite is the only
// thing standing between a large spill and a corrupt file.
//
// No engine workload we can drive reaches such a write: instrumenting xWrite
// through the store's own ingestion, through an activation and through a
// savepoint in both journal modes shows 4 096 bytes as the largest write the
// engine ever hands the shim, because it writes page by page. The file
// system's own contract is therefore tested at the file system's own door.
//
// Mutation: unixWritePiece = 1 << 30, so the whole length is handed over at
// once; 131 072 & 0x1ffff is zero and nothing is written.
func TestAWriteLongerThanTheWrappedLimitReachesTheFileWhole(t *testing.T) {
	if err := Register(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "whole.db")
	f, free := openThrough(t, path)
	defer free()

	// 131 072: a multiple of 128 KiB, the length the mask turns into zero.
	// 65 537 at an offset: one byte past the limit, which the mask leaves
	// intact but which the split must place with the right offset arithmetic
	// for its second piece.
	for _, w := range []struct {
		name string
		n    int
		off  int64
	}{
		{"a multiple of the wrapped limit", 128 << 10, 0},
		{"one byte past the wrapped limit, at an offset", (64 << 10) + 1, 128 << 10},
	} {
		t.Run(w.name, func(t *testing.T) {
			want := make([]byte, w.n)
			for i := range want {
				want[i] = byte(i*7 + int(w.off))
			}
			buf := libc.Xmalloc(tls, uint64(w.n))
			if buf == 0 {
				t.Fatal("out of memory allocating the write buffer")
			}
			defer libc.Xfree(tls, buf)
			copy(unsafe.Slice((*byte)(ptr(buf)), w.n), want)
			if rc := xWrite(tls, f, buf, int32(w.n), w.off); rc != sqlite3.SQLITE_OK {
				t.Fatalf("a write of %d bytes at %d was refused with result code %d", w.n, w.off, rc)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if int64(len(raw)) < w.off+int64(w.n) {
				t.Fatalf("the file holds %d bytes after a write of %d at %d: the write was cut short",
					len(raw), w.n, w.off)
			}
			if got := raw[w.off : w.off+int64(w.n)]; !bytes.Equal(got, want) {
				t.Fatalf("the %d bytes written at %d are not on disk whole", w.n, w.off)
			}
		})
	}
}

// openThrough opens path through the registered file system and returns the
// file object the shim's methods take, with the function that closes and frees
// it. It is the engine's own open sequence, which is the only way to obtain a
// file this file system's methods accept.
func openThrough(t *testing.T, path string) (uintptr, func()) {
	t.Helper()
	name, err := libc.CString(path)
	if err != nil {
		t.Fatal(err)
	}
	size := (*sqlite3.Tsqlite3_vfs)(ptr(outer)).FszOsFile
	pFile := libc.Xmalloc(tls, uint64(size))
	if pFile == 0 {
		libc.Xfree(tls, name)
		t.Fatal("out of memory allocating the file object")
	}
	flags := int32(sqlite3.SQLITE_OPEN_CREATE | sqlite3.SQLITE_OPEN_READWRITE | sqlite3.SQLITE_OPEN_MAIN_DB)
	if rc := xOpen(tls, outer, name, pFile, flags, 0); rc != sqlite3.SQLITE_OK {
		libc.Xfree(tls, pFile)
		libc.Xfree(tls, name)
		t.Fatalf("the file system refused to open %s: result code %d", path, rc)
	}
	return pFile, func() {
		xClose(tls, pFile)
		libc.Xfree(tls, pFile)
		libc.Xfree(tls, name)
	}
}
