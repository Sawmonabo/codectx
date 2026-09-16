package paced

import (
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

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

// FreedByPurpose is the bytes this process has removed for each purpose since
// it started, as measured before each removal.
//
// It does not add up to what Steps reports. Steps counts every window the
// pacer handed back, including removals no call site names -- a caller's own
// temporary file, a test's fixture -- while this accounts only the removals
// the product labels, and it accounts their length rather than their windows.
// The two answer different questions on purpose: Steps says how much freeing
// this process did, and this says what the freeing was for.
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

// RemoveFor removes one file as Remove does and attributes its length to p.
func RemoveFor(p Purpose, path string) error {
	if st, err := os.Lstat(path); err == nil && st.Mode().IsRegular() {
		attribute(p, st.Size())
	}
	return Remove(path)
}

// RemoveAllFor removes a tree as RemoveAll does and attributes the length of
// every regular file in it to p. The measurement is taken before the removal,
// so a tree that vanishes underneath it is accounted as what was there when it
// was read, which is the honest figure either way.
func RemoveAllFor(p Purpose, dir string) error {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil //nolint:nilerr // a tree that cannot be read is removed anyway
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	attribute(p, total)
	return RemoveAll(dir)
}
