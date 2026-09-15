package snapshot

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Sawmonabo/codectx/internal/fslock"
	"github.com/Sawmonabo/codectx/internal/model"
)

// MaterializeOptions bound one private materialization.
type MaterializeOptions struct {
	// Dir is the parent for materializations, normally MaterializeDir(dataDir).
	Dir string
	// MaxBytes bounds the total content copied. Zero -- the default -- is
	// unlimited. Under a user-set budget the files that fit are materialized
	// and every file left out is named in Skipped; the tree is never refused,
	// because an analyzer given a smaller tree with a list of what is missing
	// can still answer, while one given an error cannot.
	MaxBytes int64
	// Include, when set, decides membership file by file: only versions it
	// accepts are copied, and the ones it rejects cost neither a byte of the
	// MaxBytes budget nor an inode. It is the selector for a subset whose
	// shape is a predicate rather than a list, which model.FileSelection.Paths
	// cannot express: that list is bounded at MaxFilterValues entries, far
	// below the file count of one analysis unit. A nil Include copies every
	// nondeleted file the selection yields.
	Include func(model.FileVersion) bool
}

// Materialization is a private tree of copied snapshot files for an external
// analyzer (Section 10.4). Files are copies: never hard links to the CAS or
// the repository, so an analyzer that writes cannot alter retained source.
type Materialization struct {
	root  string
	owner *os.File
	once  sync.Once
	err   error
	// skippedPaths are the files a user-set MaxBytes budget left out, by path;
	// skippedCount is complete. The count is retained rather than the whole
	// list so a budget far below the tree size cannot itself become the
	// repository-sized allocation the budget exists to prevent.
	skippedPaths []string
	skippedCount int
}

// maxSkippedPaths bounds the exemplar list beside the complete count, matching
// the capture notes' convention.
const maxSkippedPaths = 32

func (m *Materialization) skipped(path string) {
	m.skippedCount++
	if len(m.skippedPaths) < maxSkippedPaths {
		m.skippedPaths = append(m.skippedPaths, path)
	}
}

// reportSkips puts a user-set budget's exclusions in front of the operator.
// The tree is handed to an analyzer whose answer is then narrower than the
// snapshot, so the budget that narrowed it is never silent.
func (m *Materialization) reportSkips(maxBytes int64) {
	if m.skippedCount == 0 {
		return
	}
	slog.Default().Warn("materialization budget exceeded; files were not materialized",
		"component", "snapshot.materialize", "max_bytes", maxBytes,
		"skipped_files", m.skippedCount, "skipped_examples", m.skippedPaths)
}

// Root is the absolute directory holding the copied files.
func (m *Materialization) Root() string { return m.root }

// Close removes the tree. It is idempotent and safe after a partial failure.
func (m *Materialization) Close() error {
	if m == nil {
		return nil
	}
	m.once.Do(func() {
		if err := os.RemoveAll(m.root); err != nil {
			m.err = ioError("materialization cleanup", err)
		}
		if m.owner != nil {
			fslock.Unlock(m.owner)
			m.owner.Close()
			if err := os.Remove(m.owner.Name()); err != nil && !errors.Is(err, fs.ErrNotExist) && m.err == nil {
				m.err = ioError("materialization cleanup", err)
			}
		}
	})
	return m.err
}

