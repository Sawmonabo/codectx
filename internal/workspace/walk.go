package workspace

import (
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

const (
	// dirBatchEntries is how many children one ReadDir call requests. It is a
	// batching constant, not a bound: the loop keeps calling until the
	// directory is exhausted, so no directory size is refused. It exists so a
	// directory with millions of children is not requested in a single
	// allocation sized by a caller-supplied ceiling.
	dirBatchEntries = 4096
	// cancelCheckInterval is how often the walk polls cancellation while
	// emitting files from one directory.
	cancelCheckInterval = 256
)

// Skip reasons reported through Policy.OnSkip. They are stable strings: the
// snapshot builder folds them into its capture notes and the operator log, so
// renaming one changes an operator-visible field.
const (
	// SkipPathTooLong is one path longer than model.MaxPathBytes. The path is
	// skipped; the walk continues. It is reported with the over-long path
	// truncated to the bound, because the identity of the file that was not
	// captured is the whole point of the report.
	SkipPathTooLong = "path_too_long"
	// SkipFileBudget is the first file past a user-set Policy.MaxFiles. The
	// file is still emitted -- exceeding a user-set limit is reported, never
	// clamped -- and the report names the limit that was passed.
	SkipFileBudget = "file_budget_exceeded"
	// SkipDirEntries is a directory holding more than a user-set
	// Policy.MaxDirEntries children. Every child is still listed.
	SkipDirEntries = "dir_entries_exceeded"
	// SkipDepth is a directory nested deeper than a user-set Policy.MaxDepth.
	// It is still descended into.
	SkipDepth = "depth_exceeded"
)

// vendorDirs are the directory names excluded when index_vendor is false. The
// list is deliberately small, fixed and documented in docs/configuration.md:
// it is the definition of what that setting means, so it may not drift into an
// undocumented heuristic.
var vendorDirs = map[string]bool{
	"vendor":           true,
	"node_modules":     true,
	"third_party":      true,
	"bower_components": true,
	"Godeps":           true,
}

// generatedDirs and generatedSuffixes are the built-in classification used when
// index_generated is false. Classification is by path only: no file is opened
// to guess at its provenance, because opening every candidate to sniff for a
// generated-code banner would read the whole repository before capture.
var generatedDirs = map[string]bool{
	"__pycache__": true,
	".next":       true,
}

var generatedSuffixes = []string{
	".pb.go",
	".pb.cc",
	".pb.h",
	"_pb2.py",
	"_pb2_grpc.py",
	".generated.go",
	"_generated.go",
	".g.dart",
	".freezed.dart",
	".min.js",
	".min.css",
}

// Policy is the traversal policy for one walk. The toggles come from
// configuration; the two hooks come from the Git owner, which is the only
// component that knows what the repository tracks and ignores.
type Policy struct {
	// FollowSymlinks opts into in-root symlink traversal. Even then a symlink
	// that resolves outside the workspace is rejected, not skipped.
	FollowSymlinks bool
	IndexVendor    bool
	IndexGenerated bool
	// MaxFiles bounds the number of emitted files. Zero means unlimited, the
	// default: a repository is never refused for its size. A user-set bound
	// that is exceeded is reported through OnSkip once and the walk continues
	// to completion -- it is never a clamp and never a refusal.
	//
	// The type is int64 rather than config.Limit only because internal/config
	// imports this package (Config.TraversalPolicy), so the dependency cannot
	// run the other way. exceeds() below is the single place the zero-means-
	// unlimited comparison is written; no caller repeats it.
	MaxFiles int64
	// MaxDirEntries bounds how many children one directory may hold and
	// MaxDepth how deeply directories may nest. Both are zero -- unlimited --
	// by default: one generated directory or one deep vendored tree must not
	// refuse a capture. A user-set bound that is exceeded is reported through
	// OnSkip and the traversal continues.
	MaxDirEntries int64
	MaxDepth      int64
	// MaxIgnoredRoots is carried for the traversal-policy builder, which asks
	// Git for the worktree's outermost ignored paths; Walk does not interpret
	// it, exactly as it does not interpret IncludeUntracked. It travels on
	// Policy because that is the one value every traversing component already
	// shares. Zero means unlimited.
	MaxIgnoredRoots int64
	// OnSkip receives every path the traversal could not emit, or emitted past
	// a user-set bound, with one of the Skip* reasons. It is the walk's only
	// report channel: without it such a path is lost, which is why the snapshot
	// builder always supplies one. It must not retain rel beyond the call and
	// must not return an error; a report never fails a walk.
	OnSkip func(rel, reason string)
	// DataDir is the absolute data directory. When it lies inside the
	// workspace it is excluded unconditionally, along with ".git": neither is
	// repository source, and indexing the store while writing it would be a
	// feedback loop.
	DataDir string
	// Ignore reports whether a path is ignored. It is supplied by the Git
	// owner, which evaluates real ignore rules through Git plumbing; this
	// package deliberately contains no ignore-pattern parser.
	Ignore func(rel string, isDir bool) bool
	// ForceInclude reports whether a path must be emitted regardless of Ignore
	// and of the vendor and generated toggles. Section 10.2 requires tracked
	// files to win over an ignore match. It cannot override the unconditional
	// exclusions or any safety rule: neither .git, nor the data directory, nor
	// a symlink escape, nor the file budget is negotiable.
	ForceInclude func(rel string) bool
	// ForceIncludeDir answers the same question for a whole directory: may
	// anything beneath relDir be forced back in? An excluded directory is read
	// only when this says yes, so "vendor" with index_vendor disabled is not
	// traversed at all unless a tracked path actually lies inside it. Without
	// it, a single ForceInclude hook would turn every excluded directory into a
	// full traversal, and a huge ignored directory could abort the walk on the
	// entry cap alone.
	//
	// It is asked about every directory on the way down, not only the outermost
	// excluded one, so it must answer true for each ancestor of a forced path:
	// a hook that recognizes "vendor" but not "vendor/sub" prunes the subtree
	// and drops the tracked file beneath it.
	ForceIncludeDir func(relDir string) bool
	// IncludeUntracked is carried for the Git owner, which decides from it
	// whether an untracked file is eligible. Walk does not interpret it: it
	// cannot know what Git tracks, and guessing would contradict the snapshot.
	IncludeUntracked bool
}

// ExcludeDir reports whether the directory named name at root-relative path rel
// is excluded by this policy: the built-in vendor and generated classification
// and the Git ignore hook, in that order. It does not answer the unconditional
// exclusions (the Git directory and the data directory), which are not
// policy-negotiable and are applied by the traversal itself, and it does not
// consider ForceIncludeDir, which asks the different question of whether an
// excluded directory may still hold a forced path.
//
// It is exported because the watcher derives its watch set from the same
// classification. A second copy of these rules elsewhere would let the set of
// directories a capture reads and the set a watcher watches drift apart
// silently, which is a missed change reported as fresh coverage.
func (p Policy) ExcludeDir(rel, name string) bool {
	if !p.IndexVendor && vendorDirs[name] {
		return true
	}
	if !p.IndexGenerated && generatedDirs[name] {
		return true
	}
	return p.Ignore != nil && p.Ignore(rel, true)
}

// Walk streams every eligible file in deterministic order, calling visit once
// per file. It retains no repository-wide file list: only the entries of the
// directories on the current path are held at any moment.
//
// Order is the lexicographic order of the root-relative paths themselves. That
// is not the same as sorting each directory's entry names: "a.txt" precedes
// "a/b" because "." sorts before "/", while the names "a" and "a.txt" sort the
// other way. Each level is therefore sorted by the name a directory contributes
// to a path, which is its name followed by the separator.
func Walk(ctx context.Context, root Root, policy Policy, visit func(File) error) error {
	if root.root == nil {
		return notFound("the workspace is not open")
	}
	if policy.MaxFiles < 0 || policy.MaxDirEntries < 0 || policy.MaxDepth < 0 {
		return resourceLimit("a traversal bound is negative; zero means unlimited")
	}
	w := &walker{root: root, policy: policy, visit: visit, dataDirRel: dataDirRelative(root.Path, policy.DataDir)}
	if w.dataDirRel == "." {
		return resourceLimit("the data directory is the workspace root; no file would be eligible")
	}
	return w.walkDir(ctx, ".", 0, false)
}

// WalkDirs streams every directory this traversal would descend into, starting
// with the root ".", in the same order Walk visits them. No file is inspected:
// a directory is emitted because the policy admits it, not because it already
// holds an eligible file, so a directory that is empty today is still emitted.
//
// That is the difference the watcher needs. Deriving a watch set from Walk
// yields only the ancestors of the files that exist at that moment, so the
// first file to appear in an empty admitted directory produces no notification
// at all. The set WalkDirs yields is a strict superset of those ancestors --
// every emitted file was reached by descending through its ancestors, and each
// of those directories is emitted here -- so it can only widen coverage, never
// narrow it.
//
// An excluded directory that ForceIncludeDir claims may hold a forced path is
// emitted too, for the same reason Walk descends into it: a tracked file inside
// an ignored tree is eligible, so its directory must be watched.
//
// The traversal applies the same user-set depth and per-directory entry bounds
// as Walk, which report rather than refuse; the file budget does not apply
// because no file is emitted. A
// caller that needs a bound on the number of directories imposes it by
// returning an error from visit, which stops the traversal.
func WalkDirs(ctx context.Context, root Root, policy Policy, visit func(relDir string) error) error {
	if root.root == nil {
		return notFound("the workspace is not open")
	}
	w := &walker{root: root, policy: policy, visitDir: visit, dataDirRel: dataDirRelative(root.Path, policy.DataDir)}
	if w.dataDirRel == "." {
		return resourceLimit("the data directory is the workspace root; no file would be eligible")
	}
	if err := visit("."); err != nil {
		return err
	}
	return w.walkDir(ctx, ".", 0, false)
}

type walker struct {
	root   Root
	policy Policy
	visit  func(File) error
	// visitDir, when set, makes this a directory-only traversal: every
	// directory the walk descends into is reported and no file is emitted.
	visitDir   func(string) error
	dataDirRel string
	emitted    int64
	seen       int
	// reported marks the one-shot reports: a user-set bound is announced the
	// first time it is passed, not once per file, so a 300 000-file walk over a
	// 200 000-file budget produces one report and not 100 000.
	reported map[string]bool
	// ancestors holds the directories on the current path so a followed symlink
	// that re-enters one is detected instead of looping forever. It is filled
	// only when the policy follows symlinks: without them no entry can name a
	// directory already on the path, so the stat per level that fills it buys
	// nothing. It holds one FileInfo per level of the CURRENT path, never one
	// per directory child.
	ancestors []fs.FileInfo
	// sortBuf is how many entries one level may hold before the level spills to
	// a sorted run on disk. Zero takes dirSortBufferEntries.
	sortBuf int
	// live is the number of entries the open levels retain right now and
	// peakLive its high-water mark, including the level currently being read.
	// They are the walk's memory invariant: a test asserts peakLive stays
	// inside the envelope the sort buffer defines however wide a directory is.
	live     int
	peakLive int
}

// dirSortBufferEntries is how many children one directory level may hold in
// memory. A level wider than this is sorted through the external merge sort,
// which keeps the live set at this many entries plus the merge's fan-in
// whatever the directory's width -- so a flat directory of 300 000 children
// costs this buffer and a spill file, not 300 000 resident entries.
//
// It is not a limit: no directory is refused, skipped or truncated for its
// width. It is the point at which the level stops being free.
const dirSortBufferEntries = 4096

// observeLive records the live-entry high-water mark.
func (w *walker) observeLive(n int) {
	if n > w.peakLive {
		w.peakLive = n
	}
}

// entry is one directory child with the ordering key its path contributes.
//
// It is a flat value on purpose: an fs.FileInfo retains the operating system's
// stat structure (and, on some platforms, the name it was read with), so a
// level of 300 000 children held one each is a repository-sized heap structure.
// The four scalars below are everything the traversal and File need, so an
// entry costs its two names and 32 bytes, and it survives a round trip through
// a spilled sort run byte for byte.
type entry struct {
	name    string
	sortKey string
	isDir   bool
	mode    fs.FileMode
	size    int64
	modTime int64
}

// encodeEntry and decodeEntry are the codec the external sort spills with. The
// encoding is length-prefixed and little-endian, and it round-trips sortKey
// byte for byte, which is what keeps a spilled level's order identical to a
// level that fit in memory.
func encodeEntry(e entry) ([]byte, error) {
	buf := make([]byte, 0, len(e.name)+len(e.sortKey)+32)
	buf = binary.AppendUvarint(buf, uint64(len(e.name)))
	buf = append(buf, e.name...)
	buf = binary.AppendUvarint(buf, uint64(len(e.sortKey)))
	buf = append(buf, e.sortKey...)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(e.mode))
	buf = binary.LittleEndian.AppendUint64(buf, uint64(e.size))
	buf = binary.LittleEndian.AppendUint64(buf, uint64(e.modTime))
	return buf, nil
}

