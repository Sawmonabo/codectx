//go:build linux

package neo4jcsv_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/dependence/neo4jcsv"
	"github.com/Sawmonabo/codectx/internal/provider/providertest"
)

// procIO reads one counter of /proc/self/io.
func procIO(t *testing.T, field string) int64 {
	t.Helper()
	raw, err := os.ReadFile("/proc/self/io")
	if err != nil {
		t.Skipf("/proc/self/io: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if rest, ok := strings.CutPrefix(line, field+": "); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
			if err != nil {
				t.Fatalf("%s: %v", field, err)
			}
			return n
		}
	}
	t.Fatalf("/proc/self/io has no %s line", field)
	return 0
}

func envInt(t *testing.T, name string, fallback int) int {
	t.Helper()
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return n
}

// syntheticExport writes an engine export of the shape the importer stages --
// methods with a parameter, a local, assignments whose right-hand sides call
// other methods, identifiers bound to declarations by REF, a def-use chain
// through the identifiers and a control-dependence chain through the
// assignments -- for files × methods × calls, together with the source tree
// its coordinates name. Ids ascend within each label the way the engine's do.
func syntheticExport(t *testing.T, dir string, files, methods, calls int) (map[string]string, []string, int64) {
	t.Helper()
	type csvFile struct {
		header string
		rows   strings.Builder
	}
	nodeFiles := map[string]*csvFile{
		"METHOD":              {header: ":ID,:LABEL,CODE:string,COLUMN_NUMBER:int,FILENAME:string,FULL_NAME:string,IS_EXTERNAL:boolean,LINE_NUMBER:int,LINE_NUMBER_END:int,NAME:string,SIGNATURE:string"},
		"BLOCK":               {header: ":ID,:LABEL,CODE:string,COLUMN_NUMBER:int,LINE_NUMBER:int,ARGUMENT_INDEX:int"},
		"METHOD_PARAMETER_IN": {header: ":ID,:LABEL,CODE:string,COLUMN_NUMBER:int,LINE_NUMBER:int,NAME:string,TYPE_FULL_NAME:string"},
		"LOCAL":               {header: ":ID,:LABEL,CODE:string,COLUMN_NUMBER:int,LINE_NUMBER:int,NAME:string,TYPE_FULL_NAME:string"},
		"CALL":                {header: ":ID,:LABEL,ARGUMENT_INDEX:int,CODE:string,COLUMN_NUMBER:int,LINE_NUMBER:int,METHOD_FULL_NAME:string,NAME:string,SIGNATURE:string,TYPE_FULL_NAME:string"},
		"IDENTIFIER":          {header: ":ID,:LABEL,ARGUMENT_INDEX:int,CODE:string,COLUMN_NUMBER:int,LINE_NUMBER:int,NAME:string,TYPE_FULL_NAME:string"},
		"META_DATA":           {header: ":ID,:LABEL,LANGUAGE:string,VERSION:string"},
	}
	edgeFiles := map[string]*csvFile{}
	for _, label := range []string{"AST", "CONTAINS", "ARGUMENT", "CALL", "REF", "CDG"} {
		edgeFiles[label] = &csvFile{header: ":START_ID,:END_ID,:TYPE"}
	}
	edgeFiles["REACHING_DEF"] = &csvFile{header: ":START_ID,:END_ID,:TYPE,VARIABLE:string"}
	next := map[string]int64{"METHOD": 1e11, "BLOCK": 2e10, "METHOD_PARAMETER_IN": 2e11, "LOCAL": 3e11,
		"CALL": 3e10, "IDENTIFIER": 6e10, "META_DATA": 1e9}
	node := func(label string, cols ...string) int64 {
		id := next[label]
		next[label]++
		f := nodeFiles[label]
		f.rows.WriteString(strconv.FormatInt(id, 10) + "," + label)
		for _, c := range cols {
			f.rows.WriteString("," + csvQuote(c))
		}
		f.rows.WriteString("\n")
		return id
	}
	edge := func(label string, from, to int64, extra ...string) {
		f := edgeFiles[label]
		f.rows.WriteString(strconv.FormatInt(from, 10) + "," + strconv.FormatInt(to, 10) + "," + label)
		for _, c := range extra {
			f.rows.WriteString("," + csvQuote(c))
		}
		f.rows.WriteString("\n")
	}
	node("META_DATA", "PYTHONSRC", "test")
	source := map[string]string{}
	var paths []string
	methodID := func(file, m int) int64 { return int64(1e11) + int64(file*methods+m) }
	for fi := 0; fi < files; fi++ {
		path := fmt.Sprintf("pkg%d/m%d.py", fi%17, fi)
		var src strings.Builder
		line := 1
		for m := 0; m < methods; m++ {
			name := "f" + strconv.Itoa(m)
			full := path + ":<module>." + name
			first := line
			src.WriteString("def " + name + "(a):\n")
			mid := node("METHOD", "def "+name+"(a):", "1", path, full, "false", strconv.Itoa(first), strconv.Itoa(first+calls+1), name, "")
			if mid != methodID(fi, m) {
				t.Fatalf("method id %d, want %d", mid, methodID(fi, m))
			}
			param := node("METHOD_PARAMETER_IN", "a", "8", strconv.Itoa(first), "a", "ANY")
			edge("AST", mid, param)
			block := node("BLOCK", "", "1", strconv.Itoa(first), "-1")
			edge("AST", mid, block)
			edge("CONTAINS", mid, block)
			local := node("LOCAL", "x", "5", strconv.Itoa(first+1), "x", "ANY")
			edge("AST", block, local)
			var prevAssign, prevX int64
			for c := 0; c < calls; c++ {
				line++
				target := (fi*methods + m + c + 1) % (files * methods)
				tf, tm := target/methods, target%methods
				tname := "f" + strconv.Itoa(tm)
				tfull := fmt.Sprintf("pkg%d/m%d.py", tf%17, tf) + ":<module>." + tname
				src.WriteString("    x = " + tname + "(a)\n")
				assign := node("CALL", "-1", "x = "+tname+"(a)", "5", strconv.Itoa(line), "<operator>.assignment", "<operator>.assignment", "", "ANY")
				edge("AST", block, assign)
				edge("CONTAINS", mid, assign)
				x := node("IDENTIFIER", "1", "x", "5", strconv.Itoa(line), "x", "ANY")
				edge("AST", assign, x)
				edge("ARGUMENT", assign, x)
				edge("CONTAINS", mid, x)
				edge("REF", x, local)
				call := node("CALL", "2", tname+"(a)", "9", strconv.Itoa(line), tfull, tname, "", "ANY")
				edge("AST", assign, call)
				edge("ARGUMENT", assign, call)
				edge("CONTAINS", mid, call)
				edge("CALL", call, methodID(tf, tm))
				a := node("IDENTIFIER", "1", "a", strconv.Itoa(10+len(tname)), strconv.Itoa(line), "a", "ANY")
				edge("AST", call, a)
				edge("ARGUMENT", call, a)
				edge("CONTAINS", mid, a)
				edge("REF", a, param)
				edge("REACHING_DEF", param, a, "a")
				if prevX != 0 {
					edge("REACHING_DEF", prevX, x, "x")
					edge("CDG", prevAssign, assign)
				}
				prevAssign, prevX = assign, x
			}
			line++
			src.WriteString("    return x\n")
			line++
		}
		source[path] = src.String()
		paths = append(paths, path)
	}
	var bytes int64
	write := func(kind, label string, f *csvFile) {
		for _, part := range [...]struct{ suffix, body string }{{"_header.csv", f.header + "\n"}, {"_data.csv", f.rows.String()}} {
			name := filepath.Join(dir, kind+"_"+label+part.suffix)
			if err := os.WriteFile(name, []byte(part.body), 0o600); err != nil {
				t.Fatal(err)
			}
			bytes += int64(len(part.body))
		}
	}
	for label, f := range nodeFiles {
		write("nodes", label, f)
	}
	for label, f := range edgeFiles {
		write("edges", label, f)
	}
	return source, paths, bytes
}

