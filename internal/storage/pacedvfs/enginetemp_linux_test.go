//go:build linux

package pacedvfs

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/Sawmonabo/codectx/internal/paced"
	"github.com/Sawmonabo/codectx/internal/scratch"
)

// The requirement: the engine's own temporaries are pooled like every other
// working file the product writes.
//
// A sort that outgrows its cache spills to a file the engine creates and
// unlinks itself. The product never writes that code, so it was the one place
// left where a run still created and freed a file of exactly the size it had
// just written -- once per sort, in the middle of the run -- which is the
// burst of freeing this whole design exists to remove. A second sort must
// write over the first one's surface and free nothing.
//
// The second half is the hazard that comes with pooling: the surface still
// holds the previous sort's records past what this one wrote, so a read past
// this tenant's length must be the short read a file system gives at the end
// of a file. If it were not, the engine would sort another query's records
// into this query's answer. Both sorts' answers are checked in full for that
// reason.
//
// Mutation: drop the pooling branch from xOpen -- the engine creates and
// unlinks its own temporary again -- and the pool holds nothing.
func TestTheEnginesTemporariesAreTakenFromThePoolAndGivenBack(t *testing.T) {
	if err := Register(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	Pool(dir)
	if poolArena == nil {
		t.Fatal("no pool: the file system was given no directory to take the engine's temporaries from")
	}

	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "sort.db")+
		"?_pragma=journal_mode(OFF)&_pragma=synchronous(OFF)&_pragma=cache_size(-64)&_pragma=temp_store(FILE)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t(k TEXT, b BLOB)`); err != nil {
		t.Fatal(err)
	}
	const rows = 20000
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	blob := make([]byte, 4096)
	for i := range rows {
		if _, err := tx.Exec(`INSERT INTO t VALUES(?,?)`, fmt.Sprintf("%08d", (i*7919)%rows), blob); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	sorted := func(round int) {
		t.Helper()
		r, err := db.Query(`SELECT k FROM t ORDER BY k, b`)
		if err != nil {
			t.Fatalf("sort %d: %v", round, err)
		}
		defer r.Close()
		n := 0
		for r.Next() {
			var k string
			if err := r.Scan(&k); err != nil {
				t.Fatalf("sort %d row %d: %v", round, n, err)
			}
			if want := fmt.Sprintf("%08d", n); k != want {
				t.Fatalf("sort %d returned %q at row %d, want %q: the spill read bytes this sort did not write",
					round, k, n, want)
			}
			n++
		}
		if err := r.Err(); err != nil {
			t.Fatalf("sort %d: %v", round, err)
		}
		if n != rows {
			t.Fatalf("sort %d returned %d rows of %d", round, n, rows)
		}
	}

	freed, trunc := paced.FreedBytes(), truncations.Load()
	sorted(1)
	first := pooledTemps(t, dir)
	if len(first) == 0 {
		t.Fatal("the sort left nothing in the pool: the engine created and unlinked its own temporary")
	}
	sorted(2)
	if second := pooledTemps(t, dir); len(second) != len(first) {
		t.Fatalf("the second sort left %d pooled surfaces where the first left %d: it created its own instead of writing over the pool's",
			len(second), len(first))
	}
	if got := paced.FreedBytes() - freed; got != 0 {
		t.Fatalf("two sorts gave the filesystem %d bytes back; a sort that writes over a pooled surface frees nothing", got)
	}
	if got := truncations.Load() - trunc; got != 0 {
		t.Fatalf("the file system shortened a file %d times for the sorts; a truncation of a pooled temporary is a reset, not a free", got)
	}
}

// pooledTemps names the engine-temp surfaces the arena of dir holds.
func pooledTemps(t *testing.T, dir string) []string {
	t.Helper()
	pool := filepath.Join(scratch.Dir(dir), "0", string(scratch.EngineTemp))
	entries, err := os.ReadDir(pool)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}
