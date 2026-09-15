package context

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// Ruling C7: resources.query_timeout must end a PASS of a streamed compile, not
// the answer. This file is the persistence half of that -- how the streams a
// completed pass produced survive between two calls -- and nothing here decides
// WHEN a compile stops.
//
// The unit of persistence is a STATE DIRECTORY, not a record spool. A compile's
// cross-pass state is a handful of sorted runs, which are already files; copying
// them through a record spool would charge a second full copy against
// resources.max_temp_bytes for no gain. pagination.Spools.AdoptDir exists for
// exactly this shape: the directory is named, leased, budgeted and swept like a
// spool, so a continuation that is never resumed is reclaimed by the same sweep
// rather than left behind. The compile stages the directory beside the store's
// sort area (checkpointDir) so that adoption is an O(1) same-filesystem rename.
//
// The two-class rule below is the one thing a caller MUST get right, because
// getting it wrong produces a wrong plan with no error. pagination.AdoptRuns
// documents it: a merge re-applies the comparator and the fold, and a divergent
// or missing one "merges without error into an answer that is not sorted, and
// nothing here can detect that".
//
//   - A checkpointed *SortedRun* is ALREADY merged and ALREADY folded. It is
//     restored with its comparator and NO fold: re-attaching one would fold a
//     stream whose equal neighbours were collapsed a call ago, which for a fold
//     that sums (foldPkgCount) double-counts.
//   - A checkpointed, not-yet-Sorted *ExternalSort* is PRE-fold. It is restored
//     with exactly the fold it was built with, or the collapse the pass ahead of
//     it depends on never happens (foldPkgEdgeDistinct is what keeps a package
//     reached twice over one edge from over-counting its centrality boost).
//
// checkpointRun and checkpointSort are the two entry points those classes get,
// and they are separate functions rather than one with a flag so that a caller
// picks the class at the call site instead of passing the wrong argument.

// checkpointState is one interrupted compile's state.json: what the next call
// must re-validate before it adopts a single byte, plus where each stream's
// runs live inside the directory.
//
// Identity is the whole of the validation. RequestHash is the compile's
// normalised request identity (manifestIdentity, which already folds the
// binding, the request and the configured bounds), so a cursor minted for one
// request can never resume another, and a configuration change between two
// calls ends the continuation rather than splicing two different compiles. The
// lease, generation and query checks are the spool's own (Spools.OpenDir) and
// are deliberately NOT duplicated here.
type checkpointState struct {
	// Version fences the on-disk shape. A directory written by a different
	// build is refused rather than decoded into a plan.
	Version int `json:"v"`
	// Pass is the index of the FIRST UNFINISHED pass: the compile resumes by
	// running it, and every pass below it is represented by the streams here.
	Pass int `json:"pass"`
	// RequestHash is the manifest identity the interrupted call computed.
	RequestHash string `json:"request_hash"`
	// Streams maps a stream's name to the run files it was checkpointed as,
	// relative to the state directory so the directory stays relocatable
	// across the adoption rename.
	Streams map[string][]string `json:"streams"`
	// Scalars is the cross-pass state that is not a stream: the scope verdict,
	// the completeness rows and the flags a later pass may only ever narrow.
	Scalars checkpointScalars `json:"scalars"`
}

// checkpointScalars is the non-stream carry. Each field is bounded by something
// other than the repository -- a verdict, a flag, a count, or the capability
// report, which is one row per provider -- so holding it in the state file is
// not the repository-sized list this wave removes.
type checkpointScalars struct {
	Completeness     []model.CapabilityState `json:"completeness,omitempty"`
	ScopeComplete    bool                    `json:"scope_complete"`
	ReasonsDropped   int64                   `json:"reasons_dropped,omitempty"`
	ReasonsTruncated int64                   `json:"reasons_truncated,omitempty"`
}

// checkpointVersion is the on-disk shape of a compile's continuation state.
const checkpointVersion = 2

// checkpointFile is state.json's name inside the state directory. It does not
// collide with pagination's own header file, which the adoption writes beside
// it.
const checkpointFile = "compile-state.json"

