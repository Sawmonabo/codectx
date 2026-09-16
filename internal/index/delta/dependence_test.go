package delta_test

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/index/delta"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/dependence"
	"github.com/Sawmonabo/codectx/internal/provider/dependence/neo4jcsv"
)

// goScope is the scope key of the root Go module of the trees below.
const goScope = "pkg:go:"

// baseTree is the predecessor's source. Every case below moves one file of it.
var baseTree = map[string]string{
	"go.mod": "module example.com/app\n\ngo 1.27\n",
	"app.go": "package app\n\nfunc Run(x int) int { return x + 1 }\n",
	"lib.go": "package app\n\nfunc Help() string { return \"help\" }\n",
}

// fakeBackend stands in for the analysis engine: it writes the artifacts the
// provider validates — a graph file and an export directory with one row — so
// the applier's own path runs without a toolchain. The export it writes is not
// one any real reader can import, which is why the importer is replaced with
// it.
type fakeBackend struct{}

func (fakeBackend) Engine() dependence.Engine {
	return dependence.Engine{ParseArgv: []string{"/opt/payload/parse"}, ExportArgv: []string{"/opt/payload/export"},
		Version: "1.0.0", Digest: strings.Repeat("a", 64), RuntimeDigest: strings.Repeat("b", 64)}
}

func (fakeBackend) Argv(dependence.Family) []string { return []string{"--pinned"} }

func (fakeBackend) NeutralOptions(dependence.Family) []string { return nil }

func (fakeBackend) Parse(_ context.Context, req dependence.ParseRequest) (dependence.Outcome, error) {
	return dependence.Outcome{}, os.WriteFile(req.OutputPath, []byte("graph"), 0o600)
}

func (fakeBackend) Export(_ context.Context, req dependence.ExportRequest) (dependence.ExportOutcome, error) {
	if err := os.MkdirAll(req.OutputDir, 0o700); err != nil {
		return dependence.ExportOutcome{}, err
	}
	rows := "1,METHOD,Run\n"
	if err := os.WriteFile(filepath.Join(req.OutputDir, "nodes_METHOD_data.csv"), []byte(rows), 0o600); err != nil {
		return dependence.ExportOutcome{}, err
	}
	return dependence.ExportOutcome{Live: true, Bytes: int64(len(rows))}, nil
}

// factImporter is the synthetic report the coordinator's applier consumes: one
// located node fact per source path, keyed by the path and its bytes, so an
// edited file moves its key and an untouched one does not. It honours the
// filter the applier asks for — a supplied previous key set emits only the
// keys that are new — which is the behaviour the replaced-set decisions are
// built on.
type factImporter struct{ paths []string }

func (im *factImporter) Import(ctx context.Context, _ string, res provider.Resolver,
	sink provider.Sink, opts neo4jcsv.Options) (dependence.ImportReport, error) {

	// The keyed puts are how a fact reaches fact_keys, which is what the
	// replaced-key set deletes against; the production reader reaches them
	// through the same assertion.
	keyed, ok := sink.(provider.DeltaSink)
	if !ok {
		return dependence.ImportReport{}, errNotKeyed
	}
	files := map[string]model.FileVersion{}
	if err := opts.Content.EachFile(ctx, model.FileSelection{}, func(fv model.FileVersion) error {
		files[fv.Path] = fv
		return nil
	}); err != nil {
		return dependence.ImportReport{}, err
	}
	previous, err := keysOf(opts.PreviousKeys)
	if err != nil {
		return dependence.ImportReport{}, err
	}
	var report dependence.ImportReport
	fresh := make([]string, 0, len(im.paths)+1)
	// The index-level fact names no file, so no per-path bucket can hold it
	// and no per-path replacement can drop it. Its key never moves, which is
	// what makes it the fact a filtered emit leaves to the carry-over.
	indexKey := factKey("", opts.UnitScopeKey)
	fresh = append(fresh, indexKey)
	if _, held := previous[indexKey]; !held {
		if err := im.put(ctx, keyed, res, opts, indexKey,
			"unit:"+opts.UnitScopeKey, model.FileVersion{}); err != nil {
			return dependence.ImportReport{}, err
		}
		report.Nodes++
	}
	for _, p := range slices.Sorted(slices.Values(im.paths)) {
		fv, ok := files[p]
		if !ok {
			return dependence.ImportReport{}, os.ErrNotExist
		}
		key := factKey(p, fv.ContentHash)
		fresh = append(fresh, key)
		if _, held := previous[key]; held {
			// The filtered emit leaves an unchanged relation unpublished:
			// only the carry-over can put it in this unit.
			continue
		}
		if err := im.put(ctx, keyed, res, opts, key, "method:"+p, fv); err != nil {
			return dependence.ImportReport{}, err
		}
		report.Nodes++
	}
	if report.Keys, err = writeKeys(opts.KeysPath, fresh); err != nil {
		return dependence.ImportReport{}, err
	}
	d, err := report.Keys.Diff(opts.PreviousKeys, nil)
	if err != nil {
		return dependence.ImportReport{}, err
	}
	report.Changed, report.Unchanged, report.Removed = d.Changed, d.Unchanged, d.Removed
	return report, nil
}

