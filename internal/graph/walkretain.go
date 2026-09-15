package graph

import (
	"bufio"
	"bytes"
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
	// retainLevelFile holds the LAST COMMITTED frontier level, whole, replaced
	// atomically each time a level commits. It is the frontier carrier, not a
	// log: a walk only ever resumes from the newest level, and a file that grew
	// with the walk would make a page's write a function of the walk behind it.
	//
	// It is written BEFORE that level's bits are set, which is the whole of
	// ADR-0005 Decision 2's page atomicity. Bits marked without their records
	// would leave nodes admitted that nothing can expand -- they are lost, and
	// the answer silently shrinks. Records written without their bits cost a
	// re-application, which is idempotent because the bitset counts bit
	// TRANSITIONS rather than set calls.
	retainLevelFile = "level.recs"
)

// retainedWalk is one request's handle on that directory.
type retainedWalk struct {
	dir string
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
	// emitted is the walk's cumulative ADMITTED-RELATION set, the same
	// structure over relation surrogates. It is what keeps one edge from being
	// counted or served twice when a cycle, an overlapping level or a
	// DirectionBoth edge brings the walk back to it.
	emitted *pagedBitset
	// owned marks a directory this request created and must remove itself if
	// nothing adopts it. A directory REOPENED from the store is owned by the
	// store, and is released through the consumed continuation instead --
	// removing it here would take the bytes out from under the store's own
	// accounting.
	owned   bool
	entries *retainFile
	pairs   *retainFile
}

// openRetainedWalk creates a fresh retained input under parent. maxNode is the
// pinned generation's largest node surrogate, which sizes the bitset's ADDRESS
// space; nothing is allocated for it.
func openRetainedWalk(parent string, maxNode NodeRef, probe *heapProbe) (*retainedWalk, error) {
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
	if err = w.openSets(maxNode, probe); err != nil {
		w.discard()
		return nil, err
	}
	return w, nil
}

// reopenRetainedWalk reopens the input an earlier leg retained, for append. The
// directory belongs to the spool store, which is what releases it.
func reopenRetainedWalk(dir, prevID string, maxNode NodeRef, probe *heapProbe) (*retainedWalk, error) {
	w := &retainedWalk{dir: dir, prevID: prevID}
	if err := w.open(); err != nil {
		return nil, err
	}
	if err := w.openSets(maxNode, probe); err != nil {
		w.discard()
		return nil, err
	}
	return w, nil
}

// openSets opens the two cumulative sets this directory holds. The relation set
// is opened UNBOUNDED because the frozen reader port publishes no maximum
// relation surrogate; the cursor's generation fence is what keeps a foreign
// surrogate out of it.
func (w *retainedWalk) openSets(maxNode NodeRef, probe *heapProbe) error {
	var err error
	if w.bits, err = openBitset(w.dir, bitsetNodeFile, maxNode, probe); err != nil {
		return err
	}
	w.emitted, err = openBitset(w.dir, bitsetRelFile, 0, probe)
	return err
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
	for _, b := range []*pagedBitset{w.bits, w.emitted} {
		if b == nil {
			continue
		}
		if err := b.close(); err != nil && first == nil {
			first = err
		}
	}
	w.bits, w.emitted = nil, nil
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

// The frontier level file. Its records are SURROGATES encoded as uvarints,
// which is what makes a spooled level cost bytes rather than 64-hex ids: a
// level of 100 000 nodes at depth 3 is a few hundred kilobytes here and was
// megabytes before.

// spoolLevel replaces the retained frontier with level, whole and atomically.
//
// It is called BEFORE the level's bits are set. A page cut between the two
// writes therefore leaves a level whose records are on disk and whose bits are
// not, and adoptLevel + pagedBitset.set repair exactly that: the records come
// back as the frontier and re-applying their refs sets the missing bits without
// counting the ones that were already there.
//
// Atomically, because a torn level file is a frontier that lost its tail: those
// nodes are marked admitted by bits an earlier attempt may already have set,
// so nothing would ever expand them again.
func (w *retainedWalk) spoolLevel(level []frontierState) error {
	buf := make([]byte, 0, 32*len(level)+binary.MaxVarintLen64)
	buf = binary.AppendUvarint(buf, uint64(len(level)))
	for _, fs := range level {
		if fs.Depth < 0 || fs.Cost < 0 {
			return internalErr("graph: a frontier record carries a negative depth or cost")
		}
		buf = binary.AppendUvarint(buf, uint64(fs.Depth))
		buf = binary.AppendUvarint(buf, uint64(fs.Cost))
		buf = binary.AppendUvarint(buf, uint64(fs.Node))
		buf = binary.AppendUvarint(buf, uint64(fs.Via))
		buf = binary.AppendUvarint(buf, uint64(len(fs.Route)))
		for _, r := range fs.Route {
			buf = binary.AppendUvarint(buf, uint64(r))
		}
	}
	tmp := filepath.Join(w.dir, retainLevelFile+".tmp")
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return internalErr("graph: spooling the frontier level: " + err.Error())
	}
	if err := os.Rename(tmp, filepath.Join(w.dir, retainLevelFile)); err != nil {
		_ = os.Remove(tmp)
		return internalErr("graph: spooling the frontier level: " + err.Error())
	}
	return nil
}

