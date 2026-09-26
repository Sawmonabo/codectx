package paced

import "sync"

// A Purpose names what a removal was for. The set is small and closed on
// purpose: an operator reading the resources block should be able to tell at a
// glance that none of the labelled removals is a working file the run will
// need again.
//
// It is the set of removals this product labels, not the set it makes. A
// removal with no purpose here is not a removal that did not happen: its
// bytes are in FreedBytes like any other, they simply have no name to appear
// under in FreedByPurpose.
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
	// freed is the one counter of bytes this process has given back. Its
	// total is FreedBytes and its labelled part is FreedByPurpose, so the two
	// figures cannot disagree: they are the same additions, read two ways.
	freed      = map[Purpose]int64{}
	freedTotal int64
)

// FreedBytes is the bytes this process has actually given back to the
// filesystem since it started, counted as each truncation and each unlink
// released them, not as whole windows: a file smaller than the window is
// unlinked without a windowed step and still frees its length, and the last
// step of a shrink frees the remainder rather than a window.
//
// It is what the freeing cost, not what the removals asked for. Space a
// removal has queued and the reclaimer has not reached is not here; it is
// PendingFreeBytes.
func FreedBytes() int64 {
	freedMu.Lock()
	defer freedMu.Unlock()
	return freedTotal
}

// Freed records n bytes a caller has just released by its own truncation --
// the shrink that trims a pooled surface, the engine's shim shortening a file
// -- and makes that caller wait the pace those bytes owe before it goes on.
//
// The budget is the host's: bytes freed here and bytes freed by the
// reclaimer spend the same windows under one lock, and the processes over one
// cache take their windows in turn through a file at its root. A run cannot
// outrun the pace by splitting its freeing across several chargers, and two
// runs over one cache cannot outrun it by being two.
func Freed(n int64) {
	attribute("", n)
	reclaim.charge(n, nil)
}

// FreedByPurpose is the part of FreedBytes whose removal named a purpose,
// split by that purpose: not how much freeing this process did, which is
// FreedBytes, but what the freeing was for. It is counted as the bytes were
// released rather than when the removal was asked for, so it never exceeds
// FreedBytes and falls short of it by exactly the removals no call site
// names -- a caller's own temporary file, the engine's own shortening of a
// file it owns.
func FreedByPurpose() map[Purpose]int64 {
	freedMu.Lock()
	defer freedMu.Unlock()
	out := make(map[Purpose]int64, len(freed))
	for p, n := range freed {
		out[p] = n
	}
	return out
}

// attribute records n bytes freed, for p when the removal named one. An
// unnamed removal still counts towards the total: the figure an operator
// reads as "what this run freed" must not depend on whether the call site
// had a label for it.
func attribute(p Purpose, n int64) {
	if n <= 0 {
		return
	}
	freedMu.Lock()
	freedTotal += n
	if p != "" {
		freed[p] += n
	}
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
// QueueForRemoval renames path into the to-free set that serves it, so the
// space is given back off the caller's path, and reports whether it did.
//
// It is the form for a caller that must not wait and must not free where it
// stands: the engine's file system shim, whose delete the engine makes while
// it holds the database file. Emptying a file there at the pace holds that
// lock for as long as the file has windows, and every other process's first
// read waits it out. The rename is the whole of what such a caller needs --
// the name is gone when it returns -- and the reclaimer gives the space back
// at the same pace afterwards.
//
// A path no set serves cannot be renamed anywhere, and freeing it is then the
// caller's own.
func QueueForRemoval(path string) bool { return queue("", path) }

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