// errNotKeyed is the refusal of a sink that cannot take fact keys: a
// dependence import that published facts without them would leave the
// carry-over's replaced-key set nothing to delete against.
var errNotKeyed = errors.New("the delta build's sink takes no fact keys")

// put publishes one keyed node fact. A zero FileVersion is the fileless fact
// the unit's index-level bucket holds.
func (im *factImporter) put(ctx context.Context, sink provider.DeltaSink, res provider.Resolver,
	opts neo4jcsv.Options, key, nativeKey string, fv model.FileVersion) error {

	cand := model.NodeCandidate{ProviderID: dependence.ProviderID, ScopeKey: opts.UnitScopeKey,
		NativeKey: nativeKey, Kind: model.NodeFunction, Language: opts.Language, Name: nativeKey}
	resolution, err := res.Resolve(ctx, cand)
	if err != nil {
		return err
	}
	e := model.Evidence{UnitID: opts.Unit.ID, ProviderID: opts.Unit.ProviderID,
		ProviderVersion: opts.Unit.ProviderVersion, OriginRunID: opts.Run, NodeID: resolution.Node.ID,
		Precision: model.PrecisionStaticAnalysis, FileID: fv.ID, ContentHash: fv.ContentHash}
	e.ID = model.NewEvidenceID(e)
	fact := model.NodeFact{Node: resolution.Node, CanonicalKey: resolution.CanonicalKey, Evidence: []model.Evidence{e}}
	return sink.PutKeyedNodes(ctx, []model.NodeFact{fact}, [][]string{{key}})
}

// factKey is an id-independent fact key: the unit of replacement is the fact,
// so the key moves with the bytes the fact was read from.
func factKey(path, contentHash string) string {
	sum := sha256.Sum256([]byte(path + "\x00" + contentHash))
	return hex.EncodeToString(sum[:])
}

// writeKeys stores a key set in the on-disk form the applier round-trips
// through storage: the format line and every key in ascending order.
func writeKeys(path string, keys []string) (neo4jcsv.KeySet, error) {
	f, err := os.Create(path)
	if err != nil {
		return neo4jcsv.KeySet{}, err
	}
	w := bufio.NewWriter(f)
	if _, err := w.WriteString("codectx-dependence-keyset v1\n"); err != nil {
		f.Close()
		return neo4jcsv.KeySet{}, err
	}
	for _, k := range slices.Sorted(slices.Values(keys)) {
		if _, err := w.WriteString(k + "\n"); err != nil {
			f.Close()
			return neo4jcsv.KeySet{}, err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return neo4jcsv.KeySet{}, err
	}
	if err := f.Close(); err != nil {
		return neo4jcsv.KeySet{}, err
	}
	return neo4jcsv.LoadKeySet(path)
}

// keysOf reads a key set back. The sets here are three keys long, so holding
// one is a test fixture's liberty and not the streaming contract's.
func keysOf(k neo4jcsv.KeySet) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	if k.Empty() {
		return out, nil
	}
	raw, err := os.ReadFile(k.Path())
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(raw), "\n")[1:] {
		if line != "" {
			out[line] = struct{}{}
		}
	}
	return out, nil
}

