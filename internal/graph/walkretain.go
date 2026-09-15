package graph

import (
	"bufio"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"

	"github.com/Sawmonabo/codectx/internal/model"
)

// A walk that is split across REQUESTS cannot be ranked from what one request
// saw. Ruling P2 ranks the whole admitted set in two passes over a
// pagination.ExternalSort, and that sort is built and dropped inside one
// request; ruling P3 lets the query deadline end a page mid-walk and carry the
// frontier forward in the `f` cursor. Put together, a resumed request used to
// rank only its own leg: the cumulative visited set the cursor carries
// guarantees the earlier legs' nodes are never admitted again, so they were
// neither re-walked nor ranked, and the answer silently lost them.
//
// retainedWalk is the missing half. It is the pass-1 INPUT -- every admitted
// impactRecord and every rollup pairRecord, exactly as the walk produced them
// -- appended to a state directory that survives the request behind the same
// `f` cursor as the frontier. Each continuation reopens it, resumes the walk,
// appends what its own leg admits, and only when the walk is EXHAUSTED runs
// pass 1 and pass 2 over the WHOLE retained input. The served order is then the
// single unbounded walk's order whatever requests it was spread over.
//
// Retaining the pass-1 input rather than the sort's runs is what also makes
// ruling P7 work: a deadline that lands mid-RANK persists nothing extra,
// because the input the sort would re-read is already retained. The next
// request re-sorts from it, which is O(n log n) over spooled records and holds
// nothing more in heap than a fresh rank would.
//
// The retention mechanism is the one the shortest-path search uses for its
// external-memory scratch (pagination.AdoptDir/OpenDir): a state directory
// named, bound, leased and swept exactly like a spool file. Records are folded
// -- foldImpact and foldPair are total, commutative and associative functions
// of the records they merge (impactrank.go), never of arrival order -- so a
// record appended by leg 1 and one appended by leg 5 fold to the same answer
// whichever run buffer they land in.
//
// Appending is streaming and peak heap is unchanged: one record at a time
// through a buffered writer, and the replay decodes one record at a time into
// the sort's run buffer.

// The two files a retained pass-1 input holds. They are separate because the
// two rankings are separate sorts with different codecs, and one interleaved
// file would have to tag every record to tell them apart.
const (
	retainEntriesFile = "entries"
	retainPairsFile   = "pairs"
)

// retainedWalk is one request's handle on that directory.
type retainedWalk struct {
	dir string
	// owned marks a directory this request created and must remove itself if
	// nothing adopts it. A directory REOPENED from the store is owned by the
	// store, and is released through the consumed continuation instead --
	// removing it here would take the bytes out from under the store's own
	// accounting.
	owned   bool
	entries *retainFile
	pairs   *retainFile
}

// openRetainedWalk creates a fresh retained input under parent.
func openRetainedWalk(parent string) (*retainedWalk, error) {
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, internalErr("graph: opening the walk retention directory: " + err.Error())
	}
	dir, err := os.MkdirTemp(parent, "walkretain-")
	if err != nil {
		return nil, internalErr("graph: opening the walk retention directory: " + err.Error())
	}
	w := &retainedWalk{dir: dir, owned: true}
	if err := w.open(); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return w, nil
}

// reopenRetainedWalk reopens the input an earlier leg retained, for append. The
// directory belongs to the spool store, which is what releases it.
func reopenRetainedWalk(dir string) (*retainedWalk, error) {
	w := &retainedWalk{dir: dir}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *retainedWalk) open() error {
	var err error
	if w.entries, err = openRetainFile(filepath.Join(w.dir, retainEntriesFile)); err != nil {
		return err
	}
	w.pairs, err = openRetainFile(filepath.Join(w.dir, retainPairsFile))
	return err
}

// addEntry appends one admitted impactRecord. It is the accumulator's emit
// callback: what used to go straight into pass 1 now lands here, and pass 1 is
// fed from the replay once the walk is exhausted.
func (w *retainedWalk) addEntry(r impactRecord) error {
	b, err := encodeImpactRecord(r)
	if err != nil {
		return err
	}
	return w.entries.append(b)
}

// addPair appends one rollup pairRecord, the same way.
func (w *retainedWalk) addPair(r pairRecord) error {
	b, err := encodePairRecord(r)
	if err != nil {
		return err
	}
	return w.pairs.append(b)
}

