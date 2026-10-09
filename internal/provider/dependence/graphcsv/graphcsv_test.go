package graphcsv_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/dependence/graphcsv"
	"github.com/Sawmonabo/codectx/internal/provider/providertest"
	arena "github.com/Sawmonabo/codectx/internal/scratch"
)

// run drives one import through the production sink, resolver and seal path:
// providertest.Run opens a real unit, hands the import the real byte-bounded
// sink and reconciler, and seals or fails the unit exactly as the coordinator
// will. counts is the facts the sink accepted, by relation kind and node kind.
type counts struct {
	rel    map[model.RelationKind]int
	node   map[model.NodeKind]int
	alias  map[string]string // scope\x00native key -> node id
	aliasN int               // alias records accepted, duplicates included
	detail map[string]int
	// sig is the signature each published node carried, by node name, so a
	// test can read what actually reached the sink rather than what the
	// export held.
	sig map[string]string
	// id holds the identities published under one `<kind>:<name>`: keyed by
	// kind because a fixture may publish a method and a variable of one name,
	// and a list because a name may be declared in several scopes. meta is the
	// metadata each node carried, by node name.
	id   map[string][]model.NodeID
	meta map[string]string
}

func newCounts() counts {
	return counts{rel: map[model.RelationKind]int{}, node: map[model.NodeKind]int{},
		alias: map[string]string{}, detail: map[string]int{},
		sig: map[string]string{}, id: map[string][]model.NodeID{}, meta: map[string]string{}}
}

type recorder struct {
	provider.Sink
	c *counts
}

func (r recorder) PutNodes(ctx context.Context, f []model.NodeFact) error {
	return r.PutKeyedNodes(ctx, f, nil)
}

// PutKeyedNodes counts the batch and forwards it with its keys, so the import
// takes the keyed path it takes in production; a destination that cannot carry
// keys still receives the facts. Same shape as providertest.Recorder.
func (r recorder) PutKeyedNodes(ctx context.Context, f []model.NodeFact, keys [][]string) error {
	for _, n := range f {
		r.c.node[n.Node.Kind]++
		r.c.sig[n.Node.Name] = n.Node.Signature
		key := string(n.Node.Kind) + ":" + n.Node.Name
		r.c.id[key] = append(r.c.id[key], n.Node.ID)
		r.c.meta[n.Node.Name] = string(n.Node.Metadata)
	}
	if d, ok := r.Sink.(provider.DeltaSink); ok {
		return d.PutKeyedNodes(ctx, f, keys)
	}
	return r.Sink.PutNodes(ctx, f)
}

func (r recorder) PutRelations(ctx context.Context, f []model.RelationFact) error {
	return r.PutKeyedRelations(ctx, f, nil)
}

// PutKeyedRelations is PutKeyedNodes for relation facts.
func (r recorder) PutKeyedRelations(ctx context.Context, f []model.RelationFact, keys [][]string) error {
	for _, x := range f {
		r.c.rel[x.Relation.Kind]++
		for _, ev := range x.Evidence {
			r.c.detail[ev.Detail]++
		}
	}
	if d, ok := r.Sink.(provider.DeltaSink); ok {
		return d.PutKeyedRelations(ctx, f, keys)
	}
	return r.Sink.PutRelations(ctx, f)
}

func (r recorder) PutAliases(ctx context.Context, a []model.NativeAlias) error {
	for _, x := range a {
		r.c.alias[x.ScopeKey+"\x00"+x.NativeKey] = string(x.NodeID)
		r.c.aliasN++
	}
	return r.Sink.PutAliases(ctx, a)
}

func run(t *testing.T, src, export string, opts graphcsv.Options) (graphcsv.Report, counts, model.UnitState, error) {
	t.Helper()
	files, paths := readSource(t, src)
	h := providertest.New(t, files)
	c := newCounts()
	var rep graphcsv.Report
	var importErr error
	p := providertest.Func{
		Desc: descriptor(),
		IndexFn: func(ctx context.Context, req provider.UnitRequest, sink provider.Sink) (model.ProviderResult, error) {
			o := opts
			o.UnitScopeKey, o.ProjectRoot = req.Unit.ScopeKey, t.TempDir()
			o.Limits = providertest.Limits
			o.Repository, o.Unit, o.Run, o.Content = req.Binding.RepositoryID, req.Unit, req.Run, req.Content
			if o.ScratchDir == "" {
				o.ScratchDir = t.TempDir()
			}
			rep, importErr = graphcsv.Import(ctx, export, req.Resolver, recorder{Sink: sink, c: &c}, o)
			if importErr != nil {
				return model.ProviderResult{}, importErr
			}
			return providertest.Succeeded(req, uint64(rep.Nodes+rep.Aliases), rep.BytesRead), nil
		},
	}
	_, id, err := h.Run(t, p, "pkg:fixture", paths)
	state, _ := h.UnitState(t, id)
	if importErr != nil {
		err = importErr
	}
	return rep, c, state, err
}

