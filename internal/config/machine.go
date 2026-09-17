package config

import (
	"math"
	"runtime"
)

// IdleFootprintBytes is what this process holds before it reserves anything:
// the Go runtime, the resolved configuration, the open store and the bounded
// buffers a command needs to answer at all. Everything else in the footprint
// is a reservation the configuration states, which is why this is the only
// part of it that has to be measured.
//
// Measured, not declared: `codectx status` over a freshly indexed five-file,
// 432-byte fixture peaked at 26,584 / 27,052 / 26,732 KiB of resident set over
// three samples ("Maximum resident set size", /usr/bin/time -v), so 26.4 MiB
// at the worst of the three, rounded up to the next binary step for run-to-run
// variation. The method and the samples are recorded in
// docs/adr/ADR-0010-engine-memory.md.
const IdleFootprintBytes int64 = 32 * (1 << 20)

// BaseFootprint is what this process holds for itself on this machine under
// configuration c: the idle overhead above, plus the three reservations it
// makes up front -- one query slot's memory per core, the parsed-graph cache
// and the indexing queue.
//
// It is derived from the machine and the configuration rather than compared
// against a figure. How many queries run at once comes from the cores, so a
// host with more cores has a larger base footprint and leaves its children a
// smaller allocation, and no core count can make the shipped defaults
// unresolvable.
func BaseFootprint(c Config) int64 {
	total, err := baseFootprintFor(c, QuerySlots())
	if err != nil {
		// Validation refuses a configuration whose base-footprint arithmetic
		// does not fit 64-bit range, so a loaded configuration never reaches
		// this. Saturating is the safe direction for anything else:
		// over-stating the footprint only leaves the children less.
		return math.MaxInt64
	}
	return total
}

// baseFootprintFor is BaseFootprint against a stated query-slot count, so the
// arithmetic can be exercised for machines this one is not and so validation
// and the figure itself come from one implementation rather than two that
// drift.
func baseFootprintFor(c Config, querySlots int) (int64, error) {
	concurrent, err := mulNoOverflow("query slots * resources.query_memory_bytes",
		int64(querySlots), c.Resources.QueryMemoryBytes)
	if err != nil {
		return 0, err
	}
	return addNoOverflow("base footprint of this process",
		IdleFootprintBytes, concurrent, c.Resources.CacheBytes, c.Index.QueueBytes)
}

// How much CPU-bound work runs at once is a property of the machine, not a
// number anyone types. Memory-bound work is admitted against the machine's one
// memory allocation; the counts here are the other half of that rule, for work
// whose scarce resource is a core rather than a byte.
//
// None of these ever refuses anything. A structural parse that finds no free
// worker waits for one, and a query that finds no free slot queues: a gate
// derived from the hardware would otherwise turn a busy moment on a small
// machine into a refusal the caller cannot act on.

// CPUs is how many cores this process may run on, never below one.
// runtime.NumCPU already honours the CPU affinity the process was started
// with, so a container given two cores of a large host reads two.
func CPUs() int {
	if n := cpuCount(); n > 0 {
		return n
	}
	return 1
}

// cpuCount is the machine observation CPUs reads, indirected so a test can
// resolve a configuration for a core count this host does not have.
var cpuCount = runtime.NumCPU

// ParserWorkers is how many structural-parse workers run at once: one per
// core. A worker is a subprocess that parses one file at a time and spends
// essentially all of its time computing, so more than one per core only adds
// context switching, and fewer leaves the machine idle on the phase that
// dominates a cold index. The kernel is left to share the cores between them
// and the rest of the machine rather than a core being held back for it: a
// reserved core is idle in the common case, where nothing else wants it.
func ParserWorkers() int { return parserWorkersFor(CPUs()) }

func parserWorkersFor(cpus int) int { return cpus }

// QuerySlots is how many tool calls run at once: one per core. A query is
// bounded work over the store rather than a subprocess, so the slot count is
// what keeps the answers from sharing one core between them; a call that finds
// no free slot waits.
func QuerySlots() int { return querySlotsFor(CPUs()) }

func querySlotsFor(cpus int) int { return cpus }

// GraphSlots is how many of those calls may be graph traversals, half the
// cores and never below one. A traversal holds a query slot as well as this
// one and does far more work per slot than a lookup, so letting every core run
// one would leave nothing answering the cheap calls an agent interleaves with
// them. A traversal that finds no free slot waits.
func GraphSlots() int { return graphSlotsFor(CPUs()) }

func graphSlotsFor(cpus int) int {
	if n := cpus / 2; n > 0 {
		return n
	}
	return 1
}
