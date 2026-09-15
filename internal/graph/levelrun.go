package graph

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// One BFS LEVEL is delivered through two states and nothing per-level is ever
// held whole in heap.
//
// COLLECTING appends every delivered edge to a raw run in scan order, resident
// while it fits the frontier byte ceiling and spilled to raw.<level> beyond it.
// The transition sorts that run by the canonical key and writes sorted.<level>
// ONCE. SERVING then reads pages straight out of that file by byte offset, so a
// page costs one seek and the records it returns.
//
// The canonical key is what makes the served order a function of the CONTENT:
// surrogates are generation-local, so a fresh index and a delta-built index of
// the same tree would order the same answer differently if the level were keyed
// on them.

// levelFilePrefix names the three files one level owns inside the retained
// directory. The level number is the suffix, so a walk's levels never collide
// and a released level takes its own files with it.
const (
	rawLevelPrefix      = "raw."
	sortedLevelPrefix   = "sorted."
	admittedLevelPrefix = "admitted."
)

func levelFileName(prefix string, level int) string { return prefix + strconv.Itoa(level) }

// levelRecord is one dedup-passed delivered edge of a level.
//
// It carries the canonical ids the sort key needs, resolved at collect time in
// batches, so no level-sized name table exists anywhere: the alternative is a
// ref -> id map built over the whole level, which is the structure the surrogate
// walk exists to remove.
type levelRecord struct {
	// Owner is the frontier state of the node whose list this entry came from.
	Owner frontierState
	// Edge is the delivered entry itself.
	Edge Edge
	// OwnerID, NodeID and RelID are the canonical ids of Edge.Owner,
	// Edge.Neighbour and Edge.Rel.
	OwnerID model.NodeID
	NodeID  model.NodeID
	RelID   model.RelationID
}

// encodeLevelRecord frames one record: uvarints for every number and every
// surrogate, length-prefixed bytes for the three canonical ids. Signed fields
// go through their unsigned two's-complement form, which round-trips exactly.
func encodeLevelRecord(r levelRecord) []byte {
	buf := make([]byte, 0, 96+len(r.Owner.Route)*binary.MaxVarintLen64)
	buf = binary.AppendUvarint(buf, uint64(int64(r.Owner.Depth)))
	buf = binary.AppendUvarint(buf, uint64(r.Owner.Cost))
	buf = binary.AppendUvarint(buf, uint64(r.Owner.Node))
	buf = binary.AppendUvarint(buf, uint64(r.Owner.Via))
	buf = binary.AppendUvarint(buf, uint64(len(r.Owner.Route)))
	for _, rel := range r.Owner.Route {
		buf = binary.AppendUvarint(buf, uint64(rel))
	}
	buf = binary.AppendUvarint(buf, uint64(r.Edge.Owner))
	buf = binary.AppendUvarint(buf, uint64(r.Edge.Neighbour))
	buf = binary.AppendUvarint(buf, uint64(r.Edge.Rel))
	buf = append(buf, byte(r.Edge.Kind))
	var outgoing byte
	if r.Edge.Outgoing {
		outgoing = 1
	}
	buf = append(buf, outgoing)
	for _, id := range []string{string(r.OwnerID), string(r.NodeID), string(r.RelID)} {
		buf = binary.AppendUvarint(buf, uint64(len(id)))
		buf = append(buf, id...)
	}
	return buf
}

// decodeLevelRecord reverses it. Damage is CTX_STORAGE_CORRUPT rather than a
// partial record: a level read short serves fewer entries than the walk
// admitted and reads as the whole answer.
func decodeLevelRecord(b []byte) (levelRecord, error) {
	d := &recordDecoder{b: b}
	var r levelRecord
	r.Owner.Depth = int(int64(d.uvarint()))
	r.Owner.Cost = int64(d.uvarint())
	r.Owner.Node = NodeRef(d.uvarint())
	r.Owner.Via = RelRef(d.uvarint())
	routeLen := d.uvarint()
	if d.err == nil {
		if routeLen > model.MaxRelationsPerPath+1 {
			return levelRecord{}, levelCorrupt("a level record carries a route longer than a servable path")
		}
		// A routeless state -- the seed level's shape -- comes back with a NIL
		// route, not an empty one: the two are the same set of relations but
		// not the same value, and a resumed frontier is compared against one
		// the walk built in heap.
		for i := uint64(0); i < routeLen; i++ {
			r.Owner.Route = append(r.Owner.Route, RelRef(d.uvarint()))
		}
	}
	r.Edge.Owner = NodeRef(d.uvarint())
	r.Edge.Neighbour = NodeRef(d.uvarint())
	r.Edge.Rel = RelRef(d.uvarint())
	r.Edge.Kind = KindCode(d.byteAt())
	r.Edge.Outgoing = d.byteAt() == 1
	r.OwnerID = model.NodeID(d.text())
	r.NodeID = model.NodeID(d.text())
	r.RelID = model.RelationID(d.text())
	if d.err != nil {
		return levelRecord{}, d.err
	}
	if len(d.b) != 0 {
		return levelRecord{}, levelCorrupt("a level record carries trailing bytes")
	}
	return r, nil
}

