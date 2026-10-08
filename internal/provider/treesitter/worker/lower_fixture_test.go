package worker

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	ts "github.com/tree-sitter/go-tree-sitter"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/flow"
)

// goldenCase is one hand-derived dependence fixture: the fn-th callable (in
// Functions preorder, from 0) of src, and the exact control-dependence and
// def-use pairs and unresolved-jump count its lowering must produce. protects
// names the failure mode the case guards; mutation names the code change that
// would fail it. recovered marks a source that parses with a syntax error on
// purpose, to pin how a lowering treats an error-recovered tree; every other
// source must parse clean. illFormed marks a source that parses clean but that
// its language's compiler rejects on purpose, such as a jump to a label no
// frame opens, to pin how a lowering treats a program no compiler accepts;
// every other source is one its compiler accepts, names it leaves undeclared
// aside. callables, when nonzero, is the exact number of callables Functions
// reports in src, which pins what is and is not a function (a bodiless
// method, a signature) where no pair could show it.
type goldenCase struct {
	name, protects, mutation, src string
	fn                            int
	cd, du                        []string
	unresolved                    int
	recovered, illFormed          bool
	callables                     int
}

// runGolden checks every case of language, one subtest per case. Every
// golden table derives its cases by these rules, and states only what its
// language adds:
//
//   - Grammars. A construct is covered for a grammar only by a case run
//     through that grammar. Languages that share a lowering share cases: a
//     c case whose source parses clean as C++ runs under cpp too, a
//     javascript case under typescript and tsx, and a typescript case under
//     tsx, each with the one expected set its table states. A case whose
//     source does not parse clean in a sibling grammar runs only under its
//     own, and the sibling's table holds its own case for the construct.
//   - Parsing. Every source must parse with no ERROR or MISSING node in the
//     grammar it runs under, so a pair set is never derived from a tree the
//     parser recovered; a recovered case must hold one instead.
//   - Rendering. A node renders as ENTRY, EXIT or "<source text of its span,
//     whitespace collapsed>@<start byte>", every other node alike, a Handler
//     included (a catch clause's Handler spans the clause's keyword, so it
//     renders as catch@<offset>); the offset keeps two identical statements
//     distinct. A pair renders as "<from> -> <to>", and the sorted rendered
//     lists must equal cd and du exactly.
//   - Exit augmentation. An edge to EXIT is added from every node with no
//     successor, and from the smallest-reverse-post-order member of each
//     sink strongly connected component that cannot reach EXIT. A node with
//     no predecessor keeps its successors and gains no such edge unless it
//     has none.
//   - Control dependence is the post-dominance frontier over the augmented
//     graph with no entry-to-exit edge: nothing depends on ENTRY, and a loop
//     head whose back edge it controls depends on itself.
//   - Def-use. A pair is (defining node, using node), with every φ resolved
//     to the definitions it merges. A may-definition is a χ (lower.go,
//     May-definitions): a use pairs with every killing definition reaching
//     it through any number of may-definitions, and with the nearest
//     may-definition on each path, the one no later may-definition of the
//     variable follows; the may-defining node pairs with what reaches it by
//     the same rule, whether or not it reads the variable. A node may make
//     several killing definitions.
//     A Handler node carries the values on entry to each node that threw to
//     it. A use that no definition reaches (on entry to ENTRY or to a node
//     with no predecessor, around a cycle no definition enters, or of a name
//     no node defines) makes no pair; a region with no path from ENTRY
//     still pairs its own definitions with its own uses.
//   - Unresolved is the count of jumps (a break, continue or goto) whose
//     target names no open frame or label. Only an ill-formed or recovered
//     source can hold one.
func runGolden(t *testing.T, language string, cases []goldenCase) {
	t.Helper()
	tl, ok := Grammar(language)
	if !ok {
		t.Fatalf("no grammar for %q", language)
	}
	low, ok := LoweringFor(language)
	if !ok {
		t.Fatalf("no lowering for %q", language)
	}
	// One Scratch serves every case, as one serves every function a worker
	// lowers, so a list a case leaves behind reaches the next one's lowering.
	var s Scratch
	defer s.Close()
	// Each grammar is its own subtest, so a failure names the grammar that
	// produced it even where two grammars share a case's name.
	t.Run(language, func(t *testing.T) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				src := []byte(c.src)
				p := ts.NewParser()
				defer p.Close()
				if err := p.SetLanguage(tl); err != nil {
					t.Fatalf("set language: %v", err)
				}
				tree := p.Parse(src, nil)
				if tree == nil {
					t.Fatal("the parser produced no tree")
				}
				// The production path: the tree is flattened, closed, and every
				// callable is found and lowered from the flat array alone.
				sexp := tree.RootNode().ToSexp()
				flat, err := Flatten(tree, language)
				tree.Close()
				if err != nil {
					t.Fatalf("flatten: %v", err)
				}
				if got := flat.Root().HasError(); got != c.recovered {
					t.Fatalf("syntax error in the %s tree = %v, want %v: %s", language, got, c.recovered, sexp)
				}
				if c.unresolved != 0 && !c.illFormed && !c.recovered {
					t.Errorf("unresolved = %d on a source not marked ill-formed: a jump whose target no frame opens is rejected by every compiler", c.unresolved)
				}
				var fn Node
				i := 0
				if err := low.Functions(flat.Root(), func(n Node) error {
					if i == c.fn {
						fn = n
					}
					i++
					return nil
				}); err != nil {
					t.Fatalf("functions: %v", err)
				}
				if c.callables != 0 && i != c.callables {
					t.Errorf("callables = %d, want %d\nprotects: %s\nmutation: %s", i, c.callables, c.protects, c.mutation)
				}
				if fn.IsNull() {
					t.Fatalf("callable %d not found (%d callables)", c.fn, i)
				}
				var a flow.Arena
				g := low.Lower(fn, src, &a, &s)
				pd := flow.PostDominators(g, &a)
				render := func(e flow.Edges) []string {
					out := make([]string, 0, e.Len())
					for j := range e.Len() {
						from, to := e.At(j)
						out = append(out, renderNode(g, src, from)+" -> "+renderNode(g, src, to))
					}
					slices.Sort(out)
					return out
				}
				cd := render(flow.ControlDependence(g, pd, &a))
				du := render(flow.DefUse(g, &a))
				want := func(s []string) []string {
					s = slices.Clone(s)
					slices.Sort(s)
					return s
				}
				mismatch := false
				if w := want(c.cd); !slices.Equal(cd, w) {
					mismatch = true
					t.Errorf("control dependence\n got  %q\n want %q\nprotects: %s\nmutation: %s", cd, w, c.protects, c.mutation)
				}
				if w := want(c.du); !slices.Equal(du, w) {
					mismatch = true
					t.Errorf("def-use\n got  %q\n want %q\nprotects: %s\nmutation: %s", du, w, c.protects, c.mutation)
				}
				if got := g.Unresolved(); got != c.unresolved {
					mismatch = true
					t.Errorf("unresolved = %d, want %d\nprotects: %s\nmutation: %s", got, c.unresolved, c.protects, c.mutation)
				}
				if mismatch {
					t.Logf("lowered graph:\n%s\ncd:\n  %s\ndu:\n  %s\ntree: %s",
						dumpGraph(g, src), strings.Join(cd, "\n  "), strings.Join(du, "\n  "), sexp)
				}
			})
		}
	})
}

