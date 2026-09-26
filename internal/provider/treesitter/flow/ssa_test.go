package flow

import (
	"cmp"
	"slices"
	"testing"
)

// TestDefUseEdgeSemantics protects the value DefUse carries along an edge,
// on graphs built directly with the Builder. A wrong answer here is a wrong
// data-flow fact served for every function of that shape.
func TestDefUseEdgeSemantics(t *testing.T) {
	cases := []struct {
		name string
		// build lowers the function and returns the expected pairs.
		build func(b *Builder) [][2]int32
	}{{
		// Fails when an edge into a Handler carries the throwing node's own
		// definition (mutation: use inclusive for Handler-led blocks): the
		// catch would see x = g() instead of x = 1.
		name: "a Handler sees the entry values of the node that threw",
		build: func(b *Builder) [][2]int32 {
			x := b.Var()
			one := b.Node(Stmt, Span{})
			b.Def(one, x)
			c := b.OpenCatch()
			call := b.Node(Stmt, Span{})
			b.Def(call, x)
			b.MayThrow(call)
			done := b.Push()
			b.EnterHandler(c, Span{})
			ret := b.Node(Jump, Span{})
			b.Use(ret, x)
			b.Return()
			b.Merge(done)
			b.Pop(done)
			after := b.Node(Stmt, Span{})
			b.Use(after, x)
			return [][2]int32{{one, ret}, {call, after}}
		},
	}, {
		// Fails when a may-definition kills (mutation: out returns d for a
		// may-definition): the return would lose n = 0.
		name: "a may-definition pairs with itself and with what reaches it",
		build: func(b *Builder) [][2]int32 {
			n := b.Var()
			zero := b.Node(Stmt, Span{})
			b.Def(zero, n)
			closure := b.Node(Stmt, Span{})
			b.MayDef(closure, n)
			ret := b.Node(Jump, Span{})
			b.Use(ret, n)
			b.Return()
			return [][2]int32{{zero, ret}, {closure, ret}}
		},
	}, {
		// Fails when a use reads its own node's definition or the search
		// inside a block misses the nearest earlier site (mutation: inclusive
		// lookup for a use).
		name: "a straight line pairs each use with the nearest earlier definition",
		build: func(b *Builder) [][2]int32 {
			x := b.Var()
			d1 := b.Node(Stmt, Span{})
			b.Def(d1, x)
			d2 := b.Node(Stmt, Span{})
			b.Use(d2, x)
			b.Def(d2, x)
			u := b.Node(Stmt, Span{})
			b.Use(u, x)
			return [][2]int32{{d1, d2}, {d2, u}}
		},
	}, {
		// Fails when a join's φ misses its back-edge operand (mutation: a
		// join takes only its first predecessor).
		name: "a loop head merges the initial and the loop-carried definition",
		build: func(b *Builder) [][2]int32 {
			i := b.Var()
			init := b.Node(Stmt, Span{})
			b.Def(init, i)
			f := b.OpenLoop()
			head := b.Node(Branch, Span{})
			b.Use(head, i)
			exit := b.Push()
			body := b.Node(Stmt, Span{})
			b.Use(body, i)
			b.Def(body, i)
			b.ContinueHere(f)
			b.Close(head)
			b.Restore(exit)
			b.CloseFrame(f)
			b.Pop(exit)
			after := b.Node(Stmt, Span{})
			b.Use(after, i)
			return [][2]int32{
				{init, head}, {init, body}, {init, after},
				{body, head}, {body, body}, {body, after},
			}
		},
	}, {
		// Fails when a cycle no leader enters is left without a block or
		// walked forever (mutation: drop the second layout pass), and when a
		// definition outside it leaks in.
		name: "an unreachable cycle sees only its own definitions",
		build: func(b *Builder) [][2]int32 {
			x, y := b.Var(), b.Var()
			outer := b.Node(Stmt, Span{})
			b.Def(outer, x)
			b.Def(b.Node(Stmt, Span{}), y)
			b.Return()
			top := b.Label("top", Span{})
			b.Use(top, x)
			b.Use(top, y)
			tail := b.Node(Stmt, Span{})
			b.Def(tail, x)
			b.Goto("top")
			return [][2]int32{{tail, top}}
		},
	}}
	var a Arena
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := a.Begin(Span{})
			want := c.build(b)
			du := DefUse(b.Finish(), &a)
			got := make([][2]int32, du.Len())
			for i := range got {
				got[i][0], got[i][1] = du.At(i)
			}
			slices.SortFunc(want, func(p, q [2]int32) int {
				return cmp.Compare(pack(p[0], p[1]), pack(q[0], q[1]))
			})
			if !slices.Equal(got, want) {
				t.Errorf("DefUse = %v, want %v", got, want)
			}
		})
	}
}
