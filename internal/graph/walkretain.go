package graph

import (
	"bufio"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/paced"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// A walk that is split across REQUESTS cannot be ranked from what one request
// saw. Ruling P2 ranks the whole admitted set in two passes over a
// pagination.ExternalSort, and that sort is built and dropped inside one
// request; ruling P3 lets the query deadline end a page mid-walk and carry the
// frontier forward in the `f` cursor. A request that ranked only its own leg
// would lose every earlier leg silently: the cumulative visited set the cursor
// carries guarantees those nodes are never admitted again, so nothing re-walks
// or re-ranks them.
//
// retainedWalk is the other half. It is the pass-1 INPUT -- every admitted
// impactRecord and every rollup pairRecord, exactly as the walk produced them
// -- appended to a state directory that survives the request behind the same
// `f` cursor as the frontier. Each continuation reopens it, resumes the walk,
// appends what its own leg admits, and only when the walk is EXHAUSTED runs
// pass 1 and pass 2 over the WHOLE retained input. The served order is then the
// single unbounded walk's order whatever requests it was spread over.
//
// Retaining the pass-1 input rather than the sort's runs is what makes ruling
// P7 work: a deadline that lands mid-RANK persists nothing extra,
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

// dirMaker is the retained directory, created on the FIRST write into it.
//
// A walk whose level fits in memory, whose bitsets stay inside their page
// caches and which serves its whole answer in one page has no state to carry
// forward, so it must leave nothing behind: an empty directory would still be
// adopted, leased, charged against the spool budget and swept. Every write path
// -- a bitset page eviction, a raw, sorted or admitted level file, and the
// detach that hands the directory to a continuation -- goes through ensure, and
// nothing else creates it.
//
// The NAME is chosen up front so that every path inside the directory is
// absolute from the moment the handle exists; only the mkdir is deferred.
type dirMaker struct {
	path    string
	created bool
}

// existingDir is a directory that is already on disk: the one a continuation
// reopens, and the temporary directory a test hands a bitset.
func existingDir(path string) *dirMaker { return &dirMaker{path: path, created: true} }

// pendingDir names a directory under parent that nothing has created yet.
func pendingDir(parent string) (*dirMaker, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, internalErr("graph: naming the walk retention directory: " + err.Error())
	}
	return &dirMaker{path: filepath.Join(parent, "walkretain-"+hex.EncodeToString(raw[:]))}, nil
}

// exists reports whether the directory is on disk. A reader consults it so that
// asking for a file does not bring the directory into being.
func (d *dirMaker) exists() bool { return d.created }

// ensure creates the directory, once, and returns it.
func (d *dirMaker) ensure() (string, error) {
	if d.created {
		return d.path, nil
	}
	if err := os.MkdirAll(d.path, 0o700); err != nil {
		return "", internalErr("graph: opening the walk retention directory: " + err.Error())
	}
	d.created = true
	return d.path, nil
}

// retainedWalk is one request's handle on that directory.
type retainedWalk struct {
	home *dirMaker
	// pool is the directory whose scratch pool this walk's sorts take their
	// run files from: the store's, which every walk shares, and never this
	// walk's own directory. A pool is claimed with a lock held for the
	// process's life and registered with the space reclaimer, so a pool per
	// walk would be two descriptors and three permanent entries per request
	// in a server that never restarts.
	pool string
	// prevID is the store id this directory was adopted under by the page that
	// created or last extended it, empty for a directory this request created.
	// The re-adoption that carries it to the next page charges only the bytes
	// it GREW by, which is what keeps the shared budget from holding two copies
	// of the cumulative state at every page boundary (cursor.go retain).
	prevID string
	// bits is the walk's cumulative admitted-node set: a bitset over the
	// generation's surrogate range (bitset.go). It lives here because this
	// directory is already the state a walk continuation carries forward by
	// rename, and because the LEVEL file beside it is what makes a page cut
	// between the two writes recoverable.
	bits *pagedBitset
	// frontier is the CURRENT level's node set, rebuilt from admitted.<level>
	// at every level transition. It answers "is this neighbour on the level
	// being scanned?", which the direction dedup rule asks of every delivered
	// entry, without holding the level in heap.
	frontier *pagedBitset
	// owned marks a directory this request created and must remove itself if
	// nothing adopts it. A directory REOPENED from the store is owned by the
	// store, and is released through the consumed continuation instead --
	// removing it here would take the bytes out from under the store's own
	// accounting.
	owned bool
	// chunkSize overrides frontierChunk. It is zero outside tests.
	chunkSize int
	// held are the files no failure of this page may delete: the ones the
	// cursor this leg was HANDED still names, and the ones the levels this leg
	// has served whole leave behind. A retryable failure tells the caller to
	// present that same cursor again, so nothing the walk behind it reads may
	// be deleted until the next cursor supersedes it (releaseHeld). Its size is
	// the entry state's plus two names per level this page served out.
	held []string
	// probe counts what a RESUMED leg decodes to pick the walk up again, for
	// the invariant that the cost of a resume is a function of the frontier and
	// never of the cumulative set behind it. It is nil outside tests.
	probe *heapProbe
	// crashInAdmit ends a level transition between choosing what to admit and
	// writing admitted.<level>, which is the window the order of effects exists
	// to survive. It is the only way to reach that state, because the code
	// never leaves it behind on purpose, and it is false outside tests.
	crashInAdmit bool
	entries      *retainFile
	pairs        *retainFile
}

