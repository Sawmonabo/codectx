package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/paced"
	"github.com/Sawmonabo/codectx/internal/snapshot"
)

// resetFixture makes a repository with one Go file whose user configuration
// names dataDir as the data directory, and isolates every user directory under
// home. An empty dataDir names the repository itself.
func resetFixture(t *testing.T, home, dataDir string) string {
	t.Helper()
	repo := filepath.Join(home, "repo")
	userCfg := filepath.Join(home, "config", "codectx")
	for _, dir := range []string{filepath.Join(repo, "svc"), userCfg} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if dataDir == "" {
		dataDir = repo
	}
	cfg := "[storage]\ndata_dir = \"" + dataDir + "\"\n" +
		"[tools]\noffline = true\n" +
		"[providers.scip]\nenabled = false\n[providers.lsp]\nenabled = false\n"
	writeFile(t, filepath.Join(userCfg, "config.toml"), []byte(cfg))
	writeFile(t, filepath.Join(repo, "go.mod"), []byte("module example.com/svc\n\ngo 1.27\n"))
	writeFile(t, filepath.Join(repo, "svc", "a.go"), []byte(sourceV1))
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "share"))
	return repo
}

// entryNames lists a directory's entries by name.
func entryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// The requirement: a reset never takes a workspace's files out from under the
// process building it. It takes the workspace lock first, and a holder is
// answered with the busy refusal that names it, with nothing removed.
//
// Mutation: skip the lock in emptyDataDir and the database file is gone while
// its holder still holds the workspace.
func TestResetRefusesAHeldWorkspaceAndRemovesNothing(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	data := filepath.Join(home, "data")
	repo := resetFixture(t, home, data)
	holder, err := snapshot.LockWorkspace(ctx, data, "index", snapshot.TryOnce())
	if err != nil {
		t.Fatalf("hold the workspace: %v", err)
	}
	defer holder.Close()
	db := filepath.Join(data, databaseName)
	writeFile(t, db, []byte("the holder's database"))

	_, err = ResetWorkspace(ctx, repo, ResetOptions{})
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != model.CodeWorkspaceBusy {
		t.Fatalf("reset of a held workspace = %v, want %s", err, model.CodeWorkspaceBusy)
	}
	if typed.Details[snapshot.DetailHolderOperation] != "index" ||
		typed.Details[snapshot.DetailHolderPID] != strconv.Itoa(os.Getpid()) {
		t.Fatalf("the refusal names holder %v; want the index in process %d", typed.Details, os.Getpid())
	}
	if _, err := os.Stat(db); err != nil {
		t.Fatalf("the holder's database did not survive a refused reset: %v", err)
	}
}

// The requirement: a reset empties the data directory and nothing else. A
// symbolic link inside it -- to a file or to a directory outside -- is removed
// as a link, and what it points at is left exactly as it was. The lock file is
// the one entry left, for the next holder to take.
//
// Mutation: follow links (stat an entry instead of lstat-ing it, or resolve it
// before removing) and the directory outside is emptied through the link.
func TestResetRemovesOnlyTheDataDirectoryAndNoLinkTarget(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	data := filepath.Join(home, "data")
	repo := resetFixture(t, home, data)
	outside := filepath.Join(home, "outside")
	if err := os.MkdirAll(filepath.Join(outside, "tree"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(outside, "target.txt"), []byte("outside file"))
	writeFile(t, filepath.Join(outside, "tree", "keep.txt"), []byte("outside tree"))
	if err := os.MkdirAll(filepath.Join(data, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(data, "nested", "a.bin"), []byte("derived"))
	for link, target := range map[string]string{
		filepath.Join(data, "link-file"):          filepath.Join(outside, "target.txt"),
		filepath.Join(data, "link-dir"):           filepath.Join(outside, "tree"),
		filepath.Join(data, "nested", "link-dir"): filepath.Join(outside, "tree"),
	} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}

	report, err := ResetWorkspace(ctx, repo, ResetOptions{})
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if got, want := entryNames(t, data), []string{filepath.Base(snapshot.LockPath(data))}; !slices.Equal(got, want) {
		t.Fatalf("the data directory holds %v after a reset; want only %v", got, want)
	}
	var removed []string
	for _, e := range report.Removed {
		removed = append(removed, filepath.Base(e.Path))
	}
	if want := []string{"link-dir", "link-file", "nested"}; !slices.Equal(removed, want) {
		t.Fatalf("the reset reported removing %v; want %v", removed, want)
	}
	for path, want := range map[string]string{
		filepath.Join(outside, "target.txt"):       "outside file",
		filepath.Join(outside, "tree", "keep.txt"): "outside tree",
	} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("%s reads %q, %v after the reset; a link's target must survive it", path, got, err)
		}
	}
}

