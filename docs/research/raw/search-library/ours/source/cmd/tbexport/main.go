// Command tbexport (benchmark harness) runs the real `codectx index` in-process
// over <repo> and records, from inside the store's write path, every search
// document the filesystem and tree-sitter providers persisted:
//
//	tbexport <repo> <out>
//
// It writes <out>/docs.jsonl, <out>/terms.tsv and <out>/prereq/{source.json,units.jsonl}.
package main

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sync"
	"syscall"

	"github.com/Sawmonabo/codectx/internal/cli"
	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/worker"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
	"github.com/Sawmonabo/codectx/internal/tbench"
)

var exported = map[string]bool{"filesystem": true, "treesitter": true}

type unitState struct {
	key      string
	build    model.UnitBuild
	inputs   []model.UnitInput
	docs     *os.File
	docsW    *bufio.Writer
	nodes    *os.File
	nodesW   *bufio.Writer
	refs     map[model.NodeID]bool
	count    int
	repo     model.RepositoryID
	gen      model.GenerationID
	provider string
}

type observer struct {
	tmp string

	mu       sync.Mutex
	units    map[*sqlite.UnitWriter]*unitState
	seq      int
	out      *bufio.Writer
	unitsOut *bufio.Writer
	df       map[string]int64
	byProv   map[string]int64
	byKind   map[string]int64
	excluded map[string]int64
	sealed   int
	repo     model.RepositoryID
	gen      model.GenerationID
	snap     model.SnapshotID
	err      error
}

func (o *observer) fail(err error) {
	if err != nil && o.err == nil {
		o.err = err
	}
}

func (o *observer) UnitBegun(w *sqlite.UnitWriter, inputs []model.UnitInput) {
	b := w.TBBuild()
	o.mu.Lock()
	defer o.mu.Unlock()
	if !exported[b.Spec.ProviderID] {
		return
	}
	if o.snap == "" {
		snap, err := w.TBSnapshot(context.Background())
		if err != nil {
			o.fail(err)
			return
		}
		o.snap = snap
	}
	o.seq++
	u := &unitState{key: tbench.UnitKey(b.Spec.ProviderID, b.Spec.ScopeKey), build: b, inputs: inputs,
		refs: map[model.NodeID]bool{}, repo: w.TBRepo(), gen: w.TBGeneration(), provider: b.Spec.ProviderID}
	var err error
	if u.docs, err = os.Create(filepath.Join(o.tmp, fmt.Sprintf("%d.docs", o.seq))); err != nil {
		o.fail(err)
		return
	}
	if u.nodes, err = os.Create(filepath.Join(o.tmp, fmt.Sprintf("%d.nodes", o.seq))); err != nil {
		o.fail(err)
		return
	}
	u.docsW, u.nodesW = bufio.NewWriter(u.docs), bufio.NewWriter(u.nodes)
	o.units[w] = u
}

func (o *observer) NodesPut(w *sqlite.UnitWriter, facts []model.NodeFact) {
	o.mu.Lock()
	defer o.mu.Unlock()
	u := o.units[w]
	if u == nil {
		return
	}
	enc := json.NewEncoder(u.nodesW)
	for _, f := range facts {
		// The store requires one evidence row per node fact (seal check
		// "node facts without evidence"); the rest is not search work.
		f.Evidence = f.Evidence[:1]
		o.fail(enc.Encode(f))
	}
}

func (o *observer) SearchPut(w *sqlite.UnitWriter, docs []model.SearchUnit, counts []int64, terms [][]string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	u := o.units[w]
	if u == nil {
		o.excluded[w.TBBuild().Spec.ProviderID] += int64(len(docs))
		return
	}
	enc := json.NewEncoder(u.docsW)
	for i, d := range docs {
		d.TokenCount = counts[i]
		o.fail(enc.Encode(tbench.Doc{SearchUnit: d, Unit: u.key}))
		if d.NodeID != "" {
			u.refs[d.NodeID] = true
		}
		seen := map[string]bool{}
		for _, t := range terms[i] {
			if !seen[t] {
				seen[t] = true
				o.df[t]++
			}
		}
		o.byKind[string(d.Kind)]++
	}
	u.count += len(docs)
	o.byProv[u.provider] += int64(len(docs))
}

func (o *observer) UnitSealed(w *sqlite.UnitWriter) {
	o.mu.Lock()
	defer o.mu.Unlock()
	u := o.units[w]
	if u == nil {
		return
	}
	delete(o.units, w)
	o.sealed++
	if o.repo == "" {
		o.repo, o.gen = u.repo, u.gen
	}
	o.fail(u.docsW.Flush())
	o.fail(u.nodesW.Flush())
	if _, err := u.docs.Seek(0, io.SeekStart); err != nil {
		o.fail(err)
		return
	}
	_, err := io.Copy(o.out, u.docs)
	o.fail(err)
	rec := tbench.UnitRecord{Unit: u.key, Build: u.build, Inputs: u.inputs, Docs: u.count}
	have := map[model.NodeID]bool{}
	o.fail(tbench.Lines(u.nodes.Name(), func(line []byte) error {
		var f model.NodeFact
		if err := json.Unmarshal(line, &f); err != nil {
			return err
		}
		if u.refs[f.Node.ID] && !have[f.Node.ID] {
			have[f.Node.ID] = true
			rec.Nodes = append(rec.Nodes, f)
		}
		return nil
	}))
	if len(have) != len(u.refs) {
		o.fail(fmt.Errorf("unit %s: documents reference %d nodes, %d were put", u.key, len(u.refs), len(have)))
	}
	b, err := json.Marshal(rec)
	o.fail(err)
	_, err = o.unitsOut.Write(append(b, '\n'))
	o.fail(err)
	u.docs.Close()
	u.nodes.Close()
	os.Remove(u.docs.Name())
	os.Remove(u.nodes.Name())
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == wire.Subcommand {
		os.Exit(worker.Main(context.Background(), os.Stdin, os.Stdout, os.Stderr))
	}
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: tbexport <repo> <out>")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, "tbexport:", err)
		os.Exit(1)
	}
}

