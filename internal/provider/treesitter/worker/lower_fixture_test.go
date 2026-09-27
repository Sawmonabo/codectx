package worker

import (
	"errors"
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
// source must parse clean. callables, when nonzero, is the exact number of
// callables Functions reports in src, which pins what is and is not a function
// (a bodiless method, a signature) where no pair could show it.
type goldenCase struct {
	name, protects, mutation, src string
	fn                            int
	cd, du                        []string
	unresolved                    int
	recovered                     bool
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
//     it. A use that no definition reaches (a node with no path from ENTRY,
//     a name no node defines) makes no pair.
//   - Unresolved is the count of jumps (a break, continue or goto) whose
//     target names no open frame or label.
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
			defer tree.Close()
			if got := tree.RootNode().HasError(); got != c.recovered {
				t.Fatalf("syntax error in the %s tree = %v, want %v: %s", language, got, c.recovered, tree.RootNode().ToSexp())
			}
			var fn *ts.Node
			i := 0
			if err := low.Functions(tree.RootNode(), func(n *ts.Node) error {
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
			if fn == nil {
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
			if w := want(c.cd); !slices.Equal(cd, w) {
				t.Errorf("control dependence\n got  %q\n want %q\nprotects: %s\nmutation: %s", cd, w, c.protects, c.mutation)
			}
			if w := want(c.du); !slices.Equal(du, w) {
				t.Errorf("def-use\n got  %q\n want %q\nprotects: %s\nmutation: %s", du, w, c.protects, c.mutation)
			}
			if got := g.Unresolved(); got != c.unresolved {
				t.Errorf("unresolved = %d, want %d\nprotects: %s\nmutation: %s", got, c.unresolved, c.protects, c.mutation)
			}
		})
	}
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
			defer tree.Close()
			var fn *ts.Node
			found := errors.New("found")
			_ = low.Functions(tree.RootNode(), func(m *ts.Node) error {
				if m.Kind() == "function_declaration" {
					fn = m
					return found
				}
				return nil
			})
			if fn == nil {
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