func csvQuote(s string) string {
	if strings.ContainsAny(s, ",\"\n") {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}

// digestSink folds every fact it is handed into a digest that depends on
// the export and the source alone -- node identities are named by their
// canonical keys, files by their content hashes, and nothing run-scoped
// (repository, run or unit ids) enters it -- and forwards nothing: the
// import's own writes are what is measured, not storage's, and the digest is
// what two imports of one export, by two versions of the importer, must
// agree on. Facts are folded in sorted order, so the emission order does not
// matter either.
type digestSink struct {
	keys    map[model.NodeID]string
	facts   []string
	n, r, a int
}

// evidenceKey names a fact's evidence set without regard to its order.
func evidenceKey(ev []model.Evidence) string {
	parts := make([]string, 0, len(ev))
	for _, e := range ev {
		parts = append(parts, fmt.Sprintf("[%s %q %s %v %q]", e.Precision, e.NativeKey, e.ContentHash, e.Range, e.Detail))
	}
	sort.Strings(parts)
	return strings.Join(parts, "")
}

func (d *digestSink) name(id model.NodeID) string {
	if k, ok := d.keys[id]; ok {
		return k
	}
	return "unknown:" + string(id)
}

func (d *digestSink) PutNodes(ctx context.Context, f []model.NodeFact) error {
	return d.PutKeyedNodes(ctx, f, nil)
}

func (d *digestSink) PutKeyedNodes(ctx context.Context, f []model.NodeFact, keys [][]string) error {
	if d.keys == nil {
		d.keys = map[model.NodeID]string{}
	}
	for i, fact := range f {
		d.keys[fact.Node.ID] = fact.CanonicalKey
		var k []string
		if keys != nil {
			k = keys[i]
		}
		d.facts = append(d.facts, fmt.Sprintf("node %s %s %q %q %q %s %s %v %s %s %v", fact.CanonicalKey, fact.Node.Kind,
			fact.Node.Name, fact.Node.QualifiedName, fact.Node.Signature, fact.Node.Language, fact.Node.ContentHash,
			fact.Node.Range, string(fact.Node.Metadata), evidenceKey(fact.Evidence), k))
	}
	d.n += len(f)
	return nil
}

func (d *digestSink) PutRelations(ctx context.Context, f []model.RelationFact) error {
	return d.PutKeyedRelations(ctx, f, nil)
}

func (d *digestSink) PutKeyedRelations(ctx context.Context, f []model.RelationFact, keys [][]string) error {
	for i, fact := range f {
		var k []string
		if keys != nil {
			k = keys[i]
		}
		d.facts = append(d.facts, fmt.Sprintf("relation %s %s %s %s %v", d.name(fact.Relation.From), fact.Relation.Kind,
			d.name(fact.Relation.To), evidenceKey(fact.Evidence), k))
	}
	d.r += len(f)
	return nil
}

func (d *digestSink) PutAliases(ctx context.Context, a []model.NativeAlias) error {
	for _, alias := range a {
		d.facts = append(d.facts, fmt.Sprintf("alias %s %s %s", alias.ScopeKey, alias.NativeKey, d.name(alias.NodeID)))
	}
	d.a += len(a)
	return nil
}

func (d *digestSink) PutSearchUnits(ctx context.Context, u []model.SearchUnit) error {
	for _, unit := range u {
		d.facts = append(d.facts, fmt.Sprintf("search %+v", unit))
	}
	return nil
}

// digest is the run-independent digest of everything the sink was handed.
func (d *digestSink) digest() string {
	sort.Strings(d.facts)
	h := sha256.New()
	for _, f := range d.facts {
		h.Write([]byte(f))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// TestImportWritesAreProportionalToTheExport imports an export of files ×
// methods × calls (CODECTX_DEPENDENCE_FILES, _METHODS, _CALLS; or a real
// export and its source tree named by CODECTX_DEPENDENCE_EXPORT and
// CODECTX_DEPENDENCE_SOURCE) through a sink that keeps nothing, and compares
// the bytes the import wrote with the bytes of the export. It logs the
// run-independent digest of what was emitted and, when
// CODECTX_DEPENDENCE_DIGEST names one, requires that digest: that is how two
// versions of the importer are shown to publish the same facts.
//
// Requirement: an import's disk traffic is a small constant times the
// export it stages. The staging holds the export's rows once, each ordered
// copy or index a later phase reads once, and the entities, locations and
// occurrences derived from them once, appended and then sorted once;
// nothing rewrites a page it has already written. An import that inserts
// random-keyed rows into a b-tree larger than its cache, or rewrites its
// widest table per derivation pass, writes tens of times the export, which on
// a monorepo is tens of gigabytes at the disk's full rate for the length of
// the import.
//
// Mutation that fails it: build the relation staging's unique index before
// the occurrences are appended, so every occurrence is a random insert (or
// restore the whole-table owner updates).
func TestImportWritesAreProportionalToTheExport(t *testing.T) {
	files := envInt(t, "CODECTX_DEPENDENCE_FILES", 60)
	methods := envInt(t, "CODECTX_DEPENDENCE_METHODS", 40)
	calls := envInt(t, "CODECTX_DEPENDENCE_CALLS", 5)
	export := os.Getenv("CODECTX_DEPENDENCE_EXPORT")
	var source map[string]string
	var paths []string
	var exportBytes int64
	if export == "" {
		export = t.TempDir()
		source, paths, exportBytes = syntheticExport(t, export, files, methods, calls)
	} else {
		source, paths = readSource(t, os.Getenv("CODECTX_DEPENDENCE_SOURCE"))
		entries, err := os.ReadDir(export)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if info, err := e.Info(); err == nil {
				exportBytes += info.Size()
			}
		}
	}
	h := providertest.New(t, source)
	sink := &digestSink{}
	scratch := t.TempDir()
	type sample struct {
		phase               string
		engine, disk, stage int64
		at                  time.Time
	}
	var samples []sample
	mark := func(phase string) {
		var stage int64
		if m, _ := filepath.Glob(filepath.Join(scratch, "*", "stage.db")); len(m) == 1 {
			if info, err := os.Stat(m[0]); err == nil {
				stage = info.Size()
			}
		}
		samples = append(samples, sample{phase, procIO(t, "wchar"), procIO(t, "write_bytes"), stage, time.Now()})
	}
	var rep neo4jcsv.Report
	var importErr error
	p := providertest.Func{
		Desc: descriptor(),
		IndexFn: func(ctx context.Context, req provider.UnitRequest, _ provider.Sink) (model.ProviderResult, error) {
			o := neo4jcsv.Options{UnitScopeKey: req.Unit.ScopeKey, ProjectRoot: t.TempDir(), Limits: providertest.Limits,
				Repository: req.Binding.RepositoryID, Unit: req.Unit, Run: req.Run, Content: req.Content,
				ScratchDir: scratch, OnPhase: mark, StagingCacheKiB: envInt(t, "CODECTX_DEPENDENCE_CACHE_KIB", 2048)}
			mark("start")
			rep, importErr = neo4jcsv.Import(ctx, export, req.Resolver, sink, o)
			if importErr != nil {
				return model.ProviderResult{}, importErr
			}
			return providertest.Succeeded(req, uint64(rep.Nodes+rep.Relations+rep.Aliases), rep.BytesRead), nil
		},
	}
	if _, _, err := h.Run(t, p, "pkg:fixture", paths); importErr != nil {
		t.Fatalf("Import: %v", importErr)
	} else if err != nil {
		t.Logf("unit run: %v", err)
	}
	if len(samples) < 2 {
		t.Fatal("the import reported no phases")
	}
	t.Logf("export %.1f MiB; nodes=%d relations=%d aliases=%d staged=%d derived=%d",
		float64(exportBytes)/(1<<20), sink.n, sink.r, sink.a, rep.StagedRows, rep.DerivedRows)
	for i := 1; i < len(samples); i++ {
		s, prev := samples[i], samples[i-1]
		t.Logf("%-32s %7.1fs engine %8.1f MiB  disk %8.1f MiB  staging +%7.1f MiB", s.phase, s.at.Sub(prev.at).Seconds(),
			float64(s.engine-prev.engine)/(1<<20), float64(s.disk-prev.disk)/(1<<20), float64(s.stage-prev.stage)/(1<<20))
	}
	first, last := samples[0], samples[len(samples)-1]
	engine, disk := last.engine-first.engine, last.disk-first.disk
	t.Logf("total engine %.1f MiB (%.1fx) disk %.1f MiB (%.1fx) in %.1fs", float64(engine)/(1<<20), float64(engine)/float64(exportBytes),
		float64(disk)/(1<<20), float64(disk)/float64(exportBytes), last.at.Sub(first.at).Seconds())
	digest := sink.digest()
	t.Logf("digest %s", digest)
	if want := os.Getenv("CODECTX_DEPENDENCE_DIGEST"); want != "" && want != digest {
		t.Errorf("the import published digest %s, want %s: the facts differ from the reference import", digest, want)
	}
	const bound = 8
	if engine > bound*exportBytes {
		t.Errorf("the import wrote %.1f MiB for a %.1f MiB export (%.1fx): the staging rewrites what it has already written",
			float64(engine)/(1<<20), float64(exportBytes)/(1<<20), float64(engine)/float64(exportBytes))
	}
}