// writeCheckpointState writes state.json into dir. It is written LAST, after
// every run file is in place, so a directory that carries a readable state file
// always carries the runs it names: a resume never meets a half-written
// checkpoint and has no partial-state case to handle.
func writeCheckpointState(dir string, st checkpointState) error {
	st.Version = checkpointVersion
	b, err := json.Marshal(st)
	if err != nil {
		return &model.Error{Code: model.CodeInternal, Message: "context checkpoint: " + err.Error()}
	}
	if err := os.WriteFile(filepath.Join(dir, checkpointFile), b, 0o600); err != nil {
		return &model.Error{Code: model.CodeInternal, Message: "context checkpoint: " + err.Error()}
	}
	return nil
}

// readCheckpointState reads and version-checks the state file in dir. A
// directory whose state file is missing, unreadable or of another version is
// reported as an expired continuation and never as a compile failure: the
// caller's recourse is identical either way (compile again without a cursor),
// and a stale directory is an ordinary consequence of the sweep.
func readCheckpointState(dir string) (checkpointState, error) {
	b, err := os.ReadFile(filepath.Join(dir, checkpointFile))
	if err != nil {
		return checkpointState{}, cursorExpired("the continuation state has expired or was released")
	}
	var st checkpointState
	if err := json.Unmarshal(b, &st); err != nil {
		return checkpointState{}, cursorExpired("the continuation state has expired or was released")
	}
	if st.Version != checkpointVersion {
		return checkpointState{}, cursorExpired("the continuation state was written by another build")
	}
	return st, nil
}

// cursorExpired is the typed answer to continuation state that is gone. It
// matches what pagination returns for a released spool, so a caller handles one
// case and not two.
func cursorExpired(msg string) error {
	return &model.Error{Code: model.CodeCursorInvalid, Message: msg}
}

// checkpointRun persists an ALREADY-SORTED, already-folded stream into dir
// under the given name and answers the run files it became, relative to dir.
//
// It replays the run through a fresh external sort rooted in dir and detaches
// it, rather than moving the merged file. That is deliberate and it is what
// makes the round trip exact in both of a SortedRun's two shapes: a run that
// never spilled has no file to move, and a run that did has a file the
// pagination package does not expose the path of. Replay costs one bounded pass
// -- peak live records stays the run buffer, never the length of the stream --
// and it is ordered on arrival, so each detached run is already sorted and the
// merge that adopts them reproduces the identical sequence: runs are
// stable-sorted, and the merge breaks ties by run index over runs written in
// arrival order.
//
// A nil run checkpoints as no files, which restores as no stream. That is the
// honest encoding of a carry slot a boundary does not have live yet.
func checkpointRun[T any](dir, name string, runBytes int64, run *pagination.SortedRun[T],
	compare func(a, b T) int, sizeOf func(T) int64,
) ([]string, error) {
	if run == nil {
		return nil, nil
	}
	return checkpointStream(dir, name, runBytes, run.Each, compare, sizeOf)
}

// checkpointSort persists a NOT-YET-SORTED external sort -- a pre-fold stream
// whose collapse has not happened -- by detaching its runs directly. No replay
// is possible here, and none is needed: Detach spills the pending buffer first,
// so the detached runs hold every record added.
//
// OWNERSHIP MOVES to the state directory, which is what the interrupted sort
// wants: its own deferred Close no longer removes these files (Detach clears
// its run list), and the lease that owns the directory is what reclaims them if
// the continuation is never resumed.
//
// The detached files are written wherever the sort was rooted, which is the
// compile's sort area, so they are MOVED into dir here. Both are inside the
// store's own spool area, so the move is a same-filesystem rename.
func checkpointSort[T any](dir, name string, sorter *pagination.ExternalSort[T]) ([]string, error) {
	if sorter == nil {
		return nil, nil
	}
	runs, err := sorter.Detach()
	if err != nil {
		return nil, err
	}
	return moveRuns(dir, name, runs)
}

