//go:build linux

package writeback_test

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/Sawmonabo/codectx/internal/writeback"
)

func procIOField(t *testing.T, field string) int64 {
	t.Helper()
	raw, err := os.ReadFile("/proc/self/io")
	if err != nil {
		t.Skipf("/proc/self/io: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if rest, ok := strings.CutPrefix(line, field+": "); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
			if err != nil {
				t.Fatalf("%s: %v", field, err)
			}
			return n
		}
	}
	t.Fatalf("/proc/self/io has no %s line", field)
	return 0
}

// TestPacerStreamsAFileToDisk writes a log's worth of pages while a pacer
// runs and shows they reached the disk before the file was truncated: the
// kernel counts a dirty page that is truncated away as a cancelled write, and
// a page the pacer had already written back is not one.
//
// Requirement: a file a pacer covers reaches the disk steadily rather than in
// one burst at a sync. Left to itself the kernel holds a dirty page for
// thirty seconds, so a writer's commit would hand the disk everything at
// once.
//
// Mutation that fails it: make the pacer's pass a no-op (every byte is
// cancelled).
func TestPacerStreamsAFileToDisk(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/store.db"
	if err := os.WriteFile(path, []byte("header"), 0o600); err != nil {
		t.Fatal(err)
	}
	log, err := os.OpenFile(path+"-wal", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()

	p := writeback.Start(path, path+"-wal")
	const chunk = 1 << 20
	const total = 64 * chunk
	buf := make([]byte, chunk)
	for i := range buf {
		buf[i] = byte(i)
	}
	before := procIOField(t, "write_bytes")
	for off := 0; off < total; off += chunk {
		if _, err := log.WriteAt(buf, int64(off)); err != nil {
			t.Fatal(err)
		}
	}
	// The pacer's last pass queues whatever the ticks had not; waiting for
	// that writeback to land is the only wait here, and it is bounded by the
	// disk's rate for 64 MiB.
	p.Stop()
	if err := unix.SyncFileRange(int(log.Fd()), 0, 0, unix.SYNC_FILE_RANGE_WAIT_AFTER); err != nil {
		t.Fatal(err)
	}
	if procIOField(t, "write_bytes")-before == 0 {
		t.Skip("the temporary directory is not on a block device, so writeback cannot be observed")
	}
	cancelledBefore := procIOField(t, "cancelled_write_bytes")
	if err := log.Truncate(0); err != nil {
		t.Fatal(err)
	}
	cancelled := procIOField(t, "cancelled_write_bytes") - cancelledBefore
	t.Logf("wrote %d MiB, %.1f MiB still dirty when truncated", total>>20, float64(cancelled)/(1<<20))
	if cancelled > total/8 {
		t.Errorf("%.1f of %d MiB were still dirty after the pacer stopped: the log is not being paced to disk",
			float64(cancelled)/(1<<20), total>>20)
	}
}
