package neo4jcsv_test

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/dependence/neo4jcsv"
	"github.com/Sawmonabo/codectx/internal/provider/providertest"
	arena "github.com/Sawmonabo/codectx/internal/scratch"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
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
	// relKey is the fact key list the import published for one relation,
	// under edgeID(from, kind, to). Only a keyed put carries it.
	relKey map[string][]string
	// sig is the signature each published node carried, by node name, so a
	// test can read what actually reached the sink rather than what the
	// export held.
	sig map[string]string
	// id is the identity each published node resolved to, by node name, and
	// meta the metadata it carried.
	id   map[string]model.NodeID
	meta map[string]string
	// edges are the published edges themselves, so a test can ask which
	// entities one names rather than only how many were published.
	edges []model.Relation
}

func newCounts() counts {
	return counts{rel: map[model.RelationKind]int{}, node: map[model.NodeKind]int{},
		alias: map[string]string{}, detail: map[string]int{}, relKey: map[string][]string{},
		sig: map[string]string{}, id: map[string]model.NodeID{}, meta: map[string]string{}}
}

// has reports whether the edge from -> to of kind k was published, naming its
// endpoints by the node names they were published under.
func (c counts) has(kind model.RelationKind, from, to string) bool {
	src, okFrom := c.id[from]
	dst, okTo := c.id[to]
	if !okFrom || !okTo {
		return false
	}
	for _, e := range c.edges {
		if e.Kind == kind && e.From == src && e.To == dst {
			return true
		}
	}
	return false
}