// openRetainedWalk names a fresh retained input under parent. Nothing is
// created on disk until the first write into it (dirMaker).
func openRetainedWalk(parent string, max walkBounds, probe *heapProbe) (*retainedWalk, error) {
	home, err := pendingDir(parent)
	if err != nil {
		return nil, err
	}
	w := &retainedWalk{home: home, pool: parent, owned: true}
	w.open()
	if err = w.openSets(max, probe); err != nil {
		w.discard()
		return nil, err
	}
	return w, nil
}

// reopenRetainedWalk reopens the input an earlier leg retained, for append. The
// directory belongs to the spool store, which is what releases it.
func reopenRetainedWalk(dir, prevID string, max walkBounds, probe *heapProbe) (*retainedWalk, error) {
	w := &retainedWalk{home: existingDir(dir), pool: filepath.Dir(dir), prevID: prevID}
	w.open()
	if err := w.openSets(max, probe); err != nil {
		w.discard()
		return nil, err
	}
	return w, nil
}

// openSets opens the two node sets this directory holds -- the walk's
// cumulative admitted set and the current frontier -- each bounded by the
// pinned generation's largest node surrogate. A ref above that bound came from
// another generation and is refused by RANGE before the cursor's fence is ever
// consulted.
func (w *retainedWalk) openSets(max walkBounds, probe *heapProbe) error {
	var err error
	w.probe = probe
	if w.bits, err = openBitset(w.home, bitsetNodeFile, uint64(max.Node), probe); err != nil {
		return err
	}
	w.frontier, err = openBitset(w.home, bitsetFrontierFile, uint64(max.Node), probe)
	return err
}

// walkBounds is the pinned generation's surrogate range. It sizes the two
// sets' ADDRESS space; nothing is allocated for it.
type walkBounds struct {
	Node NodeRef
}

// boundsOf reads that range off the reader the request is pinned to.
func boundsOf(r GraphReader) walkBounds {
	return walkBounds{Node: r.MaxNode()}
}

func (w *retainedWalk) open() {
	w.entries = openRetainFile(w.home, retainEntriesFile)
	w.pairs = openRetainFile(w.home, retainPairsFile)
}

// addEntry appends one admitted impactRecord. It is the accumulator's emit
// callback: every admitted record lands here, and pass 1 is fed from the replay
// once the walk is exhausted.
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
	// The mint is a write: the next request opens this directory by name, so
	// it has to be there even when nothing else forced it into being.
	dir, err := w.home.ensure()
	if err != nil {
		return "", err
	}
	if err := w.close(); err != nil {
		return "", err
	}
	w.home, w.owned = existingDir(""), false
	return dir, nil
}

// discard ends the handle. A directory this request created and nothing
// adopted is removed; one reopened from the store is left to the consumed
// continuation's release, and one already detached is left to whoever took it.
func (w *retainedWalk) discard() {
	_ = w.close()
	if w.owned && w.home.exists() {
		_ = paced.RemoveAll(w.home.path)
	}
	w.home = existingDir("")
}

