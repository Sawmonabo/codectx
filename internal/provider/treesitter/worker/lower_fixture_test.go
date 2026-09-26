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
// would fail it.
type goldenCase struct {
	name, protects, mutation, src string
	fn                            int
	cd, du                        []string
	unresolved                    int
}

// runGolden checks every case of language, one subtest per case. Every
// golden table derives its cases by these rules, and states only what its
// language adds:
//
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
			var fn *ts.Node
			i := 0
			errFound := errors.New("found")
			err := low.Functions(tree.RootNode(), func(n *ts.Node) error {
				if i == c.fn {
					fn = n
					return errFound
				}
				i++
				return nil
			})
			if fn == nil || !errors.Is(err, errFound) {
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
