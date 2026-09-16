//go:build linux

package pacedvfs

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"unsafe"

	"modernc.org/libc"
	_ "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/Sawmonabo/codectx/internal/paced"
	"github.com/Sawmonabo/codectx/internal/scratch"
)

// The requirement: the engine's own temporaries are pooled like every other
// working file the product writes.
//
// A sort that outgrows its cache spills to a file the engine creates and
// unlinks itself. The product never writes that code, so it was the one place
// left where a run still created and freed a file of exactly the size it had
// just written -- once per sort, in the middle of the run -- which is the
// burst of freeing this whole design exists to remove. A second sort must
// write over the first one's surface and free nothing.
//
// The second half is the hazard that comes with pooling: the surface still
// holds the previous sort's records past what this one wrote, so a read past
// this tenant's length must be the short read a file system gives at the end
// of a file. If it were not, the engine would sort another query's records
// into this query's answer. Both sorts' answers are checked in full for that
// reason.
//
// Mutation: drop the pooling branch from xOpen -- the engine creates and
// unlinks its own temporary again -- and the pool holds nothing.
func TestTheEnginesTemporariesAreTakenFromThePoolAndGivenBack(t *testing.T) {
	if err := Register(); err != nil {
		t.Fatal(err)
	}
	dir := enginePoolDir(t)

	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "sort.db")+
		"?_pragma=journal_mode(OFF)&_pragma=synchronous(OFF)&_pragma=cache_size(-64)&_pragma=temp_store(FILE)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t(k TEXT, b BLOB)`); err != nil {
		t.Fatal(err)
	}
	const rows = 20000
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	blob := make([]byte, 4096)
	for i := range rows {
		if _, err := tx.Exec(`INSERT INTO t VALUES(?,?)`, fmt.Sprintf("%08d", (i*7919)%rows), blob); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	sorted := func(round int) {
		t.Helper()
		r, err := db.Query(`SELECT k FROM t ORDER BY k, b`)
		if err != nil {
			t.Fatalf("sort %d: %v", round, err)
		}
		defer r.Close()
		n := 0
		for r.Next() {
			var k string
			if err := r.Scan(&k); err != nil {
				t.Fatalf("sort %d row %d: %v", round, n, err)
			}
			if want := fmt.Sprintf("%08d", n); k != want {
				t.Fatalf("sort %d returned %q at row %d, want %q: the spill read bytes this sort did not write",
					round, k, n, want)
			}
			n++
		}
		if err := r.Err(); err != nil {
			t.Fatalf("sort %d: %v", round, err)
		}
		if n != rows {
			t.Fatalf("sort %d returned %d rows of %d", round, n, rows)
		}
	}

	freed, trunc := paced.FreedBytes(), truncations.Load()
	sorted(1)
	first := pooledTemps(t, dir)
	if len(first) == 0 {
		t.Fatal("the sort left nothing in the pool: the engine created and unlinked its own temporary")
	}
	sorted(2)
	if second := pooledTemps(t, dir); len(second) != len(first) {
		t.Fatalf("the second sort left %d pooled surfaces where the first left %d: it created its own instead of writing over the pool's",
			len(second), len(first))
	}
	if got := paced.FreedBytes() - freed; got != 0 {
		t.Fatalf("two sorts gave the filesystem %d bytes back; a sort that writes over a pooled surface frees nothing", got)
	}
	if got := truncations.Load() - trunc; got != 0 {
		t.Fatalf("the file system shortened a file %d times for the sorts; a truncation of a pooled temporary is a reset, not a free", got)
	}
}

