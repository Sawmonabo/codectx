package snapshot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/process"
	store "github.com/Sawmonabo/codectx/internal/storage/sqlite"
	"github.com/Sawmonabo/codectx/internal/vcs/git"
	"github.com/Sawmonabo/codectx/internal/workspace"
)

// The store satisfies both narrow contracts this package declares. This is
// the only place the two packages meet; a drift in either fails compilation.
var (
	_ Store   = (*store.Store)(nil)
	_ Catalog = (*store.Store)(nil)
)

// fixture is the one retained-source fixture: a temporary worktree (Git or
// plain), a data directory with a real SQLite store and CAS, and the shared
// runner every Git command goes through.
type fixture struct {
	t       *testing.T
	ctx     context.Context
	dataDir string
	repoDir string
	store   *store.Store
	cas     *CAS
	runner  *process.Runner
	git     *git.Git
	gitPath string
	repo    model.RepositoryID
	root    workspace.Root
}

func newFixture(t *testing.T, withGit bool) *fixture {
	t.Helper()
	ctx := context.Background()
	base := t.TempDir()
	f := &fixture{t: t, ctx: ctx, dataDir: filepath.Join(base, "data"), repoDir: filepath.Join(base, "repo")}
	if err := os.MkdirAll(f.repoDir, 0o700); err != nil {
		t.Fatal(err)
	}
	runner, err := process.NewRunner(process.Limits{MaxConcurrent: 2, MemoryBudgetBytes: 1 << 30, DiskBudgetBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	f.runner = runner
	if withGit {
		f.gitPath, err = git.Locate()
		if err != nil {
			t.Skipf("git is unavailable: %v", err)
		}
		f.git, err = git.New(runner, f.gitPath, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		f.gitCmd("init", "-q", "-b", "main")
	}
	s, err := store.Open(ctx, filepath.Join(f.dataDir, "codectx.db"), store.Options{})
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	f.store = s
	f.repo = model.RepositoryID(model.H("test-repo", f.repoDir))
	if err := s.EnsureRepository(ctx, f.repo, f.repoDir); err != nil {
		t.Fatal(err)
	}
	if f.cas, err = OpenCAS(CASDir(f.dataDir)); err != nil {
		t.Fatal(err)
	}
	return f
}

// gitCmd runs an arbitrary git command in the worktree through the runner,
// with the identity a commit needs and no inherited environment.
func (f *fixture) gitCmd(args ...string) string {
	f.t.Helper()
	argv := append([]string{"-c", "user.name=codectx", "-c", "user.email=codectx@example.com",
		"-c", "commit.gpgsign=false", "-c", "core.autocrlf=false"}, args...)
	result, err := f.runner.Run(f.ctx, process.Spec{
		Path: f.gitPath, Args: argv, Dir: f.repoDir,
		Env:            []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "HOME=" + f.t.TempDir(), "LC_ALL=C"},
		MaxStdoutBytes: 1 << 20, MaxStderrBytes: 1 << 20, Timeout: time.Minute, Grace: time.Second,
	})
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, result.Stderr)
	}
	return string(result.Stdout)
}

func (f *fixture) write(rel string, content []byte, mode os.FileMode) {
	f.t.Helper()
	p := filepath.Join(f.repoDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, content, mode); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) builder() *Builder {
	f.t.Helper()
	root, err := workspace.Discover(f.repoDir)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { root.Close() })
	f.root = root
	return &Builder{
		Root:             root,
		Policy:           workspace.Policy{IncludeUntracked: true, MaxFiles: 10000, DataDir: f.dataDir},
		Repository:       f.repo,
		SourcePolicyHash: model.H("test-policy"),
		Store:            f.store,
		CAS:              f.cas,
		Git:              f.git,
	}
}

func (f *fixture) build(b *Builder) (model.Snapshot, *View) {
	f.t.Helper()
	snap, err := b.Build(f.ctx)
	if err != nil {
		f.t.Fatalf("Build: %v", err)
	}
	view, err := OpenView(f.ctx, f.store, f.cas, snap.ID)
	if err != nil {
		f.t.Fatalf("OpenView: %v", err)
	}
	return snap, view
}

