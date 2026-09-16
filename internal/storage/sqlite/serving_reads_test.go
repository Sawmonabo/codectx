package sqlite_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	store "github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// A process that indexes and answers questions at the same time -- the MCP
// server, whose refresh tool writes while its exploration tools read for the
// same client -- must not let a read change what the run writes.
//
// The failure this protects: a read served through the WRITING handle pins its
// generation, and a pin writes a retention lease. That write force-commits
// whatever ingestion group the run has open. The group bound exists so a run
// commits when the writer's page cache would spill, not when a reader arrives,
// so a reader served that way rewrites the run's commit schedule: more, smaller
// groups, each one a disk sync the run never asked for, and the reader itself
// queued behind the writer for the rest of the group. Reading through a second,
// read-only handle on the same file -- the composition the serving process now
// opens -- touches no write transaction, so the run commits exactly the groups
// it would have committed alone.
//
// Two handles over one file are two independent connection sets, which is what
// the serving process's two halves are to the engine.
//
// Mutation that fails this test: pin through the writing handle (`f.s`) instead
// of the reader, which is the composition the server had before the read path
// was split off. The counts then differ.
func TestConcurrentReadsDoNotCommitTheRunsIngestionGroupEarly(t *testing.T) {
	// A writer cache small enough that the run below spills it several times,
	// so "the same number of groups" is a count with something in it.
	const cacheKiB = 2048
	// The run every measurement drives: the same units, the same facts, the
	// same order, so the only difference between the two measurements is
	// whether reads were served throughout.
	const units, factsPerUnit = 120, 20

	measure := func(t *testing.T, withReaders bool) (commits int64, reads int) {
		t.Helper()
		ctx := context.Background()
		path := filepath.Join(t.TempDir(), "serve.db")
		f := newFixtureWithOptions(t, path, store.Options{WriterCacheKiB: cacheKiB})

		// A published generation for the read side to pin, and its group
		// committed, so nothing of the setup is counted below.
		f.activate(sealRepositoryInto(t, f, 2, 4, "published", nil), 0)
		if err := f.s.Flush(ctx); err != nil {
			t.Fatalf("Flush: %v", err)
		}

		// A short busy timeout: a read that has to wait out the writer must
		// fail this test quickly rather than hide inside the default five
		// seconds.
		reader, err := store.Open(ctx, path, store.Options{ReadOnly: true, BusyTimeout: 500 * time.Millisecond})
		if err != nil {
			t.Fatalf("the reader handle could not be opened beside the writer: %v", err)
		}
		defer reader.Close()

		stop := make(chan struct{})
		var wg sync.WaitGroup
		var readErr error
		if withReaders {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					pinned, err := reader.PinGeneration(ctx, f.repo, 0, time.Minute)
					if err != nil {
						readErr = err
						return
					}
					_, err = pinned.Capabilities(ctx)
					pinned.Close()
					if err != nil {
						readErr = err
						return
					}
					reads++
				}
			}()
		}

		before := f.s.Commits()
		sealRepositoryInto(t, f, units, factsPerUnit, "run", nil)
		commits = f.s.Commits() - before

		close(stop)
		wg.Wait()
		if readErr != nil {
			t.Fatalf("a read was refused while the run held its ingestion group: %v", readErr)
		}
		return commits, reads
	}

	alone, _ := measure(t, false)
	withReaders, reads := measure(t, true)
	t.Logf("ingestion-group commits: alone=%d with %d concurrent reads=%d", alone, reads, withReaders)
	if alone < 2 {
		t.Fatalf("the run committed %d group(s) on its own: the fixture never fills the writer cache, so this measures nothing", alone)
	}
	if reads == 0 {
		t.Fatal("no read was served while the run wrote: the reader answered nothing this measurement could be about")
	}
	if withReaders != alone {
		t.Fatalf("the run committed %d ingestion groups with %d concurrent reads and %d without: the reads are rewriting the run's commit schedule",
			withReaders, reads, alone)
	}
}