// readSource loads a fixture source tree as the harness's repository content.
func readSource(t *testing.T, dir string) (map[string]string, []string) {
	t.Helper()
	files := map[string]string{}
	var paths []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		files[rel] = string(b)
		paths = append(paths, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("read fixture %s: %v", dir, err)
	}
	return files, paths
}

// TestImport drives the import over the recorded exports. Every fixture under
// testdata is a real `--repr=all --format=neo4jcsv` export of the matching
// source tree, produced by the pinned engine; the cases below protect the
// behaviors whose silent breakage would publish facts about the wrong bytes,
// admit an unsealed or fabricated fact, or make a refresh rewrite a whole
// unit.
func TestImport(t *testing.T) {
	cases := []struct {
		name   string
		src    string
		export string
		check  func(t *testing.T, rep graphcsv.Report, c counts)
	}{{
		// The c, go, java and python sources hold assignments and call no
		// function they declare: their exports carry the CDG, REACHING_DEF and
		// assignment rows the four families could be derived from. Their
		// assertions are the shared ones after the table: the unit seals,
		// every key is new, and no relation reaches the sink.
		name: "c", src: "src/c", export: "c",
	}, {
		name: "go", src: "src/golang", export: "golang",
	}, {
		name: "java", src: "src/javasrc", export: "javasrc",
	}, {
		name: "javascript invented callee", src: "src/jssrc", export: "jssrc",
		check: func(t *testing.T, rep graphcsv.Report, c counts) {
			// Failure mode: the export marks a callee it invented -- a method
			// it emitted with no definition anywhere in the graph, so that an
			// unresolved site still has a target -- and the import drops the
			// mark, so its node cannot be told from a real dependency.
			// `__ecma.Array:` is the invented callee of this export.
			if !strings.Contains(c.meta["__ecma.Array:"], `"resolution":"speculated"`) {
				t.Errorf("the invented callee's node metadata = %q, want a speculated resolution", c.meta["__ecma.Array:"])
			}
		},
	}, {
		name: "python", src: "src/pythonsrc", export: "pythonsrc",
	}, {
		// Failure mode: an export label this import does not map is emitted as
		// a fact instead of being counted. The Rust export's CONFIG_FILE row
		// (its Cargo.toml) is the real unmapped label.
		name: "rust unknown label counted", src: "src/rust", export: "rust",
		check: func(t *testing.T, rep graphcsv.Report, c counts) {
			if rep.UnknownLabels["CONFIG_FILE"] != 1 {
				t.Errorf("unknown labels = %v; the export's CONFIG_FILE row must be counted, not emitted", rep.UnknownLabels)
			}
			if c.node[model.NodeConfiguration] != 0 {
				t.Errorf("an unmapped label was published as a node")
			}
		},
	}, {
		// The forward-id case: every edges_* file sorts before every nodes_*
		// file, so every call edge here was staged before its endpoints' rows
		// were read, and the unit still seals with no relation published.
		name: "calls", src: "src/gofix", export: "gofix",
	}, {
		// A method reached through a variable (`const f = helper; f(i)`): the
		// export binds `f(i)` to an invented callee named `f`, never to
		// `helper`.
		name: "javascript method reached through a value", src: "src/jsvalue", export: "jsvalue",
		check: func(t *testing.T, rep graphcsv.Report, c counts) {
			// Failure mode: this invented callee is the one the engine parks
			// under the enclosing program rather than under its speculated
			// namespace, so a marker read off that namespace publishes it as a
			// real import -- a dependency on `f` the source never states. It
			// is the only external method this export publishes, so no node
			// here may carry an import resolution.
			//
			// Mutation that fails it: in project
			// (scratch.go), drop the second arm of the invented table's
			// INSERT, leaving only the speculated-namespace test; `f` is then
			// published with a resolution of import.
			var speculated int
			for name, meta := range c.meta {
				if strings.Contains(meta, `"resolution":"import"`) {
					t.Errorf("node %q published as a real import; the export invented it for the call through the value", name)
				}
				if strings.Contains(meta, `"resolution":"speculated"`) {
					speculated++
				}
			}
			if speculated == 0 {
				t.Errorf("no node carries a speculated resolution; the invented callee is indistinguishable from a real dependency")
			}
		},
	}, {
		// Same shape in Python, where the export binds the call site to
		// nothing at all.
		name: "python method reached through a value", src: "src/pyvalue", export: "pyvalue",
	}, {
		// Failure mode: a callee that lives in another unit is dropped as
		// junk, losing the declaration the reconciler binds by full name. The
		// export is the same module parsed without its second package, so
		// helper.Scale is a real IS_EXTERNAL stub.
		name: "external declaration kept and aliased", src: "src/goapp", export: "goapp",
		check: func(t *testing.T, rep graphcsv.Report, c counts) {
			if rep.ExternalMethods != 1 {
				t.Errorf("external methods = %d, want 1", rep.ExternalMethods)
			}
			if _, ok := c.alias[provider.ScopeWorkspace+"\x00"+graphcsv.ProviderID+":method:example.com/fix/helper.Scale"]; !ok {
				t.Errorf("the external declaration was not aliased by full name; aliases = %v", keysOf(c.alias))
			}
		},
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep, c, state, err := run(t, filepath.Join("testdata", tc.src), filepath.Join("testdata", tc.export), graphcsv.Options{Language: "go"})
			if err != nil {
				t.Fatalf("Import: %v", err)
			}
			if state != model.UnitSealed {
				t.Fatalf("unit state = %q, want sealed", state)
			}
			if rep.Keys.Count() == 0 || rep.Changed != rep.Keys.Count() || rep.Removed != 0 {
				t.Errorf("full import reported %d/%d changed and %d removed; every key is new against the absent set",
					rep.Changed, rep.Keys.Count(), rep.Removed)
			}
			// Failure mode: the import publishes a relation, so storage holds
			// two producers' facts for one relation and every consumer
			// double-counts it: `calls` is the linking provider's, and control
			// dependence, data flow, reads and writes the structural
			// provider's. Every fixture's export carries call edges and the
			// CDG, REACHING_DEF and assignment rows the other families could
			// be derived from, so each language is a case.
			// Mutation that fails it: hand the projected call edges to the
			// sink's PutRelations in Import -> the gofix case puts its `calls`
			// edges to the sink.
			if len(c.rel) != 0 {
				t.Errorf("relations put to the sink: %v; the importer publishes no relation", c.rel)
			}
			if tc.check != nil {
				tc.check(t, rep, c)
			}
		})
	}
}