// newDependence binds the applier over the real store and a provider whose
// engine and export reader are the stand-ins above. The provider is returned
// with it because a unit is keyed by its descriptor: the request a test builds
// must name the provider the applier will run.
func newDependence(f *fixture, im *factImporter) (delta.Applier, *dependence.Provider) {
	f.t.Helper()
	p, err := dependence.NewWithImporter(fakeBackend{}, im, dependence.Options{
		DataDir: filepath.Join(f.dir, "dependence"), Timeout: 2 * time.Minute, CacheBytes: 1 << 20, Limits: limits})
	if err != nil {
		f.t.Fatal(err)
	}
	return delta.NewDependence(f.store, p, limits, f.pool), p
}

// applyWithin runs one build and fails with its own message if the applier has
// not returned within d. A refresh that waits on itself would otherwise be
// reported by the package's timeout, which names no call and no reason.
func applyWithin(t *testing.T, a delta.Applier, ctx context.Context, req delta.Request, d time.Duration) delta.Result {
	t.Helper()
	type outcome struct {
		res delta.Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := a.Apply(ctx, req)
		done <- outcome{res, err}
	}()
	select {
	case out := <-done:
		if out.err != nil {
			t.Fatalf("Apply: %v", out.err)
		}
		return out.res
	case <-time.After(d):
		t.Fatalf("Apply did not return within %s: the replaced-input walk is waiting on the ingestion "+
			"group its own carry-over holds", d)
		return delta.Result{}
	}
}

