package graph

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
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

// Ruling P7 -- a deadline that lands after the walk is complete but before the
// RANKING is -- is served out of the same directory. The ranking is two
// sequential external sorts, and both are resumable:
//
//   - the runs the interrupted sort had already spilled are MOVED into this
//     directory (they spill into the engine's sort directory, which nothing
//     leases) and the next request opens a sort over them with
//     pagination.AdoptRuns instead of re-sorting the records they hold;
//   - the manifest records how many records of the retained input were already
//     Added into those runs, because eachEntry replays the WHOLE input and the
//     adopted runs already hold a prefix of it. Without that count the resumed
//     sort would add every adopted record a second time and the fold would
//     double it;
//   - pass 2's input is pass 1's OUTPUT, which the sort removes as soon as it
//     has been read, so it is retained here too. A deadline in pass 2 therefore
//     resumes pass 2 rather than re-running the fold.
//
// The manifest and the runs it names are written together, at the interruption
// and nowhere else: a manifest that named runs a later step had consumed would
// resume into a missing file, and one written without its runs would drop
// every record they held.
const (
	retainRankFile   = "rank"
	retainFoldedFile = "folded"
)

// rankProgress is that manifest. A directory with no manifest has a zero one,
// which reads as "pass 1, nothing adopted, nothing added" -- the state a walk
// that has never been ranked is in.
type rankProgress struct {
	// Pass is the pass the next request resumes INTO: 0 or 1 for the fold, 2
	// for the rank. It is explicit rather than inferred from Folded because a
	// pass-2 resume with nothing yet added has no runs and no offset to tell
	// it apart from a pass-1 resume.
	Pass int `json:"pass"`
	// Runs are the adopted runs' names WITHIN this directory. They are base
	// names, never paths: the directory is renamed when the spool store adopts
	// it, so a stored absolute path would name a file that no longer exists.
	Runs []string `json:"runs,omitempty"`
	// Added is how many records of that pass's input the runs already hold,
	// counted from the front of the input in replay order.
	Added int64 `json:"added"`
}

// rankProgress reads the manifest. A missing one is the zero value.
func (w *retainedWalk) rankProgress() (rankProgress, error) {
	b, err := os.ReadFile(filepath.Join(w.dir, retainRankFile))
	if os.IsNotExist(err) {
		return rankProgress{}, nil
	}
	if err != nil {
		return rankProgress{}, internalErr("graph: the retained ranking manifest: " + err.Error())
	}
	var p rankProgress
	if err := json.Unmarshal(b, &p); err != nil {
		return rankProgress{}, &model.Error{Code: model.CodeStorageCorrupt,
			Message: "graph: the retained ranking manifest is not readable"}
	}
	return p, nil
}

// setRankProgress replaces the manifest atomically: a torn manifest would
// resume the ranking from a state that never existed.
func (w *retainedWalk) setRankProgress(p rankProgress) error {
	b, err := json.Marshal(p)
	if err != nil {
		return internalErr("graph: encoding the retained ranking manifest: " + err.Error())
	}
	tmp := filepath.Join(w.dir, retainRankFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return internalErr("graph: the retained ranking manifest: " + err.Error())
	}
	if err := os.Rename(tmp, filepath.Join(w.dir, retainRankFile)); err != nil {
		_ = os.Remove(tmp)
		return internalErr("graph: the retained ranking manifest: " + err.Error())
	}
	return nil
}

// adoptRunFiles moves the runs a detached sort handed over into this directory
// and returns their names there. Ownership moves with them, exactly as
// pagination.Detach states: the sort that adopts them removes them.
func (w *retainedWalk) adoptRunFiles(paths []string) ([]string, error) {
	names := make([]string, 0, len(paths))
	for i, from := range paths {
		// The name is the run's INDEX, not its source name. Detach returns the
		// runs this sort adopted followed by the ones it spilled itself, in
		// that order and never reordered (pagination.AdoptRuns), so index i
		// names the same run across any number of interruptions -- and a run
		// already sitting at that name is the one being re-detached and is left
		// where it is rather than renamed onto itself.
		name := fmt.Sprintf("run-%06d", i)
		to := filepath.Join(w.dir, name)
		if from != to {
			if err := os.Rename(from, to); err != nil {
				return nil, internalErr("graph: retaining an interrupted sort run: " + err.Error())
			}
		}
		names = append(names, name)
	}
	return names, nil
}

// runPaths resolves manifest names against the directory's CURRENT location.
func (w *retainedWalk) runPaths(names []string) []string {
	paths := make([]string, 0, len(names))
	for _, name := range names {
		paths = append(paths, filepath.Join(w.dir, name))
	}
	return paths
}

// openFolded opens the retained pass-2 input for append, truncating whatever an
// abandoned earlier attempt left: the folded set is rewritten whole or not at
// all, and half of one appended to half of another is not a fold of anything.
func (w *retainedWalk) openFolded() (*retainFile, error) {
	path := filepath.Join(w.dir, retainFoldedFile)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, internalErr("graph: the retained fold output: " + err.Error())
	}
	return openRetainFile(path)
}

// eachFolded replays the retained pass-2 input, the same way eachEntry replays
// pass 1's. The file has no open writer by then, so nothing is flushed first.
func (w *retainedWalk) eachFolded(add func(impactRecord) error) error {
	f := &retainFile{path: filepath.Join(w.dir, retainFoldedFile)}
	return f.each(func(b []byte) error {
		r, err := decodeImpactRecord(b)
		if err != nil {
			return err
		}
		return add(r)
	})
}