// kindNames renders a flow.Kind in a graph dump.
var kindNames = [...]string{
	flow.Entry:   "entry",
	flow.Exit:    "exit",
	flow.Stmt:    "stmt",
	flow.Branch:  "branch",
	flow.Jump:    "jump",
	flow.Handler: "handler",
}

// dumpGraph renders every node of g, one per line: its id, kind and
// rendering, its successors by id, and the variable ids it defines, may
// define and uses. A failing golden case logs it, so a wrong pair can be
// traced to the node or edge that made it.
func dumpGraph(g *flow.Graph, src []byte) string {
	var b strings.Builder
	for n := range int32(g.Len()) {
		fmt.Fprintf(&b, "  %d %s %s succ=%v def=%v may=%v use=%v\n",
			n, kindNames[g.Kind(n)], renderNode(g, src, n), g.Succ(n), g.Defs(n), g.MayDefs(n), g.Uses(n))
	}
	return b.String()
}

// renderNode renders node n of g for golden comparison.
func renderNode(g *flow.Graph, src []byte, n int32) string {
	switch n {
	case flow.EntryNode:
		return "ENTRY"
	case flow.ExitNode:
		return "EXIT"
	}
	s := g.Span(n)
	return strings.Join(strings.Fields(string(src[s.Start:s.End])), " ") + "@" + strconv.FormatUint(uint64(s.Start), 10)
}

