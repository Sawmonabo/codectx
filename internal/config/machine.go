package config

import "runtime"

// BaseFootprintBytes is the resident set this process keeps for itself and its
// base index. It is the one figure for that footprint in the product: the
// machine-derived allocation subtracts it from available memory before handing
// the rest to the analyzers and language servers this process starts, and
// validation checks that this process's own reservations fit inside it. Two
// figures for it meant children were admitted against memory the parent had
// already promised itself.
//
// It is a constant and not a setting, because it describes what this build
// does rather than what the operator wants. Over-stating it only leaves the
// children less, so where the measurement is uncertain the larger figure is
// the safe one.
const BaseFootprintBytes int64 = 1 << 30

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
	if n := runtime.NumCPU(); n > 0 {
		return n
	}
	return 1
}

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