func decodeEntry(b []byte) (entry, error) {
	var e entry
	for _, field := range []*string{&e.name, &e.sortKey} {
		n, w := binary.Uvarint(b)
		if w <= 0 || uint64(len(b)-w) < n {
			return entry{}, internalError("a spilled directory entry is truncated")
		}
		b = b[w:]
		*field = string(b[:n])
		b = b[n:]
	}
	if len(b) != 20 {
		return entry{}, internalError("a spilled directory entry is truncated")
	}
	e.mode = fs.FileMode(binary.LittleEndian.Uint32(b))
	e.size = int64(binary.LittleEndian.Uint64(b[4:]))
	e.modTime = int64(binary.LittleEndian.Uint64(b[12:]))
	e.isDir = e.mode.IsDir()
	return e, nil
}

// entryRun is one directory level in sorted order. Both implementations are
// read exactly once and closed by the level that opened them.
type entryRun interface {
	Each(yield func(entry) error) error
	Close() error
}

// memRun is the level that fit in the sort buffer: no file was created, so
// there is nothing to remove.
type memRun struct{ entries []entry }

func (r *memRun) Each(yield func(entry) error) error {
	for _, e := range r.entries {
		if err := yield(e); err != nil {
			return err
		}
	}
	return nil
}

func (r *memRun) Close() error { r.entries = nil; return nil }

