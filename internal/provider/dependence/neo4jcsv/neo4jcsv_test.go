package neo4jcsv_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/dependence/neo4jcsv"
	"github.com/Sawmonabo/codectx/internal/provider/providertest"
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
}

type recorder struct {
	provider.Sink
	c *counts
}

func (r recorder) PutNodes(ctx context.Context, f []model.NodeFact) error {
	for _, n := range f {
		r.c.node[n.Node.Kind]++
	}
	return r.Sink.PutNodes(ctx, f)
}

func (r recorder) PutRelations(ctx context.Context, f []model.RelationFact) error {
	for _, x := range f {
		r.c.rel[x.Relation.Kind]++
		for _, ev := range x.Evidence {
			r.c.detail[ev.Detail]++
		}
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
	c := counts{rel: map[model.RelationKind]int{}, node: map[model.NodeKind]int{},
		alias: map[string]string{}, detail: map[string]int{}}
	var rep neo4jcsv.Report
	var importErr error
	p := providertest.Func{
		Desc: descriptor(),
		IndexFn: func(ctx context.Context, req provider.UnitRequest, sink provider.Sink) (model.ProviderResult, error) {
			o := opts
			o.UnitScopeKey, o.ProjectRoot = req.Unit.ScopeKey, t.TempDir()
			o.Limits = providertest.Limits
			o.Repository, o.Unit, o.Run, o.Content = req.Binding.RepositoryID, req.Unit, req.Run, req.Content
			o.ScratchDir = t.TempDir()
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
	// Only the changed relations reach the sink; the unchanged ones are the
	// rows storage keeps.
	var emitted int
	for _, n := range c.rel {
		emitted += n
	}
	if emitted >= rep3.Changed+rep3.Unchanged {
		t.Fatalf("a delta import published %d relation occurrences out of %d keys; it must publish only the changed ones",
			emitted, rep3.Changed+rep3.Unchanged)
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

// TestDedupeSinkAdmitsASubdividedUnit protects the subdivision path. A
// subdivided unit runs the engine per part and imports every part's export
// into the one sink opened for the unit, and two parts legitimately describe
// the same identity (above all the external stub of a shared callee). Storage
// keys a node fact by (unit, node) with no conflict clause, so the second part
// republishing that identity fails the whole unit and every fact in it is
// lost. DedupeSink is the only thing between that and a lost unit.
func TestDedupeSinkAdmitsASubdividedUnit(t *testing.T) {
	files, paths := readSource(t, filepath.Join("testdata", "src", "gofix"))
	export := filepath.Join("testdata", "gofix")
	h := providertest.New(t, files)
	c := counts{rel: map[model.RelationKind]int{}, node: map[model.NodeKind]int{},
		alias: map[string]string{}, detail: map[string]int{}}
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
			dedupe, err := neo4jcsv.NewDedupeSink(recorder{Sink: sink, c: &c}, t.TempDir())
			if err != nil {
				return model.ProviderResult{}, err
			}
			defer dedupe.Close()
			for i := range part {
				nodes, aliases := nodesSoFar(), c.aliasN
				o := neo4jcsv.Options{Language: "go", UnitScopeKey: req.Unit.ScopeKey, ProjectRoot: t.TempDir(),
					Limits: providertest.Limits, Repository: req.Binding.RepositoryID, Unit: req.Unit,
					Run: req.Run, Content: req.Content, ScratchDir: t.TempDir()}
				if _, err := neo4jcsv.Import(ctx, export, req.Resolver, dedupe, o); err != nil {
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
	if part[1].nodes != 0 || part[1].aliases != 0 {
		t.Fatalf("the second part re-published %d node facts and %d aliases; storage would reject them and fail the unit",
			part[1].nodes, part[1].aliases)
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
