package worker

/*
#include <stdlib.h>
#ifdef __GLIBC__
#include <malloc.h>
static int holdOneArena(void) { return mallopt(M_ARENA_MAX, 1) == 1; }
static int trimHeap(void) { malloc_trim(0); return 1; }
#else
static int holdOneArena(void) { return 0; }
static int trimHeap(void) { return 0; }
#endif
*/
import "C"

import (
	"os"
	"runtime/debug"

	"github.com/Sawmonabo/codectx/internal/residency"
)

// nativeHost holds the C heap to one arena, so that every tree allocation of
// the parse thread lands in the heap the boundary returns, and supplies the
// kernel's resident figures and resettable peak. A C library without the arena
// setting or the trim call reports the heap as not returned, so no need is
// measured from a heap that kept the previous file's pages.
//
// The boundary returns the Go heap first: the file's flat array and its
// dependence arena and lists are Go allocations sized to the file, and a
// runtime that kept their pages would serve the next file's from them, so its
// peak would not rise and its need would be censored as the C heap's would.
// debug.FreeOSMemory collects them and returns the freed pages, which the
// runtime's default on Linux advises away at once (MADV_DONTNEED), so the
// resident set falls before the base is read.
func nativeHost() host {
	held := C.holdOneArena() == 1
	return host{
		read: residency.Read,
		returnHeap: func() bool {
			debug.FreeOSMemory()
			return held && C.trimHeap() == 1
		},
		resetPeak: func() bool {
			// Writing 5 to clear_refs resets the resident peak (VmHWM) to the
			// current resident set.
			f, err := os.OpenFile("/proc/self/clear_refs", os.O_WRONLY, 0)
			if err != nil {
				return false
			}
			_, werr := f.WriteString("5")
			return f.Close() == nil && werr == nil
		},
	}
}
