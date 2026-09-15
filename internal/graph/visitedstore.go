package graph

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/Sawmonabo/codectx/internal/model"
)

// This file is the walk's cumulative admitted-node set as APPEND-ONLY state.
//
// The set has to survive a page, because a resumed walk must not admit a node
// an earlier page already admitted -- report the same entity twice is exactly
// what that would do. It used to survive as the continuation spool's "visited
// section": every page wrote a FRESH spool holding the WHOLE cumulative set,
// streamed record by record out of the previous page's spool, and the next
// resume decoded that same section again to rebuild the membership summary.
// Both are O(visited) per page, so paging a walk to completion was quadratic --
// measured at 0.31 s/page over the first fifty pages of a real repository walk
// and 2.20 s/page by page 400.
//
// visitedStore replaces both halves with state that only ever GROWS:
//
//   - one new ascending RUN per page, holding only the nodes that page
//     admitted. Nothing earlier is re-copied, so the bytes a page writes are a
//     function of what it admitted, never of the walk.
//   - the membership summary is PERSISTED beside the runs and loaded on
//     resume, rather than rebuilt by decoding the set. A resume therefore
//     decodes the manifest and the filter and nothing else: O(1) in visited.
//
// The reader is unchanged, which is why the runs are appended rather than
// merged. visitedSet.warm already reads its stream for what it is -- a
// CONCATENATION of ascending runs, where a key below its predecessor starts the
// next run and restarts the join at the smallest unanswered candidate -- so a
// run boundary needs no offset recorded anywhere and the manifest stays O(1).
//
// Two things are FROZEN at creation and carried in the manifest: the filter's
// bit count and its probe count. Bits set under one size or one probe count and
// read back under another produce false NEGATIVES, and a false negative is a
// node the walk has admitted being reported absent -- a cross-page
// re-admission, the same entity on two pages. Nothing else about the filter is
// derived at read time.
//
// The store lives inside the retained walk's own state directory
// (walkretain.go), which is already named, bound, leased, adopted by rename and
// swept exactly like a spool file, so the runs and the filter need no lifecycle
// of their own.

const (
	// visitedRunsFile holds the concatenated ascending runs, one node id per
	// line. A model.NodeID is hex and cannot contain the separator, so the file
	// needs no framing.
	visitedRunsFile = "visited"
	// visitedFilterFile holds the membership summary's raw bit words, little
	// endian. It is a FIXED-SIZE file written in place: a page rewrites only
	// the words its own admissions touched, so the bytes a page writes stay a
	// function of what it admitted.
	visitedFilterFile = "visited.filter"
	// visitedManifestFile holds the frozen filter geometry and the record
	// count, replaced atomically after every run.
	visitedManifestFile = "visited.json"
)

// visitedStoreVersion is the on-disk layout version of this directory. A
// manifest written by another layout is refused rather than read under this
// one's assumptions; the traversal cursor version is bumped with it, so a token
// naming such a directory never reaches here in the first place.
const visitedStoreVersion = 1

// visitedFilterProbes is the FROZEN probe count. It is a constant rather than
// a value derived from the walk's size because the walk's size is not known
// when the filter is created and the probe count may never change afterwards:
// k = ln2 * m/n is 11 at the sixteen-bits-per-node density this filter is
// budgeted for, which puts the false-positive rate near 1 in 2 000 there.
const visitedFilterProbes = 11

// visitedManifest is the O(1) state a resume reads. It carries no per-run
// offsets on purpose: a manifest with one entry per page would make a page's
// own write grow with the page number, which is the defect this store closes,
// and the reader derives run boundaries from the key order itself.
type visitedManifest struct {
	Version int `json:"version"`
	// Records is how many node ids the runs hold in total, including any a
	// later run repeats. It is the disclosed size of the cumulative set.
	Records int64 `json:"records"`
	// FilterBits and FilterProbes are the frozen geometry.
	FilterBits   uint64 `json:"filter_bits"`
	FilterProbes uint32 `json:"filter_probes"`
}

// visitedStore is one request's handle on that state.
type visitedStore struct {
	dir     string
	f       *os.File
	w       *bufio.Writer
	filter  *visitedFilter
	records int64
}

// openVisitedStore creates the store under dir. filterBytes is the heap and
// disk the membership summary may claim -- Limits.FrontierBytes divided by
// visitedFilterBudgetShare, the same budget the per-page filter used -- and it
// is spent in full, because the geometry is frozen here and the walk's size is
// not yet known.
func openVisitedStore(dir string, filterBytes int64) (*visitedStore, error) {
	s := &visitedStore{dir: dir}
	words := filterBytes / 8
	if words > 0 {
		s.filter = newFrozenVisitedFilter(uint64(words)*64, visitedFilterProbes)
		if err := os.WriteFile(s.filterPath(), make([]byte, words*8), 0o600); err != nil {
			return nil, visitedStoreErr(err)
		}
	}
	if _, err := s.writeManifest(); err != nil {
		return nil, err
	}
	return s, s.openRuns()
}