// Materialize copies the selected nondeleted files of view that opts.Include
// accepts into a fresh private directory under opts.Dir, preserving the
// executable bit and the root-relative layout. Every byte passes through the view's verified reader.
// Writes go through an os.Root over the new directory, so a manifest path can
// neither escape it nor follow a link out of it. The tree is removed on any
// failure or cancellation; on success the caller owns it until Close.
func Materialize(ctx context.Context, view model.SnapshotView, sel model.FileSelection, opts MaterializeOptions) (*Materialization, error) {
	if !filepath.IsAbs(opts.Dir) {
		return nil, invalid("the materialization directory must be an absolute path")
	}
	if opts.MaxBytes < 0 {
		return nil, resourceLimit("the materialization byte bound is negative; zero means unlimited")
	}
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return nil, ioError("materialization directory", err)
	}
	id, err := model.NewRandomID()
	if err != nil {
		return nil, err
	}
	name := materializePrefix + id[:16]
	owner, err := os.OpenFile(filepath.Join(opts.Dir, name+".lock"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, ioError("materialization lock", err)
	}
	if held, err := fslock.TryLock(owner); err != nil || !held {
		owner.Close()
		os.Remove(owner.Name())
		if err == nil {
			err = errors.New("fresh lock file is already locked")
		}
		return nil, internal("materialization lock: %v", err)
	}
	m := &Materialization{root: filepath.Join(opts.Dir, name), owner: owner}
	if err := os.Mkdir(m.root, 0o700); err != nil {
		m.Close()
		return nil, ioError("materialization root", err)
	}
	if err := m.fill(ctx, view, sel, opts); err != nil {
		return nil, errors.Join(err, m.Close())
	}
	m.reportSkips(opts.MaxBytes)
	return m, nil
}

func (m *Materialization) fill(ctx context.Context, view model.SnapshotView, sel model.FileSelection, opts MaterializeOptions) error {
	root, err := os.OpenRoot(m.root)
	if err != nil {
		return ioError("materialization root", err)
	}
	defer root.Close()
	var total int64
	return view.EachFile(ctx, sel, func(fv model.FileVersion) error {
		if err := ctx.Err(); err != nil {
			return model.Canceled(err)
		}
		if fv.Status == model.FileDeleted {
			return nil
		}
		if opts.Include != nil && !opts.Include(fv) {
			return nil
		}
		if opts.MaxBytes > 0 && total+fv.Size > opts.MaxBytes {
			// A user-set budget admits what fits and reports the rest by path;
			// it never fails the analyzer tree. Zero is unlimited, the default.
			m.skipped(fv.Path)
			return nil
		}
		total += fv.Size
		if strings.HasPrefix(fv.Path, "/") || path.Clean(fv.Path) != fv.Path {
			return &model.Error{Code: model.CodePathEscape, Message: "manifest path is not a normalized root-relative path"}
		}
		if dir := path.Dir(fv.Path); dir != "." {
			if err := root.MkdirAll(filepath.FromSlash(dir), 0o700); err != nil {
				return ioError("materialization mkdir", err)
			}
		}
		mode := fs.FileMode(0o600)
		if fv.Executable {
			mode = 0o700
		}
		dst, err := root.OpenFile(filepath.FromSlash(fv.Path), os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return ioError("materialization create", err)
		}
		defer dst.Close()
		src, _, err := view.Open(ctx, fv.ID)
		if err != nil {
			return err
		}
		defer src.Close()
		n, err := io.Copy(dst, io.LimitReader(src, fv.Size+1))
		if err != nil {
			return ioError("materialization copy", err)
		}
		if n != fv.Size {
			return integrity("retained bytes for %s differ in length from the manifest", fv.ID)
		}
		return nil
	})
}

// sweepMaterializations removes materializations whose owner no longer holds
// its lock: the process that created them is gone. A live one, locked by a
// running process, is left alone.
func sweepMaterializations(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return ioError("materialization directory", err)
	}
	var errs []error
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), materializePrefix) || strings.HasSuffix(e.Name(), ".lock") {
			continue
		}
		lockPath := filepath.Join(dir, e.Name()+".lock")
		f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			errs = append(errs, ioError("materialization sweep", err))
			continue
		}
		held, err := fslock.TryLock(f)
		if err != nil || !held {
			// Locked: its owner is alive.
			f.Close()
			if err != nil {
				errs = append(errs, internal("materialization sweep: %v", err))
			}
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			errs = append(errs, ioError("materialization sweep", err))
		}
		fslock.Unlock(f)
		f.Close()
		if err := os.Remove(lockPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, ioError("materialization sweep", err))
		}
	}
	return errors.Join(errs...)
}