// checkpointStream is the shared body of checkpointRun: it drains a record
// sequence into a fresh sort rooted in dir and detaches the runs it spilled.
func checkpointStream[T any](dir, name string, runBytes int64, each func(func(T) error) error,
	compare func(a, b T) int, sizeOf func(T) int64,
) ([]string, error) {
	sorter, err := pagination.NewExternalSort(dir, checkpointPrefix(name), 0,
		encodeRecord[T], decodeRecord[T], compare)
	if err != nil {
		return nil, err
	}
	sorter = sorter.WithRunBytes(runBytes, sizeOf)
	if err := each(func(v T) error { return sorter.Add(v) }); err != nil {
		sorter.Close()
		return nil, err
	}
	// Detach spills the pending buffer, so a stream that fit one buffer still
	// becomes a file: a checkpoint has to survive the process, and an in-heap
	// run does not.
	runs, err := sorter.Detach()
	if err != nil {
		sorter.Close()
		return nil, err
	}
	// Renamed into DETACH order, never listed by their os.CreateTemp suffix:
	// the merge that adopts these runs breaks ties by run index, so the index a
	// run is stored under is what reproduces the single stable sort this stream
	// already is. The files are already inside dir, so the rename only fixes
	// their names.
	return moveRuns(dir, name, runs)
}

// restoreRun adopts a checkpointed ALREADY-FOLDED stream and answers it as a
// sorted run again. No fold is attached: see the two-class rule at the top of
// this file.
//
// The restored sort is registered with the compile's sort area, so the merged
// output is removed on every exit path exactly as a freshly computed run is.
func restoreRun[T any](s *compileSorts, dir, name string, runs []string,
	compare func(a, b T) int, sizeOf func(T) int64,
) (*pagination.SortedRun[T], error) {
	sorter, err := restoreSort(s, dir, name, runs, compare, sizeOf)
	if err != nil {
		return nil, err
	}
	return sortedRun(s, sorter)
}

// restoreSort adopts a checkpointed PRE-fold stream as an external sort the
// resuming pass drives to completion itself. The caller re-attaches the exact
// fold the interrupted sort carried, with WithFold, before handing it on.
//
// An empty run list answers an EMPTY sort and never a nil one. A stream that
// held no record spills no run, so "no files" is the honest encoding of an
// empty stream -- and pagination.SortedRun.Each reads a nil run as an empty one
// without complaint (extsort.go:662), so answering nil here would turn a
// restore that lost a stream into a plan that is silently short of it.
func restoreSort[T any](s *compileSorts, dir, name string, runs []string,
	compare func(a, b T) int, sizeOf func(T) int64,
) (*pagination.ExternalSort[T], error) {
	if len(runs) == 0 {
		return newSort(s, "restored-"+name, compare, sizeOf)
	}
	abs, err := absoluteRuns(dir, runs)
	if err != nil {
		return nil, err
	}
	sorter, err := pagination.AdoptRuns(dir, checkpointPrefix(name), 0, abs,
		encodeRecord[T], decodeRecord[T], compare)
	if err != nil {
		return nil, err
	}
	s.track(sorter.Close)
	return sorter, nil
}

// checkpointPrefix namespaces one stream's run files inside the state
// directory, so two streams of one checkpoint never share a name and a resume
// can tell them apart by inspection.
func checkpointPrefix(name string) string { return "ctx-resume-" + name + "-" }

// absoluteRuns resolves stored names back against the state directory and
// refuses any name that escapes it. The names come from a file inside a
// directory a cursor named, so they are validated rather than trusted: a
// traversing name would otherwise let a crafted state file point the merge at a
// file outside the store.
func absoluteRuns(dir string, runs []string) ([]string, error) {
	out := make([]string, 0, len(runs))
	for _, r := range runs {
		if r == "" || filepath.IsAbs(r) || r != filepath.Clean(r) ||
			strings.HasPrefix(r, "..") || strings.ContainsRune(r, filepath.Separator) {
			return nil, cursorExpired("the continuation state names a run outside its directory")
		}
		out = append(out, filepath.Join(dir, r))
	}
	return out, nil
}