// walkDir emits one directory level. excluded marks a subtree that policy
// excluded but that is still traversed because a forced include may live in it.
func (w *walker) walkDir(ctx context.Context, rel string, depth int64, excluded bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if exceeds(w.policy.MaxDepth, depth) {
		// Reported, not refused: the traversal continues into the deeper tree.
		// The symlink cycle check below still compares against every ancestor,
		// so cycle safety does not depend on this bound.
		w.reportOnce(rel, SkipDepth)
	}
	if w.policy.FollowSymlinks && rel != "." {
		// Only a followed symlink can name a directory already on the path, so
		// this is the only policy under which the ancestor stack is worth its
		// stat. The stat goes through the confined handle, so a link that
		// resolves outside the workspace fails here rather than being walked.
		dirInfo, err := w.root.root.Stat(filepath.FromSlash(rel))
		if err != nil {
			return pathError("%q cannot be inspected: %v", rel, err)
		}
		for _, a := range w.ancestors {
			if os.SameFile(a, dirInfo) {
				return pathError("%q re-enters a directory already on the path; the workspace contains a symlink cycle", rel)
			}
		}
		w.ancestors = append(w.ancestors, dirInfo)
		defer func() { w.ancestors = w.ancestors[:len(w.ancestors)-1] }()
	}

	run, retained, err := w.readDir(rel)
	if err != nil {
		return err
	}
	// The level's retained entries are live for the whole subtree beneath it,
	// because the recursion happens inside the walk over them; a spilled level
	// retains none, since its records are read from the run one at a time.
	w.live += retained
	w.observeLive(w.live)
	defer func() {
		w.live -= retained
		run.Close()
	}()

	return run.Each(func(e entry) error {
		childRel := e.name
		if rel != "." {
			childRel = rel + "/" + e.name
		}
		return w.visitEntry(ctx, childRel, e, depth+1, excluded)
	})
}

