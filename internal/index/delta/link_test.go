package delta_test

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Sawmonabo/codectx/internal/admission"
	"github.com/Sawmonabo/codectx/internal/index/delta"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/process"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/link"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/worker"
)

// TestMain makes this test binary its own parser worker, as the codectx
// binary is in production: invoked with wire.Subcommand as its first argument
// it runs worker.Main instead of the tests.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == wire.Subcommand {
		os.Exit(worker.Main(context.Background(), os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// TestLinkBodyEditRelinksNothing builds one Go linking unit over the real
// structural provider, then rebuilds it after an edit inside the body of the
// callee's file, and compares the rebuild with a full build of the edited
// tree.
//
// Failure mode: an edit that changes no link table is resolved again, or
// republishes the facts of files it did not touch, or loses the facts of the
// file it did. b.go calls Run in a.go, and only a.go's body changes, with a
// length change so every range after the edit moves. The rebuild must hand
// Link the predecessor's resolution and read no table of b.go (a resolver
// reads every member's table; republication needs only a.go's, for Run's
// occurrence), carry b.go's facts over (Twice and its calls of Run), and
// republish a.go's from the fresh table: the unit then holds exactly what a
// full build of the edited tree holds.
//
// Mutations that fail it: resolve in full whatever opts.Previous holds (b.go's
// table is read); publish every fact on the inheriting path (b.go's table is
// read, nothing is carried); carry a.go's facts instead of republishing them
// (the carry-over is refused, a.go's bytes having changed).
func TestLinkBodyEditRelinksNothing(t *testing.T) {
	f := newFixture(t)
	ts := f.structuralProvider()
	tables := &countedTables{Tables: link.StoreTables(f.store), reads: map[string]int{}}
	lp := link.New(tables)
	a := delta.NewLink(f.store, lp, tables, limits, f.pool)
	paths := []string{"go.mod", "a.go", "b.go"}
	const goMod = "module example.com/app\n\ngo 1.27\n"
	const before = "package app\n\nfunc Run(x int) int { return x * 2 }\n"
	const after = "package app\n\nfunc Run(x int) int { return x * 20 }\n"
	const caller = "package app\n\nfunc Twice(x int) int { return Run(x) + Run(x) }\n"

	base := f.snapshot(map[string]string{"go.mod": goMod, "a.go": before, "b.go": caller})
	baseGen := f.generation(base)
	f.structural(ts, base, baseGen, "a.go")
	callerUnit := f.structural(ts, base, baseGen, "b.go")
	prev := f.request(lp, build{tree: base, gen: baseGen, cfgHash: "config", scopeKey: goScope, paths: paths})
	first, err := a.Apply(f.ctx, prev)
	if err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if !first.Full || first.FullReason != delta.FullNoPredecessor || first.Filtered {
		t.Fatalf("the first build is %+v, want a full build with no predecessor", first)
	}
	f.flush()

	edited := f.snapshot(map[string]string{"go.mod": goMod, "a.go": after, "b.go": caller})
	editedGen := f.generation(edited)
	calleeUnit := f.structural(ts, edited, editedGen, "a.go")
	f.attach(editedGen, callerUnit)
	refresh := f.request(lp, build{tree: edited, gen: editedGen, cfgHash: "config", scopeKey: goScope,
		paths: paths, previous: prev.Build.Spec.ID})
	tables.reset()
	out, err := a.Apply(f.ctx, refresh)
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if out.Full || !out.Filtered {
		t.Fatalf("the rebuild is %+v, want the predecessor's resolution handed to Link: no table moved", out)
	}
	if want := (delta.Stats{Unchanged: int64(len(paths))}); out.Delta != want {
		t.Errorf("Delta = %+v, want %+v: every member keeps its digest", out.Delta, want)
	}
	if n := tables.read("b.go"); n != 0 {
		t.Errorf("the rebuild read b.go's table %d times: it resolved, or republished the untouched file", n)
	}
	if tables.read("a.go") == 0 {
		t.Error("the rebuild never read a.go's table: Run's occurrence was not republished from the fresh table")
	}
	if out.Carried.Nodes == 0 || out.Carried.Relations == 0 || out.Carried.Evidence == 0 {
		t.Errorf("Carried = %+v, want b.go's caller, its call of Run and their evidence carried", out.Carried)
	}
	f.flush()

	fullGen := f.generation(edited)
	f.attach(fullGen, calleeUnit)
	f.attach(fullGen, callerUnit)
	full := f.request(lp, build{tree: edited, gen: fullGen, cfgHash: "full", scopeKey: goScope, paths: paths})
	if res, err := a.Apply(f.ctx, full); err != nil || !res.Full {
		t.Fatalf("full build of the edited tree: %+v, %v", res, err)
	}
	f.flush()
	db := f.raw()
	var calls int
	if err := db.QueryRow(`SELECT count(*) FROM relation_facts WHERE unit_id = ?`,
		unitRowID(t, db, refresh.Build.Spec.ID)).Scan(&calls); err != nil {
		t.Fatal(err)
	}
	if calls == 0 {
		t.Fatal("the rebuilt unit holds no call: Twice's calls of Run are lost")
	}
	sameFacts(t, db, full.Build.Spec.ID, refresh.Build.Spec.ID)
}

// countedTables counts the tables read, by path; digests are not counted.
type countedTables struct {
	link.Tables
	mu    sync.Mutex
	reads map[string]int
}

func (c *countedTables) Table(ctx context.Context, gen model.GenerationID, fv model.FileVersion) (*link.Table, bool, error) {
	c.mu.Lock()
	c.reads[fv.Path]++
	c.mu.Unlock()
	return c.Tables.Table(ctx, gen, fv)
}

func (c *countedTables) read(path string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads[path]
}

func (c *countedTables) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.reads)
}

