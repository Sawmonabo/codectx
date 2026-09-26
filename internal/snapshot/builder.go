package snapshot

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"time"

	"github.com/Sawmonabo/codectx/internal/lang"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/vcs/git"
	"github.com/Sawmonabo/codectx/internal/workspace"
)

const (
	// maxRetries is the Section 10.2 bound: a changing file or membership is
	// recaptured at most twice before the capture is reported unstable.
	// DefaultRetryDeadline bounds validated capture when no retry budget is
	// set. It is a wall clock, not a count: a busy monorepo whose worktree
	// settles on the fifth pass must capture, and one that never settles must
	// still end. Exported so an operator-facing caller can raise it.
	DefaultRetryDeadline = 10 * time.Minute
	// domainManifest is the hash domain of the canonical sorted manifest whose
	// aggregate digest feeds SnapshotID (Section 9.1).
	domainManifest = "source-manifest-v1"
	// maxNotePaths bounds every path list a Notes carries; the count beside
	// it is complete.
	maxNotePaths = 32
	// lfsPointerPrefix is the first line of a Git LFS pointer file (spec v1).
	lfsPointerPrefix = "version https://git-lfs.github.com/spec/v1"
)

// Store is the write side of storage the builder needs: it persists each
// blob's metadata before the manifest that names it, and imports the streamed
// manifest. Blob lets a capture skip re-persisting a blob the store already
// holds. *sqlite.Store satisfies it.
type Store interface {
	Blob(ctx context.Context, hash string) (model.BlobRecord, error)
	PutBlob(ctx context.Context, b model.BlobRecord) error
	PutSnapshot(ctx context.Context, snap model.Snapshot, files func(yield func(model.FileVersion) error) error) error
}

// Notes are the explicit capability observations of one capture (Section
// 10.2): conditions under which the snapshot is exact but narrower than a
// reader might assume. Nothing here is inferred into the snapshot itself.
type Notes struct {
	// SparseCheckout means tracked paths absent from the worktree were never
	// checked out; they are not recorded as deletions of the operator's.
	SparseCheckout bool
	// Gitmodules reports a tracked .gitmodules file.
	Gitmodules bool
	// Submodules are gitlink paths; their contents are another repository and
	// were not captured or recursed into.
	Submodules     []string
	SubmoduleCount int
	// LFSPointers are files whose captured bytes are Git LFS pointers; the
	// large content was not fetched.
	LFSPointers     []string
	LFSPointerCount int
	// SparseSkipped counts skip-worktree index entries: tracked paths a
	// sparse checkout deliberately left out. They are neither captured nor
	// recorded as deletions.
	SparseSkipped int
	// SkippedSymlinks counts symbolic links excluded by policy, including
	// tracked ones.
	SkippedSymlinks int
	// SkippedNonRegular counts tracked paths whose worktree entry is not a
	// regular file (a directory, device or socket in place of a file). The
	// file Git tracks is no longer a file at that path, so each is also a
	// deleted tombstone; the count says why.
	SkippedNonRegular int
	// ExcludedTracked counts tracked regular files the walk did not reach:
	// inside the data directory when it lies in the workspace, or behind a
	// symlinked directory component pruned under follow_symlinks = false.
	// They remain regular files, so they are neither captured nor tombstoned.
	ExcludedTracked int
	// Retries is how many validation passes found changes and recaptured.
	Retries int
	// LongPaths are paths the traversal skipped because they exceed
	// model.MaxPathBytes, truncated to that bound; LongPathCount is complete.
	// Each is one file that is not in the snapshot: the walk reports it rather
	// than failing, so this is the only place it is visible.
	LongPaths     []string
	LongPathCount int
	// BoundsExceeded names every user-set bound this capture passed, each at
	// most once: the workspace.Policy traversal bounds by their Skip* reason
	// and the Git listing bounds by their git.Bound* name. Passing a bound is
	// reported and the capture completes; nothing is dropped or clamped.
	BoundsExceeded []string
}

func (n *Notes) addLongPath(path string) {
	n.LongPathCount++
	if len(n.LongPaths) < maxNotePaths {
		n.LongPaths = append(n.LongPaths, path)
	}
}

// addBound records a passed bound once. The list is bounded by the number of
// distinct bound names, which is fixed by the code, so it needs no cap.
func (n *Notes) addBound(name string) {
	if slices.Contains(n.BoundsExceeded, name) {
		return
	}
	n.BoundsExceeded = append(n.BoundsExceeded, name)
}