func run(repo, out string) error {
	repo, err := filepath.Abs(repo)
	if err != nil {
		return err
	}
	out, err = filepath.Abs(out)
	if err != nil {
		return err
	}
	pre := tbench.PrereqDir(filepath.Join(out, "docs.jsonl"))
	tmp := filepath.Join(pre, "tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	// The data directory is the default one, under a cache home inside <out>,
	// so the run never touches the operator's own cache.
	cacheHome := filepath.Join(out, "export-cache")
	if err := os.RemoveAll(cacheHome); err != nil {
		return err
	}
	if err := os.Setenv("XDG_CACHE_HOME", cacheHome); err != nil {
		return err
	}
	docsF, err := os.Create(filepath.Join(out, "docs.jsonl"))
	if err != nil {
		return err
	}
	defer docsF.Close()
	unitsF, err := os.Create(filepath.Join(pre, "units.jsonl"))
	if err != nil {
		return err
	}
	defer unitsF.Close()
	o := &observer{tmp: tmp, units: map[*sqlite.UnitWriter]*unitState{}, out: bufio.NewWriterSize(docsF, 1<<20),
		unitsOut: bufio.NewWriterSize(unitsF, 1<<20), df: map[string]int64{}, byProv: map[string]int64{},
		byKind: map[string]int64{}, excluded: map[string]int64{}}
	sqlite.TBObserve = o

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	build := model.CurrentBuildInfo()
	root := cli.NewRoot(build, os.Stderr, os.Stderr)
	if err := cli.Execute(ctx, build, root, []string{"index", "--repo", repo}); err != nil {
		return fmt.Errorf("codectx index: %w", err)
	}
	sqlite.TBObserve = nil
	if o.err != nil {
		return o.err
	}
	if len(o.units) != 0 {
		fmt.Fprintf(os.Stderr, "note: %d exported units began and never sealed; their documents are not in the export\n", len(o.units))
	}
	if err := o.out.Flush(); err != nil {
		return err
	}
	if err := o.unitsOut.Flush(); err != nil {
		return err
	}
	os.RemoveAll(tmp)
	if err := writeSource(ctx, repo, pre, o); err != nil {
		return err
	}
	if err := writeTerms(filepath.Join(out, "terms.tsv"), o.df); err != nil {
		return err
	}
	var total int64
	for _, n := range o.byProv {
		total += n
	}
	fmt.Printf("self-check: %d documents in %d units, %d distinct terms\n", total, o.sealed, len(o.df))
	printCounts("provider", o.byProv)
	printCounts("kind", o.byKind)
	printCounts("not exported (other providers)", o.excluded)
	return nil
}

func printCounts(what string, m map[string]int64) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		fmt.Printf("  %s %-24s %d\n", what, k, m[k])
	}
}

func writeTerms(path string, df map[string]int64) error {
	type td struct {
		t string
		n int64
	}
	list := make([]td, 0, len(df))
	for t, n := range df {
		list = append(list, td{t, n})
	}
	slices.SortFunc(list, func(a, b td) int { return cmp.Or(cmp.Compare(b.n, a.n), cmp.Compare(a.t, b.t)) })
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 1<<20)
	for _, e := range list {
		fmt.Fprintf(w, "%s\t%d\n", e.t, e.n)
	}
	return w.Flush()
}

// writeSource reads the snapshot the real run built over back out of its
// store: the header, every file version and every blob record.
func writeSource(ctx context.Context, repo, pre string, o *observer) error {
	dataDir, err := config.DefaultDataDir(repo)
	if err != nil {
		return err
	}
	cfg, err := tbench.Config(repo, dataDir)
	if err != nil {
		return err
	}
	s, err := tbench.OpenStore(ctx, cfg, true)
	if err != nil {
		return err
	}
	defer s.Close()
	src := tbench.Source{Repo: o.repo, Root: repo, DataDir: dataDir}
	// The generation the units were sealed into may have been superseded by
	// a deferred publication over the same snapshot; read the active one.
	active, err := s.ActiveGeneration(ctx, o.repo)
	if err != nil {
		return err
	}
	snapID, err := s.TBSnapshotOf(ctx, active)
	if err != nil {
		return err
	}
	if snapID != o.snap {
		return fmt.Errorf("the active generation's snapshot %s is not the one the exported units were built over (%s)", snapID, o.snap)
	}
	if src.Ref, err = s.TBRef(ctx, active); err != nil {
		return err
	}
	if src.Snapshot, err = s.Snapshot(ctx, snapID); err != nil {
		return err
	}
	after := ""
	for {
		page, err := s.SnapshotFiles(ctx, snapID, after, model.MaxPageItems)
		if err != nil {
			return err
		}
		src.Files = append(src.Files, page...)
		if len(page) < model.MaxPageItems {
			break
		}
		after = page[len(page)-1].Path
	}
	seen := map[string]bool{}
	for _, f := range src.Files {
		if f.ContentHash == "" || seen[f.ContentHash] {
			continue
		}
		seen[f.ContentHash] = true
		b, err := s.Blob(ctx, f.ContentHash)
		if err != nil {
			return err
		}
		src.Blobs = append(src.Blobs, b)
	}
	b, err := json.Marshal(src)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(pre, "source.json"), b, 0o644)
}
