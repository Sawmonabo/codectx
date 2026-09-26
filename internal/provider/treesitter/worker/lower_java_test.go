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
	})
}