// TestImportRefusesMalformedExport proves the two ways an export can be
// hostile are refused before a fact is admitted: a record larger than the
// sink's record bound must fail before the decoder allocates it, and a
// declaration whose file is not in the pinned snapshot must be dropped rather
// than bound to whatever file the snapshot does hold at that path.
func TestImportRefusesMalformedExport(t *testing.T) {
	t.Run("oversized record refused before allocation", func(t *testing.T) {
		dir := copyExport(t, filepath.Join("testdata", "c"), func(name string, data []byte) []byte {
			if name != "nodes_CALL_data.csv" {
				return data
			}
			// One record far over providertest.Limits.MaxRecordBytes, built
			// here rather than committed so the fixture stays small.
			return append(data, []byte("\n1,CALL,-1,,,\""+strings.Repeat("x", 4*int(providertest.Limits.MaxRecordBytes))+"\",1,STATIC_DISPATCH,,1,<operator>.assignment,a,,,1,,,,ANY\n")...)
		})
		_, _, state, err := run(t, filepath.Join("testdata", "src", "c"), dir, graphcsv.Options{Language: "c"})
		var typed *model.Error
		if !errors.As(err, &typed) || typed.Code != model.CodeResourceLimit {
			t.Fatalf("err = %v, want %s", err, model.CodeResourceLimit)
		}
		if state == model.UnitSealed {
			t.Fatalf("the unit sealed on a refused export")
		}
	})

	t.Run("mismatched source path dropped", func(t *testing.T) {
		// The C export's facts name w.c; the repository here holds only
		// other.c, so nothing may be published against it.
		rep, c, _, err := runFiles(t, map[string]string{"other.c": "int g;\n"}, filepath.Join("testdata", "c"))
		if err != nil {
			t.Fatalf("Import: %v", err)
		}
		if rep.DroppedMethods == 0 {
			t.Fatalf("no entity was dropped although no exported path is in the snapshot")
		}
		if n := c.node[model.NodeFunction]; n != 0 {
			t.Fatalf("%d nodes published from an export whose paths are not in the snapshot", n)
		}
	})
}

