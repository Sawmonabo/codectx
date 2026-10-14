//go:build unix

package wire

import (
	"os"
	"strconv"
	"syscall"
)

// fileNumber is the file's device and inode, which tell two files apart
// whatever their size and time.
func fileNumber(info os.FileInfo) string {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return " " + strconv.FormatUint(uint64(st.Dev), 10) + ":" + strconv.FormatUint(st.Ino, 10)
}
