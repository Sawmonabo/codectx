package app

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/paced"
	"github.com/Sawmonabo/codectx/internal/snapshot"
	"github.com/Sawmonabo/codectx/internal/workspace"
)

// resetOperation is what a reset records in the workspace lock, so a process
// refused while it holds the workspace is told what it is waiting behind.
const resetOperation = "reset"

// ResetOptions are the inputs of ResetWorkspace.
type ResetOptions struct {
	// Reindex runs a full index once the data directory has been emptied, so
	// the command leaves a workspace that answers rather than one that has
	// nothing in it.
	Reindex bool
	// OnWaiting is handed to that index's open, which waits for a holder
	// that is getting somewhere exactly as `codectx index` does. The removal
	// itself never waits: it takes the workspace now or is refused.
	OnWaiting func(WaitingHolder)
}

// ResetWorkspace empties the workspace's data directory, so the next index
// builds from nothing. When the index a reset was asked to follow with fails,
// the report of what was removed is returned beside that failure; every other
// failure returns no report, because nothing it would describe is certain. Everything there -- the database and its side files,
// the search index, the content store, spools, scratch pools and the run
// ledger -- is derived from the repository, which is the only source of truth.
//
// It opens no store. The database may be the very thing that is corrupt or of
// a schema this build cannot read, and a reset that needed to open it could
// not recover from either. What it takes is the workspace lock, and only that:
// once, without waiting, so a running index, refresh or watch beat is refused
// with the CTX_WORKSPACE_BUSY that names it rather than having its files taken
// out from under it. A session that is idle between the things it builds -- a
// watch, an MCP server -- holds no lock and cannot be seen here; it has to be
// stopped first.
//
// The lock file lives inside the directory it guards, and it is the one entry
// left behind. Unlinking it while holding it would let the next process create
// a second lock file beside a waiter still holding the first, and the two would
// both believe they own the workspace. Left in place it is an empty file the
// next holder takes, which costs the next index nothing.
//
// Every removal is paced: freed a window at a time with a wait between
// windows, because a burst of frees stalls the host this product runs on. This
// composes nothing, so no to-free set serves the data directory and each entry
// is freed where it lies before the next is started. A symbolic link inside the
// directory is removed as a link; nothing is followed out of it.
func ResetWorkspace(ctx context.Context, repo string, o ResetOptions) (model.WorkspaceReset, error) {
	// Resolved exactly as a composition resolves it, so the directory emptied
	// is the one the next `codectx index` of the same repository opens.
	cfg, err := config.Load(repo)
	if err != nil {
		return model.WorkspaceReset{}, err
	}
	root, err := workspace.Discover(repo)
	if err != nil {
		return model.WorkspaceReset{}, err
	}
	out := model.WorkspaceReset{DataDir: cfg.Storage.DataDir, Removed: []model.RemovedEntry{}}
	named, err := filepath.Abs(repo)
	if err != nil {
		return model.WorkspaceReset{}, &model.Error{Code: model.CodeArgumentInvalid,
			Message: "the repository path " + repo + " cannot be resolved: " + err.Error()}
	}
	dataDir, present, err := resetTarget(cfg.Storage.DataDir, named, root.Path)
	if err != nil {
		return model.WorkspaceReset{}, err
	}
	if present {
		out.DataDir = dataDir
		if out.Removed, out.FreedBytes, err = emptyDataDir(ctx, dataDir); err != nil {
			return model.WorkspaceReset{}, err
		}
	}
	if o.Reindex {
		result, err := reindex(ctx, repo, o.OnWaiting)
		if err != nil {
			// The removal happened and is reported whole beside the failure:
			// the operator is told what went as well as why the index did not
			// follow.
			return out, err
		}
		out.Index = &result
	}
	return out, nil
}

