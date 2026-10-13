package delta_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/Sawmonabo/codectx/internal/admission"
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

// relinkBase is a Go module whose files call into a.go and into one of its
// own packages: b.go calls Run and e.go calls Other, both in a.go; c.go calls
// Twice in b.go; d.go calls Help through the module's own import path.
var relinkBase = map[string]string{
	"go.mod":    "module example.com/app\n\ngo 1.22\n\nrequire example.com/dep v1.0.0\n",
	"a.go":      "package app\n\nfunc Run(x int) int { return x * 2 }\n\nfunc Other() int { return 1 }\n",
	"b.go":      "package app\n\nfunc Twice(x int) int { return Run(x) + Run(x) }\n",
	"c.go":      "package app\n\nfunc Thrice(x int) int { return Twice(x) + x }\n",
	"e.go":      "package app\n\nfunc UseOther() int { return Other() }\n",
	"d.go":      "package app\n\nimport \"example.com/app/util\"\n\nfunc Use() int { return util.Help() }\n",
	"util/u.go": "package util\n\nfunc Help() int { return 1 }\n",
}

// TestLinkBodyEditRelinksNothing refreshes a Go linking unit after an edit
// inside a function body, then after two go.mod edits, and compares each
// refresh with a full build of the same tree (session.check).
//
// A body edit that lengthens Run republishes a.go alone: Run keeps its node
// (a declaration is keyed by its descriptor path, not its range), so b.go's
// carried calls still name it, and nothing of b.go, c.go, d.go, e.go or
// util/u.go is republished or has its table read. A dependency version bump
// changes no field the linker reads: nothing is republished and no table is
// read. A module path change unbinds d.go's call of util.Help, which only
// that site read: d.go is republished, and so is util/u.go, whose Help is
// no longer a target, and every other file is carried.
//
// Mutations that fail it: republish every file with a site bound into the
// edited file (b.go and e.go are short of their carried rows); key a
// declaration on its range (Run's node moves, and b.go's carried call names
// the old one); compare go.mod by its bytes (the version bump reruns and
// reads tables); skip the reverse index for the module path (d.go's call
// stays bound and differs from the full build); republish only the edited
// file there (util/u.go keeps Help's occurrence).
func TestLinkBodyEditRelinksNothing(t *testing.T) {
	s := newSession(t)
	files := relinkBase
	base := s.only(s.advance(files))
	s.check("base", []linkBuild{base})

	files = edit{name: "body", replace: []sub{{"a.go", "return x * 2 }", "return x * 20 }"}}}.apply(t, files)
	body := s.only(s.advance(files))
	s.check("body edit", []linkBuild{body})
	s.wantRepublished("body edit", body, []string{"a.go"}, "b.go", "e.go", "c.go", "d.go", "util/u.go")
	for _, p := range []string{"b.go", "c.go", "d.go", "e.go", "util/u.go"} {
		if n := body.reads[p]; n != 0 {
			t.Errorf("the body edit read %s's table %d times: no fact of it moved", p, n)
		}
	}
	if body.reads["a.go"] == 0 {
		t.Error("the body edit never read a.go's table: Run's occurrence was not republished from the fresh table")
	}
	run := s.nodeNamed(base.unit, "Run")
	if got := s.nodeNamed(body.unit, "Run"); got != run {
		t.Errorf("Run is node %d after the body edit and %d before it: a body edit moved a declaration's identity", got, run)
	}
	if n := s.callsInto(body.unit, run); n == 0 {
		t.Error("no carried call names Run's node after the body edit")
	}

	files = edit{name: "bump", replace: []sub{{"go.mod", "v1.0.0", "v1.1.0"}}}.apply(t, files)
	bump := s.only(s.advance(files))
	s.check("version bump", []linkBuild{bump})
	s.wantRepublished("version bump", bump, nil, "a.go", "b.go", "c.go", "d.go", "e.go", "util/u.go")
	for p, n := range bump.reads {
		if n != 0 {
			t.Errorf("the version bump read %s's table %d times: it changes no field the linker reads", p, n)
		}
	}

	files = edit{name: "module", replace: []sub{{"go.mod", "module example.com/app\n", "module example.com/renamed\n"}}}.apply(t, files)
	moved := s.only(s.advance(files))
	s.check("module path change", []linkBuild{moved})
	s.wantRepublished("module path change", moved, []string{"d.go", "util/u.go"}, "a.go", "b.go", "c.go", "e.go")
}