func (w *retainedWalk) close() error {
	var first error
	for _, b := range []*pagedBitset{w.bits, w.frontier} {
		if b == nil {
			continue
		}
		if err := b.close(); err != nil && first == nil {
			first = err
		}
	}
	w.bits, w.frontier = nil, nil
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
//
// The file, and the directory holding it, are created by the first append. A
// file nothing ever appended to reads as empty rather than as an error, which
// is the same answer it would give if it had been created and left empty.
type retainFile struct {
	home *dirMaker
	name string
	f    *os.File
	w    *bufio.Writer
	// bytes is how many record bytes, frames included, this file holds. It is
	// the truncation point a COLLECTING level's cursor carries.
	bytes int64
}

func openRetainFile(home *dirMaker, name string) *retainFile {
	return &retainFile{home: home, name: name}
}

// path is the file's absolute location, whether or not it is there.
func (r *retainFile) path() string { return filepath.Join(r.home.path, r.name) }

// writer opens the file for append, creating the retained directory with it.
func (r *retainFile) writer() (*bufio.Writer, error) {
	if r.w != nil {
		return r.w, nil
	}
	dir, err := r.home.ensure()
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, r.name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, internalErr("graph: the retained walk input: " + err.Error())
	}
	r.f, r.w = f, bufio.NewWriter(paced.NewWriter(f))
	return r.w, nil
}

// truncate cuts the file back to n bytes and reopens it for append. It is how a
// COLLECTING level resumes: the cursor carries the byte count the interrupted
// request had committed, and anything after it is a partial frame or a record
// the scan is about to deliver again.
func (r *retainFile) truncate(n int64) error {
	if err := r.close(); err != nil {
		return err
	}
	if n == 0 && !r.home.exists() {
		r.bytes = 0
		return nil
	}
	if err := os.Truncate(r.path(), n); err != nil {
		if os.IsNotExist(err) && n == 0 {
			r.bytes = 0
			return nil
		}
		return internalErr("graph: the retained walk input: " + err.Error())
	}
	r.bytes = n
	return nil
}

// remove deletes the file. A file that was never created is already gone.
func (r *retainFile) remove() error {
	if err := r.close(); err != nil {
		return err
	}
	r.bytes = 0
	if err := paced.Remove(r.path()); err != nil && !os.IsNotExist(err) {
		return internalErr("graph: the retained walk input: " + err.Error())
	}
	return nil
}

// retainFrameBytes is the fixed frame prefix: one record's length.
const retainFrameBytes = 4

func (r *retainFile) append(record []byte) error {
	w, err := r.writer()
	if err != nil {
		return err
	}
	var head [retainFrameBytes]byte
	binary.BigEndian.PutUint32(head[:], uint32(len(record)))
	if _, err := w.Write(head[:]); err != nil {
		return internalErr("graph: the retained walk input: " + err.Error())
	}
	if _, err := w.Write(record); err != nil {
		return internalErr("graph: the retained walk input: " + err.Error())
	}
	r.bytes += retainFrameBytes + int64(len(record))
	return nil
}

// each replays the whole file, one record at a time.
func (r *retainFile) each(fn func([]byte) error) error {
	return r.eachFrom(0, func(rec []byte, _ int64) error { return fn(rec) })
}

// eachFrom replays the file from byte offset from, handing fn each record and
// the offset of the NEXT one. That offset is what a SERVING level's cursor
// carries: a page reads from it and the page after resumes exactly where this
// one stopped, with no record delivered twice and none skipped.
//
// The writer is flushed first, so a replay inside the request that is still
// appending sees everything it has appended. A file that was never created
// holds no records, which is what an empty replay means.
func (r *retainFile) eachFrom(from int64, fn func(rec []byte, next int64) error) error {
	if r.w != nil {
		if err := r.w.Flush(); err != nil {
			return internalErr("graph: the retained walk input: " + err.Error())
		}
	}
	f, err := os.Open(r.path())
	if err != nil {
		if os.IsNotExist(err) {
			// A file nothing has appended to holds no records. It is not
			// created until the first append (writer), so its absence is the
			// empty replay and not a loss: a file that was written and then
			// lost its tail is caught by the frame reader below instead.
			return nil
		}
		return internalErr("graph: the retained walk input: " + err.Error())
	}
	defer f.Close()
	if from < 0 {
		return internalErr("graph: the retained walk input was replayed from a negative offset")
	}
	if from > 0 {
		if _, err := f.Seek(from, io.SeekStart); err != nil {
			return internalErr("graph: the retained walk input: " + err.Error())
		}
	}
	in := bufio.NewReader(f)
	var head [retainFrameBytes]byte
	// One reusable buffer, grown to the largest record seen: a replay's heap is
	// one record, never the retained set.
	var buf []byte
	at := from
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
		at += retainFrameBytes + int64(n)
		if err := fn(buf, at); err != nil {
			return err
		}
	}
}

