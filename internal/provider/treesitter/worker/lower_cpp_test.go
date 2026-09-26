package worker

import "testing"

// TestCppLoweringGolden pins the C++ additions of the C-family lowering on
// hand-derived functions. Offsets are byte offsets into src. The derivation
// and rendering rules are runGolden's.
func TestCppLoweringGolden(t *testing.T) {
	runGolden(t, "cpp", []goldenCase{
		{
			// [except.handle]/1–4: handlers are tried in order, `catch (...)`
			// matches anything. Nodes: x@10, g(x)@21 (may throw), the Handler
			// catch@29, (int e)@35 (the type test, defining e), return
			// e;@45, return 0;@71, return 1;@83. Succ: g(x)→{catch, return
			// 1}; catch→(int e)→{return e, return 0}. IPDom: g(x), (int e) →
			// EXIT; catch → (int e).
			name:     "typed catch then catch-all",
			protects: "a call in a try body reaches the handlers, each typed clause is a test in order, and catch (...) ends the chain without a rethrow",
			mutation: "lower catch (...) as a typed test (a Branch spanning (...) appears, rethrowing to EXIT), or record no throw for a call (the handlers are unreachable and g(x)@21 controls nothing)",
			src:      "int f(int x) { try { g(x); } catch (int e) { return e; } catch (...) { return 0; } return 1; }",
			cd: []string{"g(x)@21 -> catch@29", "g(x)@21 -> (int e)@35", "g(x)@21 -> return 1;@83",
				"(int e)@35 -> return e;@45", "(int e)@35 -> return 0;@71"},
			du: []string{"x@10 -> g(x)@21", "(int e)@35 -> return e;@45"},
		},
		{
			// [except.throw], [except.handle]/9: a throw reaches the handlers
			// directly; an exception no handler matches propagates. Nodes:
			// x@11, x@26, throw 1;@29 (a Throw, no Handler node since nothing
			// may throw), (long e)@46, g()@57, h()@64. Succ: x@26→{throw 1,
			// h()}; throw→(long e)→{g(), EXIT}; g()→h()→EXIT. IPDom: x@26,
			// (long e) → EXIT.
			name:     "a throw no clause matches leaves the function",
			protects: "the fringe matching no catch clause is rethrown to the caller, never falling through to the code after the try",
			mutation: "let the unmatched fringe fall through after the try ((long e)@46 loses control of h()@64)",
			src:      "void f(int x) { try { if (x) throw 1; } catch (long e) { g(); } h(); }",
			cd: []string{"x@26 -> throw 1;@29", "x@26 -> (long e)@46", "x@26 -> h()@64", "(long e)@46 -> g()@57",
				"(long e)@46 -> h()@64"},
			du: []string{"x@11 -> x@26"},
		},
		{
			// [stmt.ranged]/1: the range is evaluated once, then each element
			// is bound before the body. Nodes: v@8, s = 0@17, v@37 (the range,
			// defining the iteration variable), x : v@33 (the head, Using
			// it), x@33 (the bound name), s += x@40, return s;@48. Succ: v@37→
			// head→{x, return s}; x→s += x→head. IPDom: x → s += x → head →
			// return s.
			name:     "range for binds each element from the range evaluated once",
			protects: "the head and the bound name depend on the range expression as evaluated before the loop, and the name is defined only on the body path",
			mutation: "define the name on the head (x : v@33 becomes the definition and x@33 vanishes), or give the head no use of the range's result (loses v@37 -> x : v@33)",
			src:      "int f(V v) { int s = 0; for (int x : v) s += x; return s; }",
			cd:       []string{"x : v@33 -> x@33", "x : v@33 -> s += x@40", "x : v@33 -> x : v@33"},
			du: []string{"v@8 -> v@37", "v@37 -> x : v@33", "v@37 -> x@33", "x@33 -> s += x@40", "s = 0@17 -> s += x@40",
				"s += x@40 -> s += x@40", "s = 0@17 -> return s;@48", "s += x@40 -> return s;@48"},
		},
		{
			// [expr.prim.lambda.capture]/10–12: `&a` captures by reference, `b`
			// by copy, so only the write to a changes the enclosing variable.
			// Nodes: a@10, b@17, the lambda @31 (Uses its captures a and b,
			// may-defines a only, defines the created closure's owned
			// result), g = …@27 (defines g, Uses that result and none of the
			// captures), g()@68, h(a)@73, return b;@79. Succ: a straight line to EXIT. h(a) pairs with the
			// lambda, the nearest may-definition of a, and with a@10, the
			// killing definition reaching it through the lambda; return b
			// sees b@17 alone.
			name:     "a lambda's by-reference capture is a may-definition, a by-copy capture is not",
			protects: "a write through a by-reference capture reaches later uses of the variable, while a write to a by-copy capture stays inside the closure, and the declarator consuming the lambda does not repeat its captures",
			mutation: "may-define every captured variable written (adds [&a, b]() mutable { a = b; b = 0; }@31 -> return b;@79), or none (loses the lambda's pair with h(a)@73), let the declarator re-read the captures (adds a@10 and b@17 -> g = [&a, b]() mutable { a = b; b = 0; }@27), or give the creating node no result (loses [&a, b]() mutable { a = b; b = 0; }@31 -> g = [&a, b]() mutable { a = b; b = 0; }@27)",
			src:      "int f(int a, int b) { auto g = [&a, b]() mutable { a = b; b = 0; }; g(); h(a); return b; }",
			du: []string{"a@10 -> [&a, b]() mutable { a = b; b = 0; }@31", "b@17 -> [&a, b]() mutable { a = b; b = 0; }@31",
				"[&a, b]() mutable { a = b; b = 0; }@31 -> g = [&a, b]() mutable { a = b; b = 0; }@27",
				"g = [&a, b]() mutable { a = b; b = 0; }@27 -> g()@68",
				"a@10 -> h(a)@73", "[&a, b]() mutable { a = b; b = 0; }@31 -> h(a)@73", "b@17 -> return b;@79"},
		},
		{
			// [expr.prim.lambda.capture]/7, 12: under a `&` default an
			// odr-used variable is captured by reference, and binding the
			// reference Uses a, as taking its address does. Nodes: a@10, the
			// lambda @24 (Uses a, may-defines a, defines its owned result),
			// g = …@20 (defines g, Uses the result), g()@42, return a;@47. Succ: a straight line to EXIT.
			name:     "an implicit by-reference capture that is written is a may-definition",
			protects: "a variable captured implicitly under [&] and written in the body is may-defined where the lambda is created",
			mutation: "treat an implicit capture as by copy (loses [&]() { a = 1; }@24 -> return a;@47), or let the declarator re-read the captures (adds a@10 -> g = [&]() { a = 1; }@20)",
			src:      "int f(int a) { auto g = [&]() { a = 1; }; g(); return a; }",
			du: []string{"a@10 -> [&]() { a = 1; }@24", "[&]() { a = 1; }@24 -> g = [&]() { a = 1; }@20",
				"g = [&]() { a = 1; }@20 -> g()@42",
				"a@10 -> return a;@47", "[&]() { a = 1; }@24 -> return a;@47"},
		},
		{
			// [stmt.return.coroutine]: co_return ends the coroutine. Nodes:
			// x@8, x@17, co_return 1;@20, co_return 2;@33. Succ: x@17→
			// {co_return 1, co_return 2}, both → EXIT.
			name:     "co_return leaves the function",
			protects: "co_return is a return, so the statement after it is reached only when the branch is not taken",
			mutation: "lower co_return as a plain statement (co_return 1;@20 falls into co_return 2;@33, which x@17 no longer controls)",
			src:      "T f(int x) { if (x) co_return 1; co_return 2; }",
			cd:       []string{"x@17 -> co_return 1;@20", "x@17 -> co_return 2;@33"},
			du:       []string{"x@8 -> x@17"},
		},
		{
			// [stmt.pre] (a condition that is a declaration) and [stmt.if]: the
			// declared name is in scope in both branches and initialized by
			// the condition.
			// Nodes: int x = g()@14 (the Branch, defining x), return x;@27,
			// return 0;@37.
			name:     "a condition declaration defines its name on the decision node",
			protects: "the variable an if condition declares is defined by the condition and read in the branch",
			mutation: "lower the condition declaration without its definition (loses int x = g()@14 -> return x;@27)",
			src:      "int f() { if (int x = g()) return x; return 0; }",
			cd:       []string{"int x = g()@14 -> return x;@27", "int x = g()@14 -> return 0;@37"},
			du:       []string{"int x = g()@14 -> return x;@27"},
		},
		{
			// [expr.unary.op]/3: `&x` yields a pointer to x, through which the
			// callee may write it. The call takes a second argument because
			// `g(&x);` alone also reads as a declaration of a reference x of
			// type g ([stmt.ambig]), which a parser without types may pick.
			// Nodes: x = 1@14 (defines x), g(0, &x)@21 (Uses
			// x, may-defines x), return x;@31. The return's x pairs with the
			// nearest may-definition and with the killing x = 1 behind it.
			name:     "taking an address is a may-definition of the variable",
			protects: "a use after a call that received a variable's address sees the write the call may make through it",
			mutation: "lower `&x` as a plain read (g(0, &x)@21 -> return x;@31 vanishes), or make it a killing Def (x = 1@14 -> return x;@31 vanishes)",
			src:      "int f() { int x = 1; g(0, &x); return x; }",
			du:       []string{"x = 1@14 -> g(0, &x)@21", "x = 1@14 -> return x;@31", "g(0, &x)@21 -> return x;@31"},
		},
		{
			// [lex.digraph]/2: `and` and `or` are the same operators as &&
			// and ||, which evaluate the right operand only when the left
			// does not decide ([expr.log.and]/1, [expr.log.or]/1). Nodes:
			// p@11, p@23 (Branch, Uses p, defines the and result), g(*p)@29
			// (Uses p, defines the and result), p and g(*p)@23 (Branch, Uses
			// the and result, defines the or result), h()@38 (defines the or
			// result), return …@16 (Uses the or result). Succ: p@23→{g(*p),
			// p and g(*p)}; g(*p)→p and g(*p)→{h(), return}; h()→return.
			name:     "the alternative tokens and and or short-circuit",
			protects: "`p and g(*p)` evaluates g(*p) only when p is nonzero, so the dereference depends on the test",
			mutation: "match only the && and || tokens (both Branch nodes and their control dependences vanish), or let each consumer re-read the operands' names (p@11 pairs with p and g(*p)@23 and the return, and the pairs from p@23, g(*p)@29, p and g(*p)@23 and h()@38 to their consumers vanish)",
			src:      "int f(int *p) { return p and g(*p) or h(); }",
			cd:       []string{"p@23 -> g(*p)@29", "p and g(*p)@23 -> h()@38"},
			du: []string{"p@11 -> p@23", "p@11 -> g(*p)@29", "p@23 -> p and g(*p)@23", "g(*p)@29 -> p and g(*p)@23",
				"p and g(*p)@23 -> return p and g(*p) or h();@16", "h()@38 -> return p and g(*p) or h();@16"},
		},
		{
			// [dcl.init.ref]/5: `int &r = x` binds r to x, so a write through
			// r writes x; the binding is the may-definition of x (the
			// address-taking rule), and the write `r = 2` through the
			// reference is given up. Nodes: x = 0@14, &r = x@25 (Uses x,
			// defines r, may-defines x), r = 2@33, return x;@40.
			name:     "binding a reference to a local may-defines the local",
			protects: "a use of x after a reference is bound to it sees the writes the reference may make",
			mutation: "lower a reference's initializer as a plain read (loses &r = x@25 -> return x;@40)",
			src:      "int f() { int x = 0; int &r = x; r = 2; return x; }",
			du:       []string{"x = 0@14 -> &r = x@25", "x = 0@14 -> return x;@40", "&r = x@25 -> return x;@40"},
		},
		{
			// [dcl.ref]/1 and [dcl.init.ref]/5: `const int &c = x` binds c to
			// x as a const int, a read-only view, so the binding only reads
			// x; `int &r = x` binds a reference to a non-const type and is a
			// may-definition of x. Nodes: x = 0@14, &c = x@31 (Uses x,
			// defines c), &r = x@43 (Uses x, defines r, may-defines x),
			// return x;@51. No pair leaves &c = x@31.
			name:     "a reference to a const type only reads the local it binds",
			protects: "a const reference binding adds no may-definition of the bound local, while a non-const reference binding still does",
			mutation: "treat every reference binding as a may-definition (adds &c = x@31 -> &r = x@43 and &c = x@31 -> return x;@51), or none (loses &r = x@43 -> return x;@51)",
			src:      "int f() { int x = 0; const int &c = x; int &r = x; return x; }",
			du: []string{"x = 0@14 -> &c = x@31", "x = 0@14 -> &r = x@43", "x = 0@14 -> return x;@51",
				"&r = x@43 -> return x;@51"},
		},
		{
			// [except.pre]/4: a function-try-block's handlers catch exceptions
			// from the member initializers and the body; [except.handle]/15:
			// the end of a constructor's handler rethrows. Rethrowing and
			// returning both reach EXIT, so the rethrow adds no pair; the case
			// pins that the try covers the initializers. Nodes: x@24,
			// a(g(x))@33 (may throw), h()@43 (may throw), the Handler
			// catch@50, k()@64. Succ: a(g(x))→{h(), catch}; h()→{EXIT,
			// catch}; catch→k()→EXIT. IPDom: every node → EXIT; catch → k().
			name:     "a constructor's function-try-block covers its member initializers",
			protects: "the try statement a constructor's definition holds without a body field is lowered, and a throwing member initializer reaches its handlers",
			mutation: "lower only a body-field function-try-block (the constructor lowers to x@24 alone: every pair vanishes), or lower the initializers outside the try (a(g(x))@33 loses control of catch@50 and k()@64)",
			src:      "struct S { int a; S(int x) try : a(g(x)) { h(); } catch (...) { k(); } };",
			cd: []string{"a(g(x))@33 -> h()@43", "a(g(x))@33 -> catch@50", "a(g(x))@33 -> k()@64", "h()@43 -> catch@50",
				"h()@43 -> k()@64"},
			du: []string{"x@24 -> a(g(x))@33"},
		},
		{
			// [dcl.struct.bind]/1: a structured binding declaration introduces
			// one object, initialized once from the initializer, and each
			// name refers to an element of it. Nodes: p@8, p@27 (the
			// initializer, evaluated once, defining an owned variable), a@19,
			// b@22 (each defining its name and Using that variable), return
			// a + b;@30. Succ: a straight line to EXIT.
			name:     "a structured binding evaluates its initializer once",
			protects: "each name a structured binding declares reaches the initializer through the one node that evaluates it, never by re-reading the initializer's names",
			mutation: "let each bound name re-read the initializer's names (p@27 vanishes, and p@8 pairs with a@19 and b@22 directly)",
			src:      "int f(P p) { auto [a, b] = p; return a + b; }",
			du: []string{"p@8 -> p@27", "p@27 -> a@19", "p@27 -> b@22", "a@19 -> return a + b;@30",
				"b@22 -> return a + b;@30"},
		},
		{
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
			// [class.mfct.non.static]/2: a member named without `this->` in a
			// member function is `(*this).m`, a member, no variable of f.
			// Nodes: y@42, m = y@47 (Uses y, defines nothing), m += y@54
			// (Uses y only), m++@62 (Uses nothing), m = y@72 (the embedded
			// assignment: Uses y, defines the owned result), g(0, m = y)@67
			// (Uses that result), g(0, &m)@80 (Uses and may-defines nothing),
			// the lambda [&] { m = 1; }@99 (captures nothing, defines its
			// owned result), l = …@95 (Uses it, defines l), v@130 (the range,
			// defining the iteration variable), &e : v@125 (the head, Using
			// it, the range base may-defining nothing), e@126 (Uses it), g(0,
			// e)@133, delete p@142 (Uses nothing). The calls take a first
			// argument so no statement reads as a declaration ([stmt.ambig]).
			// Succ: a straight line to the head; head→{e, delete p};
			// e→g(0, e)→head. IPDom: head → delete p; e → g(0, e) → head.
			name:     "a member named without its object is no variable in any position",
			protects: "an assignment, compound assignment, update, embedded assignment, address-taking, by-reference capture, iterated range and deletion of a name that resolves to no variable make their nodes with their other operands' reads and never reach the builder as a variable",
			mutation: "drop read's filter (m += y@54 indexes the held reads with -1 and panics), def's (Builder.Def panics at m = y@47), mayDef's (Builder.MayDef panics at g(0, &m)@80), or mayDefs' (the range base v reaches Builder.MayDef at e@126 and panics; with capWrite's filter dropped too, so does the lambda's by-reference write of m at [&] { m = 1; }@99)",
			src:      "struct S { int m; int *p; V v; void f(int y) { m = y; m += y; m++; g(0, m = y); g(0, &m); auto l = [&] { m = 1; }; for (auto &e : v) g(0, e); delete p; } };",
			cd:       []string{"&e : v@125 -> e@126", "&e : v@125 -> g(0, e)@133", "&e : v@125 -> &e : v@125"},
			du: []string{"y@42 -> m = y@47", "y@42 -> m += y@54", "y@42 -> m = y@72", "m = y@72 -> g(0, m = y)@67",
				"[&] { m = 1; }@99 -> l = [&] { m = 1; }@95", "v@130 -> &e : v@125", "v@130 -> e@126",
				"e@126 -> g(0, e)@133"},
		},
	})
}
