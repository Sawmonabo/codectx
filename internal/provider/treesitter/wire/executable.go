package wire

import (
	"os"
	"runtime"
	"strconv"
	"sync"
)

// Executable is the identity of the file this process is running: its
// size, its modification time and, where the platform has one, its device
// and file number. A worker states it in its hello and the parent refuses a
// worker whose file is not its own. The parent starts its workers from its
// own executable, so the same file is the same code, grammars and queries,
// and nothing has to read or hash the sources to prove it: a binary rebuilt
// or replaced while the parent runs is a different file and is refused. It
// is empty when the file cannot be read, and an empty identity matches
// nothing.
func Executable() string { return executable() }

var executable = sync.OnceValue(func() string {
	// On Linux /proc/self/exe is the file this process was started from even
	// after its path has been replaced; elsewhere the path is read once, at
	// the first call.
	path := "/proc/self/exe"
	if runtime.GOOS != "linux" {
		p, err := os.Executable()
		if err != nil {
			return ""
		}
		path = p
	}
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return strconv.FormatInt(info.Size(), 10) + " " + strconv.FormatInt(info.ModTime().UnixNano(), 10) + fileNumber(info)
})