func (r *retainFile) close() error {
	if r.f == nil {
		r.w = nil
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
	// Runs are the adopted runs WITHIN this directory. Each is a base name,
	// never a path: the directory is renamed when the spool store adopts it,
	// so a stored absolute path would name a file that no longer exists.
	Runs []retainedRun `json:"runs,omitempty"`
	// Added is how many records of that pass's input the runs already hold,
	// counted from the front of the input in replay order.
	Added int64 `json:"added"`
}

// rankProgress reads the manifest. A missing one is the zero value.
func (w *retainedWalk) rankProgress() (rankProgress, error) {
	b, err := os.ReadFile(filepath.Join(w.home.path, retainRankFile))
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
	dir, err := w.home.ensure()
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, retainRankFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return internalErr("graph: the retained ranking manifest: " + err.Error())
	}
	if err := os.Rename(tmp, filepath.Join(dir, retainRankFile)); err != nil {
		_ = paced.Remove(tmp)
		return internalErr("graph: the retained ranking manifest: " + err.Error())
	}
	return nil
}

// A retainedRun is one interrupted sort run kept in this directory: its name
// here, and the bytes of the file that are the run's.
//
// The length is persisted because a run file is a pooled scratch surface that
// nothing truncates: the bytes past a run's own belong to whatever tenant held
// the surface before it, and a resumed merge that read to end of file would
// decode them as records of this walk.
type retainedRun struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
}

// retainRunName is the name the run with the given index takes in this
// directory. The name is the run's INDEX, not its source name: DetachTo hands
// over the runs the sort adopted followed by the ones it spilled itself, in
// that order and never reordered (pagination.AdoptRuns), so index i names the
// same run across any number of interruptions -- and a run already sitting at
// that name is the one being re-detached, which DetachTo leaves where it is.
func retainRunName(i int) string { return fmt.Sprintf("run-%06d", i) }

// retainRuns records what a detached sort moved into this directory.
func retainRuns(refs []pagination.RunRef) []retainedRun {
	runs := make([]retainedRun, 0, len(refs))
	for _, ref := range refs {
		runs = append(runs, retainedRun{Name: filepath.Base(ref.Path), Bytes: ref.Bytes})
	}
	return runs
}

// runRefs resolves manifest entries against the directory's CURRENT location.
func (w *retainedWalk) runRefs(runs []retainedRun) []pagination.RunRef {
	refs := make([]pagination.RunRef, 0, len(runs))
	for _, r := range runs {
		refs = append(refs, pagination.RunRef{Path: filepath.Join(w.home.path, r.Name), Bytes: r.Bytes})
	}
	return refs
}

// openFolded opens the retained pass-2 input for append, truncating whatever an
// abandoned earlier attempt left: the folded set is rewritten whole or not at
// all, and half of one appended to half of another is not a fold of anything.
func (w *retainedWalk) openFolded() (*retainFile, error) {
	f := openRetainFile(w.home, retainFoldedFile)
	if err := f.remove(); err != nil {
		return nil, err
	}
	return f, nil
}

// eachFolded replays the retained pass-2 input, the same way eachEntry replays
// pass 1's. The file has no open writer by then, so nothing is flushed first.
func (w *retainedWalk) eachFolded(add func(impactRecord) error) error {
	// A fold that produced no record writes no file: openFolded creates it on
	// the first append, and a walk that admitted nothing has none to make. The
	// empty replay is that walk's answer, and a file written and then cut short
	// is still caught by the frame reader.
	f := openRetainFile(w.home, retainFoldedFile)
	return f.each(func(b []byte) error {
		r, err := decodeImpactRecord(b)
		if err != nil {
			return err
		}
		return add(r)
	})
}

