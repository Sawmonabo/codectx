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
	"strconv"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/vcs/git"
	"github.com/Sawmonabo/codectx/internal/workspace"
)

const (
	// maxRetries is the Section 10.2 bound: a changing file or membership is
	// recaptured at most twice before the capture is reported unstable.
	maxRetries = 2
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
	// regular file (a directory, device or socket in place of a file). They
	// are not source bytes and are not deletions.
	SkippedNonRegular int
	// ExcludedTracked counts tracked regular files the unconditional
	// exclusions kept out: the data directory inside the workspace.
	ExcludedTracked int
	// Retries is how many validation passes found changes and recaptured.
	Retries int
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
	// acquires the lock for the capture and releases it before returning.
	Lock     *WorkspaceLock
	LockWait time.Duration
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
	notes        Notes
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
	lock := b.Lock
	if lock == nil {
		var err error
		if lock, err = LockWorkspace(ctx, b.Policy.DataDir, b.LockWait); err != nil {
			return model.Snapshot{}, err
		}
		defer lock.Close()
	}
	st, err := openStaging(ctx, StagingDir(b.Policy.DataDir))
	if err != nil {
		return model.Snapshot{}, err
	}
	defer st.Close()

	c := &capture{b: b, st: st}
	head, err := c.enumerateGit(ctx)
	if err != nil {
		return model.Snapshot{}, typed(err)
	}
	for pass := 1; ; pass++ {
		detectOnly := pass >= 2+maxRetries
		changes, err := c.pass(ctx, pass, detectOnly)
		if err != nil {
			return model.Snapshot{}, typed(err)
		}
		if pass > 1 && changes == 0 {
			break
		}
		if detectOnly {
			return model.Snapshot{}, &model.Error{Code: model.CodeSnapshotUnstable, Retryable: true,
				Message:     "the worktree kept changing during capture; " + strconv.Itoa(changes) + " paths differed after " + strconv.Itoa(maxRetries) + " recaptures",
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
		"duration", time.Since(started))
	return snap, nil
}

func (b *Builder) validate() error {
	switch {
	case b.Root.Path == "":
		return invalid("snapshot builder needs an opened workspace root")
	case !filepath.IsAbs(b.Policy.DataDir):
		return invalid("snapshot builder needs an absolute data directory in its policy")
	case b.Policy.MaxFiles <= 0:
		return invalid("snapshot builder needs a positive file budget; no zero or negative bound means unlimited")
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
		Language:    languageOf(r.path),
		Executable:  r.executable,
	}
}

// capture is the state of one Build.
type capture struct {
	b  *Builder
	st *staging
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
	rec, err := b.CAS.Put(ctx, sniff)
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
			// Present but not walked: not a regular file, or a regular file
			// the unconditional exclusions keep out. Reported, not
			// tombstoned: the path was not deleted.
			c.noteUnwalked(info)
			return nil
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

// noteUnwalked classifies a present tracked path the walk did not emit.
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
