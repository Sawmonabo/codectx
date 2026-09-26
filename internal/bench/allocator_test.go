package bench

import "testing"

// TestNativeCounterAccounting drives a private counter through every entry
// point the parser library calls and checks Live, Peak and Untracked after
// each step.
//
// Failure mode: the parse-memory rows report a live or peak figure that is
// not the bytes the library holds. The binding frees C strings it allocated
// outside the hook (see nativeCounter), so a counter that subtracts a free it
// never recorded under-reports every parse, and one that drops a realloc's old
// size or a pause-time free double-counts. Mutations that fail it: subtract a
// guessed size for an unknown pointer in forget; skip forget in realloc; stop
// consulting the table for frees while paused; let ResetPeak keep the old
// peak.
func TestNativeCounterAccounting(t *testing.T) {
	if testing.Short() {
		t.Skip("native allocator accounting; run without -short")
	}
	c := newNativeCounter()
	check := func(step string, live, peak, untracked uint64) {
		t.Helper()
		if got := c.Live(); got != live {
			t.Fatalf("%s: live = %d, want %d", step, got, live)
		}
		if got := c.Peak(); got != peak {
			t.Fatalf("%s: peak = %d, want %d", step, got, peak)
		}
		if got := c.Untracked(); got != untracked {
			t.Fatalf("%s: untracked = %d, want %d", step, got, untracked)
		}
	}

	p := c.malloc(16)
	check("malloc 16", 16, 16, 0)
	p = c.realloc(p, 48)
	check("realloc to 48", 48, 48, 0)
	q := c.calloc(4, 8)
	check("calloc 4x8", 80, 80, 0)
	c.free(q)
	check("free the calloc", 48, 80, 0)
	if got := c.ResetPeak(); got != 48 {
		t.Fatalf("ResetPeak returned %d, want the live 48", got)
	}
	check("reset peak", 48, 48, 0)

	// A block allocated outside the counter, as cgo's C.CString is.
	foreign := newNativeCounter()
	foreign.SetCounting(false)
	f := foreign.malloc(8)
	c.free(f)
	check("free a foreign pointer", 48, 48, 1)
	c.free(nil)
	check("free nil", 48, 48, 1)

	// A realloc of a foreign pointer is untracked on the way in and counted
	// on the way out.
	f = foreign.malloc(8)
	f = c.realloc(f, 24)
	check("realloc a foreign pointer", 72, 72, 2)
	c.free(f)
	check("free the adopted pointer", 48, 72, 2)

	c.SetCounting(false)
	r := c.malloc(32)
	check("paused malloc", 48, 72, 2)
	c.free(r)
	check("free of a paused allocation", 48, 72, 3)
	p = c.realloc(p, 64)
	check("paused realloc of a counted block", 0, 72, 3)
	c.free(p)
	check("free of the paused realloc's block", 0, 72, 4)
	c.SetCounting(true)

	n := c.realloc(nil, 12)
	check("realloc of nil", 12, 72, 4)
	c.free(n)
	check("free it", 0, 72, 4)
}