// moveRuns renames detached run files into the state directory under this
// stream's prefix, NUMBERED BY DETACH ORDER, and answers their names relative
// to it. The index is load-bearing and not cosmetic: pagination's merge breaks
// ties by run index, so storing runs in arrival order is what makes the resumed
// merge reproduce the order the interrupted stream was in -- which matters for
// every comparator that is not total (lessScoredPkg orders on Pkg alone).
// The "run" infix keeps a target name out of the namespace os.CreateTemp draws
// the source suffixes from, so a rename can never land on a run not moved yet. A failure part way
// through leaves the already-moved files in the directory, which the lease
// reclaims: the caller's answer is an uninterrupted compile or an error, never
// a checkpoint that names files it does not have.
func moveRuns(dir, name string, runs []string) ([]string, error) {
	out := make([]string, 0, len(runs))
	for i, r := range runs {
		base := checkpointPrefix(name) + "run" + strconv.Itoa(i)
		if err := os.Rename(r, filepath.Join(dir, base)); err != nil {
			return nil, &model.Error{Code: model.CodeInternal, Message: "context checkpoint: " + err.Error()}
		}
		out = append(out, base)
	}
	return out, nil
}

// The Compile-side half of ruling C7: WHEN a compile stops, what it writes at
// that boundary, and how the call after it comes back.
//
// EVERY pass boundary is a checkpoint. A compile that runs out of deadline
// between any two passes persists what the passes it finished produced, records
// the index of the first unfinished pass and answers a continuation cursor; the
// next call restores exactly those streams and re-enters the pipeline there. A
// deadline is therefore never a lost compile, whichever pass it lands behind --
// which is the whole of ruling C7 and not the single boundary C-D3 could reach.
//
// The pass indices are the plan's P-A..P-I in order. A checkpoint's Pass is the
// FIRST UNFINISHED pass, so the boundary behind P-A is passHydrate and the last
// boundary a compile can stop at is passEmit: P-I finishes the plan in the call
// that runs it, so there is no boundary behind it.
const (
	passIngest     = 1 // P-A
	passHydrate    = 2 // P-B
	passAttributes = 3 // P-C
	passRoutes     = 4 // P-D
	passCentrality = 5 // P-E
	passBoosts     = 6 // P-F
	passMeasure    = 7 // P-G
	passPack       = 8 // P-H
	passEmit       = 9 // P-I
)

// stream names inside a checkpoint. They are the state file's keys, so they are
// constants rather than literals at the checkpoint and restore call sites.
const (
	streamCands     = "cands"
	streamHydrated  = "hydrated"
	streamRoutes    = "route-path"
	streamHops      = "route-hop"
	streamScored    = "scored"
	streamCounts    = "pkg-count"
	streamKeptPaths = "kept-path"
	streamKeptHops  = "kept-hop"
	streamEdges     = "pkg-edge"
	streamRanked    = "ranked"
	streamByFile    = "by-file"
	streamGroups    = "groups"
	streamExcluded  = "excluded"
	streamMeasPaths = "measured-path"
	streamMeasHops  = "measured-hop"
	streamVerdicts  = "verdicts"
)

// passStreams answers the streams a checkpoint taken at boundary `pass` carries
// -- exactly the live carry the pass named there reads, and nothing else.
//
// It is ONE table driving both directions: the checkpoint writes exactly these
// names and refuses a slot the boundary should have live but does not, and the
// restore requires exactly these names to be present. Without it a state file
// that lost a stream would restore a nil run, which pagination.SortedRun.Each
// reads as an EMPTY stream without complaint -- a wrong plan with no error, the
// same failure class this file's header warns about for a divergent fold.
//
// An unknown pass answers no streams, which the caller reports as a boundary
// this build does not resume.
func passStreams(pass int) []string {
	switch pass {
	case passHydrate:
		return []string{streamCands, streamRoutes, streamHops}
	case passAttributes:
		return []string{streamCands, streamHydrated, streamRoutes, streamHops}
	case passRoutes:
		return []string{streamHydrated, streamRoutes, streamHops}
	case passCentrality:
		return []string{streamScored, streamKeptPaths, streamKeptHops, streamEdges}
	case passBoosts:
		return []string{streamScored, streamCounts, streamKeptPaths, streamKeptHops}
	case passMeasure:
		return []string{streamRanked, streamKeptPaths, streamKeptHops}
	case passPack:
		return []string{streamByFile, streamGroups, streamExcluded, streamMeasPaths, streamMeasHops}
	case passEmit:
		return []string{streamByFile, streamGroups, streamExcluded, streamMeasPaths, streamMeasHops,
			streamVerdicts}
	default:
		return nil
	}
}