// openWalkState opens a fresh retained walk for this request, sized from the
// pinned generation. It goes through the reader GUARD rather than reading
// e.reader directly, so a workspace wired without the packed reader is refused
// with the typed defect report the guard carries instead of failing somewhere
// inside the scan.
func (e *Engine) openWalkState() (*retainedWalk, error) {
	reader, err := e.Reader()
	if err != nil {
		return nil, err
	}
	return openRetainedWalk(e.walkScratchDir(), boundsOf(reader), e.probe)
}

// reopenWalkState reopens the retained walk a continuation names, through the
// same guard.
func (e *Engine) reopenWalkState(dir, prevID string) (*retainedWalk, error) {
	reader, err := e.Reader()
	if err != nil {
		return nil, err
	}
	return reopenRetainedWalk(dir, prevID, boundsOf(reader), e.probe)
}

// frontierChunk is how many frontier states one scan of the next level takes at
// a time. It is an INTERNAL constant and not a user limit: it bounds resident
// memory, never the work a walk may do or the answer it returns. 65 536 states
// is one Neighbours call over a wide level, so a million-node frontier costs
// sixteen scans and never sixteen million.
const frontierChunk = 65536

// chunk is the frontier chunk this walk uses. The field exists so a test can
// prove the chunking itself -- that a resumed scan starts at the right state --
// on a fixture of a few records rather than of sixty-five thousand.
func (w *retainedWalk) chunk() int {
	if w.chunkSize > 0 {
		return w.chunkSize
	}
	return frontierChunk
}

// encodeFrontierState frames one admitted state: uvarints throughout, so a
// level of admitted nodes costs bytes rather than 64-hex canonical ids.
func encodeFrontierState(fs frontierState) []byte {
	buf := make([]byte, 0, 40+len(fs.Route)*binary.MaxVarintLen64)
	buf = binary.AppendUvarint(buf, uint64(int64(fs.Depth)))
	buf = binary.AppendUvarint(buf, uint64(fs.Cost))
	buf = binary.AppendUvarint(buf, uint64(fs.Node))
	buf = binary.AppendUvarint(buf, uint64(fs.Via))
	buf = binary.AppendUvarint(buf, uint64(len(fs.Route)))
	for _, r := range fs.Route {
		buf = binary.AppendUvarint(buf, uint64(r))
	}
	return buf
}

func decodeFrontierState(b []byte) (frontierState, error) {
	d := &recordDecoder{b: b}
	var fs frontierState
	fs.Depth = int(int64(d.uvarint()))
	fs.Cost = int64(d.uvarint())
	fs.Node = NodeRef(d.uvarint())
	fs.Via = RelRef(d.uvarint())
	n := d.uvarint()
	if d.err != nil {
		return frontierState{}, d.err
	}
	if n > model.MaxRelationsPerPath+1 {
		return frontierState{}, levelCorrupt("an admitted state carries a route longer than a servable path")
	}
	// Nil, not empty, for a routeless state: see decodeLevelRecord.
	for i := uint64(0); i < n; i++ {
		fs.Route = append(fs.Route, RelRef(d.uvarint()))
	}
	if d.err != nil {
		return frontierState{}, d.err
	}
	if len(d.b) != 0 {
		return frontierState{}, levelCorrupt("an admitted state carries trailing bytes")
	}
	return fs, nil
}

// errAdmitCrash is the injected end of a level transition. It never escapes a
// test: nothing in the walk sets crashInAdmit.
var errAdmitCrash = internalErr("graph: the level transition was cut short")

// admitState is the state the winning record of a neighbour group admits: one
// hop past its owner, at its owner's cost plus the edge's kind cost, with the
// admitting relation appended to the owner's route.
func admitState(r levelRecord, costs kindCosts) frontierState {
	return frontierState{
		Depth: r.Owner.Depth + 1,
		Cost:  r.Owner.Cost + costs.of(r.Edge.Kind),
		Node:  r.Edge.Neighbour,
		Via:   r.Edge.Rel,
		Route: appendRoute(r.Owner.Route, r.Edge.Rel),
	}
}

