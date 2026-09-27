package worker

import "testing"

// TestJavaLoweringGolden pins the control-dependence and def-use pairs of
// hand-derived Java methods. Each source is one class; callable 0 in
// Functions preorder is its class body, so each case is callable 1, the
// method. Every pair was derived by hand from the lowering's documented
// granularity and runGolden's derivation and rendering rules.
// Section numbers cite The Java Language Specification, Java SE 21 Edition.
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
			// x@114; c@76 over both yields. The selector x@52 Uses x and
			// defines the selector's variable, which case 1 Uses. Both
			// yields and the default arm's x@114 define the switch
			// expression's result, which the declarator Uses: its value
			// depends on c through the yields' control dependence on c@76.
			name:     "a yield inside a loop leaves the switch expression, not the loop",
			protects: "yield breaks to the innermost switch expression through a loop inside the arm, so the statement after the loop runs only when the loop exits normally, and every yield and arm result reaches the consuming node through the result variable",
			mutation: "issue Break(\"\") at yield (yield 2 lands on yield 3: c@76 loses control of yield 3;@92 and case 1@57 gains it, and yield 2;@81 -> y = switch (x) { … }@40 is lost), give the yields no definition of the result (loses yield 2;@81 and yield 3;@92 -> y = switch (x) { … }@40), or let the label read the selector's names (x@20 -> case 1@57 replaces x@52 -> case 1@57)",
			src:      "class A { int f(int x, boolean c) { int y = switch (x) { case 1 -> { while (c) { yield 2; } yield 3; } default -> x; }; return y; } }",
			fn:       1,
			cd: []string{"case 1@57 -> c@76", "case 1@57 -> x@114",
				"c@76 -> yield 2;@81", "c@76 -> yield 3;@92"},
			du: []string{"x@20 -> x@52", "x@52 -> case 1@57", "x@20 -> x@114", "c@31 -> c@76",
				"yield 2;@81 -> y = switch (x) { case 1 -> { while (c) { yield 2; } yield 3; } default -> x; }@40", "yield 3;@92 -> y = switch (x) { case 1 -> { while (c) { yield 2; } yield 3; } default -> x; }@40", "x@114 -> y = switch (x) { case 1 -> { while (c) { yield 2; } yield 3; } default -> x; }@40",
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
			// y = 1 are killed on every path. The selector x@44 defines the
			// selector's variable, which both labels Use.
			name:     "a colon-form case body falls through into the next body",
			protects: "a colon-form body without break continues into the next group's body, each label is tested only when the previous failed, and each label reads the selector's value, evaluated once",
			mutation: "end every colon-form body with a break (y = 1@57 reaches return y;@104 and case 1@49 loses y = 2@72 and break;@79), or let a label read the selector's names (x@20 -> case 1@49 replaces x@44 -> case 1@49)",
			src:      "class A { int f(int x) { int y = 0; switch (x) { case 1: y = 1; case 2: y = 2; break; default: y = 3; } return y; } }",
			fn:       1,
			cd: []string{"case 1@49 -> y = 1@57", "case 1@49 -> y = 2@72", "case 1@49 -> break;@79",
				"case 1@49 -> case 2@64", "case 2@64 -> y = 2@72", "case 2@64 -> break;@79", "case 2@64 -> y = 3@95"},
			du: []string{"x@20 -> x@44", "x@44 -> case 1@49", "x@44 -> case 2@64",
				"y = 2@72 -> return y;@104", "y = 3@95 -> return y;@104"},
		},
		{
			// §14.11.3 (arrow form). Nodes: x@20, y = 0@29, x@44, case
			// 1@49, case 2, 3@66 (one label, one test), y = 1@59, y = 2@81,
			// return y;@92. Succ: case 1→{y = 1, case 2, 3}; y = 1→return;
			// case 2, 3→{y = 2, return} (no default: the switch is left);
			// y = 2→return. IPDom: every node → return. Both labels Use the
			// variable the selector x@44 defines.
			name:     "an arrow-form arm leaves the switch statement",
			protects: "an arrow arm never falls through, and a switch statement without default is left when no label matches",
			mutation: "let an arrow arm fall into the next arm (y = 1@59 no longer reaches return y;@92), or let a label read the selector's names (x@20 -> case 2, 3@66 replaces x@44 -> case 2, 3@66)",
			src:      "class A { int f(int x) { int y = 0; switch (x) { case 1 -> y = 1; case 2, 3 -> { y = 2; } } return y; } }",
			fn:       1,
			cd:       []string{"case 1@49 -> y = 1@59", "case 1@49 -> case 2, 3@66", "case 2, 3@66 -> y = 2@81"},
			du: []string{"x@20 -> x@44", "x@44 -> case 1@49", "x@44 -> case 2, 3@66",
				"y = 0@29 -> return y;@92", "y = 1@59 -> return y;@92", "y = 2@81 -> return y;@92"},
		},
		{
			// §14.20.3.1. Nodes: a = open()@28 (acquires a), b =
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
			// uses its capture n and defines the owned result, the created
			// lambda), r = () -> g(n)@45 (Uses the result), r.run()@61,
			// return n;@70. A capture is effectively final, so the lambda
			// defines no local and n = x alone reaches return n.
			name:     "a lambda's capture is a use where it is created and never a definition",
			protects: "the lambda node reads the captured local and hands the created value to its declarator through the result variable, and the local's definition still reaches later uses unmerged",
			mutation: "record a capture as a may-definition (() -> g(n)@49 gains a pair to return n;@70), leave captures unread (n = x@29 loses its pair to the lambda), or let the declarator read the captures instead of the result (n = x@29 -> r = () -> g(n)@45 replaces () -> g(n)@49 -> r = () -> g(n)@45)",
			src:      "class A { int f(int x) { int n = x; Runnable r = () -> g(n); r.run(); return n; } }",
			fn:       1,
			du: []string{"x@20 -> n = x@29", "n = x@29 -> () -> g(n)@49", "() -> g(n)@49 -> r = () -> g(n)@45",
				"r = () -> g(n)@45 -> r.run()@61", "n = x@29 -> return n;@70"},
		},
		{
			// §15.26.1. Nodes: a@25, p@30, v@37, a[v] = v@42 (may-defines
			// a), p.q.r = v@52 (may-defines p, the local at the base),
			// this.s = v@63 (a field: defines nothing), return a == p;@75.
			// Each write is a χ of its base: the return pairs with it, the
			// nearest may-definition, and with the parameter, the killing
			// definition that reaches it through the write.
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
			// §14.10, §14.19. Nodes: o@24, x@31, assert@36 (whether
			// assertions are enabled), x > 0@43 (the assertion), x@51 (the
			// message, on the false path, then a throw to EXIT), o@68 (the
			// monitor expression), g(x)@73. Succ: assert→{x > 0, o@68} (a
			// disabled assertion evaluates nothing); x > 0→{x@51, o@68};
			// x@51→EXIT; o@68→g(x)→EXIT. IPDom: assert, x > 0 → EXIT; o@68
			// → g(x) → EXIT. Frontier walks: assert over x > 0, o@68 and
			// g(x); x > 0 over x@51, o@68 and g(x).
			name:     "an assertion runs only when enabled, and a failed one evaluates its message and throws; a synchronized block runs after its monitor",
			protects: "a disabled assertion skips the statement, so the code after it does not depend on the assertion alone, and the message is evaluated only when it fails",
			mutation: "lower assert as always enabled (assert@36 vanishes with its three control dependences), lower it as a plain statement (x > 0@43 controls nothing), or let the message fall through (x@51 gains an edge to o@68)",
			src:      "class A { void f(Object o, int x) { assert x > 0 : x; synchronized (o) { g(x); } } }",
			fn:       1,
			cd: []string{"assert@36 -> x > 0@43", "assert@36 -> o@68", "assert@36 -> g(x)@73",
				"x > 0@43 -> x@51", "x > 0@43 -> o@68", "x > 0@43 -> g(x)@73"},
			du: []string{"x@31 -> x > 0@43", "x@31 -> x@51", "o@24 -> o@68", "x@31 -> g(x)@73"},
		},
		{
			// §15.20.2, §6.3.2.2. Nodes: o@23, o@34 (the tested value,
			// evaluated once: Uses o, defines an owned variable), s@54 (the
			// pattern variable, defined from that variable), !(o instanceof
			// String s)@32 (the condition, the test folded into it: Uses the
			// variable), return 0;@58, return s.length();@68. The pattern
			// variable of a negated test is in scope after the if whose then
			// branch cannot complete normally.
			name:     "an instanceof pattern variable is defined by the test and in scope after the if",
			protects: "a use of a pattern variable after an if that exits on the failed test resolves to the pattern's definition, and the tested value is read once, at its own node",
			mutation: "scope the pattern variable to the if statement (loses s@54 -> return s.length();@68), or let the binding and the test read the tested expression's names (o@23 -> s@54 and o@23 -> !(o instanceof String s)@32 replace the o@34 pairs)",
			src:      "class A { int f(Object o) { if (!(o instanceof String s)) return 0; return s.length(); } }",
			fn:       1,
			cd:       []string{"!(o instanceof String s)@32 -> return 0;@58", "!(o instanceof String s)@32 -> return s.length();@68"},
			du: []string{"o@23 -> o@34", "o@34 -> s@54", "o@34 -> !(o instanceof String s)@32",
				"s@54 -> return s.length();@68"},
		},
		{
			// §14.11.1, §15.28 (guards). Nodes: o@23, o@43 (selector), case
			// Integer i when i > 0@48 (the type test), i@61 (the pattern
			// variable), i > 0@68 (the guard), i@77 (the arm's yield), 0@91
			// (default), return …@28. Succ: case→{i@61, 0}; i@61→i >
			// 0→{i@77, 0}; i@77, 0 → return. IPDom: case, i > 0 → return;
			// i@61 → i > 0. Frontier walks: case over i@61, i > 0 and 0;
			// i > 0 over i@77 and 0. The selector o@43 defines the
			// selector's variable, which the label and the pattern variable
			// node Use. The arm result i@77, reading the arm's pattern
			// variable, and the default arm's 0@91 define the result the
			// return Uses.
			name:     "a guarded pattern label falls to the next label when its guard fails",
			protects: "a guard is tested only after its pattern matched, its false edge joins the no-match path, the pattern variable is defined before the guard reads it, and the arm result reading the pattern variable reaches the consuming node through the result variable",
			mutation: "send a failed guard to the arm's body (i > 0@68 controls nothing), bind the pattern variable after the guard (loses i@61 -> i > 0@68), or give an arrow arm's result node no definition of the result (loses i@77 and 0@91 -> return switch (o) { … };@28)",
			src:      "class A { int f(Object o) { return switch (o) { case Integer i when i > 0 -> i; default -> 0; }; } }",
			fn:       1,
			cd: []string{"case Integer i when i > 0@48 -> i@61", "case Integer i when i > 0@48 -> i > 0@68",
				"case Integer i when i > 0@48 -> 0@91", "i > 0@68 -> i@77", "i > 0@68 -> 0@91"},
			du: []string{"o@23 -> o@43", "o@43 -> case Integer i when i > 0@48", "o@43 -> i@61", "i@61 -> i > 0@68",
				"i@61 -> i@77", "i@77 -> return switch (o) { case Integer i when i > 0 -> i; default -> 0; };@28", "0@91 -> return switch (o) { case Integer i when i > 0 -> i; default -> 0; };@28"},
		},
		{
			// §15.9.5, §8.3. Nodes: x@23, y@30, the creation new Object() {
			// … }@42 (uses its captures, defines the owned result), the
			// return @35 (Uses the result). The anonymous class's field x
			// shadows the parameter x throughout its body, so only y is
			// captured.
			name:     "an anonymous class's field shadows an enclosing local of the same name",
			protects: "a name the anonymous class declares is resolved to its own member, so it is no capture of the enclosing local, and the created object reaches its consumer through the result variable",
			mutation: "resolve names inside the class body against the enclosing scope only (x@23 pairs with the creation), or let the return read the captures instead of the result (y@30 -> return new Object() { … };@35 replaces the creation's pair to it)",
			src:      "class A { Object f(int x, int y) { return new Object() { int x = 1; int g() { return x + y; } }; } }",
			fn:       1,
			du: []string{"y@30 -> new Object() { int x = 1; int g() { return x + y; } }@42",
				"new Object() { int x = 1; int g() { return x + y; } }@42 -> return new Object() { int x = 1; int g() { return x + y; } };@35"},
		},
		{
			// §15.27.2, §15.26.1. Nodes: arr = new int[1]@28, the lambda
			// () -> arr[0] = 1@59 (uses arr, may-defines it: its body writes
			// an element of the captured array), r = () -> arr[0] = 1@55
			// (Uses the owned result the lambda defines, not arr),
			// r.run()@77, return arr;@86. The creation is a χ of arr, so a
			// use of arr after it pairs with the creation, the nearest
			// may-definition, and with the killing definition before it.
			name:     "a lambda's write through a captured local's element may-defines the local where it is created",
			protects: "a use of a captured array after the lambda's creation sees the element write the lambda may perform, and the array's earlier definition, while the declarator consuming the lambda reads only its result",
			mutation: "record no write through a captured local (loses () -> arr[0] = 1@59 -> return arr;@86), a killing definition (loses arr = new int[1]@28 -> return arr;@86), or let the declarator read the captures instead of the result (gains arr = new int[1]@28 -> r = () -> arr[0] = 1@55)",
			src:      "class A { int[] f() { int[] arr = new int[1]; Runnable r = () -> arr[0] = 1; r.run(); return arr; } }",
			fn:       1,
			du: []string{"arr = new int[1]@28 -> () -> arr[0] = 1@59", "() -> arr[0] = 1@59 -> r = () -> arr[0] = 1@55",
				"r = () -> arr[0] = 1@55 -> r.run()@77",
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
			// and h()@42, in source order. c and d are fields, so they are
			// no use; the arms f()@22 and g()@28 define the conditional's
			// owned result, which the declarator Uses. The static
			// initializer is callable 1, not part of this unit.
			name:     "a class body's unit lowers its field initializers and instance initializer blocks",
			protects: "the decisions inside a field initializer and an instance initializer block are lowered in the class body's unit, and a static initializer is not",
			mutation: "skip instance initializer blocks in the unit (loses d@39 -> h()@42), lower the static initializer in the unit (s > 0@75 -> m(s)@82 appears here), or give a conditional's arms no definition of its result (loses f()@22 and g()@28 -> a = c ? f() : g()@14)",
			src:      "class A { int a = c ? f() : g(); { if (d) h(); } static { int s = k(); if (s > 0) m(s); } }",
			fn:       0,
			cd:       []string{"c@18 -> f()@22", "c@18 -> g()@28", "d@39 -> h()@42"},
			du:       []string{"f()@22 -> a = c ? f() : g()@14", "g()@28 -> a = c ? f() : g()@14"},
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
		{
			// §6.3, §14.11.3 (colon form). Nodes: x@20, x@33 (selector),
			// case 1@38, case 2@64, y = 1@50, break;@57, y = 2@72, return
			// y;@79, return 0;@91. Succ: case 1→{y = 1, case 2}; y =
			// 1→break→return 0; case 2→{y = 2, return 0} (no default, no
			// pattern: the switch is left); y = 2→return y→EXIT. IPDom:
			// case 1, case 2 → EXIT; y = 1 → break → return 0. Frontier
			// walks: case 1 over y = 1, break, return 0 and case 2; case 2
			// over y = 2, return y and return 0. The y of the second group is
			// the local the first group declared: its scope is the rest of the
			// switch block. Both labels Use the variable the selector x@33
			// defines.
			name:     "a local declared in a colon-form switch group is in scope in the groups after it",
			protects: "an assignment and a use of a local in a later statement group resolve to the local an earlier group declared",
			mutation: "truncate the scope at the end of each colon-form group (loses y = 2@72 -> return y;@79), or let a label read the selector's names (x@20 -> case 2@64 replaces x@33 -> case 2@64)",
			src:      "class A { int f(int x) { switch (x) { case 1: int y = 1; break; case 2: y = 2; return y; } return 0; } }",
			fn:       1,
			cd: []string{"case 1@38 -> y = 1@50", "case 1@38 -> break;@57", "case 1@38 -> return 0;@91",
				"case 1@38 -> case 2@64", "case 2@64 -> y = 2@72", "case 2@64 -> return y;@79", "case 2@64 -> return 0;@91"},
			du: []string{"x@20 -> x@33", "x@33 -> case 1@38", "x@33 -> case 2@64", "y = 2@72 -> return y;@79"},
		},
		{
			// §14.11.2, §14.11.3. S is a sealed interface permitting exactly
			// the records P and Q, declared elsewhere, so the switch is
			// exhaustive without default. Nodes: o@18, y = 0@27, o@42
			// (selector), case P p@47, p@54, case Q q@66, q@73, y = 1@59, y =
			// 2@78, return y;@87. Succ: case P→{p, case Q}; case Q→{q, EXIT}
			// (no label matched: MatchException); p→y = 1→return y; q→y =
			// 2→return y. IPDom: case P, case Q → EXIT; p → y = 1 → return y.
			// Frontier walks: case P over p, y = 1, return y and case Q; case
			// Q over q, y = 2 and return y. y = 0 is killed on every path to
			// the return. The labels and the pattern variable nodes Use the
			// variable the selector o@42 defines.
			name:     "a pattern switch statement without default throws when no label matches",
			protects: "an enhanced switch statement's no-match path leaves by a throw, so the statement after it depends on the labels and no definition before the switch reaches it unkilled",
			mutation: "leave a pattern switch statement without default through its no-match edge (gains y = 0@27 -> return y;@87, and return y;@87 loses its control dependences), or let a pattern variable node read the selector's names (o@18 -> p@54 replaces o@42 -> p@54)",
			src:      "class A { int f(S o) { int y = 0; switch (o) { case P p -> y = 1; case Q q -> y = 2; } return y; } }",
			fn:       1,
			cd: []string{"case P p@47 -> p@54", "case P p@47 -> y = 1@59", "case P p@47 -> return y;@87",
				"case P p@47 -> case Q q@66", "case Q q@66 -> q@73", "case Q q@66 -> y = 2@78", "case Q q@66 -> return y;@87"},
			du: []string{"o@18 -> o@42", "o@42 -> case P p@47", "o@42 -> p@54", "o@42 -> case Q q@66", "o@42 -> q@73",
				"y = 1@59 -> return y;@87", "y = 2@78 -> return y;@87"},
		},
		{
			// §6.3.2.5, §6.3.1.3, §6.3.1.5, §14.14.1.2. Nodes: o@23, o@37
			// (the tested value, the loop head's first node: Uses o, defines
			// an owned variable), s@57 (the pattern variable, defined from
			// it), !(o instanceof String s)@35 (the condition, Using it),
			// return s.length();@65. Succ: o@37→s→condition→{o@37 (back edge
			// through the empty body: the condition is evaluated again),
			// return}. IPDom: o@37 → s → condition → return. Frontier walk:
			// the condition over o@37, s and itself. The condition
			// introduces s when false and the body holds no break, so s is in
			// scope after the loop.
			name:     "a basic for condition's when-false pattern variable is in scope after a loop no break leaves",
			protects: "a use after the loop resolves to the pattern variable the negated test defines, while only the header's locals end with the loop",
			mutation: "truncate the condition's pattern variables with the header's locals at the loop's end (loses s@57 -> return s.length();@65), or send the back edge to the condition's Branch (loses !(o instanceof String s)@35 -> o@37 and -> s@57)",
			src:      "class A { int f(Object o) { for (; !(o instanceof String s);) {} return s.length(); } }",
			fn:       1,
			cd: []string{"!(o instanceof String s)@35 -> o@37", "!(o instanceof String s)@35 -> s@57",
				"!(o instanceof String s)@35 -> !(o instanceof String s)@35"},
			du: []string{"o@23 -> o@37", "o@37 -> s@57", "o@37 -> !(o instanceof String s)@35",
				"s@57 -> return s.length();@65"},
		},
		{
			// §6.3.1.1, §6.3.2.2, §8.3, §15.20.2. Nodes: o@33, o@42 (the
			// tested value: Uses o, defines an owned variable), s@62 (the
			// pattern variable, Using it), o instanceof String s@42 (the &&
			// left operand, the test folded into it: Uses it),
			// s.isEmpty()@67, o instanceof String s && s.isEmpty()@42 (the
			// if's condition), g(s)@82, return s.length();@90. s is in scope
			// in the && right operand and the then block; the if introduces
			// nothing (its condition has no when-false set), so the s of the
			// return is the field. The left operand's Branch and, on its true
			// path, s.isEmpty()@67 define the operator's owned result, which
			// the if's Branch Uses.
			name:     "a pattern variable is in scope only where its test is true, not after the if",
			protects: "a when-true pattern variable reaches the && right operand and the then statement, and a later use of the same name resolves to the field",
			mutation: "keep an instanceof pattern variable in scope for the rest of the block (gains s@62 -> return s.length();@90), let the if's Branch read the operands' names instead of the operator's result (o@42 and s@62 -> o instanceof String s && s.isEmpty()@42 replace the operands' pairs to it), or let the test read the tested expression's names (o@33 -> o instanceof String s@42 replaces o@42's pair to it)",
			src:      "class A { String s; int f(Object o) { if (o instanceof String s && s.isEmpty()) { g(s); } return s.length(); } }",
			fn:       1,
			cd:       []string{"o instanceof String s@42 -> s.isEmpty()@67", "o instanceof String s && s.isEmpty()@42 -> g(s)@82"},
			du: []string{"o@33 -> o@42", "o@42 -> s@62", "o@42 -> o instanceof String s@42", "o instanceof String s@42 -> o instanceof String s && s.isEmpty()@42",
				"s@62 -> s.isEmpty()@67", "s.isEmpty()@67 -> o instanceof String s && s.isEmpty()@42", "s@62 -> g(s)@82"},
		},
		{
			// §6.3.2.2, §6.4, §15.9.5. Nodes: o@26, s@36, the creation new
			// Object() { … }@48 (uses its captures), the return @41. Inside
			// g, the if introduces the negated test's s after it, since its
			// then statement cannot complete normally, so the s of g's last
			// return is g's pattern variable, which shadows the parameter s;
			// only o is captured. The creation defines the owned result the
			// return Uses.
			name:     "in nested code a pattern variable introduced after an if shadows an enclosing local",
			protects: "the capture walk applies the when-false set an if introduces, so a name it shadows is no capture",
			mutation: "drop a negated instanceof's pattern variable at the end of its if in the capture walk (s@36 pairs with the creation)",
			src:      "class A { Object f(Object o, String s) { return new Object() { int g() { if (!(o instanceof String s)) return 0; return s.length(); } }; } }",
			fn:       1,
			du: []string{"o@26 -> new Object() { int g() { if (!(o instanceof String s)) return 0; return s.length(); } }@48",
				"new Object() { int g() { if (!(o instanceof String s)) return 0; return s.length(); } }@48 -> return new Object() { int g() { if (!(o instanceof String s)) return 0; return s.length(); } };@41"},
		},
		{
			// §15.28.2, §15.26.1. Nodes: x@20, z = 0@29, x@52 (selector),
			// case 1@57, z = 5@69, yield 1;@76, 0@98 (default arm), the
			// declarator y = switch …@40, return y + z;@104. Succ: case
			// 1→{z = 5, 0}; z = 5→yield 1→declarator; 0→declarator;
			// declarator→return. yield 1 and the default arm's 0 define the
			// switch expression's result, which the declarator Uses; z = 5
			// is a statement of the arm, a node of its own, which writes z
			// without reading it. case 1 Uses the variable the selector x@52
			// defines.
			name:     "a plain assignment inside a switch expression is no read of its target by the consuming node",
			protects: "the node consuming a switch expression uses only its result variable, not what a statement inside an arm reads or writes",
			mutation: "let the consuming node take every variable referenced inside the switch expression, a plain assignment's target included (gains z = 0@29 -> y = switch (x) { … }@40)",
			src:      "class A { int f(int x) { int z = 0; int y = switch (x) { case 1 -> { z = 5; yield 1; } default -> 0; }; return y + z; } }",
			fn:       1,
			cd:       []string{"case 1@57 -> z = 5@69", "case 1@57 -> yield 1;@76", "case 1@57 -> 0@98"},
			du: []string{"x@20 -> x@52", "x@52 -> case 1@57",
				"yield 1;@76 -> y = switch (x) { case 1 -> { z = 5; yield 1; } default -> 0; }@40", "0@98 -> y = switch (x) { case 1 -> { z = 5; yield 1; } default -> 0; }@40",
				"z = 0@29 -> return y + z;@104", "z = 5@69 -> return y + z;@104",
				"y = switch (x) { case 1 -> { z = 5; yield 1; } default -> 0; }@40 -> return y + z;@104"},
		},
		{
			// §14.7, §14.15. Nodes: x@20, y = 0@29, x > 0@47, break
			// out;@54, y = 1@65, return y;@74. Succ: x > 0→{break out, y =
			// 1}; break out→return (the labelled block's end); y = 1→return.
			// IPDom: x > 0, break out, y = 1 → return.
			name:     "a labelled break leaves the labelled block it names",
			protects: "a break naming a block's label continues after the block, carrying the definitions before it",
			mutation: "resolve a labelled break only against loops and switches (break out;@54 is unresolved and loses y = 0@29 -> return y;@74)",
			src:      "class A { int f(int x) { int y = 0; out: { if (x > 0) break out; y = 1; } return y; } }",
			fn:       1,
			cd:       []string{"x > 0@47 -> break out;@54", "x > 0@47 -> y = 1@65"},
			du:       []string{"x@20 -> x > 0@47", "y = 0@29 -> return y;@74", "y = 1@65 -> return y;@74"},
		},
		{
			// §14.15, §14.20.2. Nodes: x@20, for@30 (the head, a Stmt: no
			// condition), x > 0@51, break out;@58, x--@69, g(x)@86 (the
			// finally), return x;@96. The break is intercepted by the
			// finally and re-issued from it to the labelled loop's exit.
			// Succ: for→x > 0→{break out, x--}; break out, x-- → g(x);
			// g(x)→{for, return x}. IPDom: for → x > 0 → g(x) → return x;
			// break out, x-- → g(x). Frontier walks: x > 0 over break out
			// and x--; g(x) over for, x > 0 and itself.
			name:     "a labelled break of a loop runs the finally it leaves",
			protects: "a labelled break inside a try reaches the finally with its definitions, and only the finally continues to the labelled loop's exit",
			mutation: "send a labelled break straight to its loop's exit (break out;@58 gains an edge to return x;@96, and g(x)@86 loses its control dependences)",
			src:      "class A { int f(int x) { out: for (;;) { try { if (x > 0) break out; x--; } finally { g(x); } } return x; } }",
			fn:       1,
			cd: []string{"x > 0@51 -> break out;@58", "x > 0@51 -> x--@69",
				"g(x)@86 -> for@30", "g(x)@86 -> x > 0@51", "g(x)@86 -> g(x)@86"},
			du: []string{"x@20 -> x > 0@51", "x--@69 -> x > 0@51", "x@20 -> x--@69", "x--@69 -> x--@69",
				"x@20 -> g(x)@86", "x--@69 -> g(x)@86", "x@20 -> return x;@96", "x--@69 -> return x;@96"},
		},
		{
			// §14.20.2. Nodes: r = 0@24, r = g()@37 (throws), the Handler
			// catch@48, E@55, e@57, r = 1@62, h(r)@81 (the finally), return
			// r;@89. Succ: r = g()→{catch, h(r)}; catch→E→{e, h(r)} (no
			// clause matched: the finally runs, then rethrows); e→r = 1→h(r);
			// h(r)→{return, EXIT}. IPDom: r = g(), E, r = 1 → h(r) → EXIT;
			// catch → E. The Handler carries r = g()'s entry value r = 0,
			// which the rethrow path brings to the finally.
			name:     "a catch clause's end and an unmatched exception both run the finally",
			protects: "the finally follows the try block, the catch body and the rethrow of an exception no clause catches, each with its definitions",
			mutation: "leave the catch body's end outside the finally (loses r = 1@62 -> h(r)@81), or rethrow an unmatched exception straight to EXIT (loses r = 0@24 -> h(r)@81 and r = 0@24 -> return r;@89)",
			src:      "class A { int f() { int r = 0; try { r = g(); } catch (E e) { r = 1; } finally { h(r); } return r; } }",
			fn:       1,
			cd: []string{"r = g()@37 -> catch@48", "r = g()@37 -> E@55", "E@55 -> e@57", "E@55 -> r = 1@62",
				"h(r)@81 -> return r;@89"},
			du: []string{"r = 0@24 -> h(r)@81", "r = g()@37 -> h(r)@81", "r = 1@62 -> h(r)@81",
				"r = 0@24 -> return r;@89", "r = g()@37 -> return r;@89", "r = 1@62 -> return r;@89"},
		},
		{
			// §15.25, §15.26.1. Nodes: x@20, x > 0@29 (the condition, a
			// Branch using x), 1@37 and 2@41 (the arms, each defining the
			// conditional's owned result), x = x > 0 ? 1 : 2@25 (Uses the
			// result, defines x), return x;@44. Succ: x > 0→{1, 2}; 1,
			// 2→the assignment→return. IPDom: x > 0, 1, 2 → the assignment
			// → return. Frontier walk: x > 0 over 1 and 2. The condition's
			// read of x is its own node's, so the assignment, which defines
			// x, does not Use x.
			name:     "a conditional hands its arms' values to its consumer, and its condition's reads stay on the condition",
			protects: "the node consuming a conditional depends on the arms through the result variable and not on the condition's variables, even when it defines the variable the condition read",
			mutation: "give the arms no definition of the result (loses 1@37 and 2@41 -> x = x > 0 ? 1 : 2@25), leave the condition's reads to the consumer (gains x@20 -> x = x > 0 ? 1 : 2@25), or let a defining node Use a variable its statement read on a nested node (gains x@20 -> x = x > 0 ? 1 : 2@25)",
			src:      "class A { int f(int x) { x = x > 0 ? 1 : 2; return x; } }",
			fn:       1,
			cd:       []string{"x > 0@29 -> 1@37", "x > 0@29 -> 2@41"},
			du: []string{"x@20 -> x > 0@29", "1@37 -> x = x > 0 ? 1 : 2@25", "2@41 -> x = x > 0 ? 1 : 2@25",
				"x = x > 0 ? 1 : 2@25 -> return x;@44"},
		},
		{
			// §15.26.1, §15.12.4.2. Nodes: o@19, a@26, o.f = a@33 (the
			// embedded assignment: Uses o and a, may-defines o, defines the
			// owned result), g(o.f = a)@31 (Uses the result). Succ: a
			// straight line. No local carries the assigned value to the
			// call, so only the result variable does.
			name:     "an embedded assignment to a field hands its value to its consumer through a result variable",
			protects: "a call whose argument assigns a field depends on the assignment node, and does not re-read the assignment's operands",
			mutation: "give an embedded assignment no result (loses o.f = a@33 -> g(o.f = a)@31), or leave its reads to the consumer (o@19 and a@26 -> g(o.f = a)@31 replace o.f = a@33 -> g(o.f = a)@31)",
			src:      "class A { void f(P o, int a) { g(o.f = a); } }",
			fn:       1,
			du:       []string{"o@19 -> o.f = a@33", "a@26 -> o.f = a@33", "o.f = a@33 -> g(o.f = a)@31"},
		},
		{
			// §15.25, §15.26.1. Nodes: c@24, x@31 (params), c@44 (the
			// condition), x = 1@49 (the first arm, which is its own
			// assignment node: it defines both x and the conditional's
			// result, each killing), 2@58 (the second arm, defining the
			// result), y = c ? (x = 1) : 2@40 (Uses the result, defines y), return x +
			// y;@61. Succ: c@44→{x = 1, 2}; both→the declarator→return.
			// IPDom: c@44, x = 1, 2 → the declarator → return. Frontier
			// walk: c@44 over x = 1 and 2.
			name:     "an arm that is an assignment defines both its local and the conditional's result",
			protects: "a node that assigns a local also yields the construct's value, both by killing definitions, and the local it assigns still reaches later uses",
			mutation: "give that arm no share of the result (loses x = 1@49 -> y = c ? (x = 1) : 2@40), or make its definition of x a may-definition (gains x@31 -> x = 1@49)",
			src:      "class A { int f(boolean c, int x) { int y = c ? (x = 1) : 2; return x + y; } }",
			fn:       1,
			cd:       []string{"c@44 -> x = 1@49", "c@44 -> 2@58"},
			du: []string{"c@24 -> c@44", "x = 1@49 -> y = c ? (x = 1) : 2@40", "2@58 -> y = c ? (x = 1) : 2@40",
				"x@31 -> return x + y;@61", "x = 1@49 -> return x + y;@61", "y = c ? (x = 1) : 2@40 -> return x + y;@61"},
		},
		{
			// §15.26.1, §15.7.1 (left operand first). Nodes: x = 1@36 and
			// x = 2@46 (each defines x and its own owned
			// result), y = (x = 1) + (x = 2)@31 (Uses both results, defines y), return
			// y;@54. Succ: a straight line. The declarator reads each
			// assignment's value, not x, so both assignments reach it,
			// though x = 2 kills x = 1's definition of x.
			name:     "two embedded assignments to one local each hand their value to the consumer",
			protects: "a consumer of several embedded assignments to the same local depends on every one of them, through their result variables rather than the local",
			mutation: "hand an embedded assignment to a local its value through the local (loses x = 1@36 -> y = (x = 1) + (x = 2)@31)",
			src:      "class A { int f() { int x; int y = (x = 1) + (x = 2); return y; } }",
			fn:       1,
			du:       []string{"x = 1@36 -> y = (x = 1) + (x = 2)@31", "x = 2@46 -> y = (x = 1) + (x = 2)@31", "y = (x = 1) + (x = 2)@31 -> return y;@54"},
		},
		{
			// §15.7.1, §15.26.1. Nodes: x@20, x = 1@38 (defines x; it
			// carries the declarator's earlier read of x: Uses x's value from
			// before it and defines an owned variable holding it, and
			// defines its own result), y = x + (x = 1)@29 (Uses both owned
			// variables, defines y), return y;@46. Succ: a straight line.
			// Java evaluates the left operand x before the assignment, so
			// that read sees the parameter, which reaches the declarator
			// through x = 1's carried value, not x = 1's definition of x.
			name:     "a read made before an embedded assignment redefines its variable sees the earlier value",
			protects: "the value a consumer read before an embedded assignment overwrote the variable reaches the consumer from the definition that preceded the assignment",
			mutation: "leave the declarator's read of x to pair with the nearest definition (loses x@20 -> x = 1@38, so the parameter no longer reaches the declarator)",
			src:      "class A { int f(int x) { int y = x + (x = 1); return y; } }",
			fn:       1,
			du:       []string{"x@20 -> x = 1@38", "x = 1@38 -> y = x + (x = 1)@29", "y = x + (x = 1)@29 -> return y;@46"},
		},
		{
			// §15.25, §15.7.1. Nodes: c@24, x@31 (params), c@49 (the
			// condition), x = 1@54 (the first arm: defines x and
			// the conditional's result), 0@63 (the second arm, defining the
			// result), y = x + (c ? (x = 1) : 0)@40 (Uses x and the result, defines y), return
			// y;@67. Succ: c@49→{x = 1, 0}; both→the declarator→return.
			// IPDom: c@49, x = 1, 0 → the declarator → return. Frontier walk:
			// c@49 over x = 1 and 0. The assignment runs only on one arm, so
			// the declarator keeps its read of x: it sees the parameter when
			// c is false and x = 1 when c is true.
			name:     "a read before an assignment in a conditional's arm stays on its consumer",
			protects: "an earlier read is handed to an embedded assignment only when the assignment runs whenever the read's consumer does, so the definition before it still reaches the consumer on the path that skips it",
			mutation: "hand the earlier read to the assignment unconditionally (loses x@31 -> y = x + (c ? (x = 1) : 0)@40, gains x@31 -> x = 1@54)",
			src:      "class A { int f(boolean c, int x) { int y = x + (c ? (x = 1) : 0); return y; } }",
			fn:       1,
			cd:       []string{"c@49 -> x = 1@54", "c@49 -> 0@63"},
			du: []string{"c@24 -> c@49", "x@31 -> y = x + (c ? (x = 1) : 0)@40", "x = 1@54 -> y = x + (c ? (x = 1) : 0)@40", "0@63 -> y = x + (c ? (x = 1) : 0)@40",
				"y = x + (c ? (x = 1) : 0)@40 -> return y;@67"},
		},
		{
			// JLS §6.5.6.1 (a simple name that is not a local variable or
			// parameter in scope names a field), §15.26.1, §15.26.2, §15.14.2,
			// §15.27.2. x and xs are fields, no variable of this function, in
			// every position a name takes: an assignment target, a compound
			// assignment, an increment, an embedded assignment, a name the
			// lambda captures (read and incremented there), and the base of an
			// element write, at statement level and inside the lambda. Nodes:
			// a@38 (the parameter), x = a@43, x += a@50, x++@58 (Uses and
			// defines nothing), x = a@65 (the embedded assignment: Uses a,
			// defines only its owned result), g(x = a)@63 (Uses the result),
			// () -> { x++; xs[0] = a; }@86 (the lambda: Uses its one capture
			// a, may-defines nothing, defines the created value), r = () -> {
			// x++; xs[0] = a; }@82 (Uses it), xs[0] = a@113 (Uses a,
			// may-defines nothing). Straight line: no control dependence.
			name:     "an assignment to a field named without this defines no local",
			protects: "a name that resolves to no local or parameter is state the pairs do not track in every position it takes: a write to it defines nothing, a read of it Uses nothing, a write through it may-defines nothing, and the lowering never treats it as a variable",
			mutation: "drop the -1 test in holds (earlier indexes before the start of the read table at x = a@43 and panics), in read (x += a@50 indexes it and panics), in def (x = a@43 reaches the builder's Def with -1 and panics), in assign's test of base (xs[0] = a@113 reaches MayDef with -1 and panics), or in cap's test of base (the lambda's xs[0] = a is recorded as a write and closure passes -1 to MayDef)",
			src:      "class A { int x; int[] xs; void f(int a) { x = a; x += a; x++; g(x = a); Runnable r = () -> { x++; xs[0] = a; }; xs[0] = a; } }",
			fn:       1,
			du: []string{"a@38 -> x = a@43", "a@38 -> x += a@50", "a@38 -> x = a@65", "x = a@65 -> g(x = a)@63",
				"a@38 -> () -> { x++; xs[0] = a; }@86", "() -> { x++; xs[0] = a; }@86 -> r = () -> { x++; xs[0] = a; }@82",
				"a@38 -> xs[0] = a@113"},
		},
		{
			// §15.23, §15.26.1. Nodes: c@28, d@39 (params), c@67 (the
			// deciding operand's Branch: Uses c, defines the operator's
			// owned result), b = d@73 (the second operand, its own
			// assignment node: Uses d, defines b, its own result and the
			// operator's result, each killing), y = c && (b = d)@63 (Uses
			// the operator's result, defines y), return y;@81. Succ: c@67→{b
			// = d, the declarator}; b = d→the declarator→return. IPDom:
			// c@67, b = d → the declarator. Frontier walk: c@67 over b = d.
			// On the true path b = d's definition of the result kills
			// c@67's, so the declarator pairs with c@67 only through the
			// false edge, and b = d reads no earlier value of the result.
			name:     "a second operand that assigns a local defines the operator's result by a killing definition",
			protects: "the yields of one construct's value never read one another: the operand evaluated after the deciding one replaces its value on that path",
			mutation: "may-define the operator's result on a node that also defines a local (gains c@67 -> b = d@73: the second operand would read the deciding operand's value through the χ)",
			src:      "class A { boolean f(boolean c, boolean d) { boolean b; boolean y = c && (b = d); return y; } }",
			fn:       1,
			cd:       []string{"c@67 -> b = d@73"},
			du: []string{"c@28 -> c@67", "d@39 -> b = d@73", "c@67 -> y = c && (b = d)@63", "b = d@73 -> y = c && (b = d)@63",
				"y = c && (b = d)@63 -> return y;@81"},
		},
		{
			// §8.4.3.1, §8.4.7. Callable 0 is the class body, 1 the abstract
			// method, which has no body: Entry → Exit, no parameter node.
			name:      "a method without a body is a callable that lowers to entry and exit",
			protects:  "a bodiless method stays one callable of its own, with no node and no pair",
			mutation:  "skip a bodiless method in Functions (callables becomes 1 and callable 1 does not exist)",
			src:       "abstract class A { abstract int f(int x); }",
			fn:        1,
			callables: 2,
		},
		{
			// §14.4.2, §14.9.2, §16. Nodes: c@24, c@40 (Branch), y = 1@43,
			// y = 2@55, return y;@62. The declarator int y makes no node.
			// Succ: c@40→{y = 1, y = 2}; both → return. IPDom: c@40, y = 1,
			// y = 2 → return. Frontier walk: c@40 over y = 1 and y = 2.
			name:     "an if with an else merges both branches",
			protects: "each branch's definition reaches the statement after the if, and each branch depends on the condition",
			mutation: "lower an if as if it had no else (y = 2@55 is not made: c@40 loses its control of it and y = 2@55 -> return y;@62 is lost), or let the then branch fall into the else (y = 1@43 -> return y;@62 is lost)",
			src:      "class A { int f(boolean c) { int y; if (c) y = 1; else y = 2; return y; } }",
			fn:       1,
			cd:       []string{"c@40 -> y = 1@43", "c@40 -> y = 2@55"},
			du:       []string{"c@24 -> c@40", "y = 1@43 -> return y;@62", "y = 2@55 -> return y;@62"},
		},
		{
			// §15.27.1, §15.27.2, §15.25. Callable 2, the lambda: its
			// inferred parameters a@68 and b@71, the condition a > b@77 (a
			// Branch), the arms a + n@85 and b@93 (each defining the
			// conditional's result), and the body's value node a > b ? a + n :
			// b@77 (Uses the result). n is the enclosing method's parameter,
			// no variable of the lambda, so a + n Uses a only.
			name:      "a lambda is a callable of its own, with its parameters and its expression body",
			protects:  "a lambda's inferred parameters are its own defining nodes and its expression body is lowered as one value, while the enclosing method's locals are no variables of it",
			mutation:  "drop lambda_expression from the callables (callables becomes 2 and callable 2 does not exist), declare no inferred parameters (loses every pair from a@68 and b@71), or end an expression body at its last node (the value node a > b ? a + n : b@77 vanishes with its two pairs)",
			src:       "class A { void f(int n) { java.util.function.IntBinaryOperator g = (a, b) -> a > b ? a + n : b; } }",
			fn:        2,
			callables: 3,
			cd:        []string{"a > b@77 -> a + n@85", "a > b@77 -> b@93"},
			du: []string{"a@68 -> a > b@77", "b@71 -> a > b@77", "a@68 -> a + n@85", "b@71 -> b@93",
				"a + n@85 -> a > b ? a + n : b@77", "b@93 -> a > b ? a + n : b@77"},
		},
		{
			// §14.12, §14.22. Nodes: x@20, true@32 (the head, a Stmt: no exit
			// edge), x > 0@44, break;@51, x++@58, return x;@65. Succ:
			// true→x > 0→{break, x++}; x++→true; break→return. Every path
			// from x > 0 to EXIT leaves through the break, so IPDom: true →
			// x > 0 → break → return; x++ → true. Frontier walk: x > 0 over
			// x++, true and itself; nothing controls the break.
			name:     "a while on the literal true has no exit edge and is left only by its break",
			protects: "the statement after while (true) is reached only through a break, so the break post-dominates the loop's decision and the literal controls nothing",
			mutation: "lower the literal true as a Branch with an exit edge (gains true@32 -> x > 0@44 and x > 0@44 -> break;@51, and loses x > 0@44 -> x > 0@44)",
			src:      "class A { int f(int x) { while (true) { if (x > 0) break; x++; } return x; } }",
			fn:       1,
			cd:       []string{"x > 0@44 -> x++@58", "x > 0@44 -> true@32", "x > 0@44 -> x > 0@44"},
			du: []string{"x@20 -> x > 0@44", "x++@58 -> x > 0@44", "x@20 -> x++@58", "x++@58 -> x++@58",
				"x@20 -> return x;@65", "x++@58 -> return x;@65"},
		},
		{
			// §14.16, §14.12. Nodes: x@20, x > 0@32 (head), x--@41, x ==
			// 2@50, continue;@58, g(x)@68, return x;@76. Succ: x > 0→{x--,
			// return}; x--→x == 2→{continue, g(x)}; continue, g(x) → x > 0.
			// IPDom: x > 0 → return; x-- → x == 2 → x > 0; continue, g(x) →
			// x > 0. Frontier walks: x > 0 over x--, x == 2 and itself; x == 2
			// over continue; and g(x).
			name:     "an unlabelled continue re-enters the innermost loop's condition",
			protects: "a continue skips the rest of the body and evaluates the condition again, so the skipped statement depends on the test guarding the continue",
			mutation: "resolve an unlabelled continue to no loop (unresolved becomes 1 and continue;@58 loses its edge to x > 0@32)",
			src:      "class A { int f(int x) { while (x > 0) { x--; if (x == 2) continue; g(x); } return x; } }",
			fn:       1,
			cd: []string{"x > 0@32 -> x--@41", "x > 0@32 -> x == 2@50", "x > 0@32 -> x > 0@32",
				"x == 2@50 -> continue;@58", "x == 2@50 -> g(x)@68"},
			du: []string{"x@20 -> x > 0@32", "x--@41 -> x > 0@32", "x@20 -> x--@41", "x--@41 -> x--@41",
				"x--@41 -> x == 2@50", "x--@41 -> g(x)@68", "x@20 -> return x;@76", "x--@41 -> return x;@76"},
		},
		{
			// §15.20.2. Nodes: o@24, o instanceof String@33 (the condition, the
			// test folded whole into it: Uses o), g()@54.
			name:     "an instanceof without a pattern is folded into its condition",
			protects: "a type test that binds nothing makes no node for its tested value, so the condition itself reads the tested local",
			mutation: "evaluate a pattern-less instanceof's tested value at a node of its own (o@24 -> o@33 replaces o@24 -> o instanceof String@33)",
			src:      "class A { void f(Object o) { if (o instanceof String) g(); } }",
			fn:       1,
			cd:       []string{"o instanceof String@33 -> g()@54"},
			du:       []string{"o@24 -> o instanceof String@33"},
		},
		{
			// §15.20.2, §14.4. Nodes: o@27, o@44 (the tested value: Uses o,
			// defines an owned variable), s@64 (the pattern variable, Using
			// it), b = o instanceof String s@40 (the deciding node, the
			// statement holding the test: Uses the owned variable, defines
			// b), return b;@67.
			name:     "an instanceof pattern in a declarator is decided by the declarator's node",
			protects: "outside a condition the node of the statement holding the test decides it and reads the tested value once through the owned variable",
			mutation: "let the declarator read the tested expression's names (o@27 -> b = o instanceof String s@40 replaces o@44 -> b = o instanceof String s@40)",
			src:      "class A { boolean f(Object o) { boolean b = o instanceof String s; return b; } }",
			fn:       1,
			du: []string{"o@27 -> o@44", "o@44 -> s@64", "o@44 -> b = o instanceof String s@40",
				"b = o instanceof String s@40 -> return b;@67"},
		},
		{
			// §14.11.1, §14.11.3 (colon form). Nodes: x@20, y = 0@29, x@44
			// (selector), case 1@49, case 2@57 (both labels of one group), y =
			// 1@65, break;@72, y = 2@88 (default), return y;@97. Succ: case
			// 1→{y = 1, case 2}; case 2→{y = 1, y = 2}; y = 1→break→return;
			// y = 2→return. IPDom: case 1, case 2 → return; y = 1 → break →
			// return. Frontier walks: case 1 over y = 1, break; and case 2;
			// case 2 over y = 1, break; and y = 2.
			name:     "several labels of one colon-form group each enter its body",
			protects: "a group listing several case labels is entered when any of them matches, each tested in order",
			mutation: "enter a group only from its last label (case 1@49 loses its control of y = 1@65 and break;@72)",
			src:      "class A { int f(int x) { int y = 0; switch (x) { case 1: case 2: y = 1; break; default: y = 2; } return y; } }",
			fn:       1,
			cd: []string{"case 1@49 -> y = 1@65", "case 1@49 -> break;@72", "case 1@49 -> case 2@57",
				"case 2@57 -> y = 1@65", "case 2@57 -> break;@72", "case 2@57 -> y = 2@88"},
			du: []string{"x@20 -> x@44", "x@44 -> case 1@49", "x@44 -> case 2@57",
				"y = 1@65 -> return y;@97", "y = 2@88 -> return y;@97"},
		},
		{
			// §14.11.3 (colon form). Nodes: x@20, y = 0@29, x@44 (selector),
			// case 1@49, y = 1@57, y++@73 (the default group, not last),
			// case 3@78, y += 3@86, return y;@96. Succ: case 1→{y = 1, case
			// 3}; y = 1→y++ (fall through); case 3→{y += 3, y++} (no label
			// matched: the default group); y++→y += 3 (fall through); y +=
			// 3→return. IPDom: case 1, case 3, y++ → y += 3; y = 1 → y++.
			// Frontier walks: case 1 over y = 1, y++ and case 3; case 3 over
			// y++. y++ is reached with y = 1's definition by the fall-through
			// and with y = 0's by the no-match edge.
			name:     "a default group not last is entered by the no-match edge and by fall-through, and falls through itself",
			protects: "the default group takes both the previous body's fall-through and the path on which no label matched, wherever it stands",
			mutation: "enter the default group only by the no-match edge (loses y = 1@57 -> y++@73), or only by fall-through (loses y = 0@29 -> y++@73)",
			src:      "class A { int f(int x) { int y = 0; switch (x) { case 1: y = 1; default: y++; case 3: y += 3; } return y; } }",
			fn:       1,
			cd: []string{"case 1@49 -> y = 1@57", "case 1@49 -> y++@73", "case 1@49 -> case 3@78",
				"case 3@78 -> y++@73"},
			du: []string{"x@20 -> x@44", "x@44 -> case 1@49", "x@44 -> case 3@78",
				"y = 0@29 -> y++@73", "y = 1@57 -> y++@73", "y = 0@29 -> y += 3@86", "y++@73 -> y += 3@86",
				"y += 3@86 -> return y;@96"},
		},
		{
			// §14.11.1, §14.11.1.1, §14.11.2 (a case null label makes the
			// switch enhanced, so it must be exhaustive: E is an enum whose one
			// constant P the labels cover), §14.11.3. Nodes: s@18, y = 0@27,
			// s@42 (selector), case null@47, y = 1@60, case P@67 (P is a
			// constant, no variable), y = 2@77, return y;@86. Succ: case
			// null→{y = 1, case P}; case P→{y = 2, EXIT} (no match:
			// MatchException); y = 1, y = 2 → return. IPDom: case null, case
			// P → EXIT; y = 1, y = 2 → return. Frontier walks: case null over
			// y = 1, return and case P; case P over y = 2 and return. The
			// enum's body is callable 2.
			name:     "a case null label makes a switch statement without default throw when no label matches",
			protects: "a switch statement with a null label is enhanced, so its no-match path leaves by a throw and the definition before the switch never reaches the statement after it",
			mutation: "take only a pattern label as making the switch enhanced (gains y = 0@27 -> return y;@86, and return y;@86 loses its control dependences)",
			src:      "class A { int f(E s) { int y = 0; switch (s) { case null -> y = 1; case P -> y = 2; } return y; } enum E { P } }",
			fn:       1,
			cd: []string{"case null@47 -> y = 1@60", "case null@47 -> return y;@86", "case null@47 -> case P@67",
				"case P@67 -> y = 2@77", "case P@67 -> return y;@86"},
			du: []string{"s@18 -> s@42", "s@42 -> case null@47", "s@42 -> case P@67",
				"y = 1@60 -> return y;@86", "y = 2@77 -> return y;@86"},
		},
		{
			// §15.28.1, §15.28.2. S is a sealed interface permitting exactly
			// P and Q, declared elsewhere. Nodes: o@18, y = 0@27, o@46
			// (selector), case P p@51, p@58, 1@63 (arm result), case Q q@66,
			// q@73, 2@78, y = switch (o) { … }@34 (Uses the result, defines
			// y), return y;@84. Succ: case P→{p, case Q}; p→1→assignment;
			// case Q→{q, EXIT} (no match: MatchException); q→2→assignment;
			// assignment→return. IPDom: case P, case Q → EXIT; p → 1 →
			// assignment → return. Frontier walks: case P over p, 1, the
			// assignment, return and case Q; case Q over q, 2, the
			// assignment and return.
			name:     "a switch expression without default throws when no label matches",
			protects: "an exhaustive switch expression's no-match path leaves by a throw, so its consumer depends on the labels and the definition before it is killed on every path",
			mutation: "leave a switch expression without default through its no-match edge (case Q q@66 loses its control of the assignment and return y;@84)",
			src:      "class A { int f(S o) { int y = 0; y = switch (o) { case P p -> 1; case Q q -> 2; }; return y; } }",
			fn:       1,
			cd: []string{"case P p@51 -> p@58", "case P p@51 -> 1@63",
				"case P p@51 -> y = switch (o) { case P p -> 1; case Q q -> 2; }@34", "case P p@51 -> return y;@84",
				"case P p@51 -> case Q q@66", "case Q q@66 -> q@73", "case Q q@66 -> 2@78",
				"case Q q@66 -> y = switch (o) { case P p -> 1; case Q q -> 2; }@34", "case Q q@66 -> return y;@84"},
			du: []string{"o@18 -> o@46", "o@46 -> case P p@51", "o@46 -> p@58", "o@46 -> case Q q@66", "o@46 -> q@73",
				"1@63 -> y = switch (o) { case P p -> 1; case Q q -> 2; }@34", "2@78 -> y = switch (o) { case P p -> 1; case Q q -> 2; }@34",
				"y = switch (o) { case P p -> 1; case Q q -> 2; }@34 -> return y;@84"},
		},
		{
			// §15.28, §15.7.1. Nodes: x@20, y@27, x@51 (selector), case 1@56,
			// 2@66 and 3@80 (arm results, defining the switch expression's
			// result), return y + switch (x) { … };@32 (Uses y, read before
			// the switch expression, and the result). Succ: case 1→{2, 3};
			// both → return. IPDom: case 1 → return. Frontier walk: case 1
			// over 2 and 3.
			name:     "a switch expression inside a larger expression keeps the enclosing statement's earlier reads",
			protects: "the reads the consuming node made before a nested switch expression survive the arms' statements, which reset the reads",
			mutation: "drop the enclosing statement's saved reads around a nested switch expression (loses y@27 -> return y + switch (x) { … };@32)",
			src:      "class A { int f(int x, int y) { return y + switch (x) { case 1 -> 2; default -> 3; }; } }",
			fn:       1,
			cd:       []string{"case 1@56 -> 2@66", "case 1@56 -> 3@80"},
			du: []string{"x@20 -> x@51", "x@51 -> case 1@56", "y@27 -> return y + switch (x) { case 1 -> 2; default -> 3; };@32",
				"2@66 -> return y + switch (x) { case 1 -> 2; default -> 3; };@32", "3@80 -> return y + switch (x) { case 1 -> 2; default -> 3; };@32"},
		},
		{
			// §15.14.2, §15.7.1. Nodes: i@20, i++@39 (the embedded update: Uses
			// i, defines i, the owned variable carrying the declarator's
			// earlier read of i, and its own result), y = i + g(i++)@29 (Uses
			// both owned variables, defines y), return y + i;@45. The earlier
			// read's hand-off and the update's own read of i pair alike here
			// (an update always reads its target), so the case pins the
			// update's own node and its definition of i.
			name:     "an embedded update is its own node, defining its local and handing its value on",
			protects: "an update inside a larger expression defines the local where it runs, and its consumer depends on it through its result",
			mutation: "fold an embedded update into its consumer (i++@39 vanishes, and i@20 -> y = i + g(i++)@29 and -> return y + i;@45 replace its pairs)",
			src:      "class A { int f(int i) { int y = i + g(i++); return y + i; } }",
			fn:       1,
			du: []string{"i@20 -> i++@39", "i++@39 -> y = i + g(i++)@29", "y = i + g(i++)@29 -> return y + i;@45",
				"i++@39 -> return y + i;@45"},
		},
		{
			// §15.23, §15.7.1. Nodes: c@28, x@39, c@62 (the && deciding operand's
			// Branch, defining the operator's result), x = true@68 (the right
			// operand's assignment: defines x, its own result and the
			// operator's), y = x == (c && (x = true))@52 (Uses x, read before
			// the operator, and the operator's result), return y;@80. Succ:
			// c@62→{x = true, the declarator}; x = true→the declarator. IPDom:
			// c@62 → the declarator. Frontier walk: c@62 over x = true.
			name:     "a read before an assignment in an && right operand stays on its consumer",
			protects: "an earlier read is not handed to an assignment the short circuit may skip, so the parameter still reaches the consumer on the path where c is false",
			mutation: "hand the earlier read to an assignment inside a right operand (loses x@39 -> y = x == (c && (x = true))@52, gains x@39 -> x = true@68)",
			src:      "class A { boolean f(boolean c, boolean x) { boolean y = x == (c && (x = true)); return y; } }",
			fn:       1,
			cd:       []string{"c@62 -> x = true@68"},
			du: []string{"c@28 -> c@62", "x@39 -> y = x == (c && (x = true))@52", "c@62 -> y = x == (c && (x = true))@52",
				"x = true@68 -> y = x == (c && (x = true))@52", "y = x == (c && (x = true))@52 -> return y;@80"},
		},
		{
			// §6.3, §14.14.1. y and z are fields of A. Nodes: n@30, y = 0@44
			// (the for header's local), y < n@51, y++@58, z = 1@72 (the
			// block's local), return y + z;@81. Succ: y < n→{y++ (empty body),
			// z = 1}; y++→y < n. IPDom: y < n → z = 1; y++ → y < n. Frontier
			// walk: y < n over y++ and itself. The header's y ends with the
			// loop and the block's z with the block, so the return reads the
			// fields: no pair.
			name:     "a local's scope ends with its for header and with its block",
			protects: "a use after a loop or block of a name its header or block declared resolves to what the name meant outside, not to the ended local",
			mutation: "keep a for header's locals in scope after the loop (gains y = 0@44 and y++@58 -> return y + z;@81), or a block's (gains z = 1@72 -> return y + z;@81)",
			src:      "class A { int y, z; int f(int n) { for (int y = 0; y < n; y++) {} { int z = 1; } return y + z; } }",
			fn:       1,
			cd:       []string{"y < n@51 -> y++@58", "y < n@51 -> y < n@51"},
			du: []string{"n@30 -> y < n@51", "y = 0@44 -> y < n@51", "y++@58 -> y < n@51", "y = 0@44 -> y++@58",
				"y++@58 -> y++@58"},
		},
		{
			// §14.6, §14.9. Nodes: c@25, c@34 (Branch), g()@39. The then
			// statement is the empty statement, which makes no node, so both
			// of c's edges reach g(), which post-dominates it.
			name:     "an empty statement as an if's body makes no node",
			protects: "an empty then statement executes nothing, so nothing depends on the condition",
			mutation: "lower the anonymous ; token as a statement (a node ;@37 appears and c@34 controls it)",
			src:      "class A { void f(boolean c) { if (c) ; g(); } }",
			fn:       1,
			du:       []string{"c@25 -> c@34"},
		},
		{
			// §14.6, §14.7, §14.9. Nodes: c@25, c@34 (Branch), g()@42. The then
			// statement is a labelled empty statement, which executes nothing
			// and makes no node, so both of c's edges reach g().
			name:     "a labelled empty statement makes no node",
			protects: "a label on an empty statement adds no node, so nothing depends on the condition",
			mutation: "lower a labelled statement without a statement child as one node (a node l: ;@37 appears and c@34 controls it)",
			src:      "class A { void f(boolean c) { if (c) l: ; g(); } }",
			fn:       1,
			du:       []string{"c@25 -> c@34"},
		},
		{
			// §15.8.5, §14.9. Nodes: c@25, c@36 (the condition, every pair of
			// parentheses stripped), g()@41.
			name:     "a condition spans its expression without any of its nested parentheses",
			protects: "a Branch node spans the condition with every enclosing pair of parentheses removed, however deeply nested",
			mutation: "strip only the if statement's own parentheses (the Branch renders as ((c))@34)",
			src:      "class A { void f(boolean c) { if (((c))) g(); } }",
			fn:       1,
			cd:       []string{"c@36 -> g()@41"},
			du:       []string{"c@25 -> c@36"},
		},
		{
			// §8.4.1. Nodes: xs@23 (the variable arity parameter), return
			// xs.length;@29 (Uses xs).
			name:     "a variable arity parameter is a defining node",
			protects: "a use of a varargs parameter pairs with the parameter's definition",
			mutation: "declare no variable for a spread parameter (loses xs@23 -> return xs.length;@29)",
			src:      "class A { int f(int... xs) { return xs.length; } }",
			fn:       1,
			du:       []string{"xs@23 -> return xs.length;@29"},
		},
		{
			// §8.8.7.1. Callable 1 is the first constructor. Nodes: x@16, this(x,
			// 0);@21 (the explicit constructor invocation, one node spanning
			// itself: Uses x), g(x)@33.
			name:     "an explicit constructor invocation is one node that reads its arguments",
			protects: "the arguments of this(…) are read at the invocation's own node",
			mutation: "make no node for an explicit constructor invocation (loses x@16 -> this(x, 0);@21)",
			src:      "class A { A(int x) { this(x, 0); g(x); } A(int x, int y) {} }",
			fn:       1,
			du:       []string{"x@16 -> this(x, 0);@21", "x@16 -> g(x)@33"},
		},
		{
			// §14.7, §14.15 (a break's target may be any labelled statement;
			// a continue naming a, which labels the labelled statement b: …,
			// not the loop, is rejected by §14.16). Nodes: n@21, i = 0@41, i
			// < n@48 (head), i > 1@66, break a;@73, g(i)@82, i++@55. Succ: i
			// < n→{i > 1, EXIT}; i > 1→{break a, g(i)}; break a→EXIT (the
			// loop's exit ends the method); g(i) → i++ → i < n. IPDom: i < n,
			// i > 1, break a → EXIT; g(i) → i++ → i < n. Frontier walks: i <
			// n over i > 1; i > 1 over break a;, and over g(i), i++ and i < n.
			name:     "every label of a loop labelled twice names that loop",
			protects: "a break naming the outer of two labels on one loop leaves that loop",
			mutation: "hand a loop only its innermost label (break a;@73 is unresolved: unresolved becomes 1)",
			src:      "class A { void f(int n) { a: b: for (int i = 0; i < n; i++) { if (i > 1) break a; g(i); } } }",
			fn:       1,
			cd: []string{"i < n@48 -> i > 1@66", "i > 1@66 -> break a;@73", "i > 1@66 -> g(i)@82",
				"i > 1@66 -> i++@55", "i > 1@66 -> i < n@48"},
			du: []string{"n@21 -> i < n@48", "i = 0@41 -> i < n@48", "i++@55 -> i < n@48",
				"i = 0@41 -> i > 1@66", "i++@55 -> i > 1@66", "i = 0@41 -> g(i)@82", "i++@55 -> g(i)@82",
				"i = 0@41 -> i++@55", "i++@55 -> i++@55"},
		},
		{
			// §14.11.1, §15.29. Nodes: x@20, k = 1@35, x@50 (selector), case
			// k@55 (Uses the selector's variable and its constant's read of
			// k), x = 0@65, x = 1@83 (default), return x;@92. Succ: case
			// k→{x = 0, x = 1}; both → return. IPDom: case k → return.
			name:     "a case label reads the constant variables it names",
			protects: "a label naming a constant variable depends on that variable's definition as well as on the selector's value",
			mutation: "let a label Use only the selector's variable (loses k = 1@35 -> case k@55)",
			src:      "class A { int f(int x) { final int k = 1; switch (x) { case k -> x = 0; default -> x = 1; } return x; } }",
			fn:       1,
			cd:       []string{"case k@55 -> x = 0@65", "case k@55 -> x = 1@83"},
			du: []string{"x@20 -> x@50", "x@50 -> case k@55", "k = 1@35 -> case k@55",
				"x = 0@65 -> return x;@92", "x = 1@83 -> return x;@92"},
		},
		{
			// §14.30.1, §15.20.2. Nodes: o@23, o@32 (the tested value: Uses o,
			// defines an owned variable), the pattern variables a@51 and b@58
			// (each Using it), o instanceof P(int a, int b)@32
			// (the condition, the test folded into it), return a + b;@62,
			// return 0;@76.
			name:     "a record pattern defines each component variable from the tested value",
			protects: "the component variables of a record pattern are defined on the match path from the value tested once, and are in scope in the then statement",
			mutation: "bind no variables in a record pattern's components (loses a@51 and b@58 -> return a + b;@62)",
			src:      "class A { int f(Object o) { if (o instanceof P(int a, int b)) return a + b; return 0; } }",
			fn:       1,
			cd:       []string{"o instanceof P(int a, int b)@32 -> return a + b;@62", "o instanceof P(int a, int b)@32 -> return 0;@76"},
			du: []string{"o@23 -> o@32", "o@32 -> a@51", "o@32 -> b@58", "o@32 -> o instanceof P(int a, int b)@32",
				"a@51 -> return a + b;@62", "b@58 -> return a + b;@62"},
		},
		{
			// §11.1.1, §14.20.1. Nodes: r = 0@24, r = g()@37 (throws), the
			// Handler catch@48, Throwable@55 (a Stmt: it catches everything),
			// t@65, r = 1@70, return r;@79. Succ: r = g()→{return, catch};
			// catch→Throwable→t→r = 1→return; no rethrow. IPDom: every node →
			// return. Frontier walk: r = g() over catch, Throwable, t and r =
			// 1.
			name:     "a clause catching Throwable ends the catch chain",
			protects: "an exception that reaches a Throwable clause is always caught, so the statement after the try depends on nothing in the chain",
			mutation: "test a Throwable clause like any other (Throwable@55 becomes a Branch with a rethrow edge: it gains control of t@65, r = 1@70 and return r;@79, and r = g()@37 gains return r;@79)",
			src:      "class A { int f() { int r = 0; try { r = g(); } catch (Throwable t) { r = 1; } return r; } }",
			fn:       1,
			cd: []string{"r = g()@37 -> catch@48", "r = g()@37 -> Throwable@55", "r = g()@37 -> t@65",
				"r = g()@37 -> r = 1@70"},
			du: []string{"r = g()@37 -> return r;@79", "r = 1@70 -> return r;@79"},
		},
		{
			// §14.20.3. Nodes: a@19, use(a)@34 (throws to the resource's
			// finally), the Handler )@30, a@29 (the close, Using the
			// expression's read of a). The resource names an existing
			// variable, so it acquires nothing. Succ: use(a)→{a@29, )@30};
			// )@30→a@29. IPDom: use(a), )@30 → a@29. Frontier walk: use(a)
			// over )@30.
			name:     "a resource naming an existing variable acquires nothing and its close reads the variable",
			protects: "try (a) makes no acquiring node and closes a on every exit, reading the variable at the close",
			mutation: "close it Using nothing (loses a@19 -> a@29)",
			src:      "class A { void f(R a) { try (a) { use(a); } } }",
			fn:       1,
			cd:       []string{"use(a)@34 -> )@30"},
			du:       []string{"a@19 -> use(a)@34", "a@19 -> a@29"},
		},
		{
			// §14.10. Nodes: x@21, assert@26, x > 0@33, g(x)@40. Succ:
			// assert→{x > 0, g(x)}; x > 0→{g(x), EXIT} (the failed assertion
			// throws from the false edge: no message node). IPDom: assert,
			// x > 0 → EXIT. Frontier walks: assert over x > 0 and g(x); x >
			// 0 over g(x).
			name:     "an assertion without a message throws from its condition's false edge",
			protects: "a failed assertion without a message leaves at once, so the statement after it depends on the assertion",
			mutation: "let the false edge of a message-less assertion fall through (x > 0@33 controls nothing)",
			src:      "class A { void f(int x) { assert x > 0; g(x); } }",
			fn:       1,
			cd:       []string{"assert@26 -> x > 0@33", "assert@26 -> g(x)@40", "x > 0@33 -> g(x)@40"},
			du:       []string{"x@21 -> x > 0@33", "x@21 -> g(x)@40"},
		},
		{
			// §15.13.3. Nodes: a@23, return a::g;@28 (the method reference is
			// folded, with its receiver's read of a).
			name:     "a method reference is folded with its receiver's reads",
			protects: "the receiver of a method reference is evaluated where the reference is, by the consuming node",
			mutation: "fold a method reference without its receiver's reads (loses a@23 -> return a::g;@28)",
			src:      "class A { Runnable f(A a) { return a::g; } }",
			fn:       1,
			du:       []string{"a@23 -> return a::g;@28"},
		},
		{
			// §15.26.2, §15.14.2. Nodes: a@22, i@29, a[i] += 1@34 (Uses a and
			// i, may-defines a), a[i]++@45 (the same), return a[i];@53. Each
			// write is a χ of a: a use of a after it pairs with it, the
			// nearest may-definition, and with the parameter.
			name:     "a compound element assignment and an element update may-define the array local",
			protects: "a[i] += v and a[i]++ read their base and may-define it without killing the earlier definition",
			mutation: "record no may-definition for a compound element assignment (loses a[i] += 1@34 -> a[i]++@45) or for an element update (loses a[i]++@45 -> return a[i];@53)",
			src:      "class A { int f(int[] a, int i) { a[i] += 1; a[i]++; return a[i]; } }",
			fn:       1,
			du: []string{"a@22 -> a[i] += 1@34", "i@29 -> a[i] += 1@34",
				"a@22 -> a[i]++@45", "a[i] += 1@34 -> a[i]++@45", "i@29 -> a[i]++@45",
				"a@22 -> return a[i];@53", "a[i]++@45 -> return a[i];@53", "i@29 -> return a[i];@53"},
		},
		{
			// §14.11.1, §14.11.3. `case null, default` matches every value, null
			// included: it is the default label and makes no node. Nodes: o@23,
			// y = 0@32, o@47 (selector), case String s@52, s@64 (the pattern
			// variable), y = 1@69, y = 2@98, return y;@107. Succ: case String
			// s→{s, y = 2} (no label matched: the default group); s→y = 1→return;
			// y = 2→return. IPDom: case String s, y = 1, y = 2 → return; s → y
			// = 1. Frontier walk: case String s over s, y = 1 and y = 2. y = 0
			// is killed on every path.
			name:     "a case null, default label is the default and makes no node",
			protects: "the label matching null and every other value enters its group on the no-match path, so a pattern switch holding it never throws there and nothing tests it",
			mutation: "lower case null, default as a case label (a Branch case null, default@76 appears, controlled by case String s@52 and using the selector's variable, and its no-match path throws: it gains control of y = 2@98 and return y;@107)",
			src:      "class A { int f(Object o) { int y = 0; switch (o) { case String s -> y = 1; case null, default -> y = 2; } return y; } }",
			fn:       1,
			cd:       []string{"case String s@52 -> s@64", "case String s@52 -> y = 1@69", "case String s@52 -> y = 2@98"},
			du: []string{"o@23 -> o@47", "o@47 -> case String s@52", "o@47 -> s@64",
				"y = 1@69 -> return y;@107", "y = 2@98 -> return y;@107"},
		},
		{
			// §14.11.1, §14.11.2. The arm's block lacks the `;` after y = 1, so
			// the source parses with a syntax error; the parser recovers with
			// an ERROR node it marks as an extra, spanning y = 1 and holding
			// the assignment. An ERROR node is lowered as a kind the lowering
			// does not name, for its value, so the assignment inside it is the
			// node y = 1@71 defining y, as it would be in an expression
			// statement holding a missing `;`. A pattern switch statement without default
			// throws when no label matches, but a switch whose tree holds a
			// syntax error may have lost its default, so this one is left.
			// Nodes: o@23, y = 0@32, o@47 (selector), case String s@52, s@64,
			// y = 1@71, return y;@81. Succ: case String s→{s, return} (the
			// no-match path leaves the switch); s→y = 1→return. IPDom: every
			// node → return. Frontier walk: case String s over s and y = 1.
			name:      "a switch holding a syntax error is left when no label matches",
			protects:  "a recovery that may have dropped a default makes no exhaustive-switch throw, so the statement after the switch runs on the no-match path with the definitions before the switch",
			mutation:  "throw on the no-match path of a switch whose tree holds a syntax error, as for any enhanced switch (loses y = 0@32 -> return y;@81, and case String s@52 gains control of return y;@81); or leave an ERROR node the parser made an extra out of the block's statement list, with the comments (loses case String s@52 -> y = 1@71 and y = 1@71 -> return y;@81)",
			src:       "class A { int f(Object o) { int y = 0; switch (o) { case String s -> { y = 1 } } return y; } }",
			fn:        1,
			recovered: true,
			cd:        []string{"case String s@52 -> s@64", "case String s@52 -> y = 1@71"},
			du: []string{"o@23 -> o@47", "o@47 -> case String s@52", "o@47 -> s@64",
				"y = 0@32 -> return y;@81", "y = 1@71 -> return y;@81"},
		},
		{
			// An unmatched `)` in a block, ill-formed on purpose: the parser
			// recovers with an error node over `)` among the block's
			// statements, between two intact statements. Nodes: x@20, y =
			// x@29, the error node )@36 (a kind the lowering does not name:
			// one Stmt node with its uses, none here, falling through), return
			// y;@38. The error node makes a node of its own, extra or not; the
			// pairs hold as long as both statements survive it.
			name:      "an error node among a block's statements is one node that falls through",
			protects:  "a statement the parser could not recover is lowered as one node and control continues past it with the definitions before it",
			mutation:  "end the path at a kind the lowering does not name (loses y = x@29 -> return y;@38)",
			src:       "class A { int f(int x) { int y = x; ) return y; } }",
			fn:        1,
			recovered: true,
			du:        []string{"x@20 -> y = x@29", "y = x@29 -> return y;@38"},
		},
		{
			// §14.18, §14.20.1, §15.9.4. Nodes: x@20, x > 0@35, throw new
			// E();@42 (a Throw, whose Throw source enters the first clause
			// test, and MayThrow, since the creation may throw first: an edge
			// into the Handler), x = 1@57, the Handler catch@66, E@73, e@75, x
			// = 2@80, return x;@89. Succ: x > 0→{throw, x = 1}; throw→{catch,
			// E}; catch→E→{e, EXIT (rethrown)}; e→x = 2→return; x = 1→return.
			// IPDom: x > 0, E → EXIT; throw, catch → E; x = 1, x = 2 → return.
			// Frontier walks: x > 0 over throw and E, and over x = 1 and
			// return; throw over catch; E over e, x = 2 and return.
			name:     "a throw statement inside a try leaves for the catch chain",
			protects: "the statement after a throw inside a try runs only when the throw is skipped, the thrown path reaches the catch clause's body, and a throw whose operand may itself throw also reaches the Handler",
			mutation: "give a throw statement's node no Throw (it falls through to x = 1@57 besides its MayThrow edge: gains throw new E();@42 -> x = 1@57), or no MayThrow (loses throw new E();@42 -> catch@66: the Handler vanishes)",
			src:      "class A { int f(int x) { try { if (x > 0) throw new E(); x = 1; } catch (E e) { x = 2; } return x; } }",
			fn:       1,
			cd: []string{"x > 0@35 -> throw new E();@42", "throw new E();@42 -> catch@66", "x > 0@35 -> E@73",
				"x > 0@35 -> x = 1@57", "x > 0@35 -> return x;@89",
				"E@73 -> e@75", "E@73 -> x = 2@80", "E@73 -> return x;@89"},
			du: []string{"x@20 -> x > 0@35", "x = 1@57 -> return x;@89", "x = 2@80 -> return x;@89"},
		},
		{
			// §14.12, §6.3.2.3. Nodes: o@23, o@37 (the tested value, the
			// head's first node), s@57, !(o instanceof String s)@35 (the
			// condition), o = g()@63, return s.length();@74. Succ: o@37→s→the
			// condition→{o = g(), return}; o = g()→o@37 (the back edge).
			// IPDom: o@37 → s → the condition → return; o = g() → o@37.
			// Frontier walk: the condition over o = g(), o@37, s and itself.
			// No break leaves the body, so the when-false s is in scope after
			// the loop.
			name:     "a while's back edge re-enters the first node of its condition",
			protects: "each iteration evaluates the whole condition again, its tested value included, and the body's definition reaches it",
			mutation: "send the back edge to the condition's Branch (loses o = g()@63 -> o@37, and the condition no longer controls o@37 and s@57)",
			src:      "class A { int f(Object o) { while (!(o instanceof String s)) { o = g(); } return s.length(); } }",
			fn:       1,
			cd: []string{"!(o instanceof String s)@35 -> o = g()@63", "!(o instanceof String s)@35 -> o@37",
				"!(o instanceof String s)@35 -> s@57", "!(o instanceof String s)@35 -> !(o instanceof String s)@35"},
			du: []string{"o@23 -> o@37", "o = g()@63 -> o@37", "o@37 -> s@57", "o@37 -> !(o instanceof String s)@35",
				"s@57 -> return s.length();@74"},
		},
		{
			// §14.13, §6.3.2.4. Nodes: o@23, o = g()@33 (the body, first),
			// o@53 (tested value), s@73, !(o instanceof String s)@51 (the
			// condition), return s.length();@78. Succ: o = g()→o@53→s→the
			// condition→{o = g(), return}. IPDom: each → the next, the
			// condition → return. Frontier walk: the condition over o = g(),
			// o@53, s and itself. The body holds no break, so the when-false
			// s is in scope after the loop.
			name:     "a do statement's when-false pattern variable is in scope after it",
			protects: "a use after a do loop resolves to the pattern variable its negated condition defines",
			mutation: "introduce nothing after a do (loses s@73 -> return s.length();@78)",
			src:      "class A { int f(Object o) { do { o = g(); } while (!(o instanceof String s)); return s.length(); } }",
			fn:       1,
			cd: []string{"!(o instanceof String s)@51 -> o = g()@33", "!(o instanceof String s)@51 -> o@53",
				"!(o instanceof String s)@51 -> s@73", "!(o instanceof String s)@51 -> !(o instanceof String s)@51"},
			du: []string{"o = g()@33 -> o@53", "o@53 -> s@73", "o@53 -> !(o instanceof String s)@51",
				"s@73 -> return s.length();@78"},
		},
		{
			// §6.3.1.4, §15.25. Nodes: o@23, o@35 (tested value), s@55, o
			// instanceof String s@35 (the condition's Branch), s.length()@59
			// and 0@72 (the arms, defining the conditional's result), return
			// …@28 (Uses the result). The condition's when-true s is in scope
			// in the second operand.
			name:     "a conditional's when-true pattern variable is in scope in its second operand",
			protects: "the arm after ? reads the pattern variable its condition defines",
			mutation: "keep the condition's sets out of the arms (loses s@55 -> s.length()@59)",
			src:      "class A { int f(Object o) { return o instanceof String s ? s.length() : 0; } }",
			fn:       1,
			cd:       []string{"o instanceof String s@35 -> s.length()@59", "o instanceof String s@35 -> 0@72"},
			du: []string{"o@23 -> o@35", "o@35 -> s@55", "o@35 -> o instanceof String s@35", "s@55 -> s.length()@59",
				"s.length()@59 -> return o instanceof String s ? s.length() : 0;@28", "0@72 -> return o instanceof String s ? s.length() : 0;@28"},
		},
		{
			// §6.3.2.2. Nodes: o@23, o@34 (tested value), s@54, !(o
			// instanceof String s)@32 (the condition), return 0;@58, return
			// s.length();@73. The negated test's when-false set, {s}, is in
			// scope in the else statement.
			name:     "an if's when-false pattern variable is in scope in its else statement",
			protects: "the else statement of a negated instanceof reads the pattern variable the test defines",
			mutation: "scope no set in the else statement (loses s@54 -> return s.length();@73)",
			src:      "class A { int f(Object o) { if (!(o instanceof String s)) return 0; else return s.length(); } }",
			fn:       1,
			cd:       []string{"!(o instanceof String s)@32 -> return 0;@58", "!(o instanceof String s)@32 -> return s.length();@73"},
			du: []string{"o@23 -> o@34", "o@34 -> s@54", "o@34 -> !(o instanceof String s)@32",
				"s@54 -> return s.length();@73"},
		},
		{
			// §8.9.1, §12.4.2. Callable 0, the enum body: the enum constant
			// P's argument c() ? 1 : 2 lowers to c()@11 (Branch), 1@17 and
			// 2@21 (each defining the conditional's result), then P(c() ? 1 :
			// 2)@9 (a Stmt node Using the result), then Q@25. The
			// constructors and c are callables of their own.
			name:     "an enum body's unit lowers each enum constant as a node after its arguments",
			protects: "the decision inside an enum constant's arguments is lowered in the enum body's unit, and the constant's node consumes its arguments' value",
			mutation: "skip enum constants in the unit (loses every pair and c()@11's control dependences)",
			src:      "enum E { P(c() ? 1 : 2), Q; E() {} E(int x) {} static boolean c() { return true; } }",
			fn:       0,
			cd:       []string{"c()@11 -> 1@17", "c()@11 -> 2@21"},
			du:       []string{"1@17 -> P(c() ? 1 : 2)@9", "2@21 -> P(c() ? 1 : 2)@9"},
		},
		{
			// §9.3. Callable 0, the interface body: the constant X's
			// initializer lowers to I.c()@22 (Branch; I is no variable),
			// 1@30 and 2@34 (the arms), then X = I.c() ? 1 : 2@18.
			name:     "an interface body's unit lowers its constant declarators",
			protects: "an interface constant's initializer is lowered in the interface body's unit",
			mutation: "drop interface_body from the callables (callable 0 becomes the method c: no pair), or skip constant declarations in the unit (loses every pair)",
			src:      "interface I { int X = I.c() ? 1 : 2; static boolean c() { return true; } }",
			fn:       0,
			cd:       []string{"I.c()@22 -> 1@30", "I.c()@22 -> 2@34"},
			du:       []string{"1@30 -> X = I.c() ? 1 : 2@18", "2@34 -> X = I.c() ? 1 : 2@18"},
		},
		{
			// §8.1.3, §15.26.1. Nodes: a = new int[1]@28, class L { … }@44 (the
			// local class declaration: Uses its capture a and may-defines it,
			// since its method writes an element of a), new L().m()@79,
			// return a;@92. The declaration is a χ of a, so the return pairs
			// with it, the nearest may-definition, and with the killing
			// definition before it.
			name:     "a local class writing through a captured array may-defines it where it is declared",
			protects: "a use of a captured array after a local class declaration sees the element write its methods may perform",
			mutation: "record no write through a captured local in a local class (loses class L { void m() { a[0] = 1; } }@44 -> return a;@92)",
			src:      "class A { int[] f() { int[] a = new int[1]; class L { void m() { a[0] = 1; } } new L().m(); return a; } }",
			fn:       1,
			du: []string{"a = new int[1]@28 -> class L { void m() { a[0] = 1; } }@44",
				"a = new int[1]@28 -> return a;@92", "class L { void m() { a[0] = 1; } }@44 -> return a;@92"},
		},
		{
			// §15.28, §15.7.1, §14.20.1. Nodes: x@20, r = 0@29, x@60 (the
			// selector, the first node made after g() is evaluated: g()'s
			// throw is its own, so it may throw), case 1@65, 2@75, 3@89, r =
			// g() + switch (x) { … }@42 (the consumer: nothing it evaluates
			// after the switch expression throws), the Handler catch@97,
			// E@104, e@106, r = 1@111, return r;@120. Succ: x@60→{case 1,
			// catch}; case 1→{2, 3}; 2, 3 → the assignment→return;
			// catch→E→{e, EXIT}; e→r = 1→return. IPDom: x@60, E, return →
			// EXIT; case 1, 2, 3 → the assignment → return; catch → E; e → r
			// = 1 → return. Frontier walks: x@60 over case 1, the assignment
			// and return, and over catch and E; case 1 over 2 and 3; E over
			// e, r = 1 and return.
			name:     "a throw evaluated before a nested switch expression is its selector's, not its consumer's",
			protects: "a throwing construct the statement evaluated before a nested switch expression attaches once, to the selector's node, the first node after it, and the consumer carries only what it evaluates after the switch",
			mutation: "restore the pending throw after a nested switch expression (the assignment gains a MayThrow edge: gains its control of return r;@120, catch@97 and E@104, and x@60 loses return r;@120)",
			src:      "class A { int f(int x) { int r = 0; try { r = g() + switch (x) { case 1 -> 2; default -> 3; }; } catch (E e) { r = 1; } return r; } }",
			fn:       1,
			cd: []string{"x@60 -> case 1@65", "x@60 -> r = g() + switch (x) { case 1 -> 2; default -> 3; }@42",
				"x@60 -> return r;@120", "x@60 -> catch@97", "x@60 -> E@104", "case 1@65 -> 2@75", "case 1@65 -> 3@89",
				"E@104 -> e@106", "E@104 -> r = 1@111", "E@104 -> return r;@120"},
			du: []string{"x@20 -> x@60", "x@60 -> case 1@65",
				"2@75 -> r = g() + switch (x) { case 1 -> 2; default -> 3; }@42", "3@89 -> r = g() + switch (x) { case 1 -> 2; default -> 3; }@42",
				"r = g() + switch (x) { case 1 -> 2; default -> 3; }@42 -> return r;@120", "r = 1@111 -> return r;@120"},
		},
		{
			// §14.20.2. Nodes: r = 0@24, r = g()@37 (throws into the finally),
			// the Handler finally@48, h(r)@58 (the finally body, entered by
			// normal completion and by the Handler), return r;@66. Succ: r =
			// g()→{h(r), finally}; finally→h(r); h(r)→{return, EXIT (the
			// re-issued throw)}. IPDom: r = g(), finally → h(r) → EXIT.
			// Frontier walks: r = g() over finally; h(r) over return. The
			// Handler carries r = g()'s entry value r = 0.
			name:     "a finally without a catch has a Handler at finally when its block may throw",
			protects: "an exception from a try block without catch clauses runs the finally with the values before the throwing node, then leaves",
			mutation: "open no Handler for a finally frame's MayThrow sources (loses r = 0@24 -> h(r)@58 and -> return r;@66, and r = g()@37 loses its control of finally@48)",
			src:      "class A { int f() { int r = 0; try { r = g(); } finally { h(r); } return r; } }",
			fn:       1,
			cd:       []string{"r = g()@37 -> finally@48", "h(r)@58 -> return r;@66"},
			du: []string{"r = 0@24 -> h(r)@58", "r = g()@37 -> h(r)@58",
				"r = 0@24 -> return r;@66", "r = g()@37 -> return r;@66"},
		},
		{
			// §14.20.3.2. Nodes: r = 0@24, a = open()@38 (the acquisition: its
			// call throws to the statement's catch), r = a.u()@52 (throws to
			// the resource's finally), the resource's Handler )@48, R a =
			// open()@36 (the close: Uses a, throws to the catch, and re-issues
			// the intercepted throw there), the Handler catch@65, E@72, e@74,
			// r = 1@79, h(r)@98 (the statement's finally, entered by normal
			// completion, the catch body and E's rethrow), return r;@106.
			// Succ: a = open()→{r = a.u(), catch}; r = a.u()→{R a, )@48};
			// )@48→R a; R a→{h(r), catch, E}; catch→E→{e, h(r)}; e→r =
			// 1→h(r); h(r)→{return, EXIT}. IPDom: a = open(), R a, E → h(r)
			// → EXIT; r = a.u(), )@48 → R a; catch → E. Frontier walks: a =
			// open() over r = a.u(), R a, catch and E; r = a.u() over )@48;
			// R a over catch and E; E over e and r = 1; h(r) over return.
			// Each Handler carries its sources' entry values, so r = 0 reaches
			// the finally along every exceptional path.
			name:     "an extended try-with-resources closes its resources inside its own catch and finally",
			protects: "the statement's catch clauses catch what the acquisition, the block and the close throw, and its finally follows every path",
			mutation: "close the resources outside the statement's catch clauses (R a = open()@36 throws past the catch: loses R a = open()@36 -> catch@65 and -> E@72)",
			src:      "class A { int f() { int r = 0; try (R a = open()) { r = a.u(); } catch (E e) { r = 1; } finally { h(r); } return r; } }",
			fn:       1,
			cd: []string{"a = open()@38 -> r = a.u()@52", "a = open()@38 -> R a = open()@36", "a = open()@38 -> catch@65",
				"a = open()@38 -> E@72", "r = a.u()@52 -> )@48", "R a = open()@36 -> catch@65", "R a = open()@36 -> E@72",
				"E@72 -> e@74", "E@72 -> r = 1@79", "h(r)@98 -> return r;@106"},
			du: []string{"a = open()@38 -> r = a.u()@52", "a = open()@38 -> R a = open()@36",
				"r = 0@24 -> h(r)@98", "r = a.u()@52 -> h(r)@98", "r = 1@79 -> h(r)@98",
				"r = 0@24 -> return r;@106", "r = a.u()@52 -> return r;@106", "r = 1@79 -> return r;@106"},
		},
		{
			// §15.9.4, §15.9.5. Nodes: k@23, n@30, new B(k) { … }@42 (the
			// creation: Uses its argument's read of k and its capture n,
			// defines the owned result), return …@35 (Uses the result).
			name:     "an anonymous class creation reads its arguments as well as its captures",
			protects: "the creating node carries the constructor arguments' reads, evaluated where the object is created",
			mutation: "let the creation node Use only its captures (loses k@23 -> new B(k) { int m() { return n; } }@42)",
			src:      "class A { Object f(int k, int n) { return new B(k) { int m() { return n; } }; } }",
			fn:       1,
			du: []string{"k@23 -> new B(k) { int m() { return n; } }@42", "n@30 -> new B(k) { int m() { return n; } }@42",
				"new B(k) { int m() { return n; } }@42 -> return new B(k) { int m() { return n; } };@35"},
		},
		{
			// §15.27.2. Ill-formed on purpose: a compiler rejects the source with
			// "local variables referenced from a lambda expression must be
			// final or effectively final". No valid program expresses the
			// point, since nested code may neither assign nor compound-assign
			// an enclosing local, yet the lowering meets such code in a
			// working tree, and its capture walk must neither read a plain
			// assignment's target nor record it as a write. Nodes: n@21, ()
			// -> { n = 1; }@39 (the lambda: Uses nothing, may-defines
			// nothing), r = () -> { n = 1; }@35 (Uses the result).
			name:      "the capture walk takes a plain assignment's local target as written, not read",
			protects:  "a lambda that only assigns an enclosing name does not read it",
			mutation:  "let the capture walk read a plain assignment's target (gains n@21 -> () -> { n = 1; }@39)",
			src:       "class A { void f(int n) { Runnable r = () -> { n = 1; }; } }",
			fn:        1,
			illFormed: true,
			du:        []string{"() -> { n = 1; }@39 -> r = () -> { n = 1; }@35"},
		},
		{
			// §15.11.1, §15.10.4, §15.16. Nodes: o@25, a@34, r = 0@43, r =
			// this.v@56 (no throw: the receiver is this), r = o.f@68, r =
			// a[0]@77, r = (int) (Object) r@87 (each may throw), the Handler
			// catch@111, E@118, e@120, r = 1@125, return r;@134. Succ: r =
			// this.v→r = o.f→{r = a[0], catch}; r = a[0]→{the cast, catch};
			// the cast→{return, catch}; catch→E→{e, EXIT}; e→r = 1→return.
			// IPDom: r = o.f, r = a[0], the cast, E, return → EXIT; catch → E.
			// Frontier walks: each throwing node over its next node, catch and
			// E; E over e, r = 1 and return.
			name:     "a field access through a receiver, an array access and a cast may throw, and a field of this does not",
			protects: "only the constructs that can throw give a node an exceptional edge into the catch",
			mutation: "count a field access through this as throwing (gains r = this.v@56 -> r = o.f@68, -> catch@111 and -> E@118)",
			src:      "class A { int v; int f(P o, int[] a) { int r = 0; try { r = this.v; r = o.f; r = a[0]; r = (int) (Object) r; } catch (E e) { r = 1; } return r; } }",
			fn:       1,
			cd: []string{"r = o.f@68 -> r = a[0]@77", "r = o.f@68 -> catch@111", "r = o.f@68 -> E@118",
				"r = a[0]@77 -> r = (int) (Object) r@87", "r = a[0]@77 -> catch@111", "r = a[0]@77 -> E@118",
				"r = (int) (Object) r@87 -> return r;@134", "r = (int) (Object) r@87 -> catch@111", "r = (int) (Object) r@87 -> E@118",
				"E@118 -> e@120", "E@118 -> r = 1@125", "E@118 -> return r;@134"},
			du: []string{"o@25 -> r = o.f@68", "a@34 -> r = a[0]@77", "r = a[0]@77 -> r = (int) (Object) r@87",
				"r = (int) (Object) r@87 -> return r;@134", "r = 1@125 -> return r;@134"},
		},
		{
			// §14.14.2, §14.20.1. Nodes: xs@40, s = 0@50, xs@76 (the iterated
			// expression), int x : xs@68 (the head: its iterator step may
			// throw), x@72, s += x@80, the Handler catch@90, E@97, e@99, s =
			// -1@104, return s;@114. Succ: xs@76→head→{x, return, catch};
			// x→s += x→head; catch→E→{e, EXIT}; e→s = -1→return. IPDom: head,
			// E, return → EXIT; x → s += x → head; catch → E. Frontier walks:
			// the head over x, s += x, itself, return, catch and E; E over e,
			// s = -1 and return.
			name:     "an enhanced for's head may throw",
			protects: "the iterator step of an enhanced for inside a try reaches the catch",
			mutation: "give the enhanced-for head no MayThrow (loses int x : xs@68 -> catch@90 and -> E@97)",
			src:      "class A { int f(java.util.List<Integer> xs) { int s = 0; try { for (int x : xs) s += x; } catch (E e) { s = -1; } return s; } }",
			fn:       1,
			cd: []string{"int x : xs@68 -> x@72", "int x : xs@68 -> s += x@80", "int x : xs@68 -> int x : xs@68",
				"int x : xs@68 -> return s;@114", "int x : xs@68 -> catch@90", "int x : xs@68 -> E@97",
				"E@97 -> e@99", "E@97 -> s = -1@104", "E@97 -> return s;@114"},
			du: []string{"xs@40 -> xs@76", "xs@76 -> int x : xs@68", "xs@76 -> x@72", "x@72 -> s += x@80",
				"s = 0@50 -> s += x@80", "s += x@80 -> s += x@80", "s = 0@50 -> return s;@114", "s += x@80 -> return s;@114",
				"s = -1@104 -> return s;@114"},
		},
		{
			// §14.19, §14.20.1. Nodes: o@23, r = 0@32, o@59 (the monitor
			// expression: may throw), r = 1@64, the Handler catch@75, E@82,
			// e@84, r = 2@89, return r;@98. Succ: o@59→{r = 1, catch}; r =
			// 1→return; catch→E→{e, EXIT}; e→r = 2→return. IPDom: o@59, E,
			// return → EXIT; r = 1 → return; catch → E. Frontier walks: o@59
			// over r = 1, return, catch and E; E over e, r = 2 and return.
			name:     "a monitor expression may throw",
			protects: "a synchronized statement inside a try can fail at its monitor and reach the catch",
			mutation: "give a monitor expression no MayThrow (loses o@59 -> catch@75 and -> E@82)",
			src:      "class A { int f(Object o) { int r = 0; try { synchronized (o) { r = 1; } } catch (E e) { r = 2; } return r; } }",
			fn:       1,
			cd: []string{"o@59 -> r = 1@64", "o@59 -> return r;@98", "o@59 -> catch@75", "o@59 -> E@82",
				"E@82 -> e@84", "E@82 -> r = 2@89", "E@82 -> return r;@98"},
			du: []string{"o@23 -> o@59", "r = 1@64 -> return r;@98", "r = 2@89 -> return r;@98"},
		},
		{
			// §15.17.2, §14.20.2. Nodes: a@20, b@27, r = 0@36, r = a / b@49,
			// h(r)@72 (the finally), return r;@80. Division is not counted as
			// throwing, so nothing reaches the finally by MayThrow: no Handler,
			// and a straight line. A deliberate give-up: an ArithmeticException
			// path is not modelled.
			name:     "division is not counted as throwing",
			protects: "a try whose only possible exception is a division by zero has no exceptional edge",
			mutation: "count division as throwing (a Handler finally@62 appears, and gains r = 0@36 -> h(r)@72 and -> return r;@80)",
			src:      "class A { int f(int a, int b) { int r = 0; try { r = a / b; } finally { h(r); } return r; } }",
			fn:       1,
			du: []string{"a@20 -> r = a / b@49", "b@27 -> r = a / b@49", "r = a / b@49 -> h(r)@72",
				"r = a / b@49 -> return r;@80"},
		},
		{
			// §6.3.4, §14.11.1. Nodes: o@23, o@43 (selector), case Object x
			// when x instanceof String s@48 (the label), x@60 (the pattern
			// variable), x@67 (the guard's tested value, reading x), s@87,
			// x instanceof String s@67 (the guard's Branch), s.length()@92
			// (the arm), 0@115 (default), return …@28. Succ: the label→{x@60,
			// 0}; x@60→x@67→s→the guard→{s.length(), 0}; both → return.
			// IPDom: the label, the guard → return; x@60 → x@67 → s → the
			// guard. Frontier walks: the label over x@60, x@67, s, the guard
			// and 0; the guard over s.length() and 0. The guard's when-true s
			// is in scope in the rule.
			name:     "a guard's when-true pattern variable is in scope in its rule",
			protects: "an arm reads the pattern variable its guard's test defines",
			mutation: "keep a guard's when-true set out of its rule (loses s@87 -> s.length()@92)",
			src:      "class A { int f(Object o) { return switch (o) { case Object x when x instanceof String s -> s.length(); default -> 0; }; } }",
			fn:       1,
			cd: []string{"case Object x when x instanceof String s@48 -> x@60", "case Object x when x instanceof String s@48 -> x@67",
				"case Object x when x instanceof String s@48 -> s@87", "case Object x when x instanceof String s@48 -> x instanceof String s@67",
				"case Object x when x instanceof String s@48 -> 0@115",
				"x instanceof String s@67 -> s.length()@92", "x instanceof String s@67 -> 0@115"},
			du: []string{"o@23 -> o@43", "o@43 -> case Object x when x instanceof String s@48", "o@43 -> x@60",
				"x@60 -> x@67", "x@67 -> s@87", "x@67 -> x instanceof String s@67", "s@87 -> s.length()@92",
				"s.length()@92 -> return switch (o) { case Object x when x instanceof String s -> s.length(); default -> 0; };@28",
				"0@115 -> return switch (o) { case Object x when x instanceof String s -> s.length(); default -> 0; };@28"},
		},
		{
			// §6.3.1.2, §6.3.2.2. Nodes: o@27, o@38 (tested value), s@58, !(o
			// instanceof String s)@36 (the || deciding operand's Branch,
			// defining the operator's result), s.isEmpty()@64 (the right
			// operand: s, the left's when-false set, is in scope), the
			// condition @36 (Uses the result), return false;@77, return
			// s.length() > 1;@91. Succ: the left→{the condition, s.isEmpty()};
			// s.isEmpty()→the condition→{return false, the last return}.
			// IPDom: the left → the condition → EXIT. The || introduces its
			// when-false set {s} and the then statement cannot complete
			// normally, so s is in scope after the if.
			name:     "a || left operand's when-false pattern variable is in scope in its right operand and after the if",
			protects: "the right operand of || and the code after an if that exits when || is true read the pattern variable of the negated test",
			mutation: "put a || left operand's when-true set in scope in its right operand (loses s@58 -> s.isEmpty()@64), or introduce nothing after the if (loses s@58 -> return s.length() > 1;@91)",
			src:      "class A { boolean f(Object o) { if (!(o instanceof String s) || s.isEmpty()) return false; return s.length() > 1; } }",
			fn:       1,
			cd: []string{"!(o instanceof String s)@36 -> s.isEmpty()@64",
				"!(o instanceof String s) || s.isEmpty()@36 -> return false;@77",
				"!(o instanceof String s) || s.isEmpty()@36 -> return s.length() > 1;@91"},
			du: []string{"o@27 -> o@38", "o@38 -> s@58", "o@38 -> !(o instanceof String s)@36", "s@58 -> s.isEmpty()@64",
				"!(o instanceof String s)@36 -> !(o instanceof String s) || s.isEmpty()@36",
				"s.isEmpty()@64 -> !(o instanceof String s) || s.isEmpty()@36", "s@58 -> return s.length() > 1;@91"},
		},
		{
			// §6.3.2.2. Nodes: o@23, o@32 (tested value), s@52, o instanceof
			// String s@32 (the condition), return 0;@63, return s.length();@73.
			// The then statement completes normally and the else does not, so
			// the when-true s is in scope after the if.
			name:     "an if whose else alone cannot complete introduces its when-true pattern variable after it",
			protects: "a use after an if that leaves only through its else resolves to the pattern variable of the test",
			mutation: "introduce only a when-false set after an if (loses s@52 -> return s.length();@73)",
			src:      "class A { int f(Object o) { if (o instanceof String s) {} else return 0; return s.length(); } }",
			fn:       1,
			cd:       []string{"o instanceof String s@32 -> return 0;@63", "o instanceof String s@32 -> return s.length();@73"},
			du: []string{"o@23 -> o@32", "o@32 -> s@52", "o@32 -> o instanceof String s@32",
				"s@52 -> return s.length();@73"},
		},
		{
			// §6.3.2.5, §14.14.1.2. Nodes: o@24, o@36 (tested value, the head's
			// first node), s@56, o instanceof String s@36 (the condition),
			// g(s)@81, o = s.substring(1)@59 (the update). Succ: o@36→s→the
			// condition→{g(s), EXIT}; g(s)→the update→o@36. IPDom: o@36 → s
			// → the condition → EXIT; g(s) → the update → o@36. Frontier walk:
			// the condition over g(s), the update, o@36, s and itself.
			name:     "a basic for's when-true pattern variable is in scope in its update clause",
			protects: "the update clause reads the pattern variable the condition defines on each iteration",
			mutation: "scope the condition's when-true set to the body only (loses s@56 -> o = s.substring(1)@59)",
			src:      "class A { void f(Object o) { for (; o instanceof String s; o = s.substring(1)) { g(s); } } }",
			fn:       1,
			cd: []string{"o instanceof String s@36 -> g(s)@81", "o instanceof String s@36 -> o = s.substring(1)@59",
				"o instanceof String s@36 -> o@36", "o instanceof String s@36 -> s@56",
				"o instanceof String s@36 -> o instanceof String s@36"},
			du: []string{"o@24 -> o@36", "o = s.substring(1)@59 -> o@36", "o@36 -> s@56", "o@36 -> o instanceof String s@36",
				"s@56 -> g(s)@81", "s@56 -> o = s.substring(1)@59"},
		},
		{
			// §6.3.2.3, §14.15. s is also a field. Nodes: o@33, o@47 (tested
			// value), s@67, !(o instanceof String s)@45 (the condition), o ==
			// null@77, break;@88, o = g()@95, return s.length();@106. Succ:
			// o@47→s→the condition→{o == null, return}; o == null→{break, o =
			// g()}; break→return; o = g()→o@47. IPDom: the condition, o ==
			// null, break → return; o@47 → s → the condition; o = g() → o@47.
			// Frontier walks: the condition over o == null; o == null over
			// break, and over o = g(), o@47, s and the condition. A break
			// leaves the body, so the loop introduces nothing: the return
			// reads the field.
			name:     "a break that leaves a loop body blocks the loop's pattern variable after it",
			protects: "a use after a loop a break may leave resolves to what the name meant before the loop, not to the condition's pattern variable",
			mutation: "introduce the when-false set after a loop a break leaves (gains s@67 -> return s.length();@106)",
			src:      "class A { String s; int f(Object o) { while (!(o instanceof String s)) { if (o == null) break; o = g(); } return s.length(); } }",
			fn:       1,
			cd: []string{"!(o instanceof String s)@45 -> o == null@77", "o == null@77 -> break;@88",
				"o == null@77 -> o = g()@95", "o == null@77 -> o@47", "o == null@77 -> s@67",
				"o == null@77 -> !(o instanceof String s)@45"},
			du: []string{"o@33 -> o@47", "o = g()@95 -> o@47", "o@47 -> s@67", "o@47 -> !(o instanceof String s)@45",
				"o@33 -> o == null@77", "o = g()@95 -> o == null@77"},
		},
		{
			// §6.3.2.7, §14.7. s is also a field. Nodes: o@33, o@47 (tested
			// value), s@67, !(o instanceof String s)@45 (the condition), return
			// 0;@71, return s.length();@81. The labelled if holds no break
			// naming its label, so it introduces what the if does: s.
			name:     "a labelled statement introduces its statement's pattern variables when no break leaves it",
			protects: "a label does not hide the pattern variable its if introduces",
			mutation: "introduce nothing after a labelled statement (loses s@67 -> return s.length();@81)",
			src:      "class A { String s; int f(Object o) { l: if (!(o instanceof String s)) return 0; return s.length(); } }",
			fn:       1,
			cd:       []string{"!(o instanceof String s)@45 -> return 0;@71", "!(o instanceof String s)@45 -> return s.length();@81"},
			du: []string{"o@33 -> o@47", "o@47 -> s@67", "o@47 -> !(o instanceof String s)@45",
				"s@67 -> return s.length();@81"},
		},
		{
			// §6.3.2.7, §14.15. s is also a field. Nodes: o@33, o@47, s@67, the
			// condition @45, break l;@71 (leaves the labelled if for its end),
			// return s.length();@80. A break leaves the labelled statement,
			// so it introduces nothing: the return reads the field.
			name:     "a labelled statement a break leaves introduces nothing",
			protects: "a use after a labelled statement its break may leave resolves to what the name meant before it",
			mutation: "let a labelled statement introduce its statement's set though a break leaves it (gains s@67 -> return s.length();@80)",
			src:      "class A { String s; int f(Object o) { l: if (!(o instanceof String s)) break l; return s.length(); } }",
			fn:       1,
			cd:       []string{"!(o instanceof String s)@45 -> break l;@71"},
			du:       []string{"o@33 -> o@47", "o@47 -> s@67", "o@47 -> !(o instanceof String s)@45"},
		},
		{
			// §14.22, §14.20.2, §6.3.2.2. Nodes: o@23, o@34, s@54, the
			// condition @32, return 0;@66 (intercepted by the finally), g()@88
			// (the finally, which re-issues the return), return s.length();@97.
			// A try whose block cannot complete normally cannot either, though
			// its finally can, so the then block cannot and the if introduces
			// s. Succ: the condition→{return 0, return s.length()}; return
			// 0→g()→EXIT. IPDom: the condition → EXIT. Frontier walks: the
			// condition over return 0, g() and return s.length().
			name:     "a then block ending in a try whose block returns cannot complete normally",
			protects: "normal completion follows the language's rules for try with finally, so the pattern variable is in scope after the if",
			mutation: "take every try as completing normally (loses s@54 -> return s.length();@97)",
			src:      "class A { int f(Object o) { if (!(o instanceof String s)) { try { return 0; } finally { g(); } } return s.length(); } }",
			fn:       1,
			cd: []string{"!(o instanceof String s)@32 -> return 0;@66", "!(o instanceof String s)@32 -> g()@88",
				"!(o instanceof String s)@32 -> return s.length();@97"},
			du: []string{"o@23 -> o@34", "o@34 -> s@54", "o@34 -> !(o instanceof String s)@32",
				"s@54 -> return s.length();@97"},
		},
	})
}