// missingStream is the internal refusal of a boundary whose carry is not what
// passStreams says it is. It is an invariant of this package and not a data
// condition, so it is CTX_INTERNAL rather than an expired continuation.
func missingStream(name string) error {
	return &model.Error{Code: model.CodeInternal,
		Message: "context checkpoint: the boundary has no " + name + " stream"}
}

// cpRun is checkpointRun with the boundary's own liveness check: a stream
// passStreams lists must be live, because an absent one and an EMPTY one both
// checkpoint as no files and only this check tells them apart.
func cpRun[T any](dir, name string, runBytes int64, run *pagination.SortedRun[T],
	compare func(a, b T) int, sizeOf func(T) int64,
) ([]string, error) {
	if run == nil {
		return nil, missingStream(name)
	}
	return checkpointRun(dir, name, runBytes, run, compare, sizeOf)
}

// checkpointStream persists ONE named stream of st into dir. The two-class rule
// at the top of this file is applied here, at the call site, which is why this
// is a switch over names rather than a loop over an interface: streamEdges is
// the compile's one PRE-fold carry and is the only case that uses
// checkpointSort.
func (st *compileState) checkpointStream(dir, name string, runBytes int64) ([]string, error) {
	switch name {
	case streamCands:
		return cpRun(dir, name, runBytes, st.cands, lessCandSeq, sizeOfCand)
	case streamHydrated:
		return cpRun(dir, name, runBytes, st.hydrated, lessCandSeq, sizeOfCand)
	case streamRoutes:
		return cpRun(dir, name, runBytes, st.paths, lessPathSeq, sizeOfPath)
	case streamHops:
		return cpRun(dir, name, runBytes, st.hops, lessHopSeq, sizeOfHop)
	case streamScored:
		return cpRun(dir, name, runBytes, st.scored, lessScoredPkg, sizeOfScored)
	case streamCounts:
		return cpRun(dir, name, runBytes, st.counts, lessPkg, sizeOfPkgCount)
	case streamKeptPaths:
		return cpRun(dir, name, runBytes, st.keptPaths, lessPathSeq, sizeOfPath)
	case streamKeptHops:
		return cpRun(dir, name, runBytes, st.keptHops, lessHopSeq, sizeOfHop)
	case streamEdges:
		// The one PRE-fold carry: P-D has finished writing it but P-E has not
		// merged it, so it is detached as it stands and restored with
		// foldPkgEdgeDistinct re-attached.
		if st.edges == nil {
			return nil, missingStream(name)
		}
		return checkpointSort(dir, name, st.edges)
	case streamRanked:
		return cpRun(dir, name, runBytes, st.ranked, lessRank, sizeOfCand)
	case streamByFile:
		return cpRun(dir, name, runBytes, st.measuredRun(name), lessFileIndex, sizeOfCand)
	case streamGroups:
		if st.measured == nil {
			return nil, missingStream(name)
		}
		return cpRun(dir, name, runBytes, st.measured.Groups, lessGroupIndex, sizeOfGroup)
	case streamExcluded:
		return cpRun(dir, name, runBytes, st.measuredRun(name), lessCandSeq, sizeOfCand)
	case streamMeasPaths:
		if st.measured == nil {
			return nil, missingStream(name)
		}
		return cpRun(dir, name, runBytes, st.measured.Paths, lessPathSeq, sizeOfPath)
	case streamMeasHops:
		if st.measured == nil {
			return nil, missingStream(name)
		}
		return cpRun(dir, name, runBytes, st.measured.Hops, lessHopSeq, sizeOfHop)
	case streamVerdicts:
		if st.packed == nil {
			return nil, missingStream(name)
		}
		return cpRun(dir, name, runBytes, st.packed.Verdicts, lessVerdict, sizeOfVerdict)
	default:
		return nil, missingStream(name)
	}
}

// measuredRun answers one of the two candRec streams of a measured plan, or nil
// when P-G has not run: the two share a record type, so one helper serves both
// and cpRun's own nil check reports the absent measurement.
func (st *compileState) measuredRun(name string) *pagination.SortedRun[candRec] {
	if st.measured == nil {
		return nil
	}
	if name == streamByFile {
		return st.measured.ByFile
	}
	return st.measured.Excluded
}