// TestOversizeDescriptiveFieldIsCutNotDropped proves the producer half of the
// storage-field contract. The model accepts an oversize storage field, so a
// field over its ceiling that is not cut here reaches the sink whole and a
// page of hits carries an unbounded response. A descriptive field is therefore
// cut to its ceiling and counted by name; the declaration is still published,
// because a clipped signature answers more than a dropped declaration does.
// Identity fields keep the opposite rule and are proven by the dropped-path
// case above.
func TestOversizeDescriptiveFieldIsCutNotDropped(t *testing.T) {
	const oversize = model.MaxSignatureBytes + 1000
	dir := copyExport(t, filepath.Join("testdata", "c"), func(name string, data []byte) []byte {
		if name != "nodes_METHOD_data.csv" {
			return data
		}
		// SIGNATURE is the last column; run's is the only non-empty one.
		return bytes.Replace(data, []byte(`"void(S*,int*,int)"`),
			[]byte(strings.Repeat("x", oversize)), 1)
	})
	rep, c, _, err := run(t, filepath.Join("testdata", "src", "c"), dir, graphcsv.Options{Language: "c"})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	got, ok := c.sig["run"]
	if !ok {
		t.Fatalf("the declaration with the oversize signature was not published at all; published %v", c.sig)
	}
	if len(got) != model.MaxSignatureBytes {
		t.Fatalf("published signature is %d bytes, want it cut to the %d-byte ceiling",
			len(got), model.MaxSignatureBytes)
	}
	if n := rep.TruncatedFields["signature"]; n != 1 {
		t.Fatalf("TruncatedFields[signature] = %d, want 1: the cut must be counted, not silent", n)
	}
}

// descriptor is the provider identity the import is driven under. It
// declares no capability, as the dependence provider does: the import
// publishes no relation.
func descriptor() model.ProviderDescriptor {
	return model.ProviderDescriptor{ID: graphcsv.ProviderID, Version: "test-1",
		InvalidationScope: model.InvalidationPackage}
}

// TestImportConformsUnderPerPutFlush runs the import through the shared
// provider conformance check, which persists after every Put instead of
// batching. Failure modes: an alias or a relation handed to the sink before
// the node fact that names its identity — storage rejects the unregistered
// identity and the unit is lost, and the batched sink every other case here
// uses hides it because 64 records reach storage together; and a node identity
// that depends on map iteration or on the order the export's files were read
// rather than on their bytes, which would make the same declaration compare
// unequal between two snapshots and defeat every reuse decision.
func TestImportConformsUnderPerPutFlush(t *testing.T) {
	files, paths := readSource(t, filepath.Join("testdata", "src", "gofix"))
	export := filepath.Join("testdata", "gofix")
	p := providertest.Func{
		Desc: descriptor(),
		IndexFn: func(ctx context.Context, req provider.UnitRequest, sink provider.Sink) (model.ProviderResult, error) {
			o := graphcsv.Options{Language: "go", UnitScopeKey: req.Unit.ScopeKey, ProjectRoot: t.TempDir(),
				Limits: providertest.Limits, Repository: req.Binding.RepositoryID, Unit: req.Unit,
				Run: req.Run, Content: req.Content, ScratchDir: t.TempDir()}
			rep, err := graphcsv.Import(ctx, export, req.Resolver, sink, o)
			if err != nil {
				return model.ProviderResult{}, err
			}
			return providertest.Succeeded(req, uint64(rep.Nodes+rep.Aliases), rep.BytesRead), nil
		},
	}
	providertest.Conform(t, p, files, "pkg:fixture", paths)
}

