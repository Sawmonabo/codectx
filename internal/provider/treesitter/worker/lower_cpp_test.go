package worker

import "testing"

// TestCppLoweringGolden pins the C++ additions of the C-family lowering on
// hand-derived functions, with TestCLoweringGolden's derivation rules.
// Offsets are byte offsets into src.
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
			// Nodes: a@10, b@17, the lambda @31 (Uses a and b, may-defines a
			// only), g = …@27 (defines g), g()@68, h(a)@73, return b;@79.
			name:     "a lambda's by-reference capture is a may-definition, a by-copy capture is not",
			protects: "a write through a by-reference capture reaches later uses of the variable, while a write to a by-copy capture stays inside the closure",
			mutation: "may-define every captured variable written (adds [&a, b]() mutable { a = b; b = 0; }@31 -> return b;@79), or none (loses the lambda's pair with h(a)@73)",
			src:      "int f(int a, int b) { auto g = [&a, b]() mutable { a = b; b = 0; }; g(); h(a); return b; }",
			du: []string{"a@10 -> [&a, b]() mutable { a = b; b = 0; }@31", "b@17 -> [&a, b]() mutable { a = b; b = 0; }@31",
				"a@10 -> g = [&a, b]() mutable { a = b; b = 0; }@27", "b@17 -> g = [&a, b]() mutable { a = b; b = 0; }@27",
				"[&a, b]() mutable { a = b; b = 0; }@31 -> g = [&a, b]() mutable { a = b; b = 0; }@27",
				"g = [&a, b]() mutable { a = b; b = 0; }@27 -> g()@68",
				"a@10 -> h(a)@73", "[&a, b]() mutable { a = b; b = 0; }@31 -> h(a)@73", "b@17 -> return b;@79"},
		},
		{
			// [expr.prim.lambda.capture]/7, 12: under a `&` default an
			// odr-used variable is captured by reference. Nodes: a@10, the
			// lambda @24 (Uses a, may-defines a), g = …@20, g()@42, return
			// a;@47.
			name:     "an implicit by-reference capture that is written is a may-definition",
			protects: "a variable captured implicitly under [&] and written in the body is may-defined where the lambda is created",
			mutation: "treat an implicit capture as by copy (loses [&]() { a = 1; }@24 -> return a;@47)",
			src:      "int f(int a) { auto g = [&]() { a = 1; }; g(); return a; }",
			du: []string{"a@10 -> [&]() { a = 1; }@24", "a@10 -> g = [&]() { a = 1; }@20",
				"[&]() { a = 1; }@24 -> g = [&]() { a = 1; }@20", "g = [&]() { a = 1; }@20 -> g()@42",
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
			// [stmt.select]/2–3 (condition declarations): the declared name is
			// in scope in both branches and initialized by the condition.
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
			// x, may-defines x), return x;@31. The may-definition kills
			// nothing, so the return's x pairs with both.
			name:     "taking an address is a may-definition of the variable",
			protects: "a use after a call that received a variable's address sees the write the call may make through it",
			mutation: "lower `&x` as a plain read (g(0, &x)@21 -> return x;@31 vanishes), or make it a killing Def (x = 1@14 -> return x;@31 vanishes)",
			src:      "int f() { int x = 1; g(0, &x); return x; }",
			du:       []string{"x = 1@14 -> g(0, &x)@21", "x = 1@14 -> return x;@31", "g(0, &x)@21 -> return x;@31"},
		},
	})
}