func (w *walker) visitEntry(ctx context.Context, rel string, e entry, depth int64, excluded bool) error {
	if e.name == gitDirName || rel == w.dataDirRel {
		// Unconditional: not source, and not subject to any include hook.
		return nil
	}
	if e.isDir {
		childExcluded := excluded || w.policy.ExcludeDir(rel, e.name)
		if childExcluded && !w.mayForceInside(rel) {
			// Nothing inside can be forced back in, so the subtree is not read
			// at all: not listed, not counted against the entry cap, not a
			// source of errors.
			return nil
		}
		if w.visitDir != nil {
			if err := w.visitDir(rel); err != nil {
				return err
			}
		}
		return w.walkDir(ctx, rel, depth, childExcluded)
	}
	if w.visitDir != nil {
		// A directory-only traversal: files are neither classified nor emitted,
		// and the file budget is not charged.
		return nil
	}
	if !e.mode.IsRegular() {
		// Devices, sockets, pipes and unresolved links are not source bytes.
		return nil
	}
	if excluded || w.fileExcluded(rel) {
		// Only a file policy would drop is worth asking the Git owner about;
		// an eligible file costs no lookup.
		if w.policy.ForceInclude == nil || !w.policy.ForceInclude(rel) {
			return nil
		}
	}
	w.seen++
	if w.seen%cancelCheckInterval == 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	w.emitted++
	if exceeds(w.policy.MaxFiles, w.emitted) {
		w.reportOnce(rel, SkipFileBudget)
	}
	return w.visit(File{
		Path:    rel,
		Size:    e.size,
		Mode:    e.mode,
		ModTime: e.modTime,
	})
}