// recordDecoder reads the frame one field at a time and latches the first
// failure, so the decoder above reads as a field list rather than as a chain of
// error checks. Every accessor is a no-op once the latch is set.
type recordDecoder struct {
	b   []byte
	err error
}

func (d *recordDecoder) uvarint() uint64 {
	if d.err != nil {
		return 0
	}
	v, n := binary.Uvarint(d.b)
	if n <= 0 {
		d.err = levelCorrupt("a level record ends mid-number")
		return 0
	}
	d.b = d.b[n:]
	return v
}

func (d *recordDecoder) byteAt() byte {
	if d.err != nil {
		return 0
	}
	if len(d.b) == 0 {
		d.err = levelCorrupt("a level record ends mid-field")
		return 0
	}
	v := d.b[0]
	d.b = d.b[1:]
	return v
}

func (d *recordDecoder) text() string {
	n := d.uvarint()
	if d.err != nil {
		return ""
	}
	if n > uint64(len(d.b)) {
		d.err = levelCorrupt("a level record ends mid-identifier")
		return ""
	}
	v := string(d.b[:n])
	d.b = d.b[n:]
	return v
}

func levelCorrupt(what string) error {
	return &model.Error{Code: model.CodeStorageCorrupt, Message: "graph: " + what}
}

// lessLevelRecord is the canonical level order: the neighbour the answer lists,
// then the owner it was reached from, then the relation that reached it. All
// three are CONTENT-derived, so two indexes of the same tree serve one order.
func lessLevelRecord(a, b levelRecord) bool { return compareLevelRecord(a, b) < 0 }

func compareLevelRecord(a, b levelRecord) int {
	if a.NodeID != b.NodeID {
		if a.NodeID < b.NodeID {
			return -1
		}
		return 1
	}
	if a.OwnerID != b.OwnerID {
		if a.OwnerID < b.OwnerID {
			return -1
		}
		return 1
	}
	if a.RelID != b.RelID {
		if a.RelID < b.RelID {
			return -1
		}
		return 1
	}
	return 0
}

// levelCollector receives one level's records in scan order.
//
// It is resident while the encoded bytes it holds fit the frontier ceiling,
// which is what keeps a walk that answers in one page from touching the disk at
// all, and it spills the whole run to raw.<level> the moment they do not. Peak
// heap is therefore the ceiling plus one record, never the level.
type levelCollector struct {
	w     *retainedWalk
	level int
	// ceiling is the encoded-byte budget the resident run may occupy. A
	// non-positive one means "never resident": every record goes to disk.
	ceiling int64
	// resident holds the encoded records while the run fits, in scan order.
	resident [][]byte
	bytes    int64
	// raw is the spilled run, nil while resident.
	raw   *retainFile
	count int64
}

// newLevelCollector starts a fresh level.
func newLevelCollector(w *retainedWalk, level int, frontierBytes int64) *levelCollector {
	return &levelCollector{w: w, level: level, ceiling: frontierBytes}
}

// reopenLevelCollector resumes a COLLECTING level a deadline interrupted. The
// cursor carries rawBytes, the length the interrupted request had committed,
// and the run is cut back to it: anything past it is a partial frame or a
// record the resumed scan is about to deliver again from its own EdgePos.
func reopenLevelCollector(w *retainedWalk, level int, frontierBytes, rawBytes int64) (*levelCollector, error) {
	c := newLevelCollector(w, level, frontierBytes)
	c.raw = openRetainFile(w.home, levelFileName(rawLevelPrefix, level))
	if err := c.raw.truncate(rawBytes); err != nil {
		return nil, err
	}
	if err := c.raw.each(func([]byte) error {
		c.count++
		return nil
	}); err != nil {
		return nil, err
	}
	return c, nil
}

// add appends one record in scan order.
func (c *levelCollector) add(r levelRecord) error {
	enc := encodeLevelRecord(r)
	c.count++
	if c.raw != nil {
		return c.raw.append(enc)
	}
	c.resident = append(c.resident, enc)
	c.bytes += retainFrameBytes + int64(len(enc))
	if c.bytes <= c.ceiling {
		return nil
	}
	return c.spill()
}

// spill moves the resident run to raw.<level>, in scan order, and switches the
// collector to appending straight to it.
func (c *levelCollector) spill() error {
	c.raw = openRetainFile(c.w.home, levelFileName(rawLevelPrefix, c.level))
	for _, enc := range c.resident {
		if err := c.raw.append(enc); err != nil {
			return err
		}
	}
	c.resident, c.bytes = nil, 0
	return nil
}

// persist puts the whole collected run on disk and commits it. A COLLECTING
// continuation is minted from it, so the byte count it carries has to name
// bytes that are there: a resident run the cursor reported as zero would
// resume the level with everything the scan had already delivered dropped.
func (c *levelCollector) persist() error {
	if c.raw == nil {
		if err := c.spill(); err != nil {
			return err
		}
	}
	return c.raw.close()
}

