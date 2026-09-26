package worker

import "testing"

// TestJavaLoweringGolden pins the control-dependence and def-use pairs of
// hand-derived Java methods. Each source is one class; callable 0 in
// Functions preorder is its class body, so each case is callable 1, the
// method. Every pair was derived by hand from the lowering's documented
// granularity and the rules: exit augmentation adds an edge to Exit from
// every successor-less node and from the smallest-reverse-post-order member
// of each sink strongly connected component that cannot reach Exit; control
// dependence is the post-dominance frontier over the augmented graph with
// no entry-to-exit edge, so nothing depends on Entry and a loop head
// depends on itself; def-use pairs are (defining node, using node) with
// every φ resolved, and a Handler takes each predecessor's entry values.
// Section numbers cite The Java Language Specification.
func TestJavaLoweringGolden(t *testing.T) {
	runGolden(t, "java", []goldenCase{
		{
			// §15.28.1, §14.21 (yield). Nodes: x@20, c@31 (params), x@52
			// (selector), case 1@57, c@76 (while head), yield 2;@81,
			// yield 3;@92, x@114 (default arm), the declarator @40,
			// return y;@120. Succ: case 1→{c@76, x@114}; c@76→{yield 2,
			// yield 3}; both yields and x@114 → declarator → return. The
			// loop has no back edge: its body always yields. IPDom: case 1,
			// c@76 → declarator. Frontier walks: case 1 over c@76 and
			// x@114; c@76 over both yields. The declarator reads every
			// variable read inside the switch expression.
			name:     "a yield inside a loop leaves the switch expression, not the loop",
			protects: "yield breaks to the innermost switch expression through a loop inside the arm, so the statement after the loop runs only when the loop exits normally",
			mutation: "issue Break(\"\") at yield (yield 2 lands on yield 3: c@76 loses control of yield 3;@92 and case 1@57 gains it)",
			src:      "class A { int f(int x, boolean c) { int y = switch (x) { case 1 -> { while (c) { yield 2; } yield 3; } default -> x; }; return y; } }",
			fn:       1,
			cd: []string{"case 1@57 -> c@76", "case 1@57 -> x@114",
				"c@76 -> yield 2;@81", "c@76 -> yield 3;@92"},
			du: []string{"x@20 -> x@52", "x@20 -> case 1@57", "x@20 -> x@114", "c@31 -> c@76",
				"x@20 -> y = switch (x) { case 1 -> { while (c) { yield 2; } yield 3; } default -> x; }@40",
				"c@31 -> y = switch (x) { case 1 -> { while (c) { yield 2; } yield 3; } default -> x; }@40",
				"y = switch (x) { case 1 -> { while (c) { yield 2; } yield 3; } default -> x; }@40 -> return y;@120"},
		},
		{
			// §14.11.3 (colon form). Nodes: x@20, y = 0@29, x@44, case
			// 1@49, case 2@64, y = 1@57, y = 2@72, break;@79, y = 3@95,
			// return y;@104. Succ: case 1→{y = 1, case 2}; y = 1→y = 2 (fall
			// through); case 2→{y = 2, y = 3}; y = 2→break→return; y =
			// 3→return. IPDom: y = 1→y = 2→break→return; case 1, case 2 →
			// return. Frontier walks: case 1 over y = 1, y = 2, break;
			// and case 2; case 2 over y = 2, break; and y = 3. y = 0 and
			// y = 1 are killed on every path.
			name:     "a colon-form case body falls through into the next body",
			protects: "a colon-form body without break continues into the next group's body, and each label is tested only when the previous failed",
			mutation: "end every colon-form body with a break (y = 1@57 reaches return y;@104 and case 1@57 loses y = 2@72 and break;@79)",
			src:      "class A { int f(int x) { int y = 0; switch (x) { case 1: y = 1; case 2: y = 2; break; default: y = 3; } return y; } }",
			fn:       1,
			cd: []string{"case 1@49 -> y = 1@57", "case 1@49 -> y = 2@72", "case 1@49 -> break;@79",
				"case 1@49 -> case 2@64", "case 2@64 -> y = 2@72", "case 2@64 -> break;@79", "case 2@64 -> y = 3@95"},
			du: []string{"x@20 -> x@44", "x@20 -> case 1@49", "x@20 -> case 2@64",
				"y = 2@72 -> return y;@104", "y = 3@95 -> return y;@104"},
		},
		{
			// §14.11.3 (arrow form). Nodes: x@20, y = 0@29, x@44, case
			// 1@49, case 2, 3@66 (one label, one test), y = 1@59, y = 2@81,
			// return y;@92. Succ: case 1→{y = 1, case 2, 3}; y = 1→return;
			// case 2, 3→{y = 2, return} (no default: the switch is left);
			// y = 2→return. IPDom: every node → return.
			name:     "an arrow-form arm leaves the switch statement",
			protects: "an arrow arm never falls through, and a switch statement without default is left when no label matches",
			mutation: "let an arrow arm fall into the next arm (y = 1@59 no longer reaches return y;@92)",
			src:      "class A { int f(int x) { int y = 0; switch (x) { case 1 -> y = 1; case 2, 3 -> { y = 2; } } return y; } }",
			fn:       1,
			cd:       []string{"case 1@49 -> y = 1@59", "case 1@49 -> case 2, 3@66", "case 2, 3@66 -> y = 2@81"},
			du: []string{"x@20 -> x@44", "x@20 -> case 1@49", "x@20 -> case 2, 3@66",
				"y = 0@29 -> return y;@92", "y = 1@59 -> return y;@92", "y = 2@81 -> return y;@92"},
		},
		{
			// §14.20.3.2. Nodes: a = open()@28 (acquires a), b =
			// a.next()@42 (acquires b; its call throws to a's finally),
			// use(b)@58 (throws to b's finally), then the closes in
			// reverse: b's Handler )@54, R b = a.next()@40 (the close of b,
			// which throws to a's finally), a's Handler ;@38, R a =
			// open()@26. Succ: b = a.next()→{use(b), ;@38}; use(b)→{R b,
			// )@54}; )@54→R b; R b→{R a, ;@38}; ;@38→R a; R a→EXIT. IPDom:
			// use(b), )@54 → R b; R b, ;@38, b = a.next() → R a. Frontier
			// walks: b = a.next() over use(b), R b and ;@38; use(b) over
			// )@54; R b over ;@38. A Handler carries its predecessors' entry
			// values, so both closes see the acquiring definitions.
			name:     "try-with-resources closes every resource in reverse order on every exit",
			protects: "the resource acquired last is closed first, a failing acquisition closes only the resources acquired before it, and every close reads its resource",
			mutation: "close the resources in source order (R a = open()@26 precedes R b = a.next()@40), or open each resource's finally before its acquisition (b = a.next()@42 throws to its own close)",
			src:      "class A { void f() { try (R a = open(); R b = a.next()) { use(b); } } }",
			fn:       1,
			cd: []string{"b = a.next()@42 -> use(b)@58", "b = a.next()@42 -> R b = a.next()@40",
				"b = a.next()@42 -> ;@38", "use(b)@58 -> )@54", "R b = a.next()@40 -> ;@38"},
			du: []string{"a = open()@28 -> b = a.next()@42", "a = open()@28 -> R a = open()@26",
				"b = a.next()@42 -> use(b)@58", "b = a.next()@42 -> R b = a.next()@40"},
		},
		{
			// §14.20.1, §14.20 (multi-catch). Nodes: r = 0@24, r = g()@37
			// (throws), the Handler catch@48, E1 | E2@55 (one test), e@63,
			// r = 1@68, E3@84, e@87, r = 2@92, return r;@101. Succ: r =
			// g()→{return, catch}; catch→E1 | E2→{e@63, E3}; e@63→r =
			// 1→return; E3→{e@87, EXIT} (no clause matched: rethrown);
			// e@87→r = 2→return. IPDom: E1 | E2, E3, r = g() → EXIT; catch →
			// E1 | E2. Frontier walks: r = g() over return, catch and E1 |
			// E2; E1 | E2 over e@63, r = 1, return and E3; E3 over e@87,
			// r = 2 and return. r = 0 reaches the Handler, then is killed.
			name:     "a catch chain tests each clause in order and rethrows when none matches",
			protects: "a multi-catch is one type test, clauses are tested in order, and an exception no clause catches leaves the method",
			mutation: "let the last clause's no-match fall out of the try (E3@84 gains an edge to return r;@101 and loses its control of it)",
			src:      "class A { int f() { int r = 0; try { r = g(); } catch (E1 | E2 e) { r = 1; } catch (E3 e) { r = 2; } return r; } }",
			fn:       1,
			cd: []string{"r = g()@37 -> return r;@101", "r = g()@37 -> catch@48", "r = g()@37 -> E1 | E2@55",
				"E1 | E2@55 -> e@63", "E1 | E2@55 -> r = 1@68", "E1 | E2@55 -> return r;@101", "E1 | E2@55 -> E3@84",
				"E3@84 -> e@87", "E3@84 -> r = 2@92", "E3@84 -> return r;@101"},
			du: []string{"r = g()@37 -> return r;@101", "r = 1@68 -> return r;@101", "r = 2@92 -> return r;@101"},
		},
		{
			// §14.7, §14.15, §14.16. Nodes: n@21, i = 0@42, i < n@49
			// (outer head), i++@56, for@63 (the inner head, a Stmt: no
			// condition), i > 1@78, continue outer;@85, break outer;@101.
			// Succ: i < n→{for@63, EXIT}; for→i > 1→{continue outer, break
			// outer}; continue outer→i++→i < n; break outer→EXIT. IPDom:
			// for@63 → i > 1 → EXIT; continue outer → i++ → i < n.
			name:     "labelled break and continue target the labelled loop",
			protects: "continue outer and break outer leave the inner loop for the labelled outer loop's update and exit",
			mutation: "resolve a labelled jump to the innermost loop (continue outer;@85 re-enters for@63, which gains a self-dependence)",
			src:      "class A { void f(int n) { outer: for (int i = 0; i < n; i++) { for (;;) { if (i > 1) continue outer; break outer; } } } }",
			fn:       1,
			cd: []string{"i < n@49 -> for@63", "i < n@49 -> i > 1@78", "i > 1@78 -> continue outer;@85",
				"i > 1@78 -> i++@56", "i > 1@78 -> i < n@49", "i > 1@78 -> break outer;@101"},
			du: []string{"n@21 -> i < n@49", "i = 0@42 -> i < n@49", "i++@56 -> i < n@49",
				"i = 0@42 -> i > 1@78", "i++@56 -> i > 1@78", "i = 0@42 -> i++@56", "i++@56 -> i++@56"},
		},
		{
			// §15.27.2. Nodes: x@20, n = x@29, () -> g(n)@49 (the lambda:
			// uses its capture n), r = () -> g(n)@45, r.run()@61, return
			// n;@70. A capture is effectively final, so the lambda defines
			// nothing and n = x alone reaches return n.
			name:     "a lambda's capture is a use where it is created and never a definition",
			protects: "the lambda node and its declarator read the captured local, and the local's definition still reaches later uses unmerged",
			mutation: "record a capture as a may-definition (() -> g(n)@49 gains a pair to return n;@70), or leave captures unread (n = x@29 loses its pairs to the lambda and its declarator)",
			src:      "class A { int f(int x) { int n = x; Runnable r = () -> g(n); r.run(); return n; } }",
			fn:       1,
			du: []string{"x@20 -> n = x@29", "n = x@29 -> () -> g(n)@49", "n = x@29 -> r = () -> g(n)@45",
				"r = () -> g(n)@45 -> r.run()@61", "n = x@29 -> return n;@70"},
		},
		{
			// §15.26.1. Nodes: a@25, p@30, v@37, a[v] = v@42 (may-defines
			// a), p.q.r = v@52 (may-defines p, the local at the base),
			// this.s = v@63 (a field: defines nothing), return a == p;@75.
			// A may-definition kills nothing, so both the write and the
			// parameter reach the return.
			name:     "a write through an array element or field of a local may-defines the local",
			protects: "an element or field write reaches later uses of its base local without killing the local's earlier definition, and a write to a field of this defines no local",
			mutation: "record no definition of the base (loses a[v] = v@42 and p.q.r = v@52 -> return a == p;@75), or a killing one (loses a@25 and p@30 -> return a == p;@75)",
			src:      "class A { Object f(int[] a, P p, int v) { a[v] = v; p.q.r = v; this.s = v; return a == p; } }",
			fn:       1,
			du: []string{"a@25 -> a[v] = v@42", "v@37 -> a[v] = v@42", "p@30 -> p.q.r = v@52", "v@37 -> p.q.r = v@52",
				"v@37 -> this.s = v@63", "a@25 -> return a == p;@75", "a[v] = v@42 -> return a == p;@75",
				"p@30 -> return a == p;@75", "p.q.r = v@52 -> return a == p;@75"},
		},
		{
			// §14.14.2. Nodes: xs@22, s = 0@32, xs@52 (the iterated
			// expression, evaluated once, defining the iteration variable),
			// int x : xs@44 (head), x@48 (the loop variable, on the body
			// path), s += x@56, return s;@64. Succ: xs@52→head→{x,
			// return}; x→s += x→head. IPDom: x → s += x → head → return.
			name:     "an enhanced for reads its iterated expression once and defines its variable on the body path",
			protects: "the head and the loop variable depend on the iterated expression as evaluated before the loop, and the loop variable is defined only when an element is taken",
			mutation: "define the loop variable on the head (the head renders as its definition and x@48 vanishes), or give the head no use of the iteration variable (loses xs@52 -> int x : xs@44)",
			src:      "class A { int f(int[] xs) { int s = 0; for (int x : xs) s += x; return s; } }",
			fn:       1,
			cd:       []string{"int x : xs@44 -> x@48", "int x : xs@44 -> s += x@56", "int x : xs@44 -> int x : xs@44"},
			du: []string{"xs@22 -> xs@52", "xs@52 -> int x : xs@44", "xs@52 -> x@48", "x@48 -> s += x@56",
				"s = 0@32 -> s += x@56", "s += x@56 -> s += x@56", "s = 0@32 -> return s;@64", "s += x@56 -> return s;@64"},
		},
		{
			// §14.10, §14.19. Nodes: o@24, x@31, x > 0@43 (the assertion),
			// x@51 (the message, on the false path, then a throw to EXIT),
			// o@68 (the monitor expression), g(x)@73. Succ: x > 0→{x@51,
			// o@68}; x@51→EXIT; o@68→g(x)→EXIT. IPDom: x > 0 → EXIT.
			name:     "a failed assertion evaluates its message and throws, and a synchronized block runs after its monitor",
			protects: "the code after an assert depends on the assertion holding, and the message is evaluated only when it fails",
			mutation: "lower assert as a plain statement (x > 0@43 controls nothing), or let the message fall through (x@51 gains an edge to o@68)",
			src:      "class A { void f(Object o, int x) { assert x > 0 : x; synchronized (o) { g(x); } } }",
			fn:       1,
			cd:       []string{"x > 0@43 -> x@51", "x > 0@43 -> o@68", "x > 0@43 -> g(x)@73"},
			du:       []string{"x@31 -> x > 0@43", "x@31 -> x@51", "o@24 -> o@68", "x@31 -> g(x)@73"},
		},
		{
			// §15.20.2, §6.3.2.2. Nodes: o@23, s@54 (the pattern variable,
			// defined from o), !(o instanceof String s)@32 (the condition),
			// return 0;@58, return s.length();@68. The pattern variable of a
			// negated test is in scope after the if whose then branch cannot
			// complete normally.
			name:     "an instanceof pattern variable is defined by the test and in scope after the if",
			protects: "a use of a pattern variable after an if that exits on the failed test resolves to the pattern's definition",
			mutation: "scope the pattern variable to the if statement (loses s@54 -> return s.length();@68)",
			src:      "class A { int f(Object o) { if (!(o instanceof String s)) return 0; return s.length(); } }",
			fn:       1,
			cd:       []string{"!(o instanceof String s)@32 -> return 0;@58", "!(o instanceof String s)@32 -> return s.length();@68"},
			du:       []string{"o@23 -> s@54", "o@23 -> !(o instanceof String s)@32", "s@54 -> return s.length();@68"},
		},
		{
			// §14.11.1, §15.28 (guards). Nodes: o@23, o@43 (selector), case
			// Integer i when i > 0@48 (the type test), i@61 (the pattern
			// variable), i > 0@68 (the guard), i@77 (the arm's yield), 0@91
			// (default), return …@28. Succ: case→{i@61, 0}; i@61→i >
			// 0→{i@77, 0}; i@77, 0 → return. IPDom: case, i > 0 → return;
			// i@61 → i > 0. Frontier walks: case over i@61, i > 0 and 0;
			// i > 0 over i@77 and 0. The return reads only o: every other
			// name inside the switch is its own pattern variable.
			name:     "a guarded pattern label falls to the next label when its guard fails",
			protects: "a guard is tested only after its pattern matched, its false edge joins the no-match path, and the pattern variable is defined before the guard reads it",
			mutation: "send a failed guard to the arm's body (i > 0@68 controls nothing), or bind the pattern variable after the guard (loses i@61 -> i > 0@68)",
			src:      "class A { int f(Object o) { return switch (o) { case Integer i when i > 0 -> i; default -> 0; }; } }",
			fn:       1,
			cd: []string{"case Integer i when i > 0@48 -> i@61", "case Integer i when i > 0@48 -> i > 0@68",
				"case Integer i when i > 0@48 -> 0@91", "i > 0@68 -> i@77", "i > 0@68 -> 0@91"},
			du: []string{"o@23 -> o@43", "o@23 -> case Integer i when i > 0@48", "o@23 -> i@61", "i@61 -> i > 0@68",
				"i@61 -> i@77", "o@23 -> return switch (o) { case Integer i when i > 0 -> i; default -> 0; };@28"},
		},
		{
			// §15.9.5, §8.3. Nodes: x@23, y@30, the creation new Object() {
			// … }@42 (uses its captures), the return @35. The anonymous
			// class's field x shadows the parameter x throughout its body,
			// so only y is captured.
			name:     "an anonymous class's field shadows an enclosing local of the same name",
			protects: "a name the anonymous class declares is resolved to its own member, so it is no capture of the enclosing local",
			mutation: "resolve names inside the class body against the enclosing scope only (x@23 pairs with the creation and the return)",
			src:      "class A { Object f(int x, int y) { return new Object() { int x = 1; int g() { return x + y; } }; } }",
			fn:       1,
			du: []string{"y@30 -> new Object() { int x = 1; int g() { return x + y; } }@42",
				"y@30 -> return new Object() { int x = 1; int g() { return x + y; } };@35"},
		},
		{
			// §15.27.2, §15.26.1. Nodes: arr = new int[1]@28, the lambda
			// () -> arr[0] = 1@59 (uses arr, may-defines it: its body writes
			// an element of the captured array), r = () -> arr[0] = 1@55
			// (reads arr inside its span), r.run()@77, return arr;@86. A
			// may-definition kills nothing, so a use of arr after the
			// creation pairs with the creation and with the definition before
			// it.
			name:     "a lambda's write through a captured local's element may-defines the local where it is created",
			protects: "a use of a captured array after the lambda's creation sees the element write the lambda may perform, and the array's earlier definition",
			mutation: "record no write through a captured local (loses () -> arr[0] = 1@59 -> return arr;@86), or a killing definition (loses arr = new int[1]@28 -> return arr;@86)",
			src:      "class A { int[] f() { int[] arr = new int[1]; Runnable r = () -> arr[0] = 1; r.run(); return arr; } }",
			fn:       1,
			du: []string{"arr = new int[1]@28 -> () -> arr[0] = 1@59", "arr = new int[1]@28 -> r = () -> arr[0] = 1@55",
				"() -> arr[0] = 1@59 -> r = () -> arr[0] = 1@55", "r = () -> arr[0] = 1@55 -> r.run()@77",
				"arr = new int[1]@28 -> return arr;@86", "() -> arr[0] = 1@59 -> return arr;@86"},
		},
		{
			// §14.13. Nodes: x@20, x--@30 (the body, entered first), x >
			// 0@44 (the condition, after the body), return x;@52. Succ:
			// x--→x > 0→{x--, return}. IPDom: x-- → x > 0 → return. The back
			// edge targets the body's first node, so x > 0 controls x-- and
			// itself; x-- kills the parameter before the condition reads x.
			name:     "a do statement runs its body before its condition",
			protects: "the first execution of a do body is unconditional and the condition reads the body's definition",
			mutation: "lower do as while (x > 0@44 comes first and x@20 -> x > 0@44 appears)",
			src:      "class A { int f(int x) { do { x--; } while (x > 0); return x; } }",
			fn:       1,
			cd:       []string{"x > 0@44 -> x--@30", "x > 0@44 -> x > 0@44"},
			du:       []string{"x@20 -> x--@30", "x--@30 -> x--@30", "x--@30 -> x > 0@44", "x--@30 -> return x;@52"},
		},
		{
			// §14.20.2, §14.15, §14.17. Nodes: x@20, x > 0@32 (head), x >
			// 5@51, return x;@58, x > 3@72, break;@79, x--@86, g(x)@103 (the
			// finally), return 0;@113. The return and the break are
			// intercepted by the finally and re-issued from it. Succ: x >
			// 0→{x > 5, return 0}; x > 5→{return x, x > 3}; x > 3→{break,
			// x--}; return x, break, x-- → g(x); g(x)→{x > 0, EXIT, return
			// 0}. IPDom: x > 5, x > 3, return x, break, x-- → g(x); g(x),
			// x > 0, return 0 → EXIT. Frontier walks: x > 0 over x > 5, g(x)
			// and return 0; x > 5 over return x and x > 3; x > 3 over break
			// and x--; g(x) over x > 0 and return 0.
			name:     "a finally inside a loop runs before a return leaves the method and before a break leaves the loop",
			protects: "a return and a break inside a try reach the finally with their definitions, and the finally then continues to EXIT and to the loop's exit",
			mutation: "send the break straight to the loop's exit (break;@79 bypasses g(x)@103, and x > 0@32 loses g(x)@103)",
			src:      "class A { int f(int x) { while (x > 0) { try { if (x > 5) return x; if (x > 3) break; x--; } finally { g(x); } } return 0; } }",
			fn:       1,
			cd: []string{"x > 0@32 -> x > 5@51", "x > 0@32 -> g(x)@103", "x > 0@32 -> return 0;@113",
				"x > 5@51 -> return x;@58", "x > 5@51 -> x > 3@72", "x > 3@72 -> break;@79", "x > 3@72 -> x--@86",
				"g(x)@103 -> x > 0@32", "g(x)@103 -> return 0;@113"},
			du: []string{"x@20 -> x > 0@32", "x@20 -> x > 5@51", "x@20 -> return x;@58", "x@20 -> x > 3@72",
				"x@20 -> x--@86", "x@20 -> g(x)@103", "x--@86 -> x > 0@32", "x--@86 -> x > 5@51",
				"x--@86 -> return x;@58", "x--@86 -> x > 3@72", "x--@86 -> x--@86", "x--@86 -> g(x)@103"},
		},
		{
			// §12.5, §8.3.2, §8.6. Callable 0, the class body: the field
			// initializer's nodes c@18 (Branch), f()@22, g()@28, then a = c
			// ? f() : g()@14, then the instance initializer's d@39 (Branch)
			// and h()@42, in source order. c and d are fields, so nothing is
			// a use. The static initializer is callable 1, not part of this
			// unit.
			name:     "a class body's unit lowers its field initializers and instance initializer blocks",
			protects: "the decisions inside a field initializer and an instance initializer block are lowered in the class body's unit, and a static initializer is not",
			mutation: "skip instance initializer blocks in the unit (loses d@39 -> h()@42), or lower the static initializer in the unit (s > 0@75 -> m(s)@82 appears here)",
			src:      "class A { int a = c ? f() : g(); { if (d) h(); } static { int s = k(); if (s > 0) m(s); } }",
			fn:       0,
			cd:       []string{"c@18 -> f()@22", "c@18 -> g()@28", "d@39 -> h()@42"},
		},
		{
			// §8.7, §12.4.2. Callable 1, the static initializer: s =
			// k()@62, s > 0@75, m(s)@82.
			name:     "a static initializer is its own callable",
			protects: "a static initializer's locals and decisions form a function of their own",
			mutation: "drop static_initializer from the callables (callable 1 does not exist)",
			src:      "class A { int a = c ? f() : g(); { if (d) h(); } static { int s = k(); if (s > 0) m(s); } }",
			fn:       1,
			cd:       []string{"s > 0@75 -> m(s)@82"},
			du:       []string{"s = k()@62 -> s > 0@75", "s = k()@62 -> m(s)@82"},
		},
		{
			// §14.3, §8.1.3, §6.4.1. Nodes: x@20, y@27, class L { … }@32 (the
			// local class declaration: uses its captures), return new
			// L().g(y);@75. g's parameter y shadows the enclosing y inside
			// L, so only x is captured.
			name:     "a local class declaration is a node that reads its captures, and its members shadow",
			protects: "a local class's captured locals are read where it is declared, and a name its method declares is not a capture",
			mutation: "make no node for a local class declaration (loses x@20 -> class L …), or resolve its body against the enclosing scope only (y@27 pairs with class L …)",
			src:      "class A { int f(int x, int y) { class L { int g(int y) { return x + y; } } return new L().g(y); } }",
			fn:       1,
			du: []string{"x@20 -> class L { int g(int y) { return x + y; } }@32",
				"y@27 -> return new L().g(y);@75"},
		},
		{
			// §8.10.4. Callable 0 is the record body, 1 the compact
			// constructor, whose parameters are the record components x@13
			// and y@20, spanning their identifiers in the header. Nodes: x <
			// 0@33, x = 0@40, y = x@47. y = x kills the parameter y.
			name:     "a compact constructor's parameters are its record's components",
			protects: "a use of a component name in a compact constructor resolves to the implicit parameter, which the body may reassign",
			mutation: "declare no parameters for a compact constructor (x@13 pairs with nothing)",
			src:      "record P(int x, int y) { P { if (x < 0) x = 0; y = x; } }",
			fn:       1,
			cd:       []string{"x < 0@33 -> x = 0@40"},
			du:       []string{"x@13 -> x < 0@33", "x@13 -> y = x@47", "x = 0@40 -> y = x@47"},
		},
	})
}