// compareAdmitRoute picks the surviving route inside ONE neighbour group:
// cheapest, then shallowest, then the smallest admitting relation, then the
// smallest owner. It is compareImpactRoute's order over the fields a level
// record carries, and every term is CONTENT-derived -- the canonical ids and
// the kind costs -- so the winner is the same on a fresh index and on a
// delta-built one. Surrogates never enter it.
func compareAdmitRoute(a, b levelRecord, costs kindCosts) int {
	if c := cmp.Compare(a.Owner.Cost+costs.of(a.Edge.Kind), b.Owner.Cost+costs.of(b.Edge.Kind)); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Owner.Depth, b.Owner.Depth); c != 0 {
		return c
	}
	if c := cmp.Compare(a.RelID, b.RelID); c != 0 {
		return c
	}
	return cmp.Compare(a.OwnerID, b.OwnerID)
}

// admitLevel is the level transition's COMMIT, and the order of its effects is
// the whole of page atomicity.
//
// admitted.<level> is written FIRST, and reused untouched when it is already
// there. Bits marked without the states they belong to would leave nodes
// admitted that nothing can ever expand -- they are lost, and the answer
// silently shrinks -- while states written without their bits cost only a
// re-application, which is idempotent because the bitset counts bit
// TRANSITIONS. A redo after a crash therefore admits nothing twice and rebuilds
// the identical frontier.
//
// The visited bits come next, then the frontier set is rebuilt from the same
// file (cleared, not merged: the frontier is one level, not a running union),
// and both are synced before the manifest naming this level can be written.
//
// Peak heap is one neighbour group plus the sort's run buffer: the admitted
// states are ordered by surrogate through an external sort, because the file is
// read back in ascending surrogate order by both bitsets and by the next
// level's scan, and the level arrives in canonical order.
func (w *retainedWalk) admitLevel(ctx context.Context, s *sortedLevel, level int, costs kindCosts) (int64, error) {
	if s.level != level {
		return 0, internalErr("graph: a level transition was asked to commit a run from another level")
	}
	file := openRetainFile(w.home, levelFileName(admittedLevelPrefix, level))
	count, err := w.writeAdmitted(ctx, s, file, level, costs)
	if err != nil {
		return 0, err
	}
	if err := w.applyAdmitted(file, w.bits); err != nil {
		return 0, err
	}
	if err := w.frontier.clear(); err != nil {
		return 0, err
	}
	if err := w.applyAdmitted(file, w.frontier); err != nil {
		return 0, err
	}
	if err := w.bits.sync(); err != nil {
		return 0, err
	}
	if err := w.frontier.sync(); err != nil {
		return 0, err
	}
	return count, nil
}

// writeAdmitted builds admitted.<level>, or counts the one a previous attempt
// already committed. A file that exists is the committed decision of this
// level: rebuilding it would test visited bits that attempt has already set and
// admit nothing at all.
func (w *retainedWalk) writeAdmitted(ctx context.Context, s *sortedLevel, file *retainFile,
	level int, costs kindCosts) (int64, error) {
	var count int64
	if _, err := os.Stat(file.path()); err == nil {
		err := file.each(func([]byte) error {
			count++
			return nil
		})
		return count, err
	} else if !os.IsNotExist(err) {
		return 0, internalErr("graph: the admitted level: " + err.Error())
	}

	// The retained directory must exist before the admitted file is written
	// into it; the sort's runs come from the store's pool, not from here.
	if _, err := w.home.ensure(); err != nil {
		return 0, err
	}
	sorter, err := pagination.NewExternalSort(w.pool, 0,
		func(fs frontierState) ([]byte, error) { return encodeFrontierState(fs), nil },
		decodeFrontierState,
		func(a, b frontierState) int { return cmp.Compare(a.Node, b.Node) })
	if err != nil {
		return 0, err
	}
	defer sorter.Close()

	var best levelRecord
	var have bool
	admit := func() error {
		if !have {
			return nil
		}
		have = false
		in, err := w.bits.test(uint64(best.Edge.Neighbour))
		if err != nil || in {
			return err
		}
		count++
		return sorter.Add(admitState(best, costs))
	}
	if err := s.each(0, func(r levelRecord, _ int64) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if have && r.NodeID != best.NodeID {
			if err := admit(); err != nil {
				return err
			}
		}
		if !have || compareAdmitRoute(r, best, costs) < 0 {
			best, have = r, true
		}
		return nil
	}); err != nil {
		return 0, err
	}
	if err := admit(); err != nil {
		return 0, err
	}
	if w.crashInAdmit {
		return 0, errAdmitCrash
	}

	run, err := sorter.Sorted()
	if err != nil {
		return 0, err
	}
	defer run.Close()
	if err := run.Each(func(fs frontierState) error { return file.append(encodeFrontierState(fs)) }); err != nil {
		return 0, err
	}
	return count, file.close()
}