// eachEntry replays every entity record EVERY leg of this walk appended, in
// append order, into add. Its signature is rankImpact's emit callback, so the
// ranking pass reads the whole walk without ever holding it.
func (w *retainedWalk) eachEntry(add func(impactRecord) error) error {
	return w.entries.each(func(b []byte) error {
		r, err := decodeImpactRecord(b)
		if err != nil {
			return err
		}
		return add(r)
	})
}

// eachPair is the same replay for the package rollup, and rankPairs' emit.
func (w *retainedWalk) eachPair(add func(pairRecord) error) error {
	return w.pairs.each(func(b []byte) error {
		r, err := decodePairRecord(b)
		if err != nil {
			return err
		}
		return add(r)
	})
}

// detach commits this leg's appends and hands the directory to the caller,
// which retains it under a fresh lease. The handle owns nothing afterwards, so
// the deferred discard becomes a no-op.
func (w *retainedWalk) detach() (string, error) {
	if err := w.close(); err != nil {
		return "", err
	}
	dir := w.dir
	w.dir, w.owned = "", false
	return dir, nil
}

// discard ends the handle. A directory this request created and nothing
// adopted is removed; one reopened from the store is left to the consumed
// continuation's release, and one already detached is left to whoever took it.
func (w *retainedWalk) discard() {
	_ = w.close()
	if w.owned && w.dir != "" {
		_ = os.RemoveAll(w.dir)
	}
	w.dir = ""
}

func (w *retainedWalk) close() error {
	var first error
	for _, f := range []*retainFile{w.entries, w.pairs} {
		if f == nil {
			continue
		}
		if err := f.close(); err != nil && first == nil {
			first = err
		}
	}
	w.entries, w.pairs = nil, nil
	return first
}

// retainFile is one append-only record file of that directory: length-prefixed
// frames, the same framing pagination's spools use, so a record's bytes are
// opaque to the file and no record can be mistaken for a delimiter.
type retainFile struct {
	path string
	f    *os.File
	w    *bufio.Writer
}

func openRetainFile(path string) (*retainFile, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, internalErr("graph: the retained walk input: " + err.Error())
	}
	return &retainFile{path: path, f: f, w: bufio.NewWriter(f)}, nil
}

// retainFrameBytes is the fixed frame prefix: one record's length.
const retainFrameBytes = 4

func (r *retainFile) append(record []byte) error {
	var head [retainFrameBytes]byte
	binary.BigEndian.PutUint32(head[:], uint32(len(record)))
	if _, err := r.w.Write(head[:]); err != nil {
		return internalErr("graph: the retained walk input: " + err.Error())
	}
	if _, err := r.w.Write(record); err != nil {
		return internalErr("graph: the retained walk input: " + err.Error())
	}
	return nil
}

// each replays the file from the start, one record at a time. The writer is
// flushed first, so a replay inside the request that is still appending sees
// everything it has appended.
func (r *retainFile) each(fn func([]byte) error) error {
	if r.w != nil {
		if err := r.w.Flush(); err != nil {
			return internalErr("graph: the retained walk input: " + err.Error())
		}
	}
	f, err := os.Open(r.path)
	if err != nil {
		return internalErr("graph: the retained walk input: " + err.Error())
	}
	defer f.Close()
	in := bufio.NewReader(f)
	var head [retainFrameBytes]byte
	// One reusable buffer, grown to the largest record seen: a replay's heap is
	// one record, never the retained set.
	var buf []byte
	for {
		if _, err := io.ReadFull(in, head[:]); err != nil {
			if err == io.EOF {
				return nil
			}
			return retainCorrupt(err)
		}
		n := int(binary.BigEndian.Uint32(head[:]))
		if cap(buf) < n {
			buf = make([]byte, n)
		}
		buf = buf[:n]
		if _, err := io.ReadFull(in, buf); err != nil {
			return retainCorrupt(err)
		}
		if err := fn(buf); err != nil {
			return err
		}
	}
}

func (r *retainFile) close() error {
	if r.f == nil {
		return nil
	}
	err := r.w.Flush()
	if cerr := r.f.Close(); err == nil {
		err = cerr
	}
	r.f, r.w = nil, nil
	if err != nil {
		return internalErr("graph: the retained walk input: " + err.Error())
	}
	return nil
}

// retainCorrupt reports a retained input that ends mid-frame. It is
// CTX_STORAGE_CORRUPT rather than a short answer: a truncated pass-1 input
// would rank a subset of the walk and read as the whole blast radius, which is
// the silent loss this whole file exists to remove.
func retainCorrupt(err error) error {
	return &model.Error{Code: model.CodeStorageCorrupt,
		Message: "graph: the retained walk input ends mid-record: " + err.Error()}
}