// TestSubdividedUnitAdmitsRepeatedIdentities protects the subdivision path. A
// subdivided unit runs the engine per part and imports every part's export
// into the one sink storage opened for the unit, and two parts legitimately
// describe the same identity — above all the external stub of a callee both
// parts reference. Storage admits a repeated node identity whose stored
// columns are identical and refuses one whose columns diverge, so nothing
// between the import and storage may drop or rewrite the repeat; what the
// import publishes for one identity must be a function of that identity
// alone. Silent breakage here loses a whole unit's facts on the one path that
// exists to rescue them.
func TestSubdividedUnitAdmitsRepeatedIdentities(t *testing.T) {
	files, paths := readSource(t, filepath.Join("testdata", "src", "gofix"))
	export := filepath.Join("testdata", "gofix")
	h := providertest.New(t, files)
	c := newCounts()
	var part [2]struct{ nodes, aliases int }
	nodesSoFar := func() int {
		n := 0
		for _, v := range c.node {
			n += v
		}
		return n
	}
	p := providertest.Func{
		Desc: descriptor(),
		IndexFn: func(ctx context.Context, req provider.UnitRequest, sink provider.Sink) (model.ProviderResult, error) {
			for i := range part {
				nodes, aliases := nodesSoFar(), c.aliasN
				o := graphcsv.Options{Language: "go", UnitScopeKey: req.Unit.ScopeKey, ProjectRoot: t.TempDir(),
					Limits: providertest.Limits, Repository: req.Binding.RepositoryID, Unit: req.Unit,
					Run: req.Run, Content: req.Content, ScratchDir: t.TempDir()}
				if _, err := graphcsv.Import(ctx, export, req.Resolver, recorder{Sink: sink, c: &c}, o); err != nil {
					return model.ProviderResult{}, err
				}
				part[i].nodes, part[i].aliases = nodesSoFar()-nodes, c.aliasN-aliases
			}
			return providertest.Succeeded(req, uint64(nodesSoFar()), 0), nil
		},
	}
	if _, id, err := h.Run(t, p, "pkg:fixture", paths); err != nil {
		t.Fatalf("Run: %v", err)
	} else if state, _ := h.UnitState(t, id); state != model.UnitSealed {
		t.Fatalf("unit state = %q, want sealed; a subdivided unit must survive two parts that share identities", state)
	}
	if part[0].nodes == 0 || part[0].aliases == 0 {
		t.Fatalf("the first part published %d node facts and %d aliases; the fixture must publish both", part[0].nodes, part[0].aliases)
	}
	if part[1].nodes != part[0].nodes || part[1].aliases != part[0].aliases {
		t.Fatalf("the second part published %d node facts and %d aliases against the first part's %d and %d; "+
			"the repeats must be identical, or storage refuses them and the unit is lost",
			part[1].nodes, part[1].aliases, part[0].nodes, part[0].aliases)
	}
}

func runFiles(t *testing.T, files map[string]string, export string) (graphcsv.Report, counts, model.UnitState, error) {
	t.Helper()
	dir := t.TempDir()
	for p, body := range files {
		if err := os.WriteFile(filepath.Join(dir, p), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return run(t, dir, export, graphcsv.Options{Language: "c"})
}

// copyExport copies an export directory, letting rewrite alter one file.
func copyExport(t *testing.T, src string, rewrite func(name string, data []byte) []byte) string {
	t.Helper()
	dst := t.TempDir()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), rewrite(e.Name(), b), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, strings.ReplaceAll(k, "\x00", " "))
	}
	return out
}

// TestStagedRowsOverAUserSetBoundImportsAndReports pins the one invariant of
// providers.dependence.max_staged_rows: crossing it publishes the whole import
// and says so. A bound that failed the unit would make a row count — a
// property of the repository's source — refuse the repository. A regression
// that refuses, or that stops setting the flag, is the silence the scale
// posture forbids, and neither shows in any other assertion.
func TestStagedRowsOverAUserSetBoundImportsAndReports(t *testing.T) {
	src, export := filepath.Join("testdata", "src", "gofix"), filepath.Join("testdata", "gofix")
	whole, _, _, err := run(t, src, export, graphcsv.Options{Language: "go"})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if whole.StagedRows == 0 {
		t.Fatalf("the import staged no rows, so the bound below proves nothing")
	}
	if whole.OverStagedRows {
		t.Fatalf("an import with no max_staged_rows reported crossing one")
	}
	bounded, _, state, err := run(t, src, export, graphcsv.Options{Language: "go", MaxStagedRows: 1})
	if err != nil {
		t.Fatalf("an import over max_staged_rows must not fail the unit: %v", err)
	}
	if state != model.UnitSealed {
		t.Fatalf("unit state = %s, want sealed: crossing a reporting threshold may not fail the unit", state)
	}
	if !bounded.OverStagedRows {
		t.Fatalf("an import of %d rows over max_staged_rows = 1 did not report crossing it", bounded.StagedRows)
	}
	if bounded.StagedRows != whole.StagedRows || bounded.Nodes != whole.Nodes {
		t.Fatalf("the bounded import published less than the unbounded one: rows %d/%d nodes %d/%d",
			bounded.StagedRows, whole.StagedRows, bounded.Nodes, whole.Nodes)
	}
}