func (n *Notes) addSubmodule(path string) {
	n.SubmoduleCount++
	if len(n.Submodules) < maxNotePaths {
		n.Submodules = append(n.Submodules, path)
	}
}

func (n *Notes) addLFS(path string) {
	n.LFSPointerCount++
	if len(n.LFSPointers) < maxNotePaths {
		n.LFSPointers = append(n.LFSPointers, path)
	}
}

// Builder captures one snapshot of a workspace. Every field except Git, Lock,
// OperatorFrozen and Logger is required. It is not safe for concurrent use;
// Notes describes the most recent Build.
type Builder struct {
	Root workspace.Root
	// Policy is the traversal policy from configuration. Its DataDir is also
	// where the lock and staging live. The Git hooks are filled in here.
	Policy           workspace.Policy
	Repository       model.RepositoryID
	SourcePolicyHash string
	Store            Store
	CAS              *CAS
	// Git is required when Root.HasGit; a Git repository is never captured
	// without its membership and ignore rules.
	Git *git.Git
	// Lock is the workspace lock the caller already holds. When nil, Build
	// acquires the lock for the capture and releases it before returning. It
	// tries once: an unheld capture is a capture nobody is coordinating, and
	// there is no holder whose progress it could be waiting on.
	Lock *WorkspaceLock
	// MaxRetries bounds how many validation passes may find changes before the
	// capture is declared unstable. Zero -- the default -- means unlimited
	// retries, bounded instead by RetryDeadline: a count that refuses a busy
	// monorepo is a scale refusal, whereas a deadline ends a capture that
	// genuinely never settles. Attempts are reported in Notes.Retries either
	// way.
	MaxRetries int64
	// RetryDeadline bounds the whole validated capture. Zero selects
	// DefaultRetryDeadline; it is never unlimited, because with MaxRetries
	// unlimited it is the only thing that ends a worktree that never settles.
	RetryDeadline time.Duration
	// OperatorFrozen records that the operator supplied a quiescent or
	// OS-snapshotted source. It is never inferred: the default is
	// validated_capture.
	OperatorFrozen bool
	Logger         *slog.Logger

	// afterCapture runs after each file is read and recorded. It exists for
	// exactly one caller: the unstable-capture test, which must change a file
	// between the read and the validation pass to prove the retry bound and
	// the typed failure. No production code sets it; a sleep-based race would
	// be flaky and a filesystem hook does not exist.
	afterCapture func(rel string)
	// onBatch receives the capture's CAS batch when it is opened. It exists
	// for exactly one caller: the durability-barrier test, which installs the
	// batch's sync hook. No production code sets it.
	onBatch func(*Batch)
	notes   Notes
}

// Notes returns the observations of the most recent Build.
func (b *Builder) Notes() Notes { return b.notes }

