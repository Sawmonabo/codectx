package context

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
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
	// AttrsComplete is P-C's edge-scan verdict. It is carried rather than
	// recomputed because P-C does not run again on the resumed call.
	AttrsComplete bool `json:"attrs_complete,omitempty"`
}

// checkpointVersion is the on-disk shape of a compile's continuation state.
const checkpointVersion = 1

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
	return relativeRuns(dir, runs)
}

// restoreRun adopts a checkpointed ALREADY-FOLDED stream and answers it as a
// sorted run again. No fold is attached: see the two-class rule at the top of
// this file.
//
// The restored sort is registered with the compile's sort area, so the merged
// output is removed on every exit path exactly as a freshly computed run is.
func restoreRun[T any](s *compileSorts, dir, name string, runs []string,
	compare func(a, b T) int,
) (*pagination.SortedRun[T], error) {
	sorter, err := restoreSort(s, dir, name, runs, compare)
	if err != nil || sorter == nil {
		return nil, err
	}
	return sortedRun(s, sorter)
}

// restoreSort adopts a checkpointed PRE-fold stream as an external sort the
// resuming pass drives to completion itself. The caller re-attaches the exact
// fold the interrupted sort carried, with WithFold, before handing it on.
//
// An empty run list answers a nil sort, which is the round trip of a carry slot
// that was not live at the checkpointed boundary.
func restoreSort[T any](s *compileSorts, dir, name string, runs []string,
	compare func(a, b T) int,
) (*pagination.ExternalSort[T], error) {
	if len(runs) == 0 {
		return nil, nil
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

// relativeRuns turns the absolute paths a sort wrote into names relative to the
// state directory. Paths are stored relative because the directory is RENAMED
// when the spool store adopts it: an absolute path recorded before the rename
// names a directory that no longer exists, which would turn every resume into
// an expired continuation.
func relativeRuns(dir string, runs []string) ([]string, error) {
	out := make([]string, 0, len(runs))
	for _, r := range runs {
		rel, err := filepath.Rel(dir, r)
		if err != nil {
			return nil, &model.Error{Code: model.CodeInternal, Message: "context checkpoint: " + err.Error()}
		}
		out = append(out, rel)
	}
	slices.Sort(out)
	return out, nil
}

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
// stream's prefix, and answers their names relative to it. A failure part way
// through leaves the already-moved files in the directory, which the lease
// reclaims: the caller's answer is an uninterrupted compile or an error, never
// a checkpoint that names files it does not have.
func moveRuns(dir, name string, runs []string) ([]string, error) {
	out := make([]string, 0, len(runs))
	for i, r := range runs {
		base := checkpointPrefix(name) + strconv.Itoa(i)
		if err := os.Rename(r, filepath.Join(dir, base)); err != nil {
			return nil, &model.Error{Code: model.CodeInternal, Message: "context checkpoint: " + err.Error()}
		}
		out = append(out, base)
	}
	return out, nil
}
