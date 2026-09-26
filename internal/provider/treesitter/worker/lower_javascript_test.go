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
			protects: "a label on a loop names the loop's own frame, so a labelled continue targets that loop's condition and a labelled break leaves that loop",
			mutation: "open a block frame for every labelled statement instead of handing a loop its labels (continue outer then names no loop: unresolved becomes 1)",
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
			mutation: "send the throw statement to Exit instead of the catch (e and r = e lose their control dependence on c), or skip MayThrow on a call statement (h() loses its three control dependences)",
			src:      "function f(c) { let r = 0; try { if (c) throw c; r = 1; h(); } catch (e) { r = e; } return r; }",
			fn:       1,
			cd: []string{
				"c@37 -> throw c;@40", "c@37 -> e@70", "c@37 -> r = e@75", "c@37 -> r = 1@49", "c@37 -> h()@56",
				"h()@56 -> catch@63", "h()@56 -> e@70", "h()@56 -> r = e@75",
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
			name:     "while re-enters its condition and if/else merges both arms",
			protects: "a while loop's back edge re-enters its condition node and both if/else arms flow on, so both arms' definitions reach the loop, each other and the return, self-pairs included",
			mutation: "drop the Merge of the then arm's end in ifStmt (s = s + i@67 keeps no successor and every pair from it is lost), or drop the loop's back edge (i < n@45 loses its self-dependence and i++@100 its pair to i < n@45)",
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
		{
			name:     "a throwing assignment in a try leaves the old value to the catch",
			protects: "an assignment whose right side throws lands on the catch's Handler, which sees the value from before the assignment, not the assignment's",
			mutation: "carry a node's own definition along its exceptional edge (return x pairs with x = g() only, not with x = 1)",
			src:      "function f() { let x = 1; try { x = g(); } catch (e) { return x; } }",
			fn:       1,
			cd:       []string{"x = g()@32 -> catch@43", "x = g()@32 -> e@50", "x = g()@32 -> return x;@55"},
			du:       []string{"x = 1@19 -> return x;@55"},
		},
		{
			name:     "a finally sees both a throwing assignment's old value and its new one",
			protects: "a finally is entered normally with the assignment's value and exceptionally, through its Handler, with the value from before it",
			mutation: "carry a node's own definition along its exceptional edge (h(x) loses x = 1), or land a MayThrow source on the finally's first node directly (there is no Handler, x = g() controls nothing and h(x) loses x = 1)",
			src:      "function f() { let x = 1; try { x = g(); } finally { h(x); } }",
			fn:       1,
			cd:       []string{"x = g()@32 -> finally@43"},
			du:       []string{"x = 1@19 -> h(x)@53", "x = g()@32 -> h(x)@53"},
		},
		{
			name:     "an assignment embedded in a loop condition flows into the condition",
			protects: "the node evaluating an expression that embeds `m = e` reads the m it defined, and the loop's back edge re-enters that assignment",
			mutation: "drop the read-back of the variable an embedded assignment defined (the condition loses m = re.exec(s)@35)",
			src:      "function f(re, s) { let m; while ((m = re.exec(s)) !== null) g(m); }",
			fn:       1,
			cd: []string{
				"(m = re.exec(s)) !== null@34 -> (m = re.exec(s)) !== null@34",
				"(m = re.exec(s)) !== null@34 -> m = re.exec(s)@35",
				"(m = re.exec(s)) !== null@34 -> g(m)@61",
			},
			du: []string{
				"re@11 -> m = re.exec(s)@35", "s@15 -> m = re.exec(s)@35",
				"re@11 -> (m = re.exec(s)) !== null@34", "s@15 -> (m = re.exec(s)) !== null@34",
				"m = re.exec(s)@35 -> (m = re.exec(s)) !== null@34", "m = re.exec(s)@35 -> g(m)@61",
			},
		},
		{
			name:     "an assignment embedded in an initializer flows into the declared name",
			protects: "a declaration whose initializer embeds `x = e` uses the x it defined",
			mutation: "drop the read-back of the variable an embedded assignment defined (y's definition uses nothing)",
			src:      "function f(x) { const y = (x = g()) + 1; return y; }",
			fn:       1,
			du:       []string{"x = g()@27 -> y = (x = g()) + 1@22", "y = (x = g()) + 1@22 -> return y;@41"},
		},
		{
			name:     "a for…of over a bare identifier defines it only on the body path",
			protects: "the loop's exit edge carries the definition from before the loop, since an empty iterable assigns nothing",
			mutation: "define the identifier on the loop head (return last loses last = null@22)",
			src:      "function f(arr) { let last = null; for (last of arr) {} return last; }",
			fn:       1,
			cd:       []string{"last@40 -> last@40", "last@40 -> last@40"},
			du: []string{
				"arr@11 -> arr@48", "arr@11 -> last@40", "arr@11 -> last@40",
				"last = null@22 -> return last;@56", "last@40 -> return last;@56",
			},
		},
		{
			name:     "a property write whose right side branches is its own throwing node",
			protects: "a property write is one node after its right side, and only it throws for the store, so the right side's decision does not control the catch",
			mutation: "count the store's throw before the right side (c@31 gains control of catch@43, e@50 and h()@55), or skip the write node when the right side made a node (the write node and its pairs vanish)",
			src:      "function f(o, c) { try { o.p = c ? 1 : 2 } catch (e) { h() } }",
			fn:       1,
			cd: []string{
				"c@31 -> 1@35", "c@31 -> 2@39",
				"o.p = c ? 1 : 2@25 -> catch@43", "o.p = c ? 1 : 2@25 -> e@50", "o.p = c ? 1 : 2@25 -> h()@55",
			},
			du: []string{
				"o@11 -> c@31", "c@14 -> c@31",
				"o@11 -> o.p = c ? 1 : 2@25", "c@14 -> o.p = c ? 1 : 2@25",
			},
		},
		{
			name:     "a hoisted function declaration captures at its own position",
			protects: "the hoisted name is defined at the block's start, and the declaration's captures are read where it stands, so a definition before it flows into later uses of the name",
			mutation: "read the captures on the hoisted node at the block's start (b = a@22 flows nowhere)",
			src:      "function f(a) { const b = a; function g() { return b } return g; }",
			fn:       1,
			du: []string{
				"a@11 -> b = a@22", "b = a@22 -> function g() { return b }@29",
				"g@38 -> return g;@55", "function g() { return b }@29 -> return g;@55",
			},
		},
		{
			name:     "a closure's write to an enclosing variable is a may-definition where it is created",
			protects: "a use after a closure's creation sees both the closure's write and the definition reaching the creation (the xs pairs to the arrow come from the receiver read pending when it is created)",
			mutation: "record a closure's write as a use only (x => { n += x }@39 loses its pairs to the call and to return n), or as a killing definition (return n loses n = 0@21)",
			src:      "function f(xs) { let n = 0; xs.forEach(x => { n += x }); return n; }",
			fn:       1,
			du: []string{
				"xs@11 -> x => { n += x }@39", "xs@11 -> xs.forEach(x => { n += x })@28",
				"n = 0@21 -> x => { n += x }@39", "n = 0@21 -> xs.forEach(x => { n += x })@28",
				"x => { n += x }@39 -> xs.forEach(x => { n += x })@28",
				"n = 0@21 -> return n;@57", "x => { n += x }@39 -> return n;@57",
			},
		},
	})
}
