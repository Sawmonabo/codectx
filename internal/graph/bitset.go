package graph

import (
	"container/list"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/paced"
)

// A walk's cumulative admitted-node set is a BITSET over the generation's node
// surrogates (ADR-0005 Decision 2), held in a sparse file inside the retained
// walk directory and reached through a bounded cache of pages.
//
// It replaces the append-only sorted runs, the frozen Bloom filter and the
// merge-join sweep those two needed. The filter's geometry had to be fixed
// before the walk's size was known, so past roughly m/16 admitted nodes it
// saturated and every level fell back to a full merge-join against the whole
// cumulative set -- the quadratic paging the layout exists to remove. A bitset
// over dense surrogates has no geometry to freeze, no false positives and no
// fallback: one bit per node, and only the pages a level actually touches are
// ever resident.
//
// Cost. The file is sized from GraphReader.MaxNode, so at 10^8 nodes it is
// 11.9 MiB of ADDRESS space; the filesystem allocates only the blocks that
// were written, and a walk that admits a thousand nodes in one region occupies
// one block. Heap is the page cache alone: bitsetCachePages pages of
// bitsetPageBytes each, a constant, whatever the walk reaches.
//
// Ordering. Levels test and set in ascending surrogate order, so a level walks
// the cache forward and touches each page once instead of thrashing it.
const (
	// bitsetNodeFile is the sparse NODE bit file. Bit i of byte j is surrogate
	// j*8+i, least significant bit first. Its manifest count is the answer's
	// disclosed visited_count.
	bitsetNodeFile = "visited.bits"
	// bitsetFrontierFile is the CURRENT level's frontier over node surrogates.
	// It is rebuilt from admitted.<level> at every level transition, so it
	// answers "is this neighbour on the level being scanned?" without holding
	// the level in heap -- the question the direction dedup rule asks of every
	// delivered entry.
	bitsetFrontierFile = "frontier.bits"
	// bitsetManifestSuffix names a bit file's manifest. The count is maintained
	// rather than recomputed because scanning the file for it would make an
	// O(1) report cost O(nodes).
	bitsetManifestSuffix = ".json"
)

const (
	// bitsetPageBytes is one cached page: 4 KiB, the page size the filesystem
	// reads and writes anyway, so a partial write never costs a read-modify
	// -write below this layer.
	bitsetPageBytes = 4096
	// bitsetCachePages bounds the cache at 256 pages, one mebibyte. It is an
	// INTERNAL constant and not a user limit: it bounds resident memory, never
	// the work a walk may do or the answer it returns. A walk whose level
	// touches more pages than this still admits every node; it re-reads a page
	// it evicted, which is one 4 KiB read.
	bitsetCachePages = 256
)

// bitsetManifest is the O(1) state a resumed page reads: the population count
// and nothing per-page, so a page's own write never grows with the page number.
//
// It carries no version of its own. The retained directory is reachable only
// through a signed traversal cursor, and that cursor's wire version is the one
// fence that refuses state written under another layout.
type bitsetManifest struct {
	// Count is how many DISTINCT surrogates have been admitted. It counts bit
	// transitions, not set calls, because adoption re-applies the last spooled
	// level and a re-applied bit must not be counted twice.
	Count uint64 `json:"count"`
}

// bitsetPage is one resident page and whether it has unwritten changes.
type bitsetPage struct {
	index int64
	data  []byte
	dirty bool
}

// pagedBitset is one request's handle on the set.
type pagedBitset struct {
	name string
	// home is the retained directory, which does not exist until something is
	// written into it. A set that stays inside its page cache for a whole
	// request never asks for it, so a walk that neither evicts nor detaches
	// leaves nothing on disk.
	home *dirMaker
	f    *os.File
	// limit is the largest surrogate the pinned generation carries. A ref above
	// it is a surrogate from another generation and is refused rather than
	// silently extending the file: the cursor fence exists to make that
	// impossible, and a bitset that quietly accepted one would hide the breach.
	//
	// ZERO means the set has no declared upper bound. Both sets a walk opens
	// declare one -- the reader's MaxNode -- so the range check is the FIRST
	// guard against a surrogate from another generation and the cursor's fence
	// is the second. Zero survives only for a generation that genuinely
	// carries no surrogate of that kind.
	limit uint64

	pages map[int64]*list.Element // page index -> element holding *bitsetPage
	lru   *list.List              // most recently used at the front

	admitted uint64
	// grown is how many bytes this handle has written: the pages it flushed
	// plus the manifests it replaced. It is what the walk charges to the probe,
	// and it must stay a function of what a page ADMITTED rather than of the
	// set behind it.
	grown int64
	probe *heapProbe
}