// restoreStream adopts ONE named stream back into st. It mirrors
// checkpointStream case for case; streamEdges is the one that re-attaches its
// fold, because it is the one that was checkpointed PRE-fold.
func (st *compileState) restoreStream(s *compileSorts, dir, name string, files []string) error {
	var err error
	switch name {
	case streamCands:
		st.cands, err = restoreRun(s, dir, name, files, lessCandSeq, sizeOfCand)
	case streamHydrated:
		st.hydrated, err = restoreRun(s, dir, name, files, lessCandSeq, sizeOfCand)
	case streamRoutes:
		st.paths, err = restoreRun(s, dir, name, files, lessPathSeq, sizeOfPath)
	case streamHops:
		st.hops, err = restoreRun(s, dir, name, files, lessHopSeq, sizeOfHop)
	case streamScored:
		st.scored, err = restoreRun(s, dir, name, files, lessScoredPkg, sizeOfScored)
	case streamCounts:
		st.counts, err = restoreRun(s, dir, name, files, lessPkg, sizeOfPkgCount)
	case streamKeptPaths:
		st.keptPaths, err = restoreRun(s, dir, name, files, lessPathSeq, sizeOfPath)
	case streamKeptHops:
		st.keptHops, err = restoreRun(s, dir, name, files, lessHopSeq, sizeOfHop)
	case streamEdges:
		var sorter *pagination.ExternalSort[pkgEdgeRec]
		sorter, err = restoreSort(s, dir, name, files, lessPkgEdge, sizeOfPkgEdge)
		if err == nil {
			// Re-attached, and this is the line the whole two-class rule is
			// about: without it P-E counts a (package, relation) pair once per
			// route that reached it and every centrality boost inflates.
			st.edges = sorter.WithFold(foldPkgEdgeDistinct)
		}
	case streamRanked:
		st.ranked, err = restoreRun(s, dir, name, files, lessRank, sizeOfCand)
	case streamByFile:
		st.ensureMeasured().ByFile, err = restoreRun(s, dir, name, files, lessFileIndex, sizeOfCand)
	case streamGroups:
		st.ensureMeasured().Groups, err = restoreRun(s, dir, name, files, lessGroupIndex, sizeOfGroup)
	case streamExcluded:
		st.ensureMeasured().Excluded, err = restoreRun(s, dir, name, files, lessCandSeq, sizeOfCand)
	case streamMeasPaths:
		st.ensureMeasured().Paths, err = restoreRun(s, dir, name, files, lessPathSeq, sizeOfPath)
	case streamMeasHops:
		st.ensureMeasured().Hops, err = restoreRun(s, dir, name, files, lessHopSeq, sizeOfHop)
	case streamVerdicts:
		var run *pagination.SortedRun[packVerdict]
		run, err = restoreRun(s, dir, name, files, lessVerdict, sizeOfVerdict)
		if err == nil {
			st.packed = &packedPlan{Verdicts: run}
		}
	default:
		return missingStream(name)
	}
	return err
}

// ensureMeasured opens the measured carry the back-half boundaries restore into.
func (st *compileState) ensureMeasured() *measuredPlan {
	if st.measured == nil {
		st.measured = &measuredPlan{}
	}
	return st.measured
}

// deadlineReached reports whether ctx has nothing left to spend. It is checked
// at every pass boundary, where a true answer means "stop and continue" rather
// than "fail".
//
// The question is asked of the COMPILER'S clock (Options.Now, the same one the
// manifest is stamped from) and not of time.Now, so that "the deadline fired at
// this boundary" is a thing a caller can arrange deterministically. That is not
// a test affordance bolted on: a wall-clock deadline inside a compile lands in
// the middle of a pass far more often than on a boundary, so without a clock
// the caller owns, the branch that writes the checkpoint and refuses to persist
// a manifest is reachable in production and unreachable in any test.
func (c *Compiler) deadlineReached(ctx context.Context) bool {
	if ctx.Err() != nil {
		return true
	}
	dl, ok := ctx.Deadline()
	return ok && !c.now().Before(dl)
}

// resumedHalf is an opened continuation: the verified cursor, the leased state
// directory and the state file it carries.
type resumedHalf struct {
	Cursor contextCursor
	Dir    string
	State  checkpointState
}