// TestDependenceRefresh drives the dependence applier's refresh path against a
// real store for the three shapes a moved tree can take, and holds each
// refreshed unit against a full import of the same tree.
//
// Requirement (Section 11.4): a unit built as a delta holds exactly the facts
// a full build of the same tree holds, and inherits only what the emit it
// requested left unpublished. Which of the two it did is Result.Filtered, and
// each case pins it: a replaced or removed declared input forces the
// unfiltered emit that republishes everything, and only an addition keeps the
// filter and inherits rows.
//
// Mutations that fail it, one per case:
//
//   - (a) one replaced input — read the predecessor's declared inputs on the
//     ingestion group's own connection (`s.read` back to `s.readOwn` at the two
//     sites in storage's UnitInputs): the carry-over's own call holds that
//     connection, the refresh waits on itself, and applyWithin reports it by
//     its own timeout.
//   - (b) no replaced input — `IndexLevel: true` in dependence.go, so the
//     filtered emit's index-level bucket is dropped although that emit never
//     republishes it: nothing is inherited and the refreshed unit seals
//     missing three rows the full import holds.
//   - (c) a removed input — drop the `fn(a.FileID)` call from the "the
//     predecessor declared it and this unit does not" branch in inputs.go: the
//     gone file's bucket is never named, and CarryOver refuses a unit that
//     would inherit facts about a file it does not declare.
func TestDependenceRefresh(t *testing.T) {
	cases := []struct {
		name string
		// files is the refreshed tree, paths the inputs the unit declares
		// over it, and facts the paths the import publishes a fact for.
		files map[string]string
		paths []string
		// filtered is the emit the build must have requested and kept.
		filtered bool
		// carried is the fact count the unit inherited rather than imported.
		carried  int64
		changed  int64
		unchangd int64
		removed  int64
	}{
		{
			name:  "one replaced input",
			files: withFile(baseTree, "app.go", "package app\n\nfunc Run(x int) int { return x + 2 }\n"),
			paths: []string{"go.mod", "app.go", "lib.go"},
			// app.go is replaced, so the whole emit is republished and
			// nothing can be inherited.
			filtered: false, carried: 0, changed: 1, unchangd: 2, removed: 1,
		},
		{
			name:  "no replaced input",
			files: withFile(baseTree, "extra.go", "package app\n\nfunc More() {}\n"),
			paths: []string{"go.mod", "app.go", "lib.go", "extra.go"},
			// An added path names nothing of the predecessor: the filter
			// holds and the three unchanged facts — two located and the
			// index-level one — are inherited.
			filtered: true, carried: 3, changed: 1, unchangd: 3, removed: 0,
		},
		{
			name:  "a removed input",
			files: withoutFile(baseTree, "lib.go"),
			paths: []string{"go.mod", "app.go"},
			// lib.go is gone: its bucket is replaced, the emit is
			// unfiltered, and the fact it held is neither re-imported nor
			// carried.
			filtered: false, carried: 0, changed: 0, unchangd: 2, removed: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			im := &factImporter{paths: []string{"app.go", "lib.go"}}
			a, p := newDependence(f, im)
			if a.Kind() != delta.KindDependenceKeys {
				t.Fatalf("applier kind %q, want %q", a.Kind(), delta.KindDependenceKeys)
			}

			base := f.snapshot(baseTree)
			prev := f.request(p, build{tree: base, gen: f.generation(base),
				cfgHash: "config-base", scopeKey: goScope, paths: []string{"go.mod", "app.go", "lib.go"}})
			res := applyWithin(t, a, f.ctx, prev, 2*time.Minute)
			if !res.Full || res.FullReason != delta.FullNoPredecessor {
				t.Fatalf("the first build is %+v, want a full build with no predecessor", res)
			}
			// The predecessor is committed, as it is by the activation of the
			// generation it belongs to, before the run that carries from it.
			f.flush()

			fresh := f.snapshot(tc.files)
			im.paths = sourcePaths(tc.paths)

			full := f.request(p, build{tree: fresh, gen: f.generation(fresh),
				cfgHash: "config-full", scopeKey: goScope, paths: tc.paths})
			if out := applyWithin(t, a, f.ctx, full, 2*time.Minute); !out.Full {
				t.Fatalf("the baseline import is %+v, want a full build", out)
			}

			refresh := f.request(p, build{tree: fresh, gen: f.generation(fresh),
				cfgHash: "config-refresh", scopeKey: goScope, paths: tc.paths, previous: prev.Build.Spec.ID})
			// A minute is far inside the package's own bound, so the message
			// above is what a waiting refresh reports.
			out := applyWithin(t, a, f.ctx, refresh, time.Minute)
			if out.Full {
				t.Fatalf("the refresh is %+v, want a delta against the predecessor", out)
			}
			if out.Filtered != tc.filtered {
				t.Errorf("Filtered = %v, want %v: the emit this build requested decides what may be inherited",
					out.Filtered, tc.filtered)
			}
			if got := (delta.Stats{Changed: tc.changed, Unchanged: tc.unchangd, Removed: tc.removed}); out.Delta != got {
				t.Errorf("Delta = %+v, want %+v", out.Delta, got)
			}
			if out.Carried.Nodes != tc.carried {
				t.Errorf("carried %d node facts, want %d", out.Carried.Nodes, tc.carried)
			}
			f.flush()
			sameFacts(t, f.raw(), full.Build.Spec.ID, refresh.Build.Spec.ID)
		})
	}
}

// sourcePaths are the declared inputs a fact is published for: every path but
// the project marker.
func sourcePaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if p != "go.mod" {
			out = append(out, p)
		}
	}
	return out
}

func withFile(base map[string]string, path, content string) map[string]string {
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	out[path] = content
	return out
}

func withoutFile(base map[string]string, path string) map[string]string {
	out := map[string]string{}
	for k, v := range base {
		if k != path {
			out[k] = v
		}
	}
	return out
}
