package worker

import (
	"slices"
	"testing"
)

// cShared are the C cases whose source parses clean as C++ too, so each runs
// under both grammars with its one expected set. A call statement takes at
// least two arguments: `g(x);` alone may read as a declaration of x in C++
// ([stmt.ambig]). The cases that stay C-only use a construct C++ lacks, or a
// statement the C++ grammar reads as a declaration or a cast; for the latter
// the C++ table holds a case of its own.
var cShared = []goldenCase{
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
		// *p;@31.
		name:     "a write through a pointer updates its base without killing it",
		protects: "a store through *p reaches a later read through p, and so does p's earlier definition",
		mutation: "record no definition of the base (loses *p = v@23 -> return *p;@31), or a killing one (loses p@11 -> return *p;@31)",
		src:      "int f(int *p, int v) { *p = v; return *p; }",
		du:       []string{"p@11 -> *p = v@23", "v@18 -> *p = v@23", "*p = v@23 -> return *p;@31", "p@11 -> return *p;@31"},
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
		// C17 §6.8.5.2 (do) and §6.8.6.2 (continue); GNU C extension,
		// Statements and Declarations in Expressions: a jump out of the
		// construct is permitted. A loop's condition lies outside its body,
		// so the continue in the do loop's condition binds to the enclosing
		// while and goes to its condition, as the compilers bind it. The
		// condition strips the do's parentheses and the statement
		// expression's own. Nodes: n@10, n@22 (the while's Branch), n--@30,
		// n & 1@49 (Branch), continue;@56, n > 0@66 (defining the statement
		// expression's result), the do's condition { … }@43 (a Branch Using
		// it), return n;@80. Succ: n@22→{n--, return n}; n--→n & 1→
		// {continue, n > 0}; continue→n@22; n > 0→{ … }→{n-- (the do's back
		// edge), n@22}. IPDom: n@22 → return n; n-- → n & 1 → n@22; n > 0 →
		// { … } → n@22. Frontier walks: n@22→n-- gives n--, n & 1, n@22;
		// n & 1→n > 0 gives n > 0, { … }; { … }→n-- gives n--, n & 1.
		name:     "a continue in a do loop's condition binds to the enclosing loop",
		protects: "a jump in a statement expression in a loop's condition binds to the loop enclosing that loop, so the continue re-evaluates the outer condition",
		mutation: "lower the do loop's condition with its own frame open (the continue stays pending on the do loop and CloseFrame panics), or bind the continue to nothing (unresolved 1)",
		src:      "int f(int n) { while (n) { do n--; while (({ if (n & 1) continue; n > 0; })); } return n; }",
		cd: []string{"n@22 -> n--@30", "n@22 -> n & 1@49", "n@22 -> n@22", "n & 1@49 -> continue;@56",
			"n & 1@49 -> n > 0@66", "n & 1@49 -> { if (n & 1) continue; n > 0; }@43",
			"{ if (n & 1) continue; n > 0; }@43 -> n--@30", "{ if (n & 1) continue; n > 0; }@43 -> n & 1@49"},
		du: []string{"n@10 -> n@22", "n@10 -> n--@30", "n--@30 -> n--@30", "n--@30 -> n & 1@49", "n--@30 -> n > 0@66",
			"n--@30 -> n@22", "n--@30 -> return n;@80", "n > 0@66 -> { if (n & 1) continue; n > 0; }@43",
			"n@10 -> return n;@80"},
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
	{
		// C17 §6.8.6.1p1: a goto's identifier names a label in the
		// enclosing function; M names none. Nodes: goto M;@15 (a Jump with
		// no target, so it keeps no successor and gains the edge to EXIT).
		name:       "a goto to a label the function does not define is an unresolved jump",
		protects:   "a goto whose label names nothing is counted as unresolved rather than landing anywhere",
		mutation:   "drop the unresolved count for a goto whose label is never defined (unresolved 0)",
		src:        "void f(void) { goto M; }",
		unresolved: 1,
		// Ill-formed on purpose (C17 §6.8.6.1p1 is a constraint): the
		// unresolved count exists only for such a source.
		illFormed: true,
	},
	{
		// C17 §6.8.4.1p2: the first substatement runs when the condition is
		// nonzero, the else substatement otherwise. Nodes: x@10, x@26
		// (Branch), y = 1@29, y = 2@41, return y;@48 (`int y;` makes no
		// node). Succ: x@26→{y = 1, y = 2}; both → return y. IPDom: x@26 →
		// return y.
		name:     "an if with an else runs one arm and merges after it",
		protects: "each arm of an if-else is a path from its condition, and both arms' definitions reach the statement after it",
		mutation: "let the then arm fall into the else arm (y = 2@41 kills y = 1@29: loses y = 1@29 -> return y;@48 and x@26 -> y = 2@41)",
		src:      "int f(int x) { int y; if (x) y = 1; else y = 2; return y; }",
		cd:       []string{"x@26 -> y = 1@29", "x@26 -> y = 2@41"},
		du:       []string{"x@10 -> x@26", "y = 1@29 -> return y;@48", "y = 2@41 -> return y;@48"},
	},
	{
		// C17 §6.5.14p4: || evaluates its right operand only when the left
		// compares equal to 0. Nodes: a@10, b@17, a@30 (Branch, Uses a,
		// defines the || result), g(b)@35 (Uses b, defines the result on
		// the path that evaluates it), r = a || g(b)@26 (Uses the result,
		// defines r), return r;@41. Succ: a@30→{g(b), r = a || g(b)};
		// g(b)→r = a || g(b). IPDom: a@30 → r = a || g(b).
		name:     "|| decides on its left operand and yields through a result",
		protects: "the right operand of || depends on the left, and the consumer reaches the value through the result both operands define",
		mutation: "match only the && token (a@30 and its control of g(b)@35 vanish, and a@10 and b@17 pair with r = a || g(b)@26)",
		src:      "int f(int a, int b) { int r = a || g(b); return r; }",
		cd:       []string{"a@30 -> g(b)@35"},
		du: []string{"a@10 -> a@30", "b@17 -> g(b)@35", "a@30 -> r = a || g(b)@26", "g(b)@35 -> r = a || g(b)@26",
			"r = a || g(b)@26 -> return r;@41"},
	},
	{
		// C17 §6.5.3.4p2: the operand of sizeof is not evaluated unless its
		// type is a variable-length array; [expr.sizeof]/1 alike. Nodes:
		// x@10, y = sizeof x@19 (reads nothing), return y;@33.
		name:     "the operand of sizeof is not read",
		protects: "sizeof of an ordinary variable makes no use of it",
		mutation: "read sizeof's operand (adds x@10 -> y = sizeof x@19)",
		src:      "int f(int x) { int y = sizeof x; return y; }",
		du:       []string{"y = sizeof x@19 -> return y;@33"},
	},
	{
		// C17 §6.8.5p5 and [stmt.for]: a for statement is a block, so the
		// x its clause-1 declares ends with the loop. The call takes two
		// arguments so the body cannot read as a declaration in C++
		// ([stmt.ambig]). Nodes: x@10, y@17, x = 0@31, x < y@38 (Branch),
		// g(0, x)@50, x++@45 (the update), return x;@59. Succ: x = 0→
		// x < y→{g(0, x), return x}; g(0, x)→x++→x < y. IPDom: x < y →
		// return x; g(0, x) → x++ → x < y.
		name:     "a for-init declaration is scoped to its loop",
		protects: "the variable a for clause declares shadows the parameter inside the loop only, so the read after the loop sees the parameter",
		mutation: "keep the for-init binding after the loop (return x;@59 pairs with x = 0@31 and x++@45 instead of x@10)",
		src:      "int f(int x, int y) { for (int x = 0; x < y; x++) g(0, x); return x; }",
		cd:       []string{"x < y@38 -> g(0, x)@50", "x < y@38 -> x++@45", "x < y@38 -> x < y@38"},
		du: []string{"y@17 -> x < y@38", "x = 0@31 -> x < y@38", "x++@45 -> x < y@38", "x = 0@31 -> g(0, x)@50",
			"x++@45 -> g(0, x)@50", "x = 0@31 -> x++@45", "x++@45 -> x++@45", "x@10 -> return x;@59"},
	},
	{
		// C17 §6.8.4.1p2 and §6.5.3.1p2 (the value of --x is x after the
		// decrement). Nodes: x@10, --x@19 (the condition: one Branch node
		// that reads and defines x), return x;@24, return 0;@34. IPDom:
		// --x → EXIT.
		name:     "an update that is the whole condition is the decision node",
		protects: "a condition that is only an increment or decrement makes one Branch node defining its operand",
		mutation: "lower the update as a node of its own before a Branch spanning the same text (adds --x@19 -> --x@19)",
		src:      "int f(int x) { if (--x) return x; return 0; }",
		cd:       []string{"--x@19 -> return x;@24", "--x@19 -> return 0;@34"},
		du:       []string{"x@10 -> --x@19", "--x@19 -> return x;@24"},
	},
	{
		// C17 §6.5.3.2p3, §6.10.1p6: after the directive x is the #ifdef
		// group's variable in one build and the outer x in the other, so
		// &x may-defines both. Nodes: x = 0@18, A@34 (Branch), x = 1@42,
		// g(0, &x)@58 (Uses and may-defines both), return x;@70 (the outer
		// x). Succ: x = 0→A→{x = 1, g(0, &x)}; x = 1→g(0, &x)→return x.
		// IPDom: A → g(0, &x).
		name:     "taking the address of a name a conditional left standing for two bindings may-defines both",
		protects: "an address taken through a joined name reaches later reads of every variable the name may denote",
		mutation: "may-define only the arms' variable (loses g(0, &x)@58 -> return x;@70)",
		src:      "int f(void) { int x = 0; {\n#ifdef A\n  int x = 1;\n#endif\n  g(0, &x); } return x; }",
		cd:       []string{"A@34 -> x = 1@42"},
		du: []string{"x = 0@18 -> g(0, &x)@58", "x = 1@42 -> g(0, &x)@58", "x = 0@18 -> return x;@70",
			"g(0, &x)@58 -> return x;@70"},
	},
	{
		// C17 §6.5.2.3p3: a member name designates a member of the
		// structure, not an ordinary identifier (§6.2.3: members have a name
		// space of their own). Nodes: s@15, m = 1@24, return s.m;@31. The
		// local m is never read.
		name:     "a field name is no read of a local of the same name",
		protects: "the member identifier in s.m does not resolve to a local variable named m",
		mutation: "resolve field identifiers as names (adds m = 1@24 -> return s.m;@31)",
		src:      "int f(struct S s) { int m = 1; return s.m; }",
		du:       []string{"s@15 -> return s.m;@31"},
	},
	{
		// C17 §6.8.5.3p2: an omitted condition is replaced by a nonzero
		// constant, so the loop leaves only by break. Nodes: x@11, for@16
		// (the head, a Stmt node spanning the keyword with no exit edge),
		// x@31 (Branch), break;@34, g()@41, h()@48. Succ: for→x@31→{break,
		// g()}; break→h(); g()→for. IPDom: x@31 → break; g() → for → x@31.
		name:     "a for with no condition leaves only by break",
		protects: "for (;;) has no exit edge, so the break's condition controls the loop and nothing depends on the head",
		mutation: "give a condition-less for an exit edge (for@16 becomes a Branch controlling x@31, break;@34 and g()@41)",
		src:      "void f(int x) { for (;;) { if (x) break; g(); } h(); }",
		cd:       []string{"x@31 -> g()@41", "x@31 -> for@16", "x@31 -> x@31"},
		du:       []string{"x@11 -> x@31"},
	},
	{
		// C17 §6.8.6.2p2: continue jumps to the end of the loop body, and a
		// loop on 1 re-enters at its head. Nodes: x@11, 1@23 (the head, a
		// Stmt node with no exit edge), x@32 (Branch), continue;@35, g()@45.
		// Succ: 1→x@32→{continue, g()}; continue→1; g()→1. Nothing reaches
		// EXIT, so the exit augmentation adds 1→EXIT (1 is the sink
		// component's first node in reverse post-order). IPDom: x@32,
		// continue, g() → 1 → EXIT. Frontier walks: 1→x@32 gives x@32, 1;
		// x@32→continue gives continue; x@32→g() gives g().
		name:     "a continue in a loop with no exit lands on the head",
		protects: "a continue in a loop that never exits re-enters at the head's node, so the body's test still controls the statement after it",
		mutation: "land the continue on the body's first node x@32 (every path from x@32 then passes g()@45: loses x@32 -> g()@45)",
		src:      "void f(int x) { while (1) { if (x) continue; g(); } }",
		cd:       []string{"1@23 -> x@32", "1@23 -> 1@23", "x@32 -> continue;@35", "x@32 -> g()@45"},
		du:       []string{"x@11 -> x@32"},
	},
	{
		// C17 §6.8.6.2p2 and §6.8.5.3p1: a continue in a for loop goes to
		// the end of the body, after which the update runs. Nodes: n@10,
		// s = 0@19, i = 0@35, i < n@42 (Branch), i == 1@60 (Branch),
		// continue;@68, s += i@78, i++@49 (the update), return s;@88. Succ:
		// i = 0→i < n→{i == 1, return s}; i == 1→{continue, s += i};
		// continue→i++; s += i→i++; i++→i < n. IPDom: i == 1, continue,
		// s += i → i++ → i < n → return s.
		name:     "a continue in a for loop lands on the update",
		protects: "a continue in a for body runs the update before the condition, so the update post-dominates the body's test",
		mutation: "land the continue on the condition (i++@49 no longer post-dominates i == 1@60: adds i == 1@60 -> i++@49)",
		src:      "int f(int n) { int s = 0; for (int i = 0; i < n; i++) { if (i == 1) continue; s += i; } return s; }",
		cd: []string{"i < n@42 -> i == 1@60", "i < n@42 -> i++@49", "i < n@42 -> i < n@42",
			"i == 1@60 -> continue;@68", "i == 1@60 -> s += i@78"},
		du: []string{"n@10 -> i < n@42", "i = 0@35 -> i < n@42", "i = 0@35 -> i == 1@60", "i = 0@35 -> s += i@78",
			"i = 0@35 -> i++@49", "i++@49 -> i < n@42", "i++@49 -> i == 1@60", "i++@49 -> s += i@78", "i++@49 -> i++@49",
			"s = 0@19 -> s += i@78", "s = 0@19 -> return s;@88", "s += i@78 -> s += i@78", "s += i@78 -> return s;@88"},
	},
	{
		// C17 §6.8.6.2p2 and §6.8.5.1: a continue in a while body goes to
		// the end of the body, after which the condition is evaluated
		// again. Nodes: x@10, y@17, x@29 (Branch head), y@38 (Branch),
		// continue;@41, x--@51, return x;@58. Succ: x@29→{y@38, return x};
		// y@38→{continue, x--}; continue→x@29; x--→x@29. IPDom: y@38,
		// continue, x-- → x@29 → return x.
		name:     "a continue in a while body lands on the condition",
		protects: "a continue in a while body re-evaluates the condition, so the body's test does not control the loop head",
		mutation: "lower the continue as a break (y@38 gains control of x@29, and loses the loop's back edge from continue;@41)",
		src:      "int f(int x, int y) { while (x) { if (y) continue; x--; } return x; }",
		cd:       []string{"x@29 -> y@38", "x@29 -> x@29", "y@38 -> continue;@41", "y@38 -> x--@51"},
		du: []string{"x@10 -> x@29", "x--@51 -> x@29", "y@17 -> y@38", "x@10 -> x--@51", "x--@51 -> x--@51",
			"x@10 -> return x;@58", "x--@51 -> return x;@58"},
	},
	{
		// C17 §6.5.13p4 and §6.8.4.1: a && condition evaluates g(b) only
		// when a is nonzero, and the if branches on the && value. Nodes:
		// a@10, b@17, a@26 (Branch, defining the && result), g(b)@31
		// (defining it again), a && g(b)@26 (the condition's Branch, Using
		// the result), return 1;@37, return 0;@47. Succ: a@26→{g(b),
		// a && g(b)}; g(b)→a && g(b)→{return 1, return 0}. IPDom: a@26,
		// g(b) → a && g(b) → EXIT.
		name:     "a && that is the whole condition branches on its result",
		protects: "the condition's decision node reaches the operands' values through the result, not by re-reading their names",
		mutation: "let the condition re-read the operands' names (a@10 and b@17 pair with a && g(b)@26, and a@26 -> a && g(b)@26 and g(b)@31 -> a && g(b)@26 vanish)",
		src:      "int f(int a, int b) { if (a && g(b)) return 1; return 0; }",
		cd:       []string{"a@26 -> g(b)@31", "a && g(b)@26 -> return 1;@37", "a && g(b)@26 -> return 0;@47"},
		du:       []string{"a@10 -> a@26", "b@17 -> g(b)@31", "a@26 -> a && g(b)@26", "g(b)@31 -> a && g(b)@26"},
	},
	{
		// C17 §6.7.6.3p8: a parameter declared as a function returning int
		// is adjusted to a pointer to one ([dcl.fct]/5 alike), and is still
		// a parameter. Nodes: g@10, x@22, return g(x);@27 (Uses g and x).
		name:     "a parameter declared as a function is a parameter node",
		protects: "a parameter whose declarator is a function declarator is declared and defined like any other parameter",
		mutation: "skip a parameter whose declarator is a function declarator (loses g@10 -> return g(x);@27)",
		src:      "int f(int g(int), int x) { return g(x); }",
		du:       []string{"g@10 -> return g(x);@27", "x@22 -> return g(x);@27"},
	},
	{
		// C17 §6.7.6.3 ([dcl.fct] alike): in `int (*f(int x))(int)` the
		// declarator nearest f takes (int x), and the outer (int) is the
		// parameter list of the returned pointer's type. Nodes: x@12,
		// return h(x);@23.
		name:     "a function returning a function pointer takes its parameters from the innermost declarator",
		protects: "the parameters of a definition are those of the function declarator nearest its name",
		mutation: "take the outermost function declarator's parameters (its (int) names no parameter: loses x@12 -> return h(x);@23)",
		src:      "int (*f(int x))(int) { return h(x); }",
		du:       []string{"x@12 -> return h(x);@23"},
	},
	{
		// C17 §6.2.4p3 and §6.7.9p10 ([stmt.dcl]/3 alike): a static local's
		// initializer takes effect once, before its first use; the lowering
		// makes it a defining node at its position. Nodes: x@10, s = 1@26,
		// s += x@33, return s;@41.
		name:     "a static local's initializer is a defining node at its position",
		protects: "a static local is a variable of the function whose initializer defines it where it is written",
		mutation: "treat a static local as no variable (every pair of s vanishes: loses s = 1@26 -> s += x@33 and s += x@33 -> return s;@41)",
		src:      "int f(int x) { static int s = 1; s += x; return s; }",
		du:       []string{"x@10 -> s += x@33", "s = 1@26 -> s += x@33", "s += x@33 -> return s;@41"},
	},
	{
		// C17 §6.8.4.2p7 ([stmt.switch] alike): control enters the body only
		// at a matching label, so a statement before the first one is
		// reached by no path from the switch. Nodes: x@11, x@24 (the
		// controlling expression, defining the switch value), g(0, x)@29 (no
		// predecessor), 1@43 (the test, Using the switch value), h()@46.
		// Succ: x@24→1→{h(), EXIT}; g(0, x)→h(). IPDom: 1 → EXIT.
		name:     "a statement before the first case label is reached by no path",
		protects: "the switch head jumps to its tests, never falling into the statements before the first label, so their reads pair with nothing",
		mutation: "enter the body at its first statement (adds x@11 -> g(0, x)@29)",
		src:      "void f(int x) { switch (x) { g(0, x); case 1: h(); } }",
		cd:       []string{"1@43 -> h()@46"},
		du:       []string{"x@11 -> x@24", "x@24 -> 1@43"},
	},
	{
		// C17 §6.8.4.2p4 (a case label may sit anywhere in the switch body)
		// and §6.10.1 (the group is kept or not at translation time). The
		// case label inside the #ifdef is nested below another statement of
		// the body, so its test jumps to its label node. Nodes: x@11, x@24
		// (the controlling expression, defining the switch value), 1@44 and
		// 2@65 (the tests in source order, Using it), A@36 (the directive's
		// Branch, before the first top-level label: no predecessor),
		// case@39 (the nested label's node), g()@47, h()@68. Succ:
		// x@24→1→{case@39, 2}; 2→{h(), EXIT}; A→{case@39, h()};
		// case@39→g()→h()→EXIT. IPDom: 1, 2 → EXIT; A → h(); case@39 →
		// g() → h(). Frontier walks: 1→case@39 gives case@39, g(), h();
		// 1→2 gives 2; 2→h() gives h(); A→case@39 gives case@39, g().
		name:     "a case label inside a preprocessor conditional is reached from its test",
		protects: "a case label a conditional wraps is still tested in source order and entered at its own label node, and the arm falls into the next case body",
		mutation: "skip case labels nested in a preprocessor conditional (1@44 and case@39 vanish, and x@24 enters only 2@65)",
		src:      "void f(int x) { switch (x) {\n#ifdef A\n case 1: g();\n#endif\n case 2: h(); } }",
		cd: []string{"1@44 -> case@39", "1@44 -> g()@47", "1@44 -> h()@68", "1@44 -> 2@65", "2@65 -> h()@68",
			"A@36 -> case@39", "A@36 -> g()@47"},
		du: []string{"x@11 -> x@24", "x@24 -> 1@44", "x@24 -> 2@65"},
	},
	{
		// C17 §6.8.4.2p5: with no matching case label control jumps to the
		// default label, here inside a loop of the switch body. Nodes: x@11,
		// n@18, x@31 (the controlling expression), 0@41 (the test), n--@51
		// (the while condition: one Branch node defining n), default@58
		// (the nested label's node), g()@67. Succ: x@31→0→{n--, default@58
		// (the no-match path's jump)}; n--→{default@58, EXIT};
		// default@58→g()→n--. IPDom: 0 → n--; default@58 → g() → n-- →
		// EXIT. Frontier walks: 0→default@58 gives default@58, g();
		// n--→default@58 gives default@58, g(), n--.
		name:     "a nested default label takes the no-match path",
		protects: "when the default label sits inside a loop of the switch body, a failed test jumps into the loop at the label",
		mutation: "let the no-match path leave the switch (adds 0@41 -> n--@51, loses 0@41 -> default@58 and 0@41 -> g()@67)",
		src:      "void f(int x, int n) { switch (x) { case 0: while (n--) { default: g(); } } }",
		cd: []string{"0@41 -> default@58", "0@41 -> g()@67", "n--@51 -> default@58", "n--@51 -> g()@67",
			"n--@51 -> n--@51"},
		du: []string{"x@11 -> x@31", "x@31 -> 0@41", "n@18 -> n--@51", "n--@51 -> n--@51"},
	},
	{
		// GNU C extension, Extended Asm: the input operands are read by the
		// assembly statement. Nodes: x@10, asm("" : : "r"(x))@15 (a Stmt
		// node Using x), return x;@35.
		name:     "a plain assembly statement reads its input operands",
		protects: "an asm statement without goto labels is one node whose input operand expressions are its reads",
		mutation: "read no operand of an assembly statement (loses x@10 -> asm(\"\" : : \"r\"(x))@15)",
		src:      "int f(int x) { asm(\"\" : : \"r\"(x)); return x; }",
		du:       []string{"x@10 -> asm(\"\" : : \"r\"(x))@15", "x@10 -> return x;@35"},
	},
	{
		// GNU C extension, Extended Asm, Output Operands: an output operand
		// is written by the assembly, a write through its expression, so the
		// node Uses and may-defines x. Nodes: x@10, asm("" : "=r"(x))@15,
		// return x;@34, which pairs with the may-definition and with x@10
		// behind it.
		name:     "an assembly output operand may-defines its variable",
		protects: "a variable an asm statement writes is may-defined there, so a later read sees the write",
		mutation: "read an output operand as an input (loses asm(\"\" : \"=r\"(x))@15 -> return x;@34)",
		src:      "int f(int x) { asm(\"\" : \"=r\"(x)); return x; }",
		du: []string{"x@10 -> asm(\"\" : \"=r\"(x))@15", "x@10 -> return x;@34",
			"asm(\"\" : \"=r\"(x))@15 -> return x;@34"},
	},
	{
		// C17 §6.10.1: #if tests an expression, #elif tests another when the
		// ones before failed, #ifndef tests a macro name; with no #else a
		// build may keep no group. Nodes: x@10, A@19 (Branch), x = 1@23,
		// B@36 (the #elif's Branch, on A's false path), x = 2@40, C@62 (the
		// #ifndef's Branch spanning the name), x = 3@66, return x;@82. Succ:
		// A→{x = 1, B}; B→{x = 2, C}; x = 1, x = 2→C; C→{x = 3, return x};
		// x = 3→return x. IPDom: A, B → C → return x.
		name:     "#if, #elif and #ifndef each branch on their condition",
		protects: "an #elif is a decision on its predecessor's false path, and each conditional without #else keeps a path that runs none of its groups",
		mutation: "lower #elif as an arm with no condition of its own (B@36 vanishes, A@19 controls x = 2@40, and loses x@10 -> return x;@82)",
		src:      "int f(int x) {\n#if A\n  x = 1;\n#elif B\n  x = 2;\n#endif\n#ifndef C\n  x = 3;\n#endif\n  return x;\n}",
		cd:       []string{"A@19 -> x = 1@23", "A@19 -> B@36", "B@36 -> x = 2@40", "C@62 -> x = 3@66"},
		du: []string{"x@10 -> return x;@82", "x = 1@23 -> return x;@82", "x = 2@40 -> return x;@82",
			"x = 3@66 -> return x;@82"},
	},
	{
		// C17 §6.10.1p4: a directive's identifiers are macro names, not
		// variables. Nodes: x@10, x@19 (the #if's Branch, reading nothing),
		// g()@23, return x;@37.
		name:     "a directive's condition reads no variable",
		protects: "a name in an #if condition that matches a local is no read of the local",
		mutation: "read the directive's condition as an expression (adds x@10 -> x@19)",
		src:      "int f(int x) {\n#if x\n  g();\n#endif\n  return x;\n}",
		cd:       []string{"x@19 -> g()@23"},
		du:       []string{"x@10 -> return x;@37"},
	},
	{
		// C17 §6.10.3: a #define is a translation-time directive and runs
		// nothing. Nodes: x@10, x@19 (Branch), return 0;@39; the #define
		// inside the if makes no node, so the Branch controls nothing.
		name:     "a #define inside a body makes no node",
		protects: "preprocessor lines other than conditionals are not statements",
		mutation: "lower a #define as a plain Stmt node (adds x@19 -> #define Y 1@24)",
		src:      "int f(int x) { if (x) {\n#define Y 1\n } return 0; }",
		du:       []string{"x@10 -> x@19"},
	},
	{
		// C17 §6.10.1p6 and §6.7.6.3: the #ifdef group declares x as a
		// function, no variable; the empty group a build takes without A
		// declares nothing, so after the directive x keeps the parameter's
		// binding. Nodes: x@10, A@24 (Branch, both edges to the return),
		// return x;@49.
		name:     "a prototype only one arm declares leaves the earlier binding after the directive",
		protects: "a name the arms bind only as no variable, and some build leaves unbound, still names the variable it named before the directive",
		mutation: "make the name no variable after the directive when any arm binds it as none (loses x@10 -> return x;@49)",
		src:      "int f(int x) { {\n#ifdef A\n  int x(int);\n#endif\n  return x; } }",
		du:       []string{"x@10 -> return x;@49"},
	},
	{
		// C17 §6.7.2.1 (a structure declaration) and §6.7.8 (typedef)
		// declare types and run nothing; [class.pre] and [dcl.typedef]
		// alike. Nodes: x@10, x@19 (Branch), return 0;@62.
		name:     "a structure or typedef declaration in a body makes no node",
		protects: "type declarations inside a function are not statements",
		mutation: "lower a struct specifier or type definition as a plain Stmt node (adds x@19 -> struct T { int a; }@24 or x@19 -> typedef int U;@47)",
		src:      "int f(int x) { if (x) { struct T { int a; }; typedef int U; } return 0; }",
		du:       []string{"x@10 -> x@19"},
	},
	{
		// C17 §6.5.2.2p10 and §6.5.16p3 in C, and in C++
		// [expr.ass]: an assignment's result is its left operand, and
		// [expr.call]: the arguments are indeterminately sequenced, each
		// evaluated completely before the call. Nodes: x@10, x = 1@25
		// (defines x and its own result), x = 2@34 (defines x, killing
		// x = 1, and its own result), return g((x = 1),
		// (x = 2));@15 (Uses both results). Succ: a straight line to EXIT.
		name:     "each embedded assignment to a local hands its own value to the consumer",
		protects: "the consumer of two assignments to one local depends on both, since it Uses each assignment's result and not the local the second one overwrites",
		mutation: "let the consumer Use the assigned local instead of each result (loses x = 1@25 -> return g((x = 1), (x = 2));@15)",
		src:      "int f(int x) { return g((x = 1), (x = 2)); }",
		du:       []string{"x = 1@25 -> return g((x = 1), (x = 2));@15", "x = 2@34 -> return g((x = 1), (x = 2));@15"},
	},
	{
		// C17 §6.5.2.2p10 and §6.5.16p3 in C, and in C++
		// [expr.call]: the arguments are indeterminately sequenced; the
		// lowering takes source order, so the read of x in the first
		// argument precedes x = 1. Nodes: x@10, x = 1@27 (Uses the x the
		// call already read, defines x, an owned variable carrying that
		// earlier value, and its own result), return g(x, x
		// = 1);@15 (Uses the carried value and the result). Succ: a
		// straight line to EXIT.
		name:     "a read made before an embedded assignment of the same local keeps the earlier value",
		protects: "the call's first argument reaches the parameter's value through the assignment's node, not the value the assignment stores",
		mutation: "let the held read of x pair with the definition after it (loses x@10 -> x = 1@27)",
		src:      "int f(int x) { return g(x, x = 1); }",
		du:       []string{"x@10 -> x = 1@27", "x = 1@27 -> return g(x, x = 1);@15"},
	},
	{
		// C17 §6.5.2.2p10 and §6.5.16p3 in C, and in C++
		// [expr.log.and]/1: the right operand is evaluated only when the
		// left is true; [expr.call]: the arguments are indeterminately
		// sequenced, taken in source order. Nodes: x@10, c@17, c@34
		// (Branch, Uses c, defines the && result), x = 1@40 (defines x,
		// its own result and the && result; it runs only when c
		// is true, so it takes no read held before it), return g(x, c &&
		// (x = 1));@22 (Uses the held x and the && result). Succ: c@34→
		// {x = 1, return}; x = 1→return. IPDom: c@34 → return.
		name:     "a read held before a conditionally evaluated assignment stays on the consumer",
		protects: "on the path that skips an assignment in a && operand the consumer still sees the local's earlier definition, and on the other path the assignment",
		mutation: "hand the held read off to the assignment unconditionally (adds x@10 -> x = 1@40, loses x@10 -> return g(x, c && (x = 1));@22)",
		src:      "int f(int x, int c) { return g(x, c && (x = 1)); }",
		cd:       []string{"c@34 -> x = 1@40"},
		du: []string{"c@17 -> c@34", "x@10 -> return g(x, c && (x = 1));@22", "x = 1@40 -> return g(x, c && (x = 1));@22",
			"c@34 -> return g(x, c && (x = 1));@22"},
	},
	{
		// C17 §6.2.1p4 and §6.7.6.3 ([basic.scope.block] alike): the block's
		// `int x(int);` declares a function that hides the parameter until
		// the block ends. Nodes: x@10, y@17, y = x(1)@36 (reads no
		// variable), return x + y;@48.
		name:     "a block-scope prototype hides a parameter with no variable",
		protects: "a name a block declares as a function is no variable there, and the enclosing variable is visible again after the block",
		mutation: "leave the parameter visible under the prototype (adds x@10 -> y = x(1)@36)",
		src:      "int f(int x, int y) { { int x(int); y = x(1); } return x + y; }",
		du:       []string{"x@10 -> return x + y;@48", "y = x(1)@36 -> return x + y;@48"},
	},
	{
		// C17 §6.5.17p2: the comma operator evaluates both operands and
		// yields the right one; in value position it is folded into the
		// node that evaluates it. Nodes: x@10, y@17, y = (g(), x)@22 (Uses
		// x, defines y), return y;@36.
		name:     "a comma operator in value position is folded into its consumer",
		protects: "the reads of a value-position comma expression are its consumer's own",
		mutation: "drop a comma expression's operand reads in value position (loses x@10 -> y = (g(), x)@22)",
		src:      "int f(int x, int y) { y = (g(), x); return y; }",
		du:       []string{"x@10 -> y = (g(), x)@22", "y = (g(), x)@22 -> return y;@36"},
	},
	{
		// C17 §6.5.4 and §6.5.3.2p4: `*(int *)p` designates the object p
		// points to, so the store is a write through p. Nodes: p@13, v@20,
		// *(int *)p = v@25 (Uses p and v, may-defines p), g(0, p)@40.
		name:     "a write through a cast pointer may-defines the pointer",
		protects: "the base of a write is found through a cast, so a later read of the pointer sees the store",
		mutation: "stop baseIdent at a cast (loses *(int *)p = v@25 -> g(0, p)@40)",
		src:      "void f(void *p, int v) { *(int *)p = v; g(0, p); }",
		du: []string{"p@13 -> *(int *)p = v@25", "v@20 -> *(int *)p = v@25", "p@13 -> g(0, p)@40",
			"*(int *)p = v@25 -> g(0, p)@40"},
	},
	{
		// C17 §6.7.9p7: a designator names a member, not a variable; the
		// initializer list is folded into the declarator's node. Nodes:
		// x@10, a = 0@19, s = { .a = x }@35 (Uses x only, defines s),
		// return s.a;@51.
		name:     "a designated initializer reads only its value",
		protects: "the member a designator names is no read of a local of the same name",
		mutation: "read the designator as a name (adds a = 0@19 -> s = { .a = x }@35)",
		src:      "int f(int x) { int a = 0; struct S s = { .a = x }; return s.a; }",
		du:       []string{"x@10 -> s = { .a = x }@35", "s = { .a = x }@35 -> return s.a;@51"},
	},
	{
		// C17 §6.5.15p4: only the chosen arm of ?: is evaluated, so an
		// assignment in an arm does not run on every path to the call and
		// takes none of its earlier reads. Nodes: x@10, c@17, c@34
		// (Branch), x = 1@39 (defines x and the ?: result), 0@48 (defines
		// the result), return g(x, c ? (x = 1) : 0);@22 (Uses x and the
		// result). Succ: c@34→{x = 1, 0}→return. IPDom: c@34 → return.
		name:     "an assignment in a conditional arm takes none of the consumer's earlier reads",
		protects: "on the path through the other arm the consumer still sees the local's earlier definition",
		mutation: "hand the held read off to an assignment in a ?: arm (adds x@10 -> x = 1@39, loses x@10 -> return g(x, c ? (x = 1) : 0);@22)",
		src:      "int f(int x, int c) { return g(x, c ? (x = 1) : 0); }",
		cd:       []string{"c@34 -> x = 1@39", "c@34 -> 0@48"},
		du: []string{"c@17 -> c@34", "x@10 -> return g(x, c ? (x = 1) : 0);@22",
			"x = 1@39 -> return g(x, c ? (x = 1) : 0);@22", "0@48 -> return g(x, c ? (x = 1) : 0);@22"},
	},
	{
		// C17 §6.10.1p6: inside the #ifdef A group, the inner #ifdef B leaves
		// x standing for the group's variable and the parameter. The #else
		// group redeclares x without an initializer; there x is the groups'
		// variable alone, so its g(0, x) reads no parameter. After the outer
		// directive the link holds again, since the build with A and not B
		// keeps the parameter. Nodes: x@10, A@24, B@33 (Branches), x = 1@41,
		// g(0, x)@57 (Uses both), #else@66, g(0, x)@83 (Uses the groups'
		// variable, which nothing defines on that path), return x;@101 (Uses
		// both). `int x;` makes no node. Succ: A→{B, #else}; B→{x = 1,
		// g(0, x)@57}; x = 1→g(0, x)@57→return; #else→g(0, x)@83→return.
		// IPDom: A → return; B → g(0, x)@57; #else → g(0, x)@83.
		name:     "a name an arm redeclares is that arm's variable alone",
		protects: "inside a sibling arm that redeclares a name an inner conditional joined to an enclosing variable, the name no longer stands for the enclosing one",
		mutation: "keep the link while lowering the redeclaring arm (adds x@10 -> g(0, x)@83)",
		src:      "int f(int x) { {\n#ifdef A\n#ifdef B\n  int x = 1;\n#endif\n  g(0, x);\n#else\n  int x;\n  g(0, x);\n#endif\n  return x; } }",
		cd: []string{"A@24 -> B@33", "A@24 -> g(0, x)@57", "A@24 -> #else@66", "A@24 -> g(0, x)@83",
			"B@33 -> x = 1@41"},
		du: []string{"x@10 -> g(0, x)@57", "x = 1@41 -> g(0, x)@57", "x@10 -> return x;@101",
			"x = 1@41 -> return x;@101"},
	},
	{
		// GNU C extension, Statements and Declarations in Expressions: the
		// declaration is in the construct's own list with no label after
		// it, so it runs on every path to the value and its embedded
		// assignment takes the call's earlier read of x. Nodes: x@10,
		// x = 1@39 (Uses the held x, defines x, the owned variable holding
		// the read and its own result), t = (x = 1)@34 (Uses that result,
		// defines t), t@47 (the last statement, defining the construct's
		// result), return g(x, ({ … }));@15 (Uses the held value and the
		// result). Succ: a straight line to EXIT.
		name:     "a statement expression's declaration on every path hands the consumer's read off",
		protects: "a declaration of a statement expression's own list carries the consumer's earlier read, as an expression statement does",
		mutation: "treat a declaration as skippable (loses x@10 -> x = 1@39)",
		src:      "int f(int x) { return g(x, ({ int t = (x = 1); t; })); }",
		du: []string{"x@10 -> x = 1@39", "x = 1@39 -> t = (x = 1)@34", "t = (x = 1)@34 -> t@47",
			"x = 1@39 -> return g(x, ({ int t = (x = 1); t; }));@15", "t@47 -> return g(x, ({ int t = (x = 1); t; }));@15"},
	},
	{
		// C17 §6.5.16p3 leaves the order of the operands unsequenced; the
		// lowering takes the right operand before the target's operands, so
		// a[x] reads the x that x = 1 stored. Nodes: x@10, a@18, x = 1@31
		// (defines x and its result), a[x] = (x = 1)@23 (Uses a, x and the
		// result, may-defines a), return x;@39.
		name:     "an assignment evaluates its right operand before its target's operands",
		protects: "the subscript of a written element reads the value the right operand's assignment stored",
		mutation: "evaluate the target's operands first (the subscript's read is handed off: adds x@10 -> x = 1@31)",
		src:      "int f(int x, int *a) { a[x] = (x = 1); return x; }",
		du:       []string{"x = 1@31 -> a[x] = (x = 1)@23", "a@18 -> a[x] = (x = 1)@23", "x = 1@31 -> return x;@39"},
	},
	{
		// GNU C extension, Statements and Declarations in Expressions: a
		// construct whose last statement is not an expression statement
		// has no value: it has void type, so it can only be discarded, here
		// by a cast to void (C17 §6.5.4p2), folded into the statement's
		// node. Nodes: x@10, x@28 (Branch), h()@31, (void)({ if (x) h();
		// })@15 (the statement, reading nothing), return x;@40. Succ:
		// x@28→{h(), (void)…}; h()→(void)…→return. IPDom: x@28 → (void)….
		name:     "a statement expression ending in an if has no value",
		protects: "a statement expression whose last statement is not an expression statement hands no result to its consumer",
		mutation: "yield the last statement's condition as the value (adds x@28 -> (void)({ if (x) h(); })@15)",
		src:      "int f(int x) { (void)({ if (x) h(); }); return x; }",
		cd:       []string{"x@28 -> h()@31"},
		du:       []string{"x@10 -> x@28", "x@10 -> return x;@40"},
	},
	{
		// C17 §6.8.5.3 (for) and §6.8.6.2 (continue); GNU C extension,
		// Statements and Declarations in Expressions. A for loop's update
		// lies outside its body, so the continue in it binds to the
		// enclosing while and goes to its condition, as the compilers bind
		// it. The update's value is discarded, so it makes no node of its
		// own. Nodes: x@10, y@17, y@29 (the while's Branch), x@41 (the for's
		// Branch), g()@73, y@51 (the update's Branch), continue;@54,
		// x--@64, return x;@80. Succ: y@29→{x@41, return x}; x@41→{g(),
		// y@29 (the for ends the while's body)}; g()→y@51→{continue, x--};
		// continue→y@29; x--→x@41. IPDom: y@29 → return x; x@41, y@51,
		// continue → y@29; g() → y@51; x-- → x@41. Frontier walks:
		// y@29→x@41 gives x@41, y@29; x@41→g() gives g(), y@51;
		// y@51→continue gives continue; y@51→x-- gives x--, x@41.
		name:     "a continue in a for update's statement expression binds to the enclosing loop",
		protects: "a jump in a statement expression in a for loop's update binds to the loop enclosing that loop, so the continue re-evaluates the outer condition rather than re-running the update",
		mutation: "lower the update with the for loop's frame open (the continue stays pending on the for loop and CloseFrame panics), or bind the continue to nothing (unresolved 1)",
		src:      "int f(int x, int y) { while (y) { for (; x; ({ if (y) continue; x--; })) g(); } return x; }",
		cd: []string{"y@29 -> x@41", "y@29 -> y@29", "x@41 -> g()@73", "x@41 -> y@51", "y@51 -> continue;@54",
			"y@51 -> x--@64", "y@51 -> x@41"},
		du: []string{"x@10 -> x@41", "x@10 -> x--@64", "x@10 -> return x;@80", "x--@64 -> x@41", "x--@64 -> x--@64",
			"x--@64 -> return x;@80", "y@17 -> y@29", "y@17 -> y@51"},
	},
	{
		// Structured exception handling, try-except statement: `->` and `[]`
		// dereference, so they may raise in a __try body. Nodes: p@16,
		// a@24, r = 0@33, r = p->f + a[1]@48, the Handler __except@67, 1@77
		// (the filter, a Branch), r = 1@82, return r;@91. Succ: r = p->f +
		// a[1]→{return r, __except}; __except→1→{r = 1, EXIT}; r = 1→
		// return r. IPDom: r = p->f + a[1], 1 → EXIT; __except → 1.
		name:     "a field access through a pointer and a subscript may raise in a __try body",
		protects: "-> and [] inside a __try reach its handler",
		mutation: "count no throw for -> and [] (the __except path is unreachable: r = p->f + a[1]@48 controls nothing)",
		src:      "int f(struct S *p, int *a) { int r = 0; __try { r = p->f + a[1]; } __except (1) { r = 1; } return r; }",
		cd: []string{"r = p->f + a[1]@48 -> __except@67", "r = p->f + a[1]@48 -> 1@77", "r = p->f + a[1]@48 -> return r;@91",
			"1@77 -> r = 1@82", "1@77 -> return r;@91"},
		du: []string{"p@16 -> r = p->f + a[1]@48", "a@24 -> r = p->f + a[1]@48", "r = p->f + a[1]@48 -> return r;@91",
			"r = 1@82 -> return r;@91"},
	},
	{
		// Structured exception handling, try-except statement: `__leave`
		// ends the __try body, a normal completion that skips the handler.
		// Nodes: x@10, r = 0@19, x@38 (Branch), __leave;@41, r = g()@50 (may
		// raise), the Handler __except@61, 1@71 (the filter), r = 1@76,
		// return r;@85. Succ: x@38→{__leave, r = g()}; __leave→return r;
		// r = g()→{return r, __except}; __except→1→{r = 1, EXIT}; r = 1→
		// return r. IPDom: x@38, r = g(), 1 → EXIT; __leave, r = 1 →
		// return r.
		name:     "__leave in a __try/__except completes the body normally",
		protects: "__leave reaches the statement after the try, not the handler, so the value from before the body reaches it",
		mutation: "lower __leave as a plain statement falling into r = g() (loses r = 0@19 -> return r;@85)",
		src:      "int f(int x) { int r = 0; __try { if (x) __leave; r = g(); } __except (1) { r = 1; } return r; }",
		cd: []string{"x@38 -> __leave;@41", "x@38 -> return r;@85", "x@38 -> r = g()@50", "r = g()@50 -> return r;@85",
			"r = g()@50 -> __except@61", "r = g()@50 -> 1@71", "1@71 -> r = 1@76", "1@71 -> return r;@85"},
		du: []string{"x@10 -> x@38", "r = 0@19 -> return r;@85", "r = g()@50 -> return r;@85", "r = 1@76 -> return r;@85"},
	},
	{
		// Structured exception handling, try-finally statement: the
		// __finally block runs however the __try body is left, by break or
		// by return, and each resumes after it. Nodes: x@10, x@22 (the loop
		// head), x > 1@39 (Branch), break;@46, return 1;@53, g()@77 (the
		// finally), return 0;@86. Succ: x@22→{x > 1, return 0}; x > 1→
		// {break, return 1}; break, return 1→g(); g()→{return 0 (the
		// break), EXIT (the return)}. No path completes the body, so the
		// loop has no back edge. IPDom: x@22, g() → EXIT; x > 1 → g().
		name:     "a break and a return out of a __try body both run its __finally",
		protects: "a break and a return leaving a __try pass through the __finally, which then resumes each at its own target",
		mutation: "resolve the break straight to its target (g()@77 no longer controls return 0;@86)",
		src:      "int f(int x) { while (x) { __try { if (x > 1) break; return 1; } __finally { g(); } } return 0; }",
		cd: []string{"x@22 -> x > 1@39", "x@22 -> g()@77", "x@22 -> return 0;@86", "x > 1@39 -> break;@46",
			"x > 1@39 -> return 1;@53", "g()@77 -> return 0;@86"},
		du: []string{"x@10 -> x@22", "x@10 -> x > 1@39"},
	},
	{
		// C17 §6.8.6.1 (goto), §6.8.1 (labels). Nodes: x@11, L@16 (the
		// label's own node), x++@19, x < 9@28, goto L;@35, goto E;@43,
		// g()@51 (no predecessor), E@56, h(0, x)@59. Succ: L→x++→x < 9→
		// {goto L, goto E}; goto L→L (backward); goto E→E (forward);
		// g()→E→h(0, x)→EXIT. IPDom: x < 9 → goto E (every path leaves by
		// it); goto L → L → x++ → x < 9. Frontier walk from x < 9 over
		// goto L: goto L, L, x++, x < 9. The only definition reaching
		// h(0, x) is x++: g() has no path from ENTRY.
		name:     "goto backward to a label and forward over dead code",
		protects: "a goto lands on its label's own node whether the label is before or after it, and code skipped by a goto is not on any path",
		mutation: "resolve goto only to labels declared before it (goto E dangles, unresolved 1), or let a goto fall through to the next statement (g()@51 becomes reachable and x < 9 gains it)",
		src:      "void f(int x) { L: x++; if (x < 9) goto L; goto E; g(); E: h(0, x); }",
		cd:       []string{"x < 9@28 -> goto L;@35", "x < 9@28 -> L@16", "x < 9@28 -> x++@19", "x < 9@28 -> x < 9@28"},
		du:       []string{"x@11 -> x++@19", "x++@19 -> x++@19", "x++@19 -> x < 9@28", "x++@19 -> h(0, x)@59"},
	},
	{
		// Structured exception handling, try-finally statement (the
		// extension's documented semantics): `__leave` ends the `__try` body, the `__finally` block runs on
		// every exit, and a call inside the body may raise. Nodes: x@11,
		// x@28, __leave;@31, g()@40, the Handler __finally@47 (g()
		// raising), h(0, x)@59, k()@70. Succ: x@28→{__leave, g()};
		// __leave→h(0, x); g()→{h(0, x), __finally}; __finally→h(0, x);
		// h(0, x)→{k() (normal), EXIT (the re-raised exception)};
		// k()→EXIT. IPDom: x@28, g(), __finally → h(0, x) → EXIT.
		name:     "__try/__finally with __leave",
		protects: "__leave reaches the __finally body, not the statement after it, and a raising call reaches the __finally through its Handler and leaves the function after it",
		mutation: "lower __leave as a jump past the __finally (h(0, x)@59 loses its edge from __leave;@31 and x@28 gains control of h(0, x)), or count no throw for calls in a __try body (__finally@47 and h(0, x) -> k() vanish)",
		src:      "void f(int x) { __try { if (x) __leave; g(); } __finally { h(0, x); } k(); }",
		cd:       []string{"x@28 -> __leave;@31", "x@28 -> g()@40", "g()@40 -> __finally@47", "h(0, x)@59 -> k()@70"},
		du:       []string{"x@11 -> x@28", "x@11 -> h(0, x)@59"},
	},
	{
		// C17 §6.5.16p3: an assignment's value is the value stored. Nodes:
		// c@10, c = g()@23 (the loop head's first node), (c = g()) !=
		// 0@22, h(0, c)@38, return c;@47. Succ: c = g()→(c = g()) != 0→
		// {h(0, c), return c}; h(0, c)→c = g(). IPDom: h(0, c) → c = g() →
		// (c = g()) != 0 → return c. c = g() kills the parameter.
		name:     "an assignment embedded in a loop condition flows into the condition",
		protects: "the node evaluating an expression that embeds `c = e` reads the c it defined, and the back edge re-enters the assignment",
		mutation: "drop the read-back of the variable an embedded assignment defined (loses c = g()@23 -> (c = g()) != 0@22)",
		src:      "int f(int c) { while ((c = g()) != 0) h(0, c); return c; }",
		cd: []string{"(c = g()) != 0@22 -> h(0, c)@38", "(c = g()) != 0@22 -> c = g()@23",
			"(c = g()) != 0@22 -> (c = g()) != 0@22"},
		du: []string{"c = g()@23 -> (c = g()) != 0@22", "c = g()@23 -> h(0, c)@38", "c = g()@23 -> return c;@47"},
	},
	{
		// C17 §6.2.1p4: a block's declaration hides the outer one until
		// the block ends. Nodes: x@10, x = 1@21, g(0, x)@28, return x;@39.
		name:     "a block-scope declaration shadows a parameter only inside its block",
		protects: "the inner x is a new variable whose scope ends with its block",
		mutation: "keep the block's bindings after it closes (return x;@39 pairs with x = 1@21 instead of x@10)",
		src:      "int f(int x) { { int x = 1; g(0, x); } return x; }",
		du:       []string{"x = 1@21 -> g(0, x)@28", "x@10 -> return x;@39"},
	},
	{
		// C17 §6.5.3.2p3: `&x` yields a pointer to x, through which the
		// callee may write it. Nodes: x = 1@14 (defines x), g(0, &x)@21
		// (Uses x, may-defines x), return x;@31. The return's x pairs
		// with the nearest may-definition g(0, &x) and with the killing
		// x = 1 that reaches it through it.
		name:     "taking an address is a may-definition of the variable",
		protects: "a use after a call that received a variable's address sees the write the call may make through it",
		mutation: "lower `&x` as a plain read (g(0, &x)@21 -> return x;@31 vanishes), or make it a killing Def (x = 1@14 -> return x;@31 vanishes)",
		src:      "int f() { int x = 1; g(0, &x); return x; }",
		du:       []string{"x = 1@14 -> g(0, &x)@21", "x = 1@14 -> return x;@31", "g(0, &x)@21 -> return x;@31"},
	},
	{
		// C17 §6.3.2.1p3: an array evaluated other than as the operand of
		// sizeof or & is converted to a pointer to its first element, so
		// fill may write buf through it; a subscript reads an element.
		// Nodes: `char buf[8];` makes none, fill(0, buf)@24 (Uses buf,
		// may-defines it), use(0, buf[0])@38 (Uses buf), g(0, buf)@54.
		name:     "an array passed to a call decays to its address and is may-defined there",
		protects: "a use of an array after a call that received it sees the write the call may make, while a subscript read writes nothing",
		mutation: "read a decaying array without the may-definition (loses both pairs from fill(0, buf)@24), or let a subscript operand decay too (adds use(0, buf[0])@38 -> g(0, buf)@54)",
		src:      "void f() { char buf[8]; fill(0, buf); use(0, buf[0]); g(0, buf); }",
		du:       []string{"fill(0, buf)@24 -> use(0, buf[0])@38", "fill(0, buf)@24 -> g(0, buf)@54"},
	},
	{
		// C17 §6.10.1p6: only one group of a conditional is kept, so a
		// declaration in the #ifdef group does not exist in the #else
		// group, whose x is the parameter. Nodes: x@10, A@24, x = 1@32,
		// g(0, x)@41, #else@50, g(0, x)@58, return x;@76. After the directive x
		// names the #ifdef group's variable in the build that keeps that
		// group and the parameter in the build that keeps #else, which
		// does not rebind x: return x Uses both.
		name:     "a preprocessor arm does not see a sibling arm's declarations",
		protects: "each arm of a conditional resolves names against the scope before the directive, so the #else arm's x is the parameter, and after the directive x stands for every binding it has in some build",
		mutation: "keep the #ifdef arm's bindings while lowering the #else arm (loses x@10 -> g(0, x)@58), or bind a name after the directive to the arms' variable alone (loses x@10 -> return x;@76)",
		src:      "int f(int x) { {\n#ifdef A\n  int x = 1;\n  g(0, x);\n#else\n  g(0, x);\n#endif\n  return x; } }",
		cd:       []string{"A@24 -> x = 1@32", "A@24 -> g(0, x)@41", "A@24 -> #else@50", "A@24 -> g(0, x)@58"},
		du:       []string{"x@10 -> g(0, x)@58", "x = 1@32 -> g(0, x)@41", "x = 1@32 -> return x;@76", "x@10 -> return x;@76"},
	},
	{
		// C17 §6.8.5.3 (for) and §6.8.6.3 (break); GNU C extension,
		// Statements and Declarations in Expressions. A for loop's update
		// lies outside its body, so the break in it binds to the enclosing
		// while and ends it, as the compilers bind it. The update's value is
		// discarded: its last statement's node j++ yields the statement
		// expression's result, which no node reads, so the update makes no
		// node of its own. Nodes: n@10, j = 0@19, n@33 (the while's Branch),
		// j < n@45 (the for's Branch), g(0, j)@82, j > 5@59, break;@66,
		// j++@73, return j;@93. Succ: j = 0→n@33→{j < n, return j};
		// j < n→{g(0, j), n@33}; g(0, j)→j > 5→{break, j++}; break→return j;
		// j++→j < n. IPDom: n@33, j < n, j > 5, break → return j; g(0, j) →
		// j > 5; j++ → j < n. Frontier walks: n@33→j < n gives j < n;
		// j < n→g(0, j) gives g(0, j), j > 5; j < n→n@33 gives n@33;
		// j > 5→break gives break; j > 5→j++ gives j++, j < n.
		name:     "a break in a for update's statement expression leaves the enclosing loop",
		protects: "a break in a statement expression in a for loop's update binds to the loop enclosing that loop and lands after it",
		mutation: "lower the update with the for loop's frame open (break;@66 ends the for loop only and reaches n@33: loses j < n@45 -> n@33), or end the discarded update with a node Using its result (adds { if (j > 5) break; j++; }@53, controlled by j > 5@59 and pairing with j++@73)",
		src:      "int f(int n) { int j = 0; while (n) { for (; j < n; ({ if (j > 5) break; j++; })) g(0, j); } return j; }",
		cd: []string{"n@33 -> j < n@45", "j < n@45 -> g(0, j)@82", "j < n@45 -> j > 5@59", "j < n@45 -> n@33",
			"j > 5@59 -> break;@66", "j > 5@59 -> j++@73", "j > 5@59 -> j < n@45"},
		du: []string{"n@10 -> n@33", "n@10 -> j < n@45", "j = 0@19 -> j < n@45", "j = 0@19 -> g(0, j)@82",
			"j = 0@19 -> j > 5@59", "j = 0@19 -> j++@73", "j = 0@19 -> return j;@93",
			"j++@73 -> j < n@45", "j++@73 -> g(0, j)@82", "j++@73 -> j > 5@59", "j++@73 -> j++@73",
			"j++@73 -> return j;@93"},
	},
	{
		// GNU C extension, Statements and Declarations in Expressions: the
		// last thing in the compound statement is an expression followed
		// by a semicolon, whose value is the value of the construct; the
		// statements before it are evaluated for their effect. Nodes:
		// y@10, z@17, g(0, y)@33 (Uses y), y = z@42 (Uses z, defines y and
		// the statement expression's result), x = ({ g(0, y);
		// y = z; })@26 (Uses the result, defines x), return x;@53. Succ:
		// a straight line to EXIT.
		name:     "a statement expression's value comes from its last statement's node",
		protects: "the node consuming a statement expression reaches its value through the last statement's node, and reads nothing an earlier statement read",
		mutation: "let the declarator re-read the statement expression's names (adds y@10 -> x = ({ g(0, y); y = z; })@26 and z@17 -> x = ({ g(0, y); y = z; })@26), or lower the last statement for its effect only (loses y = z@42 -> x = ({ g(0, y); y = z; })@26)",
		src:      "int f(int y, int z) { int x = ({ g(0, y); y = z; }); return x; }",
		du: []string{"y@10 -> g(0, y)@33", "z@17 -> y = z@42", "y = z@42 -> x = ({ g(0, y); y = z; })@26",
			"x = ({ g(0, y); y = z; })@26 -> return x;@53"},
	},
	{
		// C17 §6.2.1p4: g, declared outside any block, has file scope, so
		// it is no variable of f, which returns long so that `return g;`
		// is valid. Nodes: y@18, g = y@23 (Uses y, defines
		// nothing), g += y@30 (Uses y only), g++@38 (Uses nothing), g =
		// y@48 (the embedded assignment: Uses y, defines the owned result),
		// h(0, g = y)@43 (Uses that result), h(0, &g)@56 (Uses and may-defines
		// nothing), return g;@66 (Uses nothing). Succ: a straight line to
		// EXIT.
		name:     "a name with file scope is no variable in any position",
		protects: "an assignment, compound assignment, update, embedded assignment, address-taking and read of a name that resolves to no variable make their nodes with their other operands' reads and never reach the builder as a variable",
		mutation: "drop read's filter (Builder.Use panics on g's -1 at g += y@30), def's (Builder.Def panics at g = y@23), or mayDef's (Builder.MayDef panics at h(0, &g)@56), or fold the embedded assignment into its consumer (loses g = y@48 -> h(0, g = y)@43)",
		src:      "int g; long f(int y) { g = y; g += y; g++; h(0, g = y); h(0, &g); return g; }",
		du:       []string{"y@18 -> g = y@23", "y@18 -> g += y@30", "y@18 -> g = y@48", "g = y@48 -> h(0, g = y)@43"},
	},
	{
		// C17 §6.10.1p6: a conditional without #else keeps no group when
		// A is undefined, so after the directive x is the #ifdef group's
		// variable in one build and the parameter in the other. Nodes:
		// x@10, A@24 (Branch), x = 1@32, g(0, x)@48 (Uses both variables),
		// x = 2@59 (a killing definition of both: whichever build is
		// taken, x names one of them and the write replaces it), return
		// x;@69 (after the block, the parameter, which x = 2 killed).
		// Succ: A→{x = 1, g(0, x)}; x = 1→g(0, x)→x = 2→return x. IPDom: A →
		// g(0, x).
		name:     "a name only one arm declares stands for both bindings after the directive",
		protects: "a read after a conditional whose other build keeps the enclosing binding sees both, and a write there kills both, so the enclosing variable's earlier value does not reach past it",
		mutation: "count a group without #else as having no empty arm, or bind the name to the arms' variable alone (loses x@10 -> g(0, x)@48 and x = 2@59 -> return x;@69, adds x@10 -> return x;@69), or let the write may-define the enclosing variable instead of killing it (adds x@10 -> x = 2@59 and x@10 -> return x;@69)",
		src:      "int f(int x) { {\n#ifdef A\n  int x = 1;\n#endif\n  g(0, x);\n  x = 2;\n } return x; }",
		cd:       []string{"A@24 -> x = 1@32"},
		du:       []string{"x@10 -> g(0, x)@48", "x = 1@32 -> g(0, x)@48", "x = 2@59 -> return x;@69"},
	},
	{
		// C17 §6.3.2.1p3 (an array evaluated as a call argument decays to
		// its address), §6.10.1p6. x is an array in the build without A
		// and an int in the build with it, so h(0, x) passes the array's
		// address in one build. Nodes: A@33 (Branch), x = 1@41, h(0, x)@57
		// (Uses both variables, may-defines both: the name is an array in
		// some build), return x[0];@68 (after the block, the array; a
		// subscript's operand does not decay). `int x[2];` makes no node.
		// Succ: A→{x = 1, h(0, x)}; x = 1→h(0, x)→return. IPDom: A → h(0, x).
		name:     "a name that is an array in some build decays to its address after the directive",
		protects: "a call after a conditional that passes a name which is an array in one build may-defines that array, so a later read of the array depends on the call",
		mutation: "test only the arms' variable for an array shape (h(0, x)@57 no longer may-defines the enclosing array: loses h(0, x)@57 -> return x[0];@68)",
		src:      "int f(void) { int x[2]; {\n#ifdef A\n  int x = 1;\n#endif\n  h(0, x); } return x[0]; }",
		cd:       []string{"A@33 -> x = 1@41"},
		du:       []string{"x = 1@41 -> h(0, x)@57", "h(0, x)@57 -> return x[0];@68"},
	},
	{
		// GNU C extension, Statements and Declarations in Expressions:
		// when the last statement is a comma expression, its left
		// operand is a statement and its right operand the value. Nodes: x@10, y@17,
		// g(0, x)@33, y@42 (defining the construct's result), z = ({ g(0, x),
		// y; })@26 (Uses it), return z;@49.
		name:     "a comma expression as a statement expression's last statement yields its right operand",
		protects: "only the right operand of a last comma statement is the value, and the left operand is a statement of its own",
		mutation: "yield the whole comma expression (one node g(0, x), y@33 Using x and y replaces g(0, x)@33 and y@42: loses y@17 -> y@42)",
		src:      "int f(int x, int y) { int z = ({ g(0, x), y; }); return z; }",
		du: []string{"x@10 -> g(0, x)@33", "y@17 -> y@42", "y@42 -> z = ({ g(0, x), y; })@26",
			"z = ({ g(0, x), y; })@26 -> return z;@49"},
	},
	{
		// GNU C extension, Conditionals with Omitted Operands: `a ?: b`
		// is a when a is nonzero, evaluated once, else b. Nodes: a@10,
		// b@17, a@30 (Branch, defining the result), b@35 (defining it on
		// the path that evaluates it), r = a ?: b@26 (Uses the result),
		// return r;@38. Succ: a@30→{b@35,
		// r = a ?: b}; b@35→r = a ?: b.
		name:     "a two-operand conditional yields its condition or its alternative",
		protects: "in a ?: b only b is conditional, and the condition's node yields the value when it is nonzero",
		mutation: "give the condition's node no definition of the result (loses a@30 -> r = a ?: b@26)",
		src:      "int f(int a, int b) { int r = a ?: b; return r; }",
		cd:       []string{"a@30 -> b@35"},
		du: []string{"a@10 -> a@30", "b@17 -> b@35", "a@30 -> r = a ?: b@26", "b@35 -> r = a ?: b@26",
			"r = a ?: b@26 -> return r;@38"},
	},
	{
		// C17 §6.7.9p10 ([dcl.init]/12 alike): a declarator without an
		// initializer leaves the value indeterminate and defines nothing.
		// Nodes: x@10, x@26 (Branch), y = 1@29, return y;@36; `int y;`
		// makes no node, so on the path that skips y = 1 no definition of
		// y reaches the return.
		name:     "a declarator without an initializer defines nothing",
		protects: "an uninitialized local has no definition for a later read to pair with",
		mutation: "make `int y;` a defining node (adds y@19 -> return y;@36)",
		src:      "int f(int x) { int y; if (x) y = 1; return y; }",
		cd:       []string{"x@26 -> y = 1@29"},
		du:       []string{"x@10 -> x@26", "y = 1@29 -> return y;@36"},
	},
}

