package worker

import (
	"slices"
	"testing"
)

// TestTypeScriptLoweringGolden pins the control-dependence and def-use pairs
// of hand-derived TypeScript and TSX callables. The JavaScript lowering's
// shared cases (javascriptShared) run under both grammars, and its JSX cases
// (javascriptJSX) under tsx; the TypeScript cases in shared run under both
// grammars, since TSX is TypeScript with JSX; typescriptOnly runs under
// typescript only. Pairs are derived as in TestJavaScriptLoweringGolden, from the
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
			mutation: "lower a namespace body as a nested callable (its creating node may-defines x, so g(x) pairs with it and, through it, with x = 1@4)",
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
			// ECMA-262 §15.7.10 ClassFieldDefinitionEvaluation: the compiler
			// emits a TypeScript field (public_field_definition) as the field
			// without its annotation, and its initializer runs in the class
			// body's unit (§15.7.14 ClassDefinitionEvaluation). Branch c@22,
			// arms 1@26 and 2@30, each defining the conditional's result, which
			// the field x: number = c ? 1 : 2@10 Uses; c is free in the unit.
			name:     "a typed field initializer is lowered in the class body's unit",
			protects: "public_field_definition takes field_definition's place in the class body unit",
			mutation: "match only field_definition in classBody (the initializer makes no node and cd and du are empty)",
			src:      "class A { x: number = c ? 1 : 2; }",
			fn:       1,
			cd:       []string{"c@22 -> 1@26", "c@22 -> 2@30"},
			du:       []string{"1@26 -> x: number = c ? 1 : 2@10", "2@30 -> x: number = c ? 1 : 2@10"},
		},
	}
	shared = append(shared,
		goldenCase{
			// The compiler's emit of `f?.(a)` is ECMA-262's optional call
			// (§13.3.9 OptionalChain): when f is null or undefined nothing
			// right of `?.` is evaluated. Nodes: g@11, x = 0@25, the Branch
			// g@32 before `?.` (Uses g, hands the callee on through the chain's
			// result), x = 1@36 (defines x and its own result) and the chain
			// g?.(x = 1)@32 (Uses both results) on the non-nullish path, return
			// x;@44, reached by x = 0 on the nullish path and x = 1 on the other.
			name:     "a TypeScript optional call short-circuits its arguments",
			protects: "the anonymous `?.` of a TypeScript optional call is an optional chain, so its arguments are conditional and the definition before them still reaches past the call",
			mutation: "test only the optional_chain field in chain (g@32 and its control dependences vanish, and return x;@44 loses x = 0@25), or give the chain node the callee's reads (g@11 -> g?.(x = 1)@32 appears)",
			src:      "function f(g: any) { let x = 0; g?.(x = 1); return x; }",
			fn:       1,
			cd:       []string{"g@32 -> x = 1@36", "g@32 -> g?.(x = 1)@32"},
			du: []string{
				"g@11 -> g@32", "g@32 -> g?.(x = 1)@32", "x = 1@36 -> g?.(x = 1)@32",
				"x = 0@25 -> return x;@44", "x = 1@36 -> return x;@44",
			},
		},
		goldenCase{
			// The compiler's emit of `x as T` is x: the type `typeof y`
			// evaluates nothing. Nodes: x@11, y@19, the arrow
			// () => x as typeof y@39 (Uses its capture x, defines its result),
			// the declarator g = () => x as typeof y@35 (Uses the result,
			// defines g), return g;@60.
			name:     "a closure's captures skip the type of an as expression",
			protects: "the capture collection walks only the operand of a transparent wrapper, never its type",
			mutation: "walk every child of an as expression in cap (y@19 -> () => x as typeof y@39 appears)",
			src:      "function f(x: any, y: any) { const g = () => x as typeof y; return g; }",
			fn:       1,
			du: []string{
				"x@11 -> () => x as typeof y@39", "() => x as typeof y@39 -> g = () => x as typeof y@35",
				"g = () => x as typeof y@35 -> return g;@60",
			},
		},
		goldenCase{
			// The compiler's emit of `[y!] = a` is `[y] = a`, an ECMA-262
			// destructuring assignment (§13.15.5.5 IteratorDestructuringAssignmentEvaluation)
			// that binds y, its right side evaluated once. Nodes: a@11,
			// y = 0@25, the right side a@39 (Uses a, defines the incoming
			// value), the element y@33 (Uses it, defines y), return y;@42;
			// y@33 kills y = 0.
			name:     "a destructuring element through a non-null assertion defines the variable",
			protects: "pattern elements are read through the transparent wrappers, so the element kills the earlier definition",
			mutation: "drop the strip at the start of bind (y@33 vanishes with a@39 -> y@33 and y@33 -> return y;@42, and y = 0@25 -> return y;@42 appears)",
			src:      "function f(a: any) { let y = 0; [y!] = a; return y; }",
			fn:       1,
			du:       []string{"a@11 -> a@39", "a@39 -> y@33", "y@33 -> return y;@42"},
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
	shared = append(shared,
		goldenCase{
			// The compiler's emit: a `declare` declaration emits no code, so
			// `declare let g` binds no variable and g names the global the
			// program assumes (ECMA-262 §9.4.2 ResolveBinding), never a
			// variable of f; h is free too. The wrappers are erased, so each
			// target is g read through them; `delete g` is left out, the
			// compiler rejecting a delete of anything but a property
			// reference. Nodes: x@31; g! = x@41 and (g as
			// any) += x@49 (each Uses x, defines nothing), g++@66 (Uses
			// nothing); the embedded g! = x@73 (Uses x, defines its result) and
			// h(g! = x)@71 (Uses the result); (g as any).p = x@82 (Uses x,
			// may-defines no base); the right side x@107 (Uses x, defines the
			// incoming value) and the element g@101 (Uses it); the iterated
			// x@120 (Uses x, defines the iteration variable), the head g of
			// x@115 and the binding g@115 (each Uses it), h(g)@123 (Uses
			// nothing); the arrow () => g! += x@139 (Uses its capture x,
			// may-defines nothing, defines its result), the declarator c = ()
			// => g! += x@135 (Uses the result, defines c); return c;@154.
			name:     "a name only a declare form introduces is read and written as nothing through the wrappers",
			protects: "an ambient name reached through a transparent wrapper as an assignment, compound, update, embedded, property-base, destructuring, iteration or capture target defines and reads nothing, while its node keeps its other reads",
			mutation: "drop the v < 0 return in def (flow.Builder.Def panics on g's -1 at g! = x@41), in read (seen[-1] panics at (g as any) += x@49), in mayDefBase (MayDef(-1) at (g as any).p = x@82) or in capTarget (MayDef(-1) on the arrow's node)",
			src:      "declare let g: any; function f(x: any) { g! = x; (g as any) += x; g++; h(g! = x); (g as any).p = x; [g!] = x; for (g of x) h(g); const c = () => g! += x; return c; }",
			fn:       1,
			cd:       []string{"g of x@115 -> g of x@115", "g of x@115 -> g@115", "g of x@115 -> h(g)@123"},
			du: []string{
				"x@31 -> g! = x@41", "x@31 -> (g as any) += x@49", "x@31 -> g! = x@73", "g! = x@73 -> h(g! = x)@71",
				"x@31 -> (g as any).p = x@82", "x@31 -> x@107", "x@107 -> g@101",
				"x@31 -> x@120", "x@120 -> g of x@115", "x@120 -> g@115",
				"x@31 -> () => g! += x@139", "() => g! += x@139 -> c = () => g! += x@135",
				"c = () => g! += x@135 -> return c;@154",
			},
		},
	)
	shared = append(shared, []goldenCase{
		{
			// The compiler's emit: an overload signature emits no code, so it is no callable. Functions preorder:
			// the program 0, the implementation 1. Nodes: a@40, g(a)@50.
			name:      "an overload signature is not a callable",
			protects:  "callable indexes skip a function signature without a body",
			mutation:  "count function_signature as a callable (callables becomes 3 and callable 1 is the signature, with no pairs)",
			src:       "function h(a: string): void; function h(a: any) { g(a); }",
			fn:        1,
			du:        []string{"a@40 -> g(a)@50"},
			callables: 2,
		},
		{
			// The compiler's emit erases an index signature, so the class node class A { [k: string]: any }@21
			// reads nothing, its k being the signature's own parameter name; it defines A, which return A;@50
			// reads.
			name:     "an index signature is erased and reads nothing",
			protects: "an index signature's bracketed name is no computed key and no read",
			mutation: "walk an index signature as a computed key (k@11 -> class A { [k: string]: any }@21 appears)",
			src:      "function f(k: any) { class A { [k: string]: any } return A; }",
			fn:       1,
			du:       []string{"class A { [k: string]: any }@21 -> return A;@50"},
		},
		{
			// The compiler's emit elides `import type`, and a default export of a name that has no value
			// meaning (the compiler accepts one naming a type-only import and emits nothing for it). Program
			// nodes: the default export's T@43, which reads nothing.
			name:     "an import type declaration binds nothing",
			protects: "import type makes no node and no variable, so a later read of its name pairs with nothing",
			mutation: "bind the names of an import type declaration (T@14 -> T@43 appears)",
			src:      "import type { T } from \"m\"; export default T;",
			fn:       0,
		},
		{
			// The compiler's emit elides a `type` import specifier and keeps the others. Program nodes: v@17
			// (the one binding), g(v)@31, the default export's U@52, which reads nothing.
			name:     "a type import specifier binds nothing",
			protects: "a type specifier makes no node and no variable, while the value specifiers beside it bind",
			mutation: "bind type specifiers (U@14 -> U@52 appears), or skip the whole clause (v@17 -> g(v)@31 vanishes)",
			src:      "import { type U, v } from \"m\"; g(v); export default U;",
			fn:       0,
			du:       []string{"v@17 -> g(v)@31"},
		},
		{
			// The compiler's emit inlines a const enum's members and emits no declaration, so the if's then block
			// makes no node and nothing depends on the Branch c@25.
			name:     "a const enum is erased",
			protects: "a const enum makes no node, even inside a branch",
			mutation: "lower a const enum as an enum (E@41 and its member node appear, control dependent on c@25)",
			src:      "function f(c: any) { if (c) { const enum E { a = 1 } } return c; }",
			fn:       1,
			du:       []string{"c@11 -> c@25", "c@11 -> return c;@55"},
		},
		{
			// The compiler's emit of `x satisfies T` is x. The Branch spans the condition as written, x satisfies
			// number@25, and reads x; it controls g()@45.
			name:     "satisfies evaluates to its operand",
			protects: "a satisfies expression is read through to its operand and a Branch spans it as written",
			mutation: "treat satisfies as opaque (x@11 -> x satisfies number@25 vanishes)",
			src:      "function f(x: any) { if (x satisfies number) g(); return x; }",
			fn:       1,
			cd:       []string{"x satisfies number@25 -> g()@45"},
			du:       []string{"x@11 -> x satisfies number@25", "x@11 -> return x;@50"},
		},
		{
			// The compiler's emit of `h<T>` (TS 4.7) is h; h is generic, so the compiler accepts the type
			// arguments. Nodes: h@11, k = h<number>@38 (Uses h), return k;@53.
			name:     "an instantiation expression evaluates to its operand",
			protects: "type arguments on a value are erased and the operand is read",
			mutation: "treat an instantiation expression as opaque (h@11 -> k = h<number>@38 vanishes)",
			src:      "function f(h: <T>(x: T) => T) { const k = h<number>; return k; }",
			fn:       1,
			du:       []string{"h@11 -> k = h<number>@38", "k = h<number>@38 -> return k;@53"},
		},
		{
			// The compiler's emit of `o?.p!` is `o?.p`: the chain's node o?.p@21 stands for the wrapper, and the
			// statement makes no second node. Nodes: o@11, the Branch o@21, the chain o?.p@21.
			name:     "the node an operand's lowering made stands for a transparent wrapper",
			protects: "a statement ending in a wrapped chain takes no second node spanning the wrapper",
			mutation: "end the statement with its own node when a wrapper encloses the chain (o?.p!@21 appears, Using the chain's result)",
			src:      "function f(o: any) { o?.p!; }",
			fn:       1,
			cd:       []string{"o@21 -> o?.p@21"},
			du:       []string{"o@11 -> o@21", "o@21 -> o?.p@21"},
		},
		{
			// Parameter decorators exist only under the compiler's experimentalDecorators (the legacy
			// decorators; ECMAScript decorators have none), whose emit applies them where the class is defined
			// (`__decorate([__param(0, d)], …)`, a call). The class node class A { m(@d p: any) {} }@27 reads d and may throw, so it
			// controls the Handler catch@57, e@64 and g()@69.
			name:     "a parameter decorator is read and may throw at the class's creation",
			protects: "a method parameter's decorators are the class's, not the method's",
			mutation: "leave parameter decorators to the method (d@11 -> the class node vanishes and it controls nothing)",
			src:      "function f(d: any) { try { class A { m(@d p: any) {} } } catch (e) { g(); } }",
			fn:       1,
			cd:       []string{"class A { m(@d p: any) {} }@27 -> catch@57", "class A { m(@d p: any) {} }@27 -> e@64", "class A { m(@d p: any) {} }@27 -> g()@69"},
			du:       []string{"d@11 -> class A { m(@d p: any) {} }@27"},
		},
		{
			// The compiler's emit applies a member decorator where the class is defined (`__decorate([d],
			// A.prototype, "m")`, a call). The class node class A { @d m() {} }@27 reads d and may throw, so it
			// controls the Handler catch@51, e@58 and g()@63.
			name:     "a member decorator is read and may throw at the class's creation",
			protects: "a method's decorators are evaluated by the class node",
			mutation: "count only class-level decorators (the class node controls nothing and d@11 -> class A { @d m() {} }@27 vanishes)",
			src:      "function f(d: any) { try { class A { @d m() {} } } catch (e) { g(); } }",
			fn:       1,
			cd:       []string{"class A { @d m() {} }@27 -> catch@51", "class A { @d m() {} }@27 -> e@58", "class A { @d m() {} }@27 -> g()@63"},
			du:       []string{"d@11 -> class A { @d m() {} }@27"},
		},
		{
			// The compiler's emit drops `abstract`, so an abstract class is a class: its node abstract class A { x
			// = a; }@21 captures its field initializer's a and defines A, which return A;@49 reads.
			name:     "an abstract class is lowered as a class",
			protects: "abstract_class_declaration creates and captures as a class declaration does",
			mutation: "treat an abstract class as erased (both pairs vanish)",
			src:      "function f(a: any) { abstract class A { x = a; } return A; }",
			fn:       1,
			du:       []string{"a@11 -> abstract class A { x = a; }@21", "abstract class A { x = a; }@21 -> return A;@49"},
		},
		{
			// The compiler's emit of two enums of one name is one variable, `E || (E = {})` twice. Nodes: E@20
			// (Uses and defines E), a = 1@24, E@37 (Uses the first definition, defines E again), b = 2@41, return
			// E;@49.
			name:     "a second enum of a name merges into the first's variable",
			protects: "an enum whose name its block already binds declares no second variable",
			mutation: "declare a new variable for each enum (E@20 -> E@37 vanishes)",
			src:      "function f() { enum E { a = 1 } enum E { b = 2 } return E; }",
			fn:       1,
			du:       []string{"E@20 -> E@37", "E@37 -> return E;@49"},
		},
		{
			// The compiler's emit merges a namespace into the class of its name (`(function (C) { … })(C || (C =
			// {}))`); a namespace stands only at a file's or a namespace's top level. Program nodes: class C
			// {}@0 (defines C), the namespace's C@21 (Uses and defines C), g()@25, h(C)@32.
			name:     "a namespace merges into the class of its name",
			protects: "a namespace whose name a class in its block binds reads and assigns that variable",
			mutation: "declare a new variable for a namespace that merges (class C {}@0 -> C@21 vanishes)",
			src:      "class C {} namespace C { g(); } h(C);",
			fn:       0,
			du:       []string{"class C {}@0 -> C@21", "C@21 -> h(C)@32"},
		},
		{
			// The compiler's emit of `namespace A.B` is nested functions invoked on A. Program nodes: A@10 (the
			// path's leftmost name, Uses and defines A), g()@16, h(A)@23.
			name:     "a dotted namespace defines its leftmost name",
			protects: "the namespace node spans and defines the leftmost name of its path",
			mutation: "span and define the whole path (A.B@10 appears in A@10's place and defines no variable, so A@10 -> h(A)@23 vanishes)",
			src:      "namespace A.B { g(); } h(A);",
			fn:       0,
			du:       []string{"A@10 -> h(A)@23"},
		},
		{
			// The compiler's emit of a namespace body is a function, so its var is its own. Program nodes: x =
			// 1@4, N@21, x = 2@29 (the namespace's x), g(x)@38, which reads the program's x.
			name:     "a namespace body has its own var scope",
			protects: "a var inside a namespace does not merge into the enclosing variable of its name",
			mutation: "hoist a namespace's vars into the enclosing function (x = 2@29 -> g(x)@38 replaces x = 1@4 -> g(x)@38)",
			src:      "var x = 1; namespace N { var x = 2; } g(x);",
			fn:       0,
			du:       []string{"x = 1@4 -> g(x)@38"},
		},
		{
			// The compiler's emit: a module named by a string is an ambient declaration and emits nothing. Program
			// nodes: x = 1@4, g(x)@33. Ill-formed on purpose: the compiler rejects a quoted name outside an ambient
			// context (TS1035), and in a declaration file, the one place written without `declare`, a module holds
			// no statement a pair could show. The case pins that the lowering never runs such a body, which the
			// grammar parses as the namespace kind.
			name:      "a string-named module is ambient",
			protects:  "nothing inside a string-named module declaration is lowered",
			mutation:  "lower a string-named module as a namespace (x = 2 kills x = 1: x = 2@24 -> g(x)@33 replaces x = 1@4 -> g(x)@33)",
			src:       "let x = 1; module \"m\" { x = 2; } g(x);",
			fn:        0,
			du:        []string{"x = 1@4 -> g(x)@33"},
			illFormed: true,
		},
		{
			// The compiler's emit of `import m = require("m")` is `var m = require("m")`, a call (ECMA-262
			// §13.3.6). Nodes: N@31, m@42 (may throw), the Handler catch@64, e@71, g()@76. Ill-formed on purpose:
			// the compiler admits an import-equals only at a file's or a namespace's top level (TS1235) and a
			// require form only at a file's (TS1147), so no try frame can enclose one and no valid program shows
			// its throw; the case pins that the form is a call, as its emit is.
			name:      "a require import-equals may throw",
			protects:  "the import-equals node of the require form is a throw point",
			mutation:  "stop counting the require form as a throw (m@42 controls nothing)",
			src:       "function f() { try { namespace N { import m = require(\"m\"); } } catch (e) { g(); } }",
			fn:        1,
			cd:        []string{"m@42 -> catch@64", "m@42 -> e@71", "m@42 -> g()@76"},
			illFormed: true,
		},
		{
			// The compiler's emit of `export = e` is `module.exports = e`: e is evaluated like a default export.
			// Program nodes: x = 1@4, x@20.
			name:     "export = evaluates its expression",
			protects: "an export assignment is a value node reading its expression",
			mutation: "make an export assignment no node (x = 1@4 -> x@20 vanishes)",
			src:      "let x = 1; export = x;",
			fn:       0,
			du:       []string{"x = 1@4 -> x@20"},
		},
	}...)
	// typescriptOnly holds the cases whose source only the typescript grammar
	// parses: the tsx grammar has no type assertion.
	typescriptOnly := []goldenCase{
		{
			// The compiler's emit of `<T>e` is e. Nodes: x@11, y = <number>x@27 (Uses x), return y;@42. The tsx
			// grammar has no type assertion, so this case runs under typescript only.
			name:     "a type assertion evaluates to its operand",
			protects: "an angle-bracket assertion is read through to its operand",
			mutation: "treat a type assertion as opaque (x@11 -> y = <number>x@27 vanishes)",
			src:      "function f(x: any) { const y = <number>x; return y; }",
			fn:       1,
			du:       []string{"x@11 -> y = <number>x@27", "y = <number>x@27 -> return y;@42"},
		},
	}
	runGolden(t, "typescript", slices.Concat(javascriptShared, shared, typescriptOnly))
	runGolden(t, "tsx", slices.Concat(javascriptShared, javascriptJSX, shared))
}