// openBitset opens the set named name in home, adopting the count an earlier
// page left when the directory is already there. maxRef sizes the address
// space; nothing is allocated for it, and no file is opened until a page is
// evicted or the set is persisted, because the file is extended lazily by the
// first write into a region.
func openBitset(home *dirMaker, name string, maxRef uint64, probe *heapProbe) (*pagedBitset, error) {
	b := &pagedBitset{
		name:  name,
		home:  home,
		limit: maxRef,
		pages: make(map[int64]*list.Element, bitsetCachePages),
		lru:   list.New(),
		probe: probe,
	}
	m, err := readBitsetManifest(home.path, name)
	if err != nil {
		return nil, err
	}
	b.admitted = m.Count
	return b, nil
}

// file opens the set's backing file, creating the retained directory with it.
// Every caller is a WRITE, or a read of a directory that already exists.
func (b *pagedBitset) file() (*os.File, error) {
	if b.f != nil {
		return b.f, nil
	}
	dir, err := b.home.ensure()
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, b.name), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, bitsetErr(err)
	}
	b.f = f
	return f, nil
}

// readBitsetManifest reads the count a previous page left. A missing manifest
// is the zero value, which is the state a set that has admitted nothing is in.
func readBitsetManifest(dir, name string) (bitsetManifest, error) {
	raw, err := os.ReadFile(filepath.Join(dir, name+bitsetManifestSuffix))
	if os.IsNotExist(err) {
		return bitsetManifest{}, nil
	}
	if err != nil {
		return bitsetManifest{}, bitsetErr(err)
	}
	var m bitsetManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return bitsetManifest{}, &model.Error{Code: model.CodeStorageCorrupt,
			Message: "graph: the retained visited bitset manifest is not readable"}
	}
	return m, nil
}

// count is the number of distinct surrogates admitted so far.
func (b *pagedBitset) count() uint64 { return b.admitted }

// test reports whether ref has been admitted. It is an I/O operation, not a
// lookup: the page holding the bit is read on a miss, so the error is returned
// rather than folded into a false, which would re-admit a node the walk has
// already served and put the same entity on two pages.
func (b *pagedBitset) test(ref uint64) (bool, error) {
	if ref == 0 {
		// Zero is never a node (ADR-0002). It has no bit, and answering "not
		// admitted" would let a caller that lost a surrogate admit a phantom.
		return false, nil
	}
	if err := b.inRange(ref); err != nil {
		return false, err
	}
	page, off, mask := b.locate(ref)
	p, err := b.page(page)
	if err != nil {
		return false, err
	}
	return p.data[off]&mask != 0, nil
}

// set admits every ref, which MUST be ascending and duplicate-free, and reports
// how many bytes the set grew by.
//
// It is IDEMPOTENT and the count follows bit TRANSITIONS: a page cut between
// spooling a level and applying its bits is resumed by re-applying that level,
// so a bit that was already set must leave the count alone. A set that counted
// calls would disclose a visited_count larger than the set it describes on
// every resumed walk.
//
// The refs are required ascending for the same reason GraphReader.Neighbours
// requires it: the cache is walked forward, so a level in order touches each
// page once. It is CHECKED rather than assumed, because an unsorted level would
// merely be slow and nothing downstream would report it.
func bitsetSet[T ~uint64](b *pagedBitset, refs []T) (int64, error) {
	if err := ascendingRefs(refs); err != nil {
		return 0, err
	}
	before := b.grown
	for _, r := range refs {
		ref := uint64(r)
		if ref == 0 {
			continue
		}
		if err := b.inRange(ref); err != nil {
			return b.grown - before, err
		}
		page, off, mask := b.locate(ref)
		p, err := b.page(page)
		if err != nil {
			return b.grown - before, err
		}
		if p.data[off]&mask != 0 {
			continue
		}
		p.data[off] |= mask
		p.dirty = true
		b.admitted++
	}
	if err := b.sync(); err != nil {
		return b.grown - before, err
	}
	grown := b.grown - before
	if b.probe != nil {
		b.probe.VisitedBytes += grown
	}
	return grown, nil
}

// sync writes every dirty page and replaces the manifest, in that order: a
// manifest that named a count whose bits were not yet on disk would resume a
// walk into a set smaller than the count it discloses.
func (b *pagedBitset) sync() error {
	// Nothing on disk and no directory to put it in: the set is resident pages
	// only, and syncing it would be the eager creation item 6 removes. The
	// pages reach disk when one is evicted or when the walk detaches, which is
	// the only moment a later request can read them.
	if b.f == nil && !b.home.exists() {
		return nil
	}
	for e := b.lru.Back(); e != nil; e = e.Prev() {
		if err := b.flush(e.Value.(*bitsetPage)); err != nil {
			return err
		}
	}
	return b.writeManifest()
}