// edgeID names one published edge independently of the run that published it.
func edgeID(from model.NodeID, kind model.RelationKind, to model.NodeID) string {
	return string(from) + "\x00" + string(kind) + "\x00" + string(to)
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
		r.c.id[n.Node.Name] = n.Node.ID
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

// PutKeyedRelations is PutKeyedNodes for relation facts, and records each
// edge's fact keys for TestRelationKeyTracksItsEndpoints.
func (r recorder) PutKeyedRelations(ctx context.Context, f []model.RelationFact, keys [][]string) error {
	for i, x := range f {
		r.c.rel[x.Relation.Kind]++
		r.c.edges = append(r.c.edges, x.Relation)
		for _, ev := range x.Evidence {
			r.c.detail[ev.Detail]++
		}
		if i < len(keys) {
			id := edgeID(x.Relation.From, x.Relation.Kind, x.Relation.To)
			r.c.relKey[id] = append(r.c.relKey[id], keys[i]...)
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

func run(t *testing.T, src, export string, opts neo4jcsv.Options) (neo4jcsv.Report, counts, model.UnitState, error) {
	t.Helper()
	files, paths := readSource(t, src)
	h := providertest.New(t, files)
	c := newCounts()
	var rep neo4jcsv.Report
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
			rep, importErr = neo4jcsv.Import(ctx, export, req.Resolver, recorder{Sink: sink, c: &c}, o)
			if importErr != nil {
				return model.ProviderResult{}, importErr
			}
			return providertest.Succeeded(req, uint64(rep.Nodes+rep.Relations+rep.Aliases), rep.BytesRead), nil
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

// TestImport is the import half of Task 11 Step 1. Every fixture under
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
		check  func(t *testing.T, rep neo4jcsv.Report, c counts)
	}{{
		// Failure mode: a frontend's write lowering is not understood and the
		// unit publishes no reads/writes at all, silently losing the only
		// honest source of write facts (no SCIP indexer sets a write role).
		name: "c reads and writes", src: "src/c", export: "c",
		check: func(t *testing.T, rep neo4jcsv.Report, c counts) {
			wantRelations(t, c, model.RelWrites, model.RelReads, model.RelDataFlowsTo)
			// `s->f = 3` writes the member the base type declares, and the
			// `<global> g` closure binding makes the flow a capture.
			if c.node[model.NodeField] == 0 {
				t.Errorf("no field declaration published; indirect field writes did not resolve")
			}
			if c.detail["reaching_def capture"] == 0 {
				t.Errorf("no capture detail published; a flow through a global was labelled intraprocedural")
			}
			// Failure mode: a method taken as a value never anchors, so every
			// fact that would name the method the value carries -- and with it
			// any call made through that value -- is silently dropped. The
			// export's METHOD_REF for `run` is the only node here whose REF
			// edge names a method rather than a declaration.
			if !c.has(model.RelDataFlowsTo, "<global>", "run") {
				t.Errorf("the method the reference names is not an endpoint of the flow that reaches it; "+
					"the method reference did not anchor (edges = %d)", len(c.edges))
			}
		},
	}, {
		name: "go reads and writes", src: "src/golang", export: "golang",
		check: func(t *testing.T, rep neo4jcsv.Report, c counts) {
			wantRelations(t, c, model.RelWrites, model.RelReads, model.RelDataFlowsTo)
		},
	}, {
		name: "java reads and writes", src: "src/javasrc", export: "javasrc",
		check: func(t *testing.T, rep neo4jcsv.Report, c counts) {
			wantRelations(t, c, model.RelWrites, model.RelReads, model.RelDataFlowsTo)
		},
	}, {
		name: "javascript reads and writes", src: "src/jssrc", export: "jssrc",
		check: func(t *testing.T, rep neo4jcsv.Report, c counts) {
			wantRelations(t, c, model.RelWrites, model.RelReads, model.RelDataFlowsTo, model.RelMayReferTo)
			// Failure mode: a destructuring or computed target is published as
			// a precise write against a guessed declaration.
			if rep.UnresolvedWrites == 0 {
				t.Errorf("no unresolved write shape reported; a guessed target would be published as a precise write")
			}
			// Failure mode: the export marks a callee it invented -- a method
			// it emitted with no definition anywhere in the graph, so that an
			// unresolved site still has a target -- and the import drops the
			// mark, so a consumer of the callees answer cannot tell a guess
			// from a real dependency. `__ecma.Array:` is the invented callee
			// of this export.
			if c.detail["call speculated"] == 0 {
				t.Errorf("evidence details = %v; the call edge to an invented callee is indistinguishable from a real one", c.detail)
			}
			// A traversal answers with nodes and relations and never with the
			// evidence behind them, so the node has to carry it too.
			if !strings.Contains(c.meta["__ecma.Array:"], `"resolution":"speculated"`) {
				t.Errorf("the invented callee's node metadata = %q, want a speculated resolution", c.meta["__ecma.Array:"])
			}
		},
	}, {
		name: "python reads and writes", src: "src/pythonsrc", export: "pythonsrc",
		check: func(t *testing.T, rep neo4jcsv.Report, c counts) {
			wantRelations(t, c, model.RelWrites, model.RelReads, model.RelDataFlowsTo, model.RelMayReferTo)
		},
	}, {
		// Failure mode: an export label this import does not map is emitted as
		// a fact instead of being counted. The Rust export's CONFIG_FILE row
		// (its Cargo.toml) is the real unmapped label.
		name: "rust reads and writes, unknown label counted", src: "src/rust", export: "rust",
		check: func(t *testing.T, rep neo4jcsv.Report, c counts) {
			wantRelations(t, c, model.RelWrites, model.RelReads, model.RelDataFlowsTo)
			if rep.UnknownLabels["CONFIG_FILE"] != 1 {
				t.Errorf("unknown labels = %v; the export's CONFIG_FILE row must be counted, not emitted", rep.UnknownLabels)
			}
			if c.node[model.NodeConfiguration] != 0 {
				t.Errorf("an unmapped label was published as a node")
			}
		},
	}, {
		// Failure mode: control dependence or calls silently publish nothing,
		// so a declared capability is empty. Also the forward-id case: every
		// edges_* file sorts before every nodes_* file, so every relation here
		// was staged before its endpoints' rows were read.
		name: "calls and control dependence", src: "src/gofix", export: "gofix",
		check: func(t *testing.T, rep neo4jcsv.Report, c counts) {
			wantRelations(t, c, model.RelCalls, model.RelControlDependsOn, model.RelDataFlowsTo,
				model.RelReads, model.RelWrites)
			if c.detail["cdg"] == 0 || c.detail["call"] == 0 {
				t.Errorf("evidence details = %v; want cdg and call", c.detail)
			}
		},
	}, {
		// Failure mode: a callee that lives in another unit is dropped as
		// junk, losing the cross-unit call edge the reconciler binds by full
		// name. The export is the same module parsed without its second
		// package, so helper.Scale is a real IS_EXTERNAL stub.
		name: "external declaration kept and aliased", src: "src/goapp", export: "goapp",
		check: func(t *testing.T, rep neo4jcsv.Report, c counts) {
			if rep.ExternalMethods != 1 {
				t.Errorf("external methods = %d, want 1", rep.ExternalMethods)
			}
			wantRelations(t, c, model.RelCalls)
			if _, ok := c.alias[provider.ScopeWorkspace+"\x00"+neo4jcsv.ProviderID+":method:example.com/fix/helper.Scale"]; !ok {
				t.Errorf("the external declaration was not aliased by full name; aliases = %v", keysOf(c.alias))
			}
		},
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep, c, state, err := run(t, filepath.Join("testdata", tc.src), filepath.Join("testdata", tc.export), neo4jcsv.Options{Language: "go"})
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
			tc.check(t, rep, c)
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
		_, _, state, err := run(t, filepath.Join("testdata", "src", "c"), dir, neo4jcsv.Options{Language: "c"})
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
		if n := c.node[model.NodeFunction] + c.node[model.NodeVariable] + c.node[model.NodeField]; n != 0 {
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
	rep, c, _, err := run(t, filepath.Join("testdata", "src", "c"), dir, neo4jcsv.Options{Language: "c"})
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

// TestImportDelta proves the refresh contract: two imports of one export
// produce the same key set, and an import of the edited unit against the
// previous key set reports exactly the keys that changed and publishes only
// those relations. Silent breakage here makes every refresh rewrite the whole
// unit (or, worse, keep a stale row whose key it thinks is unchanged).
func TestImportDelta(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base")
	again := filepath.Join(dir, "again")
	rep1, _, _, err := run(t, filepath.Join("testdata", "src", "gofix"), filepath.Join("testdata", "gofix"),
		neo4jcsv.Options{Language: "go", KeysPath: base})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	rep2, _, _, err := run(t, filepath.Join("testdata", "src", "gofix"), filepath.Join("testdata", "gofix"),
		neo4jcsv.Options{Language: "go", KeysPath: again})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if !sameFile(t, base, again) {
		t.Fatalf("two imports of one export produced different key sets (%d and %d keys)", rep1.Keys.Count(), rep2.Keys.Count())
	}

	prev, err := neo4jcsv.LoadKeySet(base)
	if err != nil {
		t.Fatalf("LoadKeySet: %v", err)
	}
	rep3, c, state, err := run(t, filepath.Join("testdata", "src", "gofix-edit"), filepath.Join("testdata", "gofix-edit"),
		neo4jcsv.Options{Language: "go", PreviousKeys: prev, KeysPath: filepath.Join(dir, "edit")})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if state != model.UnitSealed {
		t.Fatalf("unit state = %q, want sealed", state)
	}
	if rep3.Unchanged == 0 || rep3.Changed == 0 {
		t.Fatalf("delta = %d changed, %d unchanged, %d removed; a one-line edit changes some keys and keeps most",
			rep3.Changed, rep3.Unchanged, rep3.Removed)
	}
	if rep3.Changed+rep3.Unchanged != rep3.Keys.Count() {
		t.Fatalf("changed %d + unchanged %d != %d keys", rep3.Changed, rep3.Unchanged, rep3.Keys.Count())
	}
	// This fixture's edit removes keys, which is the case a delta import may
	// not filter: a removed key cannot be mapped back to the edge it backed,
	// and that edge's previous row is already gone, so every relation is
	// republished. What must hold is that the delta unit is not poorer than a
	// full one — the same relations, the same evidence.
	if rep3.Removed == 0 {
		t.Fatalf("the fixture edit removed no key; it no longer covers the unfiltered case")
	}
	_, full, _, err := run(t, filepath.Join("testdata", "src", "gofix-edit"), filepath.Join("testdata", "gofix-edit"),
		neo4jcsv.Options{Language: "go", KeysPath: filepath.Join(dir, "full")})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if !maps.Equal(c.rel, full.rel) || !maps.Equal(c.detail, full.detail) {
		t.Fatalf("a delta import published %v relations with %v evidence; a full import of the same export published %v and %v",
			c.rel, c.detail, full.rel, full.detail)
	}
	delta, err := rep3.Keys.Diff(prev, nil)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if delta.Changed != rep3.Changed || delta.Removed != rep3.Removed || delta.Unchanged != rep3.Unchanged {
		t.Fatalf("Diff = %+v, report = %d/%d/%d", delta, rep3.Changed, rep3.Unchanged, rep3.Removed)
	}
}

// descriptor is the provider identity the import is driven under. The
// capabilities are the relation kinds this package publishes.
func descriptor() model.ProviderDescriptor {
	return model.ProviderDescriptor{ID: neo4jcsv.ProviderID, Version: "test-1",
		Capabilities:      []string{"control_depends_on", "data_flows_to", "reads", "writes", "calls"},
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
			o := neo4jcsv.Options{Language: "go", UnitScopeKey: req.Unit.ScopeKey, ProjectRoot: t.TempDir(),
				Limits: providertest.Limits, Repository: req.Binding.RepositoryID, Unit: req.Unit,
				Run: req.Run, Content: req.Content, ScratchDir: t.TempDir()}
			rep, err := neo4jcsv.Import(ctx, export, req.Resolver, sink, o)
			if err != nil {
				return model.ProviderResult{}, err
			}
			return providertest.Succeeded(req, uint64(rep.Nodes+rep.Relations+rep.Aliases), rep.BytesRead), nil
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
				o := neo4jcsv.Options{Language: "go", UnitScopeKey: req.Unit.ScopeKey, ProjectRoot: t.TempDir(),
					Limits: providertest.Limits, Repository: req.Binding.RepositoryID, Unit: req.Unit,
					Run: req.Run, Content: req.Content, ScratchDir: t.TempDir()}
				if _, err := neo4jcsv.Import(ctx, export, req.Resolver, recorder{Sink: sink, c: &c}, o); err != nil {
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

func runFiles(t *testing.T, files map[string]string, export string) (neo4jcsv.Report, counts, model.UnitState, error) {
	t.Helper()
	dir := t.TempDir()
	for p, body := range files {
		if err := os.WriteFile(filepath.Join(dir, p), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return run(t, dir, export, neo4jcsv.Options{Language: "c"})
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

func wantRelations(t *testing.T, c counts, kinds ...model.RelationKind) {
	t.Helper()
	for _, k := range kinds {
		if c.rel[k] == 0 {
			t.Errorf("no %s relation published; published %v", k, c.rel)
		}
	}
}

func sameFile(t *testing.T, a, b string) bool {
	t.Helper()
	x, err := os.ReadFile(a)
	if err != nil {
		t.Fatal(err)
	}
	y, err := os.ReadFile(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(x) == string(y)
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, strings.ReplaceAll(k, "\x00", " "))
	}
	return out
}

// TestRelationKeyTracksItsEndpoints guards the endpoints component of the
// fact-key algebra (keys.go: keyAlgebraVersion "2").
//
// Failure mode: a relation's key is built only from the call site's own
// coordinates — its owner, file, operator, target name and byte range — none
// of which move when the *callee's declaration* moves. Editing above a callee
// mints a new identity for it, but the call edge into it would keep its key,
// the delta import would not re-emit the edge, and storage would carry the
// previous row, whose `to` node id no longer exists in the unit. The unit then
// fails to seal with dangling relation endpoints — or, worse, a later algebra
// keeps it. Nothing else in the repository notices this component regressing.
//
// The export is the committed gofix export with one column run rewritten: the
// callee `Scale` declared at helper.go:4-6 is moved to 5-7, exactly as a line
// inserted above it would move it. The source tree is deliberately NOT
// shifted: shifting it would also move every occurrence site inside helper.go
// and change the positional component too, so the test would pass even with
// the endpoints component removed. Moving only the declaration row leaves the
// callee's identity as the single variable, which is the variable under test.
func TestRelationKeyTracksItsEndpoints(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join("testdata", "src", "gofix")
	basePath := filepath.Join(dir, "base")
	_, base, _, err := run(t, src, filepath.Join("testdata", "gofix"),
		neo4jcsv.Options{Language: "go", KeysPath: basePath})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	moved := 0
	export := copyExport(t, filepath.Join("testdata", "gofix"), func(name string, data []byte) []byte {
		if name != "nodes_METHOD_data.csv" {
			return data
		}
		// CODE is a multi-line quoted field, so the row is matched by its
		// LINE_NUMBER,LINE_NUMBER_END,NAME column run, not by line surgery.
		out := bytes.Replace(data, []byte(",false,4,6,Scale,"), []byte(",false,5,7,Scale,"), -1)
		moved = bytes.Count(data, []byte(",false,4,6,Scale,"))
		return out
	})
	if moved != 1 {
		t.Fatalf("the callee declaration row was rewritten %d times, want exactly 1; the fixture's columns moved", moved)
	}
	mutRep, mut, state, err := run(t, src, export, neo4jcsv.Options{Language: "go", KeysPath: filepath.Join(dir, "moved")})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if state != model.UnitSealed {
		t.Fatalf("unit state = %q, want sealed", state)
	}

	// Precondition: moving the declaration moves the callee's published
	// identity, and nothing else. The workspace alias is the cross-run handle.
	scaleBase := base.alias[provider.ScopeWorkspace+"\x00"+neo4jcsv.ProviderID+":method:example.com/fix/helper.Scale"]
	scaleMut := mut.alias[provider.ScopeWorkspace+"\x00"+neo4jcsv.ProviderID+":method:example.com/fix/helper.Scale"]
	runBase := base.alias[provider.ScopeWorkspace+"\x00"+neo4jcsv.ProviderID+":method:example.com/fix/app.Run"]
	runMut := mut.alias[provider.ScopeWorkspace+"\x00"+neo4jcsv.ProviderID+":method:example.com/fix/app.Run"]
	if scaleBase == "" || scaleMut == "" || runBase == "" || runMut == "" {
		t.Fatalf("the fixture did not publish both declarations: Scale %q/%q, Run %q/%q", scaleBase, scaleMut, runBase, runMut)
	}
	if scaleBase == scaleMut {
		t.Fatalf("the moved callee kept identity %s; the declaration range no longer decides a declaration's identity", scaleBase)
	}
	if runBase != runMut {
		t.Fatalf("the caller's identity moved (%s -> %s); the fixture no longer isolates the callee", runBase, runMut)
	}

	// The call edge Run -> Scale. Its key must move with the callee.
	edge := func(c counts, from, to string) []string {
		t.Helper()
		k := c.relKey[edgeID(model.NodeID(from), model.RelCalls, model.NodeID(to))]
		if len(k) == 0 {
			t.Fatalf("the call edge %s -> %s published no fact key; a keyed put carries one per edge", from, to)
		}
		return k
	}
	baseKeys, mutKeys := edge(base, runBase, scaleBase), edge(mut, runMut, scaleMut)
	for _, k := range mutKeys {
		if slices.Contains(baseKeys, k) {
			t.Fatalf("the call edge into the moved callee kept fact key %s; storage would carry a row whose endpoint no longer exists", k)
		}
	}

	// The leg storage consumes: the refresh must report those keys changed.
	prev, err := neo4jcsv.LoadKeySet(basePath)
	if err != nil {
		t.Fatalf("LoadKeySet: %v", err)
	}
	var changed []string
	if _, err := mutRep.Keys.Diff(prev, func(key string, removed bool) error {
		if !removed {
			changed = append(changed, key)
		}
		return nil
	}); err != nil {
		t.Fatalf("Diff: %v", err)
	}
	for _, k := range mutKeys {
		if !slices.Contains(changed, k) {
			t.Fatalf("Diff did not report the call edge's key %s changed; the delta import would not re-emit the edge", k)
		}
	}

	// And the leg where storage keeps the rows: the refresh inherits the base
	// unit, so a key that failed to move becomes a dangling endpoint.
	carryOver(t, src, filepath.Join("testdata", "gofix"), export, dir)
}

// carryOver is the leg storage actually consumes: both imports run over one
// store, the base unit is sealed with its fact keys, and the refresh inherits
// it minus everything the key diff names — which is what a delta build does in
// production (internal/index/delta).
//
// The assertion is that the refreshed unit SEALS, and that it inherited
// something. The base unit holds the call edge Run -> Scale declared at 4-6.
// The refresh publishes Run -> Scale declared at 5-7 and never publishes the
// old callee's node fact, so the base edge is carried if and only if its key
// survived the diff. A carried relation whose `to` node the unit does not hold
// is precisely what SealUnit refuses. Remove the endpoints component from
// FactKey and this leg fails with "relation endpoints with no visible node
// fact" — the import assertions above cannot see that, because they never let
// storage keep a row.
func carryOver(t *testing.T, src, baseExport, mutExport, dir string) {
	t.Helper()
	ctx := context.Background()
	files, paths := readSource(t, src)
	// The refresh must be a different unit: a unit cannot carry over from
	// itself, and these two imports read byte-identical sources. One extra
	// declared input is the smallest honest difference, and it declares no
	// facts, so it changes nothing else.
	const marker = "refresh.marker"
	files[marker] = "refresh\n"
	h := providertest.New(t, files)

	baseKeys := filepath.Join(dir, "carry-base")
	base, baseRep := imports(t, baseExport, neo4jcsv.Options{Language: "go", KeysPath: baseKeys})
	baseUnit := h.Plan(t, base, "pkg:fixture", paths)
	res, err := provider.RunUnit(ctx, base, baseUnit.Request, h.Begin(t, baseUnit, paths), providertest.Limits, h.Pool)
	if err != nil {
		t.Fatalf("base RunUnit: %v", err)
	}
	if err := h.Store.CompleteProviderRun(ctx, res, provider.CodeOf(err)); err != nil {
		t.Fatalf("CompleteProviderRun: %v", err)
	}

	prev, err := neo4jcsv.LoadKeySet(baseKeys)
	if err != nil {
		t.Fatalf("LoadKeySet: %v", err)
	}
	// A refresh is a new generation: one generation selects one unit per
	// provider and scope, so the successor cannot be sealed beside the unit it
	// carries over from. (L1 widens BeginGeneration with a ref parameter; this
	// call site is listed in the lane report.)
	gen, err := h.Store.BeginGeneration(ctx, h.Repo, h.Snapshot.ID, model.H("providertest-semantic"), "refs/heads/providertest")
	if err != nil {
		t.Fatalf("BeginGeneration: %v", err)
	}
	h.Gen = gen

	refresh, refreshRep := imports(t, mutExport, neo4jcsv.Options{Language: "go",
		KeysPath: filepath.Join(dir, "carry-refresh"), PreviousKeys: prev})
	refreshPaths := append(slices.Clone(paths), marker)
	refreshUnit := h.Plan(t, refresh, "pkg:fixture", refreshPaths)
	w, err := h.Store.BeginUnit(ctx, h.Gen, refreshUnit.Build, unitInputs(t, h, refreshPaths))
	if err != nil {
		t.Fatalf("BeginUnit: %v", err)
	}

	var stats sqlite.CarryOverStats
	var delta neo4jcsv.Delta
	var diffErr error
	out := &carryOut{UnitWriter: w, store: h.Store, carry: func(ctx context.Context, w *sqlite.UnitWriter) error {
		fresh := refreshRep.Keys
		d, err := fresh.Diff(prev, nil)
		if err != nil {
			return err
		}
		delta = d
		replaced := sqlite.Replaced{
			// The unlocated bucket is dropped exactly when the emit was
			// unfiltered, which is when a key was removed.
			IndexLevel: d.Removed > 0,
			Keys: func(yield func(string) bool) {
				_, diffErr = fresh.Diff(prev, func(key string, removed bool) error {
					yield(key)
					return nil
				})
			},
		}
		if stats, err = w.CarryOver(ctx, baseUnit.Build.Spec.ID, replaced); err != nil {
			return err
		}
		return diffErr
	}}
	res, err = provider.RunUnit(ctx, refresh, refreshUnit.Request, out, providertest.Limits, h.Pool)
	if err != nil {
		t.Fatalf("refresh RunUnit: %v; the refresh inherited a relation the fresh import no longer publishes", err)
	}
	if err := h.Store.CompleteProviderRun(ctx, res, provider.CodeOf(err)); err != nil {
		t.Fatalf("CompleteProviderRun: %v", err)
	}
	if state, _ := h.UnitState(t, refreshUnit.Build.Spec.ID); state != model.UnitSealed {
		t.Fatalf("refreshed unit state = %q, want sealed", state)
	}
	if baseRep.Keys.Count() == 0 {
		t.Fatal("the base unit published no fact keys; carry-over had nothing to diff")
	}
	// Moving the callee retires the keys of its declaration and of every edge
	// into it, so this refresh emits unfiltered and republishes every relation
	// that still exists. The carried set is therefore the complement: it must
	// be empty, and the one row that would land in it is the stale edge whose
	// endpoints moved. Strip the endpoints component from FactKey and this
	// fires, one line before the seal refuses the dangling endpoint.
	if delta.Removed == 0 {
		t.Fatalf("the refresh removed no key (%+v); the fixture no longer retires the moved callee's keys", delta)
	}
	if stats.Relations != 0 {
		t.Fatalf("the refresh inherited %d relations from a unit whose every surviving relation it republished; "+
			"a relation whose endpoints moved kept its fact key", stats.Relations)
	}
}

// imports is one provider whose whole index is a neo4jcsv import of export,
// plus the report it produced. The report is read after RunUnit returns, or
// inside the seal the caller wraps around it.
func imports(t *testing.T, export string, opts neo4jcsv.Options) (provider.Provider, *neo4jcsv.Report) {
	t.Helper()
	projectRoot := t.TempDir()
	rep := new(neo4jcsv.Report)
	p := providertest.Func{
		Desc: descriptor(),
		IndexFn: func(ctx context.Context, req provider.UnitRequest, sink provider.Sink) (model.ProviderResult, error) {
			o := opts
			o.UnitScopeKey, o.ProjectRoot = req.Unit.ScopeKey, projectRoot
			o.Limits = providertest.Limits
			o.Repository, o.Unit, o.Run, o.Content = req.Binding.RepositoryID, req.Unit, req.Run, req.Content
			r, err := neo4jcsv.Import(ctx, export, req.Resolver, sink, o)
			if err != nil {
				return model.ProviderResult{}, err
			}
			*rep = r
			return providertest.Succeeded(req, uint64(r.Nodes+r.Relations+r.Aliases), r.BytesRead), nil
		},
	}
	return p, rep
}

// unitInputs streams the harness rows for paths in the ascending file order
// BeginUnit requires.
func unitInputs(t *testing.T, h *providertest.Harness, paths []string) func(yield func(model.UnitInput) error) error {
	t.Helper()
	ins := make([]model.UnitInput, 0, len(paths))
	for _, p := range paths {
		fv := h.File(t, p)
		ins = append(ins, model.UnitInput{FileID: fv.ID, ContentHash: fv.ContentHash, Executable: fv.Executable})
	}
	slices.SortFunc(ins, func(a, b model.UnitInput) int { return strings.Compare(string(a.FileID), string(b.FileID)) })
	return func(yield func(model.UnitInput) error) error {
		for _, in := range ins {
			if err := yield(in); err != nil {
				return err
			}
		}
		return nil
	}
}

// carryOut is provider.StoreUnit with a carry-over wedged in front of the
// seal, the shape internal/index/delta builds a unit in. It embeds the
// concrete writer so the sink still finds the keyed puts behind its
// provider.DeltaSink assertion.
type carryOut struct {
	*sqlite.UnitWriter
	store *sqlite.Store
	carry func(ctx context.Context, w *sqlite.UnitWriter) error
}

func (o *carryOut) Seal(ctx context.Context) error {
	if err := o.carry(ctx, o.UnitWriter); err != nil {
		return err
	}
	return o.store.SealUnit(ctx, o.UnitWriter)
}

// TestStagedRowsOverAUserSetBoundImportsAndReports pins the one invariant of
// providers.dependence.max_staged_rows: crossing it publishes the whole import
// and says so. The bound used to fail the unit outright, which made a row
// count — a property of the repository's source — refuse the repository. A
// regression that restores the refusal, or that stops setting the flag, is the
// silence the scale posture forbids, and neither shows in any other assertion.
func TestStagedRowsOverAUserSetBoundImportsAndReports(t *testing.T) {
	src, export := filepath.Join("testdata", "src", "gofix"), filepath.Join("testdata", "gofix")
	whole, _, _, err := run(t, src, export, neo4jcsv.Options{Language: "go"})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if whole.StagedRows == 0 {
		t.Fatalf("the import staged no rows, so the bound below proves nothing")
	}
	if whole.OverStagedRows {
		t.Fatalf("an import with no max_staged_rows reported crossing one")
	}
	bounded, _, state, err := run(t, src, export, neo4jcsv.Options{Language: "go", MaxStagedRows: 1})
	if err != nil {
		t.Fatalf("an import over max_staged_rows must not fail the unit: %v", err)
	}
	if state != model.UnitSealed {
		t.Fatalf("unit state = %s, want sealed: crossing a reporting threshold may not fail the unit", state)
	}
	if !bounded.OverStagedRows {
		t.Fatalf("an import of %d rows over max_staged_rows = 1 did not report crossing it", bounded.StagedRows)
	}
	if bounded.StagedRows != whole.StagedRows || bounded.Nodes != whole.Nodes || bounded.Relations != whole.Relations {
		t.Fatalf("the bounded import published less than the unbounded one: rows %d/%d nodes %d/%d relations %d/%d",
			bounded.StagedRows, whole.StagedRows, bounded.Nodes, whole.Nodes, bounded.Relations, whole.Relations)
	}
}

// TestDerivedRowsOverAUserSetBoundImportsAndReports pins the same invariant for
// providers.dependence.max_derived_rows. The projected occurrence count used to
// be a hard 4,000,000 that failed the unit outright, and the projection query
// carried a matching `LIMIT bound+1` that silently truncated it — a refusal and
// a silent cut on a count that belongs to the analysed source. The equality
// assertions below are what catch a restored truncation: a surviving LIMIT under
// a bound of 1 leaves two occurrences and every published relation behind it.
func TestDerivedRowsOverAUserSetBoundImportsAndReports(t *testing.T) {
	src, export := filepath.Join("testdata", "src", "gofix"), filepath.Join("testdata", "gofix")
	whole, _, _, err := run(t, src, export, neo4jcsv.Options{Language: "go"})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if whole.DerivedRows == 0 {
		t.Fatalf("the import derived no occurrences, so the bound below proves nothing")
	}
	if whole.OverDerivedRows {
		t.Fatalf("an import with no max_derived_rows reported crossing one")
	}
	bounded, _, state, err := run(t, src, export, neo4jcsv.Options{Language: "go", MaxDerivedRows: 1})
	if err != nil {
		t.Fatalf("an import over max_derived_rows must not fail the unit: %v", err)
	}
	if state != model.UnitSealed {
		t.Fatalf("unit state = %s, want sealed: crossing a reporting threshold may not fail the unit", state)
	}
	if !bounded.OverDerivedRows {
		t.Fatalf("an import of %d occurrences over max_derived_rows = 1 did not report crossing it", bounded.DerivedRows)
	}
	if bounded.DerivedRows != whole.DerivedRows || bounded.Relations != whole.Relations {
		t.Fatalf("the bounded import published less than the unbounded one: derived %d/%d relations %d/%d",
			bounded.DerivedRows, whole.DerivedRows, bounded.Relations, whole.Relations)
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

	if _, _, _, err := run(t, filepath.Join("testdata", "src", "c"), filepath.Join("testdata", "c"), neo4jcsv.Options{Language: "c", ScratchDir: dir}); err != nil {
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

	if _, _, _, err := run(t, filepath.Join("testdata", "src", "c"), filepath.Join("testdata", "c"), neo4jcsv.Options{Language: "c", ScratchDir: dir}); err != nil {
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
	if _, _, _, err := run(t, src, export, neo4jcsv.Options{Language: "c", ScratchDir: dir}); err == nil {
		t.Fatal("the import staged into a file that is not a database and reported success")
	}
	if _, err := os.Stat(poisoned); !os.IsNotExist(err) {
		t.Fatalf("the surface that could not be emptied is still in the pool: %v", err)
	}
	if _, _, _, err := run(t, src, export, neo4jcsv.Options{Language: "c", ScratchDir: dir}); err != nil {
		t.Fatalf("the next import was handed the same unusable surface: %v", err)
	}
}
