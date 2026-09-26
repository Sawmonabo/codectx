package bench

// #include <stdlib.h>
import "C"

import (
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"unsafe"

	ts "github.com/tree-sitter/go-tree-sitter"
)

// nativeCounter is the counting allocator the benchmarks install in their
// own process with the parser library's process-global SetAllocator. It is
// never installed in the production parser worker: TestMain installs it only
// after the worker and ledger-arm subcommand branches have returned, and the
// codectx binary never calls installNativeCounter.
//
// # What routes through the hook
//
// Verified in the sources of the module lang.BindingModule pins and of each
// grammar module lang.All pins (paths relative to each module's root):
//
//   - The core library routes every allocation. src/lib.c:1-12 compiles the
//     whole library in one unit; src/alloc.h:25,28,31,34 define ts_malloc,
//     ts_calloc, ts_realloc and ts_free as the ts_current_* pointers with no
//     guard, and every allocating core file (alloc.c, array.h, language.c,
//     lexer.c, parser.c, query.c, stack.c, subtree.c, tree.c) uses those
//     macros. src/alloc.c:38-47 (ts_set_allocator) repoints them, and the
//     binding's init (allocator.go:35) already points them at its C
//     trampolines (allocator.c:8,10,12,14), which call back into the Go
//     functions SetAllocator stores (allocator.go:59-111). So the parse
//     stack, subtrees, the tree, lexer buffers, tree cursors and queries are
//     all counted.
//   - Grammar external scanners do NOT route. Every grammar's
//     src/tree_sitter/alloc.h:13 maps ts_malloc and friends to the
//     ts_current_* pointers only under TREE_SITTER_REUSE_ALLOCATOR, and
//     otherwise (alloc.h:35-46) straight to libc malloc/calloc/realloc/free.
//     No grammar's bindings/go/binding.go defines it (their cgo CFLAGS are
//     -std=c11 -fPIC only, binding.go:3; the typescript module's
//     bindings/go/typescript.go:3-4 and tsx.go:3-4 likewise). Hence the cpp
//     scanner's state (src/scanner.c:98,147), the rust scanner's
//     (src/scanner.c:24,26) and the python scanner's state and its indent and
//     delimiter stacks (src/scanner.c:425; src/tree_sitter/array.h:165,186,188)
//     are libc allocations the counter never sees. The javascript, typescript
//     and tsx scanners allocate nothing (their create returns NULL:
//     javascript src/scanner.c:17, typescript/src/scanner.c:3); c, go and java
//     have no external scanner; no grammar's parser.c allocates.
//   - The binding's own C strings half-route: they are allocated by cgo's
//     C.CString (libc, uncounted) and released through go_free (counted).
//     Every parse does this: readUTF8 copies each chunk the read callback
//     returns into a C string (parser.go:276) held until the parse ends and
//     freed at parser.go:328. Node.ChildByFieldName does it per call
//     (node.go:190-191), as does Query.DisableCapture (query.go:630-632).
//     Those frees arrive for pointers the counter never saw; they are
//     forwarded to libc and counted in Untracked, never subtracted from Live.
//     The input copies' size is known exactly to the caller (each returned
//     chunk plus its terminator), so the parse benchmark reports it beside
//     the counted figures instead of leaving it invisible.
//
// # Why a pointer table
//
// Live bytes are tracked exactly with a pointer→requested-size table under a
// mutex, not with malloc_usable_size. The half-routed frees above are real
// and frequent, and a usable-size query cannot tell a pointer the counter
// never saw from one it did (every live heap block has a usable size), so it
// would subtract the binding's C strings from a total they were never added
// to. A table is exact about requested sizes and about which frees are
// foreign. Its cost is one Go map entry per live counted allocation; that
// memory is Go heap and so is inside the resident-set figures the same
// benchmark rows report.
//
// # Pausing
//
// Every hooked call already crosses from C back into Go (that is how the
// binding dispatches, counter or not); the table adds a lock and a map
// operation on top. SetCounting(false) makes allocations bypass the table so
// a timing run pays what the default allocator pays. Frees and reallocs
// still consult the table so an allocation counted before the pause is
// subtracted when it goes, which keeps Live exact across a pause; an
// allocation made during the pause is untracked when it is freed, and a
// counted realloc of it records its whole new size. So a native object that
// allocates while paused — a parser, which keeps and regrows its buffers
// across parses — must not be the one a counted measurement then uses: the
// benchmarks time on separate parsers.
type nativeCounter struct {
	counting atomic.Bool

	mu        sync.Mutex
	sizes     map[uintptr]uint64
	live      uint64
	peak      uint64
	untracked uint64
}