// applyAdmitted sets every admitted surrogate in set, in ascending chunks so
// that the bitset's page cache is walked forward once.
func (w *retainedWalk) applyAdmitted(file *retainFile, set *pagedBitset) error {
	refs := make([]NodeRef, 0, w.chunk())
	flush := func() error {
		if len(refs) == 0 {
			return nil
		}
		if _, err := bitsetSet(set, refs); err != nil {
			return err
		}
		refs = refs[:0]
		return nil
	}
	if err := file.each(func(b []byte) error {
		fs, err := decodeFrontierState(b)
		if err != nil {
			return err
		}
		refs = append(refs, fs.Node)
		if len(refs) < w.chunk() {
			return nil
		}
		return flush()
	}); err != nil {
		return err
	}
	return flush()
}

// eachFrontier streams the level's admitted states in ascending surrogate
// order, in chunks of at most frontierChunk, resuming at fromNode.
//
// The next level's scan calls GraphReader.Neighbours once per chunk, so the
// frontier is never held whole: a page cut inside a level carries the surrogate
// it stopped on and the resumed scan picks the chunk up from there.
func (w *retainedWalk) eachFrontier(level int, fromNode NodeRef, fn func(chunk []frontierState) error) error {
	file := openRetainFile(w.home, levelFileName(admittedLevelPrefix, level))
	chunk := make([]frontierState, 0, w.chunk())
	if err := file.each(func(b []byte) error {
		fs, err := decodeFrontierState(b)
		if err != nil {
			return err
		}
		if fs.Node < fromNode {
			return nil
		}
		chunk = append(chunk, fs)
		if len(chunk) < w.chunk() {
			return nil
		}
		if err := fn(chunk); err != nil {
			return err
		}
		chunk = chunk[:0]
		return nil
	}); err != nil {
		return err
	}
	if len(chunk) == 0 {
		return nil
	}
	return fn(chunk)
}

// countResumed records the frontier states a RESUMED leg decoded to pick its
// level up again. Nothing else decodes anything on a resume: the level and the
// position inside it ride in the token, and the cumulative sets are bitsets the
// resumed leg never reads whole.
func (w *retainedWalk) countResumed(n int) {
	if w.probe != nil {
		w.probe.ResumeRecords += int64(n)
	}
}

// testFrontier reports whether ref is on the level being scanned. The direction
// dedup rule asks it of every delivered entry, which is why it is a bitset and
// not a set the level would have to be held in heap to build.
func (w *retainedWalk) testFrontier(ref NodeRef) (bool, error) {
	return w.frontier.test(uint64(ref))
}

// commitSeeds writes the seed level as admitted.0 and marks it, in the same
// order of effects every level transition uses: the states first, then the
// visited bits, then the frontier bits the level after it is scanned against.
//
// The seeds are level 0's admitted file because that file IS the frontier the
// next level's scan streams; holding them anywhere else would give the seed
// level a shape no other level has and a resume no continuation could adopt.
func (w *retainedWalk) commitSeeds(level []frontierState) error {
	file := openRetainFile(w.home, levelFileName(admittedLevelPrefix, 0))
	if err := file.remove(); err != nil {
		return err
	}
	for _, fs := range level {
		if err := file.append(encodeFrontierState(fs)); err != nil {
			return err
		}
	}
	if err := file.close(); err != nil {
		return err
	}
	return w.applyFrontier(file)
}

