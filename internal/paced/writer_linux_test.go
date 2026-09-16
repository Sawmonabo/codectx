//go:build linux

package paced

import (
	"bufio"
	"crypto/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
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

// The requirement: a stream the process writes itself reaches the disk as it
// is written, one window at a time, so that when the file is truncated right
// after the write almost none of it is still waiting in the page cache.
// Mutation: make Write never wait (compare since against a bound it cannot
// reach) and the whole 64 MiB is cancelled at the truncate.
func TestAWriterHandsTheDiskOneWindowAtATime(t *testing.T) {
	dir := t.TempDir()
	var fs unix.Statfs_t
	if err := unix.Statfs(dir, &fs); err != nil {
		t.Fatal(err)
	}
	if fs.Type == tmpfsMagic {
		t.Skip("the temporary directory is memory-backed; nothing is ever written back from it")
	}
	path := filepath.Join(dir, "stream")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	const chunks, chunkBytes = 64, 1 << 20
	chunk := make([]byte, chunkBytes)
	if _, err := rand.Read(chunk); err != nil {
		t.Fatal(err)
	}
	writtenBefore := procIO(t, "write_bytes")
	waitsBefore := Waits()
	w := bufio.NewWriter(NewWriter(f))
	for i := 0; i < chunks; i++ {
		if _, err := w.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := Waits() - waitsBefore; got < chunks*chunkBytes/Window-1 {
		t.Fatalf("%d window waits for %d MiB; want one per window", got, chunks)
	}
	cancelledBefore := procIO(t, "cancelled_write_bytes")
	if err := f.Truncate(0); err != nil {
		t.Fatal(err)
	}
	f.Close()
	cancelled := procIO(t, "cancelled_write_bytes") - cancelledBefore
	written := procIO(t, "write_bytes") - writtenBefore
	t.Logf("written to disk %d MiB, still dirty at the truncate %d MiB", written>>20, cancelled>>20)
	if written < int64(chunks*chunkBytes)*3/4 {
		t.Fatalf("only %d MiB of %d reached the disk before the truncate", written>>20, chunks)
	}
	if limit := int64(2*Window + chunkBytes); cancelled > limit {
		t.Fatalf("%d MiB were still dirty at the truncate; the bound is %d MiB", cancelled>>20, limit>>20)
	}
}