// manifest reads the whole manifest by path; the fixtures are small.
func (f *fixture) manifest(view *View) map[string]model.FileVersion {
	f.t.Helper()
	out := map[string]model.FileVersion{}
	err := view.EachFile(f.ctx, model.FileSelection{}, func(fv model.FileVersion) error {
		out[fv.Path] = fv
		return nil
	})
	if err != nil {
		f.t.Fatalf("EachFile: %v", err)
	}
	return out
}

func (f *fixture) readAll(view *View, id model.FileID) ([]byte, error) {
	rc, _, err := view.Open(f.ctx, id)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != code {
		t.Fatalf("got %v, want a typed %s", err, code)
	}
}

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TestCaptureRetainsExactWorktreeBytes protects the Section 10 invariant that
// a snapshot is the bytes actually in the worktree, retained and verifiable
// independently of Git. Each step names what silently breaking it would do:
// serve HEAD's bytes for a dirty or attribute-transformed file, drop a
// deletion or an untracked file, store one content twice, serve a corrupt
// block as verified, lose source once Git history is gone, or report the wrong
// line for a served range.
func TestCaptureRetainsExactWorktreeBytes(t *testing.T) {
	f := newFixture(t, true)
	crlf := []byte("one\r\ntwo\r\n")
	longLine := append(append(bytes.Repeat([]byte("x"), 200000), '\n'), []byte("line two\n")...)
	f.write(".gitattributes", []byte("*.txt text eol=crlf\n"), 0o644)
	f.write(".gitignore", []byte("*.log\n"), 0o644)
	f.write("a.txt", crlf, 0o644)
	f.write("b.go", []byte("package b\n"), 0o644)
	f.write("c.txt", []byte("gone\r\n"), 0o644)
	f.write("dup1.dat", []byte("same\n"), 0o644)
	f.write("dup2.dat", []byte("same\n"), 0o644)
	f.write("empty.dat", nil, 0o644)
	f.write("long.dat", longLine, 0o644)
	f.write("run.sh", []byte("#!/bin/sh\n"), 0o755)
	f.write("vendor/lib.go", []byte("package lib\n"), 0o644)
	f.gitCmd("add", "-A")
	f.gitCmd("commit", "-q", "-m", "base")
	// Worktree state that must win over HEAD: an edit, a deletion, an
	// untracked file, an ignored file and an untracked file under an excluded
	// directory.
	dirty := []byte("package b // dirty\n")
	f.write("b.go", dirty, 0o644)
	if err := os.Remove(filepath.Join(f.repoDir, "c.txt")); err != nil {
		t.Fatal(err)
	}
	f.write("d.md", []byte("# d\n"), 0o644)
	f.write("ignored.log", []byte("log\n"), 0o644)
	f.write("vendor/untracked.go", []byte("package v\n"), 0o644)

	snap, view := f.build(f.builder())
	files := f.manifest(view)
	if snap.HeadObjectID == "" || snap.CaptureConsistency != model.CaptureValidated {
		t.Fatalf("header %+v: want a HEAD and validated_capture", snap)
	}
	for _, absent := range []string{"ignored.log", "vendor/untracked.go"} {
		if _, ok := files[absent]; ok {
			t.Errorf("%s is in the manifest; ignore rules and policy must exclude it", absent)
		}
	}
	if _, ok := files["vendor/lib.go"]; !ok {
		t.Error("tracked vendor/lib.go was dropped; a tracked path must win over the vendor exclusion")
	}

	// Clean under Git, yet the worktree bytes are not the blob's: the blob is
	// LF after the eol attribute, the checkout is CRLF.
	a := files["a.txt"]
	blobLF := f.gitCmd("cat-file", "blob", a.GitObjectID)
	if a.Status != model.FileTracked || a.ContentHash != sha(crlf) || a.ContentHash == sha([]byte(blobLF)) {
		t.Fatalf("a.txt = %+v (blob %q); want the CRLF worktree bytes as tracked content", a, blobLF)
	}
	if got, err := f.readAll(view, a.ID); err != nil || !bytes.Equal(got, crlf) {
		t.Fatalf("Open(a.txt) = %q, %v; want the CRLF bytes", got, err)
	}
	b := files["b.go"]
	if b.Status != model.FileModified || b.ContentHash != sha(dirty) {
		t.Fatalf("b.go = %+v; want modified with the dirty worktree hash", b)
	}
	if b.GitObjectID == "" {
		t.Error("b.go carries no Git provenance")
	}
	c := files["c.txt"]
	if c.Status != model.FileDeleted || c.ContentHash != "" || c.Size != 0 || c.GitObjectID == "" {
		t.Fatalf("c.txt = %+v; want a tombstone with Git provenance", c)
	}
	if _, _, err := view.Open(f.ctx, c.ID); err == nil {
		t.Error("Open on a tombstone returned content")
	}
	d := files["d.md"]
	if d.Status != model.FileUntracked || d.GitObjectID != "" || d.Language != "markdown" {
		t.Fatalf("d.md = %+v; want untracked with no provenance", d)
	}
	if !files["run.sh"].Executable || files["a.txt"].Executable {
		t.Error("executable bit not preserved")
	}

	// Deduplication: one retained object for two paths.
	if files["dup1.dat"].ContentHash != files["dup2.dat"].ContentHash {
		t.Fatal("identical content has two hashes")
	}
	blobPath := filepath.Join(CASDir(f.dataDir), files["dup1.dat"].ContentHash[:2])
	if entries, err := os.ReadDir(blobPath); err != nil || len(entries) != 1 {
		t.Fatalf("CAS bucket holds %d objects (%v); want exactly one for duplicated content", len(entries), err)
	}

	// Determinism: unchanged bytes and policy yield the same snapshot identity.
	if again, _ := f.build(f.builder()); again.ID != snap.ID || again.ManifestHash != snap.ManifestHash {
		t.Fatalf("second capture of unchanged bytes has id %s, want %s", again.ID, snap.ID)
	}

	// Empty file and a very long line round-trip with correct positions.
	empty := files["empty.dat"]
	if got, err := f.readAll(view, empty.ID); err != nil || len(got) != 0 {
		t.Fatalf("Open(empty) = %q, %v", got, err)
	}
	if rng, _, err := view.Read(f.ctx, empty.ID, model.ByteRange{}); err != nil || len(rng.Bytes) != 0 || rng.Start.Line != 1 || rng.End.Line != 1 {
		t.Fatalf("Read(empty, [0,0)) = %+v, %v; want the EOF position on line 1", rng, err)
	}
	long := files["long.dat"]
	rng, _, err := view.Read(f.ctx, long.ID, model.ByteRange{Start: 199990, End: 200010})
	if err != nil {
		t.Fatalf("Read(long): %v", err)
	}
	if !bytes.Equal(rng.Bytes, longLine[199990:200010]) {
		t.Fatalf("Read(long) bytes = %q", rng.Bytes)
	}
	if rng.Start != (model.Position{Byte: 199990, Line: 1, Column: 199990}) || rng.End != (model.Position{Byte: 200010, Line: 3, Column: 0}) {
		t.Fatalf("Read(long) positions = %+v/%+v; want line 1 col 199990 to line 3 col 0", rng.Start, rng.End)
	}

	// A corrupt block is detected by the range read that touches it and by a
	// full stream, while a range in another block still verifies.
	longPath := filepath.Join(CASDir(f.dataDir), long.ContentHash[:2], long.ContentHash)
	corrupt := func(flip bool) {
		t.Helper()
		if err := os.Chmod(longPath, 0o600); err != nil {
			t.Fatal(err)
		}
		fh, err := os.OpenFile(longPath, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		v := byte('x')
		if flip {
			v = 'y'
		}
		if _, err := fh.WriteAt([]byte{v}, 10); err != nil {
			t.Fatal(err)
		}
		fh.Close()
	}
	corrupt(true)
	if got, _, err := view.ReadRange(f.ctx, long.ID, model.ByteRange{Start: 131072, End: 131080}); err != nil || !bytes.Equal(got, longLine[131072:131080]) {
		t.Fatalf("ReadRange in an intact block = %q, %v; want success without touching the corrupt block", got, err)
	}
	_, _, err = view.ReadRange(f.ctx, long.ID, model.ByteRange{Start: 0, End: 8})
	wantCode(t, err, model.CodeSourceIntegrity)
	_, err = f.readAll(view, long.ID)
	wantCode(t, err, model.CodeSourceIntegrity)
	corrupt(false)
	if _, err := f.readAll(view, long.ID); err != nil {
		t.Fatalf("Open after restoring the block: %v", err)
	}

	// Repair: a missing object comes back from Git only when the complete
	// hash matches. The CRLF file's blob is not its worktree bytes, so its
	// repair must be refused and the store left without it.
	remove := func(hash string) {
		t.Helper()
		if err := os.Remove(filepath.Join(CASDir(f.dataDir), hash[:2], hash)); err != nil {
			t.Fatal(err)
		}
	}
	src := RepairSource{Git: f.git, Root: f.repoDir}
	remove(a.ContentHash)
	_, err = f.readAll(view, a.ID)
	wantCode(t, err, model.CodeSourceIntegrity)
	wantCode(t, view.Repair(f.ctx, a.ID, src), model.CodeSourceIntegrity)
	if present, _ := f.cas.Has(a.ContentHash); present {
		t.Fatal("Repair published content whose hash does not match the manifest")
	}
	dup := files["dup1.dat"]
	remove(dup.ContentHash)
	if err := view.Repair(f.ctx, dup.ID, src); err != nil {
		t.Fatalf("Repair(dup1.dat): %v", err)
	}
	if got, err := f.readAll(view, dup.ID); err != nil || string(got) != "same\n" {
		t.Fatalf("Open after repair = %q, %v", got, err)
	}
	if err := view.Repair(f.ctx, d.ID, src); err != nil {
		t.Fatalf("Repair of a present, intact blob: %v", err)
	}

	// Retained reads survive the loss of Git entirely.
	if err := os.RemoveAll(filepath.Join(f.repoDir, ".git")); err != nil {
		t.Fatal(err)
	}
	for _, fv := range []model.FileVersion{b, d, long, dup} {
		if _, err := f.readAll(view, fv.ID); err != nil {
			t.Fatalf("Open(%s) after .git was removed: %v", fv.Path, err)
		}
	}
}

// TestUnstableCaptureIsReported protects the Section 10.2 bound: a file that
// keeps changing under the builder is recaptured at most twice and then the
// capture fails with CTX_SNAPSHOT_UNSTABLE, rather than publishing a manifest
// whose bytes were never validated or retrying forever.
func TestUnstableCaptureIsReported(t *testing.T) {
	f := newFixture(t, false)
	f.write("flaky.dat", []byte("v0\n"), 0o644)
	f.write("stable.dat", []byte("stable\n"), 0o644)
	b := f.builder()
	captures := 0
	b.afterCapture = func(rel string) {
		if rel != "flaky.dat" {
			return
		}
		captures++
		f.write("flaky.dat", []byte(strings.Repeat("v", captures+2)+"\n"), 0o644)
	}
	_, err := b.Build(f.ctx)
	wantCode(t, err, model.CodeSnapshotUnstable)
	if captures != 1+maxRetries {
		t.Fatalf("flaky.dat was captured %d times, want the initial read plus %d retries", captures, maxRetries)
	}
	if entries, _ := os.ReadDir(StagingDir(f.dataDir)); len(entries) != 0 {
		t.Fatalf("a failed capture left %d staging files behind", len(entries))
	}
}

// TestNonGitCaptureIsExact protects the non-Git path: a plain directory is a
// legitimate workspace whose files are retained and served exactly, with no
// Git provenance and no HEAD, rather than rejected or mislabeled.
func TestNonGitCaptureIsExact(t *testing.T) {
	f := newFixture(t, false)
	f.write("src/main.go", []byte("package main\n"), 0o644)
	f.write("notes.md", []byte("# notes\n"), 0o644)
	snap, view := f.build(f.builder())
	files := f.manifest(view)
	if snap.HeadObjectID != "" || snap.FileCount != 2 {
		t.Fatalf("header %+v; want no HEAD and two files", snap)
	}
	for _, fv := range files {
		if fv.Status != model.FileUntracked || fv.GitObjectID != "" {
			t.Fatalf("%+v; want untracked with no provenance", fv)
		}
	}
	if got, err := f.readAll(view, files["src/main.go"].ID); err != nil || string(got) != "package main\n" {
		t.Fatalf("Open = %q, %v", got, err)
	}
}