// inRange refuses a surrogate the pinned generation does not carry.
func (b *pagedBitset) inRange(ref uint64) error {
	if b.limit == 0 || ref <= b.limit {
		return nil
	}
	return internalErr("graph: the " + b.name + " set was handed a surrogate outside the pinned generation")
}

// locate maps a surrogate to its page, the byte inside that page and its bit.
func (b *pagedBitset) locate(ref uint64) (page int64, off int, mask byte) {
	byteAt := int64(ref / 8)
	return byteAt / bitsetPageBytes, int(byteAt % bitsetPageBytes), byte(1) << (ref % 8)
}

// page returns the resident page, reading it and evicting the least recently
// used one when it is not. A read past the end of the sparse file yields zeros,
// which is exactly what an unwritten region means.
func (b *pagedBitset) page(index int64) (*bitsetPage, error) {
	if e, ok := b.pages[index]; ok {
		b.lru.MoveToFront(e)
		return e.Value.(*bitsetPage), nil
	}
	if b.lru.Len() >= bitsetCachePages {
		// Evicting DROPS the page's bytes, so it is flushed first. A dropped
		// dirty page is a node admitted in heap and never on disk, which the
		// next page re-admits.
		oldest := b.lru.Back()
		p := oldest.Value.(*bitsetPage)
		if err := b.flush(p); err != nil {
			return nil, err
		}
		b.lru.Remove(oldest)
		delete(b.pages, p.index)
	}
	p := &bitsetPage{index: index, data: make([]byte, bitsetPageBytes)}
	// A directory that does not exist holds no bits, and asking for the file
	// would create it. An unwritten region reads as zeros either way.
	if b.f != nil || b.home.exists() {
		f, err := b.file()
		if err != nil {
			return nil, err
		}
		if _, err := f.ReadAt(p.data, index*bitsetPageBytes); err != nil &&
			!errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, bitsetErr(err)
		}
	}
	b.pages[index] = b.lru.PushFront(p)
	return p, nil
}

// flush writes one dirty page in place. WriteAt extends the file, so a region
// no page has touched is never written and stays a hole.
func (b *pagedBitset) flush(p *bitsetPage) error {
	if !p.dirty {
		return nil
	}
	f, err := b.file()
	if err != nil {
		return err
	}
	n, err := f.WriteAt(p.data, p.index*bitsetPageBytes)
	b.grown += int64(n)
	if err != nil {
		return bitsetErr(err)
	}
	p.dirty = false
	return nil
}

// writeManifest replaces the count atomically: a torn manifest would resume a
// walk with a visited_count that never existed.
func (b *pagedBitset) writeManifest() error {
	raw, err := json.Marshal(bitsetManifest{Count: b.admitted})
	if err != nil {
		return internalErr("graph: encoding the retained visited bitset manifest: " + err.Error())
	}
	dir, err := b.home.ensure()
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, b.name+bitsetManifestSuffix+".tmp")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return bitsetErr(err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, b.name+bitsetManifestSuffix)); err != nil {
		_ = paced.Remove(tmp)
		return bitsetErr(err)
	}
	b.grown += int64(len(raw))
	return nil
}

// close flushes and releases the handle. The FILE outlives it: the retained
// walk directory is what the continuation carries forward.
func (b *pagedBitset) close() error {
	if b.pages == nil {
		return nil
	}
	err := b.sync()
	if b.f != nil {
		if cerr := b.f.Close(); err == nil && cerr != nil {
			err = bitsetErr(cerr)
		}
	}
	b.f, b.pages, b.lru = nil, nil, nil
	return err
}

// clear empties the set: every resident page is dropped unflushed, the count
// goes back to zero and the file behind it is truncated. It is how the FRONTIER
// set is rebuilt at a level transition -- the frontier is one level's nodes, not
// a cumulative set, so the previous level's bits must be gone before the new
// level's are set rather than merged with them.
//
// It never touches the visited set, which is cumulative by contract.
func (b *pagedBitset) clear() error {
	if b.pages == nil {
		return internalErr("graph: the " + b.name + " set was cleared after it was closed")
	}
	b.pages = make(map[int64]*list.Element, bitsetCachePages)
	b.lru = list.New()
	b.admitted = 0
	if b.f == nil && !b.home.exists() {
		return nil
	}
	f, err := b.file()
	if err != nil {
		return err
	}
	if err := f.Truncate(0); err != nil {
		return bitsetErr(err)
	}
	return b.writeManifest()
}

func bitsetErr(err error) error {
	return internalErr("graph: the retained visited bitset: " + err.Error())
}
