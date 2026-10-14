package sqlite_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"

	_ "modernc.org/sqlite"

	store "github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// The ingestion group commits when the writer's page cache spills into the
// store's own write-ahead log. Another database writing its own log in the
// same process -- the run ledger flushes every quarter second -- is not that
// store's cache spilling, and must not cut the group: counted that way, a
// first index commits every few hundred milliseconds and writes the store
// many times over instead of once per cache.
//
// The run below spills a small writer cache several times, once alone and
// once beside a second WAL database committing in a loop, and the two must
// commit the same number of groups.
//
// Mutation that fails this test: make the spill signal the process's total
// of log bytes (count every log in one figure, as pacedvfs did before it
// kept a figure per database). The second run then commits many more groups.
func TestAnotherDatabasesLogDoesNotCutTheIngestionGroup(t *testing.T) {
	const cacheKiB = 2048
	const units, factsPerUnit = 120, 20

	measure := func(t *testing.T, beside bool) int64 {
		t.Helper()
		ctx := context.Background()
		dir := t.TempDir()
		f := newFixtureWithOptions(t, filepath.Join(dir, "store.db"), store.Options{WriterCacheKiB: cacheKiB})
		f.activate(sealRepositoryInto(t, f, 2, 4, "setup", nil), 0)
		if err := f.s.Flush(ctx); err != nil {
			t.Fatalf("Flush: %v", err)
		}

		stop := make(chan struct{})
		var wg sync.WaitGroup
		var writes int
		var writeErr error
		if beside {
			// Opened after the store, so it writes through the same file
			// system the store registered as the process default.
			other, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "other.db")+"?_pragma=journal_mode(WAL)")
			if err != nil {
				t.Fatalf("open the second database: %v", err)
			}
			other.SetMaxOpenConns(1)
			defer other.Close()
			if _, err := other.Exec(`CREATE TABLE beat(n INTEGER)`); err != nil {
				t.Fatalf("create the second database's table: %v", err)
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					if _, err := other.Exec(`INSERT INTO beat(n) VALUES (?)`, writes); err != nil {
						writeErr = err
						return
					}
					writes++
				}
			}()
		}

		before := f.s.Commits()
		sealRepositoryInto(t, f, units, factsPerUnit, "run", nil)
		commits := f.s.Commits() - before
		close(stop)
		wg.Wait()
		if writeErr != nil {
			t.Fatalf("the second database's write failed: %v", writeErr)
		}
		if beside && writes == 0 {
			t.Fatal("the second database wrote nothing while the run wrote: this measures nothing")
		}
		t.Logf("beside=%v: %d ingestion-group commits, %d writes to the second database", beside, commits, writes)
		return commits
	}

	alone := measure(t, false)
	beside := measure(t, true)
	if alone < 2 {
		t.Fatalf("the run committed %d group(s) on its own: the fixture never spills the writer cache, so this measures nothing", alone)
	}
	if beside != alone {
		t.Fatalf("the run committed %d ingestion groups beside a second WAL database and %d alone: another database's log is cutting the group",
			beside, alone)
	}
}