// TestCLoweringGolden pins the C lowering's control-dependence and def-use
// pairs on hand-derived functions. A wrong pair here is a wrong dependence
// fact served to every consumer. Offsets are byte offsets into src. The
// derivation and rendering rules are runGolden's. The cases of cShared run
// under the C and the C++ grammar; the others under C only.
func TestCLoweringGolden(t *testing.T) {
	runGolden(t, "c", append(slices.Clone(cShared), []goldenCase{
		{
			// C17 §6.9.1p6: in an old-style definition the identifier list
			// names the parameters and the declaration list gives their
			// types (C only; C++ has no identifier list). Nodes: a@6 (the
			// parameter's node, spanning its identifier in the list),
			// return a;@18. The declaration `int a;` makes no node.
			name:     "an old-style definition's identifier list names its parameters",
			protects: "a parameter named in an identifier list is defined at entry, and the declaration list does not redeclare it as a local",
			mutation: "skip identifier-list parameters (loses a@6 -> return a;@18)",
			src:      "int f(a) int a; { return a; }",
			du:       []string{"a@6 -> return a;@18"},
		},
		{
			// C17 §6.5.3.2p3: &s.f and &a[0] are addresses into s and a, so
			// the callee may write both (C only: in C++ `struct S s;` is also
			// a default-initialization node). Nodes: g(&s.f, &a[0])@36 (Uses
			// and may-defines s and a), h(0, s.f)@52, return a[1];@63. No
			// killing definition of s or a exists, so each read pairs with
			// the call alone.
			name:     "the address of a member or an element may-defines its base",
			protects: "taking the address of a field or an element reaches later reads of the whole variable",
			mutation: "take no base through &s.f (loses g(&s.f, &a[0])@36 -> h(0, s.f)@52) or through &a[i] (loses g(&s.f, &a[0])@36 -> return a[1];@63)",
			src:      "int f(void) { struct S s; int a[2]; g(&s.f, &a[0]); h(0, s.f); return a[1]; }",
			du:       []string{"g(&s.f, &a[0])@36 -> h(0, s.f)@52", "g(&s.f, &a[0])@36 -> return a[1];@63"},
		},
		{
			// GNU C extension, Nested Functions: a nested function is its
			// own function, and the enclosing one sees its definition as the
			// node that creates it, which reads the variables it uses. Nodes:
			// x@10, int g(void) { return x + 1; }@15 (the creating node,
			// Using x), return g();@45.
			name:     "a nested function's creating node reads its captures",
			protects: "the enclosing function sees a nested definition as one node whose reads are the enclosing variables the body uses",
			mutation: "give the creating node no reads (loses x@10 -> int g(void) { return x + 1; }@15)",
			src:      "int f(int x) { int g(void) { return x + 1; } return g(); }",
			du:       []string{"x@10 -> int g(void) { return x + 1; }@15"},
		},
		{
			// GNU C extension, Nested Functions: the nested function reaches
			// enclosing variables by reference, so its write to x may change
			// the x f reads later; the may-definition sits on the creating
			// node. Nodes: x@10, int g(void) { return x++; }@15 (Uses and
			// may-defines x), g();@43, return x;@48.
			name:     "a nested function's write is a may-definition on its creating node",
			protects: "a read after a nested function that writes an enclosing variable sees the write the calls may make",
			mutation: "treat the nested function's accesses as by copy (loses int g(void) { return x++; }@15 -> return x;@48)",
			src:      "int f(int x) { int g(void) { return x++; } g(); return x; }",
			du: []string{"x@10 -> int g(void) { return x++; }@15", "x@10 -> return x;@48",
				"int g(void) { return x++; }@15 -> return x;@48"},
		},
		{
			// C17 §6.8.3p2: an expression statement is evaluated for its
			// effect, and §6.5.13p4: && evaluates g(b) only when a is nonzero.
			// Nodes: a@10, b@17, a@22 (Branch, defining the && result), g(b)@27
			// (defining it again), return a;@33. The discarded result makes no
			// further node. Succ: a@22→{g(b), return a}; g(b)→return a. C
			// only: the C++ grammar reads `a && g(b);` as a declaration of g
			// ([stmt.ambig]); the C++ table holds its own case.
			name:     "a discarded && statement is its operands' nodes alone",
			protects: "an expression statement whose value nodes hand on a result nobody reads ends there, and the right operand still depends on the left",
			mutation: "end the statement with a node Using the result (adds a && g(b)@22, with a@22 -> a && g(b)@22 and g(b)@27 -> a && g(b)@22)",
			src:      "int f(int a, int b) { a && g(b); return a; }",
			cd:       []string{"a@22 -> g(b)@27"},
			du:       []string{"a@10 -> a@22", "b@17 -> g(b)@27", "a@10 -> return a;@33"},
		},
		{
			// C17 §6.7.6.2p5: a variable-length array's size is evaluated when
			// the declaration is reached (C only: C++ has no variable-length
			// array, [dcl.array]/1 requires a constant bound). Nodes: n@11,
			// a[n]@20 (a Stmt node reading n, defining nothing), g(0, a)@26 (a
			// decays: Uses and may-defines a, which no definition reaches).
			name:     "a variable-length array declarator reads its size at its own node",
			protects: "a declarator without an initializer still makes a node when it evaluates an array size, so the size's definitions reach it",
			mutation: "make no node for a declarator without an initializer (loses n@11 -> a[n]@20)",
			src:      "void f(int n) { int a[n]; g(0, a); }",
			du:       []string{"n@11 -> a[n]@20"},
		},
		{
			// C17 §6.5.3.4p2: sizeof of a variable-length array evaluates its
			// operand, and so does sizeof of a VLA type name, whose size
			// expression is read. g(0, a) may-define a (it decays), so the read
			// in sizeof a shows. Nodes: n@10, a[n]@19 (reads n), g(0, a)@25
			// (Uses and may-defines a), y = sizeof a@38 (Uses a), z =
			// sizeof(int[n])@56 (Uses n), return y + z;@76. C only, as C++
			// has no variable-length array.
			name:     "sizeof of a variable-length array reads it and a VLA type name reads its size",
			protects: "a VLA operand of sizeof is evaluated, unlike every other sizeof operand",
			mutation: "treat every sizeof operand as unevaluated (loses g(0, a)@25 -> y = sizeof a@38 and n@10 -> z = sizeof(int[n])@56)",
			src:      "int f(int n) { int a[n]; g(0, a); int y = sizeof a; int z = sizeof(int[n]); return y + z; }",
			du: []string{"n@10 -> a[n]@19", "g(0, a)@25 -> y = sizeof a@38", "n@10 -> z = sizeof(int[n])@56",
				"y = sizeof a@38 -> return y + z;@76", "z = sizeof(int[n])@56 -> return y + z;@76"},
		},
		{
			// C17 §6.5.1.1p3: the controlling expression of a generic selection
			// is not evaluated, and the lowering reads every association since
			// the choice depends on types. Nodes: x@10, y@17, return
			// _Generic(x, int: y, default: 0);@22 (Uses y only). C only: C++
			// has no generic selection.
			name:     "a generic selection reads its associations and not its controlling expression",
			protects: "the controlling expression of _Generic is no read, while an association's expression is",
			mutation: "read the controlling expression (adds x@10 -> return _Generic(x, int: y, default: 0);@22)",
			src:      "int f(int x, int y) { return _Generic(x, int: y, default: 0); }",
			du:       []string{"y@17 -> return _Generic(x, int: y, default: 0);@22"},
		},
		{
			// C17 §6.7.6.1 and §6.7.6.2: in `char (*p)[4]` the declarator
			// nearest p is the pointer, so p is a pointer to an array and does
			// not decay. Nodes: (*p)[4] = 0@19 (defines p), g(0, p)@32 (Uses p,
			// no may-definition), return p != 0;@41. C only: the C++ grammar
			// reads `char (*p)[4] = 0;` as an assignment to a functional cast;
			// the C++ table holds its own case.
			name:     "a pointer to an array is no array",
			protects: "the declarator nearest the name decides its shape, so passing a pointer to an array is a plain read",
			mutation: "take any array declarator on the chain as the shape (p decays: adds g(0, p)@32 -> return p != 0;@41)",
			src:      "int f(void) { char (*p)[4] = 0; g(0, p); return p != 0; }",
			du:       []string{"(*p)[4] = 0@19 -> g(0, p)@32", "(*p)[4] = 0@19 -> return p != 0;@41"},
		},
		{
			// Ill-formed on purpose, and recovered: the parser skips the stray
			// `]`, wrapping it in an ERROR node it marks as an extra inside the
			// if's compound statement. No C statement kind the lowering does
			// not name reaches its default branch, so an ERROR node is the only
			// way to exercise it. Nodes: x@11, x@20 (Branch), ]@25 (a plain Stmt
			// node spanning the ERROR node), h()@29.
			name:      "an unknown node kind is a plain statement",
			protects:  "a node of a kind the lowering does not name, an error-recovered one included, still makes a Stmt node on its path",
			mutation:  "make no node for an unnamed kind, or drop an ERROR node the parser made an extra with the comments (either loses x@20 -> ]@25)",
			src:       "void f(int x) { if (x) { ] } h(); }",
			recovered: true,
			cd:        []string{"x@20 -> ]@25"},
			du:        []string{"x@11 -> x@20"},
		},
	}...))
	runGolden(t, "cpp", cShared)
}