// mayForceInside reports whether the policy claims some path beneath an
// excluded directory must still be visited.
func (w *walker) mayForceInside(relDir string) bool {
	return w.policy.ForceIncludeDir != nil && w.policy.ForceIncludeDir(relDir)
}

func (w *walker) fileExcluded(rel string) bool {
	if !w.policy.IndexGenerated && isGeneratedPath(rel) {
		return true
	}
	return w.policy.Ignore != nil && w.policy.Ignore(rel, false)
}

func isGeneratedPath(rel string) bool {
	base := path.Base(rel)
	for _, suffix := range generatedSuffixes {
		if strings.HasSuffix(base, suffix) {
			return true
		}
	}
	return false
}

// readDir lists one directory through the confined handle, resolves each
// child's kind and answers the level in the deterministic order Walk
// documents. A symlink is skipped unless the policy opts into following one,
// and a followed symlink that leaves the workspace fails the walk.
//
// The listing is drawn in bounded batches and the entries stream into a sort
// that spills to disk past sortBuf records, so neither the listing nor the
// ordering holds the level's whole width in heap. That is the difference a
// flat repository makes: one directory of 300 000 children costs sortBuf
// resident entries plus the merge's fan-in, and the rest of the level lives in
// a spill file that is removed when the level is done with. Nothing about the
// order changes -- the comparator, the keys and the codec are the same whether
// a level spilled or not, so a spilled level yields exactly the sequence the
// in-memory sort would have -- and no directory is refused for its width.
//
// retained is how many entries the returned run holds in heap for as long as
// the caller walks it: the level's own entries when it fit the buffer, and
// none when it spilled.
func (w *walker) readDir(rel string) (run entryRun, retained int, err error) {
	native := "."
	if rel != "." {
		native = filepath.FromSlash(rel)
	}
	f, err := w.root.root.Open(native)
	if err != nil {
		return nil, 0, pathError("%q cannot be listed: %v", rel, err)
	}
	defer f.Close()

	bufN := w.sortBuf
	if bufN <= 0 {
		bufN = dirSortBufferEntries
	}
	// Deliberately not pre-sized: sizing from the buffer constant would floor
	// every directory level of the descent at that many entries, which for a
	// tree of small directories is more memory than a right-sized allocation.
	var buf []entry
	var sorter *pagination.ExternalSort[entry]
	defer func() {
		if sorter != nil && run == nil {
			sorter.Close()
		}
	}()
	// add takes one resolved entry, spilling the level to the sort the first
	// time the buffer would be exceeded. The sort is created lazily so an
	// ordinary directory creates no file and touches no temporary directory.
	add := func(e entry) error {
		if sorter == nil && len(buf) < bufN {
			buf = append(buf, e)
			return nil
		}
		if sorter == nil {
			s, err := pagination.NewExternalSort[entry](w.sortDir(), bufN,
				encodeEntry, decodeEntry, func(a, b entry) int { return cmp.Compare(a.sortKey, b.sortKey) })
			if err != nil {
				return err
			}
			sorter = s
			for _, held := range buf {
				if err := sorter.Add(held); err != nil {
					return err
				}
			}
			buf = nil
		}
		return sorter.Add(e)
	}

	var listed int64
	for {
		batch, err := f.ReadDir(dirBatchEntries)
		// ReadDir(n > 0) reports io.EOF once the directory is exhausted; that
		// is the end of the listing, not a failure.
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, 0, pathError("%q cannot be listed: %v", rel, err)
		}
		listed += int64(len(batch))
		if exceeds(w.policy.MaxDirEntries, listed) {
			w.reportOnce(rel, SkipDirEntries)
		}
		if err := w.appendEntries(rel, batch, add); err != nil {
			return nil, 0, err
		}
		w.observeLive(w.live + len(buf))
		if len(batch) == 0 || errors.Is(err, io.EOF) {
			break
		}
	}
	if sorter == nil {
		slices.SortFunc(buf, func(a, b entry) int { return cmp.Compare(a.sortKey, b.sortKey) })
		return &memRun{entries: buf}, len(buf), nil
	}
	sorted, err := sorter.Sorted()
	if err != nil {
		return nil, 0, err
	}
	w.observeLive(w.live + sorter.PeakLiveRecords())
	return &spilledRun{sorted: sorted, sorter: sorter}, 0, nil
}

