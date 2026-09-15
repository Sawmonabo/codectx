package toolchain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Sawmonabo/codectx/internal/fslock"
	"github.com/Sawmonabo/codectx/internal/model"
)

// store is the private tool store. Installs are atomic -- fetch and extract
// into .staging, verify, rename the finished tree into place, then write the
// publication marker -- so a version directory a reader can see is either
// complete or invisible, never half a payload.
//
// The directory is not created here. A store value is a path and nothing else,
// so building a resolver -- which `tools status`, `tools verify` and `tools gc`
// all do -- touches no filesystem at all; the first install creates the tree on
// the way in, through the MkdirAll that newStaging, acquire and publish each
// already perform for the path they need. A read-only report that materialized
// a per-workspace data directory would leave one behind in every checkout it
// was ever run in.
type store struct{ dir string }

// lockPollInterval is how often a bounded wait for a per-tool install lock
// retries.
const lockPollInterval = 100 * time.Millisecond

func (s *store) toolDir(name string) string    { return filepath.Join(s.dir, name) }
func (s *store) locksDir() string              { return filepath.Join(s.dir, locksDirName) }
func (s *store) stagingDir(name string) string { return filepath.Join(s.dir, stagingDirName, name) }

func (s *store) versionDir(name, version string) string {
	return filepath.Join(s.dir, name, version)
}

// marker reads the publication marker of one version directory. It returns the
// empty string when the directory is unpublished, which is the same answer as
// "not installed": Section 11.7 makes a directory without a marker invisible.
func (s *store) marker(name, version string) (string, error) {
	data, err := os.ReadFile(filepath.Join(s.versionDir(name, version), completeName))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", ioError("tool publication marker", err)
	}
	if len(data) > maxMarkerBytes {
		return "", nil
	}
	digest := strings.TrimSpace(string(data))
	if !model.ValidHexID(digest) {
		return "", nil
	}
	return digest, nil
}

// toolLock is the cross-process serialization of one tool's installs. It lives
// in .locks, outside any directory an install renames or GC removes, so the
// lock file a waiter is holding open can never be swept from under it.
type toolLock struct {
	f    *os.File
	once sync.Once
	err  error
}

// acquire takes the install lock for name, retrying until wait has elapsed or
// ctx ends. wait <= 0 tries exactly once and reports busy immediately, which is
// what GC uses so collection never blocks behind a running install.
func (s *store) acquire(ctx context.Context, name string, wait time.Duration) (*toolLock, error) {
	if err := os.MkdirAll(s.locksDir(), storeDirPerm); err != nil {
		return nil, ioError("tool lock directory", err)
	}
	f, err := os.OpenFile(filepath.Join(s.locksDir(), name+lockFileSuffix), os.O_RDWR|os.O_CREATE, lockFileMode)
	if err != nil {
		return nil, ioError("tool lock file", err)
	}
	deadline := time.Now().Add(wait)
	for {
		held, err := fslock.TryLock(f)
		if err != nil {
			f.Close()
			return nil, internalError("tool install lock: %v", bareCause(err))
		}
		if held {
			return &toolLock{f: f}, nil
		}
		if !time.Now().Add(lockPollInterval).Before(deadline) {
			f.Close()
			return nil, (&model.Error{Code: model.CodeWorkspaceBusy, Retryable: true,
				Message:     "another codectx process is installing this managed tool",
				Remediation: "wait for the running install to finish, or run `codectx tools prefetch` once and reuse the store"}).
				WithDetail("tool", name)
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, model.Canceled(ctx.Err())
		case <-time.After(lockPollInterval):
		}
	}
}

// release drops the lock. It is idempotent. The lock file itself is left in
// place: it is the lock's identity, and removing it would let two processes
// take locks on two different files with the same name.
func (l *toolLock) release() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		if err := fslock.Unlock(l.f); err != nil {
			l.err = internalError("tool install unlock: %v", bareCause(err))
		}
		if err := l.f.Close(); err != nil && l.err == nil {
			l.err = ioError("tool lock close", err)
		}
	})
	return l.err
}

