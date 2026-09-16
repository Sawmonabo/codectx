//go:build unix

package diskfree

import (
	"math"

	"golang.org/x/sys/unix"
)

// Available reports the bytes available to an unprivileged user under dir,
// and false when the figure could not be measured.
//
// Bavail, not Bfree: Bfree includes the reserve only root may use, so
// reporting it would tell an operator there is space this process cannot
// actually write. Both fields are widened to int64 before anything is
// computed, because their declared types differ by platform (Bavail is uint64
// on linux/amd64, int64 or uint32 elsewhere) and some filesystems report a
// negative Bavail once the reserve is over-consumed; read as unsigned that is
// a near-2^64 "free" figure. A non-positive field, or a product that would not
// round-trip through int64, is reported as unmeasured.
func Available(dir string) (uint64, bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, false
	}
	avail, size := int64(st.Bavail), int64(st.Bsize)
	if avail <= 0 || size <= 0 {
		return 0, false
	}
	if avail > math.MaxInt64/size {
		return 0, false
	}
	return uint64(avail * size), true
}
