package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/index"
	store "github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// A process that indexes and answers questions at the same time -- the MCP
// server, whose refresh tool writes while an agent's tools read for the same
// client -- must not let a read change what the run writes.
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
// What reads throughout the run here is the product's own status path,
// index.StatusReader.Status: the report codectx_index_status answers, and the
// single most likely question an agent asks while a refresh runs. It is driven
// rather than restated so this measures the store calls the product makes and
// keeps making -- its pin and capability read are also what an exploration tool
// performs, so the exploration half is measured by the same loop.
//
// Two handles over one file are two independent connection sets, which is what
// the serving process's two halves are to the engine.
//
// Mutation that fails this test: build the status reader over the WRITING
// handle (`f.s`) instead of the reader, which is where the status report was
// served from before this split. The counts then differ.
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
		published := f.activate(sealRepositoryInto(t, f, 2, 4, "published", nil), 0)
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

		// The status path as the serving composition binds it: over the
		// read-only handle, with no workspace root and no git executable, so
		// the report is the store read alone and the worktree comparison --
		// which is not a store call and not what this measures -- is skipped.
		status, err := index.NewStatusReader(index.StatusOptions{Store: reader, Repo: f.repo})
		if err != nil {
			t.Fatalf("the status reader could not be built over the reader handle: %v", err)
		}

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
					st, err := status.Status(ctx)
					if err != nil {
						readErr = err
						return
					}
					if st.Binding.GenerationID != published.GenerationID {
						readErr = errors.New("the status answered a binding other than the published generation")
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