// TestLayoutBreakReadsOnlyMovedTables adds a file to a Go unit and to a
// Python unit, then removes one from the Go unit, and compares each refresh
// with a full build of the same tree (session.check). A layout break
// rebuilds the hierarchy (and the Python module index) from the projections
// the predecessor stored for the members whose digest held, and from the
// tables of the members that are new: f.go, which nothing calls, is the one
// Go table read, and extra.py the one Python table. Removing e.go unbinds
// nothing but its call of Other, so only a.go, whose Other is no longer a
// target, is read again (to re-run and to republish it).
//
// Mutations that fail it: build the hierarchy again from every table on a
// layout break (buildHierarchy ignoring the held projections: every member's
// table is read); read the Python module index from the tables (every
// Python table is read).
func TestLayoutBreakReadsOnlyMovedTables(t *testing.T) {
	onlyRead := func(step string, b linkBuild, want ...string) {
		t.Helper()
		for p, n := range b.reads {
			if n != 0 && !slices.Contains(want, p) {
				t.Errorf("%s read %s's table %d times: its digest held", step, p, n)
			}
		}
		for _, p := range want {
			if b.reads[p] == 0 {
				t.Errorf("%s never read %s's table", step, p)
			}
		}
	}
	s := newSession(t)
	files := relinkBase
	s.check("base", []linkBuild{s.only(s.advance(files))})

	files = edit{name: "add", write: map[string]string{"f.go": "package app\n\nfunc Fresh() int { return 3 }\n"}}.apply(t, files)
	added := s.only(s.advance(files))
	s.check("file added", []linkBuild{added})
	onlyRead("the added file", added, "f.go")

	files = edit{name: "remove", remove: []string{"e.go"}}.apply(t, files)
	removed := s.only(s.advance(files))
	s.check("file removed", []linkBuild{removed})
	onlyRead("the removed file", removed, "a.go")

	py := newSession(t)
	pyFiles := map[string]string{
		"leaf.py": "def other():\n    return 2\n\n\ndef helper():\n    return 1\n",
		"pets.py": "class Dog:\n    def speak(self):\n        return \"woof\"\n",
		"main.py": "from leaf import helper\nfrom pets import Dog\n\n\ndef main():\n    helper()\n    Dog().speak()\n",
	}
	py.check("python base", []linkBuild{py.only(py.advance(pyFiles))})
	pyFiles = edit{name: "python add", write: map[string]string{"extra.py": "def extra():\n    return 3\n"}}.apply(t, pyFiles)
	pyAdded := py.only(py.advance(pyFiles))
	py.check("python file added", []linkBuild{pyAdded})
	onlyRead("the added Python file", pyAdded, "extra.py")
}

// TestLinkRenameRepublishesOnlyTheSitesThatNamedIt renames Run in a.go and
// compares the refresh with a full build of the same tree. The rename
// republishes a.go and b.go, whose sites named Run and are now unbound; e.go
// calls Other in the same file and c.go calls Twice in b.go, and neither
// named Run, so both are carried.
//
// Mutation that fails it: republish every file with a site bound into the
// edited file (e.go is short of its carried rows); republish the caller's
// callers (c.go is). Republishing a.go alone leaves b.go's carried calls of
// Run, which the full build does not have.
func TestLinkRenameRepublishesOnlyTheSitesThatNamedIt(t *testing.T) {
	s := newSession(t)
	files := relinkBase
	s.check("base", s.advance(files))
	files = edit{name: "rename", replace: []sub{{"a.go", "func Run(", "func Exec("}}}.apply(t, files)
	renamed := s.only(s.advance(files))
	s.check("rename", []linkBuild{renamed})
	s.wantRepublished("rename", renamed, []string{"a.go", "b.go"}, "c.go", "e.go")
}

