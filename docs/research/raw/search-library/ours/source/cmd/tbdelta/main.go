// Command tbdelta (benchmark harness) is the delta arm: over a store tbindex
// built, it changes one document of one unit, as an edit to that document's
// file would, and publishes a second generation the way a refresh does:
//
//	tbdelta [-unit U] <docs.jsonl> <store dir>
//
// The changed file gets a new content hash in a new snapshot; every unit whose
// inputs include it is rebuilt (the filesystem and tree-sitter providers have
// no delta applier, so a refresh rebuilds such a unit whole), every other unit
// is attached unchanged, and the generation is activated.
// Results go to <dir of docs.jsonl>/results/ours-delta.jsonl.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/Sawmonabo/codectx/internal/index"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/tbench"
)

func main() {
	unit := flag.String("unit", "", "the unit to change one document of (default: the unit with the most documents)")
	flag.Parse()
	if flag.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: tbdelta [-unit U] <docs.jsonl> <store dir>")
		os.Exit(2)
	}
	if err := run(flag.Arg(0), flag.Arg(1), *unit); err != nil {
		fmt.Fprintln(os.Stderr, "tbdelta:", err)
		os.Exit(1)
	}
}

const edit = " tbdeltaedit"

func run(docsPath, dir, chosen string) error {
	ctx := context.Background()
	docsPath, _ = filepath.Abs(docsPath)
	dir, _ = filepath.Abs(dir)
	src, err := tbench.ReadSource(docsPath)
	if err != nil {
		return err
	}
	cfgHash, err := tbench.ConfigHash(docsPath)
	if err != nil {
		return err
	}
	if chosen == "" {
		most := 0
		if err := tbench.Stream(docsPath, func(r tbench.UnitRecord, _ []model.SearchUnit) error {
			if r.Docs > most {
				chosen, most = r.Unit, r.Docs
			}
			return nil
		}); err != nil {
			return err
		}
	}
	// The chosen unit's documents name the changed file.
	var docs []model.SearchUnit
	if err := tbench.Stream(docsPath, func(r tbench.UnitRecord, d []model.SearchUnit) error {
		if r.Unit == chosen {
			docs = d
		}
		return nil
	}); err != nil {
		return err
	}
	at := slices.IndexFunc(docs, func(d model.SearchUnit) bool { return d.Body != "" })
	if at < 0 {
		return fmt.Errorf("unit %q has no document with a body to change", chosen)
	}
	docs[at].Body += edit
	changed := docs[at].FileID
	touches := func(r tbench.UnitRecord) bool {
		return slices.ContainsFunc(r.Inputs, func(in model.UnitInput) bool { return in.FileID == changed })
	}
	cfg, err := tbench.Config(src.Root, dir)
	if err != nil {
		return err
	}
	s, err := tbench.OpenStore(ctx, cfg, false)
	if err != nil {
		return err
	}
	defer s.Close()
	gen1, err := s.ActiveGeneration(ctx, src.Repo)
	if err != nil {
		return err
	}
	var oldHash string
	for _, f := range src.Files {
		if f.ID == changed {
			oldHash = f.ContentHash
		}
	}
	if oldHash == "" {
		return errors.New("the changed document's file is not in the snapshot")
	}
	newHash := model.H("tbench-delta-blob", oldHash)

	// The new snapshot: the edited file's version is new. Capture cost, not
	// search work; not timed as a phase.
	seedStart := time.Now()
	for _, b := range src.Blobs {
		if b.Hash == oldHash {
			b.Hash = newHash
			if err := s.PutBlob(ctx, b); err != nil {
				return err
			}
		}
	}
	snap := src.Snapshot
	snap.ID = model.SnapshotID(model.H("tbench-delta-snapshot", string(snap.ID)))
	snap.ManifestHash = model.H("tbench-delta-manifest", snap.ManifestHash)
	snap.CreatedAt = time.Now().UTC()
	if err := s.PutSnapshot(ctx, snap, func(yield func(model.FileVersion) error) error {
		for _, f := range src.Files {
			if f.ID == changed {
				f.ContentHash = newHash
			}
			if err := yield(f); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	gen2, err := s.BeginGeneration(ctx, src.Repo, snap.ID, cfgHash, src.Ref)
	if err != nil {
		return err
	}
	b, err := tbench.NewBuilder(s, gen2, cfg)
	if err != nil {
		return err
	}
	seed := time.Since(seedStart)

	results, err := tbench.OpenAppend(filepath.Join(tbench.ResultsDir(filepath.Dir(docsPath)), "ours-delta.jsonl"))
	if err != nil {
		return err
	}
	defer results.Close()
	u0 := tbench.Rusage()

	// A unit is rebuilt when its inputs include the edited file or it
	// depends on a rebuilt unit (its dependency hash, and so its identity,
	// moves); every other unit is attached unchanged. Seal order puts a
	// dependency before its dependents.
	type rebuildUnit struct {
		rec  tbench.UnitRecord
		docs []model.SearchUnit
	}
	moved := map[model.UnitID]model.UnitID{}
	var rebuild []rebuildUnit
	attached := 0
	var attach time.Duration
	if err := tbench.Stream(docsPath, func(r tbench.UnitRecord, d []model.SearchUnit) error {
		if touches(r) || slices.ContainsFunc(r.Build.Dependencies, func(dep model.UnitID) bool { _, ok := moved[dep]; return ok }) {
			nr := retarget(r, changed, oldHash, newHash, cfgHash, moved)
			moved[r.Build.Spec.ID] = nr.Build.Spec.ID
			if r.Unit == chosen {
				d = docs
			}
			rebuild = append(rebuild, rebuildUnit{nr, d})
			return nil
		}
		t := time.Now()
		if err := s.AttachUnit(ctx, gen2, r.Build.Spec.ID); err != nil {
			return fmt.Errorf("attach %s: %w", r.Unit, err)
		}
		attach += time.Since(t)
		attached++
		return nil
	}); err != nil {
		return err
	}

	var sum tbench.UnitTimes
	var rebuilt int64
	var names []string
	buildStart := time.Now()
	if err := s.Flush(ctx); err != nil {
		return err
	}
	current := ""
	for _, u := range rebuild {
		r := u.rec
		if r.Build.Spec.ProviderID != current {
			if err := s.Flush(ctx); err != nil {
				return err
			}
			current = r.Build.Spec.ProviderID
		}
		t, err := b.Build(ctx, r, u.docs)
		if err != nil {
			return fmt.Errorf("rebuild %s: %w", r.Unit, err)
		}
		sum.Begin += t.Begin
		sum.Nodes += t.Nodes
		sum.Put += t.Put
		sum.Seal += t.Seal
		rebuilt += int64(len(u.docs))
		names = append(names, r.Unit)
	}
	build := time.Since(buildStart)
	u1 := tbench.Rusage()

	actStart := time.Now()
	if _, err := s.Activate(ctx, gen2, gen1, model.HealthFresh, nil, index.NormalizationVersion); err != nil {
		return fmt.Errorf("activate: %w", err)
	}
	if err := s.Flush(ctx); err != nil {
		return err
	}
	act := time.Since(actStart)
	u2 := tbench.Rusage()
	disk := tbench.DirBytes(dir)

	type phase struct {
		Arm     string `json:"arm"`
		Phase   string `json:"phase"`
		WallMS  int64  `json:"wall_ms"`
		CPUMS   *int64 `json:"cpu_ms"`
		PeakRSS int64  `json:"peak_rss_bytes"`
		Disk    int64  `json:"disk_bytes"`
		Docs    int64  `json:"docs"`
		Note    string `json:"note,omitempty"`
	}
	cpu := func(a, b tbench.Usage) *int64 { v := (b.CPU - a.CPU).Milliseconds(); return &v }
	note := fmt.Sprintf("changed one document of %s; rebuilt %v; attached %d", chosen, names, attached)
	rows := []phase{
		{Phase: "delta-seed", WallMS: seed.Milliseconds(), Note: "new blob + snapshot + generation: capture rows, not search work"},
		{Phase: "delta-attach", WallMS: attach.Milliseconds(), Note: "AttachUnit for every untouched unit"},
		{Phase: "delta-begin", WallMS: sum.Begin.Milliseconds()},
		{Phase: "delta-nodes", WallMS: sum.Nodes.Milliseconds(), Note: "rows the store requires"},
		{Phase: "delta-put", WallMS: sum.Put.Milliseconds()},
		{Phase: "delta-seal", WallMS: sum.Seal.Milliseconds()},
		{Phase: "delta-build", WallMS: build.Milliseconds(), CPUMS: cpu(u0, u1), PeakRSS: u1.PeakRSS, Note: note},
		{Phase: "delta-activation", WallMS: act.Milliseconds(), CPUMS: cpu(u1, u2), PeakRSS: u2.PeakRSS, Disk: disk},
	}
	for _, r := range rows {
		r.Arm, r.Docs = "ours", rebuilt
		if err := results.Write(r); err != nil {
			return err
		}
		fmt.Printf("%-17s wall_ms=%-6d %s\n", r.Phase, r.WallMS, r.Note)
	}
	fmt.Printf("generation %d -> %d, rebuilt documents %d, disk_bytes=%d\n", gen1, gen2, rebuilt, disk)
	return nil
}

// retarget is a unit's record as the edited snapshot plans it: the edited
// file's input carries its new hash, so the input hash and the unit identity
// move (and a moved dependency moves the dependency hash), and every node and
// evidence row names the new unit and file version.
func retarget(r tbench.UnitRecord, file model.FileID, oldHash, newHash, cfgHash string, moved map[model.UnitID]model.UnitID) tbench.UnitRecord {
	inputs := slices.Clone(r.Inputs)
	for i := range inputs {
		if inputs[i].FileID == file {
			inputs[i].ContentHash = newHash
		}
	}
	h := model.NewUnitInputHasher()
	for _, in := range inputs {
		_ = h.Add(in)
	}
	deps := slices.Clone(r.Build.Dependencies)
	for i, d := range deps {
		if n, ok := moved[d]; ok {
			deps[i] = n
		}
	}
	spec := r.Build.Spec
	spec.InputHash = h.Sum()
	spec.DependencyHash = model.DependencyHash(deps, r.Build.DependencyKeys...)
	r.Build.Dependencies = deps
	spec.ID = model.NewUnitID(spec, cfgHash)
	nodes := make([]model.NodeFact, len(r.Nodes))
	for i, f := range r.Nodes {
		ev := slices.Clone(f.Evidence)
		for j := range ev {
			ev[j].UnitID = spec.ID
			if ev[j].FileID == file && ev[j].ContentHash == oldHash {
				ev[j].ContentHash = newHash
			}
			ev[j].ID = model.NewEvidenceID(ev[j])
		}
		f.Evidence = ev
		if f.Node.FileID == file && f.Node.ContentHash == oldHash {
			f.Node.ContentHash = newHash
		}
		nodes[i] = f
	}
	r.Inputs, r.Nodes = inputs, nodes
	r.Build.Spec = spec
	return r
}