// Build captures the workspace and imports the snapshot. On success the
// returned header names a snapshot whose every nondeleted row is retained in
// the CAS with persisted integrity metadata.
func (b *Builder) Build(ctx context.Context) (model.Snapshot, error) {
	if err := b.validate(); err != nil {
		return model.Snapshot{}, err
	}
	started := time.Now()
	b.notes = Notes{}
	// Every traversal and listing bound is reported, not enforced: these two
	// sinks are the whole reporting path from the walker and from Git into the
	// capture notes and the operator log. Without them a skipped long path or
	// a passed user-set budget would be invisible, which is the class-G defect
	// this replaces.
	b.Policy.OnSkip = func(rel, reason string) {
		if reason == workspace.SkipPathTooLong {
			b.notes.addLongPath(rel)
			return
		}
		b.notes.addBound(reason)
	}
	if b.Git != nil {
		b.Git.OnOverBound = func(bound string, _, _ int64) { b.notes.addBound(bound) }
	}
	lock := b.Lock
	if lock == nil {
		var err error
		// A capture the caller did not already hold the workspace for is what
		// this lock names itself as to whoever is refused while it runs.
		if lock, err = LockWorkspace(ctx, b.Policy.DataDir, "capture", TryOnce()); err != nil {
			return model.Snapshot{}, err
		}
		defer lock.Close()
	}
	st, err := openStaging(ctx, StagingDir(b.Policy.DataDir))
	if err != nil {
		return model.Snapshot{}, err
	}
	defer st.Close()
	// One batch for the whole capture, including its recapture passes: blobs
	// are staged as they are read and made durable once, below, immediately
	// before the snapshot that names them is committed. Discard clears the
	// temporaries of any capture that ends without reaching that barrier.
	batch := b.CAS.NewBatch()
	defer batch.Discard()
	if b.onBatch != nil {
		b.onBatch(batch)
	}

	c := &capture{b: b, st: st, batch: batch}
	head, err := c.enumerateGit(ctx)
	if err != nil {
		return model.Snapshot{}, typed(err)
	}
	deadline := b.RetryDeadline
	if deadline <= 0 {
		deadline = DefaultRetryDeadline
	}
	expiry := started.Add(deadline)
	for pass := 1; ; pass++ {
		// A detect-only pass is the last one: it compares without recapturing,
		// so a difference it finds is the unstable-capture failure. It is
		// reached when a user-set retry budget is spent or when the deadline
		// has passed, whichever comes first.
		budgetSpent := b.MaxRetries > 0 && int64(pass) >= 2+b.MaxRetries
		detectOnly := budgetSpent || (pass > 1 && !time.Now().Before(expiry))
		changes, err := c.pass(ctx, pass, detectOnly)
		if err != nil {
			return model.Snapshot{}, typed(err)
		}
		if pass > 1 && changes == 0 {
			break
		}
		if detectOnly {
			reason := "after " + strconv.Itoa(b.notes.Retries) + " recaptures within " + deadline.String()
			if budgetSpent {
				reason = "after the configured " + strconv.FormatInt(b.MaxRetries, 10) + " recaptures"
			}
			return model.Snapshot{}, &model.Error{Code: model.CodeSnapshotUnstable, Retryable: true,
				Message:     "the worktree kept changing during capture; " + strconv.Itoa(changes) + " paths differed " + reason,
				Remediation: "pause writers to the worktree and run the capture again, or supply an operator-frozen source"}
		}
		if pass > 1 {
			b.notes.Retries++
		}
	}

	snap, err := c.header(ctx, head)
	if err != nil {
		return model.Snapshot{}, err
	}
	// The durability barrier of Section 10.3: every blob the manifest below
	// names is on disk, and so is every directory entry naming one, before the
	// snapshot that names them exists. A crash before this point leaves
	// temporaries the startup sweep removes and no snapshot at all.
	if err := batch.Barrier(ctx); err != nil {
		return model.Snapshot{}, typed(err)
	}
	err = b.Store.PutSnapshot(ctx, snap, func(yield func(model.FileVersion) error) error {
		return st.eachManifest(ctx, func(r row) error { return yield(b.fileVersion(r)) })
	})
	if err != nil {
		return model.Snapshot{}, typed(err)
	}
	b.logger().Info("snapshot captured", "component", "snapshot", "repository_id", string(b.Repository),
		"snapshot_id", string(snap.ID), "file_count", snap.FileCount, "source_bytes", snap.SourceBytes,
		"retries", b.notes.Retries, "submodules", b.notes.SubmoduleCount, "lfs_pointers", b.notes.LFSPointerCount,
		"sparse_skipped", b.notes.SparseSkipped, "skipped_symlinks", b.notes.SkippedSymlinks,
		"skipped_non_regular", b.notes.SkippedNonRegular, "excluded_tracked", b.notes.ExcludedTracked,
		"long_paths", b.notes.LongPathCount, "bounds_exceeded", b.notes.BoundsExceeded)
	return snap, nil
}

func (b *Builder) validate() error {
	switch {
	case b.Root.Path == "":
		return invalid("snapshot builder needs an opened workspace root")
	case !filepath.IsAbs(b.Policy.DataDir):
		return invalid("snapshot builder needs an absolute data directory in its policy")
	case b.Policy.MaxFiles < 0 || b.MaxRetries < 0:
		return invalid("a snapshot builder bound is negative; zero means unlimited")
	case !model.ValidHexID(string(b.Repository)):
		return invalid("snapshot builder needs a repository identity")
	case !model.ValidHexID(b.SourcePolicyHash):
		return invalid("snapshot builder needs the source policy hash")
	case b.Store == nil || b.CAS == nil:
		return invalid("snapshot builder needs a store and a CAS")
	case b.Root.HasGit && b.Git == nil:
		return &model.Error{Code: model.CodeProviderUnavailable,
			Message:     "the workspace is a Git repository but no git executable is available",
			Remediation: "install git; a Git worktree is never captured without its membership and ignore rules"}
	}
	return nil
}

func (b *Builder) logger() *slog.Logger {
	if b.Logger != nil {
		return b.Logger
	}
	return slog.Default()
}

