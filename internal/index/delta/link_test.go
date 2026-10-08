package delta_test

import (
	"context"
	"os"
	"path/filepath"
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

// TestLinkBodyEditRelinksNothing builds one Go linking unit twice over the
// real structural provider, the second time after a literal inside a function
// body changed.
//
// Failure mode: an edit that changes neither link table is resolved again in
// full. The file's bytes moved, so the linking unit is rebuilt; its table's
// digest did not, so the build must hand Link the predecessor's resolution and
// resolve nothing, and what it republishes must be exactly what the first
// build published.
//
// The edit (`* 2` to `* 3`) keeps the file's length, so every range, and with
// it every structural identity, is unchanged; that is what lets the rebuilt
// unit's facts be compared row for row with the first build's.
//
// Mutation that fails it: hand Link no previous state when every digest
// matches (drop `opts.Previous = f` in link.go): Filtered is then false.
func TestLinkBodyEditRelinksNothing(t *testing.T) {
	f := newFixture(t)
	ts := f.structuralProvider()
	lp := link.New(link.StoreTables(f.store))
	a := delta.NewLink(f.store, lp, link.StoreTables(f.store), limits, f.pool)
	paths := []string{"go.mod", "a.go"}
	const goMod = "module example.com/app\n\ngo 1.27\n"
	const before = "package app\n\nfunc helper(x int) int { return x + 1 }\n\nfunc Run(x int) int { return helper(x) * 2 }\n"
	const after = "package app\n\nfunc helper(x int) int { return x + 1 }\n\nfunc Run(x int) int { return helper(x) * 3 }\n"

	base := f.snapshot(map[string]string{"go.mod": goMod, "a.go": before})
	baseGen := f.generation(base)
	f.structural(ts, base, baseGen, "a.go")
	prev := f.request(lp, build{tree: base, gen: baseGen, cfgHash: "config", scopeKey: goScope, paths: paths})
	first, err := a.Apply(f.ctx, prev)
	if err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if !first.Full || first.FullReason != delta.FullNoPredecessor || first.Filtered {
		t.Fatalf("the first build is %+v, want a full build with no predecessor", first)
	}
	f.flush()

	edited := f.snapshot(map[string]string{"go.mod": goMod, "a.go": after})
	editedGen := f.generation(edited)
	f.structural(ts, edited, editedGen, "a.go")
	refresh := f.request(lp, build{tree: edited, gen: editedGen, cfgHash: "config", scopeKey: goScope,
		paths: paths, previous: prev.Build.Spec.ID})
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
	f.flush()
	db := f.raw()
	var calls int
	if err := db.QueryRow(`SELECT count(*) FROM relation_facts WHERE unit_id = ?`,
		unitRowID(t, db, refresh.Build.Spec.ID)).Scan(&calls); err != nil {
		t.Fatal(err)
	}
	if calls == 0 {
		t.Fatal("the rebuilt unit publishes no call: Run's call of helper is not re-emitted")
	}
	sameFacts(t, db, prev.Build.Spec.ID, refresh.Build.Spec.ID)
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
	p, err := treesitter.New(treesitter.Options{MaxWorkers: 1, Rederive: func(int64) {}, Admission: room,
		Worker: treesitter.WorkerCommand{Path: exe, Args: []string{wire.Subcommand}}, Runner: runner,
		WorkDir: filepath.Join(f.dir, "parsers")})
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(p.Close)
	return p
}

// structural seals the structural unit of one file into gen, which stores the
// file's link table for the linking unit to read.
func (f *fixture) structural(p provider.Provider, tr *tree, gen model.GenerationID, path string) {
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
}