// install fetches, verifies, extracts and publishes one payload. The caller
// holds the tool's install lock.
func (s *store) install(ctx context.Context, f *fetcher, name string, e Entry, p Payload) error {
	staging, err := s.newStaging(name)
	if err != nil {
		return err
	}
	defer func() {
		os.RemoveAll(staging)
		// Leave no empty per-tool staging parent behind: it is indistinguishable
		// from an abandoned one, and GC would report collecting it every time.
		os.Remove(s.stagingDir(name))
	}()

	archive, err := os.OpenFile(filepath.Join(staging, "payload"), os.O_RDWR|os.O_CREATE|os.O_EXCL, storeFilePerm)
	if err != nil {
		return ioError("tool payload staging file", err)
	}
	defer archive.Close()
	if err := f.download(ctx, name, e, p, archive); err != nil {
		return err
	}

	tree := filepath.Join(staging, "root")
	if err := os.Mkdir(tree, storeDirPerm); err != nil {
		return ioError("tool payload staging tree", err)
	}
	if err := extract(ctx, name, archive, tree, e.entryPath(p), p.Size); err != nil {
		return err
	}
	// The archive is dead weight once unpacked, and a large payload is unpacked
	// beside it; releasing it here halves the peak disk an install needs.
	archive.Close()
	if err := os.Remove(filepath.Join(staging, "payload")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return ioError("tool payload staging cleanup", err)
	}

	// The entry executable is verified while the tree is still private, so a
	// payload whose pinned entry is wrong never becomes visible at all.
	if _, err := hashEntry(name, tree, e.entryPath(p), e.entryDigest(p)); err != nil {
		return err
	}
	if err := fslock.SyncDir(tree); err != nil {
		return ioError("tool payload directory sync", err)
	}
	return s.publish(name, e.Version, tree, p.SHA256)
}

// publish renames a verified tree into place and marks it complete. The old
// directory is removed first: it can only be an unpublished or superseded
// install, because a published one with this version is what the caller
// already checked for.
func (s *store) publish(name, version, tree, digest string) error {
	final := s.versionDir(name, version)
	if err := os.MkdirAll(filepath.Dir(final), storeDirPerm); err != nil {
		return ioError("tool store directory", err)
	}
	if err := os.RemoveAll(final); err != nil {
		return ioError("tool store cleanup", err)
	}
	if err := os.Rename(tree, final); err != nil {
		return ioError("tool store publish", err)
	}
	if err := fslock.SyncDir(filepath.Dir(final)); err != nil {
		return ioError("tool store directory sync", err)
	}
	// The marker is written last and through a rename of its own, so a crash
	// leaves either no marker (the tree stays invisible and is swept) or a whole
	// one, never a truncated digest that would fail every later comparison.
	tmp := filepath.Join(final, completeName+".tmp")
	if err := os.WriteFile(tmp, []byte(digest+"\n"), markerFileMode); err != nil {
		return ioError("tool publication marker", err)
	}
	if err := os.Rename(tmp, filepath.Join(final, completeName)); err != nil {
		return ioError("tool publication marker", err)
	}
	if err := fslock.SyncDir(final); err != nil {
		return ioError("tool store directory sync", err)
	}
	return nil
}

func (s *store) newStaging(name string) (string, error) {
	id, err := model.NewRandomID()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(s.stagingDir(name), id[:16])
	if err := os.MkdirAll(dir, storeDirPerm); err != nil {
		return "", ioError("tool staging directory", err)
	}
	return dir, nil
}

// hashEntry opens one payload-relative entry through an os.Root over the
// payload and reports its SHA-256, failing when it is absent, is not a regular
// file, or disagrees with want. The confined open is what keeps a symlink that
// appeared after extraction from redirecting the hash to another file.
func hashEntry(name, payloadDir, entry, want string) (string, error) {
	root, err := os.OpenRoot(payloadDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", corrupt(name, "the installed payload directory is missing")
		}
		return "", ioError("tool payload open", err)
	}
	defer root.Close()
	rel := filepath.FromSlash(entry)
	before, err := root.Lstat(rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", corrupt(name, "the payload does not carry its pinned entry executable")
		}
		return "", ioError("tool entry stat", err)
	}
	if !before.Mode().IsRegular() {
		return "", corrupt(name, "the payload's entry is not a regular file")
	}
	f, err := root.Open(rel)
	if err != nil {
		return "", ioError("tool entry open", err)
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil {
		return "", ioError("tool entry stat", err)
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return "", corrupt(name, "the payload's entry changed between the check and the open")
	}
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", ioError("tool entry read", err)
	}
	got := hex.EncodeToString(sum.Sum(nil))
	if want != "" && got != want {
		return "", corrupt(name, "the payload's entry executable does not hash to the digest the lock pins")
	}
	return got, nil
}

// gc removes what the current lock does not name: unknown tools, superseded
// versions, unpublished directories left by a crash and abandoned staging
// trees. It never waits on an install lock, so a collection running beside an
// install skips that tool rather than blocking or racing it.
func (s *store) gc(ctx context.Context, lock Lock) (int, error) {
	payloads, err := s.payloadEntries()
	if err != nil {
		return 0, err
	}
	var removed int
	var errs []error
	for _, e := range payloads {
		if err := ctx.Err(); err != nil {
			return removed, model.Canceled(err)
		}
		n, err := s.collectTool(ctx, lock, e)
		removed += n
		errs = append(errs, err)
	}
	// sweepStaging reports a store with no staging tree as nothing to do, so it
	// is called unconditionally rather than only when the listing named it.
	n, err := s.sweepStaging(ctx)
	removed += n
	errs = append(errs, err)
	return removed, errors.Join(errs...)
}

