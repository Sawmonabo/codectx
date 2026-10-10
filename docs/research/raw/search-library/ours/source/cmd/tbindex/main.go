// Command tbindex (benchmark harness) builds one generation from docs.jsonl
// through the store calls a real run makes for search documents:
//
//	tbindex [-workers N] <docs.jsonl> <store dir>
//
// Results go to <dir of docs.jsonl>/results/ours-index.jsonl.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Sawmonabo/codectx/internal/index"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/snapshot"
	"github.com/Sawmonabo/codectx/internal/tbench"
)

func main() {
	workers := flag.Int("workers", 0, "units built at once (0: the coordinator's default for this machine)")
	flag.Parse()
	if flag.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: tbindex [-workers N] <docs.jsonl> <store dir>")
		os.Exit(2)
	}
	if err := run(flag.Arg(0), flag.Arg(1), *workers); err != nil {
		fmt.Fprintln(os.Stderr, "tbindex:", err)
		os.Exit(1)
	}
}

type job struct {
	rec  tbench.UnitRecord
	docs []model.SearchUnit
}

type phase struct {
	Arm     string `json:"arm"`
	Phase   string `json:"phase"`
	WallMS  int64  `json:"wall_ms"`
	CPUMS   *int64 `json:"cpu_ms"`
	PeakRSS int64  `json:"peak_rss_bytes"`
	Disk    int64  `json:"disk_bytes"`
	Docs    int64  `json:"docs"`
	Workers int    `json:"workers,omitempty"`
	Units   int    `json:"units,omitempty"`
	Summed  bool   `json:"summed_over_units,omitempty"`
	Note    string `json:"note,omitempty"`
}

func ms(d time.Duration) int64 { return d.Milliseconds() }