// records is how many records this level holds.
func (c *levelCollector) records() int64 { return c.count }

// rawBytes is the spilled run's committed length, and 0 while the level is
// resident: a resident level persists nothing, so there is nothing to truncate
// back to.
func (c *levelCollector) rawBytes() int64 {
	if c.raw == nil {
		return 0
	}
	return c.raw.bytes
}

// finish is the level transition's sort. The records are ordered by
// lessLevelRecord -- in memory while the level is resident, through
// pagination.ExternalSort once it has spilled -- and written to sorted.<level>
// ONCE. The raw run is deleted with it: it is the input to this sort and
// nothing reads it again.
func (c *levelCollector) finish(ctx context.Context) (*sortedLevel, error) {
	out := openRetainFile(c.w.home, levelFileName(sortedLevelPrefix, c.level))
	if err := out.remove(); err != nil {
		return nil, err
	}
	if c.raw == nil {
		if err := c.finishResident(out); err != nil {
			return nil, err
		}
	} else if err := c.finishSpilled(ctx, out); err != nil {
		return nil, err
	}
	if err := out.close(); err != nil {
		return nil, err
	}
	if c.raw != nil {
		if err := c.raw.remove(); err != nil {
			return nil, err
		}
		c.raw = nil
	}
	return &sortedLevel{level: c.level, file: out, count: c.count}, nil
}

// finishResident sorts the level in heap. It holds what it already held: the
// encoded run, which is bounded by the ceiling.
func (c *levelCollector) finishResident(out *retainFile) error {
	recs := make([]levelRecord, 0, len(c.resident))
	for _, enc := range c.resident {
		r, err := decodeLevelRecord(enc)
		if err != nil {
			return err
		}
		recs = append(recs, r)
	}
	slices.SortStableFunc(recs, compareLevelRecord)
	for _, r := range recs {
		if err := out.append(encodeLevelRecord(r)); err != nil {
			return err
		}
	}
	c.resident, c.bytes = nil, 0
	return nil
}

// finishSpilled sorts the level externally: the run buffer is bounded by the
// same ceiling the resident run was, so a level that did not fit in heap is
// ordered without ever being held there.
func (c *levelCollector) finishSpilled(ctx context.Context, out *retainFile) error {
	dir, err := c.w.home.ensure()
	if err != nil {
		return err
	}
	sorter, err := pagination.NewExternalSort(filepath.Join(dir, "levelsort"),
		levelFileName("level", c.level), 0,
		func(r levelRecord) ([]byte, error) { return encodeLevelRecord(r), nil },
		decodeLevelRecord, compareLevelRecord)
	if err != nil {
		return err
	}
	defer sorter.Close()
	if c.ceiling > 0 {
		sorter.WithRunBytes(c.ceiling, func(r levelRecord) int64 {
			return retainFrameBytes + int64(len(encodeLevelRecord(r)))
		})
	}
	if err := c.raw.each(func(b []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		r, err := decodeLevelRecord(b)
		if err != nil {
			return err
		}
		return sorter.Add(r)
	}); err != nil {
		return err
	}
	run, err := sorter.Sorted()
	if err != nil {
		return err
	}
	defer run.Close()
	return run.Each(func(r levelRecord) error { return out.append(encodeLevelRecord(r)) })
}

// sortedLevel is one level in its SERVING state: the sorted run on disk, read
// by byte offset, which is what makes a page cost a seek rather than a scan.
type sortedLevel struct {
	// level is the level number this run holds. admitLevel checks it against
	// the level it was asked to commit: naming one level's admitted file after
	// another's would commit a decision under a name nothing can redo.
	level int
	file  *retainFile
	count int64
}

// openSortedLevel reopens the sorted level a continuation resumes into.
func openSortedLevel(w *retainedWalk, level int) (*sortedLevel, error) {
	f := openRetainFile(w.home, levelFileName(sortedLevelPrefix, level))
	if _, err := os.Stat(f.path()); err != nil {
		// The sorted level IS the answer being served: a missing one would
		// serve an empty page and read as the end of the level.
		return nil, levelCorrupt("the sorted level a continuation names is gone: " + err.Error())
	}
	s := &sortedLevel{level: level, file: f}
	if err := f.each(func([]byte) error {
		s.count++
		return nil
	}); err != nil {
		return nil, err
	}
	return s, nil
}

// each streams the level from byte offset from, handing fn each record and the
// offset of the NEXT one. Cutting at any reported offset and resuming there
// yields the level exactly once.
func (s *sortedLevel) each(from int64, fn func(levelRecord, int64) error) error {
	return s.file.eachFrom(from, func(b []byte, next int64) error {
		r, err := decodeLevelRecord(b)
		if err != nil {
			return err
		}
		return fn(r, next)
	})
}

// records is how many records the level holds.
func (s *sortedLevel) records() int64 { return s.count }

// release deletes the level. It is called when the level has been served
// whole; the walk never reads it again.
func (s *sortedLevel) release() error { return s.file.remove() }