// applyFrontier sets one admitted file's surrogates in the cumulative set and
// rebuilds the frontier set from it (cleared, not merged: the frontier is one
// level, not a running union), then syncs both.
func (w *retainedWalk) applyFrontier(file *retainFile) error {
	if err := w.applyAdmitted(file, w.bits); err != nil {
		return err
	}
	if err := w.frontier.clear(); err != nil {
		return err
	}
	if err := w.applyAdmitted(file, w.frontier); err != nil {
		return err
	}
	if err := w.bits.sync(); err != nil {
		return err
	}
	return w.frontier.sync()
}

// committed reports whether this level's TRANSITION has already run: the
// admitted file is written once, last, by writeAdmitted, so its existence is
// the committed decision of the level.
func (w *retainedWalk) committed(level int) (bool, error) {
	_, err := os.Stat(openRetainFile(w.home, levelFileName(admittedLevelPrefix, level)).path())
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, internalErr("graph: the admitted level: " + err.Error())
}

// setFrontier makes the frontier bitset the named level's admitted set: the
// invariant every served level depends on, since that set is what closeGroup
// charges the visited budget from and what the NEXT level's scan applies the
// direction rule against -- an edge to a node already admitted is served only
// when its far end is on the frontier of the level being scanned.
//
// It is what a transition leaves behind, so it is only ever called where a
// transition is NOT going to run: a level whose transition committed in an
// earlier leg, picked up by a leg whose frontier is still the level before it.
func (w *retainedWalk) setFrontier(level int) error {
	file := openRetainFile(w.home, levelFileName(admittedLevelPrefix, level))
	if err := w.frontier.clear(); err != nil {
		return err
	}
	if err := w.applyAdmitted(file, w.frontier); err != nil {
		return err
	}
	return w.frontier.sync()
}

// alignFrontier is setFrontier for the level a leg is RESUMED into, skipped
// where the bitset on disk is already that level's.
//
// The bitset is rebuilt in place at every transition, so the frontier a leg
// finds is the highest committed level's. Levels commit in order, so it is the
// resumed level's unless admitted.<level+1> exists -- which it does when the
// page that failed retryably had gone on past this level, and the cursor its
// caller was told to present again names this one. Rebuilding on every resumed
// page instead would cost a page the whole level it merely seeks into.
func (w *retainedWalk) alignFrontier(level int) error {
	stale, err := w.committed(level + 1)
	if err != nil || !stale {
		return err
	}
	return w.setFrontier(level)
}

// hasFrontier reports whether the level admitted anything. An empty level is
// an exhausted walk: nothing it could expand was admitted.
func (w *retainedWalk) hasFrontier(level int) (bool, error) {
	file := openRetainFile(w.home, levelFileName(admittedLevelPrefix, level))
	found := false
	if err := file.each(func([]byte) error {
		found = true
		return errLevelCut
	}); err != nil && !errors.Is(err, errLevelCut) {
		return false, err
	}
	return found, nil
}

// hold marks one file of this directory as one no failure of this page may
// delete, so the walk leaves it alone however far past it this leg gets. A name
// already held is not added twice: a leg resumed into a level it then serves
// whole holds that level's run at both points.
func (w *retainedWalk) hold(name string) {
	if !w.isHeld(name) {
		w.held = append(w.held, name)
	}
}

// releaseLevel removes the named files of a level this leg has finished with,
// leaving alone any the walk is holding: a held name belongs to the cursor
// this leg was handed, which a retryable failure can still send the caller
// back to, and only the mint that supersedes that cursor may remove it.
func (w *retainedWalk) releaseLevel(names ...string) error {
	for _, name := range names {
		if w.isHeld(name) {
			continue
		}
		if err := openRetainFile(w.home, name).remove(); err != nil {
			return err
		}
	}
	return nil
}

// isHeld reports whether the walk must leave a file where it is.
func (w *retainedWalk) isHeld(name string) bool { return slices.Contains(w.held, name) }

// releaseHeld deletes them, except the ones keep names. It is called when the
// next continuation is minted: the cursor the caller held is superseded then,
// so a file the NEW cursor does not resume from can go.
func (w *retainedWalk) releaseHeld(keep []string) error {
	held := w.held
	w.held = nil
	for _, name := range held {
		if slices.Contains(keep, name) {
			continue
		}
		if err := openRetainFile(w.home, name).remove(); err != nil {
			return err
		}
	}
	return nil
}