// fileVersion is the manifest row for one staged entry. Language is derived
// from the path here, deterministically, rather than stored.
func (b *Builder) fileVersion(r row) model.FileVersion {
	return model.FileVersion{
		ID:          model.NewFileID(b.Repository, r.path),
		Path:        r.path,
		Status:      r.status,
		Size:        r.size,
		ContentHash: r.hash,
		GitObjectID: r.oid,
		Language:    lang.Of(r.path),
		Executable:  r.executable,
	}
}

// capture is the state of one Build.
type capture struct {
	b  *Builder
	st *staging
	// batch stages every blob this capture retains. It spans the recapture
	// passes, so a file read twice is staged twice and published once.
	batch *Batch
	// hookErr holds the first staging failure raised inside a walk hook,
	// which can only answer with a bool.
	hookErr error
}

// enumerateGit records HEAD, the index and the repository-level observations.
// The index is listed once; later passes re-run status only, which reports
// index changes made during the capture as added or deleted paths.
func (c *capture) enumerateGit(ctx context.Context) (string, error) {
	if !c.b.Root.HasGit {
		return "", nil
	}
	g, root := c.b.Git, c.b.Root.Path
	head, err := g.Head(ctx, root)
	if err != nil {
		return "", err
	}
	if c.b.notes.SparseCheckout, err = g.SparseCheckout(ctx, root); err != nil {
		return "", err
	}
	err = g.ListIndex(ctx, root, c.b.Policy.MaxFiles, func(e git.IndexEntry) error {
		if e.Path == ".gitmodules" {
			c.b.notes.Gitmodules = true
		}
		if e.SkipWorktree {
			// A sparse checkout never materialized this path. Staging does
			// not learn it, so its absence is neither a deletion nor a
			// forced include; the count reports it.
			c.b.notes.SparseSkipped++
			return nil
		}
		return c.st.putIndex(ctx, e.Path, e.Mode, e.ObjectID)
	})
	return head, err
}

// pass runs one capture or validation pass: refresh Git status, walk the
// worktree capturing what is new or changed, then account for every staged
// path the walk did not reach. It returns how many paths changed membership
// or bytes. With detectOnly the changes are counted but nothing is captured
// or altered, which is how the final pass judges stability without a third
// recapture.
func (c *capture) pass(ctx context.Context, pass int, detectOnly bool) (int, error) {
	b := c.b
	// The unseen sweep below recomputes these from scratch every pass.
	b.notes.SkippedSymlinks, b.notes.SkippedNonRegular, b.notes.ExcludedTracked = 0, 0, 0
	// Every validation pass walks the tree again and reports the same skips, so
	// these are reset here with the other per-pass counters: without it a
	// two-pass capture would claim twice as many skipped paths as there are.
	b.notes.LongPaths, b.notes.LongPathCount = nil, 0
	b.notes.BoundsExceeded = nil
	b.notes.Submodules, b.notes.SubmoduleCount = nil, 0
	changes := 0
	policy := b.Policy
	if b.Root.HasGit {
		if err := c.st.resetChanges(ctx); err != nil {
			return 0, err
		}
		err := b.Git.Status(ctx, b.Root.Path, policy.IncludeUntracked, policy.MaxFiles, func(ch git.Change) error {
			return c.st.putChange(ctx, ch.Path, string(ch.Kind), ch.HeadObjectID)
		})
		if err != nil {
			return 0, err
		}
		policy.Ignore = func(rel string, isDir bool) bool {
			if isDir {
				return !c.hook(c.st.knownUnder(ctx, rel))
			}
			return !c.hook(c.st.known(ctx, rel))
		}
		policy.ForceInclude = func(rel string) bool { return c.hook(c.st.forced(ctx, rel)) }
		policy.ForceIncludeDir = func(rel string) bool { return c.hook(c.st.forcedUnder(ctx, rel)) }
	}

	err := workspace.Walk(ctx, b.Root, policy, func(f workspace.File) error {
		if c.hookErr != nil {
			return c.hookErr
		}
		r, found, err := c.st.get(ctx, f.Path)
		if err != nil {
			return err
		}
		status, eligible := c.deriveStatus(r, found)
		if !eligible {
			return nil
		}
		// chmod does not touch mtime, so the executable bit is compared too.
		unchanged := found && r.status != "" && r.status != model.FileDeleted &&
			r.size == f.Size && r.mtime == f.ModTime && r.executable == isExecutable(f.Mode, r.mode)
		if unchanged {
			if r.status == status {
				return c.st.markSeen(ctx, f.Path, pass)
			}
			// Same bytes, different Git label: the status refresh moved it.
			return c.st.relabel(ctx, f.Path, status, pass)
		}
		changes++
		if detectOnly {
			return nil
		}
		return c.captureFile(ctx, f, r, status, pass, &changes)
	})
	if err != nil {
		if c.hookErr != nil {
			return 0, c.hookErr
		}
		return 0, err
	}
	// Everything staged that the walk did not emit: tombstones, paths that
	// left the manifest, and the entries policy keeps out.
	err = c.st.eachUnseen(ctx, pass, func(r row) error {
		return c.reconcileUnseen(ctx, r, pass, detectOnly, &changes)
	})
	return changes, err
}