// spilledRun is a level that outgrew the sort buffer. Closing it removes both
// the merged output and the sort's own run files.
type spilledRun struct {
	sorted *pagination.SortedRun[entry]
	sorter *pagination.ExternalSort[entry]
}

func (r *spilledRun) Each(yield func(entry) error) error { return r.sorted.Each(yield) }

func (r *spilledRun) Close() error {
	err := r.sorted.Close()
	if cerr := r.sorter.Close(); err == nil {
		err = cerr
	}
	return err
}

// sortDir is where a level that outgrew its buffer spills. The data directory
// is preferred because it is the one place this application already owns and
// the traversal excludes it from itself; a Walk configured without one -- the
// library caller with no configuration -- falls back to the system temporary
// directory. Files are created 0600 under a 0700 directory and removed when
// the level is done with, so nothing outlives the walk.
func (w *walker) sortDir() string {
	if w.policy.DataDir != "" {
		return filepath.Join(w.policy.DataDir, "walk-sort")
	}
	return filepath.Join(os.TempDir(), "codectx-walk-sort")
}

// appendEntries resolves one ReadDir batch into the level's ordered run. No
// fs.FileInfo outlives the batch: what the entry keeps is the four scalars
// File is built from.
func (w *walker) appendEntries(rel string, names []os.DirEntry, add func(entry) error) error {
	for _, d := range names {
		if w.visitDir != nil && d.Type()&fs.ModeSymlink == 0 && !d.IsDir() {
			// A directory-only traversal does not stat files: the kind the
			// listing already reported is enough to drop them, and stating
			// every file of the repository is precisely the cost this
			// traversal exists to avoid. Only a symlink still needs resolving,
			// because it may name a directory.
			continue
		}
		childRel := d.Name()
		if rel != "." {
			childRel = rel + "/" + d.Name()
		}
		if len(childRel) > model.MaxPathBytes {
			// One path, skipped and reported -- never the whole walk. The
			// reported path is truncated to the bound so the report itself
			// carries no unbounded string.
			w.report(childRel[:model.MaxPathBytes], SkipPathTooLong)
			continue
		}
		info, err := w.resolve(childRel, d)
		if err != nil {
			return err
		}
		if info == nil {
			continue
		}
		key := d.Name()
		if info.IsDir() {
			key += "/"
		}
		if err := add(entry{
			name: d.Name(), sortKey: key, isDir: info.IsDir(),
			mode: info.Mode(), size: info.Size(), modTime: info.ModTime().UnixNano(),
		}); err != nil {
			return err
		}
	}
	return nil
}

