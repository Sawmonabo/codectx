//go:build unix

package app

import (
	"math"

	"golang.org/x/sys/unix"
)

// freeDiskBytes reports the bytes available to an unprivileged user under dir.
//
// Bavail, not Bfree: Bfree includes the reserve only root may use, so reporting
// it would tell an operator there is space this process cannot actually write.
// A statfs that fails is not a measurement, so the figure is absent rather than
// zero -- a directory on a filesystem the syscall cannot describe is not a full
// disk.
//
// Both fields are widened to int64 before anything is computed, because their
// declared types differ by platform (Bavail is uint64 on linux/amd64, int64 or
// uint32 elsewhere) and some filesystems report a negative Bavail once the
// reserve is over-consumed. Read as unsigned that is a near-2^64 "free" figure,
// which is the false pass this whole function exists to avoid. A non-positive
// field, or a product that would not round-trip through int64 (model's byte
// counts are validated against that bound), is reported as absent.
func freeDiskBytes(dir string) (*uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return nil, nil
	}
	avail, size := int64(st.Bavail), int64(st.Bsize)
	if avail <= 0 || size <= 0 {
		return nil, nil
	}
	if avail > math.MaxInt64/size {
		return nil, nil
	}
	free := uint64(avail * size)
	return &free, nil
}
