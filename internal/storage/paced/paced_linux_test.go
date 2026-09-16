//go:build linux

package paced

import (
	"bufio"
	"crypto/rand"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"
)

const tmpfsMagic = 0x01021994

// procIO reads one counter of the process's own I/O accounting.
func procIO(t *testing.T, field string) int64 {
	t.Helper()
	f, err := os.Open("/proc/self/io")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), ": "); ok && k == field {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			return n
		}
	}
	t.Fatalf("/proc/self/io has no %s line", field)
	return 0
}

// The requirement: a transaction the engine writes at memory speed reaches
// the disk as it is written, one window at a time, so that when the file is
// truncated afterwards almost none of it is still waiting in the page cache.
// Mutation: make xWrite never wait (compare since against a bound it cannot
// reach) and the whole 64 MiB is cancelled at the truncate.
func TestWritesReachTheDiskOneWindowAtATime(t *testing.T) {
	dir := t.TempDir()
	var fs unix.Statfs_t
	if err := unix.Statfs(dir, &fs); err != nil {
		t.Fatal(err)
	}
	if fs.Type == tmpfsMagic {
		t.Skip("the temporary directory is memory-backed; nothing is ever written back from it")
	}
	if err := Register(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "paced.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(OFF)&_pragma=synchronous(OFF)&_pragma=cache_size(-512)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE t(b BLOB)`); err != nil {
		t.Fatal(err)
	}
	const rows, rowBytes = 64, 1 << 20
	blob := make([]byte, rowBytes)
	if _, err := rand.Read(blob); err != nil {
		t.Fatal(err)
	}
	writtenBefore := procIO(t, "write_bytes")
	waitsBefore := windows.Load()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < rows; i++ {
		if _, err := tx.Exec(`INSERT INTO t VALUES(?)`, blob); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if !layoutTrusted.Load() {
		t.Fatal("the named database's descriptor did not match its path, so the wait fell back to the wrapped sync")
	}
	if windows.Load() == waitsBefore {
		t.Fatalf("no window wait was issued while writing %d MiB", rows)
	}
	cancelledBefore := procIO(t, "cancelled_write_bytes")
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	cancelled := procIO(t, "cancelled_write_bytes") - cancelledBefore
	written := procIO(t, "write_bytes") - writtenBefore
	t.Logf("written to disk %d MiB, still dirty at the truncate %d MiB", written>>20, cancelled>>20)
	if written < int64(rows*rowBytes)*3/4 {
		t.Fatalf("only %d MiB of %d reached the disk before the truncate", written>>20, rows)
	}
	// One window may be in flight and one dirty when the writer stops, plus
	// the row that straddles the last window boundary.
	if limit := int64(2*Window + rowBytes); cancelled > limit {
		t.Fatalf("%d MiB were still dirty at the truncate; the bound is %d MiB", cancelled>>20, limit>>20)
	}
}