// resetTarget resolves the data directory to the real directory a removal
// would empty, and refuses one whose removal would take anything that is not
// derived data with it: the repository -- the directory named and the root
// discovered above it -- the home directory or the filesystem root, or any
// directory holding one of them. A data directory that does not exist reports
// present false: there is nothing to remove, and taking the lock would create
// the directory only to empty it.
func resetTarget(dataDir, named, repoRoot string) (string, bool, error) {
	resolved, err := filepath.EvalSymlinks(dataDir)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, &model.Error{Code: model.CodeConfigInvalid,
			Message:     "storage.data_dir " + dataDir + " cannot be resolved: " + err.Error(),
			Remediation: "Set storage.data_dir to a directory this user may read, or leave it unset for the default."}
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", false, &model.Error{Code: model.CodeConfigInvalid,
			Message: "storage.data_dir " + dataDir + " cannot be read: " + err.Error()}
	}
	if !info.IsDir() {
		return "", false, &model.Error{Code: model.CodeConfigInvalid,
			Message:     "storage.data_dir " + dataDir + " is not a directory, so it is not a data directory this product made",
			Remediation: "Set storage.data_dir to a directory of its own, or leave it unset for the default."}
	}
	protected := map[string]string{string(filepath.Separator): "the filesystem root",
		named: "the repository", repoRoot: "the repository"}
	if home, err := os.UserHomeDir(); err == nil {
		protected[home] = "the home directory"
	}
	for path, what := range protected {
		target, err := filepath.EvalSymlinks(path)
		if err != nil {
			target = filepath.Clean(path)
		}
		if holds(resolved, target) {
			return "", false, &model.Error{Code: model.CodeConfigInvalid,
				Message: "storage.data_dir " + dataDir + " resolves to " + resolved + ", which is or holds " + what +
					" (" + target + "); emptying it would remove more than the data this product derived",
				Remediation: "Set storage.data_dir to a directory of its own, or leave it unset for the default."}
		}
	}
	return resolved, true, nil
}

// holds reports whether dir is path or one of its ancestors.
func holds(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// emptyDataDir removes every entry of dataDir but its lock file, under that
// lock, and reports each entry with the bytes its removal gave back.
//
// An entry that cannot be removed does not stop the others: everything that
// can go goes, and the first failure is returned once the directory has been
// walked. It is returned as a failure and not reported as a partial success,
// because what a reset promises is that the next index builds from nothing.
func emptyDataDir(ctx context.Context, dataDir string) ([]model.RemovedEntry, uint64, error) {
	lock, err := snapshot.LockWorkspace(ctx, dataDir, resetOperation, snapshot.TryOnce())
	if err != nil {
		return nil, 0, err
	}
	// Released last, after every entry has gone, so no process can take the
	// workspace while any of it is still on the disk.
	defer lock.Close()

	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return nil, 0, dataDirNotWritable(dataDir, err)
	}
	lockName := filepath.Base(snapshot.LockPath(dataDir))
	removed := []model.RemovedEntry{}
	var freed uint64
	var first error
	for _, e := range entries {
		if e.Name() == lockName {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, 0, model.Canceled(err)
		}
		path := filepath.Join(dataDir, e.Name())
		// The purpose's counter is credited as each truncation and unlink
		// releases its bytes, so its growth across one removal is what that
		// removal gave back. Nothing else in this process frees for this
		// purpose.
		before := paced.FreedByPurpose()[paced.WorkspaceReset]
		err := paced.RemoveAllFor(paced.WorkspaceReset, path)
		n := uint64(max(paced.FreedByPurpose()[paced.WorkspaceReset]-before, 0))
		freed += n
		if err != nil {
			if first == nil {
				first = dataDirNotWritable(path, err)
			}
			continue
		}
		removed = append(removed, model.RemovedEntry{Path: path, Bytes: n})
	}
	if first != nil {
		return nil, 0, first
	}
	return removed, freed, nil
}

// reindex runs the full index a reset was asked to follow with, as
// `codectx index` runs it.
func reindex(ctx context.Context, repo string, onWaiting func(WaitingHolder)) (model.IndexResult, error) {
	ws, err := OpenWorkspace(ctx, repo, OpenOptions{Operation: resetOperation + " --reindex", OnWaiting: onWaiting})
	if err != nil {
		return model.IndexResult{}, err
	}
	result, err := ws.Services().Index(ctx, model.IndexRequest{})
	if closeErr := ws.Close(); err == nil && closeErr != nil {
		return model.IndexResult{}, closeErr
	}
	return result, err
}