// The requirement: a data directory that is the repository is refused, and
// nothing in it is touched. storage.data_dir is the operator's to set, and a
// value naming the repository would otherwise have the reset empty the source
// it exists to index again.
//
// Mutation: drop the guard in resetTarget and the repository's files are gone.
func TestResetRefusesADataDirectoryThatIsTheRepository(t *testing.T) {
	repo := resetFixture(t, t.TempDir(), "")
	_, err := ResetWorkspace(context.Background(), repo, ResetOptions{})
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != model.CodeConfigInvalid {
		t.Fatalf("reset with the repository as its data directory = %v, want %s", err, model.CodeConfigInvalid)
	}
	for _, path := range []string{filepath.Join(repo, "go.mod"), filepath.Join(repo, "svc", "a.go")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s did not survive a refused reset: %v", path, err)
		}
	}
}

// The requirement: the data directory is given back at the paced rate, a
// window at a time, never in one bulk unlink. The chunks are observed, not
// the time: a file of two windows and a byte is emptied in three truncation
// steps before it is unlinked, and the report counts every byte.
//
// Mutation: remove the entries with os.RemoveAll and no step is taken.
func TestResetFreesTheDataDirectoryAWindowAtATime(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	data := filepath.Join(home, "data")
	repo := resetFixture(t, home, data)
	if err := os.MkdirAll(filepath.Join(data, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	const size = 2*paced.Window + 1
	large := filepath.Join(data, "nested", "large")
	f, err := os.OpenFile(large, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	before := paced.Steps()
	report, err := ResetWorkspace(ctx, repo, ResetOptions{})
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if got := paced.Steps() - before; got != 3 {
		t.Fatalf("the reset freed a %d-byte file in %d window steps; want 3", size, got)
	}
	if report.FreedBytes != size || len(report.Removed) != 1 || report.Removed[0].Bytes != size {
		t.Fatalf("the reset reported %d bytes freed in %+v; want %d in the one entry", report.FreedBytes, report.Removed, size)
	}
	if _, err := os.Lstat(large); !os.IsNotExist(err) {
		t.Fatalf("the file survived the reset: %v", err)
	}
}

// The requirement: `--reindex` leaves a workspace that answers, built from
// nothing. The data directory starts with a database that is not a database
// at all -- the store a reset exists to recover from -- and after the reset
// with a reindex a search finds the repository's function.
//
// Mutation: ignore Reindex and the reset returns no index result, and the
// search finds no active generation.
func TestResetReindexLeavesAFreshIndexThatAnswersASearch(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	data := filepath.Join(home, "data")
	repo := resetFixture(t, home, data)
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(data, databaseName), []byte("not a database"))
	marker := filepath.Join(data, "left-by-an-older-run")
	writeFile(t, marker, []byte("stale"))

	report, err := ResetWorkspace(ctx, repo, ResetOptions{Reindex: true})
	if err != nil {
		t.Fatalf("reset --reindex: %v", err)
	}
	if report.Index == nil || report.Index.Binding.GenerationID == 0 {
		t.Fatalf("reset --reindex returned index result %+v; want a published generation", report.Index)
	}
	if _, err := os.Lstat(marker); !os.IsNotExist(err) {
		t.Fatalf("an entry of the old data directory survived the reset: %v", err)
	}

	ws, err := OpenWorkspaceForQuery(ctx, repo)
	if err != nil {
		t.Fatalf("open the reset workspace: %v", err)
	}
	defer ws.Close()
	page, err := ws.Services().Search(ctx, model.SearchRequest{Query: "Alpha"})
	if err != nil {
		t.Fatalf("search the reset workspace: %v", err)
	}
	found := false
	for _, hit := range page.Items {
		found = found || hit.Name == "Alpha"
	}
	if !found {
		t.Fatalf("a search for Alpha in the reindexed workspace returned %+v; want the function", page.Items)
	}
}