// openResumed verifies token against this compile's binding and request
// identity, opens the leased state directory and reads its state file. The
// state file's own pass and request hash are re-checked against the token's:
// the two are written at the same instant, and a disagreement means the
// directory is not the one the token was minted for.
//
// ANY pass boundary is accepted: passStreams is what decides whether this build
// knows the boundary, and a pass it has no stream table for is an expired
// continuation rather than a compile failure, exactly as a state file from
// another build is.
func (c *Compiler) openResumed(ctx context.Context, token string, b model.Binding, requestHash string) (*resumedHalf, error) {
	cur, dir, err := c.resumeState(ctx, token, b, requestHash)
	if err != nil {
		return nil, err
	}
	st, err := readCheckpointState(dir)
	if err != nil {
		return nil, err
	}
	if st.Pass != cur.Pass || st.RequestHash != requestHash {
		return nil, cursorExpired("the continuation state does not belong to this cursor")
	}
	if len(passStreams(st.Pass)) == 0 {
		return nil, cursorExpired("the continuation state was written at a boundary this build does not resume")
	}
	return &resumedHalf{Cursor: cur, Dir: dir, State: st}, nil
}

// checkpointAt writes the carry live at st's boundary into a fresh state
// directory beside the compile's runs, hands it to the spool store under a
// fresh cursor-owned lease and answers the signed token.
//
// The directory is staged in the sort area so adoption is an O(1)
// same-filesystem rename, and the state file is written LAST so a directory a
// cursor names always carries the runs it names.
func (c *Compiler) checkpointAt(ctx context.Context, b model.Binding, requestHash string,
	st *compileState,
) (string, error) {
	if !c.continuationsAvailable() {
		// No continuation is on offer, so the deadline is what it always was.
		return "", contextErr(ctx, context.DeadlineExceeded)
	}
	names := passStreams(st.pass)
	if len(names) == 0 {
		return "", &model.Error{Code: model.CodeInternal,
			Message: "context checkpoint: no carry is defined for this pass boundary"}
	}
	dir, err := os.MkdirTemp(c.sortDir, "ctx-state-")
	if err != nil {
		return "", &model.Error{Code: model.CodeInternal, Message: "context checkpoint: " + err.Error()}
	}
	runBytes := pagination.SortRunBytes(int64(c.cfg.Resources.QueryMemoryBytes))
	state := checkpointState{
		Pass:        st.pass,
		RequestHash: requestHash,
		Streams:     make(map[string][]string, len(names)),
		Scalars: checkpointScalars{
			Completeness:     st.scope.Completeness,
			ScopeComplete:    st.scopeComplete,
			ReasonsDropped:   st.scope.ReasonsDropped,
			ReasonsTruncated: st.scope.ReasonsTruncated,
		},
	}
	fail := func(err error) (string, error) {
		_ = os.RemoveAll(dir)
		return "", err
	}
	for _, name := range names {
		files, ferr := st.checkpointStream(dir, name, runBytes)
		if ferr != nil {
			return fail(ferr)
		}
		state.Streams[name] = files
	}
	if err := writeCheckpointState(dir, state); err != nil {
		return fail(err)
	}
	return c.nextStateCursor(ctx, b, requestHash, state.Pass, dir)
}

// restoreAt adopts a checkpoint's streams into this call's sort area and its
// scalars into st, so the pipeline re-enters at the recorded pass with exactly
// the carry the interrupted call had there.
func (c *Compiler) restoreAt(s *compileSorts, r *resumedHalf, st *compileState) error {
	names := passStreams(r.State.Pass)
	if len(names) == 0 {
		return cursorExpired("the continuation state was written at a boundary this build does not resume")
	}
	for _, name := range names {
		files, ok := r.State.Streams[name]
		if !ok {
			return cursorExpired("the continuation state is missing its " + name + " stream")
		}
		if err := st.restoreStream(s, r.Dir, name, files); err != nil {
			return err
		}
	}
	st.pass = r.State.Pass
	st.scope = scopeResult{
		Completeness:     r.State.Scalars.Completeness,
		ReasonsDropped:   r.State.Scalars.ReasonsDropped,
		ReasonsTruncated: r.State.Scalars.ReasonsTruncated,
	}
	st.scopeComplete = r.State.Scalars.ScopeComplete
	return nil
}