// hook adapts a staging lookup to a walk hook's bool, retaining the error.
func (c *capture) hook(ok bool, err error) bool {
	if err != nil && c.hookErr == nil {
		c.hookErr = err
	}
	return ok && err == nil
}

// deriveStatus maps a staged entry's Git view to the manifest vocabulary for a
// path the walk found on disk. Without Git nothing tracks the files, so they
// are untracked.
func (c *capture) deriveStatus(r row, found bool) (model.FileStatus, bool) {
	if !c.b.Root.HasGit {
		return model.FileUntracked, true
	}
	if !found {
		return "", false
	}
	switch git.ChangeKind(r.change) {
	case git.ChangeUntracked:
		return model.FileUntracked, true
	case git.ChangeAdded:
		return model.FileAdded, true
	case git.ChangeModified:
		return model.FileModified, true
	case git.ChangeDeleted:
		// Git reports it gone but the walk found bytes at the path: HEAD's
		// version is not what is there.
		return model.FileModified, true
	}
	if r.tracked {
		return model.FileTracked, true
	}
	return "", false
}

// captureFile reads one file through the confined opener into the CAS,
// persists its metadata and records the outcome. The stat after the read is
// the baseline the next pass compares against; a size or modification time
// that moved during the read counts as a change so the file is read again.
func (c *capture) captureFile(ctx context.Context, f workspace.File, r row, status model.FileStatus, pass int, changes *int) error {
	b := c.b
	file, err := b.Root.Open(f.Path)
	if err != nil {
		if _, gone, ierr := c.inspect(f.Path); ierr == nil && gone {
			// Deleted between the listing and the open; the unseen sweep
			// records it.
			return nil
		}
		return err
	}
	defer file.Close()
	sniff := &headSniffer{r: file}
	rec, err := c.batch.Put(ctx, sniff)
	if err != nil {
		return err
	}
	post, gone, err := c.inspect(f.Path)
	if err != nil || gone {
		return err
	}
	if post.Size() != rec.Size || post.ModTime().UnixNano() != f.ModTime {
		*changes++
	}
	if _, err := b.Store.Blob(ctx, rec.Hash); err != nil {
		if err := b.Store.PutBlob(ctx, rec); err != nil {
			return err
		}
	}
	if err := c.st.captured(ctx, row{path: f.Path, status: status, hash: rec.Hash, size: rec.Size,
		executable: isExecutable(post.Mode(), r.mode), mtime: post.ModTime().UnixNano(), lfs: sniff.isLFSPointer()}, pass); err != nil {
		return err
	}
	if b.afterCapture != nil {
		b.afterCapture(f.Path)
	}
	return nil
}

// isExecutable reads the executable bit from the worktree mode. Permission
// bits are not meaningful on Windows, where the Git index mode, when there is
// one, is the only record of it.
func isExecutable(mode fs.FileMode, gitMode string) bool {
	if runtime.GOOS == "windows" && gitMode != "" {
		return gitMode == git.ModeExecutable
	}
	return mode.Perm()&0o111 != 0
}