// TestDerivedRowsOverAUserSetBoundImportsAndReports pins the same invariant for
// providers.dependence.max_derived_rows. A fixed count that failed the unit,
// or a projection query carrying a `LIMIT bound+1` that truncated it, would be
// a refusal or a silent cut on a count that belongs to the analysed source. The
// equality assertions below are what catch a truncation: a LIMIT under a bound
// of 1 leaves the JavaScript fixture's other call occurrences behind.
func TestDerivedRowsOverAUserSetBoundImportsAndReports(t *testing.T) {
	src, export := filepath.Join("testdata", "src", "jssrc"), filepath.Join("testdata", "jssrc")
	whole, _, _, err := run(t, src, export, graphcsv.Options{Language: "javascript"})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if whole.DerivedRows < 2 {
		t.Fatalf("the import derived %d occurrences, so the bound of 1 below proves nothing", whole.DerivedRows)
	}
	if whole.OverDerivedRows {
		t.Fatalf("an import with no max_derived_rows reported crossing one")
	}
	bounded, _, state, err := run(t, src, export, graphcsv.Options{Language: "javascript", MaxDerivedRows: 1})
	if err != nil {
		t.Fatalf("an import over max_derived_rows must not fail the unit: %v", err)
	}
	if state != model.UnitSealed {
		t.Fatalf("unit state = %s, want sealed: crossing a reporting threshold may not fail the unit", state)
	}
	if !bounded.OverDerivedRows {
		t.Fatalf("an import of %d occurrences over max_derived_rows = 1 did not report crossing it", bounded.DerivedRows)
	}
	if bounded.DerivedRows != whole.DerivedRows {
		t.Fatalf("the bounded import projected less than the unbounded one: derived %d/%d",
			bounded.DerivedRows, whole.DerivedRows)
	}
}

// TestOneStagingDatabaseIsReusedAcrossImports runs two imports through one
// scratch directory and looks at what they left behind.
//
// Requirement: a run reuses the space it holds and frees nothing in the middle
// of its work. A staging database created and deleted per import frees
// hundreds of megabytes per unit; on a host that discards freed blocks into a
// sparse image, that free stalls every process on the machine for about a
// minute, minutes later, with nothing able to observe or wait for it. One file
// per slot, emptied by dropping its tables so the engine writes over its own
// free list, frees nothing at all and the file never shrinks.
//
// It is also what keeps the largest surface the product writes inside the
// figure the resources block reports: the staging database is a surface of the
// shared scratch arena, so a pool of its own would be disk the run holds and
// never discloses.
//
// Mutation that fails it: give each import its own staging database again --
// take a fresh surface per import and remove the file when the import ends.
func TestOneStagingDatabaseIsReusedAcrossImports(t *testing.T) {
	dir := t.TempDir()
	pool := filepath.Join(arena.Dir(dir), "0", string(arena.ImportStaging))
	staged := func() []os.DirEntry {
		t.Helper()
		entries, err := os.ReadDir(pool)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			t.Fatalf("scratch: %v", err)
		}
		return entries
	}
	sizeOf := func(e os.DirEntry) int64 {
		t.Helper()
		info, err := e.Info()
		if err != nil {
			t.Fatalf("scratch entry: %v", err)
		}
		return info.Size()
	}

	if _, _, _, err := run(t, filepath.Join("testdata", "src", "c"), filepath.Join("testdata", "c"), graphcsv.Options{Language: "c", ScratchDir: dir}); err != nil {
		t.Fatalf("first import: %v", err)
	}
	first := staged()
	if len(first) != 1 {
		t.Fatalf("the first import left %d staging databases, want exactly one", len(first))
	}
	firstSize := sizeOf(first[0])
	if firstSize == 0 {
		t.Fatalf("the first import left an empty staging database: nothing was staged in it")
	}

	if _, _, _, err := run(t, filepath.Join("testdata", "src", "c"), filepath.Join("testdata", "c"), graphcsv.Options{Language: "c", ScratchDir: dir}); err != nil {
		t.Fatalf("second import: %v", err)
	}
	second := staged()
	if len(second) != 1 {
		t.Fatalf("the second import left %d staging databases, want the one the first created", len(second))
	}
	if second[0].Name() != first[0].Name() {
		t.Fatalf("the second import staged into %q, not the %q the first created: the file was not reused",
			second[0].Name(), first[0].Name())
	}
	if got := sizeOf(second[0]); got < firstSize {
		t.Errorf("the staging database shrank from %d to %d bytes: its pages were freed rather than recycled", firstSize, got)
	}
}

