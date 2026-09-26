package worker

import "testing"

// TestCLoweringGolden pins the C lowering's control-dependence and def-use
// pairs on hand-derived functions. A wrong pair here is a wrong dependence
// fact served to every consumer. Offsets are byte offsets into src. The
// derivation and rendering rules are runGolden's.
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
			// into the next one; the controlling expression is evaluated once.
			// Nodes: x@10, y = 0@19, x@34 (head, Uses x and defines the owned
			// switch value), 1@60, 2@82 (case tests, each Using the switch
			// value only), y = 1@48 (default body),
			// y = 2@63, break;@70, return y;@85, return y;@97. Succ: x@34→1→
			// {y = 2, 2}; 2→{return y@85, y = 1}; y = 1→y = 2→break→
			// return y@97→EXIT; return y@85→EXIT. IPDom: 1, 2 → EXIT; y = 1 →
			// y = 2 → break → return y@97. y = 2 kills y = 0 and y = 1.
			name:     "switch with default first and fallthrough into a case",
			protects: "default is taken only when no label matches, whatever its position, and its body falls into the next case body",
			mutation: "test default in source order (x@34 reaches y = 1 before any test), end every body with a break (y = 1 flows out of the switch and pairs with return y@97), or let the case tests re-read the controlling expression's names (x@34 -> 1@60 and x@34 -> 2@82 become x@10 -> 1@60 and x@10 -> 2@82)",
			src:      "int f(int x) { int y = 0; switch (x) { default: y = 1; case 1: y = 2; break; case 2: return y; } return y; }",
			cd: []string{"1@60 -> y = 2@63", "1@60 -> break;@70", "1@60 -> return y;@97", "1@60 -> 2@82",
				"2@82 -> return y;@85", "2@82 -> y = 1@48", "2@82 -> y = 2@63", "2@82 -> break;@70", "2@82 -> return y;@97"},
			du: []string{"x@10 -> x@34", "x@34 -> 1@60", "x@34 -> 2@82", "y = 0@19 -> return y;@85",
				"y = 2@63 -> return y;@97"},
		},
		{
			// C17 §6.8.4.2p4 (a case label anywhere in the switch body) and
			// §6.8.5.2 (do). Nodes: n@11, n@24 (head, defining the owned
			// switch value), 0@34, 1@52 (the nested label's test; both tests
			// Use the switch value), g()@42, case@47 (the nested label's own
			// node), h()@55, --n@69 (defining n and its result), --n > 0@69
			// (Using the result). Succ: n@24→0→{g(), 1}; 1→{case@47,
			// EXIT (no match leaves the switch)}; g()→case→h()→--n→--n > 0→
			// {g() (back edge to the body's first node), EXIT}. IPDom: 0, 1,
			// --n > 0 → EXIT; g() → case → h() → --n → --n > 0.
			name:     "a case label nested in a loop of the switch body",
			protects: "a case label below another statement of its switch is entered from its test, into the middle of the loop, and the loop's back edge targets the body's start",
			mutation: "ignore nested case labels (1@52 and its dependences vanish), target the do loop's back edge at its condition (--n > 0 loses control of g()@42), or let a nested test re-read the controlling expression's names (n@24 -> 1@52 becomes n@11 -> 1@52)",
			src:      "void f(int n) { switch (n) { case 0: do { g(); case 1: h(); } while (--n > 0); } }",
			cd: []string{"0@34 -> g()@42", "0@34 -> case@47", "0@34 -> h()@55", "0@34 -> --n@69", "0@34 -> --n > 0@69",
				"0@34 -> 1@52", "1@52 -> case@47", "1@52 -> h()@55", "1@52 -> --n@69", "1@52 -> --n > 0@69",
				"--n > 0@69 -> g()@42", "--n > 0@69 -> case@47", "--n > 0@69 -> h()@55", "--n > 0@69 -> --n@69",
				"--n > 0@69 -> --n > 0@69"},
			du: []string{"n@11 -> n@24", "n@24 -> 0@34", "n@24 -> 1@52", "n@11 -> --n@69", "--n@69 -> --n@69",
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
			// Structured exception handling, try-finally statement (the
			// extension's documented semantics): `__leave` ends the `__try` body, the `__finally` block runs on
			// every exit, and a call inside the body may raise. Nodes: x@11,
			// x@28, __leave;@31, g()@40, the Handler __finally@47 (g()
			// raising), h(x)@59, k()@67. Succ: x@28→{__leave, g()};
			// __leave→h(x); g()→{h(x), __finally}; __finally→h(x);
			// h(x)→{k() (normal), EXIT (the re-raised exception)};
			// k()→EXIT. IPDom: x@28, g(), __finally → h(x) → EXIT.
			name:     "__try/__finally with __leave",
			protects: "__leave reaches the __finally body, not the statement after it, and a raising call reaches the __finally through its Handler and leaves the function after it",
			mutation: "lower __leave as a jump past the __finally (h(x)@59 loses its edge from __leave;@31 and x@28 gains control of h(x)), or count no throw for calls in a __try body (__finally@47 and h(x) -> k() vanish)",
			src:      "void f(int x) { __try { if (x) __leave; g(); } __finally { h(x); } k(); }",
			cd:       []string{"x@28 -> __leave;@31", "x@28 -> g()@40", "g()@40 -> __finally@47", "h(x)@59 -> k()@67"},
			du:       []string{"x@11 -> x@28", "x@11 -> h(x)@59"},
		},
		{
			// Structured exception handling, try-except statement (the
			// extension's documented semantics): a dereference in a `__try` body may raise; the `__except` filter
			// decides whether the handler runs, otherwise the exception
			// propagates. Nodes: p@11, r = 0@20, r = *p@35, the Handler
			// __except@45, g()@55 (the filter, a Branch), r = 1@62, return r;@71. Succ: r = *p→{__except, return
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
			// *p;@31, which pairs with the nearest may-definition and with
			// the killing p@11 that reaches it through it.
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
		{
			// C17 §6.5.3.2p3: `&x` yields a pointer to x, through which the
			// callee may write it. Nodes: x = 1@14 (defines x), g(&x)@21
			// (Uses x, may-defines x), return x;@28. The return's x pairs
			// with the nearest may-definition g(&x) and with the killing
			// x = 1 that reaches it through it.
			name:     "taking an address is a may-definition of the variable",
			protects: "a use after a call that received a variable's address sees the write the call may make through it",
			mutation: "lower `&x` as a plain read (g(&x)@21 -> return x;@28 vanishes), or make it a killing Def (x = 1@14 -> return x;@28 vanishes)",
			src:      "int f() { int x = 1; g(&x); return x; }",
			du:       []string{"x = 1@14 -> g(&x)@21", "x = 1@14 -> return x;@28", "g(&x)@21 -> return x;@28"},
		},
		{
			// C17 §6.5.3.2p3 and §6.5.2.2p10 (the arguments are evaluated
			// before the call): the call is the node that passes &x. Nodes:
			// i@10, x = 1@19, i++@32 (made first, defining i), g(&x, i++)@26
			// (Uses x and the i that i++ defined, may-defines x), return
			// x;@38.
			name:     "an address-taking may-definition lands on the call even when a later argument makes a node first",
			protects: "the may-definition of &x is carried by the node of the expression that evaluates it, not by whichever node the lowering creates next",
			mutation: "attach pending may-definitions to the next node made (adds i++@32 -> return x;@38, loses g(&x, i++)@26 -> return x;@38)",
			src:      "int f(int i) { int x = 1; g(&x, i++); return x; }",
			du: []string{"i@10 -> i++@32", "i++@32 -> g(&x, i++)@26", "x = 1@19 -> g(&x, i++)@26",
				"x = 1@19 -> return x;@38", "g(&x, i++)@26 -> return x;@38"},
		},
		{
			// C17 §6.3.2.1p3: an array evaluated other than as the operand of
			// sizeof or & is converted to a pointer to its first element, so
			// fill may write buf through it; a subscript reads an element.
			// Nodes: `char buf[8];` makes none, fill(buf)@24 (Uses buf,
			// may-defines it), use(buf[0])@35 (Uses buf), g(buf)@48.
			name:     "an array passed to a call decays to its address and is may-defined there",
			protects: "a use of an array after a call that received it sees the write the call may make, while a subscript read writes nothing",
			mutation: "read a decaying array without the may-definition (loses both pairs from fill(buf)@24), or let a subscript operand decay too (adds use(buf[0])@35 -> g(buf)@48)",
			src:      "void f() { char buf[8]; fill(buf); use(buf[0]); g(buf); }",
			du:       []string{"fill(buf)@24 -> use(buf[0])@35", "fill(buf)@24 -> g(buf)@48"},
		},
		{
			// Structured exception handling, try-finally statement (the
			// extension's documented semantics): the `__finally` block runs however the `__try` body is left, a
			// goto out of it included. Nodes: x@10, r = 0@19, x@38, goto
			// E;@41, r = 1@49, g()@70, E@77, return r;@80. Succ: x@38→{goto
			// E, r = 1}; goto E→g() (the finally); r = 1→g(); g()→E→return r.
			// IPDom: x@38 → g().
			name:     "a goto out of a __try body runs its __finally first",
			protects: "a goto whose label lies outside the __try passes through the __finally, so the finally post-dominates both ways out of the body",
			mutation: "resolve the goto straight to its label (x@38 gains control of g()@70)",
			src:      "int f(int x) { int r = 0; __try { if (x) goto E; r = 1; } __finally { g(); } E: return r; }",
			cd:       []string{"x@38 -> goto E;@41", "x@38 -> r = 1@49"},
			du:       []string{"x@10 -> x@38", "r = 0@19 -> return r;@80", "r = 1@49 -> return r;@80"},
		},
		{
			// C17 §6.10.1p6: only one group of a conditional is kept, so a
			// declaration in the #ifdef group does not exist in the #else
			// group, whose x is the parameter. Nodes: x@10, A@24, x = 1@32,
			// g(x)@41, #else@47, g(x)@55, return x;@70. After the directive x
			// names the #ifdef group's variable in the build that keeps that
			// group and the parameter in the build that keeps #else, which
			// does not rebind x: return x Uses both.
			name:     "a preprocessor arm does not see a sibling arm's declarations",
			protects: "each arm of a conditional resolves names against the scope before the directive, so the #else arm's x is the parameter, and after the directive x stands for every binding it has in some build",
			mutation: "keep the #ifdef arm's bindings while lowering the #else arm (loses x@10 -> g(x)@55), or bind a name after the directive to the arms' variable alone (loses x@10 -> return x;@70)",
			src:      "int f(int x) { {\n#ifdef A\n  int x = 1;\n  g(x);\n#else\n  g(x);\n#endif\n  return x; } }",
			cd:       []string{"A@24 -> x = 1@32", "A@24 -> g(x)@41", "A@24 -> #else@47", "A@24 -> g(x)@55"},
			du:       []string{"x@10 -> g(x)@55", "x = 1@32 -> g(x)@41", "x = 1@32 -> return x;@70", "x@10 -> return x;@70"},
		},
		{
			// GNU C extension, Statements and Declarations in Expressions:
			// jumping out of a statement expression is permitted, so a break
			// in a for loop's third expression ends that loop (C17 §6.8.5.3,
			// §6.8.6.3). The update's value is discarded: its last statement's
			// node j++ yields the statement expression's result, which no
			// node reads, so the update makes no node of its own. Nodes:
			// n@10, j = 0@27, j < n@34, g(j)@71, j > 5@48, break;@55,
			// j++@62, return j;@77. Succ: j < n→{g(j), return j};
			// g(j)→j > 5→{break, j++}; break→return j; j++→j < n. IPDom:
			// j < n, j > 5 → return j; g(j) → j > 5; j++ → j < n.
			name:     "a break in a for update's statement expression leaves that loop",
			protects: "a break lowered in the update clause targets the loop whose update holds it and lands after the loop",
			mutation: "send the update's break to the loop head instead of past the loop (j > 5@48 loses control of j < n@34), or end the discarded update with a node Using its result (adds ({ if (j > 5) break; j++; })@41, with j > 5@48 -> it and j++@62 -> it)",
			src:      "int f(int n) { int j; for (j = 0; j < n; ({ if (j > 5) break; j++; })) g(j); return j; }",
			cd: []string{"j < n@34 -> g(j)@71", "j < n@34 -> j > 5@48", "j > 5@48 -> break;@55", "j > 5@48 -> j++@62",
				"j > 5@48 -> j < n@34"},
			du: []string{"n@10 -> j < n@34", "j = 0@27 -> j < n@34", "j = 0@27 -> g(j)@71", "j = 0@27 -> j > 5@48",
				"j = 0@27 -> j++@62", "j = 0@27 -> return j;@77",
				"j++@62 -> j < n@34", "j++@62 -> g(j)@71", "j++@62 -> j > 5@48", "j++@62 -> j++@62",
				"j++@62 -> return j;@77"},
		},
		{
			// C17 §6.8.6.2p2: a continue jumps to the end of the loop body,
			// after which a do loop evaluates its condition again; GNU C
			// extension, Statements and Declarations in Expressions: the jump
			// out of the condition is permitted. The condition strips the do's
			// parentheses and the statement expression's own. Nodes: n@10,
			// n--@18, n & 1@37, continue;@44, n > 0@54, the condition
			// { … }@31 (a Branch Using the statement expression's result,
			// which n > 0@54, the last statement's node, defines), return
			// n;@66. Succ: n--→n & 1→{continue, n > 0}; continue→
			// n & 1; n > 0→{ … }→{n--, return n}. IPDom: n & 1 → n > 0;
			// { … } → return n.
			name:     "a continue in a do loop's condition re-evaluates the condition",
			protects: "a continue issued on the loop's continue path lands on the first node of the condition instead of panicking or re-entering the body",
			mutation: "land the continue on the loop head (continue;@44 -> n--@18: n & 1@37 gains control of n--@18), call ContinueHere only once (CloseFrame panics), or let the condition re-read its last statement's names (n > 0@54 -> { … }@31 becomes n--@18 -> { … }@31)",
			src:      "int f(int n) { do n--; while (({ if (n & 1) continue; n > 0; })); return n; }",
			cd: []string{"n & 1@37 -> continue;@44", "n & 1@37 -> n & 1@37",
				"{ if (n & 1) continue; n > 0; }@31 -> n--@18", "{ if (n & 1) continue; n > 0; }@31 -> n & 1@37",
				"{ if (n & 1) continue; n > 0; }@31 -> n > 0@54",
				"{ if (n & 1) continue; n > 0; }@31 -> { if (n & 1) continue; n > 0; }@31"},
			du: []string{"n@10 -> n--@18", "n--@18 -> n--@18", "n--@18 -> n & 1@37", "n--@18 -> n > 0@54",
				"n > 0@54 -> { if (n & 1) continue; n > 0; }@31", "n--@18 -> return n;@66"},
		},
		{
			// C17 §6.5.13p4 (&& evaluates its right operand only when the left
			// is nonzero), §6.5.15p4 (only the selected operand of ?: is
			// evaluated), §6.5.17p2 (the comma operator's left operand is
			// evaluated first). Nodes: a@10, b@17, a@30 (Branch, Uses a,
			// defines the && result), g(b)@35 (Uses b, defines the result on
			// the path that evaluates it), r = a && g(b)@26 (Uses the
			// result), r@45 (Branch, Uses r), a@49, b@53 (arm nodes, each
			// defining the ?: result), r = r ? a : b@41 (Uses that result),
			// a = 1@56, b = 2@63, return a + b + r;@70. Succ: a@30→{g(b),
			// r = a && g(b)}; r@45→{a@49, b@53}→r = r ? a : b.
			name:     "&&, ?: and the comma operator in C",
			protects: "a conditionally evaluated operand depends on the operand that decides it, the value of && and ?: reaches its consumer from the nodes that yield it and not by re-reading their names, and each comma operand is its own defining statement",
			mutation: "lower && as a plain binary (a@30 and its control of g(b)@35 vanish), evaluate both ?: operands unconditionally (r@45 loses control of a@49 and b@53), let the consumer re-read the operands' names instead of the result (a@10 and b@17 pair with r = a && g(b)@26 and r = r ? a : b@41, and the pairs from a@30, g(b)@35, a@49 and b@53 to them vanish), or lower a comma statement as one node (a = 1@56 and b = 2@63 are replaced by one node spanning both)",
			src:      "int f(int a, int b) { int r = a && g(b); r = r ? a : b; a = 1, b = 2; return a + b + r; }",
			cd:       []string{"a@30 -> g(b)@35", "r@45 -> a@49", "r@45 -> b@53"},
			du: []string{"a@10 -> a@30", "b@17 -> g(b)@35", "a@30 -> r = a && g(b)@26", "g(b)@35 -> r = a && g(b)@26",
				"r = a && g(b)@26 -> r@45", "a@10 -> a@49", "b@17 -> b@53", "a@49 -> r = r ? a : b@41",
				"b@53 -> r = r ? a : b@41", "a = 1@56 -> return a + b + r;@70",
				"b = 2@63 -> return a + b + r;@70", "r = r ? a : b@41 -> return a + b + r;@70"},
		},
		{
			// GNU C extension, Extended Asm, Goto Labels: control may
			// continue at the next statement or at any listed label. Nodes:
			// x@10, the Branch asm goto("" : : "r"(x) : : L)@15 (Uses x),
			// x = 1@46, L@53, return x;@56. Succ: asm→{x = 1, L}; x = 1→L.
			name:     "an assembly goto branches to its labels and to the next statement",
			protects: "an asm goto is a decision with an edge to each listed label, so code it can skip depends on it and the value from before it reaches the label",
			mutation: "give the asm statement no edge to its labels (asm loses control of x = 1@46, and x@10 -> return x;@56 vanishes)",
			src:      "int f(int x) { asm goto(\"\" : : \"r\"(x) : : L); x = 1; L: return x; }",
			cd:       []string{"asm goto(\"\" : : \"r\"(x) : : L)@15 -> x = 1@46"},
			du: []string{"x@10 -> asm goto(\"\" : : \"r\"(x) : : L)@15", "x@10 -> return x;@56",
				"x = 1@46 -> return x;@56"},
		},
		{
			// GNU C extension, Statements and Declarations in Expressions: the
			// last thing in the compound statement is an expression followed
			// by a semicolon, whose value is the value of the construct; the
			// statements before it are evaluated for their effect. Nodes:
			// y@10, z@17, g(y)@33 (Uses y), y = z@39 (Uses z, defines y and
			// the statement expression's result), x = ({ g(y);
			// y = z; })@26 (Uses the result, defines x), return x;@50. Succ:
			// a straight line to EXIT.
			name:     "a statement expression's value comes from its last statement's node",
			protects: "the node consuming a statement expression reaches its value through the last statement's node, and reads nothing an earlier statement read",
			mutation: "let the declarator re-read the statement expression's names (adds y@10 -> x = ({ g(y); y = z; })@26 and z@17 -> x = ({ g(y); y = z; })@26), or lower the last statement for its effect only (loses y = z@39 -> x = ({ g(y); y = z; })@26)",
			src:      "int f(int y, int z) { int x = ({ g(y); y = z; }); return x; }",
			du: []string{"y@10 -> g(y)@33", "z@17 -> y = z@39", "y = z@39 -> x = ({ g(y); y = z; })@26",
				"x = ({ g(y); y = z; })@26 -> return x;@50"},
		},
		{
			// GNU C extension, Statements and Declarations in Expressions
			// (the statements run in order and jumping into the construct is
			// not permitted), C17 §6.5.2.2 (the arguments are evaluated
			// before the call; the lowering takes them left to right). x = 1
			// is an expression statement of the construct's own list with no
			// label after it, so it runs on every path to the value and takes
			// the call's earlier read of x. Nodes: x@10, x = 1@30 (Uses x,
			// defines x and the owned variable holding the read), 0@37
			// (defines the construct's result), return g(x, ({ x = 1; 0;
			// }));@15 (Uses the owned variable and the result). Succ: a
			// straight line to EXIT.
			name:     "a statement expression's earlier statement on every path hands the consumer's read off",
			protects: "a read the consumer makes before a statement expression pairs with the definition that reached it, carried by an earlier statement's assignment that runs whenever the value does",
			mutation: "treat every statement before the last as skippable (loses x@10 -> x = 1@30: the call's read of x pairs through the name with the assignment after it)",
			src:      "int f(int x) { return g(x, ({ x = 1; 0; })); }",
			du: []string{"x@10 -> x = 1@30", "x = 1@30 -> return g(x, ({ x = 1; 0; }));@15",
				"0@37 -> return g(x, ({ x = 1; 0; }));@15"},
		},
		{
			// GNU C extension, Statements and Declarations in Expressions;
			// C17 §6.8.6.1 (a goto jumps to its label), §6.8.4.1 (the
			// substatement of if runs only when the condition is nonzero).
			// The goto to L can skip x = 1, and x = 2 runs only when c is
			// nonzero, so neither takes the call's earlier read of x, which
			// stays on the consumer. Nodes: x@10, c@17, c@41 (Branch), goto
			// L;@44, x = 1@52, L@59 (the label's node), c@67 (Branch), x =
			// 2@70, 0@77 (defines the construct's result), return g(x, ({ … }));@22
			// (Uses x and the result). Succ: c@41→{goto L, x = 1}; goto L→L;
			// x = 1→L→c@67→{x = 2, 0}; x = 2→0→return. IPDom: c@41 → L;
			// c@67 → 0.
			name:     "a statement expression's statement that a jump can skip leaves the consumer's read in place",
			protects: "a statement a goto can skip, or one nested in an if, hands on none of the consumer's earlier reads, so the read still pairs with the definitions reaching it on the paths that skip the assignment",
			mutation: "drop the label test (x = 1@52 takes the read: adds x@10 -> x = 1@52, loses x@10 -> return …@22 and x = 2@70 -> return …@22), or keep an if statement on the hand-off (x = 2@70 takes the read: adds x@10 -> x = 2@70 and x = 1@52 -> x = 2@70, loses x@10 -> return …@22 and x = 1@52 -> return …@22)",
			src:      "int f(int x, int c) { return g(x, ({ if (c) goto L; x = 1; L:; if (c) x = 2; 0; })); }",
			cd:       []string{"c@41 -> goto L;@44", "c@41 -> x = 1@52", "c@67 -> x = 2@70"},
			du: []string{"c@17 -> c@41", "c@17 -> c@67", "x@10 -> return g(x, ({ if (c) goto L; x = 1; L:; if (c) x = 2; 0; }));@22",
				"x = 1@52 -> return g(x, ({ if (c) goto L; x = 1; L:; if (c) x = 2; 0; }));@22",
				"x = 2@70 -> return g(x, ({ if (c) goto L; x = 1; L:; if (c) x = 2; 0; }));@22",
				"0@77 -> return g(x, ({ if (c) goto L; x = 1; L:; if (c) x = 2; 0; }));@22"},
		},
		{
			// C17 §6.5.16p3: an assignment expression has the value of the
			// left operand after the assignment. Nodes: s@15, a@22, s.f =
			// a@36 (Uses a and s, may-defines s, defines the owned result),
			// return g(s.f = a);@27 (Uses the result). Succ: a straight line
			// to EXIT.
			name:     "an embedded assignment to a field hands its value on through a result",
			protects: "an assignment to a field inside a larger expression is a node of its own whose value reaches the consumer, which does not re-read its operands",
			mutation: "fold a field-target assignment into its consumer (s.f = a@36 vanishes, and a@22 and s@15 pair with return g(s.f = a);@27 instead)",
			src:      "int f(struct S s, int a) { return g(s.f = a); }",
			du:       []string{"a@22 -> s.f = a@36", "s@15 -> s.f = a@36", "s.f = a@36 -> return g(s.f = a);@27"},
		},
		{
			// C17 §6.8.4.2p3 (a case label's constant expression) and
			// §6.5.1p5 (a parenthesized expression has the value of the
			// expression it encloses). Nodes: x@10, x@23 (the controlling
			// expression, evaluated once, defining the owned switch value),
			// 1@34 (the test, spanning the value without its parentheses and
			// Using the switch value), return 1;@38, return 0;@50. Succ:
			// x@23→1→{return 1, return 0}. IPDom: 1 → EXIT.
			name:     "a case label's value is spanned without its parentheses",
			protects: "a parenthesized case value renders as its expression and its test Uses the switch value evaluated once at the controlling expression's node",
			mutation: "span a case label's value with its parentheses (1@34 renders as (1)@33), or let the test re-read the controlling expression's names (x@23 -> 1@34 becomes x@10 -> 1@34)",
			src:      "int f(int x) { switch (x) { case (1): return 1; } return 0; }",
			cd:       []string{"1@34 -> return 1;@38", "1@34 -> return 0;@50"},
			du:       []string{"x@10 -> x@23", "x@23 -> 1@34"},
		},
		{
			// C17 §6.8.4.1p2 (the first substatement runs when the controlling
			// expression compares unequal to 0) and §6.5.16p3 (an assignment
			// has the value of its left operand after the assignment). Nodes:
			// x@10, x = g()@20 (the condition with both pairs of parentheses
			// stripped: one Branch node that defines x and branches on the
			// value), return x;@30, return 0;@40. Succ: x@10→x = g()→{return
			// x, return 0}. IPDom: x = g() → EXIT.
			name:     "an assignment that is the whole condition is the decision node",
			protects: "a condition that is only an assignment makes one Branch node defining its target, so the branch and the definition are one node",
			mutation: "lower the assignment as a node of its own before a Branch spanning the same text (adds x = g()@20 -> x = g()@20, the Branch Using the assignment's result)",
			src:      "int f(int x) { if ((x = g())) return x; return 0; }",
			cd:       []string{"x = g()@20 -> return x;@30", "x = g()@20 -> return 0;@40"},
			du:       []string{"x = g()@20 -> return x;@30"},
		},
		{
			// C17 §6.2.1p4: g, declared outside any block, has file scope, so
			// it is no variable of f. Nodes: y@18, g = y@23 (Uses y, defines
			// nothing), g += y@30 (Uses y only), g++@38 (Uses nothing), g =
			// y@45 (the embedded assignment: Uses y, defines the owned result),
			// h(g = y)@43 (Uses that result), h(&g)@53 (Uses and may-defines
			// nothing), return g;@60 (Uses nothing). Succ: a straight line to
			// EXIT.
			name:     "a name with file scope is no variable in any position",
			protects: "an assignment, compound assignment, update, embedded assignment, address-taking and read of a name that resolves to no variable make their nodes with their other operands' reads and never reach the builder as a variable",
			mutation: "drop read's filter (Builder.Use panics on g's -1 at g += y@30), def's (Builder.Def panics at g = y@23), or mayDef's (Builder.MayDef panics at h(&g)@53), or fold the embedded assignment into its consumer (loses g = y@45 -> h(g = y)@43)",
			src:      "int g; void f(int y) { g = y; g += y; g++; h(g = y); h(&g); return g; }",
			du:       []string{"y@18 -> g = y@23", "y@18 -> g += y@30", "y@18 -> g = y@45", "g = y@45 -> h(g = y)@43"},
		},
		{
			// C17 §6.10.1p6: a conditional without #else keeps no group when
			// A is undefined, so after the directive x is the #ifdef group's
			// variable in one build and the parameter in the other. Nodes:
			// x@10, A@24 (Branch), x = 1@32, g(x)@48 (Uses both variables),
			// x = 2@56 (a killing definition of both: whichever build is
			// taken, x names one of them and the write replaces it), return
			// x;@66 (after the block, the parameter, which x = 2 killed).
			// Succ: A→{x = 1, g(x)}; x = 1→g(x)→x = 2→return x. IPDom: A →
			// g(x).
			name:     "a name only one arm declares stands for both bindings after the directive",
			protects: "a read after a conditional whose other build keeps the enclosing binding sees both, and a write there kills both, so the enclosing variable's earlier value does not reach past it",
			mutation: "count a group without #else as having no empty arm, or bind the name to the arms' variable alone (loses x@10 -> g(x)@48 and x = 2@56 -> return x;@66, adds x@10 -> return x;@66), or let the write may-define the enclosing variable instead of killing it (adds x@10 -> x = 2@56 and x@10 -> return x;@66)",
			src:      "int f(int x) { {\n#ifdef A\n  int x = 1;\n#endif\n  g(x);\n  x = 2;\n } return x; }",
			cd:       []string{"A@24 -> x = 1@32"},
			du:       []string{"x@10 -> g(x)@48", "x = 1@32 -> g(x)@48", "x = 2@56 -> return x;@66"},
		},
		{
			// C17 §6.7.6.3 (a block-scope `int g(int);` declares a function,
			// not an object), §6.10.1. The #ifdef group binds g to no
			// variable; the #else group's declaration is still a variable of
			// its own, and after the directive g is that variable. Nodes:
			// x@10, A@24 (Branch), #else@40 (Branch), g = x@52 (Uses x,
			// defines g), return g;@68 (Uses g). Succ: A→{#else, return g}
			// (the #ifdef group makes no node); #else→g = x→return g. IPDom:
			// A → return g.
			name:     "a prototype in one arm leaves a sibling arm's declaration a variable",
			protects: "a name one arm declares as a function is no variable in that arm only, and a sibling arm's object declaration of the same name gets a variable whose definition reaches the code after the directive",
			mutation: "let a sibling arm's declaration reuse the parked binding even when it names no variable (g = x@52 defines nothing: loses g = x@52 -> return g;@68)",
			src:      "int f(int x) { {\n#ifdef A\n  int g(int);\n#else\n  int g = x;\n#endif\n  return g; } }",
			cd:       []string{"A@24 -> #else@40", "A@24 -> g = x@52"},
			du:       []string{"x@10 -> g = x@52", "g = x@52 -> return g;@68"},
		},
		{
			// C17 §6.3.2.1p3 (an array evaluated as a call argument decays to
			// its address), §6.10.1p6. x is an array in the build without A
			// and an int in the build with it, so h(x) passes the array's
			// address in one build. Nodes: A@33 (Branch), x = 1@41, h(x)@57
			// (Uses both variables, may-defines both: the name is an array in
			// some build), return x[0];@65 (after the block, the array; a
			// subscript's operand does not decay). `int x[2];` makes no node.
			// Succ: A→{x = 1, h(x)}; x = 1→h(x)→return. IPDom: A → h(x).
			name:     "a name that is an array in some build decays to its address after the directive",
			protects: "a call after a conditional that passes a name which is an array in one build may-defines that array, so a later read of the array depends on the call",
			mutation: "test only the arms' variable for an array shape (h(x)@57 no longer may-defines the enclosing array: loses h(x)@57 -> return x[0];@65)",
			src:      "int f(void) { int x[2]; {\n#ifdef A\n  int x = 1;\n#endif\n  h(x); } return x[0]; }",
			cd:       []string{"A@33 -> x = 1@41"},
			du:       []string{"x = 1@41 -> h(x)@57", "h(x)@57 -> return x[0];@65"},
		},
		{
			// C17 §6.10.1p6 (one group kept, so after the directive x is the
			// #ifdef group's variable or the parameter) and §6.5.2.2 (the
			// arguments are evaluated before the call; the lowering takes them
			// left to right). Nodes: x@10, A@24 (Branch), x = 1@32, x = 2@53
			// (kills both variables; it Uses both, since the call read both
			// before it, and defines an owned variable holding each earlier
			// value and its own result), g(x, x = 2)@48 (Uses those owned
			// variables and the result), return x;@64 (the parameter). Succ:
			// A→{x = 1, x = 2}; x = 1→x = 2→g(x, x = 2)→return x. IPDom: A →
			// x = 2.
			name:     "a write through a name a conditional left standing for two bindings hands on the held read of each",
			protects: "the call's read of x made before x = 2 pairs with the definitions that reached it in both builds, the parameter's included, rather than with the write after it",
			mutation: "hand off only the arms' variable's held read (the parameter's read stays on the call, which pairs it with x = 2@53: loses x@10 -> x = 2@53)",
			src:      "int f(int x) { {\n#ifdef A\n  int x = 1;\n#endif\n  g(x, x = 2);\n } return x; }",
			cd:       []string{"A@24 -> x = 1@32"},
			du: []string{"x@10 -> x = 2@53", "x = 1@32 -> x = 2@53", "x = 2@53 -> g(x, x = 2)@48",
				"x = 2@53 -> return x;@64"},
		},
	})
}