// native is the process's counter; installNativeCounter hooks it in.
var native = newNativeCounter()

func newNativeCounter() *nativeCounter {
	c := &nativeCounter{sizes: map[uintptr]uint64{}}
	c.counting.Store(true)
	return c
}

// installNativeCounter hooks native into the parser library. Call it once,
// before any parser exists in the process: an allocation made earlier is
// untracked when it is freed.
func installNativeCounter() {
	ts.SetAllocator(native.malloc, native.calloc, native.realloc, native.free)
}

// SetCounting turns table accounting of new allocations on or off; see
// Pausing.
func (c *nativeCounter) SetCounting(on bool) { c.counting.Store(on) }

// Live is the counted bytes currently allocated.
func (c *nativeCounter) Live() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.live
}

// Peak is the highest Live since the last ResetPeak.
func (c *nativeCounter) Peak() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.peak
}

// ResetPeak makes Peak equal Live and returns it.
func (c *nativeCounter) ResetPeak() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.peak = c.live
	return c.live
}

// Untracked is the number of frees and reallocs so far of a pointer the
// counter did not record.
func (c *nativeCounter) Untracked() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.untracked
}

// record adds p of size bytes to the table. The caller holds mu.
func (c *nativeCounter) record(p unsafe.Pointer, size uint64) {
	c.sizes[uintptr(p)] = size
	c.live += size
	if c.live > c.peak {
		c.peak = c.live
	}
}

// forget removes p from the table, subtracting its size, or counts it as
// untracked when the table does not hold it. The caller holds mu.
func (c *nativeCounter) forget(p unsafe.Pointer) {
	size, ok := c.sizes[uintptr(p)]
	if !ok {
		c.untracked++
		return
	}
	delete(c.sizes, uintptr(p))
	c.live -= size
}

// exhausted fails the process the way the library's own default allocator
// does (src/alloc.c aborts), but with the Go stack: the parse cannot continue
// on a null block.
func exhausted(size uint64) {
	panic(fmt.Sprintf("native allocation of %d bytes failed", size))
}

func (c *nativeCounter) malloc(size uint) unsafe.Pointer {
	p := C.malloc(C.size_t(size))
	if p == nil {
		if size > 0 {
			exhausted(uint64(size))
		}
		return nil
	}
	if c.counting.Load() {
		c.mu.Lock()
		c.record(p, uint64(size))
		c.mu.Unlock()
	}
	return p
}

func (c *nativeCounter) calloc(num, size uint) unsafe.Pointer {
	if num != 0 && size > math.MaxUint/num {
		// The product overflows: exactly the request libc calloc refuses.
		exhausted(math.MaxUint64)
	}
	p := C.calloc(C.size_t(num), C.size_t(size))
	if p == nil {
		if num*size > 0 {
			exhausted(uint64(num * size))
		}
		return nil
	}
	if c.counting.Load() {
		c.mu.Lock()
		c.record(p, uint64(num*size))
		c.mu.Unlock()
	}
	return p
}

func (c *nativeCounter) realloc(ptr unsafe.Pointer, size uint) unsafe.Pointer {
	if ptr == nil {
		return c.malloc(size)
	}
	// The table is updated under one lock around the libc call, so no other
	// thread can be handed ptr's address, freed here, before its entry goes.
	c.mu.Lock()
	p := C.realloc(ptr, C.size_t(size))
	if p == nil && size > 0 {
		// libc left ptr allocated; the table is unchanged.
		c.mu.Unlock()
		exhausted(uint64(size))
	}
	// Either ptr moved or grew in place (p), or realloc(ptr, 0) returned
	// null having freed it.
	c.forget(ptr)
	if p != nil && c.counting.Load() {
		c.record(p, uint64(size))
	}
	c.mu.Unlock()
	return p
}

func (c *nativeCounter) free(ptr unsafe.Pointer) {
	if ptr == nil {
		return
	}
	c.mu.Lock()
	c.forget(ptr)
	c.mu.Unlock()
	C.free(ptr)
}