// reopenVisitedStore loads the store an earlier page left. It reads the
// manifest and the filter -- both O(1) in the size of the walk -- and never the
// runs: the runs are read only by a membership sweep, one level at a time.
func reopenVisitedStore(dir string) (*visitedStore, error) {
	b, err := os.ReadFile(filepath.Join(dir, visitedManifestFile))
	if err != nil {
		return nil, visitedStoreErr(err)
	}
	var m visitedManifest
	if err := json.Unmarshal(b, &m); err != nil || m.Version != visitedStoreVersion ||
		m.Records < 0 || m.FilterBits%64 != 0 {
		return nil, &model.Error{Code: model.CodeStorageCorrupt,
			Message: "graph: the retained visited manifest is not readable"}
	}
	s := &visitedStore{dir: dir, records: m.Records}
	if m.FilterBits > 0 {
		raw, err := os.ReadFile(s.filterPath())
		if err != nil {
			return nil, visitedStoreErr(err)
		}
		if uint64(len(raw))*8 != m.FilterBits {
			return nil, &model.Error{Code: model.CodeStorageCorrupt,
				Message: "graph: the retained membership filter does not match its manifest"}
		}
		words := make([]uint64, len(raw)/8)
		for i := range words {
			words[i] = binary.LittleEndian.Uint64(raw[i*8:])
		}
		s.filter = &visitedFilter{bits: words, m: m.FilterBits, k: m.FilterProbes}
	}
	return s, s.openRuns()
}

func (s *visitedStore) filterPath() string { return filepath.Join(s.dir, visitedFilterFile) }
func (s *visitedStore) runsPath() string   { return filepath.Join(s.dir, visitedRunsFile) }

func (s *visitedStore) openRuns() error {
	f, err := os.OpenFile(s.runsPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return visitedStoreErr(err)
	}
	s.f, s.w = f, bufio.NewWriter(f)
	return nil
}

// appendRun writes one ascending run -- the nodes ONE leg of the walk admitted
// and nothing else -- adds them to the persisted filter, and advances the
// manifest. ids must be ascending; visitedSet.addedNodes is what produces them.
//
// The run file is flushed before this returns, because the very next thing the
// walk does is stream it: an internal link's membership sweep reads the run the
// previous link just wrote, and a buffered tail would report a node the walk
// HAS admitted as absent -- the re-admission this store exists to prevent.
//
// It returns how many BYTES this leg wrote -- the run, the filter words its
// probes dirtied and the manifest. That number is the whole point of the
// layout: it is a function of what this leg admitted and of nothing else, so a
// page of a long walk writes what a page of a short one writes. The two call
// sites hand it to the engine's memory probe, which is what a measurement test
// asserts the constant on (impactrank.go heapProbe).
func (s *visitedStore) appendRun(ids []model.NodeID) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var written int64
	for _, id := range ids {
		if _, err := s.w.WriteString(string(id)); err != nil {
			return 0, visitedStoreErr(err)
		}
		if err := s.w.WriteByte('\n'); err != nil {
			return 0, visitedStoreErr(err)
		}
		written += int64(len(id)) + 1
	}
	if err := s.w.Flush(); err != nil {
		return 0, visitedStoreErr(err)
	}
	s.records += int64(len(ids))
	filterBytes, err := s.addToFilter(ids)
	if err != nil {
		return 0, err
	}
	manifestBytes, err := s.writeManifest()
	if err != nil {
		return 0, err
	}
	return written + filterBytes + manifestBytes, nil
}

// addToFilter records ids in the persisted summary, rewriting only the WORDS
// the probes changed. A full rewrite would cost the filter's whole size on
// every page, which is constant in the page number but needlessly large; the
// dirty set is bounded by the run's own length times the probe count.
func (s *visitedStore) addToFilter(ids []model.NodeID) (int64, error) {
	if s.filter == nil {
		return 0, nil
	}
	dirty := make(map[uint64]struct{}, len(ids)*int(s.filter.k))
	for _, id := range ids {
		s.filter.addWords(id, dirty)
	}
	f, err := os.OpenFile(s.filterPath(), os.O_WRONLY, 0o600)
	if err != nil {
		return 0, visitedStoreErr(err)
	}
	var word [8]byte
	for w := range dirty {
		binary.LittleEndian.PutUint64(word[:], s.filter.bits[w])
		if _, err := f.WriteAt(word[:], int64(w)*8); err != nil {
			_ = f.Close()
			return 0, visitedStoreErr(err)
		}
	}
	if err := f.Close(); err != nil {
		return 0, visitedStoreErr(err)
	}
	return int64(len(dirty)) * 8, nil
}

// writeManifest replaces the manifest atomically: a torn one would resume a
// walk against a filter geometry that never existed, and bits probed under the
// wrong geometry produce false negatives.
func (s *visitedStore) writeManifest() (int64, error) {
	m := visitedManifest{Version: visitedStoreVersion, Records: s.records}
	if s.filter != nil {
		m.FilterBits, m.FilterProbes = s.filter.m, s.filter.k
	}
	b, err := json.Marshal(m)
	if err != nil {
		return 0, internalErr("graph: encoding the retained visited manifest: " + err.Error())
	}
	tmp := filepath.Join(s.dir, visitedManifestFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return 0, visitedStoreErr(err)
	}
	if err := os.Rename(tmp, filepath.Join(s.dir, visitedManifestFile)); err != nil {
		_ = os.Remove(tmp)
		return 0, visitedStoreErr(err)
	}
	return int64(len(b)), nil
}

// stream replays every node every run holds, in run order. It is the
// visitedStream the membership sweep merge-joins against.
func (s *visitedStore) stream(ctx context.Context, fn func(model.NodeID) error) error {
	f, err := os.Open(s.runsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return visitedStoreErr(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), model.MaxIdentifierBytes+1)
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return typedContextError(ctx, err)
		}
		if err := fn(model.NodeID(sc.Text())); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return visitedStoreErr(err)
	}
	return nil
}

func (s *visitedStore) close() error {
	if s.f == nil {
		return nil
	}
	err := s.w.Flush()
	if cerr := s.f.Close(); err == nil {
		err = cerr
	}
	s.f, s.w = nil, nil
	if err != nil {
		return visitedStoreErr(err)
	}
	return nil
}

func visitedStoreErr(err error) error {
	return internalErr("graph: the retained visited set: " + err.Error())
}