// structuralProvider is the real structural provider over this test binary
// as its parser worker.
func (f *fixture) structuralProvider() *treesitter.Provider {
	f.t.Helper()
	exe, err := os.Executable()
	if err != nil {
		f.t.Fatal(err)
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		f.t.Fatal(err)
	}
	room, err := admission.NewLedger(4<<30, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	runner, err := process.NewRunner(process.Limits{MaxConcurrent: 2, MemoryBudgetBytes: 2 << 30, DiskBudgetBytes: 1 << 30})
	if err != nil {
		f.t.Fatal(err)
	}
	// The worker runs in WorkDir, which must exist: a missing working
	// directory fails the start as `fork/exec <worker>: no such file or
	// directory`, naming the executable, not the directory.
	p, err := treesitter.New(treesitter.Options{MaxWorkers: 1, Rederive: func(int64) {}, Admission: room,
		Worker: treesitter.WorkerCommand{Path: exe, Args: []string{wire.Subcommand}}, Runner: runner,
		WorkDir: f.t.TempDir()})
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(p.Close)
	return p
}

// structural seals the structural unit of one file into gen, which stores the
// file's link table for the linking unit to read, and returns its id.
func (f *fixture) structural(p provider.Provider, tr *tree, gen model.GenerationID, path string) model.UnitID {
	f.t.Helper()
	req := f.request(p, build{tree: tr, gen: gen, cfgHash: "config", scopeKey: lang.ScopePrefix + path, paths: []string{path}})
	w, err := f.store.BeginUnit(f.ctx, gen, req.Build, req.Inputs)
	if err != nil {
		f.t.Fatalf("BeginUnit: %v", err)
	}
	res, err := provider.RunUnit(f.ctx, p, req.Unit, provider.StoreUnit(f.store, w), limits, f.pool)
	if cerr := f.store.CompleteProviderRun(f.ctx, res, provider.CodeOf(err)); cerr != nil {
		f.t.Fatalf("CompleteProviderRun: %v", cerr)
	}
	if err != nil {
		f.t.Fatalf("structural unit of %s: %v", path, err)
	}
	return req.Build.Spec.ID
}

// attach reuses a sealed unit in gen, as the coordinator does for a file
// whose bytes did not move.
func (f *fixture) attach(gen model.GenerationID, unit model.UnitID) {
	f.t.Helper()
	if err := f.store.AttachUnit(f.ctx, gen, unit); err != nil {
		f.t.Fatalf("AttachUnit: %v", err)
	}
}