// pooledTemps names the engine-temp surfaces the arena of dir holds.
func pooledTemps(t *testing.T, dir string) []string {
	t.Helper()
	pool := filepath.Join(scratch.Dir(dir), "0", string(scratch.EngineTemp))
	entries, err := os.ReadDir(pool)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// The requirement: a pooled temporary reads as an empty file, however many
// bytes the surface under it still holds.
//
// The pool's whole economy is that nothing is freed between two tenants: the
// surface keeps the last sort's records and the next sort writes over as much
// of them as it needs. So the file system, not the disk, holds the engine's
// length for such a file: a read past what this tenant wrote is the short
// read a file system gives at the end of a file, and the size it reports is
// this tenant's. Without that, a sort would read the previous query's records
// as its own -- silently, because they are well-formed records.
//
// Mutation: answer a pooled read from the wrapped file whatever the length,
// and the second tenant reads the first tenant's bytes.
func TestAPooledTemporaryNeverServesTheBytesTheLastOneLeft(t *testing.T) {
	if err := Register(); err != nil {
		t.Fatal(err)
	}
	enginePoolDir(t)

	const n = 4096
	want := make([]byte, n)
	for i := range want {
		want[i] = byte(i%251 + 1) // never zero: a zero would hide a short read
	}
	buf := libc.Xmalloc(tls, n)
	if buf == 0 {
		t.Fatal("out of memory allocating the buffer")
	}
	defer libc.Xfree(tls, buf)
	copy(unsafe.Slice((*byte)(ptr(buf)), n), want)

	first, firstPath, closeFirst := openTempThrough(t)
	if rc := xWrite(tls, first, buf, n, 0); rc != sqlite3.SQLITE_OK {
		t.Fatalf("the first tenant's write was refused with result code %d", rc)
	}
	closeFirst()

	second, secondPath, closeSecond := openTempThrough(t)
	defer closeSecond()
	if secondPath != firstPath {
		t.Fatalf("the second temporary is %s, not the surface %s the first one left in the pool",
			filepath.Base(secondPath), filepath.Base(firstPath))
	}
	// The surface still holds the bytes -- that is the economy -- and the
	// engine must not see them.
	raw, err := os.ReadFile(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw[:min(len(raw), n)], want) {
		t.Fatal("the surface was emptied on the disk: the pool freed the space it exists to keep")
	}
	var size int64
	if rc := xFileSize(tls, second, uintptr(unsafe.Pointer(&size))); rc != sqlite3.SQLITE_OK {
		t.Fatalf("the size of the second temporary was refused with result code %d", rc)
	}
	if size != 0 {
		t.Fatalf("a temporary nothing has written reports %d bytes", size)
	}
	libc.Xmemset(tls, buf, 0, n)
	if rc := xRead(tls, second, buf, n, 0); rc != sqlite3.SQLITE_IOERR_SHORT_READ {
		t.Fatalf("reading %d bytes of an empty temporary answered with result code %d, not a short read", n, rc)
	}
	if got := unsafe.Slice((*byte)(ptr(buf)), n); !bytes.Equal(got, make([]byte, n)) {
		t.Fatal("a read past what this tenant wrote returned the last tenant's bytes")
	}

	// The engine shortens its temporary back to zero between two sorts. That
	// is a reset of what it may read, not a request for the space: the
	// surface keeps its bytes and the pool keeps the blocks.
	copy(unsafe.Slice((*byte)(ptr(buf)), n), want)
	if rc := xWrite(tls, second, buf, n, 0); rc != sqlite3.SQLITE_OK {
		t.Fatalf("the second tenant's write was refused with result code %d", rc)
	}
	if rc := xTruncate(tls, second, 0); rc != sqlite3.SQLITE_OK {
		t.Fatalf("shortening the temporary was refused with result code %d", rc)
	}
	if rc := xFileSize(tls, second, uintptr(unsafe.Pointer(&size))); rc != sqlite3.SQLITE_OK || size != 0 {
		t.Fatalf("a temporary shortened to nothing reports %d bytes (result code %d)", size, rc)
	}
	after, err := os.Stat(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() < n {
		t.Fatalf("the surface is %d bytes after the engine reset its temporary: the reset gave the space back",
			after.Size())
	}

	// A tenant that writes at its own offsets rather than in order -- a
	// temporary database's pager writes each page where that page belongs --
	// leaves everything below the offset it wrote unwritten. The surface
	// still physically holds the last tenant's bytes there, and a read of it
	// is BELOW the high-water mark, so it is served from the file rather than
	// as the end of it. Unless the gap is zeroed, the engine reads another
	// sort's records as its own: well formed, and wrong.
	if rc := xWrite(tls, second, buf, n, 2*n); rc != sqlite3.SQLITE_OK {
		t.Fatalf("a write past the tenant's own length was refused with result code %d", rc)
	}
	if rc := xFileSize(tls, second, uintptr(unsafe.Pointer(&size))); rc != sqlite3.SQLITE_OK || size != 3*n {
		t.Fatalf("after a write of %d bytes at offset %d the temporary reports %d bytes (result code %d)", n, 2*n, size, rc)
	}
	gap := libc.Xmalloc(tls, n)
	if gap == 0 {
		t.Fatal("out of memory allocating the buffer")
	}
	defer libc.Xfree(tls, gap)
	libc.Xmemset(tls, gap, 0xff, n)
	if rc := xRead(tls, second, gap, n, 0); rc != sqlite3.SQLITE_OK {
		t.Fatalf("reading below the tenant's high-water mark answered with result code %d", rc)
	}
	if got := unsafe.Slice((*byte)(ptr(gap)), n); !bytes.Equal(got, make([]byte, n)) {
		t.Fatal("a read in the gap a write past the tenant's own length left returned the last tenant's bytes, " +
			"not the zeroes a file system gives for a hole")
	}
}

// openTempThrough opens one engine temporary through the registered file
// system -- an unnamed delete-on-close file, which is the only open the engine
// makes for its own sorts -- and returns the file object, the pooled surface
// serving it and the function that closes it.
func openTempThrough(t *testing.T) (uintptr, string, func()) {
	t.Helper()
	size := (*sqlite3.Tsqlite3_vfs)(ptr(outer)).FszOsFile
	pFile := libc.Xmalloc(tls, uint64(size))
	if pFile == 0 {
		t.Fatal("out of memory allocating the file object")
	}
	flags := int32(sqlite3.SQLITE_OPEN_TEMP_JOURNAL | sqlite3.SQLITE_OPEN_DELETEONCLOSE |
		sqlite3.SQLITE_OPEN_EXCLUSIVE | sqlite3.SQLITE_OPEN_CREATE | sqlite3.SQLITE_OPEN_READWRITE)
	if rc := xOpen(tls, outer, 0, pFile, flags, 0); rc != sqlite3.SQLITE_OK {
		libc.Xfree(tls, pFile)
		t.Fatalf("the file system refused a temporary: result code %d", rc)
	}
	leasesMu.Lock()
	l := leases[pFile]
	leasesMu.Unlock()
	if l == nil {
		xClose(tls, pFile)
		libc.Xfree(tls, pFile)
		t.Fatal("the temporary was not served from the pool")
	}
	path := l.Path()
	return pFile, path, func() {
		xClose(tls, pFile)
		libc.Xfree(tls, pFile)
	}
}

// enginePoolDir is the directory every test in this package takes pooled
// temporaries from. The pool is the process's and is set once, so a test with
// a directory of its own would be served out of whichever directory was
// registered first -- and out of a directory already removed, if that test had
// finished.
var (
	poolDirOnce sync.Once
	poolDirPath string
	poolDirErr  error
)

func enginePoolDir(t *testing.T) string {
	t.Helper()
	poolDirOnce.Do(func() {
		poolDirPath, poolDirErr = os.MkdirTemp("", "pool")
		if poolDirErr == nil {
			Pool(poolDirPath)
		}
	})
	if poolDirErr != nil {
		t.Fatal(poolDirErr)
	}
	if poolArena == nil {
		t.Fatal("no pool: the file system was given no directory to take the engine's temporaries from")
	}
	return poolDirPath
}

func TestMain(m *testing.M) {
	code := m.Run()
	if poolDirPath != "" {
		_ = os.RemoveAll(poolDirPath)
	}
	os.Exit(code)
}

// TestTheGapAPooledTemporaryZeroesIsPacedLikeAnyOtherWrite protects against
// the zero-fill handing the disk the whole gap in one submission. A pager
// that writes its first page at a high offset makes a gap as large as that
// offset, so an unpaced fill is exactly the multi-window burst this file
// system exists to prevent -- and it would be invisible, because the bytes
// are still counted and the file still ends up correct.
func TestTheGapAPooledTemporaryZeroesIsPacedLikeAnyOtherWrite(t *testing.T) {
	if err := Register(); err != nil {
		t.Fatal(err)
	}
	enginePoolDir(t)
	pFile, _, closeFile := openTempThrough(t)
	defer closeFile()

	const gapWindows = 4
	n := int32(4096)
	buf := libc.Xmalloc(tls, uint64(n))
	if buf == 0 {
		t.Fatal("out of memory allocating the buffer")
	}
	defer libc.Xfree(tls, buf)
	libc.Xmemset(tls, buf, 0x5a, uint64(n))

	before := Windows()
	if rc := xWrite(tls, pFile, buf, n, gapWindows*Window); rc != sqlite3.SQLITE_OK {
		t.Fatalf("a write past the tenant's own length was refused with result code %d", rc)
	}
	if got := Windows() - before; got < gapWindows {
		t.Fatalf("zeroing a %d-window gap closed %d windows: the fill reached the disk in one submission",
			gapWindows, got)
	}
}