// TestAStagingSurfaceThatCannotBeEmptiedLeavesThePool protects the pool
// against the failure that turns it into a trap.
//
// The staging database is opened with journalling off, so a crash mid-write
// leaves an image whose tables cannot be dropped. The surface is taken
// last-in-first-out, so an import that gave such a surface back would be
// handed it again by the next import, and the next, for the life of the data
// directory -- every dependence unit of the workspace failing identically,
// with no recovery but removing the directory by hand.
//
// Mutation: release the surface instead of retiring it (lease.Release in place
// of lease.Unusable on the failed reset) and the second import fails too.
func TestAStagingSurfaceThatCannotBeEmptiedLeavesThePool(t *testing.T) {
	dir := t.TempDir()
	pool := filepath.Join(arena.Dir(dir), "0", string(arena.ImportStaging))
	if err := os.MkdirAll(pool, 0o700); err != nil {
		t.Fatal(err)
	}
	// What a crash mid-write leaves: a file in the pool that is not a readable
	// database. The import that takes it cannot empty it.
	poisoned := filepath.Join(pool, "0")
	if err := os.WriteFile(poisoned, bytes.Repeat([]byte("not a database"), 512), 0o600); err != nil {
		t.Fatal(err)
	}

	src, export := filepath.Join("testdata", "src", "c"), filepath.Join("testdata", "c")
	if _, _, _, err := run(t, src, export, graphcsv.Options{Language: "c", ScratchDir: dir}); err == nil {
		t.Fatal("the import staged into a file that is not a database and reported success")
	}
	if _, err := os.Stat(poisoned); !os.IsNotExist(err) {
		t.Fatalf("the surface that could not be emptied is still in the pool: %v", err)
	}
	if _, _, _, err := run(t, src, export, graphcsv.Options{Language: "c", ScratchDir: dir}); err != nil {
		t.Fatalf("the next import was handed the same unusable surface: %v", err)
	}
}

// TestAFileTheExportDidNotReadIsCounted protects the one record of source a
// frontend dropped by a rule of its own. The failure mode: a unit's file the
// engine was handed has no file node in the export, the import publishes the
// rest, and the unit seals fresh with no fact for that file and nothing that
// says so. Mutation that fails it: drop the anti-join (or stage no file node,
// or ignore Options.Expected) so the count stays zero.
func TestAFileTheExportDidNotReadIsCounted(t *testing.T) {
	dir := t.TempDir()
	files, _ := readSource(t, filepath.Join("testdata", "src", "javasrc"))
	files["test/T.java"] = "class T {}\n"
	for p, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	java := func(fv model.FileVersion) bool { return strings.HasSuffix(fv.Path, ".java") }
	rep, _, state, err := run(t, dir, filepath.Join("testdata", "javasrc"), graphcsv.Options{Language: "java", Expected: java})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if state != model.UnitSealed {
		t.Fatalf("unit state = %s, want sealed: a dropped file is disclosed, not a failure", state)
	}
	if rep.UnanalysedFiles != 1 || rep.UnanalysedFirst != "test/T.java" {
		t.Fatalf("unanalysed = %d first %q, want 1 first %q: the export has a file node for W.java only",
			rep.UnanalysedFiles, rep.UnanalysedFirst, "test/T.java")
	}
}