// adoptLevel reads the last committed frontier back. A missing file is a walk
// that has committed no level yet -- the seeds' own level -- and is the empty
// frontier, not an error.
func (w *retainedWalk) adoptLevel() ([]frontierState, error) {
	raw, err := os.ReadFile(filepath.Join(w.dir, retainLevelFile))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, internalErr("graph: reading the spooled frontier level: " + err.Error())
	}
	rd := bytes.NewReader(raw)
	n, err := binary.ReadUvarint(rd)
	if err != nil {
		return nil, retainCorrupt(err)
	}
	// The count is a LENGTH read off disk, so it never sizes an allocation on
	// its own: a corrupt or tampered header would otherwise reserve gigabytes
	// before the first record failed to decode. append grows it against the
	// bytes that are actually there.
	level := make([]frontierState, 0, min(uint64(rd.Len()/4)+1, n))
	for i := uint64(0); i < n; i++ {
		var fs frontierState
		fields := [4]uint64{}
		for j := range fields {
			if fields[j], err = binary.ReadUvarint(rd); err != nil {
				return nil, retainCorrupt(err)
			}
		}
		fs.Depth, fs.Cost = int(fields[0]), int64(fields[1])
		fs.Node, fs.Via = NodeRef(fields[2]), RelRef(fields[3])
		routeLen, err := binary.ReadUvarint(rd)
		if err != nil {
			return nil, retainCorrupt(err)
		}
		if routeLen > model.MaxRelationsPerPath+1 {
			return nil, retainCorrupt(fmt.Errorf("a spooled route is longer than a servable path"))
		}
		fs.Route = make([]RelRef, 0, routeLen)
		for j := uint64(0); j < routeLen; j++ {
			r, err := binary.ReadUvarint(rd)
			if err != nil {
				return nil, retainCorrupt(err)
			}
			fs.Route = append(fs.Route, RelRef(r))
		}
		level = append(level, fs)
	}
	return level, nil
}

// commitTaken is what one level actually took, written in the order ADR-0005
// Decision 2 froze so that no caller can take the two writes in the other
// order: the frontier RECORDS first, then the relations this level emitted,
// then the nodes it admitted.
//
// Records before bits, because bits without their records leave nodes marked
// admitted that nothing can ever expand -- they are lost, and the answer
// silently shrinks. Records without their bits cost only a re-application,
// which is idempotent because the bitset counts bit transitions.
//
// Emitted relations before admitted nodes, for the same asymmetry one level
// down: a page cut between the two resumes with the edges already dropped and
// the nodes re-admitted from the spooled records, where the other order would
// serve those edges a second time.
func (w *retainedWalk) commitTaken(level []frontierState, admitted []NodeRef, emitted []RelRef) error {
	if err := w.spoolLevel(level); err != nil {
		return err
	}
	if _, err := bitsetSet(w.emitted, ascending(emitted)); err != nil {
		return err
	}
	_, err := bitsetSet(w.bits, ascending(admitted))
	return err
}

// commitLevel is the seed level's commit: it has admitted nodes and no emitted
// edges, because nothing has been read yet.
func (w *retainedWalk) commitLevel(level []frontierState, refs []NodeRef) error {
	return w.commitTaken(level, refs, nil)
}

// adopt re-applies the last spooled level's bits and returns it as the frontier
// a continuation resumes from. It is the repair half of the commit order: a
// page cut between the records and the bits left bits missing, and re-applying
// every record's surrogate restores them without counting one twice.
func (w *retainedWalk) adopt() ([]frontierState, error) {
	level, err := w.adoptLevel()
	if err != nil {
		return nil, err
	}
	refs := make([]NodeRef, 0, len(level))
	for _, fs := range level {
		refs = append(refs, fs.Node)
	}
	if _, err := bitsetSet(w.bits, ascending(refs)); err != nil {
		return nil, err
	}
	return level, nil
}

// membership answers the whole level's "has this node been admitted?" questions
// at once, testing the bitset in ASCENDING surrogate order so the page cache is
// walked forward and each page is touched once, rather than in the emission
// order, which is canonical and therefore unrelated to page layout.
func (w *retainedWalk) membership(rows []edgeRow) (map[NodeRef]bool, error) {
	refs := make([]NodeRef, 0, len(rows))
	for _, row := range rows {
		refs = append(refs, row.edge.Neighbour)
	}
	refs = ascending(refs)
	seen := make(map[NodeRef]bool, len(refs))
	for _, ref := range refs {
		in, err := w.bits.test(uint64(ref))
		if err != nil {
			return nil, err
		}
		if in {
			seen[ref] = true
		}
	}
	return seen, nil
}

// openWalkState opens a fresh retained walk for this request, sized from the
// pinned generation. It goes through the reader GUARD rather than reading
// e.reader directly, so a workspace wired without the packed reader is refused
// with the typed defect report the guard carries instead of failing somewhere
// inside the scan.
func (e *Engine) openWalkState() (*retainedWalk, error) {
	reader, err := e.consumerReader()
	if err != nil {
		return nil, err
	}
	return openRetainedWalk(e.walkScratchDir(), reader.MaxNode(), e.probe)
}

// reopenWalkState reopens the retained walk a continuation names, through the
// same guard.
func (e *Engine) reopenWalkState(dir, prevID string) (*retainedWalk, error) {
	reader, err := e.consumerReader()
	if err != nil {
		return nil, err
	}
	return reopenRetainedWalk(dir, prevID, reader.MaxNode(), e.probe)
}