// exceeds is the one place the zero-means-unlimited comparison is written in
// this package. It mirrors config.Limit.Exceeded exactly -- strictly greater,
// unlimited at zero -- which this package cannot call because internal/config
// imports it.
func exceeds(bound, n int64) bool { return bound > 0 && n > bound }

// report hands one skipped or over-bound path to the policy's sink. A walk
// without a sink loses the report, which is why every production caller sets
// one; the nil check keeps a test or a tool that only wants the file stream
// from having to.
func (w *walker) report(rel, reason string) {
	if w.policy.OnSkip != nil {
		w.policy.OnSkip(rel, reason)
	}
}

// reportOnce announces a user-set bound the first time it is passed. The bound
// is not enforced, so without this the report would repeat for every remaining
// entry of the traversal.
func (w *walker) reportOnce(rel, reason string) {
	if w.reported[reason] {
		return
	}
	if w.reported == nil {
		w.reported = map[string]bool{}
	}
	w.reported[reason] = true
	w.report(rel, reason)
}

// resolve returns the metadata the walk should use for one child, or nil when
// the child is skipped. A nil result is never an error the caller should hide:
// it means the entry is not eligible source, such as a symlink under the
// default policy.
func (w *walker) resolve(rel string, d fs.DirEntry) (fs.FileInfo, error) {
	if d.Type()&fs.ModeSymlink == 0 {
		info, err := d.Info()
		if err != nil {
			if os.IsNotExist(err) {
				// The entry disappeared between the listing and the stat.
				// Capture reconciliation detects that; the walk does not.
				return nil, nil
			}
			return nil, pathError("%q cannot be inspected: %v", rel, err)
		}
		return info, nil
	}
	if !w.policy.FollowSymlinks {
		return nil, nil
	}
	// Stat through the confined handle: it resolves the link only within the
	// workspace, so a target outside it fails here rather than being read.
	info, err := w.root.root.Stat(filepath.FromSlash(rel))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, pathError("%q is a symbolic link that does not resolve inside the workspace: %v", rel, err)
	}
	return info, nil
}

// dataDirRelative returns the data directory's root-relative path when it lies
// inside the workspace, or the empty string when it does not.
func dataDirRelative(rootPath, dataDir string) string {
	if dataDir == "" {
		return ""
	}
	rel, err := filepath.Rel(rootPath, dataDir)
	if err != nil {
		return ""
	}
	slashed := filepath.ToSlash(rel)
	if slashed == ".." || strings.HasPrefix(slashed, "../") {
		return ""
	}
	return slashed
}

// ExclusionDigest is a stable digest of the built-in vendor and generated
// classification lists.
//
// These lists decide which files exist as far as the rest of the system is
// concerned, exactly like the configured toggles that switch them on and off.
// A build that ships a different list therefore produces a different capture
// from the same bytes and the same configuration, and the source fingerprint
// has to say so; otherwise a stored snapshot would be reused under a policy it
// was not captured under.
func ExclusionDigest() string {
	h := model.NewHasher(domainExclusions)
	for _, set := range []map[string]bool{vendorDirs, generatedDirs} {
		names := make([]string, 0, len(set))
		for name := range set {
			names = append(names, name)
		}
		slices.Sort(names)
		h.AddString(strconv.Itoa(len(names)))
		for _, name := range names {
			h.AddString(name)
		}
	}
	suffixes := append([]string(nil), generatedSuffixes...)
	slices.Sort(suffixes)
	h.AddString(strconv.Itoa(len(suffixes)))
	for _, suffix := range suffixes {
		h.AddString(suffix)
	}
	return h.Sum()
}

// domainExclusions keeps the digest in its own hash domain, so it can never
// collide with another fingerprint over the same strings.
const domainExclusions = "workspace-exclusions-v1"