// TestMayDefinitionChainIsLinear protects the def-use pass from a chain of
// writes through one base, the shape that exhausted a worker's memory: each
// write is a may-definition of the base (a χ), and pairing a use with every
// may-definition reaching it asks for a quadratic pair set.
//
// Derivation, for n writes `o.f = 0` after the parameter o: the parameter's
// node is o's one killing definition, and write i Uses o and may-defines it.
// Write 1 pairs with the parameter alone. Write i > 1 pairs with the
// parameter, the killing definition behind the chain, and with write i-1,
// its nearest may-definition; the writes before i-1 are behind that nearest
// one. So the count is 1 + 2(n-1) = 2n - 1, whatever n is: this asserts the
// size of the algorithm's output, not a limit.
//
// Mutation: pair a use with every may-definition reaching it (write i then
// pairs with the parameter and all i-1 writes before it), and the count
// becomes n(n+1)/2, failing the assertion.
func TestMayDefinitionChainIsLinear(t *testing.T) {
	const n = 4000
	for _, c := range []struct{ language, head, write, tail string }{
		{"javascript", "function f(o) {", " o.f = 0;", " }"},
		{"go", "package p\nfunc f(o *T) {", "\no.f = 0", "\n}"},
	} {
		t.Run(c.language, func(t *testing.T) {
			src := []byte(c.head + strings.Repeat(c.write, n) + c.tail)
			tl, ok := Grammar(c.language)
			if !ok {
				t.Fatalf("no grammar for %q", c.language)
			}
			low, ok := LoweringFor(c.language)
			if !ok {
				t.Fatalf("no lowering for %q", c.language)
			}
			p := ts.NewParser()
			defer p.Close()
			if err := p.SetLanguage(tl); err != nil {
				t.Fatalf("set language: %v", err)
			}
			tree := p.Parse(src, nil)
			if tree == nil {
				t.Fatal("the parser produced no tree")
			}
			flat, err := Flatten(tree, c.language)
			tree.Close()
			if err != nil {
				t.Fatalf("flatten: %v", err)
			}
			declaration := tl.IdForNodeKind("function_declaration", true)
			var fn Node
			found := errors.New("found")
			_ = low.Functions(flat.Root(), func(m Node) error {
				if m.KindId() == declaration {
					fn = m
					return found
				}
				return nil
			})
			if fn.IsNull() {
				t.Fatal("no function declaration")
			}
			var s Scratch
			defer s.Close()
			var a flow.Arena
			g := low.Lower(fn, src, &a, &s)
			if got, want := flow.DefUse(g, &a).Len(), 2*n-1; got != want {
				t.Errorf("def-use pairs = %d, want 2n-1 = %d for n = %d writes through one base", got, want, n)
			}
		})
	}
}

// TestOneBindingIsOneNamedVariable pins which identifiers a lowering
// declares with Builder.Named, the spans the parent mints one variable entity
// from (docs/providers-treesitter.md, Variable entity).
//
// Failure modes: a lowering that declares one binding at two identifiers
// publishes two variable entities for it, so a fact through one is missing
// from the other; one that declares a selector publishes a variable named
// `a.b` that names no binding.
//
// Mutation: declare every `:=` range target in goLower.rangeLoop, whatever
// its kind -> the go case declares a.b. Declare a function declaration in
// jsLower.predeclare whatever its var scope already binds -> each
// javascript case declares f twice.
func TestOneBindingIsOneNamedVariable(t *testing.T) {
	var s Scratch
	defer s.Close()
	for _, c := range []struct {
		name, language, src string
		fn                  int
		want                []string
	}{
		{"a selector range target declares nothing", "go",
			"package p\n\nfunc f(s []int) {\n\tfor a.b := range s {\n\t}\n}\n", 0, []string{"s"}},
		{"a var and a function declaration in a body are one binding", "javascript",
			"function g() {\n  var f = 1;\n  function f() {}\n}\n", 1, []string{"f"}},
		{"a var and a function declaration in a program are one binding", "javascript",
			"var f = 1;\nfunction f() {}\n", 0, []string{"f"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			tl, ok := Grammar(c.language)
			if !ok {
				t.Fatalf("no grammar for %q", c.language)
			}
			low, ok := LoweringFor(c.language)
			if !ok {
				t.Fatalf("no lowering for %q", c.language)
			}
			src := []byte(c.src)
			p := ts.NewParser()
			defer p.Close()
			if err := p.SetLanguage(tl); err != nil {
				t.Fatalf("set language: %v", err)
			}
			tree := p.Parse(src, nil)
			if tree == nil {
				t.Fatal("the parser produced no tree")
			}
			flat, err := Flatten(tree, c.language)
			tree.Close()
			if err != nil {
				t.Fatalf("flatten: %v", err)
			}
			var fn Node
			i := 0
			if err := low.Functions(flat.Root(), func(n Node) error {
				if i == c.fn {
					fn = n
				}
				i++
				return nil
			}); err != nil {
				t.Fatalf("functions: %v", err)
			}
			if fn.IsNull() {
				t.Fatalf("callable %d not found (%d callables)", c.fn, i)
			}
			var a flow.Arena
			g := low.Lower(fn, src, &a, &s)
			var got []string
			for v := range int32(g.Vars()) {
				if span, named := g.Declared(v); named {
					got = append(got, string(src[span.Start:span.End]))
				}
			}
			slices.Sort(got)
			if !slices.Equal(got, c.want) {
				t.Fatalf("named variables = %q, want %q", got, c.want)
			}
		})
	}
}
