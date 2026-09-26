package worker

import "testing"

// TestTypeScriptLoweringGolden pins the control-dependence and def-use pairs
// of hand-derived TypeScript and TSX callables. The shared cases run under
// both grammars, since TSX is TypeScript with JSX; the JSX case runs under
// tsx only. Pairs are derived as in TestJavaScriptLoweringGolden, from the
// granularity lowerJavaScript documents under "TypeScript". A TypeScript-only
// construct is anchored in the compiler's documented emit, the JavaScript it
// compiles to, and that JavaScript's runtime semantics in ECMA-262; each case
// says which it cites.
func TestTypeScriptLoweringGolden(t *testing.T) {
	shared := []goldenCase{
		{
			// The compiler's emit: a `declare` declaration emits no code, so
			// the declared class's body is no callable and f is the
			// program's first nested callable (fn 1). f starts at 30: x@41
			// defines x, the if's Branch x@58 (ECMA-262 §14.6.2) reads it and
			// controls g()@61.
			name:     "a class body under declare is not a callable",
			protects: "Functions does not walk an ambient declaration, so declared-only code is never lowered as a function",
			mutation: "descend into ambient kinds in Functions (fn 1 becomes the declared class body, with no pairs)",
			src:      "declare class C { m(): void } function f(x: number) { if (x) g(); }",
			fn:       1,
			cd:       []string{"x@58 -> g()@61"},
			du:       []string{"x@41 -> x@58"},
		},
		{
			// The compiler's emit removes the annotation; the default is
			// ECMA-262's (§10.2.11 FunctionDeclarationInstantiation, the
			// default evaluated only for an undefined argument). Nodes: a@11
			// defines a; the
			// wrapper `b = a` is a Branch@22 defining b from the argument,
			// then a@26 defines b from the default reading a; return b@31.
			// return post-dominates both, so only a@26 depends on @22.
			name:     "a parameter wrapper with a type and a default is its pattern",
			protects: "required parameters are unwrapped to their pattern and a default follows the default rule, whatever the annotation",
			mutation: "drop the required_parameter case in bind (no parameter is defined: every pair is lost)",
			src:      "function f(a: number, b = a) { return b; }",
			fn:       1,
			cd:       []string{"b = a@22 -> a@26"},
			du:       []string{"a@11 -> a@26", "b = a@22 -> return b;@31", "a@26 -> return b;@31"},
		},
		{
			// The compiler's emit removes type arguments, so the `typeof y`
			// inside `M<typeof y>` reads nothing at run time, and emits `x!`
			// and `z as boolean` as their operands. Nodes: x@11, y@23, the
			// declarator@40 reading
			// x, the if's Branch spanning `z as boolean`@69 reading z, g()@83,
			// return z@88.
			name:     "type arguments read nothing and non-null and as are transparent",
			protects: "an erased subtree inside a walked expression records no read, and a wrapped operand is still read",
			mutation: "drop the erased check in value (y@23 gains a pair with the declarator)",
			src:      "function f(x: unknown, y: number) { let z = new M<typeof y>(x!); if (z as boolean) g(); return z; }",
			fn:       1,
			cd:       []string{"z as boolean@69 -> g()@83"},
			du: []string{
				"x@11 -> z = new M<typeof y>(x!)@40",
				"z = new M<typeof y>(x!)@40 -> z as boolean@69", "z = new M<typeof y>(x!)@40 -> return z;@88",
			},
		},
		{
			// The compiler's emit drops a non-null assertion, so `x! = 1` is
			// ECMA-262's `x = 1` (§13.15.2): the one node x! = 1@24 kills
			// x@11 before return x@32.
			name:     "an assignment through a non-null assertion defines the variable",
			protects: "assignment targets are read through the transparent wrappers",
			mutation: "strip only parentheses from an assignment target (x! = 1 defines nothing and return x pairs with x@11)",
			src:      "function f(x: number) { x! = 1; return x; }",
			fn:       1,
			du:       []string{"x! = 1@24 -> return x;@32"},
		},
		{
			// The compiler's emit: an interface and a type alias emit no
			// code. The if's Branch x@28 has an empty then arm, so nothing
			// depends on it; the interface's `typeof x` reads nothing.
			name:     "interfaces and type aliases produce no node",
			protects: "an erased declaration makes no node and no read, even inside a branch",
			mutation: "lower an erased declaration as a plain statement (it becomes control dependent on x@28 and the interface a use of x)",
			src:      "function f(x: number) { if (x) { interface I { y: typeof x } type T = I; } return x; }",
			fn:       1,
			du:       []string{"x@11 -> x@28", "x@11 -> return x;@75"},
		},
		{
			// The compiler's emit: an enum in a function body is `let E;
			// (function (E) { … })(E || (E = {}))`, whose body assigns each
			// member as a property of E, and inside an initializer a
			// member's name is read as that property. Nodes: a@11, c@22,
			// E@40 (Uses and defines E; no
			// definition reaches the use), the member a = c@44 reading c,
			// the member b = a@51 reading nothing, return E@59.
			name:     "an enum defines its name and its member names shadow",
			protects: "an enum is a runtime definition whose initializers are evaluated, and a member name inside an initializer is not an enclosing variable",
			mutation: "drop the member-name bindings (a@11 -> b = a@51 appears), or treat an enum as erased (E@40 and a = c@44 vanish)",
			src:      "function f(a: number, c: number) { enum E { a = c, b = a } return E; }",
			fn:       1,
			du:       []string{"c@22 -> a = c@44", "E@40 -> return E;@59"},
		},
		{
			// The compiler's emit: a namespace is `var N; (function (N) { …
			// })(N || (N = {}))`, its body an immediately invoked function
			// that runs once, where it stands (ECMA-262 §13.3.6 EvaluateCall),
			// so it is lowered inline. Program nodes: x = 1@4, N@21, x =
			// 2@25, g(x)@34; x = 2 kills x = 1.
			name:     "a namespace body runs inline where it stands",
			protects: "a namespace's statements are ordinary definitions of the enclosing function, not a closure's may-definitions",
			mutation: "lower a namespace body as a nested callable (g(x) also pairs with x = 1@4, a may-definition)",
			src:      "let x = 1; namespace N { x = 2; } g(x);",
			fn:       0,
			du:       []string{"x = 2@25 -> g(x)@34"},
		},
		{
			// The compiler's emit: `import m = require("m")` and `import a =
			// m.b` are `var m = require("m")` and `var a = m.b` where they
			// stand. Program nodes: m@7 defines m, a@32 reads m and defines a,
			// g(a)@41 reads a.
			name:     "import-equals declarations are defining nodes",
			protects: "an import-equals declaration defines its name at its position and an alias reads its target's root",
			mutation: "make an import-equals declaration no node (m@7 and a@32 vanish with their pairs)",
			src:      "import m = require(\"m\"); import a = m.b; g(a);",
			fn:       0,
			du:       []string{"m@7 -> a@32", "a@32 -> g(a)@41"},
		},
		{
			// The compiler's emit: a class decorator is applied by a call
			// where the class is defined (`A = __decorate([d], A)`, a call,
			// ECMA-262 §13.3.6), after d is evaluated. The class node
			// @d class A {}@27 reads d and may throw, so it controls the
			// catch's Handler catch@43, e@50 and g()@55 (`}` closes the try
			// block at 41, `catch` spans 43-47, `(` is 49, `{` 53).
			name:     "a decorated class reads and may throw at its creation",
			protects: "decorators are evaluated expressions read by the decorated class's node, and their application may throw",
			mutation: "stop counting a decorator as a throwing call (the class node controls nothing)",
			src:      "function f(d: any) { try { @d class A {} } catch (e) { g(); } }",
			fn:       1,
			cd: []string{
				"@d class A {}@27 -> catch@43", "@d class A {}@27 -> e@50", "@d class A {}@27 -> g()@55",
			},
			du: []string{"d@11 -> @d class A {}@27"},
		},
		{
			// The compiler's emit drops an abstract method, which has no
			// body, and emits a parameter property as a parameter assigned
			// to the instance in the constructor. Functions preorder: program
			// 0, class body 1, constructor 2. Nodes: x@58 (inside `public x:
			// number`), y@69 (optional), g(x, y)@83.
			name:     "a body-less signature is not a callable and parameter properties are parameters",
			protects: "callable indexes skip abstract signatures, and modifiers and optional markers do not hide the bound name",
			mutation: "count abstract_method_signature as a callable (callable 2 is the signature), or bind the whole parameter property rather than its pattern",
			src:      "abstract class A { abstract m(): void; constructor(public x: number, y?: number) { g(x, y); } }",
			fn:       2,
			du:       []string{"x@58 -> g(x, y)@83", "y@69 -> g(x, y)@83"},
		},
		{
			// A TypeScript field is a public_field_definition, emitted as the
			// field without its annotation; its initializer runs in the class
			// body's unit (ECMA-262 §15.7.14 ClassDefinitionEvaluation).
			// Branch c@22, arms 1@26 and 2@30.
			name:     "a typed field initializer is lowered in the class body's unit",
			protects: "public_field_definition takes field_definition's place in the class body unit",
			mutation: "match only field_definition in classBody (the initializer makes no node and cd is empty)",
			src:      "class A { x: number = c ? 1 : 2; }",
			fn:       1,
			cd:       []string{"c@22 -> 1@26", "c@22 -> 2@30"},
		},
	}
	shared = append(shared,
		goldenCase{
			// The compiler's emit of `f?.(a)` is ECMA-262's optional call
			// (§13.3.9 OptionalChain): when f is null or undefined nothing
			// right of `?.` is evaluated. Nodes: g@11, x = 0@25, the Branch
			// g@32 before `?.`, x = 1@36 and the chain g?.(x = 1)@32 (Uses g
			// and the x it read back) on the non-nullish path, return x;@44,
			// reached by x = 0 on the nullish path and x = 1 on the other.
			name:     "a TypeScript optional call short-circuits its arguments",
			protects: "the anonymous `?.` of a TypeScript optional call is an optional chain, so its arguments are conditional and the definition before them still reaches past the call",
			mutation: "test only the optional_chain field in chain (g@32 and its control dependences vanish, and return x;@44 loses x = 0@25)",
			src:      "function f(g: any) { let x = 0; g?.(x = 1); return x; }",
			fn:       1,
			cd:       []string{"g@32 -> x = 1@36", "g@32 -> g?.(x = 1)@32"},
			du: []string{
				"g@11 -> g@32", "g@11 -> g?.(x = 1)@32", "x = 1@36 -> g?.(x = 1)@32",
				"x = 0@25 -> return x;@44", "x = 1@36 -> return x;@44",
			},
		},
		goldenCase{
			// The compiler's emit of `x as T` is x: the type `typeof y`
			// evaluates nothing. Nodes: x@11, y@19, the arrow
			// () => x as typeof y@39 (Uses its capture x), the declarator
			// g = () => x as typeof y@35 (Uses x, defines g), return g;@60.
			name:     "a closure's captures skip the type of an as expression",
			protects: "the capture collection walks only the operand of a transparent wrapper, never its type",
			mutation: "walk every child of an as expression in cap (y@19 pairs with the arrow and the declarator)",
			src:      "function f(x: any, y: any) { const g = () => x as typeof y; return g; }",
			fn:       1,
			du: []string{
				"x@11 -> () => x as typeof y@39", "x@11 -> g = () => x as typeof y@35",
				"g = () => x as typeof y@35 -> return g;@60",
			},
		},
		goldenCase{
			// The compiler's emit of `[y!] = a` is `[y] = a`, an ECMA-262
			// destructuring assignment (§13.15.5.5 IteratorDestructuringAssignmentEvaluation)
			// that binds y. Nodes: a@11, y = 0@25, the element y@33 (Uses a,
			// defines y), return y;@42; y@33 kills y = 0.
			name:     "a destructuring element through a non-null assertion defines the variable",
			protects: "pattern elements are read through the transparent wrappers, so the element kills the earlier definition",
			mutation: "drop the strip at the start of bind (y@33 defines nothing: a@11 -> y@33 and y@33 -> return y;@42 vanish and y = 0@25 -> return y;@42 appears)",
			src:      "function f(a: any) { let y = 0; [y!] = a; return y; }",
			fn:       1,
			du:       []string{"a@11 -> y@33", "y@33 -> return y;@42"},
		},
		goldenCase{
			// The compiler's emit of an enum outside a file's top level is a
			// `let` where it stands (ECMA-262 §14.3.1: block-scoped), so the
			// enum inside the braces binds its own E. Nodes: c@11, E@19, the
			// if's Branch c@33, the enum's node E@43 (Uses and defines the
			// inner E, which no definition reaches), return E;@53, which reads
			// the parameter.
			name:     "an enum in a block is scoped to the block",
			protects: "a non-const enum's name is bound in its block, never merged into an enclosing variable of the same name",
			mutation: "hoist a non-const enum's name into the function scope (it merges into the parameter: E@19 -> E@43 and E@43 -> return E;@53 appear)",
			src:      "function f(c: any, E: any) { if (c) { enum E { a } } return E; }",
			fn:       1,
			cd:       []string{"c@33 -> E@43"},
			du:       []string{"c@11 -> c@33", "E@19 -> return E;@53"},
		},
		goldenCase{
			// The compiler's emit drops `implements I`, and a method body
			// runs only when the method is called, so creating A evaluates
			// neither and its node class A implements I { … }@21 may not
			// throw (ECMA-262 §15.7.14). No throwing node reaches the catch,
			// so no Handler is made: there is no decision and no read.
			name:     "an implements clause and a decorator inside a method body are not throws of the class",
			protects: "only an extends clause and the decorators evaluated where the class is created make its creation a throw point",
			mutation: "count an implements-only heritage as a throw, or keep the decorator count of a method body for its class (either way the class node controls catch@70, e@77 and h()@82)",
			src:      "function f() { try { class A implements I { m() { @d class B {} } } } catch (e) { h(); } }",
			fn:       1,
		},
	)
	// The JavaScript switch, compound-write and class-creation rules hold on
	// both grammars; the sources carry no type syntax, so the offsets are
	// JavaScript's.
	shared = append(shared,
		goldenCase{
			// ECMA-262 §14.12.4 CaseClauseIsSelected; derivation as in the
			// JavaScript case of the same name.
			name:     "every case test reads the discriminant",
			protects: "a case test's decision depends on the discriminant's variables, so their definitions reach every test",
			mutation: "reset the discriminant's reads before each case test (x@11 -> 1@34 and x@11 -> 2@54 vanish)",
			src:      "function f(x) { switch (x) { case 1: g(); break; case 2: h(); } }",
			fn:       1,
			cd:       []string{"1@34 -> g()@37", "1@34 -> break;@42", "1@34 -> 2@54", "2@54 -> h()@57"},
			du:       []string{"x@11 -> x@24", "x@11 -> 1@34", "x@11 -> 2@54"},
		},
		goldenCase{
			// ECMA-262 §13.15.2; derivation as in the JavaScript case of
			// the same name.
			name:     "a compound property assignment reads the property before its right side",
			protects: "the read of a compound property assignment throws before the right side is evaluated, and the store after it, each on its own node",
			mutation: "count the read's throw with the store, after the right side (o.p@25 vanishes with its five control dependences and o@11 -> o.p@25)",
			src:      "function f(o, c) { try { o.p += c ? 1 : 2 } catch (e) { h() } }",
			fn:       1,
			cd: []string{
				"o.p@25 -> c@32", "o.p@25 -> o.p += c ? 1 : 2@25",
				"o.p@25 -> catch@44", "o.p@25 -> e@51", "o.p@25 -> h()@56",
				"c@32 -> 1@36", "c@32 -> 2@40",
				"o.p += c ? 1 : 2@25 -> catch@44", "o.p += c ? 1 : 2@25 -> e@51", "o.p += c ? 1 : 2@25 -> h()@56",
			},
			du: []string{
				"o@11 -> o.p@25", "c@14 -> c@32",
				"o@11 -> o.p += c ? 1 : 2@25", "c@14 -> o.p += c ? 1 : 2@25",
			},
		},
		goldenCase{
			// ECMA-262 §15.7.14; derivation as in the JavaScript case of the
			// same name, here through public_field_definition.
			name:     "a class whose static initializer runs at its creation may throw there",
			protects: "a static field initializer, a static block or a computed key is a throw point of the class's creation",
			mutation: "count only a heritage and a decorator as a class-level throw (the class node controls nothing and the catch is unreachable)",
			src:      "function f() { try { class A { static x = g(); } } catch (e) { h(); } }",
			fn:       1,
			cd: []string{
				"class A { static x = g(); }@21 -> catch@51", "class A { static x = g(); }@21 -> e@58",
				"class A { static x = g(); }@21 -> h()@63",
			},
		},
	)
	runGolden(t, "typescript", shared)
	runGolden(t, "tsx", append(shared, goldenCase{
		// JSX (the JSX specification's JSXElement): a capitalized tag name
		// is a reference. && is ECMAScript §13.13: Branch a@40, the right
		// operand <B />@45 reading B, the return@33 reading both.
		name:     "a JSX element in TSX reads its component",
		protects: "TSX resolves the JSX kinds its grammar has, so a component tag is a read and a conditional operand",
		mutation: "resolve the JSX kinds against the typescript grammar for tsx (<B /> reads nothing)",
		src:      "function f(a: boolean, B: any) { return a && <B />; }",
		fn:       1,
		cd:       []string{"a@40 -> <B />@45"},
		du: []string{
			"a@11 -> a@40", "B@23 -> <B />@45",
			"a@11 -> return a && <B />;@33", "B@23 -> return a && <B />;@33",
		},
	}))
}
