package worker

import "testing"

// TestTypeScriptLoweringGolden pins the control-dependence and def-use pairs
// of hand-derived TypeScript and TSX callables. The shared cases run under
// both grammars, since TSX is TypeScript with JSX; the JSX case runs under
// tsx only. Pairs are derived as in TestJavaScriptLoweringGolden, from the
// granularity lowerJavaScript documents under "TypeScript". References: the
// TypeScript handbook ("Erased Types", "Enums", "Namespaces", "Decorators",
// "Classes"), and the ECMAScript specification for the runtime forms.
func TestTypeScriptLoweringGolden(t *testing.T) {
	shared := []goldenCase{
		{
			// Handbook "Ambient Declarations": nothing under `declare` runs,
			// so the declared class's body is no callable and f is the
			// program's first nested callable (fn 1). f starts at 30: x@41
			// defines x, the if's Branch x@57 reads it and controls g()@60.
			name:     "a class body under declare is not a callable",
			protects: "Functions does not walk an ambient declaration, so declared-only code is never lowered as a function",
			mutation: "descend into ambient kinds in Functions (fn 1 becomes the declared class body, with no pairs)",
			src:      "declare class C { m(): void } function f(x: number) { if (x) g(); }",
			fn:       1,
			cd:       []string{"x@57 -> g()@60"},
			du:       []string{"x@41 -> x@57"},
		},
		{
			// Handbook "Erased Types": the annotation is removed; a
			// parameter's default is ECMAScript's (§10.2.11
			// FunctionDeclarationInstantiation). Nodes: a@11 defines a; the
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
			// Handbook "Erased Types": type arguments are removed, so the
			// `typeof y` inside `M<typeof y>` reads nothing at run time; `x!`
			// and `z as boolean` evaluate to their operand (non-null and
			// `as` are erased). Nodes: x@11, y@23, the declarator@40 reading
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
			// A non-null assertion is erased (handbook "Non-null Assertion
			// Operator"), so `x! = 1` is ECMAScript's `x = 1` (§13.15.2): the
			// one node x! = 1@24 kills x@11 before return x@32.
			name:     "an assignment through a non-null assertion defines the variable",
			protects: "assignment targets are read through the transparent wrappers",
			mutation: "strip only parentheses from an assignment target (x! = 1 defines nothing and return x pairs with x@11)",
			src:      "function f(x: number) { x! = 1; return x; }",
			fn:       1,
			du:       []string{"x! = 1@24 -> return x;@32"},
		},
		{
			// Handbook "Erased Types": an interface and a type alias emit no
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
			// Handbook "Enums": an enum emits `var E; (function (E) { … })(E ||
			// (E = {}))`, and inside an initializer a member's name refers to
			// the member. Nodes: a@11, c@22, E@40 (Uses and defines E; no
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
			// Handbook "Namespaces": a namespace body runs once, where it
			// stands, as an immediately invoked function, so it is lowered
			// inline. Program nodes: x = 1@4, N@21, x = 2@25, g(x)@34;
			// x = 2 kills x = 1.
			name:     "a namespace body runs inline where it stands",
			protects: "a namespace's statements are ordinary definitions of the enclosing function, not a closure's may-definitions",
			mutation: "lower a namespace body as a nested callable (g(x) also pairs with x = 1@4, a may-definition)",
			src:      "let x = 1; namespace N { x = 2; } g(x);",
			fn:       0,
			du:       []string{"x = 2@25 -> g(x)@34"},
		},
		{
			// `import m = require("m")` and `import a = m.b` emit `var`
			// assignments where they stand (handbook "Modules", export = and
			// import = require()). Program nodes: m@7 defines m, a@32 reads m
			// and defines a, g(a)@41 reads a.
			name:     "import-equals declarations are defining nodes",
			protects: "an import-equals declaration defines its name at its position and an alias reads its target's root",
			mutation: "make an import-equals declaration no node (m@7 and a@32 vanish with their pairs)",
			src:      "import m = require(\"m\"); import a = m.b; g(a);",
			fn:       0,
			du:       []string{"m@7 -> a@32", "a@32 -> g(a)@41"},
		},
		{
			// Handbook "Decorators": a class decorator is evaluated and
			// applied, a call, where the class is created. The class node
			// @d class A {}@27 reads d and may throw, so it controls the
			// catch's Handler@42, e@49 and g()@54.
			name:     "a decorated class reads and may throw at its creation",
			protects: "decorators are evaluated expressions read by the decorated class's node, and their application may throw",
			mutation: "stop counting a decorator as a throwing call (the class node controls nothing)",
			src:      "function f(d: any) { try { @d class A {} } catch (e) { g(); } }",
			fn:       1,
			cd: []string{
				"@d class A {}@27 -> catch@42", "@d class A {}@27 -> e@49", "@d class A {}@27 -> g()@54",
			},
			du: []string{"d@11 -> @d class A {}@27"},
		},
		{
			// Handbook "Classes": an abstract method has no body and a
			// parameter property is a parameter. Functions preorder: program
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
			// A TypeScript field is a public_field_definition; its
			// initializer runs in the class body's unit (ECMAScript §15.7.14
			// ClassDefinitionEvaluation). Branch c@22, arms 1@26 and 2@30.
			name:     "a typed field initializer is lowered in the class body's unit",
			protects: "public_field_definition takes field_definition's place in the class body unit",
			mutation: "match only field_definition in classBody (the initializer makes no node and cd is empty)",
			src:      "class A { x: number = c ? 1 : 2; }",
			fn:       1,
			cd:       []string{"c@22 -> 1@26", "c@22 -> 2@30"},
		},
	}
	// The JavaScript switch and compound-write rules hold on both grammars;
	// the sources carry no type syntax, so the offsets are JavaScript's.
	shared = append(shared,
		goldenCase{
			// ECMAScript §14.12.4 CaseClauseIsSelected; derivation as in the
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
			// ECMAScript §13.15.2; derivation as in the JavaScript case of
			// the same name.
			name:     "a compound property write's throw is on the write's node",
			protects: "the throw of a compound property assignment is not attached to the first node its right side makes",
			mutation: "count the target's throw before the right side (c@32 gains control of catch@44, e@51 and h()@56)",
			src:      "function f(o, c) { try { o.p += c ? 1 : 2 } catch (e) { h() } }",
			fn:       1,
			cd: []string{
				"c@32 -> 1@36", "c@32 -> 2@40",
				"o.p += c ? 1 : 2@25 -> catch@44", "o.p += c ? 1 : 2@25 -> e@51", "o.p += c ? 1 : 2@25 -> h()@56",
			},
			du: []string{"c@14 -> c@32", "o@11 -> o.p += c ? 1 : 2@25", "c@14 -> o.p += c ? 1 : 2@25"},
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
