package snapshot

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestSweepOrphansKeepsEverythingButUnnamedSettledContent pins the three-way
// decision SweepOrphans makes about a published object, because two of the
// three outcomes destroy source that is still in use:
//
//   - an object no blobs row names, settled past the grace, is the orphan a
//     rolled-back or crashed capture left and is the ONLY thing removed;
//   - an object no row names YET but younger than the grace is kept, because
//     Put and Barrier publish before the commit that names the object, so a
//     young unnamed file may be a publication whose commit is in flight;
//   - an object a row names is kept whatever its age -- rows are the
//     authority, and a blob mid-grace-protocol has one.
//
// The grace clause is the load-bearing one: dropping it deletes content a
// generation is about to reference, and case two is the row that fails.
func TestSweepOrphansKeepsEverythingButUnnamedSettledContent(t *testing.T) {
	dir := t.TempDir()
	c, err := OpenCAS(filepath.Join(dir, "cas"))
	if err != nil {
		t.Fatalf("open CAS: %v", err)
	}
	ctx := context.Background()
	const grace = time.Hour
	now := time.Now()

	put := func(content string, age time.Duration) string {
		t.Helper()
		rec, err := c.Put(ctx, strings.NewReader(content))
		if err != nil {
			t.Fatalf("put %q: %v", content, err)
		}
		p, err := c.path(rec.Hash)
		if err != nil {
			t.Fatalf("path: %v", err)
		}
		stamp := now.Add(-age)
		if err := os.Chtimes(p, stamp, stamp); err != nil {
			t.Fatalf("stamp %q: %v", content, err)
		}
		return rec.Hash
	}

	orphan := put("content no commit ever named", 3*time.Hour)
	inflight := put("content published a minute ago", time.Minute)
	named := put("content a generation names", 3*time.Hour)

	known := func(_ context.Context, hashes []string) (map[string]struct{}, error) {
		out := map[string]struct{}{}
		for _, h := range hashes {
			if h == named {
				out[h] = struct{}{}
			}
		}
		return out, nil
	}

	removed, err := c.SweepOrphans(ctx, known, now, grace, 2)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 1 {
		t.Errorf("swept %d objects, want exactly the one orphan", removed)
	}
	for _, in := range []string{inflight, named} {
		if has, err := c.Has(in); err != nil || !has {
			t.Errorf("object %s was removed (err %v); only unnamed settled content may go", in[:8], err)
		}
	}
	if has, err := c.Has(orphan); err != nil || has {
		t.Errorf("orphan %s survived (present %v, err %v)", orphan[:8], has, err)
	}
}

// TestSweepOrphansRemovesNothingWhenTheIndexCannotAnswer pins the direction of
// an oracle failure. The sweep's whole authority is "no row names this file",
// so an unreadable or partial answer read as "nothing is named" would delete
// every settled object in the store. It must remove nothing and report.
func TestSweepOrphansRemovesNothingWhenTheIndexCannotAnswer(t *testing.T) {
	dir := t.TempDir()
	c, err := OpenCAS(filepath.Join(dir, "cas"))
	if err != nil {
		t.Fatalf("open CAS: %v", err)
	}
	ctx := context.Background()
	rec, err := c.Put(ctx, strings.NewReader("content the index cannot be asked about"))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	p, err := c.path(rec.Hash)
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	unreadable := errors.New("the index could not be read")
	removed, err := c.SweepOrphans(ctx, func(context.Context, []string) (map[string]struct{}, error) {
		return nil, unreadable
	}, time.Now(), time.Hour, 16)
	if !errors.Is(err, unreadable) {
		t.Fatalf("sweep error %v, want the oracle's failure reported", err)
	}
	if removed != 0 {
		t.Errorf("swept %d objects on an unreadable index, want none", removed)
	}
	if has, err := c.Has(rec.Hash); err != nil || !has {
		t.Errorf("settled content was removed without an answer from the index (present %v, err %v)", has, err)
	}
}

// The requirement: a process that answers questions opens the content store
// for READING -- it creates nothing and it can write nothing.
//
// OpenCAS makes the store's directory, and a command that only answers used to
// call it: on a workspace nothing had ever indexed that command created the
// workspace on its way to reporting it empty, and on one an operator had made
// read-only it failed before a byte had been read. A read-only store reports
// the missing directory as the workspace that has published nothing -- the
// first thing an index makes is this directory -- and refuses every entry point
// that stores or removes content, so a command composed with it cannot discover
// at runtime that it cannot write.
//
// NOT RUN: written under the owner's order of 2026-09-17 to run no tests.
//
// Mutation: make OpenCASForReading call OpenCAS and the missing directory is
// created instead of reported; drop the readOnly check from stage and the Put
// below succeeds.
func TestAReadingContentStoreCreatesNothingAndRefusesEveryWrite(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "cas")

	_, err := OpenCASForReading(dir)
	if err == nil {
		t.Fatal("a read-only open of a content store that is not there succeeded")
	}
	var typed *model.Error
	if !errors.As(err, &typed) {
		t.Fatalf("the refusal is untyped: %v", err)
	}
	if typed.Code != model.CodeNoActiveGeneration {
		t.Fatalf("a content store that is not there is reported %s; want %s",
			typed.Code, model.CodeNoActiveGeneration)
	}
	if _, statErr := os.Stat(dir); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("the read-only open created the store: %v", statErr)
	}

	// One that IS there: it reads, and every storing entry point refuses.
	writing, err := OpenCAS(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := writing.Put(ctx, strings.NewReader("package pkg\n"))
	if err != nil {
		t.Fatal(err)
	}
	reading, err := OpenCASForReading(dir)
	if err != nil {
		t.Fatalf("a read-only open of a content store that is there: %v", err)
	}
	if body, readErr := reading.ReadRange(ctx, rec, model.ByteRange{Start: 0, End: uint64(len("package"))}); readErr != nil ||
		string(body) != "package" {
		t.Fatalf("the read-only store read %q (%v); want the published bytes", body, readErr)
	}
	if _, putErr := reading.Put(ctx, strings.NewReader("other\n")); putErr == nil {
		t.Fatal("a read-only content store stored content")
	}
	if removeErr := reading.Remove(rec.Hash); removeErr == nil {
		t.Fatal("a read-only content store removed content")
	}
	if _, sweepErr := reading.SweepOrphans(ctx, nil, time.Now(), 0, 0); sweepErr == nil {
		t.Fatal("a read-only content store swept orphans")
	}
	// The blob is still there: a refusal that had removed it first would be
	// worse than the write it refused.
	if has, hasErr := reading.Has(rec.Hash); hasErr != nil || !has {
		t.Fatalf("the published blob is gone after the refused removal (%v)", hasErr)
	}
}