// payloadEntries lists the store's top-level entries that are neither the lock
// directory nor the staging tree: everything else is a payload directory the
// current lock either names or does not. A store that does not exist holds
// none, which is not an error -- nothing has been installed yet.
//
// It is the one place that knows the store's reserved names, so the collector
// and the unlisted report cannot drift into disagreeing about them.
func (s *store) payloadEntries() ([]fs.DirEntry, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, ioError("tool store listing", err)
	}
	out := make([]fs.DirEntry, 0, len(entries))
	for _, e := range entries {
		switch e.Name() {
		case locksDirName, stagingDirName:
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// unlisted returns the payload entries no lock entry names. They are bytes
// nothing in this binary vouches for -- a superseded tool the store kept across
// an upgrade, or a directory something else wrote -- so a report that omits
// them tells an operator the store holds exactly what the lock pins when it
// does not.
func (s *store) unlisted(lock Lock) ([]fs.DirEntry, error) {
	payloads, err := s.payloadEntries()
	if err != nil {
		return nil, err
	}
	out := make([]fs.DirEntry, 0, len(payloads))
	for _, e := range payloads {
		if _, pinned := lock.Tools[e.Name()]; pinned {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// pruneUnlisted removes exactly those entries, taking each tool's install lock
// the way gc does and skipping one another process holds. It is gc's first half
// with the superseded-version pass left out: prefetch is asked to make the
// store hold what the lock names, and removing a version of a pinned tool that
// another workspace may still be resolving is gc's decision, not prefetch's.
func (s *store) pruneUnlisted(ctx context.Context, lock Lock) (int, error) {
	entries, err := s.unlisted(lock)
	if err != nil {
		return 0, err
	}
	var removed int
	var errs []error
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return removed, model.Canceled(err)
		}
		n, err := s.collectTool(ctx, lock, e)
		removed += n
		errs = append(errs, err)
	}
	return removed, errors.Join(errs...)
}

func (s *store) collectTool(ctx context.Context, lock Lock, e fs.DirEntry) (int, error) {
	name := e.Name()
	if !e.IsDir() || validToolName(name) != nil {
		// Neither a plain file nor a name outside the lock's alphabet is
		// something this package writes, so there is no install to serialize
		// against and nothing that could be holding a lock on it.
		if err := os.RemoveAll(filepath.Join(s.dir, name)); err != nil {
			return 0, ioError("tool store cleanup", err)
		}
		return 1, nil
	}
	held, err := s.acquire(ctx, name, 0)
	if err != nil {
		var typed *model.Error
		if errors.As(err, &typed) && typed.Code == model.CodeWorkspaceBusy {
			return 0, nil
		}
		return 0, err
	}
	defer held.release()

	entry, pinned := lock.Tools[name]
	if !pinned {
		if err := os.RemoveAll(s.toolDir(name)); err != nil {
			return 0, ioError("tool store cleanup", err)
		}
		return 1, nil
	}
	versions, err := os.ReadDir(s.toolDir(name))
	if err != nil {
		return 0, ioError("tool store listing", err)
	}
	var removed int
	var errs []error
	for _, v := range versions {
		keep := false
		if v.IsDir() && v.Name() == entry.Version {
			digest, err := s.marker(name, v.Name())
			errs = append(errs, err)
			keep = digest != ""
		}
		if keep {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.toolDir(name), v.Name())); err != nil {
			errs = append(errs, ioError("tool store cleanup", err))
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

// sweepStaging removes abandoned fetch and extraction trees. Each is nested
// under its tool's name so the collector can take that tool's install lock and
// leave a live install alone.
func (s *store) sweepStaging(ctx context.Context) (int, error) {
	root := filepath.Join(s.dir, stagingDirName)
	tools, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, ioError("tool staging listing", err)
	}
	var removed int
	var errs []error
	for _, t := range tools {
		if err := ctx.Err(); err != nil {
			return removed, model.Canceled(err)
		}
		if validToolName(t.Name()) != nil {
			if err := os.RemoveAll(filepath.Join(root, t.Name())); err != nil {
				errs = append(errs, ioError("tool staging cleanup", err))
			} else {
				removed++
			}
			continue
		}
		held, err := s.acquire(ctx, t.Name(), 0)
		if err != nil {
			var typed *model.Error
			if errors.As(err, &typed) && typed.Code == model.CodeWorkspaceBusy {
				continue
			}
			errs = append(errs, err)
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, t.Name())); err != nil {
			errs = append(errs, ioError("tool staging cleanup", err))
		} else {
			removed++
		}
		errs = append(errs, held.release())
	}
	return removed, errors.Join(errs...)
}