// reconcileUnseen decides what a staged path the walk did not emit means.
func (c *capture) reconcileUnseen(ctx context.Context, r row, pass int, detectOnly bool, changes *int) error {
	b := c.b
	if r.status == "" {
		// Never in the manifest. Only a path Git tracks can become a tombstone.
		if !r.tracked && git.ChangeKind(r.change) != git.ChangeDeleted {
			return nil
		}
		switch r.mode {
		case git.ModeSymlink:
			b.notes.SkippedSymlinks++
			return nil
		case git.ModeGitlink:
			b.notes.addSubmodule(r.path)
			return nil
		}
		info, gone, err := c.inspect(r.path)
		if err != nil {
			return err
		}
		if !gone {
			c.noteUnwalked(info)
			if info.Mode().IsRegular() {
				// A regular file the walk did not reach (excluded or behind
				// a pruned symlink). It exists and is a file, so it is not
				// deleted; it is reported, not tombstoned.
				return nil
			}
			// Replaced by a directory, device or socket: the file Git tracks
			// is no longer a file at this path. Tombstoned exactly as the
			// mid-capture branch below does, so the manifest does not depend
			// on whether the replacement happened before or during the walk.
		}
		if pass > 1 {
			*changes++
		}
		if detectOnly {
			return nil
		}
		return c.st.tombstone(ctx, r.path, pass)
	}
	info, gone, err := c.inspect(r.path)
	if err != nil {
		return err
	}
	if r.status == model.FileDeleted {
		// Still gone, or reappeared as something the walk does not emit:
		// the tombstone stands either way.
		if !gone {
			c.noteUnwalked(info)
		}
		return c.st.markSeen(ctx, r.path, pass)
	}
	// A captured file the walk no longer reaches: gone, replaced by a
	// non-regular entry, or no longer eligible.
	*changes++
	if detectOnly {
		return nil
	}
	tracked := r.tracked || git.ChangeKind(r.change) == git.ChangeDeleted
	if !gone {
		c.noteUnwalked(info)
	}
	if tracked && (gone || !info.Mode().IsRegular()) {
		// The file Git tracks is no longer a file at this path.
		return c.st.tombstone(ctx, r.path, pass)
	}
	return c.st.drop(ctx, r.path)
}

// noteUnwalked classifies a present tracked path the walk did not emit: a
// regular file the walk could not reach (inside the data directory, or behind
// a symlinked directory component) or a non-regular replacement.
func (c *capture) noteUnwalked(info fs.FileInfo) {
	if info.Mode().IsRegular() {
		c.b.notes.ExcludedTracked++
	} else {
		c.b.notes.SkippedNonRegular++
	}
}

// inspect stats rel through the confined root. gone is true when the path no
// longer exists; any other failure to inspect it is an error, because a path
// that cannot be examined is not known to be deleted.
func (c *capture) inspect(rel string) (fs.FileInfo, bool, error) {
	info, err := c.b.Root.Lstat(rel)
	if err == nil {
		return info, false, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil, true, nil
	}
	return nil, false, err
}

// header folds the sorted manifest into its aggregate digest and builds the
// snapshot header. The manifest is streamed from staging; PutSnapshot streams
// it a second time and validates the counts against the rows it receives.
func (c *capture) header(ctx context.Context, head string) (model.Snapshot, error) {
	b := c.b
	h := model.NewHasher(domainManifest)
	var count, total uint64
	b.notes.LFSPointers, b.notes.LFSPointerCount = nil, 0
	err := c.st.eachManifest(ctx, func(r row) error {
		if r.lfs {
			b.notes.addLFS(r.path)
		}
		h.AddString(r.path)
		h.AddString(string(r.status))
		h.AddString(strconv.FormatInt(r.size, 10))
		h.AddString(r.hash)
		h.AddString(strconv.FormatInt(boolInt(r.executable), 10))
		count++
		total += uint64(r.size)
		return nil
	})
	if err != nil {
		return model.Snapshot{}, err
	}
	manifest := h.Sum()
	consistency := model.CaptureValidated
	if b.OperatorFrozen {
		consistency = model.CaptureOperatorFrozen
	}
	snap := model.Snapshot{
		ID:                 model.NewSnapshotID(b.Repository, head, b.SourcePolicyHash, manifest),
		RepositoryID:       b.Repository,
		HeadObjectID:       head,
		CaptureConsistency: consistency,
		SourcePolicyHash:   b.SourcePolicyHash,
		FileCount:          count,
		SourceBytes:        total,
		ManifestHash:       manifest,
		CreatedAt:          time.Now().UTC(),
	}
	return snap, snap.Validate()
}

// headSniffer retains the first bytes of a stream so a Git LFS pointer can be
// recognized without a second read of the file.
type headSniffer struct {
	r    io.Reader
	head []byte
}

func (h *headSniffer) Read(p []byte) (int, error) {
	n, err := h.r.Read(p)
	if need := len(lfsPointerPrefix) - len(h.head); need > 0 && n > 0 {
		h.head = append(h.head, p[:min(n, need)]...)
	}
	return n, err
}

func (h *headSniffer) isLFSPointer() bool {
	return bytes.Equal(h.head, []byte(lfsPointerPrefix))
}
