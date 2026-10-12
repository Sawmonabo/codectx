package sqlite_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestANewDatabaseNeverAdoptsTheSearchIndexOfARemovedOne builds a store,
// publishes a generation whose documents are committed in the search index
// directory, closes it, and removes the database and its log files by hand --
// the index directory beside them stays, as it does when an operator deletes
// only the database. A store opened at the same path creates a new database
// and publishes a generation over different documents.
//
// Requirement: the new database allocates doc ids from 1 again and numbers its
// generations from 1 again, so the old directory's last commit still "holds"
// the new generation and its documents sit under the doc ids the new
// documents receive. Creating the schema must remove that directory before
// the index is opened: a search of the new generation sees only its own
// documents, and none of the old directory's files or segments are there.
//
// Mutation that fails it: drop the removal from schema creation (or open the
// index before it). The old document then answers under the new generation's
// doc id 1 and zqxstale matches, and the planted file and the old segments
// are still in the directory.
func TestANewDatabaseNeverAdoptsTheSearchIndexOfARemovedOne(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codectx.db")
	searchDir := dbPath + ".search"

	f := newFixture(t, dbPath)
	old := f.file("pkg/a.go", "package pkg\nfunc F() { zqxstale() }\n")
	snap := f.snapshot("old", old)
	gen, err := f.s.BeginGeneration(f.ctx, f.repo, snap.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatal(err)
	}
	f.unit(gen, f.run(gen), old)
	f.activate(gen, 0)
	if n := searchMatches(t, f, gen, "zqxstale"); n != 1 {
		t.Fatalf("the first database's generation matches zqxstale %d times, want its one document", n)
	}
	if err := f.s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	oldSegments := segmentSummaries(t, searchDir)
	if len(oldSegments) == 0 {
		t.Fatalf("the index directory %s holds no segment after a published generation; the fixture proves nothing", searchDir)
	}
	planted := filepath.Join(searchDir, "left-by-the-removed-database")
	if err := os.WriteFile(planted, []byte("stale"), 0o600); err != nil {
		t.Fatalf("plant: %v", err)
	}
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("remove %s: %v", p, err)
		}
	}

	g := newFixture(t, dbPath)
	if _, err := os.Stat(planted); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the new database opened beside the removed one's index directory and kept it (stat of a file planted there: %v)", err)
	}
	fresh := g.file("pkg/b.go", "package pkg\nfunc F() { zqxfresh() }\n")
	snap2 := g.snapshot("new", fresh)
	gen2, err := g.s.BeginGeneration(g.ctx, g.repo, snap2.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatal(err)
	}
	g.unit(gen2, g.run(gen2), fresh)
	g.activate(gen2, 0)
	if n := searchMatches(t, g, gen2, "zqxfresh"); n != 1 {
		t.Fatalf("the new generation matches zqxfresh %d times, want its one document", n)
	}
	if n := searchMatches(t, g, gen2, "zqxstale"); n != 0 {
		t.Fatalf("the new generation matches zqxstale %d times: the removed database's document answers in the new one", n)
	}
	now := segmentSummaries(t, searchDir)
	for name := range oldSegments {
		if now[name] {
			t.Fatalf("segment summary %s of the removed database's index is still in %s", name, searchDir)
		}
	}
}

// segmentSummaries is the set of per-segment summary files in an index
// directory, one per segment, named by the segment's id.
func segmentSummaries(t *testing.T, dir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	out := map[string]bool{}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".cxsum") {
			out[e.Name()] = true
		}
	}
	return out
}
