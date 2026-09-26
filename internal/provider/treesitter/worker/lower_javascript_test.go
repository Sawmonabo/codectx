package worker

import "testing"

// TestJavaScriptLoweringGolden pins the control-dependence and def-use pairs
// of hand-derived JavaScript functions. Each case is callable 1 in Functions
// preorder (callable 0 is the program). Every pair was derived by hand from
// the lowering's documented granularity and the rules: exit augmentation adds
// an edge to Exit from every successor-less node and from the
// smallest-reverse-post-order member of each sink strongly connected
// component that cannot reach Exit; control dependence is the post-dominance
// frontier over the augmented graph with no entry-to-exit edge, so nothing
// depends on Entry and a loop head depends on itself; def-use pairs are
// (defining node, using node) with every φ resolved, self-pairs included.
func TestJavaScriptLoweringGolden(t *testing.T) {
	runGolden(t, "javascript", []goldenCase{
		{
			name:     "infinite loop with a branch inside",
			protects: "an infinite loop's nodes get post-dominators only through exit augmentation, and `while (true)` has no exit edge",
			mutation: "drop the sink-component augmentation (every control dependence here is lost), or give `while (true)` a false edge (h(a) becomes reachable and gains a def-use pair)",
			src:      "function f(a) { while (true) { if (a) g(); } h(a); }",
			fn:       1,
			cd:       []string{"true@23 -> true@23", "true@23 -> a@35", "a@35 -> g()@38"},
			du:       []string{"a@11 -> a@35"},
		},
		{
			name:     "region unreachable from exit",
			protects: "a sink loop inside a branch is augmented at its smallest-reverse-post-order member, the loop head",
			mutation: "augment every member of the sink component, or its largest member (g() becomes a controller)",
			src:      "function f(c) { if (c) { for (;;) { g(); } } return c; }",
			fn:       1,
			cd:       []string{"c@20 -> for@25", "c@20 -> return c;@45", "for@25 -> for@25", "for@25 -> g()@36"},
			du:       []string{"c@11 -> c@20", "c@11 -> return c;@45"},
		},
		{
			name:     "labelled break and continue out of nested loops",
			protects: "a labelled continue targets the labelled loop's head and a labelled break leaves the labelled loop",
			mutation: "resolve a labelled jump against the innermost loop instead of the one its label names",
			src:      "function f(a, b) { outer: while (a) { while (b) { if (g()) continue outer; if (h()) break outer; } k(); } }",
			fn:       1,
			cd: []string{
				"a@33 -> b@45",
				"b@45 -> g()@54", "b@45 -> k()@99", "b@45 -> a@33",
				"g()@54 -> continue outer;@59", "g()@54 -> a@33", "g()@54 -> h()@79",
				"h()@79 -> break outer;@84", "h()@79 -> b@45",
			},
			du: []string{"a@11 -> a@33", "b@14 -> b@45"},
		},
		{
			name:       "break to a missing label",
			protects:   "a jump to no label is counted, keeps no successor and is augmented to Exit",
			mutation:   "wire an unresolved break to the innermost loop, or drop the count",
			src:        "function f() { while (g()) { break missing; } }",
			fn:         1,
			cd:         []string{"g()@22 -> break missing;@29"},
			unresolved: 1,
		},
		{
			name:     "return inside try reaches Exit through the finally",
			protects: "a return inside a try is intercepted by the finally, which then runs with the definitions reaching the return",
			mutation: "send the return straight to Exit (b = 0 no longer reaches the finally, and the finally becomes control dependent on c)",
			src:      "function f(c) { let b = 0; try { if (c) return 1; b = 1; } finally { c = b; } }",
			fn:       1,
			cd:       []string{"c@37 -> return 1;@40", "c@37 -> b = 1@50"},
			du:       []string{"c@11 -> c@37", "b = 0@20 -> c = b@69", "b = 1@50 -> c = b@69"},
		},
		{
			name:     "a throw from the first of several try statements reaches the handler",
			protects: "an explicit throw inside a try reaches its handler, and every throwing node, not only the last, has an exceptional edge to it",
			mutation: "send the throw to Exit and wire only the try body's last statement to the handler, as the hosted engine does (the handler is no longer control dependent on c)",
			src:      "function f(c) { let r = 0; try { if (c) throw c; r = 1; h(); } catch (e) { r = e; } return r; }",
			fn:       1,
			cd: []string{
				"c@37 -> throw c;@40", "c@37 -> e@70", "c@37 -> r = e@75", "c@37 -> r = 1@49", "c@37 -> h()@56",
				"h()@56 -> e@70", "h()@56 -> r = e@75",
			},
			du: []string{
				"c@11 -> c@37", "c@11 -> throw c;@40",
				"r = 1@49 -> return r;@84", "r = e@75 -> return r;@84", "e@70 -> r = e@75",
			},
		},
		{
			name:     "short-circuit && and ?? dependences",
			protects: "the right operand of && and ?? is control dependent on the left, and the result still uses both",
			mutation: "lower && or ?? as a plain binary operator (no decision node), or drop the operand reads from the defining node",
			src:      "function f(a, b) { const x = a && g(b); const y = x ?? b; return y; }",
			fn:       1,
			cd:       []string{"a@29 -> g(b)@34", "x@50 -> b@55"},
			du: []string{
				"a@11 -> a@29", "a@11 -> x = a && g(b)@25",
				"b@14 -> g(b)@34", "b@14 -> x = a && g(b)@25", "b@14 -> b@55", "b@14 -> y = x ?? b@46",
				"x = a && g(b)@25 -> x@50", "x = a && g(b)@25 -> y = x ?? b@46",
				"y = x ?? b@46 -> return y;@58",
			},
		},
		{
			name:     "φ through a loop and through if/else",
			protects: "definitions merged at the loop head and after if/else reach every use, self-pairs included",
			mutation: "emit the φ instead of resolving it to the definitions it merges",
			src:      "function f(n) { let s = 0; let i = 0; while (i < n) { if (i > 2) { s = s + i; } else { s = s - 1; } i++; } return s; }",
			fn:       1,
			cd: []string{
				"i < n@45 -> i < n@45", "i < n@45 -> i > 2@58", "i < n@45 -> i++@100",
				"i > 2@58 -> s = s + i@67", "i > 2@58 -> s = s - 1@87",
			},
			du: []string{
				"n@11 -> i < n@45",
				"s = 0@20 -> s = s + i@67", "s = 0@20 -> s = s - 1@87", "s = 0@20 -> return s;@107",
				"s = s + i@67 -> s = s + i@67", "s = s + i@67 -> s = s - 1@87", "s = s + i@67 -> return s;@107",
				"s = s - 1@87 -> s = s + i@67", "s = s - 1@87 -> s = s - 1@87", "s = s - 1@87 -> return s;@107",
				"i = 0@31 -> i < n@45", "i = 0@31 -> i > 2@58", "i = 0@31 -> s = s + i@67", "i = 0@31 -> i++@100",
				"i++@100 -> i < n@45", "i++@100 -> i > 2@58", "i++@100 -> s = s + i@67", "i++@100 -> i++@100",
			},
		},
		{
			name:     "destructuring default and a closure that shadows",
			protects: "one defining node per bound name, a default as a conditional definition, and a closure's own parameter shadowing the enclosing name",
			mutation: "resolve names inside the arrow against the enclosing scope only (a@24 gains uses at the arrow and at g), or bind the default unconditionally",
			src:      "function f(o) { const { a, b = a } = o; const g = (a) => a + b; return g; }",
			fn:       1,
			cd:       []string{"b = a@27 -> a@31"},
			du: []string{
				"o@11 -> a@24", "o@11 -> b = a@27", "a@24 -> a@31",
				"b = a@27 -> (a) => a + b@50", "a@31 -> (a) => a + b@50",
				"b = a@27 -> g = (a) => a + b@46", "a@31 -> g = (a) => a + b@46",
				"g = (a) => a + b@46 -> return g;@64",
			},
		},
		{
			name:     "destructuring swap reads the values from before the statement",
			protects: "a destructuring element reading a variable an earlier element of the same statement wrote still depends on its old value, through that element's node, which read it before writing",
			mutation: "attach to each element node only the reads not yet attached to an earlier node (y@23 loses x@20 -> y@23 and y@14 -> y@23), or erase an element's read of a variable an earlier element wrote",
			src:      "function f(x, y) { [x, y] = [y, x]; return x + y; }",
			fn:       1,
			du: []string{
				"x@11 -> x@20", "y@14 -> x@20", "y@14 -> y@23", "x@20 -> y@23",
				"x@20 -> return x + y;@36", "y@23 -> return x + y;@36",
			},
		},
	})
}