func run(docsPath, dir string, workers int) error {
	ctx := context.Background()
	docsPath, _ = filepath.Abs(docsPath)
	dir, _ = filepath.Abs(dir)
	if _, err := os.Stat(filepath.Join(dir, tbench.DatabaseName)); err == nil {
		return errors.New(dir + " already holds a database; the index arm builds into an empty store")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	src, err := tbench.ReadSource(docsPath)
	if err != nil {
		return err
	}
	cfgHash, err := tbench.ConfigHash(docsPath)
	if err != nil {
		return err
	}
	cfg, err := tbench.Config(src.Root, dir)
	if err != nil {
		return err
	}
	if workers <= 0 {
		workers = tbench.Workers(cfg)
	}
	results, err := tbench.OpenAppend(filepath.Join(tbench.ResultsDir(filepath.Dir(docsPath)), "ours-index.jsonl"))
	if err != nil {
		return err
	}
	defer results.Close()

	start := time.Now()
	u0 := tbench.Rusage()
	s, err := tbench.OpenStore(ctx, cfg, false)
	if err != nil {
		return err
	}
	defer s.Close()
	// The cursor key a writing composition makes beside the database.
	if _, err := pagination.OpenSigner(dir); err != nil {
		return err
	}

	// Seed: the rows the store requires before a unit can begin. Not search work.
	if err := s.EnsureRepository(ctx, src.Repo, src.Root); err != nil {
		return err
	}
	for _, b := range src.Blobs {
		if err := s.PutBlob(ctx, b); err != nil {
			return err
		}
	}
	if err := s.PutSnapshot(ctx, src.Snapshot, func(yield func(model.FileVersion) error) error {
		for _, f := range src.Files {
			if err := yield(f); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	gen, err := s.BeginGeneration(ctx, src.Repo, src.Snapshot.ID, cfgHash, src.Ref)
	if err != nil {
		return err
	}
	b, err := tbench.NewBuilder(s, gen, cfg)
	if err != nil {
		return err
	}
	u1 := tbench.Rusage()
	seed := time.Since(start)

	// Build: every exported unit, in seal order, `workers` at once
	// within one provider; a provider's group is drained and committed before
	// the next provider's first unit runs (generation.build).
	var mu sync.Mutex
	var sum tbench.UnitTimes
	var firstErr error
	var docsTotal int64
	built := 0
	var jobs chan job
	var wg sync.WaitGroup
	openGroup := func() {
		jobs = make(chan job, workers)
		for range workers {
			wg.Add(1)
			go func(jobs <-chan job) {
				defer wg.Done()
				for j := range jobs {
					t, err := b.Build(ctx, j.rec, j.docs)
					mu.Lock()
					if err != nil && firstErr == nil {
						firstErr = fmt.Errorf("unit %s: %w", j.rec.Unit, err)
					}
					sum.Begin += t.Begin
					sum.Nodes += t.Nodes
					sum.Put += t.Put
					sum.Seal += t.Seal
					built++
					mu.Unlock()
				}
			}(jobs)
		}
	}
	drain := func() error {
		if jobs == nil {
			return nil
		}
		close(jobs)
		wg.Wait()
		jobs = nil
		if firstErr != nil {
			return firstErr
		}
		return s.Flush(ctx)
	}
	buildStart := time.Now()
	if err := s.Flush(ctx); err != nil {
		return err
	}
	current := ""
	empty := 0
	feedErr := tbench.Stream(docsPath, func(rec tbench.UnitRecord, docs []model.SearchUnit) error {
		if p := rec.Build.Spec.ProviderID; p != current {
			if err := drain(); err != nil {
				return err
			}
			current = p
			openGroup()
		}
		if len(docs) == 0 {
			empty++
		}
		docsTotal += int64(len(docs))
		mu.Lock()
		failed := firstErr
		mu.Unlock()
		if failed != nil {
			return failed
		}
		jobs <- job{rec: rec, docs: docs}
		return nil
	})
	if err := drain(); err != nil && feedErr == nil {
		feedErr = err
	}
	if feedErr != nil {
		return feedErr
	}
	buildWall := time.Since(buildStart)
	u2 := tbench.Rusage()

	actStart := time.Now()
	if _, err := s.Activate(ctx, gen, 0, model.HealthFresh, nil, index.NormalizationVersion); err != nil {
		return fmt.Errorf("activate: %w", err)
	}
	if err := s.Flush(ctx); err != nil {
		return err
	}
	act := time.Since(actStart)
	u3 := tbench.Rusage()
	disk := tbench.DirBytes(dir)

	cpu := func(a, b tbench.Usage) *int64 { v := (b.CPU - a.CPU).Milliseconds(); return &v }
	rows := []phase{
		{Phase: "seed", WallMS: ms(seed), CPUMS: cpu(u0, u1), PeakRSS: u1.PeakRSS,
			Note: "open, repository, blobs, snapshot, generation: rows the store requires, not search work"},
		{Phase: "begin", WallMS: ms(sum.Begin), Summed: true, Note: "provider run + BeginUnit (inputs streamed and hashed)"},
		{Phase: "nodes", WallMS: ms(sum.Nodes), Summed: true, Note: "referenced node facts only, one evidence row each: rows the store requires"},
		{Phase: "put", WallMS: ms(sum.Put), Summed: true, Note: "PutSearchUnits through the batch sink, flushed"},
		{Phase: "seal", WallMS: ms(sum.Seal), Summed: true, Note: "SealUnit, unit lexical segment folded"},
		{Phase: "build", WallMS: ms(buildWall), CPUMS: cpu(u1, u2), PeakRSS: u2.PeakRSS,
			Note: "wall of begin+nodes+put+seal over all units at the configured concurrency"},
		{Phase: "activation", WallMS: ms(act), CPUMS: cpu(u2, u3), PeakRSS: u3.PeakRSS,
			Note: "Activate (lexical compaction inside it) and the writer flush"},
		{Phase: "total", WallMS: ms(time.Since(start)), CPUMS: cpu(u0, u3), PeakRSS: u3.PeakRSS, Disk: disk},
	}
	for _, r := range rows {
		r.Arm, r.Docs, r.Workers, r.Units = "ours", docsTotal, workers, built
		if err := results.Write(r); err != nil {
			return err
		}
		fmt.Printf("%-10s wall_ms=%-7d cpu_ms=%-7s peak_rss=%-11d %s\n", r.Phase, r.WallMS, fmtPtr(r.CPUMS), r.PeakRSS, r.Note)
	}
	fmt.Printf("docs=%d units=%d (of which without documents: %d) workers=%d disk_bytes=%d\n",
		docsTotal, built, empty, workers, disk)

	tables, err := s.TBTableBytes(ctx)
	if err != nil {
		fmt.Printf("dbstat unavailable in the embedded engine: %v\n", err)
	} else {
		names := make([]string, 0, len(tables))
		var all, search int64
		for n, v := range tables {
			all += v
			if isSearch(n) {
				names = append(names, n)
				search += v
			}
		}
		slices.Sort(names)
		for _, n := range names {
			fmt.Printf("  dbstat %-40s %d\n", n, tables[n])
		}
		fmt.Printf("  dbstat search tables total %d of %d database bytes\n", search, all)
		if err := results.Write(map[string]any{"arm": "ours", "phase": "dbstat", "tables": tables, "search_bytes": search, "database_bytes": all}); err != nil {
			return err
		}
	}
	return tbench.WriteInfo(dir, tbench.StoreInfo{Repo: src.Repo, Root: src.Root, Docs: docsPath, CAS: snapshot.CASDir(src.DataDir)})
}

func isSearch(name string) bool {
	return strings.Contains(name, "search") || strings.Contains(name, "lexical") || strings.Contains(name, "segment")
}

func fmtPtr(p *int64) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprint(*p)
}