// TestGoDependencyBumpRerunsWhatItRead bumps a requirement the unit imports,
// whose module the module cache holds at every version, and compares each
// refresh with a full build of the same tree (session.check). n.go imports
// example.com/dep and a.go's T has the one method of dep.Namer, so T
// implements it.
//
// A bump to a version that declares the same Namer moves a manifest field
// and nothing the unit's resolution read: no table is read and nothing is
// republished. A bump to a version whose Namer gains a method rebuilds the
// hierarchy (T no longer satisfies it): a.go, whose T held the implements
// and overrides facts, is republished and b.go, which reads nothing of dep,
// is carried.
//
// Mutations that fail it: let every manifest field break the layout (the
// identical bump reads every table); keep the predecessor's hierarchy on
// any bump (the changed Namer's facts differ from the full build's);
// digest the requirement's version instead of what its packages declare
// (the identical bump reads every table).
func TestGoDependencyBumpRerunsWhatItRead(t *testing.T) {
	cache := t.TempDir()
	namer := map[string]string{
		"v1.0.0": "package dep\n\ntype Namer interface {\n\tName() string\n}\n",
		"v1.1.0": "package dep\n\ntype Namer interface {\n\tName() string\n}\n",
		"v1.2.0": "package dep\n\ntype Namer interface {\n\tName() string\n\tID() int\n}\n",
	}
	for v, src := range namer {
		dir := filepath.Join(cache, "example.com", "dep@"+v)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, b := range map[string]string{"go.mod": "module example.com/dep\n\ngo 1.22\n", "dep.go": src} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(b), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Setenv("GOMODCACHE", cache)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	s := newSession(t)
	files := map[string]string{
		"go.mod": "module example.com/app\n\ngo 1.22\n\nrequire example.com/dep v1.0.0\n",
		"a.go":   "package app\n\ntype T struct{}\n\nfunc (T) Name() string { return \"t\" }\n",
		"n.go":   "package app\n\nimport \"example.com/dep\"\n\nfunc Use(n dep.Namer) string { return n.Name() }\n",
		"b.go":   "package app\n\nfunc Other() int { return 1 }\n\nfunc Call() int { return Other() }\n",
	}
	s.check("base", s.advance(files))

	files = edit{name: "same surface", replace: []sub{{"go.mod", "v1.0.0", "v1.1.0"}}}.apply(t, files)
	same := s.only(s.advance(files))
	s.check("bump keeping the surface", []linkBuild{same})
	s.wantRepublished("bump keeping the surface", same, nil, "a.go", "b.go", "n.go")
	for p, n := range same.reads {
		if n != 0 {
			t.Errorf("the bump keeping what dep declares read %s's table %d times, want none", p, n)
		}
	}

	files = edit{name: "changed surface", replace: []sub{{"go.mod", "v1.1.0", "v1.2.0"}}}.apply(t, files)
	moved := s.only(s.advance(files))
	s.check("bump changing the surface", []linkBuild{moved})
	s.wantRepublished("bump changing the surface", moved, []string{"a.go"}, "b.go")
}

// inputDigest is the member digest unit recorded for its input at path:
// the digest of the link table it read in place of the file's bytes.
func (s *session) inputDigest(unit model.UnitID, path string) []byte {
	s.t.Helper()
	var d []byte
	if err := s.db.QueryRow(`SELECT digest FROM unit_inputs WHERE unit_id = ? AND lower(hex(file_id)) = ?`,
		unitRowID(s.t, s.db, unit), string(model.NewFileID(s.f.repo, path))).Scan(&d); err != nil {
		s.t.Fatalf("the input %s of unit %s: %v", path, unit, err)
	}
	return d
}

// nodeNamed is the node surrogate of the one node fact named name in unit.
func (s *session) nodeNamed(unit model.UnitID, name string) int64 {
	s.t.Helper()
	var id int64
	if err := s.db.QueryRow(`SELECT node_id FROM node_facts WHERE unit_id = ? AND name = ?`,
		unitRowID(s.t, s.db, unit), name).Scan(&id); err != nil {
		s.t.Fatalf("the node %s of unit %s: %v", name, unit, err)
	}
	return id
}

// callsInto counts unit's calls relations whose target is node.
func (s *session) callsInto(unit model.UnitID, node int64) int {
	s.t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM relation_facts rf JOIN relation_ids r ON r.id = rf.relation_id
		WHERE rf.unit_id = ? AND r.kind = ? AND r.to_node_id = ?`,
		unitRowID(s.t, s.db, unit), string(model.RelCalls), node).Scan(&n); err != nil {
		s.t.Fatal(err)
	}
	return n
}

// countedTables counts the tables read and the flow-family sites resolved
// (link.Observer), by path; digests are not counted.
type countedTables struct {
	link.StoredTables
	mu    sync.Mutex
	reads map[string]int
	sites map[string]int
}

var _ link.Observer = (*countedTables)(nil)

func (c *countedTables) Resolved(path string) {
	c.mu.Lock()
	c.sites[path]++
	c.mu.Unlock()
}

func (c *countedTables) Table(ctx context.Context, gen model.GenerationID, fv model.FileVersion) (*link.Table, bool, error) {
	c.mu.Lock()
	c.reads[fv.Path]++
	c.mu.Unlock()
	return c.StoredTables.Table(ctx, gen, fv)
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
	clear(c.sites)
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
	if _, err := f.store.AttachUnit(f.ctx, gen, unit); err != nil {
		f.t.Fatalf("AttachUnit: %v", err)
	}
}

// TestFlowBodyEditRerunsNoOtherFile refreshes a JavaScript and a Python
// unit after an edit that adds a call inside leaf's helper, which changes
// leaf's sites and no interface entry and sends no value across its
// boundary (helper is called with no argument and returns a constant), and
// compares each refresh with a full build of the same tree (session.check).
// The refresh re-derives leaf's nodes alone: it reads no other file's table
// (the hierarchy and the module index are the predecessor's) and resolves
// no other file's site.
//
// Mutation: re-resolve the unit after any moved table -> main and pets are
// read and their sites resolved again. Mutation: rebuild the Python module
// index on every refresh -> main.py and pets.py are read.
func TestFlowBodyEditRerunsNoOtherFile(t *testing.T) {
	for _, c := range []struct {
		name         string
		files        map[string]string
		leaf, before string
		after        string
		others       []string
	}{
		{
			name: "javascript",
			files: map[string]string{
				"package.json": "{\n  \"name\": \"app\",\n  \"type\": \"module\"\n}\n",
				"leaf.js":      "export function other() {\n  return 2;\n}\n\nexport function helper() {\n  return 1;\n}\n",
				"pets.js":      "export class Dog {\n  speak() {\n    return \"woof\";\n  }\n}\n",
				"main.js": "import { helper } from \"./leaf\";\nimport { Dog } from \"./pets\";\n\n" +
					"export function main() {\n  helper();\n  new Dog().speak();\n}\n",
			},
			leaf: "leaf.js", before: "  return 1;\n", after: "  other();\n  return 1;\n",
			others: []string{"main.js", "pets.js"},
		},
		{
			name: "python",
			files: map[string]string{
				"leaf.py": "def other():\n    return 2\n\n\ndef helper():\n    return 1\n",
				"pets.py": "class Dog:\n    def speak(self):\n        return \"woof\"\n",
				"main.py": "from leaf import helper\nfrom pets import Dog\n\n\ndef main():\n    helper()\n    Dog().speak()\n",
			},
			leaf: "leaf.py", before: "    return 1\n", after: "    other()\n    return 1\n",
			others: []string{"main.py", "pets.py"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newSession(t)
			files := c.files
			s.check("base", s.advance(files))
			files = edit{name: "body adding a call", replace: []sub{{c.leaf, c.before, c.after}}}.apply(t, files)
			b := s.only(s.advance(files))
			s.check("body adding a call", []linkBuild{b})
			for _, p := range c.others {
				if n := b.reads[p]; n != 0 {
					t.Errorf("the refresh read %s's table %d times, want none: no value or name it reads moved", p, n)
				}
				if n := b.sites[p]; n != 0 {
					t.Errorf("the refresh resolved %d of %s's sites, want none: no value or name they read moved", n, p)
				}
			}
			if b.reads[c.leaf] == 0 || b.sites[c.leaf] == 0 {
				t.Errorf("the refresh read %s's table %d times and resolved %d of its sites, want both: its sites moved",
					c.leaf, b.reads[c.leaf], b.sites[c.leaf])
			}
		})
	}
}

// TestRenamedParameterMovesKeywordFlow renames the parameter a of f to b in
// callee.py, which caller.py calls as f(b=x): before the rename the keyword
// names no parameter and binds f's **kw; after it, it binds b. The refresh
// must equal the full build of the same tree (session.check), and the
// caller's one data_flows_to relation moves from **kw to b. JavaScript and
// TypeScript have no keyword arguments, so this is Python's alone.
//
// Mutation: keep the old parameter target (re-run nothing of caller.py when
// only callee.py's parameters moved) -> the refreshed unit still flows into
// **kw and none into b.
func TestRenamedParameterMovesKeywordFlow(t *testing.T) {
	s := newSession(t)
	files := map[string]string{
		"callee.py": "def f(a=0, **kw):\n    return a\n",
		"caller.py": "from callee import f\n\n\ndef g(x):\n    return f(b=x)\n",
	}
	base := s.only(s.advance(files))
	s.check("base", []linkBuild{base})
	if n := s.flows(base.unit, ""); n != 1 {
		t.Fatalf("the base unit holds %d data_flows_to relations, want the one of f(b=x) into **kw", n)
	}
	if n := s.flows(base.unit, "b"); n != 0 {
		t.Fatalf("the base unit holds %d data_flows_to relations into a parameter b, want none: f has no b", n)
	}
	files = edit{name: "parameter renamed", replace: []sub{{"callee.py", "def f(a=0, **kw):\n    return a\n",
		"def f(b=0, **kw):\n    return b\n"}}}.apply(t, files)
	renamed := s.only(s.advance(files))
	s.check("parameter renamed", []linkBuild{renamed})
	if n := s.flows(renamed.unit, ""); n != 1 {
		t.Errorf("the refreshed unit holds %d data_flows_to relations, want the one of f(b=x)", n)
	}
	if n := s.flows(renamed.unit, "b"); n != 1 {
		t.Errorf("the refreshed unit holds %d data_flows_to relations into the parameter b, want f(b=x)'s", n)
	}
	if renamed.sites["caller.py"] == 0 {
		t.Errorf("the refresh resolved none of caller.py's sites, want f(b=x): the parameter it names moved")
	}
}

// flows counts unit's argument flows, the data_flows_to relations an
// argument's evidence carries (not the return flows beside them), into a
// node of unit named name when name is set.
func (s *session) flows(unit model.UnitID, name string) int {
	s.t.Helper()
	row := unitRowID(s.t, s.db, unit)
	q := `SELECT count(*) FROM relation_facts rf JOIN relation_ids r ON r.id = rf.relation_id
		WHERE rf.unit_id = ?1 AND r.kind = ?2 AND EXISTS (SELECT 1 FROM evidence e
			WHERE e.unit_id = rf.unit_id AND e.relation_id = rf.relation_id AND e.detail = 'argument')`
	args := []any{row, string(model.RelDataFlowsTo)}
	if name != "" {
		q += ` AND r.to_node_id IN (SELECT node_id FROM node_facts WHERE unit_id = ?1 AND name = ?3)`
		args = append(args, name)
	}
	var n int
	if err := s.db.QueryRow(q, args...).Scan(&n); err != nil {
		s.t.Fatal(err)
	}
	return n
}

// TestRenamedParameterMovesFlowThroughAValue renames the parameter a of f to
// b in callee.py, which caller.py takes as a value and calls through it as
// h(b=x): the call binds f through the value (no name at the site names f),
// and the keyword names no parameter before the rename and binds f's **kw,
// then binds b after it. The refresh must equal the full build of the same
// tree (session.check), and the caller's one argument flow moves from **kw
// to b.
//
// Mutation: keep the stored flows of a site bound through a value when only
// the callee's parameters moved (re-run nothing of caller.py) -> the
// refreshed unit still flows into **kw and none into b.
func TestRenamedParameterMovesFlowThroughAValue(t *testing.T) {
	s := newSession(t)
	files := map[string]string{
		"callee.py": "def f(a=0, **kw):\n    return a\n",
		"caller.py": "from callee import f\n\n\ndef g(x):\n    h = f\n    return h(b=x)\n",
	}
	base := s.only(s.advance(files))
	s.check("base", []linkBuild{base})
	if n := s.flows(base.unit, ""); n != 1 {
		t.Fatalf("the base unit holds %d argument flows, want the one of h(b=x) into **kw", n)
	}
	if n := s.flows(base.unit, "b"); n != 0 {
		t.Fatalf("the base unit holds %d argument flows into a parameter b, want none: f has no b", n)
	}
	files = edit{name: "parameter renamed", replace: []sub{{"callee.py", "def f(a=0, **kw):\n    return a\n",
		"def f(b=0, **kw):\n    return b\n"}}}.apply(t, files)
	renamed := s.only(s.advance(files))
	s.check("parameter renamed", []linkBuild{renamed})
	if n := s.flows(renamed.unit, "b"); n != 1 {
		t.Errorf("the refreshed unit holds %d argument flows into the parameter b, want h(b=x)'s", n)
	}
	if renamed.sites["caller.py"] == 0 {
		t.Errorf("the refresh resolved none of caller.py's sites, want h(b=x): the parameter it names moved")
	}
}
