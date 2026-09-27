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
		{
			// [dcl.init.ref]/5: a reference to a non-const type binds to the
			// object its initializer denotes, and std::move(x) is a cast to an
			// rvalue referring to x itself ([utility.syn], [forward]), so each
			// binding may-defines x (lower.go, Address-taking). The
			// comment inside the call's parentheses is no argument, so the call
			// still has its one operand. Nodes: x@10, &&r = std::move(/* c */
			// x)@19, &q{x}@51 (each Uses x, defines its reference and
			// may-defines x), return x;@58. Succ: a straight line to EXIT.
			// Each binding pairs with x@10, the killing definition behind the
			// chain, and with the binding before it, its nearest
			// may-definition; so does the return with the last.
			name:     "a reference bound through std::move or braces may-defines the local",
			protects: "an rvalue reference bound to std::move(x), a comment among its arguments included, and a braced reference binding reach the uses of x after them",
			mutation: "stop baseIdent at a call, or count the comment as a second argument of std::move (either way &&r = std::move(/* c */ x)@19 makes no may-definition: loses &&r = std::move(/* c */ x)@19 -> &q{x}@51), or read a braced reference initializer as a plain value (loses &q{x}@51 -> return x;@58)",
			src:      "int f(int x) { int &&r = std::move(/* c */ x); int &q{x}; return x; }",
			du: []string{"x@10 -> &&r = std::move(/* c */ x)@19", "x@10 -> &q{x}@51",
				"&&r = std::move(/* c */ x)@19 -> &q{x}@51", "x@10 -> return x;@58", "&q{x}@51 -> return x;@58"},
		},
		{
			// [expr.prim.lambda.closure]: a lambda is a function of its own,
			// fn 1 in preorder. Its parameter x is a node after Entry; the
			// captured a is no variable of the lambda's own function. Nodes:
			// x@32, return x + a;@37.
			name:      "a lambda is lowered as its own function with its parameters",
			protects:  "the lambda's body is a callable whose parameters are defined at its entry",
			mutation:  "lower a lambda without its parameters (loses x@32 -> return x + a;@37)",
			src:       "int f(int a) { auto l = [a](int x) { return x + a; }; return l(1); }",
			fn:        1,
			callables: 2,
			du:        []string{"x@32 -> return x + a;@37"},
		},
		{
			// [dcl.fct.default]: a default argument is evaluated by the caller,
			// so the callee sees only its parameter. Nodes: x@10, return x;@21.
			name:     "a default argument makes no node",
			protects: "the parameter's entry node is its only definition, whatever the default",
			mutation: "lower the default as a node defining the parameter (adds g()@14 -> return x;@21, loses x@10 -> return x;@21)",
			src:      "int f(int x = g()) { return x; }",
			du:       []string{"x@10 -> return x;@21"},
		},
		{
			// [dcl.init]/7: an object of class type without an initializer is
			// default-initialized by its constructor, which may throw. Nodes:
			// w@18 (defines w, may throw), return w.n;@21, the Handler
			// catch@35, return 0;@49. Succ: w→{return w.n, catch};
			// catch→return 0. IPDom: w → EXIT.
			name:     "a class-type declarator without an initializer is a throwing definition",
			protects: "a default-constructed local is defined at its declarator and its constructor reaches the handler",
			mutation: "make no node for a declarator without an initializer (every pair vanishes)",
			src:      "int f() { try { V w; return w.n; } catch (...) { return 0; } }",
			cd:       []string{"w@18 -> return w.n;@21", "w@18 -> catch@35", "w@18 -> return 0;@49"},
			du:       []string{"w@18 -> return w.n;@21"},
		},
		{
			// [dcl.stc]/5: an extern block-scope declaration refers to an
			// object defined elsewhere and constructs nothing. Nodes: x@10,
			// x@19 (Branch), return 0;@38; `extern V e;` makes no node.
			name:     "an extern declaration makes no node",
			protects: "an extern declaration of a class-type object is not a default initialization",
			mutation: "drop the extern check in classType (adds x@19 -> e@33)",
			src:      "int f(int x) { if (x) { extern V e; } return 0; }",
			du:       []string{"x@10 -> x@19"},
		},
		{
			// [dcl.struct.bind]/1 and [dcl.init.ref]/5: `auto &[a, b] = p`
			// binds a reference to p, so the initializer's node may-defines p.
			// Nodes: p@8, p@28 (the initializer: Uses and may-defines p,
			// defines the owned variable), a@20, b@23, a = 1@31, return g(0,
			// p);@38.
			name:     "a structured binding by reference may-defines its initializer's base",
			protects: "a write through a name bound by `auto &[a, b] = p` is accounted as a possible write of p",
			mutation: "read a by-reference structured binding's initializer as a plain value (loses p@28 -> return g(0, p);@38)",
			src:      "int f(P p) { auto &[a, b] = p; a = 1; return g(0, p); }",
			du: []string{"p@8 -> p@28", "p@28 -> a@20", "p@28 -> b@23", "p@8 -> return g(0, p);@38",
				"p@28 -> return g(0, p);@38"},
		},
		{
			// [stmt.switch] and [stmt.pre]: a switch condition that declares y
			// initializes y once; the case tests compare against it. Nodes:
			// int y = g()@18 (defines y), 1@38 (the test, Using y), return
			// y;@41, return 0;@53.
			name:     "a switch condition declaration defines the name its tests use",
			protects: "the case tests of a declaring switch condition Use the declared name, which the body also reads",
			mutation: "give the declaring condition no definition of y (loses int y = g()@18 -> 1@38 and int y = g()@18 -> return y;@41)",
			src:      "int f() { switch (int y = g()) { case 1: return y; } return 0; }",
			cd:       []string{"1@38 -> return y;@41", "1@38 -> return 0;@53"},
			du:       []string{"int y = g()@18 -> 1@38", "int y = g()@18 -> return y;@41"},
		},
		{
			// [dcl.ref]/1: in `const int *&r` the reference refers to a
			// non-const pointer (the const qualifies the pointee), so binding
			// it may-defines p. Nodes: p@17, *&r = p@32 (Uses p, defines r,
			// may-defines p), g(0, r)@41, return p != 0;@50.
			name:     "a reference to a pointer to const may-define the pointer",
			protects: "the const nearest the reference decides whether its binding may write, not a const further out",
			mutation: "take the declaration's const as the reference's (loses *&r = p@32 -> return p != 0;@50)",
			src:      "int f(const int *p) { const int *&r = p; g(0, r); return p != 0; }",
			du: []string{"p@17 -> *&r = p@32", "*&r = p@32 -> g(0, r)@41", "p@17 -> return p != 0;@50",
				"*&r = p@32 -> return p != 0;@50"},
		},
		{
			// [expr.delete]: delete p destroys the object p points to and
			// reads p. Nodes: p@12, delete p@17 (Uses p), g(0, p)@27.
			name:     "delete of a local pointer only reads it",
			protects: "deleting through a local is no write of the local",
			mutation: "treat delete's operand as written (adds delete p@17 -> g(0, p)@27)",
			src:      "void f(int *p) { delete p; g(0, p); }",
			du:       []string{"p@12 -> delete p@17", "p@12 -> g(0, p)@27"},
		},
		{
			// [expr.new]: the allocation or the constructor may throw. Nodes:
			// x@11, *p = 0@20, p = new int(x)@34 (may throw), the Handler
			// catch@52, return 0;@66, return p;@78. Succ: p = new int(x)→
			// {return p, catch}; catch→return 0. IPDom: p = new int(x) → EXIT.
			name:     "a new expression may throw",
			protects: "an allocation in a try body reaches its handler",
			mutation: "count no throw for new (p = new int(x)@34 controls nothing)",
			src:      "int *f(int x) { int *p = 0; try { p = new int(x); } catch (...) { return 0; } return p; }",
			cd: []string{"p = new int(x)@34 -> catch@52", "p = new int(x)@34 -> return 0;@66",
				"p = new int(x)@34 -> return p;@78"},
			du: []string{"x@11 -> p = new int(x)@34", "p = new int(x)@34 -> return p;@78"},
		},
		{
			// [expr.delete]: the destructor and the deallocation function may
			// throw. Nodes: p@9, delete p@20 (may throw), the Handler catch@32,
			// return 1;@46, return 0;@58.
			name:     "a delete expression may throw",
			protects: "a deletion in a try body reaches its handler",
			mutation: "count no throw for delete (delete p@20 controls nothing)",
			src:      "int f(S *p) { try { delete p; } catch (...) { return 1; } return 0; }",
			cd:       []string{"delete p@20 -> catch@32", "delete p@20 -> return 1;@46", "delete p@20 -> return 0;@58"},
			du:       []string{"p@9 -> delete p@20"},
		},
		{
			// [dcl.init]/16: a parenthesized or braced initializer calls a
			// constructor, which may throw. Two arguments keep `V u(x, 1)` from
			// reading as a function declaration ([dcl.ambig.res]). Nodes: x@10,
			// u(x, 1)@23, w{x}@34 (each may throw), the Handler catch@42,
			// return 1;@56, return 0;@68. Succ: u→{w, catch}; w→{return 0,
			// catch}; catch→return 1. IPDom: u, w → EXIT; catch → return 1.
			name:     "direct initialization may throw",
			protects: "a constructor call written as a parenthesized or braced initializer reaches the handler",
			mutation: "count no throw for an argument-list or braced initializer (u(x, 1)@23 and w{x}@34 control nothing)",
			src:      "int f(int x) { try { V u(x, 1); V w{x}; } catch (...) { return 1; } return 0; }",
			cd: []string{"u(x, 1)@23 -> w{x}@34", "u(x, 1)@23 -> catch@42", "u(x, 1)@23 -> return 1;@56",
				"w{x}@34 -> return 0;@68", "w{x}@34 -> catch@42", "w{x}@34 -> return 1;@56"},
			du: []string{"x@10 -> u(x, 1)@23", "x@10 -> w{x}@34"},
		},
		{
			// [except.pre]/4: a function-try-block of an ordinary function
			// covers its body; the end of its handler returns. Nodes: x@10,
			// return g(x);@19 (may throw), the Handler catch@34, return x;@48.
			// Succ: return g(x)→{EXIT, catch}; catch→return x.
			name:     "a function-try-block of an ordinary function is lowered",
			protects: "a definition whose body is a try statement lowers the try with its handlers",
			mutation: "lower only a compound body (the handlers vanish: loses return g(x);@19 -> catch@34 and x@10 -> return x;@48)",
			src:      "int f(int x) try { return g(x); } catch (...) { return x; }",
			cd:       []string{"return g(x);@19 -> catch@34", "return g(x);@19 -> return x;@48"},
			du:       []string{"x@10 -> return g(x);@19", "x@10 -> return x;@48"},
		},
		{
			// [basic.scope.block]/2: a handler's parameter is scoped to the
			// handler. Nodes: e@10, g()@21 (may throw), the Handler catch@28,
			// (int e)@34 (the test, defining the caught e), h(0, e)@44, return
			// e;@55. Succ: g()→{return e, catch}; catch→(int e)→{h(0, e),
			// EXIT}; h(0, e)→return e. IPDom: g(), (int e) → EXIT; catch →
			// (int e).
			name:     "a catch parameter is scoped to its handler",
			protects: "the caught name shadows the parameter inside the handler only",
			mutation: "keep the catch parameter's binding after the handler (return e;@55 pairs with (int e)@34 instead of e@10)",
			src:      "int f(int e) { try { g(); } catch (int e) { h(0, e); } return e; }",
			cd: []string{"g()@21 -> catch@28", "g()@21 -> (int e)@34", "g()@21 -> return e;@55",
				"(int e)@34 -> h(0, e)@44", "(int e)@34 -> return e;@55"},
			du: []string{"(int e)@34 -> h(0, e)@44", "e@10 -> return e;@55"},
		},
		{
			// [stmt.pre]/5: a name a condition declares is scoped to the
			// statement. Nodes: x@10, int x = g()@19 (Branch, defining the
			// inner x), h(0, x)@32, return x;@41.
			name:     "a condition declaration is scoped to its statement",
			protects: "the x an if condition declares shadows the parameter inside the if only",
			mutation: "keep the condition's binding after the if (return x;@41 pairs with int x = g()@19 instead of x@10)",
			src:      "int f(int x) { if (int x = g()) h(0, x); return x; }",
			cd:       []string{"int x = g()@19 -> h(0, x)@32"},
			du:       []string{"int x = g()@19 -> h(0, x)@32", "x@10 -> return x;@41"},
		},
		{
			// [stmt.ranged]/1: the for-range-declaration is scoped to the loop.
			// Nodes: v@8, x@15, v@33 (the range, defining the iteration
			// variable), x : v@29 (the head), x@29 (the bound name), h(0,
			// x)@36, return x;@45. Succ: v@33→head→{x@29, return x};
			// x@29→h(0, x)→head.
			name:     "a range for's name is scoped to the loop",
			protects: "the bound name shadows the parameter inside the loop only",
			mutation: "keep the bound name after the loop (return x;@45 pairs with x@29 instead of x@15)",
			src:      "int f(V v, int x) { for (int x : v) h(0, x); return x; }",
			cd:       []string{"x : v@29 -> x@29", "x : v@29 -> h(0, x)@36", "x : v@29 -> x : v@29"},
			du: []string{"v@8 -> v@33", "v@33 -> x : v@29", "v@33 -> x@29", "x@29 -> h(0, x)@36",
				"x@15 -> return x;@45"},
		},
		{
			// [expr.prim.lambda.capture]/7: a name the lambda body declares is
			// its own, so nothing is captured. Nodes: a@10, the lambda @24
			// (Uses nothing, defines its result), l = …@20, l()@51, return
			// a;@56.
			name:     "a name a lambda declares shadows the enclosing one",
			protects: "a write to a lambda's own local is no capture and no may-definition of the enclosing variable",
			mutation: "resolve the lambda body's names in the enclosing scope (adds a@10 -> [&] { int a = 0; a = 1; }@24 and [&] { int a = 0; a = 1; }@24 -> return a;@56)",
			src:      "int f(int a) { auto l = [&] { int a = 0; a = 1; }; l(); return a; }",
			du: []string{"[&] { int a = 0; a = 1; }@24 -> l = [&] { int a = 0; a = 1; }@20",
				"l = [&] { int a = 0; a = 1; }@20 -> l()@51", "a@10 -> return a;@56"},
		},
		{
			// [expr.prim.lambda.capture]/12: a by-reference capture whose
			// address the body takes may be written through it. Nodes: a@10,
			// the lambda @24 (Uses and may-defines a), l = …@20, l()@43,
			// return a;@48.
			name:     "a by-reference capture whose address is taken is a may-definition",
			protects: "taking the address of a variable captured by reference reaches the reads after the lambda",
			mutation: "count only direct writes as by-reference writes (loses [&] { g(0, &a); }@24 -> return a;@48)",
			src:      "int f(int a) { auto l = [&] { g(0, &a); }; l(); return a; }",
			du: []string{"a@10 -> [&] { g(0, &a); }@24", "[&] { g(0, &a); }@24 -> l = [&] { g(0, &a); }@20",
				"l = [&] { g(0, &a); }@20 -> l()@43", "a@10 -> return a;@48", "[&] { g(0, &a); }@24 -> return a;@48"},
		},
		{
			// [expr.prim.lambda.capture]/6: `&r = x` declares a reference bound
			// to x, `y = x` a copy. Nodes: x@10, the lambda @24 (Uses x for
			// both init-captures, may-defines x for &r), l = …@20, l()@69,
			// return x;@74.
			name:     "a by-reference init-capture may-defines what it binds",
			protects: "a reference init-capture reaches the reads of its bound variable after the lambda, a copy init-capture does not",
			mutation: "treat `&r = x` as a copy (loses [&r = x, y = x]() mutable { r = 1; y = 2; }@24 -> return x;@74)",
			src:      "int f(int x) { auto l = [&r = x, y = x]() mutable { r = 1; y = 2; }; l(); return x; }",
			du: []string{"x@10 -> [&r = x, y = x]() mutable { r = 1; y = 2; }@24",
				"[&r = x, y = x]() mutable { r = 1; y = 2; }@24 -> l = [&r = x, y = x]() mutable { r = 1; y = 2; }@20",
				"l = [&r = x, y = x]() mutable { r = 1; y = 2; }@20 -> l()@69", "x@10 -> return x;@74",
				"[&r = x, y = x]() mutable { r = 1; y = 2; }@24 -> return x;@74"},
		},
		{
			// [dcl.typedef], [namespace.alias] and [dcl.pre]/10: an alias, a
			// namespace alias and a static assertion run nothing. Nodes: x@10,
			// x@19 (Branch), return 0;@76.
			name:     "C++ declarations that run nothing make no node",
			protects: "using, namespace alias and static_assert inside a body are not statements",
			mutation: "lower an alias declaration as a plain Stmt node (adds x@19 -> using T = int;@24)",
			src:      "int f(int x) { if (x) { using T = int; namespace N = M; static_assert(1); } return 0; }",
			du:       []string{"x@10 -> x@19"},
		},
		{
			// [expr.prim.id.qual] and [expr.prim.this]: S::m names the static
			// member and this->m the object's member, neither the parameter m.
			// Nodes: m@35, return this->m + S::m;@40, which reads no variable.
			name:     "a qualified name and a member through this are no reads of a local",
			protects: "a qualified identifier and a this-> member access do not resolve to a parameter of the same name",
			mutation: "read a qualified name's last component (adds m@35 -> return this->m + S::m;@40)",
			src:      "struct S { static int m; int f(int m) { return this->m + S::m; } };",
		},
		{
			// [expr.unary.op]/3: &s.f and &a[0] are addresses into s and a.
			// `S s;` default-initializes s, a node defining it ([dcl.init]/7).
			// Nodes: s@12, g(&s.f, &a[0])@25 (Uses and may-defines s and a),
			// h(0, s.f)@41, return a[1];@52.
			name:     "the address of a member or an element may-defines its base in C++",
			protects: "taking the address of a field or an element reaches later reads of the whole variable, behind the object's own definition",
			mutation: "take no base through &s.f (loses g(&s.f, &a[0])@25 -> h(0, s.f)@41) or through &a[i] (loses g(&s.f, &a[0])@25 -> return a[1];@52)",
			src:      "int f() { S s; int a[2]; g(&s.f, &a[0]); h(0, s.f); return a[1]; }",
			du: []string{"s@12 -> g(&s.f, &a[0])@25", "s@12 -> h(0, s.f)@41", "g(&s.f, &a[0])@25 -> h(0, s.f)@41",
				"g(&s.f, &a[0])@25 -> return a[1];@52"},
		},
		{
			// [stmt.if]/3, [stmt.switch]/3 and [stmt.ranged]/1: an
			// init-statement runs before the condition. Nodes: v@8, y =
			// g()@21, y@30 (Branch), return y;@33, z = g()@55, z@64 (the
			// switch value), 1@74, return z;@77, s = 0@93, w = v@107, w@122
			// (the range), x : w@118 (the head), x@118, s += x@125, return
			// s;@133. Succ: y@30→{return y, z = g()}; z = g()→z@64→1→{return
			// z, s = 0}; s = 0→w = v→w@122→head→{x@118, return s};
			// x@118→s += x→head. IPDom: y@30, 1 → EXIT; head → return s.
			name:     "the init-statements of if, switch and range for run before their conditions",
			protects: "each init-statement is lowered as a statement before its condition, so the condition and the body read what it defines",
			mutation: "drop the initializer of a condition clause (loses y = g()@21 -> y@30, z = g()@55 -> z@64 and w = v@107 -> w@122)",
			src:      "int f(V v) { if (int y = g(); y) return y; switch (int z = g(); z) { case 1: return z; } int s = 0; for (V w = v; int x : w) s += x; return s; }",
			cd: []string{"y@30 -> return y;@33", "y@30 -> z = g()@55", "y@30 -> z@64", "y@30 -> 1@74",
				"1@74 -> return z;@77", "1@74 -> s = 0@93", "1@74 -> w = v@107", "1@74 -> w@122", "1@74 -> x : w@118",
				"1@74 -> return s;@133", "x : w@118 -> x@118", "x : w@118 -> s += x@125", "x : w@118 -> x : w@118"},
			du: []string{"y = g()@21 -> y@30", "y = g()@21 -> return y;@33", "z = g()@55 -> z@64",
				"z = g()@55 -> return z;@77", "z@64 -> 1@74", "v@8 -> w = v@107", "w = v@107 -> w@122",
				"w@122 -> x : w@118", "w@122 -> x@118", "x@118 -> s += x@125", "s = 0@93 -> s += x@125",
				"s += x@125 -> s += x@125", "s = 0@93 -> return s;@133", "s += x@125 -> return s;@133"},
		},
		{
			// [stmt.ranged] and [dcl.init.ref]/5: `auto &e` binds each element
			// of v, so each binding may-defines v. Nodes: v@8, v@28 (the
			// range), &e : v@23 (the head, spanning from the declarator), e@24
			// (Uses the iteration variable, may-defines v), e = 0@31, return
			// g(0, v);@38. Each iteration's binding pairs with the one before
			// it, its nearest may-definition of v.
			name:     "a range for by non-const reference may-defines the range's base",
			protects: "a write through a by-reference loop variable reaches the reads of the range after the loop",
			mutation: "read a non-const reference binding as a copy (loses e@24 -> return g(0, v);@38, v@8 -> e@24 and e@24 -> e@24)",
			src:      "int f(V v) { for (auto &e : v) e = 0; return g(0, v); }",
			cd:       []string{"&e : v@23 -> e@24", "&e : v@23 -> e = 0@31", "&e : v@23 -> &e : v@23"},
			du: []string{"v@8 -> v@28", "v@28 -> &e : v@23", "v@28 -> e@24", "v@8 -> e@24", "e@24 -> e@24",
				"v@8 -> return g(0, v);@38", "e@24 -> return g(0, v);@38"},
		},
		{
			// [dcl.ref]/1: `const auto &e` is a read-only view of each element.
			// Nodes: v@8, s = 0@17, v@45 (the range), &e : v@40 (the head),
			// e@41 (no may-definition), s += e@48, return g(s, v);@56.
			name:     "a range for by const reference only reads the range",
			protects: "a const reference loop variable adds no may-definition of the range's base",
			mutation: "treat a const reference binding as non-const (adds v@8 -> e@41, e@41 -> e@41 and e@41 -> return g(s, v);@56)",
			src:      "int f(V v) { int s = 0; for (const auto &e : v) s += e; return g(s, v); }",
			cd:       []string{"&e : v@40 -> e@41", "&e : v@40 -> s += e@48", "&e : v@40 -> &e : v@40"},
			du: []string{"v@8 -> v@45", "v@45 -> &e : v@40", "v@45 -> e@41", "e@41 -> s += e@48", "s = 0@17 -> s += e@48",
				"s += e@48 -> s += e@48", "s = 0@17 -> return g(s, v);@56", "s += e@48 -> return g(s, v);@56",
				"v@8 -> return g(s, v);@56"},
		},
		{
			// [stmt.ranged] and [dcl.struct.bind]: each name of the binding is
			// defined from the element. Nodes: m@8, s = 0@17, m@43 (the range),
			// [k, x] : m@34 (the head), k@35, x@38 (each Using the iteration
			// variable), s += x@46, return s;@54. Succ: head→{k, return s};
			// k→x→s += x→head.
			name:     "a range for with a structured binding defines each name from the element",
			protects: "every name a structured binding in a range for binds is defined on the body path from the iteration variable",
			mutation: "bind only the first name of a structured binding (loses m@43 -> x@38 and x@38 -> s += x@46)",
			src:      "int f(M m) { int s = 0; for (auto [k, x] : m) s += x; return s; }",
			cd: []string{"[k, x] : m@34 -> k@35", "[k, x] : m@34 -> x@38", "[k, x] : m@34 -> s += x@46",
				"[k, x] : m@34 -> [k, x] : m@34"},
			du: []string{"m@8 -> m@43", "m@43 -> [k, x] : m@34", "m@43 -> k@35", "m@43 -> x@38", "x@38 -> s += x@46",
				"s = 0@17 -> s += x@46", "s += x@46 -> s += x@46", "s = 0@17 -> return s;@54", "s += x@46 -> return s;@54"},
		},
		{
			// [expr.yield] and [expr.await]: co_yield and co_await suspend the
			// coroutine and resume it where it stopped, so neither is a CFG
			// edge. Nodes: x@8, co_yield x;@13 (a plain Stmt node), y =
			// co_await g(x)@29, co_return y;@48.
			name:     "co_yield and co_await fall through",
			protects: "a suspension point is an ordinary statement or operand, so the code after it stays on the path",
			mutation: "lower co_yield as a return (y = co_await g(x)@29 becomes unreachable: loses its pairs)",
			src:      "G f(int x) { co_yield x; int y = co_await g(x); co_return y; }",
			du: []string{"x@8 -> co_yield x;@13", "x@8 -> y = co_await g(x)@29",
				"y = co_await g(x)@29 -> co_return y;@48"},
		},
		{
			// [except.throw]: evaluating the operand g() may itself throw, so
			// the throw is MayThrow as well as a Throw, and the try gets a
			// Handler, which a throw of a constant does not make. Nodes: x@11,
			// x@26 (Branch), throw g();@29, the Handler catch@42, h()@56,
			// k()@63. Succ: x@26→{throw, k()}; throw→{catch (MayThrow), h()
			// (the Throw)}; catch→h()→k(). IPDom: x@26 → k(); throw → h().
			name:     "a throw whose operand calls may throw before it throws",
			protects: "a call in a throw's operand reaches the handler through the Handler node, as any call in a try body does",
			mutation: "give a throw no MayThrow whatever its operand (the Handler vanishes: loses throw g();@29 -> catch@42)",
			src:      "void f(int x) { try { if (x) throw g(); } catch (...) { h(); } k(); }",
			cd:       []string{"x@26 -> throw g();@29", "x@26 -> h()@56", "throw g();@29 -> catch@42"},
			du:       []string{"x@11 -> x@26"},
		},
		{
			// [stmt.ranged]/1: the range is bound and begin and end are called
			// once, each step compares and increments the iterator, and each
			// element is bound from `*__begin`; on a class-type iterator each
			// is a call, which may throw. Nodes: v@8, s = 0@17, v@43 (the
			// range, may throw), x : v@39 (the head: the comparison and the
			// increment, may throw), x@39 (the bound name: the dereference,
			// may throw), s += x@46, the Handler catch@56, return -1;@70,
			// return s;@83. Succ: v@43→{head, catch}; head→{x, return s,
			// catch}; x→{s += x, catch}; s += x→head; catch→return -1. IPDom:
			// v@43, head, x → EXIT; s += x → head; catch → return -1.
			// Frontier walks: head→x gives x; x→s += x gives s += x, head.
			name:     "a range for's range, iterator step and element binding may throw",
			protects: "the range evaluation, each step and each element's dereference of a range for in a try body reach its handler",
			mutation: "count no throw for a range for (catch@56 is unreachable: loses v@43 -> catch@56 and x : v@39 -> catch@56), or none for the element's dereference (x@39 -> catch@56 vanishes and the head controls s += x@46 again)",
			src:      "int f(V v) { int s = 0; try { for (int x : v) s += x; } catch (...) { return -1; } return s; }",
			cd: []string{"v@43 -> x : v@39", "v@43 -> catch@56", "v@43 -> return -1;@70", "x : v@39 -> x@39",
				"x : v@39 -> return s;@83", "x : v@39 -> catch@56", "x : v@39 -> return -1;@70",
				"x@39 -> s += x@46", "x@39 -> x : v@39", "x@39 -> catch@56", "x@39 -> return -1;@70"},
			du: []string{"v@8 -> v@43", "v@43 -> x : v@39", "v@43 -> x@39", "x@39 -> s += x@46", "s = 0@17 -> s += x@46",
				"s += x@46 -> s += x@46", "s = 0@17 -> return s;@83", "s += x@46 -> return s;@83"},
		},
		{
			// [except] with structured exception handling: inside a __try, a
			// dereference in a nested C++ try is also given MayThrow, so it
			// reaches the C++ handler. Nodes: p@11, r = 0@20, r = *p@41, the
			// Handler catch@51, r = 1@65, h()@88 (the __finally), return
			// r;@95. Succ: r = *p→{h(), catch}; catch→r = 1→h()→return r.
			// IPDom: r = *p → h().
			name:     "a dereference in a C++ try inside a __try may throw",
			protects: "the structured-exception dereference rule reaches through a nested C++ try to its handler",
			mutation: "give a dereference MayThrow only directly in a __try body (catch@51 is unreachable: loses r = *p@41 -> catch@51 and r = *p@41 -> r = 1@65)",
			src:      "int f(int *p) { int r = 0; __try { try { r = *p; } catch (...) { r = 1; } } __finally { h(); } return r; }",
			// Ill-formed on purpose: the compilers that implement structured
			// exceptions reject a C++ try and a __try in one function ("only
			// one form of exception handling permitted per function"). The
			// parser accepts it, so the case pins the defined lowering of such
			// a tree: nothing reaches the __finally by a throw, since the
			// catch-all takes every one, so it has no Handler node.
			illFormed: true,
			cd:        []string{"r = *p@41 -> catch@51", "r = *p@41 -> r = 1@65"},
			du:        []string{"p@11 -> r = *p@41", "r = *p@41 -> return r;@95", "r = 1@65 -> return r;@95"},
		},
		{
			// C17 §6.8.3p2 alike ([stmt.expr]): an expression statement is
			// evaluated for its effect, and [expr.log.and]/1: the right operand
			// is evaluated only when the left is true. The right operand is
			// negated so the statement cannot read as a declaration of g
			// ([stmt.ambig]), as `a && g(b);` does. Nodes: a@10, b@17, a@22
			// (Branch, defining the && result), !g(b)@27 (defining it again),
			// return a;@34. The discarded result makes no further node. Succ:
			// a@22→{!g(b), return a}; !g(b)→return a.
			name:     "a discarded && statement is its operands' nodes alone in C++",
			protects: "an expression statement whose value nodes hand on a result nobody reads ends there, and the right operand still depends on the left",
			mutation: "end the statement with a node Using the result (adds a && !g(b)@22, with a@22 -> a && !g(b)@22 and !g(b)@27 -> a && !g(b)@22)",
			src:      "int f(int a, int b) { a && !g(b); return a; }",
			cd:       []string{"a@22 -> !g(b)@27"},
			du:       []string{"a@10 -> a@22", "b@17 -> !g(b)@27", "a@10 -> return a;@34"},
		},
		{
			// [dcl.meaning] and [dcl.array]: in `char (*p)[4]` the declarator
			// nearest p is the pointer, so p is a pointer to an array and does
			// not decay. `static` makes the statement a declaration, which the
			// grammar would otherwise read as an assignment to a functional
			// cast; a static local's initializer is a defining node at its
			// position (see the C table). Nodes: (*p)[4] = 0@26 (defines p),
			// g(0, p)@39 (Uses p, no may-definition), return p != 0;@48.
			name:     "a pointer to an array is no array in C++",
			protects: "the declarator nearest the name decides its shape, so passing a pointer to an array is a plain read",
			mutation: "take any array declarator on the chain as the shape (p decays: adds g(0, p)@39 -> return p != 0;@48)",
			src:      "int f(void) { static char (*p)[4] = 0; g(0, p); return p != 0; }",
			du:       []string{"(*p)[4] = 0@26 -> g(0, p)@39", "(*p)[4] = 0@26 -> return p != 0;@48"},
		},
	})
}
