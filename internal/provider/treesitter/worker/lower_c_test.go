package worker

import "testing"

// TestCLoweringGolden pins the C lowering's control-dependence and def-use
// pairs on hand-derived functions. A wrong pair here is a wrong dependence
// fact served to every consumer. Offsets are byte offsets into src. The
// derivation rules are TestGoLoweringGolden's: exit augmentation, the
// post-dominance frontier with nothing depending on ENTRY, every φ resolved,
// a may-definition killing nothing, and a node with no path from ENTRY
// reaching a use with no definition.
func TestCLoweringGolden(t *testing.T) {
	runGolden(t, "c", []goldenCase{
		{
			// C17 §6.8.6.1 (goto), §6.8.1 (labels). Nodes: x@11, L@16 (the
			// label's own node), x++@19, x < 9@28, goto L;@35, goto E;@43,
			// g()@51 (no predecessor), E@56, h(x)@59. Succ: L→x++→x < 9→
			// {goto L, goto E}; goto L→L (backward); goto E→E (forward);
			// g()→E→h(x)→EXIT. IPDom: x < 9 → goto E (every path leaves by
			// it); goto L → L → x++ → x < 9. Frontier walk from x < 9 over
			// goto L: goto L, L, x++, x < 9. The only definition reaching
			// h(x) is x++: g() has no path from ENTRY.
			name:     "goto backward to a label and forward over dead code",
			protects: "a goto lands on its label's own node whether the label is before or after it, and code skipped by a goto is not on any path",
			mutation: "resolve goto only to labels declared before it (goto E dangles, unresolved 1), or let a goto fall through to the next statement (g()@51 becomes reachable and x < 9 gains it)",
			src:      "void f(int x) { L: x++; if (x < 9) goto L; goto E; g(); E: h(x); }",
			cd:       []string{"x < 9@28 -> goto L;@35", "x < 9@28 -> L@16", "x < 9@28 -> x++@19", "x < 9@28 -> x < 9@28"},
			du:       []string{"x@11 -> x++@19", "x++@19 -> x++@19", "x++@19 -> x < 9@28", "x++@19 -> h(x)@59"},
		},
		{
			// C17 §6.8.4.2: the labels are tested against the controlling
			// expression, default taken when none matches, and a body falls
			// into the next one. Nodes: x@10, y = 0@19, x@34 (head), 1@60,
			// 2@82 (case tests, each Using x), y = 1@48 (default body),
			// y = 2@63, break;@70, return y;@85, return y;@97. Succ: x@34→1→
			// {y = 2, 2}; 2→{return y@85, y = 1}; y = 1→y = 2→break→
			// return y@97→EXIT; return y@85→EXIT. IPDom: 1, 2 → EXIT; y = 1 →
			// y = 2 → break → return y@97. y = 2 kills y = 0 and y = 1.
			name:     "switch with default first and fallthrough into a case",
			protects: "default is taken only when no label matches, whatever its position, and its body falls into the next case body",
			mutation: "test default in source order (x@34 reaches y = 1 before any test), or end every body with a break (y = 1 flows out of the switch and pairs with return y@97)",
			src:      "int f(int x) { int y = 0; switch (x) { default: y = 1; case 1: y = 2; break; case 2: return y; } return y; }",
			cd: []string{"1@60 -> y = 2@63", "1@60 -> break;@70", "1@60 -> return y;@97", "1@60 -> 2@82",
				"2@82 -> return y;@85", "2@82 -> y = 1@48", "2@82 -> y = 2@63", "2@82 -> break;@70", "2@82 -> return y;@97"},
			du: []string{"x@10 -> x@34", "x@10 -> 1@60", "x@10 -> 2@82", "y = 0@19 -> return y;@85",
				"y = 2@63 -> return y;@97"},
		},
		{
			// C17 §6.8.4.2p3 (a case label anywhere in the switch body) and
			// §6.8.5.2 (do). Nodes: n@11, n@24 (head), 0@34, 1@52 (the nested
			// label's test), g()@42, case@47 (the nested label's own node),
			// h()@55, --n@69, --n > 0@69. Succ: n@24→0→{g(), 1}; 1→{case@47,
			// EXIT (no match leaves the switch)}; g()→case→h()→--n→--n > 0→
			// {g() (back edge to the body's first node), EXIT}. IPDom: 0, 1,
			// --n > 0 → EXIT; g() → case → h() → --n → --n > 0.
			name:     "a case label nested in a loop of the switch body",
			protects: "a case label below another statement of its switch is entered from its test, into the middle of the loop, and the loop's back edge targets the body's start",
			mutation: "ignore nested case labels (1@52 and its dependences vanish), or target the do loop's back edge at its condition (--n > 0 loses control of g()@42)",
			src:      "void f(int n) { switch (n) { case 0: do { g(); case 1: h(); } while (--n > 0); } }",
			cd: []string{"0@34 -> g()@42", "0@34 -> case@47", "0@34 -> h()@55", "0@34 -> --n@69", "0@34 -> --n > 0@69",
				"0@34 -> 1@52", "1@52 -> case@47", "1@52 -> h()@55", "1@52 -> --n@69", "1@52 -> --n > 0@69",
				"--n > 0@69 -> g()@42", "--n > 0@69 -> case@47", "--n > 0@69 -> h()@55", "--n > 0@69 -> --n@69",
				"--n > 0@69 -> --n > 0@69"},
			du: []string{"n@11 -> n@24", "n@11 -> 0@34", "n@11 -> 1@52", "n@11 -> --n@69", "--n@69 -> --n@69",
				"--n@69 -> --n > 0@69"},
		},
		{
			// C17 §6.8.5p4: the loop runs while the controlling expression is
			// nonzero, so `while (1)` leaves only by break. Nodes: x@11,
			// 1@23 (a Stmt head with no exit edge), x@32, break;@35, g()@42,
			// h()@49. Succ: 1→x@32→{break, g()}; break→h(); g()→1. IPDom:
			// x@32 → break; g() → 1 → x@32.
			name:     "a loop on a nonzero literal leaves only by break",
			protects: "`while (1)` has no exit edge, so nothing in the loop depends on the literal and the break's condition controls the loop",
			mutation: "lower the literal as a Branch head with an exit edge (1@23 gains control of x@32, break;@35 and g()@42)",
			src:      "void f(int x) { while (1) { if (x) break; g(); } h(); }",
			cd:       []string{"x@32 -> g()@42", "x@32 -> 1@23", "x@32 -> x@32"},
			du:       []string{"x@11 -> x@32"},
		},
		{
			// C17 §6.10.1: which group is kept is decided at translation time,
			// so every arm stays reachable. Nodes: x@10, A@22 (Branch), y =
			// x@30, #else@37 (one successor), y = 2@49, return y;@65. Succ:
			// A→{y = x, #else}; #else→y = 2; both → return y. IPDom: A →
			// return y. Both declarations are one variable, so each arm's
			// definition reaches the use after the directive.
			name:     "a preprocessor conditional keeps both arms",
			protects: "each arm of #ifdef/#else is a path from the directive, merged after it, and a name each arm declares is one variable",
			mutation: "lower only the first arm (y = 2@49 vanishes), give a sibling arm's redeclaration a fresh variable (loses y = x@30 -> return y;@65), or merge the #else directive past its arm (A@22 loses control of y = 2@49)",
			src:      "int f(int x) {\n#ifdef A\n  int y = x;\n#else\n  int y = 2;\n#endif\n  return y;\n}",
			cd:       []string{"A@22 -> y = x@30", "A@22 -> #else@37", "A@22 -> y = 2@49"},
			du:       []string{"x@10 -> y = x@30", "y = x@30 -> return y;@65", "y = 2@49 -> return y;@65"},
		},
		{
			// Structured exceptions: `__leave` ends the `__try` body, the
			// `__finally` block runs on every exit, and a call inside the body
			// may raise. Nodes: x@11, x@28, __leave;@31, g()@40, the Handler
			// __finally@47 (g() raising), h(x)@59, k()@67. Succ: x@28→
			// {__leave, g()}; __leave→h(x); g()→{h(x), __finally}; __finally→
			// h(x); h(x)→{k() (normal), EXIT (the re-raised exception)};
			// k()→EXIT. IPDom: x@28, g(), __finally → h(x) → EXIT.
			name:     "__try/__finally with __leave",
			protects: "__leave reaches the __finally body, not the statement after it, and a raising call reaches the __finally through its Handler and leaves the function after it",
			mutation: "lower __leave as a jump past the __finally (h(x)@59 loses its edge from __leave;@31 and x@28 gains control of h(x)), or count no throw for calls in a __try body (__finally@47 and h(x) -> k() vanish)",
			src:      "void f(int x) { __try { if (x) __leave; g(); } __finally { h(x); } k(); }",
			cd:       []string{"x@28 -> __leave;@31", "x@28 -> g()@40", "g()@40 -> __finally@47", "h(x)@59 -> k()@67"},
			du:       []string{"x@11 -> x@28", "x@11 -> h(x)@59"},
		},
		{
			// Structured exceptions: a dereference in a `__try` body may raise;
			// the `__except` filter decides whether the handler runs,
			// otherwise the exception propagates. Nodes: p@11, r = 0@20,
			// r = *p@35, the Handler __except@45, g()@55 (the filter, a
			// Branch), r = 1@62, return r;@71. Succ: r = *p→{__except, return
			// r}; __except→g()→{r = 1, EXIT}; r = 1→return r. IPDom: r = *p,
			// g() → EXIT; __except → g(). The Handler carries r's value from
			// before r = *p, which r = 1 kills.
			name:     "__try/__except with a filter",
			protects: "a dereference in a __try body reaches the handler, whose filter controls it, and a filter that declines propagates to the caller",
			mutation: "count no throw for a dereference (the __except path is unreachable: r = *p@35 controls nothing), or let the declined filter fall through to return r (g()@55 no longer controls return r;@71)",
			src:      "int f(int *p) { int r = 0; __try { r = *p; } __except (g()) { r = 1; } return r; }",
			cd: []string{"r = *p@35 -> __except@45", "r = *p@35 -> g()@55", "r = *p@35 -> return r;@71",
				"g()@55 -> r = 1@62", "g()@55 -> return r;@71"},
			du: []string{"p@11 -> r = *p@35", "r = *p@35 -> return r;@71", "r = 1@62 -> return r;@71"},
		},
		{
			// C17 §6.5.3.2: `*p = v` writes the object p points to. Nodes:
			// p@11, v@18, *p = v@23 (Uses p and v, may-defines p), return
			// *p;@31.
			name:     "a write through a pointer updates its base without killing it",
			protects: "a store through *p reaches a later read through p, and so does p's earlier definition",
			mutation: "record no definition of the base (loses *p = v@23 -> return *p;@31), or a killing one (loses p@11 -> return *p;@31)",
			src:      "int f(int *p, int v) { *p = v; return *p; }",
			du:       []string{"p@11 -> *p = v@23", "v@18 -> *p = v@23", "*p = v@23 -> return *p;@31", "p@11 -> return *p;@31"},
		},
		{
			// C17 §6.5.16p3: an assignment's value is the value stored. Nodes:
			// c@10, c = g()@23 (the loop head's first node), (c = g()) !=
			// 0@22, h(c)@38, return c;@44. Succ: c = g()→(c = g()) != 0→
			// {h(c), return c}; h(c)→c = g(). IPDom: h(c) → c = g() →
			// (c = g()) != 0 → return c. c = g() kills the parameter.
			name:     "an assignment embedded in a loop condition flows into the condition",
			protects: "the node evaluating an expression that embeds `c = e` reads the c it defined, and the back edge re-enters the assignment",
			mutation: "drop the read-back of the variable an embedded assignment defined (loses c = g()@23 -> (c = g()) != 0@22)",
			src:      "int f(int c) { while ((c = g()) != 0) h(c); return c; }",
			cd: []string{"(c = g()) != 0@22 -> h(c)@38", "(c = g()) != 0@22 -> c = g()@23",
				"(c = g()) != 0@22 -> (c = g()) != 0@22"},
			du: []string{"c = g()@23 -> (c = g()) != 0@22", "c = g()@23 -> h(c)@38", "c = g()@23 -> return c;@44"},
		},
		{
			// C17 §6.2.1p4: a block's declaration hides the outer one until
			// the block ends. Nodes: x@10, x = 1@21, g(x)@28, return x;@36.
			name:     "a block-scope declaration shadows a parameter only inside its block",
			protects: "the inner x is a new variable whose scope ends with its block",
			mutation: "keep the block's bindings after it closes (return x;@36 pairs with x = 1@21 instead of x@10)",
			src:      "int f(int x) { { int x = 1; g(x); } return x; }",
			du:       []string{"x = 1@21 -> g(x)@28", "x@10 -> return x;@36"},
		},
		{
			// C17 §6.2.1p7: an identifier's scope begins just after its
			// declarator, so the initializer reads the new, uninitialized x.
			// Nodes: x@10, x = x + 1@21, return x;@32.
			name:     "an initializer reads the variable it declares",
			protects: "a declaration's name is in scope in its own initializer, so the outer x does not flow into it",
			mutation: "declare the name after lowering the initializer (adds x@10 -> x = x + 1@21)",
			src:      "int f(int x) { { int x = x + 1; return x; } }",
			du:       []string{"x = x + 1@21 -> return x;@32"},
		},
	})
}
