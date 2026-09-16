package paced

import "sync"

// A Purpose names what a removal was for. The set is small and closed on
// purpose: an operator reading the resources block should be able to tell at a
// glance that none of the labelled removals is a working file the run will
// need again.
//
// It is the set of removals this product labels, not the set it makes. A
// removal with no purpose here is not a removal that did not happen -- see
// FreedByPurpose on what Steps counts that this does not.
type Purpose string

const (
	// AnalyzerOutput is a file a foreign writer produced for this run to read
	// once -- an indexer's index, a dependence engine's export. The product
	// does not choose its layout or its length, so it cannot be written over
	// and has to be removed.
	AnalyzerOutput Purpose = "analyzer-output"
	// Materialization is a copy of snapshot files made for an analyzer to
	// read as a source tree. One unit's copy cannot be handed to another --
	// the files in it are that unit's, and an analyzer writes into its own
	// input tree -- so it is removed when the unit ends.
	Materialization Purpose = "materialization"
	// LeaseReclamation is state a run left behind for a caller that never
	// came back: a continuation's spool, a cursor's retained search. It is
	// removed by the sweep that reclaims its lease, not by the run.
	LeaseReclamation Purpose = "lease-reclamation"
	// ScratchCollection is the scratch pool itself, emptied because an
	// operator asked for it.
	ScratchCollection Purpose = "scratch-collection"
)

var (
	freedMu sync.Mutex
	freed   = map[Purpose]int64{}
)

// FreedByPurpose is the bytes this process has actually given back to the
// filesystem for each purpose since it started, counted as they were released
// rather than when the removal was asked for. Space a removal has queued and
// the reclaimer has not reached yet is not here; it is PendingFreeBytes.
//
// It does not add up to what Steps reports. Steps counts every window the
// pacer handed back, including removals no call site names -- a caller's own
// temporary file, a test's fixture -- while this accounts only the removals
// the product labels, and it accounts bytes rather than whole windows. The two
// answer different questions on purpose: Steps says how much freeing this
// process did, and this says what the freeing was for.
func FreedByPurpose() map[Purpose]int64 {
	freedMu.Lock()
	defer freedMu.Unlock()
	out := make(map[Purpose]int64, len(freed))
	for p, n := range freed {
		out[p] = n
	}
	return out
}

// attribute records n bytes freed for p.
func attribute(p Purpose, n int64) {
	if n <= 0 {
		return
	}
	freedMu.Lock()
	freed[p] += n
	freedMu.Unlock()
}

// RemoveFor removes one file or empty directory as Remove does and attributes
// the bytes it releases to p. RemoveAllFor does the same for a tree.
//
// The removal renames the path into the to-free set that serves it and
// returns; the reclaimer frees it, and attributes it, as the space actually
// goes back to the filesystem. So the purpose travels with the path -- it is
// the name of the directory the path is renamed into -- and a process that
// exits with removals still queued has not yet accounted them, because it has
// not yet freed them. The next process to claim the same set frees them and
// accounts them as its own.
func RemoveFor(p Purpose, path string) error {
	if queue(p, path) {
		return nil
	}
	return freeInPlace(p, path)
}

// RemoveAllFor removes a tree as RemoveAll does and attributes the bytes it
// releases to p. Symbolic links are removed, never followed.
func RemoveAllFor(p Purpose, dir string) error {
	if queue(p, dir) {
		return nil
	}
	return freeInPlace(p, dir)
}
